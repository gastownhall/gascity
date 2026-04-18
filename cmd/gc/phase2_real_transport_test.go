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

const phase2RealTransportBound = 5 * time.Second

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

type phase2RealTransportRun struct {
	Transport         string
	SocketName        string
	SessionName       string
	ProviderPath      string
	StartedPath       string
	InputPath         string
	ErrorStage        string
	Error             string
	ExpectedInput     string
	ObservedInput     string
	ObservedProvider  string
	Started           bool
	RunningAfterInput bool
	StartElapsed      time.Duration
}

func launchPhase2RealTransportSession(t *testing.T, tc phase2ProviderCase, materialized runtime.Config) phase2RealTransportRun {
	t.Helper()

	guard := tmuxtest.NewGuard(t)
	dir := t.TempDir()
	startedPath := filepath.Join(dir, "started.txt")
	providerPath := filepath.Join(dir, "provider.txt")
	inputPath := filepath.Join(dir, "input.txt")
	stopPath := filepath.Join(dir, "stop.txt")
	sessionName := guard.SessionName("phase2-" + tc.family)

	sp, err := newSessionProviderByName("", config.SessionConfig{
		Socket:             guard.SocketName(),
		SetupTimeout:       "3s",
		NudgeReadyTimeout:  "2s",
		NudgeRetryInterval: "50ms",
		NudgeLockTimeout:   "2s",
	}, guard.CityName(), dir)
	if err != nil {
		return phase2RealTransportRun{
			Transport:     "tmux",
			SocketName:    guard.SocketName(),
			SessionName:   sessionName,
			ProviderPath:  providerPath,
			StartedPath:   startedPath,
			InputPath:     inputPath,
			ErrorStage:    "provider",
			Error:         err.Error(),
			ExpectedInput: materialized.Nudge,
		}
	}

	t.Cleanup(func() {
		_ = os.WriteFile(stopPath, []byte("stop\n"), 0o644)
		_ = sp.Stop(sessionName)
	})

	script := strings.Join([]string{
		`set -eu`,
		`printf "%s\n" "$GC_PROVIDER" > "$GC_REAL_TRANSPORT_PROVIDER_PATH"`,
		`printf "started\n" > "$GC_REAL_TRANSPORT_STARTED_PATH"`,
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
	cfg.Env["GC_REAL_TRANSPORT_STOP_PATH"] = stopPath

	ctx, cancel := context.WithTimeout(context.Background(), phase2RealTransportBound)
	defer cancel()

	start := time.Now()
	if err := sp.Start(ctx, sessionName, cfg); err != nil {
		return phase2RealTransportRun{
			Transport:     "tmux",
			SocketName:    guard.SocketName(),
			SessionName:   sessionName,
			ProviderPath:  providerPath,
			StartedPath:   startedPath,
			InputPath:     inputPath,
			ErrorStage:    "start",
			Error:         err.Error(),
			ExpectedInput: materialized.Nudge,
			StartElapsed:  time.Since(start),
		}
	}
	startElapsed := time.Since(start)

	observedInput, inputErr := waitForPhase2FileText(inputPath, phase2RealTransportBound)
	observedProvider, providerErr := waitForPhase2FileText(providerPath, phase2RealTransportBound)
	_, startedErr := os.Stat(startedPath)

	errorStage := ""
	errorDetail := ""
	switch {
	case inputErr != nil:
		errorStage = "input_wait"
		errorDetail = inputErr.Error()
	case providerErr != nil:
		errorStage = "provider_marker_wait"
		errorDetail = providerErr.Error()
	}

	return phase2RealTransportRun{
		Transport:         "tmux",
		SocketName:        guard.SocketName(),
		SessionName:       sessionName,
		ProviderPath:      providerPath,
		StartedPath:       startedPath,
		InputPath:         inputPath,
		ErrorStage:        errorStage,
		Error:             errorDetail,
		ExpectedInput:     materialized.Nudge,
		ObservedInput:     strings.TrimSpace(observedInput),
		ObservedProvider:  strings.TrimSpace(observedProvider),
		Started:           startedErr == nil,
		RunningAfterInput: sp.IsRunning(sessionName),
		StartElapsed:      startElapsed,
	}
}

func phase2RealTransportResult(tc phase2ProviderCase, run phase2RealTransportRun) workertest.Result {
	evidence := map[string]string{
		"family":              tc.family,
		"profile":             string(tc.profileID),
		"transport":           run.Transport,
		"socket_name":         run.SocketName,
		"session_name":        run.SessionName,
		"started_path":        run.StartedPath,
		"provider_path":       run.ProviderPath,
		"input_path":          run.InputPath,
		"error_stage":         run.ErrorStage,
		"error":               run.Error,
		"expected_input":      run.ExpectedInput,
		"observed_input":      run.ObservedInput,
		"observed_provider":   run.ObservedProvider,
		"running_after_input": fmt.Sprintf("%t", run.RunningAfterInput),
		"start_elapsed":       run.StartElapsed.String(),
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
			"production tmux runtime launched and delivered initial input deterministically").WithEvidence(evidence)
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
