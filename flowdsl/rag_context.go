package flowdsl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redelay/go-ai/llm"
	"github.com/redelay/go-flowdsl/runtime"
	"github.com/redelay/go-modules/search"
)

// RagContextServiceProvider yields the active search service. Kept as a
// function so tests can inject a stub without touching the package
// global. In production Register uses search.Current.
type RagContextServiceProvider func() *search.Service

// RegisterRagContext wires the redelay/assistant-rag-context handler
// onto engine. Safe to call multiple times — last registration wins
// per engine contract.
//
// Intentionally separate from Register(handoff-request) so projects
// that only want one of the two nodes don't get the other along for
// the ride.
func RegisterRagContext(engine *runtime.Engine) {
	if engine == nil {
		return
	}
	engine.RegisterHandler("redelay/assistant-rag-context", ragContextHandler(search.Current))
}

// RegisterRagContextWith accepts an explicit service lookup — used by
// tests that stand up their own search service or want to verify the
// error-path (service unavailable).
func RegisterRagContextWith(engine *runtime.Engine, lookup RagContextServiceProvider) {
	if engine == nil {
		return
	}
	engine.RegisterHandler("redelay/assistant-rag-context", ragContextHandler(lookup))
}

// ragContextHandler is the redelay/assistant-rag-context node. It
// extracts the last user message from step.Input["messages"], searches
// the configured docs index for the top-K most relevant chunks, and
// prepends a fresh `role: system` message stitching them into the
// conversation so downstream llm-chat calls answer from project docs
// instead of the model's priors. The hits are also surfaced on
// step.Output["sources"] so the SSE layer can forward them as
// citations.
//
// Design notes:
//   - All per-node tuning lives in step.Node.Config (index, topK,
//     preamble). No env vars here — config-as-data keeps the flow
//     diffable and trivial to A/B.
//   - The node is a no-op pass-through when the search service isn't
//     ready (module not booted, Qdrant down) — the assistant degrades
//     to the non-RAG path instead of erroring the run. Admins can opt
//     into strict mode with `failOnSearchError: true`.
func ragContextHandler(lookup RagContextServiceProvider) runtime.NodeHandler {
	return func(ctx context.Context, step *runtime.Step) error {
		cfg := map[string]any{}
		if step.Node != nil && step.Node.Config != nil {
			cfg = step.Node.Config
		}
		index, _ := cfg["index"].(string)
		if index == "" {
			index = "redelay_docs"
		}
		// Default 3. Older runs used 5–6; we lowered this after a
		// perf audit showed the main chat spent ~60% of its prompt
		// tokens on low-rank hits that never got cited. With bge-m3 +
		// 500-token chunks, 3 covers all the "Redelay has X" factual
		// questions this assistant fields — raise per-flow only when
		// a template actually needs broader context.
		topK := 3
		if n, ok := toInt(cfg["topK"]); ok && n > 0 {
			topK = n
		}
		preamble, _ := cfg["preamble"].(string)
		if preamble == "" {
			preamble = defaultRagPreamble
		}
		strict, _ := cfg["failOnSearchError"].(bool)
		// queryKey lets an upstream node (typically a small llm-chat
		// that rewrote the user's question into a search-friendly form)
		// supply a dedicated query string instead of us extracting it
		// from the messages array. When set and present on the input
		// packet, that value takes precedence over the last-user-msg
		// fallback. Absent → behaves as before.
		queryKey, _ := cfg["queryKey"].(string)

		// Pass every input field through untouched first so the rest
		// of the flow (llm-chat expecting `messages`, guard nodes
		// expecting `content`, etc.) keeps working even if search
		// goes sideways.
		if step.Output == nil {
			step.Output = map[string]any{}
		}
		for k, v := range step.Input {
			step.Output[k] = v
		}

		messages, _ := step.Input["messages"].([]any)
		// Resolve the search query. Priority:
		//   1. queryKey setting points at a rewritten query on the
		//      input packet (set by a preceding query-rewrite node).
		//   2. Fall back to the last user message in `messages[]`.
		// When neither yields text, skip retrieval with a noted reason.
		var query string
		if queryKey != "" {
			if v, ok := step.Input[queryKey].(string); ok {
				query = strings.TrimSpace(v)
			}
		}
		if query == "" {
			query = extractLastUserMessage(messages)
		}
		if query == "" {
			step.Output["sources"] = []any{}
			step.Output["ragSkipped"] = "no user message to ground"
			return nil
		}

		svc := lookup()
		if svc == nil {
			if strict {
				return errors.New("assistant-rag-context: search service not ready")
			}
			step.Output["sources"] = []any{}
			step.Output["ragSkipped"] = "search service not ready"
			return nil
		}

		// Attach flow-correlation IDs so the embed row written by
		// search.Service → llm.Embed carries flowId/runId/nodeId.
		// The search module adds {module, purpose, index} labels on
		// top; WithUsageContext merges them — both slices survive.
		// Without this every rag-context embed lands in /ai/breakdowns
		// as an unattributable row, which defeats the dashboard's
		// "which flow is costing me what" filter.
		usageCtx := llm.UsageContext{
			RunID:  step.ExecutionID,
			NodeID: step.Node.ID,
		}
		if step.Workflow != nil {
			usageCtx.FlowID = step.Workflow.ID
		}
		ctx = llm.WithUsageContext(ctx, usageCtx)

		hits, err := svc.Search(ctx, index, search.Query{
			Text:  query,
			Limit: topK,
		})
		if err != nil {
			if strict {
				return fmt.Errorf("assistant-rag-context: search: %w", err)
			}
			step.Output["sources"] = []any{}
			step.Output["ragSkipped"] = fmt.Sprintf("search error: %v", err)
			return nil
		}
		if len(hits) == 0 {
			step.Output["sources"] = []any{}
			step.Output["ragSkipped"] = "no hits"
			return nil
		}

		systemMsg := buildRagSystemMessage(preamble, hits)
		step.Output["messages"] = prependSystemMessage(messages, systemMsg)
		step.Output["sources"] = hitsToSources(hits)
		// Propagate any `action:` frontmatter the retrieved docs
		// carried — deduped by type + id so the same suggestion
		// doesn't appear twice when multiple chunks of the same
		// page rank. Empty slice is fine (downstream omits the
		// field). Source stamped to "rag" so analytics can split
		// retrieval-triggered actions from classifier/tool ones.
		step.Output["actions"] = hitsToActions(hits)
		return nil
	}
}

// defaultRagPreamble seeds the context block when the node has no
// `preamble` setting. The wording intentionally biases the LLM toward
// citing the retrieved chunks and away from filling gaps with priors —
// the most common failure mode we've observed.
const defaultRagPreamble = "You are answering questions about the Redelay project. " +
	"Ground every answer in the documentation excerpts below. " +
	"If the excerpts do not cover the question, say you don't know — do not guess. " +
	"Prefer quoting short phrases from the excerpts over paraphrasing."

// extractLastUserMessage walks the messages array backwards and returns
// the content of the most recent role=user turn. Assistant and system
// messages are ignored so follow-up questions reliably drive the
// search.
func extractLastUserMessage(messages []any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "user" {
			continue
		}
		if c, ok := m["content"].(string); ok && c != "" {
			return c
		}
	}
	return ""
}

// buildRagSystemMessage renders the retrieved chunks into a single
// system-message string. Kept deliberately simple — a numbered list
// with source paths keeps token count predictable and gives the model
// a clear anchor to cite.
func buildRagSystemMessage(preamble string, hits []search.Hit) map[string]any {
	var b strings.Builder
	b.WriteString(preamble)
	b.WriteString("\n\n## Documentation excerpts\n\n")
	for i, h := range hits {
		path, _ := h.Payload["path"].(string)
		text, _ := h.Payload["text"].(string)
		fmt.Fprintf(&b, "### [%d] %s\n%s\n\n", i+1, firstNonEmpty(path, h.ID), text)
	}
	return map[string]any{
		"role":    "system",
		"content": b.String(),
	}
}

// prependSystemMessage returns a new slice with `systemMsg` at the
// front. Doesn't mutate the caller's slice — avoids surprising anyone
// else downstream that holds a reference to the original messages.
func prependSystemMessage(messages []any, systemMsg map[string]any) []any {
	out := make([]any, 0, len(messages)+1)
	out = append(out, systemMsg)
	out = append(out, messages...)
	return out
}

// hitsToSources projects each Hit into a compact citation object the
// UI can render without knowing about Qdrant specifics. `url` is
// promoted from payload when present so clicks can open the live doc;
// otherwise `path` is enough to copy-paste into a code editor.
func hitsToSources(hits []search.Hit) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		s := map[string]any{
			"id":    h.ID,
			"score": h.Score,
		}
		if p, ok := h.Payload["path"].(string); ok && p != "" {
			s["path"] = p
		}
		if u, ok := h.Payload["url"].(string); ok && u != "" {
			s["url"] = u
		}
		if t, ok := h.Payload["title"].(string); ok && t != "" {
			s["title"] = t
		}
		out = append(out, s)
	}
	return out
}

// hitsToActions scans retrieved chunks for a `payload.action` block
// (stamped at ingest time from docs frontmatter) and normalises them
// into the Action wire shape. Dedups by (type, id) so a page split
// into multiple ranked chunks yields exactly one button.
//
// Returns an empty slice (not nil) when no hits carry actions — lets
// the SSE layer unconditionally forward the field; JSON omitempty
// still drops it from the frame when empty.
//
// Each action gets source=rag stamped so admin analytics can split
// retrieval-driven suggestions from intent/tool ones. If a chunk
// specifies its own source, we respect it (multi-stage flows).
func hitsToActions(hits []search.Hit) []map[string]any {
	out := make([]map[string]any, 0, 2)
	seen := make(map[string]struct{}, 4)
	for _, h := range hits {
		raw, ok := h.Payload["action"]
		if !ok {
			continue
		}
		a, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := a["type"].(string)
		if typ == "" {
			continue
		}
		id, _ := a["id"].(string)
		key := typ + "\x00" + id
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		// Shallow copy so we don't mutate the Qdrant payload cache
		// that may back subsequent retrievals on the same process.
		action := make(map[string]any, len(a)+1)
		for k, v := range a {
			action[k] = v
		}
		if _, has := action["source"]; !has {
			action["source"] = "rag"
		}
		out = append(out, action)
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}
