package assistant

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redelay/go-ai/llm"
	"github.com/redelay/go-flowdsl/runtime"
)

// busyProvider answers every call with a retryable 429, the way a full Ollama
// Cloud slot or an Anthropic 529 does, and counts the calls.
type busyProvider struct{ calls atomic.Int32 }

func (p *busyProvider) ID() string                     { return "busy" }
func (p *busyProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p *busyProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls.Add(1)
	return nil, &llm.Error{Provider: "busy", Status: 429, Message: "slot busy", RetryableB: true}
}
func (p *busyProvider) ChatStream(context.Context, llm.ChatRequest) (<-chan llm.Chunk, error) {
	return nil, errors.New("not used")
}
func (p *busyProvider) Embed(context.Context, llm.EmbedRequest) (*llm.EmbedResponse, error) {
	return nil, errors.New("not used")
}

// TestChatIsNoRetry: a person waiting on POST /assistant/messages gets ONE
// model attempt and an error frame at once, not ~60 s of backoff (go-ai ≥
// v0.14 retries 429/5xx by default). The chat flow's llm-chat step calls a
// provider behind the same default retry middleware as production.
// Control: the very same wrapped provider, called without the handler's
// context, retries — so a pass is the handler's doing, not the stub's.
func TestChatIsNoRetry(t *testing.T) {
	asst, teardown := newFixture(t, "unused")
	defer teardown()

	busy := &busyProvider{}
	prov := llm.Chain(busy, llm.WithRetryConfig(llm.RetryConfig{
		MaxAttempts: 3, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond,
	}))
	asst.flowexec.Engine().RegisterHandler("redelay/llm-chat", func(ctx context.Context, step *runtime.Step) error {
		_, err := prov.Chat(ctx, llm.ChatRequest{})
		return err
	})
	if err := asst.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()

	start := time.Now()
	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	frames, err := readFrames(resp.Body, 5*time.Second)
	if err != nil {
		t.Fatalf("read frames: %v", err)
	}
	var errFrame *ChatFrame
	for i := range frames {
		if frames[i].Error != "" {
			errFrame = &frames[i]
			break
		}
	}
	if errFrame == nil {
		t.Fatalf("want an error frame for the busy model, got %+v", frames)
	}
	if n := busy.calls.Load(); n != 1 {
		t.Fatalf("interactive chat made %d model calls, want exactly 1 (no retry while a person waits)", n)
	}
	if strings.Contains(errFrame.Error, "retry exhausted") {
		t.Errorf("error frame should carry the first error, not a retry summary: %q", errFrame.Error)
	}
	t.Logf("error frame after %s: %q", time.Since(start).Round(time.Millisecond), errFrame.Error)

	// Control: the same provider without the request's context retries.
	busy.calls.Store(0)
	if _, err := prov.Chat(context.Background(), llm.ChatRequest{}); err == nil {
		t.Fatal("control: busy provider should still fail after retries")
	}
	if n := busy.calls.Load(); n != 3 {
		t.Fatalf("control: an unmarked context made %d calls, want 3 (retry is on by default)", n)
	}
}
