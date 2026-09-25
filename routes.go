package assistant

import (
	"net/http"

	"github.com/redelay/go-framework/modules"
)

// Wire-types for the HTTP API. Intentionally narrow — the frontend knows
// nothing about flows, runs, or nodes; it just POSTs {messages} and reads
// an SSE stream of {content}/{done}/{error} frames.

// ChatMessage is one turn in a conversation history.
type ChatMessage struct {
	Role    string `json:"role"    validate:"required"` // user|assistant|system
	Content string `json:"content" validate:"required"`
}

// ChatRequest is the body of POST /assistant/messages. SessionID is
// optional — when missing the server generates one + returns it via
// X-Assistant-Session-Id so the client can persist it to localStorage.
type ChatRequest struct {
	Messages  []ChatMessage  `json:"messages" validate:"required"`
	Meta      map[string]any `json:"meta,omitempty"`
	SessionID string         `json:"sessionId,omitempty"`
}

// Action is a UI-triggerable suggestion that rides along with an
// assistant reply. Every action on the wire — no matter where it came
// from (docs frontmatter via RAG, an intent-classifier node, a
// tool-calling LLM) — normalises to this single shape so the widget
// has one code path to render and one registry to dispatch clicks.
//
// This is the extension contract for ecommerce / domains / any other
// project reusing the assistant: author a docs page with
// `action: { type: "open_product", payload: {...} }` frontmatter,
// register a handler in the widget for that type, done. No backend
// code for new action types.
//
// Field notes:
//   - Type is free-form — "handoff", "open_url", "open_product",
//     "add_to_cart"… the widget's handler registry keys off this.
//   - Payload is opaque JSON — type-specific. The server never
//     introspects it.
//   - Source records the provenance (rag|intent|tool) so analytics
//     can A/B which surface drives most clicks.
type Action struct {
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type"`
	Label    string         `json:"label"`
	Icon     string         `json:"icon,omitempty"`
	Payload  map[string]any `json:"payload,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Priority string         `json:"priority,omitempty"`
	Source   string         `json:"source,omitempty"`
}

// ChatFrame is one SSE frame in the streaming response.
// The UI only needs `content`, `done`, and `error`. `trace` is forwarded
// verbatim for subscribers that want to render per-node activity.
// `sources` rides alongside the content frame when the flow used a RAG
// node — each entry is a citation the UI can render as a clickable pill.
// `actions` carries zero-or-more UI-triggerable suggestions (handoff,
// open product, add to cart…) — deduped by type so the same action
// doesn't appear twice when multiple retrieved chunks suggest it.
type ChatFrame struct {
	Content string           `json:"content,omitempty"`
	Done    bool             `json:"done,omitempty"`
	Error   string           `json:"error,omitempty"`
	Trace   map[string]any   `json:"trace,omitempty"`
	Sources []map[string]any `json:"sources,omitempty"`
	Actions []Action         `json:"actions,omitempty"`
	RunID   string           `json:"runId,omitempty"`
}

// Routes implements modules.RoutesProvider. Public routes only —
// admin listings live in the `admin` sub-module and are blank-imported
// by admin-api.
func (m *Module) Routes(r modules.Router) {
	r.Group("/assistant", func(g modules.Router) {
		g.Handle("POST", "/messages", http.HandlerFunc(m.handleChat),
			modules.Summary("Chat with the project assistant"),
			modules.Description(
				"Streams the assistant's response as server-sent events. "+
					"The underlying FlowDSL flow is resolved per-request from "+
					"the flowexec store — edits to that flow take effect on "+
					"the next message without a restart. Returns the "+
					"X-Assistant-Session-Id response header so callers can "+
					"persist the server-assigned session id when they omit "+
					"one in the request.",
			),
			modules.Body(ChatRequest{}),
			modules.Response(200, "SSE stream of chat frames", ChatFrame{}),
			modules.Response(400, "Invalid body", modules.RedelayErrorResponse{}),
			modules.Response(409, "Assistant flow has no published version", modules.RedelayErrorResponse{}),
			modules.Response(502, "Flow run failed", modules.RedelayErrorResponse{}),
		)

		// Read-own chat history by session id. Anonymous by design —
		// the session id is the capability; guessing it is impractical
		// (UUID v4) and there's nothing sensitive beyond the messages
		// the same client just sent. Admins use /admin/chats listing.
		g.Handle("GET", "/chats/me/{sessionID}", http.HandlerFunc(m.handleChatByMe),
			modules.Summary("Fetch chat history by session id"),
			modules.Response(200, "Chat found", ChatResponse{}),
			modules.Response(404, "Not found", modules.RedelayErrorResponse{}),
		)

		// Handoff — customer asks for a human. Validated + dedup-guarded
		// inside handoff.Service.
		g.Handle("POST", "/handoff", http.HandlerFunc(m.handleHandoff),
			modules.Summary("Request a human handoff for the current chat"),
			modules.Description(
				"Persists a callback request and publishes the "+
					"assistant.handoff_requested event. A notification "+
					"flow subscribes to that event to deliver the alert "+
					"— this endpoint never sends email itself.",
			),
			modules.Body(HandoffInput{}),
			modules.Response(201, "Handoff created", HandoffResponse{}),
			modules.Response(400, "Missing email / bad priority", modules.RedelayErrorResponse{}),
			modules.Response(401, "Authentication required", modules.RedelayErrorResponse{}),
			modules.Response(409, "Duplicate pending request", modules.RedelayErrorResponse{}),
		)

		// Runtime config — frontend reads this to render the widget
		// state correctly (handoff button visibility, anonymous chat
		// messaging, TTL note). Public and cheap.
		g.Handle("GET", "/config", http.HandlerFunc(m.handleConfig),
			modules.Summary("Assistant runtime configuration for the frontend"),
			modules.Response(200, "Current config", ConfigResponse{}),
		)
	})
}

// --- Wire types for the new endpoints ---

// HandoffInput is the body for POST /assistant/handoff. Narrow set
// — admin status changes + notes live on the admin PATCH endpoint.
type HandoffInput struct {
	SessionID string `json:"sessionId" validate:"required"`
	Email     string `json:"email"     validate:"required,email"`
	Phone     string `json:"phone,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Priority  string `json:"priority,omitempty" validate:"omitempty,oneof=low normal high"`
}

// HandoffResponse echoes the persisted record id + final status so
// the UI can show "submitted — we'll be in touch" and link to a
// customer-facing status page later.
type HandoffResponse struct {
	HandoffID string `json:"handoffId"`
	Status    string `json:"status"`
	Priority  string `json:"priority"`
}

// ChatResponse is the shape of GET /chats/me/{sessionID}.
type ChatResponse struct {
	SessionID string        `json:"sessionId"`
	StartedAt string        `json:"startedAt"`
	LastAt    string        `json:"lastAt"`
	Messages  []ChatMessage `json:"messages"`
	// HandoffRequested is true when the chat already has an open
	// handoff record — lets the UI hide the button.
	HandoffRequested bool `json:"handoffRequested,omitempty"`
}

// ConfigResponse describes the assistant's public runtime config.
//
// `HandoffEnabled` stays on the top level for backward compatibility
// (older widgets read it directly). `Capabilities` mirrors the
// active variant's flow topology so the widget renders buttons only
// for features the flow actually implements.
//
// `DeploymentID`, `ActiveVariant`, and `FlowID` give the widget
// everything it needs for A/B telemetry — tag every message with
// the variant it ran through without a second round-trip.
type ConfigResponse struct {
	// Tenant is the resolved tenant key ("" = default tenant), so
	// the widget can tag telemetry and confirm which assistant it
	// is talking to. Omitted for the default tenant — the wire
	// shape v0.2.x widgets see is byte-identical.
	Tenant string `json:"tenant,omitempty"`
	// DeploymentID is the named binding the module runs against.
	DeploymentID string `json:"deploymentId"`
	// ActiveVariant is the label the current session bucketed into
	// ("stable", "canary", "experiment_a", …). Stable across
	// reloads: same session, same bucket.
	ActiveVariant string `json:"activeVariant,omitempty"`
	// FlowID is the concrete flow id the active variant points at.
	// Exposed so widget telemetry can segment by flow, not just by
	// variant label.
	FlowID           string                `json:"flowId"`
	FlowName         string                `json:"flowName,omitempty"`
	HandoffEnabled   bool `json:"handoffEnabled"`
	AnonymousAllowed bool `json:"anonymousAllowed"`
	ChatTTLDays      int  `json:"chatTtlDays"`
	// SuggestedQuestions are per-tenant conversation starters the
	// widget can render before the first message. Empty for the
	// default tenant unless the host sets them via tenant config.
	SuggestedQuestions []string              `json:"suggestedQuestions,omitempty"`
	Capabilities       AssistantCapabilities `json:"capabilities"`
}

// AssistantCapabilities is derived from the published flow document.
// Empty values mean "the flow does not currently implement this
// branch" — the widget should hide the corresponding UI affordance.
//
// Adding a new capability here is a two-step dance: (1) add a field,
// (2) teach deriveCapabilities to detect the node that provides it.
// The UI only renders what this object reports, so a flow that drops
// a node automatically hides the matching button.
type AssistantCapabilities struct {
	// Handoff is true when the flow contains a redelay/assistant-handoff-request
	// node reachable from the start. The widget renders "Talk to a
	// human" only when this is true.
	Handoff bool `json:"handoff"`
	// Escalation is true when the flow has an email escalation fan-out
	// (e.g. redelay/email-send on a conditional edge from chat). Lets
	// the widget hint "a human will be notified on critical issues".
	Escalation bool `json:"escalation"`
	// Streaming is true when the LLM node is configured to stream
	// per-token packets. Widget decides whether to render tokens as
	// they arrive or wait for the full message.
	Streaming bool `json:"streaming"`
}
