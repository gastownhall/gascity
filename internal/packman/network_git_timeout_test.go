package packman

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// blackholeGitServer returns an http:// URL whose TCP listener accepts
// connections and then never writes a byte. git connects, sends its request,
// and waits forever — the same shape as a remote that is reachable but wedged,
// which is what ga-r0epd is really about. Nothing leaves loopback.
func blackholeGitServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open and answer nothing. Closing on
			// listener shutdown is what releases git if the bound never fires.
			go func() {
				<-done
				_ = conn.Close()
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return fmt.Sprintf("http://%s/blackhole.git", ln.Addr().String())
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
	remote := blackholeGitServer(t)

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
			t.Fatalf("cloning a blackholed remote succeeded after %s, want a timeout error", elapsed)
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
func TestDefaultRunNetworkGitRealFailuresAreNotTimeouts(t *testing.T) {
	// Bind and immediately release, so the port is almost certainly closed:
	// git fails fast with "connection refused" rather than hanging.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	remote := fmt.Sprintf("http://%s/refused.git", addr)

	restore := networkGitTimeout
	networkGitTimeout = 20 * time.Second
	t.Cleanup(func() { networkGitTimeout = restore })

	_, err = defaultRunNetworkGit("", remote, "", "clone", "--quiet", remote, t.TempDir()+"/dest")
	if err == nil {
		t.Fatal("cloning a closed port succeeded, want a connection error")
	}
	if errors.Is(err, errNetworkGitTimeout) {
		t.Fatalf("err = %v, want a real git failure rather than a timeout classification", err)
	}
}
