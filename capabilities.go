package assistant

import (
	"context"

	"github.com/redelay/go-flowdsl/ir"
)

// deriveCapabilities inspects the variant this session would bucket
// into and reports which features that specific flow exposes. A/B
// users on a canary variant with different capabilities than the
// stable variant see the matching UI.
//
// Any lookup failure — missing deployment, no variant, no published
// version, doc parse error — returns a zero-valued capabilities
// object so the widget hides every optional affordance. Safer
// default than assuming a feature exists and surfacing a button
// that would 404.
func (m *Module) deriveCapabilities(ctx context.Context, meta map[string]any) AssistantCapabilities {
	if m.flowexec == nil {
		return AssistantCapabilities{}
	}
	resolved, err := m.resolveVariant(ctx, meta)
	if err != nil || resolved == nil || resolved.Version == nil || resolved.Version.Document == nil {
		return AssistantCapabilities{}
	}
	return capabilitiesFromWorkflow(resolved.Version.Document)
}

// capabilitiesFromWorkflow is the pure function; deriveCapabilities
// is the side-effecting lookup that calls it. Split so tests can
// exercise the detection logic without spinning up a store.
func capabilitiesFromWorkflow(doc *ir.Workflow) AssistantCapabilities {
	caps := AssistantCapabilities{}
	if doc == nil {
		return caps
	}
	for _, n := range doc.Nodes {
		if n == nil {
			continue
		}
		switch n.ActionRef {
		case "redelay/assistant-handoff-request":
			caps.Handoff = true
		case "redelay/email-send":
			// Any email-send node is treated as an escalation path.
			// Tighten later if we need to distinguish escalation from
			// transactional templates (e.g. via a label).
			caps.Escalation = true
		case "redelay/llm-chat":
			if stream, _ := n.Config["stream"].(bool); stream {
				caps.Streaming = true
			}
		}
	}
	return caps
}
