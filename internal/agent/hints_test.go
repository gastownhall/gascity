package agent

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestStartupHintsToRuntimeConfigPropagatesEveryField is the anti-omission
// guard for gc-0tna7 / gc-wuofg. It reflectively sets every StartupHints field
// to a non-zero value and asserts ToRuntimeConfig copies each one to the
// same-named runtime.Config field. When StartupHints grows a field and the
// projection is not updated, this fails — instead of the field silently
// vanishing from every consumer (the SessionLive and MouseOn regressions that
// motivated this work).
func TestStartupHintsToRuntimeConfigPropagatesEveryField(t *testing.T) {
	var hints StartupHints
	hv := reflect.ValueOf(&hints).Elem()
	for i := 0; i < hv.NumField(); i++ {
		setNonZero(t, hv.Type().Field(i).Name, hv.Field(i))
	}

	cfg := hints.ToRuntimeConfig()
	cv := reflect.ValueOf(cfg)

	for i := 0; i < hv.NumField(); i++ {
		name := hv.Type().Field(i).Name
		field := cv.FieldByName(name)
		if !field.IsValid() {
			t.Errorf("runtime.Config has no field %q to receive StartupHints.%s", name, name)
			continue
		}
		if field.IsZero() {
			t.Errorf("ToRuntimeConfig did not propagate StartupHints.%s (runtime.Config.%s is zero)", name, name)
		}
	}
}

// setNonZero assigns a distinctive non-zero value to v based on its kind,
// covering every field kind present in StartupHints (string/defined-string,
// int, bool, *bool, and slices).
func setNonZero(t *testing.T, name string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Int:
		v.SetInt(1)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	default:
		t.Fatalf("setNonZero: unhandled kind %s for StartupHints.%s — extend this helper", v.Kind(), name)
	}
}

// TestStartupHintsToRuntimeConfigCopiesValues documents the concrete mapping
// with readable, explicit values (complementing the reflective totality guard).
func TestStartupHintsToRuntimeConfigCopiesValues(t *testing.T) {
	accept := true
	hints := StartupHints{
		Lifecycle:              "oneshot",
		ReadyPromptPrefix:      "> ",
		ReadyDelayMs:           250,
		ProcessNames:           []string{"agent-cli"},
		EmitsPermissionWarning: true,
		AcceptStartupDialogs:   &accept,
		MouseOn:                true,
		Nudge:                  "go",
		PreStart:               []string{"mkdir -p x"},
		SessionSetup:           []string{"echo setup"},
		SessionSetupScript:     "/tmp/setup.sh",
		SessionLive:            []string{"tmux set -g status on"},
		ProviderName:           "claude",
		ProviderOverlayName:    "claude-custom",
		InstallAgentHooks:      []string{"gemini"},
		PackOverlayDirs:        []string{"/pack/overlay"},
		OverlayDir:             "/agent/overlay",
		CopyFiles:              make([]runtime.CopyEntry, 1),
	}

	cfg := hints.ToRuntimeConfig()

	if string(cfg.Lifecycle) != "oneshot" {
		t.Errorf("Lifecycle = %q, want oneshot", cfg.Lifecycle)
	}
	if cfg.ReadyPromptPrefix != "> " {
		t.Errorf("ReadyPromptPrefix = %q", cfg.ReadyPromptPrefix)
	}
	if cfg.ReadyDelayMs != 250 {
		t.Errorf("ReadyDelayMs = %d", cfg.ReadyDelayMs)
	}
	if len(cfg.ProcessNames) != 1 || cfg.ProcessNames[0] != "agent-cli" {
		t.Errorf("ProcessNames = %v", cfg.ProcessNames)
	}
	if !cfg.EmitsPermissionWarning {
		t.Error("EmitsPermissionWarning = false, want true")
	}
	if cfg.AcceptStartupDialogs == nil || !*cfg.AcceptStartupDialogs {
		t.Errorf("AcceptStartupDialogs = %v, want ptr to true", cfg.AcceptStartupDialogs)
	}
	if !cfg.MouseOn {
		t.Error("MouseOn = false, want true")
	}
	if cfg.Nudge != "go" {
		t.Errorf("Nudge = %q", cfg.Nudge)
	}
	if len(cfg.PreStart) != 1 || cfg.PreStart[0] != "mkdir -p x" {
		t.Errorf("PreStart = %v", cfg.PreStart)
	}
	if len(cfg.SessionSetup) != 1 || cfg.SessionSetup[0] != "echo setup" {
		t.Errorf("SessionSetup = %v", cfg.SessionSetup)
	}
	if cfg.SessionSetupScript != "/tmp/setup.sh" {
		t.Errorf("SessionSetupScript = %q", cfg.SessionSetupScript)
	}
	if len(cfg.SessionLive) != 1 || cfg.SessionLive[0] != "tmux set -g status on" {
		t.Errorf("SessionLive = %v", cfg.SessionLive)
	}
	if cfg.ProviderName != "claude" {
		t.Errorf("ProviderName = %q", cfg.ProviderName)
	}
	if cfg.ProviderOverlayName != "claude-custom" {
		t.Errorf("ProviderOverlayName = %q", cfg.ProviderOverlayName)
	}
	if len(cfg.InstallAgentHooks) != 1 || cfg.InstallAgentHooks[0] != "gemini" {
		t.Errorf("InstallAgentHooks = %v", cfg.InstallAgentHooks)
	}
	if len(cfg.PackOverlayDirs) != 1 || cfg.PackOverlayDirs[0] != "/pack/overlay" {
		t.Errorf("PackOverlayDirs = %v", cfg.PackOverlayDirs)
	}
	if cfg.OverlayDir != "/agent/overlay" {
		t.Errorf("OverlayDir = %q", cfg.OverlayDir)
	}
	if len(cfg.CopyFiles) != 1 {
		t.Errorf("CopyFiles len = %d, want 1", len(cfg.CopyFiles))
	}
}
