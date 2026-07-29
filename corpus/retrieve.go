package corpus

import (
	"context"
	"errors"
	"fmt"

	"github.com/redelay/go-modules/search"
)

// ErrNotReady means the search service is not available. Callers that can
// degrade — a flow node that would rather answer without citations than fail
// the run — should check for it rather than treating it as a hard error.
var ErrNotReady = errors.New("corpus: search service not ready")

// Request is one retrieval.
type Request struct {
	Query string

	// TopK and MinScore override the corpus defaults when non-zero. Left at
	// zero — the normal case — the corpus declaration decides.
	TopK     int
	MinScore float32

	// Filter is an equality filter on payload fields, e.g. {"language": "en"}.
	Filter map[string]any

	// NoRewrite embeds Query verbatim, skipping the corpus's Rewriter. For
	// callers that need the score for exactly this text — threshold tuning in
	// the admin, and any measurement of the rewriter's own effect.
	NoRewrite bool
}

// Retrieve returns citations above the corpus's relevance floor.
//
// The floor is applied here, once, for every consumer. That is the difference
// between this and calling search.Search directly: search.Query has no score
// threshold, so today every caller reimplements one and the constants end up
// buried at the call site — the coach has 0.45 in one function and 0.55 in
// another, and an operator asking "why does it never cite anything" has to
// read Go to find out.
//
// A result below the floor is not a weak citation, it is not a citation. The
// model treats anything handed to it as relevant, so a 0.2 hit does not
// produce a hedged answer — it produces a confident answer about the wrong
// paper.
func Retrieve(ctx context.Context, corpusName string, r Request) ([]Citation, error) {
	c, ok := Get(corpusName)
	if !ok {
		return nil, fmt.Errorf("corpus: %q is not registered", corpusName)
	}
	svc := search.Current()
	if svc == nil {
		return nil, ErrNotReady
	}

	topK := r.TopK
	if topK <= 0 {
		topK = c.TopK
	}
	floor := r.MinScore
	if floor <= 0 {
		floor = c.MinScore
	}

	// Over-fetch, because the floor is applied after ranking: asking for
	// exactly topK and then dropping half of them returns fewer citations
	// than the caller asked for whenever the corpus is thin on a topic.
	limit := topK * 3
	if limit < topK {
		limit = topK
	}

	// Rewrite before embedding. Measured on the sports-science corpus: "does
	// creatine really work?" retrieves the wrong paper at 0.534, the rewritten
	// form the right one at 0.752, while an off-topic question gains only
	// 0.03 — so this widens the gap the floor sits in rather than lifting
	// every score into it.
	//
	// Skipped when the caller passes NoRewrite, which the admin test-query
	// does: an operator moving a threshold needs to see the raw score for the
	// text they typed, not for something a model wrote.
	query := r.Query
	if c.Rewriter != nil && !r.NoRewrite {
		query = c.Rewriter.Rewrite(ctx, r.Query)
	}

	hits, err := svc.Search(ctx, c.Name, search.Query{
		Text:    query,
		Limit:   limit,
		Payload: r.Filter,
	})
	if err != nil {
		return nil, err
	}

	out := make([]Citation, 0, topK)
	for _, h := range hits {
		if h.Score < floor {
			// Hits are score-ordered, so the first miss ends the list.
			break
		}
		out = append(out, CitationFrom(h, c))
		if len(out) >= topK {
			break
		}
	}
	return out, nil
}

// CitationFrom turns a search hit into a citation.
//
// Everything a card needs is on the payload, so this is a projection rather
// than a lookup — no database round trip on the chat hot path.
func CitationFrom(h search.Hit, c Corpus) Citation {
	cit := Citation{
		ID:        h.ID,
		Score:     h.Score,
		DocID:     str(h.Payload["docId"]),
		Kind:      firstNonEmpty(str(h.Payload["kind"]), c.Kind),
		Title:     str(h.Payload["title"]),
		URL:       str(h.Payload["url"]),
		Path:      str(h.Payload["path"]),
		Snippet:   str(h.Payload["text"]),
		Published: str(h.Payload["published"]),
		Container: str(h.Payload["container"]),
		Licence:   str(h.Payload["licence"]),
	}
	// Anything the corpus stamped that is not a known field travels on Extra,
	// so a consumer that knows about `citedBy` can read it without the generic
	// layer knowing what a citation count is.
	known := map[string]bool{
		"docId": true, "kind": true, "title": true, "url": true, "path": true,
		"text": true, "published": true, "container": true, "licence": true,
		"corpus": true, "snapshot": true, "ordinal": true, "heading": true,
	}
	for k, v := range h.Payload {
		if !known[k] {
			if cit.Extra == nil {
				cit.Extra = map[string]any{}
			}
			cit.Extra[k] = v
		}
	}
	return cit
}

// ToCard flattens a citation into the map the SSE layer forwards as a source.
func (c Citation) ToCard() map[string]any {
	m := map[string]any{"id": c.ID, "kind": c.Kind, "title": c.Title, "score": c.Score}
	putIf(m, "url", c.URL)
	putIf(m, "path", c.Path)
	putIf(m, "published", c.Published)
	putIf(m, "container", c.Container)
	for k, v := range c.Extra {
		if _, taken := m[k]; !taken {
			m[k] = v
		}
	}
	// Snippet is deliberately absent. It is the embedded text, and for some
	// corpora that text is the publisher's copyright — it grounds the model
	// and does not go on screen. A consumer that may render it can read
	// Citation.Snippet knowingly.
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
