package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/gitcred"
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

func TestHandlePackAddMapsAuthErrorToCredentialRequiredConflict(t *testing.T) {
	restoreResolver := stubPackSourceResolver(t, map[string][]net.IP{
		"github.com": {net.ParseIP("140.82.112.3")},
	})
	defer restoreResolver()

	const secret = "ghp_must_not_reach_the_response"
	orig := packAddImport
	packAddImport = func(fsys.FS, string, string, string, string) (*importsvc.AddResult, error) {
		return nil, fmt.Errorf("resolving pack version: %w", &gitcred.AuthError{
			Host:      "github.com",
			OrgPrefix: "github.com/gascity",
			Repo:      "https://github.com/gascity/maintainer-city",
			Output:    "fatal: Authentication failed for " + secret,
			Err:       errors.New(secret),
		})
	}
	defer func() { packAddImport = orig }()

	state := newFakeMutatorState(t)
	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodPost, cityURL(state, "/packs"),
		strings.NewReader(`{"source":"https://github.com/gascity/maintainer-city/tree/main"}`))
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	var problem apierr.ErrorModel
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem response: %v; body = %s", err, rec.Body.String())
	}
	if problem.Type != "urn:gascity:error:pack-credential-required" ||
		problem.Code != "pack-credential-required" {
		t.Fatalf("type/code = %q/%q, want pack-credential-required; body = %s",
			problem.Type, problem.Code, rec.Body.String())
	}
	wantDetails := map[string]string{
		"body.host": "github.com/gascity",
		"body.repo": "https://github.com/gascity/maintainer-city",
		"body.hint": "register a pack credential for this host",
	}
	for _, detail := range problem.Errors {
		value, ok := detail.Value.(string)
		if !ok {
			continue
		}
		if want, exists := wantDetails[detail.Location]; exists && value == want {
			delete(wantDetails, detail.Location)
		}
	}
	if len(wantDetails) != 0 {
		t.Fatalf("missing safe credential details %v; body = %s", wantDetails, rec.Body.String())
	}
	for _, forbidden := range []string{secret, "Authentication failed"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("credential response leaked %q: %s", forbidden, rec.Body.String())
		}
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
