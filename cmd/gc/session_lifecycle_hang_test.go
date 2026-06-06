package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
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
}

func newHangingProvider() *hangingProvider {
	return &hangingProvider{
		Fake:     runtime.NewFake(),
		releaseC: make(chan struct{}),
	}
}

func (p *hangingProvider) Stop(name string) error {
	<-p.releaseC
	return p.Fake.Stop(name)
}

func (p *hangingProvider) Interrupt(name string) error {
	<-p.releaseC
	return p.Fake.Interrupt(name)
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
	origStop := stopPerTargetTimeoutDefault
	stopPerTargetTimeoutDefault = 200 * time.Millisecond
	t.Cleanup(func() { stopPerTargetTimeoutDefault = origStop })

	sp := newHangingProvider()
	t.Cleanup(sp.release)

	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Daemon: config.DaemonConfig{ShutdownTimeout: "50ms"},
	}

	var stdout, stderr bytes.Buffer
	done := make(chan struct{})
	go func() {
		gracefulStopAll(
			[]string{"alpha", "bravo", "charlie"},
			sp,
			cfg.Daemon.ShutdownTimeoutDuration(),
			events.Discard,
			cfg,
			nil,
			&stdout,
			&stderr,
		)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gracefulStopAll did not return within 5s — unbounded wait regression")
	}
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
