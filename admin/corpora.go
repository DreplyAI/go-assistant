package admin

import (
	"net/http"
	"strconv"

	"github.com/dreplyai/go-assistant/corpus"
	"github.com/redelay/go-framework/server/httputil"
	"github.com/redelay/go-modules/search"
)

// The corpus admin surface.
//
// Two questions the search admin cannot answer, because it browses an index of
// chunks and neither of them is about a chunk:
//
//   - which documents do we actually have, from which source, under which
//     licence, and is this corpus any good?
//   - why does the assistant never cite anything?
//
// The second is the one that earns this page. Retrieval applies a relevance
// floor, so a corpus can be full, correctly embedded and still silent — and
// from the search admin's raw test-query, which knows nothing about the floor,
// that looks identical to a corpus with no matches. Here the dropped hits are
// shown with their scores, so tuning the threshold is watching rows cross a
// line instead of reading Go.

// corpusSummary is one row of the corpora list.
type corpusSummary struct {
	Name        string  `json:"name"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Icon        string  `json:"icon"`
	Owner       string  `json:"owner"`
	Kind        string  `json:"kind"`
	Dim         int     `json:"dim"`
	MinScore    float32 `json:"minScore"`
	TopK        int     `json:"topK"`

	// Points is the live vector count; Documents the stored document count.
	// Both are best-effort — a corpus with no doc store reports -1 documents
	// rather than 0, because "none registered" and "none stored" are
	// different answers and a zero would read as an empty corpus.
	Points    int64 `json:"points"`
	Documents int64 `json:"documents"`
	HasStore  bool  `json:"hasStore"`
}

func (m *Module) handleListCorpora(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	svc := search.Current()

	out := make([]corpusSummary, 0)
	for _, c := range corpus.All() {
		row := corpusSummary{
			Name: c.Name, Title: c.Title, Description: c.Description,
			Icon: c.Icon, Owner: c.Owner, Kind: c.Kind, Dim: c.Dim,
			MinScore: c.MinScore, TopK: c.TopK, Documents: -1,
		}
		if svc != nil {
			if ix, err := svc.GetIndex(ctx, c.Name); err == nil && ix != nil {
				row.Points = ix.PointsCount
			}
		}
		if store, ok := corpus.Store(c.Name); ok {
			row.HasStore = true
			if n, err := store.Count(ctx, c.Name); err == nil {
				row.Documents = n
			}
		}
		out = append(out, row)
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"corpora": out})
}

func (m *Module) handleListCorpusDocs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	store, ok := corpus.Store(name)
	if !ok {
		// 404 rather than an empty page: a corpus without a document store is
		// not an empty corpus, and rendering one as empty would send someone
		// looking for a data problem that does not exist.
		httputil.Error(w, http.StatusNotFound, "corpus has no document store")
		return
	}
	q := r.URL.Query()
	page, err := store.List(r.Context(), name, corpus.DocQuery{
		Search:  q.Get("search"),
		Licence: q.Get("licence"),
		Source:  q.Get("source"),
		Kind:    q.Get("kind"),
		From:    atoi(q.Get("from")),
		To:      atoi(q.Get("to")),
		Limit:   atoi(q.Get("limit")),
		Offset:  atoi(q.Get("offset")),
	})
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, page)
}

func (m *Module) handleGetCorpusDoc(w http.ResponseWriter, r *http.Request) {
	name, id := r.PathValue("name"), r.PathValue("id")
	store, ok := corpus.Store(name)
	if !ok {
		httputil.Error(w, http.StatusNotFound, "corpus has no document store")
		return
	}
	doc, err := store.Get(r.Context(), name, id)
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if doc == nil {
		httputil.Error(w, http.StatusNotFound, "document not found")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, doc)
}

// testQueryRequest is the threshold-tuning body.
type testQueryRequest struct {
	Query string `json:"query"`
	TopK  int    `json:"topK"`
	// MinScore overrides the corpus floor for this call only. Zero means
	// "use the corpus setting", which is the point — an operator moves this
	// until the right rows survive, then changes the declaration.
	MinScore float32 `json:"minScore"`

	// Raw skips the corpus's query rewriter.
	//
	// Defaults to false, i.e. the tool measures what RETRIEVAL does. It
	// previously always skipped it, silently: an operator tuned a threshold
	// against scores for the text they typed while the assistant embedded
	// something a model had rewritten, so the numbers on screen were for a
	// different query than the one production runs. Raw is still worth having —
	// comparing the two is how you see whether the rewriter earns its latency.
	Raw bool `json:"raw"`
}

// scoredHit is one result, with whether the floor kept it.
type scoredHit struct {
	Kept bool           `json:"kept"`
	Hit  map[string]any `json:"hit"`
}

func (m *Module) handleCorpusTestQuery(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, ok := corpus.Get(name)
	if !ok {
		httputil.Error(w, http.StatusNotFound, "corpus not registered")
		return
	}
	var body testQueryRequest
	if err := httputil.ReadJSON(r, &body); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Query == "" {
		httputil.Error(w, http.StatusBadRequest, "query is required")
		return
	}
	svc := search.Current()
	if svc == nil {
		httputil.Error(w, http.StatusServiceUnavailable, "search disabled")
		return
	}

	floor := body.MinScore
	if floor <= 0 {
		floor = c.MinScore
	}
	topK := body.TopK
	if topK <= 0 {
		topK = c.TopK
	}

	// Still not corpus.Retrieve — that returns only survivors, which is right
	// for the assistant and useless here: the operator's question is "what did
	// the floor throw away". So this over-fetches and labels each hit.
	//
	// But it now runs the corpus's rewriter first, exactly as Retrieve would,
	// because a threshold tuned against un-rewritten scores is tuned against a
	// query production never issues.
	embedded := body.Query
	if c.Rewriter != nil && !body.Raw {
		embedded = c.Rewriter.Rewrite(r.Context(), body.Query)
	}
	hits, err := svc.Search(r.Context(), c.Name, search.Query{Text: embedded, Limit: topK * 4})
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]scoredHit, 0, len(hits))
	kept := 0
	for _, h := range hits {
		keep := h.Score >= floor && kept < topK
		if keep {
			kept++
		}
		card := corpus.CitationFrom(h, c).ToCard()
		card["score"] = h.Score
		out = append(out, scoredHit{Kept: keep, Hit: card})
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"corpus": c.Name,
		"query":  body.Query,
		// What was actually embedded. Equal to `query` unless a rewriter ran —
		// and when they differ, seeing both is the only way to tell a bad floor
		// from a bad rewrite.
		"embedded":  embedded,
		"rewritten": embedded != body.Query,
		"minScore": floor,
		"topK":     topK,
		"kept":     kept,
		"results":  out,
	})
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
