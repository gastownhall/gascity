package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// The browser-safe provider schema must not advertise a catalog model or
// effort as builtin:codex's default: with no explicit choice Codex runs on
// the user's own configuration, so clients (the dashboard) must show the
// provider default rather than a pinned, possibly unsupported, model.
func TestProviderPublicList_CodexAdvertisesNoModelOrEffortDefault(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Providers["codex"] = config.BuiltinProviderAlias("codex")
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/providers/public"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body ProviderPublicListBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var codex *ProviderPublicResponse
	for i := range body.Items {
		if body.Items[i].Name == "codex" {
			codex = &body.Items[i]
			break
		}
	}
	if codex == nil {
		t.Fatalf("codex missing from public providers: %+v", body.Items)
	}
	for _, key := range []string{"model", "effort"} {
		if v := codex.EffectiveDefaults[key]; v != "" {
			t.Errorf("effective_defaults[%s] = %q, want unset (Codex's own config decides)", key, v)
		}
	}
	if v := codex.EffectiveDefaults["permission_mode"]; v != "unrestricted" {
		t.Errorf("effective_defaults[permission_mode] = %q, want unrestricted", v)
	}
	sawModel := false
	for _, opt := range codex.OptionsSchema {
		if opt.Key != "model" {
			continue
		}
		sawModel = true
		if opt.Default != "" {
			t.Errorf("model option default = %q, want empty", opt.Default)
		}
		if len(opt.Choices) == 0 || opt.Choices[0].Value != "" {
			t.Errorf("model option choices = %+v, want the provider-default (empty) choice first", opt.Choices)
		}
	}
	if !sawModel {
		t.Fatal("codex options_schema has no model option")
	}
}
