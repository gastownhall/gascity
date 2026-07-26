package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Stop and the final Start commit must serialize on p.mu. This test gates the
// session/new response, then wraps the sentinel cancellation. If Stop invokes
// cancellation after releasing p.mu, the wrapper deliberately lets Start
// commit first and reproduces the lost-stop bug deterministically.
func TestStopAtStartupCommitCannotBeLost(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "release-session-new")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatalf("create session/new gate: %v", err)
	}
	releaseGate, err := os.OpenFile(gate, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open session/new gate: %v", err)
	}
	t.Cleanup(func() { _ = releaseGate.Close() })
	sentinelReady := filepath.Join(dir, "session-new-waiting")
	watcher := newPathCreationWatcher(t, dir)
	command := fmt.Sprintf(`exec python3 -u -c '
import json, os, sys
gate = %q
sentinel_ready = %q
for line in sys.stdin:
    msg = json.loads(line)
    method = msg.get("method", "")
    if method == "initialize":
        print(json.dumps({"jsonrpc":"2.0","id":msg["id"],"result":{}}), flush=True)
    elif method == "session/new":
        release = open(gate, "r")
        open(sentinel_ready, "w").close()
        release.read(1)
        print(json.dumps({"jsonrpc":"2.0","id":msg["id"],"result":{"sessionId":"handoff"}}), flush=True)
'`, gate, sentinelReady)

	p := newPreStartTestProvider(t, Config{HandshakeTimeout: 5 * time.Second})
	name := testName()
	t.Cleanup(func() { _ = p.Stop(name) })
	var startResult error
	startDone := make(chan struct{})
	go func() {
		startResult = p.Start(context.Background(), name, runtime.Config{
			Command: command,
			WorkDir: dir,
		})
		close(startDone)
	}()

	waitForPathCreation(t, watcher, sentinelReady)
	p.mu.Lock()
	sentinel := p.conns[name]
	if sentinel == nil || sentinel.cmd != nil {
		p.mu.Unlock()
		t.Fatalf("session/new gate reached with connection = %#v, want startup sentinel", sentinel)
	}
	originalCancel := sentinel.cancel
	sentinel.cancel = func() {
		// A successful TryLock means Stop released p.mu before delivering
		// cancellation. Force the vulnerable interleaving by letting the
		// handshake finish and waiting for Start to replace the sentinel.
		unlocked := p.mu.TryLock()
		if unlocked {
			p.mu.Unlock()
			if _, err := releaseGate.Write([]byte("go")); err != nil {
				t.Errorf("release handshake: %v", err)
			}
			select {
			case <-startDone:
				if startResult != nil {
					t.Errorf("unlocked Start before sentinel cancellation = %v, want committed session", startResult)
				}
			case <-time.After(5 * time.Second):
				t.Error("unlocked sentinel cancellation did not reach the forced startup commit")
			}
		}
		originalCancel()
		// In the fixed path cancellation wins while Stop holds p.mu. Release
		// the child as well so process teardown never depends on the gate.
		if !unlocked {
			if _, err := releaseGate.Write([]byte("go")); err != nil {
				t.Errorf("release canceled handshake: %v", err)
			}
		}
	}
	p.mu.Unlock()

	stopErr := make(chan error, 1)
	go func() { stopErr <- p.Stop(name) }()

	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	select {
	case <-startDone:
		if startResult == nil || !errors.Is(startResult, context.Canceled) {
			if p.IsRunning(name) {
				_ = p.Stop(name)
			}
			t.Fatalf("Start after concurrent Stop = %v, want context.Canceled", startResult)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not unwind")
	}
	if p.IsRunning(name) {
		_ = p.Stop(name)
		t.Fatal("Stop returned nil but the session survived startup")
	}
}
