package assistant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dreplyai/go-assistant/chats"
	flowexecmod "github.com/redelay/go-flowdsl/flowexec/module"
	"github.com/redelay/go-framework/modules"
	"github.com/redelay/go-framework/modules/auth"
	"github.com/redelay/go-framework/testing/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
)

// Does "delete this chat" delete the chat — both copies of it?
//
// The transcript is in `assistant_chats`; the same sentences are in
// `flow_events.payload`, once per node per turn, because the executor stores
// every packet. A delete that removes the first and leaves the second is the
// precise shape of a button that lies to the person who pressed it, and it is
// what this endpoint was added to make impossible.
//
// So the assertion searches both collections for the planted sentence rather
// than checking a status code or a document count.
func TestDeletingAChatErasesBothCopiesOfIt(t *testing.T) {
	db := testutil.TestDB(t)
	ctx := context.Background()

	fx, err := flowexecmod.New(modules.ModuleDeps{Logger: zap.NewNop(), DB: db})
	if err != nil {
		t.Fatalf("flowexec.New: %v", err)
	}
	flowexecmod.SetCurrentForTest(fx)
	t.Cleanup(func() { flowexecmod.SetCurrentForTest(nil) })
	stubChatNodes(t, fx)

	asst, err := New(modules.ModuleDeps{Logger: zap.NewNop(), DB: db})
	if err != nil {
		t.Fatalf("assistant.New: %v", err)
	}
	asst.chats = chats.NewService(db, chats.Options{
		Collection: "test_delete_chats",
		TTL:        7 * 24 * time.Hour,
		Now:        func() time.Time { return time.Now().UTC() },
	})
	if err := asst.chats.EnsureIndexes(ctx); err != nil {
		t.Fatalf("chat indexes: %v", err)
	}
	if err := asst.Startup(ctx); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	userID := primitive.NewObjectID()
	const secret = "PLANTEDQUESTION my lower back aches after romanian deadlifts"
	withUser := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.ContextWithClaims(r.Context(), &auth.Claims{UserID: userID.Hex()})
			h(w, r.WithContext(ctx))
		}
	}

	// Hold a real conversation.
	chatSrv := httptest.NewServer(withUser(asst.handleChat))
	defer chatSrv.Close()
	resp, err := http.Post(chatSrv.URL, "application/json",
		strings.NewReader(`{"sessionId":"sess-delete-me","messages":[{"role":"user","content":"`+secret+`"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if _, err := readFrames(resp.Body, 10*time.Second); err != nil {
		t.Fatalf("read frames: %v", err)
	}
	resp.Body.Close()

	// Wait for the executor's goroutine to finish writing the history.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := db.Collection("flow_events").CountDocuments(ctx, bson.M{}); n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	inChats := grepColl(t, db, "test_delete_chats", secret)
	inEvents := grepColl(t, db, "flow_events", secret)
	if inChats == 0 {
		t.Fatal("the transcript was not stored — the fixture does not reproduce " +
			"a real chat, so nothing below proves anything")
	}
	if inEvents == 0 {
		t.Fatal("the question is not in flow_events — either the flow no longer " +
			"carries it or the sink is not wired; either way this test is blind")
	}
	t.Logf("before delete: %d transcript rows, %d flow_events rows hold the question",
		inChats, inEvents)

	// Delete it.
	delSrv := httptest.NewServer(withUser(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("sessionID", "sess-delete-me")
		asst.handleChatDelete(w, r)
	}))
	defer delSrv.Close()
	req, _ := http.NewRequest(http.MethodDelete, delSrv.URL, nil)
	dres, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	dres.Body.Close()
	if dres.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE returned %d, want 204", dres.StatusCode)
	}

	if n := grepColl(t, db, "test_delete_chats", secret); n != 0 {
		t.Errorf("%d transcript rows still hold the question", n)
	}
	if n := grepColl(t, db, "flow_events", secret); n != 0 {
		t.Errorf("%d flow_events rows still hold the question — the transcript is "+
			"gone and the run history is not, which is the same false statement "+
			"one collection down", n)
	}
}

// A session id is not authorisation to erase someone's account history.
func TestAnotherMembersChatCannotBeDeleted(t *testing.T) {
	db := testutil.TestDB(t)
	ctx := context.Background()
	svc := chats.NewService(db, chats.Options{Collection: "test_owner_chats"})
	if err := svc.EnsureIndexes(ctx); err != nil {
		t.Fatalf("indexes: %v", err)
	}

	owner, intruder := primitive.NewObjectID(), primitive.NewObjectID()
	if _, err := svc.CreateIfAbsent(ctx, "sess-owned", "fitness-coach", "v1", "stable",
		owner, "", nil); err != nil {
		t.Fatalf("CreateIfAbsent: %v", err)
	}

	if _, err := svc.DeleteSession(ctx, "sess-owned", intruder); err == nil {
		t.Error("another member deleted this chat by knowing its session id")
	}
	if _, err := svc.GetBySessionID(ctx, "sess-owned"); err != nil {
		t.Errorf("the chat was deleted anyway: %v", err)
	}
	if _, err := svc.DeleteSession(ctx, "sess-owned", owner); err != nil {
		t.Errorf("the owner could not delete their own chat: %v", err)
	}
}

// grepColl counts documents in one collection whose stored bytes contain
// text.
//
// Serialises the row rather than querying a field path, so it answers "is the
// sentence in the database" and not "is it where I expected it to be" — the
// second question is the one that passes while a copy survives somewhere else
// in the document.
func grepColl(t *testing.T, db *mongo.Database, coll, text string) int {
	t.Helper()
	ctx := context.Background()
	cur, err := db.Collection(coll).Find(ctx, bson.M{})
	if err != nil {
		t.Fatalf("find %s: %v", coll, err)
	}
	defer cur.Close(ctx)
	n := 0
	for cur.Next(ctx) {
		if strings.Contains(cur.Current.String(), text) {
			n++
		}
	}
	return n
}
