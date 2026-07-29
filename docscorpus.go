package assistant

import (
	"context"

	"github.com/dreplyai/go-assistant/corpus"
	"go.uber.org/zap"
)

// registerDocsCorpus declares the project-docs corpus and, when there is a
// database, attaches a document store to it.
//
// Called from Startup and from the ingest command, because those are different
// processes and only one of them runs Startup. Registration is last-write-wins,
// so calling it twice is a no-op.
//
// Gated on ASSISTANT_DOCS_CORPUS. It was unconditional for one release, which
// meant a project with no documentation still showed a docs corpus in its
// admin — a corpus nobody had ingested into, inviting a query that could only
// come back empty. The ingest command turns it on for itself, since running
// the command is the clearest possible statement of intent.
//
// The store is what makes the admin's Documents tab work. Without it the corpus
// is still fully searchable — the vectors carry everything a citation needs —
// so its absence degrades one admin view rather than the feature, which is why
// a failure here is logged and not returned.
func (m *Module) registerDocsCorpus(ctx context.Context) {
	if !m.cfg.DocsCorpus {
		return
	}
	corpus.Register(corpus.Corpus{
		Name:        m.cfg.DocsIndex,
		Title:       "Project documentation",
		Description: "Markdown docs, chunked by heading and indexed for the assistant to cite.",
		Icon:        "book-open",
		Owner:       "assistant",
		Kind:        "doc",
		MinScore:    0.35,
		TopK:        3,
		Examples:    []string{"how do I create a module", "what is a flow deployment"},
	})

	if m.db == nil {
		return
	}
	store, err := corpus.NewMongoStore(ctx, m.db, "")
	if err != nil {
		m.logger.Warn("assistant: docs corpus has no document store",
			zap.String("corpus", m.cfg.DocsIndex), zap.Error(err))
		return
	}
	corpus.RegisterDocStore(m.cfg.DocsIndex, store)
}
