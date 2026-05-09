package assistant

import (
	"encoding/json"

	"github.com/redelay/go-flowdsl/flowexec"
	"github.com/redelay/go-flowdsl/ir"
	"go.uber.org/zap"
)

// Templates implements flowexec.TemplateProvider so the Studio's
// "Create flow" modal — and the admin UI's /flows/templates page —
// can offer the assistant's curated flow shapes as starting points.
//
// Two layers:
//
//  1. The kernel's seven generic shapes — minimal, default,
//     default-stream, multi-guard, production, echo,
//     handoff-notification. Returned inline; each JSON is small and
//     already embedded.
//
//  2. Project knowledge-pack extensions (assistant.TemplateProvider)
//     discovered at Configure time — typically RAG-flavor flows that
//     reference the project's docs index. Appended after the kernel
//     templates.
//
// Template IDs use the "assistant/<name>" namespace for the kernel's
// own shapes and a project-scoped prefix (e.g. "assistant-redelay/rag")
// for extension shapes. Keep names aligned with the reseed endpoint
// keys — both surfaces consult embeddedTemplateBytes for the raw
// JSON, and an extension that ships a Studio template should also
// declare its short name in AssistantEmbeddedTemplates.
func (m *Module) Templates() []flowexec.FlowTemplate {
	specs := []struct {
		raw  []byte
		id   string
		name string
		desc string
		tags []string
	}{
		{minimalFlowJSON, "assistant/minimal",
			"Assistant — T1 Minimal (dev)",
			"Smallest possible assistant flow: start → LLM → end. No safety, no refusals, no handoff. Dev-only — use `default` or above for anything user-facing.",
			[]string{"ai", "chat", "dev"}},
		{defaultFlowJSON, "assistant/default",
			"Assistant — T2 Guarded (baseline)",
			"Guards both sides of the LLM, configurable refusal copy, human-handoff branch. Baseline for any production deployment.",
			[]string{"ai", "chat", "guards", "handoff"}},
		{productionFlowJSON, "assistant/production",
			"Assistant — T3 Guarded + Trust & Safety",
			"T2 plus trust-and-safety email fan-out on blocks and [[ESCALATE]] token detection for user-initiated handoff. Public-facing deployments.",
			[]string{"ai", "chat", "guards", "handoff", "production"}},
		{defaultStreamFlowJSON, "assistant/default-stream",
			"Assistant — T2s Guarded (streaming)",
			"Streaming variant of `default`. Lower time-to-first-token; guard-out sees the full draft after streaming ends.",
			[]string{"ai", "chat", "streaming"}},
		{multiGuardFlowJSON, "assistant/multi-guard",
			"Assistant — Defense in Depth",
			"Two Qwen3Guard classifiers on each side of the LLM (first fails open, second fails closed). For regulated / high-liability deployments.",
			[]string{"ai", "chat", "guards", "regulated"}},
		{echoFlowJSON, "assistant/echo",
			"Assistant — T0 Echo (dev-only)",
			"Zero-LLM flow. start → core/template-render → end. Echoes the user's message back. Use for CI smoke tests, UI dev, and cost-free demos.",
			[]string{"ai", "dev", "no-cost"}},
		{handoffNotificationFlowJSON, "assistant/handoff-notification",
			"Handoff → email",
			"Listens for assistant.handoff_requested events and emits email.send to the ops inbox. Pair with any tier that has `handoff` in the tags.",
			[]string{"assistant", "handoff", "email", "events"}},
	}

	out := make([]flowexec.FlowTemplate, 0, len(specs)+len(m.templateProviders)*2)
	for _, s := range specs {
		doc := decodeFlowJSON(s.raw, m.logger)
		if doc == nil {
			continue
		}
		out = append(out, flowexec.FlowTemplate{
			ID:          s.id,
			Name:        s.name,
			Description: s.desc,
			Source:      "assistant",
			Tags:        s.tags,
			Document:    doc,
		})
	}
	// Append project knowledge-pack templates. Extensions own their
	// own ID namespace + Source string — the kernel just stitches.
	for _, p := range m.templateProviders {
		out = append(out, p.AssistantFlowTemplates()...)
	}
	return out
}

// decodeFlowJSON parses an embedded flow JSON file into an ir.Workflow.
// Failures are logged and the template silently dropped — a malformed
// embed shouldn't crash the templates listing.
func decodeFlowJSON(raw []byte, logger *zap.Logger) *ir.Workflow {
	var doc ir.Workflow
	if err := json.Unmarshal(raw, &doc); err != nil {
		if logger != nil {
			logger.Warn("assistant: malformed embedded flow template", zap.Error(err))
		}
		return nil
	}
	return &doc
}
