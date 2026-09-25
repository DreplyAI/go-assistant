// Package chats persists assistant conversations with a TTL. One Chat
// document per client session_id — the client keeps that id in
// localStorage so reconnecting picks the history back up. The TTL is
// per-write: every append bumps ExpiresAt forward so active chats
// don't get GC'd mid-session, while dormant ones fall off after
// ASSISTANT_CHAT_TTL_DAYS.
//
// Handoff records live in a sibling package and reference chats by
// ObjectID so an admin viewing a handoff can still pull the
// (pre-TTL) transcript.
package chats

import (
	"encoding/json"
	"time"

	"github.com/redelay/go-framework/crud"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Message is a single turn in a conversation. Role mirrors the OpenAI
// / Anthropic chat convention so the wire shape the Vue component
// already POSTs needs no conversion.
type Message struct {
	Role      string    `bson:"role"    json:"role"`
	Content   string    `bson:"content" json:"content"`
	Timestamp time.Time `bson:"ts"      json:"timestamp"`
	// RunID ties an assistant message back to the flow run that
	// produced it — lets admins correlate a conversation turn with
	// its flow events + LLM calls + cost.
	RunID string `bson:"run_id,omitempty" json:"runId,omitempty"`
}

// Chat is the persisted conversation document. The collection is
// indexed on session_id (unique) so POST /assistant/messages can
// upsert idempotently; TTL on expires_at means stale chats are
// cleaned automatically.
type Chat struct {
	crud.BaseModel `bson:",inline"`

	// SessionID is supplied by the client. UUID v4 from localStorage;
	// server generates one if missing and returns it via response
	// header so the client can persist it.
	SessionID string `bson:"session_id"      json:"sessionId"`

	// Tenant is the assistant-tenant key this chat belongs to. Empty
	// means the default (env-configured) tenant — which is also what
	// every pre-multi-tenant document decodes to, so existing data
	// keeps working without a migration.
	Tenant string `bson:"tenant,omitempty" json:"tenant,omitempty"`

	// UserID is set when the caller is authenticated. Anonymous
	// chats leave it empty; AnonID below captures the client-side
	// handle instead. ObjectID's JSON encoding always emits a
	// 24-char hex string (even for the zero value) so this field's
	// rendering is overridden in Chat.MarshalJSON — we drop it from
	// the wire entirely when it's the zero OID.
	UserID primitive.ObjectID `bson:"user_id,omitempty" json:"userId,omitempty"`

	// AnonID identifies anonymous callers in a human-readable form
	// ("anon-<client-ip>"). Set only when UserID is zero. Lets admins
	// eyeball a chat list and group turns from the same visitor
	// without leaking PII into the authenticated userId column.
	// Hashing is intentionally NOT applied — operators triaging
	// abuse reports need the raw IP to act on it; a rotating tag
	// would force a second lookup in access logs every time.
	AnonID string `bson:"anon_id,omitempty" json:"anonId,omitempty"`

	// FlowID / VersionHash / VariantLabel capture the routing
	// decision at the time the chat started — used for per-variant
	// conversation-level dashboards and to detect when a chat
	// straddled a publish / rollback boundary.
	FlowID       string `bson:"flow_id"                 json:"flowId"`
	VersionHash  string `bson:"version_hash,omitempty"  json:"versionHash,omitempty"`
	VariantLabel string `bson:"variant_label,omitempty" json:"variantLabel,omitempty"`

	Messages []Message `bson:"messages" json:"messages"`

	// RunIDs is an append-only set of flow runs that serviced this
	// chat. Lets admins click into any turn's event stream even
	// weeks later (while flow_runs is retained).
	RunIDs []string `bson:"run_ids,omitempty" json:"runIds,omitempty"`

	StartedAt time.Time `bson:"started_at" json:"startedAt"`
	LastAt    time.Time `bson:"last_at"    json:"lastAt"`

	// ExpiresAt drives the Mongo TTL monitor. Rolled forward on
	// every AppendMessage so active chats survive; dormant chats
	// are dropped after the configured TTL.
	ExpiresAt time.Time `bson:"expires_at" json:"expiresAt"`

	// Meta carries free-form client context (page, referrer, locale).
	// Never promoted to a typed field — the admin just reads the
	// JSON when investigating an odd session.
	Meta map[string]any `bson:"meta,omitempty" json:"meta,omitempty"`

	// HandoffID — set when the user requested a human. Non-nil
	// means an assistant_handoffs row exists and admin dashboards
	// should surface this chat.
	HandoffID primitive.ObjectID `bson:"handoff_id,omitempty" json:"handoffId,omitempty"`
}

// MarshalJSON emits Chat in its default shape but silently drops
// `userId` and `handoffId` when they're the zero ObjectID. Go's
// encoding/json treats `primitive.ObjectID` (a `[12]byte`) as
// non-empty under `omitempty`, so without this override every
// anonymous chat would serialise "userId":"000000000000000000000000"
// and admin dashboards would render a misleading "logged in" pill.
//
// We alias the struct to avoid infinite recursion — Marshal on the
// alias uses default field handling, then we post-process the map.
func (c Chat) MarshalJSON() ([]byte, error) {
	type alias Chat
	raw, err := json.Marshal(alias(c))
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, nil
	}
	if c.UserID.IsZero() {
		delete(m, "userId")
	}
	if c.HandoffID.IsZero() {
		delete(m, "handoffId")
	}
	return json.Marshal(m)
}
