package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgepoller"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
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
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
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
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
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
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
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

// TestSubmitDefaultCodexDismissesDialogsThroughAutoProvider covers a city
// whose provider is auto-wrapped: the deferred dismissal must reach the tmux
// leg hosting the session (mc-zndi7.92).
func TestSubmitDefaultCodexDismissesDialogsThroughAutoProvider(t *testing.T) {
	store := beads.NewMemStore()
	tmuxLeg := runtime.NewFake()
	mgr := newTestManager(t, store, sessionauto.New(tmuxLeg, runtime.NewFake()))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault); err != nil {
		t.Fatalf("Submit(default): %v", err)
	}
	dismissals := 0
	for _, call := range tmuxLeg.Calls {
		if call.Method == "DismissKnownDialogs" && call.Name == info.SessionName {
			dismissals++
		}
	}
	if dismissals != 2 {
		t.Fatalf("tmux leg DismissKnownDialogs calls = %d, want 2; calls = %#v", dismissals, tmuxLeg.Calls)
	}
	updated, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if got := updated.Metadata[startupDialogVerifiedKey]; got != "true" {
		t.Fatalf("%s = %q, want true", startupDialogVerifiedKey, got)
	}
}

// TestSubmitDefaultCodexLeavesDialogsUnverifiedWhenUnsupported: a composite
// whose backend for the session cannot dismiss dialogs answers
// ErrInteractionUnsupported, and the session must not be marked verified.
func TestSubmitDefaultCodexLeavesDialogsUnverifiedWhenUnsupported(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.DialogErrors[info.SessionName] = runtime.ErrInteractionUnsupported
	if _, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault); err != nil {
		t.Fatalf("Submit(default): %v", err)
	}
	updated, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if got := updated.Metadata[startupDialogVerifiedKey]; got != "" {
		t.Fatalf("%s = %q, want unset", startupDialogVerifiedKey, got)
	}
}

func TestSubmitDefaultCodexSkipsDeferredDialogsAfterVerification(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, startupDialogVerifiedKey, "true"); err != nil {
		t.Fatalf("SetMetadata(%s): %v", startupDialogVerifiedKey, err)
	}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
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
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "gemini", WorkDir: t.TempDir(), Provider: "gemini", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
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
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "gemini", WorkDir: t.TempDir(), Provider: "gemini", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
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

// TestSubmitDefaultTreatsDeliveredUnobservedNudgeAsDelivered: a provider that
// proved delivery but never observed the turn start reports
// runtime.ErrNudgeDeliveredUnobserved. tmux does so when a Claude turn outlasts
// its idle wait and Claude queues the message. Submit must succeed: an error
// fails the CLI and the API, rolls back create-with-message, and a retry
// delivers the message twice. An unconfirmed submit stays a failure.
func TestSubmitDefaultTreatsDeliveredUnobservedNudgeAsDelivered(t *testing.T) {
	tests := []struct {
		name     string
		nudgeErr error
		wantErr  error
	}{
		{"runtime sentinel", runtime.ErrNudgeDeliveredUnobserved, nil},
		{"tmux delivered-unobserved", fmt.Errorf("%w: session %q", tmux.ErrNudgeSubmitDeliveredUnobserved, "helper"), nil},
		{"tmux unconfirmed stays a failure", fmt.Errorf("%w: session %q", tmux.ErrNudgeSubmitUnconfirmed, "helper"), tmux.ErrNudgeSubmitUnconfirmed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := newTestManager(t, store, sp)

			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			sp.NudgeErrors = map[string]error{info.SessionName: tt.nudgeErr}

			outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
			switch {
			case tt.wantErr == nil && err != nil:
				t.Fatalf("Submit(default) = %v, want nil (delivery was proven)", err)
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Fatalf("Submit(default) = %v, want %v", err, tt.wantErr)
			}
			if outcome.Queued {
				t.Fatal("Submit(default) unexpectedly queued")
			}
			nudges := 0
			for _, call := range sp.Calls {
				if (call.Method == "Nudge" || call.Method == "NudgeNow") && call.Name == info.SessionName {
					nudges++
				}
			}
			if nudges != 1 {
				t.Fatalf("nudges = %d, want 1; calls = %#v", nudges, sp.Calls)
			}
		})
	}
}

func TestSubmitDefaultConfirmsLiveCreatingSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)

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

	if _, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), created.ID, "hello", "gemini", runtime.Config{WorkDir: workDir}, SubmitIntentDefault); err != nil {
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
	mgr := newTestManager(t, store, sp, WithCityPath(cityPath))

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

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "follow up later", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentFollowUp)
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
	cityPath := filepath.Join(t.TempDir(), "city with spaces")
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
	mgr := newTestManager(t, store, sp, WithCityPath(cityPath))

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

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "follow up later", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentFollowUp)
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
	mgr := newTestManager(t, store, sp, WithCityPath(cityPath))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "send this now", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentFollowUp)
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
	mgr := newTestManager(t, store, sp, WithCityPath(cityPath))

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

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "wake and send", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentFollowUp)
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
	mgr := newTestManager(t, store, sp, WithCityPath(cityPath))

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

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "deliver after wake", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
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
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "antigravity", WorkDir: t.TempDir(), Provider: "antigravity", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	callsBefore := len(sp.Calls)

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "take this now", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
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
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "gemini", WorkDir: t.TempDir(), Provider: "gemini", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "take this now", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
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
	mgr := newTestManager(t, store, sp)

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

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "take this now", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
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
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
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
		if call.Method == "ClearInput" && call.Name == info.SessionName {
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
		t.Fatalf("calls = %#v, want interrupt + WaitForIdle + ClearInput + nudge", sp.Calls)
	}
	if clearIdx < 0 || nudgeIdx < 0 || clearIdx > nudgeIdx {
		t.Fatalf("calls = %#v, want ClearInput before nudge", sp.Calls)
	}
	if sawStop {
		t.Fatalf("calls = %#v, did not want Stop for claude interrupt_now", sp.Calls)
	}
}

func TestSubmitInterruptNowFallsBackToRestartOnIdleTimeout(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sp.WaitForIdleErrors = map[string]error{}
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.WaitForIdleErrors[info.SessionName] = fmt.Errorf("not idle yet")

	// WaitForIdle fails → fallback stops session → restart also calls
	// WaitForIdle which still fails. The error propagates from the restart
	// path, confirming the fallback was attempted.
	_, err = mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
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
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.WaitForIdleSequence[info.SessionName] = []error{fmt.Errorf("not idle yet"), nil}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
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
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.InterruptBoundaryErrors[info.SessionName] = fmt.Errorf("no turn_aborted marker yet")

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
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

// TestSubmitInterruptNowRestartFallbackAbsorbsDownedServer pins the restart
// fallback as a cleanup of the outgoing incarnation: replacement only needs
// that session gone, so tmux's ErrNoServer answer from the fallback stop must
// clear the way to restart and deliver the message rather than fail the submit.
// The control below keeps a real stop failure fatal.
func TestSubmitInterruptNowRestartFallbackAbsorbsDownedServer(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.InterruptBoundaryErrors[info.SessionName] = fmt.Errorf("no turn_aborted marker yet")
	sp.StopErrors[info.SessionName] = fmt.Errorf("killing session %s: %w", info.SessionName, tmux.ErrNoServer)

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now) with a downed server on the fallback stop: %v", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}

	var sawStop, sawNudge bool
	for _, call := range sp.Calls {
		if call.Method == "Stop" && call.Name == info.SessionName {
			sawStop = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "replace the current turn" {
			sawNudge = true
		}
	}
	if !sawStop || !sawNudge {
		t.Fatalf("calls = %#v, want Stop + NudgeNow via restart fallback", sp.Calls)
	}
}

// TestSubmitInterruptNowRestartFallbackStopFailurePropagates is the control for
// the absorption above: a stop failure that is not "the session is gone" may
// leave the outgoing incarnation alive, so the replacement must not proceed.
func TestSubmitInterruptNowRestartFallbackStopFailurePropagates(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.InterruptBoundaryErrors[info.SessionName] = fmt.Errorf("no turn_aborted marker yet")
	stopErr := errors.New("permission denied")
	sp.StopErrors[info.SessionName] = stopErr

	_, err = mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if !errors.Is(err, stopErr) {
		t.Fatalf("Submit(interrupt_now) with a real fallback stop failure = %v, want %v", err, stopErr)
	}
	for _, call := range sp.Calls {
		if call.Method == "NudgeNow" && call.Name == info.SessionName {
			t.Fatalf("calls = %#v, want no NudgeNow after a failed fallback stop", sp.Calls)
		}
	}
}

func TestSubmitInterruptNowHardRestartsAndTruncatesPiPendingTurn(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)

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
	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), hints, SubmitIntentInterruptNow)
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
	testPiRestoreAfterTranscriptResetFailure(t, ActorOperator, "")
}

// TestPiRestoreKeepsHeartbeatHold is the re-review's N1 on the restore path:
// a background interrupt_now to a live pi row with a heartbeat held_until
// restores the runtime it stopped without re-reading the hold.
func TestPiRestoreKeepsHeartbeatHold(t *testing.T) {
	testPiRestoreAfterTranscriptResetFailure(t, ActorAgent, "2099-01-01T00:00:00Z")
}

func testPiRestoreAfterTranscriptResetFailure(t *testing.T, policy ActorKind, heldUntil string) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "pi --session abc123", WorkDir: t.TempDir(), Provider: "pi", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if heldUntil != "" {
		if err := store.SetMetadata(info.ID, "held_until", heldUntil); err != nil {
			t.Fatal(err)
		}
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
	_, err = mgr.Submit(context.Background(), testActor(mgr, policy), info.ID, "replace the current turn", BuildResumeCommand(info), hints, SubmitIntentInterruptNow)
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
	mgr := newTestManager(t, store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))

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
	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replacement prompt", BuildResumeCommand(info), hints, SubmitIntentInterruptNow)
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
	mgr := newTestManager(t, store, sp)

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
	if _, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replacement prompt", BuildResumeCommand(info), hints, SubmitIntentInterruptNow); err == nil || !strings.Contains(err.Error(), "session_key") {
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
	mgr := newTestManager(t, store, sp)

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
	if _, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replacement prompt", BuildResumeCommand(info), hints, SubmitIntentInterruptNow); err == nil || !strings.Contains(err.Error(), "ambiguous pi session file") {
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
	mgr := newTestManager(t, store, sp)

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

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replacement prompt", BuildResumeCommand(info), hints, SubmitIntentInterruptNow)
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
	mgr := newTestManager(t, store, sp)

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

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
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
	mgr := newTestManager(t, store, sp)

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
	mgr := newTestManager(t, store, sp)

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

// TestSubmitDefaultDeferredResetPendingStampsEmptyEpochToSurviveRotation proves
// a submit deferred while a conversation reset is pending is fenced by session
// alone, not by the current epoch. commitPendingContinuationReset rotates
// continuation_epoch N->N+1 when the replacement incarnation starts, so a
// fixed epoch-N stamp would be rejected by the queued-nudge fence and
// dead-lettered — silently dropping the message. An empty epoch survives the
// rotation.
func TestSubmitDefaultDeferredResetPendingStampsEmptyEpochToSurviveRotation(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	mgr := newTestManager(t, store, sp, WithCityPath(cityPath))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sp.Stop(info.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := store.SetMetadataBatch(info.ID, map[string]string{
		"continuation_epoch":         "1",
		"continuation_reset_pending": "true",
	}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "deliver after reset", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
	if err != nil {
		t.Fatalf("Submit(default): %v", err)
	}
	if !outcome.Queued {
		t.Fatal("Submit(default) should queue while a conversation reset is pending")
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
	if item.ContinuationEpoch != "" {
		t.Fatalf("ContinuationEpoch = %q, want empty so the deferred submit survives the N->N+1 reset rotation", item.ContinuationEpoch)
	}
}

// TestSubmitDefaultDeferredRestartRequestedKeepsCurrentEpoch guards the fix's
// scope: a plain restart (restart_requested, no reset) does NOT rotate the
// epoch, so those defers keep their current-epoch stamp and stay fenced to the
// resumed conversation.
func TestSubmitDefaultDeferredRestartRequestedKeepsCurrentEpoch(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	mgr := newTestManager(t, store, sp, WithCityPath(cityPath))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sp.Stop(info.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := store.SetMetadataBatch(info.ID, map[string]string{
		"continuation_epoch": "4",
		"restart_requested":  "true",
	}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "deliver after restart", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
	if err != nil {
		t.Fatalf("Submit(default): %v", err)
	}
	if !outcome.Queued {
		t.Fatal("Submit(default) should queue while a restart is pending")
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending queued submits = %d, want 1", len(state.Pending))
	}
	if got := state.Pending[0].ContinuationEpoch; got != "4" {
		t.Fatalf("ContinuationEpoch = %q, want 4 (plain restart keeps the current epoch)", got)
	}
}

// TestSubmitInterruptNowRejectsPendingInteraction pins that interrupt_now does
// not barrel through a pending permission prompt: its interrupt keystroke
// would dismiss (reject) the prompt as a side effect and the text would
// follow. The caller must answer the prompt through Respond first (#2892).
func TestSubmitInterruptNowRejectsPendingInteraction(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "gemini", WorkDir: t.TempDir(), Provider: "gemini", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "tmux-abc",
		Kind:      "approval",
		Prompt:    "Bash: rm -rf build",
	})

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "take this now", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("Submit(interrupt_now) error = %v, want ErrPendingInteraction", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) unexpectedly queued")
	}
	for _, call := range sp.Calls {
		if call.Name != info.SessionName {
			continue
		}
		switch call.Method {
		case "SendKeys", "Interrupt", "Nudge", "NudgeNow", "Stop":
			t.Fatalf("unexpected %s while a permission prompt is pending", call.Method)
		}
	}
}

// TestPendingInteractionErrorNamesThePrompt pins that the refusal says what
// is pending, so an API or CLI caller can tell the operator what to answer.
func TestPendingInteractionErrorNamesThePrompt(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "tmux-abc",
		Kind:      "approval",
		Prompt:    "Bash: rm -rf build",
	})

	_, err = mgr.Send(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello?", "", runtime.Config{})
	if !errors.Is(err, ErrPendingInteraction) || !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("Send error = %v, want session and runtime ErrPendingInteraction", err)
	}
	for _, want := range []string{"tmux-abc", "Bash: rm -rf build"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Send error = %q, want it to mention %q", err, want)
		}
	}
}

type respondErrorProvider struct {
	*runtime.Fake
	err error
}

func (p *respondErrorProvider) Respond(string, runtime.InteractionResponse) error { return p.err }

// TestRespondReportsUnavailableActionAsMismatch pins that a provider refusing
// to guess an option (no single menu entry matches the action) surfaces as
// an interaction mismatch, not an internal error.
func TestRespondReportsUnavailableActionAsMismatch(t *testing.T) {
	store := beads.NewMemStore()
	sp := &respondErrorProvider{Fake: runtime.NewFake(), err: fmt.Errorf("%w: 0 menu options match action \"deny\"", runtime.ErrInteractionActionUnavailable)}
	mgr := newTestManager(t, store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "tmux-abc", Kind: "approval"})

	err = mgr.Respond(info.ID, runtime.InteractionResponse{Action: "deny"})
	if !errors.Is(err, ErrInteractionMismatch) {
		t.Fatalf("Respond error = %v, want ErrInteractionMismatch", err)
	}
}

// TestSubmitDefaultQueuesOverACrashedAgent (mc-48az4): a message submitted
// right after the agent crashed, its pane still up but the agent dead and
// the controller's heal not yet run, is queued for the incarnation the heal
// brings up (queued, deferred: the controller is poked) rather than typed
// into the dead pane and lost. It keeps the current epoch's stamp: delivery
// re-stamps the live epoch, so a heal's rotation does not lose it
// (TestCrashQueuedMessageSurvivesEpochRotation in cmd/gc). A live agent's
// message is delivered as before.
func TestSubmitDefaultQueuesOverACrashedAgent(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithCityPath(cityPath))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, "continuation_epoch", "3"); err != nil {
		t.Fatal(err)
	}
	origPoller := startSessionSubmitPoller
	startSessionSubmitPoller = func(string, string, string) error { return nil }
	defer func() { startSessionSubmitPoller = origPoller }()
	hints := runtime.Config{WorkDir: info.WorkDir, ProcessNames: []string{"claude"}}

	sp.Zombies[info.SessionName] = true
	before := len(sp.Calls)
	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorBackground), info.ID, "after the crash", BuildResumeCommand(info), hints, SubmitIntentDefault)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !outcome.Queued || !outcome.Deferred {
		t.Fatalf("outcome %+v, want queued and deferred", outcome)
	}
	for _, call := range sp.Calls[before:] {
		if call.Name == info.SessionName && (call.Method == "Nudge" || call.Method == "NudgeNow" || call.Method == "SendKeys" || call.Method == "Start") {
			t.Fatalf("call %+v on the crashed pane, want the message queued untouched", call)
		}
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || state.Pending[0].SessionID != info.ID || state.Pending[0].Message != "after the crash" {
		t.Fatalf("pending %+v, want the message queued for the session", state.Pending)
	}

	delete(sp.Zombies, info.SessionName)
	outcome, err = mgr.Submit(context.Background(), testActor(mgr, ActorBackground), info.ID, "to a live agent", BuildResumeCommand(info), hints, SubmitIntentDefault)
	if err != nil || outcome.Queued {
		t.Fatalf("live: outcome %+v (%v), want delivered", outcome, err)
	}
}

// busyNudgeProvider defers every wait-idle Nudge with runtime.ErrSessionBusy,
// typing nothing, the way the tmux provider answers a Codex pane still running
// a turn when its wait for idle runs out.
type busyNudgeProvider struct{ *runtime.Fake }

func (p *busyNudgeProvider) Nudge(name string, _ []runtime.ContentBlock) error {
	return fmt.Errorf("%w: session %q is still running a turn", runtime.ErrSessionBusy, name)
}

// TestSendQueuesWhenTheRuntimeDefersABusySession: a default send the runtime
// defers rather than interrupt a running turn is queued for later delivery
// (queued, deferred), not reported as a failure that loses it; the live-only
// send, whose caller is the queue itself, reports the deferral so the queue
// keeps its item.
func TestSendQueuesWhenTheRuntimeDefersABusySession(t *testing.T) {
	store := beads.NewMemStore()
	sp := &busyNudgeProvider{Fake: runtime.NewFake()}
	cityPath := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithCityPath(cityPath))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	origPoller := startSessionSubmitPoller
	startSessionSubmitPoller = func(string, string, string) error { return nil }
	defer func() { startSessionSubmitPoller = origPoller }()
	hints := runtime.Config{WorkDir: info.WorkDir}

	out, err := mgr.Send(context.Background(), testActor(mgr, ActorBackground), info.ID, "while you work", BuildResumeCommand(info), hints)
	if err != nil || !out.Queued || !out.Deferred {
		t.Fatalf("Send = %+v, %v; want queued and deferred", out, err)
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || state.Pending[0].SessionID != info.ID || state.Pending[0].Message != "while you work" {
		t.Fatalf("pending %+v, want the message queued for the session", state.Pending)
	}

	delivered, err := mgr.SendLiveOnly(context.Background(), info.ID, "x", nil)
	if delivered || !errors.Is(err, runtime.ErrSessionBusy) {
		t.Fatalf("SendLiveOnly = %t, %v; want undelivered with runtime.ErrSessionBusy", delivered, err)
	}
}

// TestSubmitDefaultCodexQueuesWhenTheRuntimeDefersABusyTurn: a default Codex
// submit goes out through NudgeNow, and the tmux runtime answers a Codex pane
// still running a turn with runtime.ErrSessionBusy, nothing typed, rather than
// lead its submit with an Escape that aborts that turn. The submit is then
// queued for the next idle boundary, never reported delivered.
func TestSubmitDefaultCodexQueuesWhenTheRuntimeDefersABusyTurn(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithCityPath(cityPath))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	origPoller := startSessionSubmitPoller
	startSessionSubmitPoller = func(string, string, string) error { return nil }
	defer func() { startSessionSubmitPoller = origPoller }()
	sp.NudgeErrors = map[string]error{info.SessionName: fmt.Errorf("%w: session %q is still running a turn", runtime.ErrSessionBusy, info.SessionName)}

	out, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "marker", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
	if err != nil || !out.Queued {
		t.Fatalf("Submit(default) = %+v, %v; want queued", out, err)
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || state.Pending[0].SessionID != info.ID || state.Pending[0].Message != "marker" {
		t.Fatalf("pending %+v, want the submit queued for the session", state.Pending)
	}
}

// TestSubmitDefaultCodexReportsALostDeliveryAsAFailure: when the runtime
// proves the message never landed (the tmux runtime's ErrNudgeSubmitLost:
// Codex's rollout records the turn aborted and never the message), the submit
// fails rather than report a delivery. Only a delivery the runtime proved
// (runtime.ErrNudgeDeliveredUnobserved) counts as one.
func TestSubmitDefaultCodexReportsALostDeliveryAsAFailure(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	lost := errors.New("nudge: submit Enter delivered to tmux but not confirmed (busy state never observed): the provider ended the turn before it recorded the message")
	sp.NudgeErrors = map[string]error{info.SessionName: lost}

	out, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "marker", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
	if !errors.Is(err, lost) {
		t.Fatalf("Submit(default) = %+v, %v; want the lost delivery reported", out, err)
	}
	if out.Queued {
		t.Fatalf("Submit(default) = %+v, want not queued", out)
	}
}

// TestSendPathsQueueOverACrashedAgent (mc-wdjni): every send path, not only
// Submit's default, holds text off a pane whose agent is dead: Send and
// SendImmediate queue it (queued, deferred), the live-only sends and the
// wait-idle nudges report it undelivered for their caller to queue, and
// nothing is typed into the pane. A suspended row with its pane up is no
// such pane: an operator's resume resumes it (the suspended exclusion).
func TestSendPathsQueueOverACrashedAgent(t *testing.T) {
	setup := func(t *testing.T) (*Manager, *runtime.Fake, Info, string, runtime.Config) {
		store := beads.NewMemStore()
		sp := runtime.NewFake()
		cityPath := t.TempDir()
		mgr := NewManagerWithOptions(store, sp, WithCityPath(cityPath))
		info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		origPoller := startSessionSubmitPoller
		startSessionSubmitPoller = func(string, string, string) error { return nil }
		t.Cleanup(func() { startSessionSubmitPoller = origPoller })
		sp.Zombies[info.SessionName] = true
		return mgr, sp, info, cityPath, runtime.Config{WorkDir: info.WorkDir, ProcessNames: []string{"claude"}}
	}
	nudged := func(sp *runtime.Fake, name string, before int) bool {
		for _, call := range sp.Calls[before:] {
			if call.Name == name && (call.Method == "Nudge" || call.Method == "NudgeNow") {
				return true
			}
		}
		return false
	}
	for name, send := range map[string]func(*Manager, Info, runtime.Config) (SubmitOutcome, error){
		"Send": func(m *Manager, i Info, h runtime.Config) (SubmitOutcome, error) {
			return m.Send(context.Background(), testActor(m, ActorBackground), i.ID, "after the crash", BuildResumeCommand(i), h)
		},
		"SendImmediate": func(m *Manager, i Info, h runtime.Config) (SubmitOutcome, error) {
			return m.SendImmediate(context.Background(), testActor(m, ActorBackground), i.ID, "after the crash", BuildResumeCommand(i), h)
		},
	} {
		t.Run(name, func(t *testing.T) {
			mgr, sp, info, cityPath, hints := setup(t)
			before := len(sp.Calls)
			out, err := send(mgr, info, hints)
			if err != nil || !out.Queued || !out.Deferred || nudged(sp, info.SessionName, before) {
				t.Fatalf("outcome %+v (%v), nudged %t: want queued and deferred, nothing typed", out, err, nudged(sp, info.SessionName, before))
			}
			if state, err := nudgequeue.LoadState(cityPath); err != nil || len(state.Pending) != 1 {
				t.Fatalf("pending %+v (%v), want the message queued", state.Pending, err)
			}
		})
	}
	t.Run("live-only and wait-idle", func(t *testing.T) {
		mgr, sp, info, _, hints := setup(t)
		before := len(sp.Calls)
		for name, deliver := range map[string]func() (bool, error){
			"SendLiveOnly": func() (bool, error) { return mgr.SendLiveOnly(context.Background(), info.ID, "x", hints.ProcessNames) },
			"SendImmediateLiveOnly": func() (bool, error) {
				return mgr.SendImmediateLiveOnly(context.Background(), info.ID, "x", hints.ProcessNames)
			},
			"TryWaitIdleNudgeLiveOnly": func() (bool, error) {
				return mgr.TryWaitIdleNudgeLiveOnly(context.Background(), info.ID, "session", "x", hints.ProcessNames)
			},
			"TryWaitIdleNudge": func() (bool, error) {
				return mgr.TryWaitIdleNudge(context.Background(), testActor(mgr, ActorBackground), info.ID, "session", "x", BuildResumeCommand(info), hints)
			},
		} {
			if delivered, err := deliver(); err != nil || delivered {
				t.Errorf("%s = %t, %v; want undelivered over the dead agent", name, delivered, err)
			}
		}
		// An ACP session's wait-idle nudge goes straight to the pane: held
		// off a dead agent too.
		if err := mgr.store.SetMetadata(info.ID, "transport", "acp"); err != nil {
			t.Fatal(err)
		}
		if delivered, err := mgr.TryWaitIdleNudgeLiveOnly(context.Background(), info.ID, "session", "x", hints.ProcessNames); err != nil || delivered {
			t.Errorf("ACP TryWaitIdleNudgeLiveOnly = %t, %v; want undelivered over the dead agent", delivered, err)
		}
		if nudged(sp, info.SessionName, before) {
			t.Fatal("a send typed into the dead pane")
		}
	})
	t.Run("suspended exclusion", func(t *testing.T) {
		mgr, _, info, cityPath, hints := setup(t)
		if err := mgr.store.SetMetadata(info.ID, "state", string(StateSuspended)); err != nil {
			t.Fatal(err)
		}
		out, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "resume me", BuildResumeCommand(info), hints, SubmitIntentDefault)
		if err != nil || out.Queued {
			t.Fatalf("outcome %+v (%v), want the operator's resume to resume the suspended row, not queue", out, err)
		}
		if state, _ := nudgequeue.LoadState(cityPath); len(state.Pending) != 0 {
			t.Fatalf("pending %+v, want nothing queued", state.Pending)
		}
	})
}

// emptyNamesDeadFake answers ProcessAlive false for an empty process-name
// list, as a provider that cannot check may.
type emptyNamesDeadFake struct{ *runtime.Fake }

func (f emptyNamesDeadFake) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return false
	}
	return f.Fake.ProcessAlive(name, processNames)
}

// TestSendWithoutProcessNamesReadsAlive (mc-wdjni): with no process names to
// check, the agent reads alive whatever the provider answers for an empty
// list, as the runtime's liveness does, so the text is delivered.
func TestSendWithoutProcessNamesReadsAlive(t *testing.T) {
	store := beads.NewMemStore()
	fake := runtime.NewFake()
	mgr := NewManagerWithOptions(store, emptyNamesDeadFake{fake}, WithCityPath(t.TempDir()))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	out, err := mgr.Send(context.Background(), testActor(mgr, ActorBackground), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
	if err != nil || out.Queued {
		t.Fatalf("outcome %+v (%v), want delivered", out, err)
	}
	if delivered, err := mgr.SendLiveOnly(context.Background(), info.ID, "hello", nil); err != nil || !delivered {
		t.Fatalf("SendLiveOnly = %t, %v; want delivered", delivered, err)
	}
}

// staleCacheFake is a caching provider caught between a crash and its next
// refresh, as the tmux state cache is for up to its TTL (and longer on a
// failed refresh): IsRunning and ProcessAlive answer from the snapshot taken
// before the crash (the agent alive), while a fresh read (ObserveLivenessSince)
// sees the truth, or fails with freshErr.
type staleCacheFake struct {
	*runtime.Fake
	freshErr error
}

func (f staleCacheFake) ProcessAlive(name string, _ []string) bool { return f.IsRunning(name) }

func (f staleCacheFake) ObserveLivenessSince(name string, processNames []string, _ time.Time) (runtime.Liveness, error) {
	if f.freshErr != nil {
		return runtime.Liveness{}, f.freshErr
	}
	running := f.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running && f.Fake.ProcessAlive(name, processNames)}, nil
}

// crashedBehindStaleCache is a claude row whose agent crashed in a pane
// still up, behind a provider cache that still shows it alive. Its fresh read
// fails with freshErr only once the row is created, since a create refuses
// on a fresh read that cannot answer.
func crashedBehindStaleCache(t *testing.T, provider string, freshErr error) (*Manager, *runtime.Fake, Info, string, runtime.Config) {
	t.Helper()
	store := beads.NewMemStore()
	fake := runtime.NewFake()
	cityPath := t.TempDir()
	sp := &staleCacheFake{Fake: fake}
	mgr := NewManagerWithOptions(store, sp, WithCityPath(cityPath))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: provider, WorkDir: t.TempDir(), Provider: provider, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.freshErr = freshErr
	origPoller := startSessionSubmitPoller
	startSessionSubmitPoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startSessionSubmitPoller = origPoller })
	fake.Zombies[info.SessionName] = true
	return mgr, fake, info, cityPath, runtime.Config{WorkDir: info.WorkDir, ProcessNames: []string{provider}}
}

// touchedPane reports a call after before that typed into, interrupted,
// stopped or started name.
func touchedPane(sp *runtime.Fake, name string, before int) []string {
	var out []string
	for _, c := range sp.Calls[before:] {
		switch c.Method {
		case "Nudge", "NudgeNow", "SendKeys", "Interrupt", "Stop", "Start":
			if c.Name == name {
				out = append(out, c.Method)
			}
		}
	}
	return out
}

// TestDeliveryDecidesOnAFreshRead (J26 C2, mc-48az4): delivery decides on a
// fresh read of the runtime, not the provider's cache, which a crash does
// not invalidate. With the cache still showing the crashed agent alive, a
// send of every kind is queued (queued and deferred, so the controller is
// poked) or reported undelivered, and nothing touches the pane; a fresh read
// that fails queues too.
func TestDeliveryDecidesOnAFreshRead(t *testing.T) {
	for name, freshErr := range map[string]error{"agent dead": nil, "fresh read fails": errors.New("tmux state refresh failed")} {
		t.Run(name, func(t *testing.T) {
			for kind, send := range map[string]func(*Manager, Info, runtime.Config) (SubmitOutcome, error){
				"Submit default, background": func(m *Manager, i Info, h runtime.Config) (SubmitOutcome, error) {
					return m.Submit(context.Background(), testActor(m, ActorBackground), i.ID, "after the crash", BuildResumeCommand(i), h, SubmitIntentDefault)
				},
				"Submit default, operator": func(m *Manager, i Info, h runtime.Config) (SubmitOutcome, error) {
					return m.Submit(context.Background(), testActor(m, ActorOperator), i.ID, "after the crash", BuildResumeCommand(i), h, SubmitIntentDefault)
				},
				"Submit interrupt_now": func(m *Manager, i Info, h runtime.Config) (SubmitOutcome, error) {
					return m.Submit(context.Background(), testActor(m, ActorBackground), i.ID, "after the crash", BuildResumeCommand(i), h, SubmitIntentInterruptNow)
				},
				"SendImmediate": func(m *Manager, i Info, h runtime.Config) (SubmitOutcome, error) {
					return m.SendImmediate(context.Background(), testActor(m, ActorBackground), i.ID, "after the crash", BuildResumeCommand(i), h)
				},
			} {
				t.Run(kind, func(t *testing.T) {
					mgr, sp, info, cityPath, hints := crashedBehindStaleCache(t, "claude", freshErr)
					before := len(sp.Calls)
					out, err := send(mgr, info, hints)
					if err != nil || !out.Queued || !out.Deferred {
						t.Fatalf("outcome %+v (%v), want queued and deferred", out, err)
					}
					if touched := touchedPane(sp, info.SessionName, before); len(touched) != 0 {
						t.Fatalf("calls %v on the crashed pane, want none", touched)
					}
					if state, err := nudgequeue.LoadState(cityPath); err != nil || len(state.Pending) != 1 {
						t.Fatalf("pending %+v (%v), want the message queued", state.Pending, err)
					}
				})
			}
			t.Run("live-only", func(t *testing.T) {
				mgr, sp, info, _, hints := crashedBehindStaleCache(t, "claude", freshErr)
				before := len(sp.Calls)
				if delivered, err := mgr.SendLiveOnly(context.Background(), info.ID, "x", hints.ProcessNames); err != nil || delivered {
					t.Fatalf("SendLiveOnly = %t, %v; want undelivered", delivered, err)
				}
				if delivered, err := mgr.TryWaitIdleNudgeLiveOnly(context.Background(), info.ID, "session", "x", hints.ProcessNames); err != nil || delivered {
					t.Fatalf("TryWaitIdleNudgeLiveOnly = %t, %v; want undelivered", delivered, err)
				}
				if touched := touchedPane(sp, info.SessionName, before); len(touched) != 0 {
					t.Fatalf("calls %v on the crashed pane, want none", touched)
				}
			})
		})
	}
}

// TestInterruptNowOverACrashedAgentReportsQueued (mc-wdjni): interrupt_now
// over a crashed agent sends no interrupt keys and queues the message, and
// says so (queued and deferred), for every provider family's interrupt.
func TestInterruptNowOverACrashedAgentReportsQueued(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "gemini", "pi"} {
		t.Run(provider, func(t *testing.T) {
			mgr, sp, info, cityPath, hints := crashedBehindStaleCache(t, provider, nil)
			before := len(sp.Calls)
			out, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "after the crash", BuildResumeCommand(info), hints, SubmitIntentInterruptNow)
			if err != nil || !out.Queued || !out.Deferred {
				t.Fatalf("outcome %+v (%v), want queued and deferred", out, err)
			}
			if touched := touchedPane(sp, info.SessionName, before); len(touched) != 0 {
				t.Fatalf("calls %v on the crashed pane, want none", touched)
			}
			if state, err := nudgequeue.LoadState(cityPath); err != nil || len(state.Pending) != 1 {
				t.Fatalf("pending %+v (%v), want the message queued", state.Pending, err)
			}
		})
	}
}

// TestCrashedAgentStopAndKillFencesStillRefuse (mc-wdjni): the dead-agent
// check runs after ensureRunning's refusals, so a send to a row whose
// drain-ack stop or kill is pending is still refused (the API's 409), not
// queued for a session that is going away.
func TestCrashedAgentStopAndKillFencesStillRefuse(t *testing.T) {
	for name, c := range map[string]struct {
		meta map[string]string
		want error
	}{
		"drain-ack stop pending": {map[string]string{"state": string(StateDraining), "state_reason": DrainAckStopPendingReason}, ErrSessionStopping},
		"kill pending":           {map[string]string{"state": string(StateAsleep), "state_reason": KillPendingReason, "sleep_reason": string(SleepReasonKilled), "slept_at": time.Now().UTC().Format(time.RFC3339)}, ErrSessionKillPending},
	} {
		t.Run(name, func(t *testing.T) {
			mgr, sp, info, cityPath, hints := crashedBehindStaleCache(t, "claude", nil)
			if err := mgr.store.SetMetadataBatch(info.ID, c.meta); err != nil {
				t.Fatal(err)
			}
			before := len(sp.Calls)
			out, err := mgr.Send(context.Background(), testActor(mgr, ActorBackground), info.ID, "x", BuildResumeCommand(info), hints)
			if !errors.Is(err, c.want) || out.Queued {
				t.Fatalf("Send = %+v, %v; want %v, nothing queued", out, err, c.want)
			}
			if touched := touchedPane(sp, info.SessionName, before); len(touched) != 0 {
				t.Fatalf("calls %v on the crashed pane, want none", touched)
			}
			if state, _ := nudgequeue.LoadState(cityPath); len(state.Pending) != 0 {
				t.Fatalf("pending %+v, want nothing queued", state.Pending)
			}
		})
	}
}

func TestIsGoTestExecutableForBazelRunfilesSymlink(t *testing.T) {
	dir := t.TempDir()
	realBin := filepath.Join(dir, "bin", "session_test_", "session_test")
	spyBin := filepath.Join(dir, "spy", "session_test")
	// A real gc started by a bazel integration test runs with TEST_SRCDIR set
	// and is exec'd through its runfiles symlink; only the _test name gate
	// keeps it from being treated as a test binary.
	gcBin := filepath.Join(dir, "bin", "gc_", "gc")
	for _, p := range []string{realBin, spyBin, gcBin} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	// Bazel's test-setup.sh execs the test via its runfiles path, which is a
	// symlink to the built binary under local (non-RBE) execution, while
	// os.Executable reports the resolved target.
	runfilesLink := filepath.Join(dir, "runfiles", "_main", "internal", "session", "session_test")
	if err := os.MkdirAll(filepath.Dir(runfilesLink), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(realBin, runfilesLink); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	gcRunfilesLink := filepath.Join(dir, "runfiles", "_main", "cmd", "gc", "gc_", "gc")
	if err := os.MkdirAll(filepath.Dir(gcRunfilesLink), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(gcBin, gcRunfilesLink); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	resolvedReal, err := filepath.EvalSymlinks(realBin)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	resolvedGC, err := filepath.EvalSymlinks(gcBin)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	const srcdir = "/runfiles"

	tests := []struct {
		name       string
		path       string
		argv0      string
		testSrcdir string
		want       bool
	}{
		{"go test binary", filepath.Join(dir, "session.test"), "/elsewhere/x", "", true},
		{"bazel direct exec", realBin, realBin, srcdir, true},
		{"bazel runfiles symlink exec", resolvedReal, runfilesLink, srcdir, true},
		{"symlink exec outside bazel", resolvedReal, runfilesLink, "", false},
		{"renamed spy binary", spyBin, runfilesLink, srcdir, false},
		{"renamed spy binary direct argv0", spyBin, realBin, srcdir, false},
		{"unresolvable argv0", resolvedReal, filepath.Join(dir, "missing", "session_test"), srcdir, false},
		{"production gc binary", "/usr/local/bin/gc", "/usr/local/bin/gc", "", false},
		{"gc under bazel runfiles symlink exec", resolvedGC, gcRunfilesLink, srcdir, false},
		{"gc under bazel direct exec", resolvedGC, resolvedGC, srcdir, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGoTestExecutableFor(tt.path, tt.argv0, tt.testSrcdir); got != tt.want {
				t.Fatalf("isGoTestExecutableFor(%q, %q, %q) = %v, want %v", tt.path, tt.argv0, tt.testSrcdir, got, tt.want)
			}
		})
	}
}

// TestIsGoTestExecutableRecognizesRunningTestBinary pins the argv0 and
// TEST_SRCDIR wiring in isGoTestExecutable, which the table above bypasses by
// calling isGoTestExecutableFor. Plain go test names the binary *.test, which
// matches without that wiring, so the wiring is only exercised under bazel.
func TestIsGoTestExecutableRecognizesRunningTestBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if !isGoTestExecutable(exe) {
		t.Fatalf("isGoTestExecutable(%q) = false (argv0 %q, TEST_SRCDIR %q), want true for the running test binary", exe, os.Args[0], os.Getenv("TEST_SRCDIR"))
	}
}
