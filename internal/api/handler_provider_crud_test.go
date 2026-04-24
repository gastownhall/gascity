package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleProviderCreate_AllowsBaseOnlyDescendant(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/providers"), strings.NewReader(`{"name":"codex-max","base":"builtin:codex"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	spec, ok := fs.cfg.Providers["codex-max"]
	if !ok {
		t.Fatal("provider codex-max not created")
	}
	if spec.Base == nil || *spec.Base != "builtin:codex" {
		t.Fatalf("Base = %#v, want builtin:codex", spec.Base)
	}
	if spec.Command != "" {
		t.Fatalf("Command = %q, want empty for base-only descendant", spec.Command)
	}
}

func TestHandleProviderUpdate_UpdatesInheritanceFields(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Providers["custom"] = fs.cfg.Providers["test-agent"]
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := httptest.NewRequest(http.MethodPatch, cityURL(fs, "/provider/custom"), strings.NewReader(`{"base":"builtin:codex","args_append":["--sandbox"],"options_schema_merge":"by_key"}`))
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	spec := fs.cfg.Providers["custom"]
	if spec.Base == nil || *spec.Base != "builtin:codex" {
		t.Fatalf("Base = %#v, want builtin:codex", spec.Base)
	}
	if len(spec.ArgsAppend) != 1 || spec.ArgsAppend[0] != "--sandbox" {
		t.Fatalf("ArgsAppend = %#v, want [--sandbox]", spec.ArgsAppend)
	}
	if spec.OptionsSchemaMerge != "by_key" {
		t.Fatalf("OptionsSchemaMerge = %q, want by_key", spec.OptionsSchemaMerge)
	}
}
