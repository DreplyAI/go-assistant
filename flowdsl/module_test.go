package flowdsl_test

import (
	"context"
	"errors"
	"testing"

	"github.com/redelay/go-flowdsl/ir"
	"github.com/redelay/go-flowdsl/runtime"

	assistantflowdsl "github.com/dreplyai/go-assistant/flowdsl"
)

type stubRequester struct {
	called  bool
	gotIn   assistantflowdsl.HandoffRequestInput
	result  assistantflowdsl.HandoffResult
	err     error
}

func (s *stubRequester) Request(_ context.Context, in assistantflowdsl.HandoffRequestInput) (assistantflowdsl.HandoffResult, error) {
	s.called = true
	s.gotIn = in
	return s.result, s.err
}

// TestHandler_MissingSessionIDGoesToErrorPort — validation failures
// surface on the Error output port, not as a run-level error, so the
// flow can route them to a user-facing notification without tearing
// down the run.
func TestHandler_MissingSessionIDGoesToErrorPort(t *testing.T) {
	eng := runtime.NewEngine(runtime.EngineConfig{})
	stub := &stubRequester{result: assistantflowdsl.HandoffResult{HandoffID: "abc", Status: "pending"}}
	assistantflowdsl.Register(eng, stub)

	wf := &ir.Workflow{
		ID: "wf",
		Nodes: []*ir.Node{
			{ID: "start", Name: "start", Kind: ir.NodeKindStart},
			{ID: "handoff", Name: "handoff", Kind: ir.NodeKindAction, ActionRef: "redelay/assistant-handoff-request"},
		},
		Edges: []*ir.Edge{{ID: "e1", From: "start", To: "handoff"}},
	}

	exec, err := eng.Start(context.Background(), wf, map[string]any{"email": "u@x.com"})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if stub.called {
		t.Fatal("stub was called despite missing sessionId")
	}
	last := exec.Steps[len(exec.Steps)-1]
	if last.Status != runtime.StatusCompleted {
		t.Fatalf("step status = %v, want completed (error surfaces on port, not as failure)", last.Status)
	}
	if last.Output["error"] == nil {
		t.Fatalf("expected error payload; got %+v", last.Output)
	}
}

// TestHandler_RoutesToHandoffOnSuccess — the happy path writes the
// handoff id to Output and selects the "Handoff" port, so downstream
// edges can diverge on the OutputPort selector.
func TestHandler_RoutesToHandoffOnSuccess(t *testing.T) {
	eng := runtime.NewEngine(runtime.EngineConfig{})
	stub := &stubRequester{result: assistantflowdsl.HandoffResult{HandoffID: "id-123", Status: "pending"}}
	assistantflowdsl.Register(eng, stub)

	wf := &ir.Workflow{
		ID: "wf",
		Nodes: []*ir.Node{
			{ID: "start", Name: "start", Kind: ir.NodeKindStart},
			{
				ID:     "handoff",
				Name:   "handoff",
				Kind:   ir.NodeKindAction,
				ActionRef: "redelay/assistant-handoff-request",
				Config: map[string]any{"defaultPriority": "high", "defaultReason": "fallback"},
			},
		},
		Edges: []*ir.Edge{{ID: "e1", From: "start", To: "handoff"}},
	}

	input := map[string]any{
		"sessionId": "sess-1",
		"email":     "u@x.com",
		"reason":    "explicit reason",
	}
	exec, err := eng.Start(context.Background(), wf, input)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !stub.called {
		t.Fatal("stub Request was never invoked")
	}
	// Explicit reason wins over defaultReason, but priority falls through to defaults.
	if stub.gotIn.Reason != "explicit reason" {
		t.Errorf("Reason = %q, want %q", stub.gotIn.Reason, "explicit reason")
	}
	if stub.gotIn.Priority != "high" {
		t.Errorf("Priority = %q, want %q", stub.gotIn.Priority, "high")
	}

	last := exec.Steps[len(exec.Steps)-1]
	if last.Output["handoffId"] != "id-123" {
		t.Errorf("handoffId = %v, want id-123", last.Output["handoffId"])
	}
	if last.Output["duplicate"] != false {
		t.Errorf("duplicate = %v, want false", last.Output["duplicate"])
	}
}

// TestHandler_DuplicateIsSuccess — the service returns ErrDuplicate
// when an open handoff already exists for the session. The flow
// must treat this as a valid terminal state (user mashed the
// button) and still route to the Handoff port with duplicate=true.
func TestHandler_DuplicateIsSuccess(t *testing.T) {
	eng := runtime.NewEngine(runtime.EngineConfig{})
	stub := &stubRequester{err: assistantflowdsl.ErrDuplicate}
	assistantflowdsl.Register(eng, stub)

	wf := &ir.Workflow{
		ID: "wf",
		Nodes: []*ir.Node{
			{ID: "start", Name: "start", Kind: ir.NodeKindStart},
			{ID: "handoff", Name: "handoff", Kind: ir.NodeKindAction, ActionRef: "redelay/assistant-handoff-request"},
		},
		Edges: []*ir.Edge{{ID: "e1", From: "start", To: "handoff"}},
	}

	input := map[string]any{"sessionId": "sess-1", "email": "u@x.com"}
	exec, err := eng.Start(context.Background(), wf, input)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	last := exec.Steps[len(exec.Steps)-1]
	if last.Output["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", last.Output["duplicate"])
	}
	if last.Output["status"] != "pending" {
		t.Errorf("status = %v, want pending", last.Output["status"])
	}
}

// TestHandler_ServiceErrorRoutesToErrorPort — non-duplicate service
// errors surface on the Error port. Keeps the run successful so
// parent flows can decide how to respond (e.g. tell the user to try
// again later).
func TestHandler_ServiceErrorRoutesToErrorPort(t *testing.T) {
	eng := runtime.NewEngine(runtime.EngineConfig{})
	stub := &stubRequester{err: errors.New("mongo timeout")}
	assistantflowdsl.Register(eng, stub)

	wf := &ir.Workflow{
		ID: "wf",
		Nodes: []*ir.Node{
			{ID: "start", Name: "start", Kind: ir.NodeKindStart},
			{ID: "handoff", Name: "handoff", Kind: ir.NodeKindAction, ActionRef: "redelay/assistant-handoff-request"},
		},
		Edges: []*ir.Edge{{ID: "e1", From: "start", To: "handoff"}},
	}

	exec, err := eng.Start(context.Background(), wf, map[string]any{"sessionId": "s", "email": "u@x.com"})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	last := exec.Steps[len(exec.Steps)-1]
	if last.Output["error"] == nil {
		t.Fatalf("expected error payload; got %+v", last.Output)
	}
}
