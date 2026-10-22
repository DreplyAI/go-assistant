package assistant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redelay/go-ai/llm"
	"github.com/redelay/go-flowdsl/runtime"
)

// The chat node runs with purpose assistant.chat in its context — what the
// go-ai ledger stamps on the call (it read "other" before).
func TestChatRunCarriesUsagePurpose(t *testing.T) {
	asst, teardown := newFixture(t, "unused")
	defer teardown()
	var mu sync.Mutex
	got := ""
	asst.flowexec.Engine().RegisterHandler("redelay/llm-chat", func(ctx context.Context, step *runtime.Step) error {
		mu.Lock()
		got = llm.UsageFromContext(ctx).Labels["purpose"]
		mu.Unlock()
		step.Output = map[string]any{"content": "ok"}
		return nil
	})
	if err := asst.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := readFrames(resp.Body, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != UsagePurpose {
		t.Errorf("purpose in the run = %q, want %q", got, UsagePurpose)
	}
}
