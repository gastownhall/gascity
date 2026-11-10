package testutil

import "net/http"

// IsForeignLoopbackProbe reports whether r is a bare "GET /" that is not a
// WebSocket upgrade: the request a host daemon sweeping listening loopback
// sockets sends to whatever is listening. moshi-hook's Browser Preview port
// scan sends exactly that to each new loopback listener, on every loopback
// address (ga-ylpbsy.94.5).
//
// It is foreign only to a test server whose client never requests a bare
// "/": the gc API clients (routes under /v0/), the registry client (/api/),
// health checks (/health). A fake whose client does request "/" (a JWKS or
// discovery fetch) must not use IgnoreForeignLoopbackProbes.
func IsForeignLoopbackProbe(r *http.Request) bool {
	return r != nil && r.Method == http.MethodGet && r.URL != nil &&
		r.URL.Path == "/" && r.URL.RawQuery == "" && r.Header.Get("Upgrade") == ""
}

// IgnoreForeignLoopbackProbes wraps a test server's handler so a foreign
// loopback probe gets a 404 before h sees it: before h records it, counts it
// or fails the test as a misroute. Wrap the handler where the server is
// built (httptest.NewServer(testutil.IgnoreForeignLoopbackProbes(h))), so
// every request the client under test makes still reaches h unchanged.
func IgnoreForeignLoopbackProbes(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsForeignLoopbackProbe(r) {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}
