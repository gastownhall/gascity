package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSubmitDefaultResumesSuspendedClaudeSessionAndWaitsForIdleNudge(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", t.TempDir(), "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
	if err != nil {
		t.Fatalf("Submit(default): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(default) unexpectedly queued")
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after default submit")
	}
	found := false
	for _, call := range sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want Nudge(hello)", sp.Calls)
	}
}

func TestSubmitDefaultResumesSuspendedCodexSessionAndNudgesImmediately(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "codex", t.TempDir(), "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
	if err != nil {
		t.Fatalf("Submit(default): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(default) unexpectedly queued")
	}
	found := false
	for _, call := range sp.Calls {
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want NudgeNow(hello)", sp.Calls)
	}
}

func TestSubmitDefaultCodexDismissesDeferredDialogsOnFirstDelivery(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "codex", t.TempDir(), "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
	if err != nil {
		t.Fatalf("Submit(default): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(default) unexpectedly queued")
	}

	methods := make([]string, 0, len(sp.Calls))
	for _, call := range sp.Calls {
		methods = append(methods, call.Method)
	}
	want := []string{"IsRunning", "DismissKnownDialogs", "Pending", "NudgeNow", "DismissKnownDialogs"}
	if !containsSubsequence(methods, want) {
		t.Fatalf("methods = %v, want subsequence %v", methods, want)
	}

	updated, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if got := updated.Metadata[startupDialogVerifiedKey]; got != "true" {
		t.Fatalf("%s = %q, want true", startupDialogVerifiedKey, got)
	}
}

func TestSubmitDefaultCodexSkipsDeferredDialogsAfterVerification(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "codex", t.TempDir(), "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, startupDialogVerifiedKey, "true"); err != nil {
		t.Fatalf("SetMetadata(%s): %v", startupDialogVerifiedKey, err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
	if err != nil {
		t.Fatalf("Submit(default): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(default) unexpectedly queued")
	}

	for _, call := range sp.Calls {
		if call.Method == "DismissKnownDialogs" {
			t.Fatalf("calls = %#v, did not want deferred dialog dismissal after verification", sp.Calls)
		}
	}
}

func TestSubmitDefaultResumesSuspendedGeminiSessionAndNudgesImmediately(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "gemini", t.TempDir(), "gemini", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
	if err != nil {
		t.Fatalf("Submit(default): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(default) unexpectedly queued")
	}

	var sawNudge, sawNudgeNow bool
	for _, call := range sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			sawNudge = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "hello" {
			sawNudgeNow = true
		}
	}
	if !sawNudgeNow {
		t.Fatalf("calls = %#v, want NudgeNow(hello)", sp.Calls)
	}
	if sawNudge {
		t.Fatalf("calls = %#v, did not want Nudge(hello)", sp.Calls)
	}
}

func TestSubmitDefaultToRunningGeminiSessionWaitsForIdleNudge(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "gemini", t.TempDir(), "gemini", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
	if err != nil {
		t.Fatalf("Submit(default): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(default) unexpectedly queued")
	}

	var sawNudge, sawNudgeNow bool
	for _, call := range sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			sawNudge = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "hello" {
			sawNudgeNow = true
		}
	}
	if !sawNudge {
		t.Fatalf("calls = %#v, want Nudge(hello)", sp.Calls)
	}
	if sawNudgeNow {
		t.Fatalf("calls = %#v, did not want NudgeNow(hello)", sp.Calls)
	}
}

func TestSubmitDefaultConfirmsLiveCreatingSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	workDir := t.TempDir()
	sessionName := "s-live-create"
	if err := sp.Start(context.Background(), sessionName, runtime.Config{WorkDir: workDir, Command: "gemini"}); err != nil {
		t.Fatalf("fake Start: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   BeadType,
		Labels: []string{LabelSession, "template:helper"},
		Metadata: map[string]string{
			"template":             "helper",
			"state":                "creating",
			"pending_create_claim": "true",
			"provider":             "gemini",
			"command":              "gemini",
			"work_dir":             workDir,
			"session_name":         sessionName,
		},
	})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}

	if _, err := mgr.Submit(context.Background(), created.ID, "hello", "gemini", runtime.Config{WorkDir: workDir}, SubmitIntentDefault); err != nil {
		t.Fatalf("Submit(default): %v", err)
	}

	updated, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if got := updated.Metadata["state"]; got != string(StateActive) {
		t.Fatalf("state = %q, want %q", got, StateActive)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", got)
	}
	if got := updated.Metadata["state_reason"]; got != "creation_complete" {
		t.Fatalf("state_reason = %q, want creation_complete", got)
	}
}

func TestSubmitFollowUpQueuesDeferredMessageAndStartsCodexPoller(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	mgr := NewManagerWithCityPath(store, sp, cityPath)

	info, err := mgr.Create(context.Background(), "helper", "", "codex", t.TempDir(), "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var pollerCalls int
	origPoller := startSessionSubmitPoller
	startSessionSubmitPoller = func(city, agent, sessionName string) error {
		pollerCalls++
		if city != cityPath {
			t.Fatalf("poller cityPath = %q, want %q", city, cityPath)
		}
		if agent != info.ID {
			t.Fatalf("poller agent = %q, want %q", agent, info.ID)
		}
		if sessionName != info.SessionName {
			t.Fatalf("poller sessionName = %q, want %q", sessionName, info.SessionName)
		}
		return nil
	}
	defer func() { startSessionSubmitPoller = origPoller }()

	outcome, err := mgr.Submit(context.Background(), info.ID, "follow up later", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentFollowUp)
	if err != nil {
		t.Fatalf("Submit(follow_up): %v", err)
	}
	if !outcome.Queued {
		t.Fatal("Submit(follow_up) should report queued")
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending queued submits = %d, want 1", len(state.Pending))
	}
	item := state.Pending[0]
	if item.SessionID != info.ID {
		t.Fatalf("SessionID = %q, want %q", item.SessionID, info.ID)
	}
	if item.Agent != info.ID {
		t.Fatalf("Agent = %q, want %q", item.Agent, info.ID)
	}
	if item.Message != "follow up later" {
		t.Fatalf("Message = %q, want %q", item.Message, "follow up later")
	}
	if pollerCalls != 1 {
		t.Fatalf("pollerCalls = %d, want 1", pollerCalls)
	}
}

func TestSubmitFollowUpQueuesDeferredMessageForPoolManagedSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	mgr := NewManagerWithCityPath(store, sp, cityPath)

	info, err := mgr.Create(context.Background(), "helper", "", "codex", t.TempDir(), "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Update(info.ID, beads.UpdateOpts{
		Metadata: map[string]string{
			"pool_managed": "true",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "follow up later", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentFollowUp)
	if err != nil {
		t.Fatalf("Submit(follow_up): %v", err)
	}
	if !outcome.Queued {
		t.Fatal("Submit(follow_up) should report queued")
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending queued submits = %d, want 1", len(state.Pending))
	}
}

func TestSubmitFollowUpOnSuspendedSessionFallsBackToImmediateSend(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	mgr := NewManagerWithCityPath(store, sp, cityPath)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", t.TempDir(), "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "send this now", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentFollowUp)
	if err != nil {
		t.Fatalf("Submit(follow_up): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(follow_up) unexpectedly queued for suspended session")
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after follow_up on suspended session")
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 0 {
		t.Fatalf("pending queued submits = %d, want 0", len(state.Pending))
	}
	found := false
	for _, call := range sp.Calls {
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "send this now" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want NudgeNow(send this now)", sp.Calls)
	}
}

func TestSubmissionCapabilitiesFollowUpUnsupportedForACP(t *testing.T) {
	caps := SubmissionCapabilitiesForMetadata(
		map[string]string{
			"provider":  "acp",
			"transport": "acp",
		},
		true,
	)
	if caps.SupportsFollowUp {
		t.Fatal("SupportsFollowUp = true, want false for ACP transport")
	}
	if !caps.SupportsInterruptNow {
		t.Fatal("SupportsInterruptNow = false, want true")
	}
}

func TestSubmissionCapabilitiesRemainEnabledForPoolManagedSessions(t *testing.T) {
	caps := SubmissionCapabilitiesForMetadata(
		map[string]string{
			"provider":     "codex",
			"pool_managed": "true",
			"pool_slot":    "1",
		},
		true,
	)
	if !caps.SupportsFollowUp {
		t.Fatal("SupportsFollowUp = false, want true")
	}
	if !caps.SupportsInterruptNow {
		t.Fatal("SupportsInterruptNow = false, want true")
	}
}

func TestSubmitInterruptNowUsesInterruptAndIdleWaitForGemini(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "gemini", t.TempDir(), "gemini", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "take this now", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}

	var sawEscape, sawInterrupt, sawWaitForIdle, sawClear, sawNudge, sawStop bool
	interruptIdx := -1
	waitIdx := -1
	clearIdx := -1
	nudgeIdx := -1
	for i, call := range sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
			interruptIdx = i
		}
		if call.Method == "WaitForIdle" && call.Name == info.SessionName {
			sawWaitForIdle = true
			waitIdx = i
		}
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "C-u" {
			sawClear = true
			clearIdx = i
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "take this now" {
			sawNudge = true
			nudgeIdx = i
		}
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
	}
	if sawEscape {
		t.Fatalf("calls = %#v, did not want Escape for gemini interrupt_now", sp.Calls)
	}
	if !sawInterrupt || !sawWaitForIdle || !sawClear || !sawNudge {
		t.Fatalf("calls = %#v, want Interrupt + WaitForIdle + SendKeys(C-u) + NudgeNow", sp.Calls)
	}
	if interruptIdx < 0 || waitIdx < 0 || clearIdx < 0 || nudgeIdx < 0 {
		t.Fatalf("calls = %#v, want Interrupt + WaitForIdle + SendKeys(C-u) before NudgeNow", sp.Calls)
	}
	if !(interruptIdx < waitIdx && waitIdx < clearIdx && clearIdx < nudgeIdx) {
		t.Fatalf("calls = %#v, want Interrupt -> WaitForIdle -> SendKeys(C-u) before NudgeNow", sp.Calls)
	}
	if sawStop {
		t.Fatalf("calls = %#v, did not want Stop for gemini interrupt_now", sp.Calls)
	}
}

func TestSubmitInterruptNowAllowsPoolManagedCodexSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "codex", t.TempDir(), "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Update(info.ID, beads.UpdateOpts{
		Metadata: map[string]string{
			"pool_managed": "true",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "take this now", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}

	var sawEscape, sawWaitForIdle, sawNudge, sawStop bool
	waitIdx := -1
	nudgeIdx := -1
	for i, call := range sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
		if call.Method == "WaitForIdle" && call.Name == info.SessionName {
			sawWaitForIdle = true
			waitIdx = i
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "take this now" {
			sawNudge = true
			nudgeIdx = i
		}
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
	}
	if !sawEscape || !sawWaitForIdle || !sawNudge {
		t.Fatalf("calls = %#v, want SendKeys(Escape) + WaitForIdle + NudgeNow", sp.Calls)
	}
	if waitIdx < 0 || nudgeIdx < 0 || waitIdx > nudgeIdx {
		t.Fatalf("calls = %#v, want WaitForIdle before NudgeNow", sp.Calls)
	}
	if sawStop {
		t.Fatalf("calls = %#v, did not want Stop for codex interrupt_now", sp.Calls)
	}
}

func TestSubmitInterruptNowUsesInterruptAndIdleWaitForClaude(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", t.TempDir(), "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}

	var sawInterrupt, sawWaitForIdle, sawClear, sawNudge, sawStop bool
	clearIdx := -1
	nudgeIdx := -1
	for i, call := range sp.Calls {
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
		}
		if call.Method == "WaitForIdle" && call.Name == info.SessionName {
			sawWaitForIdle = true
		}
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "C-u" {
			sawClear = true
			clearIdx = i
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "replace the current turn" {
			sawNudge = true
			nudgeIdx = i
		}
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
	}
	if !sawInterrupt || !sawWaitForIdle || !sawClear || !sawNudge {
		t.Fatalf("calls = %#v, want interrupt + WaitForIdle + SendKeys(C-u) + nudge", sp.Calls)
	}
	if clearIdx < 0 || nudgeIdx < 0 || clearIdx > nudgeIdx {
		t.Fatalf("calls = %#v, want SendKeys(C-u) before nudge", sp.Calls)
	}
	if sawStop {
		t.Fatalf("calls = %#v, did not want Stop for claude interrupt_now", sp.Calls)
	}
}

func TestSubmitInterruptNowFallsBackToRestartOnIdleTimeout(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sp.WaitForIdleErrors = map[string]error{}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", t.TempDir(), "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.WaitForIdleErrors[info.SessionName] = fmt.Errorf("not idle yet")

	// WaitForIdle fails → fallback stops session → restart also calls
	// WaitForIdle which still fails. The error propagates from the restart
	// path, confirming the fallback was attempted.
	_, err = mgr.Submit(context.Background(), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err == nil {
		t.Fatal("Submit(interrupt_now) should error when idle wait persistently fails")
	}

	var sawStop, sawInterrupt bool
	for _, call := range sp.Calls {
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
		}
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
	}
	if !sawInterrupt {
		t.Fatalf("calls = %#v, want Interrupt", sp.Calls)
	}
	if !sawStop {
		t.Fatalf("calls = %#v, want Stop (fallback after idle timeout)", sp.Calls)
	}
}

func TestSubmitInterruptNowUsesControlCFallbackAfterSoftEscapeTimeoutForCodex(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "codex", t.TempDir(), "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.WaitForIdleSequence[info.SessionName] = []error{fmt.Errorf("not idle yet"), nil}

	outcome, err := mgr.Submit(context.Background(), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}

	var sawEscape, sawInterrupt, sawNudge, sawStop bool
	waitCalls := 0
	for _, call := range sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
		}
		if call.Method == "WaitForIdle" && call.Name == info.SessionName {
			waitCalls++
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "replace the current turn" {
			sawNudge = true
		}
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
	}
	if !sawEscape || !sawInterrupt || !sawNudge || waitCalls != 2 {
		t.Fatalf("calls = %#v, want SendKeys(Escape) + WaitForIdle + Interrupt + WaitForIdle + NudgeNow", sp.Calls)
	}
	if sawStop {
		t.Fatalf("calls = %#v, did not want Stop after successful control-c fallback", sp.Calls)
	}
}

func TestStopTurnUsesSoftEscapeAndIdleWaitForCodex(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "codex", t.TempDir(), "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}

	var sawEscape, sawInterrupt, sawWaitForIdle bool
	for _, call := range sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
		}
		if call.Method == "WaitForIdle" && call.Name == info.SessionName {
			sawWaitForIdle = true
		}
	}
	if !sawEscape || !sawWaitForIdle {
		t.Fatalf("calls = %#v, want SendKeys(Escape) + WaitForIdle", sp.Calls)
	}
	if sawInterrupt {
		t.Fatalf("calls = %#v, did not want Interrupt for codex stop", sp.Calls)
	}
}

func TestStopTurnUsesControlCFallbackAfterSoftEscapeTimeoutForCodex(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "codex", t.TempDir(), "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.WaitForIdleSequence[info.SessionName] = []error{fmt.Errorf("not idle yet"), nil}

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}

	var sawEscape, sawInterrupt bool
	waitCalls := 0
	for _, call := range sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
		}
		if call.Method == "WaitForIdle" && call.Name == info.SessionName {
			waitCalls++
		}
	}
	if !sawEscape || !sawInterrupt || waitCalls != 2 {
		t.Fatalf("calls = %#v, want SendKeys(Escape) + WaitForIdle + Interrupt + WaitForIdle", sp.Calls)
	}
}

func containsSubsequence(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	idx := 0
	for _, item := range have {
		if item == want[idx] {
			idx++
			if idx == len(want) {
				return true
			}
		}
	}
	return false
}
