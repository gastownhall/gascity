package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
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

func TestHandleProviderCreate_PersistsACPTransportOverrides(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/providers"), strings.NewReader(
		`{"name":"custom-acp","command":"custom","acp_command":"custom-acp","acp_args":["rpc","--stdio"]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	spec, ok := fs.cfg.Providers["custom-acp"]
	if !ok {
		t.Fatal("provider custom-acp not created")
	}
	if spec.ACPCommand != "custom-acp" {
		t.Fatalf("ACPCommand = %q, want %q", spec.ACPCommand, "custom-acp")
	}
	if len(spec.ACPArgs) != 2 || spec.ACPArgs[0] != "rpc" || spec.ACPArgs[1] != "--stdio" {
		t.Fatalf("ACPArgs = %#v, want [rpc --stdio]", spec.ACPArgs)
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

func TestHandleProviderUpdate_UpdatesACPTransportOverrides(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Providers["custom"] = fs.cfg.Providers["test-agent"]
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := httptest.NewRequest(http.MethodPatch, cityURL(fs, "/provider/custom"), strings.NewReader(
		`{"acp_command":"custom-acp","acp_args":["rpc","--stdio"]}`))
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	spec := fs.cfg.Providers["custom"]
	if spec.ACPCommand != "custom-acp" {
		t.Fatalf("ACPCommand = %q, want %q", spec.ACPCommand, "custom-acp")
	}
	if len(spec.ACPArgs) != 2 || spec.ACPArgs[0] != "rpc" || spec.ACPArgs[1] != "--stdio" {
		t.Fatalf("ACPArgs = %#v, want [rpc --stdio]", spec.ACPArgs)
	}
}

func TestHandleProviderGet_IncludesACPTransportOverrides(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Providers["custom"] = config.ProviderSpec{
		Command:    "custom",
		ACPCommand: "custom-acp",
		ACPArgs:    []string{"rpc", "--stdio"},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/provider/custom"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp providerResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ACPCommand != "custom-acp" {
		t.Fatalf("ACPCommand = %q, want %q", resp.ACPCommand, "custom-acp")
	}
	if resp.ACPArgs == nil || len(*resp.ACPArgs) != 2 || (*resp.ACPArgs)[0] != "rpc" || (*resp.ACPArgs)[1] != "--stdio" {
		t.Fatalf("ACPArgs = %#v, want [rpc --stdio]", resp.ACPArgs)
	}
}

func TestHandleProviderGetPreservesExplicitEmptyACPArgs(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Providers["custom"] = config.ProviderSpec{
		Command:    "custom",
		ACPCommand: "custom-acp",
		ACPArgs:    []string{},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/provider/custom"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	acpArgs, ok := resp["acp_args"].([]any)
	if !ok {
		t.Fatalf("acp_args = %#v, want empty array field", resp["acp_args"])
	}
	if len(acpArgs) != 0 {
		t.Fatalf("acp_args len = %d, want 0", len(acpArgs))
	}
}
