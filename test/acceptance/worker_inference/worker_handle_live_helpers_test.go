//go:build acceptance_c

package workerinference_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	runtimetmux "github.com/gastownhall/gascity/internal/runtime/tmux"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
	workerpkg "github.com/gastownhall/gascity/internal/worker"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

const workerHandleProbeInstructions = `
You are a worker-inference probe session for Gas City tests.

Follow the user's requests directly.
Use the workspace tools when needed.
After startup, do not inspect files, run commands, or do any other work until the user gives you a task.
When a later message asks you to recall prior turn context, use conversation memory rather than searching files or external history unless the user explicitly asks for that.
`

type liveWorkerHandleHarness struct {
	handle     workerpkg.Handle
	profile    workerpkg.Profile
	provider   string
	authSource string
	workDir    string
	gcHome     string
	adapter    workerpkg.SessionLogAdapter
}

func newLiveWorkerHandleHarness(t *testing.T) (*liveWorkerHandleHarness, error) {
	t.Helper()

	root, err := liveWorkerTempDir(t)
	if err != nil {
		return nil, err
	}
	gcHome := filepath.Join(root, "gc-home")
	runtimeDir := filepath.Join(root, "runtime")
	for _, dir := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	gcPath, err := helpers.ResolveGCPath(liveEnv)
	if err != nil {
		return nil, err
	}
	env := helpers.NewEnv(gcPath, gcHome, runtimeDir).
		Without("GC_SESSION").
		Without("GC_BEADS").
		Without("GC_DOLT").
		With("DOLT_ROOT_PATH", gcHome)

	authSource, err := stageProviderAuth(gcHome, env, liveSetup.Profile)
	if err != nil {
		return nil, err
	}
	if err := seedLiveProviderStateFor(liveSetup.Profile, gcHome, root); err != nil {
		return nil, err
	}

	resolved, err := resolveLiveHandleProvider()
	if err != nil {
		return nil, err
	}
	if err := writeWorkerHandleInstructions(root, resolved.InstructionsFile); err != nil {
		return nil, err
	}

	socketName := filepath.Base(root)
	tmuxCfg := runtimetmux.DefaultConfig()
	tmuxCfg.SocketName = socketName

	provider := runtimetmux.NewProviderWithConfig(tmuxCfg)
	manager := sessionpkg.NewManager(beads.NewMemStore(), provider)
	handle, err := workerpkg.NewSessionHandle(workerpkg.SessionHandleConfig{
		Manager: manager,
		Adapter: workerpkg.SessionLogAdapter{
			SearchPaths: profileSearchPaths(gcHome, liveSetup.Profile),
		},
		SearchPaths: profileSearchPaths(gcHome, liveSetup.Profile),
		Session: workerpkg.SessionSpec{
			Profile:   liveSetup.Profile,
			Template:  inferenceProbeTemplate,
			Title:     "Worker Inference Probe",
			Command:   liveHandleCommand(resolved),
			WorkDir:   root,
			Provider:  liveSetup.Provider,
			Transport: "tmux",
			Env:       mergeStringMaps(envMapFromAcceptanceEnv(env), resolved.Env),
			Resume: sessionpkg.ProviderResume{
				ResumeFlag:    resolved.ResumeFlag,
				ResumeStyle:   resolved.ResumeStyle,
				ResumeCommand: resolved.ResumeCommand,
				SessionIDFlag: resolved.SessionIDFlag,
			},
			Hints: liveHandleHints(resolved),
		},
	})
	if err != nil {
		return nil, err
	}

	harness := &liveWorkerHandleHarness{
		handle:     handle,
		profile:    liveSetup.Profile,
		provider:   liveSetup.Provider,
		authSource: authSource,
		workDir:    root,
		gcHome:     gcHome,
		adapter: workerpkg.SessionLogAdapter{
			SearchPaths: profileSearchPaths(gcHome, liveSetup.Profile),
		},
	}
	t.Cleanup(func() {
		_ = harness.handle.Stop(context.Background())
		if os.Getenv("GC_ACCEPTANCE_KEEP") != "1" {
			_ = os.RemoveAll(root)
		}
	})
	return harness, nil
}

func liveWorkerDebugf(format string, args ...any) {
	if strings.TrimSpace(os.Getenv("GC_WORKER_HANDLE_DEBUG")) != "1" {
		return
	}
	fmt.Fprintf(os.Stderr, "worker-handle-debug: "+format+"\n", args...) //nolint:errcheck
}

func liveWorkerTempDir(t *testing.T) (string, error) {
	t.Helper()

	tmpRoot, err := acceptanceTempRoot()
	if err != nil {
		return "", err
	}
	return os.MkdirTemp(tmpRoot, "gcwi-live-handle-*")
}

func resolveLiveHandleProvider() (*config.ResolvedProvider, error) {
	agent := &config.Agent{
		Name:     "worker-inference",
		Provider: liveSetup.Provider,
	}
	workspace := &config.Workspace{
		Name:     "worker-inference",
		Provider: liveSetup.Provider,
	}
	return config.ResolveProvider(agent, workspace, map[string]config.ProviderSpec{
		liveSetup.Provider: {
			Command: liveSetup.BinaryPath,
		},
	}, exec.LookPath)
}

func liveHandleCommand(resolved *config.ResolvedProvider) string {
	command := strings.TrimSpace(resolved.CommandString())
	defaultArgs := resolved.ResolveDefaultArgs()
	if len(defaultArgs) == 0 {
		return command
	}
	if command == "" {
		return shellquote.Join(defaultArgs)
	}
	return command + " " + shellquote.Join(defaultArgs)
}

func liveHandleHints(resolved *config.ResolvedProvider) runtime.Config {
	providerName := strings.TrimSpace(resolved.Kind)
	if providerName == "" {
		providerName = strings.TrimSpace(resolved.Name)
	}
	return runtime.Config{
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           append([]string(nil), resolved.ProcessNames...),
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
		ProviderName:           providerName,
	}
}

func writeWorkerHandleInstructions(workDir, instructionsFile string) error {
	name := strings.TrimSpace(instructionsFile)
	if name == "" {
		name = "AGENTS.md"
	}
	path := filepath.Join(workDir, name)
	return os.WriteFile(path, []byte(strings.TrimSpace(workerHandleProbeInstructions)+"\n"), 0o644)
}

func envMapFromAcceptanceEnv(env *helpers.Env) map[string]string {
	if env == nil {
		return nil
	}
	values := env.List()
	out := make(map[string]string, len(values))
	for _, item := range values {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		out[key] = value
	}
	return out
}

func seedLiveProviderStateFor(profile workerpkg.Profile, gcHome, workDir string) error {
	switch profile {
	case workerpkg.ProfileClaudeTmuxCLI:
		for _, path := range []string{
			filepath.Join(gcHome, ".claude.json"),
			filepath.Join(gcHome, ".claude", ".claude.json"),
		} {
			if err := seedClaudeProjectOnboarding(path, workDir); err != nil {
				return err
			}
		}
	case workerpkg.ProfileCodexTmuxCLI:
		if err := seedCodexProjectTrust(filepath.Join(gcHome, ".codex", "config.toml"), workDir); err != nil {
			return err
		}
	case workerpkg.ProfileGeminiTmuxCLI:
		if err := seedGeminiFolderTrust(filepath.Join(gcHome, ".gemini", "trustedFolders.json"), workDir); err != nil {
			return err
		}
	}
	return nil
}

func (h *liveWorkerHandleHarness) start() (workerpkg.State, map[string]string, error) {
	ctx := context.Background()
	evidence := h.baseEvidence()
	err := h.handle.Start(ctx)
	state, stateErr := h.handle.State(ctx)
	evidence = h.withStateEvidence(evidence, state, stateErr)
	liveWorkerDebugf("start work_dir=%s phase=%s session_id=%s session_name=%s err=%v state_err=%v", h.workDir, state.Phase, state.SessionID, state.SessionName, err, stateErr)
	if err == nil {
		return state, evidence, nil
	}
	return state, h.withBlockedEvidence(evidence, state.SessionName), err
}

func (h *liveWorkerHandleHarness) stop() (workerpkg.State, map[string]string, error) {
	ctx := context.Background()
	evidence := h.baseEvidence()
	err := h.handle.Stop(ctx)
	state, stateErr := h.handle.State(ctx)
	evidence = h.withStateEvidence(evidence, state, stateErr)
	liveWorkerDebugf("stop work_dir=%s phase=%s session_id=%s session_name=%s err=%v state_err=%v", h.workDir, state.Phase, state.SessionID, state.SessionName, err, stateErr)
	if err != nil {
		return state, h.withBlockedEvidence(evidence, state.SessionName), err
	}
	return state, evidence, nil
}

func (h *liveWorkerHandleHarness) submitAndWaitForFile(prompt, outputRel string, delivery workerpkg.DeliveryIntent) (workerpkg.State, string, map[string]string, error) {
	ctx := context.Background()
	evidence := h.baseEvidence()
	evidence["prompt"] = prompt
	evidence["submit_delivery"] = string(delivery)
	outputPath := filepath.Join(h.workDir, outputRel)
	evidence["output_path"] = outputPath

	result, err := h.handle.Message(ctx, workerpkg.MessageRequest{
		Text:     prompt,
		Delivery: delivery,
	})
	evidence["submit_queued"] = strconv.FormatBool(result.Queued)

	state, stateErr := h.handle.State(ctx)
	evidence = h.withStateEvidence(evidence, state, stateErr)
	liveWorkerDebugf("submit-and-wait work_dir=%s delivery=%s phase=%s session_id=%s session_name=%s queued=%v err=%v state_err=%v prompt=%q", h.workDir, delivery, state.Phase, state.SessionID, state.SessionName, result.Queued, err, stateErr, prompt)
	if err != nil {
		return state, "", h.withBlockedEvidence(evidence, state.SessionName), err
	}

	output, fileEvidence, fileErr := waitForLiveFileText(h.workDir, state.SessionName, outputPath, 4*time.Minute)
	evidence = mergeEvidence(evidence, fileEvidence)
	evidence["output_contents"] = output
	if fileErr != nil {
		return state, output, h.withBlockedEvidence(evidence, state.SessionName), fileErr
	}
	return state, output, evidence, nil
}

func (h *liveWorkerHandleHarness) submit(prompt string, delivery workerpkg.DeliveryIntent) (workerpkg.State, map[string]string, error) {
	ctx := context.Background()
	evidence := h.baseEvidence()
	evidence["prompt"] = prompt
	evidence["submit_delivery"] = string(delivery)

	result, err := h.handle.Message(ctx, workerpkg.MessageRequest{
		Text:     prompt,
		Delivery: delivery,
	})
	evidence["submit_queued"] = strconv.FormatBool(result.Queued)

	state, stateErr := h.handle.State(ctx)
	evidence = h.withStateEvidence(evidence, state, stateErr)
	liveWorkerDebugf("submit work_dir=%s delivery=%s phase=%s session_id=%s session_name=%s queued=%v err=%v state_err=%v prompt=%q", h.workDir, delivery, state.Phase, state.SessionID, state.SessionName, result.Queued, err, stateErr, prompt)
	if err != nil {
		return state, h.withBlockedEvidence(evidence, state.SessionName), err
	}
	return state, evidence, nil
}

func (h *liveWorkerHandleHarness) waitForBusyTurnStart(sessionName, outputNeedle string) (map[string]string, error) {
	evidence := h.baseEvidence()
	evidence["busy_session_name"] = sessionName
	evidence["busy_output_needle"] = outputNeedle

	var (
		lastPane string
		lastErr  error
	)
	found := pollForCondition(30*time.Second, 500*time.Millisecond, func() bool {
		pane, err := captureTmuxPane(h.workDir, sessionName, 120)
		if err != nil {
			lastErr = err
			return false
		}
		lastPane = pane
		if strings.Contains(pane, outputNeedle) || livePaneShowsBusyIndicator(strings.Split(pane, "\n")) {
			return true
		}
		return false
	})

	if strings.TrimSpace(lastPane) != "" {
		evidence["pane_tail"] = lastPane
	}
	if found {
		return evidence, nil
	}
	if lastErr != nil {
		return evidence, lastErr
	}
	return evidence, fmt.Errorf("busy turn did not show in-flight activity for %q", outputNeedle)
}

func livePaneShowsBusyIndicator(lines []string) bool {
	for _, line := range lines {
		if strings.Contains(line, "esc to interrupt") ||
			strings.Contains(line, "Press Esc or Ctrl+C to cancel") ||
			strings.Contains(line, "[current working directory ") {
			return true
		}
	}
	return false
}

func (h *liveWorkerHandleHarness) waitForHistory(prompt, outputText string) (*workerpkg.HistorySnapshot, map[string]string, error) {
	ctx := context.Background()
	evidence := h.baseEvidence()
	wantPrompt := strings.TrimSpace(prompt)
	wantOutput := strings.TrimSpace(outputText)
	var (
		snapshot *workerpkg.HistorySnapshot
		lastErr  error
	)

	found := pollForCondition(90*time.Second, 5*time.Second, func() bool {
		snapshot, lastErr = h.handle.History(ctx, workerpkg.HistoryRequest{})
		if lastErr == nil && snapshot != nil && len(snapshot.Entries) > 0 {
			if wantPrompt == "" && wantOutput == "" {
				return true
			}
			if historyContainsExpectedEvidence(snapshot, wantPrompt, wantOutput) {
				return true
			}
			lastErr = fmt.Errorf("live transcript for %s did not contain the expected task evidence", h.profile)
		} else if lastErr == nil {
			lastErr = fmt.Errorf("normalized transcript for %s is empty", h.profile)
		}
		state, stateErr := h.handle.State(ctx)
		if stateErr == nil {
			evidence = h.withStateEvidence(evidence, state, nil)
			if blocked, blockErr := detectLiveBlockedInteraction(h.workDir, state.SessionName); blockErr == nil && blocked != nil {
				lastErr = blocked.err()
				evidence = mergeEvidence(evidence, blocked.evidence())
				return true
			} else if blockErr != nil {
				lastErr = blockErr
			}
		}
		return false
	})

	evidence = mergeEvidence(evidence, historySnapshotEvidence(snapshot))
	if found && lastErr == nil {
		return snapshot, evidence, nil
	}
	if lastErr != nil {
		return snapshot, evidence, lastErr
	}
	return snapshot, evidence, fmt.Errorf("live transcript for %s did not contain the expected task evidence", h.profile)
}

func (h *liveWorkerHandleHarness) waitForContinuationHistory(before *workerpkg.HistorySnapshot, prompt string) (*workerpkg.HistorySnapshot, map[string]string, error) {
	ctx := context.Background()
	evidence := h.baseEvidence()
	if before != nil {
		evidence = mergeEvidence(evidence, map[string]string{
			"before_transcript":    before.TranscriptStreamID,
			"before_entry_count":   strconv.Itoa(len(before.Entries)),
			"before_logical_conv":  before.LogicalConversationID,
			"before_provider_sess": before.ProviderSessionID,
		})
	}
	var (
		snapshot *workerpkg.HistorySnapshot
		lastErr  error
		lastNote string
	)

	found := pollForCondition(90*time.Second, 5*time.Second, func() bool {
		snapshot, lastErr = h.handle.History(ctx, workerpkg.HistoryRequest{})
		if lastErr == nil && snapshot != nil {
			lastErr = continuationSnapshotError(
				h.profile,
				before.TranscriptStreamID,
				before,
				snapshot.TranscriptStreamID,
				snapshot,
				prompt,
			)
			if lastErr != nil {
				note := lastErr.Error()
				if note != lastNote {
					lastNote = note
					liveWorkerDebugf("continuation-check work_dir=%s transcript=%s err=%s", h.workDir, snapshot.TranscriptStreamID, note)
				}
			}
			if lastErr == nil {
				return true
			}
		}
		state, stateErr := h.handle.State(ctx)
		if stateErr == nil {
			evidence = h.withStateEvidence(evidence, state, nil)
			if blocked, blockErr := detectLiveBlockedInteraction(h.workDir, state.SessionName); blockErr == nil && blocked != nil {
				lastErr = blocked.err()
				evidence = mergeEvidence(evidence, blocked.evidence())
				return true
			} else if blockErr != nil {
				lastErr = blockErr
			}
		}
		return false
	})

	evidence = mergeEvidence(evidence, historySnapshotEvidence(snapshot))
	if found && lastErr == nil {
		return snapshot, evidence, nil
	}
	if lastErr != nil {
		return snapshot, evidence, lastErr
	}
	return snapshot, evidence, fmt.Errorf("continuation transcript for %s did not show the expected follow-up turn", h.profile)
}

func (h *liveWorkerHandleHarness) waitForInterruptContinuationHistory(before *workerpkg.HistorySnapshot, interruptedPrompt, prompt string) (*workerpkg.HistorySnapshot, map[string]string, error) {
	ctx := context.Background()
	evidence := h.baseEvidence()
	if before != nil {
		evidence = mergeEvidence(evidence, map[string]string{
			"before_transcript":    before.TranscriptStreamID,
			"before_entry_count":   strconv.Itoa(len(before.Entries)),
			"before_logical_conv":  before.LogicalConversationID,
			"before_provider_sess": before.ProviderSessionID,
		})
	}
	var (
		snapshot *workerpkg.HistorySnapshot
		lastErr  error
		lastNote string
	)

	found := pollForCondition(90*time.Second, 5*time.Second, func() bool {
		snapshot, lastErr = h.handle.History(ctx, workerpkg.HistoryRequest{})
		if lastErr == nil && snapshot != nil {
			lastErr = interruptContinuationSnapshotError(
				h.profile,
				before,
				snapshot,
				interruptedPrompt,
				prompt,
			)
			if lastErr != nil {
				note := lastErr.Error()
				if note != lastNote {
					lastNote = note
					liveWorkerDebugf("interrupt-continuation-check work_dir=%s transcript=%s err=%s", h.workDir, snapshot.TranscriptStreamID, note)
				}
			}
			if lastErr == nil {
				return true
			}
		}
		state, stateErr := h.handle.State(ctx)
		if stateErr == nil {
			evidence = h.withStateEvidence(evidence, state, nil)
			if blocked, blockErr := detectLiveBlockedInteraction(h.workDir, state.SessionName); blockErr == nil && blocked != nil {
				lastErr = blocked.err()
				evidence = mergeEvidence(evidence, blocked.evidence())
				return true
			} else if blockErr != nil {
				lastErr = blockErr
			}
		}
		return false
	})

	evidence = mergeEvidence(evidence, historySnapshotEvidence(snapshot))
	if found && lastErr == nil {
		return snapshot, evidence, nil
	}
	if lastErr != nil {
		return snapshot, evidence, lastErr
	}
	return snapshot, evidence, fmt.Errorf("interrupt continuation transcript for %s did not show the expected follow-up turn", h.profile)
}

func (h *liveWorkerHandleHarness) baseEvidence() map[string]string {
	return map[string]string{
		"work_dir":    h.workDir,
		"gc_home":     h.gcHome,
		"profile":     string(h.profile),
		"provider":    h.provider,
		"auth_source": h.authSource,
		"binary_path": liveSetup.BinaryPath,
	}
}

func (h *liveWorkerHandleHarness) withStateEvidence(evidence map[string]string, state workerpkg.State, stateErr error) map[string]string {
	if stateErr != nil {
		evidence["worker_state_error"] = stateErr.Error()
		return evidence
	}
	evidence["worker_phase"] = string(state.Phase)
	if strings.TrimSpace(state.Detail) != "" {
		evidence["worker_detail"] = state.Detail
	}
	if strings.TrimSpace(state.SessionID) != "" {
		evidence["gc_session_id"] = state.SessionID
	}
	if strings.TrimSpace(state.SessionName) != "" {
		evidence["session_name"] = state.SessionName
	}
	if strings.TrimSpace(state.Provider) != "" {
		evidence["worker_provider"] = state.Provider
	}
	if state.Pending != nil {
		if state.Pending.RequestID != "" {
			evidence["pending_request_id"] = state.Pending.RequestID
		}
		if state.Pending.Kind != "" {
			evidence["pending_kind"] = state.Pending.Kind
		}
	}
	return evidence
}

func (h *liveWorkerHandleHarness) withBlockedEvidence(evidence map[string]string, sessionName string) map[string]string {
	blocked, err := detectLiveBlockedInteraction(h.workDir, sessionName)
	if err != nil {
		evidence["blocked_detect_error"] = err.Error()
		return evidence
	}
	if blocked != nil {
		return mergeEvidence(evidence, blocked.evidence())
	}
	return evidence
}

func historySnapshotEvidence(snapshot *workerpkg.HistorySnapshot) map[string]string {
	if snapshot == nil {
		return nil
	}
	return map[string]string{
		"transcript_path":      snapshot.TranscriptStreamID,
		"entry_count":          strconv.Itoa(len(snapshot.Entries)),
		"tail_activity":        string(snapshot.TailState.Activity),
		"logical_conversation": snapshot.LogicalConversationID,
		"provider_session_id":  snapshot.ProviderSessionID,
	}
}

func mergeStringMaps(base, extra map[string]string) map[string]string {
	switch {
	case len(base) == 0 && len(extra) == 0:
		return nil
	case len(base) == 0:
		out := make(map[string]string, len(extra))
		for key, value := range extra {
			out[key] = value
		}
		return out
	case len(extra) == 0:
		out := make(map[string]string, len(base))
		for key, value := range base {
			out[key] = value
		}
		return out
	}
	out := make(map[string]string, len(base)+len(extra))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}
