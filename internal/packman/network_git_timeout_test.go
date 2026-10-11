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

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// wedgedRemote is what wedgedGit hands back: the remote URL the shim ignores,
// plus the two artifacts its backgrounded child produces so a test can observe
// the child's lifecycle without polling it.
type wedgedRemote struct {
	// URL is a syntactically valid remote the shim never actually contacts.
	URL string
	// PIDPath receives the backgrounded child's pid, for cleanup.
	PIDPath string
	// HeartbeatPath grows for as long as that child is alive. Its size is the
	// direct measurement of the thing the bound has to stop: not "did the call
	// return" but "did the work stop".
	HeartbeatPath string
}

// wedgedGit puts a `git` on PATH that hangs the way a wedged remote does.
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
// The child appends to a heartbeat file rather than merely sleeping, so its
// liveness is observable as a file that stops growing. That is what lets these
// tests assert on the descendants with no polling loop of their own.
//
// exec.Command resolves the binary against the parent process PATH rather than
// cmd.Env, so t.Setenv is enough to intercept even though defaultRunNetworkGit
// hands the child a hermetic environment.
func wedgedGit(t *testing.T) wedgedRemote {
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
	w := wedgedRemote{
		URL:           "http://packman.invalid/wedged.git",
		PIDPath:       filepath.Join(dir, "child.pid"),
		HeartbeatPath: filepath.Join(dir, "child.heartbeat"),
	}
	// The first heartbeat is written by the foreground shim, before the loop is
	// backgrounded. Left to the child, it races the deadline: the tests shrink
	// networkGitTimeout to a few hundred milliseconds, and on a loaded host a
	// fork/exec can lose that race, so *correct* code would kill the group
	// before any byte landed and the test would fail waiting for a file that is
	// never coming. Writing it here removes the race without weakening the
	// assertion — a shim whose child never starts exits immediately, which
	// makes the clone succeed, which every caller already fails on.
	//
	// The 50ms cadence is the convention the other users of these helpers use
	// (internal/orders, cmd/gc): it has to be well under the caller's stability
	// window, or a live child idling between writes reads as a dead one.
	script := "#!/bin/sh\n" +
		"echo . >> " + w.HeartbeatPath + "\n" +
		"{ while : ; do echo . >> " + w.HeartbeatPath + " ; " + sleep + " 0.05 ; done ; } &\n" +
		"echo $! > " + w.PIDPath + "\n" +
		"wait\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing git shim: %v", err)
	}
	t.Setenv("PATH", dir)
	// A test that fails before the bound kills the group must not leave the
	// child running for the rest of the package.
	t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, w.PIDPath) })
	return w
}

// TestDefaultRunNetworkGitErrorsCarryNoCredential pins that a credential
// embedded in a source URL never reaches the error string.
//
// There are two independent routes for it to leak and both are live: git's argv
// carries the remote verbatim, and git itself echoes the remote back in
// transport failures. The callers' own wraps redact the URL they interpolate,
// which is what makes this easy to miss — the outer label reads
// "cloning ***@host/repo" while the inner string still holds the raw token.
//
// The shim stands in for git failing the way it does against a bad remote:
// echoing the URL it was handed and exiting 128. That exercises the output
// route, and passing the credentialed URL as an argument exercises the argv one.
func TestDefaultRunNetworkGitErrorsCarryNoCredential(t *testing.T) {
	const token = "s3cr3t-token-value"
	remote := "https://gituser:" + token + "@packman.invalid/repo.git"

	dir := t.TempDir()
	// Echo the remote back the way git does, then fail as git does.
	script := "#!/bin/sh\necho \"fatal: could not read from remote repository $*\" >&2\nexit 128\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing git shim: %v", err)
	}
	t.Setenv("PATH", dir)

	_, err := defaultRunNetworkGit("", remote, "", "clone", "--quiet", remote, filepath.Join(t.TempDir(), "dest"))
	if err == nil {
		t.Fatal("cloning from the failing shim succeeded, want an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaks the credential: %v", err)
	}
	// The error still has to be usable: masking the token must not cost the
	// operator the host and repo they need to act on.
	if !strings.Contains(err.Error(), "packman.invalid/repo.git") {
		t.Fatalf("err = %v, want the redacted remote to remain identifiable", err)
	}
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
	remote := wedgedGit(t).URL

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
	// this one would go green having executed no git. Exit 128 is what closes
	// that hole: it proves a real git ran and exited on its own diagnosis. A
	// missing git yields *exec.Error rather than *exec.ExitError, and a killed
	// one exits -1, so neither can reach here.
	//
	// The exit code rather than git's message text, because that text is in
	// git's translation catalogs and HermeticEnv passes LANG/LC_ALL through
	// (it strips only git's own variables) — so asserting the English wording
	// would fail loudly on a localized image for no good reason.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128 {
		t.Fatalf("err = %v, want git's own failure as an *exec.ExitError with code 128", err)
	}
}
