//go:build !windows

package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execgrace"
)

// fakeIdleClock is an execgrace.Clock whose time moves only when a test calls
// advance, so an idle-budget test never depends on how fast the host runs.
//
// The Monitor reads the clock exactly when it observes output (and once when
// it starts), so every Now call made outside advance signals on stamped. That
// is how a test waits for "the command's last line has been seen" without
// sleeping or polling.
type fakeIdleClock struct {
	mu        sync.Mutex
	now       time.Time
	timers    []*fakeIdleTimer
	advancing bool
	stamped   chan struct{}
}

type fakeIdleTimer struct {
	clk    *fakeIdleClock
	due    time.Time
	fn     func()
	active bool
}

func newFakeIdleClock() *fakeIdleClock {
	return &fakeIdleClock{now: time.Unix(1_700_000_000, 0), stamped: make(chan struct{}, 64)}
}

func (c *fakeIdleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.advancing {
		select {
		case c.stamped <- struct{}{}:
		default:
		}
	}
	return c.now
}

func (c *fakeIdleClock) AfterFunc(d time.Duration, f func()) execgrace.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeIdleTimer{clk: c, due: c.now.Add(d), fn: f, active: true}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeIdleTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

func (t *fakeIdleTimer) Reset(d time.Duration) bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	was := t.active
	t.due = t.clk.now.Add(d)
	t.active = true
	return was
}

// advance moves the clock forward by d, firing each timer that comes due, in
// due order, on the calling goroutine with the clock set to that timer's
// deadline (a callback may Reset its own timer).
func (c *fakeIdleClock) advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.advancing = true
	for {
		var next *fakeIdleTimer
		for _, t := range c.timers {
			if t.active && !t.due.After(target) && (next == nil || t.due.Before(next.due)) {
				next = t
			}
		}
		if next == nil {
			break
		}
		c.now = next.due
		next.active = false
		c.mu.Unlock()
		next.fn()
		c.mu.Lock()
	}
	c.now = target
	c.advancing = false
	c.mu.Unlock()
}

// awaitStamp blocks until the Monitor next reads the clock outside advance.
// The 30s bound only turns a hang into a failure; no assertion depends on it.
func (c *fakeIdleClock) awaitStamp(t *testing.T) {
	t.Helper()
	select {
	case <-c.stamped:
	case <-time.After(30 * time.Second):
		t.Fatal("the Monitor never observed the command's output")
	}
}

// setupGate paces a setup command with a FIFO instead of sleep: the command
// blocks reading it until the test calls release.
type setupGate struct {
	path string
	w    *os.File
}

func newSetupGate(t *testing.T) *setupGate {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	// O_RDWR never blocks on open and keeps the FIFO alive between the
	// command's per-line opens, so a release before the command reaches its
	// read is not lost.
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open gate: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return &setupGate{path: path, w: w}
}

func (g *setupGate) release(t *testing.T) {
	t.Helper()
	if _, err := g.w.WriteString("\n"); err != nil {
		t.Fatalf("release gate: %v", err)
	}
}

// gatedProgressCommand prints a progress line, then waits for the test to
// release the gate, ten times.
const gatedProgressCommand = `i=1; while [ $i -le 10 ]; do echo "progress $i"; read -r _ < "$GC_TEST_GATE"; i=$((i+1)); done; exit 0`

// TestRunSetupCommandActivityStreamingSurvivesIdleWindow is the regression for
// slow-but-healthy setup commands killed mid-flight by the fixed wall-clock
// deadline (e.g. a large `git worktree add` checkout streaming progress past
// setup_timeout). With the activity budget enabled, output resets the idle
// clock, so a command that streams for several idle windows and exits 0 must
// succeed.
//
// The idle clock is injected and the command is paced by a gate, so the
// verdict never depends on host speed: each line is released only after the
// Monitor has seen the previous one, and time advances two thirds of an idle
// window per line (about 6.7 windows in all) only after that line was seen. A
// real clock and a sleeping command made this test fail whenever a loaded
// shard stalled the stream past the 300ms window.
func TestRunSetupCommandActivityStreamingSurvivesIdleWindow(t *testing.T) {
	const idle = 300 * time.Millisecond
	clk := newFakeIdleClock()
	ops := &tmuxStartOps{tm: &Tmux{}, setupMaxTimeout: 30 * time.Second, monitorClock: clk}
	gate := newSetupGate(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- ops.runSetupCommand(ctx, gatedProgressCommand, map[string]string{"GC_TEST_GATE": gate.path}, idle)
	}()

	clk.awaitStamp(t) // the Monitor's start stamp
	for i := 1; i <= 10; i++ {
		clk.awaitStamp(t) // line i observed
		clk.advance(2 * idle / 3)
		gate.release(t)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("streaming setup command killed despite visible progress: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("setup command never finished")
	}
}

// TestRunSetupCommandActivityIdleFiresOnInjectedClock is the control for the
// streaming test: on the same injected clock, a command that goes silent is
// canceled with the idle cause once a full window passes. Without it the
// streaming test would also pass if the injected clock never fired.
func TestRunSetupCommandActivityIdleFiresOnInjectedClock(t *testing.T) {
	const idle = 300 * time.Millisecond
	clk := newFakeIdleClock()
	ops := &tmuxStartOps{tm: &Tmux{}, setupMaxTimeout: 30 * time.Second, monitorClock: clk}
	gate := newSetupGate(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- ops.runSetupCommand(ctx, gatedProgressCommand, map[string]string{"GC_TEST_GATE": gate.path}, idle)
	}()

	clk.awaitStamp(t) // the Monitor's start stamp
	clk.awaitStamp(t) // "progress 1" observed, then the command goes silent
	clk.advance(idle)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("silent setup command must fail once the idle window passes")
		}
		if !strings.Contains(err.Error(), "no output within the idle timeout") {
			t.Fatalf("error should name the idle budget, got: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("silent setup command was never canceled")
	}
}
