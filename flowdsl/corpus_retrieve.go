package flowdsl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dreplyai/go-assistant/corpus"
	"github.com/redelay/go-flowdsl/runtime"
)

// CorpusRetrieveActionRef is the node id.
const CorpusRetrieveActionRef = "redelay/assistant-corpus-retrieve"

// RegisterCorpusRetrieve wires the node onto engine.
//
// Separate from RegisterRagContext so a project can take one node without the
// other — they solve the same problem for different flow shapes and a project
// rarely wants both.
func RegisterCorpusRetrieve(engine *runtime.Engine) {
	if engine == nil {
		return
	}
	engine.RegisterHandler(CorpusRetrieveActionRef, corpusRetrieveHandler)
}

// corpusRetrieveHandler retrieves citations from a corpus and writes them to
// ONE packet key, leaving everything else on the packet untouched.
//
// That restraint is the whole reason this node exists next to
// `redelay/assistant-rag-context` rather than replacing it. The older node
// rewrites `messages` — prepending a system block of its own — and overwrites
// `sources` and `actions`. That is right for a flow whose entire job is
// document Q&A, and impossible for a flow that composes its own prompt from
// several sources, because the node would clobber the composition. Writing one
// key means the flow decides what the citations are worth.
//
// Config:
//
//	corpus            required — a registered corpus name
//	topK, minScore    override the corpus defaults; omit to inherit them
//	outputKey         packet key to write; default "evidence"
//	queryKey          read the query from this input key instead of the last
//	                  user message — for flows that rewrite the query first
//	gateKey           skip retrieval unless this input key is truthy
//	filter            payload equality filter, e.g. {"language": "en"}
//	failOnSearchError treat an unavailable index as a run failure
func corpusRetrieveHandler(ctx context.Context, step *runtime.Step) error {
	cfg := step.Node.Config
	name := strings.TrimSpace(configString(cfg, "corpus"))
	if name == "" {
		return fmt.Errorf("%s: config.corpus is required", CorpusRetrieveActionRef)
	}
	outputKey := configString(cfg, "outputKey")
	if outputKey == "" {
		outputKey = "evidence"
	}

	// Pass the packet through. A retrieval node that dropped the packet would
	// break any flow that has stages after it.
	step.Output = passthrough(step.Input)

	// The gate is the caller's, not ours: whether a turn deserves a lookup is
	// a domain question, and the node only honours the answer. Skipping is a
	// recorded outcome rather than an empty result, so "why were there no
	// citations" is answerable from the trace.
	if key := configString(cfg, "gateKey"); key != "" && !truthy(step.Input[key]) {
		step.Output[outputKey] = result{Asked: false, Reason: "gate " + key + " not set"}
		return nil
	}

	query := extractQuery(step.Input, configString(cfg, "queryKey"))
	if query == "" {
		step.Output[outputKey] = result{Asked: false, Reason: "no query"}
		return nil
	}

	hits, err := corpus.Retrieve(ctx, name, corpus.Request{
		Query:    query,
		TopK:     configInt(cfg, "topK"),
		MinScore: float32(configFloat(cfg, "minScore")),
		Filter:   configMap(cfg, "filter"),
	})
	if err != nil {
		if configBool(cfg, "failOnSearchError") {
			return fmt.Errorf("%s: %w", CorpusRetrieveActionRef, err)
		}
		// Degrade rather than fail. An assistant that answers without
		// citations is worth more than one that returns an error because the
		// vector store is restarting — and `reason` says which happened.
		reason := "retrieval failed"
		if errors.Is(err, corpus.ErrNotReady) {
			reason = "search not ready"
		}
		step.Output[outputKey] = result{Asked: true, Query: query, Reason: reason}
		return nil
	}

	cards := make([]any, 0, len(hits))
	for _, h := range hits {
		cards = append(cards, h.ToCard())
	}
	r := result{Asked: true, Query: query, Hits: cards}
	if len(hits) == 0 {
		// Distinct from "we did not look": nothing cleared the floor, which
		// is a normal outcome and the prompt should say nothing about
		// research rather than hedge.
		r.Reason = "no hit above the corpus floor"
	}
	step.Output[outputKey] = r
	return nil
}

// result is the packet this node writes.
type result struct {
	Asked  bool   `json:"asked"`
	Reason string `json:"reason,omitempty"`
	Query  string `json:"query,omitempty"`
	Hits   []any  `json:"hits,omitempty"`
}

// extractQuery prefers an explicit key, else the last user message.
func extractQuery(in map[string]any, key string) string {
	if key != "" {
		if s := strings.TrimSpace(configString(in, key)); s != "" {
			return s
		}
	}
	msgs, _ := in["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		if c, _ := m["content"].(string); strings.TrimSpace(c) != "" {
			return strings.TrimSpace(c)
		}
	}
	return ""
}

func passthrough(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != "" && t != "false" && t != "0"
	case float64:
		return t != 0
	case int:
		return t != 0
	case nil:
		return false
	}
	return true
}

func configString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func configBool(m map[string]any, k string) bool { return truthy(m[k]) }

func configInt(m map[string]any, k string) int {
	switch n := m[k].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func configFloat(m map[string]any, k string) float64 {
	switch n := m[k].(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

func configMap(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}
