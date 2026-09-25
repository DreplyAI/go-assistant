# Changelog

## v0.3.0 (unreleased)

Multi-tenant kernel. One backend serves a separate assistant per
tenant — own flow/prompt, docs corpus/index, handoff-inbox scoping,
optional per-tenant rate limit — selected per request.

- **`Tenant`** model (`assistant_tenants` collection + in-process
  registry): key, flow/deployment ids, docs index, suggested
  questions, `handoffEnabled`/`anonymousAllowed` overrides,
  `rateLimitPerMinute`, `disabled`. Programmatic registration via
  `Module.RegisterTenant` for embedded hosts.
- **`TenantResolver` seam** — any registered module implementing
  `ResolveTenant(r *http.Request) string` is discovered at Configure
  time. Resolution order: `X-Assistant-Site` header → `?site=` query
  → resolver → default (env-configured) tenant.
- Public endpoints (`/assistant/messages`, `/config`, `/handoff`,
  `/chats/me/{id}`) are tenant-aware; chats + handoff records carry a
  `tenant` field (empty = default, so existing data keeps working);
  run meta carries `tenant` + `docs_index` for shared RAG flows.
- Admin: tenant CRUD under `/admin/assistant/tenants`; chats/handoffs
  listings accept an optional `?tenant=` filter (default: all).
- Per-tenant flowexec deployments auto-ensured at Startup / create.
- Simple in-memory per-tenant per-session rate limit (sliding minute
  window; 0 = unlimited).
- Regression tests: two tenants in one process, resolution order,
  cross-tenant scoping, back-compat (v0.2.4 wire shape byte-identical
  for the default tenant), and SSE passthrough of `sources`/`actions`
  /`trace` unchanged.

**Back-compat:** zero config changes needed. No tenants + no resolver
= exactly v0.2.4 behaviour.

## v0.2.4 and earlier

See git history.
