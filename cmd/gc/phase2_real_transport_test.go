//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
	workertest "github.com/gastownhall/gascity/internal/worker/workertest"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

const (
	phase2RealTransportBound       = 5 * time.Second
	phase2RealTransportMarkerBound = 500 * time.Millisecond
)

func TestPhase2WorkerCoreRealTransportProof(t *testing.T) {
	tmuxtest.RequireTmux(t)
	reporter := newPhase2Reporter(t, "phase2-real-transport")

	for _, tc := range selectedPhase2ProviderCases(t) {
		tc := tc
		t.Run(string(tc.profileID), func(t *testing.T) {
			tp := resolvePhase2Template(t, tc)
			materialized := templateParamsToConfig(tp)
			reporter.Require(t, startupRuntimeConfigMaterializationResult(tc, tp, materialized))

			run := launchPhase2RealTransportSession(t, tc, materialized)
			reporter.Require(t, phase2RealTransportResult(tc, run))
		})
	}
}

func TestPhase2HookEnabledClaudeLaunchPromptDeliveryProof(t *testing.T) {
	tmuxtest.RequireTmux(t)

	tc := phase2ProviderCaseForFamily(t, "claude")
	tp := resolvePhase2Template(t, tc)
	if !tp.HookEnabled {
		t.Fatal("HookEnabled = false, want true for Claude phase2 profile")
	}
	if tp.ResolvedProvider == nil {
		t.Fatal("ResolvedProvider = nil, want Claude provider metadata")
	}
	if !tp.ResolvedProvider.SupportsHooks {
		t.Fatal("SupportsHooks = false, want true for Claude phase2 profile")
	}

	materialized := templateParamsToConfig(tp)
	run := launchPhase2RealTransportSession(t, tc, materialized)

	if run.ErrorStage != "" {
		t.Fatalf("%s failed: %s", run.ErrorStage, run.Error)
	}
	if got, want := run.ObservedStartupPrompt, run.ExpectedStartupPrompt; got != want {
		t.Fatalf("startup prompt = %q, want %q", got, want)
	}
	if !run.AutonomousStarted {
		t.Fatal("launch prompt marker = missing, want command-line startup prompt matched before any explicit rescue nudge")
	}
}

type phase2RealTransportRun struct {
	Transport                string
	SocketName               string
	SessionName              string
	ProviderPath             string
	StartedPath              string
	InputPath                string
	SessionOriginPath        string
	StartupDeliveredPath     string
	StartupPromptPath        string
	AutonomousPath           string
	ErrorStage               string
	Error                    string
	ExpectedInput            string
	ObservedInput            string
	ExpectedSessionOrigin    string
	ObservedSessionOrigin    string
	ExpectedStartupDelivered string
	ObservedStartupDelivered string
	ExpectedStartupPrompt    string
	ObservedStartupPrompt    string
	ObservedProvider         string
	Started                  bool
	AutonomousStarted        bool
	RunningAfterInput        bool
	StartElapsed             time.Duration
}

func launchPhase2RealTransportSession(t *testing.T, tc phase2ProviderCase, materialized runtime.Config) phase2RealTransportRun {
	t.Helper()

	guard := tmuxtest.NewGuard(t)
	dir := t.TempDir()
	startedPath := filepath.Join(dir, "started.txt")
	providerPath := filepath.Join(dir, "provider.txt")
	inputPath := filepath.Join(dir, "input.txt")
	sessionOriginPath := filepath.Join(dir, "session-origin.txt")
	startupDeliveredPath := filepath.Join(dir, "startup-delivered.txt")
	startupPromptPath := filepath.Join(dir, "startup-prompt.txt")
	expectedPromptPath := filepath.Join(dir, "expected-startup-prompt.txt")
	autonomousPath := filepath.Join(dir, "autonomous.txt")
	stopPath := filepath.Join(dir, "stop.txt")
	sessionName := guard.SessionName("phase2-" + tc.family)
	expectedStartupPrompt, promptErr := singleShellArgValue(materialized.PromptSuffix)
	if promptErr != nil {
		return phase2RealTransportRun{
			Transport:                "tmux",
			SocketName:               guard.SocketName(),
			SessionName:              sessionName,
			ProviderPath:             providerPath,
			StartedPath:              startedPath,
			InputPath:                inputPath,
			SessionOriginPath:        sessionOriginPath,
			StartupDeliveredPath:     startupDeliveredPath,
			StartupPromptPath:        startupPromptPath,
			AutonomousPath:           autonomousPath,
			ErrorStage:               "prompt_suffix_parse",
			Error:                    promptErr.Error(),
			ExpectedInput:            materialized.Nudge,
			ExpectedSessionOrigin:    materialized.Env["GC_SESSION_ORIGIN"],
			ExpectedStartupDelivered: materialized.Env[startupPromptDeliveredEnv],
			ExpectedStartupPrompt:    expectedStartupPrompt,
		}
	}
	if err := os.WriteFile(expectedPromptPath, []byte(expectedStartupPrompt), 0o644); err != nil {
		return phase2RealTransportRun{
			Transport:                "tmux",
			SocketName:               guard.SocketName(),
			SessionName:              sessionName,
			ProviderPath:             providerPath,
			StartedPath:              startedPath,
			InputPath:                inputPath,
			SessionOriginPath:        sessionOriginPath,
			StartupDeliveredPath:     startupDeliveredPath,
			StartupPromptPath:        startupPromptPath,
			AutonomousPath:           autonomousPath,
			ErrorStage:               "expected_prompt_write",
			Error:                    err.Error(),
			ExpectedInput:            materialized.Nudge,
			ExpectedSessionOrigin:    materialized.Env["GC_SESSION_ORIGIN"],
			ExpectedStartupDelivered: materialized.Env[startupPromptDeliveredEnv],
			ExpectedStartupPrompt:    expectedStartupPrompt,
		}
	}

	sp, err := newSessionProviderByName("", config.SessionConfig{
		Socket:             guard.SocketName(),
		SetupTimeout:       "3s",
		NudgeReadyTimeout:  "2s",
		NudgeRetryInterval: "50ms",
		NudgeLockTimeout:   "2s",
	}, guard.CityName(), dir)
	if err != nil {
		return phase2RealTransportRun{
			Transport:                "tmux",
			SocketName:               guard.SocketName(),
			SessionName:              sessionName,
			ProviderPath:             providerPath,
			StartedPath:              startedPath,
			InputPath:                inputPath,
			SessionOriginPath:        sessionOriginPath,
			StartupDeliveredPath:     startupDeliveredPath,
			StartupPromptPath:        startupPromptPath,
			AutonomousPath:           autonomousPath,
			ErrorStage:               "provider",
			Error:                    err.Error(),
			ExpectedInput:            materialized.Nudge,
			ExpectedSessionOrigin:    materialized.Env["GC_SESSION_ORIGIN"],
			ExpectedStartupDelivered: materialized.Env[startupPromptDeliveredEnv],
			ExpectedStartupPrompt:    expectedStartupPrompt,
		}
	}

	t.Cleanup(func() {
		_ = os.WriteFile(stopPath, []byte("stop\n"), 0o644)
		_ = sp.Stop(sessionName)
	})

	script := strings.Join([]string{
		`set -eu`,
		`printf "%s\n" "$GC_PROVIDER" > "$GC_REAL_TRANSPORT_PROVIDER_PATH"`,
		`printf "%s" "${GC_SESSION_ORIGIN:-}" > "$GC_REAL_TRANSPORT_SESSION_ORIGIN_PATH"`,
		`printf "%s" "${GC_STARTUP_PROMPT_DELIVERED:-}" > "$GC_REAL_TRANSPORT_STARTUP_DELIVERED_PATH"`,
		`printf "started\n" > "$GC_REAL_TRANSPORT_STARTED_PATH"`,
		`if [ -n "${GC_REAL_TRANSPORT_STARTUP_PROMPT_FLAG:-}" ]; then printf "%s" "${1:-}" > "$GC_REAL_TRANSPORT_STARTUP_PROMPT_PATH"; else printf "%s" "$0" > "$GC_REAL_TRANSPORT_STARTUP_PROMPT_PATH"; fi`,
		`if cmp -s "$GC_REAL_TRANSPORT_STARTUP_PROMPT_PATH" "$GC_REAL_TRANSPORT_EXPECTED_PROMPT_PATH"; then printf "launch-prompt\n" > "$GC_REAL_TRANSPORT_AUTONOMOUS_PATH"; fi`,
		`IFS= read -r line`,
		`printf "%s\n" "$line" > "$GC_REAL_TRANSPORT_INPUT_PATH"`,
		`while [ ! -f "$GC_REAL_TRANSPORT_STOP_PATH" ]; do sleep 0.05; done`,
	}, "; ")

	cfg := materialized
	cfg.WorkDir = dir
	cfg.Command = "sh -c " + shellquote.Quote(script)
	cfg.ReadyPromptPrefix = ""
	cfg.ReadyDelayMs = 100
	cfg.ProcessNames = nil
	cfg.EmitsPermissionWarning = false
	cfg.PreStart = nil
	cfg.SessionSetup = nil
	cfg.SessionSetupScript = ""
	cfg.SessionLive = nil
	cfg.Env = copyRuntimeEnv(materialized.Env)
	cfg.Env["GC_DIR"] = dir
	cfg.Env["GC_PROVIDER"] = tc.family
	cfg.Env["GC_REAL_TRANSPORT_PROVIDER_PATH"] = providerPath
	cfg.Env["GC_REAL_TRANSPORT_STARTED_PATH"] = startedPath
	cfg.Env["GC_REAL_TRANSPORT_INPUT_PATH"] = inputPath
	cfg.Env["GC_REAL_TRANSPORT_SESSION_ORIGIN_PATH"] = sessionOriginPath
	cfg.Env["GC_REAL_TRANSPORT_STARTUP_DELIVERED_PATH"] = startupDeliveredPath
	cfg.Env["GC_REAL_TRANSPORT_STARTUP_PROMPT_PATH"] = startupPromptPath
	cfg.Env["GC_REAL_TRANSPORT_EXPECTED_PROMPT_PATH"] = expectedPromptPath
	cfg.Env["GC_REAL_TRANSPORT_AUTONOMOUS_PATH"] = autonomousPath
	cfg.Env["GC_REAL_TRANSPORT_STOP_PATH"] = stopPath
	if cfg.PromptFlag != "" {
		cfg.Env["GC_REAL_TRANSPORT_STARTUP_PROMPT_FLAG"] = cfg.PromptFlag
	}

	ctx, cancel := context.WithTimeout(context.Background(), phase2RealTransportBound)
	defer cancel()

	start := time.Now()
	if err := sp.Start(ctx, sessionName, cfg); err != nil {
		return phase2RealTransportRun{
			Transport:                "tmux",
			SocketName:               guard.SocketName(),
			SessionName:              sessionName,
			ProviderPath:             providerPath,
			StartedPath:              startedPath,
			InputPath:                inputPath,
			SessionOriginPath:        sessionOriginPath,
			StartupDeliveredPath:     startupDeliveredPath,
			StartupPromptPath:        startupPromptPath,
			AutonomousPath:           autonomousPath,
			ErrorStage:               "start",
			Error:                    err.Error(),
			ExpectedInput:            materialized.Nudge,
			ExpectedSessionOrigin:    materialized.Env["GC_SESSION_ORIGIN"],
			ExpectedStartupDelivered: materialized.Env[startupPromptDeliveredEnv],
			ExpectedStartupPrompt:    expectedStartupPrompt,
			StartElapsed:             time.Since(start),
		}
	}
	startElapsed := time.Since(start)

	observedStartupPrompt, startupPromptErr := waitForPhase2FileText(startupPromptPath, phase2RealTransportBound)
	autonomousStarted := waitForPhase2FileExists(autonomousPath, phase2RealTransportMarkerBound)
	observedInput, inputErr := waitForPhase2FileText(inputPath, phase2RealTransportBound)
	observedProvider, providerErr := waitForPhase2FileText(providerPath, phase2RealTransportBound)
	observedSessionOrigin, sessionOriginErr := waitForPhase2FileText(sessionOriginPath, phase2RealTransportBound)
	observedStartupDelivered, startupDeliveredErr := waitForPhase2FileText(startupDeliveredPath, phase2RealTransportBound)
	_, startedErr := os.Stat(startedPath)

	errorStage := ""
	errorDetail := ""
	switch {
	case startupPromptErr != nil:
		errorStage = "startup_prompt_wait"
		errorDetail = startupPromptErr.Error()
	case inputErr != nil:
		errorStage = "input_wait"
		errorDetail = inputErr.Error()
	case providerErr != nil:
		errorStage = "provider_marker_wait"
		errorDetail = providerErr.Error()
	case sessionOriginErr != nil:
		errorStage = "session_origin_wait"
		errorDetail = sessionOriginErr.Error()
	case startupDeliveredErr != nil:
		errorStage = "startup_delivered_wait"
		errorDetail = startupDeliveredErr.Error()
	}

	return phase2RealTransportRun{
		Transport:                "tmux",
		SocketName:               guard.SocketName(),
		SessionName:              sessionName,
		ProviderPath:             providerPath,
		StartedPath:              startedPath,
		InputPath:                inputPath,
		SessionOriginPath:        sessionOriginPath,
		StartupDeliveredPath:     startupDeliveredPath,
		StartupPromptPath:        startupPromptPath,
		AutonomousPath:           autonomousPath,
		ErrorStage:               errorStage,
		Error:                    errorDetail,
		ExpectedInput:            materialized.Nudge,
		ObservedInput:            strings.TrimSpace(observedInput),
		ExpectedSessionOrigin:    materialized.Env["GC_SESSION_ORIGIN"],
		ObservedSessionOrigin:    strings.TrimSpace(observedSessionOrigin),
		ExpectedStartupDelivered: materialized.Env[startupPromptDeliveredEnv],
		ObservedStartupDelivered: strings.TrimSpace(observedStartupDelivered),
		ExpectedStartupPrompt:    expectedStartupPrompt,
		ObservedStartupPrompt:    observedStartupPrompt,
		ObservedProvider:         strings.TrimSpace(observedProvider),
		Started:                  startedErr == nil,
		AutonomousStarted:        autonomousStarted,
		RunningAfterInput:        sp.IsRunning(sessionName),
		StartElapsed:             startElapsed,
	}
}

func phase2RealTransportResult(tc phase2ProviderCase, run phase2RealTransportRun) workertest.Result {
	evidence := map[string]string{
		"family":                     tc.family,
		"profile":                    string(tc.profileID),
		"transport":                  run.Transport,
		"socket_name":                run.SocketName,
		"session_name":               run.SessionName,
		"started_path":               run.StartedPath,
		"provider_path":              run.ProviderPath,
		"input_path":                 run.InputPath,
		"session_origin_path":        run.SessionOriginPath,
		"startup_delivered_path":     run.StartupDeliveredPath,
		"startup_prompt_path":        run.StartupPromptPath,
		"autonomous_path":            run.AutonomousPath,
		"error_stage":                run.ErrorStage,
		"error":                      run.Error,
		"expected_input":             run.ExpectedInput,
		"observed_input":             run.ObservedInput,
		"expected_session_origin":    run.ExpectedSessionOrigin,
		"observed_session_origin":    run.ObservedSessionOrigin,
		"expected_startup_delivered": run.ExpectedStartupDelivered,
		"observed_startup_delivered": run.ObservedStartupDelivered,
		"expected_startup_prompt":    run.ExpectedStartupPrompt,
		"observed_startup_prompt":    run.ObservedStartupPrompt,
		"observed_provider":          run.ObservedProvider,
		"autonomous_started":         fmt.Sprintf("%t", run.AutonomousStarted),
		"running_after_input":        fmt.Sprintf("%t", run.RunningAfterInput),
		"start_elapsed":              run.StartElapsed.String(),
	}
	switch {
	case run.ErrorStage != "":
		return workertest.Fail(tc.profileID, workertest.RequirementRealTransportProof,
			fmt.Sprintf("%s failed: %s", run.ErrorStage, run.Error)).WithEvidence(evidence)
	case run.Transport != "tmux":
		return workertest.Fail(tc.profileID, workertest.RequirementRealTransportProof,
			fmt.Sprintf("transport = %q, want tmux", run.Transport)).WithEvidence(evidence)
	case !run.Started:
		return workertest.Fail(tc.profileID, workertest.RequirementRealTransportProof,
			"production runtime launch did not execute the started marker").WithEvidence(evidence)
	case run.ObservedProvider != tc.family:
		return workertest.Fail(tc.profileID, workertest.RequirementRealTransportProof,
			fmt.Sprintf("GC_PROVIDER = %q, want %q", run.ObservedProvider, tc.family)).WithEvidence(evidence)
	case run.ExpectedSessionOrigin != "" && run.ObservedSessionOrigin != run.ExpectedSessionOrigin:
		return workertest.Fail(tc.profileID, workertest.RequirementRealTransportProof,
			fmt.Sprintf("GC_SESSION_ORIGIN = %q, want %q", run.ObservedSessionOrigin, run.ExpectedSessionOrigin)).WithEvidence(evidence)
	case run.ExpectedStartupDelivered != "" && run.ObservedStartupDelivered != run.ExpectedStartupDelivered:
		return workertest.Fail(tc.profileID, workertest.RequirementRealTransportProof,
			fmt.Sprintf("%s = %q, want %q", startupPromptDeliveredEnv, run.ObservedStartupDelivered, run.ExpectedStartupDelivered)).WithEvidence(evidence)
	case run.ObservedStartupPrompt != run.ExpectedStartupPrompt:
		return workertest.Fail(tc.profileID, workertest.RequirementRealTransportProof,
			fmt.Sprintf("startup prompt = %q, want %q", run.ObservedStartupPrompt, run.ExpectedStartupPrompt)).WithEvidence(evidence)
	case !run.AutonomousStarted:
		return workertest.Fail(tc.profileID, workertest.RequirementRealTransportProof,
			"command-line startup prompt did not match expected payload before nudge").WithEvidence(evidence)
	case run.ObservedInput != run.ExpectedInput:
		return workertest.Fail(tc.profileID, workertest.RequirementRealTransportProof,
			fmt.Sprintf("nudge input = %q, want %q", run.ObservedInput, run.ExpectedInput)).WithEvidence(evidence)
	case !run.RunningAfterInput:
		return workertest.Fail(tc.profileID, workertest.RequirementRealTransportProof,
			"session was not running after initial input delivery").WithEvidence(evidence)
	case run.StartElapsed > phase2RealTransportBound:
		return workertest.Fail(tc.profileID, workertest.RequirementRealTransportProof,
			fmt.Sprintf("startup elapsed = %s, want <= %s", run.StartElapsed, phase2RealTransportBound)).WithEvidence(evidence)
	default:
		return workertest.Pass(tc.profileID, workertest.RequirementRealTransportProof,
			"production tmux runtime launched, delivered the command-line startup prompt, and preserved stdin nudge delivery deterministically").WithEvidence(evidence)
	}
}

func copyRuntimeEnv(input map[string]string) map[string]string {
	out := make(map[string]string, len(input)+6)
	for key, value := range input {
		out[key] = value
	}
	return out
}

func waitForPhase2FileText(path string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data), nil
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for %s: %w", path, lastErr)
}

func waitForPhase2FileExists(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}
