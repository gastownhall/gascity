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
	indexBytes, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, fmt.Errorf("dashboardspa: read index.html: %w", err)
	}
	csp := buildCSP(indexBytes)
	fileServer := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, p := range reservedPrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				http.NotFound(w, r)
				return
			}
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
		"img-src 'self' data:",
		"font-src 'self' data:",
		"connect-src 'self'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"frame-src 'none'",
		"worker-src 'self'",
		"manifest-src 'self'",
		"object-src 'none'",
	}, "; ")
}
