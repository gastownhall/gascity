package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func TestSupervisorCityCreateConflictsWhenTargetAlreadyInitialized(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{
			name: "scaffold_present",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				for _, path := range []string{
					filepath.Join(dir, citylayout.RuntimeRoot),
					filepath.Join(dir, citylayout.RuntimeRoot, "cache"),
					filepath.Join(dir, citylayout.RuntimeRoot, "runtime"),
					filepath.Join(dir, citylayout.RuntimeRoot, "system"),
				} {
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, citylayout.RuntimeRoot, "events.jsonl"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "city")
			tc.setup(t, dir)

			sm := newTestSupervisorMux(t, map[string]*fakeState{})
			req := httptest.NewRequest(http.MethodPost, "/v0/city", strings.NewReader(`{"dir":"`+dir+`","provider":"claude"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-GC-Request", "test")
			rec := httptest.NewRecorder()

			sm.ServeHTTP(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "already initialized") {
				t.Fatalf("body = %q, want already initialized detail", rec.Body.String())
			}
		})
	}
}

func TestCityDirAlreadyInitializedAllowsConfigOnlyBootstrap(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, citylayout.CityConfigFile), []byte("[workspace]\nname = \"alpha\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if cityDirAlreadyInitialized(dir) {
		t.Fatal("config-only city should be left for gc init bootstrap")
	}
}
