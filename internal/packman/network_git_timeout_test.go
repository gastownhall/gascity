package packman

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// wedgedGit puts a `git` on PATH that hangs the way a wedged remote does, and
// returns a remote URL it will ignore.
//
// A loopback listener that accepts and never answers is the more literal
// reproduction, but it is not the more faithful one. What has to be exercised
// is the shape that makes the bound hard: git spawns helpers (git-remote-http,
// credential helpers) that inherit the output pipes, so killing git alone
// leaves CombinedOutput blocked on a pipe a child still holds. This shim
// reproduces that on purpose — it backgrounds a child that inherits stdout and
// stderr, then waits — instead of hoping a real git happens to do it. It also
// needs no port, no listener and no network, so it cannot flake on a busy host.
//
// exec.Command resolves the binary against the parent process PATH rather than
// cmd.Env, so t.Setenv is enough to intercept even though defaultRunNetworkGit
// hands the child a hermetic environment.
func wedgedGit(t *testing.T) (remote, childPIDPath string) {
	t.Helper()
	dir := t.TempDir()
	// The shim runs with the hermetic environment defaultRunNetworkGit builds,
	// whose PATH is not this machine's, so sleep is resolved here and embedded
	// absolute. Left as a bare name it silently fails to start and the shim
	// exits instantly — which looks exactly like a bound that fired early.
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		// Skipping here would delete exactly the coverage this file exists for,
		// silently. sleep is POSIX-required, so on the platforms we test it is
		// a broken image, not an unsupported one.
		if runtime.GOOS != "windows" {
			t.Fatalf("no sleep binary to build a wedged git shim: %v", err)
		}
		t.Skipf("no sleep binary on %s: %v", runtime.GOOS, err)
	}
	childPIDPath = filepath.Join(dir, "child.pid")
	script := "#!/bin/sh\n" + sleep + " 300 &\necho $! > " + childPIDPath + "\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing git shim: %v", err)
	}
	t.Setenv("PATH", dir)
	return "http://packman.invalid/wedged.git", childPIDPath
}

// TestDefaultRunNetworkGitIsBounded is the ga-r0epd regression test.
//
// defaultRunNetworkGit runs inside WithRepoCacheWriteLock (EnsureRepoInCache),
// so an unbounded network git does not merely hang its own caller — it holds
// the machine-wide repo-cache lock for as long as the remote stays wedged, and
// every other gc process on the host that touches pack state blocks behind it.
// That is why `gc help` could hang forever and why `make test` and the pre-push
// hook died on a Go test timeout instead of failing.
//
// The assertion is deliberately about the deadline, not the error text: what
// matters is that the call RETURNS.
func TestDefaultRunNetworkGitIsBounded(t *testing.T) {
	remote, _ := wedgedGit(t)

	restore := networkGitTimeout
	networkGitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { networkGitTimeout = restore })
	restoreWait := networkGitWaitDelay
	networkGitWaitDelay = time.Second
	t.Cleanup(func() { networkGitWaitDelay = restoreWait })

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		out, err := defaultRunNetworkGit("", remote, "", "clone", "--quiet", remote, t.TempDir()+"/dest")
		done <- result{out: out, err: err}
	}()

	select {
	case got := <-done:
		elapsed := time.Since(start)
		if got.err == nil {
			t.Fatalf("cloning a wedged remote succeeded after %s, want a timeout error", elapsed)
		}
		if !errors.Is(got.err, errNetworkGitTimeout) {
			t.Fatalf("err = %v, want it to wrap errNetworkGitTimeout (elapsed %s)", got.err, elapsed)
		}
		// The message has to name the bound. An operator seeing this in a log
		// needs to know a deadline fired rather than that the remote refused.
		if !strings.Contains(got.err.Error(), networkGitTimeout.String()) {
			t.Fatalf("err = %v, want the message to name the %s bound", got.err, networkGitTimeout)
		}
		// Returning eventually is not the same as respecting the deadline. The
		// real bound is networkGitTimeout + networkGitWaitDelay (1.3s here);
		// the slack is wide because this asserts "tracks the knobs", not
		// scheduler precision.
		if budget := 8 * time.Second; elapsed > budget {
			t.Fatalf("returned after %s with a %s deadline and %s wait delay; the bound is not tracking its knobs", elapsed, networkGitTimeout, networkGitWaitDelay)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("defaultRunNetworkGit did not return: the network git call is unbounded (ga-r0epd)")
	}
}

// TestDefaultRunNetworkGitRealFailuresAreNotTimeouts is the control for the
// test above. A bound that reported every failure as a timeout would pass the
// deadline assertion while destroying the diagnosis of ordinary failures —
// connection refused, no such repo, auth rejected. This pins that the new error
// path is reached only when the deadline actually fires.
//
// The remote is a file:// URL for a path that does not exist, so real git fails
// immediately with "does not appear to be a git repository" — a real failure
// with no shim, no listener and no network, which is what keeps this control
// honest.
func TestDefaultRunNetworkGitRealFailuresAreNotTimeouts(t *testing.T) {
	remote := "file://" + filepath.Join(t.TempDir(), "nonexistent.git")

	restore := networkGitTimeout
	networkGitTimeout = 20 * time.Second
	t.Cleanup(func() { networkGitTimeout = restore })

	_, err := defaultRunNetworkGit("", remote, "", "clone", "--quiet", remote, t.TempDir()+"/dest")
	if err == nil {
		t.Fatal("cloning a nonexistent repository succeeded, want a git failure")
	}
	if errors.Is(err, errNetworkGitTimeout) {
		t.Fatalf("err = %v, want a real git failure rather than a timeout classification", err)
	}
	// Asserting only "some error" would let this pass vacuously on an image
	// with no git at all — where the bounded test supplies its own shim and
	// this one would go green having executed no git at all. Naming git's own
	// diagnosis is what keeps the control honest.
	if !strings.Contains(err.Error(), "does not appear to be a git repository") {
		t.Fatalf("err = %v, want git's own no-such-repository diagnosis", err)
	}
}
