package session

import (
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
		t.Fatalf("epoch after a plain restart = %d, want 4", again)
	}
}
