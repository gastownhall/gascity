package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type routeExpectation struct {
	name       string
	method     string
	path       string
	wantStatus int
}

func assertRouteStatuses(t *testing.T, h http.Handler, routes []routeExpectation) {
	t.Helper()

	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s status = %d, want %d", tc.method, tc.path, rec.Code, tc.wantStatus)
			}
		})
	}
}

func legacyCityRoutes() []routeExpectation {
	// Inventory derived from the merge-base HTTP/SSE city surface. Non-survivor
	// operations should stay unavailable after the WS cutover.
	return []routeExpectation{
		{name: "status", method: http.MethodGet, path: "/v0/status", wantStatus: http.StatusNotFound},
		{name: "city get", method: http.MethodGet, path: "/v0/city", wantStatus: http.StatusMethodNotAllowed},
		{name: "city patch", method: http.MethodPatch, path: "/v0/city", wantStatus: http.StatusForbidden},
		{name: "agents list", method: http.MethodGet, path: "/v0/agents", wantStatus: http.StatusNotFound},
		{name: "agent get", method: http.MethodGet, path: "/v0/agent/coder", wantStatus: http.StatusNotFound},
		{name: "agent output", method: http.MethodGet, path: "/v0/agent/coder/output", wantStatus: http.StatusNotFound},
		{name: "agent output stream", method: http.MethodGet, path: "/v0/agent/coder/output/stream", wantStatus: http.StatusNotFound},
		{name: "agent create", method: http.MethodPost, path: "/v0/agents", wantStatus: http.StatusForbidden},
		{name: "agent update", method: http.MethodPatch, path: "/v0/agent/coder", wantStatus: http.StatusForbidden},
		{name: "agent delete", method: http.MethodDelete, path: "/v0/agent/coder", wantStatus: http.StatusForbidden},
		{name: "agent action", method: http.MethodPost, path: "/v0/agent/coder/suspend", wantStatus: http.StatusForbidden},
		{name: "config get", method: http.MethodGet, path: "/v0/config", wantStatus: http.StatusNotFound},
		{name: "config explain", method: http.MethodGet, path: "/v0/config/explain", wantStatus: http.StatusNotFound},
		{name: "config validate", method: http.MethodGet, path: "/v0/config/validate", wantStatus: http.StatusNotFound},
		{name: "agent patches list", method: http.MethodGet, path: "/v0/patches/agents", wantStatus: http.StatusNotFound},
		{name: "agent patch get", method: http.MethodGet, path: "/v0/patches/agent/coder", wantStatus: http.StatusNotFound},
		{name: "agent patch set", method: http.MethodPut, path: "/v0/patches/agents", wantStatus: http.StatusForbidden},
		{name: "agent patch delete", method: http.MethodDelete, path: "/v0/patches/agent/coder", wantStatus: http.StatusForbidden},
		{name: "rig patches list", method: http.MethodGet, path: "/v0/patches/rigs", wantStatus: http.StatusNotFound},
		{name: "rig patch get", method: http.MethodGet, path: "/v0/patches/rig/default", wantStatus: http.StatusNotFound},
		{name: "rig patch set", method: http.MethodPut, path: "/v0/patches/rigs", wantStatus: http.StatusForbidden},
		{name: "rig patch delete", method: http.MethodDelete, path: "/v0/patches/rig/default", wantStatus: http.StatusForbidden},
		{name: "provider patches list", method: http.MethodGet, path: "/v0/patches/providers", wantStatus: http.StatusNotFound},
		{name: "provider patch get", method: http.MethodGet, path: "/v0/patches/provider/claude", wantStatus: http.StatusNotFound},
		{name: "provider patch set", method: http.MethodPut, path: "/v0/patches/providers", wantStatus: http.StatusForbidden},
		{name: "provider patch delete", method: http.MethodDelete, path: "/v0/patches/provider/claude", wantStatus: http.StatusForbidden},
		{name: "providers list", method: http.MethodGet, path: "/v0/providers", wantStatus: http.StatusNotFound},
		{name: "provider get", method: http.MethodGet, path: "/v0/provider/claude", wantStatus: http.StatusNotFound},
		{name: "provider create", method: http.MethodPost, path: "/v0/providers", wantStatus: http.StatusForbidden},
		{name: "provider update", method: http.MethodPatch, path: "/v0/provider/claude", wantStatus: http.StatusForbidden},
		{name: "provider delete", method: http.MethodDelete, path: "/v0/provider/claude", wantStatus: http.StatusForbidden},
		{name: "rigs list", method: http.MethodGet, path: "/v0/rigs", wantStatus: http.StatusNotFound},
		{name: "rig get", method: http.MethodGet, path: "/v0/rig/default", wantStatus: http.StatusNotFound},
		{name: "rig create", method: http.MethodPost, path: "/v0/rigs", wantStatus: http.StatusForbidden},
		{name: "rig update", method: http.MethodPatch, path: "/v0/rig/default", wantStatus: http.StatusForbidden},
		{name: "rig delete", method: http.MethodDelete, path: "/v0/rig/default", wantStatus: http.StatusForbidden},
		{name: "rig action", method: http.MethodPost, path: "/v0/rig/default/suspend", wantStatus: http.StatusForbidden},
		{name: "beads list", method: http.MethodGet, path: "/v0/beads", wantStatus: http.StatusNotFound},
		{name: "beads graph", method: http.MethodGet, path: "/v0/beads/graph/gc-1", wantStatus: http.StatusNotFound},
		{name: "beads ready", method: http.MethodGet, path: "/v0/beads/ready", wantStatus: http.StatusNotFound},
		{name: "bead create", method: http.MethodPost, path: "/v0/beads", wantStatus: http.StatusForbidden},
		{name: "bead get", method: http.MethodGet, path: "/v0/bead/gc-1", wantStatus: http.StatusNotFound},
		{name: "bead deps", method: http.MethodGet, path: "/v0/bead/gc-1/deps", wantStatus: http.StatusNotFound},
		{name: "bead close", method: http.MethodPost, path: "/v0/bead/gc-1/close", wantStatus: http.StatusForbidden},
		{name: "bead reopen", method: http.MethodPost, path: "/v0/bead/gc-1/reopen", wantStatus: http.StatusForbidden},
		{name: "bead update post", method: http.MethodPost, path: "/v0/bead/gc-1/update", wantStatus: http.StatusForbidden},
		{name: "bead update patch", method: http.MethodPatch, path: "/v0/bead/gc-1", wantStatus: http.StatusForbidden},
		{name: "bead assign", method: http.MethodPost, path: "/v0/bead/gc-1/assign", wantStatus: http.StatusForbidden},
		{name: "bead delete", method: http.MethodDelete, path: "/v0/bead/gc-1", wantStatus: http.StatusForbidden},
		{name: "mail list", method: http.MethodGet, path: "/v0/mail", wantStatus: http.StatusNotFound},
		{name: "mail send", method: http.MethodPost, path: "/v0/mail", wantStatus: http.StatusForbidden},
		{name: "mail count", method: http.MethodGet, path: "/v0/mail/count", wantStatus: http.StatusNotFound},
		{name: "mail thread", method: http.MethodGet, path: "/v0/mail/thread/thread-1", wantStatus: http.StatusNotFound},
		{name: "mail get", method: http.MethodGet, path: "/v0/mail/msg-1", wantStatus: http.StatusNotFound},
		{name: "mail read", method: http.MethodPost, path: "/v0/mail/msg-1/read", wantStatus: http.StatusForbidden},
		{name: "mail mark unread", method: http.MethodPost, path: "/v0/mail/msg-1/mark-unread", wantStatus: http.StatusForbidden},
		{name: "mail archive", method: http.MethodPost, path: "/v0/mail/msg-1/archive", wantStatus: http.StatusForbidden},
		{name: "mail reply", method: http.MethodPost, path: "/v0/mail/msg-1/reply", wantStatus: http.StatusForbidden},
		{name: "mail delete", method: http.MethodDelete, path: "/v0/mail/msg-1", wantStatus: http.StatusForbidden},
		{name: "convoys list", method: http.MethodGet, path: "/v0/convoys", wantStatus: http.StatusNotFound},
		{name: "convoy get", method: http.MethodGet, path: "/v0/convoy/cv-1", wantStatus: http.StatusNotFound},
		{name: "convoy create", method: http.MethodPost, path: "/v0/convoys", wantStatus: http.StatusForbidden},
		{name: "convoy add", method: http.MethodPost, path: "/v0/convoy/cv-1/add", wantStatus: http.StatusForbidden},
		{name: "convoy remove", method: http.MethodPost, path: "/v0/convoy/cv-1/remove", wantStatus: http.StatusForbidden},
		{name: "convoy check", method: http.MethodGet, path: "/v0/convoy/cv-1/check", wantStatus: http.StatusNotFound},
		{name: "convoy close", method: http.MethodPost, path: "/v0/convoy/cv-1/close", wantStatus: http.StatusForbidden},
		{name: "convoy delete", method: http.MethodDelete, path: "/v0/convoy/cv-1", wantStatus: http.StatusForbidden},
		{name: "events list", method: http.MethodGet, path: "/v0/events", wantStatus: http.StatusNotFound},
		{name: "events stream", method: http.MethodGet, path: "/v0/events/stream", wantStatus: http.StatusNotFound},
		{name: "events emit", method: http.MethodPost, path: "/v0/events", wantStatus: http.StatusForbidden},
		{name: "orders list", method: http.MethodGet, path: "/v0/orders", wantStatus: http.StatusNotFound},
		{name: "orders feed", method: http.MethodGet, path: "/v0/orders/feed", wantStatus: http.StatusNotFound},
		{name: "orders check", method: http.MethodGet, path: "/v0/orders/check", wantStatus: http.StatusNotFound},
		{name: "orders history", method: http.MethodGet, path: "/v0/orders/history", wantStatus: http.StatusNotFound},
		{name: "order history detail", method: http.MethodGet, path: "/v0/order/history/gc-1", wantStatus: http.StatusNotFound},
		{name: "order get", method: http.MethodGet, path: "/v0/order/daily", wantStatus: http.StatusNotFound},
		{name: "order enable", method: http.MethodPost, path: "/v0/order/daily/enable", wantStatus: http.StatusForbidden},
		{name: "order disable", method: http.MethodPost, path: "/v0/order/daily/disable", wantStatus: http.StatusForbidden},
		{name: "formulas list", method: http.MethodGet, path: "/v0/formulas", wantStatus: http.StatusNotFound},
		{name: "formulas feed", method: http.MethodGet, path: "/v0/formulas/feed", wantStatus: http.StatusNotFound},
		{name: "formula runs", method: http.MethodGet, path: "/v0/formulas/daily/runs", wantStatus: http.StatusNotFound},
		{name: "formulas detail", method: http.MethodGet, path: "/v0/formulas/daily", wantStatus: http.StatusNotFound},
		{name: "formula detail", method: http.MethodGet, path: "/v0/formula/daily", wantStatus: http.StatusNotFound},
		{name: "workflow get", method: http.MethodGet, path: "/v0/workflow/wf-1", wantStatus: http.StatusNotFound},
		{name: "workflow delete", method: http.MethodDelete, path: "/v0/workflow/wf-1", wantStatus: http.StatusForbidden},
		{name: "sessions list", method: http.MethodGet, path: "/v0/sessions", wantStatus: http.StatusNotFound},
		{name: "session create", method: http.MethodPost, path: "/v0/sessions", wantStatus: http.StatusForbidden},
		{name: "session get", method: http.MethodGet, path: "/v0/session/s-1", wantStatus: http.StatusNotFound},
		{name: "session transcript", method: http.MethodGet, path: "/v0/session/s-1/transcript", wantStatus: http.StatusNotFound},
		{name: "session pending", method: http.MethodGet, path: "/v0/session/s-1/pending", wantStatus: http.StatusNotFound},
		{name: "session stream", method: http.MethodGet, path: "/v0/session/s-1/stream", wantStatus: http.StatusNotFound},
		{name: "session patch", method: http.MethodPatch, path: "/v0/session/s-1", wantStatus: http.StatusForbidden},
		{name: "session submit", method: http.MethodPost, path: "/v0/session/s-1/submit", wantStatus: http.StatusForbidden},
		{name: "session messages", method: http.MethodPost, path: "/v0/session/s-1/messages", wantStatus: http.StatusForbidden},
		{name: "session stop", method: http.MethodPost, path: "/v0/session/s-1/stop", wantStatus: http.StatusForbidden},
		{name: "session kill", method: http.MethodPost, path: "/v0/session/s-1/kill", wantStatus: http.StatusForbidden},
		{name: "session respond", method: http.MethodPost, path: "/v0/session/s-1/respond", wantStatus: http.StatusForbidden},
		{name: "session suspend", method: http.MethodPost, path: "/v0/session/s-1/suspend", wantStatus: http.StatusForbidden},
		{name: "session close", method: http.MethodPost, path: "/v0/session/s-1/close", wantStatus: http.StatusForbidden},
		{name: "session wake", method: http.MethodPost, path: "/v0/session/s-1/wake", wantStatus: http.StatusForbidden},
		{name: "session rename", method: http.MethodPost, path: "/v0/session/s-1/rename", wantStatus: http.StatusForbidden},
		{name: "session agents list", method: http.MethodGet, path: "/v0/session/s-1/agents", wantStatus: http.StatusNotFound},
		{name: "session agent get", method: http.MethodGet, path: "/v0/session/s-1/agents/agent-1", wantStatus: http.StatusNotFound},
		{name: "packs list", method: http.MethodGet, path: "/v0/packs", wantStatus: http.StatusNotFound},
		{name: "sling", method: http.MethodPost, path: "/v0/sling", wantStatus: http.StatusForbidden},
		{name: "services list", method: http.MethodGet, path: "/v0/services", wantStatus: http.StatusNotFound},
		{name: "service get", method: http.MethodGet, path: "/v0/service/review-intake", wantStatus: http.StatusNotFound},
		{name: "service restart", method: http.MethodPost, path: "/v0/service/review-intake/restart", wantStatus: http.StatusForbidden},
		{name: "extmsg inbound", method: http.MethodPost, path: "/v0/extmsg/inbound", wantStatus: http.StatusForbidden},
		{name: "extmsg outbound", method: http.MethodPost, path: "/v0/extmsg/outbound", wantStatus: http.StatusForbidden},
		{name: "extmsg bindings", method: http.MethodGet, path: "/v0/extmsg/bindings", wantStatus: http.StatusNotFound},
		{name: "extmsg bind", method: http.MethodPost, path: "/v0/extmsg/bind", wantStatus: http.StatusForbidden},
		{name: "extmsg unbind", method: http.MethodPost, path: "/v0/extmsg/unbind", wantStatus: http.StatusForbidden},
		{name: "extmsg groups lookup", method: http.MethodGet, path: "/v0/extmsg/groups", wantStatus: http.StatusNotFound},
		{name: "extmsg groups ensure", method: http.MethodPost, path: "/v0/extmsg/groups", wantStatus: http.StatusForbidden},
		{name: "extmsg participant upsert", method: http.MethodPost, path: "/v0/extmsg/participants", wantStatus: http.StatusForbidden},
		{name: "extmsg participant remove", method: http.MethodDelete, path: "/v0/extmsg/participants", wantStatus: http.StatusForbidden},
		{name: "extmsg transcript", method: http.MethodGet, path: "/v0/extmsg/transcript", wantStatus: http.StatusNotFound},
		{name: "extmsg transcript ack", method: http.MethodPost, path: "/v0/extmsg/transcript/ack", wantStatus: http.StatusForbidden},
		{name: "extmsg adapters", method: http.MethodGet, path: "/v0/extmsg/adapters", wantStatus: http.StatusNotFound},
		{name: "extmsg adapter register", method: http.MethodPost, path: "/v0/extmsg/adapters", wantStatus: http.StatusForbidden},
		{name: "extmsg adapter unregister", method: http.MethodDelete, path: "/v0/extmsg/adapters", wantStatus: http.StatusForbidden},
	}
}

func prefixedLegacyRoutes(prefix string, routes []routeExpectation, mapPath func(path string) (string, bool)) []routeExpectation {
	var out []routeExpectation
	for _, route := range routes {
		mapped, ok := mapPath(route.path)
		if !ok {
			continue
		}
		out = append(out, routeExpectation{
			name:       prefix + route.name,
			method:     route.method,
			path:       mapped,
			wantStatus: route.wantStatus,
		})
	}
	return out
}

func bareCompatRoutes(routes []routeExpectation) []routeExpectation {
	out := prefixedLegacyRoutes("bare compat ", routes, func(path string) (string, bool) {
		return path, true
	})
	for i := range out {
		if out[i].path == "/v0/city" && out[i].method == http.MethodGet {
			out[i].wantStatus = http.StatusNotFound
		}
	}
	return out
}

func namespacedCompatRoutes(city string, routes []routeExpectation) []routeExpectation {
	out := prefixedLegacyRoutes("namespaced ", routes, func(path string) (string, bool) {
		switch path {
		case "/v0/status":
			return "/v0/city/" + city, true
		case "/v0/city":
			return "", false
		default:
			if !strings.HasPrefix(path, "/v0/") {
				return "", false
			}
			return "/v0/city/" + city + strings.TrimPrefix(path, "/v0"), true
		}
	})
	for i := range out {
		if out[i].path == "/v0/city/"+city && out[i].method == http.MethodGet {
			out[i].wantStatus = http.StatusBadRequest
		}
	}
	return out
}

func TestCityLegacyHTTPRoutesUnavailable(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	assertRouteStatuses(t, srv, legacyCityRoutes())
}

func TestSupervisorLegacyHTTPRoutesUnavailable(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	sm := newTestSupervisorMux(t, map[string]*fakeState{"alpha": s1})
	handler := sm.Handler()

	routes := []routeExpectation{
		{name: "cities list", method: http.MethodGet, path: "/v0/cities", wantStatus: http.StatusNotFound},
	}
	routes = append(routes, bareCompatRoutes(legacyCityRoutes())...)
	routes = append(routes, namespacedCompatRoutes("alpha", legacyCityRoutes())...)

	assertRouteStatuses(t, handler, routes)
}
