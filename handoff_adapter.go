package assistant

import (
	"context"
	"errors"

	"github.com/dreplyai/go-assistant/flowdsl"
	"github.com/dreplyai/go-assistant/handoff"
)

// handoffAdapter bridges the concrete handoff.Service onto the narrow
// flowdsl.HandoffRequester interface. Kept out of the flowdsl package
// so that package stays free of imports on its sibling (no cycle, no
// bloat) and translates the errors it exports.
type handoffAdapter struct {
	svc *handoff.Service
}

func (a *handoffAdapter) Request(ctx context.Context, in flowdsl.HandoffRequestInput) (flowdsl.HandoffResult, error) {
	req := handoff.RequestInput{
		SessionID: in.SessionID,
		UserID:    in.UserID,
		Email:     in.Email,
		Phone:     in.Phone,
		Reason:    in.Reason,
		Priority:  handoff.Priority(in.Priority),
	}
	rec, err := a.svc.Request(ctx, req)
	if errors.Is(err, handoff.ErrDuplicate) {
		// Translate to the flowdsl-package sentinel so the handler
		// doesn't need to import handoff directly.
		return flowdsl.HandoffResult{}, flowdsl.ErrDuplicate
	}
	if err != nil {
		return flowdsl.HandoffResult{}, err
	}
	return flowdsl.HandoffResult{
		HandoffID: rec.ID.Hex(),
		Status:    string(rec.Status),
	}, nil
}
