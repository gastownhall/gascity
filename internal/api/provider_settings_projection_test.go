package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

type providerSettingsWire struct {
	Resolution             string            `json:"resolution"`
	Reason                 string            `json:"reason"`
	Scope                  string            `json:"scope"`
	SupportsPerTurnOptions bool              `json:"supports_per_turn_options"`
	ConfiguredDefaults     map[string]string `json:"configured_defaults"`
	OptionsSchema          []struct {
		Key                 string `json:"key"`
		AcceptsCustomValues bool   `json:"accepts_custom_values"`
	} `json:"options_schema"`
}
type agentSettingsWire struct {
	Name            string                `json:"name"`
	ProviderOptions *providerSettingsWire `json:"provider_options"`
}
type publicProviderSettingsWire struct {
	Name                   string            `json:"name"`
	OptionsResolution      string            `json:"options_resolution"`
	OptionsReason          string            `json:"options_reason"`
	OptionsScope           string            `json:"options_scope"`
	SupportsPerTurnOptions bool              `json:"supports_per_turn_options"`
	EffectiveDefaults      map[string]string `json:"effective_defaults"`
	OptionsSchema          []struct {
		Key                 string `json:"key"`
		AcceptsCustomValues bool   `json:"accepts_custom_values"`
	} `json:"options_schema"`
}

func settingsProjectionGet(t *testing.T, fs *fakeState, path string, dest any) string {
	t.Helper()
	rec := httptest.NewRecorder()
	newTestCityHandler(t, fs).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, path), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s=%d %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), dest); err != nil {
		t.Fatal(err)
	}
	return rec.Body.String()
}

func settingsProjectionFixture(t *testing.T) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	base := config.BuiltinProviderAlias("claude")
	base.OptionDefaults = map[string]string{"model": "configured-provider-model", "effort": "low"}
	parent := "provider:parent"
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"parent": base,
		"child":  {Base: &parent},
	}
	fs.cfg.Workspace.Provider = "child"
	fs.cfg.Agents = []config.Agent{{
		Name: "worker", Dir: "myrig", Provider: "child",
		OptionDefaults: map[string]string{"model": "configured-agent-model", "effort": "medium"},
		Args:           []string{"--effort", "high"},
	}}
	return fs
}

func TestProviderSettingsAgentProjectionUsesResolvedAgentDefaults(t *testing.T) {
	fs := settingsProjectionFixture(t)
	var one agentSettingsWire
	body := settingsProjectionGet(t, fs, "/agent/myrig/worker", &one)
	if one.ProviderOptions == nil {
		t.Fatalf("agent-resolved settings missing: %s", body)
	}
	got := one.ProviderOptions
	if !strings.Contains(body, "\"supports_per_turn_options\":false") {
		t.Fatal("unsupported per-turn options must be explicit on the wire")
	}
	if got.Resolution != "resolved" || got.Scope != "session_start" || got.SupportsPerTurnOptions ||
		got.ConfiguredDefaults["model"] != "configured-agent-model" || got.ConfiguredDefaults["effort"] != "high" {
		t.Fatalf("wrong configured session-start settings: %+v", got)
	}
	var list struct {
		Items []agentSettingsWire `json:"items"`
	}
	settingsProjectionGet(t, fs, "/agents", &list)
	if len(list.Items) != 1 || list.Items[0].ProviderOptions == nil ||
		list.Items[0].ProviderOptions.ConfiguredDefaults["model"] != "configured-agent-model" ||
		list.Items[0].ProviderOptions.ConfiguredDefaults["effort"] != "high" {
		t.Fatalf("agent list/get disagree: %+v", list)
	}
	if fs.cfg.Agents[0].OptionDefaults["model"] != "configured-agent-model" ||
		strings.Join(fs.cfg.Agents[0].Args, " ") != "--effort high" {
		t.Fatal("discovery mutated authored settings")
	}
}

func TestProviderSettingsPublicProjectionResolvesFullChainAndValidation(t *testing.T) {
	fs := settingsProjectionFixture(t)
	fs.cfg.Providers["parent"] = func() config.ProviderSpec {
		p := fs.cfg.Providers["parent"]
		p.Env = map[string]string{"PRIVATE_CREDENTIAL": "DO_NOT_DISCLOSE_THIS_VALUE"}
		return p
	}()
	var body struct {
		Items []publicProviderSettingsWire `json:"items"`
	}
	raw := settingsProjectionGet(t, fs, "/providers/public", &body)
	var child *publicProviderSettingsWire
	for i := range body.Items {
		if body.Items[i].Name == "child" {
			child = &body.Items[i]
		}
	}
	if child == nil || child.OptionsResolution != "resolved" || child.OptionsScope != "session_start" ||
		child.SupportsPerTurnOptions || child.EffectiveDefaults["model"] != "configured-provider-model" ||
		child.EffectiveDefaults["effort"] != "low" {
		t.Fatalf("provider inheritance/defaults missing: %+v", child)
	}
	if !strings.Contains(raw, "\"supports_per_turn_options\":false") ||
		!strings.Contains(raw, "\"accepts_custom_values\":false") {
		t.Fatal("unsupported/closed option behavior must be explicit on the wire")
	}
	seenOpen, seenClosed, seenEffortOpen := false, false, false
	for _, opt := range child.OptionsSchema {
		if opt.Key == "model" {
			seenOpen = opt.AcceptsCustomValues
		}
		if opt.Key == "permission_mode" {
			seenClosed = !opt.AcceptsCustomValues
		}
		if opt.Key == "effort" {
			seenEffortOpen = opt.AcceptsCustomValues
		}
	}
	if !seenOpen || !seenClosed || !seenEffortOpen {
		t.Fatalf("missing open/closed validation semantics: %+v", child.OptionsSchema)
	}
	for _, private := range []string{"DO_NOT_DISCLOSE_THIS_VALUE", "flag_args", "flag_template", "acp_command", "\"command\""} {
		if strings.Contains(raw, private) {
			t.Fatalf("provider discovery exposed %q", private)
		}
	}
}

func TestProviderSettingsCustomLaunchRemainsUnknown(t *testing.T) {
	for _, kind := range []string{"agent command", "workspace command", "opaque agent arguments", "noncurated model arguments"} {
		t.Run(kind, func(t *testing.T) {
			fs := settingsProjectionFixture(t)
			fs.cfg.Agents[0].Args = nil
			reason := "custom_start_command"
			switch kind {
			case "agent command":
				fs.cfg.Agents[0].StartCommand = "private-wrapper --token NEVER_EXPOSE_THIS"
			case "workspace command":
				fs.cfg.Workspace.StartCommand = "private-wrapper --token NEVER_EXPOSE_THIS"
			case "opaque agent arguments":
				fs.cfg.Agents[0].Args = []string{"--profile", "NEVER_EXPOSE_THIS"}
				reason = "custom_arguments"
			case "noncurated model arguments":
				// Open option pins are supported, but legacy raw-argument
				// normalization does not infer values outside curated flags.
				// Do not guess how duplicate CLI/default flags will resolve.
				fs.cfg.Agents[0].Args = []string{"--model", "NEVER_EXPOSE_THIS"}
				reason = "custom_arguments"
			}
			var body agentSettingsWire
			raw := settingsProjectionGet(t, fs, "/agent/myrig/worker", &body)
			if body.ProviderOptions == nil || body.ProviderOptions.Resolution != "unknown" ||
				body.ProviderOptions.Reason != reason || len(body.ProviderOptions.ConfiguredDefaults) != 0 ||
				body.ProviderOptions.SupportsPerTurnOptions {
				t.Fatalf("opaque launch guessed actual options: %+v", body.ProviderOptions)
			}
			if strings.Contains(raw, "NEVER_EXPOSE_THIS") {
				t.Fatal("launch details leaked")
			}
		})
	}
}

func TestProviderSettingsUnhonoredPinsAndFailedResolutionDoNotAdvertiseDefaults(t *testing.T) {
	for _, kind := range []string{"unhonored defaults", "unknown provider"} {
		t.Run(kind, func(t *testing.T) {
			fs := settingsProjectionFixture(t)
			wantReason := "unhonored_defaults"
			if kind == "unhonored defaults" {
				fs.cfg.Agents[0].OptionDefaults["permission_mode"] = "not-a-mode"
			} else {
				fs.cfg.Agents[0].Provider = "absent-provider"
				wantReason = "resolution_failed"
			}
			var body agentSettingsWire
			settingsProjectionGet(t, fs, "/agent/myrig/worker", &body)
			if body.ProviderOptions == nil || body.ProviderOptions.Resolution != "unknown" ||
				body.ProviderOptions.Reason != wantReason || len(body.ProviderOptions.ConfiguredDefaults) != 0 {
				t.Fatalf("unresolvable settings advertised as defaults: %+v", body.ProviderOptions)
			}
		})
	}
}
