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

// TestSourcesFrom — only display fields survive; a card that carried the
// chunk text (it must never be stored) loses it; empty cards are dropped.
func TestSourcesFrom(t *testing.T) {
	got := chats.SourcesFrom([]map[string]any{
		{"id": "how-to-return", "kind": "doc", "title": "How to start a return", "url": "https://x/help/returns", "score": 0.86, "text": "SECRET CHUNK", "snippet": "also secret"},
		{"title": "Exchanges", "path": "help/exchanges.md", "score": 1},
		{"text": "only text — nothing to show"},
	})
	if len(got) != 2 {
		t.Fatalf("want 2 sources, got %+v", got)
	}
	if got[0].Title != "How to start a return" || got[0].URL != "https://x/help/returns" || got[0].Score != 0.86 || got[0].ID != "how-to-return" {
		t.Errorf("first source mangled: %+v", got[0])
	}
	if got[1].Path != "help/exchanges.md" || got[1].Score != 1 {
		t.Errorf("second source mangled: %+v", got[1])
	}
	if chats.SourcesFrom(nil) != nil {
		t.Error("no cards must be nil (omitted in JSON/BSON)")
	}
}

// TestAppendMessage_KeepsSources — sources round-trip through Mongo.
func TestAppendMessage_KeepsSources(t *testing.T) {
	svc, _ := newSvc(t)
	ctx := context.Background()
	_, _ = svc.CreateIfAbsent(ctx, "sess-src", "assistant", "h", "", primitive.NilObjectID, "", nil)
	src := chats.SourcesFrom([]map[string]any{{"title": "Warranty", "url": "https://x/w", "score": 0.79}})
	if _, err := svc.AppendMessage(ctx, "sess-src", chats.Message{Role: "assistant", Content: "yes [1]", Sources: src}, "r1"); err != nil {
		t.Fatalf("append: %v", err)
	}
	c, err := svc.GetBySessionID(ctx, "sess-src")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(c.Messages) != 1 || len(c.Messages[0].Sources) != 1 || c.Messages[0].Sources[0].Title != "Warranty" || c.Messages[0].Sources[0].Score != 0.79 {
		t.Fatalf("sources not stored: %+v", c.Messages)
	}
}

// TestCount_MatchesListFilter — Count sees every page, narrowed by tenant.
func TestCount_MatchesListFilter(t *testing.T) {
	svc, _ := newSvc(t)
	ctx := context.Background()
	for i, tenant := range []string{"demo", "demo", "demo", "other"} {
		if _, err := svc.CreateIfAbsentTenant(ctx, tenant, "cnt-"+string(rune('a'+i)), "assistant", "h", "", primitive.NilObjectID, "", nil); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if n, err := svc.Count(ctx, chats.ListFilter{Tenant: "demo"}); err != nil || n != 3 {
		t.Errorf("demo count = %d, %v; want 3", n, err)
	}
	if n, err := svc.Count(ctx, chats.ListFilter{}); err != nil || n != 4 {
		t.Errorf("all count = %d, %v; want 4", n, err)
	}
}

// walk pages through every chat and fails on a repeat.
func walk(t *testing.T, svc *chats.Service, limit int, cursor string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	for first := true; first || cursor != ""; first = false {
		items, next, err := svc.List(context.Background(), chats.ListFilter{}, limit, cursor)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, c := range items {
			if seen[c.SessionID] {
				t.Errorf("chat %s listed twice", c.SessionID)
			}
			seen[c.SessionID] = true
		}
		cursor = next
	}
	return seen
}

// TestList_CursorFollowsLastAt — an old chat that gets a new message moves
// to the top; paging must still return every chat exactly once.
func TestList_CursorFollowsLastAt(t *testing.T) {
	svc, setNow := newSvc(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		setNow(time.Date(2025, 1, 1, 12, i, 0, 0, time.UTC))
		if _, err := svc.CreateIfAbsent(ctx, "move-"+string(rune('a'+i)), "assistant", "h", "", primitive.NilObjectID, "", nil); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	// the oldest chat (smallest _id) is now the most recent
	setNow(time.Date(2025, 1, 1, 13, 0, 0, 0, time.UTC))
	if _, err := svc.AppendMessage(ctx, "move-a", chats.Message{Role: "user", Content: "again"}, ""); err != nil {
		t.Fatalf("append: %v", err)
	}
	items, _, _ := svc.List(ctx, chats.ListFilter{}, 1, "")
	if len(items) != 1 || items[0].SessionID != "move-a" {
		t.Fatalf("newest first: got %v", items)
	}
	if seen := walk(t, svc, 2, ""); len(seen) != 6 {
		t.Errorf("walked %d chats, want 6", len(seen))
	}
}

// TestList_CursorTiesOnLastAt — chats sharing a last_at page by _id.
func TestList_CursorTiesOnLastAt(t *testing.T) {
	svc, setNow := newSvc(t)
	ctx := context.Background()
	setNow(time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))
	for i := 0; i < 5; i++ {
		if _, err := svc.CreateIfAbsent(ctx, "tie-"+string(rune('a'+i)), "assistant", "h", "", primitive.NilObjectID, "", nil); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if seen := walk(t, svc, 2, ""); len(seen) != 5 {
		t.Errorf("walked %d chats, want 5", len(seen))
	}
}

// TestList_LegacyIDCursor — a bare ObjectID cursor (pre-v0.3.4 clients)
// continues from that chat's position.
func TestList_LegacyIDCursor(t *testing.T) {
	svc, setNow := newSvc(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		setNow(time.Date(2025, 1, 1, 12, i, 0, 0, time.UTC))
		if _, err := svc.CreateIfAbsent(ctx, "leg-"+string(rune('a'+i)), "assistant", "h", "", primitive.NilObjectID, "", nil); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	first, _, _ := svc.List(ctx, chats.ListFilter{}, 2, "")
	rest := walk(t, svc, 2, first[1].GetID().Hex())
	if len(rest) != 2 || rest[first[0].SessionID] || rest[first[1].SessionID] {
		t.Errorf("legacy cursor continued wrong: %v", rest)
	}
}
