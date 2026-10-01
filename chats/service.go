package chats

import (
	"context"
	"errors"
	"time"

	"github.com/redelay/go-framework/crud"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Service owns the assistant_chats collection. Thin wrapper around
// crud.MongoCRUD so we get cursors, counts, and paging for free —
// the collection-level indexes (unique session_id + TTL) are
// enforced here, not by the CRUD generic.
type Service struct {
	col    *mongo.Collection
	crud   *crud.MongoCRUD[*Chat]
	ttl    time.Duration
	now    func() time.Time
}

// Options tunes Service construction. Zero values pick sensible
// defaults so the caller can pass Options{} in tests.
type Options struct {
	// Collection name. Defaults to "assistant_chats".
	Collection string
	// TTL is how long a chat lives past its last activity before
	// the Mongo TTL monitor reaps it. Defaults to 30 days.
	TTL time.Duration
	// Now lets tests freeze the clock.
	Now func() time.Time
}

// ErrNotFound is returned when no chat matches the filter — callers
// treat this as "client has never talked to us" rather than an
// error worth logging.
var ErrNotFound = errors.New("chats: not found")

// NewService builds a Service against an existing *mongo.Database.
// The caller is responsible for calling EnsureIndexes at startup
// (once per boot — it's idempotent).
func NewService(db *mongo.Database, opts Options) *Service {
	if opts.Collection == "" {
		opts.Collection = "assistant_chats"
	}
	if opts.TTL == 0 {
		opts.TTL = 30 * 24 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	col := db.Collection(opts.Collection)
	return &Service{
		col:  col,
		crud: crud.NewMongoCRUD[*Chat](col),
		ttl:  opts.TTL,
		now:  opts.Now,
	}
}

// EnsureIndexes creates the indexes the service relies on. Idempotent
// — safe to call at every startup.
//
// Indexes:
//   - {session_id: 1} unique — append / lookup by session is the hot
//     path; duplicate session_ids would break idempotent appends.
//   - {user_id: 1, last_at: -1} — admin listing by user, newest first.
//   - {flow_id: 1, variant_label: 1} — per-variant chat reports pair
//     naturally with the flowexec variant labels.
//   - {expires_at: 1} TTL, expireAfterSeconds=0 — Mongo reaps at
//     expires_at directly so the app never runs a janitor.
func (s *Service) EnsureIndexes(ctx context.Context) error {
	_, err := s.col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "session_id", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("session_id_unique"),
		},
		{
			Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "last_at", Value: -1}},
		},
		{
			Keys: bson.D{{Key: "flow_id", Value: 1}, {Key: "variant_label", Value: 1}},
		},
		{
			Keys:    bson.D{{Key: "expires_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(0).SetName("expires_at_ttl"),
		},
	})
	return err
}

// CreateIfAbsent returns the existing chat for a session_id or
// creates a new one. Idempotent — safe to call on every inbound
// request so the caller can just unconditionally bump LastAt.
//
// flowID / versionHash / variantLabel are stamped on first create
// only; later turns leave them alone so an in-flight chat that
// straddles a publish keeps its original variant stamp for reports.
// (A follow-up could track variant changes per turn; today we keep
// the first pick because swapping mid-chat would confuse users.)
//
// anonID is the client-readable handle used when userID is the zero
// ObjectID (anonymous caller). Passed as "" for authenticated chats
// — persisting an empty string would drop to omitempty at insert
// time anyway, but the caller should skip it explicitly so admin
// logs show the intent.
func (s *Service) CreateIfAbsent(ctx context.Context, sessionID, flowID, versionHash, variantLabel string, userID primitive.ObjectID, anonID string, meta map[string]any) (*Chat, error) {
	return s.CreateIfAbsentTenant(ctx, "", sessionID, flowID, versionHash, variantLabel, userID, anonID, meta)
}

// CreateIfAbsentTenant is CreateIfAbsent with a tenant stamp. The
// tenant key is set on first insert only (like the flow/variant
// stamps) — a chat never migrates between tenants. Empty tenant =
// the default tenant, and the field is omitted from the document so
// pre-multi-tenant rows and default-tenant rows look identical.
func (s *Service) CreateIfAbsentTenant(ctx context.Context, tenant, sessionID, flowID, versionHash, variantLabel string, userID primitive.ObjectID, anonID string, meta map[string]any) (*Chat, error) {
	if sessionID == "" {
		return nil, errors.New("chats: sessionID is required")
	}
	now := s.now()

	// Try insert-if-not-exists via upsert + $setOnInsert. The
	// {session_id} unique index means concurrent first-writes for
	// the same session_id collapse safely.
	setOnInsert := bson.M{
		"session_id":    sessionID,
		"flow_id":       flowID,
		"version_hash":  versionHash,
		"variant_label": variantLabel,
		"user_id":       userID,
		"messages":      []Message{},
		"run_ids":       []string{},
		"started_at":    now,
		"meta":          meta,
		"created_at":    now,
	}
	// Only stamp anon_id on first insert when we actually have one —
	// leaving the key out keeps authenticated chats free of a stray
	// empty "anon_id":"" field.
	if anonID != "" {
		setOnInsert["anon_id"] = anonID
	}
	// Same rationale as anon_id: default-tenant chats carry no tenant
	// key so they're indistinguishable from pre-multi-tenant rows.
	if tenant != "" {
		setOnInsert["tenant"] = tenant
	}
	update := bson.M{
		"$setOnInsert": setOnInsert,
		"$set": bson.M{
			"last_at":    now,
			"expires_at": now.Add(s.ttl),
			"updated_at": now,
		},
	}
	opts := options.FindOneAndUpdate().
		SetUpsert(true).
		SetReturnDocument(options.After)
	res := s.col.FindOneAndUpdate(ctx, bson.M{"session_id": sessionID}, update, opts)

	var c Chat
	if err := res.Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// AppendMessage adds a message to the chat and rolls the TTL
// forward. Run is optional — attach it when the message came out
// of a flow run so admin dashboards can drill in.
func (s *Service) AppendMessage(ctx context.Context, sessionID string, msg Message, runID string) (*Chat, error) {
	if sessionID == "" {
		return nil, errors.New("chats: sessionID is required")
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = s.now()
	}
	if runID != "" {
		msg.RunID = runID
	}
	now := s.now()

	update := bson.M{
		"$push": bson.M{"messages": msg},
		"$set": bson.M{
			"last_at":    now,
			"expires_at": now.Add(s.ttl),
			"updated_at": now,
		},
	}
	// Only push the run id when one is attached AND we haven't
	// recorded it already — keeps run_ids a set without a second
	// query.
	if runID != "" {
		update["$addToSet"] = bson.M{"run_ids": runID}
	}
	res := s.col.FindOneAndUpdate(
		ctx,
		bson.M{"session_id": sessionID},
		update,
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	var c Chat
	if err := res.Decode(&c); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

// GetBySessionID fetches a chat by its client-supplied session_id.
// Returns ErrNotFound when nothing matches — callers should treat
// that as "fresh client" not "error".
func (s *Service) GetBySessionID(ctx context.Context, sessionID string) (*Chat, error) {
	var c Chat
	err := s.col.FindOne(ctx, bson.M{"session_id": sessionID}).Decode(&c)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetByID fetches by Mongo _id. Used by the admin listing endpoints.
func (s *Service) GetByID(ctx context.Context, id primitive.ObjectID) (*Chat, error) {
	var c Chat
	err := s.col.FindOne(ctx, bson.M{"_id": id}).Decode(&c)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// AttachHandoff stamps the chat with the handoff record id. Called
// by handoff.Service.Request after persisting the HandoffRequest so
// the chat → handoff link is bidirectional.
func (s *Service) AttachHandoff(ctx context.Context, sessionID string, handoffID primitive.ObjectID) error {
	_, err := s.col.UpdateOne(ctx,
		bson.M{"session_id": sessionID},
		bson.M{"$set": bson.M{"handoff_id": handoffID, "updated_at": s.now()}},
	)
	return err
}

// List pages chats newest-first, with optional user + flow filters.
// Powers the admin /assistant/chats endpoint.
// Count returns how many chats match the filter (all pages).
func (s *Service) Count(ctx context.Context, filter ListFilter) (int64, error) {
	return s.col.CountDocuments(ctx, filter.query())
}

// query is the Mongo filter shared by List and Count.
func (f ListFilter) query() bson.M {
	q := bson.M{}
	if !f.UserID.IsZero() {
		q["user_id"] = f.UserID
	}
	if f.FlowID != "" {
		q["flow_id"] = f.FlowID
	}
	if f.VariantLabel != "" {
		q["variant_label"] = f.VariantLabel
	}
	if f.Tenant != "" {
		q["tenant"] = f.Tenant
	}
	return q
}

func (s *Service) List(ctx context.Context, filter ListFilter, limit int, cursor string) ([]*Chat, string, error) {
	q := filter.query()
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
		options.Find().SetSort(bson.D{{Key: "last_at", Value: -1}}).SetLimit(int64(limit+1)),
	)
	if err != nil {
		return nil, "", err
	}
	defer cur.Close(ctx)
	out := []*Chat{}
	for cur.Next(ctx) {
		var c Chat
		if err := cur.Decode(&c); err != nil {
			return nil, "", err
		}
		out = append(out, &c)
	}
	next := ""
	if len(out) > limit {
		next = out[limit-1].ID.Hex()
		out = out[:limit]
	}
	return out, next, cur.Err()
}

// ListFilter narrows a List query. Zero values mean "no filter".
type ListFilter struct {
	UserID       primitive.ObjectID
	FlowID       string
	VariantLabel string
	// Tenant narrows to one tenant's chats. Empty means "all
	// tenants" (the admin default) — filtering for exactly the
	// default tenant isn't expressible here on purpose: its rows
	// carry no tenant key, matching pre-multi-tenant data.
	Tenant string
}
