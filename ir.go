package assistant

import _ "embed"

//go:embed module.yaml
var moduleYAML []byte

//go:embed flows/default.flowdsl.json
var defaultFlowJSON []byte

// Production flow template — richer than the default. Loaded by
// Templates() so the Studio's "Create flow" picker offers it as a
// starting point. Not used to seed the live "assistant" flow on
// startup (that's defaultFlowJSON's job).
//
//go:embed flows/production.flowdsl.json
var productionFlowJSON []byte

// Handoff notification — a downstream flow that subscribes to the
// assistant.handoff_requested event (published by the handoff
// service) and emits an email.send. Ships as a template so projects
// can customise the recipient / body in Studio.
//
//go:embed flows/handoff-notification.flowdsl.json
var handoffNotificationFlowJSON []byte

// Minimal flow — start → llm-chat → end, no safety guards, no
// handoff. Useful for local development and fast model-comparison
// experiments. Not recommended for user-facing deployments.
//
//go:embed flows/minimal.flowdsl.json
var minimalFlowJSON []byte

// Echo (dev-only) — zero-LLM flow. start → core/template-render → end.
// Echoes the user's message back via Go text/template. Useful for
// CI smoke tests, UI development, and customer demos where cost-per-
// interaction must be zero.
//
//go:embed flows/echo.flowdsl.json
var echoFlowJSON []byte

// Default + streaming — baseline guarded flow with llm-chat
// stream=true. Lower time-to-first-token; guard-out only sees the
// complete draft after streaming ends.
//
//go:embed flows/default-stream.flowdsl.json
var defaultStreamFlowJSON []byte

// Multi-guard — chains two Qwen3Guard classifiers on each side of
// the LLM. First passes on Allow/Flag; second is onError=block
// (fails closed). Demonstrates defense-in-depth for regulated or
// high-liability deployments where single-classifier recall is
// insufficient.
//
//go:embed flows/multi-guard.flowdsl.json
var multiGuardFlowJSON []byte

