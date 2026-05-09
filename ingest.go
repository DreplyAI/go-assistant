package assistant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
	indexName := strings.TrimSpace(args["index"])
	if indexName == "" {
		indexName = "redelay_docs"
	}
	urlBase := strings.TrimSpace(args["url-base"])
	maxTokens := parseIntDefault(args["max-chunk-tokens"], 500)
	batchSize := parseIntDefault(args["batch-size"], 25)
	dryRun := args["dry-run"] == "true"
	noPrune := args["no-prune"] == "true"

	svc := search.Current()
	if svc == nil && !dryRun {
		return fmt.Errorf("assistant-ingest-docs: search service not ready (SEARCH_BACKEND configured? SEARCH_EMBEDDING_PROVIDER credentials set?)")
	}

	// Walk + chunk first so we can report totals before hitting the
	// embedding API — saves operators money when something's wrong
	// with the corpus.
	chunks := make([]search.Document, 0, 256)
	var files, skipped int
	walkErr := filepath.WalkDir(docsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip .git and node_modules aggressively — these never
			// contain docs worth indexing and add significant walk
			// time on large repos.
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "dist" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			skipped++
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", path, err)
			return nil
		}
		files++
		rel, relErr := filepath.Rel(docsDir, path)
		if relErr != nil {
			rel = path
		}
		var url string
		if urlBase != "" {
			// Translate the on-disk path into the URL docus/nuxt-content
			// serves. Files are stored with docus-style numeric ordering
			// prefixes (e.g. `4.reference/08.go-cli.md`) but the web
			// routes strip those, giving `/reference/go-cli`. Keep the
			// hash-based ID independent of this transform so we can
			// change URL conventions without re-embedding.
			url = strings.TrimRight(urlBase, "/") + "/" + docsRelToURL(rel)
		}
		// Strip docus frontmatter before chunking so the YAML block
		// doesn't pollute the embedded text, but capture any `action:`
		// block — stamped onto every chunk of this page so retrieval
		// surfaces the action no matter which slice ranks.
		fm, body := splitFrontmatter(string(raw))
		pageAction := fm.Action
		for _, ch := range chunkMarkdown(body, maxTokens) {
			title := ch.heading
			if title == "" {
				if fm.Title != "" {
					title = fm.Title
				} else {
					title = deriveTitleFromPath(rel)
				}
			}
			id := makeChunkID(rel, ch.offset)
			payload := map[string]any{
				"path":    rel,
				"title":   title,
				"heading": ch.heading,
				"url":     url,
				"text":    ch.body,
			}
			if pageAction != nil {
				payload["action"] = pageAction
			}
			chunks = append(chunks, search.Document{
				ID:      id,
				Text:    ch.body,
				Payload: payload,
			})
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("assistant-ingest-docs: walk: %w", walkErr)
	}
	fmt.Fprintf(os.Stdout, "scanned %d files, skipped %d, produced %d chunks\n",
		files, skipped, len(chunks))
	if len(chunks) == 0 {
		return nil
	}
	if dryRun {
		// Peek at the first chunk so operators can sanity-check the
		// shape before running for real.
		first := chunks[0]
		fmt.Fprintf(os.Stdout, "--- sample chunk ---\nid=%s\npath=%s\ntitle=%s\n\n%s\n---\n",
			first.ID, first.Payload["path"], first.Payload["title"], truncate(first.Text, 400))
		return nil
	}

	// Ensure the collection exists before upserting — the service
	// doesn't auto-create. CreateIndex is a no-op when the index is
	// already present with matching dims, so this is safe to call
	// on every run. Dim comes from the service's configured
	// SEARCH_EMBEDDING_DIM (e.g. 1024 for bge-m3).
	if err := svc.CreateIndex(ctx, search.Index{Name: indexName}); err != nil {
		return fmt.Errorf("assistant-ingest-docs: ensure index %q: %w", indexName, err)
	}

	// Chunked upsert so we don't hold hundreds of embeddings in one
	// request. Provider caps differ: OVH bge-m3 rejects >25, OpenAI
	// ada-002 allows ~2048, Ollama is unbounded. --batch-size=25
	// is the default so the common case (OVH) works without tuning.
	for i := 0; i < len(chunks); i += batchSize {
		j := i + batchSize
		if j > len(chunks) {
			j = len(chunks)
		}
		if err := svc.Upsert(ctx, indexName, chunks[i:j]); err != nil {
			return fmt.Errorf("assistant-ingest-docs: upsert batch %d-%d: %w", i, j, err)
		}
		fmt.Fprintf(os.Stdout, "  upserted %d/%d chunks\n", j, len(chunks))
	}
	fmt.Fprintf(os.Stdout, "ingest complete — index %q has %d chunks from %d files\n",
		indexName, len(chunks), files)

	if noPrune {
		return nil
	}
	// Sweep stale chunks: anything in the index that isn't in this
	// run's chunk set is from an older version of the corpus (renamed
	// file, deleted heading, edits that shifted byte offsets and
	// produced new IDs). Delete them so the index stays a faithful
	// mirror of docs-dir.
	wanted := make(map[string]struct{}, len(chunks))
	for _, c := range chunks {
		wanted[c.ID] = struct{}{}
	}
	stale := make([]string, 0)
	cursor := ""
	for {
		page, err := svc.ListPoints(ctx, indexName, search.ListPointsOpts{Limit: 256, Offset: cursor})
		if err != nil {
			return fmt.Errorf("assistant-ingest-docs: scroll for prune: %w", err)
		}
		for _, p := range page.Points {
			if _, ok := wanted[p.ID]; !ok {
				stale = append(stale, p.ID)
			}
		}
		if page.NextOffset == "" {
			break
		}
		cursor = page.NextOffset
	}
	if len(stale) == 0 {
		fmt.Fprintf(os.Stdout, "prune: index already in sync — no stale chunks\n")
		return nil
	}
	for i := 0; i < len(stale); i += batchSize {
		j := i + batchSize
		if j > len(stale) {
			j = len(stale)
		}
		if err := svc.Delete(ctx, indexName, stale[i:j]); err != nil {
			return fmt.Errorf("assistant-ingest-docs: prune batch %d-%d: %w", i, j, err)
		}
	}
	fmt.Fprintf(os.Stdout, "prune: deleted %d stale chunks (pass --no-prune to skip)\n", len(stale))
	return nil
}

// chunk represents one slice of a markdown document ready for
// embedding. `heading` is the nearest `#`/`##`/`###` ancestor so
// downstream prompts can show "where" the chunk came from.
type chunk struct {
	heading string
	body    string
	offset  int // byte offset in the source file, used for stable IDs
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

// chunkMarkdown splits `src` on `#`/`##`/`###` heading boundaries and
// further splits any section that exceeds `maxTokens` (rough: 4 chars
// ~= 1 token) on blank-line boundaries. Keeps semantic units intact
// when it can — paragraphs aren't fractured mid-sentence.
//
// Simple by design: a markdown-AST parser would be more robust but
// also more surface area to vendor. For the framework's docs this
// heuristic chunker produces clean, well-scoped chunks.
func chunkMarkdown(src string, maxTokens int) []chunk {
	maxChars := maxTokens * 4 // rough token→byte heuristic
	if maxChars < 400 {
		maxChars = 400
	}

	lines := strings.Split(src, "\n")
	var (
		out          []chunk
		currentHead  string
		currentBody  strings.Builder
		sectionStart int
	)
	flush := func(endOffset int) {
		body := strings.TrimSpace(currentBody.String())
		if body == "" {
			return
		}
		if len(body) <= maxChars {
			out = append(out, chunk{heading: currentHead, body: body, offset: sectionStart})
			return
		}
		// Oversized section — split on blank lines, pack paragraphs
		// greedily up to maxChars.
		parts := strings.Split(body, "\n\n")
		var bucket strings.Builder
		off := sectionStart
		for _, p := range parts {
			if bucket.Len() > 0 && bucket.Len()+len(p)+2 > maxChars {
				out = append(out, chunk{heading: currentHead, body: strings.TrimSpace(bucket.String()), offset: off})
				off = endOffset
				bucket.Reset()
			}
			if bucket.Len() > 0 {
				bucket.WriteString("\n\n")
			}
			bucket.WriteString(p)
		}
		if bucket.Len() > 0 {
			out = append(out, chunk{heading: currentHead, body: strings.TrimSpace(bucket.String()), offset: off})
		}
	}

	offset := 0
	for _, line := range lines {
		trim := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trim, "# ") || strings.HasPrefix(trim, "## ") || strings.HasPrefix(trim, "### ") {
			flush(offset)
			currentBody.Reset()
			sectionStart = offset
			currentHead = strings.TrimSpace(strings.TrimLeft(trim, "#"))
		}
		currentBody.WriteString(line)
		currentBody.WriteString("\n")
		offset += len(line) + 1
	}
	flush(offset)
	return out
}

// makeChunkID returns a stable content-address for a chunk. Using
// sha256 over (path, byte-offset) means re-ingesting the same file
// overwrites the old chunks in place instead of accumulating
// duplicates — Qdrant's upsert deduplicates on ID.
func makeChunkID(path string, offset int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s:%d", path, offset)
	return hex.EncodeToString(h.Sum(nil))[:32]
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
