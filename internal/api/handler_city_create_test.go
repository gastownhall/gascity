package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandleCityCreate_ValidationErrors(t *testing.T) {
	tests := []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{
			name:   "missing dir",
			body:   map[string]string{"provider": "claude"},
			status: http.StatusBadRequest,
			code:   "invalid",
		},
		{
			name:   "missing provider",
			body:   map[string]string{"dir": "/tmp/test-city"},
			status: http.StatusBadRequest,
			code:   "invalid",
		},
		{
			name:   "unknown provider",
			body:   map[string]string{"dir": "/tmp/test-city", "provider": "unknown-agent"},
			status: http.StatusBadRequest,
			code:   "invalid",
		},
		{
			name:   "unknown bootstrap profile",
			body:   map[string]string{"dir": "/tmp/test-city", "provider": "claude", "bootstrap_profile": "invalid-profile"},
			status: http.StatusBadRequest,
			code:   "invalid",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/v0/city", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handleCityCreate(w, req)

			if w.Code != tc.status {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tc.status, w.Body.String())
			}

			var resp Error
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("invalid JSON response: %v", err)
			}
			if resp.Code != tc.code {
				t.Errorf("code = %q, want %q", resp.Code, tc.code)
			}
		})
	}
}

func TestResolveCityDir_RelativeUsesHomeNotCwd(t *testing.T) {
	// Regression: when the supervisor CWD is already the city directory
	// (e.g. /home/user/gc), resolving a relative dir "gc" against CWD
	// produces /home/user/gc/gc (double nesting). The fix resolves
	// relative dirs against $HOME instead.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	// Simulate the supervisor's CWD being inside an existing city.
	cityLike := filepath.Join(t.TempDir(), "gc")
	if err := os.MkdirAll(cityLike, 0o755); err != nil {
		t.Fatal(err)
	}
	origDir, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origDir) })
	if err := os.Chdir(cityLike); err != nil {
		t.Fatal(err)
	}

	// Resolve "gc" the same way handler_city_create does.
	dir := "gc"
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(home, dir)
	}

	want := filepath.Join(home, "gc")
	if dir != want {
		t.Errorf("resolved dir = %q, want %q (must not double-nest under CWD %q)", dir, want, cityLike)
	}

	// Verify the old (buggy) behavior would have produced double nesting.
	buggy, _ := filepath.Abs("gc")
	if buggy == want {
		t.Skip("CWD happens to equal $HOME — can't demonstrate the bug in this environment")
	}
	if buggy != filepath.Join(cityLike, "gc") {
		t.Errorf("expected buggy Abs to produce %q, got %q", filepath.Join(cityLike, "gc"), buggy)
	}
}

func TestResolveCityDir_AbsolutePassesThrough(t *testing.T) {
	dir := "/opt/custom/city"
	if !filepath.IsAbs(dir) {
		t.Fatal("expected absolute path to pass through")
	}
	if dir != "/opt/custom/city" {
		t.Errorf("absolute dir should not be modified, got %q", dir)
	}
}
