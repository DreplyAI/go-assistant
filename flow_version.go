package assistant

import (
	"context"
	"encoding/json"
	"strconv"

	"go.uber.org/zap"

	flowstore "github.com/redelay/go-flowdsl/flowexec/store"
	"github.com/redelay/go-flowdsl/ir"
)

// embeddedSchemaVersion reads the top-level meta.schemaVersion out of
// the embedded default flow JSON. Any parse error or missing field
// returns 1 — the lowest version we shipped — so older flows in the
// store that predate the schemaVersion field still compare sanely.
func embeddedSchemaVersion() int {
	var doc struct {
		Meta struct {
			SchemaVersion int `json:"schemaVersion"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(defaultFlowJSON, &doc); err != nil {
		return 1
	}
	if doc.Meta.SchemaVersion <= 0 {
		return 1
	}
	return doc.Meta.SchemaVersion
}

// inspectPublishedVersion returns (storedSchemaVersion, ownedByModule)
// for the currently-published version of flow. When anything can't be
// looked up we fall back to (0, false) — the caller then treats the
// flow as user-owned and refuses to upgrade, erring on the side of
// preserving customisation.
func (m *Module) inspectPublishedVersion(ctx context.Context, flow *flowstore.Flow) (int, bool) {
	if flow == nil || flow.PublishedVersionID == "" {
		return 0, false
	}
	store := m.flowexec.Store()
	v, err := store.GetVersion(ctx, flow.PublishedVersionID)
	if err != nil || v == nil {
		m.logger.Debug("assistant: inspect published version failed",
			zap.String("versionID", flow.PublishedVersionID),
			zap.Error(err))
		return 0, false
	}
	if v.CreatedBy != "assistant-module" {
		return 0, false
	}

	// Prefer the explicit label; fall back to scanning meta on the
	// document. Labels were added in v2, so v1 seeds lack them.
	if s, ok := v.Labels["schemaVersion"]; ok {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n, true
		}
	}
	if v.Document != nil {
		return documentSchemaVersion(v.Document), true
	}
	return 1, true
}

// documentSchemaVersion pulls meta.schemaVersion out of a live
// ir.Workflow. Absence means "v1" (the implicit version for seeds
// produced before the field existed).
func documentSchemaVersion(doc *ir.Workflow) int {
	if doc == nil || doc.Meta == nil {
		return 1
	}
	if raw, ok := doc.Meta["schemaVersion"]; ok {
		switch v := raw.(type) {
		case float64:
			if int(v) > 0 {
				return int(v)
			}
		case int:
			if v > 0 {
				return v
			}
		case string:
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1
}
