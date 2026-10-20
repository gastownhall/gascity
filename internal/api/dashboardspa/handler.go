// Package dashboardspa serves the embedded Gas City dashboard single-page app.
//
// The compiled Vite bundle lives under dist/ and is embedded into the gc
// binary. NewStaticHandler returns an http.Handler that the supervisor mounts
// as its "/" catch-all, so the SPA, the typed /v0 API, and the host-side /api
// plane are all served same-origin from one listener.
package dashboardspa

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

// reservedPrefixes are request paths the SPA handler must never answer with
// index.html. They belong to sibling handlers (the typed /v0 API, health, the
// host-side /api plane, the OpenAPI document, pprof). Because the SPA is the
// "/" catch-all, a request that reaches this handler under one of these
// prefixes means the sibling handler did not match it, so returning 404 makes
// stale callers fail visibly instead of silently receiving the SPA shell.
var reservedPrefixes = []string{
	"/v0/",
	"/api/",
	"/health",
	"/openapi.json",
	"/debug/",
}

// sidecarPaths are never served by gc. A deployment can put its own reverse
// proxy in front of gc and send these paths to small local helpers instead:
// /upload, /pane, /keys, /file and /commands to an attachment and pane
// service, and /term/ to a browser terminal. A request for one of them only
// reaches gc when nothing in front of gc serves it.
//
// gc answers those with 404 so a page can probe for a helper: it asks for the
// path once and hides the matching control (attach, live pane, terminal link),
// or for /commands offers its built-in slash commands alone, when the answer
// is not a success. If gc handed back the SPA shell instead
// (200, HTML), every probe would read as "served", and a plain supervisor
// with no proxy would show controls that can only fail.
//
// Each entry matches itself and anything under it, so "/term" covers "/term/"
// but not a client route such as "/terminal/<session>".
var sidecarPaths = []string{
	"/upload",
	"/pane",
	"/keys",
	"/file",
	"/commands",
	"/term",
}

// notForTheSPA reports whether a request path belongs to something other than
// the SPA, so the catch-all must answer 404 rather than the app shell.
func notForTheSPA(path string) bool {
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	for _, p := range sidecarPaths {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// NewStaticHandler returns the embedded-SPA handler. It serves hashed assets
// with long-lived caching, serves index.html (no-store) for the app shell and
// for any unknown client-side route, 404s the reserved non-SPA prefixes, and
// applies a strict same-origin Content-Security-Policy whose script-src is
// pinned to the sha256 of each inline <script> found in index.html.
func NewStaticHandler() (http.Handler, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, fmt.Errorf("dashboardspa: sub fs: %w", err)
	}
	return newStaticHandler(sub)
}

// newStaticHandler serves the page from any file tree shaped like dist/.
// NewStaticHandler passes the embedded bundle. A test passes a small fixture
// when it needs a file the embedded bundle does not have, such as a
// manifest.webmanifest.
func newStaticHandler(sub fs.FS) (http.Handler, error) {
	indexBytes, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, fmt.Errorf("dashboardspa: read index.html: %w", err)
	}
	csp := buildCSP(indexBytes)
	fileServer := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if notForTheSPA(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		setSecurityHeaders(w, csp)

		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" || path == "index.html" {
			writeIndex(w, indexBytes)
			return
		}
		if st, statErr := fs.Stat(sub, path); statErr == nil && !st.IsDir() {
			// Vite emits content-hashed assets under assets/; they are safe to
			// cache forever. Everything else (favicons, manifest) revalidates.
			if strings.HasPrefix(path, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			// Go's own type table has no entry for a web app manifest, so
			// without this it would go out as text/plain.
			if strings.HasSuffix(path, ".webmanifest") {
				w.Header().Set("Content-Type", "application/manifest+json")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		// Unknown path under the SPA: hand back the shell so the client-side
		// router can render it (or its own not-found view).
		writeIndex(w, indexBytes)
	})
	return mux, nil
}

func writeIndex(w http.ResponseWriter, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(index)
}

func setSecurityHeaders(w http.ResponseWriter, csp string) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
}

var (
	inlineScriptRE = regexp.MustCompile(`(?is)<script((?:\s[^>]*)?)>(.*?)</script>`)
	srcAttrRE      = regexp.MustCompile(`(?i)\ssrc\s*=`)
)

// buildCSP computes a same-origin Content-Security-Policy. Each inline
// <script> (one without a src attribute) in index.html is hashed (sha256,
// base64) and pinned in script-src, so the strict policy admits exactly the
// bundle's own inline boot script and nothing else; external module scripts
// are governed by 'self'. The hash is read from the embedded index.html at
// boot rather than hardcoded, so it always tracks the shipped bundle.
func buildCSP(index []byte) string {
	var hashes []string
	for _, m := range inlineScriptRE.FindAllSubmatch(index, -1) {
		attrs, body := m[1], m[2]
		if srcAttrRE.Match(attrs) {
			continue
		}
		sum := sha256.Sum256(body)
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	scriptSrc := "script-src 'self'"
	if len(hashes) > 0 {
		scriptSrc += " " + strings.Join(hashes, " ")
	}
	return strings.Join([]string{
		"default-src 'self'",
		scriptSrc,
		"style-src 'self' 'unsafe-inline'",
		// blob: lets a page show an attached image as a thumbnail from an
		// object URL before anything is uploaded.
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src 'self'",
		"base-uri 'self'",
		"form-action 'self'",
		// Nobody may frame the dashboard. The dashboard may frame its own
		// origin: a terminal page can frame a browser terminal served at
		// /term/ on this same host, so a home-screen web app with no URL bar
		// still has a Back button around it.
		"frame-ancestors 'none'",
		"frame-src 'self'",
		"worker-src 'self'",
		"manifest-src 'self'",
		"object-src 'none'",
	}, "; ")
}
