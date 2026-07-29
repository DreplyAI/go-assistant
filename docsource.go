package assistant

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dreplyai/go-assistant/corpus"
)

// markdownSource walks a docs tree and yields one corpus.Doc per file.
//
// The walk happens up front and the file contents are read lazily, one at a
// time. That split matters: the walk is cheap and its result is the run's
// scope — knowing "412 files" before the first embedding call is what makes a
// dry run meaningful — while holding 412 file bodies in memory to learn the
// same number is not.
type markdownSource struct {
	root    string
	urlBase string

	files []string
	next  int
	err   error

	// skipped counts files that were listed but could not be read. Reported
	// rather than silently dropped: one unreadable file is a permissions
	// mistake, two hundred is a broken mount.
	skipped int
}

func newMarkdownSource(root, urlBase string) (*markdownSource, error) {
	s := &markdownSource{root: root, urlBase: strings.TrimRight(urlBase, "/")}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored and build directories hold megabytes of markdown that
			// is not this project's documentation.
			switch d.Name() {
			case ".git", "node_modules", "dist", ".output", ".nuxt":
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			s.files = append(s.files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	return s, nil
}

func (s *markdownSource) Name() string { return "docs:" + s.root }

// Total is the file count, known before any content is read.
func (s *markdownSource) Total() int { return len(s.files) }

// Skipped is how many listed files could not be read.
func (s *markdownSource) Skipped() int { return s.skipped }

func (s *markdownSource) Next(ctx context.Context) (corpus.Doc, error) {
	for {
		if err := ctx.Err(); err != nil {
			return corpus.Doc{}, err
		}
		if s.next >= len(s.files) {
			return corpus.Doc{}, io.EOF
		}
		path := s.files[s.next]
		s.next++

		raw, err := os.ReadFile(path)
		if err != nil {
			// One bad file does not end the run — the other four hundred are
			// still worth indexing.
			fmt.Fprintf(os.Stderr, "  ! skipping %s: %v\n", path, err)
			s.skipped++
			continue
		}
		rel, relErr := filepath.Rel(s.root, path)
		if relErr != nil {
			rel = filepath.Base(path)
		}
		rel = filepath.ToSlash(rel)

		fm, body := splitFrontmatter(string(raw))
		title := strings.TrimSpace(fm.Title)
		if title == "" {
			title = deriveTitleFromPath(rel)
		}

		doc := corpus.Doc{
			// Id is the relative path: stable across runs, and it is what
			// makes a re-ingest update in place rather than duplicate.
			ID:      rel,
			Kind:    "doc",
			Title:   title,
			Body:    body,
			Path:    "/" + docsRelToURL(rel),
			Licence: "internal",
			Prov:    corpus.Provenance{Source: "docs", SourceID: rel},
		}
		if s.urlBase != "" {
			doc.URL = s.urlBase + "/" + docsRelToURL(rel)
		}
		if fm.Action != nil {
			// The page's action rides on every chunk of it, so retrieval
			// surfaces the button no matter which slice ranked.
			doc.Extra = map[string]any{"action": fm.Action}
		}
		return doc, nil
	}
}
