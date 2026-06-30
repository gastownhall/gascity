package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/importsvc"
)

// The pack write handlers delegate to importsvc and only map its typed errors to
// HTTP, so the seams are stubbed here — no real source resolve / clone happens.

func TestHandlePackAdd(t *testing.T) {
	for _, tc := range []struct {
		name string
		add  func(fsys.FS, string, string, string, string) (*importsvc.AddResult, error)
		want int
	}{
		{"created", func(_ fsys.FS, _, source, _, version string) (*importsvc.AddResult, error) {
			return &importsvc.AddResult{Name: "review", Source: source, Version: version, GitBacked: true}, nil
		}, http.StatusCreated},
		{"already imported -> 409", func(fsys.FS, string, string, string, string) (*importsvc.AddResult, error) {
			return nil, importsvc.ErrImportExists
		}, http.StatusConflict},
		{"invalid source -> 400", func(fsys.FS, string, string, string, string) (*importsvc.AddResult, error) {
			return nil, importsvc.ErrInvalidSource
		}, http.StatusBadRequest},
		{"version resolve failed -> 502", func(fsys.FS, string, string, string, string) (*importsvc.AddResult, error) {
			return nil, importsvc.ErrVersionResolveFailed
		}, http.StatusBadGateway},
		{"install failed -> 500", func(fsys.FS, string, string, string, string) (*importsvc.AddResult, error) {
			return nil, importsvc.ErrInstallFailed
		}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := packAddImport
			packAddImport = tc.add
			defer func() { packAddImport = orig }()

			fs := newFakeMutatorState(t)
			h := newTestCityHandler(t, fs)
			req := httptest.NewRequest("POST", cityURL(fs, "/packs"),
				strings.NewReader(`{"source":"https://github.com/org/repo/tree/main/packs/review"}`))
			req.Header.Set("X-GC-Request", "true")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusCreated && !strings.Contains(w.Body.String(), `"review"`) {
				t.Errorf("created body missing binding name: %s", w.Body.String())
			}
		})
	}
}

func TestHandlePackRemove(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(fsys.FS, string, string) (*importsvc.RemoveResult, error)
		want   int
	}{
		{"ok", func(_ fsys.FS, _, name string) (*importsvc.RemoveResult, error) {
			return &importsvc.RemoveResult{Name: name}, nil
		}, http.StatusOK},
		{"not found -> 404", func(fsys.FS, string, string) (*importsvc.RemoveResult, error) {
			return nil, importsvc.ErrNotFound
		}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := packRemoveImport
			packRemoveImport = tc.remove
			defer func() { packRemoveImport = orig }()

			fs := newFakeMutatorState(t)
			h := newTestCityHandler(t, fs)
			req := httptest.NewRequest("DELETE", cityURL(fs, "/packs/review"), nil)
			req.Header.Set("X-GC-Request", "true")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
