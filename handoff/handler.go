package handoff

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/redelay/go-framework/modules"
	"github.com/redelay/go-framework/modules/auth"
	"github.com/redelay/go-framework/server/httputil"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// HTTPInput is the public request body. Keep it narrow: enough for a
// customer to ask for a callback, nothing that needs admin gating.
type HTTPInput struct {
	SessionID string `json:"sessionId" validate:"required"`
	Email     string `json:"email"     validate:"required,email"`
	Phone     string `json:"phone,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Priority is optional; server defaults to "normal" when missing.
	Priority string `json:"priority,omitempty"`
}

// HTTPResponse echoes just enough so the frontend can show a
// confirmation + link the user back to their case. We intentionally
// do NOT echo the transcript — it's already in the user's chat
// window.
type HTTPResponse struct {
	HandoffID string `json:"handoffId"`
	Status    Status `json:"status"`
	Priority  Priority `json:"priority"`
}

// Handler wires a POST /assistant/handoff route that any authenticated
// OR anonymous caller (per the module's anonymous_allowed setting) can
// hit. Service does the heavy lifting — this handler is validation +
// shape translation.
type Handler struct {
	svc *Service
	// AnonymousAllowed mirrors the assistant.anonymous_allowed
	// setting. When false, unauthenticated requests return 401.
	AnonymousAllowed bool
}

// NewHandler builds a Handler around a live Service.
func NewHandler(svc *Service, anonymousAllowed bool) *Handler {
	return &Handler{svc: svc, AnonymousAllowed: anonymousAllowed}
}

// Handle is the HTTP handler func (signed as net/http so the caller
// can wrap with middleware however they like). Responses:
//
//	201 — request accepted + event published
//	400 — missing email, bad priority
//	401 — anonymous disallowed
//	409 — duplicate pending request for this session
//	500 — persistence failure
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	var body HTTPInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	var userID primitive.ObjectID
	claims, authed := auth.ClaimsFromContext(r.Context())
	if authed {
		if oid, err := primitive.ObjectIDFromHex(claims.UserID); err == nil {
			userID = oid
		}
	}
	if !authed && !h.AnonymousAllowed {
		httputil.Error(w, http.StatusUnauthorized, "login required")
		return
	}

	priority := Priority(body.Priority)
	if priority == "" {
		priority = PriorityNormal
	}

	rec, err := h.svc.Request(r.Context(), RequestInput{
		SessionID: body.SessionID,
		UserID:    userID,
		Email:     body.Email,
		Phone:     body.Phone,
		Reason:    body.Reason,
		Priority:  priority,
	})
	if errors.Is(err, ErrDuplicate) {
		httputil.Error(w, http.StatusConflict, "a pending handoff already exists for this session")
		return
	}
	if err != nil {
		if err.Error() == "handoff: email is required" || err.Error() == "handoff: sessionID is required" || err.Error() == "handoff: invalid priority" {
			httputil.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		httputil.Error(w, http.StatusInternalServerError, "could not register handoff")
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, HTTPResponse{
		HandoffID: rec.GetID().Hex(),
		Status:    rec.Status,
		Priority:  rec.Priority,
	})
}

// RouteOptions describes the endpoint to the OpenAPI generator.
func RouteOptions() []modules.EndpointOption {
	return []modules.EndpointOption{
		modules.Summary("Request a human handoff"),
		modules.Description("Persists a callback request for the current chat session and publishes " +
			"the assistant.handoff_requested event. A notification flow (email, Slack, CRM) " +
			"subscribes to that event to deliver the alert — this endpoint never sends email itself."),
		modules.Body(HTTPInput{}),
		modules.Response(201, "Handoff created", HTTPResponse{}),
		modules.Response(400, "Missing required fields", modules.RedelayErrorResponse{}),
		modules.Response(409, "Duplicate pending request", modules.RedelayErrorResponse{}),
	}
}
