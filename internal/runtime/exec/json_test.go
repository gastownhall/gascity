package exec //nolint:revive // internal package, always imported with alias

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestMarshalStartConfig(t *testing.T) {
	cfg := runtime.Config{
		WorkDir:            "/tmp/work",
		Command:            "claude --dangerously-skip-permissions",
		Env:                map[string]string{"FOO": "bar", "BAZ": "qux"},
		StartupEnvelope:    []byte(`{"version":1,"runtime":{"provider":"claude"}}`),
		ProcessNames:       []string{"claude", "node"},
		Nudge:              "hello agent",
		ReadyPromptPrefix:  "> ",
		ReadyDelayMs:       2000,
		SessionSetup:       []string{"echo setup1", "echo setup2"},
		SessionSetupScript: "/tmp/setup.sh",
		OverlayDir:         "/tmp/overlay",
	}

	data, err := marshalStartConfig(cfg)
	if err != nil {
		t.Fatalf("marshalStartConfig: %v", err)
	}

	var got startConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}

	if got.WorkDir != cfg.WorkDir {
		t.Errorf("WorkDir = %q, want %q", got.WorkDir, cfg.WorkDir)
	}
	if got.Command != cfg.Command {
		t.Errorf("Command = %q, want %q", got.Command, cfg.Command)
	}
	if got.Nudge != cfg.Nudge {
		t.Errorf("Nudge = %q, want %q", got.Nudge, cfg.Nudge)
	}
	if len(got.Env) != 2 || got.Env["FOO"] != "bar" || got.Env["BAZ"] != "qux" {
		t.Errorf("Env = %v, want %v", got.Env, cfg.Env)
	}
	if string(got.StartupEnvelope) != string(cfg.StartupEnvelope) {
		t.Errorf("StartupEnvelope = %s, want %s", string(got.StartupEnvelope), string(cfg.StartupEnvelope))
	}
	if len(got.ProcessNames) != 2 || got.ProcessNames[0] != "claude" || got.ProcessNames[1] != "node" {
		t.Errorf("ProcessNames = %v, want %v", got.ProcessNames, cfg.ProcessNames)
	}
	if got.ReadyPromptPrefix != cfg.ReadyPromptPrefix {
		t.Errorf("ReadyPromptPrefix = %q, want %q", got.ReadyPromptPrefix, cfg.ReadyPromptPrefix)
	}
	if got.ReadyDelayMs != cfg.ReadyDelayMs {
		t.Errorf("ReadyDelayMs = %d, want %d", got.ReadyDelayMs, cfg.ReadyDelayMs)
	}
	if len(got.SessionSetup) != 2 || got.SessionSetup[0] != "echo setup1" || got.SessionSetup[1] != "echo setup2" {
		t.Errorf("SessionSetup = %v, want %v", got.SessionSetup, cfg.SessionSetup)
	}
	if got.SessionSetupScript != cfg.SessionSetupScript {
		t.Errorf("SessionSetupScript = %q, want %q", got.SessionSetupScript, cfg.SessionSetupScript)
	}
	if got.OverlayDir != cfg.OverlayDir {
		t.Errorf("OverlayDir = %q, want %q", got.OverlayDir, cfg.OverlayDir)
	}
}

func TestMarshalStartConfig_empty(t *testing.T) {
	data, err := marshalStartConfig(runtime.Config{})
	if err != nil {
		t.Fatalf("marshalStartConfig: %v", err)
	}

	// Empty config should produce minimal JSON (omitempty).
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// All fields have omitempty, so empty config → empty object.
	if len(got) != 0 {
		t.Errorf("expected empty JSON object, got %v", got)
	}
}

func TestMarshalStartConfig_doesNotLeakSessionFields(t *testing.T) {
	// FingerprintExtra and EmitsPermissionWarning are gc-internal.
	// They should NOT appear in the JSON exec protocol.
	// EmitsPermissionWarning is handled in Go (runtime.AcceptStartupDialogs)
	// after the script returns, not passed to the script.
	cfg := runtime.Config{
		Command:                "test",
		FingerprintExtra:       map[string]string{"x": "y"},
		EmitsPermissionWarning: true,
	}

	data, err := marshalStartConfig(cfg)
	if err != nil {
		t.Fatalf("marshalStartConfig: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	leaked := []string{
		"fingerprint_extra",
		"emits_permission_warning",
	}
	for _, key := range leaked {
		if _, ok := got[key]; ok {
			t.Errorf("leaked internal field %q in JSON output", key)
		}
	}
}
