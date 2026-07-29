package corpus

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCleanRewriteRefusesWhatWouldRetrieveTheWrongThing(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"creatine supplementation and strength", "creatine supplementation and strength"},
		{"  \"creatine supplementation\"  ", "creatine supplementation"},
		{"Query: creatine supplementation", "creatine supplementation"},
		{"Search query: \"creatine supplementation\"", "creatine supplementation"},
		{"creatine supplementation\nAlso, creatine is safe.", "creatine supplementation"},
		{"`creatine supplementation`", "creatine supplementation"},
	} {
		if got := CleanRewrite(c.in); got != c.want {
			t.Errorf("CleanRewrite(%q) = %q; want %q", c.in, got, c.want)
		}
	}
	// An essay means the model answered instead of rewriting.
	if got := CleanRewrite(strings.Repeat("creatine is effective ", 40)); got != "" {
		t.Errorf("a 400+ char rewrite should be refused, got %d chars", len(got))
	}
}

func TestRewriteFallsBackToTheQuestion(t *testing.T) {
	// Every failure path keeps retrieval working. A rewriter outage must
	// degrade the query, never fail the run.
	for name, complete := range map[string]Completer{
		"error": func(context.Context, string, string) (string, error) {
			return "", errors.New("model unavailable")
		},
		"empty": func(context.Context, string, string) (string, error) { return "   ", nil },
		"essay": func(context.Context, string, string) (string, error) {
			return strings.Repeat("creatine is effective ", 40), nil
		},
	} {
		r := NewRewriter(complete, "English abstracts")
		if got := r.Rewrite(context.Background(), "does creatine work?"); got != "does creatine work?" {
			t.Errorf("%s: got %q; want the original question", name, got)
		}
	}
}

func TestNewRewriterIsNilWithoutACompleter(t *testing.T) {
	// A corpus registered in a process with no model must not get a rewriter
	// that panics on first use.
	if NewRewriter(nil, "anything") != nil {
		t.Error("a rewriter with no completer should be nil, so Retrieve skips it")
	}
}
