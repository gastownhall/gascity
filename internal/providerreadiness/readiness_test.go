package providerreadiness

import (
	"context"
	"errors"
	"reflect"
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
			if command != "/selected/codex" || !reflect.DeepEqual(args, []string{"login", "status"}) || env["CODEX_HOME"] != "/selected/home" {
				t.Fatalf("wrong Codex context: %s %v", command, args)
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
			runStatus = func(context.Context, string, []string, map[string]string) (statusOutput, error) {
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
