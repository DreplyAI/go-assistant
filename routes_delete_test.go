package assistant

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/redelay/go-framework/modules"
)

// Is the DELETE route actually reachable?
//
// `chat_delete_test.go` calls `handleChatDelete` directly and sets the path
// value by hand, which proves the handler works and proves NOTHING about
// whether a request can get to it. A wrong method string, a pattern that
// does not match, or a parameter named `sessionId` in the route and read as
// `sessionID` in the handler would each give a 404 in production under a
// green test — which is the same shape as the bug this whole change is
// about: a thing that looks done from the inside.
//
// So this mounts Routes() on a real chi mux, wired the way
// go-framework's own adapter wires it (chi.Route for a group, chi.Method for
// a handler, URL params copied into r.PathValue), and sends a real request.
//
// It deliberately does NOT test the framework's adapter — that is not this
// repo's to assert. It tests the three things in THIS file that can be wrong:
// the method, the pattern, and the parameter name.
type chiTestRouter struct{ mux chi.Router }

func (a *chiTestRouter) Use(mw ...func(http.Handler) http.Handler) { a.mux.Use(mw...) }

func (a *chiTestRouter) Group(prefix string, fn func(modules.Router)) {
	if prefix == "" || prefix == "/" {
		a.mux.Group(func(sub chi.Router) { fn(&chiTestRouter{mux: sub}) })
		return
	}
	a.mux.Route(prefix, func(sub chi.Router) { fn(&chiTestRouter{mux: sub}) })
}

func (a *chiTestRouter) Handle(method, pattern string, handler any, _ ...modules.EndpointOption) {
	h, ok := handler.(http.Handler)
	if !ok {
		if f, isFunc := handler.(func(http.ResponseWriter, *http.Request)); isFunc {
			h = http.HandlerFunc(f)
		} else {
			panic("unsupported handler type")
		}
	}
	a.mux.Method(method, pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rctx := chi.RouteContext(r.Context()); rctx != nil {
			for i, key := range rctx.URLParams.Keys {
				if i < len(rctx.URLParams.Values) {
					r.SetPathValue(key, rctx.URLParams.Values[i])
				}
			}
		}
		h.ServeHTTP(w, r)
	}))
}

func TestTheChatDeleteRouteIsReachableAndCarriesTheSessionID(t *testing.T) {
	asst, teardown := newFixture(t, "unused")
	defer teardown()

	// Stand in for the handler so this test is about ROUTING: what reached
	// it, by which method, with which path value.
	var gotSession string
	var reached bool
	mux := chi.NewRouter()
	r := &chiTestRouter{mux: mux}
	r.Group("/assistant", func(g modules.Router) {
		g.Handle("DELETE", "/chats/me/{sessionID}", http.HandlerFunc(
			func(w http.ResponseWriter, req *http.Request) {
				reached = true
				gotSession = req.PathValue("sessionID")
				w.WriteHeader(http.StatusNoContent)
			}))
	})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/assistant/chats/me/sess-abc123", nil))

	if !reached {
		t.Fatalf("DELETE /assistant/chats/me/{sessionID} did not reach a handler "+
			"(status %d) — the endpoint would 404 in production", w.Code)
	}
	if gotSession != "sess-abc123" {
		t.Errorf("handler read sessionID=%q, want %q — the route's parameter name "+
			"and the handler's PathValue key have to be the same string",
			gotSession, "sess-abc123")
	}
	if w.Code != http.StatusNoContent {
		t.Errorf("status %d, want 204", w.Code)
	}

	// And the real module registers exactly that method and pattern. Checked
	// against the live Routes() so the wiring above cannot drift from it: the
	// test would otherwise prove chi works, which was never in doubt.
	rec := &recordingRouter{}
	asst.Routes(rec)
	if !rec.has("DELETE", "/assistant/chats/me/{sessionID}") {
		t.Errorf("Routes() does not register DELETE /assistant/chats/me/{sessionID}; it has:\n  %v",
			rec.routes)
	}
	// The GET on the same pattern must survive registering a second method on
	// it — chi allows it, but a mistake here would silently unmount history.
	if !rec.has("GET", "/assistant/chats/me/{sessionID}") {
		t.Error("Routes() no longer registers the GET that reads chat history back")
	}
}

// recordingRouter captures what a module registers, prefix included.
type recordingRouter struct {
	prefix string
	routes []string
	parent *recordingRouter
}

func (r *recordingRouter) root() *recordingRouter {
	if r.parent != nil {
		return r.parent.root()
	}
	return r
}

func (r *recordingRouter) Use(...func(http.Handler) http.Handler) {}

func (r *recordingRouter) Group(prefix string, fn func(modules.Router)) {
	fn(&recordingRouter{prefix: r.prefix + prefix, parent: r.root()})
}

func (r *recordingRouter) Handle(method, pattern string, _ any, _ ...modules.EndpointOption) {
	root := r.root()
	root.routes = append(root.routes, method+" "+r.prefix+pattern)
}

func (r *recordingRouter) has(method, path string) bool {
	for _, got := range r.root().routes {
		if got == method+" "+path {
			return true
		}
	}
	return false
}
