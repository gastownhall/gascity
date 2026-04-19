package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestResolvedSessionConfigForProviderBuildsNormalizedConfig(t *testing.T) {
	metadata := map[string]string{"session_origin": "named"}
	env := map[string]string{"API_TOKEN": "present"}
	resolved := &config.ResolvedProvider{
		Name:                   "stub",
		Command:                "/bin/echo",
		ReadyPromptPrefix:      "stub-ready>",
		ReadyDelayMs:           250,
		ProcessNames:           []string{"echo"},
		EmitsPermissionWarning: true,
		Env:                    env,
		ResumeFlag:             "--resume",
		ResumeStyle:            "flag",
		ResumeCommand:          "resume-cmd",
		SessionIDFlag:          "--session-id",
	}

	cfg, err := resolvedSessionConfigForProvider(
		"worker",
		"worker-named",
		"myrig/worker",
		"Worker Named",
		"acp",
		metadata,
		resolved,
		"",
		"/tmp/workdir",
	)
	if err != nil {
		t.Fatalf("resolvedSessionConfigForProvider: %v", err)
	}

	if got, want := cfg.Runtime.Command, "/bin/echo"; got != want {
		t.Fatalf("Runtime.Command = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Provider, "stub"; got != want {
		t.Fatalf("Runtime.Provider = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.WorkDir, "/tmp/workdir"; got != want {
		t.Fatalf("Runtime.WorkDir = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Hints.WorkDir, "/tmp/workdir"; got != want {
		t.Fatalf("Runtime.Hints.WorkDir = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Hints.ReadyPromptPrefix, "stub-ready>"; got != want {
		t.Fatalf("Runtime.Hints.ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Resume.SessionIDFlag, "--session-id"; got != want {
		t.Fatalf("Runtime.Resume.SessionIDFlag = %q, want %q", got, want)
	}

	metadata["session_origin"] = "mutated"
	env["API_TOKEN"] = "mutated"
	if got, want := cfg.Metadata["session_origin"], "named"; got != want {
		t.Fatalf("Metadata[session_origin] = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.SessionEnv["API_TOKEN"], "present"; got != want {
		t.Fatalf("Runtime.SessionEnv[API_TOKEN] = %q, want %q", got, want)
	}
}

func TestResolvedSessionConfigForProviderRejectsNilProvider(t *testing.T) {
	if _, err := resolvedSessionConfigForProvider(
		"worker",
		"",
		"myrig/worker",
		"Worker",
		"",
		nil,
		nil,
		"",
		"/tmp/workdir",
	); err == nil {
		t.Fatal("resolvedSessionConfigForProvider() error = nil, want error")
	}
}
