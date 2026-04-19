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
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/providers"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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

func TestHandleProviderGet_CityLevel(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {DisplayName: "Custom Agent", Command: "custom-cli"},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/provider/custom"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/provider/claude"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/provider/nonexistent"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleProviderCreate(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"name":"myagent","command":"myagent-cli","display_name":"My Agent"}`
	req := newPostRequest(cityURL(fs, "/providers"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandler(t, fs)

	body := `{"command":"myagent-cli"}`
	req := newPostRequest(cityURL(fs, "/providers"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandleProviderCreate_MissingCommand(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"name":"myagent"}`
	req := newPostRequest(cityURL(fs, "/providers"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandleProviderCreate_Duplicate(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"existing": {Command: "existing-cli"},
	}
	h := newTestCityHandler(t, fs)

	body := `{"name":"existing","command":"other-cli"}`
	req := newPostRequest(cityURL(fs, "/providers"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestHandleProviderUpdate(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {Command: "old-cli", DisplayName: "Old Name"},
	}
	h := newTestCityHandler(t, fs)

	body := `{"command":"new-cli","display_name":"New Name"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/provider/custom"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandler(t, fs)

	body := `{"command":"new-cli"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/provider/nonexistent"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleProviderDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {Command: "custom-cli"},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/provider/custom"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if _, ok := fs.cfg.Providers["custom"]; ok {
		t.Error("provider 'custom' still exists after delete")
	}
}

func TestHandleProviderDelete_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/provider/nonexistent"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleProviderUpdate_BuiltinConflict(t *testing.T) {
	fs := newFakeMutatorState(t)
	// No city-level "claude" — it's only a builtin.
	h := newTestCityHandler(t, fs)

	body := `{"command":"new-claude"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/provider/claude"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleProviderDelete_BuiltinConflict(t *testing.T) {
	fs := newFakeMutatorState(t)
	// No city-level "claude" — it's only a builtin.
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/provider/claude"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusConflict, w.Body.String())
	}
}
