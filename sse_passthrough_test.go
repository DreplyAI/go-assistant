package assistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redelay/go-flowdsl/runtime"
)

// TestSSEPassthroughSourcesActionsTrace is the regression contract for
// the typed-cards consumers (gymmer's coach): `sources`, `actions`,
// and `trace` from the flow must reach the SSE stream UNCHANGED —
// same keys, same shapes, no filtering beyond the documented Action
// normalisation (drop entries without a type).
func TestSSEPassthroughSourcesActionsTrace(t *testing.T) {
	asst, teardown := newFixture(t, "unused")
	defer teardown()

	// The stub terminal payload: content + citations + UI actions in
	// exactly the shape redelay/assistant-rag-context stamps them.
	wantSources := []map[string]any{
		{"title": "Doc One", "url": "https://example.com/1", "score": 0.91},
		{"title": "Doc Two", "path": "guides/two.md"},
	}
	wantActions := []map[string]any{
		{"id": "act-1", "type": "open_url", "label": "Open the guide",
			"payload": map[string]any{"url": "https://example.com/1"}, "source": "rag"},
		{"type": "handoff", "label": "Talk to a human", "priority": "high"},
		{"label": "no type — must be dropped"},
	}
	asst.flowexec.Engine().RegisterHandler("redelay/llm-chat", func(ctx context.Context, step *runtime.Step) error {
		time.Sleep(50 * time.Millisecond)
		step.Output = map[string]any{
			"content": "grounded reply",
			"sources": wantSources,
			"actions": wantActions,
		}
		return nil
	})

	if err := asst.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()

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

	var content *ChatFrame
	var traces int
	for i := range frames {
		if frames[i].Content != "" {
			content = &frames[i]
		}
		if frames[i].Trace != nil {
			traces++
			// Trace frames carry at least nodeId + kind, verbatim
			// from the flow event stream.
			if _, ok := frames[i].Trace["nodeId"]; !ok {
				t.Errorf("trace frame missing nodeId: %+v", frames[i].Trace)
			}
			if _, ok := frames[i].Trace["kind"]; !ok {
				t.Errorf("trace frame missing kind: %+v", frames[i].Trace)
			}
		}
	}
	if content == nil {
		t.Fatalf("no content frame in %+v", frames)
	}
	if traces == 0 {
		t.Error("no trace frames — non-terminal node activity must be forwarded")
	}

	// Sources pass through unchanged — full deep-equality via JSON so
	// numeric types normalise the same way they do on the real wire.
	gotSrc, _ := json.Marshal(content.Sources)
	wantSrc, _ := json.Marshal(wantSources)
	if string(gotSrc) != string(wantSrc) {
		t.Errorf("sources changed in transit:\n got %s\nwant %s", gotSrc, wantSrc)
	}

	// Actions: typed passthrough. Two valid actions survive with every
	// field intact; the type-less third is dropped (documented).
	if len(content.Actions) != 2 {
		t.Fatalf("actions: got %d, want 2 (%+v)", len(content.Actions), content.Actions)
	}
	a0 := content.Actions[0]
	if a0.ID != "act-1" || a0.Type != "open_url" || a0.Label != "Open the guide" ||
		a0.Source != "rag" || a0.Payload["url"] != "https://example.com/1" {
		t.Errorf("action[0] mangled: %+v", a0)
	}
	a1 := content.Actions[1]
	if a1.Type != "handoff" || a1.Label != "Talk to a human" || a1.Priority != "high" {
		t.Errorf("action[1] mangled: %+v", a1)
	}

	// Wire-shape check: the raw JSON keys the widgets parse.
	raw, _ := json.Marshal(content)
	for _, key := range []string{`"sources"`, `"actions"`, `"content"`, `"runId"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("frame JSON missing %s: %s", key, raw)
		}
	}
}

// TestBackCompatDefaultTenant locks in requirement (b): with no
// resolver and no tenant key, the module behaves exactly like v0.2.4
// — default deployment, no tenant fields anywhere on the wire.
func TestBackCompatDefaultTenant(t *testing.T) {
	asst, teardown := newFixture(t, "plain single-tenant reply")
	defer teardown()
	if err := asst.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	// Chat with zero tenant signals answers from the default flow.
	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()
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
	var got string
	for _, fr := range frames {
		if fr.Content != "" {
			got = fr.Content
		}
	}
	if got != "plain single-tenant reply" {
		t.Errorf("default-tenant content = %q", got)
	}

	// /config raw JSON carries no tenant-era keys — the v0.2.4 wire
	// shape is byte-compatible for existing widgets.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/assistant/config", nil)
	asst.handleConfig(w, r)
	body := w.Body.String()
	for _, key := range []string{`"tenant"`, `"suggestedQuestions"`} {
		if strings.Contains(body, key) {
			t.Errorf("default-tenant config leaked %s: %s", key, body)
		}
	}
	var cfg ConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.DeploymentID != asst.cfg.DeploymentID {
		t.Errorf("deployment id changed: %q", cfg.DeploymentID)
	}
	if cfg.FlowID != asst.cfg.FlowID {
		t.Errorf("flow id changed: %q", cfg.FlowID)
	}
}
