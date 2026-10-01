package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/dreplyai/go-assistant"
	"github.com/dreplyai/go-assistant/admin"
	"github.com/dreplyai/go-assistant/chats"
	"github.com/dreplyai/go-assistant/handoff"
	flowexecmod "github.com/redelay/go-flowdsl/flowexec/module"
	"github.com/redelay/go-framework/modules"
	"github.com/redelay/go-framework/testing/testutil"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

// bus stub — admin handlers don't publish but the assistant module
// initialisation expects a non-nil EventBus.
type nopBus struct{}

func (nopBus) Publish(context.Context, *modules.EventMessage) error { return nil }
func (nopBus) Close() error                                         { return nil }

// newAdminFixture wires the assistant core + admin module against a
// scratch DB. Unlike the public_handlers test, this setup also
// exercises Configure() so we catch registry-lookup regressions.
func newAdminFixture(t *testing.T) (*admin.Module, *assistant.Module) {
	t.Helper()
	db := testutil.TestDB(t)

	fx, err := flowexecmod.New(modules.ModuleDeps{Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("flowexec.New: %v", err)
	}
	flowexecmod.SetCurrentForTest(fx)
	t.Cleanup(func() { flowexecmod.SetCurrentForTest(nil) })

	asst, err := assistant.New(modules.ModuleDeps{
		Logger:   zap.NewNop(),
		DB:       db,
		EventBus: nopBus{},
	})
	if err != nil {
		t.Fatalf("assistant.New: %v", err)
	}

	// Construct admin module directly (the factory path runs only
	// under Bootstrap) and wire it up.
	adm := &admin.Module{}
	reg := modules.NewRegistry()
	if err := reg.Register(asst); err != nil {
		t.Fatalf("register assistant: %v", err)
	}
	if err := adm.Configure(reg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return adm, asst
}

// TestListChats — seeded chats surface in newest-first order.
func TestListChats(t *testing.T) {
	adm, asst := newAdminFixture(t)
	ctx := context.Background()

	// Seed 3 chats.
	for i := 0; i < 3; i++ {
		sid := "s" + string(rune('a'+i))
		_, err := asst.Chats().CreateIfAbsent(ctx, sid, "assistant", "h", "stable", primitive.NilObjectID, "", nil)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/assistant/chats", nil)
	adm.HandlerListChats()(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp admin.ChatListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 3 {
		t.Errorf("items: %d, want 3", len(resp.Items))
	}
}

// TestGetChat_ByObjectID + 404 path.
func TestGetChat(t *testing.T) {
	adm, asst := newAdminFixture(t)
	ctx := context.Background()
	c, _ := asst.Chats().CreateIfAbsent(ctx, "sess-x", "assistant", "h", "", primitive.NilObjectID, "", nil)

	// Happy path.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/assistant/chats/"+c.GetID().Hex(), nil)
	r.SetPathValue("id", c.GetID().Hex())
	adm.HandlerGetChat()(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}

	// 404 for a valid-but-absent id.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/x", nil)
	r2.SetPathValue("id", primitive.NewObjectID().Hex())
	adm.HandlerGetChat()(w2, r2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("absent id status: %d, want 404", w2.Code)
	}
}

// TestListHandoffs — default status filter is pending.
func TestListHandoffs(t *testing.T) {
	adm, asst := newAdminFixture(t)
	ctx := context.Background()

	_, _ = asst.Handoff().Request(ctx, handoff.RequestInput{
		SessionID: "s1", Email: "a@b.c",
	})
	_, _ = asst.Handoff().Request(ctx, handoff.RequestInput{
		SessionID: "s2", Email: "a@b.c",
	})

	// Default list (pending).
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/assistant/handoffs", nil)
	adm.HandlerListHandoffs()(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp admin.HandoffListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 2 {
		t.Errorf("items: %d, want 2", len(resp.Items))
	}
}

// TestUpdateHandoff — status transitions + notes.
func TestUpdateHandoff(t *testing.T) {
	adm, asst := newAdminFixture(t)
	ctx := context.Background()
	rec, _ := asst.Handoff().Request(ctx, handoff.RequestInput{
		SessionID: "s-update", Email: "a@b.c",
	})

	body, _ := json.Marshal(admin.HandoffUpdateInput{
		Status: "contacted",
		Notes:  "sent welcome email",
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/x", bytes.NewReader(body))
	r.SetPathValue("id", rec.GetID().Hex())
	adm.HandlerUpdateHandoff()(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var out handoff.HandoffRequest
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Status != handoff.StatusContacted {
		t.Errorf("status: %s", out.Status)
	}
	if out.Notes != "sent welcome email" {
		t.Errorf("notes: %q", out.Notes)
	}
	if out.ContactedAt == nil {
		t.Error("ContactedAt should be stamped on transition to contacted")
	}
}

// TestUpdateHandoff_InvalidID — 400 on malformed object id.
func TestUpdateHandoff_InvalidID(t *testing.T) {
	adm, _ := newAdminFixture(t)

	body, _ := json.Marshal(admin.HandoffUpdateInput{Status: "contacted"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/x", bytes.NewReader(body))
	r.SetPathValue("id", "not-an-objectid")
	adm.HandlerUpdateHandoff()(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: %d, want 400", w.Code)
	}
}

// TestResetEndpoint_PickTemplate — POST /admin/assistant/reset?template=production
// reseeds the live flow from the production template. Verifies both
// shapes end up published and that an unknown template yields 400.
func TestResetEndpoint_PickTemplate(t *testing.T) {
	adm, asst := newAdminFixture(t)

	mustReset := func(qs string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/admin/assistant/reset"+qs, bytes.NewBufferString(""))
		adm.HandlerReset()(w, r)
		return w
	}

	// Default template — no query param.
	if w := mustReset(""); w.Code != http.StatusOK {
		t.Fatalf("default template status = %d body=%s", w.Code, w.Body.String())
	}
	fx := flowexecmod.Current()
	cfg := asst.Cfg()
	flow, err := fx.Store().GetFlow(context.Background(), cfg.FlowID)
	if err != nil || flow.PublishedVersionID == "" {
		t.Fatalf("expected published version after reset, got err=%v flow=%+v", err, flow)
	}
	v, err := fx.Store().GetVersion(context.Background(), flow.PublishedVersionID)
	if err != nil {
		t.Fatalf("get published version: %v", err)
	}
	if v.Labels["template"] != "default" {
		t.Errorf("published template label = %q, want default", v.Labels["template"])
	}

	// Switch to production.
	if w := mustReset("?template=production"); w.Code != http.StatusOK {
		t.Fatalf("production template status = %d body=%s", w.Code, w.Body.String())
	}
	flow, _ = fx.Store().GetFlow(context.Background(), cfg.FlowID)
	v, _ = fx.Store().GetVersion(context.Background(), flow.PublishedVersionID)
	if v.Labels["template"] != "production" {
		t.Errorf("published template label = %q, want production", v.Labels["template"])
	}

	// Unknown template → 400, published version unchanged.
	prevID := flow.PublishedVersionID
	if w := mustReset("?template=bogus"); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown template status = %d, want 400", w.Code)
	}
	flow, _ = fx.Store().GetFlow(context.Background(), cfg.FlowID)
	if flow.PublishedVersionID != prevID {
		t.Errorf("published version changed after rejected reset")
	}
}

// TestListChats_Views — the Conversations tabs: exact counts on the first
// page, per-view listing, and each chat's handoff status.
func TestListChats_Views(t *testing.T) {
	adm, asst := newAdminFixture(t)
	ctx := context.Background()
	cs, hs := asst.Chats(), asst.Handoff()

	// sid → (assistant replied?, handoff status or "")
	seed := []struct {
		sid     string
		replied bool
		status  handoff.Status
	}{
		{"v-answered", true, ""},
		{"v-silent", false, ""},
		{"v-pending", false, handoff.StatusPending},
		{"v-taken", true, handoff.StatusContacted},
		{"v-resolved", false, handoff.StatusResolved},
		{"v-dismissed", false, handoff.StatusDismissed},
	}
	for _, s := range seed {
		if _, err := cs.CreateIfAbsent(ctx, s.sid, "assistant", "h", "", primitive.NilObjectID, "", nil); err != nil {
			t.Fatalf("seed %s: %v", s.sid, err)
		}
		_, _ = cs.AppendMessage(ctx, s.sid, chats.Message{Role: "user", Content: "q"}, "")
		if s.replied {
			_, _ = cs.AppendMessage(ctx, s.sid, chats.Message{Role: "assistant", Content: "a"}, "")
		}
		if s.status != "" {
			h, err := hs.Request(ctx, handoff.RequestInput{SessionID: s.sid, Email: "v@demo.example"})
			if err != nil {
				t.Fatalf("handoff %s: %v", s.sid, err)
			}
			if s.status != handoff.StatusPending {
				if _, err := hs.UpdateStatus(ctx, h.ID, handoff.UpdateStatusInput{Status: s.status}); err != nil {
					t.Fatalf("status %s: %v", s.sid, err)
				}
			}
		}
	}

	list := func(view string) admin.ChatListResponse {
		w := httptest.NewRecorder()
		adm.HandlerListChats()(w, httptest.NewRequest(http.MethodGet, "/assistant/chats?view="+view, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", view, w.Code, w.Body)
		}
		var resp admin.ChatListResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return resp
	}
	sids := func(r admin.ChatListResponse) map[string]bool {
		out := map[string]bool{}
		for _, c := range r.Items {
			out[c.SessionID] = true
		}
		return out
	}

	all := list("")
	if all.Counts["all"] != 6 || all.Counts["handoffs"] != 2 || all.Counts["noanswer"] != 2 {
		t.Errorf("counts = %v, want all 6 / handoffs 2 / noanswer 2", all.Counts)
	}
	if all.Total == nil || *all.Total != 6 {
		t.Errorf("total = %v, want 6", all.Total)
	}
	if got := sids(list("handoffs")); len(got) != 2 || !got["v-pending"] || !got["v-taken"] {
		t.Errorf("handoffs view = %v", got)
	}
	nr := list("noanswer")
	if got := sids(nr); len(got) != 2 || !got["v-silent"] || !got["v-dismissed"] {
		t.Errorf("noanswer view = %v", got)
	}
	if nr.Total == nil || *nr.Total != 2 {
		t.Errorf("noanswer total = %v, want 2", nr.Total)
	}
	byStatus := map[string]string{}
	for _, c := range all.Items {
		if st, ok := all.HandoffStatus[c.ID.Hex()]; ok {
			byStatus[c.SessionID] = st
		}
	}
	want := map[string]string{"v-pending": "pending", "v-taken": "contacted", "v-resolved": "resolved", "v-dismissed": "dismissed"}
	for sid, st := range want {
		if byStatus[sid] != st {
			t.Errorf("handoffStatus[%s] = %q, want %q", sid, byStatus[sid], st)
		}
	}
	if len(all.Handoffs) != 4 {
		t.Errorf("page handoffs = %d, want 4", len(all.Handoffs))
	}
}

// TestListChats_Search — q matches any message, case-insensitively and
// literally, and narrows every view and count.
func TestListChats_Search(t *testing.T) {
	adm, asst := newAdminFixture(t)
	ctx := context.Background()
	cs := asst.Chats()
	seed := map[string][]string{
		"s-tape":    {"The SEAM tape is peeling", ""},
		"s-invoice": {"Can you fix my invoice?", "I can't change an invoice myself."},
		"s-dots":    {"is a.b the same as axb?", "No."},
	}
	for sid, msgs := range seed {
		if _, err := cs.CreateIfAbsent(ctx, sid, "assistant", "h", "", primitive.NilObjectID, "", nil); err != nil {
			t.Fatalf("seed %s: %v", sid, err)
		}
		_, _ = cs.AppendMessage(ctx, sid, chats.Message{Role: "user", Content: msgs[0]}, "")
		if msgs[1] != "" {
			_, _ = cs.AppendMessage(ctx, sid, chats.Message{Role: "assistant", Content: msgs[1]}, "")
		}
	}
	search := func(q, view string) admin.ChatListResponse {
		w := httptest.NewRecorder()
		adm.HandlerListChats()(w, httptest.NewRequest(http.MethodGet, "/assistant/chats?view="+view+"&q="+url.QueryEscape(q), nil))
		var resp admin.ChatListResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return resp
	}
	if r := search("seam", ""); len(r.Items) != 1 || r.Items[0].SessionID != "s-tape" || r.Counts["all"] != 1 || r.Counts["noanswer"] != 1 {
		t.Errorf("seam: %d items, counts %v", len(r.Items), r.Counts)
	}
	if r := search("myself", ""); len(r.Items) != 1 || r.Items[0].SessionID != "s-invoice" {
		t.Errorf("an answer's text should match too: %d items", len(r.Items))
	}
	if r := search("a.b", ""); len(r.Items) != 1 || r.Items[0].SessionID != "s-dots" {
		t.Errorf("q is literal, not a pattern: %d items", len(r.Items))
	}
	if r := search("invoice", "noanswer"); len(r.Items) != 0 || r.Total == nil || *r.Total != 0 {
		t.Errorf("search applies within the view: %d items", len(r.Items))
	}
}
