package admin

import (
	"encoding/json"

	"github.com/dreplyai/go-assistant-core/adminkit"
	"github.com/dreplyai/go-assistant/chats"
	"github.com/dreplyai/go-assistant/handoff"
)

// ChatListResponse is the GET /admin/assistant/chats body.
type ChatListResponse struct {
	Items      []*chats.Chat `json:"items"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// HandoffListResponse is the GET /admin/assistant/handoffs body.
type HandoffListResponse struct {
	Items      []*handoff.HandoffRequest `json:"items"`
	NextCursor string                    `json:"nextCursor,omitempty"`
}

// HandoffUpdateInput is the PATCH body. Status is validated against
// the closed enum; notes are free-form.
type HandoffUpdateInput struct {
	Status string `json:"status" validate:"required,oneof=pending contacted resolved dismissed"`
	Notes  string `json:"notes,omitempty"`
}

// ResetResponse is the body of POST <admin-prefix>/assistant/reset.
type ResetResponse struct {
	FlowID   string `json:"flowID"`
	Reset    bool   `json:"reset"`
	Template string `json:"template,omitempty"`
}

// ChatDetailResponse is the enriched body returned by GET
// /admin/assistant/chats/{id}. Embeds the chat doc so existing
// clients that consumed the flat shape stay working (Go JSON
// inlines embedded structs); `flowName` and `usage` are
// additional fields any new client can opt into.
type ChatDetailResponse struct {
	*chats.Chat
	FlowName string           `json:"flowName,omitempty"`
	Usage    ChatUsageSummary `json:"usage"`
}

// MarshalJSON is required because chats.Chat implements its own
// MarshalJSON (to suppress the zero-OID userId/handoffId on the wire).
// Without this, Go's field-promotion rules would surface the embedded
// Chat's MarshalJSON through ChatDetailResponse, and our FlowName +
// Usage fields would silently drop off the response — one of those
// quiet bugs where the UI renders "$0.00" next to every chat and no
// server log ever fires.
//
// The strategy is a two-step merge: marshal the embedded Chat, parse
// it back as a key→rawMessage map (so we inherit its userId/handoffId
// suppression for free), then graft flowName + usage on top.
func (r ChatDetailResponse) MarshalJSON() ([]byte, error) {
	if r.Chat == nil {
		// Defensive — admin handlers always set Chat, but a zero-value
		// response shouldn't panic the encoder.
		return json.Marshal(struct {
			FlowName string           `json:"flowName,omitempty"`
			Usage    ChatUsageSummary `json:"usage"`
		}{r.FlowName, r.Usage})
	}
	chatJSON, err := json.Marshal(r.Chat)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(chatJSON, &m); err != nil {
		return nil, err
	}
	if r.FlowName != "" {
		if b, err := json.Marshal(r.FlowName); err == nil {
			m["flowName"] = b
		}
	}
	if b, err := json.Marshal(r.Usage); err == nil {
		m["usage"] = b
	}
	return json.Marshal(m)
}

// ChatUsageSummary is the per-chat usage aggregate — the shared adminkit roll-up
// (Total across every runId + per-model breakdown). Aliased so the wire shape
// and existing clients stay unchanged.
type ChatUsageSummary = adminkit.UsageSummary
