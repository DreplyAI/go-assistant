package assistant

import (
	fwconfig "github.com/redelay/go-framework/config"
)

// Default config values. All overridable via env.
const (
	defaultFlowID   = "assistant"
	defaultInputKey = "messages"
	// chatNodeID is the well-known id the module uses to locate the
	// LLM-chat node when applying ASSISTANT_PROVIDER / ASSISTANT_MODEL
	// env overrides to the seeded default flow. It is NOT the node that
	// produces the client-facing reply (see defaultOutputNodes for that).
	chatNodeID = "chat"
)

// defaultOutputNodes lists the terminal node ids whose `node.done`
// payload carries the final reply streamed to the client. Two entries
// because the flow has two terminals — `end` on the success path and
// `rejected` on the guard-block path (which injects a configurable
// refusal via a core/template-render transform). Either emits `content`
// under the same key; the SSE pump just picks whichever fires.
var defaultOutputNodes = []string{"end", "rejected"}

// Config is read from environment once at construction time.
type Config struct {
	// DeploymentID is the primary config knob — it names a
	// FlowDeployment in the flowexec store. The deployment's
	// variants resolve to concrete (flowId, versionId) pairs at
	// run time via sticky session bucketing.
	DeploymentID string // ASSISTANT_DEPLOYMENT_ID (default "assistant")

	// FlowID is the legacy knob from pre-deployment days. Kept so
	// existing installs keep working: on Startup, if a deployment
	// with ID=DeploymentID does not exist, one is synthesised with
	// a single stable variant pointing at this FlowID. Once you
	// manage variants through the deployment CRUD, the FlowID env
	// is no longer read.
	FlowID   string // ASSISTANT_FLOW_ID (legacy)
	InputKey string // ASSISTANT_INPUT_KEY

	// DocsIndex is the corpus/search index the docs live in. Read by both
	// the ingest command and the Startup registration, so the name cannot
	// drift between what is written and what the admin browser looks for.
	DocsIndex string // ASSISTANT_DOCS_INDEX (default "redelay_docs")
	// OutputNodes is the set of terminal node ids whose node.done
	// payload is surfaced to the client. Parsed from
	// ASSISTANT_OUTPUT_NODE as a comma-separated list so operators can
	// add a custom terminal (e.g. "handoff-ack") without code changes.
	OutputNodes      []string // ASSISTANT_OUTPUT_NODE (comma-separated)
	Provider         string   // ASSISTANT_PROVIDER
	Model            string   // ASSISTANT_MODEL
	ChatTTLDays      int      // ASSISTANT_CHAT_TTL_DAYS — default 30
	AnonymousAllowed bool     // ASSISTANT_ANONYMOUS_ALLOWED — default true
	HandoffEnabled   bool     // ASSISTANT_HANDOFF_ENABLED — default true
}

// DefaultConfig builds a Config from the ASSISTANT_* environment variables.
func DefaultConfig() Config {
	return Config{
		DeploymentID:     fwconfig.GetEnv("ASSISTANT_DEPLOYMENT_ID", defaultFlowID),
		FlowID:           fwconfig.GetEnv("ASSISTANT_FLOW_ID", defaultFlowID),
		InputKey:         fwconfig.GetEnv("ASSISTANT_INPUT_KEY", defaultInputKey),
		DocsIndex:        fwconfig.GetEnv("ASSISTANT_DOCS_INDEX", "redelay_docs"),
		OutputNodes:      parseOutputNodes(fwconfig.GetEnv("ASSISTANT_OUTPUT_NODE", "")),
		Provider:         fwconfig.GetEnv("ASSISTANT_PROVIDER", ""),
		Model:            fwconfig.GetEnv("ASSISTANT_MODEL", ""),
		ChatTTLDays:      fwconfig.GetInt("ASSISTANT_CHAT_TTL_DAYS", 30),
		AnonymousAllowed: fwconfig.GetBool("ASSISTANT_ANONYMOUS_ALLOWED", true),
		HandoffEnabled:   fwconfig.GetBool("ASSISTANT_HANDOFF_ENABLED", true),
	}
}

// parseOutputNodes splits a comma-separated env value into a clean
// slice of node ids. Empty values fall back to defaultOutputNodes so
// operators don't have to re-specify the defaults when overriding
// unrelated env vars.
func parseOutputNodes(raw string) []string {
	out := make([]string, 0, 2)
	for _, part := range splitAndTrim(raw, ",") {
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), defaultOutputNodes...)
	}
	return out
}

// splitAndTrim is a tiny helper used by parseOutputNodes; lives here
// rather than in a util package because it's the only caller.
func splitAndTrim(s, sep string) []string {
	if s == "" {
		return nil
	}
	parts := make([]string, 0, 2)
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || string(s[i]) == sep {
			chunk := s[start:i]
			// trim ASCII whitespace
			for len(chunk) > 0 && (chunk[0] == ' ' || chunk[0] == '\t') {
				chunk = chunk[1:]
			}
			for len(chunk) > 0 && (chunk[len(chunk)-1] == ' ' || chunk[len(chunk)-1] == '\t') {
				chunk = chunk[:len(chunk)-1]
			}
			parts = append(parts, chunk)
			start = i + 1
		}
	}
	return parts
}
