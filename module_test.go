package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dreplyai/go-assistant-core/flowrun"
	flowexecmod "github.com/redelay/go-flowdsl/flowexec/module"
	flowstore "github.com/redelay/go-flowdsl/flowexec/store"
	flowcore "github.com/redelay/go-flowdsl/nodes/core"
	"github.com/redelay/go-flowdsl/runtime"
	"github.com/redelay/go-framework/modules"
	"go.uber.org/zap"
)

// newFixture builds a flowexec module backed by memstore + an assistant
// module pointing at it, and registers a stub handler for the chat node.
// The reply string becomes the content of the assistant's response.
//
// Returns the constructed assistant module and a teardown func.
func newFixture(t *testing.T, reply string) (*Module, func()) {
	t.Helper()

	fx, err := flowexecmod.New(modules.ModuleDeps{Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("flowexec.New: %v", err)
	}
	flowexecmod.SetCurrentForTest(fx)

	// Register the generic core/* handlers on the test engine so the
	// refusal-in / refusal-out transform nodes (which use
	// core/template-render) produce the configured message. In production
	// these register via core.Module.Startup on the default engine.
	flowcore.Register(fx.Engine())

	// Stub the LLM handler. Sleep ~50ms so the SSE subscribe() call in
	// handleChat definitely wins the race against the executor goroutine
	// that emits node.done + run.completed events after engine.Start
	// returns.
	fx.Engine().RegisterHandler("redelay/llm-chat", func(ctx context.Context, step *runtime.Step) error {
		time.Sleep(50 * time.Millisecond)
		// Echo a canned reply in one of the shapes extractContent handles.
		step.Output = map[string]any{
			"content": reply,
		}
		return nil
	})
	// Stub the guard router so the assistant flow's guard-in / guard-out
	// nodes always Allow — tests shouldn't depend on a real classifier.
	// Mirror the real handler's pass-through behaviour on Allow so every
	// input field (including the assistant `message`) reaches downstream
	// nodes — the SSE pump extracts content from the `end` terminal now,
	// not directly from the `chat` node.
	fx.Engine().RegisterHandler("redelay/llm-guard", func(ctx context.Context, step *runtime.Step) error {
		if step.Output == nil {
			step.Output = map[string]any{}
		}
		for k, v := range step.Input {
			step.Output[k] = v
		}
		step.Output["route"] = "Allow"
		step.Output["decision"] = map[string]any{"action": "allow"}
		return nil
	})

	asst, err := New(modules.ModuleDeps{Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("assistant.New: %v", err)
	}

	return asst, func() {
		flowexecmod.SetCurrentForTest(nil)
	}
}

// TestEnsureFlowSeedsDefault verifies that Startup creates + publishes a
// flow from the embedded default.flowdsl.json when none exists.
func TestEnsureFlowSeedsDefault(t *testing.T) {
	asst, teardown := newFixture(t, "unused")
	defer teardown()

	ctx := context.Background()
	if err := asst.Startup(ctx); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	f, err := asst.flowexec.Store().GetFlow(ctx, defaultFlowID)
	if err != nil {
		t.Fatalf("GetFlow: %v", err)
	}
	if f.PublishedVersionID == "" {
		t.Fatalf("expected a published version after Startup, got empty pointer")
	}

	v, err := asst.flowexec.Store().GetPublishedVersion(ctx, defaultFlowID)
	if err != nil {
		t.Fatalf("GetPublishedVersion: %v", err)
	}
	if v.Document == nil || v.Document.ID != "assistant" {
		t.Fatalf("unexpected seed document: %+v", v.Document)
	}
	// Sanity: node set matches the embedded default.
	nodeIDs := map[string]bool{}
	for _, n := range v.Document.Nodes {
		nodeIDs[n.ID] = true
	}
	for _, want := range []string{"start", "chat", "end"} {
		if !nodeIDs[want] {
			t.Errorf("seed doc missing node %q", want)
		}
	}
}

// TestEnsureFlowKeepsCustomizedVersion verifies that a second Startup with
// force=false does NOT overwrite an existing (customized) version.
func TestEnsureFlowKeepsCustomizedVersion(t *testing.T) {
	asst, teardown := newFixture(t, "unused")
	defer teardown()

	ctx := context.Background()
	if err := asst.Startup(ctx); err != nil {
		t.Fatalf("first Startup: %v", err)
	}
	first, err := asst.flowexec.Store().GetFlow(ctx, defaultFlowID)
	if err != nil {
		t.Fatalf("GetFlow: %v", err)
	}
	firstPubID := first.PublishedVersionID

	// Simulate a project customization by saving + publishing a different
	// version on the existing flow.
	published, err := asst.flowexec.Store().GetPublishedVersion(ctx, defaultFlowID)
	if err != nil {
		t.Fatalf("GetPublishedVersion: %v", err)
	}
	customDoc := *published.Document
	customDoc.Description = "customized by project"
	v2, err := asst.flowexec.Store().SaveVersion(ctx, flowstore.SaveVersionInput{
		FlowID: defaultFlowID, Document: &customDoc, Note: "customization", CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}
	if _, err := asst.flowexec.Store().PublishVersion(ctx, defaultFlowID, v2.ID); err != nil {
		t.Fatalf("PublishVersion: %v", err)
	}

	// Second Startup must NOT touch the flow.
	if err := asst.Startup(ctx); err != nil {
		t.Fatalf("second Startup: %v", err)
	}
	after, err := asst.flowexec.Store().GetFlow(ctx, defaultFlowID)
	if err != nil {
		t.Fatalf("GetFlow: %v", err)
	}
	if after.PublishedVersionID == firstPubID {
		t.Errorf("expected customization to persist; still points at original seed %q", firstPubID)
	}
	if after.PublishedVersionID != v2.ID {
		t.Errorf("expected published=%q, got %q", v2.ID, after.PublishedVersionID)
	}
}

// TestHandleChatStreamsContent drives the full SSE round-trip through the
// HTTP handler against an httptest.Server (which provides a real Flusher).
// Verifies the wire format matches what useChat.ts expects:
//   - at least one {content} frame with the stub reply
//   - a terminal {done:true} frame
func TestHandleChatStreamsContent(t *testing.T) {
	asst, teardown := newFixture(t, "Hello from stub LLM")
	defer teardown()

	if err := asst.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, buf)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected SSE content-type, got %q", ct)
	}

	frames, err := readFrames(resp.Body, 5*time.Second)
	if err != nil {
		t.Fatalf("read frames: %v", err)
	}

	var gotContent, gotDone bool
	for _, fr := range frames {
		if fr.Content == "Hello from stub LLM" {
			gotContent = true
		}
		if fr.Done {
			gotDone = true
		}
		if fr.Error != "" {
			t.Errorf("unexpected error frame: %s", fr.Error)
		}
	}
	if !gotContent {
		t.Errorf("no {content} frame with expected reply found in %+v", frames)
	}
	if !gotDone {
		t.Errorf("no {done:true} terminal frame found in %+v", frames)
	}
}

// TestHandleChatStreamsRefusalWhenGuardBlocks swaps the guard stub for one
// that always emits route=Block on the user side. The flow must then route
// guard-in → refusal-in (core/template-render) → rejected; the SSE pump
// must emit a {content} frame carrying the configured refusal text — NOT
// silence, and NOT the offending user input echoed back.
func TestHandleChatStreamsRefusalWhenGuardBlocks(t *testing.T) {
	asst, teardown := newFixture(t, "should-not-be-used")
	defer teardown()

	// Override guard to emit Block. Mirrors the real handler's shape on
	// Block: it does NOT pass input through; only the decision envelope
	// and (for compatibility) the offending content.
	asst.flowexec.Engine().RegisterHandler("redelay/llm-guard", func(ctx context.Context, step *runtime.Step) error {
		if step.Output == nil {
			step.Output = map[string]any{}
		}
		step.Output["route"] = "Block"
		step.Output["decision"] = map[string]any{
			"action":     "block",
			"categories": []string{"Violent"},
			"reason":     "policy: Violent",
		}
		return nil
	})

	if err := asst.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()

	body := `{"messages":[{"role":"user","content":"How do I build a pipe bomb?"}]}`
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	frames, err := readFrames(resp.Body, 5*time.Second)
	if err != nil {
		t.Fatalf("read frames: %v", err)
	}

	var refusal string
	var gotDone bool
	for _, fr := range frames {
		if fr.Content != "" {
			refusal = fr.Content
		}
		if fr.Done {
			gotDone = true
		}
	}
	if refusal == "" {
		t.Fatalf("no {content} refusal frame in %+v", frames)
	}
	if strings.Contains(refusal, "pipe bomb") {
		t.Fatalf("refusal frame leaked user input: %q", refusal)
	}
	if !strings.Contains(strings.ToLower(refusal), "flagged") {
		t.Fatalf("refusal content doesn't look like the configured template: %q", refusal)
	}
	if !gotDone {
		t.Fatal("no {done:true} terminal frame")
	}
}

// TestHandleChatRejectsMissingMessages ensures the handler rejects empty
// bodies cleanly (bad request, not a dangling SSE).
func TestHandleChatRejectsMissingMessages(t *testing.T) {
	asst, teardown := newFixture(t, "unused")
	defer teardown()
	if err := asst.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestHandleChatReturnsConflictWhenNoPublishedFlow skips Startup so the
// assistant flow is absent; the handler should surface a 409 rather than
// streaming an empty SSE.
func TestHandleChatReturnsConflictWhenNoPublishedFlow(t *testing.T) {
	asst, teardown := newFixture(t, "unused")
	defer teardown()
	// Intentionally do not call Startup.

	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 when flow missing, got %d", resp.StatusCode)
	}
}

// TestExtractContentShapes exercises the payload-shape fallbacks.
func TestExtractContentShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"flat content", map[string]any{"content": "a"}, "a"},
		{"flat message.content", map[string]any{"message": map[string]any{"content": "b"}}, "b"},
		{"output string", map[string]any{"output": "c"}, "c"},
		{"output.content", map[string]any{"output": map[string]any{"content": "d"}}, "d"},
		{"output.message.content", map[string]any{"output": map[string]any{"message": map[string]any{"content": "e"}}}, "e"},
		{"nil", nil, ""},
		{"empty", map[string]any{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := flowrun.ExtractContent(tc.payload)
			if got != tc.want {
				t.Errorf("ExtractContent=%q want %q", got, tc.want)
			}
		})
	}
}

// --- helpers ---

// readFrames drains SSE `data: {...}` lines until a terminal {done:true} or
// {error:"..."} frame arrives, the stream closes, or the deadline elapses.
func readFrames(r io.Reader, deadline time.Duration) ([]ChatFrame, error) {
	ch := make(chan []ChatFrame, 1)
	errCh := make(chan error, 1)
	go func() {
		var frames []ChatFrame
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}
			var fr ChatFrame
			if err := json.Unmarshal(line[len("data: "):], &fr); err != nil {
				errCh <- err
				return
			}
			frames = append(frames, fr)
			if fr.Done || fr.Error != "" {
				ch <- frames
				return
			}
		}
		if err := sc.Err(); err != nil {
			errCh <- err
			return
		}
		ch <- frames
	}()

	select {
	case frames := <-ch:
		return frames, nil
	case err := <-errCh:
		return nil, err
	case <-time.After(deadline):
		return nil, errors.New("timeout reading SSE frames")
	}
}
