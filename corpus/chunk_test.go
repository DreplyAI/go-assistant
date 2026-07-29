package corpus

import (
	"strings"
	"testing"
)

// The bug: the docs ingester keyed chunk ids on (path, byte offset) and its
// oversize-section branch gave every sub-chunk after the first the SAME
// end-of-section offset. Three or more sub-chunks in one section therefore
// collided, and because Qdrant upserts by id, all but the last were silently
// overwritten. Sections disappeared from the index with no error anywhere.
func TestMarkdownChunkIDsAreUniqueAcrossOversizeSplits(t *testing.T) {
	// One section, far over budget, so it must split into several sub-chunks
	// — the exact case that used to collide.
	var b strings.Builder
	b.WriteString("## One long section\n\n")
	for i := range 40 {
		b.WriteString(strings.Repeat("word ", 60))
		b.WriteString("\n\n")
		_ = i
	}

	chunks := NewMarkdownChunker(100).Chunk(Doc{ID: "guides/long.md", Body: b.String()})
	if len(chunks) < 3 {
		t.Fatalf("need >=3 sub-chunks to exercise the collision, got %d", len(chunks))
	}

	seen := map[string]int{}
	for _, c := range chunks {
		seen[c.ID]++
	}
	if len(seen) != len(chunks) {
		t.Fatalf("%d chunks produced only %d distinct ids — collisions would be silently overwritten in the index", len(chunks), len(seen))
	}
	for i, c := range chunks {
		if c.Ordinal != i {
			t.Errorf("chunk %d has ordinal %d", i, c.Ordinal)
		}
		if c.ID != ChunkID("guides/long.md", i) {
			t.Errorf("chunk %d id is not derived from (docID, ordinal)", i)
		}
	}
}

// Inserting text at the top of a document must not renumber the chunks below
// it. Keying on byte offsets did exactly that, so an edit to the first
// paragraph re-embedded the whole file.
func TestMarkdownIDsAreStableUnderEditsAbove(t *testing.T) {
	body := "# A\n\nalpha\n\n## B\n\nbeta\n\n## C\n\ngamma\n"
	before := NewMarkdownChunker(100).Chunk(Doc{ID: "d.md", Body: body})
	after := NewMarkdownChunker(100).Chunk(Doc{ID: "d.md", Body: "# A\n\nalpha and more words here\n\n## B\n\nbeta\n\n## C\n\ngamma\n"})

	if len(before) != len(after) {
		t.Fatalf("chunk count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID {
			t.Errorf("chunk %d id changed after an edit above it", i)
		}
	}
}

func TestMarkdownSplitsOnHeadings(t *testing.T) {
	chunks := NewMarkdownChunker(1000).Chunk(Doc{
		ID:   "d.md",
		Body: "# Title\n\nintro\n\n## Second\n\nbody\n\n#### Deep\n\nstill second\n",
	})
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks (#### must not split), got %d", len(chunks))
	}
	if chunks[1].Heading != "Second" {
		t.Errorf("heading = %q, want Second", chunks[1].Heading)
	}
	if !strings.Contains(chunks[1].Text, "still second") {
		t.Error("#### should stay inside its parent section")
	}
}

// An abstract is one argument. Splitting it costs more than it gains, so the
// common case must produce exactly one chunk.
func TestProseKeepsAShortDocumentWhole(t *testing.T) {
	chunks := NewProseChunker(ProseOpts{MaxTokens: 400, TitlePrefix: true}).Chunk(Doc{
		ID:    "W1",
		Title: "Creatine supplementation and resistance training",
		Body:  "Creatine increased lean mass in trained adults. Effects were larger in younger participants.",
	})
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if !strings.HasPrefix(chunks[0].Text, "Creatine supplementation and resistance training.") {
		t.Errorf("title prefix missing: %q", chunks[0].Text)
	}
}

// A second chunk with no title is unretrievable by any title term — the
// orphan-chunk failure the prefix exists to prevent.
func TestProseTitlePrefixesEveryChunk(t *testing.T) {
	long := strings.Repeat("This sentence describes a measured training outcome in detail. ", 60)
	chunks := NewProseChunker(ProseOpts{MaxTokens: 120, OverlapTokens: 20, TitlePrefix: true}).Chunk(Doc{
		ID: "W2", Title: "Protein intake and hypertrophy", Body: long,
	})
	if len(chunks) < 2 {
		t.Fatalf("need >=2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if !strings.HasPrefix(c.Text, "Protein intake and hypertrophy.") {
			t.Errorf("chunk %d has no title prefix", i)
		}
		if c.ID != ChunkID("W2", i) {
			t.Errorf("chunk %d id is not (docID, ordinal)", i)
		}
	}
}

func TestProseSplitsOnSentenceBoundaries(t *testing.T) {
	long := strings.Repeat("Alpha beta gamma delta epsilon zeta eta theta. ", 40)
	chunks := NewProseChunker(ProseOpts{MaxTokens: 100}).Chunk(Doc{ID: "W3", Body: long})
	if len(chunks) < 2 {
		t.Fatalf("want a split, got %d chunks", len(chunks))
	}
	for i, c := range chunks[:len(chunks)-1] {
		// A chunk that ends mid-sentence means the boundary logic was
		// bypassed by the character budget.
		if !strings.HasSuffix(strings.TrimSpace(c.Text), ".") {
			t.Errorf("chunk %d ends mid-sentence: %q", i, tail(c.Text, 40))
		}
	}
}

func TestProseIgnoresEmptyBody(t *testing.T) {
	if got := NewProseChunker(ProseOpts{MaxTokens: 400}).Chunk(Doc{ID: "W4", Title: "T"}); len(got) != 0 {
		t.Errorf("empty body should produce no chunks, got %d", len(got))
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
