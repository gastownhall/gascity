package dashboardspa

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	h, err := NewStaticHandler()
	if err != nil {
		t.Fatalf("NewStaticHandler: %v", err)
	}
	return h
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestServesIndexAtRoot(t *testing.T) {
	rec := get(t, newHandler(t), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /: Content-Type = %q, want text/html", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("GET /: Cache-Control = %q, want no-store", cc)
	}
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Errorf("GET /: body missing SPA root element")
	}
}

func TestUnknownClientRouteFallsBackToIndex(t *testing.T) {
	rec := get(t, newHandler(t), "/city/example/agents")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /city/...: status = %d, want 200 (SPA fallback)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Errorf("GET /city/...: expected SPA shell, got %q", rec.Body.String())
	}
}

func TestReservedPrefixes404(t *testing.T) {
	h := newHandler(t)
	for _, p := range []string{"/v0/cities", "/api/city/x/config", "/health", "/openapi.json", "/debug/pprof/"} {
		rec := get(t, h, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404 (reserved, not SPA shell)", p, rec.Code)
		}
	}
}

// Helper paths belong to a proxy in front of gc. When one reaches gc, nothing
// serves it, and a page probing for the helper must hear a 404 so it hides the
// control. The SPA shell (200) would read as "served".
func TestSidecarPaths404(t *testing.T) {
	h := newHandler(t)
	for _, p := range []string{
		"/upload",
		"/pane?session=mayor&lines=1",
		"/keys",
		"/file?session=mayor&path=%2Ftmp%2Fa.png",
		"/commands?session=mayor",
		"/term",
		"/term/",
		"/term/token",
	} {
		rec := get(t, h, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404 (sidecar path, not SPA shell)", p, rec.Code)
		}
	}
}

// Client routes can share a first few letters with the helper paths. They are
// pages, so they must still get the SPA shell.
func TestClientRoutesNearHelperPathsServeTheShell(t *testing.T) {
	h := newHandler(t)
	for _, p := range []string{"/terminal/mayor", "/session/gc-123", "/pane-notes", "/uploads"} {
		rec := get(t, h, p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
			t.Errorf("GET %s: status = %d, want 200 with the SPA shell", p, rec.Code)
		}
	}
}

// The embedded dist/ has no manifest.webmanifest, so this test builds the
// handler on a fixture that has one. Go's own type table would send it as
// text/plain.
func TestManifestIsServedAsAManifest(t *testing.T) {
	h, err := newStaticHandler(fstest.MapFS{
		"index.html":           {Data: []byte(`<html><body><div id="root"></div></body></html>`)},
		"manifest.webmanifest": {Data: []byte(`{"name":"Gas City"}`)},
	})
	if err != nil {
		t.Fatalf("newStaticHandler: %v", err)
	}
	rec := get(t, h, "/manifest.webmanifest")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /manifest.webmanifest: status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("manifest Content-Type = %q, want application/manifest+json", ct)
	}
}

func TestHashedAssetIsImmutablyCached(t *testing.T) {
	// Vite emits content-hashed files under dist/assets/; discover a real one
	// from the embedded FS and confirm it is served with the immutable header.
	entries, err := fs.ReadDir(distFS, "dist/assets")
	if err != nil || len(entries) == 0 {
		t.Skip("no assets in embedded bundle")
	}
	asset := "/assets/" + entries[0].Name()
	rec := get(t, newHandler(t), asset)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", asset, rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("asset %s: Cache-Control = %q, want immutable", asset, cc)
	}
}

func TestCSPPinsInlineScriptHash(t *testing.T) {
	rec := get(t, newHandler(t), "/")
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("missing Content-Security-Policy header")
	}
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP missing script-src 'self': %q", csp)
	}
	// index.html ships an inline theme-boot script, so script-src must pin a
	// sha256 hash for it.
	if !strings.Contains(csp, "'sha256-") {
		t.Errorf("CSP script-src does not pin an inline-script hash: %q", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP missing frame-ancestors 'none': %q", csp)
	}
	// The in-app terminal frames /term/ on this same origin.
	if !strings.Contains(csp, "frame-src 'self'") {
		t.Errorf("CSP must allow framing this origin (frame-src 'self'): %q", csp)
	}
	// Attachment thumbnails are object URLs until the message is sent.
	if !strings.Contains(csp, "img-src 'self' data: blob:") {
		t.Errorf("CSP must allow blob: images for attachment thumbnails: %q", csp)
	}
}

func TestBuildCSPSkipsExternalScripts(t *testing.T) {
	// An external module script (with src=) must NOT contribute a hash; only
	// inline scripts do.
	idx := []byte(`<html><head>` +
		`<script>console.log("inline")</script>` +
		`<script type="module" src="/assets/app.js"></script>` +
		`</head><body></body></html>`)
	csp := buildCSP(idx)
	if got := strings.Count(csp, "'sha256-"); got != 1 {
		t.Errorf("buildCSP pinned %d hashes, want 1 (inline only): %q", got, csp)
	}
}
