package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dreplyai/go-assistant/chats"
	"github.com/dreplyai/go-assistant/handoff"
	flowexecmod "github.com/redelay/go-flowdsl/flowexec/module"
	flowstore "github.com/redelay/go-flowdsl/flowexec/store"
	"github.com/redelay/go-flowdsl/ir"
	flowcore "github.com/redelay/go-flowdsl/nodes/core"
	"github.com/redelay/go-flowdsl/runtime"
	"github.com/redelay/go-framework/modules"
	"github.com/redelay/go-framework/testing/testutil"
	"go.uber.org/zap"
)

// echoFlowJSONFor renders a zero-LLM flow (start → core/template-render
// → end) whose reply is the given fixed string. Two tenants seeded with
// two of these prove per-tenant flow routing without any provider.
func echoFlowJSONFor(id, reply string) []byte {
	return []byte(fmt.Sprintf(`{
	  "id": %q,
	  "name": "Echo %s",
	  "nodes": [
	    {"id": "start", "name": "In",   "kind": "start"},
	    {"id": "wait",  "name": "Wait", "kind": "action", "action_ref": "test/slow"},
	    {"id": "echo",  "name": "Say",  "kind": "transform", "action_ref": "core/template-render",
	     "config": {"outputKey": "content", "template": %q}},
	    {"id": "end",   "name": "Out",  "kind": "end"}
	  ],
	  "edges": [
	    {"id": "e1", "from": "start", "to": "wait", "delivery_mode": "direct"},
	    {"id": "e2", "from": "wait",  "to": "echo", "delivery_mode": "direct"},
	    {"id": "e3", "from": "echo",  "to": "end",  "delivery_mode": "direct"}
	  ]
	}`, id, id, reply))
}

// seedEchoFlow creates + publishes an echo flow in the store.
func seedEchoFlow(t *testing.T, fx *flowexecmod.Module, id, reply string) {
	t.Helper()
	ctx := context.Background()
	var doc ir.Workflow
	if err := json.Unmarshal(echoFlowJSONFor(id, reply), &doc); err != nil {
		t.Fatalf("parse echo flow: %v", err)
	}
	if _, err := fx.Store().CreateFlow(ctx, flowstore.CreateFlowInput{ID: id, Name: "Echo " + id}); err != nil {
		t.Fatalf("create flow %s: %v", id, err)
	}
	v, err := fx.Store().SaveVersion(ctx, flowstore.SaveVersionInput{
		FlowID: id, Document: &doc, Note: "test seed", CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("save version %s: %v", id, err)
	}
	if _, err := fx.Store().PublishVersion(ctx, id, v.ID); err != nil {
		t.Fatalf("publish %s: %v", id, err)
	}
}

// newTenantFixture wires an assistant with real Mongo-backed chats +
// handoff, a memstore flowexec with the core node handlers, and two
// registered tenants pointing at two distinct echo flows.
func newTenantFixture(t *testing.T) *Module {
	t.Helper()
	db := testutil.TestDB(t)

	fx, err := flowexecmod.New(modules.ModuleDeps{Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("flowexec.New: %v", err)
	}
	flowexecmod.SetCurrentForTest(fx)
	t.Cleanup(func() { flowexecmod.SetCurrentForTest(nil) })
	flowcore.Register(fx.Engine())
	// test/slow gives the SSE handler time to subscribe before the
	// (otherwise instantaneous) echo flow completes — same race the
	// llm-chat stub in module_test.go papers over with its sleep.
	fx.Engine().RegisterHandler("test/slow", func(ctx context.Context, step *runtime.Step) error {
		time.Sleep(60 * time.Millisecond)
		if step.Output == nil {
			step.Output = map[string]any{}
		}
		for k, v := range step.Input {
			step.Output[k] = v
		}
		return nil
	})

	asst, err := New(modules.ModuleDeps{Logger: zap.NewNop(), DB: db})
	if err != nil {
		t.Fatalf("assistant.New: %v", err)
	}
	// Isolated collections so parallel test files don't collide.
	asst.chats = chats.NewService(db, chats.Options{Collection: "test_tenant_chats", TTL: 24 * time.Hour})
	asst.handoff = handoff.NewService(db, asst.chats, nil, handoff.Options{Collection: "test_tenant_handoffs"})

	ctx := context.Background()
	seedEchoFlow(t, fx, "flow-a", "answer-from-tenant-a")
	seedEchoFlow(t, fx, "flow-b", "answer-from-tenant-b")

	if err := asst.RegisterTenant(ctx, Tenant{
		Key:                "site-a",
		Name:               "Tenant A",
		FlowID:             "flow-a",
		DocsIndex:          "docs_a",
		SuggestedQuestions: []string{"What is A?"},
	}); err != nil {
		t.Fatalf("register site-a: %v", err)
	}
	if err := asst.RegisterTenant(ctx, Tenant{
		Key:    "site-b",
		FlowID: "flow-b",
	}); err != nil {
		t.Fatalf("register site-b: %v", err)
	}
	return asst
}

// postChat POSTs one message with an optional tenant key (sent as the
// X-Assistant-Site header) and returns the parsed SSE frames plus the
// session id the server assigned.
func postChat(t *testing.T, srv *httptest.Server, tenantKey, text string) ([]ChatFrame, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL,
		strings.NewReader(fmt.Sprintf(`{"messages":[{"role":"user","content":%q}]}`, text)))
	req.Header.Set("Content-Type", "application/json")
	if tenantKey != "" {
		req.Header.Set(TenantHeader, tenantKey)
	}
	resp, err := http.DefaultClient.Do(req)
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
	return frames, resp.Header.Get("X-Assistant-Session-Id")
}

func contentOf(frames []ChatFrame) string {
	for _, fr := range frames {
		if fr.Content != "" {
			return fr.Content
		}
	}
	return ""
}

// TestTwoTenantsOneProcess is the core multi-tenant proof: one module
// instance, two tenants, each chat answered by its own flow, each
// chat persisted under its own tenant key, and /config differing per
// tenant.
func TestTwoTenantsOneProcess(t *testing.T) {
	asst := newTenantFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()
	ctx := context.Background()

	framesA, sessA := postChat(t, srv, "site-a", "hello")
	if got := contentOf(framesA); got != "answer-from-tenant-a" {
		t.Errorf("tenant A content = %q, want answer-from-tenant-a", got)
	}
	framesB, sessB := postChat(t, srv, "site-b", "hello")
	if got := contentOf(framesB); got != "answer-from-tenant-b" {
		t.Errorf("tenant B content = %q, want answer-from-tenant-b", got)
	}

	// Chats persisted with the right tenant stamp + flow id.
	ca, err := asst.chats.GetBySessionID(ctx, sessA)
	if err != nil {
		t.Fatalf("chat A: %v", err)
	}
	if ca.Tenant != "site-a" || ca.FlowID != "flow-a" {
		t.Errorf("chat A tenant=%q flow=%q, want site-a/flow-a", ca.Tenant, ca.FlowID)
	}
	cb, err := asst.chats.GetBySessionID(ctx, sessB)
	if err != nil {
		t.Fatalf("chat B: %v", err)
	}
	if cb.Tenant != "site-b" || cb.FlowID != "flow-b" {
		t.Errorf("chat B tenant=%q flow=%q, want site-b/flow-b", cb.Tenant, cb.FlowID)
	}

	// Tenant-filtered admin listing sees only its own chats.
	onlyA, _, err := asst.chats.List(ctx, chats.ListFilter{Tenant: "site-a"}, 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, c := range onlyA {
		if c.Tenant != "site-a" {
			t.Errorf("tenant filter leaked chat from %q", c.Tenant)
		}
	}
	if len(onlyA) == 0 {
		t.Error("tenant filter returned no chats for site-a")
	}

	// /config differs per tenant.
	cfgFor := func(key string) ConfigResponse {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/assistant/config?site="+key, nil)
		asst.handleConfig(w, r)
		var resp ConfigResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode config %s: %v", key, err)
		}
		return resp
	}
	a, b := cfgFor("site-a"), cfgFor("site-b")
	if a.Tenant != "site-a" || b.Tenant != "site-b" {
		t.Errorf("config tenants: %q / %q", a.Tenant, b.Tenant)
	}
	if a.FlowID != "flow-a" || b.FlowID != "flow-b" {
		t.Errorf("config flow ids: %q / %q", a.FlowID, b.FlowID)
	}
	if a.DeploymentID == b.DeploymentID {
		t.Errorf("tenants share a deployment id: %q", a.DeploymentID)
	}
	if len(a.SuggestedQuestions) != 1 || a.SuggestedQuestions[0] != "What is A?" {
		t.Errorf("tenant A suggested questions: %v", a.SuggestedQuestions)
	}
	if len(b.SuggestedQuestions) != 0 {
		t.Errorf("tenant B suggested questions should be empty: %v", b.SuggestedQuestions)
	}
}

// TestTenantResolutionOrder locks in explicit-key > resolver > default.
func TestTenantResolutionOrder(t *testing.T) {
	asst := newTenantFixture(t)
	asst.tenantResolvers = []TenantResolver{resolverFunc(func(r *http.Request) string {
		if r.Host == "b.example.com" {
			return "site-b"
		}
		return ""
	})}

	// Explicit header wins over the resolver.
	r := httptest.NewRequest(http.MethodGet, "http://b.example.com/assistant/config", nil)
	r.Header.Set(TenantHeader, "site-a")
	if got := asst.tenantForRequest(r).Key; got != "site-a" {
		t.Errorf("header should win: got %q", got)
	}

	// Query param works too.
	r = httptest.NewRequest(http.MethodGet, "/x?site=site-b", nil)
	if got := asst.tenantForRequest(r).Key; got != "site-b" {
		t.Errorf("query param: got %q", got)
	}

	// Resolver kicks in with no explicit key.
	r = httptest.NewRequest(http.MethodGet, "http://b.example.com/x", nil)
	if got := asst.tenantForRequest(r).Key; got != "site-b" {
		t.Errorf("resolver: got %q", got)
	}

	// Nothing matches → default tenant.
	r = httptest.NewRequest(http.MethodGet, "http://other.example.com/x", nil)
	if got := asst.tenantForRequest(r).Key; got != "" {
		t.Errorf("default: got %q", got)
	}

	// Unknown explicit key falls back to default rather than erroring.
	r = httptest.NewRequest(http.MethodGet, "/x?site=nope", nil)
	if got := asst.tenantForRequest(r).Key; got != "" {
		t.Errorf("unknown key: got %q", got)
	}
}

type resolverFunc func(r *http.Request) string

func (f resolverFunc) ResolveTenant(r *http.Request) string { return f(r) }

// TestTenantChatByMeScoping — a session created under tenant A is not
// readable through tenant B's widget, but is through its own.
func TestTenantChatByMeScoping(t *testing.T) {
	asst := newTenantFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()
	_, sess := postChat(t, srv, "site-a", "hello")

	get := func(tenantKey string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/assistant/chats/me/"+sess, nil)
		if tenantKey != "" {
			r.Header.Set(TenantHeader, tenantKey)
		}
		r.SetPathValue("sessionID", sess)
		asst.handleChatByMe(w, r)
		return w.Code
	}
	if code := get("site-a"); code != http.StatusOK {
		t.Errorf("own tenant: %d", code)
	}
	if code := get("site-b"); code != http.StatusNotFound {
		t.Errorf("cross tenant: %d, want 404", code)
	}
	if code := get(""); code != http.StatusNotFound {
		t.Errorf("default tenant reading site-a chat: %d, want 404", code)
	}
}

// TestTenantHandoffScoping — handoffs are stamped with the tenant and
// the admin filter sees them scoped.
func TestTenantHandoffScoping(t *testing.T) {
	asst := newTenantFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()
	_, sess := postChat(t, srv, "site-a", "help")

	body := fmt.Sprintf(`{"sessionId":%q,"email":"a@b.c"}`, sess)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/assistant/handoff", strings.NewReader(body))
	r.Header.Set(TenantHeader, "site-a")
	asst.handleHandoff(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("handoff status=%d body=%s", w.Code, w.Body.String())
	}

	ctx := context.Background()
	items, _, err := asst.handoff.ListFiltered(ctx, handoff.ListFilter{Tenant: "site-a"}, 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].Tenant != "site-a" {
		t.Fatalf("tenant handoff filter: %+v", items)
	}
	other, _, err := asst.handoff.ListFiltered(ctx, handoff.ListFilter{Tenant: "site-b"}, 10, "")
	if err != nil {
		t.Fatalf("list b: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("site-b should have no handoffs, got %d", len(other))
	}
}

// TestTenantRateLimit — a tenant with RateLimitPerMinute=1 accepts the
// first message of a session and 429s the second.
func TestTenantRateLimit(t *testing.T) {
	asst := newTenantFixture(t)
	if err := asst.RegisterTenant(context.Background(), Tenant{
		Key: "limited", FlowID: "flow-a", RateLimitPerMinute: 1,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(asst.handleChat))
	defer srv.Close()

	_, sess := postChat(t, srv, "limited", "one")

	req, _ := http.NewRequest(http.MethodPost, srv.URL,
		strings.NewReader(fmt.Sprintf(`{"messages":[{"role":"user","content":"two"}],"sessionId":%q}`, sess)))
	req.Header.Set(TenantHeader, "limited")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second message: %d, want 429", resp.StatusCode)
	}
}

// TestTenantPersistenceRoundtrip — CreateTenant persists to Mongo and
// a fresh registry load sees it (what a restart would do).
func TestTenantPersistenceRoundtrip(t *testing.T) {
	asst := newTenantFixture(t)
	ctx := context.Background()

	created, err := asst.CreateTenant(ctx, Tenant{Key: "persisted", Name: "P", FlowID: "flow-a"})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if created.Key != "persisted" {
		t.Fatalf("key: %q", created.Key)
	}
	// Duplicate key → ErrTenantExists.
	if _, err := asst.CreateTenant(ctx, Tenant{Key: "persisted"}); err != ErrTenantExists {
		t.Errorf("duplicate: %v", err)
	}
	// Invalid key rejected.
	if _, err := asst.CreateTenant(ctx, Tenant{Key: "Bad Key!"}); err != ErrInvalidTenantKey {
		t.Errorf("invalid key: %v", err)
	}

	// Simulate restart: wipe the registry, reload from Mongo.
	asst.tenantReg = newTenantRegistry()
	asst.loadTenants(ctx)
	if got := asst.TenantByKey("persisted"); got == nil || got.Name != "P" {
		t.Fatalf("reload lost tenant: %+v", got)
	}

	// Update + delete.
	name := "P2"
	upd, err := asst.UpdateTenant(ctx, "persisted", TenantUpdate{Name: &name})
	if err != nil || upd.Name != "P2" {
		t.Fatalf("update: %v %+v", err, upd)
	}
	if err := asst.DeleteTenant(ctx, "persisted"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if asst.TenantByKey("persisted") != nil {
		t.Error("delete left tenant in registry")
	}
}
