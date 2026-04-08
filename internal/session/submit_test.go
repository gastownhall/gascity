package session

import (
	"context"
	"fmt"
	"strings"
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

func TestSubmitDefaultResumesSuspendedGeminiSessionAndWaitsForIdleNudge(t *testing.T) {
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
	if !sawNudge {
		t.Fatalf("calls = %#v, want Nudge(hello)", sp.Calls)
	}
	if sawNudgeNow {
		t.Fatalf("calls = %#v, did not want NudgeNow(hello)", sp.Calls)
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

func TestSubmitInterruptNowUsesSoftEscapeForGemini(t *testing.T) {
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

	var sawEscape, sawStop bool
	for _, call := range sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
	}
	if !sawEscape {
		t.Fatalf("calls = %#v, want SendKeys(Escape)", sp.Calls)
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

	var sawEscape, sawNudge bool
	for _, call := range sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "take this now" {
			sawNudge = true
		}
	}
	if !sawEscape || !sawNudge {
		t.Fatalf("calls = %#v, want SendKeys(Escape) + NudgeNow", sp.Calls)
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

	var sawInterrupt, sawWaitForIdle, sawNudge, sawStop bool
	for _, call := range sp.Calls {
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
		}
		if call.Method == "WaitForIdle" && call.Name == info.SessionName {
			sawWaitForIdle = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "replace the current turn" {
			sawNudge = true
		}
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
	}
	if !sawInterrupt || !sawWaitForIdle || !sawNudge {
		t.Fatalf("calls = %#v, want interrupt + WaitForIdle + nudge", sp.Calls)
	}
	if sawStop {
		t.Fatalf("calls = %#v, did not want Stop for claude interrupt_now", sp.Calls)
	}
}

func TestSubmitInterruptNowReturnsWaitForIdleErrorForClaude(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sp.WaitForIdleErrors = map[string]error{}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", t.TempDir(), "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.WaitForIdleErrors[info.SessionName] = fmt.Errorf("not idle yet")

	if _, err := mgr.Submit(context.Background(), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow); err == nil || !strings.Contains(err.Error(), "waiting for interrupted session to become idle") {
		t.Fatalf("Submit(interrupt_now) error = %v, want idle wait failure", err)
	}
}

func TestStopTurnUsesSoftEscapeForCodex(t *testing.T) {
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

	var sawEscape, sawInterrupt bool
	for _, call := range sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
		}
	}
	if !sawEscape {
		t.Fatalf("calls = %#v, want SendKeys(Escape)", sp.Calls)
	}
	if sawInterrupt {
		t.Fatalf("calls = %#v, did not want Interrupt for codex stop", sp.Calls)
	}
}
