// Package assistant is the reusable AI-assistant kernel. It provides the
// chat HTTP/SSE pump, chat + handoff persistence, capability detection,
// flow-version resolution, and admin chat browser used by every consumer
// (redelay, FlowDSL, external SaaS deployments).
//
// Responsibilities:
//
//  1. Seed a default "assistant" flow on first startup from the embedded
//     default.flowdsl.json. If the flow already exists in the flowexec store,
//     the seed is skipped — the project's customized version always wins.
//
//  2. Resolve the currently-published version of the configured flow on every
//     chat request, launch a run with {messages, ...} as input, and translate
//     the resulting FlowEvent stream into chat-facing SSE frames
//     ({content}/{done}/{error}) that match the wire format used by every
//     consuming UI.
//
//  3. Aggregate project-specific knowledge packs — search index entities
//     and RAG-flavor flow templates — discovered via IndexEntityProvider
//     and TemplateProvider in the registry. See extension.go.
//
// The frontend knows nothing about flows, runs, or FlowDSL — it just POSTs
// {messages} and reads an SSE stream. All flow plumbing lives here.
package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dreplyai/go-assistant/chats"
	assistantflowdsl "github.com/dreplyai/go-assistant/flowdsl"
	"github.com/dreplyai/go-assistant/handoff"
	flowexecmod "github.com/redelay/go-flowdsl/flowexec/module"
	flowstore "github.com/redelay/go-flowdsl/flowexec/store"
	"github.com/redelay/go-flowdsl/ir"
	"github.com/redelay/go-flowdsl/runtime"
	fwconfig "github.com/redelay/go-framework/config"
	"github.com/redelay/go-framework/modules"
	"github.com/redelay/go-modules/search"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
)

func init() {
	modules.RegisterFactory("assistant", func(deps modules.ModuleDeps) (modules.Module, error) {
		return New(deps)
	})
}

// Module is the project-local assistant.
type Module struct {
	modules.IRBase

	cfg      Config
	logger   *zap.Logger
	flowexec *flowexecmod.Module

	// Persistence services — nil when the deployment has no DB.
	// Handlers check for nil and degrade gracefully (no persistence,
	// streaming still works).
	chats   *chats.Service
	handoff *handoff.Service

	// db is kept because the docs corpus's document store is built from it in
	// two different processes: the server at Startup, and the ingest command,
	// which never runs Startup.
	db *mongo.Database

	// EventBus is used to publish assistant.handoff_requested. Nil
	// when the deployment has no transport — handoff persistence
	// still works, event just doesn't fire.
	bus modules.EventBus

	// roleChecker filters deployment variants by the live cluster
	// worker registry. Discovered from the workers-admin module in
	// Configure — nil when that module is not loaded (e.g. on
	// cmd/api), in which case every variant is always considered
	// executable. Passed into flowstore.ResolveDeployment on every
	// chat turn.
	roleChecker flowstore.RoleChecker

	// Project knowledge-pack extensions discovered from the registry
	// at Configure time. The kernel ships seven generic flow
	// templates and zero search indexes; everything project-specific
	// — the docs index entity and the RAG-flavor flow templates that
	// reference it — comes from these providers. See extension.go.
	indexProviders    []IndexEntityProvider
	templateProviders []TemplateProvider

	// Multi-tenancy. tenantReg caches every registered tenant
	// (Mongo-loaded + programmatic); tenantStore is nil on DB-less
	// deployments; tenantResolvers are host-app seams discovered in
	// Configure that map a request → tenant key. With zero tenants
	// and zero resolvers every request lands on the default tenant
	// and the module behaves exactly like the single-tenant kernel.
	tenantReg       *tenantRegistry
	tenantStore     *TenantStore
	tenantResolvers []TenantResolver

	// rl enforces per-tenant per-session chat rate limits. Always
	// non-nil; tenants with RateLimitPerMinute==0 bypass it.
	rl *sessionRateLimiter
}

// IndexEntities aggregates entities from every IndexEntityProvider
// extension in the registry so search-admin discovers them through
// the assistant module like before. Returns nil when no extensions
// declare any indexes — the search module then simply lists nothing
// under "Assistant Docs" and the RAG flows from the same extensions
// will fail at run time, which is the expected behavior on a host
// that never blank-imported a knowledge pack.
func (m *Module) IndexEntities() []search.IndexEntity {
	if len(m.indexProviders) == 0 {
		return nil
	}
	var out []search.IndexEntity
	for _, p := range m.indexProviders {
		out = append(out, p.AssistantIndexEntities()...)
	}
	return out
}

// isOutputNode reports whether the given node id is one of the
// terminals the SSE pump should mine for client-facing content.
func (m *Module) isOutputNode(id string) bool {
	for _, n := range m.cfg.OutputNodes {
		if n == id {
			return true
		}
	}
	return false
}

// Chats exposes the chats service for admin submodule access.
func (m *Module) Chats() *chats.Service { return m.chats }

// Handoff exposes the handoff service for admin submodule access.
func (m *Module) Handoff() *handoff.Service { return m.handoff }

// Config exposes the loaded configuration (read-only).
func (m *Module) Cfg() Config { return m.cfg }

// Logger exposes the module logger for admin submodule access.
func (m *Module) Logger() *zap.Logger { return m.logger }

// ReseedFromTemplate saves + publishes the named embedded template as
// a fresh version of the assistant flow. Exposed for the admin
// submodule's POST /reset handler. Returns ErrUnknownTemplate for
// unrecognised names.
func (m *Module) ReseedFromTemplate(ctx context.Context, name string) error {
	return m.reseedFromTemplate(ctx, name)
}

// ErrUnknownTemplate is returned by ReseedFromTemplate when the caller
// asks for a template the module does not ship.
var ErrUnknownTemplate = errUnknownTemplate

// New constructs the module from framework deps. Resolves the sibling
// flowexec module via its package-level Current() accessor — flowexec must
// be blank-imported before this module (it is, through the import graph
// since this file imports go-modules/flowexec).
func New(deps modules.ModuleDeps) (*Module, error) {
	logger := deps.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	fx := flowexecmod.Current()
	if fx == nil {
		return nil, fmt.Errorf("assistant: go-modules/flowexec must be blank-imported before backend/modules/assistant")
	}
	cfg := DefaultConfig()
	m := &Module{
		IRBase:    modules.MustLoadIR(moduleYAML),
		cfg:       cfg,
		logger:    logger,
		flowexec:  fx,
		bus:       deps.EventBus,
		tenantReg: newTenantRegistry(),
		rl:        newSessionRateLimiter(),
	}
	// Persistence — nil DB means in-memory-only deployment. Chat
	// history + handoffs are simply not durable in that case; the
	// SSE stream still works and end users don't notice.
	if deps.DB != nil {
		m.db = deps.DB
		m.tenantStore = NewTenantStore(deps.DB)
		m.chats = chats.NewService(deps.DB, chats.Options{
			TTL: time.Duration(cfg.ChatTTLDays) * 24 * time.Hour,
		})
		m.handoff = handoff.NewService(deps.DB, m.chats, deps.EventBus, handoff.Options{})
	}
	return m, nil
}

// Startup ensures the assistant flow + persistence infrastructure
// is ready. Idempotent — flow seeding is skipped when one exists,
// and EnsureIndexes is safe to re-run on every boot.
func (m *Module) Startup(ctx context.Context) error {
	// The docs corpus is declared here, not in the ingest command.
	//
	// It lived in the CLI handler first, on the reasoning that registering at
	// import time would put a corpus in the admin browser of every project
	// that links this package. That was the wrong conclusion from a correct
	// premise: the registry is read by a running server — the admin browser,
	// the retrieval node — and the CLI is a separate process, so a
	// CLI-only registration means the server never knows the corpus exists
	// and /admin/assistant/corpora is empty however full the index is.
	//
	// Startup is the right seam because only a project that mounts the
	// assistant module gets here, which is exactly the projects that have
	// docs to cite.
	m.registerDocsCorpus(ctx)

	if m.chats != nil {
		if err := m.chats.EnsureIndexes(ctx); err != nil {
			m.logger.Warn("assistant: chats EnsureIndexes failed", zap.Error(err))
		}
	}
	if m.handoff != nil {
		if err := m.handoff.EnsureIndexes(ctx); err != nil {
			m.logger.Warn("assistant: handoff EnsureIndexes failed", zap.Error(err))
		}
	}
	// Wire the assistant-handoff-request FlowDSL node to the live
	// service. The flowdsl sibling package only ships the manifest +
	// handler skeleton; this call plugs in the concrete service so
	// flows that include the node can actually file handoffs.
	if eng := runtime.Default(); eng != nil {
		if m.handoff != nil {
			assistantflowdsl.Register(eng, &handoffAdapter{svc: m.handoff})
		}
		// redelay/assistant-rag-context resolves the search service
		// lazily via search.Current — safe to register even when the
		// search module hasn't finished booting (Startup order is
		// alphabetical, so `assistant` runs before `search`).
		assistantflowdsl.RegisterRagContext(eng)
		// The composable pair. Registered unconditionally alongside the older
		// node: both resolve their dependencies lazily, and a handler that is
		// declared in the manifest but never registered is the worst of both
		// worlds — Studio offers the node, a flow using it validates, and the
		// run fails at the step with "no handler". That was this package's
		// state until now: corpus-retrieve had a handler nothing ever wired.
		assistantflowdsl.RegisterCorpusRetrieve(eng)
		assistantflowdsl.RegisterCorpusContext(eng)
	}
	if err := m.ensureFlow(ctx, false); err != nil {
		return err
	}
	// A deployment must exist before the first chat lands — the SSE
	// handler resolves the active variant through it on every turn.
	if err := m.ensureDeployment(ctx); err != nil {
		return err
	}
	// Hydrate the tenant registry from Mongo and ensure each tenant's
	// deployment. Non-fatal: a failed load degrades to default-only.
	m.loadTenants(ctx)
	return nil
}

func (m *Module) Shutdown(_ context.Context) error { return nil }

// Configure discovers sibling modules that contribute runtime hooks:
//
//   - workers-admin (optional) — supplies the role-health checker used
//     for variant filtering in ResolveDeployment. When not loaded
//     (e.g. on the public cmd/api binary) roleChecker stays nil and
//     every variant is eligible.
//
//   - any module implementing IndexEntityProvider — declares search
//     indexes the assistant's RAG flows ground against. The kernel
//     surfaces these via IndexEntities() so search-admin can render
//     them in the admin search browser.
//
//   - any module implementing TemplateProvider — contributes flow
//     templates surfaced in Studio's "Create flow" picker and
//     addressable by short name in the reseed endpoint.
//
// Provider lookup is non-fatal — the kernel boots happily on a host
// that blank-imports zero knowledge packs (chat works, RAG flows in
// Studio's picker simply won't match any indexes).
func (m *Module) Configure(registry *modules.Registry) error {
	if registry == nil {
		return nil
	}

	// Workers-admin (existing behavior, unchanged).
	if raw := registry.Get("workers-admin"); raw != nil {
		if rc, ok := raw.(interface{ IsRoleHealthy(role string) bool }); ok {
			m.roleChecker = rc.IsRoleHealthy
			if m.logger != nil {
				m.logger.Info("assistant: workers-admin wired for role-aware deployment resolution")
			}
		} else if m.logger != nil {
			m.logger.Warn("assistant: workers-admin does not satisfy IsRoleHealthy; variant role-filtering disabled")
		}
	}

	// Knowledge-pack extension discovery.
	for _, mod := range registry.All() {
		if p, ok := mod.(IndexEntityProvider); ok {
			m.indexProviders = append(m.indexProviders, p)
		}
		if p, ok := mod.(TemplateProvider); ok {
			m.templateProviders = append(m.templateProviders, p)
		}
		if p, ok := mod.(TenantResolver); ok {
			m.tenantResolvers = append(m.tenantResolvers, p)
		}
	}
	if m.logger != nil && len(m.tenantResolvers) > 0 {
		m.logger.Info("assistant: discovered tenant resolvers",
			zap.Int("resolvers", len(m.tenantResolvers)))
	}
	if m.logger != nil && (len(m.indexProviders) > 0 || len(m.templateProviders) > 0) {
		m.logger.Info("assistant: discovered knowledge-pack extensions",
			zap.Int("indexProviders", len(m.indexProviders)),
			zap.Int("templateProviders", len(m.templateProviders)))
	}
	return nil
}

// ensureFlow idempotently creates + publishes the default flow if missing.
// When force=true, a new version is saved and published even if the flow
// already exists — used by the "reset to factory" endpoint.
func (m *Module) ensureFlow(ctx context.Context, force bool) error {
	store := m.flowexec.Store()

	existing, err := store.GetFlow(ctx, m.cfg.FlowID)
	if err != nil && !errors.Is(err, flowstore.ErrNotFound) {
		return fmt.Errorf("assistant: get flow: %w", err)
	}

	embeddedVersion := embeddedSchemaVersion()

	if existing == nil {
		_, err := store.CreateFlow(ctx, flowstore.CreateFlowInput{
			ID:          m.cfg.FlowID,
			Name:        "Project Assistant",
			Description: "Default assistant flow — customize via flowexec API.",
			Labels:      map[string]string{"owner": "assistant"},
		})
		if err != nil {
			return fmt.Errorf("assistant: create flow: %w", err)
		}
		m.logger.Info("assistant: created flow", zap.String("flowID", m.cfg.FlowID))
	} else if !force {
		// Flow exists — decide whether to auto-upgrade. When the published
		// version was written by THIS module (CreatedBy=assistant-module)
		// and the embedded schemaVersion advanced past the stored one, save
		// + publish a new version. When the published version was touched
		// by a human, we never overwrite — they own it. The flag
		// ASSISTANT_FLOW_SKIP_AUTO_UPGRADE=true disables the upgrade path
		// entirely for fully managed deployments.
		if fwconfig.GetBool("ASSISTANT_FLOW_SKIP_AUTO_UPGRADE", false) {
			m.logger.Info("assistant: auto-upgrade disabled, keeping published version",
				zap.String("publishedVersionID", existing.PublishedVersionID))
			return nil
		}
		storedVersion, ownedByModule := m.inspectPublishedVersion(ctx, existing)
		if !ownedByModule {
			m.logger.Info("assistant: published version is user-owned, skipping auto-upgrade",
				zap.String("publishedVersionID", existing.PublishedVersionID))
			return nil
		}
		if storedVersion >= embeddedVersion {
			m.logger.Info("assistant: flow up to date",
				zap.Int("schemaVersion", storedVersion))
			return nil
		}
		m.logger.Info("assistant: upgrading module-owned flow",
			zap.Int("from", storedVersion), zap.Int("to", embeddedVersion))
		// fall through — save + publish fresh version below
	}

	// Seed version from embedded default.
	var doc ir.Workflow
	if err := json.Unmarshal(defaultFlowJSON, &doc); err != nil {
		return fmt.Errorf("assistant: parse embedded default flow: %w", err)
	}

	// Apply env-var overrides to the chat node if configured. Match by
	// the well-known node id rather than OutputNodes — those identify
	// terminals, not the chat node.
	if m.cfg.Provider != "" || m.cfg.Model != "" {
		for i := range doc.Nodes {
			if doc.Nodes[i].ID == chatNodeID {
				if doc.Nodes[i].Config == nil {
					doc.Nodes[i].Config = make(map[string]any)
				}
				if m.cfg.Provider != "" {
					doc.Nodes[i].Config["providerID"] = m.cfg.Provider
				}
				if m.cfg.Model != "" {
					doc.Nodes[i].Config["model"] = m.cfg.Model
				}
				break
			}
		}
	}

	v, err := store.SaveVersion(ctx, flowstore.SaveVersionInput{
		FlowID:    m.cfg.FlowID,
		Document:  &doc,
		Note:      fmt.Sprintf("seeded from module default (schemaVersion=%d)", embeddedVersion),
		CreatedBy: "assistant-module",
		Labels: map[string]string{
			"source":        "embedded-default",
			"schemaVersion": fmt.Sprintf("%d", embeddedVersion),
		},
	})
	if err != nil {
		return fmt.Errorf("assistant: save seed version: %w", err)
	}

	if _, err := store.PublishVersion(ctx, m.cfg.FlowID, v.ID); err != nil {
		return fmt.Errorf("assistant: publish seed version: %w", err)
	}
	m.logger.Info("assistant: seeded + published default flow",
		zap.String("flowID", m.cfg.FlowID),
		zap.String("versionID", v.ID),
		zap.String("versionHash", v.VersionHash))
	return nil
}
