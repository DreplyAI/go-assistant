package flowdsl

import (
	"context"
	"fmt"
	"strings"

	"github.com/redelay/go-flowdsl/runtime"
)

// CorpusContextActionRef is the node id.
const CorpusContextActionRef = "redelay/assistant-corpus-context"

// The grounding half of RAG, separated from the retrieval half.
//
// `assistant-rag-context` does both in one node, and that is why it cannot be
// composed: it searches, rewrites `messages`, and overwrites `sources` and
// `actions`, so a flow that wants its own prompt cannot use it, and a flow that
// wants a different corpus cannot reuse its prompt building. Splitting them
// gives three usable shapes instead of one:
//
//	corpus-retrieve → corpus-context → llm-chat    the easy path, this pair
//	corpus-retrieve → <your own node> → llm-chat   compose your own prompt
//	corpus-retrieve ×2 → corpus-context            two corpora, one prompt
//
// This node reads an evidence packet — whatever `corpus-retrieve` wrote — and
// does nothing else. It never searches, so it cannot fail on an unavailable
// index, and a flow can run it against evidence assembled by hand.
//
// It is careful in one respect the older node is not: it MERGES into `sources`
// rather than replacing it. Two retrieve nodes feeding one context node is the
// whole point of splitting them, and an overwrite would silently keep only the
// last corpus's cards.
func RegisterCorpusContext(engine *runtime.Engine) {
	if engine == nil {
		return
	}
	engine.RegisterHandler(CorpusContextActionRef, corpusContextHandler)
}

// Settings:
//
//	inputKeys   evidence packet keys to read, in order. Default ["evidence"].
//	preamble    text before the excerpts. Without one the model gets a bare
//	            list of quotes and no instruction about what to do with them.
//	heading     section heading for the excerpt block. Default per below.
//	emitSources merge citation cards into `sources`. Default true.
//	emitActions lift `action` blocks out of citation Extra into `actions`.
//	            Default true.
//	skipEmpty   when no evidence survived the floor, leave `messages` alone
//	            rather than injecting an empty excerpt section. Default true.
func corpusContextHandler(_ context.Context, step *runtime.Step) error {
	cfg := step.Node.Config
	in := step.Input
	out := passthrough(in)

	keys := configStrings(cfg, "inputKeys")
	if len(keys) == 0 {
		keys = []string{"evidence"}
	}

	var cards []map[string]any
	for _, k := range keys {
		cards = append(cards, evidenceCards(in[k])...)
	}

	if len(cards) == 0 {
		// Nothing survived the floor. Injecting an empty "## Excerpts" section
		// is worse than injecting nothing: it reads to the model as "the
		// documentation contains nothing on this", which is a much stronger
		// claim than "we did not retrieve anything".
		if configBool(cfg, "skipEmpty", true) {
			out["grounded"] = false
			step.Output = out
			return nil
		}
	}

	if len(cards) > 0 {
		msgs, _ := in["messages"].([]any)
		out["messages"] = prependSystemMessage(msgs, map[string]any{
			"role": "system",
			"content": renderGrounding(
				configString(cfg, "preamble"),
				configString(cfg, "heading"),
				cards,
			),
		})
		if configBool(cfg, "emitSources", true) {
			// MERGE, not replace: a second retrieve node's cards must survive
			// the first one's. Deduped by id, because two corpora can legally
			// return the same document.
			//
			// `text` is stripped on the way out. It is the grounding copy, and
			// for at least one corpus (papers) it is licensed for the model and
			// not for the screen. `sources` is what the UI renders, so the text
			// must not be in it — and stripping here means no consumer has to
			// know which corpora that applies to.
			out["sources"] = mergeSources(in["sources"], displayOnly(cards))
		}
		if configBool(cfg, "emitActions", true) {
			if acts := cardActions(cards); len(acts) > 0 {
				out["actions"] = mergeActions(in["actions"], acts)
			}
		}
	}
	out["grounded"] = len(cards) > 0
	step.Output = out
	return nil
}

const defaultGroundingHeading = "Reference excerpts"

// renderGrounding builds the system message.
//
// A numbered list keeps the token count predictable and gives the model an
// anchor to cite — "[2]" is something it can reproduce, a paraphrased title is
// not. The title line carries the URL when there is one, so a model that names
// a source names something the user can open.
func renderGrounding(preamble, heading string, cards []map[string]any) string {
	var b strings.Builder
	if preamble == "" {
		preamble = "Answer using the excerpts below. If they do not contain the " +
			"answer, say so plainly rather than filling the gap from memory."
	}
	b.WriteString(preamble)
	b.WriteString("\n\n## ")
	if heading == "" {
		heading = defaultGroundingHeading
	}
	b.WriteString(heading)
	b.WriteString("\n\n")

	for i, c := range cards {
		label := cardStr(c, "title")
		if label == "" {
			label = firstNonEmpty(cardStr(c, "path"), firstNonEmpty(cardStr(c, "url"), cardStr(c, "id")))
		}
		fmt.Fprintf(&b, "### [%d] %s\n", i+1, label)
		if u := cardStr(c, "url"); u != "" {
			fmt.Fprintf(&b, "%s\n", u)
		}
		// `snippet` is absent from ToCard by design — a card is what the UI
		// renders, and for some corpora the text is licensed for grounding but
		// not for display. `text` is the grounding copy, carried separately.
		if t := firstNonEmpty(cardStr(c, "text"), cardStr(c, "snippet")); t != "" {
			fmt.Fprintf(&b, "%s\n", t)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// evidenceCards pulls the hit list out of whatever shape the packet key holds.
//
// Tolerant on purpose: the evidence packet crosses a JSON boundary, so a `[]any`
// of `map[string]any` is what actually arrives even though the producer wrote a
// typed struct. A malformed entry is skipped rather than failing the run — a
// grounding node that aborts a chat because one card was odd is worse than one
// that grounds on the rest.
func evidenceCards(v any) []map[string]any {
	var raw []any
	switch t := v.(type) {
	case map[string]any:
		raw, _ = t["hits"].([]any)
	case []any:
		raw = t
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// displayOnly returns cards without the grounding text. See emitSources.
func displayOnly(cards []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(cards))
	for _, c := range cards {
		cp := make(map[string]any, len(c))
		for k, v := range c {
			if k == "text" || k == "snippet" {
				continue
			}
			cp[k] = v
		}
		out = append(out, cp)
	}
	return out
}

func mergeSources(existing any, cards []map[string]any) []any {
	seen := map[string]bool{}
	out := []any{}
	if prev, ok := existing.([]any); ok {
		for _, p := range prev {
			out = append(out, p)
			if m, ok := p.(map[string]any); ok {
				seen[cardStr(m, "id")] = true
			}
		}
	}
	for _, c := range cards {
		if id := cardStr(c, "id"); id == "" || !seen[id] {
			seen[id] = true
			out = append(out, c)
		}
	}
	return out
}

// cardActions lifts `action` blocks that rode along on a citation's Extra.
//
// Deduped by (type, id) so a page split across several ranked chunks yields one
// button, matching what rag-context already does — a user seeing the same action
// three times reads it as three different things.
func cardActions(cards []map[string]any) []any {
	seen := map[string]bool{}
	out := []any{}
	for _, c := range cards {
		a, ok := c["action"].(map[string]any)
		if !ok {
			continue
		}
		key := cardStr(a, "type") + "\x00" + cardStr(a, "id")
		if seen[key] {
			continue
		}
		seen[key] = true
		cp := map[string]any{}
		for k, v := range a {
			cp[k] = v
		}
		cp["source"] = "corpus"
		out = append(out, cp)
	}
	return out
}

func mergeActions(existing any, next []any) []any {
	out := []any{}
	if prev, ok := existing.([]any); ok {
		out = append(out, prev...)
	}
	return append(out, next...)
}

// cardStr reads a string field off a card. Local rather than reaching into
// corpus: this package depends on corpus, not the reverse, and a shared
// two-line helper is not worth widening either package's surface.
func cardStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func configStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

