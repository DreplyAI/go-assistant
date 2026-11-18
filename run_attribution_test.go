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
	flowcore "github.com/redelay/go-flowdsl/nodes/core"
	"github.com/redelay/go-flowdsl/runtime"
	"github.com/redelay/go-framework/modules"
	"github.com/redelay/go-framework/modules/auth"
	"github.com/redelay/go-framework/testing/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

// Can a run's stored telemetry be found by the person it belongs to?
//
// Not a style question. The executor writes every node's whole input and
// output packet into `flow_events.payload`, so a chat's text — the person's
// messages and the model's replies — is in that collection at every node of
// the flow. `flow_events` is a Mongo TIME-SERIES collection, and a
// time-series delete may only filter on the metaField. An id that travels
// only in the run input lands in `payload`, a measurement field, and the rows
// are then unreachable by any erasure request on any Mongo version.
//
// This failed before AssistantID/UserID were set on the RunInput: measured on
// a real database, `flow_events` carried `{flowId, runId}` and nothing else,
// and 0 of 18302 rows had a `meta.userId`. The run itself worked perfectly,
// which is why nobody noticed for as long as the collection has existed.
//
// So the assertion is deliberately made against what Mongo stored rather than
// against the struct handed to RunAsync: the struct field being populated is
// not the property that matters, landing in the metaField is.
func TestAChatRunIsAttributableToTheUserInTheMetaField(t *testing.T) {
	db := testutil.TestDB(t)

	// A flowexec module WITH a database, so the real Mongo time-series sink is
	// installed rather than the noop one the other fixtures get.
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
		Collection: "test_attribution_chats",
		TTL:        7 * 24 * time.Hour,
		Now:        func() time.Time { return time.Now().UTC() },
	})
	if err := asst.chats.EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("chat indexes: %v", err)
	}
	if err := asst.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	userID := primitive.NewObjectID()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.ContextWithClaims(r.Context(), &auth.Claims{UserID: userID.Hex()})
		asst.handleChat(w, r.WithContext(ctx))
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"my knee hurts on squats"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if _, err := readFrames(resp.Body, 10*time.Second); err != nil {
		t.Fatalf("read frames: %v", err)
	}

	// The sink writes from the executor's goroutine, which outlives the
	// response: the final run event is emitted after the stream closes.
	ctx := context.Background()
	var events int64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, _ = db.Collection("flow_events").CountDocuments(ctx, bson.M{})
		if events > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if events == 0 {
		t.Fatal("no flow_events were written — the sink is not wired, so this " +
			"test would pass for the wrong reason")
	}

	attributed, err := db.Collection("flow_events").CountDocuments(ctx,
		bson.M{"meta.userId": userID.Hex()})
	if err != nil {
		t.Fatalf("count attributed: %v", err)
	}
	if attributed == 0 {
		t.Errorf("%d flow_events rows written and none carries meta.userId=%s — "+
			"the chat text in them cannot be deleted by any per-user filter, "+
			"because a time-series delete may only match the metaField",
			events, userID.Hex())
	}
	if attributed != events {
		t.Errorf("only %d of %d flow_events rows are attributed; every event of "+
			"an authenticated run must be, or erasure leaves the rest behind",
			attributed, events)
	}

	// The run summary too: it is the index the purge reads to find a user's
	// runs when the events themselves predate this fix.
	runs, err := db.Collection("flow_runs").CountDocuments(ctx,
		bson.M{"userId": userID.Hex()})
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs == 0 {
		t.Error("flow_runs carries no userId — nothing can list a member's runs")
	}
}

// An anonymous caller has no account, so there is nothing to erase and no id
// to stamp. Asserted rather than assumed: a placeholder id here (the
// all-zeroes ObjectID, or the readable "anon-<ip>" handle) would collect every
// anonymous visitor's chat under one key and make that key look like a person.
func TestAnAnonymousRunCarriesNoUserID(t *testing.T) {
	db := testutil.TestDB(t)
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
		Collection: "test_anon_chats",
		TTL:        7 * 24 * time.Hour,
		Now:        func() time.Time { return time.Now().UTC() },
	})
	if err := asst.chats.EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("chat indexes: %v", err)
	}
	if err := asst.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if _, err := readFrames(resp.Body, 10*time.Second); err != nil {
		t.Fatalf("read frames: %v", err)
	}

	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := db.Collection("flow_events").CountDocuments(ctx, bson.M{}); n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	n, err := db.Collection("flow_events").CountDocuments(ctx,
		bson.M{"meta.userId": bson.M{"$exists": true, "$ne": ""}})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d anonymous event rows carry a meta.userId; an anonymous "+
			"visitor must not be filed under an id that looks like an account", n)
	}
}

// stubChatNodes registers the same stand-ins newFixture uses — the LLM and
// the guard — on a flowexec engine, so a run completes without a provider.
// Separate from newFixture because these tests need a DB-backed flowexec
// module (for the real Mongo sink) and newFixture deliberately builds one
// without a database.
func stubChatNodes(t *testing.T, fx *flowexecmod.Module) {
	t.Helper()
	flowcore.Register(fx.Engine())
	fx.Engine().RegisterHandler("redelay/llm-chat", func(ctx context.Context, step *runtime.Step) error {
		time.Sleep(50 * time.Millisecond)
		step.Output = map[string]any{"content": "try a box squat to a depth that does not hurt"}
		return nil
	})
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
}
