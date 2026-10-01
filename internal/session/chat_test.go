package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSessionMutationLocksArePerSession(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})

	go func() {
		err := withSessionMutationLock("session-a", func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
		if err != nil {
			t.Errorf("lock session-a: %v", err)
		}
	}()

	select {
	case <-firstEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("session-a lock was not acquired")
	}

	go func() {
		err := withSessionMutationLock("session-b", func() error {
			close(secondEntered)
			return nil
		})
		if err != nil {
			t.Errorf("lock session-b: %v", err)
		}
	}()

	select {
	case <-secondEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("session-b was blocked by unrelated session lock")
	}

	close(releaseFirst)
}

// TestStripSessionIDFlag pins the first-start counterpart of stripResumeFlag.
// Without it, a dead "claude ... --session-id <key>" first start retried
// byte-identically, and claude rejects a reused id ("Session ID <uuid> is
// already in use", exit 1), so the retry could never succeed.
func TestStripSessionIDFlag(t *testing.T) {
	tests := []struct {
		name          string
		cmd           string
		sessionIDFlag string
		sessionKey    string
		want          string
	}{
		{
			name:          "removes session id flag and key at end",
			cmd:           "claude --dangerously-skip-permissions --session-id abc-123",
			sessionIDFlag: "--session-id",
			sessionKey:    "abc-123",
			want:          "claude --dangerously-skip-permissions",
		},
		{
			name:          "removes session id flag mid-command",
			cmd:           "claude --session-id abc-123 --effort max",
			sessionIDFlag: "--session-id",
			sessionKey:    "abc-123",
			want:          "claude --effort max",
		},
		{
			name:          "removes equals form",
			cmd:           "claude --session-id=abc-123 --effort max",
			sessionIDFlag: "--session-id",
			sessionKey:    "abc-123",
			want:          "claude --effort max",
		},
		{
			name:          "different key is left alone",
			cmd:           "claude --session-id other-key",
			sessionIDFlag: "--session-id",
			sessionKey:    "abc-123",
			want:          "claude --session-id other-key",
		},
		{
			name:          "empty session id flag",
			cmd:           "claude --session-id abc-123",
			sessionIDFlag: "",
			sessionKey:    "abc-123",
			want:          "claude --session-id abc-123",
		},
		{
			name:          "empty session key",
			cmd:           "claude --session-id abc-123",
			sessionIDFlag: "--session-id",
			sessionKey:    "",
			want:          "claude --session-id abc-123",
		},
		{
			// Same no-op contract as stripResumeFlag: callers detect a no-op by
			// exact equality, so a non-replacement path must not trim.
			name:          "no strip preserves surrounding whitespace",
			cmd:           "  claude --effort max  ",
			sessionIDFlag: "--session-id",
			sessionKey:    "abc-123",
			want:          "  claude --effort max  ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripSessionIDFlag(tt.cmd, tt.sessionIDFlag, tt.sessionKey)
			if got != tt.want {
				t.Errorf("stripSessionIDFlag(%q, %q, %q) = %q, want %q",
					tt.cmd, tt.sessionIDFlag, tt.sessionKey, got, tt.want)
			}
		})
	}
}

func TestStripResumeFlag(t *testing.T) {
	tests := []struct {
		name       string
		cmd        string
		resumeFlag string
		sessionKey string
		want       string
	}{
		{
			name:       "removes resume flag and key",
			cmd:        "claude --model claude-opus-4-7 --resume abc-123",
			resumeFlag: "--resume",
			sessionKey: "abc-123",
			want:       "claude --model claude-opus-4-7",
		},
		{
			name:       "resume flag at end",
			cmd:        "claude --resume abc-123",
			resumeFlag: "--resume",
			sessionKey: "abc-123",
			want:       "claude",
		},
		{
			name:       "no resume flag in command",
			cmd:        "claude --model sonnet",
			resumeFlag: "--resume",
			sessionKey: "abc-123",
			want:       "claude --model sonnet",
		},
		{
			name:       "empty resume flag",
			cmd:        "claude --resume abc-123",
			resumeFlag: "",
			sessionKey: "abc-123",
			want:       "claude --resume abc-123",
		},
		{
			name:       "empty session key",
			cmd:        "claude --resume abc-123",
			resumeFlag: "--resume",
			sessionKey: "",
			want:       "claude --resume abc-123",
		},
		{
			// PR #2035 review: callers rely on freshCmd == cmd to detect
			// a no-op strip. TrimSpace on a non-replacement path would
			// silently change the return value when cmd has padding,
			// breaking that signal.
			name:       "no strip preserves leading and trailing whitespace",
			cmd:        "  claude --model sonnet  ",
			resumeFlag: "--resume",
			sessionKey: "abc-123",
			want:       "  claude --model sonnet  ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripResumeFlag(tt.cmd, tt.resumeFlag, tt.sessionKey)
			if got != tt.want {
				t.Errorf("stripResumeFlag(%q, %q, %q) = %q, want %q",
					tt.cmd, tt.resumeFlag, tt.sessionKey, got, tt.want)
			}
		})
	}
}

func TestStripResumeFlagArg(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		resumeFlag  string
		resumeStyle string
		want        string
	}{
		{
			// The diverged-key case: the embedded key differs from the
			// bead's current session_key, so the keyed strip was a no-op.
			// The value-agnostic strip must still remove the generated
			// trailing "--resume <key>" suffix.
			name:        "flag style removes generated trailing resume key",
			cmd:         `claude --settings "x" --resume diverged-key-999`,
			resumeFlag:  "--resume",
			resumeStyle: "flag",
			want:        `claude --settings "x"`,
		},
		{
			name:        "flag style preserves earlier resume text",
			cmd:         `claude --label "--resume keep-me" --resume diverged-key-999`,
			resumeFlag:  "--resume",
			resumeStyle: "flag",
			want:        `claude --label "--resume keep-me"`,
		},
		{
			name:        "flag style preserves non-generated resume flag",
			cmd:         "claude --resume abc-123 --model sonnet",
			resumeFlag:  "--resume",
			resumeStyle: "flag",
			want:        "claude --resume abc-123 --model sonnet",
		},
		{
			name:        "subcommand-style resume token",
			cmd:         "codex resume key-abc --model o3",
			resumeFlag:  "resume",
			resumeStyle: "subcommand",
			want:        "codex --model o3",
		},
		{
			// No resume flag present: command is already a fresh start, so
			// it must be returned unchanged (callers launch it as-is).
			name:        "no resume flag returns command unchanged",
			cmd:         "claude --model sonnet",
			resumeFlag:  "--resume",
			resumeStyle: "flag",
			want:        "claude --model sonnet",
		},
		{
			name:        "empty resume flag returns command unchanged",
			cmd:         "claude --resume abc-123",
			resumeFlag:  "",
			resumeStyle: "flag",
			want:        "claude --resume abc-123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripResumeFlagArg(tt.cmd, tt.resumeFlag, tt.resumeStyle)
			if got != tt.want {
				t.Errorf("stripResumeFlagArg(%q, %q, %q) = %q, want %q",
					tt.cmd, tt.resumeFlag, tt.resumeStyle, got, tt.want)
			}
		})
	}
}

// TestStripSessionIDFlagArg pins the value-agnostic fallback for
// stripSessionIDFlag. When the session id embedded in a first-start command
// diverges from the bead's current session_key, the keyed strip is a no-op; this
// fallback must still drop the "--session-id <value>" pair regardless of value so
// the retry doesn't replay a dead id. A command with no session-id flag is
// already a valid fresh start and must be returned unchanged.
func TestStripSessionIDFlagArg(t *testing.T) {
	tests := []struct {
		name          string
		cmd           string
		sessionIDFlag string
		want          string
	}{
		{
			// The diverged-key case: the embedded key differs from the bead's
			// session_key, so the keyed strip was a no-op. The value-agnostic
			// strip must still remove the trailing "--session-id <key>".
			name:          "removes diverged trailing session id",
			cmd:           "claude --dangerously-skip-permissions --session-id key-A-diverged",
			sessionIDFlag: "--session-id",
			want:          "claude --dangerously-skip-permissions",
		},
		{
			name:          "removes diverged session id mid-command",
			cmd:           "claude --session-id key-A-diverged --effort max",
			sessionIDFlag: "--session-id",
			want:          "claude --effort max",
		},
		{
			name:          "removes equals form regardless of value",
			cmd:           "claude --session-id=key-A-diverged --effort max",
			sessionIDFlag: "--session-id",
			want:          "claude --effort max",
		},
		{
			// No session-id flag present: the command is already a fresh start,
			// so it must be returned unchanged (callers launch it as-is).
			name:          "no session id flag returns command unchanged",
			cmd:           "claude --model sonnet",
			sessionIDFlag: "--session-id",
			want:          "claude --model sonnet",
		},
		{
			name:          "empty session id flag returns command unchanged",
			cmd:           "claude --session-id key-A-diverged",
			sessionIDFlag: "",
			want:          "claude --session-id key-A-diverged",
		},
		{
			// Quote-prefixed flag text ("--session-id glued to the opening
			// quote) is not at a space boundary, so the boundary-anchored strip
			// skips it and removes only the real trailing generated pair. This is
			// a real guarantee of the in-place strip, not an incidental artifact
			// of tokenization.
			name:          "preserves quote-prefixed session id literal",
			cmd:           `claude --label "--session-id keep-me" --session-id key-A-diverged`,
			sessionIDFlag: "--session-id",
			want:          `claude --label "--session-id keep-me"`,
		},
		{
			// Documented limitation, pinned so a future regression is visible: the
			// strip is shell-token-simple, so a *space-separated* bare "--session-id"
			// token inside a quoted argument sits at a space boundary and IS matched.
			// This never occurs for the framework-generated commands this fallback
			// sees (they carry exactly one, top-level "--session-id"); if command
			// construction ever admits caller-supplied quoted text, make the strip
			// shell-quote-aware and update this expectation.
			name:          "space-separated flag token inside quotes is not shell-aware",
			cmd:           `claude --note "use --session-id here" --session-id key-A-diverged`,
			sessionIDFlag: "--session-id",
			want:          `claude --note "use --session-id key-A-diverged`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripSessionIDFlagArg(tt.cmd, tt.sessionIDFlag)
			if got != tt.want {
				t.Errorf("stripSessionIDFlagArg(%q, %q) = %q, want %q",
					tt.cmd, tt.sessionIDFlag, got, tt.want)
			}
		})
	}
}

func TestSessionMutationLocksSerializeSameSession(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})

	go func() {
		err := withSessionMutationLock("shared-session", func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
		if err != nil {
			t.Errorf("first lock: %v", err)
		}
	}()

	select {
	case <-firstEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("first lock was not acquired")
	}

	go func() {
		err := withSessionMutationLock("shared-session", func() error {
			close(secondEntered)
			return nil
		})
		if err != nil {
			t.Errorf("second lock: %v", err)
		}
	}()

	select {
	case <-secondEntered:
		t.Fatal("same-session lock should block until the first holder releases")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)

	select {
	case <-secondEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("same-session lock did not unblock after release")
	}
}

// A conversation reset committed while the runtime is restarted outside the
// controller's pre-wake path must still rotate the continuation epoch: the
// marker is the only record that a reset is owed, and a start that ignores it
// republishes the pre-reset conversation identity.
func TestCommitPendingContinuationResetBumpsAndClears(t *testing.T) {
	store := beads.NewMemStore()
	m := NewManagerWithOptions(store, runtime.NewFake())
	b, err := store.Create(beads.Bead{Title: "session", Metadata: map[string]string{
		"continuation_epoch":         "3",
		"continuation_reset_pending": "true",
	}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := b.ID

	// The consumer rotates: it bumps and clears the marker in one batch, so the
	// epoch advances exactly once per reset no matter which start path services
	// it.
	epoch, err := m.commitPendingContinuationReset(id, b)
	if err != nil {
		t.Fatalf("commitPendingContinuationReset: %v", err)
	}
	if epoch != 4 {
		t.Fatalf("epoch = %d, want 4", epoch)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if got := after.Metadata["continuation_epoch"]; got != "4" {
		t.Fatalf("persisted epoch = %q, want 4", got)
	}
	if got := after.Metadata["continuation_reset_pending"]; got != "" {
		t.Fatalf("reset marker = %q, want cleared", got)
	}

	// Idempotent: a plain restart must not keep bumping.
	again, err := m.commitPendingContinuationReset(id, after)
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if again != 4 {
		t.Fatalf("epoch after a plain restart = %d, want 4 (no second rotation)", again)
	}
}

// Rotation happens exactly once per reset, and it belongs to the consumer.
// RequestFreshRestart only records the intent, because the reconciler writes
// the same marker directly without ever calling it — rotating at request time
// would rotate on one entry path and not the other.
func TestRequestFreshRestartRecordsIntentAndConsumerRotatesOnce(t *testing.T) {
	store := beads.NewMemStore()
	m := NewManagerWithOptions(store, runtime.NewFake())
	b, err := store.Create(beads.Bead{Title: "session", Type: BeadType, Metadata: map[string]string{
		"state":              string(StateActive),
		"continuation_epoch": "2",
	}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := m.RequestFreshRestart(b.ID); err != nil {
		t.Fatalf("RequestFreshRestart: %v", err)
	}
	recorded, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := recorded.Metadata["continuation_epoch"]; got != "2" {
		t.Fatalf("epoch = %q, want 2 — the request records intent, it does not rotate", got)
	}
	if got := recorded.Metadata["continuation_reset_pending"]; got != "true" {
		t.Fatalf("reset marker = %q, want true", got)
	}

	// The start path that services the reset rotates it, once.
	epoch, err := m.commitPendingContinuationReset(b.ID, recorded)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if epoch != 3 {
		t.Fatalf("published epoch = %d, want 3", epoch)
	}
	serviced, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if got := serviced.Metadata["continuation_reset_pending"]; got != "" {
		t.Fatalf("marker = %q, want cleared so a second path cannot rotate again", got)
	}
	if again, err := m.commitPendingContinuationReset(b.ID, serviced); err != nil || again != 3 {
		t.Fatalf("second start path rotated again: epoch=%d err=%v", again, err)
	}
}

// A message arriving after a reset is recorded, but before the replacement
// runtime is up, must be queued for the incoming incarnation — not delivered
// into a conversation the operator already discarded.
func TestPendingConversationRestartDefersDelivery(t *testing.T) {
	for name, meta := range map[string]map[string]string{
		"reset pending":     {"continuation_reset_pending": "true"},
		"restart requested": {"restart_requested": "true"},
		"neither":           {},
	} {
		want := len(meta) > 0
		if got := pendingConversationRestart(beads.Bead{Metadata: meta}); got != want {
			t.Errorf("%s: pendingConversationRestart = %v, want %v", name, got, want)
		}
	}
}

// A resumed or respawned session pushes over SSH just like a freshly created
// one, so both chat.go start sites must inject the keepalive that
// createStarted does. Without it a long pre-push hook idles the transport
// until the push dies with SIGPIPE (ga-2i5).
func TestResumeInjectsSSHKeepalive(t *testing.T) {
	for name, start := range map[string]func(*Manager, Info) error{
		"Start": func(m *Manager, info Info) error {
			return m.Start(context.Background(), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
		},
		"StartRuntimeOnly": func(m *Manager, info Info) error {
			return m.StartRuntimeOnly(context.Background(), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GIT_SSH_COMMAND", "")
			mgr, sp, info := seedSuspendedResumeTarget(t)

			if err := start(mgr, info); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			cfg := sp.LastStartConfig(info.SessionName)
			if cfg == nil {
				t.Fatalf("Start call not recorded; events = %v", sp.events)
			}
			if !strings.Contains(cfg.Env["GIT_SSH_COMMAND"], "ServerAliveInterval") {
				t.Fatalf("GIT_SSH_COMMAND = %q, want SSH keepalive", cfg.Env["GIT_SSH_COMMAND"])
			}
		})
	}
}

// A seat sitting at an approval or selection prompt is waiting on a human, not
// stalled. The wait-idle paths reached that conclusion independently of
// sendLocked and delivered anyway, typing into the open prompt.
func TestTryWaitIdleNudgeRefusesPendingInteraction(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1", Kind: "approval", Prompt: "approve?",
	})

	if _, err := mgr.TryWaitIdleNudge(context.Background(), info.ID, "mail", "hello", "", runtime.Config{}); !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("TryWaitIdleNudge error = %v, want %v", err, ErrPendingInteraction)
	}
	for _, call := range sp.Calls {
		if (call.Method == "Nudge" || call.Method == "NudgeNow") && call.Name == info.SessionName {
			t.Fatalf("nudged a session awaiting human input: %#v", sp.Calls)
		}
	}
}

func TestTryWaitIdleNudgeLiveOnlyRefusesPendingInteraction(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatalf("precondition: CreateSession should leave the runtime running")
	}
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1", Kind: "approval", Prompt: "approve?",
	})

	if _, err := mgr.TryWaitIdleNudgeLiveOnly(context.Background(), info.ID, "mail", "hello"); !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("TryWaitIdleNudgeLiveOnly error = %v, want %v", err, ErrPendingInteraction)
	}
}

// The guard must sit after ensureRunning, as sendLocked's does. A dormant
// session has no pane to probe, so checking first turns a managed wake into
// ErrSessionNotFound and the session is never started.
func TestTryWaitIdleNudgeStillWakesDormantSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := sp.Stop(info.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if sp.IsRunning(info.SessionName) {
		t.Fatalf("precondition: session must be dormant before the nudge")
	}

	sp.Calls = nil
	if _, err := mgr.TryWaitIdleNudge(context.Background(), info.ID, "mail", "hello", "claude --resume", runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("TryWaitIdleNudge on a dormant session = %v, want the wake to proceed", err)
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatalf("dormant session was not started: %#v", sp.Calls)
	}
	// Pin the ORDER, not just the outcome. A fake reports no pending
	// interaction for a session that does not exist, but a real pane probe
	// returns ErrSessionNotFound, so a guard placed ahead of the start would
	// refuse the wake in production while this fake still passed.
	started := -1
	for i, call := range sp.Calls {
		if call.Method == "Start" && call.Name == info.SessionName {
			started = i
			break
		}
	}
	if started < 0 {
		t.Fatalf("no Start recorded for the dormant session: %#v", sp.Calls)
	}
	for i, call := range sp.Calls {
		if call.Method == "Pending" && call.Name == info.SessionName && i < started {
			t.Fatalf("probed for a pending interaction before starting the session: %#v", sp.Calls)
		}
	}
}

// gateWaitForIdle makes WaitForIdle block until the returned release func runs,
// and reports when the wait has actually begun. Every other test sets the
// pending interaction BEFORE the call, which cannot tell a pre-wait probe from
// a pre-delivery one; this reproduces the case that can: the prompt opens
// during the wait.
func gateWaitForIdle(sp *runtime.Fake, sessName string) (started <-chan struct{}, release func()) {
	startedCh := make(chan struct{})
	gate := make(chan struct{})
	sp.WaitForIdleErrors[sessName] = nil // the wait succeeds — as it does when a prompt clears the busy indicator
	sp.WaitForIdleGates[sessName] = gate
	sp.WaitForIdleStarted[sessName] = startedCh
	return startedCh, func() { close(gate) }
}

func assertNoNudge(t *testing.T, sp *runtime.Fake, sessName string) {
	t.Helper()
	for _, call := range sp.SnapshotCalls() {
		if (call.Method == "Nudge" || call.Method == "NudgeNow") && call.Name == sessName {
			t.Fatalf("nudged a session awaiting human input: %#v", sp.SnapshotCalls())
		}
	}
}

// The pre-wait probe only proves no prompt was open up to waitIdleNudgeTimeout
// ago. WaitForIdle is liable to return BECAUSE a dialog opened — a seat that
// raises an approval prompt stops emitting the busy indicator at that moment
// while the ready-prompt prefix stays on the pane — so the delivery needs a
// probe adjacent to its keystrokes, which sendLocked gets from the guarded
// delivery helper and this path, owning its own wait, must run itself.
func TestTryWaitIdleNudgeRefusesPromptOpenedDuringWait(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	started, release := gateWaitForIdle(sp, info.SessionName)

	type result struct {
		delivered bool
		err       error
	}
	done := make(chan result, 1)
	go func() {
		delivered, err := mgr.TryWaitIdleNudge(context.Background(), info.ID, "mail", "hello", "", runtime.Config{})
		done <- result{delivered, err}
	}()

	<-started // the pre-wait probe has already passed: no prompt was open
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1", Kind: "approval", Prompt: "approve?",
	})
	release()

	got := <-done
	if !errors.Is(got.err, ErrPendingInteraction) {
		t.Fatalf("TryWaitIdleNudge error = %v, want %v (prompt opened during the wait)", got.err, ErrPendingInteraction)
	}
	if got.delivered {
		t.Fatalf("reported delivery into a prompt that opened during the wait")
	}
	assertNoNudge(t, sp, info.SessionName)
}

func TestTryWaitIdleNudgeLiveOnlyRefusesPromptOpenedDuringWait(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	started, release := gateWaitForIdle(sp, info.SessionName)

	type result struct {
		delivered bool
		err       error
	}
	done := make(chan result, 1)
	go func() {
		delivered, err := mgr.TryWaitIdleNudgeLiveOnly(context.Background(), info.ID, "mail", "hello")
		done <- result{delivered, err}
	}()

	<-started
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1", Kind: "approval", Prompt: "approve?",
	})
	release()

	got := <-done
	if !errors.Is(got.err, ErrPendingInteraction) {
		t.Fatalf("TryWaitIdleNudgeLiveOnly error = %v, want %v (prompt opened during the wait)", got.err, ErrPendingInteraction)
	}
	if got.delivered {
		t.Fatalf("reported delivery into a prompt that opened during the wait")
	}
	assertNoNudge(t, sp, info.SessionName)
}

// Live-only delivery is where refused wait-idle nudges come back: the mail and
// sling lanes queue what they could not deliver and the push poller re-delivers
// it here. Without this guard the refusal only defers the mistype by one
// quiescence window instead of preventing it.
func TestSendLiveOnlyRefusesPendingInteraction(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatalf("precondition: CreateSession should leave the runtime running")
	}
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1", Kind: "approval", Prompt: "approve?",
	})

	delivered, err := mgr.SendLiveOnly(context.Background(), info.ID, "hello")
	if !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("SendLiveOnly error = %v, want %v", err, ErrPendingInteraction)
	}
	if delivered {
		t.Fatalf("SendLiveOnly reported delivery to a seat awaiting human input")
	}
	assertNoNudge(t, sp, info.SessionName)

	// The immediate variant shares sendLiveOnly and must refuse identically.
	if delivered, err := mgr.SendImmediateLiveOnly(context.Background(), info.ID, "hello"); delivered || !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("SendImmediateLiveOnly = (%v, %v), want (false, %v)", delivered, err, ErrPendingInteraction)
	}
	assertNoNudge(t, sp, info.SessionName)
}
