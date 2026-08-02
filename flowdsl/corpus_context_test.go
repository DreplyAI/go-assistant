package flowdsl

import (
	"context"
	"strings"
	"testing"

	"github.com/redelay/go-flowdsl/ir"
	"github.com/redelay/go-flowdsl/runtime"
)

// Everything here is a way the composable pair could quietly behave like the
// monolithic node it exists to replace — which would make splitting it
// pointless while still looking correct in a trace.

func ground(cfg map[string]any, in map[string]any) map[string]any {
	step := &runtime.Step{Node: &ir.Node{Config: cfg}, Input: in}
	if err := corpusContextHandler(context.Background(), step); err != nil {
		panic(err)
	}
	return step.Output
}

func card(id, title, text string) map[string]any {
	return map[string]any{"id": id, "title": title, "text": text}
}

func evidence(cards ...map[string]any) map[string]any {
	hits := make([]any, len(cards))
	for i, c := range cards {
		hits[i] = c
	}
	return map[string]any{"asked": true, "hits": hits}
}

func TestGroundsMessagesFromEvidence(t *testing.T) {
	out := ground(nil, map[string]any{
		"evidence": evidence(card("c1", "Creating a module", "Modules register via init().")),
		"messages": []any{map[string]any{"role": "user", "content": "how do I make a module"}},
	})

	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected the user turn plus one system message, got %d", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" {
		t.Errorf("grounding must come first and be a system turn, got role=%v", sys["role"])
	}
	content, _ := sys["content"].(string)
	if !strings.Contains(content, "Modules register via init().") {
		t.Error("the excerpt text is the entire point and is missing")
	}
	if !strings.Contains(content, "[1]") {
		t.Error("numbered anchors let the model cite something reproducible")
	}
	if out["grounded"] != true {
		t.Error("grounded must record that something was injected")
	}
}

func TestEmptyEvidenceLeavesMessagesAlone(t *testing.T) {
	// Injecting an empty excerpt section reads to the model as "the docs
	// contain nothing on this", which is a far stronger claim than "we
	// retrieved nothing" — and it is the claim a user would repeat.
	original := []any{map[string]any{"role": "user", "content": "hi"}}
	out := ground(nil, map[string]any{
		"evidence": map[string]any{"asked": false, "reason": "no query"},
		"messages": original,
	})

	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages were rewritten with nothing to say: %d turns", len(msgs))
	}
	if out["grounded"] != false {
		t.Error("grounded=false is how a trace shows the answer was ungrounded")
	}
}

func TestSourcesAreMergedNotReplaced(t *testing.T) {
	// THE reason this node exists as a separate one. Two retrieve nodes feeding
	// one prompt is the composable shape; replacing `sources` would silently
	// keep only the last corpus's cards, and the UI would show half the
	// citations the answer was actually built from.
	out := ground(
		map[string]any{"inputKeys": []any{"papers", "docs"}},
		map[string]any{
			"papers":  evidence(card("p1", "Creatine review", "…")),
			"docs":    evidence(card("d1", "Modules", "…")),
			"sources": []any{map[string]any{"id": "earlier", "title": "From a prior stage"}},
		},
	)

	srcs, _ := out["sources"].([]any)
	if len(srcs) != 3 {
		t.Fatalf("expected prior + both corpora = 3 sources, got %d", len(srcs))
	}
	ids := map[string]bool{}
	for _, s := range srcs {
		ids[cardStr(s.(map[string]any), "id")] = true
	}
	for _, want := range []string{"earlier", "p1", "d1"} {
		if !ids[want] {
			t.Errorf("source %q was dropped", want)
		}
	}
}

func TestDuplicateSourcesAreCollapsed(t *testing.T) {
	// Two corpora can legally return the same document, and a user seeing one
	// citation twice reads it as two independent confirmations.
	out := ground(
		map[string]any{"inputKeys": []any{"a", "b"}},
		map[string]any{
			"a": evidence(card("same", "One doc", "…")),
			"b": evidence(card("same", "One doc", "…")),
		},
	)
	if srcs, _ := out["sources"].([]any); len(srcs) != 1 {
		t.Errorf("expected 1 deduped source, got %d", len(srcs))
	}
}

func TestActionsAreDedupedByTypeAndID(t *testing.T) {
	// One page split across ranked chunks yields several hits carrying the same
	// action. Three identical buttons read as three different things.
	act := map[string]any{"type": "open", "id": "42", "label": "Open the guide"}
	out := ground(nil, map[string]any{
		"evidence": evidence(
			map[string]any{"id": "c1", "title": "A", "action": act},
			map[string]any{"id": "c2", "title": "B", "action": act},
		),
	})
	acts, _ := out["actions"].([]any)
	if len(acts) != 1 {
		t.Fatalf("expected 1 action, got %d", len(acts))
	}
	if a, _ := acts[0].(map[string]any); a["source"] != "corpus" {
		t.Errorf("actions must be attributable to where they came from, got %v", a["source"])
	}
}

func TestUnrelatedPacketKeysSurvive(t *testing.T) {
	// A grounding node that drops the packet breaks every stage after it. The
	// older node passes through too; this asserts the split kept that.
	out := ground(nil, map[string]any{
		"evidence":  evidence(card("c1", "T", "…")),
		"sessionId": "s-1",
		"locale":    "pl",
	})
	if out["sessionId"] != "s-1" || out["locale"] != "pl" {
		t.Error("downstream stages lost their packet")
	}
}

func TestMalformedEvidenceDoesNotFailTheRun(t *testing.T) {
	// The packet crosses a JSON boundary; one odd entry must not abort a chat.
	out := ground(nil, map[string]any{
		"evidence": map[string]any{"hits": []any{"not a card", 7, card("ok", "Fine", "text")}},
	})
	if out["grounded"] != true {
		t.Fatal("the one usable card should still have grounded the answer")
	}
	if srcs, _ := out["sources"].([]any); len(srcs) != 1 {
		t.Errorf("expected the single usable card, got %d", len(srcs))
	}
}

func TestConfigBoolDefaultsWhenAbsent(t *testing.T) {
	// `emitSources` defaults to true. Reading a missing key as false — which a
	// plain truthy(nil) does — would disable it for every flow that did not
	// spell it out, and the symptom is citations vanishing from the UI while
	// the answer stays grounded.
	if !configBool(map[string]any{}, "emitSources", true) {
		t.Error("absent must mean the default, not false")
	}
	if configBool(map[string]any{"emitSources": false}, "emitSources", true) {
		t.Error("an explicit false must win over the default")
	}
}
