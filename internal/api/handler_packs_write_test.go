package api

import (
	"net"
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
		{"name derive failed -> 400", func(fsys.FS, string, string, string, string) (*importsvc.AddResult, error) {
			return nil, importsvc.ErrNameDerive
		}, http.StatusBadRequest},
		{"reserved prefix -> 400", func(fsys.FS, string, string, string, string) (*importsvc.AddResult, error) {
			return nil, importsvc.ErrReservedPrefix
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

// TestPackAddRemoveSerializeThroughConfigWriteLock is the regression for the
// concurrency finding: the pack add/remove handlers must route their mutation
// through the per-city config write lock (ConfigWriteSerializer), so they can
// not interleave with each other or with configedit mutations of the same city.
func TestPackAddRemoveSerializeThroughConfigWriteLock(t *testing.T) {
	restore := stubPackSourceResolver(t, map[string][]net.IP{
		"github.com": {net.ParseIP("140.82.112.3")},
	})
	defer restore()

	origAdd, origRemove := packAddImport, packRemoveImport
	packAddImport = func(fsys.FS, string, string, string, string) (*importsvc.AddResult, error) {
		return &importsvc.AddResult{Name: "review", Source: "https://github.com/org/repo", GitBacked: true}, nil
	}
	packRemoveImport = func(fsys.FS, string, string) (*importsvc.RemoveResult, error) {
		return &importsvc.RemoveResult{Name: "review"}, nil
	}
	defer func() { packAddImport, packRemoveImport = origAdd, origRemove }()

	state := newFakeMutatorState(t)
	h := newTestCityHandler(t, state)

	addReq := httptest.NewRequest("POST", cityURL(state, "/packs"),
		strings.NewReader(`{"source":"https://github.com/org/repo/tree/main/packs/review"}`))
	addReq.Header.Set("X-GC-Request", "true")
	addRec := httptest.NewRecorder()
	h.ServeHTTP(addRec, addReq)
	if addRec.Code != http.StatusCreated {
		t.Fatalf("add status = %d, want %d; body=%s", addRec.Code, http.StatusCreated, addRec.Body.String())
	}
	if got := state.serializeCalls.Load(); got != 1 {
		t.Fatalf("add routed through config write lock %d times, want 1", got)
	}

	delReq := httptest.NewRequest("DELETE", cityURL(state, "/packs/review"), nil)
	delReq.Header.Set("X-GC-Request", "true")
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("remove status = %d, want %d; body=%s", delRec.Code, http.StatusOK, delRec.Body.String())
	}
	if got := state.serializeCalls.Load(); got != 2 {
		t.Fatalf("remove routed through config write lock; total calls = %d, want 2", got)
	}
}
