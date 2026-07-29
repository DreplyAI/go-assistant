package assistant

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dreplyai/go-assistant/corpus"
	"github.com/redelay/go-framework/modules"
	"github.com/redelay/go-modules/search"
	"gopkg.in/yaml.v3"
)

// CLICommands registers the assistant-ingest-docs command. The
// command walks a directory of markdown docs, chunks them by heading
// boundaries, and upserts each chunk into the configured search
// index so the RAG assistant flow can retrieve them at query time.
//
// Lives on the assistant module (not the search module) because the
// *contract* between doc format and assistant flow is assistant-owned
// — chunk size, payload schema, ID scheme all match what
// redelay/assistant-rag-context expects. Other RAG consumers can
// add their own ingest commands tuned to their own assumptions.
func (m *Module) CLICommands() []modules.CLICommand {
	return []modules.CLICommand{
		{
			Name:        "assistant-ingest-docs",
			Description: "Walk a markdown directory, chunk by heading, embed via the configured LLM provider, and upsert into a search index so the RAG assistant flow can ground answers in project docs.",
			Args: []modules.CLIArg{
				{
					Name:        "docs-dir",
					Description: "Root directory to scan for .md files. Traversed recursively.",
					Required:    true,
					Flag:        true,
				},
				{
					Name:        "index",
					Description: "Target search index. Must match the `index` setting on the redelay/assistant-rag-context node (default redelay_docs).",
					Flag:        true,
					Default:     "redelay_docs",
				},
				{
					Name:        "url-base",
					Description: "Optional public URL base. When set, each chunk's payload gets a `url` of \"{url-base}/{relative-path}\" stripped of .md so the UI can render clickable citations.",
					Flag:        true,
					Default:     "",
				},
				{
					Name:        "max-chunk-tokens",
					Description: "Soft upper bound on a single chunk's token count. Chunks longer than this are split on paragraph boundaries.",
					Flag:        true,
					Default:     "500",
				},
				{
					Name:        "batch-size",
					Description: "Embedding batch size for upserts. Provider caps vary — OVH bge-m3 rejects batches over 25, OpenAI ada-002 allows 2048, Ollama is unbounded. Default 25 is the safe cross-provider value.",
					Flag:        true,
					Default:     "25",
				},
				{
					Name:        "dry-run",
					Description: "Walk + chunk + print stats without calling the search service. Useful for verifying the chunker's output before paying for embeddings.",
					Flag:        true,
					Default:     "false",
				},
				{
					Name:        "no-prune",
					Description: "Skip the post-upsert sweep that deletes chunks no longer present in docs-dir. Default behaviour keeps the index in sync with the source tree — turn this on to layer ingests from multiple roots into the same index.",
					Flag:        true,
					Default:     "false",
				},
			},
			Handler: m.runIngestDocs,
		},
	}
}

// runIngestDocs implements the CLI handler. Plain enough to read top
// to bottom; kept in one function so operators can copy it into their
// own module if their docs layout differs.
func (m *Module) runIngestDocs(ctx context.Context, args map[string]string) error {
	docsDir := strings.TrimSpace(args["docs-dir"])
	if docsDir == "" {
		return fmt.Errorf("assistant-ingest-docs: --docs-dir is required")
	}
	if _, err := os.Stat(docsDir); err != nil {
		return fmt.Errorf("assistant-ingest-docs: docs-dir %q: %w", docsDir, err)
	}
	indexName := strOr(args["index"], m.cfg.DocsIndex)
	urlBase := strings.TrimRight(args["url-base"], "/")
	maxTokens := parseIntDefault(args["max-chunk-tokens"], 500)
	batchSize := parseIntDefault(args["batch-size"], 25)
	dryRun := args["dry-run"] == "true"
	noPrune := args["no-prune"] == "true"

	// Snapshot tags this ingest generation, so the prune can drop the previous
	// one by filter instead of by diffing against every id this run produced.
	snapshot := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	svc := search.Current()
	if svc == nil && !dryRun {
		return fmt.Errorf("assistant-ingest-docs: search service unavailable (is SEARCH_BACKEND set?)")
	}

	// Startup already registered the docs corpus, but the CLI is a separate
	// process that never runs it — and --index can point somewhere else — so
	// register defensively. Last write wins, so re-registering the same name
	// is a no-op.
	if _, ok := corpus.Get(indexName); !ok {
		corpus.Register(corpus.Corpus{
			Name:        indexName,
			Title:       "Project documentation",
			Description: "Markdown docs, chunked by heading and indexed for the assistant to cite.",
			Icon:        "book-open",
			Owner:       "assistant",
			Kind:        "doc",
			MinScore:    0.35,
			TopK:        3,
		})
	}

	src, err := newMarkdownSource(docsDir, urlBase)
	if err != nil {
		return fmt.Errorf("assistant-ingest-docs: %w", err)
	}
	chunker := corpus.NewMarkdownChunker(maxTokens)

	// A dry run must not touch the index, and must still be worth running:
	// it walks and chunks everything, so a corpus that is going to produce
	// nothing, or four times too much, is visible before any embedding is
	// paid for.
	if dryRun {
		docs, chunks := 0, 0
		var first *corpus.Chunk
		for {
			d, err := src.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			cs := chunker.Chunk(d)
			if len(cs) == 0 {
				continue
			}
			docs++
			chunks += len(cs)
			if first == nil {
				c := cs[0]
				first = &c
			}
		}
		fmt.Printf("dry run: %d files scanned, %d skipped, %d docs, %d chunks\n",
			src.Total(), src.Skipped(), docs, chunks)
		if first != nil {
			fmt.Printf("\nfirst chunk (%s, ordinal %d):\n%s\n",
				first.DocID, first.Ordinal, truncate(first.Text, 400))
		}
		return nil
	}

	ing := &corpus.Ingester{
		Search:      svc,
		Corpus:      indexName,
		Snapshot:    snapshot,
		Chunker:     chunker,
		EmbedBatch:  batchSize,
		UpsertBatch: batchSize * 8,
		OnProgress: func(s corpus.Stats) {
			fmt.Printf("  %d docs, %d chunks indexed\n", s.Docs, s.Chunks)
		},
	}
	stats, err := ing.Ingest(ctx, src)
	if err != nil {
		return fmt.Errorf("assistant-ingest-docs: %w", err)
	}
	fmt.Printf("indexed %d docs (%d chunks) from %d files; %d skipped\n",
		stats.Docs, stats.Chunks, src.Total(), src.Skipped()+stats.Skipped)

	if noPrune {
		return nil
	}
	// Renamed files, deleted headings and edits that change the chunk count
	// all leave points from the previous generation behind. Upsert alone never
	// removes them.
	pruned, err := ing.PruneGeneration(ctx)
	if err != nil {
		return fmt.Errorf("assistant-ingest-docs: prune: %w", err)
	}
	fmt.Printf("pruned %d points from earlier generations\n", pruned)
	return nil
}

func strOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// docFrontmatter captures the subset of docus frontmatter that affects
// the assistant. Title is used as a fallback for untitled chunks;
// Action is propagated onto every chunk of the page so retrieval
// surfaces the same UI action regardless of which slice ranks.
//
// Everything else (navigation, seo, icon…) is ignored — callers author
// YAML freely and we just pick the fields that matter. `yaml:"-"`
// on Extra keeps the decoder from erroring on unknown keys.
type docFrontmatter struct {
	Title  string         `yaml:"title"`
	Action map[string]any `yaml:"action"`
}

// splitFrontmatter peels a leading `---\n…\n---\n` YAML block off the
// markdown source. Returns the parsed frontmatter and the body with
// the block removed. When no frontmatter is present, returns the
// zero struct and the source unchanged — authors aren't required to
// add a header just to be ingested.
//
// Robust on Windows line endings and missing trailing newlines;
// silently returns the source untouched when the YAML fails to parse
// so one malformed page doesn't tank the whole ingest. (The parse
// error is logged to stderr for visibility.)
func splitFrontmatter(src string) (docFrontmatter, string) {
	// Normalise line endings — docus markdown files are usually LF
	// but editors sometimes save CRLF on Windows.
	s := strings.ReplaceAll(src, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return docFrontmatter{}, src
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		// Try trailing `---` without a newline (end of file).
		if strings.HasSuffix(rest, "\n---") {
			end = len(rest) - 4
		} else {
			return docFrontmatter{}, src
		}
	}
	block := rest[:end]
	body := rest[end+len("\n---\n"):]
	var fm docFrontmatter
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		fmt.Fprintf(os.Stderr, "frontmatter parse error: %v\n", err)
		return docFrontmatter{}, src
	}
	return fm, body
}

// deriveTitleFromPath turns "3.guides/9.ai-assistant.md" into
// "AI Assistant" — a readable label when no `#` heading was found at
// the start of the chunk.
func deriveTitleFromPath(rel string) string {
	base := filepath.Base(rel)
	base = strings.TrimSuffix(base, ".md")
	// Strip docus-style numeric prefixes like "9.ai-assistant" → "ai-assistant".
	if i := strings.Index(base, "."); i >= 0 && i <= 3 {
		prefix := base[:i]
		if allDigits(prefix) {
			base = base[i+1:]
		}
	}
	parts := strings.Split(base, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// docsRelToURL translates an on-disk docs path into the URL slug
// docus serves. Each path segment may carry a numeric ordering prefix
// like `4.reference` or `08.go-cli`; docus strips those when building
// routes. We mirror that exact transform here:
//
//	4.reference/08.go-cli.md  →  reference/go-cli
//	3.guides/9.ai-assistant.md → guides/ai-assistant
//	index.md                   → index
//
// Safe on paths that don't follow the convention — segments without
// a leading `<digits>.` stay verbatim.
func docsRelToURL(rel string) string {
	rel = strings.TrimSuffix(filepath.ToSlash(rel), ".md")
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		if idx := strings.Index(p, "."); idx > 0 && allDigits(p[:idx]) {
			parts[i] = p[idx+1:]
		}
	}
	return strings.Join(parts, "/")
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}
