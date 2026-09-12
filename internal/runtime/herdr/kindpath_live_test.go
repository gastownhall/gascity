package herdr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestKindPathNamesAreUnique guards the fix for ga-fh1flg: TestProviderLive-
// ClaudeKindPath used to claim the static herdr session "gctest-kind" and
// pane/agent name "kindsmoke", which concurrent fleet runs of this same test
// collided on (agent_pane_busy / herdr server never becoming ready). Each
// call must return names distinct from the last, salted with this process's
// PID so concurrent fleet processes can't collide either.
func TestKindPathNamesAreUnique(t *testing.T) {
	s1, a1 := kindPathNames()
	s2, a2 := kindPathNames()
	if s1 == s2 {
		t.Errorf("kindPathNames() session names not unique across calls: both %q", s1)
	}
	if a1 == a2 {
		t.Errorf("kindPathNames() agent names not unique across calls: both %q", a1)
	}
	pid := fmt.Sprintf("%d", os.Getpid())
	if !strings.Contains(s1, pid) || !strings.Contains(a1, pid) {
		t.Errorf("kindPathNames() = (%q, %q); want both to contain pid %s so concurrent fleet processes cannot collide", s1, a1, pid)
	}
}

// TestKindPathNamesWorkThroughFakeProviderLifecycle hermetically covers what
// TestProviderLiveClaudeKindPath cannot in a claude-less sandbox: that
// kindPathNames()'s PID-salted session/agent names actually thread correctly
// through the same New/Start/GetMeta/IsRunning/ObserveLiveness call sequence,
// using the fake-herdr harness from newFakeHerdrProvider
// (panebinding_provider_test.go) instead of a live herdr server and claude
// binary.
//
// ga-hmd2gu review round 1: TestKindPathNamesAreUnique proves the generated
// names are distinct, but never threads them through a real provider call
// sequence — exactly what a claude-absent skip leaves unverified everywhere
// this test runs without a live claude binary.
func TestKindPathNamesWorkThroughFakeProviderLifecycle(t *testing.T) {
	session, agent := kindPathNames()
	p, _, _ := newFakeHerdrProviderForSession(t, session)
	listenHerdrSocket(t, session)

	ctx := context.Background()
	cfg := runtime.Config{
		Command: "claude",
		Env:     map[string]string{"GC_SESSION_ID": session + "-session"},
	}
	if err := p.Start(ctx, agent, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// herdr registered the agent under the session name (kind path).
	if _, ok, err := p.c.getAgent(ctx, agent); err != nil || !ok {
		t.Fatalf("agent get %s = ok=%v, %v; want registered", agent, ok, err)
	}
	if mode, _ := p.GetMeta(agent, metaBoundMode); mode != bindModeAgent {
		t.Errorf("bound mode = %q; want %q", mode, bindModeAgent)
	}
	if pane, _ := p.GetMeta(agent, metaBoundPane); pane == "" {
		t.Error("bound pane empty after kind Start")
	}

	if !p.IsRunning(agent) {
		t.Error("IsRunning = false after kind Start")
	}
	if live := p.ObserveLiveness(agent, nil); !live.Running || !live.Alive {
		t.Errorf("ObserveLiveness = %+v; want Running=true Alive=true", live)
	}
	if err := p.Start(ctx, agent, cfg); !errors.Is(err, runtime.ErrSessionExists) {
		t.Errorf("re-issued Start = %v; want ErrSessionExists", err)
	}
}

var kindPathSessionSeq int64

// kindPathNames returns the herdr session name and pane/agent name that
// TestProviderLiveClaudeKindPath claims, salted with this process's PID and
// a per-process call counter so concurrent fleet runs of this test can never
// collide on the same herdr session/pane.
func kindPathNames() (session, agent string) {
	n := atomic.AddInt64(&kindPathSessionSeq, 1)
	pid := os.Getpid()
	return fmt.Sprintf("gctest-kind-%d-%d", pid, n), fmt.Sprintf("kindsmoke-%d-%d", pid, n)
}

// TestProviderLiveClaudeKindPath drives the herdr ≥0.7.5 kind-launch path
// against a real herdr AND a real claude binary: Start places a shell pane
// and has herdr launch + detect claude in it (native claude-detection), the
// agent is registered under the session name, liveness holds across checks
// (with a re-issued Start refusing), and Stop tears the pane down. Opt-in live
// tier: see requireLiveHerdr; also skipped when claude is unavailable.
func TestProviderLiveClaudeKindPath(t *testing.T) {
	requireLiveHerdr(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}

	session, agent := kindPathNames()
	p := New(session, t.TempDir(), t.TempDir(), 0, 0)
	_ = p.Stop(agent)
	t.Cleanup(func() { _ = p.Stop(agent); _ = p.TeardownServer() })

	ctx := context.Background()
	cfg := runtime.Config{
		WorkDir: t.TempDir(),
		Command: "claude",
		Env:     map[string]string{"GC_SESSION_ID": session + "-session"},
	}
	if err := p.Start(ctx, agent, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// herdr registered the agent under the session name (kind path).
	if _, ok, err := p.c.getAgent(ctx, agent); err != nil || !ok {
		t.Fatalf("agent get %s = ok=%v, %v; want registered", agent, ok, err)
	}
	if mode, _ := p.GetMeta(agent, metaBoundMode); mode != bindModeAgent {
		t.Errorf("bound mode = %q; want %q", mode, bindModeAgent)
	}
	if pane, _ := p.GetMeta(agent, metaBoundPane); pane == "" {
		t.Error("bound pane empty after kind Start")
	}

	if !p.IsRunning(agent) {
		t.Error("IsRunning = false after kind Start")
	}
	if live := p.ObserveLiveness(agent, nil); !live.Running || !live.Alive {
		t.Errorf("ObserveLiveness = %+v; want Running=true Alive=true", live)
	}
	if err := p.Start(ctx, agent, cfg); !errors.Is(err, runtime.ErrSessionExists) {
		t.Errorf("re-issued Start = %v; want ErrSessionExists", err)
	}

	if err := p.Stop(agent); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for i := 0; i < 15 && p.IsRunning(agent); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if p.IsRunning(agent) {
		t.Error("IsRunning = true after Stop")
	}
}
