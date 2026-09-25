package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dreplyai/go-assistant"
	"github.com/dreplyai/go-assistant-core/adminkit"
	"github.com/dreplyai/go-assistant/chats"
	"github.com/dreplyai/go-assistant/handoff"
	"github.com/redelay/go-framework/modules"
	"github.com/redelay/go-framework/modules/auth"
	"github.com/redelay/go-framework/server/httputil"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

// Routes registers every admin endpoint under <AdminPrefix>/assistant/*.
// Default prefix is /admin; hosts relocate the admin surface via
// ADMIN_ROUTE_PREFIX. The prefix also avoids a chi Mount collision
// with the core assistant module which owns /assistant/* on the
// same router — mirrors users/admin + groups/admin.
func (m *Module) Routes(r modules.Router) {
	r.Group(modules.AdminPrefix()+"/assistant", func(g modules.Router) {
		g.Use(auth.RequireActiveUser, auth.RequirePermission("admin:access"))

		g.Handle("GET", "/chats", http.HandlerFunc(m.handleListChats),
			modules.Summary("List assistant chats"),
			modules.Description("Filters: userId, flowId, variantLabel. Cursor is the last-seen chat _id."),
			modules.Response(200, "Paginated list", ChatListResponse{}),
			modules.Security("BearerAuth"),
		)
		g.Handle("GET", "/chats/{id}", http.HandlerFunc(m.handleGetChat),
			modules.Summary("Fetch chat by id"),
			modules.Response(200, "Chat", chats.Chat{}),
			modules.Response(404, "Not found", modules.RedelayErrorResponse{}),
			modules.Security("BearerAuth"),
		)
		g.Handle("GET", "/handoffs", http.HandlerFunc(m.handleListHandoffs),
			modules.Summary("List handoff requests"),
			modules.Description("Default status filter: pending."),
			modules.Response(200, "Paginated list", HandoffListResponse{}),
			modules.Security("BearerAuth"),
		)
		g.Handle("GET", "/handoffs/{id}", http.HandlerFunc(m.handleGetHandoff),
			modules.Summary("Fetch handoff by id"),
			modules.Response(200, "Handoff", handoff.HandoffRequest{}),
			modules.Response(404, "Not found", modules.RedelayErrorResponse{}),
			modules.Security("BearerAuth"),
		)
		g.Handle("PATCH", "/handoffs/{id}", http.HandlerFunc(m.handleUpdateHandoff),
			modules.Summary("Update handoff status / notes"),
			modules.Body(HandoffUpdateInput{}),
			modules.Response(200, "Updated handoff", handoff.HandoffRequest{}),
			modules.Response(400, "Invalid body", modules.RedelayErrorResponse{}),
			modules.Response(404, "Not found", modules.RedelayErrorResponse{}),
			modules.Security("BearerAuth"),
		)
		// Corpora — the document-level view. The search admin browses chunks;
		// these answer "which documents do we have" and "what is the floor
		// throwing away", neither of which is a question about a chunk.
		g.Handle("GET", "/corpora", http.HandlerFunc(m.handleListCorpora),
			modules.Summary("List registered corpora with live counts"),
		)
		g.Handle("GET", "/corpora/{name}/documents", http.HandlerFunc(m.handleListCorpusDocs),
			modules.Summary("Browse a corpus's documents"),
		)
		g.Handle("GET", "/corpora/{name}/documents/{id}", http.HandlerFunc(m.handleGetCorpusDoc),
			modules.Summary("One document, with provenance and licence"),
		)
		g.Handle("POST", "/corpora/{name}/test-query", http.HandlerFunc(m.handleCorpusTestQuery),
			modules.Summary("Retrieve with the floor applied, showing dropped hits"),
		)

		// Tenants — the multi-tenant registry. Each tenant is a
		// separate assistant (own flow, corpus, inbox scoping)
		// selected per request via X-Assistant-Site / ?site= / the
		// TenantResolver seam. The default tenant is env-configured
		// and implicit — it never appears in these listings.
		g.Handle("GET", "/tenants", http.HandlerFunc(m.handleListTenants),
			modules.Summary("List assistant tenants"),
			modules.Response(200, "Registered tenants", TenantListResponse{}),
			modules.Security("BearerAuth"),
		)
		g.Handle("POST", "/tenants", http.HandlerFunc(m.handleCreateTenant),
			modules.Summary("Create an assistant tenant"),
			modules.Body(TenantCreateInput{}),
			modules.Response(201, "Tenant created", assistant.Tenant{}),
			modules.Response(400, "Invalid key / body", modules.RedelayErrorResponse{}),
			modules.Response(409, "Key already exists", modules.RedelayErrorResponse{}),
			modules.Security("BearerAuth"),
		)
		g.Handle("GET", "/tenants/{key}", http.HandlerFunc(m.handleGetTenant),
			modules.Summary("Fetch tenant by key"),
			modules.Response(200, "Tenant", assistant.Tenant{}),
			modules.Response(404, "Not found", modules.RedelayErrorResponse{}),
			modules.Security("BearerAuth"),
		)
		g.Handle("PATCH", "/tenants/{key}", http.HandlerFunc(m.handleUpdateTenant),
			modules.Summary("Update tenant fields (key is immutable)"),
			modules.Body(assistant.TenantUpdate{}),
			modules.Response(200, "Updated tenant", assistant.Tenant{}),
			modules.Response(404, "Not found", modules.RedelayErrorResponse{}),
			modules.Security("BearerAuth"),
		)
		g.Handle("DELETE", "/tenants/{key}", http.HandlerFunc(m.handleDeleteTenant),
			modules.Summary("Delete a tenant (chats/handoffs history is kept)"),
			modules.Response(204, "Deleted", nil),
			modules.Response(404, "Not found", modules.RedelayErrorResponse{}),
			modules.Security("BearerAuth"),
		)

		g.Handle("POST", "/reset", http.HandlerFunc(m.handleReset),
			modules.Summary("Reset the assistant flow to one of the embedded templates"),
			modules.Description(
				"Overwrites the live flow with a fresh version seeded from one "+
					"of the module's embedded templates (minimal / default / "+
					"production / rag). Requires admin:access permission.",
			),
			modules.Response(200, "Reset complete", ResetResponse{}),
			modules.Response(400, "Unknown template", modules.RedelayErrorResponse{}),
			modules.Response(403, "Admin access required", modules.RedelayErrorResponse{}),
			modules.Security("BearerAuth"),
		)
		// Template discovery and create-flow-from-template are served
		// by flowexec's admin surface:
		//   GET  /admin/flows/templates                                list all
		//   GET  /admin/flows/templates/{id}                           single (with document)
		//   POST /admin/deployments/{id}/variants/from-template        one-shot
		//   POST /admin/flows + POST /admin/flows/{id}/versions        two-step create
		// The assistant module participates by implementing
		// flowexec.TemplateProvider (see assistant.Module.Templates)
		// so all 11 embedded variants show up in the generic catalog
		// with the "assistant/" id prefix.
	})
}

func (m *Module) handleListChats(w http.ResponseWriter, r *http.Request) {
	svc := m.asst.Chats()
	if svc == nil {
		httputil.WriteJSON(w, http.StatusOK, ChatListResponse{Items: []*chats.Chat{}})
		return
	}
	q := r.URL.Query()
	filter := chats.ListFilter{
		FlowID:       q.Get("flowId"),
		VariantLabel: q.Get("variantLabel"),
		Tenant:       q.Get("tenant"), // optional; empty = all tenants
	}
	if uid := q.Get("userId"); uid != "" {
		if oid, err := primitive.ObjectIDFromHex(uid); err == nil {
			filter.UserID = oid
		}
	}
	limit := 20
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	items, next, err := svc.List(r.Context(), filter, limit, q.Get("cursor"))
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, "list chats failed")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, ChatListResponse{Items: items, NextCursor: next})
}

func (m *Module) handleGetChat(w http.ResponseWriter, r *http.Request) {
	svc := m.asst.Chats()
	if svc == nil {
		httputil.Error(w, http.StatusNotFound, "not found")
		return
	}
	oid, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid id")
		return
	}
	c, err := svc.GetByID(r.Context(), oid)
	if errors.Is(err, chats.ErrNotFound) {
		httputil.Error(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, "lookup failed")
		return
	}

	// Enrich: flow name + LLM usage aggregate. Both are best-effort
	// — failures degrade to missing fields rather than erroring the
	// whole response, since the core chat transcript is still the
	// primary thing the operator came here to see.
	resp := ChatDetailResponse{Chat: c}
	resp.FlowName = m.lookupFlowName(r.Context(), c.FlowID)
	resp.Usage = m.aggregateUsage(r.Context(), c)

	httputil.WriteJSON(w, http.StatusOK, resp)
}

// lookupFlowName resolves the human-readable flow name from the
// flowexec store. Returns empty string on any error so the frontend
// falls back to displaying the flow id. flowexec is an optional
// dependency — when unbound we silently skip.
func (m *Module) lookupFlowName(ctx context.Context, flowID string) string {
	if m.flowexec == nil || flowID == "" {
		return ""
	}
	f, err := m.flowexec.Store().GetFlow(ctx, flowID)
	if err != nil || f == nil {
		return ""
	}
	return f.Name
}

// aggregateUsage rolls up LLM usage for every run in the chat via the shared
// adminkit roll-up: exact runIds first, then a padded flow + time-window
// fallback. Returns a non-nil zero summary when the ledger isn't bound.
func (m *Module) aggregateUsage(ctx context.Context, c *chats.Chat) ChatUsageSummary {
	if c == nil || m.ledger == nil {
		return adminkit.ZeroUsage()
	}
	return adminkit.Aggregate(ctx, m.ledger, adminkit.UsageWindow{
		FlowID: c.FlowID,
		RunIDs: c.RunIDs,
		From:   c.StartedAt,
		To:     c.LastAt,
	})
}

func (m *Module) handleListHandoffs(w http.ResponseWriter, r *http.Request) {
	svc := m.asst.Handoff()
	if svc == nil {
		httputil.WriteJSON(w, http.StatusOK, HandoffListResponse{Items: []*handoff.HandoffRequest{}})
		return
	}
	q := r.URL.Query()
	status := handoff.Status(q.Get("status"))
	if status == "" {
		status = handoff.StatusPending // default view
	}
	limit := 20
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	items, next, err := svc.ListFiltered(r.Context(), handoff.ListFilter{
		Status: status,
		Tenant: q.Get("tenant"), // optional; empty = all tenants
	}, limit, q.Get("cursor"))
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, "list handoffs failed")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, HandoffListResponse{Items: items, NextCursor: next})
}

func (m *Module) handleGetHandoff(w http.ResponseWriter, r *http.Request) {
	svc := m.asst.Handoff()
	if svc == nil {
		httputil.Error(w, http.StatusNotFound, "not found")
		return
	}
	oid, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid id")
		return
	}
	h, err := svc.GetByID(r.Context(), oid)
	if errors.Is(err, handoff.ErrNotFound) {
		httputil.Error(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, h)
}

// handleReset overwrites the live flow with one of the module's
// embedded templates.
//
// Query params (template tiers):
//
//	template=minimal     — T1. start → llm → end. Dev-only.
//	template=default     — T2 (implicit). + guards + refusals + handoff.
//	template=production  — T3. + trust-and-safety email fan-out + [[ESCALATE]].
//	template=rag         — T4. + redelay/assistant-rag-context (docs-grounded).
//
// The endpoint creates a fresh version from the chosen template and
// publishes it atomically. Existing versions stay — you can roll back
// via /flows/assistant/rollback if the new variant misbehaves.
func (m *Module) handleReset(w http.ResponseWriter, r *http.Request) {
	template := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("template")))
	if template == "" {
		template = "default"
	}
	if err := m.asst.ReseedFromTemplate(r.Context(), template); err != nil {
		if logger := m.asst.Logger(); logger != nil {
			logger.Error("assistant-admin: reset", zap.Error(err), zap.String("template", template))
		}
		if errors.Is(err, assistant.ErrUnknownTemplate) {
			httputil.Error(w, http.StatusBadRequest, "unknown template (valid: minimal, default, production, rag)")
			return
		}
		httputil.Error(w, http.StatusInternalServerError, "reset failed")
		return
	}
	cfg := m.asst.Cfg()
	httputil.WriteJSON(w, http.StatusOK, ResetResponse{FlowID: cfg.FlowID, Reset: true, Template: template})
}

func (m *Module) handleUpdateHandoff(w http.ResponseWriter, r *http.Request) {
	svc := m.asst.Handoff()
	if svc == nil {
		httputil.Error(w, http.StatusServiceUnavailable, "handoff service unavailable")
		return
	}
	oid, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body HandoffUpdateInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// Derive ContactedBy from JWT.
	var contactedBy primitive.ObjectID
	if claims, ok := auth.ClaimsFromContext(r.Context()); ok {
		if cb, err := primitive.ObjectIDFromHex(claims.UserID); err == nil {
			contactedBy = cb
		}
	}
	h, err := svc.UpdateStatus(r.Context(), oid, handoff.UpdateStatusInput{
		Status:      handoff.Status(body.Status),
		Notes:       body.Notes,
		ContactedBy: contactedBy,
	})
	if errors.Is(err, handoff.ErrNotFound) {
		httputil.Error(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, "update failed")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, h)
}

// ── Test hooks ---------------------------------------------------------------

// HandlerListChats exposes the list-chats handler for tests; kept
// stable because the admin surface rarely changes shape.
func (m *Module) HandlerListChats() http.HandlerFunc { return m.handleListChats }

// HandlerGetChat exposes the get-chat handler for tests.
func (m *Module) HandlerGetChat() http.HandlerFunc { return m.handleGetChat }

// HandlerListHandoffs exposes the list-handoffs handler for tests.
func (m *Module) HandlerListHandoffs() http.HandlerFunc { return m.handleListHandoffs }

// HandlerGetHandoff exposes the get-handoff handler for tests.
func (m *Module) HandlerGetHandoff() http.HandlerFunc { return m.handleGetHandoff }

// HandlerUpdateHandoff exposes the PATCH handler for tests.
func (m *Module) HandlerUpdateHandoff() http.HandlerFunc { return m.handleUpdateHandoff }

// HandlerReset exposes the reset handler for tests.
func (m *Module) HandlerReset() http.HandlerFunc { return m.handleReset }
