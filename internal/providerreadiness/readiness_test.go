package providerreadiness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestProbeClaudeUsesConfiguredRuntimeAndAllowsSupportedModes(t *testing.T) {
	old := runStatus
	t.Cleanup(func() { runStatus = old })
	for _, mode := range []string{"claude.ai", "oauth_token", "api_key", "bedrock", "vertex"} {
		t.Run(mode, func(t *testing.T) {
			called := false
			runStatus = func(ctx context.Context, command string, args []string, env map[string]string) (statusOutput, error) {
				called = true
				if command != "/selected/claude-alias" || !reflect.DeepEqual(args, []string{"auth", "status", "--json"}) {
					t.Fatalf("wrong runtime: %s %v", command, args)
				}
				if env["CLAUDE_CONFIG_DIR"] != "/selected/config" || env["ANTHROPIC_API_KEY"] != "test-key" {
					t.Fatal("configured credential environment lost")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 6*time.Second {
					t.Fatal("probe has no bounded deadline")
				}
				return statusOutput{stdout: []byte(`{"loggedIn":true,"authMethod":"` + mode + `","apiProvider":"custom"}`)}, nil
			}
			result := Probe(context.Background(), Target{Family: "claude", Command: "/selected/claude-alias", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/selected/config", "ANTHROPIC_API_KEY": "test-key"}})
			if !called || result.Status != Configured {
				t.Fatalf("result=%+v called=%t", result, called)
			}
		})
	}
}

func TestProbeCodexUsesNativeLoginStatusAndConfiguredHome(t *testing.T) {
	old := runStatus
	t.Cleanup(func() { runStatus = old })
	for _, output := range []string{"Logged in using ChatGPT", "Logged in using an API key - test", "Warning: optional PATH update failed\nLogged in using ChatGPT"} {
		runStatus = func(_ context.Context, command string, args []string, env map[string]string) (statusOutput, error) {
			if command != "/selected/codex" || env["CODEX_HOME"] != "/selected/home" {
				t.Fatalf("wrong Codex context: %s %v", command, args)
			}
			if reflect.DeepEqual(args, []string{"--version"}) {
				return statusOutput{stdout: []byte("codex-cli 0.162.1\n")}, nil
			}
			if !reflect.DeepEqual(args, []string{"login", "status"}) {
				t.Fatalf("wrong Codex status arguments: %v", args)
			}
			return statusOutput{stderr: []byte(output)}, nil
		}
		if result := Probe(context.Background(), Target{Family: "codex", Command: "/selected/codex", Env: map[string]string{"CODEX_HOME": "/selected/home"}}); result.Status != Configured {
			t.Fatalf("result=%+v", result)
		}
	}
}

func TestProbeNeverTurnsMissingOrMalformedStateIntoReady(t *testing.T) {
	old := runStatus
	t.Cleanup(func() { runStatus = old })
	for _, tc := range []struct {
		name, family, output, want string
		err                        error
	}{
		{name: "Claude logged out", family: "claude", output: `{"loggedIn":false}`, want: NeedsAuth, err: errors.New("exit 1")},
		{name: "Claude missing verdict", family: "claude", output: `{}`, want: ProbeError},
		{name: "Claude malformed", family: "claude", output: `oops test-secret`, want: ProbeError},
		{name: "Codex logged out", family: "codex", output: "Not logged in", want: NeedsAuth, err: errors.New("exit 1")},
		{name: "Codex crashed", family: "codex", output: "test-secret", want: ProbeError, err: errors.New("exit 9")},
		{name: "Codex empty", family: "codex", want: ProbeError},
		{name: "Codex conflicting verdicts", family: "codex", output: "Logged in using ChatGPT\nNot logged in", want: ProbeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runStatus = func(_ context.Context, _ string, args []string, _ map[string]string) (statusOutput, error) {
				if tc.family == "codex" && reflect.DeepEqual(args, []string{"--version"}) {
					return statusOutput{stdout: []byte("codex-cli 0.162.1\n")}, nil
				}
				return statusOutput{stdout: []byte(tc.output)}, tc.err
			}
			result := Probe(context.Background(), Target{Family: tc.family, Command: "/selected/runtime"})
			if result.Status != tc.want || strings.Contains(result.Detail, "test-secret") {
				t.Fatalf("result=%+v want=%s", result, tc.want)
			}
		})
	}
}

func TestProbeDoesNotGuessProtocolsForStandaloneOrWrappedProviders(t *testing.T) {
	old := runStatus
	t.Cleanup(func() { runStatus = old })
	runStatus = func(context.Context, string, []string, map[string]string) (statusOutput, error) {
		t.Fatal("unexpected provider execution")
		return statusOutput{}, nil
	}
	for _, target := range []Target{{Command: "claude"}, {Family: "claude", Command: "aimux run claude"}, {Family: "other", Command: "other"}, {Family: "codex", Command: "codex", Args: []string{"--config=model_provider=local"}}} {
		result := Probe(context.Background(), target)
		if result.Status != Unverified || result.Detail == "" {
			t.Fatalf("result=%+v", result)
		}
	}
}

func TestProbeRespectsCallerCancellation(t *testing.T) {
	old := runStatus
	t.Cleanup(func() { runStatus = old })
	runStatus = func(context.Context, string, []string, map[string]string) (statusOutput, error) {
		t.Fatal("canceled caller started a command")
		return statusOutput{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := Probe(ctx, Target{Family: "claude", Command: "claude"})
	if result.Status != ProbeError {
		t.Fatalf("result=%+v", result)
	}
}

// The provider's PATH can differ from the supervisor's. A native readiness
// check must execute the same selected CLI, including an explicitly empty PATH.
func TestProbeUsesConfiguredExecutableSearchPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native shell fixture for supported Unix providers")
	}
	directory := t.TempDir()
	executable := filepath.Join(directory, "sh")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' '{\"loggedIn\":true}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := Target{Family: "claude", Command: "sh", Env: map[string]string{"PATH": directory}}
	if result := Probe(context.Background(), target); result.Status != Configured {
		t.Fatalf("selected PATH runtime: %+v", result)
	}
	target.Env["PATH"] = ""
	if result := Probe(context.Background(), target); result.Status != NotInstalled {
		t.Fatalf("empty configured PATH must not use the supervisor PATH: %+v", result)
	}
}

func TestProbeCodexMinimumVersionBeforeAuthWithOriginalDeadline(t *testing.T) {
	old := runStatus
	t.Cleanup(func() { runStatus = old })
	for _, test := range []struct {
		name, version string
		wantAuth      bool
	}{
		{name: "qualified", version: "codex-cli 0.162.1", wantAuth: true},
		{name: "newer", version: "codex-cli 0.166.0", wantAuth: true},
		{name: "older patch", version: "codex-cli 0.162.0"},
		{name: "old CLI", version: "codex-cli 0.149.0"},
		{name: "prerelease", version: "codex-cli 0.162.1-alpha.1"},
		{name: "wrong program", version: "node 0.166.0"},
		{name: "conflicting", version: "codex-cli 0.162.1\ncodex-cli 0.149.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var commands [][]string
			var originalDeadline time.Time
			runStatus = func(ctx context.Context, command string, args []string, env map[string]string) (statusOutput, error) {
				if command != "/selected/codex" || env["CODEX_HOME"] != "/selected/home" {
					t.Fatalf("selected runtime context lost: %s %v", command, env)
				}
				deadline, bounded := ctx.Deadline()
				if !bounded || time.Until(deadline) > 5*time.Second {
					t.Fatal("version/auth probes need one existing five-second budget")
				}
				if originalDeadline.IsZero() {
					originalDeadline = deadline
				} else if deadline != originalDeadline {
					t.Fatal("auth reset the version-check deadline")
				}
				commands = append(commands, append([]string(nil), args...))
				if reflect.DeepEqual(args, []string{"--version"}) {
					return statusOutput{stdout: []byte(test.version)}, nil
				}
				if !test.wantAuth || !reflect.DeepEqual(args, []string{"login", "status"}) {
					t.Fatalf("unexpected post-version command: %v", args)
				}
				return statusOutput{stderr: []byte("Logged in using ChatGPT")}, nil
			}
			result := Probe(context.Background(), Target{Family: "codex", Command: "/selected/codex", Args: []string{"--no-daemon"}, Env: map[string]string{"CODEX_HOME": "/selected/home"}})
			if len(commands) == 0 || !reflect.DeepEqual(commands[0], []string{"--version"}) {
				t.Fatalf("version must precede unsupported launch flags/auth: %v", commands)
			}
			if test.wantAuth {
				if result.Status != Configured || len(commands) != 2 {
					t.Fatalf("qualified result=%+v calls=%v", result, commands)
				}
			} else if result.Status != ProbeError || len(commands) != 1 {
				t.Fatalf("unsupported version result=%+v calls=%v", result, commands)
			}
		})
	}
}

func TestProbeCodexVersionFailureNeverLeaksOutputOrCallsAuth(t *testing.T) {
	old := runStatus
	t.Cleanup(func() { runStatus = old })
	calls := 0
	runStatus = func(_ context.Context, _ string, args []string, _ map[string]string) (statusOutput, error) {
		calls++
		if !reflect.DeepEqual(args, []string{"--version"}) {
			t.Fatalf("auth ran after failed version check: %v", args)
		}
		return statusOutput{stdout: []byte("test-secret")}, errors.New("test-secret native failure")
	}
	result := Probe(context.Background(), Target{Family: "codex", Command: "/selected/codex"})
	if calls != 1 || result.Status != ProbeError || strings.Contains(result.Detail, "test-secret") || !strings.Contains(result.Detail, "0.162.1") {
		t.Fatalf("version failure lost privacy/upgrade detail: calls=%d result=%+v", calls, result)
	}
}
