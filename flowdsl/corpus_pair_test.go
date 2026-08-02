package flowdsl

import (
	"context"
	"strings"
	"testing"

	"github.com/dreplyai/go-assistant/corpus"
	"github.com/redelay/go-ai/llm"
	"github.com/redelay/go-flowdsl/ir"
	"github.com/redelay/go-flowdsl/runtime"
	"github.com/redelay/go-modules/search"
)

// The pair, end to end, through the real handlers.
//
// This is L10's actual claim: an assistant built on the generic corpus layer
// gets FLOORED citations. Each half is unit tested elsewhere; what is only
// testable together is whether the floor a corpus declares still applies by the
// time the excerpts reach the prompt. It would be entirely possible for both
// halves to pass their own tests while a weak hit sails through — the retrieve
// node would have to drop it, and nothing downstream would know it had not.

// fakeBackend returns a fixed ranked list, ignoring the query. The scores are
// the subject of the test; the text is not.
type fakeBackend struct{ hits []search.Hit }

func (f *fakeBackend) ID() string { return "fake" }

func (f *fakeBackend) Search(_ context.Context, _ string, _ search.Query) ([]search.Hit, error) {
	return f.hits, nil
}

// The rest of Backend, unimplemented. Retrieval touches only Search; a mock
// that pretended to do the others would invite a test to rely on behaviour
// nothing here provides.
func (f *fakeBackend) CreateIndex(context.Context, search.Index) error     { return nil }
func (f *fakeBackend) DeleteIndex(context.Context, string) error           { return nil }
func (f *fakeBackend) ListIndexes(context.Context) ([]search.Index, error) { return nil, nil }
func (f *fakeBackend) GetIndex(context.Context, string) (*search.Index, error) {
	return nil, nil
}
func (f *fakeBackend) Upsert(context.Context, string, []search.Document) error { return nil }
func (f *fakeBackend) Delete(context.Context, string, []string) error          { return nil }
func (f *fakeBackend) ListPoints(context.Context, string, search.ListPointsOpts) (*search.ListPointsResult, error) {
	return nil, nil
}

func hit(id string, score float32, title, text string) search.Hit {
	return search.Hit{
		ID:    id,
		Score: score,
		Payload: map[string]any{
			"docId": id, "title": title, "text": text, "kind": "doc",
		},
	}
}

// fakeEmbedder exists because search.Service embeds the query text before it
// reaches the backend, so a test with no embedding provider never gets as far
// as the hits it set up — it fails with "no embedding provider configured",
// which reads like a broken test rather than a missing fixture.
//
// The vector is constant and meaningless: the fake backend ignores it and
// returns a fixed ranked list. Ranking is the vector store's job and is not
// what these tests are about; what they are about is what happens to the scores
// afterwards.
type fakeEmbedder struct{}

func (fakeEmbedder) ID() string                     { return "fake-embed" }
func (fakeEmbedder) Capabilities() llm.Capabilities { return llm.Capabilities{Embeddings: true} }
func (fakeEmbedder) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return nil, nil
}
func (fakeEmbedder) ChatStream(context.Context, llm.ChatRequest) (<-chan llm.Chunk, error) {
	return nil, nil
}
func (fakeEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	out := make([][]float32, len(req.Input))
	for i := range out {
		out[i] = []float32{1, 0, 0}
	}
	return &llm.EmbedResponse{Vectors: out}, nil
}

// installSearch wires a fake service in as the live one for the duration of a
// test, and registers a corpus with a known floor.
func installSearch(t *testing.T, floor float32, hits ...search.Hit) {
	t.Helper()
	llm.Register(fakeEmbedder{})
	svc := search.NewService(search.ServiceConfig{
		Backend:         &fakeBackend{hits: hits},
		EmbedProviderID: "fake-embed",
		EmbedDim:        3,
	})
	t.Cleanup(search.SetCurrentForTest(svc))

	corpus.ResetForTest()
	corpus.Register(corpus.Corpus{
		Name: "test_docs", Title: "Test docs", Owner: "test",
		Kind: "doc", MinScore: floor, TopK: 3,
	})
	t.Cleanup(corpus.ResetForTest)
}

// run drives retrieve then context, exactly as the flow's edges do.
func run(t *testing.T, retrieveCfg, groundCfg map[string]any, in map[string]any) map[string]any {
	t.Helper()
	r := &runtime.Step{Node: &ir.Node{Config: retrieveCfg}, Input: in}
	if err := corpusRetrieveHandler(context.Background(), r); err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	g := &runtime.Step{Node: &ir.Node{Config: groundCfg}, Input: r.Output}
	if err := corpusContextHandler(context.Background(), g); err != nil {
		t.Fatalf("ground: %v", err)
	}
	return g.Output
}

func ask(q string) map[string]any {
	return map[string]any{"messages": []any{map[string]any{"role": "user", "content": q}}}
}

func TestFlooredCitationsReachThePrompt(t *testing.T) {
	installSearch(t, 0.5,
		hit("good", 0.81, "Creating a module", "Modules register via an init() factory."),
		hit("weak", 0.22, "Deployment topology", "Nothing to do with modules."),
	)

	out := run(t,
		map[string]any{"corpus": "test_docs"},
		nil,
		ask("how do I create a module"),
	)

	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected grounding + the user turn, got %d", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	content, _ := sys["content"].(string)

	if !strings.Contains(content, "init() factory") {
		t.Error("the above-floor excerpt never reached the prompt")
	}
	// The whole point. A 0.22 hit does not make the model hedge — it makes the
	// model answer confidently about deployment topology.
	if strings.Contains(content, "Nothing to do with modules") {
		t.Error("a below-floor hit was handed to the model as evidence")
	}

	srcs, _ := out["sources"].([]any)
	if len(srcs) != 1 {
		t.Fatalf("expected 1 citation card, got %d", len(srcs))
	}
	if cardStr(srcs[0].(map[string]any), "id") != "good" {
		t.Error("the wrong hit was cited")
	}
}

func TestNothingAboveTheFloorGroundsNothing(t *testing.T) {
	// Not the same as an error, and not the same as "the docs say nothing".
	// The prompt must be left alone so the model answers from what it has and
	// says so, rather than being told an empty excerpt list is the evidence.
	installSearch(t, 0.5, hit("weak", 0.31, "Unrelated", "…"))

	out := run(t, map[string]any{"corpus": "test_docs"}, nil, ask("anything"))

	if msgs, _ := out["messages"].([]any); len(msgs) != 1 {
		t.Errorf("messages were rewritten with nothing above the floor: %d turns", len(msgs))
	}
	if out["grounded"] != false {
		t.Error("grounded must be false so the trace shows an ungrounded answer")
	}
	// Read as a map, deliberately: that is what a downstream node sees, and
	// asserting against the Go struct would pass even if the node wrote
	// something no other node could read.
	ev, ok := out["evidence"].(map[string]any)
	if !ok {
		t.Fatalf("evidence is %T, not a packet map", out["evidence"])
	}
	if ev["reason"] != "no hit above the corpus floor" {
		t.Errorf("reason = %v; an operator needs to tell this from an outage", ev["reason"])
	}
}

func TestAnUnregisteredCorpusDegradesAndSaysWhy(t *testing.T) {
	// The most likely operational failure: the corpus module is opt-in and the
	// environment variable was never set. It must not kill the chat, and the
	// reason must not send someone to look at a vector store that is fine.
	installSearch(t, 0.5, hit("x", 0.9, "T", "…"))

	out := run(t, map[string]any{"corpus": "not_a_corpus"}, nil, ask("anything"))

	ev, _ := out["evidence"].(map[string]any)
	if reason, _ := ev["reason"].(string); !strings.Contains(reason, "not registered") {
		t.Errorf("reason = %q, want it to name the missing registration", reason)
	}
	if msgs, _ := out["messages"].([]any); len(msgs) != 1 {
		t.Error("a missing corpus must degrade to an ungrounded answer, not a broken prompt")
	}
}

func TestTopKCapsWhatReachesThePrompt(t *testing.T) {
	// Every hit clears the floor here; the cap is the only thing limiting the
	// prompt. Without it a broad question pulls the whole corpus into the
	// context window.
	installSearch(t, 0.1,
		hit("a", 0.9, "A", "one"), hit("b", 0.8, "B", "two"),
		hit("c", 0.7, "C", "three"), hit("d", 0.6, "D", "four"),
	)

	out := run(t, map[string]any{"corpus": "test_docs"}, nil, ask("everything"))

	if srcs, _ := out["sources"].([]any); len(srcs) != 3 {
		t.Errorf("expected the corpus TopK of 3, got %d", len(srcs))
	}
}

func TestAnUndeclaredFloorStillFloors(t *testing.T) {
	// A corpus registered without MinScore used to get 0 — and 0 does not mean
	// "default", it means every hit is a citation. The feature silently did not
	// exist for that corpus, which is worse than not having it.
	corpus.ResetForTest()
	t.Cleanup(corpus.ResetForTest)
	corpus.Register(corpus.Corpus{Name: "no_floor", Owner: "test"})

	c, _ := corpus.Get("no_floor")
	if c.MinScore <= 0 {
		t.Fatalf("MinScore = %v — every hit would be a citation", c.MinScore)
	}
}

func TestGroundingTextNeverReachesSources(t *testing.T) {
	// The licence rule, made testable.
	//
	// For the papers corpus the reconstructed abstract may be embedded and given
	// to the model, and may NEVER be rendered. `sources` is what the UI draws,
	// so the text must be in the prompt and absent from the cards. Both halves
	// matter: drop it from the prompt and the model gets a list of titles;
	// leave it on the card and we publish something we are not licensed to.
	installSearch(t, 0.5, hit("p1", 0.9, "A creatine review", "SECRET-ABSTRACT-TEXT"))

	out := run(t, map[string]any{"corpus": "test_docs"}, nil, ask("does creatine work"))

	msgs, _ := out["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	if content, _ := sys["content"].(string); !strings.Contains(content, "SECRET-ABSTRACT-TEXT") {
		t.Error("grounding text did not reach the model — the excerpt list is titles only")
	}

	srcs, _ := out["sources"].([]any)
	if len(srcs) != 1 {
		t.Fatalf("expected 1 card, got %d", len(srcs))
	}
	card := srcs[0].(map[string]any)
	for _, k := range []string{"text", "snippet"} {
		if _, present := card[k]; present {
			t.Errorf("card carries %q — the grounding copy is renderable from `sources`", k)
		}
	}
	if cardStr(card, "title") != "A creatine review" {
		t.Error("the card lost the metadata it is actually for")
	}
}
