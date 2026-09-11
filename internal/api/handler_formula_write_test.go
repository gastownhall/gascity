package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/configedit"
)

// fakeFormulaState adds FormulaMutator to the standard fake mutator state.
type fakeFormulaState struct {
	*fakeMutatorState
	formulas map[string][]byte
}

func newFakeFormulaState(t *testing.T) *fakeFormulaState {
	return &fakeFormulaState{fakeMutatorState: newFakeMutatorState(t), formulas: map[string][]byte{}}
}

func (f *fakeFormulaState) FormulaSource(name string) ([]byte, bool, error) {
	v, ok := f.formulas[name]
	return v, ok, nil
}

func (f *fakeFormulaState) UpsertFormula(name string, content []byte) error {
	f.formulas[name] = append([]byte(nil), content...)
	return nil
}

func (f *fakeFormulaState) DeleteFormula(name string) error {
	if _, ok := f.formulas[name]; !ok {
		return configedit.ErrNotFound
	}
	delete(f.formulas, name)
	return nil
}

const (
	validFormulaTOML         = "formula = \"hello\"\n"
	missingParentFormulaTOML = "formula = \"child\"\nextends = [\"parent\"]\n"
)

func TestFormulaWrite_UpsertSourceDelete(t *testing.T) {
	fs := newFakeFormulaState(t)
	h := newTestCityHandler(t, fs)

	// PUT upsert (valid).
	req := httptest.NewRequest("PUT", cityURL(fs, "/formulas/hello"), strings.NewReader(validFormulaTOML))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", w.Code, w.Body.String())
	}
	if string(fs.formulas["hello"]) != validFormulaTOML {
		t.Fatalf("upsert persisted %q, want %q", fs.formulas["hello"], validFormulaTOML)
	}

	// GET source.
	req = httptest.NewRequest("GET", cityURL(fs, "/formulas/hello/source"), nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("GET source status=%d body=%s", w.Code, w.Body.String())
	}

	// DELETE.
	req = httptest.NewRequest("DELETE", cityURL(fs, "/formulas/hello"), nil)
	req.Header.Set("X-GC-Request", "true")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := fs.formulas["hello"]; ok {
		t.Fatal("formula not deleted")
	}

	// DELETE missing -> 404.
	req = httptest.NewRequest("DELETE", cityURL(fs, "/formulas/hello"), nil)
	req.Header.Set("X-GC-Request", "true")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing status=%d, want 404", w.Code)
	}

	// GET missing source -> 404.
	req = httptest.NewRequest("GET", cityURL(fs, "/formulas/hello/source"), nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET missing source status=%d, want 404", w.Code)
	}
}

func TestFormulaWrite_Validate(t *testing.T) {
	fs := newFakeFormulaState(t)
	h := newTestCityHandler(t, fs)

	// Valid source.
	req := httptest.NewRequest("POST", cityURL(fs, "/formulas/hello/validate"), strings.NewReader(validFormulaTOML))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"valid":true`) {
		t.Fatalf("validate (valid) status=%d body=%s", w.Code, w.Body.String())
	}

	// Name mismatch -> valid:false with errors, still 200.
	req = httptest.NewRequest("POST", cityURL(fs, "/formulas/hello/validate"), strings.NewReader("formula = \"other\"\n"))
	req.Header.Set("X-GC-Request", "true")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"valid":false`) {
		t.Fatalf("validate (mismatch) status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFormulaWrite_ValidateRejectsMissingParent(t *testing.T) {
	fs := newFakeFormulaState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("POST", cityURL(fs, "/formulas/child/validate"), strings.NewReader(missingParentFormulaTOML))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("validate missing parent status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"valid":false`) {
		t.Fatalf("validate missing parent body=%s", w.Body.String())
	}
}

func TestFormulaWrite_ReservedNameRejected(t *testing.T) {
	fs := newFakeFormulaState(t)
	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest("PUT", cityURL(fs, "/formulas/feed"), strings.NewReader("formula = \"feed\"\n"))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT reserved name status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if _, ok := fs.formulas["feed"]; ok {
		t.Fatal("reserved formula must not be persisted")
	}
}

func TestFormulaWrite_UpsertRejectsInvalid(t *testing.T) {
	fs := newFakeFormulaState(t)
	h := newTestCityHandler(t, fs)

	// Name mismatch must 400 and NOT persist.
	req := httptest.NewRequest("PUT", cityURL(fs, "/formulas/zzz"), strings.NewReader("formula = \"other\"\n"))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if _, ok := fs.formulas["zzz"]; ok {
		t.Fatal("invalid formula must not be persisted")
	}
}

func TestFormulaWrite_UpsertRejectsMissingParent(t *testing.T) {
	fs := newFakeFormulaState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("PUT", cityURL(fs, "/formulas/child"), strings.NewReader(missingParentFormulaTOML))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT missing parent status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if _, ok := fs.formulas["child"]; ok {
		t.Fatal("missing-parent formula must not be persisted")
	}
}

func TestFormulaWrite_ValidateResolvesExtendsFromCityLayers(t *testing.T) {
	fs := newFakeFormulaState(t)
	formulaDir := t.TempDir()
	fs.cfg.FormulaLayers.City = []string{formulaDir}
	if err := os.WriteFile(filepath.Join(formulaDir, "parent.toml"), []byte("formula = \"parent\"\n"), 0o644); err != nil {
		t.Fatalf("write parent formula: %v", err)
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("POST", cityURL(fs, "/formulas/child/validate"), strings.NewReader("formula = \"child\"\nextends = [\"parent\"]\n"))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("validate resolved extends status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"valid":true`) {
		t.Fatalf("validate resolved extends body=%s", w.Body.String())
	}
}

// TestFormulaWrite_AcceptsNonWorkflowTypes pins that expansion and aspect
// formulas — authorable building blocks that workflows extend — validate and
// persist. The draft resolver must not impose the catalog reader's workflow-only
// gate, which would reject them with a misleading "not a workflow" error.
func TestFormulaWrite_AcceptsNonWorkflowTypes(t *testing.T) {
	fs := newFakeFormulaState(t)
	h := newTestCityHandler(t, fs)

	for _, tc := range []struct{ name, toml string }{
		{"myexp", "formula = \"myexp\"\ntype = \"expansion\"\n"},
		{"myaspect", "formula = \"myaspect\"\ntype = \"aspect\"\n"},
	} {
		req := httptest.NewRequest("POST", cityURL(fs, "/formulas/"+tc.name+"/validate"), strings.NewReader(tc.toml))
		req.Header.Set("X-GC-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"valid":true`) {
			t.Fatalf("validate %s status=%d body=%s", tc.name, w.Code, w.Body.String())
		}

		req = httptest.NewRequest("PUT", cityURL(fs, "/formulas/"+tc.name), strings.NewReader(tc.toml))
		req.Header.Set("X-GC-Request", "true")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s status=%d body=%s", tc.name, w.Code, w.Body.String())
		}
		if string(fs.formulas[tc.name]) != tc.toml {
			t.Fatalf("upsert %s persisted %q, want %q", tc.name, fs.formulas[tc.name], tc.toml)
		}
	}
}

// TestFormulaWrite_ValidatesRelativeDescriptionFileAgainstSaveDir pins that a
// strict graph.v2 draft whose step uses the dominant relative "../prompts/..."
// description_file form is validated against its eventual save directory
// (<cityRoot>/formulas, the highest-priority City layer), not an unrelated temp
// dir. The same draft must reject when the target prompt is absent and accept —
// through both /validate and PUT — once it exists at <cityRoot>/prompts.
func TestFormulaWrite_ValidatesRelativeDescriptionFileAgainstSaveDir(t *testing.T) {
	fs := newFakeFormulaState(t)
	cityRoot := t.TempDir()
	formulasDir := filepath.Join(cityRoot, "formulas")
	promptsDir := filepath.Join(cityRoot, "prompts")
	for _, d := range []string{formulasDir, promptsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	fs.cfg.FormulaLayers.City = []string{formulasDir}
	h := newTestCityHandler(t, fs)

	const draft = `formula = "adopt"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work"
description_file = "../prompts/foo.md"
`

	// Target prompt absent: validation must report invalid, not falsely accept.
	req := httptest.NewRequest("POST", cityURL(fs, "/formulas/adopt/validate"), strings.NewReader(draft))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("validate (missing prompt) status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"valid":false`) {
		t.Fatalf("validate (missing prompt) body=%s, want valid:false", w.Body.String())
	}

	// Create <cityRoot>/prompts/foo.md; the same relative reference must now
	// validate and persist, proving the anchor is the real save dir.
	if err := os.WriteFile(filepath.Join(promptsDir, "foo.md"), []byte("adopt prompt body\n"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	req = httptest.NewRequest("POST", cityURL(fs, "/formulas/adopt/validate"), strings.NewReader(draft))
	req.Header.Set("X-GC-Request", "true")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"valid":true`) {
		t.Fatalf("validate (present prompt) status=%d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("PUT", cityURL(fs, "/formulas/adopt"), strings.NewReader(draft))
	req.Header.Set("X-GC-Request", "true")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT (present prompt) status=%d body=%s", w.Code, w.Body.String())
	}
	if string(fs.formulas["adopt"]) != draft {
		t.Fatalf("upsert persisted %q, want draft", fs.formulas["adopt"])
	}
}

// TestFormulaWrite_AllowsDepth3RouteNames pins that names matching the depth-3
// detail routes (runs, source, validate, preview) are NOT reserved: those routes
// live under /formulas/{name}/..., so a formula named "runs" still routes cleanly
// to /formulas/{name}. Only "feed", the literal /formulas sibling, stays reserved.
func TestFormulaWrite_AllowsDepth3RouteNames(t *testing.T) {
	fs := newFakeFormulaState(t)
	h := newTestCityHandler(t, fs)

	for _, name := range []string{"runs", "source", "validate", "preview"} {
		body := "formula = \"" + name + "\"\n"
		req := httptest.NewRequest("PUT", cityURL(fs, "/formulas/"+name), strings.NewReader(body))
		req.Header.Set("X-GC-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %q status=%d, want 200; body=%s", name, w.Code, w.Body.String())
		}
		if string(fs.formulas[name]) != body {
			t.Fatalf("formula %q not persisted", name)
		}
	}
}

// TestFormulaWrite_RejectsInvalidNameLikeUpsert pins that validate and upsert
// agree on the editor's name-slug rule: names the save path rejects must also
// fail validate, so the validate endpoint is a faithful save preflight.
func TestFormulaWrite_RejectsInvalidNameLikeUpsert(t *testing.T) {
	fs := newFakeFormulaState(t)
	h := newTestCityHandler(t, fs)

	for _, bad := range []string{"a.b", "-lead", "trail-"} {
		body := "formula = \"" + bad + "\"\n"

		req := httptest.NewRequest("POST", cityURL(fs, "/formulas/"+bad+"/validate"), strings.NewReader(body))
		req.Header.Set("X-GC-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("validate %q status=%d body=%s", bad, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"valid":false`) {
			t.Fatalf("validate %q body=%s, want valid:false", bad, w.Body.String())
		}

		req = httptest.NewRequest("PUT", cityURL(fs, "/formulas/"+bad), strings.NewReader(body))
		req.Header.Set("X-GC-Request", "true")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT %q status=%d, want 400; body=%s", bad, w.Code, w.Body.String())
		}
		if _, ok := fs.formulas[bad]; ok {
			t.Fatalf("invalid-name formula %q must not be persisted", bad)
		}
	}
}
