package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCommandOutputTail pins the bounded-tail capture RunSetupCommand folds
// into setup-command failure messages. Copied from the tmux package with the
// extraction of its runSetupCommand core; the original still lives at
// internal/runtime/tmux/startup_test.go until tmux delegates here.
func TestCommandOutputTail(t *testing.T) {
	cases := []struct {
		name   string
		limit  int
		writes []string
		label  string
		want   string
	}{
		{name: "no output", limit: 8, writes: nil, label: "stderr", want: ""},
		{name: "whitespace only", limit: 8, writes: []string{" \n\t "}, label: "stderr", want: ""},
		{name: "under limit", limit: 8, writes: []string{"abc"}, label: "stderr", want: "stderr: abc"},
		{name: "exact limit has no marker", limit: 4, writes: []string{"abcd"}, label: "stderr", want: "stderr: abcd"},
		{name: "oversized single write keeps tail", limit: 4, writes: []string{"abcdefgh"}, label: "stderr", want: "stderr: ... efgh"},
		{name: "rollover across writes", limit: 4, writes: []string{"abc", "def"}, label: "stderr", want: "stderr: ... cdef"},
		{name: "many small writes", limit: 3, writes: []string{"a", "b", "c", "d", "e"}, label: "stdout", want: "stdout: ... cde"},
		{name: "zero limit drops content", limit: 0, writes: []string{"abc"}, label: "stderr", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tail := newCommandOutputTail(tc.limit)
			for _, w := range tc.writes {
				n, err := tail.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write(%q) error: %v", w, err)
				}
				if n != len(w) {
					t.Fatalf("Write(%q) = %d, want %d", w, n, len(w))
				}
			}
			if got := tail.Detail(tc.label); got != tc.want {
				t.Fatalf("Detail(%q) = %q, want %q", tc.label, got, tc.want)
			}
		})
	}
}

// TestRunSetupCommandUsesGCDIRAsWorkingDirectory pins the cwd contract: the
// command runs in env["GC_DIR"] when set, so relative paths in a setup
// command resolve against the session directory.
func TestRunSetupCommandUsesGCDIRAsWorkingDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	if err := RunSetupCommand(context.Background(), "pwd > out.txt", map[string]string{
		"GC_DIR": tmpDir,
	}, 5*time.Second); err != nil {
		t.Fatalf("RunSetupCommand: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "out.txt"))
	if err != nil {
		t.Fatalf("out.txt not created in GC_DIR: %v", err)
	}
	// t.TempDir can hand back a symlinked path (macOS /var -> /private/var);
	// pwd reports the resolved one, so compare resolved forms.
	wantDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", tmpDir, err)
	}
	gotDir, err := filepath.EvalSymlinks(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", strings.TrimSpace(string(data)), err)
	}
	if gotDir != wantDir {
		t.Fatalf("working directory = %q, want %q", gotDir, wantDir)
	}
}

// TestRunSetupCommandAppendsEnvOverlay pins that env entries reach the
// command on top of the inherited process environment.
func TestRunSetupCommandAppendsEnvOverlay(t *testing.T) {
	if err := RunSetupCommand(context.Background(), `[ "$GC_TEST_KEY" = v ]`, map[string]string{
		"GC_TEST_KEY": "v",
	}, 5*time.Second); err != nil {
		t.Fatalf("env overlay not visible to command: %v", err)
	}
}

// TestRunSetupCommandIncludesStreamDetailsOnFailure pins that a bounded tail
// of both streams is folded into the failure so operators see why a setup
// command failed without hunting for logs.
func TestRunSetupCommandIncludesStreamDetailsOnFailure(t *testing.T) {
	err := RunSetupCommand(context.Background(), "echo out; echo err >&2; exit 3", nil, 5*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("error = %q, want exit status", err)
	}
	if !strings.Contains(err.Error(), "stderr: err") {
		t.Fatalf("error = %q, want stderr detail", err)
	}
	if !strings.Contains(err.Error(), "stdout: out") {
		t.Fatalf("error = %q, want stdout detail", err)
	}
}

// TestRunSetupCommandTimeoutMatchesDeadlineExceeded pins that a command
// exceeding its per-command timeout reports an error callers can match with
// errors.Is(err, context.DeadlineExceeded).
func TestRunSetupCommandTimeoutMatchesDeadlineExceeded(t *testing.T) {
	err := RunSetupCommand(context.Background(), "sleep 5", nil, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %q, want errors.Is DeadlineExceeded", err)
	}
}

// TestRunSetupCommandBackgroundChildSucceedsBounded is the regression for
// setup commands that daemonize a child inheriting stdio: without
// Cmd.WaitDelay the capture pipes never reach EOF and Run blocks until the
// descendant exits, far past the timeout. The command itself exits 0, so it
// must be reported as success once setupCommandWaitDelay force-closes the
// pipes.
func TestRunSetupCommandBackgroundChildSucceedsBounded(t *testing.T) {
	start := time.Now()
	err := RunSetupCommand(context.Background(), "sleep 30 & exit 0", nil, 30*time.Second)
	elapsed := time.Since(start)
	if elapsed >= 10*time.Second {
		t.Fatalf("RunSetupCommand blocked %v on a background child holding stdio", elapsed)
	}
	if err != nil {
		t.Fatalf("daemonizing setup command exiting 0 should succeed, got %v", err)
	}
}
