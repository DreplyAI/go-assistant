package handoff

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dreplyai/go-assistant-core/record"
	"github.com/dreplyai/go-assistant/chats"
	"github.com/redelay/go-events/typed"
	"github.com/redelay/go-framework/modules"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Service persists HandoffRequests and publishes the
// assistant.handoff_requested event. Persistence (list/get/patch/insert +
// cursor pagination) is delegated to the shared record.Store kernel; this
// service keeps only the domain logic — dedup, transcript snapshot, and event
// publishing.
type Service struct {
	store *record.Store[HandoffRequest, *HandoffRequest]
	chats *chats.Service
	bus   modules.EventBus
	now   func() time.Time
}

// Options tunes the Service. Zero values mean "use defaults".
type Options struct {
	Collection string
	Now        func() time.Time
}

// ErrNotFound — caller gave an unknown handoff id.
var ErrNotFound = errors.New("handoff: not found")

// ErrDuplicate — a pending handoff already exists for this session.
var ErrDuplicate = errors.New("handoff: pending request already exists for session")

// NewService wires a Service against an existing DB + chats service + event
// bus. The bus may be nil (dev / no-transport) — requests still persist.
func NewService(db *mongo.Database, chatsSvc *chats.Service, bus modules.EventBus, opts Options) *Service {
	if opts.Collection == "" {
		opts.Collection = "assistant_handoffs"
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		store: record.NewStore[HandoffRequest, *HandoffRequest](db.Collection(opts.Collection), opts.Now),
		chats: chatsSvc,
		bus:   bus,
		now:   opts.Now,
	}
}

// EnsureIndexes creates the collection's indexes. Idempotent.
//
// Indexes:
//   - {status: 1, created_at: -1} — admin inbox default view.
//   - {session_id: 1, status: 1}  — dedup check + per-session lookup.
//   - {email: 1} — admin search by customer email.
func (s *Service) EnsureIndexes(ctx context.Context) error {
	return s.store.EnsureIndexes(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: -1}}},
		{Keys: bson.D{{Key: "session_id", Value: 1}, {Key: "status", Value: 1}}},
		{Keys: bson.D{{Key: "email", Value: 1}}},
	})
}

// RequestInput is the shape handlers pass to Request. UserID is optional.
type RequestInput struct {
	SessionID string
	// Tenant is the assistant-tenant key the request arrived under.
	// Empty = default tenant.
	Tenant string
	UserID primitive.ObjectID
	Email     string
	Phone     string
	Reason    string
	Priority  Priority
}

// Request persists a HandoffRequest, snapshots the originating chat's
// transcript, attaches the handoff id back to the chat, and publishes
// assistant.handoff_requested. Dedup: a pending handoff for the same session
// returns ErrDuplicate.
func (s *Service) Request(ctx context.Context, in RequestInput) (*HandoffRequest, error) {
	email := strings.TrimSpace(in.Email)
	if email == "" {
		return nil, errors.New("handoff: email is required")
	}
	if in.SessionID == "" {
		return nil, errors.New("handoff: sessionID is required")
	}
	if in.Priority == "" {
		in.Priority = PriorityNormal
	}
	if !in.Priority.IsValid() {
		return nil, errors.New("handoff: invalid priority")
	}

	// Dedup — refuse a second pending request for the same session.
	existing := s.store.Collection().FindOne(ctx, bson.M{"session_id": in.SessionID, "status": StatusPending})
	if existing.Err() == nil {
		return nil, ErrDuplicate
	}

	// Snapshot the chat transcript + link the ids (missing chat isn't fatal).
	var chatID primitive.ObjectID
	transcript := ""
	if s.chats != nil {
		if c, err := s.chats.GetBySessionID(ctx, in.SessionID); err == nil {
			chatID = c.ID
			transcript = renderTranscript(c)
		}
	}

	rec := HandoffRequest{
		ChatID:     chatID,
		SessionID:  in.SessionID,
		Tenant:     in.Tenant,
		UserID:     in.UserID,
		Email:      email,
		Phone:      strings.TrimSpace(in.Phone),
		Reason:     strings.TrimSpace(in.Reason),
		Transcript: transcript,
		Priority:   in.Priority,
		Status:     StatusPending,
	}
	if err := s.store.Insert(ctx, &rec); err != nil {
		return nil, err
	}

	// Best-effort chat-side attach so admins can navigate both ways.
	if s.chats != nil && !chatID.IsZero() {
		_ = s.chats.AttachHandoff(ctx, in.SessionID, rec.GetID())
	}

	// Publish the event (best-effort; nil bus = persist-only).
	if s.bus != nil {
		payload := HandoffRequestedPayload{
			HandoffID:   rec.GetID().Hex(),
			ChatID:      chatID.Hex(),
			SessionID:   in.SessionID,
			Tenant:      in.Tenant,
			Email:       email,
			Phone:       rec.Phone,
			Reason:      rec.Reason,
			Priority:    in.Priority,
			Transcript:  transcript,
			RequestedAt: rec.CreatedAt.Format(time.RFC3339),
		}
		if !in.UserID.IsZero() {
			payload.UserID = in.UserID.Hex()
		}
		if msg, err := HandoffRequestedEvent.NewMessage(rec.GetID().Hex(),
			typed.Actor{Type: typed.ActorTypeSystem, ID: "assistant-handoff"}, payload); err == nil {
			_ = s.bus.Publish(ctx, msg)
		}
	}
	return &rec, nil
}

// List pages handoffs newest-first with an optional status filter.
func (s *Service) List(ctx context.Context, status Status, limit int, cursor string) ([]*HandoffRequest, string, error) {
	return s.ListFiltered(ctx, ListFilter{Status: status}, limit, cursor)
}

// ListFilter narrows a handoff listing. Zero values mean "no filter";
// like chats.ListFilter, an empty Tenant means "all tenants".
type ListFilter struct {
	Status Status
	Tenant string
}

// ListFiltered pages handoffs newest-first with status + tenant filters.
func (s *Service) ListFiltered(ctx context.Context, f ListFilter, limit int, cursor string) ([]*HandoffRequest, string, error) {
	filter := bson.M{}
	if f.Status != "" {
		filter["status"] = f.Status
	}
	if f.Tenant != "" {
		filter["tenant"] = f.Tenant
	}
	return s.store.List(ctx, record.ListOptions{Filter: filter, Limit: limit, Cursor: cursor})
}

// GetByID fetches a single handoff.
func (s *Service) GetByID(ctx context.Context, id primitive.ObjectID) (*HandoffRequest, error) {
	h, err := s.store.Get(ctx, id)
	if errors.Is(err, record.ErrNotFound) {
		return nil, ErrNotFound
	}
	return h, err
}

// UpdateStatusInput is the admin PATCH body. Only Status is required.
type UpdateStatusInput struct {
	Status      Status
	Notes       string
	ContactedBy primitive.ObjectID
}

// UpdateStatus advances the handoff through its lifecycle and/or saves notes.
// An empty Status leaves the status alone (a notes-only save used to write
// status "" and drop the handoff out of every tab). When the new status is
// `contacted` we stamp ContactedAt + ContactedBy.
func (s *Service) UpdateStatus(ctx context.Context, id primitive.ObjectID, in UpdateStatusInput) (*HandoffRequest, error) {
	set := bson.M{}
	if in.Status != "" {
		set["status"] = in.Status
	}
	if in.Notes != "" {
		set["notes"] = in.Notes
	}
	if len(set) == 0 {
		return s.GetByID(ctx, id)
	}
	if in.Status == StatusContacted {
		set["contacted_at"] = s.now()
		if !in.ContactedBy.IsZero() {
			set["contacted_by"] = in.ContactedBy
		}
	}
	h, err := s.store.Patch(ctx, id, set)
	if errors.Is(err, record.ErrNotFound) {
		return nil, ErrNotFound
	}
	return h, err
}

// renderTranscript produces a plain-text dump of the chat so the handoff record
// is self-contained.
func renderTranscript(c *chats.Chat) string {
	var b strings.Builder
	for _, m := range c.Messages {
		b.WriteString(strings.ToUpper(m.Role))
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
