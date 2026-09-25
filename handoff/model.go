// Package handoff persists "talk to a human" requests and publishes
// an `assistant.handoff_requested` event for downstream notification
// flows. The publish happens through the framework's EventBus so
// notification is fully decoupled — a project ships (or customizes)
// a Studio flow that subscribes to the event and sends the email,
// pages on-call, writes to a CRM, whatever.
//
// Records live forever (no TTL) — they're business audit and can
// outlive the chat they reference.
package handoff

import (
	"time"

	"github.com/redelay/go-events/typed"
	"github.com/redelay/go-framework/crud"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Status values for a handoff request. Status transitions are
// monotonic — `dismissed` / `resolved` are terminal.
type Status string

const (
	StatusPending   Status = "pending"
	StatusContacted Status = "contacted"
	StatusResolved  Status = "resolved"
	StatusDismissed Status = "dismissed"
)

// Priority values. Free-form `urgent`/`low` would be tempting but
// keeping this a closed enum makes the admin UI picker and
// downstream alert routing stable.
type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
)

// IsValid reports whether p is a supported priority.
func (p Priority) IsValid() bool {
	switch p {
	case PriorityLow, PriorityNormal, PriorityHigh:
		return true
	}
	return false
}

// HandoffRequest is the Mongo document for a customer escalation.
type HandoffRequest struct {
	crud.BaseModel `bson:",inline"`

	// ChatID + SessionID both point at the originating chat. Two
	// fields because SessionID stays stable across the chat's TTL
	// collection rotation, while ChatID is what admin URLs use.
	ChatID    primitive.ObjectID `bson:"chat_id"     json:"chatId"`
	SessionID string             `bson:"session_id"  json:"sessionId"`

	// Tenant is the assistant-tenant key the handoff was filed
	// under. Empty = default tenant (and every pre-multi-tenant
	// record), so existing inbox data keeps working unchanged.
	Tenant string `bson:"tenant,omitempty" json:"tenant,omitempty"`

	// UserID set when the caller was authenticated at request time.
	UserID primitive.ObjectID `bson:"user_id,omitempty" json:"userId,omitempty"`

	// Email is required — a handoff without a contact channel is
	// unactionable.
	Email string `bson:"email"           json:"email"`
	Phone string `bson:"phone,omitempty" json:"phone,omitempty"`

	// Reason is the user's free-text explanation. Optional.
	Reason string `bson:"reason,omitempty" json:"reason,omitempty"`

	// Transcript is a snapshot of the chat at request time. We
	// copy rather than reference so the handoff is readable after
	// the chat TTL expires.
	Transcript string `bson:"transcript,omitempty" json:"transcript,omitempty"`

	Priority Priority `bson:"priority" json:"priority"`
	Status   Status   `bson:"status"   json:"status"`

	ContactedAt *time.Time         `bson:"contacted_at,omitempty" json:"contactedAt,omitempty"`
	ContactedBy primitive.ObjectID `bson:"contacted_by,omitempty" json:"contactedBy,omitempty"`
	Notes       string             `bson:"notes,omitempty"         json:"notes,omitempty"`
}

// HandoffRequestedPayload is the event payload. Published by
// handoff.Service.Request so downstream flows can route / notify
// without importing this package.
type HandoffRequestedPayload struct {
	HandoffID  string   `json:"handoffId"`
	ChatID     string   `json:"chatId"`
	SessionID  string   `json:"sessionId"`
	Tenant     string   `json:"tenant,omitempty"`
	UserID     string   `json:"userId,omitempty"`
	Email      string   `json:"email"`
	Phone      string   `json:"phone,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Priority   Priority `json:"priority"`
	Transcript string   `json:"transcript,omitempty"`
	// RequestedAt is RFC3339 — the receiving flow can render it
	// without additional parsing.
	RequestedAt string `json:"requestedAt"`
}

// HandoffRequestedEvent is the canonical event descriptor. Defined
// at package level so any producer can import `handoff.HandoffRequestedEvent`
// and any consumer can subscribe by name `assistant.handoff_requested`.
var HandoffRequestedEvent = typed.EventDefinition[HandoffRequestedPayload]{
	Name:        "assistant.handoff_requested",
	EntityType:  "assistant_handoff",
	Action:      "requested",
	Topic:       "assistant.handoff_requested",
	Description: "A chat user asked to be contacted by a human.",
}
