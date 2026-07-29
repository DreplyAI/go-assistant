package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dreplyai/go-assistant/corpus"
)

func jsonBody(s string) io.Reader { return strings.NewReader(s) }

func TestListCorporaReportsNoStoreDistinctlyFromEmpty(t *testing.T) {
	corpus.ResetForTest()
	corpus.Register(corpus.Corpus{
		Name: "papers", Title: "Papers", Owner: "coachchat",
		Kind: "paper", Dim: 1024, MinScore: 0.55, TopK: 3,
	})

	m := &Module{}
	w := httptest.NewRecorder()
	m.handleListCorpora(w, httptest.NewRequest(http.MethodGet, "/corpora", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Corpora []corpusSummary `json:"corpora"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Corpora) != 1 {
		t.Fatalf("want 1 corpus, got %d", len(body.Corpora))
	}
	c := body.Corpora[0]
	// -1, not 0. "No document store registered" and "the store is empty" are
	// different answers, and a zero would read as an empty corpus and send
	// someone looking for a data problem that does not exist.
	if c.HasStore || c.Documents != -1 {
		t.Errorf("no store should report Documents=-1, HasStore=false; got %d/%v", c.Documents, c.HasStore)
	}
	// The floor has to reach the UI, or the page cannot explain itself.
	if c.MinScore != 0.55 {
		t.Errorf("minScore = %v, want 0.55", c.MinScore)
	}
}

func TestCorpusDocsWithoutAStoreIs404NotEmpty(t *testing.T) {
	corpus.ResetForTest()
	corpus.Register(corpus.Corpus{Name: "papers", Dim: 1024})

	m := &Module{}
	r := httptest.NewRequest(http.MethodGet, "/corpora/papers/documents", nil)
	r.SetPathValue("name", "papers")
	w := httptest.NewRecorder()
	m.handleListCorpusDocs(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 — an empty page would look like an empty corpus", w.Code)
	}
}

func TestTestQueryRejectsAnEmptyQuery(t *testing.T) {
	corpus.ResetForTest()
	corpus.Register(corpus.Corpus{Name: "papers", Dim: 1024, MinScore: 0.5})

	m := &Module{}
	r := httptest.NewRequest(http.MethodPost, "/corpora/papers/test-query",
		jsonBody(`{"query":""}`))
	r.SetPathValue("name", "papers")
	w := httptest.NewRecorder()
	m.handleCorpusTestQuery(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
}

func TestTestQueryOnUnknownCorpusIs404(t *testing.T) {
	corpus.ResetForTest()

	m := &Module{}
	r := httptest.NewRequest(http.MethodPost, "/corpora/nope/test-query", jsonBody(`{"query":"x"}`))
	r.SetPathValue("name", "nope")
	w := httptest.NewRecorder()
	m.handleCorpusTestQuery(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
}
