package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dreplyai/go-assistant/chats"
	"github.com/dreplyai/go-assistant/handoff"
	flowexecmod "github.com/redelay/go-flowdsl/flowexec/module"
	"github.com/redelay/go-framework/modules"
	"github.com/redelay/go-framework/testing/testutil"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

// captureBus mirrors the handoff test helper for end-to-end coverage.
type captureBus struct {
	mu   sync.Mutex
	msgs []*modules.EventMessage
}

func (b *captureBus) Publish(_ context.Context, m *modules.EventMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, m)
	return nil
}
func (b *captureBus) Close() error { return nil }
func (b *captureBus) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.msgs)
}

// newFullFixture wires an assistant module with real chats + handoff
// services against a scratch DB. The SSE flow isn't exercised here —
// existing module_test.go covers that; this file hits the new
// handlers only.
func newFullFixture(t *testing.T) (*Module, *captureBus) {
	t.Helper()
	db := testutil.TestDB(t)

	// Stand up a minimal flowexec so assistant.New doesn't error.
	fx, err := flowexecmod.New(modules.ModuleDeps{Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("flowexec.New: %v", err)
	}
	flowexecmod.SetCurrentForTest(fx)
	t.Cleanup(func() { flowexecmod.SetCurrentForTest(nil) })

	bus := &captureBus{}
	asst, err := New(modules.ModuleDeps{
		Logger:   zap.NewNop(),
		DB:       db,
		EventBus: bus,
	})
	if err != nil {
		t.Fatalf("assistant.New: %v", err)
	}
	// Apply collections we want tests to hit (avoid clash with the
	// default ones from other test files).
	asst.chats = chats.NewService(db, chats.Options{
		Collection: "test_public_chats",
		TTL:        7 * 24 * time.Hour,
		Now:        func() time.Time { return time.Now().UTC() },
	})
	asst.handoff = handoff.NewService(db, asst.chats, bus, handoff.Options{
		Collection: "test_public_handoffs",
	})
	if err := asst.chats.EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("chat indexes: %v", err)
	}
	if err := asst.handoff.EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("handoff indexes: %v", err)
	}
	return asst, bus
}

// TestConfigEndpoint — returns the public runtime config. After the
// flow-derived-capabilities refactor, HandoffEnabled is the AND of
// (service wired, env allows, flow has handoff node). The fixture
// doesn't seed a flow so the "no flow" case is exercised here; the
// "flow seeded" case lives in TestConfigEndpoint_HandoffFromFlow.
func TestConfigEndpoint(t *testing.T) {
	asst, _ := newFullFixture(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/assistant/config", nil)
	asst.handleConfig(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var resp ConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// No flow seeded → no handoff node → handoff disabled even though
	// the service is wired. This is the new contract.
	if resp.HandoffEnabled {
		t.Error("HandoffEnabled should be false when the flow has no handoff node")
	}
	if resp.Capabilities.Handoff {
		t.Error("capabilities.handoff should be false with no seeded flow")
	}
	if !resp.AnonymousAllowed {
		t.Error("AnonymousAllowed should default to true")
	}
	if resp.ChatTTLDays <= 0 {
		t.Errorf("ChatTTLDays: %d", resp.ChatTTLDays)
	}
}

// TestConfigEndpoint_HandoffFromFlow — seeds the default flow (which
// includes the handoff branch since schemaVersion 2) and verifies the
// /config endpoint now reports handoff as enabled + capability=true.
// Locks in the "deployment is the source of truth" contract.
func TestConfigEndpoint_HandoffFromFlow(t *testing.T) {
	asst, _ := newFullFixture(t)
	if err := asst.ensureFlow(context.Background(), true); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	if err := asst.ensureDeployment(context.Background()); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/assistant/config", nil)
	asst.handleConfig(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var resp ConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Capabilities.Handoff {
		t.Error("capabilities.handoff should be true — the default flow ships with the node")
	}
	if !resp.HandoffEnabled {
		t.Error("HandoffEnabled should be true when the flow has the handoff node AND the service is wired")
	}
}

// TestChatByMe_Roundtrip — seed a chat via the service, fetch by id
// through the public handler.
func TestChatByMe_Roundtrip(t *testing.T) {
	asst, _ := newFullFixture(t)
	ctx := context.Background()

	_, _ = asst.chats.CreateIfAbsent(ctx, "sess-public", "assistant", "h", "stable", primitive.NilObjectID, "", nil)
	_, _ = asst.chats.AppendMessage(ctx, "sess-public", chats.Message{Role: "user", Content: "hello"}, "")
	_, _ = asst.chats.AppendMessage(ctx, "sess-public", chats.Message{Role: "assistant", Content: "hi!"}, "run-1")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/assistant/chats/me/sess-public", nil)
	r.SetPathValue("sessionID", "sess-public")
	asst.handleChatByMe(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp ChatResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.SessionID != "sess-public" {
		t.Errorf("SessionID: %s", resp.SessionID)
	}
	if len(resp.Messages) != 2 {
		t.Errorf("messages: %d, want 2", len(resp.Messages))
	}
	if resp.HandoffRequested {
		t.Error("HandoffRequested should be false without a handoff")
	}
}

// TestChatByMe_NotFound — unknown session returns 404.
func TestChatByMe_NotFound(t *testing.T) {
	asst, _ := newFullFixture(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/assistant/chats/me/nope", nil)
	r.SetPathValue("sessionID", "nope")
	asst.handleChatByMe(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: %d", w.Code)
	}
}

// TestHandoffEndpoint_EndToEnd — public POST persists and publishes.
func TestHandoffEndpoint_EndToEnd(t *testing.T) {
	asst, bus := newFullFixture(t)

	// Seed a chat so there's a transcript to snapshot.
	ctx := context.Background()
	_, _ = asst.chats.CreateIfAbsent(ctx, "sess-end", "assistant", "h", "", primitive.NilObjectID, "", nil)
	_, _ = asst.chats.AppendMessage(ctx, "sess-end", chats.Message{Role: "user", Content: "help"}, "")

	body := mustJSON(t, HandoffInput{
		SessionID: "sess-end",
		Email:     "user@example.com",
		Phone:     "+15550100",
		Reason:    "billing issue",
		Priority:  "high",
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/assistant/handoff", bytes.NewReader(body))
	asst.handleHandoff(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp HandoffResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "pending" || resp.Priority != "high" {
		t.Errorf("resp: %+v", resp)
	}

	// Event fired.
	if bus.Count() != 1 {
		t.Errorf("want 1 event published, got %d", bus.Count())
	}

	// Chat now has HandoffRequested=true via GET /chats/me/{id}.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/assistant/chats/me/sess-end", nil)
	r2.SetPathValue("sessionID", "sess-end")
	asst.handleChatByMe(w2, r2)
	var got ChatResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &got)
	if !got.HandoffRequested {
		t.Error("expected HandoffRequested=true after handoff")
	}
}

// TestHandoffEndpoint_Validation — 400 on missing email, 409 on
// duplicate, 503 when disabled.
func TestHandoffEndpoint_Validation(t *testing.T) {
	asst, _ := newFullFixture(t)

	// Missing sessionID → 400.
	body := mustJSON(t, HandoffInput{Email: "x@y.z"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/assistant/handoff", bytes.NewReader(body))
	asst.handleHandoff(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing sessionID: status %d, want 400 (body=%s)", w.Code, w.Body.String())
	}

	// Disabled module → 503.
	asst.cfg.HandoffEnabled = false
	ok := mustJSON(t, HandoffInput{SessionID: "s", Email: "a@b.c"})
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/assistant/handoff", bytes.NewReader(ok))
	asst.handleHandoff(w2, r2)
	if w2.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled: status %d, want 503", w2.Code)
	}
}

// TestHandoffEndpoint_Dedup — second submission for the same session
// returns 409.
func TestHandoffEndpoint_Dedup(t *testing.T) {
	asst, _ := newFullFixture(t)

	body := mustJSON(t, HandoffInput{SessionID: "sess-dup", Email: "a@b.c"})
	r1 := httptest.NewRequest(http.MethodPost, "/assistant/handoff", bytes.NewReader(body))
	w1 := httptest.NewRecorder()
	asst.handleHandoff(w1, r1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first: %d", w1.Code)
	}

	r2 := httptest.NewRequest(http.MethodPost, "/assistant/handoff", bytes.NewReader(body))
	w2 := httptest.NewRecorder()
	asst.handleHandoff(w2, r2)
	if w2.Code != http.StatusConflict {
		t.Errorf("second: %d, want 409", w2.Code)
	}
}

// TestHandoffEndpoint_AnonymousGate — when anonymous_allowed=false
// unauthenticated callers get 401.
func TestHandoffEndpoint_AnonymousGate(t *testing.T) {
	asst, _ := newFullFixture(t)
	asst.cfg.AnonymousAllowed = false

	body := mustJSON(t, HandoffInput{SessionID: "s", Email: "a@b.c"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/assistant/handoff", bytes.NewReader(body))
	asst.handleHandoff(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: %d, want 401", w.Code)
	}
}

// --- helpers ---

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// Ensure strings import is retained for any future grep.
var _ = strings.TrimSpace
