package assistant

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"

	flowstore "github.com/redelay/go-flowdsl/flowexec/store"
	"github.com/redelay/go-framework/crud"
	"go.uber.org/zap"
)

// Tenant describes one assistant instance served by this backend: its
// own flow (and therefore its own prompt), its own docs corpus/index,
// its own handoff-inbox scoping, and optional public-config overrides.
//
// The empty key is reserved for the *default tenant*, which is never
// stored — it is synthesised from the ASSISTANT_* env config on every
// lookup, so a backend with zero registered tenants behaves exactly
// like the single-tenant kernel always has.
type Tenant struct {
	crud.BaseModel `bson:",inline"`

	// Key is the tenant selector clients send (header X-Assistant-Site
	// or query ?site=). Unique, lowercase, [a-z0-9._-].
	Key string `bson:"key" json:"key"`

	// Name is a human-readable label for admin UIs.
	Name string `bson:"name,omitempty" json:"name,omitempty"`

	// DeploymentID names this tenant's FlowDeployment in the flowexec
	// store. Empty derives "<default-deployment>--<key>" so tenants
	// never collide with the default deployment or each other.
	DeploymentID string `bson:"deployment_id,omitempty" json:"deploymentId,omitempty"`

	// FlowID is the flow the tenant's auto-created deployment points
	// at with a single stable variant. Empty derives
	// "<default-flow>--<key>". Once the deployment exists, variants
	// are managed through the flowexec deployment CRUD as usual.
	FlowID string `bson:"flow_id,omitempty" json:"flowId,omitempty"`

	// DocsIndex is the search index / corpus name this tenant's RAG
	// flows ground against. Threaded into run meta as `docs_index` so
	// flows can template it; empty inherits ASSISTANT_DOCS_INDEX.
	DocsIndex string `bson:"docs_index,omitempty" json:"docsIndex,omitempty"`

	// SuggestedQuestions surface in GET /assistant/config so the
	// widget can render per-tenant conversation starters.
	SuggestedQuestions []string `bson:"suggested_questions,omitempty" json:"suggestedQuestions,omitempty"`

	// RateLimitPerMinute caps chat messages per session per minute.
	// 0 = unlimited (the default-tenant behaviour).
	RateLimitPerMinute int `bson:"rate_limit_per_minute,omitempty" json:"rateLimitPerMinute,omitempty"`

	// HandoffEnabled / AnonymousAllowed override the env defaults for
	// this tenant. Nil = inherit.
	HandoffEnabled   *bool `bson:"handoff_enabled,omitempty" json:"handoffEnabled,omitempty"`
	AnonymousAllowed *bool `bson:"anonymous_allowed,omitempty" json:"anonymousAllowed,omitempty"`

	// Disabled tenants resolve like unknown keys — requests fall back
	// to the default tenant.
	Disabled bool `bson:"disabled,omitempty" json:"disabled,omitempty"`
}

// TenantResolver is the seam a host app implements to map a request
// to a tenant key — e.g. by Host header, channel, or path. Implement
// it on any registered module; the assistant discovers it at
// Configure time (same registry-walk pattern as IndexEntityProvider).
//
// Return "" to decline — resolution then falls through to the default
// tenant. An explicit key (header/query) always wins over resolvers.
type TenantResolver interface {
	ResolveTenant(r *http.Request) string
}

// TenantHeader / TenantQueryParam are the explicit per-request tenant
// selectors, checked before any TenantResolver.
const (
	TenantHeader     = "X-Assistant-Site"
	TenantQueryParam = "site"
)

// tenantKeyRe validates registered tenant keys. Conservative on
// purpose — keys travel in headers, query params, and Mongo filters.
var tenantKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ErrTenantExists / ErrTenantNotFound / ErrInvalidTenantKey are the
// tenant CRUD sentinel errors surfaced by the admin handlers.
var (
	ErrTenantExists     = errors.New("assistant: tenant key already exists")
	ErrTenantNotFound   = errors.New("assistant: tenant not found")
	ErrInvalidTenantKey = errors.New("assistant: invalid tenant key (want ^[a-z0-9][a-z0-9._-]{0,63}$)")
)

// tenantRegistry is the in-process cache of registered tenants.
// Programmatically registered tenants and Mongo-loaded tenants both
// land here; per-request resolution never touches the DB.
type tenantRegistry struct {
	mu    sync.RWMutex
	byKey map[string]*Tenant
}

func newTenantRegistry() *tenantRegistry {
	return &tenantRegistry{byKey: map[string]*Tenant{}}
}

func (r *tenantRegistry) get(key string) *Tenant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byKey[key]
}

func (r *tenantRegistry) put(t *Tenant) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey[t.Key] = t
}

func (r *tenantRegistry) delete(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byKey, key)
}

func (r *tenantRegistry) list() []*Tenant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Tenant, 0, len(r.byKey))
	for _, t := range r.byKey {
		out = append(out, t)
	}
	return out
}

// defaultTenant synthesises the env-configured tenant. Never stored,
// never listed — it IS the v0.2.x single-tenant behaviour.
func (m *Module) defaultTenant() *Tenant {
	return &Tenant{
		Key:          "",
		Name:         "Default",
		DeploymentID: m.cfg.DeploymentID,
		FlowID:       m.cfg.FlowID,
		DocsIndex:    m.cfg.DocsIndex,
	}
}

// tenantForRequest resolves the tenant a request belongs to.
// Resolution order:
//
//  1. Explicit key — header X-Assistant-Site, then query ?site=.
//  2. TenantResolver seam — first resolver returning a non-empty key.
//  3. Default tenant (env config) — exactly v0.2.4 behaviour.
//
// Unknown or disabled keys fall back to the default tenant (with a
// warn log) rather than erroring: a typo'd widget embed should not
// kill chat, and pre-multi-tenant clients never send a key at all.
// The returned key is the *effective* key ("" for default).
func (m *Module) tenantForRequest(r *http.Request) *Tenant {
	key := strings.TrimSpace(r.Header.Get(TenantHeader))
	if key == "" {
		key = strings.TrimSpace(r.URL.Query().Get(TenantQueryParam))
	}
	if key == "" {
		for _, res := range m.tenantResolvers {
			if k := strings.TrimSpace(res.ResolveTenant(r)); k != "" {
				key = k
				break
			}
		}
	}
	if key == "" {
		return m.defaultTenant()
	}
	if t := m.tenantReg.get(strings.ToLower(key)); t != nil && !t.Disabled {
		return t
	}
	if m.logger != nil {
		m.logger.Warn("assistant: unknown or disabled tenant key, using default tenant",
			zap.String("key", key))
	}
	return m.defaultTenant()
}

// deploymentIDFor returns the flowexec deployment id a tenant's chats
// resolve through.
func (m *Module) deploymentIDFor(t *Tenant) string {
	if t == nil || t.Key == "" {
		return m.cfg.DeploymentID
	}
	if t.DeploymentID != "" {
		return t.DeploymentID
	}
	return m.cfg.DeploymentID + "--" + t.Key
}

// flowIDFor returns the flow id the tenant's auto-created deployment
// binds its stable variant to.
func (m *Module) flowIDFor(t *Tenant) string {
	if t == nil || t.Key == "" {
		return m.cfg.FlowID
	}
	if t.FlowID != "" {
		return t.FlowID
	}
	return m.cfg.FlowID + "--" + t.Key
}

// docsIndexFor returns the tenant's corpus/index name, inheriting the
// env default when unset.
func (m *Module) docsIndexFor(t *Tenant) string {
	if t != nil && t.DocsIndex != "" {
		return t.DocsIndex
	}
	return m.cfg.DocsIndex
}

// handoffEnabledFor / anonymousAllowedFor apply the tenant's optional
// overrides on top of the env defaults.
func (m *Module) handoffEnabledFor(t *Tenant) bool {
	if t != nil && t.HandoffEnabled != nil {
		return *t.HandoffEnabled
	}
	return m.cfg.HandoffEnabled
}

func (m *Module) anonymousAllowedFor(t *Tenant) bool {
	if t != nil && t.AnonymousAllowed != nil {
		return *t.AnonymousAllowed
	}
	return m.cfg.AnonymousAllowed
}

// RegisterTenant adds (or replaces) a tenant in the in-process
// registry without persisting it — the programmatic-registration
// path for embedded / single-binary hosts that configure tenants in
// code. Also ensures the tenant's flowexec deployment exists so the
// first chat can resolve a variant.
func (m *Module) RegisterTenant(ctx context.Context, t Tenant) error {
	t.Key = strings.ToLower(strings.TrimSpace(t.Key))
	if !tenantKeyRe.MatchString(t.Key) {
		return ErrInvalidTenantKey
	}
	m.tenantReg.put(&t)
	return m.ensureTenantDeployment(ctx, &t)
}

// Tenants lists every registered tenant (persisted + programmatic).
// The default tenant is not included — it is implicit.
func (m *Module) Tenants() []*Tenant { return m.tenantReg.list() }

// TenantByKey returns a registered tenant, or nil. "" returns nil —
// callers wanting the default tenant already have the env config.
func (m *Module) TenantByKey(key string) *Tenant { return m.tenantReg.get(key) }

// ensureTenantDeployment mirrors ensureDeployment for a non-default
// tenant: a deployment named deploymentIDFor(t) with a single stable
// variant pointing at flowIDFor(t). The flow itself is NOT seeded —
// the operator publishes it via Studio / flowexec; until then, chats
// against the tenant answer 409 like any unpublished deployment.
func (m *Module) ensureTenantDeployment(ctx context.Context, t *Tenant) error {
	if t == nil || t.Key == "" || m.flowexec == nil {
		return nil
	}
	name := t.Name
	if name == "" {
		name = "Assistant (" + t.Key + ")"
	}
	_, err := flowstore.EnsureDeployment(ctx, m.flowexec.Store(), flowstore.CreateDeploymentInput{
		ID:          m.deploymentIDFor(t),
		Name:        name,
		Description: fmt.Sprintf("Auto-created deployment for assistant tenant %q.", t.Key),
		Variants: []flowstore.FlowDeploymentVariant{
			{Label: "stable", FlowID: m.flowIDFor(t), Weight: 100},
		},
		StickyBy: flowstore.StickySession,
		Labels:   map[string]string{"source": "assistant-module-tenant", "tenant": t.Key},
	})
	return err
}
