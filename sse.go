package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/dreplyai/go-assistant/chats"
	"github.com/redelay/go-flowdsl/flowexec"
	flowsink "github.com/redelay/go-flowdsl/flowexec/sink"
	flowstore "github.com/redelay/go-flowdsl/flowexec/store"
	"github.com/redelay/go-framework/modules/auth"
	"github.com/redelay/go-framework/server/httputil"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

// handleChat is the single chat entrypoint. It:
//
//  1. Decodes {messages, meta}.
//  2. Resolves the currently-published version of the configured flow.
//  3. Launches a run with {messages: ...} as input.
//  4. Subscribes to live events and translates them into SSE chat frames
//     until run.completed, run.failed, or the client disconnects.
func (m *Module) handleChat(w http.ResponseWriter, r *http.Request) {
	if m.flowexec == nil {
		httputil.Error(w, http.StatusInternalServerError, "assistant: flowexec not wired")
		return
	}

	var body ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(body.Messages) == 0 {
		httputil.Error(w, http.StatusBadRequest, "messages is required")
		return
	}

	// Anonymous gate — authentication is optional, but when the
	// admin disabled anonymous chat we require a JWT. Same rule as
	// the handoff endpoint for consistency.
	claims, authed := auth.ClaimsFromContext(r.Context())
	if !authed && !m.cfg.AnonymousAllowed {
		httputil.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	// Session id: client-supplied, else generated. Always echoed in
	// the response header so the frontend can persist it to
	// localStorage on first message.
	sessionID := body.SessionID
	if sessionID == "" {
		sessionID = "sess-" + uuid.NewString()
	}
	w.Header().Set("X-Assistant-Session-Id", sessionID)

	// Resolve auth subject for chat attribution. Empty OID when anon.
	// For anonymous callers derive an "anon-<client-ip>" handle so
	// admin dashboards show a readable identifier instead of the
	// all-zeroes ObjectID; same value flows into the run meta below
	// for ledger correlation.
	var userID primitive.ObjectID
	var anonID string
	if authed {
		if oid, err := primitive.ObjectIDFromHex(claims.UserID); err == nil {
			userID = oid
		}
	} else {
		anonID = anonIDFromRequest(r)
	}

	ctx := r.Context()

	// 1. Resolve the active deployment variant → (flowID, version).
	// Delegates to the framework's flowstore.ResolveDeployment via
	// the module's thin shim. Sticky bucket keyed by session_id so
	// the same session always lands on the same variant across
	// restarts and containers.
	resolved, err := m.resolveVariant(ctx, map[string]any{
		"session_id":   sessionID,
		"user_id":      userIDHex(userID),
		"assistant_id": m.cfg.DeploymentID,
	})
	if err != nil {
		if errors.Is(err, flowstore.ErrNotFound) || errors.Is(err, flowstore.ErrNoRoute) {
			httputil.Error(w, http.StatusConflict, "assistant deployment has no published variant")
			return
		}
		m.logger.Error("assistant: resolve variant", zap.Error(err))
		httputil.Error(w, http.StatusInternalServerError, "could not resolve assistant deployment")
		return
	}
	flowDoc := resolved.Version
	variantLabel := resolved.Label

	// Persist the chat shell + last user message. Idempotent on
	// session id so reconnecting clients append to the same row.
	if m.chats != nil {
		_, err := m.chats.CreateIfAbsent(ctx, sessionID, resolved.FlowID, flowDoc.VersionHash,
			variantLabel, userID, anonID, nil)
		if err != nil {
			m.logger.Warn("assistant: CreateIfAbsent", zap.Error(err))
		} else {
			lastUser := lastUserMessage(body.Messages)
			if lastUser != nil {
				if _, err := m.chats.AppendMessage(ctx, sessionID, chats.Message{
					Role:    lastUser.Role,
					Content: lastUser.Content,
				}, ""); err != nil {
					m.logger.Warn("assistant: persist user message", zap.Error(err))
				}
			}
		}
	}

	// 2. Build run input. Session id + user + chat meta travel in
	// `meta` so LLM-cost ledger attribution + A/B sticky bucketing
	// both work end-to-end.
	msgs := make([]any, len(body.Messages))
	for i, msg := range body.Messages {
		msgs[i] = map[string]any{"role": msg.Role, "content": msg.Content}
	}
	runInput := map[string]any{
		m.cfg.InputKey: msgs,
	}
	meta := map[string]any{"session_id": sessionID}
	if !userID.IsZero() {
		meta["user_id"] = userID.Hex()
	} else if anonID != "" {
		meta["anon_id"] = anonID
	}
	if body.Meta != nil {
		for k, v := range body.Meta {
			if _, exists := meta[k]; !exists {
				meta[k] = v
			}
		}
	}
	runInput["meta"] = meta

	// 3. Launch async + subscribe.
	runID, errCh, err := m.flowexec.Executor().RunAsync(ctx, flowexec.RunInput{
		Workflow:     flowDoc.Document,
		VersionID:    flowDoc.ID,
		VersionHash:  flowDoc.VersionHash,
		VariantLabel: variantLabel,
		Input:        runInput,
	})
	if err != nil {
		m.logger.Error("assistant: launch run", zap.Error(err))
		httputil.Error(w, http.StatusBadGateway, "could not launch assistant flow")
		return
	}
	events, cancel := m.flowexec.Subscribe(runID, 256)
	defer cancel()

	// Drain errCh in background; the primary failure signal we rely on for
	// the client is run.failed from the event stream.
	go func() {
		if err := <-errCh; err != nil {
			m.logger.Warn("assistant: run error (also surfaced via SSE)",
				zap.String("runID", runID), zap.Error(err))
		}
	}()

	// 4. Stream SSE.
	flusher, ok := w.(http.Flusher)
	if !ok {
		httputil.Error(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprintf(w, ": connected runId=%s\n\n", runID)
	flusher.Flush()

	m.pumpChatFrames(ctx, w, flusher, runID, sessionID, events)
}

// lastUserMessage returns the last message whose role is "user" —
// used to persist only the new turn rather than re-writing the whole
// history on every request.
func lastUserMessage(msgs []ChatMessage) *ChatMessage {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			m := msgs[i]
			return &m
		}
	}
	return nil
}

// pumpChatFrames translates FlowEvents into SSE chat frames until a terminal
// event is seen or the client disconnects. Wire format mirrors the flowdsl.com
// assistant so the same useChat.ts composable works unchanged.
func (m *Module) pumpChatFrames(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	runID string,
	sessionID string,
	events <-chan flowsink.FlowEvent,
) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	write := func(frame ChatFrame) {
		b, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()

		case ev, open := <-events:
			if !open {
				// Broadcaster closed the channel — end of stream.
				write(ChatFrame{Done: true, RunID: runID})
				return
			}
			switch ev.Kind {

			case flowsink.KindNodeDone:
				// Emit {content} when one of the configured output
				// (terminal) nodes completes. Any other node forwards as
				// a trace frame. The set is configurable so flows can
				// have multiple terminals — success (`end`) and guard
				// rejection (`rejected`), both carrying `content`.
				if !m.isOutputNode(ev.NodeID) {
					// Non-terminal node: forward as trace for debugging UIs.
					write(ChatFrame{Trace: map[string]any{
						"nodeId":     ev.NodeID,
						"kind":       string(ev.Kind),
						"durationMs": ev.DurationMs,
					}, RunID: runID})
					continue
				}
				if content := extractContent(ev.Payload); content != "" {
					// Citations: when a RAG-style flow ran, the packet
					// carries a `sources` array stamped by
					// redelay/assistant-rag-context. Forward it
					// alongside the content frame so the UI can render
					// clickable [N] pills that resolve to the source
					// url/title/path. Missing or wrong-shaped sources
					// simply surface as nil — the UI handles both.
					//
					// Actions: same payload may carry an `actions`
					// array (UI-triggerable buttons — handoff, open
					// product, add to cart…). Forward as-is; the
					// widget's action registry decides how to render
					// and what happens on click.
					write(ChatFrame{
						Content: content,
						Sources: extractSources(ev.Payload),
						Actions: extractActions(ev.Payload),
						RunID:   runID,
					})
					// Persist the assistant turn. Best-effort —
					// the SSE reply to the client is the primary
					// contract; durability is a secondary concern.
					if m.chats != nil && sessionID != "" {
						if _, err := m.chats.AppendMessage(ctx, sessionID, chats.Message{
							Role:    "assistant",
							Content: content,
						}, runID); err != nil {
							m.logger.Warn("assistant: persist reply", zap.Error(err))
						}
					}
				}

			case flowsink.KindNodeFailed:
				errMsg := ev.ErrorMsg
				if errMsg == "" {
					errMsg = "node failed"
				}
				write(ChatFrame{Error: fmt.Sprintf("%s: %s", ev.NodeID, errMsg), RunID: runID})

			case flowsink.KindRunCompleted:
				write(ChatFrame{Done: true, RunID: runID})
				return

			case flowsink.KindRunFailed:
				errMsg := ev.ErrorMsg
				if errMsg == "" {
					errMsg = "run failed"
				}
				write(ChatFrame{Error: errMsg, RunID: runID})
				return

			default:
				// run.started, node.started, packet.emit, edge.deliver — trace only.
				write(ChatFrame{Trace: map[string]any{
					"nodeId": ev.NodeID,
					"kind":   string(ev.Kind),
				}, RunID: runID})
			}
		}
	}
}

// extractContent pulls the user-facing reply text out of a node.done payload.
// Accepts a few conventional shapes emitted by llm-chat-style handlers:
//
//	{output: {message: {content: "..."}}}
//	{output: {content: "..."}}
//	{output: "..."}
//	{message: {content: "..."}}
//	{content: "..."}
//
// Node handler outputs may contain typed Go structs (e.g. llm.Message) rather
// than plain map[string]any. We JSON-marshal the payload once to normalise
// everything to maps before extracting.
func extractContent(p map[string]any) string {
	if p == nil {
		return ""
	}
	// Normalise: JSON round-trip turns typed structs into map[string]any.
	norm := normalisePayload(p)

	if s, ok := norm["content"].(string); ok && s != "" {
		return s
	}
	if msg, ok := norm["message"].(map[string]any); ok {
		if s, ok := msg["content"].(string); ok {
			return s
		}
	}
	if out, ok := norm["output"]; ok {
		switch v := out.(type) {
		case string:
			return v
		case map[string]any:
			if s, ok := v["content"].(string); ok && s != "" {
				return s
			}
			if msg, ok := v["message"].(map[string]any); ok {
				if s, ok := msg["content"].(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

// extractSources pulls the RAG citation list out of a terminal-node
// payload. Looks in the same places extractContent does
// (root → output → output.output) because flowexec sometimes wraps
// step.Output inside an `output` key depending on delivery mode.
// Returns nil (not empty slice) when sources are absent so the JSON
// frame omits the field via `omitempty`.
func extractSources(p map[string]any) []map[string]any {
	return extractMapSlice(p, "sources")
}

// extractActions pulls the UI-action list out of a terminal-node
// payload, then casts each entry into the typed Action wire form.
// Invalid entries (missing type, wrong shape) are dropped silently —
// a bad authoring move shouldn't break chat for every user.
//
// Returns nil when none present so `omitempty` drops the field.
func extractActions(p map[string]any) []Action {
	raw := extractMapSlice(p, "actions")
	if len(raw) == 0 {
		return nil
	}
	out := make([]Action, 0, len(raw))
	for _, m := range raw {
		a := Action{}
		a.ID, _ = m["id"].(string)
		a.Type, _ = m["type"].(string)
		if a.Type == "" {
			continue // type is the dispatch key — useless without it
		}
		a.Label, _ = m["label"].(string)
		if a.Label == "" {
			a.Label = a.Type // reasonable fallback so the button renders
		}
		a.Icon, _ = m["icon"].(string)
		a.Reason, _ = m["reason"].(string)
		a.Priority, _ = m["priority"].(string)
		a.Source, _ = m["source"].(string)
		if payload, ok := m["payload"].(map[string]any); ok {
			a.Payload = payload
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractMapSlice is the shared helper behind extractSources /
// extractActions — both fields ride on the same payload and need the
// same root/output/output.output search path.
func extractMapSlice(p map[string]any, key string) []map[string]any {
	if p == nil {
		return nil
	}
	norm := normalisePayload(p)
	candidates := []any{norm[key]}
	if out, ok := norm["output"].(map[string]any); ok {
		candidates = append(candidates, out[key])
	}
	for _, c := range candidates {
		if c == nil {
			continue
		}
		raw, ok := c.([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// anonIDFromRequest builds a readable "anon-<ip>" identifier for an
// unauthenticated caller. The IP is pulled from X-Forwarded-For or
// X-Real-IP when the admin is behind a reverse proxy, falling back
// to RemoteAddr for direct connections.
//
// IPv6 passes through as-is (colons are legal in JSON strings); dots
// and colons are kept intact so operators can grep access logs with
// the exact value they see in the admin. When every upstream header
// is empty we return "anon-unknown" — beats an empty string that
// would collide across visitors.
func anonIDFromRequest(r *http.Request) string {
	ip := clientIP(r)
	if ip == "" {
		return "anon-unknown"
	}
	return "anon-" + ip
}

// clientIP returns the originating client IP. Priority order:
//  1. CF-Connecting-IP — set by Cloudflare on every request (all plans, free
//     and above); most trustworthy source when behind Cloudflare because
//     Traefik's overlay-network RemoteAddr and its X-Forwarded-For may
//     contain Docker internal IPs rather than the real visitor.
//  2. True-Client-IP  — set by Cloudflare Enterprise Managed Transform or
//     legacy Cloudflare proxy; same semantics as CF-Connecting-IP.
//  3. X-Forwarded-For — first (leftmost) hop; fallback for non-Cloudflare
//     reverse proxies (nginx, envoy).
//  4. X-Real-IP       — set by a subset of proxies.
//  5. RemoteAddr      — direct connection / last resort.
func clientIP(r *http.Request) string {
	// Cloudflare always sets CF-Connecting-IP to the real visitor IP.
	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		return cf
	}
	if tci := strings.TrimSpace(r.Header.Get("True-Client-IP")); tci != "" {
		return tci
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if v := strings.TrimSpace(xff); v != "" {
			return v
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr isn't "host:port" — e.g. a Unix socket in tests.
		// Return it raw rather than dropping to "unknown".
		return r.RemoteAddr
	}
	return host
}

// normalisePayload converts a map that may contain typed Go structs into a
// pure map[string]any via a JSON round-trip.
func normalisePayload(p map[string]any) map[string]any {
	b, err := json.Marshal(p)
	if err != nil {
		return p
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return p
	}
	return out
}

