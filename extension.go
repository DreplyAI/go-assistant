// extension.go declares the two interfaces that downstream
// "knowledge-pack" modules implement to extend this kernel.
//
// Why a kernel/extension split: the chat HTTP plumbing, SSE pump,
// flow-version resolution, capability detection, persistence and
// admin chat browser are project-agnostic — they live here and
// every consumer (redelay, FlowDSL, future SaaS customers) gets
// them for free. The bits that vary per project — which docs
// corpus the RAG flows ground against (`redelay_docs` vs
// `flowdsl_docs` vs `<customer>_docs`) and which RAG-flavor flow
// templates Studio offers in its picker — live in a sibling
// "knowledge-pack" module that the host binary blank-imports
// alongside this kernel.
//
// Both interfaces are discovered by the kernel's Configure(registry)
// hook by walking registry.All() and type-asserting. Returning a
// nil/empty slice is fine — extensions that only contribute one of
// the two surfaces simply leave the other method unimplemented (or
// returning nil).
package assistant

import (
	"github.com/redelay/go-flowdsl/flowexec"
	"github.com/redelay/go-modules/search"
)

// IndexEntityProvider lets a downstream module declare the search
// index(es) the assistant's RAG flows will ground against. The
// kernel's IndexEntities() method aggregates entries from every
// provider in the registry; search-admin discovers the kernel via
// the existing IndexEntities() contract on *Module, so projects do
// not need any extra wiring on the search side.
//
// Typical implementation in a project knowledge-pack:
//
//	func (m *Module) AssistantIndexEntities() []search.IndexEntity {
//	    return []search.IndexEntity{{
//	        Index: "flowdsl_docs", Name: "FlowDSL Docs",
//	        OwnerModule: "assistant-flowdsl", ...
//	    }}
//	}
type IndexEntityProvider interface {
	AssistantIndexEntities() []search.IndexEntity
}

// TemplateProvider lets a downstream module ship extra flow
// templates surfaced in the Studio "Create flow" picker AND
// addressable by short name in the reset-to-template endpoint.
//
// Implementations return TWO slices that must agree on the short
// names:
//
//   - AssistantFlowTemplates returns rich flowexec.FlowTemplate
//     records (id, name, description, tags, parsed Document) for
//     Studio's picker.
//   - AssistantEmbeddedTemplates returns short-name → raw JSON
//     bytes for the reseed endpoint, which re-parses the document
//     fresh so the published version always matches the embedded
//     source even if Studio has been editing the live copy.
//
// Short names are project-scoped — a redelay extension might use
// "rag", "rag-clean"; a FlowDSL extension might use "flowdsl-rag",
// "license-aware-rag". The kernel never disambiguates: the first
// extension returning a match wins, so projects should not collide.
type TemplateProvider interface {
	AssistantFlowTemplates() []flowexec.FlowTemplate
	AssistantEmbeddedTemplates() map[string][]byte
}
