// Package flowdsl registers the assistant module's FlowDSL nodes.
//
// Blank-import to surface the `redelay/assistant-handoff-request` node
// in the module browser and FlowDSL Studio:
//
//	import _ "github.com/dreplyai/go-assistant/flowdsl"
//
// The handler is wired to the real handoff.Service at runtime by
// the assistant module's Startup hook (see assistant.Module.Startup
// where it calls flowdsl.Register). Without that wiring this package
// only provides the manifest and a no-op handler so Studio can render
// the node without a live service.
package flowdsl

import (
	"context"
	"errors"
	"fmt"

	"github.com/redelay/go-flowdsl/runtime"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/redelay/go-framework/modules"
)

func init() {
	modules.RegisterFactory("assistant-flowdsl", func(_ modules.ModuleDeps) (modules.Module, error) {
		return &Module{IRBase: modules.MustLoadIR(moduleYAML)}, nil
	})
}

// Module carries only the manifest — no handlers are registered by
// this factory because it doesn't own the handoff service. The
// assistant core module calls Register with its service at Startup.
type Module struct {
	modules.IRBase
}

func (m *Module) Startup(_ context.Context) error  { return nil }
func (m *Module) Shutdown(_ context.Context) error { return nil }

// HandoffRequester is the narrow interface the node needs from the
// handoff service. Implemented by *handoff.Service; kept local so this
// package does not import backend/modules/assistant/handoff (avoids
// an import cycle — the core module already imports this package).
type HandoffRequester interface {
	Request(ctx context.Context, in HandoffRequestInput) (HandoffResult, error)
}

// HandoffRequestInput mirrors handoff.RequestInput without pulling in
// the handoff package. The assistant module's Register adapter does
// the concrete-type translation so this package stays dependency-free
// of sibling code.
type HandoffRequestInput struct {
	SessionID string
	UserID    primitive.ObjectID
	Email     string
	Phone     string
	Reason    string
	Priority  string
}

// HandoffResult captures the subset of the handoff record the flow
// needs to project into Step.Output. `Duplicate` is true when the
// service de-duplicated against an existing pending request for the
// same session; the returned HandoffID points at the pre-existing
// record in that case.
type HandoffResult struct {
	HandoffID string
	Status    string
	Duplicate bool
}

// ErrDuplicate is returned by HandoffRequester when a pending handoff
// already exists for the session. Exported so the assistant module's
// Register adapter can map handoff.ErrDuplicate onto it without this
// package importing that package.
var ErrDuplicate = errors.New("assistant-flowdsl: duplicate handoff")

// Register installs the handoff-request node handler on engine. Call
// this once at module Startup with the live handoff service. Safe to
// call multiple times — the last registration wins (matching the
// runtime.Engine.RegisterHandler contract).
func Register(engine *runtime.Engine, svc HandoffRequester) {
	if engine == nil || svc == nil {
		return
	}
	engine.RegisterHandler("redelay/assistant-handoff-request", handoffHandler(svc))
}

func handoffHandler(svc HandoffRequester) runtime.NodeHandler {
	return func(ctx context.Context, step *runtime.Step) error {
		in, err := buildHandoffInput(step)
		if err != nil {
			writeError(step, err)
			return nil // emit Error port, don't fail the whole run
		}
		res, err := svc.Request(ctx, in)
		if err != nil {
			if errors.Is(err, ErrDuplicate) {
				// Treat dedup as a successful no-op — a valid outcome
				// when the user mashes the handoff button.
				writeHandoff(step, HandoffResult{Duplicate: true, Status: "pending"})
				return nil
			}
			writeError(step, err)
			return nil
		}
		writeHandoff(step, res)
		return nil
	}
}

// buildHandoffInput pulls the request from Step.Input, falling back
// to node-level defaults when the packet omits optional fields.
func buildHandoffInput(step *runtime.Step) (HandoffRequestInput, error) {
	sessionID, _ := step.Input["sessionId"].(string)
	if sessionID == "" {
		return HandoffRequestInput{}, fmt.Errorf("sessionId is required")
	}
	email, _ := step.Input["email"].(string)
	if email == "" {
		return HandoffRequestInput{}, fmt.Errorf("email is required")
	}

	priority, _ := step.Input["priority"].(string)
	if priority == "" {
		if dp, ok := step.Node.Config["defaultPriority"].(string); ok {
			priority = dp
		}
	}
	reason, _ := step.Input["reason"].(string)
	if reason == "" {
		if dr, ok := step.Node.Config["defaultReason"].(string); ok {
			reason = dr
		}
	}

	var uid primitive.ObjectID
	if uidStr, _ := step.Input["userId"].(string); uidStr != "" {
		if parsed, err := primitive.ObjectIDFromHex(uidStr); err == nil {
			uid = parsed
		}
	}

	phone, _ := step.Input["phone"].(string)
	return HandoffRequestInput{
		SessionID: sessionID,
		UserID:    uid,
		Email:     email,
		Phone:     phone,
		Reason:    reason,
		Priority:  priority,
	}, nil
}

func writeHandoff(step *runtime.Step, res HandoffResult) {
	if step.Output == nil {
		step.Output = map[string]any{}
	}
	step.Output["handoffId"] = res.HandoffID
	step.Output["status"] = res.Status
	step.Output["duplicate"] = res.Duplicate
	step.OutputPort = "Handoff"
}

func writeError(step *runtime.Step, err error) {
	if step.Output == nil {
		step.Output = map[string]any{}
	}
	step.Output["error"] = err.Error()
	step.OutputPort = "Error"
}
