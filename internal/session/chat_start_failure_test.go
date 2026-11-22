package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestStartFailureLabel pins the verb a failed start reports: "resuming
// session" only for a command that carries a resume shape, "starting session"
// otherwise, with the cause still wrapped.
func TestStartFailureLabel(t *testing.T) {
	cause := fmt.Errorf("waiting for readiness: %w", context.DeadlineExceeded)
	cases := []struct {
		name string
		cmd  string
		meta map[string]string
		want string
	}{
		{
			name: "flag style with the bead's key",
			cmd:  "claude --dangerously --resume key-1",
			meta: map[string]string{"resume_flag": "--resume", "session_key": "key-1"},
			want: "resuming session",
		},
		{
			name: "flag style with a diverged key",
			cmd:  "claude --dangerously --resume key-0",
			meta: map[string]string{"resume_flag": "--resume", "session_key": "key-1"},
			want: "resuming session",
		},
		{
			name: "flag style with the key cleared",
			cmd:  "claude --dangerously --resume key-0",
			meta: map[string]string{"resume_flag": "--resume"},
			want: "resuming session",
		},
		{
			name: "subcommand style",
			cmd:  "codex resume key-1 --yolo",
			meta: map[string]string{"resume_flag": "resume", "resume_style": "subcommand", "session_key": "key-1"},
			want: "resuming session",
		},
		{
			name: "explicit resume_command",
			cmd:  "agent-cli continue key-1",
			meta: map[string]string{"resume_command": "agent-cli continue {{.SessionKey}}", "session_key": "key-1"},
			want: "resuming session",
		},
		{
			name: "no resume shape",
			cmd:  "claude --dangerously",
			meta: map[string]string{"resume_flag": "--resume", "session_key": "key-1"},
			want: "starting session",
		},
		{
			name: "provider without resume",
			cmd:  "agent-cli --serve",
			meta: map[string]string{},
			want: "starting session",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := startFailure(tc.cmd, tc.meta, cause)
			if want := tc.want + ": " + cause.Error(); err.Error() != want {
				t.Errorf("err = %q, want %q", err.Error(), want)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false", err)
			}
		})
	}
}

func startDeadline(sessName string) error {
	return fmt.Errorf("tmux start for %q: waiting for readiness: %w", sessName, context.DeadlineExceeded)
}

// startFailurePaths are the two bring-up paths whose provider Start failure
// the verb labels: the reconciler's runtime-only bridge and the manager's own
// Start.
var startFailurePaths = []struct {
	name  string
	start func(t *testing.T, mgr *Manager, id, cmd string) error
}{
	{
		name: "runtime only",
		start: func(t *testing.T, mgr *Manager, id, cmd string) error {
			return startRuntimeOnly(t, mgr, id, cmd, runtime.Config{WorkDir: "/tmp"})
		},
	},
	{
		name: "manager start",
		start: func(_ *testing.T, mgr *Manager, id, cmd string) error {
			return mgr.Start(context.Background(), testActor(mgr, ActorOperator), id, cmd, runtime.Config{WorkDir: "/tmp"})
		},
	},
}

func assertStartFailureVerb(t *testing.T, cmd, wantPrefix string) {
	t.Helper()
	for _, path := range startFailurePaths {
		t.Run(path.name, func(t *testing.T) {
			mgr, _, _, id, _ := seedResumableACPSession(t, startDeadline)
			err := path.start(t, mgr, id, cmd)
			if err == nil {
				t.Fatal("start succeeded, want the scripted deadline")
			}
			if !strings.HasPrefix(err.Error(), wantPrefix) {
				t.Errorf("err = %q, want prefix %q", err.Error(), wantPrefix)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false", err)
			}
		})
	}
}

// TestEnsureRunningRuntimeOnly_StartFailureSaysStarting pins that a start
// whose command carries no resume shape reports "starting session", not
// "resuming session", when the provider Start fails.
func TestEnsureRunningRuntimeOnly_StartFailureSaysStarting(t *testing.T) {
	assertStartFailureVerb(t, "claude --dangerously", "starting session: ")
}

// TestEnsureRunningRuntimeOnly_StartFailureSaysResuming pins that a start
// whose command carries "<resume_flag> <session_key>" still reports
// "resuming session".
func TestEnsureRunningRuntimeOnly_StartFailureSaysResuming(t *testing.T) {
	assertStartFailureVerb(t, capacityTestResumeCmd, "resuming session: ")
}
