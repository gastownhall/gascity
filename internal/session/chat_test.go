package session

import (
	"testing"
	"time"
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
