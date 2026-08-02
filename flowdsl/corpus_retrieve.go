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
		step.Output[outputKey] = result{Asked: false, Reason: "gate " + key + " not set"}.asPacket()
		return nil
	}

	query := extractQuery(step.Input, configString(cfg, "queryKey"))
	if query == "" {
		step.Output[outputKey] = result{Asked: false, Reason: "no query"}.asPacket()
		return nil
	}

	hits, err := corpus.Retrieve(ctx, name, corpus.Request{
		Query:    query,
		TopK:     configInt(cfg, "topK"),
		MinScore: float32(configFloat(cfg, "minScore")),
		Filter:   configMap(cfg, "filter"),
	})
	if err != nil {
		if configBool(cfg, "failOnSearchError", false) {
			return fmt.Errorf("%s: %w", CorpusRetrieveActionRef, err)
		}
		// Degrade rather than fail. An assistant that answers without
		// citations is worth more than one that returns an error because the
		// vector store is restarting — and `reason` says which happened.
		reason := "retrieval failed"
		switch {
		case errors.Is(err, corpus.ErrNotReady):
			reason = "search not ready"
		case errors.Is(err, corpus.ErrNotRegistered):
			// Names the actual problem. Most often the corpus's module is
			// opt-in and was never enabled, which looks nothing like a search
			// outage but produces the same empty answer.
			reason = "corpus " + name + " is not registered"
		}
		step.Output[outputKey] = result{Asked: true, Query: query, Reason: reason}.asPacket()
		return nil
	}

	// A hit carries BOTH representations, and they are not the same thing.
	//
	// ToCard is the DISPLAY form and deliberately omits the chunk text: for the
	// papers corpus the reconstructed abstract may be used to ground the model
	// but must never be rendered, so a card that carried it would leak it into
	// the UI the moment anything echoed `sources`.
	//
	// Grounding needs that text — an excerpt list of bare titles tells the model
	// nothing. So it rides alongside under `text`, and the grounding node strips
	// it back out when it builds `sources`. Keeping the two apart here, rather
	// than trusting each consumer to remember, is what makes the licence rule
	// structural instead of a comment someone has to read.
	cards := make([]any, 0, len(hits))
	for _, h := range hits {
		card := h.ToCard()
		if h.Snippet != "" {
			card["text"] = h.Snippet
		}
		cards = append(cards, card)
	}
	r := result{Asked: true, Query: query, Hits: cards}
	if len(hits) == 0 {
		// Distinct from "we did not look": nothing cleared the floor, which
		// is a normal outcome and the prompt should say nothing about
		// research rather than hedge.
		r.Reason = "no hit above the corpus floor"
	}
	step.Output[outputKey] = r.asPacket()
	return nil
}

// result is the packet this node writes.
//
// Written into the packet as a MAP, never as this struct — see asPacket. A
// packet is a JSON document by contract, and a struct only becomes one if the
// engine happens to serialise between the two nodes. In `direct` delivery it
// does not, so the grounding node downstream received a `result` value where it
// expected a map, found no hits, and silently produced an ungrounded answer.
// The same flow in `durable` mode worked, because the round trip through the
// store converted it. A node whose output depends on delivery mode is a node
// that will be debugged twice.
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

// configBool reads a boolean setting, honouring a default when the key is
// absent. The default matters: an omitted setting is "not stated", and for
// `emitSources` and friends the stated-default is true — reading a missing key
// as false would silently disable them for every flow that did not spell them
// out.
func configBool(m map[string]any, k string, def bool) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	if _, present := m[k]; !present {
		return def
	}
	return truthy(m[k])
}

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

// asPacket renders the result the way it would look after a JSON round trip,
// so a downstream node reads the same shape in every delivery mode.
//
// Omitted keys mirror the `omitempty` tags rather than emitting nulls: a
// consumer checking `reason` should find it absent, not present-and-empty,
// because "there was no reason" and "the reason was blank" mean different
// things when you are reading a trace to find out why nothing was cited.
func (r result) asPacket() map[string]any {
	m := map[string]any{"asked": r.Asked}
	if r.Reason != "" {
		m["reason"] = r.Reason
	}
	if r.Query != "" {
		m["query"] = r.Query
	}
	if len(r.Hits) > 0 {
		m["hits"] = r.Hits
	}
	return m
}
