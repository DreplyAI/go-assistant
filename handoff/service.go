package handoff

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dreplyai/go-assistant/chats"
	"github.com/redelay/go-events/typed"
	"github.com/redelay/go-framework/modules"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Service persists HandoffRequests and publishes the
// assistant.handoff_requested event. Notification delivery is
// handled by a downstream flow — keeping the event / delivery split
// means a project can swap email for Slack / CRM / pagerduty with
// no code change in this module.
type Service struct {
	col   *mongo.Collection
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
// Callers surface this as 409 so the user sees "we've got your
// request, we'll be in touch" rather than double-notifying ops.
var ErrDuplicate = errors.New("handoff: pending request already exists for session")

// NewService wires a Service against an existing DB + chats service
// + event bus. The bus may be nil in tests / no-transport deployments;
// requests still persist but no event is emitted.
func NewService(db *mongo.Database, chatsSvc *chats.Service, bus modules.EventBus, opts Options) *Service {
	if opts.Collection == "" {
		opts.Collection = "assistant_handoffs"
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		col:   db.Collection(opts.Collection),
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
	_, err := s.col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: -1}}},
		{Keys: bson.D{{Key: "session_id", Value: 1}, {Key: "status", Value: 1}}},
		{Keys: bson.D{{Key: "email", Value: 1}}},
	})
	return err
}

// RequestInput is the shape handlers pass to Request. UserID is
// optional — anonymous callers still get a record.
type RequestInput struct {
	SessionID string
	UserID    primitive.ObjectID
	Email     string
	Phone     string
	Reason    string
	Priority  Priority
}

// Request persists a HandoffRequest, snapshots the originating
// chat's transcript, attaches the handoff id back to the chat, and
// publishes assistant.handoff_requested for the notification flow
// to pick up.
//
// Dedup rule: if a pending handoff already exists for the same
// session_id we return ErrDuplicate instead of creating a second
// one. Keeps the admin inbox clean when a user mashes the "talk to
// human" button.
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
	existing := s.col.FindOne(ctx, bson.M{
		"session_id": in.SessionID,
		"status":     StatusPending,
	})
	if existing.Err() == nil {
		return nil, ErrDuplicate
	}

	// Pull the chat so we can copy the transcript + link the ids.
	// Missing chat isn't fatal — the handoff still persists (the
	// user might have cleared localStorage), just without a
	// transcript.
	var chatID primitive.ObjectID
	transcript := ""
	if s.chats != nil {
		c, err := s.chats.GetBySessionID(ctx, in.SessionID)
		if err == nil {
			chatID = c.ID
			transcript = renderTranscript(c)
		}
	}

	now := s.now()
	rec := HandoffRequest{
		ChatID:    chatID,
		SessionID: in.SessionID,
		UserID:    in.UserID,
		Email:     email,
		Phone:     strings.TrimSpace(in.Phone),
		Reason:    strings.TrimSpace(in.Reason),
		Transcript: transcript,
		Priority:  in.Priority,
		Status:    StatusPending,
	}
	rec.SetID(primitive.NewObjectID())
	rec.CreatedAt = now
	rec.UpdatedAt = now

	if _, err := s.col.InsertOne(ctx, rec); err != nil {
		return nil, err
	}

	// Best-effort chat-side attach so admins can navigate both ways.
	if s.chats != nil && !chatID.IsZero() {
		_ = s.chats.AttachHandoff(ctx, in.SessionID, rec.GetID())
	}

	// Publish the event. Notification is a separate flow concern
	// — we just fire and forget. A nil bus (dev / no-transport
	// deployment) means persist-only mode; the admin still sees
	// the record via the admin API.
	if s.bus != nil {
		payload := HandoffRequestedPayload{
			HandoffID:   rec.GetID().Hex(),
			ChatID:      chatID.Hex(),
			SessionID:   in.SessionID,
			Email:       email,
			Phone:       rec.Phone,
			Reason:      rec.Reason,
			Priority:    in.Priority,
			Transcript:  transcript,
			RequestedAt: now.Format(time.RFC3339),
		}
		if !in.UserID.IsZero() {
			payload.UserID = in.UserID.Hex()
		}
		msg, err := HandoffRequestedEvent.NewMessage(rec.GetID().Hex(),
			typed.Actor{Type: typed.ActorTypeSystem, ID: "assistant-handoff"},
			payload)
		if err == nil {
			_ = s.bus.Publish(ctx, msg) // best-effort, never blocks the caller
		}
	}
	return &rec, nil
}

// List pages handoffs newest-first with an optional status filter.
// Admin inbox default: `?status=pending`.
func (s *Service) List(ctx context.Context, status Status, limit int, cursor string) ([]*HandoffRequest, string, error) {
	q := bson.M{}
	if status != "" {
		q["status"] = status
	}
	if cursor != "" {
		oid, err := primitive.ObjectIDFromHex(cursor)
		if err == nil {
			q["_id"] = bson.M{"$lt": oid}
		}
	}
	if limit <= 0 {
		limit = 20
	}
	cur, err := s.col.Find(ctx, q,
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(int64(limit+1)),
	)
	if err != nil {
		return nil, "", err
	}
	defer cur.Close(ctx)
	out := []*HandoffRequest{}
	for cur.Next(ctx) {
		var h HandoffRequest
		if err := cur.Decode(&h); err != nil {
			return nil, "", err
		}
		out = append(out, &h)
	}
	next := ""
	if len(out) > limit {
		next = out[limit-1].GetID().Hex()
		out = out[:limit]
	}
	return out, next, cur.Err()
}

// GetByID fetches a single handoff.
func (s *Service) GetByID(ctx context.Context, id primitive.ObjectID) (*HandoffRequest, error) {
	var h HandoffRequest
	err := s.col.FindOne(ctx, bson.M{"_id": id}).Decode(&h)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// UpdateStatusInput is the admin PATCH body. Only Status is required.
type UpdateStatusInput struct {
	Status      Status
	Notes       string
	ContactedBy primitive.ObjectID
}

// UpdateStatus advances the handoff through its lifecycle. When the
// new status is `contacted` we stamp ContactedAt + ContactedBy.
// Status validation is loose — we accept any Status string so a
// future state (e.g. `blocked`) doesn't need a service change; the
// admin UI's picker is the real guard.
func (s *Service) UpdateStatus(ctx context.Context, id primitive.ObjectID, in UpdateStatusInput) (*HandoffRequest, error) {
	now := s.now()
	set := bson.M{
		"status":     in.Status,
		"updated_at": now,
	}
	if in.Notes != "" {
		set["notes"] = in.Notes
	}
	if in.Status == StatusContacted {
		set["contacted_at"] = now
		if !in.ContactedBy.IsZero() {
			set["contacted_by"] = in.ContactedBy
		}
	}
	res := s.col.FindOneAndUpdate(ctx,
		bson.M{"_id": id},
		bson.M{"$set": set},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	var h HandoffRequest
	if err := res.Decode(&h); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &h, nil
}

// renderTranscript produces a plain-text dump of the chat so the
// handoff record is self-contained. Format intentionally simple so
// admins can grep / skim without a dedicated viewer; the Studio's
// schema browser can pretty-print later.
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
