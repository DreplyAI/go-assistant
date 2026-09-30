package handoff_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dreplyai/go-assistant/chats"
	"github.com/dreplyai/go-assistant/handoff"
	"github.com/redelay/go-framework/modules"
	"github.com/redelay/go-framework/testing/testutil"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// captureBus records every Publish call so tests can assert on the
// event payload + name.
type captureBus struct {
	mu    sync.Mutex
	calls []*modules.EventMessage
}

func (b *captureBus) Publish(_ context.Context, msg *modules.EventMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, msg)
	return nil
}

func (b *captureBus) Close() error { return nil }

func (b *captureBus) Calls() []*modules.EventMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*modules.EventMessage, len(b.calls))
	copy(out, b.calls)
	return out
}

// fixture builds a chats + handoff service pair against scratch DBs.
func fixture(t *testing.T) (*chats.Service, *handoff.Service, *captureBus) {
	t.Helper()
	db := testutil.TestDB(t)
	bus := &captureBus{}
	chatSvc := chats.NewService(db, chats.Options{Collection: "test_assistant_chats"})
	if err := chatSvc.EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("chat indexes: %v", err)
	}
	hSvc := handoff.NewService(db, chatSvc, bus, handoff.Options{Collection: "test_assistant_handoffs"})
	if err := hSvc.EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("handoff indexes: %v", err)
	}
	return chatSvc, hSvc, bus
}

// TestRequest_PersistsAndPublishes — happy path. Request writes a
// row, snapshots the transcript, attaches the handoff id to the
// chat, and publishes exactly one event with the expected shape.
func TestRequest_PersistsAndPublishes(t *testing.T) {
	chatSvc, hSvc, bus := fixture(t)
	ctx := context.Background()

	// Seed a chat with two turns so there's a transcript to snapshot.
	_, _ = chatSvc.CreateIfAbsent(ctx, "sess-ok", "assistant", "h", "stable", primitive.NilObjectID, "", nil)
	_, _ = chatSvc.AppendMessage(ctx, "sess-ok", chats.Message{Role: "user", Content: "hello"}, "")
	_, _ = chatSvc.AppendMessage(ctx, "sess-ok", chats.Message{Role: "assistant", Content: "hi there"}, "")

	rec, err := hSvc.Request(ctx, handoff.RequestInput{
		SessionID: "sess-ok",
		Email:     "ops@example.com",
		Reason:    "integration failure",
		Priority:  handoff.PriorityHigh,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if rec.Status != handoff.StatusPending {
		t.Errorf("status: got %s, want pending", rec.Status)
	}
	if rec.Transcript == "" {
		t.Error("expected transcript snapshot, got empty")
	}

	// Chat row should carry the handoff id.
	chat, _ := chatSvc.GetBySessionID(ctx, "sess-ok")
	if chat.HandoffID != rec.GetID() {
		t.Errorf("chat.HandoffID: got %v, want %v", chat.HandoffID, rec.GetID())
	}

	// Event bus got exactly one message.
	calls := bus.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(calls))
	}
	evt := calls[0]
	if evt.Action != "requested" || evt.EntityType != "assistant_handoff" {
		t.Errorf("event envelope mismatch: %+v", evt)
	}
	if evt.Headers["event_name"] != "assistant.handoff_requested" {
		t.Errorf("event_name header: %q", evt.Headers["event_name"])
	}
	var payload handoff.HandoffRequestedPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload.Email != "ops@example.com" || payload.Priority != handoff.PriorityHigh {
		t.Errorf("payload: %+v", payload)
	}
	if payload.SessionID != "sess-ok" {
		t.Errorf("session id not threaded through: %q", payload.SessionID)
	}
}

// TestRequest_Dedup — a second pending request for the same session
// is rejected. Prevents mashy users from stacking the admin inbox.
func TestRequest_Dedup(t *testing.T) {
	chatSvc, hSvc, _ := fixture(t)
	ctx := context.Background()
	_, _ = chatSvc.CreateIfAbsent(ctx, "sess-dup", "assistant", "h", "", primitive.NilObjectID, "", nil)

	_, err := hSvc.Request(ctx, handoff.RequestInput{SessionID: "sess-dup", Email: "a@b.c"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err = hSvc.Request(ctx, handoff.RequestInput{SessionID: "sess-dup", Email: "a@b.c"})
	if !errors.Is(err, handoff.ErrDuplicate) {
		t.Errorf("second: want ErrDuplicate, got %v", err)
	}
}

// TestRequest_Validation — email required, sessionID required,
// priority validated against the closed enum.
func TestRequest_Validation(t *testing.T) {
	_, hSvc, _ := fixture(t)
	ctx := context.Background()

	if _, err := hSvc.Request(ctx, handoff.RequestInput{Email: "x@y.z"}); err == nil {
		t.Error("empty session id should fail")
	}
	if _, err := hSvc.Request(ctx, handoff.RequestInput{SessionID: "s"}); err == nil {
		t.Error("empty email should fail")
	}
	if _, err := hSvc.Request(ctx, handoff.RequestInput{SessionID: "s", Email: "a@b", Priority: "urgent"}); err == nil {
		t.Error("invalid priority should fail")
	}
}

// TestRequest_NoChat — a handoff without a prior chat row still
// persists (user might have cleared localStorage), just without a
// transcript.
func TestRequest_NoChat(t *testing.T) {
	_, hSvc, _ := fixture(t)
	ctx := context.Background()

	rec, err := hSvc.Request(ctx, handoff.RequestInput{
		SessionID: "no-chat",
		Email:     "a@b.c",
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if rec.ChatID != primitive.NilObjectID {
		t.Errorf("chat id should be zero when chat missing, got %v", rec.ChatID)
	}
	if rec.Transcript != "" {
		t.Errorf("transcript should be empty when chat missing, got %q", rec.Transcript)
	}
}

// TestList_StatusFilter — list respects status + paging.
func TestList_StatusFilter(t *testing.T) {
	_, hSvc, _ := fixture(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := hSvc.Request(ctx, handoff.RequestInput{
			SessionID: "s" + string(rune('a'+i)),
			Email:     "a@b.c",
		})
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	// Resolve one so we can filter.
	allPending, _, _ := hSvc.List(ctx, handoff.StatusPending, 10, "")
	if len(allPending) != 3 {
		t.Fatalf("pending list: got %d, want 3", len(allPending))
	}
	_, err := hSvc.UpdateStatus(ctx, allPending[0].GetID(), handoff.UpdateStatusInput{
		Status: handoff.StatusResolved,
	})
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	items, _, _ := hSvc.List(ctx, handoff.StatusPending, 10, "")
	if len(items) != 2 {
		t.Errorf("pending after resolve: got %d, want 2", len(items))
	}
	resolved, _, _ := hSvc.List(ctx, handoff.StatusResolved, 10, "")
	if len(resolved) != 1 {
		t.Errorf("resolved: got %d, want 1", len(resolved))
	}
}

// TestUpdateStatus_StampsContacted — transitioning to contacted
// records the timestamp + who did it.
func TestUpdateStatus_StampsContacted(t *testing.T) {
	_, hSvc, _ := fixture(t)
	ctx := context.Background()

	rec, _ := hSvc.Request(ctx, handoff.RequestInput{SessionID: "s", Email: "a@b.c"})
	admin := primitive.NewObjectID()

	updated, err := hSvc.UpdateStatus(ctx, rec.GetID(), handoff.UpdateStatusInput{
		Status:      handoff.StatusContacted,
		Notes:       "called — left voicemail",
		ContactedBy: admin,
	})
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if updated.Status != handoff.StatusContacted {
		t.Errorf("status: %s", updated.Status)
	}
	if updated.ContactedAt == nil {
		t.Error("ContactedAt should be stamped")
	}
	if updated.ContactedBy != admin {
		t.Errorf("ContactedBy: %v", updated.ContactedBy)
	}
	if updated.Notes != "called — left voicemail" {
		t.Errorf("Notes: %q", updated.Notes)
	}
}

// TestUpdateStatus_NotesOnlyKeepsStatus — the admin's "Save notes" sends no
// status; it must not wipe the one the handoff has.
func TestUpdateStatus_NotesOnlyKeepsStatus(t *testing.T) {
	_, hSvc, _ := fixture(t)
	ctx := context.Background()

	rec, _ := hSvc.Request(ctx, handoff.RequestInput{SessionID: "s2", Email: "n@b.c"})
	if _, err := hSvc.UpdateStatus(ctx, rec.GetID(), handoff.UpdateStatusInput{Status: handoff.StatusContacted}); err != nil {
		t.Fatal(err)
	}
	got, err := hSvc.UpdateStatus(ctx, rec.GetID(), handoff.UpdateStatusInput{Notes: "waiting for their reply"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != handoff.StatusContacted || got.Notes != "waiting for their reply" {
		t.Fatalf("status %q notes %q — a notes-only save must keep the status", got.Status, got.Notes)
	}
	// nothing to change: returns the handoff as it is
	same, err := hSvc.UpdateStatus(ctx, rec.GetID(), handoff.UpdateStatusInput{})
	if err != nil || same.Status != handoff.StatusContacted {
		t.Fatalf("empty update: %v %+v", err, same)
	}
}

// TestRequest_NoBus — event publish is best-effort. A nil bus means
// persistence still works, the event just doesn't fire.
func TestRequest_NoBus(t *testing.T) {
	db := testutil.TestDB(t)
	chatSvc := chats.NewService(db, chats.Options{Collection: "test_no_bus_chats"})
	hSvc := handoff.NewService(db, chatSvc, nil, handoff.Options{Collection: "test_no_bus_handoffs"})
	_ = hSvc.EnsureIndexes(context.Background())
	_ = chatSvc.EnsureIndexes(context.Background())

	ctx := context.Background()
	rec, err := hSvc.Request(ctx, handoff.RequestInput{SessionID: "s", Email: "a@b.c"})
	if err != nil {
		t.Fatalf("Request without bus: %v", err)
	}
	if rec.Status != handoff.StatusPending {
		t.Errorf("status: %s", rec.Status)
	}
}

// TestRequest_UserContext — authed caller's user id propagates
// through to both the row + the event payload.
func TestRequest_UserContext(t *testing.T) {
	chatSvc, hSvc, bus := fixture(t)
	ctx := context.Background()
	uid := primitive.NewObjectID()

	_, _ = chatSvc.CreateIfAbsent(ctx, "sess-u", "assistant", "h", "", uid, "", nil)
	rec, err := hSvc.Request(ctx, handoff.RequestInput{
		SessionID: "sess-u",
		UserID:    uid,
		Email:     "user@example.com",
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if rec.UserID != uid {
		t.Errorf("rec.UserID: %v", rec.UserID)
	}
	calls := bus.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls: %d", len(calls))
	}
	var payload handoff.HandoffRequestedPayload
	_ = json.Unmarshal(calls[0].Payload, &payload)
	if payload.UserID != uid.Hex() {
		t.Errorf("payload.UserID: %q, want %s", payload.UserID, uid.Hex())
	}
}

// TestRequest_RequestedAtFormat — RequestedAt is RFC3339 so a
// downstream flow can render it without additional parsing.
func TestRequest_RequestedAtFormat(t *testing.T) {
	_, hSvc, bus := fixture(t)
	ctx := context.Background()
	_, _ = hSvc.Request(ctx, handoff.RequestInput{SessionID: "s", Email: "a@b.c"})
	calls := bus.Calls()
	var payload handoff.HandoffRequestedPayload
	_ = json.Unmarshal(calls[0].Payload, &payload)
	if _, err := time.Parse(time.RFC3339, payload.RequestedAt); err != nil {
		t.Errorf("RequestedAt not RFC3339: %q (err: %v)", payload.RequestedAt, err)
	}
}
