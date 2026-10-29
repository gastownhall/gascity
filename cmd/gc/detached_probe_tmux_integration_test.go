//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// startDetachedProbeTmuxServer starts a real tmux server on the -L socket
// name in this process's environment, holding session, and kills that server
// on cleanup in the same environment.
func startDetachedProbeTmuxServer(t *testing.T, socket, session string) {
	t.Helper()
	if out, err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", session, "sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("tmux -L %s new-session: %v: %s", socket, err, out)
	}
	env := os.Environ()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "tmux", "-L", socket, "kill-server")
		cmd.Env = env
		_ = cmd.Run()
	})
}

// AC-2: against a real tmux server, a session whose name is only a prefix of
// a hosted session is dead and the hosted session is alive, with TMPDIR set
// apart from TMUX_TMPDIR.
func TestProbeDetachedWork_RealTmuxMatchesSessionExactly(t *testing.T) {
	tmuxtest.RequireTmux(t)
	root := shortTmuxTmpDir(t)
	t.Setenv("TMUX_TMPDIR", root)
	socket := "gcdp-" + filepath.Base(root)
	startDetachedProbeTmuxServer(t, socket, "soak-loop-2")
	t.Setenv("TMPDIR", t.TempDir())

	if got := probeDetachedWork(context.Background(), "tmux:"+socket+":soak-loop"); got.Status != detachedProbeDead {
		t.Fatalf("prefix-only session: Status = %q, want %q (err=%v)", got.Status, detachedProbeDead, got.Err)
	}
	if got := probeDetachedWork(context.Background(), "tmux:"+socket+":soak-loop-2"); got.Status != detachedProbeAlive {
		t.Fatalf("hosted session: Status = %q, want %q (err=%v)", got.Status, detachedProbeAlive, got.Err)
	}
}
