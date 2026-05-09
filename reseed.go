package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	flowstore "github.com/redelay/go-flowdsl/flowexec/store"
	"github.com/redelay/go-flowdsl/ir"
	"go.uber.org/zap"
)

// errUnknownTemplate is returned by reseedFromTemplate when the
// caller asks for a template this module does not ship. The HTTP
// handler detects it via errors.Is and responds 400 rather than 500.
var errUnknownTemplate = errors.New("assistant: unknown embedded template")

// embeddedTemplateBytes returns the raw JSON for one of the shipped
// templates. Two layers, in order:
//
//  1. Kernel templates: the seven generic shapes (minimal, default,
//     production, default-stream, multi-guard, echo,
//     handoff-notification) baked into this binary at build time.
//
//  2. Extension templates: project knowledge packs (e.g. RAG variants
//     keyed to a specific docs index) registered via TemplateProvider
//     and discovered at Configure time. Walked in registration order;
//     first match wins.
//
// Returns (nil, false) when the name matches neither layer — the
// caller (reset endpoint, CLI) translates that to a 400 / "unknown
// template" error.
func (m *Module) embeddedTemplateBytes(name string) ([]byte, bool) {
	switch name {
	case "", "default":
		return defaultFlowJSON, true
	case "minimal":
		return minimalFlowJSON, true
	case "production":
		return productionFlowJSON, true
	case "default-stream":
		return defaultStreamFlowJSON, true
	case "multi-guard":
		return multiGuardFlowJSON, true
	case "echo":
		return echoFlowJSON, true
	case "handoff-notification":
		return handoffNotificationFlowJSON, true
	}
	for _, p := range m.templateProviders {
		if raw, ok := p.AssistantEmbeddedTemplates()[name]; ok {
			return raw, true
		}
	}
	return nil, false
}

// reseedFromTemplate saves a new version of the assistant flow from
// the named embedded template and publishes it. Version history is
// preserved — rollback via /flows/{id}/rollback stays possible.
//
// This bypasses the schema-version auto-upgrade gate because the
// caller has explicitly asked for the embedded shape. Env-var
// overrides for Provider/Model still apply so the reseeded flow
// respects per-deployment configuration.
func (m *Module) reseedFromTemplate(ctx context.Context, name string) error {
	raw, ok := m.embeddedTemplateBytes(name)
	if !ok {
		return errUnknownTemplate
	}
	var doc ir.Workflow
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("assistant: parse embedded %q: %w", name, err)
	}

	// Ensure the flow exists — the reset endpoint must work even on
	// fresh deployments before the first Startup seed.
	store := m.flowexec.Store()
	existing, err := store.GetFlow(ctx, m.cfg.FlowID)
	if err != nil && !errors.Is(err, flowstore.ErrNotFound) {
		return fmt.Errorf("assistant: get flow: %w", err)
	}
	if existing == nil {
		if _, err := store.CreateFlow(ctx, flowstore.CreateFlowInput{
			ID:          m.cfg.FlowID,
			Name:        "Project Assistant",
			Description: "Assistant flow reseeded via /assistant/reset.",
			Labels:      map[string]string{"owner": "assistant"},
		}); err != nil {
			return fmt.Errorf("assistant: create flow: %w", err)
		}
	}

	applyProviderEnvOverrides(&doc, chatNodeID, m.cfg.Provider, m.cfg.Model)

	schemaV := documentSchemaVersion(&doc)
	v, err := store.SaveVersion(ctx, flowstore.SaveVersionInput{
		FlowID:    m.cfg.FlowID,
		Document:  &doc,
		Note:      fmt.Sprintf("reseeded from embedded %q (schemaVersion=%d)", name, schemaV),
		CreatedBy: "assistant-module",
		Labels: map[string]string{
			"source":        "embedded-" + name,
			"schemaVersion": fmt.Sprintf("%d", schemaV),
			"template":      name,
		},
	})
	if err != nil {
		return fmt.Errorf("assistant: save reseed version: %w", err)
	}
	if _, err := store.PublishVersion(ctx, m.cfg.FlowID, v.ID); err != nil {
		return fmt.Errorf("assistant: publish reseed version: %w", err)
	}
	m.logger.Info("assistant: reseeded + published template",
		zap.String("template", name),
		zap.String("versionID", v.ID),
		zap.Int("schemaVersion", schemaV))
	return nil
}

// applyProviderEnvOverrides mutates the chat node's Config to honour
// ASSISTANT_PROVIDER / ASSISTANT_MODEL. Kept as a standalone function
// so both ensureFlow (Startup) and reseedFromTemplate (runtime) share
// the behaviour without copying the loop.
func applyProviderEnvOverrides(doc *ir.Workflow, chatNodeID, provider, model string) {
	if provider == "" && model == "" {
		return
	}
	for i := range doc.Nodes {
		if doc.Nodes[i].ID != chatNodeID {
			continue
		}
		if doc.Nodes[i].Config == nil {
			doc.Nodes[i].Config = make(map[string]any)
		}
		if provider != "" {
			doc.Nodes[i].Config["providerID"] = provider
		}
		if model != "" {
			doc.Nodes[i].Config["model"] = model
		}
		return
	}
}
