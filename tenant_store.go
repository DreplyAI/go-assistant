package assistant

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
)

// TenantStore persists tenants in the assistant_tenants collection.
// Nil on DB-less deployments — tenant CRUD then errors and only
// programmatic registration works.
type TenantStore struct {
	col *mongo.Collection
	now func() time.Time
}

// NewTenantStore builds a store over db. Collection name is fixed —
// it is module-owned infrastructure, like assistant_chats.
func NewTenantStore(db *mongo.Database) *TenantStore {
	return &TenantStore{
		col: db.Collection("assistant_tenants"),
		now: func() time.Time { return time.Now().UTC() },
	}
}

// EnsureIndexes creates the unique key index. Idempotent.
func (s *TenantStore) EnsureIndexes(ctx context.Context) error {
	_, err := s.col.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "key", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("tenant_key_unique"),
	})
	return err
}

// All returns every persisted tenant, including disabled ones (the
// registry keeps them so lookups can distinguish "disabled" from
// "unknown" if that ever matters; resolution treats both the same).
func (s *TenantStore) All(ctx context.Context) ([]*Tenant, error) {
	cur, err := s.col.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "key", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []*Tenant{}
	for cur.Next(ctx) {
		var t Tenant
		if err := cur.Decode(&t); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, cur.Err()
}

// Insert persists a new tenant. Duplicate keys map to ErrTenantExists.
func (s *TenantStore) Insert(ctx context.Context, t *Tenant) error {
	now := s.now()
	t.ID = primitive.NewObjectID()
	t.CreatedAt = now
	t.UpdatedAt = now
	_, err := s.col.InsertOne(ctx, t)
	if mongo.IsDuplicateKeyError(err) {
		return ErrTenantExists
	}
	return err
}

// Update replaces the mutable fields of the tenant with the given
// key. Returns the fresh document, or ErrTenantNotFound.
func (s *TenantStore) Update(ctx context.Context, key string, set bson.M) (*Tenant, error) {
	set["updated_at"] = s.now()
	res := s.col.FindOneAndUpdate(ctx,
		bson.M{"key": key},
		bson.M{"$set": set},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	var t Tenant
	if err := res.Decode(&t); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrTenantNotFound
		}
		return nil, err
	}
	return &t, nil
}

// Delete removes the tenant with the given key.
func (s *TenantStore) Delete(ctx context.Context, key string) error {
	res, err := s.col.DeleteOne(ctx, bson.M{"key": key})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return ErrTenantNotFound
	}
	return nil
}

// ── Module-level CRUD (persist + cache + deployment) ─────────────────

// CreateTenant validates, persists, caches, and ensures the flowexec
// deployment for a new tenant. This is what the admin POST handler
// calls; RegisterTenant is the non-persisting sibling.
func (m *Module) CreateTenant(ctx context.Context, t Tenant) (*Tenant, error) {
	t.Key = strings.ToLower(strings.TrimSpace(t.Key))
	if !tenantKeyRe.MatchString(t.Key) {
		return nil, ErrInvalidTenantKey
	}
	if m.tenantStore == nil {
		return nil, errors.New("assistant: tenant persistence requires a database")
	}
	if m.tenantReg.get(t.Key) != nil {
		return nil, ErrTenantExists
	}
	if err := m.tenantStore.Insert(ctx, &t); err != nil {
		return nil, err
	}
	m.tenantReg.put(&t)
	if err := m.ensureTenantDeployment(ctx, &t); err != nil && m.logger != nil {
		m.logger.Warn("assistant: ensure tenant deployment failed", zap.String("tenant", t.Key), zap.Error(err))
	}
	return &t, nil
}

// UpdateTenant patches the tenant's mutable fields and refreshes the
// cache. The key itself is immutable — it is the routing identity
// stamped on chats and handoffs.
func (m *Module) UpdateTenant(ctx context.Context, key string, in TenantUpdate) (*Tenant, error) {
	if m.tenantStore == nil {
		return nil, errors.New("assistant: tenant persistence requires a database")
	}
	set := bson.M{}
	if in.Name != nil {
		set["name"] = *in.Name
	}
	if in.FlowID != nil {
		set["flow_id"] = *in.FlowID
	}
	if in.DeploymentID != nil {
		set["deployment_id"] = *in.DeploymentID
	}
	if in.DocsIndex != nil {
		set["docs_index"] = *in.DocsIndex
	}
	if in.SuggestedQuestions != nil {
		set["suggested_questions"] = *in.SuggestedQuestions
	}
	if in.RateLimitPerMinute != nil {
		set["rate_limit_per_minute"] = *in.RateLimitPerMinute
	}
	if in.HandoffEnabled != nil {
		set["handoff_enabled"] = *in.HandoffEnabled
	}
	if in.AnonymousAllowed != nil {
		set["anonymous_allowed"] = *in.AnonymousAllowed
	}
	if in.Disabled != nil {
		set["disabled"] = *in.Disabled
	}
	if len(set) == 0 {
		if t := m.tenantReg.get(key); t != nil {
			return t, nil
		}
		return nil, ErrTenantNotFound
	}
	t, err := m.tenantStore.Update(ctx, key, set)
	if err != nil {
		return nil, err
	}
	m.tenantReg.put(t)
	if err := m.ensureTenantDeployment(ctx, t); err != nil && m.logger != nil {
		m.logger.Warn("assistant: ensure tenant deployment failed", zap.String("tenant", t.Key), zap.Error(err))
	}
	return t, nil
}

// DeleteTenant removes the tenant from store + cache. Chats and
// handoffs stamped with the key are kept — they are history; the
// flowexec deployment is also kept (flows may still reference it).
func (m *Module) DeleteTenant(ctx context.Context, key string) error {
	if m.tenantStore == nil {
		return errors.New("assistant: tenant persistence requires a database")
	}
	if err := m.tenantStore.Delete(ctx, key); err != nil {
		return err
	}
	m.tenantReg.delete(key)
	return nil
}

// TenantUpdate is the PATCH shape — nil fields are left untouched.
type TenantUpdate struct {
	Name               *string   `json:"name,omitempty"`
	FlowID             *string   `json:"flowId,omitempty"`
	DeploymentID       *string   `json:"deploymentId,omitempty"`
	DocsIndex          *string   `json:"docsIndex,omitempty"`
	SuggestedQuestions *[]string `json:"suggestedQuestions,omitempty"`
	RateLimitPerMinute *int      `json:"rateLimitPerMinute,omitempty"`
	HandoffEnabled     *bool     `json:"handoffEnabled,omitempty"`
	AnonymousAllowed   *bool     `json:"anonymousAllowed,omitempty"`
	Disabled           *bool     `json:"disabled,omitempty"`
}

// loadTenants hydrates the registry from Mongo at Startup and ensures
// each tenant's deployment exists. Non-fatal — a failed load leaves
// only the default tenant active, which is the safe degradation.
func (m *Module) loadTenants(ctx context.Context) {
	if m.tenantStore == nil {
		return
	}
	if err := m.tenantStore.EnsureIndexes(ctx); err != nil && m.logger != nil {
		m.logger.Warn("assistant: tenant EnsureIndexes failed", zap.Error(err))
	}
	all, err := m.tenantStore.All(ctx)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("assistant: load tenants failed", zap.Error(err))
		}
		return
	}
	for _, t := range all {
		m.tenantReg.put(t)
		if err := m.ensureTenantDeployment(ctx, t); err != nil && m.logger != nil {
			m.logger.Warn("assistant: ensure tenant deployment failed",
				zap.String("tenant", t.Key), zap.Error(err))
		}
	}
	if len(all) > 0 && m.logger != nil {
		m.logger.Info("assistant: loaded tenants", zap.Int("count", len(all)))
	}
}
