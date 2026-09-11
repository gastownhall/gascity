package dashboardspa

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
