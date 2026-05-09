package assistant

import (
	"testing"

	"github.com/redelay/go-flowdsl/ir"
)

// TestCapabilitiesFromWorkflow — the detection logic for every
// capability the widget reads. Locked in behaviour so accidental
// renames ("redelay/llm-chat" → "redelay/chat") don't silently
// turn off features on the public widget.
func TestCapabilitiesFromWorkflow(t *testing.T) {
	cases := []struct {
		name string
		doc  *ir.Workflow
		want AssistantCapabilities
	}{
		{
			name: "nil doc — all off",
			doc:  nil,
			want: AssistantCapabilities{},
		},
		{
			name: "minimal chat flow — no capabilities",
			doc: &ir.Workflow{
				Nodes: []*ir.Node{
					{ID: "start", Kind: ir.NodeKindStart},
					{ID: "chat", Kind: ir.NodeKindAction, ActionRef: "redelay/llm-chat"},
					{ID: "end", Kind: ir.NodeKindEnd},
				},
			},
			want: AssistantCapabilities{},
		},
		{
			name: "handoff node present — handoff on",
			doc: &ir.Workflow{
				Nodes: []*ir.Node{
					{ID: "handoff", Kind: ir.NodeKindAction, ActionRef: "redelay/assistant-handoff-request"},
				},
			},
			want: AssistantCapabilities{Handoff: true},
		},
		{
			name: "email-send node — escalation on",
			doc: &ir.Workflow{
				Nodes: []*ir.Node{
					{ID: "email", Kind: ir.NodeKindAction, ActionRef: "redelay/email-send"},
				},
			},
			want: AssistantCapabilities{Escalation: true},
		},
		{
			name: "llm-chat with stream=true — streaming on",
			doc: &ir.Workflow{
				Nodes: []*ir.Node{
					{
						ID:        "chat",
						Kind:      ir.NodeKindAction,
						ActionRef: "redelay/llm-chat",
						Config:    map[string]any{"stream": true},
					},
				},
			},
			want: AssistantCapabilities{Streaming: true},
		},
		{
			name: "production flow — all on",
			doc: &ir.Workflow{
				Nodes: []*ir.Node{
					{ID: "chat", ActionRef: "redelay/llm-chat", Config: map[string]any{"stream": true}},
					{ID: "handoff", ActionRef: "redelay/assistant-handoff-request"},
					{ID: "escalate", ActionRef: "redelay/email-send"},
				},
			},
			want: AssistantCapabilities{Handoff: true, Escalation: true, Streaming: true},
		},
		{
			name: "nil node in slice — safe",
			doc: &ir.Workflow{
				Nodes: []*ir.Node{nil, {ActionRef: "redelay/assistant-handoff-request"}},
			},
			want: AssistantCapabilities{Handoff: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := capabilitiesFromWorkflow(c.doc)
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}
