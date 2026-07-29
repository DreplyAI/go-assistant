package corpus

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/redelay/go-modules/search"
)

// Source is where documents come from. Pull-based, one at a time, io.EOF to
// finish.
//
// Pull rather than "give me a []Doc" because the corpora that matter do not
// fit in memory: a docs tree is a few hundred documents, a paper corpus is
// nearly two hundred thousand. A Source can stream a directory walk, a scan of
// gzipped shards, or a database cursor without any of them materialising.
type Source interface {
	Name() string
	Next(ctx context.Context) (Doc, error)
}

// Stats is what an ingest run did.
type Stats struct {
	Docs    int
	Chunks  int
	Skipped int // documents with nothing embeddable
	Pruned  int
}

// Ingester writes a Source into a corpus.
//
// Streaming and bounded: documents are chunked, embedded and upserted in
// batches and then dropped. Nothing beyond one batch is ever resident, which
// is the difference between this and the docs ingester it replaces — that one
// builds every chunk of every file in memory before the first embedding call,
// which is fine for three hundred markdown files and fatal for a corpus.
type Ingester struct {
	Search   *search.Service
	Corpus   string
	Snapshot string
	Chunker  Chunker

	// EmbedBatch is documents per embedding call. The provider's cap, not a
	// preference: OVH rejects bge-m3 batches over 25.
	EmbedBatch int

	// UpsertBatch is points per write. The search layer batches too; this
	// bounds how much is held while accumulating.
	UpsertBatch int

	OnProgress func(Stats)
}

const (
	defaultEmbedBatch  = 25
	defaultUpsertBatch = 256
)

// Ingest streams src into the corpus.
func (i *Ingester) Ingest(ctx context.Context, src Source) (Stats, error) {
	var stats Stats
	if i.Search == nil {
		return stats, errors.New("corpus: no search service")
	}
	c, ok := Get(i.Corpus)
	if !ok {
		return stats, fmt.Errorf("corpus: %q is not registered", i.Corpus)
	}
	chunker := i.Chunker
	if chunker == nil {
		chunker = NewProseChunker(ProseOpts{MaxTokens: 400, OverlapTokens: 40, TitlePrefix: true})
	}
	upsertBatch := i.UpsertBatch
	if upsertBatch <= 0 {
		upsertBatch = defaultUpsertBatch
	}

	if err := i.Search.CreateIndex(ctx, search.Index{Name: c.Name, Dim: c.Dim}); err != nil {
		return stats, fmt.Errorf("corpus: create index: %w", err)
	}
	// Payload indexes before the first write, so the generational prune and
	// any filtered retrieval are lookups rather than full scans. A no-op on
	// backends that cannot index payloads.
	for _, f := range c.filterableFields() {
		if err := i.Search.EnsurePayloadIndex(ctx, c.Name, f.Name, payloadSchema(f.Type)); err != nil {
			return stats, fmt.Errorf("corpus: payload index %q: %w", f.Name, err)
		}
	}

	pending := make([]search.Document, 0, upsertBatch)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := i.Search.Upsert(ctx, c.Name, pending); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		doc, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, fmt.Errorf("corpus: %s: %w", src.Name(), err)
		}

		chunks := chunker.Chunk(doc)
		if len(chunks) == 0 {
			// A document with nothing embeddable is not a corpus entry.
			// Counted rather than silently ignored, because a source that
			// suddenly produces thousands of these is a broken source.
			stats.Skipped++
			continue
		}
		stats.Docs++
		for _, ch := range chunks {
			pending = append(pending, search.Document{
				ID:      ch.ID,
				Text:    ch.Text,
				Payload: payloadFor(c, doc, ch, i.Snapshot),
			})
			stats.Chunks++
			if len(pending) >= upsertBatch {
				if err := flush(); err != nil {
					return stats, err
				}
				if i.OnProgress != nil {
					i.OnProgress(stats)
				}
			}
		}
	}
	if err := flush(); err != nil {
		return stats, err
	}
	if i.OnProgress != nil {
		i.OnProgress(stats)
	}
	return stats, nil
}

// payloadFor builds the point payload: what a citation needs, plus the tags
// the prune filters on.
func payloadFor(c Corpus, d Doc, ch Chunk, snapshot string) map[string]any {
	p := map[string]any{
		"corpus":   c.Name,
		"snapshot": snapshot,
		"docId":    d.ID,
		"ordinal":  ch.Ordinal,
		"kind":     kindOr(d.Kind, c.Kind),
		"title":    d.Title,
		"text":     ch.Text,
	}
	// Citation fields ride on the chunk so a retrieval is one round trip. At
	// ~200 bytes per point this is a few tens of megabytes across a large
	// corpus, against a database read per hit on the chat hot path.
	putIf(p, "url", d.URL)
	putIf(p, "path", d.Path)
	putIf(p, "published", d.Published)
	putIf(p, "container", d.Container)
	putIf(p, "licence", d.Licence)
	putIf(p, "heading", ch.Heading)
	for k, v := range d.Extra {
		if _, taken := p[k]; !taken {
			p[k] = v
		}
	}
	return p
}

// PruneGeneration removes points of this corpus left over from earlier
// snapshots.
//
// Generational rather than by keep-list. search.PruneStale needs every id the
// run produced held in memory and deletes anything not in that set — which
// means two corpora sharing an index destroy each other, and a large corpus
// needs its whole id set resident. Filtering on (corpus, snapshot) is O(stale)
// and cannot touch a neighbour.
func (i *Ingester) PruneGeneration(ctx context.Context) (int, error) {
	if i.Search == nil || i.Snapshot == "" {
		return 0, nil
	}
	var stale []string
	cursor := ""
	for {
		page, err := i.Search.ListPoints(ctx, i.Corpus, search.ListPointsOpts{
			Limit:   256,
			Offset:  cursor,
			Payload: map[string]any{"corpus": i.Corpus},
		})
		if err != nil {
			return 0, err
		}
		for _, h := range page.Points {
			if s, _ := h.Payload["snapshot"].(string); s != i.Snapshot {
				stale = append(stale, h.ID)
			}
		}
		if page.NextOffset == "" {
			break
		}
		cursor = page.NextOffset
	}
	for start := 0; start < len(stale); start += 256 {
		end := min(start+256, len(stale))
		if err := i.Search.Delete(ctx, i.Corpus, stale[start:end]); err != nil {
			return start, err
		}
	}
	return len(stale), nil
}

func payloadSchema(t string) string {
	switch t {
	case "int", "integer":
		return "integer"
	case "float", "number":
		return "float"
	case "bool", "boolean":
		return "bool"
	default:
		return "keyword"
	}
}

func kindOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func putIf(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}
