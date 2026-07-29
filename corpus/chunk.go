package corpus

import "strings"

// Chunker splits a document into embeddable pieces.
//
// Two implementations, because the two corpora we have want opposite things:
// markdown documentation has structure worth preserving (a heading tells you
// what the paragraph beneath it is about), while an abstract is one dense
// block that should mostly not be split at all.
type Chunker interface {
	Chunk(d Doc) []Chunk
}

// tokensToChars is the heuristic both chunkers use to turn a token budget into
// a byte budget. Deliberately crude — it is a safety margin against the
// embedding model's real limit, not an attempt to count tokens.
const tokensToChars = 4

// --- markdown ----------------------------------------------------------

// MarkdownChunker splits on `#`/`##`/`###` and packs paragraphs.
//
// Behaviour is carried over verbatim from the docs ingester, deliberately: it
// has been indexing the project docs for a while and its output is what the
// current retrieval quality was tuned against. The one change is that chunks
// now carry an ordinal instead of a byte offset — see ChunkID.
type MarkdownChunker struct{ maxChars int }

func NewMarkdownChunker(maxTokens int) Chunker {
	c := &MarkdownChunker{maxChars: maxTokens * tokensToChars}
	if c.maxChars < 400 {
		c.maxChars = 400
	}
	return c
}

func (c *MarkdownChunker) Chunk(d Doc) []Chunk {
	var (
		out     []Chunk
		heading string
		body    strings.Builder
	)

	emit := func(text, head string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		out = append(out, Chunk{
			ID:      ChunkID(d.ID, len(out)),
			DocID:   d.ID,
			Ordinal: len(out),
			Heading: head,
			Text:    text,
		})
	}

	flush := func() {
		text := strings.TrimSpace(body.String())
		if text == "" {
			return
		}
		if len(text) <= c.maxChars {
			emit(text, heading)
			return
		}
		// Oversize section: pack paragraphs greedily rather than cutting at a
		// character count, so a chunk never ends mid-sentence.
		var bucket strings.Builder
		for _, p := range strings.Split(text, "\n\n") {
			if bucket.Len() > 0 && bucket.Len()+len(p)+2 > c.maxChars {
				emit(bucket.String(), heading)
				bucket.Reset()
			}
			if bucket.Len() > 0 {
				bucket.WriteString("\n\n")
			}
			bucket.WriteString(p)
		}
		emit(bucket.String(), heading)
	}

	for _, line := range strings.Split(d.Body, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		// `####` and deeper do not split: past three levels a heading is
		// usually labelling a paragraph, not a section.
		if strings.HasPrefix(trimmed, "# ") ||
			strings.HasPrefix(trimmed, "## ") ||
			strings.HasPrefix(trimmed, "### ") {
			flush()
			body.Reset()
			heading = strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	flush()
	return out
}

// --- prose -------------------------------------------------------------

// ProseOpts configures ProseChunker.
type ProseOpts struct {
	MaxTokens     int
	OverlapTokens int

	// TitlePrefix prepends the document title to every chunk.
	//
	// Not cosmetic. An abstract that splits in two leaves a second chunk with
	// no mention of what the paper is called or about, which makes it
	// unretrievable by any title term — the classic orphan-chunk failure. The
	// cost is roughly fifteen tokens per chunk.
	TitlePrefix bool
}

// ProseChunker splits continuous prose on sentence boundaries with overlap.
//
// Most inputs produce exactly one chunk: a typical abstract is 150–300 words
// and fits inside the budget whole, which is the right outcome — an abstract
// is a single argument and splitting it costs more than it gains.
type ProseChunker struct {
	maxChars int
	overlap  int
	title    bool
}

func NewProseChunker(o ProseOpts) Chunker {
	c := &ProseChunker{
		maxChars: o.MaxTokens * tokensToChars,
		overlap:  o.OverlapTokens * tokensToChars,
		title:    o.TitlePrefix,
	}
	if c.maxChars < 400 {
		c.maxChars = 400
	}
	if c.overlap >= c.maxChars {
		c.overlap = c.maxChars / 4
	}
	return c
}

func (c *ProseChunker) Chunk(d Doc) []Chunk {
	body := strings.TrimSpace(d.Body)
	if body == "" {
		return nil
	}

	prefix := ""
	if c.title && strings.TrimSpace(d.Title) != "" {
		prefix = strings.TrimSpace(d.Title) + ". "
	}

	var out []Chunk
	emit := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		out = append(out, Chunk{
			ID:      ChunkID(d.ID, len(out)),
			DocID:   d.ID,
			Ordinal: len(out),
			Text:    prefix + text,
		})
	}

	budget := c.maxChars - len(prefix)
	if budget < 200 {
		budget = 200
	}
	if len(body) <= budget {
		emit(body)
		return out
	}

	sentences := splitSentences(body)
	var bucket strings.Builder
	var carry []string
	for _, s := range sentences {
		if bucket.Len() > 0 && bucket.Len()+len(s)+1 > budget {
			emit(bucket.String())
			bucket.Reset()
			// Carry the tail forward. A claim split across a boundary is
			// otherwise retrievable from neither side: the setup lands in one
			// chunk and the finding in the next.
			for _, prev := range carry {
				bucket.WriteString(prev)
				bucket.WriteByte(' ')
			}
			carry = nil
		}
		if bucket.Len() > 0 {
			bucket.WriteByte(' ')
		}
		bucket.WriteString(s)
		carry = tailWithin(append(carry, s), c.overlap)
	}
	emit(bucket.String())
	return out
}

// splitSentences is a boundary heuristic, not a parser. It over-splits on
// abbreviations ("vs.", "et al.") and that is acceptable — an extra boundary
// costs a slightly shorter chunk, while a missed one costs a chunk that
// overruns the budget.
func splitSentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '.' && s[i] != '!' && s[i] != '?' {
			continue
		}
		if i+1 < len(s) && s[i+1] != ' ' && s[i+1] != '\n' {
			continue
		}
		if seg := strings.TrimSpace(s[start : i+1]); seg != "" {
			out = append(out, seg)
		}
		start = i + 1
	}
	if seg := strings.TrimSpace(s[start:]); seg != "" {
		out = append(out, seg)
	}
	return out
}

// tailWithin keeps the last sentences that fit in n characters.
func tailWithin(ss []string, n int) []string {
	if n <= 0 {
		return nil
	}
	total := 0
	for i := len(ss) - 1; i >= 0; i-- {
		total += len(ss[i]) + 1
		if total > n {
			return ss[i+1:]
		}
	}
	return ss
}
