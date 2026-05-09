package flowdsl

import (
	"context"
	"strings"
	"testing"

	"github.com/redelay/go-flowdsl/ir"
	"github.com/redelay/go-flowdsl/runtime"
	"github.com/redelay/go-modules/search"
)

// TestRagContext_NoUserMessage ensures the node never crashes on a
// packet whose messages array has only system / assistant entries —
// it should flag the skip reason and move on.
func TestRagContext_NoUserMessage(t *testing.T) {
	handler := ragContextHandler(func() *search.Service { return nil })
	step := &runtime.Step{
		Node: &ir.Node{Config: map[string]any{}},
		Input: map[string]any{
			"messages": []any{
				map[string]any{"role": "assistant", "content": "hello"},
			},
		},
	}
	if err := handler(context.Background(), step); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if step.Output["ragSkipped"] != "no user message to ground" {
		t.Errorf("expected skip reason, got %v", step.Output["ragSkipped"])
	}
	if sources, _ := step.Output["sources"].([]map[string]any); sources != nil && len(sources) != 0 {
		t.Errorf("expected empty sources, got %v", sources)
	}
}

// TestRagContext_NoService — search service not booted: node should
// NOT fail the run by default, and should set ragSkipped explaining
// why. strict=true should fail instead.
func TestRagContext_NoService(t *testing.T) {
	handler := ragContextHandler(func() *search.Service { return nil })

	t.Run("default fail-open", func(t *testing.T) {
		step := &runtime.Step{
			Node: &ir.Node{Config: map[string]any{}},
			Input: map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "what is redelay"}},
			},
		}
		if err := handler(context.Background(), step); err != nil {
			t.Fatalf("fail-open should not error: %v", err)
		}
		if step.Output["ragSkipped"] != "search service not ready" {
			t.Errorf("expected skip reason, got %v", step.Output["ragSkipped"])
		}
	})

	t.Run("failOnSearchError=true bubbles up", func(t *testing.T) {
		step := &runtime.Step{
			Node: &ir.Node{Config: map[string]any{"failOnSearchError": true}},
			Input: map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "what is redelay"}},
			},
		}
		err := handler(context.Background(), step)
		if err == nil || !strings.Contains(err.Error(), "search service not ready") {
			t.Fatalf("expected strict error, got %v", err)
		}
	})
}

// TestExtractLastUserMessage walks back past non-user turns — we
// ground against the user's latest question, not the assistant's
// last reply.
func TestExtractLastUserMessage(t *testing.T) {
	got := extractLastUserMessage([]any{
		map[string]any{"role": "system", "content": "you are helpful"},
		map[string]any{"role": "user", "content": "what is redelay"},
		map[string]any{"role": "assistant", "content": "it's a framework"},
		map[string]any{"role": "user", "content": "is it PHP?"},
	})
	if got != "is it PHP?" {
		t.Errorf("expected latest user msg, got %q", got)
	}
}

// TestBuildRagSystemMessage — the stitched prompt must (a) start with
// the preamble, (b) enumerate each hit with its path, and (c) include
// the chunk body text so the LLM has actual content to cite.
func TestBuildRagSystemMessage(t *testing.T) {
	msg := buildRagSystemMessage("PREAMBLE", []search.Hit{
		{ID: "a", Payload: map[string]any{"path": "docs/intro.md", "text": "Redelay is a Go framework."}},
		{ID: "b", Payload: map[string]any{"path": "docs/arch.md", "text": "Modules are plug-ins."}},
	})
	role, _ := msg["role"].(string)
	if role != "system" {
		t.Errorf("role = %q, want system", role)
	}
	body, _ := msg["content"].(string)
	for _, want := range []string{
		"PREAMBLE",
		"[1] docs/intro.md",
		"Redelay is a Go framework.",
		"[2] docs/arch.md",
		"Modules are plug-ins.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stitched system prompt missing %q; got:\n%s", want, body)
		}
	}
}

// TestPrependSystemMessage_DoesNotMutateInput proves the handler
// doesn't accidentally share state between the incoming packet and
// the outgoing one — a real concern because the chat SSE layer holds
// onto the input slice after the step returns.
func TestPrependSystemMessage_DoesNotMutateInput(t *testing.T) {
	original := []any{
		map[string]any{"role": "user", "content": "hi"},
	}
	result := prependSystemMessage(original, map[string]any{"role": "system", "content": "x"})
	if len(original) != 1 {
		t.Fatalf("original mutated: len=%d", len(original))
	}
	if len(result) != 2 {
		t.Fatalf("result len = %d, want 2", len(result))
	}
}

