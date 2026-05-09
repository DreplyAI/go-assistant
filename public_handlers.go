package assistant

import (
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"strings"

	"github.com/dreplyai/go-assistant/chats"
	"github.com/dreplyai/go-assistant/handoff"
	flowstore "github.com/redelay/go-flowdsl/flowexec/store"
	"github.com/redelay/go-framework/modules/auth"
	"github.com/redelay/go-framework/server/httputil"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// handleConfig returns the assistant's public runtime config. The
// frontend calls this once on widget mount to decide whether to
// render the handoff button, which variant the session lands on,
// etc.
//
// Variant selection is sticky on session_id — the caller passes one
// in the `?sessionId=` query so the widget sees the same variant
// on reloads as its chat runs will. No sessionId → non-sticky
// prng-driven pick, fine for the very first mount before a session
// has been minted.
func (m *Module) handleConfig(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.URL.Query().Get("sessionId"))
	meta := map[string]any{}
	if sessionID != "" {
		meta["session_id"] = sessionID
	}

	// When no sessionId is provided yet, describe the most-likely
	// variant the user will bucket into. Pass a zero-returning prng
	// so the CDF walk picks the first positive-weight variant
	// (stable, by convention). Deterministic across calls so the
	// widget's "what can I do" check doesn't flap.
	prng := func() float64 { return 0 }
	if sessionID != "" {
		// With a real session the sticky path wins; prng is unused.
		prng = rand.Float64
	}
	resolved, rerr := flowstore.ResolveDeployment(r.Context(), m.flowexec.Store(), m.cfg.DeploymentID, meta, prng, m.roleChecker)
	caps := AssistantCapabilities{}
	flowID := m.cfg.FlowID
	activeVariant := ""
	if rerr == nil && resolved != nil && resolved.Version != nil {
		caps = capabilitiesFromWorkflow(resolved.Version.Document)
		flowID = resolved.FlowID
		activeVariant = resolved.Label
	}

	// Back-compat: HandoffEnabled is true when the active variant's
	// flow has the node AND the service is wired AND env allows it.
	// Any of the three missing hides the button.
	handoffEnabled := caps.Handoff && m.handoff != nil && m.cfg.HandoffEnabled
	httputil.WriteJSON(w, http.StatusOK, ConfigResponse{
		DeploymentID:     m.cfg.DeploymentID,
		ActiveVariant:    activeVariant,
		FlowID:           flowID,
		FlowName:         "Project Assistant",
		HandoffEnabled:   handoffEnabled,
		AnonymousAllowed: m.cfg.AnonymousAllowed,
		ChatTTLDays:      m.cfg.ChatTTLDays,
		Capabilities:     caps,
	})
}

// handleChatByMe returns the chat document for the given session id.
// Security model: the session id IS the capability — clients hold it
// in localStorage and we never leak it anywhere. Lets the frontend
// restore history on page load without authentication.
func (m *Module) handleChatByMe(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("sessionID"))
	if sessionID == "" {
		httputil.Error(w, http.StatusBadRequest, "sessionID is required")
		return
	}
	if m.chats == nil {
		// No persistence in this deployment — treat every client
		// as fresh. Shaped like "not found" so the UI does the
		// right thing.
		httputil.Error(w, http.StatusNotFound, "chat not found")
		return
	}
	c, err := m.chats.GetBySessionID(r.Context(), sessionID)
	if errors.Is(err, chats.ErrNotFound) {
		httputil.Error(w, http.StatusNotFound, "chat not found")
		return
	}
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, "chat lookup failed")
		return
	}

	resp := ChatResponse{
		SessionID:        c.SessionID,
		StartedAt:        c.StartedAt.Format("2006-01-02T15:04:05Z07:00"),
		LastAt:           c.LastAt.Format("2006-01-02T15:04:05Z07:00"),
		Messages:         make([]ChatMessage, 0, len(c.Messages)),
		HandoffRequested: !c.HandoffID.IsZero(),
	}
	for _, msg := range c.Messages {
		resp.Messages = append(resp.Messages, ChatMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// handleHandoff is the public escalation endpoint. Anonymous or
// authenticated (per cfg.AnonymousAllowed). Calls handoff.Service
// which persists + publishes the event.
func (m *Module) handleHandoff(w http.ResponseWriter, r *http.Request) {
	if !m.cfg.HandoffEnabled || m.handoff == nil {
		httputil.Error(w, http.StatusServiceUnavailable, "handoff disabled")
		return
	}
	var body HandoffInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Anonymous gate — when disallowed, require a valid JWT.
	var userID primitive.ObjectID
	claims, authed := auth.ClaimsFromContext(r.Context())
	if authed {
		if oid, err := primitive.ObjectIDFromHex(claims.UserID); err == nil {
			userID = oid
		}
	}
	if !authed && !m.cfg.AnonymousAllowed {
		httputil.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	priority := handoff.Priority(strings.ToLower(strings.TrimSpace(body.Priority)))
	if priority == "" {
		priority = handoff.PriorityNormal
	}

	rec, err := m.handoff.Request(r.Context(), handoff.RequestInput{
		SessionID: body.SessionID,
		UserID:    userID,
		Email:     body.Email,
		Phone:     body.Phone,
		Reason:    body.Reason,
		Priority:  priority,
	})
	if errors.Is(err, handoff.ErrDuplicate) {
		httputil.Error(w, http.StatusConflict, "a pending handoff already exists for this session")
		return
	}
	if err != nil {
		// Specific validation errors are user-facing — anything
		// else is a 500.
		msg := err.Error()
		if strings.HasPrefix(msg, "handoff: ") {
			httputil.Error(w, http.StatusBadRequest, strings.TrimPrefix(msg, "handoff: "))
			return
		}
		httputil.Error(w, http.StatusInternalServerError, "could not register handoff")
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, HandoffResponse{
		HandoffID: rec.GetID().Hex(),
		Status:    string(rec.Status),
		Priority:  string(rec.Priority),
	})
}
