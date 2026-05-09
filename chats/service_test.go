package chats_test

import (
	"context"
	"testing"
	"time"

	"github.com/dreplyai/go-assistant/chats"
	"github.com/redelay/go-framework/testing/testutil"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// newSvc returns a Service bound to a per-test scratch DB plus a
// fixed clock so expiresAt math is predictable.
func newSvc(t *testing.T) (*chats.Service, func(time.Time)) {
	t.Helper()
	db := testutil.TestDB(t)
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc := chats.NewService(db, chats.Options{
		Collection: "test_assistant_chats",
		TTL:        48 * time.Hour,
		Now:        func() time.Time { return now },
	})
	if err := svc.EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}
	setNow := func(t time.Time) { now = t }
	return svc, setNow
}

// TestCreateIfAbsent_Idempotent — two calls with the same session_id
// must return the same document and not create duplicates. The
// unique index on session_id guards against concurrent first-writes.
func TestCreateIfAbsent_Idempotent(t *testing.T) {
	svc, _ := newSvc(t)
	ctx := context.Background()

	a, err := svc.CreateIfAbsent(ctx, "sess-1", "assistant", "hash-a", "stable", primitive.NilObjectID, "", nil)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	b, err := svc.CreateIfAbsent(ctx, "sess-1", "assistant", "hash-b", "canary", primitive.NilObjectID, "", nil)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if a.SessionID != b.SessionID || a.GetID() != b.GetID() {
		t.Errorf("expected same document on repeat call, got ids %s vs %s", a.GetID().Hex(), b.GetID().Hex())
	}
	// First create wins for the stamped flow metadata — a mid-chat
	// re-route should never overwrite the opening stamp.
	if b.VersionHash != "hash-a" {
		t.Errorf("VersionHash after repeat: got %q, want hash-a (first-write wins)", b.VersionHash)
	}
	if b.VariantLabel != "stable" {
		t.Errorf("VariantLabel after repeat: got %q, want stable", b.VariantLabel)
	}
}

// TestAppendMessage_BumpsTTL — expiresAt rolls forward on every
// append so active chats survive past the idle TTL.
func TestAppendMessage_BumpsTTL(t *testing.T) {
	svc, setNow := newSvc(t)
	ctx := context.Background()

	_, err := svc.CreateIfAbsent(ctx, "sess-ttl", "assistant", "h1", "", primitive.NilObjectID, "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Jump forward 24h (half the 48h TTL) and append.
	setNow(time.Date(2025, 1, 2, 12, 0, 0, 0, time.UTC))
	c, err := svc.AppendMessage(ctx, "sess-ttl", chats.Message{Role: "user", Content: "hi"}, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// TTL window starts from the now (2025-01-02 12:00) + 48h.
	wantExpires := time.Date(2025, 1, 4, 12, 0, 0, 0, time.UTC)
	if !c.ExpiresAt.Equal(wantExpires) {
		t.Errorf("ExpiresAt: got %v, want %v", c.ExpiresAt, wantExpires)
	}
	if len(c.Messages) != 1 {
		t.Fatalf("messages: got %d, want 1", len(c.Messages))
	}
	if c.Messages[0].RunID != "run-1" {
		t.Errorf("message RunID: got %q, want run-1", c.Messages[0].RunID)
	}
}

// TestAppendMessage_TracksRunIDs — run ids become a set (no dupes)
// on the chat document so admin drill-in sees each run exactly once.
func TestAppendMessage_TracksRunIDs(t *testing.T) {
	svc, _ := newSvc(t)
	ctx := context.Background()

	_, _ = svc.CreateIfAbsent(ctx, "sess-runs", "assistant", "h", "", primitive.NilObjectID, "", nil)
	_, _ = svc.AppendMessage(ctx, "sess-runs", chats.Message{Role: "user", Content: "a"}, "r1")
	_, _ = svc.AppendMessage(ctx, "sess-runs", chats.Message{Role: "assistant", Content: "b"}, "r1") // same run
	c, err := svc.AppendMessage(ctx, "sess-runs", chats.Message{Role: "user", Content: "c"}, "r2")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(c.RunIDs) != 2 {
		t.Errorf("RunIDs: got %v, want length 2 (r1, r2)", c.RunIDs)
	}
}

// TestAppendMessage_NotFound — appending before CreateIfAbsent
// surfaces ErrNotFound so handlers can decide to create or 404.
func TestAppendMessage_NotFound(t *testing.T) {
	svc, _ := newSvc(t)
	ctx := context.Background()

	_, err := svc.AppendMessage(ctx, "never-created", chats.Message{Role: "user", Content: "hi"}, "")
	if err != chats.ErrNotFound {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// TestGetBySessionID — round-trips content and respects ErrNotFound.
func TestGetBySessionID(t *testing.T) {
	svc, _ := newSvc(t)
	ctx := context.Background()

	_, _ = svc.CreateIfAbsent(ctx, "sess-get", "assistant", "h", "", primitive.NilObjectID, "", nil)
	_, _ = svc.AppendMessage(ctx, "sess-get", chats.Message{Role: "user", Content: "hi"}, "")

	got, err := svc.GetBySessionID(ctx, "sess-get")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "hi" {
		t.Errorf("messages round-trip: %+v", got.Messages)
	}

	_, err = svc.GetBySessionID(ctx, "nope")
	if err != chats.ErrNotFound {
		t.Errorf("unknown session: want ErrNotFound, got %v", err)
	}
}

// TestList_CursorPaging — newest-first + cursor walks the full set.
func TestList_CursorPaging(t *testing.T) {
	svc, setNow := newSvc(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		setNow(time.Date(2025, 1, 1, 12, i, 0, 0, time.UTC))
		_, err := svc.CreateIfAbsent(ctx, "sess-"+string(rune('a'+i)), "assistant", "h", "", primitive.NilObjectID, "", nil)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	// First page, 2 items.
	items, next, err := svc.List(ctx, chats.ListFilter{}, 2, "")
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(items) != 2 || next == "" {
		t.Fatalf("page 1: got %d items next=%q, want 2 + non-empty", len(items), next)
	}
	// Walk all pages, collect IDs, confirm no duplicates and full set.
	seen := map[string]bool{}
	for _, c := range items {
		seen[c.GetID().Hex()] = true
	}
	cursor := next
	for cursor != "" {
		items, next, err = svc.List(ctx, chats.ListFilter{}, 2, cursor)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, c := range items {
			if seen[c.GetID().Hex()] {
				t.Errorf("duplicate id %s", c.GetID().Hex())
			}
			seen[c.GetID().Hex()] = true
		}
		cursor = next
	}
	if len(seen) != 5 {
		t.Errorf("expected 5 unique chats, got %d", len(seen))
	}
}

// TestAttachHandoff — stamps the handoff id so admin UI can navigate
// chat ↔ handoff bidirectionally.
func TestAttachHandoff(t *testing.T) {
	svc, _ := newSvc(t)
	ctx := context.Background()

	_, _ = svc.CreateIfAbsent(ctx, "sess-h", "assistant", "h", "", primitive.NilObjectID, "", nil)
	hoid := primitive.NewObjectID()
	if err := svc.AttachHandoff(ctx, "sess-h", hoid); err != nil {
		t.Fatalf("AttachHandoff: %v", err)
	}
	c, _ := svc.GetBySessionID(ctx, "sess-h")
	if c.HandoffID != hoid {
		t.Errorf("HandoffID: got %v, want %v", c.HandoffID, hoid)
	}
}
