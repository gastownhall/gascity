package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgepoller"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestProviderKind_PreferenceOrder exercises the metadata preference
// used to derive a session bead's family: builtin_ancestor > provider_kind
// > provider. This keeps wrapped custom aliases (e.g. claude-max with
// base = "builtin:claude", stamped as builtin_ancestor="claude" at
// session-bead creation) routed through the same claude-family branches
// as literal "claude".
func TestProviderKind_PreferenceOrder(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want string
	}{
		{
			name: "builtin_ancestor wins over provider_kind and provider",
			meta: map[string]string{
				"builtin_ancestor": "claude",
				"provider_kind":    "claude-max",
				"provider":         "claude-max",
			},
			want: "claude",
		},
		{
			name: "builtin_ancestor alias is normalized before provider_kind",
			meta: map[string]string{
				"builtin_ancestor": "my-pi/tmux",
				"provider_kind":    "codex",
				"provider":         "codex",
			},
			want: "pi",
		},
		{
			name: "provider_kind wins over provider when builtin_ancestor absent",
			meta: map[string]string{
				"provider_kind": "claude",
				"provider":      "custom-alias",
			},
			want: "claude",
		},
		{
			name: "provider_kind alias is normalized before provider fallback",
			meta: map[string]string{
				"provider_kind": "my-pi/tmux",
				"provider":      "codex",
			},
			want: "pi",
		},
		{
			name: "provider is the last-resort fallback",
			meta: map[string]string{
				"provider": "codex",
			},
			want: "codex",
		},
		{
			name: "provider fallback is normalized through transcript family",
			meta: map[string]string{
				"provider": "my-pi/tmux",
			},
			want: "pi",
		},
		{
			name: "empty builtin_ancestor falls through",
			meta: map[string]string{
				"builtin_ancestor": "",
				"provider_kind":    "gemini",
				"provider":         "raw",
			},
			want: "gemini",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerKind(beads.Bead{Metadata: tc.meta})
			if got != tc.want {
				t.Errorf("providerKind(%+v) = %q, want %q", tc.meta, got, tc.want)
			}
		})
	}
}

// TestWaitsForIdleAfterInterrupt_WrappedClaude verifies that a session
// bead whose builtin_ancestor = "claude" (e.g. claude-max wrapping the
// built-in) triggers the same wait-for-idle-after-interrupt branch that
// a literal "claude" session does.
func TestWaitsForIdleAfterInterrupt_WrappedClaude(t *testing.T) {
	wrapped := beads.Bead{Metadata: map[string]string{
		"builtin_ancestor": "claude",
		"provider":         "claude-max",
	}}
	if !waitsForIdleAfterInterrupt(wrapped) {
		t.Error("wrapped claude (builtin_ancestor=claude) should wait for idle after interrupt")
	}
	// Control: a wrapped codex must NOT trigger the claude-only branch.
	wrappedCodex := beads.Bead{Metadata: map[string]string{
		"builtin_ancestor": "codex",
		"provider":         "codex-mini",
	}}
	if waitsForIdleAfterInterrupt(wrappedCodex) {
		t.Error("wrapped codex (builtin_ancestor=codex) should not trigger claude-only branch")
	}
}

// TestProviderFamilyFromInfoMatchesMetadata is the byte-identical oracle for the
// ProviderFamilyFromInfo twin: for every representative provider-vocab shape, the
// Info form (fed infoFromPersistedBead(b)) must agree with the metadata form on
// the builtin_ancestor → provider_kind → provider precedence ladder. It is
// self-sufficient (asserts the concrete family output, not only Info==metadata),
// so mutating any precedence rung on either projection is caught here.
func TestProviderFamilyFromInfoMatchesMetadata(t *testing.T) {
	cases := []struct {
		name     string
		meta     map[string]string
		fallback string
		want     string
	}{
		{"empty-fallback-codex", map[string]string{}, "codex", "codex"},
		{"provider-only", map[string]string{"provider": "codex"}, "", "codex"},
		{"provider-kind-wins-over-provider", map[string]string{"provider": "claude", "provider_kind": "codex"}, "", "codex"},
		{"builtin-ancestor-wins", map[string]string{"provider": "claude", "provider_kind": "gemini", "builtin_ancestor": "codex"}, "", "codex"},
		{"wrapped-alias-provider", map[string]string{"provider": "my-pi"}, "", "pi"},
		{"blank-rungs-fall-through", map[string]string{"builtin_ancestor": "   ", "provider_kind": "", "provider": "codex"}, "", "codex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := beads.Bead{ID: "s", Type: BeadType, Status: "open", Labels: []string{LabelSession}, Metadata: tc.meta}
			fromMeta := ProviderFamilyFromMetadata(tc.meta, tc.fallback)
			fromInfo := ProviderFamilyFromInfo(infoFromPersistedBead(b), tc.fallback)
			if fromInfo != fromMeta {
				t.Errorf("ProviderFamilyFromInfo = %q, ProviderFamilyFromMetadata = %q (want equal)", fromInfo, fromMeta)
			}
			if fromInfo != tc.want {
				t.Errorf("ProviderFamilyFromInfo = %q, want %q", fromInfo, tc.want)
			}
		})
	}
}

func TestInterruptStrategyUsesPiProviderFamilyAlias(t *testing.T) {
	wrappedPi := beads.Bead{Metadata: map[string]string{
		"provider": "my-pi/tmux",
	}}
	if !requiresHardRestartInterrupt(wrappedPi) {
		t.Fatal("wrapped pi provider alias should use hard-restart interrupt")
	}
	if waitsForIdleAfterHardRestart(wrappedPi) {
		t.Fatal("wrapped pi provider alias should not wait for idle after hard restart")
	}
}

func TestSubmitDefaultResumesSuspendedClaudeSessionAndWaitsForIdleNudge(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "gemini", WorkDir: t.TempDir(), Provider: "gemini", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "gemini", WorkDir: t.TempDir(), Provider: "gemini", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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
	mgr := NewManagerWithOptions(store, sp)

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
	mgr := NewManagerWithOptions(store, sp, WithCityPath(cityPath))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, "alias", "helper-alias"); err != nil {
		t.Fatalf("SetMetadata(alias): %v", err)
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
	if item.Agent != "helper-alias" {
		t.Fatalf("Agent = %q, want helper-alias", item.Agent)
	}
	if item.Message != "follow up later" {
		t.Fatalf("Message = %q, want %q", item.Message, "follow up later")
	}
	if pollerCalls != 1 {
		t.Fatalf("pollerCalls = %d, want 1", pollerCalls)
	}
}

func TestEnsureSessionSubmitPollerRejectsGoTestExecutable(t *testing.T) {
	cityPath := t.TempDir()
	exe := filepath.Join(t.TempDir(), "session.test")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(test executable): %v", err)
	}

	origExecutable := sessionSubmitPollerExecutable
	sessionSubmitPollerExecutable = func() (string, error) {
		return exe, nil
	}
	defer func() { sessionSubmitPollerExecutable = origExecutable }()

	err := ensureSessionSubmitPoller(cityPath, "agent", "s-test")
	if err == nil || !strings.Contains(err.Error(), "Go test binary") {
		t.Fatalf("ensureSessionSubmitPoller error = %v, want Go test binary refusal", err)
	}
	if _, statErr := os.Stat(sessionSubmitPollerPIDPath(cityPath, "s-test", "agent")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("poller pid file stat error = %v, want not exist", statErr)
	}
	if _, statErr := os.Stat(sessionSubmitPollerLogPath(cityPath, "s-test", "agent")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("poller log file stat error = %v, want not exist", statErr)
	}
}

func TestExistingSessionSubmitPollerPIDRejectsUnrelatedLivePID(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("poller ownership check uses /proc on linux")
	}
	cityPath := t.TempDir()
	pidPath := sessionSubmitPollerPIDPath(cityPath, "s-test", "session-id")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	running, err := existingSessionSubmitPollerPID(pidPath, cityPath, "s-test", "session-id")
	if err != nil {
		t.Fatalf("existingSessionSubmitPollerPID: %v", err)
	}
	if running {
		t.Fatalf("existingSessionSubmitPollerPID(%q) = true for unrelated live PID %d", pidPath, os.Getpid())
	}
}

func TestExistingSessionSubmitPollerPIDAcceptsMatchingCitySession(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("poller ownership check uses /proc on linux")
	}
	cityPath := t.TempDir()
	sessionName := "s-test"
	pidPath := sessionSubmitPollerPIDPath(cityPath, sessionName, "session-id")
	cmd := startSubmitPollerLikeProcess(t, cityPath, sessionName, "session-id")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	running, err := existingSessionSubmitPollerPID(pidPath, cityPath, sessionName, "session-id")
	if err != nil {
		t.Fatalf("existingSessionSubmitPollerPID: %v", err)
	}
	if !running {
		t.Fatalf("existingSessionSubmitPollerPID(%q) = false for matching poller PID %d", pidPath, cmd.Process.Pid)
	}
}

func TestExistingSessionSubmitPollerPIDRejectsDifferentCitySameSession(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("poller ownership check uses /proc on linux")
	}
	cityPath := t.TempDir()
	otherCityPath := t.TempDir()
	sessionName := "s-test"
	pidPath := sessionSubmitPollerPIDPath(cityPath, sessionName, "session-id")
	cmd := startSubmitPollerLikeProcess(t, otherCityPath, sessionName, "session-id")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	running, err := existingSessionSubmitPollerPID(pidPath, cityPath, sessionName, "session-id")
	if err != nil {
		t.Fatalf("existingSessionSubmitPollerPID: %v", err)
	}
	if running {
		t.Fatalf("existingSessionSubmitPollerPID(%q) = true for same-session poller in different city", pidPath)
	}
}

func TestExistingSessionSubmitPollerPIDRejectsDifferentTargetSameCitySession(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("poller ownership check uses /proc on linux")
	}
	cityPath := t.TempDir()
	sessionName := "s-test"
	pidPath := sessionSubmitPollerPIDPath(cityPath, sessionName, "session-id")
	cmd := startSubmitPollerLikeProcess(t, cityPath, sessionName, "old-alias")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	running, err := existingSessionSubmitPollerPID(pidPath, cityPath, sessionName, "session-id")
	if err != nil {
		t.Fatalf("existingSessionSubmitPollerPID: %v", err)
	}
	if running {
		t.Fatalf("existingSessionSubmitPollerPID(%q) = true for same-session poller with different target key", pidPath)
	}
}

func TestSessionSubmitPollerPathsScopeSameSessionByTarget(t *testing.T) {
	cityPath := t.TempDir()
	sessionName := "s-test"
	pidPathA := sessionSubmitPollerPIDPath(cityPath, sessionName, "session-a")
	pidPathB := sessionSubmitPollerPIDPath(cityPath, sessionName, "session-b")
	logPathA := sessionSubmitPollerLogPath(cityPath, sessionName, "session-a")
	logPathB := sessionSubmitPollerLogPath(cityPath, sessionName, "session-b")
	if pidPathA == pidPathB {
		t.Fatalf("sessionSubmitPollerPIDPath returned the same path for distinct targets: %q", pidPathA)
	}
	if logPathA == logPathB {
		t.Fatalf("sessionSubmitPollerLogPath returned the same path for distinct targets: %q", logPathA)
	}
}

func TestDeferredSubmitPollerKeyFallbackOrder(t *testing.T) {
	cases := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "session id wins over alias",
			bead: beads.Bead{
				ID:       "session-id",
				Metadata: map[string]string{"alias": "alias", "template": "template", "session_name": "s-test"},
				Title:    "title",
			},
			want: "session-id",
		},
		{
			name: "alias fallback",
			bead: beads.Bead{
				Metadata: map[string]string{"alias": "alias", "template": "template", "session_name": "s-test"},
				Title:    "title",
			},
			want: "alias",
		},
		{
			name: "template fallback",
			bead: beads.Bead{
				Metadata: map[string]string{"template": "template", "session_name": "s-test"},
				Title:    "title",
			},
			want: "template",
		},
		{
			name: "agent name fallback",
			bead: beads.Bead{
				Metadata: map[string]string{"agent_name": "agent", "template": "template", "session_name": "s-test"},
				Title:    "title",
			},
			want: "agent",
		},
		{
			name: "session name fallback",
			bead: beads.Bead{
				Metadata: map[string]string{"session_name": "s-test"},
				Title:    "title",
			},
			want: "s-test",
		},
		{
			name: "title fallback",
			bead: beads.Bead{Title: "title"},
			want: "title",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deferredSubmitPollerKey(tc.bead); got != tc.want {
				t.Fatalf("deferredSubmitPollerKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPollerKeyFromBeadFallbackOrder(t *testing.T) {
	cases := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "session id wins over metadata",
			bead: beads.Bead{
				ID: "session-id",
				Metadata: map[string]string{
					"alias":        "alias",
					"agent_name":   "agent",
					"template":     "template",
					"session_name": "s-test",
				},
				Title: "title",
			},
			want: "session-id",
		},
		{
			name: "alias fallback",
			bead: beads.Bead{
				Metadata: map[string]string{
					"alias":        "alias",
					"agent_name":   "agent",
					"template":     "template",
					"session_name": "s-test",
				},
				Title: "title",
			},
			want: "alias",
		},
		{
			name: "agent name fallback",
			bead: beads.Bead{
				Metadata: map[string]string{
					"agent_name":   "agent",
					"template":     "template",
					"session_name": "s-test",
				},
				Title: "title",
			},
			want: "agent",
		},
		{
			name: "template fallback",
			bead: beads.Bead{
				Metadata: map[string]string{
					"template":     "template",
					"session_name": "s-test",
				},
				Title: "title",
			},
			want: "template",
		},
		{
			name: "session name fallback",
			bead: beads.Bead{
				Metadata: map[string]string{"session_name": "s-test"},
				Title:    "title",
			},
			want: "s-test",
		},
		{
			name: "title fallback",
			bead: beads.Bead{Title: "title"},
			want: "title",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PollerKeyFromBead(tc.bead); got != tc.want {
				t.Fatalf("PollerKeyFromBead() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPollerKeyFromInfoMatchesBead pins PollerKeyFromInfo against its raw twin:
// for each fixture it asserts the exact key (self-sufficient — a mutated
// fallback order or a wrong field fails directly) AND that the Info projection of
// the same bead yields the identical key, so the two forms cannot drift. The
// session-name fixture specifically guards that PollerKeyFromInfo reads the RAW
// SessionNameMetadata (not the sessionNameFor-filled SessionName).
func TestPollerKeyFromInfoMatchesBead(t *testing.T) {
	cases := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "session id wins over metadata",
			bead: beads.Bead{ID: "session-id", Metadata: map[string]string{"alias": "alias", "session_name": "s-test"}, Title: "title"},
			want: "session-id",
		},
		{
			name: "alias fallback",
			bead: beads.Bead{Metadata: map[string]string{"alias": "alias", "agent_name": "agent", "template": "template", "session_name": "s-test"}, Title: "title"},
			want: "alias",
		},
		{
			name: "agent name fallback",
			bead: beads.Bead{Metadata: map[string]string{"agent_name": "agent", "template": "template", "session_name": "s-test"}, Title: "title"},
			want: "agent",
		},
		{
			name: "template fallback",
			bead: beads.Bead{Metadata: map[string]string{"template": "template", "session_name": "s-test"}, Title: "title"},
			want: "template",
		},
		{
			name: "raw session_name fallback",
			bead: beads.Bead{Metadata: map[string]string{"session_name": "s-test"}, Title: "title"},
			want: "s-test",
		},
		{
			name: "title fallback",
			bead: beads.Bead{Title: "title"},
			want: "title",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := infoFromPersistedBead(tc.bead)
			got := PollerKeyFromInfo(info)
			if got != tc.want {
				t.Fatalf("PollerKeyFromInfo() = %q, want %q", got, tc.want)
			}
			if raw := PollerKeyFromBead(tc.bead); got != raw {
				t.Fatalf("PollerKeyFromInfo() = %q diverged from PollerKeyFromBead() = %q", got, raw)
			}
		})
	}
}

func startSubmitPollerLikeProcess(t *testing.T, cityPath, sessionName, agentName string) *exec.Cmd {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "gc-fake")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nread _hold\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(fake poller): %v", err)
	}
	cmd := exec.Command(scriptPath, nudgepoller.CommandArgs(cityPath, sessionName, agentName)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe(fake poller): %v", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatalf("Start(fake poller): %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	waitForSubmitPollerCmdline(t, cmd.Process.Pid, cityPath, sessionName, agentName)
	return cmd
}

func waitForSubmitPollerCmdline(t *testing.T, pid int, cityPath, sessionName, agentName string) {
	t.Helper()
	matches := nudgepoller.CmdlineMatcher(cityPath, sessionName, agentName)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pidutil.AliveWithCmdline(pid, matches) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("poller PID %d did not expose matching command line", pid)
}

func TestSubmitFollowUpQueuesDeferredMessageForPoolManagedSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithCityPath(cityPath))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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
	mgr := NewManagerWithOptions(store, sp, WithCityPath(cityPath))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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

func TestSubmitFollowUpOnAsleepSessionFallsBackToImmediateSend(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithCityPath(cityPath))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sp.Stop(info.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := store.SetMetadata(info.ID, "state", string(StateAsleep)); err != nil {
		t.Fatalf("SetMetadata(state): %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "wake and send", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentFollowUp)
	if err != nil {
		t.Fatalf("Submit(follow_up): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(follow_up) unexpectedly queued for asleep session")
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after follow_up on asleep session")
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
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "wake and send" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want NudgeNow(wake and send)", sp.Calls)
	}
}

func TestSubmitDefaultQueuesWhenWakeAlreadyRequested(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithCityPath(cityPath))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sp.Stop(info.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := store.SetMetadataBatch(info.ID, map[string]string{
		"state":                string(StateCreating),
		"pending_create_claim": "true",
	}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	callsBefore := len(sp.Calls)

	outcome, err := mgr.Submit(context.Background(), info.ID, "deliver after wake", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
	if err != nil {
		t.Fatalf("Submit(default): %v", err)
	}
	if !outcome.Queued {
		t.Fatal("Submit(default) should queue while wake is already requested")
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending queued submits = %d, want 1", len(state.Pending))
	}
	if state.Pending[0].SessionID != info.ID {
		t.Fatalf("SessionID = %q, want %q", state.Pending[0].SessionID, info.ID)
	}
	if state.Pending[0].Message != "deliver after wake" {
		t.Fatalf("Message = %q, want deliver after wake", state.Pending[0].Message)
	}
	for _, call := range sp.Calls[callsBefore:] {
		if call.Method == "Start" || call.Method == "Nudge" || call.Method == "NudgeNow" {
			t.Fatalf("unexpected runtime call while queueing against requested wake: %#v", call)
		}
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

func TestSubmissionCapabilitiesDisableInterruptNowForAntigravity(t *testing.T) {
	caps := SubmissionCapabilitiesForMetadata(
		map[string]string{
			"provider_kind": "antigravity",
			"provider":      "antigravity",
		},
		true,
	)
	if !caps.SupportsFollowUp {
		t.Fatal("SupportsFollowUp = false, want true")
	}
	if caps.SupportsInterruptNow {
		t.Fatal("SupportsInterruptNow = true, want false for Antigravity")
	}
}

func TestSubmissionCapabilitiesDisableInterruptNowForWrappedAntigravity(t *testing.T) {
	caps := SubmissionCapabilitiesForMetadata(
		map[string]string{
			"builtin_ancestor": "antigravity",
			"provider":         "custom-antigravity",
		},
		true,
	)
	if caps.SupportsInterruptNow {
		t.Fatal("SupportsInterruptNow = true, want false for wrapped Antigravity")
	}
}

func TestSubmitInterruptNowRejectsAntigravitySession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "antigravity", WorkDir: t.TempDir(), Provider: "antigravity", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	callsBefore := len(sp.Calls)

	outcome, err := mgr.Submit(context.Background(), info.ID, "take this now", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if !errors.Is(err, ErrInteractionUnsupported) {
		t.Fatalf("Submit(interrupt_now) error = %v, want ErrInteractionUnsupported", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}
	for _, call := range sp.Calls[callsBefore:] {
		if call.Method == "Interrupt" || call.Method == "NudgeNow" || call.Method == "SendKeys" || call.Method == "Stop" {
			t.Fatalf("unexpected runtime call after unsupported interrupt_now: %#v", call)
		}
	}
}

func TestSubmitInterruptNowUsesInterruptAndIdleWaitForGemini(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "gemini", WorkDir: t.TempDir(), Provider: "gemini", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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

	var sawEscape, sawInterrupt, sawWaitForIdle, sawReset, sawClear, sawNudge, sawStop bool
	interruptIdx := -1
	waitIdx := -1
	resetIdx := -1
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
		if call.Method == "ResetInterruptedTurn" && call.Name == info.SessionName {
			sawReset = true
			resetIdx = i
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
	if !sawInterrupt || !sawWaitForIdle || !sawReset || !sawClear || !sawNudge {
		t.Fatalf("calls = %#v, want Interrupt + WaitForIdle + ResetInterruptedTurn + SendKeys(C-u) + NudgeNow", sp.Calls)
	}
	if interruptIdx < 0 || waitIdx < 0 || resetIdx < 0 || clearIdx < 0 || nudgeIdx < 0 {
		t.Fatalf("calls = %#v, want Interrupt + WaitForIdle + ResetInterruptedTurn + SendKeys(C-u) before NudgeNow", sp.Calls)
	}
	if interruptIdx >= waitIdx || waitIdx >= resetIdx || resetIdx >= clearIdx || clearIdx >= nudgeIdx {
		t.Fatalf("calls = %#v, want Interrupt -> WaitForIdle -> ResetInterruptedTurn -> SendKeys(C-u) before NudgeNow", sp.Calls)
	}
	if sawStop {
		t.Fatalf("calls = %#v, did not want Stop for gemini interrupt_now", sp.Calls)
	}
}

func TestSubmitInterruptNowAllowsPoolManagedCodexSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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

	var sawEscape, sawWaitForIdle, sawWaitForBoundary, sawNudge, sawStop bool
	waitIdx := -1
	boundaryIdx := -1
	nudgeIdx := -1
	for i, call := range sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
		if call.Method == "WaitForIdle" && call.Name == info.SessionName {
			sawWaitForIdle = true
			waitIdx = i
		}
		if call.Method == "WaitForInterruptBoundary" && call.Name == info.SessionName {
			sawWaitForBoundary = true
			boundaryIdx = i
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "take this now" {
			sawNudge = true
			nudgeIdx = i
		}
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
	}
	if !sawEscape || !sawWaitForIdle || !sawWaitForBoundary || !sawNudge {
		t.Fatalf("calls = %#v, want SendKeys(Escape) + WaitForIdle + WaitForInterruptBoundary + NudgeNow", sp.Calls)
	}
	if waitIdx < 0 || boundaryIdx < 0 || nudgeIdx < 0 || waitIdx >= boundaryIdx || boundaryIdx >= nudgeIdx {
		t.Fatalf("calls = %#v, want WaitForIdle -> WaitForInterruptBoundary before NudgeNow", sp.Calls)
	}
	if sawStop {
		t.Fatalf("calls = %#v, did not want Stop for codex interrupt_now", sp.Calls)
	}
}

func TestSubmitInterruptNowUsesInterruptAndIdleWaitForClaude(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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

	var sawEscape, sawInterrupt, sawBoundary, sawNudge, sawStop bool
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
		if call.Method == "WaitForInterruptBoundary" && call.Name == info.SessionName {
			sawBoundary = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "replace the current turn" {
			sawNudge = true
		}
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
	}
	if !sawEscape || !sawInterrupt || !sawBoundary || !sawNudge || waitCalls != 2 {
		t.Fatalf("calls = %#v, want SendKeys(Escape) + WaitForIdle + Interrupt + WaitForIdle + WaitForInterruptBoundary + NudgeNow", sp.Calls)
	}
	if sawStop {
		t.Fatalf("calls = %#v, did not want Stop after successful control-c fallback", sp.Calls)
	}
}

func TestSubmitInterruptNowFallsBackToRestartOnInterruptBoundaryTimeoutForCodex(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.InterruptBoundaryErrors[info.SessionName] = fmt.Errorf("no turn_aborted marker yet")

	outcome, err := mgr.Submit(context.Background(), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}

	var sawBoundary, sawStop, sawNudge bool
	for _, call := range sp.Calls {
		if call.Method == "WaitForInterruptBoundary" && call.Name == info.SessionName {
			sawBoundary = true
		}
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "replace the current turn" {
			sawNudge = true
		}
	}
	if !sawBoundary || !sawStop || !sawNudge {
		t.Fatalf("calls = %#v, want WaitForInterruptBoundary + Stop + NudgeNow via restart fallback", sp.Calls)
	}
}

func TestSubmitInterruptNowHardRestartsAndTruncatesPiPendingTurn(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "pi --session abc123", WorkDir: t.TempDir(), Provider: "pi", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionDir := t.TempDir()
	piSessionPath := filepath.Join(sessionDir, "2026-05-11T00-00-00-000Z_abc123.jsonl")
	piSession := strings.Join([]string{
		fmt.Sprintf(`{"type":"session","id":"abc123","cwd":%q}`, info.WorkDir),
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"stable prompt"}]}}`,
		`{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":[{"type":"text","text":"stable response"}]}}`,
		`{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":[{"type":"text","text":"interrupted prompt to replace"}]}}`,
		`{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":[{"type":"text","text":"partial output to discard"}]}`,
		"",
	}, "\n")
	if err := os.WriteFile(piSessionPath, []byte(piSession), 0o600); err != nil {
		t.Fatalf("WriteFile pi session: %v", err)
	}
	mirrorDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(mirrorDir, "abc123.jsonl"), []byte(piSession), 0o600); err != nil {
		t.Fatalf("WriteFile pi mirror: %v", err)
	}

	hints := runtime.Config{
		WorkDir: info.WorkDir,
		Env: map[string]string{
			"PI_CODING_AGENT_SESSION_DIR": sessionDir,
			"GC_PI_TRANSCRIPT_DIR":        mirrorDir,
		},
	}
	outcome, err := mgr.Submit(context.Background(), info.ID, "replace the current turn", BuildResumeCommand(info), hints, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}

	var sawStop, sawStart, sawWaitForIdle, sawNudge, sawInterrupt, sawEscape bool
	stopIdx := -1
	startIdx := -1
	nudgeIdx := -1
	for i, call := range sp.Calls {
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
			stopIdx = i
		}
		if call.Method == "Start" && call.Name == info.SessionName {
			sawStart = true
			startIdx = i
		}
		if call.Method == "WaitForIdle" && call.Name == info.SessionName {
			sawWaitForIdle = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "replace the current turn" {
			sawNudge = true
			nudgeIdx = i
		}
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
		}
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
	}
	if !sawStop || !sawStart || !sawNudge {
		t.Fatalf("calls = %#v, want Stop + Start + NudgeNow", sp.Calls)
	}
	if stopIdx < 0 || startIdx < 0 || nudgeIdx < 0 || stopIdx >= startIdx || startIdx >= nudgeIdx {
		t.Fatalf("calls = %#v, want Stop -> Start -> NudgeNow", sp.Calls)
	}
	if sawWaitForIdle {
		t.Fatalf("calls = %#v, did not want idle wait for pi hard-restart interrupt", sp.Calls)
	}
	if sawInterrupt || sawEscape {
		t.Fatalf("calls = %#v, did not want in-process interrupt for pi interrupt_now", sp.Calls)
	}

	truncated, err := os.ReadFile(piSessionPath)
	if err != nil {
		t.Fatalf("ReadFile pi session: %v", err)
	}
	if strings.Contains(string(truncated), "interrupted prompt to replace") {
		t.Fatalf("pi session still contains replaced prompt:\n%s", truncated)
	}
	if strings.Contains(string(truncated), "partial output to discard") {
		t.Fatalf("pi session still contains partial assistant output:\n%s", truncated)
	}
	if !strings.Contains(string(truncated), "stable response") {
		t.Fatalf("pi session lost stable history:\n%s", truncated)
	}
	mirrored, err := os.ReadFile(filepath.Join(mirrorDir, "abc123.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile pi mirror: %v", err)
	}
	if string(mirrored) != string(truncated) {
		t.Fatalf("pi mirror differs from native transcript:\nmirror:\n%s\nnative:\n%s", mirrored, truncated)
	}
}

func TestSubmitInterruptNowRestoresPiSessionWhenTranscriptResetFails(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "pi --session abc123", WorkDir: t.TempDir(), Provider: "pi", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionDir := t.TempDir()
	piSessionPath := filepath.Join(sessionDir, "2026-05-11T00-00-00-000Z_abc123.jsonl")
	piSession := strings.Join([]string{
		fmt.Sprintf(`{"type":"session","id":"abc123","cwd":%q}`, info.WorkDir),
		`{"type":"message","id":"u1","message":{"role":"user","content":"stable prompt"}}`,
		`{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":"stable response","stopReason":"stop"}}`,
		`{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"interrupted prompt to replace"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(piSessionPath, []byte(piSession), 0o600); err != nil {
		t.Fatalf("WriteFile pi session: %v", err)
	}
	if err := os.Chmod(sessionDir, 0o500); err != nil {
		t.Fatalf("Chmod session dir read-only: %v", err)
	}
	defer func() {
		if err := os.Chmod(sessionDir, 0o700); err != nil {
			t.Fatalf("restore session dir permissions: %v", err)
		}
	}()

	hints := runtime.Config{
		WorkDir: info.WorkDir,
		Env: map[string]string{
			"PI_CODING_AGENT_SESSION_DIR": sessionDir,
		},
	}
	_, err = mgr.Submit(context.Background(), info.ID, "replace the current turn", BuildResumeCommand(info), hints, SubmitIntentInterruptNow)
	if err == nil {
		t.Fatal("Submit(interrupt_now) error = nil, want transcript reset failure")
	}
	if !strings.Contains(err.Error(), "writing temp file") && !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Submit(interrupt_now) error = %v, want transcript write failure", err)
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("Pi session was not restored after transcript reset failure")
	}

	var sawStop, sawStart, sawNudge bool
	for _, call := range sp.Calls {
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
		if call.Method == "Start" && call.Name == info.SessionName {
			sawStart = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName {
			sawNudge = true
		}
	}
	if !sawStop || !sawStart {
		t.Fatalf("calls = %#v, want Stop then Start restore after transcript reset failure", sp.Calls)
	}
	if sawNudge {
		t.Fatalf("calls = %#v, did not want replacement nudge after transcript reset failure", sp.Calls)
	}
	got, readErr := os.ReadFile(piSessionPath)
	if readErr != nil {
		t.Fatalf("ReadFile pi session: %v", readErr)
	}
	if !strings.Contains(string(got), "interrupted prompt to replace") {
		t.Fatalf("pi transcript was unexpectedly truncated after reset failure:\n%s", got)
	}
}

func TestSubmitInterruptNowTruncatesPiTranscriptBySessionKey(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "pi --session target", WorkDir: t.TempDir(), Provider: "pi", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, "session_key", "target"); err != nil {
		t.Fatalf("SetMetadata session_key: %v", err)
	}

	sessionDir := t.TempDir()
	targetPath := filepath.Join(sessionDir, "target.jsonl")
	otherPath := filepath.Join(sessionDir, "other.jsonl")
	targetSession := strings.Join([]string{
		fmt.Sprintf(`{"type":"session","id":"target","cwd":%q}`, info.WorkDir),
		`{"type":"message","id":"u1","message":{"role":"user","content":"stable target prompt"}}`,
		`{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":"stable target response","stopReason":"stop"}}`,
		`{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"target interrupted prompt"}}`,
		"",
	}, "\n")
	otherSession := strings.Join([]string{
		fmt.Sprintf(`{"type":"session","id":"other","cwd":%q}`, info.WorkDir),
		`{"type":"message","id":"u1","message":{"role":"user","content":"other stable prompt"}}`,
		`{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":"other stable response","stopReason":"stop"}}`,
		`{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"other pending prompt"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(targetPath, []byte(targetSession), 0o600); err != nil {
		t.Fatalf("WriteFile target pi session: %v", err)
	}
	if err := os.WriteFile(otherPath, []byte(otherSession), 0o600); err != nil {
		t.Fatalf("WriteFile other pi session: %v", err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(otherPath, future, future); err != nil {
		t.Fatalf("Chtimes other pi session: %v", err)
	}

	hints := runtime.Config{
		WorkDir: info.WorkDir,
		Env: map[string]string{
			"PI_CODING_AGENT_SESSION_DIR": sessionDir,
		},
	}
	outcome, err := mgr.Submit(context.Background(), info.ID, "replacement prompt", BuildResumeCommand(info), hints, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}

	targetData, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile target pi session: %v", err)
	}
	if strings.Contains(string(targetData), "target interrupted prompt") {
		t.Fatalf("target pi session still contains interrupted prompt:\n%s", targetData)
	}
	otherData, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatalf("ReadFile other pi session: %v", err)
	}
	if string(otherData) != otherSession {
		t.Fatalf("other pi session was modified:\n%s", otherData)
	}
}

func TestSubmitInterruptNowFailsClosedOnPiSessionKeyMismatch(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "pi --session target", WorkDir: t.TempDir(), Provider: "pi", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, "session_key", "target"); err != nil {
		t.Fatalf("SetMetadata session_key: %v", err)
	}

	sessionDir := t.TempDir()
	otherPath := filepath.Join(sessionDir, "other.jsonl")
	otherSession := strings.Join([]string{
		fmt.Sprintf(`{"type":"session","id":"other","cwd":%q}`, info.WorkDir),
		`{"type":"message","id":"u1","message":{"role":"user","content":"other pending prompt"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(otherPath, []byte(otherSession), 0o600); err != nil {
		t.Fatalf("WriteFile other pi session: %v", err)
	}

	hints := runtime.Config{
		WorkDir: info.WorkDir,
		Env: map[string]string{
			"PI_CODING_AGENT_SESSION_DIR": sessionDir,
		},
	}
	if _, err := mgr.Submit(context.Background(), info.ID, "replacement prompt", BuildResumeCommand(info), hints, SubmitIntentInterruptNow); err == nil || !strings.Contains(err.Error(), "session_key") {
		t.Fatalf("Submit(interrupt_now) error = %v, want session_key mismatch error", err)
	}
	for _, call := range sp.Calls {
		if call.Method == "Stop" && call.Name == info.SessionName {
			t.Fatalf("calls = %#v, did not want Stop before resolving keyed Pi transcript", sp.Calls)
		}
	}
	otherData, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatalf("ReadFile other pi session: %v", err)
	}
	if string(otherData) != otherSession {
		t.Fatalf("mismatched Pi transcript was modified:\n%s", otherData)
	}
}

func TestSubmitInterruptNowFailsClosedOnAmbiguousPiTranscript(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "pi", WorkDir: t.TempDir(), Provider: "pi", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sessionDir := t.TempDir()
	firstPath := filepath.Join(sessionDir, "first.jsonl")
	secondPath := filepath.Join(sessionDir, "second.jsonl")
	firstSession := strings.Join([]string{
		fmt.Sprintf(`{"type":"session","id":"first","cwd":%q}`, info.WorkDir),
		`{"type":"message","id":"u1","message":{"role":"user","content":"first pending prompt"}}`,
		"",
	}, "\n")
	secondSession := strings.Join([]string{
		fmt.Sprintf(`{"type":"session","id":"second","cwd":%q}`, info.WorkDir),
		`{"type":"message","id":"u1","message":{"role":"user","content":"second pending prompt"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(firstPath, []byte(firstSession), 0o600); err != nil {
		t.Fatalf("WriteFile first pi session: %v", err)
	}
	if err := os.WriteFile(secondPath, []byte(secondSession), 0o600); err != nil {
		t.Fatalf("WriteFile second pi session: %v", err)
	}

	hints := runtime.Config{
		WorkDir: info.WorkDir,
		Env: map[string]string{
			"PI_CODING_AGENT_SESSION_DIR": sessionDir,
		},
	}
	if _, err := mgr.Submit(context.Background(), info.ID, "replacement prompt", BuildResumeCommand(info), hints, SubmitIntentInterruptNow); err == nil || !strings.Contains(err.Error(), "ambiguous pi session file") {
		t.Fatalf("Submit(interrupt_now) error = %v, want ambiguous pi session file", err)
	}
	for _, call := range sp.Calls {
		if call.Method == "Stop" && call.Name == info.SessionName {
			t.Fatalf("calls = %#v, did not want Stop before resolving an unambiguous Pi transcript", sp.Calls)
		}
	}
	firstData, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("ReadFile first pi session: %v", err)
	}
	secondData, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatalf("ReadFile second pi session: %v", err)
	}
	if string(firstData) != firstSession || string(secondData) != secondSession {
		t.Fatalf("ambiguous Pi transcripts changed:\nfirst:\n%s\nsecond:\n%s", firstData, secondData)
	}
}

func TestSubmitInterruptNowPiContinuesWhenSessionFileMissing(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "pi --session missing", WorkDir: t.TempDir(), Provider: "pi", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	hints := runtime.Config{
		WorkDir: info.WorkDir,
		Env: map[string]string{
			"PI_CODING_AGENT_SESSION_DIR": t.TempDir(),
		},
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "replacement prompt", BuildResumeCommand(info), hints, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}

	var sawStop, sawStart, sawNudge bool
	for _, call := range sp.Calls {
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
		if call.Method == "Start" && call.Name == info.SessionName {
			sawStart = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "replacement prompt" {
			sawNudge = true
		}
	}
	if !sawStop || !sawStart || !sawNudge {
		t.Fatalf("calls = %#v, want Stop + Start + NudgeNow despite missing Pi transcript", sp.Calls)
	}
}

func TestSubmitInterruptNowFindsPiDefaultSessionPath(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "pi --session abc123", WorkDir: t.TempDir(), Provider: "pi", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionDir := filepath.Join(home, ".pi", "agent", "sessions")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll pi sessions: %v", err)
	}
	piSessionPath := filepath.Join(sessionDir, "default.jsonl")
	piSession := strings.Join([]string{
		fmt.Sprintf(`{"type":"session","id":"abc123","cwd":%q}`, info.WorkDir),
		`{"type":"message","id":"u1","message":{"role":"user","content":"prompt"}}`,
		`{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":"partial"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(piSessionPath, []byte(piSession), 0o600); err != nil {
		t.Fatalf("WriteFile pi session: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}
}

func TestStopTurnUsesSoftEscapeAndIdleWaitForCodex(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}

	var sawEscape, sawInterrupt, sawWaitForIdle, sawWaitForBoundary bool
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
		if call.Method == "WaitForInterruptBoundary" && call.Name == info.SessionName {
			sawWaitForBoundary = true
		}
	}
	if !sawEscape || !sawWaitForIdle || !sawWaitForBoundary {
		t.Fatalf("calls = %#v, want SendKeys(Escape) + WaitForIdle + WaitForInterruptBoundary", sp.Calls)
	}
	if sawInterrupt {
		t.Fatalf("calls = %#v, did not want Interrupt for codex stop", sp.Calls)
	}
}

func TestStopTurnUsesControlCFallbackAfterSoftEscapeTimeoutForCodex(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.WaitForIdleSequence[info.SessionName] = []error{fmt.Errorf("not idle yet"), nil}

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}

	var sawEscape, sawInterrupt, sawBoundary bool
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
		if call.Method == "WaitForInterruptBoundary" && call.Name == info.SessionName {
			sawBoundary = true
		}
	}
	if !sawEscape || !sawInterrupt || !sawBoundary || waitCalls != 2 {
		t.Fatalf("calls = %#v, want SendKeys(Escape) + WaitForIdle + Interrupt + WaitForIdle + WaitForInterruptBoundary", sp.Calls)
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
