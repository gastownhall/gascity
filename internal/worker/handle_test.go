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
