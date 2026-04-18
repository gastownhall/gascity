package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

func TestSessionHandleStartStopState(t *testing.T) {
	handle, store, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	state, err := handle.State(context.Background())
	if err != nil {
		t.Fatalf("State(before start): %v", err)
	}
	if state.Phase != PhaseStopped {
		t.Fatalf("State(before start) = %s, want %s", state.Phase, PhaseStopped)
	}

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if handle.sessionID == "" {
		t.Fatal("sessionID is empty after Start")
	}

	bead, err := store.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", handle.sessionID, err)
	}
	if bead.Metadata["state"] != string(sessionpkg.StateActive) {
		t.Fatalf("bead state = %q, want %q", bead.Metadata["state"], sessionpkg.StateActive)
	}
	if bead.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", bead.Metadata["pending_create_claim"])
	}
	if bead.Metadata["worker_profile_provider_family"] != "claude" {
		t.Fatalf("worker_profile_provider_family = %q, want claude", bead.Metadata["worker_profile_provider_family"])
	}
	if bead.Metadata["worker_profile_transport_class"] != "tmux-cli" {
		t.Fatalf("worker_profile_transport_class = %q, want tmux-cli", bead.Metadata["worker_profile_transport_class"])
	}
	if bead.Metadata["worker_profile_compatibility_version"] == "" {
		t.Fatal("worker_profile_compatibility_version is empty")
	}
	if bead.Metadata["worker_profile_certification_fingerprint"] == "" {
		t.Fatal("worker_profile_certification_fingerprint is empty")
	}

	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}

	state, err = handle.State(context.Background())
	if err != nil {
		t.Fatalf("State(after start): %v", err)
	}
	if state.Phase != PhaseReady {
		t.Fatalf("State(after start) = %s, want %s", state.Phase, PhaseReady)
	}
	if state.SessionID != handle.sessionID {
		t.Fatalf("State.SessionID = %q, want %q", state.SessionID, handle.sessionID)
	}
	if state.SessionName != info.SessionName {
		t.Fatalf("State.SessionName = %q, want %q", state.SessionName, info.SessionName)
	}

	if err := handle.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	callCount := len(sp.Calls)
	state, err = handle.State(context.Background())
	if err != nil {
		t.Fatalf("State(after stop): %v", err)
	}
	if state.Phase != PhaseStopped {
		t.Fatalf("State(after stop) = %s, want %s", state.Phase, PhaseStopped)
	}
	for _, call := range sp.Calls[callCount:] {
		if call.Method == "Pending" {
			t.Fatalf("State(after stop) probed Pending on a stopped session: %#v", sp.Calls[callCount:])
		}
	}
}

func TestSessionHandleAttachUsesWorkerBoundary(t *testing.T) {
	handle, store, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	info, err := handle.Create(context.Background(), CreateModeDeferred)
	if err != nil {
		t.Fatalf("Create(deferred): %v", err)
	}
	if err := handle.Attach(context.Background()); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", info.ID, err)
	}
	if bead.Metadata["state"] != string(sessionpkg.StateActive) {
		t.Fatalf("bead state = %q, want %q", bead.Metadata["state"], sessionpkg.StateActive)
	}

	updated, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", info.ID, err)
	}

	start := firstCall(sp.Calls, "Start")
	if start == nil {
		t.Fatalf("runtime calls = %#v, want Start", sp.Calls)
	}
	if start.Name != updated.SessionName {
		t.Fatalf("Start name = %q, want %q", start.Name, updated.SessionName)
	}
	attach := firstCall(sp.Calls, "Attach")
	if attach == nil {
		t.Fatalf("runtime calls = %#v, want Attach", sp.Calls)
	}
	if attach.Name != updated.SessionName {
		t.Fatalf("Attach name = %q, want %q", attach.Name, updated.SessionName)
	}
	if attachIndex(sp.Calls, "Start") > attachIndex(sp.Calls, "Attach") {
		t.Fatalf("runtime call order = %#v, want Start before Attach", sp.Calls)
	}
}

func TestSessionHandleCreateDeferred(t *testing.T) {
	handle, store, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
		Metadata: map[string]string{
			"session_origin": "ephemeral",
		},
	})

	info, err := handle.Create(context.Background(), CreateModeDeferred)
	if err != nil {
		t.Fatalf("Create(deferred): %v", err)
	}
	if info.ID == "" {
		t.Fatal("Create(deferred) returned empty ID")
	}
	if handle.sessionID != info.ID {
		t.Fatalf("sessionID = %q, want %q", handle.sessionID, info.ID)
	}

	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", info.ID, err)
	}
	if bead.Metadata["state"] != string(sessionpkg.StateCreating) {
		t.Fatalf("bead state = %q, want %q", bead.Metadata["state"], sessionpkg.StateCreating)
	}
	if bead.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want true", bead.Metadata["pending_create_claim"])
	}
	if bead.Metadata["session_origin"] != "ephemeral" {
		t.Fatalf("session_origin = %q, want ephemeral", bead.Metadata["session_origin"])
	}
	if bead.Metadata["worker_profile_provider_family"] != "claude" {
		t.Fatalf("worker_profile_provider_family = %q, want claude", bead.Metadata["worker_profile_provider_family"])
	}
	if len(sp.Calls) != 0 {
		t.Fatalf("runtime calls = %#v, want none for deferred create", sp.Calls)
	}
}

func TestSessionHandleCreateStarted(t *testing.T) {
	handle, store, sp, _ := newTestSessionHandle(t, SessionSpec{
		Template: "probe",
		Title:    "Probe",
		Command:  "claude --dangerously-skip-permissions",
		WorkDir:  t.TempDir(),
		Provider: "claude",
		Env: map[string]string{
			"EXTRA_ENV": "present",
		},
		Hints: runtime.Config{
			ReadyDelayMs: 1234,
		},
		Metadata: map[string]string{
			"session_origin": "manual",
		},
	})

	info, err := handle.Create(context.Background(), CreateModeStarted)
	if err != nil {
		t.Fatalf("Create(started): %v", err)
	}
	if handle.sessionID != info.ID {
		t.Fatalf("sessionID = %q, want %q", handle.sessionID, info.ID)
	}

	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", info.ID, err)
	}
	if bead.Metadata["state"] != string(sessionpkg.StateActive) {
		t.Fatalf("bead state = %q, want %q", bead.Metadata["state"], sessionpkg.StateActive)
	}
	if bead.Metadata["session_origin"] != "manual" {
		t.Fatalf("session_origin = %q, want manual", bead.Metadata["session_origin"])
	}

	start := firstCall(sp.Calls, "Start")
	if start == nil {
		t.Fatalf("runtime calls = %#v, want Start", sp.Calls)
	}
	if start.Config.Command != "claude --dangerously-skip-permissions" {
		t.Fatalf("start command = %q, want command", start.Config.Command)
	}
	if start.Config.WorkDir != handle.session.WorkDir {
		t.Fatalf("start workdir = %q, want %q", start.Config.WorkDir, handle.session.WorkDir)
	}
	if start.Config.Env["EXTRA_ENV"] != "present" {
		t.Fatalf("start env EXTRA_ENV = %q, want present", start.Config.Env["EXTRA_ENV"])
	}
	if start.Config.ReadyDelayMs != 1234 {
		t.Fatalf("start ReadyDelayMs = %d, want 1234", start.Config.ReadyDelayMs)
	}

	again, err := handle.Create(context.Background(), CreateModeStarted)
	if err != nil {
		t.Fatalf("Create(started second): %v", err)
	}
	if again.ID != info.ID {
		t.Fatalf("Create(started second) ID = %q, want %q", again.ID, info.ID)
	}
	startCalls := 0
	for _, call := range sp.Calls {
		if call.Method == "Start" {
			startCalls++
		}
	}
	if startCalls != 1 {
		t.Fatalf("Start call count = %d, want 1", startCalls)
	}
}

func TestSessionHandleKillUsesWorkerBoundary(t *testing.T) {
	handle, store, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}

	sp.Calls = nil
	if err := handle.Kill(context.Background()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	stop := firstCall(sp.Calls, "Stop")
	if stop == nil || stop.Name != info.SessionName {
		t.Fatalf("runtime calls = %#v, want Stop %q", sp.Calls, info.SessionName)
	}

	bead, err := store.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", handle.sessionID, err)
	}
	if bead.Metadata["state"] != string(sessionpkg.StateActive) {
		t.Fatalf("bead state = %q, want %q after Kill", bead.Metadata["state"], sessionpkg.StateActive)
	}
}

func TestSessionHandleCloseUsesWorkerBoundary(t *testing.T) {
	handle, store, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}

	sp.Calls = nil
	if err := handle.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stop := firstCall(sp.Calls, "Stop")
	if stop == nil || stop.Name != info.SessionName {
		t.Fatalf("runtime calls = %#v, want Stop %q", sp.Calls, info.SessionName)
	}

	bead, err := store.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", handle.sessionID, err)
	}
	if bead.Status != "closed" {
		t.Fatalf("bead status = %q, want closed", bead.Status)
	}
}

func TestSessionHandleRenameUsesWorkerBoundary(t *testing.T) {
	handle, _, _, mgr := newTestSessionHandle(t, SessionSpec{
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if _, err := handle.Create(context.Background(), CreateModeDeferred); err != nil {
		t.Fatalf("Create(deferred): %v", err)
	}
	if err := handle.Rename(context.Background(), "Renamed Session"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}
	if info.Title != "Renamed Session" {
		t.Fatalf("title = %q, want Renamed Session", info.Title)
	}
}

func TestSessionHandlePeekUsesWorkerBoundary(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}
	if sp.PeekOutput == nil {
		sp.PeekOutput = map[string]string{}
	}
	sp.PeekOutput[info.SessionName] = "recent output"
	sp.Calls = nil

	output, err := handle.Peek(context.Background(), 25)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if output != "recent output" {
		t.Fatalf("Peek output = %q, want recent output", output)
	}
	peek := firstCall(sp.Calls, "Peek")
	if peek == nil || peek.Name != info.SessionName {
		t.Fatalf("runtime calls = %#v, want Peek %q", sp.Calls, info.SessionName)
	}
}

func TestCanonicalProfileIdentity(t *testing.T) {
	identity, ok := CanonicalProfileIdentity(ProfileCodexTmuxCLI)
	if !ok {
		t.Fatal("CanonicalProfileIdentity(ProfileCodexTmuxCLI) = false, want true")
	}
	if identity.ProviderFamily != "codex" {
		t.Fatalf("ProviderFamily = %q, want codex", identity.ProviderFamily)
	}
	if identity.TransportClass != "tmux-cli" {
		t.Fatalf("TransportClass = %q, want tmux-cli", identity.TransportClass)
	}
	if identity.BehaviorClaimsVersion == "" {
		t.Fatal("BehaviorClaimsVersion is empty")
	}
	if identity.TranscriptAdapterVersion == "" {
		t.Fatal("TranscriptAdapterVersion is empty")
	}
	if identity.CompatibilityVersion == "" {
		t.Fatal("CompatibilityVersion is empty")
	}
	if identity.CertificationFingerprint == "" {
		t.Fatal("CertificationFingerprint is empty")
	}
	repeat, ok := CanonicalProfileIdentity(ProfileCodexTmuxCLI)
	if !ok {
		t.Fatal("CanonicalProfileIdentity(ProfileCodexTmuxCLI) repeat = false, want true")
	}
	if identity.CertificationFingerprint != repeat.CertificationFingerprint {
		t.Fatalf("CertificationFingerprint = %q, want stable %q", repeat.CertificationFingerprint, identity.CertificationFingerprint)
	}
}

func TestSessionHandleMessageInterruptNowUsesWorkerBoundary(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}
	sp.WaitForIdleErrors[info.SessionName] = nil

	startCalls := len(sp.Calls)
	if _, err := handle.Message(context.Background(), MessageRequest{
		Text:     "replacement task",
		Delivery: DeliveryIntentInterruptNow,
	}); err != nil {
		t.Fatalf("Message(interrupt_now): %v", err)
	}

	calls := sp.Calls[startCalls:]
	methods := make([]string, 0, len(calls))
	for _, call := range calls {
		methods = append(methods, call.Method)
	}
	want := []string{"IsRunning", "Interrupt", "WaitForIdle", "SendKeys", "Pending", "NudgeNow"}
	if !containsSubsequence(methods, want) {
		t.Fatalf("methods = %v, want subsequence %v", methods, want)
	}
	if !hasCall(calls, "SendKeys", "C-u") {
		t.Fatalf("calls = %#v, want SendKeys C-u", calls)
	}
}

func TestSessionHandleNudgeImmediateUsesWorkerBoundary(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}

	startCalls := len(sp.Calls)
	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "check deploy status",
		Delivery: NudgeDeliveryImmediate,
	})
	if err != nil {
		t.Fatalf("Nudge(immediate): %v", err)
	}
	if !result.Delivered {
		t.Fatal("Nudge(immediate) Delivered = false, want true")
	}

	calls := sp.Calls[startCalls:]
	if !hasCall(calls, "NudgeNow", "check deploy status") {
		t.Fatalf("calls = %#v, want immediate nudge", calls)
	}
	if firstCall(calls, "Nudge") != nil {
		t.Fatalf("calls = %#v, want NudgeNow without fallback Nudge", calls)
	}
	if firstCall(calls, "Pending") == nil {
		t.Fatalf("calls = %#v, want pending interaction probe before nudge", calls)
	}
	if info.SessionName == "" {
		t.Fatal("SessionName is empty")
	}
}

func TestSessionHandleNudgeWaitIdleUsesWorkerBoundary(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}
	sp.WaitForIdleErrors[info.SessionName] = nil

	startCalls := len(sp.Calls)
	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "check deploy status",
		Delivery: NudgeDeliveryWaitIdle,
		Source:   "mail",
	})
	if err != nil {
		t.Fatalf("Nudge(wait_idle): %v", err)
	}
	if !result.Delivered {
		t.Fatal("Nudge(wait_idle) Delivered = false, want true")
	}

	calls := sp.Calls[startCalls:]
	methods := make([]string, 0, len(calls))
	for _, call := range calls {
		methods = append(methods, call.Method)
	}
	if !containsSubsequence(methods, []string{"IsRunning", "WaitForIdle", "NudgeNow"}) {
		t.Fatalf("methods = %v, want IsRunning -> WaitForIdle -> NudgeNow", methods)
	}
	nudge := firstCall(calls, "NudgeNow")
	if nudge == nil {
		t.Fatalf("calls = %#v, want NudgeNow", calls)
	}
	if !strings.Contains(nudge.Message, "<system-reminder>") {
		t.Fatalf("delivered message = %q, want system-reminder wrapper", nudge.Message)
	}
	if !strings.Contains(nudge.Message, "[mail] check deploy status") {
		t.Fatalf("delivered message = %q, want source-tagged reminder content", nudge.Message)
	}
}

func TestSessionHandleNudgeWaitIdleReturnsUndeliveredForUnsupportedProvider(t *testing.T) {
	handle, _, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileCodexTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "codex",
		WorkDir:  t.TempDir(),
		Provider: "codex",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	startCalls := len(sp.Calls)
	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "check deploy status",
		Delivery: NudgeDeliveryWaitIdle,
	})
	if err != nil {
		t.Fatalf("Nudge(wait_idle): %v", err)
	}
	if result.Delivered {
		t.Fatal("Nudge(wait_idle) Delivered = true, want false for unsupported provider")
	}

	for _, call := range sp.Calls[startCalls:] {
		if call.Method == "WaitForIdle" || call.Method == "Nudge" || call.Method == "NudgeNow" {
			t.Fatalf("calls = %#v, want no live wait-idle delivery on unsupported provider", sp.Calls[startCalls:])
		}
	}
}

func TestSessionHandleLiveObservationUsesProviderRuntimeState(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}
	if err := sp.Stop(info.SessionName); err != nil {
		t.Fatalf("runtime.Stop(%q): %v", info.SessionName, err)
	}

	obs, err := ObserveHandle(context.Background(), handle)
	if err != nil {
		t.Fatalf("ObserveHandle: %v", err)
	}
	if obs.Running {
		t.Fatalf("LiveObservation.Running = true, want false after runtime stop; obs=%#v", obs)
	}
	if obs.Alive {
		t.Fatalf("LiveObservation.Alive = true, want false after runtime stop; obs=%#v", obs)
	}
}

func TestSessionHandleLiveObservationTracksProcessLiveness(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
		Hints: runtime.Config{
			ProcessNames: []string{"claude"},
		},
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}
	sp.Zombies[info.SessionName] = true

	obs, err := ObserveHandle(context.Background(), handle)
	if err != nil {
		t.Fatalf("ObserveHandle: %v", err)
	}
	if !obs.Running {
		t.Fatalf("LiveObservation.Running = false, want true while tmux session still exists; obs=%#v", obs)
	}
	if obs.Alive {
		t.Fatalf("LiveObservation.Alive = true, want false for zombie process; obs=%#v", obs)
	}
}

func TestSessionHandleNudgeWaitIdleLiveOnlyDoesNotResumeStoppedSession(t *testing.T) {
	handle, _, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if _, err := handle.Create(context.Background(), CreateModeDeferred); err != nil {
		t.Fatalf("Create(deferred): %v", err)
	}

	startCalls := len(sp.Calls)
	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "queued reminder",
		Delivery: NudgeDeliveryWaitIdle,
		Source:   "mail",
		Wake:     NudgeWakeLiveOnly,
	})
	if err != nil {
		t.Fatalf("Nudge(wait_idle live_only): %v", err)
	}
	if result.Delivered {
		t.Fatal("Nudge(wait_idle live_only) Delivered = true, want false when runtime is stopped")
	}
	for _, call := range sp.Calls[startCalls:] {
		if call.Method == "Start" || call.Method == "WaitForIdle" || call.Method == "Nudge" || call.Method == "NudgeNow" {
			t.Fatalf("calls = %#v, want no live delivery or wake on stopped session", sp.Calls[startCalls:])
		}
	}
}

func TestSessionHandlePendingRespondAndBlockedState(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileCodexTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "codex",
		WorkDir:  t.TempDir(),
		Provider: "codex",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}

	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "Allow read?",
		Options:   []string{"approve", "deny"},
		Metadata:  map[string]string{"tool": "Read"},
	})

	pendingStatus, supported, err := handle.PendingStatus(context.Background())
	if err != nil {
		t.Fatalf("PendingStatus: %v", err)
	}
	if !supported {
		t.Fatal("PendingStatus supported = false, want true")
	}
	if pendingStatus == nil || pendingStatus.RequestID != "req-1" {
		t.Fatalf("PendingStatus() = %#v, want request req-1", pendingStatus)
	}

	pending, err := handle.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending == nil || pending.RequestID != "req-1" {
		t.Fatalf("Pending() = %#v, want request req-1", pending)
	}

	state, err := handle.State(context.Background())
	if err != nil {
		t.Fatalf("State(blocked): %v", err)
	}
	if state.Phase != PhaseBlocked {
		t.Fatalf("State(blocked) = %s, want %s", state.Phase, PhaseBlocked)
	}
	if state.Pending == nil || state.Pending.RequestID != "req-1" {
		t.Fatalf("State.Pending = %#v, want req-1", state.Pending)
	}

	if err := handle.Respond(context.Background(), InteractionResponse{
		Action: "approve",
		Text:   "continue",
	}); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	state, err = handle.State(context.Background())
	if err != nil {
		t.Fatalf("State(after respond): %v", err)
	}
	if state.Phase != PhaseReady {
		t.Fatalf("State(after respond) = %s, want %s", state.Phase, PhaseReady)
	}
}

func TestSessionHandleHistoryLoadsNormalizedTranscript(t *testing.T) {
	handle, _, _, _ := newTestSessionHandle(t, SessionSpec{
		ID:       "",
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  "/tmp/gascity/phase1/claude",
		Provider: "claude",
	})
	handle.adapter.SearchPaths = []string{
		filepath.Join("workertest", "testdata", "fixtures", "claude", "fresh"),
	}

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	history, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if history == nil {
		t.Fatal("History() returned nil snapshot")
	}
	if len(history.Entries) == 0 {
		t.Fatal("History().Entries is empty")
	}
	if history.LogicalConversationID == "" {
		t.Fatal("History().LogicalConversationID is empty")
	}
	if history.TranscriptStreamID == "" {
		t.Fatal("History().TranscriptStreamID is empty")
	}
}

func TestSessionHandleHistoryPersistsCodexResumeKeyForLaterRestart(t *testing.T) {
	base := t.TempDir()
	dayDir := filepath.Join(base, "2026", "04", "14")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatalf("mkdir dayDir: %v", err)
	}

	workDir := "/tmp/codex-project"
	resumeID := "019d8afb-efe8-7280-abf9-5901fd92e0cd"
	transcriptPath := filepath.Join(dayDir, "rollout-2026-04-14T09-54-20-"+resumeID+".jsonl")
	transcript := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":"2026-04-14T09:54:20Z","type":"session_meta","payload":{"cwd":%q}}`, workDir),
		`{"timestamp":"2026-04-14T09:54:21Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"remember alpha"}]}}`,
		`{"timestamp":"2026-04-14T09:54:22Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"remembered"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	handle, store, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileCodexTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "codex --dangerously-bypass-approvals-and-sandbox",
		WorkDir:  workDir,
		Provider: "codex",
		Resume: sessionpkg.ProviderResume{
			ResumeFlag:  "resume",
			ResumeStyle: "subcommand",
		},
	})
	handle.adapter.SearchPaths = []string{base}

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start(first): %v", err)
	}

	history, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if history.GCSessionID != resumeID {
		t.Fatalf("History().GCSessionID = %q, want %q", history.GCSessionID, resumeID)
	}
	if history.LogicalConversationID != resumeID {
		t.Fatalf("History().LogicalConversationID = %q, want %q", history.LogicalConversationID, resumeID)
	}

	bead, err := store.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", handle.sessionID, err)
	}
	if bead.Metadata["session_key"] != resumeID {
		t.Fatalf("session_key = %q, want %q", bead.Metadata["session_key"], resumeID)
	}

	if err := handle.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start(second): %v", err)
	}

	secondStart := lastCall(sp.Calls, "Start")
	if secondStart == nil {
		t.Fatalf("runtime calls = %#v, want second Start", sp.Calls)
	}
	wantResume := "codex resume " + resumeID
	if !strings.Contains(secondStart.Config.Command, wantResume) {
		t.Fatalf("second start command = %q, want %q", secondStart.Config.Command, wantResume)
	}
}

func TestSessionHandleAgentMappingsAndTranscriptUseWorkerBoundary(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "claude-project")
	handle, _, _, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
	})
	handle.adapter.SearchPaths = []string{base}

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}

	slugDir := filepath.Join(base, sessionlog.ProjectSlug(workDir))
	parentPath := filepath.Join(slugDir, info.SessionKey+".jsonl")
	if err := os.MkdirAll(filepath.Join(slugDir, info.SessionKey, "subagents"), 0o755); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	parentContent := `{"uuid":"u1","type":"user","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(parentPath, []byte(parentContent), 0o644); err != nil {
		t.Fatalf("write parent transcript: %v", err)
	}
	agentPath := filepath.Join(slugDir, info.SessionKey, "subagents", "agent-helper.jsonl")
	agentContent := strings.Join([]string{
		`{"uuid":"a1","type":"system","parentToolUseId":"toolu_123"}`,
		`{"uuid":"a2","parentUuid":"a1","type":"assistant","message":{"role":"assistant","content":"working"}}`,
		`{"uuid":"a3","parentUuid":"a2","type":"result","message":{"role":"result"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
		t.Fatalf("write agent transcript: %v", err)
	}

	mappings, err := handle.AgentMappings(context.Background())
	if err != nil {
		t.Fatalf("AgentMappings: %v", err)
	}
	if len(mappings) != 1 {
		t.Fatalf("len(AgentMappings) = %d, want 1", len(mappings))
	}
	if mappings[0].AgentID != "helper" {
		t.Fatalf("AgentMappings()[0].AgentID = %q, want helper", mappings[0].AgentID)
	}
	if mappings[0].ParentToolUseID != "toolu_123" {
		t.Fatalf("AgentMappings()[0].ParentToolUseID = %q, want toolu_123", mappings[0].ParentToolUseID)
	}

	agentSession, err := handle.AgentTranscript(context.Background(), "helper")
	if err != nil {
		t.Fatalf("AgentTranscript: %v", err)
	}
	if agentSession == nil || agentSession.Session == nil {
		t.Fatal("AgentTranscript returned nil session")
	}
	if agentSession.Session.Status != sessionlog.AgentStatusCompleted {
		t.Fatalf("AgentTranscript().Session.Status = %q, want %q", agentSession.Session.Status, sessionlog.AgentStatusCompleted)
	}
	if len(agentSession.RawMessages) != 3 {
		t.Fatalf("len(AgentTranscript().RawMessages) = %d, want 3", len(agentSession.RawMessages))
	}
}

func TestRuntimeHandleUsesWorkerBoundaryForLegacyRuntimeSession(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "legacy-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.SetPendingInteraction("legacy-worker", &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "Proceed?",
	})

	handle, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  "legacy-worker",
		ProviderName: "stub",
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}

	state, err := handle.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Phase != PhaseBlocked {
		t.Fatalf("State().Phase = %s, want %s", state.Phase, PhaseBlocked)
	}
	if state.Pending == nil || state.Pending.RequestID != "req-1" {
		t.Fatalf("State().Pending = %#v, want req-1", state.Pending)
	}

	if err := handle.Interrupt(context.Background(), InterruptRequest{}); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if err := handle.Kill(context.Background()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if sp.IsRunning("legacy-worker") {
		t.Fatal("legacy-worker should be stopped after Kill")
	}
}

func TestRuntimeHandleNudgeImmediateUsesImmediateProvider(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "legacy-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	handle, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  "legacy-worker",
		ProviderName: "codex",
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "check deploy status",
		Delivery: NudgeDeliveryImmediate,
	})
	if err != nil {
		t.Fatalf("Nudge(immediate): %v", err)
	}
	if !result.Delivered {
		t.Fatal("Nudge(immediate) Delivered = false, want true")
	}

	var nudgeNow, nudge int
	for _, call := range sp.Calls {
		switch call.Method {
		case "NudgeNow":
			nudgeNow++
		case "Nudge":
			nudge++
		}
	}
	if nudgeNow != 1 {
		t.Fatalf("NudgeNow calls = %d, want 1", nudgeNow)
	}
	if nudge != 0 {
		t.Fatalf("Nudge calls = %d, want 0", nudge)
	}
}

func TestRuntimeHandleNudgeWaitIdleClaudeWrapsReminder(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "legacy-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.WaitForIdleErrors["legacy-worker"] = nil

	handle, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  "legacy-worker",
		ProviderName: "claude",
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "check deploy status",
		Delivery: NudgeDeliveryWaitIdle,
		Source:   "mail",
	})
	if err != nil {
		t.Fatalf("Nudge(wait_idle): %v", err)
	}
	if !result.Delivered {
		t.Fatal("Nudge(wait_idle) Delivered = false, want true")
	}

	var waitCalls, nudgeNow int
	var delivered string
	for _, call := range sp.Calls {
		switch call.Method {
		case "WaitForIdle":
			waitCalls++
		case "NudgeNow":
			nudgeNow++
			delivered = call.Message
		}
	}
	if waitCalls != 1 {
		t.Fatalf("WaitForIdle calls = %d, want 1", waitCalls)
	}
	if nudgeNow != 1 {
		t.Fatalf("NudgeNow calls = %d, want 1", nudgeNow)
	}
	if !strings.Contains(delivered, "<system-reminder>") {
		t.Fatalf("delivered message = %q, want system reminder", delivered)
	}
	if !strings.Contains(delivered, "[mail] check deploy status") {
		t.Fatalf("delivered message = %q, want mail-tagged reminder", delivered)
	}
}

func TestRuntimeHandleNudgeWaitIdleUnsupportedProviderReturnsUndelivered(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "legacy-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	handle, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  "legacy-worker",
		ProviderName: "codex",
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "check deploy status",
		Delivery: NudgeDeliveryWaitIdle,
	})
	if err != nil {
		t.Fatalf("Nudge(wait_idle): %v", err)
	}
	if result.Delivered {
		t.Fatal("Nudge(wait_idle) Delivered = true, want false for unsupported provider")
	}
	for _, call := range sp.Calls {
		if call.Method == "WaitForIdle" || call.Method == "Nudge" || call.Method == "NudgeNow" {
			t.Fatalf("calls = %#v, want no delivery for unsupported provider", sp.Calls)
		}
	}
}

func TestSessionCatalogUsesWorkerBoundary(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := sessionpkg.NewManagerWithCityPath(store, sp, t.TempDir())
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager: mgr,
		Session: SessionSpec{
			Profile:  ProfileClaudeTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "claude",
			WorkDir:  t.TempDir(),
			Provider: "claude",
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	info, err := handle.Create(context.Background(), CreateModeDeferred)
	if err != nil {
		t.Fatalf("Create(deferred): %v", err)
	}

	catalog, err := NewSessionCatalog(mgr)
	if err != nil {
		t.Fatalf("NewSessionCatalog: %v", err)
	}

	got, err := catalog.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != info.ID {
		t.Fatalf("Get().ID = %q, want %q", got.ID, info.ID)
	}

	caps, err := catalog.SubmissionCapabilities(info.ID)
	if err != nil {
		t.Fatalf("SubmissionCapabilities: %v", err)
	}
	if !caps.SupportsFollowUp {
		t.Fatal("SupportsFollowUp = false, want true")
	}
	if !caps.SupportsInterruptNow {
		t.Fatal("SupportsInterruptNow = false, want true")
	}

	title := "Renamed Session"
	alias := "renamed-session"
	if err := catalog.UpdatePresentation(info.ID, &title, &alias); err != nil {
		t.Fatalf("UpdatePresentation: %v", err)
	}
	updated, err := catalog.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(after update): %v", err)
	}
	if updated.Title != title {
		t.Fatalf("updated.Title = %q, want %q", updated.Title, title)
	}
	if updated.Alias != alias {
		t.Fatalf("updated.Alias = %q, want %q", updated.Alias, alias)
	}
}

func TestSessionHandleHistoryStitchesGeminiRotatedTranscriptAcrossRestart(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}

	searchRoot := filepath.Join(base, ".gemini", "tmp")
	projectDir := filepath.Join(searchRoot, "project-a")
	chatsDir := filepath.Join(projectDir, "chats")
	for _, dir := range []string{searchRoot, projectDir, chatsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatalf("write .project_root: %v", err)
	}

	firstTranscript := filepath.Join(chatsDir, "session-2026-04-17T03-12-before.json")
	writeGeminiHistoryFixture(t, firstTranscript, "before-session", []string{
		`{"id":"u1","timestamp":"2026-04-17T03:12:00Z","type":"user","content":"remember alpha"}`,
		`{"id":"a1","timestamp":"2026-04-17T03:12:01Z","type":"gemini","content":"remembered alpha"}`,
	})
	firstTime := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(firstTranscript, firstTime, firstTime); err != nil {
		t.Fatalf("chtimes(first transcript): %v", err)
	}

	handle, _, _, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileGeminiTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "gemini",
		WorkDir:  workDir,
		Provider: "gemini",
	})
	handle.adapter.SearchPaths = []string{searchRoot}

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	before, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatalf("History(before): %v", err)
	}
	if before.TranscriptStreamID != firstTranscript {
		t.Fatalf("History(before).TranscriptStreamID = %q, want %q", before.TranscriptStreamID, firstTranscript)
	}
	if got := len(before.Entries); got != 2 {
		t.Fatalf("len(History(before).Entries) = %d, want 2", got)
	}

	secondTranscript := filepath.Join(chatsDir, "session-2026-04-17T03-15-after.json")
	writeGeminiHistoryFixture(t, secondTranscript, "after-session", []string{
		`{"id":"u2","timestamp":"2026-04-17T03:15:00Z","type":"user","content":"recall the earlier phrase"}`,
		`{"id":"a2","timestamp":"2026-04-17T03:15:01Z","type":"gemini","content":"alpha"}`,
	})
	secondTime := time.Now().Add(-1 * time.Minute)
	if err := os.Chtimes(secondTranscript, secondTime, secondTime); err != nil {
		t.Fatalf("chtimes(second transcript): %v", err)
	}

	after, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatalf("History(after): %v", err)
	}
	if after.TranscriptStreamID != secondTranscript {
		t.Fatalf("History(after).TranscriptStreamID = %q, want %q", after.TranscriptStreamID, secondTranscript)
	}
	if got := len(after.Entries); got != 4 {
		t.Fatalf("len(History(after).Entries) = %d, want 4", got)
	}
	if after.Entries[0].Text != "remember alpha" || after.Entries[1].Text != "remembered alpha" {
		t.Fatalf("History(after).Entries[:2] = %+v, want preserved first transcript history", after.Entries[:2])
	}
	if after.Entries[2].Text != "recall the earlier phrase" || after.Entries[3].Text != "alpha" {
		t.Fatalf("History(after).Entries[2:] = %+v, want resumed transcript tail", after.Entries[2:])
	}

	repeat, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatalf("History(repeat): %v", err)
	}
	if got := len(repeat.Entries); got != 4 {
		t.Fatalf("len(History(repeat).Entries) = %d, want stable stitched length 4", got)
	}
}

func TestSessionHandleStartPassesSessionEnv(t *testing.T) {
	handle, _, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileGeminiTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "gemini",
		WorkDir:  t.TempDir(),
		Provider: "gemini",
		Env: map[string]string{
			"CUSTOM_WORKER_ENV": "present",
		},
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var start *runtime.Call
	for i := range sp.Calls {
		if sp.Calls[i].Method == "Start" {
			start = &sp.Calls[i]
			break
		}
	}
	if start == nil {
		t.Fatalf("runtime calls = %#v, want a Start call", sp.Calls)
	}
	if got := start.Config.Env["CUSTOM_WORKER_ENV"]; got != "present" {
		t.Fatalf("Start env CUSTOM_WORKER_ENV = %q, want present", got)
	}
}

func TestSessionHandleStartResolvedUsesProvidedRuntime(t *testing.T) {
	handle, _, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileGeminiTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "gemini",
		WorkDir:  t.TempDir(),
		Provider: "gemini",
	})

	resolved := runtime.Config{
		Command: "gemini --resume existing-session",
		WorkDir: t.TempDir(),
		Env: map[string]string{
			"GC_WORKER_BOUNDARY": "start_resolved",
		},
	}
	if err := handle.StartResolved(context.Background(), resolved.Command, resolved); err != nil {
		t.Fatalf("StartResolved: %v", err)
	}

	start := firstCall(sp.Calls, "Start")
	if start == nil {
		t.Fatalf("runtime calls = %#v, want Start call", sp.Calls)
	}
	if got := start.Config.Command; got != resolved.Command {
		t.Fatalf("StartResolved command = %q, want %q", got, resolved.Command)
	}
	if got := start.Config.Env["GC_WORKER_BOUNDARY"]; got != "start_resolved" {
		t.Fatalf("StartResolved env GC_WORKER_BOUNDARY = %q, want start_resolved", got)
	}
}

func TestSessionHandleStartUsesSessionIDOnFirstStartAndResumeAfterSuspend(t *testing.T) {
	handle, _, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude --dangerously-skip-permissions",
		WorkDir:  t.TempDir(),
		Provider: "claude",
		Resume: sessionpkg.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start(first): %v", err)
	}
	firstStart := firstCall(sp.Calls, "Start")
	if firstStart == nil {
		t.Fatalf("runtime calls = %#v, want initial Start", sp.Calls)
	}
	firstCommand := firstStart.Config.Command
	if !strings.Contains(firstCommand, "--session-id") {
		t.Fatalf("first start command = %q, want --session-id", firstCommand)
	}
	if strings.Contains(firstCommand, "--resume") {
		t.Fatalf("first start command = %q, want no --resume", firstCommand)
	}

	if err := handle.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start(second): %v", err)
	}
	if len(sp.Calls) < 3 {
		t.Fatalf("runtime calls = %#v, want second Start after Stop", sp.Calls)
	}
	secondStart := lastCall(sp.Calls, "Start")
	if secondStart == nil {
		t.Fatalf("runtime calls = %#v, want second Start", sp.Calls)
	}
	if !strings.Contains(secondStart.Config.Command, "--resume") {
		t.Fatalf("second start command = %q, want --resume", secondStart.Config.Command)
	}
}

func TestSessionHandleStartUsesCurrentResumeOverridesAfterSuspend(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)

	info, err := manager.Create(
		context.Background(),
		"probe",
		"Probe",
		"legacy-agent",
		t.TempDir(),
		"legacy-agent",
		nil,
		sessionpkg.ProviderResume{
			ResumeFlag:    "--old-resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		runtime.Config{},
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := manager.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager: manager,
		Session: SessionSpec{
			ID:       info.ID,
			Command:  "fresh-agent --new-flag",
			Provider: "fresh-agent",
			WorkDir:  info.WorkDir,
			Resume: sessionpkg.ProviderResume{
				ResumeFlag:    "--resume",
				ResumeStyle:   "flag",
				SessionIDFlag: "--session-id",
			},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}

	sp.Calls = nil
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	start := firstCall(sp.Calls, "Start")
	if start == nil {
		t.Fatalf("runtime calls = %#v, want Start", sp.Calls)
	}
	if strings.Contains(start.Config.Command, "--old-resume") {
		t.Fatalf("start command = %q, used stale resume flag", start.Config.Command)
	}
	if !strings.Contains(start.Config.Command, "fresh-agent --new-flag --resume "+info.SessionKey) {
		t.Fatalf("start command = %q, want current command and resume flag for %s", start.Config.Command, info.SessionKey)
	}
}

func newTestSessionHandle(t *testing.T, spec SessionSpec) (*SessionHandle, *beads.MemStore, *runtime.Fake, *sessionpkg.Manager) {
	t.Helper()

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager: manager,
		Session: spec,
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	return handle, store, sp, manager
}

func lastCall(calls []runtime.Call, method string) *runtime.Call {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == method {
			return &calls[i]
		}
	}
	return nil
}

func firstCall(calls []runtime.Call, method string) *runtime.Call {
	for i := range calls {
		if calls[i].Method == method {
			return &calls[i]
		}
	}
	return nil
}

func attachIndex(calls []runtime.Call, method string) int {
	for i := range calls {
		if calls[i].Method == method {
			return i
		}
	}
	return -1
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

func hasCall(calls []runtime.Call, method, message string) bool {
	for _, call := range calls {
		if call.Method == method && call.Message == message {
			return true
		}
	}
	return false
}

func writeGeminiHistoryFixture(t *testing.T, path, sessionID string, messages []string) {
	t.Helper()

	body := fmt.Sprintf("{\n  \"sessionId\": %q,\n  \"messages\": [\n    %s\n  ]\n}\n", sessionID, strings.Join(messages, ",\n    "))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write gemini transcript %s: %v", path, err)
	}
}
