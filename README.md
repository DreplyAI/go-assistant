# go-assistant — DreplyAI assistant kernel

Reusable AI-assistant kernel for Redelay-framework apps. Provides chat
HTTP/SSE plumbing, chat + handoff persistence, capability detection,
flow-version resolution, and the admin chat browser. Project-specific
bits — which docs index the RAG flows ground against, which RAG-flavor
flow templates Studio offers — come from sibling **knowledge-pack**
modules that implement two interfaces declared in `extension.go`:

- `IndexEntityProvider` — declare search index entities for RAG flows.
- `TemplateProvider` — ship project-specific flow templates surfaced in
  Studio's "Create flow" picker and addressable by short name in the
  reseed endpoint.

The kernel discovers them at `Configure(registry)` time. Hosts blank-
import the kernel + their chosen knowledge pack(s) side by side.

## Reference consumers

- `github.com/redelay/backend/modules/assistant-redelay` — redelay's
  knowledge pack: `redelay_docs` index + the `rag`, `rag-clean`,
  `rag-stream`, `rag-rewrite`, `qa-only` flow templates.
- `github.com/flowdsl/api/modules/assistant-flowdsl` — FlowDSL's
  knowledge pack: `flowdsl_docs` index + FlowDSL-flavor RAG flows.

## Endpoints (mounted automatically)

- `POST /assistant/messages` — chat SSE stream
- `GET  /assistant/chats/me/{sessionID}` — fetch chat history
- `POST /assistant/handoff` — request human handoff
- `GET  /assistant/config` — runtime capabilities

Admin surface (`./admin/`, mount only on admin-api):

- `GET /admin/chats`, `GET /admin/chats/{id}` — chat browsing
- `POST /admin/assistant/reset` — reseed flow from named template

## Env vars

| Var | Default | Purpose |
|-----|---------|---------|
| `ASSISTANT_DEPLOYMENT_ID` | `assistant` | flowexec deployment id |
| `ASSISTANT_FLOW_ID` | `assistant` | flowexec flow id |
| `ASSISTANT_INPUT_KEY` | `messages` | input packet key |
| `ASSISTANT_OUTPUT_NODE` | `end,rejected` | terminal node ids the SSE pump mines |
| `ASSISTANT_PROVIDER` | _(none)_ | override chat node providerID at boot |
| `ASSISTANT_MODEL` | _(none)_ | override chat node model at boot |
| `ASSISTANT_CHAT_TTL_DAYS` | `30` | chat history TTL |
| `ASSISTANT_FLOW_SKIP_AUTO_UPGRADE` | `false` | freeze the live flow at its current version |

## Development

```bash
go build ./...
go test ./...
```

Currently uses `replace` directives in `go.mod` pointing at
`../redelay/...` siblings for in-flight development. Will pin to tagged
releases of `github.com/redelay/*` once those repos cut tags.
