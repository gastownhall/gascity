//go:build integration

package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	"github.com/gastownhall/gascity/internal/testutil"
)

// fakeSSHScript stands in for the ssh(1) client. sshArgs always shell-quotes
// the remote command into the final argv element regardless of how many
// "-o"/"-i"/"-p" flags precede it, so grabbing the last positional argument
// and handing it to a shell reproduces exactly what a real ssh client does on
// the far end: hand the quoted command string to a shell for interpretation.
// It never parses its own flags, so it touches no real known_hosts file and
// dials no network.
const fakeSSHScript = "#!/bin/sh\nfor last; do :; done\nexec sh -c \"$last\"\n"

// sshFixture caches the hermetic environment sshConformanceEndpoint builds
// for one subtest, so repeat factory calls within that subtest converge on
// the same fake-ssh-on-PATH directory and the same isolated tmux server.
type sshFixture struct {
	once     sync.Once
	endpoint Endpoint
}

var (
	sshConformanceFixturesMu sync.Mutex
	sshConformanceFixtures   = map[*testing.T]*sshFixture{}
)

// sshConformanceEndpoint builds the hermetic production-boundary fixture for
// TestSSHConformance: a fake "ssh" executable placed first on PATH so
// shellRunner's real exec.CommandContext(ctx, "ssh", ...) resolves to it
// instead of a real client, plus an isolated tmux server so the fixture's
// tmux traffic (issued by Provider.tmux over the fake connection) never
// reaches any tmux server this process happens to already be running under.
//
// Several runtimetest subtests call the provider factory more than once --
// starting a session, then calling the factory again for a second session --
// before either session is torn down. Rebuilding the fixture on every call
// would mint a fresh TMUX_TMPDIR each time, repointing tmux's default-socket
// resolution out from under the first, already-running session and orphaning
// it on an abandoned server that no later ListRunning call would ever see
// again. Caching the fixture per subtest *testing.T (via sync.Once, so
// concurrent goroutines within one subtest -- see Start_ConcurrentDistinctSessions
// -- don't race on setup) keeps it stable for that subtest's whole lifetime.
// The cache is deliberately scoped to one subtest, not the package or binary:
// t.Setenv's automatic revert on subtest completion keeps this fixture from
// leaking into ssh_test.go's own real-infra tests, which must still see a
// real (or absent) ssh client. tmux resolves its server socket from $TMUX
// (the enclosing session, when set) before it ever consults $TMUX_TMPDIR, so
// isolation requires clearing $TMUX explicitly -- t.Setenv can only set a
// value, never remove a key. In keeping with internal/runtime/tmux's own
// conformance precedent (adapter_test.go, isolated via a fixed socket name
// with no explicit teardown), the per-subtest tmux server started here is
// left running rather than explicitly killed; it is bound to a directory
// this call alone created and is reaped with normal temp-dir cleanup.
func sshConformanceEndpoint(t *testing.T) Endpoint {
	t.Helper()

	sshConformanceFixturesMu.Lock()
	f, ok := sshConformanceFixtures[t]
	if !ok {
		f = &sshFixture{}
		sshConformanceFixtures[t] = f
		t.Cleanup(func() {
			sshConformanceFixturesMu.Lock()
			delete(sshConformanceFixtures, t)
			sshConformanceFixturesMu.Unlock()
		})
	}
	sshConformanceFixturesMu.Unlock()

	f.once.Do(func() { f.endpoint = buildSSHConformanceFixture(t) })
	return f.endpoint
}

func buildSSHConformanceFixture(t *testing.T) Endpoint {
	t.Helper()

	fixtureDir := t.TempDir()
	scriptPath := filepath.Join(fixtureDir, "ssh")
	if err := os.WriteFile(scriptPath, []byte(fakeSSHScript), 0o755); err != nil {
		t.Fatalf("writing fake ssh fixture: %v", err)
	}
	t.Setenv("PATH", fixtureDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// tmux binds its server socket at $TMUX_TMPDIR/tmux-<uid>/default, and a
	// Unix socket path is capped at 104 bytes on macOS. There t.TempDir()
	// nests the subtest name under the long per-user /var/folders temp root,
	// so the socket path overran the cap, the server never started, and every
	// subtest failed with "tmux new-session exited 1". ShortTempDir roots the
	// directory at /tmp on macOS.
	t.Setenv("TMUX_TMPDIR", testutil.ShortTempDir(t, "gcssh"))

	prevTMUX, hadTMUX := os.LookupEnv("TMUX")
	if err := os.Unsetenv("TMUX"); err != nil {
		t.Fatalf("unsetting TMUX: %v", err)
	}
	t.Cleanup(func() {
		if hadTMUX {
			os.Setenv("TMUX", prevTMUX)
		}
	})

	return Endpoint{User: "conformance", Host: "fixture"}
}

// TestSSHConformance proves internal/runtime/ssh.NewSeamBacked — the
// production SSH constructor — against the full runtime.Provider contract.
func TestSSHConformance(t *testing.T) {
	var counter int64
	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBacked(sshConformanceEndpoint(t)), runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, fmt.Sprintf("gc-test-conform-%d", atomic.AddInt64(&counter, 1))
	})
}

// A bare -t name is a unique-prefix match in tmux: with worker-1 gone and
// worker-10 live on the same box, "-t worker-1" names worker-10. Through the
// production connection (fake ssh, real tmux), every verb on the absent
// worker-1 must miss worker-10 rather than observe, read, type into, respawn
// or kill it (mc-k609w).
func TestSSHProviderTargetsSessionsExactlyOnRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	p := NewProvider(sshConformanceEndpoint(t))
	p.postStartSettle = 0
	ctx := context.Background()
	if err := p.Start(ctx, "worker-10", runtime.Config{Command: "cat", WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Start worker-10: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop("worker-10") })
	if err := p.SetMeta("worker-10", "GC_SESSION_ID", "ten"); err != nil {
		t.Fatalf("SetMeta worker-10: %v", err)
	}
	pidBefore, _, err := p.tmux(ctx, "worker-10", "display-message", "-p", "-t", "=worker-10:", "#{pane_pid}")
	if err != nil || pidBefore == "" {
		t.Fatalf("worker-10 pane pid = (%q, %v)", pidBefore, err)
	}

	if p.IsRunning("worker-1") {
		t.Error("IsRunning(worker-1) = true: has-session prefix-matched worker-10")
	}
	if got, _ := p.GetMeta("worker-1", "GC_SESSION_ID"); got != "" {
		t.Errorf("GetMeta(worker-1) = %q: show-environment read worker-10's environment", got)
	}
	_ = p.SendKeys("worker-1", "Enter")
	_ = p.Nudge("worker-1", runtime.TextContent("STRAY-NUDGE"))
	if err := p.Relaunch(ctx, "worker-1", runtime.Config{Command: "sleep 300"}); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Errorf("Relaunch(worker-1) = %v, want ErrSessionNotFound", err)
	}
	if err := p.Stop("worker-1"); err != nil {
		t.Errorf("Stop(worker-1) = %v, want idempotent nil", err)
	}

	if !p.IsRunning("worker-10") {
		t.Fatal("worker-10 is gone: kill-session -t worker-1 prefix-matched it")
	}
	if pid, _, _ := p.tmux(ctx, "worker-10", "display-message", "-p", "-t", "=worker-10:", "#{pane_pid}"); pid != pidBefore {
		t.Errorf("worker-10 pane pid %q -> %q: respawn-pane -t worker-1 replaced it", pidBefore, pid)
	}
	if peek, _ := p.Peek("worker-10", 20); strings.Contains(peek, "STRAY-NUDGE") {
		t.Errorf("worker-10 received worker-1's nudge: %q", peek)
	}
}
