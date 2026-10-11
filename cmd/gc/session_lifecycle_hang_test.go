package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// hangingProvider's Stop and Interrupt block until released, simulating a
// wedged tmux subprocess or unresponsive runtime.
type hangingProvider struct {
	*runtime.Fake
	mu       sync.Mutex
	released bool
	releaseC chan struct{}
	attempts map[string]map[string]int
}

func newHangingProvider() *hangingProvider {
	return &hangingProvider{
		Fake:     runtime.NewFake(),
		releaseC: make(chan struct{}),
		attempts: make(map[string]map[string]int),
	}
}

func (p *hangingProvider) Stop(name string) error {
	p.recordAttempt("Stop", name)
	<-p.releaseC
	return p.Fake.Stop(name)
}

func (p *hangingProvider) Interrupt(name string) error {
	p.recordAttempt("Interrupt", name)
	<-p.releaseC
	return p.Fake.Interrupt(name)
}

func (p *hangingProvider) recordAttempt(method, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempts[method] == nil {
		p.attempts[method] = make(map[string]int)
	}
	p.attempts[method][name]++
}

func (p *hangingProvider) attemptCount(method, name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts[method][name]
}

func (p *hangingProvider) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.released {
		p.released = true
		close(p.releaseC)
	}
}

// TestExecuteTargetWave_BoundedByPerTargetTimeout verifies that
// executeTargetWave returns within roughly perTargetTimeout when one target's
// run() blocks; the blocked target's result records outcome="timed_out" while
// the other target still completes successfully.
func TestExecuteTargetWave_BoundedByPerTargetTimeout(t *testing.T) {
	block := make(chan struct{})
	defer close(block)

	targets := []stopTarget{
		{name: "blocked", template: "worker", resolved: true},
		{name: "fast", template: "worker", resolved: true},
	}

	done := make(chan []stopResult, 1)
	go func() {
		done <- executeTargetWave(targets, 2, 100*time.Millisecond, func(target stopTarget) error {
			if target.name == "blocked" {
				<-block
			}
			return nil
		})
	}()

	select {
	case results := <-done:
		if len(results) != 2 {
			t.Fatalf("len(results) = %d, want 2", len(results))
		}
		var blocked, fast stopResult
		for _, r := range results {
			switch r.target.name {
			case "blocked":
				blocked = r
			case "fast":
				fast = r
			}
		}
		if blocked.outcome != "timed_out" {
			t.Fatalf("blocked.outcome = %q, want timed_out", blocked.outcome)
		}
		if blocked.err == nil {
			t.Fatalf("blocked.err = nil, want non-nil timeout error")
		}
		if fast.outcome != "success" {
			t.Fatalf("fast.outcome = %q, want success", fast.outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeTargetWave did not return within 2s — perTargetTimeout regression")
	}
}

// TestGracefulStopAll_HangingProviderDoesNotWedge verifies that gracefulStopAll
// returns within a bounded time when the provider's Stop and Interrupt block
// forever. Without per-target timeouts the goroutines that run them never
// signal completion and the wave drainer hangs indefinitely.
func TestGracefulStopAll_HangingProviderDoesNotWedge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newHangingProvider()
		defer sp.release()

		names := []string{"alpha", "bravo", "charlie"}
		for _, name := range names {
			if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
		}
		cfg := &config.City{
			Daemon: config.DaemonConfig{ShutdownTimeout: "50ms"},
		}

		var stdout, stderr bytes.Buffer
		gracefulStopAll(
			names,
			sp,
			cfg.Daemon.ShutdownTimeoutDuration(),
			events.Discard,
			cfg,
			beads.SessionStore{},
			&stdout,
			&stderr,
		)

		for _, operation := range []struct {
			method string
			logOp  string
		}{
			{method: "Interrupt", logOp: "interrupt"},
			{method: "Stop", logOp: "stop"},
		} {
			for _, name := range names {
				if got := sp.attemptCount(operation.method, name); got != 1 {
					t.Errorf("%s attempts for %q = %d, want 1", operation.method, name, got)
				}
				matched := 0
				for _, line := range strings.Split(stderr.String(), "\n") {
					if strings.Contains(line, "session lifecycle:") &&
						strings.Contains(line, "op="+operation.logOp+" ") &&
						strings.Contains(line, "session="+name+" ") &&
						strings.Contains(line, "outcome=timed_out") {
						matched++
					}
				}
				if matched != 1 {
					t.Errorf("%s timed_out outcomes for %q = %d, want 1; stderr:\n%s", operation.logOp, name, matched, stderr.String())
				}
			}
		}
	})
}

// TestInterruptTargetsBounded_PoolManagedStopDoesNotWedge verifies that
// pool-managed sessions are stopped through the same bounded worker boundary as
// normal stop targets. Pool-managed sessions bypass the interrupt prompt, so an
// inline stop here would wedge the whole interrupt pass.
func TestInterruptTargetsBounded_PoolManagedStopDoesNotWedge(t *testing.T) {
	origStop := stopPerTargetTimeoutDefault
	stopPerTargetTimeoutDefault = 100 * time.Millisecond
	t.Cleanup(func() { stopPerTargetTimeoutDefault = origStop })

	sp := newHangingProvider()
	t.Cleanup(sp.release)
	if err := sp.Start(context.Background(), "pool-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "pool-worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "pool-worker",
			"template":             "pool",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "active",
		},
	}); err != nil {
		t.Fatal(err)
	}

	targets := []stopTarget{{name: "pool-worker", template: "pool", resolved: true, poolManaged: true}}
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- interruptTargetsBounded(targets, nil, store, sp, &stderr)
	}()

	select {
	case sent := <-done:
		if sent != 0 {
			t.Fatalf("sent = %d, want 0 for pool-managed stop-only target", sent)
		}
		if !strings.Contains(stderr.String(), "outcome=timed_out") {
			t.Fatalf("stderr = %q, want timed_out lifecycle outcome", stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pool-managed stop wedged interruptTargetsBounded")
	}
}
