//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
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

// installTmuxArgsRecorder puts a tmux on PATH that appends each invocation's
// arguments, one per line and ended by "--", to the returned file and then
// runs the real tmux.
func installTmuxArgsRecorder(t *testing.T) string {
	t.Helper()
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("look up tmux: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "tmux-args")
	installFakeTmux(t, `printf '%s\n' "$@" -- >> '`+logPath+`'
exec '`+realTmux+`' "$@"`)
	return logPath
}

// recordedTmuxArgs returns each recorded tmux invocation's arguments.
func recordedTmuxArgs(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read tmux args: %v", err)
	}
	var calls [][]string
	var call []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "--" {
			calls = append(calls, call)
			call = nil
			continue
		}
		call = append(call, line)
	}
	return calls
}

// assertTmuxCallsUseSocketPath fails unless every recorded tmux call names
// its server with -S want and none with -L.
func assertTmuxCallsUseSocketPath(t *testing.T, calls [][]string, want string) {
	t.Helper()
	for _, call := range calls {
		sawS := false
		for i, arg := range call {
			switch arg {
			case "-L":
				t.Fatalf("tmux call %q names its server with -L", call)
			case "-S":
				if i+1 >= len(call) || call[i+1] != want {
					t.Fatalf("tmux call %q: -S is not %q", call, want)
				}
				sawS = true
			}
		}
		if !sawS {
			t.Fatalf("tmux call %q has no -S %s", call, want)
		}
	}
}

// AC-3: a probe whose TMUX_TMPDIR differs from the one its server was started
// under observes nothing at its own canonical socket path, so the session S
// that server holds is error, never dead; the path it probes is
// tmux.SocketPath computed in the probe's environment, which a server started
// there answers at. Such a server answers for the probe: without S it attests
// S dead although S still runs under the other TMUX_TMPDIR, which is why a
// producer must start detached work in the controller's TMUX_TMPDIR, and
// with S it attests S alive.
func TestProbeDetachedWork_RealTmuxOtherTMUXTMPDIRIsError(t *testing.T) {
	tmuxtest.RequireTmux(t)
	serverDir, probeDir := shortTmuxTmpDir(t), shortTmuxTmpDir(t)
	socket := "gcdp-" + filepath.Base(serverDir)
	t.Setenv("TMUX_TMPDIR", serverDir)
	startDetachedProbeTmuxServer(t, socket, "S")
	t.Setenv("TMUX_TMPDIR", probeDir)
	want := sessiontmux.SocketPath(socket)
	logPath := installTmuxArgsRecorder(t)

	got := probeDetachedWork(context.Background(), "tmux:"+socket+":S")
	if got.Status != detachedProbeError {
		t.Fatalf("Status = %q, want %q (err=%v)", got.Status, detachedProbeError, got.Err)
	}
	if got.Err == nil || !strings.Contains(got.Err.Error(), want) {
		t.Fatalf("Err = %v, want it to name the probed socket %s", got.Err, want)
	}
	assertTmuxCallsUseSocketPath(t, recordedTmuxArgs(t, logPath), want)

	startDetachedProbeTmuxServer(t, socket, "other")
	if got := probeDetachedWork(context.Background(), "tmux:"+socket+":S"); got.Status != detachedProbeDead {
		t.Fatalf("server without S under the probe's TMUX_TMPDIR: Status = %q, want %q (err=%v)", got.Status, detachedProbeDead, got.Err)
	}
	startDetachedProbeTmuxServer(t, socket, "S")
	if err := os.Remove(logPath); err != nil {
		t.Fatalf("reset tmux args: %v", err)
	}
	if got := probeDetachedWork(context.Background(), "tmux:"+socket+":S"); got.Status != detachedProbeAlive {
		t.Fatalf("server under the probe's TMUX_TMPDIR: Status = %q, want %q (err=%v)", got.Status, detachedProbeAlive, got.Err)
	}
	calls := recordedTmuxArgs(t, logPath)
	if len(calls) == 0 {
		t.Fatal("the probe ran no tmux client against the live server")
	}
	assertTmuxCallsUseSocketPath(t, calls, want)
}
