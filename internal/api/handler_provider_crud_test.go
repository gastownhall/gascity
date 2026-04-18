package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestHandleProviderList(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {DisplayName: "Custom Agent", Command: "custom-cli"},
		"claude": {DisplayName: "My Claude", Command: "my-claude"}, // overrides builtin
	}
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/providers", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp listResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	// Should have city-level providers + builtins not overridden.
	if resp.Total < 10 {
		t.Errorf("total = %d, want >= 10 (builtins)", resp.Total)
	}

	// Verify city-level overrides appear first (alphabetically).
	items, ok := resp.Items.([]any)
	if !ok {
		t.Fatal("items is not an array")
	}
	first := items[0].(map[string]any)
	// City-level providers come first sorted alphabetically: "claude" before "custom"
	if first["name"] != "claude" {
		t.Errorf("first provider = %q, want %q", first["name"], "claude")
	}
	if first["city_level"] != true {
		t.Error("expected claude to be city_level=true")
	}
	if first["builtin"] != true {
		t.Error("expected claude to be builtin=true (overrides a builtin)")
	}
}

// TestHandleProviderList_PublicViewTriStateJSON verifies the public DTO
// (view=public) emits tri-state *bool capability fields as JSON null /
// true / false per the provider-inheritance design. Clients rely on
// this to distinguish "inherit from base" (null) from "explicitly off"
// (false).
func TestHandleProviderList_PublicViewTriStateJSON(t *testing.T) {
	trueVal := true
	falseVal := false
	fs := newFakeState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"explicit-on":  {Command: "x", SupportsHooks: &trueVal},
		"explicit-off": {Command: "y", SupportsHooks: &falseVal},
		"inherit":      {Command: "z"}, // SupportsHooks nil
	}
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/providers?view=public", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Decode into a raw map so we can inspect the JSON shape (null vs
	// omitted vs concrete value) — *bool fields serialize as null when nil.
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byName := make(map[string]map[string]any, len(resp.Items))
	for _, it := range resp.Items {
		byName[it["name"].(string)] = it
	}

	// explicit-on → JSON true
	if v, ok := byName["explicit-on"]; ok {
		if got := v["supports_hooks"]; got != true {
			t.Errorf("explicit-on supports_hooks = %v (%T), want true", got, got)
		}
	} else {
		t.Error("explicit-on not in response")
	}

	// explicit-off → JSON false (NOT null)
	if v, ok := byName["explicit-off"]; ok {
		got, present := v["supports_hooks"]
		if !present {
			t.Error("explicit-off supports_hooks key missing; want explicit false")
		} else if got != false {
			t.Errorf("explicit-off supports_hooks = %v (%T), want false", got, got)
		}
	} else {
		t.Error("explicit-off not in response")
	}

	// inherit → JSON null (nil *bool)
	if v, ok := byName["inherit"]; ok {
		got, present := v["supports_hooks"]
		if !present {
			t.Error("inherit supports_hooks key missing; want null")
		} else if got != nil {
			t.Errorf("inherit supports_hooks = %v (%T), want nil (JSON null)", got, got)
		}
	} else {
		t.Error("inherit not in response")
	}
}

func TestHandleProviderGet_CityLevel(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {DisplayName: "Custom Agent", Command: "custom-cli"},
	}
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/provider/custom", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp providerResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Name != "custom" {
		t.Errorf("name = %q, want %q", resp.Name, "custom")
	}
	if resp.CityLevel != true {
		t.Error("expected city_level=true")
	}
	if resp.Builtin != false {
		t.Error("expected builtin=false")
	}
}

func TestHandleProviderGet_Builtin(t *testing.T) {
	fs := newFakeState(t)
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/provider/claude", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp providerResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Name != "claude" {
		t.Errorf("name = %q, want %q", resp.Name, "claude")
	}
	if resp.Builtin != true {
		t.Error("expected builtin=true")
	}
	if resp.CityLevel != false {
		t.Error("expected city_level=false")
	}
}

func TestHandleProviderGet_NotFound(t *testing.T) {
	fs := newFakeState(t)
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/provider/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleProviderCreate(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"name":"myagent","command":"myagent-cli","display_name":"My Agent"}`
	req := newPostRequest("/v0/providers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// Verify provider was added.
	spec, ok := fs.cfg.Providers["myagent"]
	if !ok {
		t.Fatal("provider 'myagent' not found in config after create")
	}
	if spec.Command != "myagent-cli" {
		t.Errorf("command = %q, want %q", spec.Command, "myagent-cli")
	}
	if spec.DisplayName != "My Agent" {
		t.Errorf("display_name = %q, want %q", spec.DisplayName, "My Agent")
	}
}

func TestHandleProviderCreate_MissingName(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"command":"myagent-cli"}`
	req := newPostRequest("/v0/providers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleProviderCreate_MissingCommand(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"name":"myagent"}`
	req := newPostRequest("/v0/providers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleProviderCreate_Duplicate(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"existing": {Command: "existing-cli"},
	}
	srv := New(fs)

	body := `{"name":"existing","command":"other-cli"}`
	req := newPostRequest("/v0/providers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestHandleProviderUpdate(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {Command: "old-cli", DisplayName: "Old Name"},
	}
	srv := New(fs)

	body := `{"command":"new-cli","display_name":"New Name"}`
	req := httptest.NewRequest("PATCH", "/v0/provider/custom", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	spec := fs.cfg.Providers["custom"]
	if spec.Command != "new-cli" {
		t.Errorf("command = %q, want %q", spec.Command, "new-cli")
	}
	if spec.DisplayName != "New Name" {
		t.Errorf("display_name = %q, want %q", spec.DisplayName, "New Name")
	}
}

func TestHandleProviderUpdate_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"command":"new-cli"}`
	req := httptest.NewRequest("PATCH", "/v0/provider/nonexistent", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleProviderDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {Command: "custom-cli"},
	}
	srv := New(fs)

	req := httptest.NewRequest("DELETE", "/v0/provider/custom", nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if _, ok := fs.cfg.Providers["custom"]; ok {
		t.Error("provider 'custom' still exists after delete")
	}
}

func TestHandleProviderDelete_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	req := httptest.NewRequest("DELETE", "/v0/provider/nonexistent", nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleProviderUpdate_BuiltinConflict(t *testing.T) {
	fs := newFakeMutatorState(t)
	// No city-level "claude" — it's only a builtin.
	srv := New(fs)

	body := `{"command":"new-claude"}`
	req := httptest.NewRequest("PATCH", "/v0/provider/claude", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleProviderDelete_BuiltinConflict(t *testing.T) {
	fs := newFakeMutatorState(t)
	// No city-level "claude" — it's only a builtin.
	srv := New(fs)

	req := httptest.NewRequest("DELETE", "/v0/provider/claude", nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

// TestHandleProviderCreate_BaseOnlyNoCommand covers the relaxed CRUD
// validation: a provider with `base` set is authorable without an
// explicit `command` — the chain walk will inherit it.
func TestHandleProviderCreate_BaseOnlyNoCommand(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"name":"codex-max","base":"builtin:codex"}`
	req := newPostRequest("/v0/providers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	spec, ok := fs.cfg.Providers["codex-max"]
	if !ok {
		t.Fatal("provider 'codex-max' not found after create")
	}
	if spec.Command != "" {
		t.Errorf("command = %q, want empty", spec.Command)
	}
	if spec.Base == nil {
		t.Fatal("base pointer is nil; want set to builtin:codex")
	}
	if *spec.Base != "builtin:codex" {
		t.Errorf("*base = %q, want builtin:codex", *spec.Base)
	}
}

// TestHandleProviderCreate_BaseOnlyEmptyOptOut allows `"base": ""` as
// the explicit standalone opt-out.
func TestHandleProviderCreate_BaseOnlyEmptyOptOut(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	// command is present so this is valid even though base = "".
	body := `{"name":"standalone","base":"","command":"my-cli"}`
	req := newPostRequest("/v0/providers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}
	spec := fs.cfg.Providers["standalone"]
	if spec.Base == nil {
		t.Fatal("base pointer is nil; want pointer to empty string")
	}
	if *spec.Base != "" {
		t.Errorf("*base = %q, want empty (opt-out)", *spec.Base)
	}
}

// TestHandleProviderCreate_MissingCommandAndBase rejects a provider
// that declares neither command nor base.
func TestHandleProviderCreate_MissingCommandAndBase(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"name":"nothing"}`
	req := newPostRequest("/v0/providers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleProviderCreate_OptionsSchemaOmit round-trips an `omit = true`
// entry authored via POST /v0/providers. The subsequent GET must return
// the same entry verbatim so clients don't lose the removal sentinel.
func TestHandleProviderCreate_OptionsSchemaOmit(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{
		"name":"codex-max",
		"base":"builtin:codex",
		"options_schema_merge":"by_key",
		"options_schema":[
			{"key":"permission_mode","omit":true}
		]
	}`
	req := newPostRequest("/v0/providers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// Round-trip via GET.
	req = httptest.NewRequest("GET", "/v0/provider/codex-max", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body = %s", w.Code, w.Body.String())
	}
	var resp providerResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.OptionsSchema) != 1 {
		t.Fatalf("options_schema length = %d, want 1", len(resp.OptionsSchema))
	}
	if !resp.OptionsSchema[0].Omit {
		t.Errorf("omit = false on round-trip; want true")
	}
	if resp.OptionsSchema[0].Key != "permission_mode" {
		t.Errorf("key = %q, want permission_mode", resp.OptionsSchema[0].Key)
	}
	if resp.OptionsSchemaMerge != "by_key" {
		t.Errorf("options_schema_merge = %q, want by_key", resp.OptionsSchemaMerge)
	}
}

// TestHandleProviderCreate_OptionsSchemaOmitKeyRequired rejects omit
// entries that lack a key.
func TestHandleProviderCreate_OptionsSchemaOmitKeyRequired(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{
		"name":"bad",
		"command":"x",
		"options_schema":[{"omit":true}]
	}`
	req := newPostRequest("/v0/providers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleProviderCreate_OptionsSchemaOmitKeyOnly rejects omit
// entries that carry other fields (label, type, default, choices).
func TestHandleProviderCreate_OptionsSchemaOmitKeyOnly(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{
		"name":"bad",
		"command":"x",
		"options_schema":[{"key":"permission_mode","label":"oops","omit":true}]
	}`
	req := newPostRequest("/v0/providers", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleProviderUpdate_BasePatchSemantics verifies the
// null/omitted/""/value trichotomy documented in the design:
//
//	{"base": null}               → clear Base declaration (inherit default)
//	{"base": ""}                 → explicit standalone opt-out
//	{"base": "builtin:claude"}   → set concrete value
//	{} (field absent)            → no-op
func TestHandleProviderUpdate_BasePatchSemantics(t *testing.T) {
	newFS := func() *fakeMutatorState {
		fs := newFakeMutatorState(t)
		existing := "builtin:codex"
		fs.cfg.Providers = map[string]config.ProviderSpec{
			"codex-max": {Command: "codex", Base: &existing},
		}
		return fs
	}

	patch := func(fs *fakeMutatorState, body string) int {
		srv := New(fs)
		req := httptest.NewRequest("PATCH", "/v0/provider/codex-max", strings.NewReader(body))
		req.Header.Set("X-GC-Request", "true")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Code
	}

	t.Run("null clears base", func(t *testing.T) {
		fs := newFS()
		if code := patch(fs, `{"base":null}`); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if fs.cfg.Providers["codex-max"].Base != nil {
			t.Errorf("Base = %v, want nil (cleared)", *fs.cfg.Providers["codex-max"].Base)
		}
	})

	t.Run("empty string sets opt-out", func(t *testing.T) {
		fs := newFS()
		if code := patch(fs, `{"base":""}`); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		spec := fs.cfg.Providers["codex-max"]
		if spec.Base == nil {
			t.Fatal("Base pointer is nil; want pointer to \"\"")
		}
		if *spec.Base != "" {
			t.Errorf("*Base = %q, want \"\"", *spec.Base)
		}
	})

	t.Run("concrete value sets base", func(t *testing.T) {
		fs := newFS()
		if code := patch(fs, `{"base":"builtin:claude"}`); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		spec := fs.cfg.Providers["codex-max"]
		if spec.Base == nil || *spec.Base != "builtin:claude" {
			t.Errorf("Base = %v, want builtin:claude", spec.Base)
		}
	})

	t.Run("absent is no-op", func(t *testing.T) {
		fs := newFS()
		if code := patch(fs, `{"display_name":"X"}`); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		spec := fs.cfg.Providers["codex-max"]
		if spec.Base == nil || *spec.Base != "builtin:codex" {
			t.Errorf("Base = %v, want unchanged builtin:codex", spec.Base)
		}
		if spec.DisplayName != "X" {
			t.Errorf("DisplayName = %q, want X", spec.DisplayName)
		}
	})
}
