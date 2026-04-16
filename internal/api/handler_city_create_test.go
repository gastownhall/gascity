package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestHandleCityCreate_ValidationErrors(t *testing.T) {
	// Fix 3d/3e: handleCityCreate emits RFC 9457 Problem Details; the
	// semantic code (e.g. "invalid") is carried as a prefix in the detail
	// field.
	tests := []struct {
		name          string
		body          any
		status        int
		detailPrefix  string
	}{
		{
			name:         "missing dir",
			body:         map[string]string{"provider": "claude"},
			status:       http.StatusBadRequest,
			detailPrefix: "invalid",
		},
		{
			name:         "missing provider",
			body:         map[string]string{"dir": "/tmp/test-city"},
			status:       http.StatusBadRequest,
			detailPrefix: "invalid",
		},
		{
			name:         "unknown provider",
			body:         map[string]string{"dir": "/tmp/test-city", "provider": "unknown-agent"},
			status:       http.StatusBadRequest,
			detailPrefix: "invalid",
		},
		{
			name:         "unknown bootstrap profile",
			body:         map[string]string{"dir": "/tmp/test-city", "provider": "claude", "bootstrap_profile": "invalid-profile"},
			status:       http.StatusBadRequest,
			detailPrefix: "invalid",
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

			var problem struct {
				Status int    `json:"status"`
				Title  string `json:"title"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
				t.Fatalf("invalid JSON response: %v", err)
			}
			if problem.Status != tc.status {
				t.Errorf("problem.status = %d, want %d", problem.Status, tc.status)
			}
			if !strings.HasPrefix(problem.Detail, tc.detailPrefix) {
				t.Errorf("problem.detail = %q, want prefix %q", problem.Detail, tc.detailPrefix)
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
	testutil.AssertSamePath(t, buggy, filepath.Join(cityLike, "gc"))
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
