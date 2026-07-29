// Package corpus turns a body of documents into something an assistant can
// cite: chunked, embedded, retrieved with a relevance floor, and rendered as
// citations that point back at the source.
//
// It is deliberately generic. The project docs the assistant already answers
// from, a set of scientific papers, a knowledge base, a product catalogue —
// each is a Source feeding the same Ingester, indexed the same way, retrieved
// through the same floor, and rendered as the same card. Nothing here knows
// what a paper is.
//
// # Layout
//
//	doc.go       the shapes: Doc, Chunk, Citation, Provenance
//	ids.go       chunk identity
//	chunk.go     Chunker and its implementations
//	source.go    where documents come from
//	ingest.go    the streaming, batched, resumable write path
//	registry.go  Corpus — a named, configured, registered corpus
//	retrieve.go  the read path, including the relevance floor
//	docstore.go  optional document-level storage, for the admin browser
//
// # One rule about imports
//
// corpus must never import the parent `assistant` package — only the reverse.
// The two are in one repo because they share a wire contract, not because they
// are one thing, and keeping the dependency one-way means extracting this
// later is a `git mv` rather than an untangling.
package corpus

import "time"

// Doc is one indexable document, before chunking.
//
// The distinction that matters: a Doc has citable identity, a Chunk does not.
// Everything a citation needs to render — title, link, journal, year, licence
// — lives here and is inherited by every chunk, so retrieval can return a
// chunk and still produce a complete card. The alternative, storing citation
// fields per chunk, is what today's docs ingester does, and it is why a chunk
// there can be retrieved without anyone being able to say what document it
// came from.
type Doc struct {
	// ID is stable and corpus-scoped: an OpenAlex work id, a docs path.
	// Re-ingesting the same document must produce the same ID, or the index
	// accumulates duplicates instead of updating in place.
	ID string

	// Kind drives how the UI renders the card — "paper", "doc", "page".
	Kind string

	Title string

	// Body is what gets chunked and embedded. For a paper this is the
	// abstract; for a docs page, the markdown.
	Body string

	// URL is the canonical external link; Path an in-app route. A document
	// may have either, both, or neither — a card with no link is still a
	// legitimate citation, it just is not clickable.
	URL  string
	Path string

	// Published is a year or an ISO date, rendered as-is. A string rather
	// than a time.Time because sources disagree about precision and
	// pretending otherwise means inventing a month for "2017".
	Published string

	// Container is the journal, site or collection the document sits in.
	Container string

	// Licence is recorded even when nothing reads it, and is "unknown" rather
	// than empty when the source does not say. What we do not know has to
	// stay answerable by query.
	Licence string

	// Extra carries corpus-specific fields onto the chunk payload and the
	// citation card verbatim — citation counts, difficulty, muscle groups.
	// The generic layer never introspects it.
	Extra map[string]any

	Prov Provenance
}

// Provenance says where a document came from and under what terms.
//
// Snapshot is the load-bearing field: it identifies the ingest generation, and
// it is what lets a re-ingest delete the previous generation's points without
// needing to know which ids they were, and without touching a second corpus
// sharing the index.
type Provenance struct {
	Source      string // "openalex", "docs"
	SourceID    string // upstream identifier, if different from Doc.ID
	Snapshot    string // generation tag: a date, a version, a commit
	Licence     string
	RetrievedAt time.Time
}

// Chunk is an embeddable slice of a Doc.
type Chunk struct {
	ID    string
	DocID string

	// Ordinal is the chunk's position within its document, 0-based. It is
	// also the ID input — see ids.go for why that is not a byte offset.
	Ordinal int

	Heading string
	Text    string
	Payload map[string]any
}

// Citation is a retrieved chunk, resolved back to something quotable.
type Citation struct {
	ID      string  `json:"id"`
	DocID   string  `json:"docId"`
	Kind    string  `json:"kind"`
	Score   float32 `json:"score"`
	Title   string  `json:"title"`
	URL     string  `json:"url,omitempty"`
	Path    string  `json:"path,omitempty"`
	Snippet string  `json:"snippet,omitempty"`

	Published string `json:"published,omitempty"`
	Container string `json:"container,omitempty"`
	Licence   string `json:"licence,omitempty"`

	// Extra is flattened onto the card rather than nested, so a consumer that
	// knows about `citedBy` can read it without knowing about corpus.
	Extra map[string]any `json:"-"`
}
