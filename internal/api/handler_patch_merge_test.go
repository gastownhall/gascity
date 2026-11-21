package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
)

type patchMergeState struct {
	*fakeState
	editor *configedit.Editor
}

func (s *patchMergeState) MergeAgentPatch(change configedit.AgentPatchMerge) (configedit.AgentPatchMergeResult, error) {
	result, err := s.editor.MergeAgentPatch(change)
	if err == nil {
		s.cfg, err = s.editor.LoadRaw()
	}
	return result, err
}

func TestAgentPatchMergeHTTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.toml")
	if err := os.WriteFile(path, []byte(`[workspace]
name = "test-city"
[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
[[patches.agent]]
name = "worker"
env = { KEEP = "yes" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &patchMergeState{fakeState: newFakeState(t), editor: configedit.NewEditor(fsys.OSFS{}, path)}
	var err error
	s.cfg, err = s.editor.LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	h := newTestCityHandler(t, s)
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, cityURL(s.fakeState, "/patches/agent/worker"), nil))
	if get.Code != http.StatusOK || get.Header().Get("X-GC-Patch-Revision") == "" {
		t.Fatalf("GET %d %s", get.Code, get.Body.String())
	}
	body := `{"name":"worker","revision":"` + get.Header().Get("X-GC-Patch-Revision") + `","option_defaults":{"model":"next"}}`
	for _, want := range []int{http.StatusOK, http.StatusConflict} {
		req := httptest.NewRequest(http.MethodPatch, cityURL(s.fakeState, "/patches/agents"), strings.NewReader(body))
		req.Header.Set("X-GC-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("PATCH %d want %d: %s", w.Code, want, w.Body.String())
		}
		if want == http.StatusOK {
			var result configedit.AgentPatchMergeResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.After.Env["KEEP"] != "yes" || result.After.OptionDefaults["model"] != "next" {
				t.Fatalf("result: %+v", result)
			}
		} else if !strings.Contains(w.Body.String(), `"code":"patch-revision-conflict"`) {
			t.Fatalf("untyped conflict: %s", w.Body.String())
		}
	}
}

func TestAgentPatchMergeRequiresScopeAndRevision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		code  string
	}{
		{"revision", configedit.ErrRevisionConflict, "patch-revision-conflict"},
		{"scope", configedit.ErrPatchScope, "patch-scope-conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := mutationError(tc.cause)
			if !strings.Contains(err.Error(), tc.cause.Error()) {
				t.Fatalf("error: %v", err)
			}
			// The handler uses the same domain error mapper as other mutations.
			var problem *apierr.ErrorModel
			ok := errors.As(err, &problem)
			if !ok || problem.Code != tc.code || problem.GetStatus() != http.StatusConflict {
				t.Fatalf("problem = %#v", err)
			}
		})
	}
}

func TestAgentPatchPutAcceptsOptionDefaults(t *testing.T) {
	s := newFakeMutatorState(t)
	h := newTestCityHandler(t, s)
	req := httptest.NewRequest(http.MethodPut, cityURL(s.fakeState, "/patches/agents"), strings.NewReader(`{"name":"worker","option_defaults":{"model":"next"}}`))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT %d: %s", w.Code, w.Body.String())
	}
	if got := s.cfg.Patches.Agents[0].OptionDefaults["model"]; got != "next" {
		t.Fatalf("model=%q", got)
	}
}

var _ AgentPatchMerger = (*patchMergeState)(nil)
