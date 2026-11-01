//go:build integration

package tmux

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

var tmuxContractSession int64

// tmuxContractProvider builds a provider with the conformance configuration
// on socket.
func tmuxContractProvider(socket string) runtime.Provider {
	cfg := tmuxConformanceConfig()
	cfg.SocketName = socket
	return NewSeamBackedWithConfig(cfg)
}

// startTmuxContractSession starts a session running command on the
// conformance socket, stopped at cleanup, and returns its provider and name.
func startTmuxContractSession(t *testing.T, command string) (runtime.Provider, string) {
	t.Helper()
	p := tmuxContractProvider(testSocketName)
	name := fmt.Sprintf("gc-test-contract-%d", atomic.AddInt64(&tmuxContractSession, 1))
	if err := p.Start(context.Background(), name, runtime.Config{Command: command, WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Start(%s): %v", name, err)
	}
	t.Cleanup(func() {
		if err := p.Stop(name); err != nil {
			t.Errorf("Stop(%s): %v", name, err)
		}
	})
	return p, name
}

// TestTmuxStopContract runs the RUNTIME-CONTRACT-003 cases against real tmux:
// the missing-session case with a running server (a sentinel session holds it
// up), and the dead-server case on a private socket no server ever bound.
// tmux's unreachable case is its missing-server answer, which
// runtime.StopForCleanup absorbs by design once the server is confirmed dead,
// so it has no Unreachable fixture here.
func TestTmuxStopContract(t *testing.T) {
	runtimetest.RunStopContractTests(t, runtimetest.StopFixtures{
		Responsive: func(t *testing.T) runtime.Provider {
			p, _ := startTmuxContractSession(t, "sleep 300")
			return p
		},
		DeadServer: func(*testing.T) runtime.Provider {
			return tmuxContractProvider(privateSocketName("dead"))
		},
	})
}

// TestTmuxIdleWaitContract runs the RUNTIME-CONTRACT-016 cases against real
// tmux panes: one holding a busy indicator and no ready prompt, and a blank
// one that shows neither (an indeterminate reading).
func TestTmuxIdleWaitContract(t *testing.T) {
	runtimetest.RunIdleWaitContractTests(t, runtimetest.IdleWaitFixtures{
		Busy: func(t *testing.T) (runtime.Provider, string) {
			return startTmuxContractSession(t, "echo 'working (esc to interrupt)'; exec sleep 300")
		},
		Unobserved: func(t *testing.T) (runtime.Provider, string) {
			return startTmuxContractSession(t, "sleep 300")
		},
	})
}
