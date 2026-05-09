// Package admin exposes the assistant module's admin-only HTTP
// surface. Blank-import only from admin-api.
//
//	import _ "github.com/dreplyai/go-assistant/admin"
package admin

import (
	"context"
	"fmt"

	"github.com/dreplyai/go-assistant"
	"github.com/redelay/go-ai/ledger"
	flowexecmod "github.com/redelay/go-flowdsl/flowexec/module"
	"github.com/redelay/go-framework/modules"
)

func init() {
	modules.RegisterFactory("assistant-admin", func(deps modules.ModuleDeps) (modules.Module, error) {
		return &Module{IRBase: modules.MustLoadIR(moduleYAML)}, nil
	})
}

// Module wires admin routes against the core assistant module. It
// also holds references to sibling modules whose data we enrich the
// chat detail response with: flowexec (for flow name) and ledger
// (for per-chat LLM usage). Both are optional — chat detail still
// returns without them, just without the enrichment fields.
type Module struct {
	modules.IRBase
	asst     *assistant.Module
	flowexec *flowexecmod.Module
	ledger   *ledger.Module
}

// Startup satisfies modules.Module.
func (m *Module) Startup(_ context.Context) error { return nil }

// Shutdown satisfies modules.Module.
func (m *Module) Shutdown(_ context.Context) error { return nil }

// Configure discovers the core assistant module plus the sibling
// modules the detail endpoint enriches against. Missing optional
// modules are logged at DEBUG level (via the zap logger when the
// assistant exposes one) and the enrichment silently degrades.
func (m *Module) Configure(registry *modules.Registry) error {
	raw := registry.Get("assistant")
	if raw == nil {
		return fmt.Errorf("assistant-admin: assistant module not registered")
	}
	asst, ok := raw.(*assistant.Module)
	if !ok {
		return fmt.Errorf("assistant-admin: unexpected assistant module type %T", raw)
	}
	m.asst = asst

	// flowexec via its package-level accessor — same pattern the
	// assistant core module uses. Optional: if the binary doesn't
	// blank-import flowexec/module, we skip the flow-name lookup.
	m.flowexec = flowexecmod.Current()

	// ledger via registry walk. The module registers under "llm-ledger"
	// (see go-ai/ledger/module.go RegisterFactory). Optional: when the
	// ledger module isn't imported (e.g. a deployment that doesn't
	// track LLM usage), usage enrichment returns a zero summary.
	if raw := registry.Get("llm-ledger"); raw != nil {
		if lm, ok := raw.(*ledger.Module); ok {
			m.ledger = lm
		}
	}
	return nil
}

// PermissionDefinitions piggybacks on the shared admin gate.
func (m *Module) PermissionDefinitions() []modules.PermissionDefinition {
	return []modules.PermissionDefinition{
		{Permission: "admin:access", Name: "Admin Access", Description: "Access the admin panel", Module: "assistant-admin"},
	}
}
