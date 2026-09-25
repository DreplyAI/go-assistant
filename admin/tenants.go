package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/dreplyai/go-assistant"
	"github.com/redelay/go-framework/server/httputil"
)

// TenantListResponse is the GET /admin/assistant/tenants body. The
// default (env-configured) tenant is implicit and never listed.
type TenantListResponse struct {
	Items []*assistant.Tenant `json:"items"`
}

// TenantCreateInput is the POST body. Key is the only required field;
// everything else inherits sensible per-tenant derivations (see the
// Tenant type docs).
type TenantCreateInput struct {
	Key                string   `json:"key" validate:"required"`
	Name               string   `json:"name,omitempty"`
	FlowID             string   `json:"flowId,omitempty"`
	DeploymentID       string   `json:"deploymentId,omitempty"`
	DocsIndex          string   `json:"docsIndex,omitempty"`
	SuggestedQuestions []string `json:"suggestedQuestions,omitempty"`
	RateLimitPerMinute int      `json:"rateLimitPerMinute,omitempty"`
	HandoffEnabled     *bool    `json:"handoffEnabled,omitempty"`
	AnonymousAllowed   *bool    `json:"anonymousAllowed,omitempty"`
}

func (m *Module) handleListTenants(w http.ResponseWriter, r *http.Request) {
	items := m.asst.Tenants()
	if items == nil {
		items = []*assistant.Tenant{}
	}
	httputil.WriteJSON(w, http.StatusOK, TenantListResponse{Items: items})
}

func (m *Module) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	var body TenantCreateInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	t, err := m.asst.CreateTenant(r.Context(), assistant.Tenant{
		Key:                body.Key,
		Name:               body.Name,
		FlowID:             body.FlowID,
		DeploymentID:       body.DeploymentID,
		DocsIndex:          body.DocsIndex,
		SuggestedQuestions: body.SuggestedQuestions,
		RateLimitPerMinute: body.RateLimitPerMinute,
		HandoffEnabled:     body.HandoffEnabled,
		AnonymousAllowed:   body.AnonymousAllowed,
	})
	if err != nil {
		writeTenantErr(w, err)
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, t)
}

func (m *Module) handleGetTenant(w http.ResponseWriter, r *http.Request) {
	t := m.asst.TenantByKey(strings.ToLower(r.PathValue("key")))
	if t == nil {
		httputil.Error(w, http.StatusNotFound, "tenant not found")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, t)
}

func (m *Module) handleUpdateTenant(w http.ResponseWriter, r *http.Request) {
	var body assistant.TenantUpdate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	t, err := m.asst.UpdateTenant(r.Context(), strings.ToLower(r.PathValue("key")), body)
	if err != nil {
		writeTenantErr(w, err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, t)
}

func (m *Module) handleDeleteTenant(w http.ResponseWriter, r *http.Request) {
	if err := m.asst.DeleteTenant(r.Context(), strings.ToLower(r.PathValue("key"))); err != nil {
		writeTenantErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeTenantErr maps the tenant sentinel errors onto HTTP statuses.
func writeTenantErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, assistant.ErrTenantNotFound):
		httputil.Error(w, http.StatusNotFound, "tenant not found")
	case errors.Is(err, assistant.ErrTenantExists):
		httputil.Error(w, http.StatusConflict, "tenant key already exists")
	case errors.Is(err, assistant.ErrInvalidTenantKey):
		httputil.Error(w, http.StatusBadRequest, err.Error())
	default:
		httputil.Error(w, http.StatusInternalServerError, "tenant operation failed")
	}
}
