package corpus

import (
	"context"
	"strings"
)

// Rewriting a question into something the corpus can match.
//
// A user asks "does creatine really work?". The corpus holds abstracts, and an
// abstract does not read like that — it reads like "creatine supplementation
// effects on muscular strength and lean body mass during resistance training".
// Embedding the first found the wrong paper at 0.534; embedding the second
// found the right one at 0.752. The gap is register, not relevance.
//
// The same call fixes language. A Polish question retrieves the correct paper
// but scores about 0.026 lower than its English equivalent — harmless until it
// meets a threshold, at which point the same question cites a paper in one
// language and nothing in another. Translating and re-registering are one job:
// put the question into the corpus's language and its register.
//
// This lives here rather than in a caller for one reason that is not tidiness:
// **the eval has to measure the query the app actually sends.** With the prompt
// in an app module, `papersrc eval` would measure raw questions while the coach
// sends rewritten ones, and the recall and floor it reports would describe a
// system nobody runs.
//
// The interface takes a Completer rather than an LLM client so this package
// keeps its dependencies — a corpus is documents and retrieval, not a model.

// Completer runs one prompt. Satisfied by any chat model wrapper; the app
// supplies it, so this package never learns about providers.
type Completer func(ctx context.Context, system, user string) (string, error)

// Rewriter turns a user's question into a retrieval query.
type Rewriter interface {
	Rewrite(ctx context.Context, question string) string
}

// promptRewriter is the default implementation: one model call, defended
// against the ways a model returns something unusable.
type promptRewriter struct {
	complete Completer
	prompt   string
}

// NewRewriter builds the standard rewriter for a corpus.
//
// `describe` says what the corpus holds and in what language — it is
// interpolated into the prompt, so "English sports-science paper abstracts"
// and "German legal opinions" both work without a second implementation.
func NewRewriter(complete Completer, describe string) Rewriter {
	if complete == nil {
		return nil
	}
	return &promptRewriter{complete: complete, prompt: rewritePrompt(describe)}
}

// rewritePrompt is deliberately explicit about NOT answering.
//
// A rewriter that answers is a hypothetical-document retriever, and embedding
// a hypothetical answer embeds the model's priors — which is the thing a
// citable corpus exists to replace. Restating the question in the vocabulary a
// document about it would use keeps the grounding in the corpus.
func rewritePrompt(describe string) string {
	return "You rewrite a user's question into a search query for a database of " + describe +
		". Output English only, whatever language the input is in. Restate the question using the " +
		"terminology such a document would use — name the intervention, the outcome measures and " +
		"the population where the question implies them. Do NOT answer the question, do not add " +
		"findings, do not invent numbers. Keep it under 30 words, one line, no quotes, no preamble. " +
		"Example: \"does creatine really work?\" -> creatine supplementation effects on muscular " +
		"strength, power output and lean body mass during resistance training"
}

// Rewrite returns the rewritten query, or the original question on any failure.
//
// Never returns an error: a question that could not be rewritten still
// retrieves, slightly worse. Failing the retrieval because the rewriter was
// unavailable would turn an optional improvement into a dependency.
func (p *promptRewriter) Rewrite(ctx context.Context, question string) string {
	out, err := p.complete(ctx, p.prompt, question)
	if err != nil {
		return question
	}
	if clean := CleanRewrite(out); clean != "" {
		return clean
	}
	return question
}

// CleanRewrite strips what a model adds around an answer it was asked to give
// bare. Exported because it is the part worth testing, and worth reusing by
// anyone implementing Rewriter differently.
//
// A malformed rewrite is worse than no rewrite: it retrieves confidently
// against the wrong text, and nothing downstream can tell.
func CleanRewrite(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] // a second line is commentary, not query
	}
	s = strings.Trim(s, "\"'` ")
	for _, label := range []string{"query:", "search query:", "rewritten:"} {
		if len(s) >= len(label) && strings.EqualFold(s[:len(label)], label) {
			s = strings.TrimSpace(strings.Trim(s[len(label):], "\"'` "))
		}
	}
	// Far longer than the prompt allows means the model started explaining or
	// answering. The user's own words are the safer query.
	if len(s) > 400 {
		return ""
	}
	return s
}
