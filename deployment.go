package assistant

import (
	"context"

	flowstore "github.com/redelay/go-flowdsl/flowexec/store"
)

// ensureDeployment calls the generic flowstore.EnsureDeployment with
// the assistant's default shape (single stable variant pointing at
// the legacy FlowID). Thin shim so the module carries only the
// domain-specific bits; everything else is framework-level.
func (m *Module) ensureDeployment(ctx context.Context) error {
	flowID := m.cfg.FlowID
	if flowID == "" {
		flowID = defaultFlowID
	}
	_, err := flowstore.EnsureDeployment(ctx, m.flowexec.Store(), flowstore.CreateDeploymentInput{
		ID:          m.cfg.DeploymentID,
		Name:        "Assistant",
		Description: "Auto-created deployment. Add canary / A-B variants via POST /deployments/{id}/variants/from-template.",
		Variants: []flowstore.FlowDeploymentVariant{
			{Label: "stable", FlowID: flowID, Weight: 100},
		},
		StickyBy: flowstore.StickySession,
		Labels:   map[string]string{"source": "assistant-module-default"},
	})
	return err
}

// resolveVariant is the per-turn lookup: pick the active variant for
// this session and load the flow version to run. Pure delegation to
// the framework helper — kept here only so SSE can write
// `m.resolveVariant(...)` without threading the store through.
func (m *Module) resolveVariant(ctx context.Context, meta map[string]any) (*flowstore.ResolvedDeployment, error) {
	return m.resolveVariantFor(ctx, m.cfg.DeploymentID, meta)
}

// resolveVariantFor is resolveVariant against an explicit deployment
// id — the per-tenant path. The default tenant's id is
// m.cfg.DeploymentID, so both paths share one implementation.
func (m *Module) resolveVariantFor(ctx context.Context, deploymentID string, meta map[string]any) (*flowstore.ResolvedDeployment, error) {
	return flowstore.ResolveDeployment(ctx, m.flowexec.Store(), deploymentID, meta, nil, m.roleChecker)
}

// userIDHex returns the hex string form of a Mongo ObjectID, or ""
// when the id is the zero value. Helper used by SSE to thread user
// id into the deployment's routing meta without leaking primitive
// types into the signature.
func userIDHex(uid interface {
	IsZero() bool
	Hex() string
}) string {
	if uid == nil || uid.IsZero() {
		return ""
	}
	return uid.Hex()
}
