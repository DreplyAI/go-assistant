package corpus

import (
	"sort"
	"sync"

	"github.com/redelay/go-modules/search"
)

// Corpus is a named, configured body of documents.
//
// The descriptor exists so that retrieval, the admin browser and the ingester
// all read one declaration instead of three copies of the same constants. In
// particular MinScore lives here rather than at each call site: today the
// coach hardcodes 0.45 in one function and 0.55 in another, and neither number
// is visible to whoever is asking why the assistant never cites anything.
type Corpus struct {
	// Name is the search index name and the payload tag.
	Name string

	Title       string
	Description string
	Icon        string
	Owner       string // module id, for the admin browser

	// Kind is the default Doc.Kind, and drives how the UI renders the card.
	Kind string

	Dim int

	// MinScore is the relevance floor. Below it, a hit is not a citation.
	//
	// A property of the corpus and its embedding model, not of a query: the
	// same 0.6 means different things against bge-m3 and ada-002, and nothing
	// about an individual question changes where the line should sit.
	MinScore float32

	TopK int

	// Rewriter turns a user's question into a query for THIS corpus — into its
	// language and the register its documents are written in. Optional: a
	// corpus without one embeds what the user typed.
	//
	// On the corpus rather than global because the right rewrite depends on
	// what is indexed. A corpus of English abstracts and one of German legal
	// opinions want different instructions, and a single process can hold both.
	Rewriter Rewriter

	Fields   []search.IndexField
	Examples []string

	Reindex search.Reindexer
}

var (
	mu       sync.RWMutex
	registry = map[string]Corpus{}
)

// Register declares a corpus and wires up everything that can be derived
// from the declaration.
//
// Three things happen for free, which is the reason to have a descriptor at
// all rather than passing an index name around:
//
//   - the admin index browser gains a labelled entry, with typed fields and
//     working example queries;
//   - a Reindex button appears, when the caller supplied a reindexer;
//   - retrieval and the admin test-query share one MinScore, so tuning the
//     floor is a config change rather than a code read.
//
// Last registration wins, matching search.RegisterEntity, so a project can
// override a corpus a library declared.
func Register(c Corpus) {
	if c.Name == "" {
		return
	}
	if c.TopK <= 0 {
		c.TopK = 3
	}
	if c.Kind == "" {
		c.Kind = "doc"
	}

	mu.Lock()
	registry[c.Name] = c
	mu.Unlock()

	search.RegisterEntity(c.entity())
	if c.Reindex != nil {
		search.RegisterReindexer(c.Name, c.Reindex)
	}
}

// Get returns a registered corpus.
func Get(name string) (Corpus, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[name]
	return c, ok
}

// All returns every registered corpus, name-ordered so listings are stable.
func All() []Corpus {
	mu.RLock()
	out := make([]Corpus, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (c Corpus) entity() search.IndexEntity {
	fields := c.Fields
	if len(fields) == 0 {
		// Every corpus writes these, so a caller that declares nothing still
		// gets a browser that renders rather than an empty schema tab.
		fields = []search.IndexField{
			{Name: "title", Type: "string", Description: "Document title"},
			{Name: "text", Type: "string", Description: "The embedded chunk"},
			{Name: "docId", Type: "string", Description: "Owning document", Filterable: true},
			{Name: "corpus", Type: "string", Description: "Corpus tag", Filterable: true},
			{Name: "snapshot", Type: "string", Description: "Ingest generation", Filterable: true},
		}
	}
	return search.IndexEntity{
		Index:        c.Name,
		Name:         c.Title,
		Description:  c.Description,
		Icon:         c.Icon,
		OwnerModule:  c.Owner,
		Fields:       fields,
		TitleField:   "title",
		SnippetField: "text",
		Dim:          c.Dim,
		Examples:     c.Examples,
	}
}

// filterableFields is what the ingester asks the backend to index. Derived
// from the declaration rather than listed twice.
func (c Corpus) filterableFields() []search.IndexField {
	var out []search.IndexField
	for _, f := range c.entity().Fields {
		if f.Filterable {
			out = append(out, f)
		}
	}
	return out
}

// ResetForTest clears the registry.
func ResetForTest() {
	mu.Lock()
	registry = map[string]Corpus{}
	mu.Unlock()
}
