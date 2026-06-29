package api

import (
	"net/http"
	"net/http/httptest"
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

const validFormulaTOML = "formula = \"hello\"\n"

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
