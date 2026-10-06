package main

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
)

const testBDContextJSON = `{"backend":"dolt","dolt_mode":"server","bd_version":"0.52.1","schema_version":7}`

// bdContextProbeStub stands in for the bd subprocess the preflight probe forks:
// it counts spawns, remembers the last call, and owns a clock the test advances
// instead of sleeping.
type bdContextProbeStub struct {
	spawns   atomic.Int32
	mu       sync.Mutex
	lastDir  string
	lastArgs []string
	now      time.Time
}

func (s *bdContextProbeStub) advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = s.now.Add(d)
}

func (s *bdContextProbeStub) lastCall() (string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastDir, append([]string(nil), s.lastArgs...)
}

func (s *bdContextProbeStub) clock() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// stubBDContextProbe replaces the probe's runner and clock and starts from an
// empty memo, restoring all three on cleanup. respond sees the 1-based spawn
// ordinal so a test can make the first probe fail.
func stubBDContextProbe(t *testing.T, respond func(spawn int32) ([]byte, error)) *bdContextProbeStub {
	t.Helper()

	if respond == nil {
		respond = func(int32) ([]byte, error) { return []byte(testBDContextJSON), nil }
	}
	stub := &bdContextProbeStub{now: time.Unix(1_700_000_000, 0).UTC()}

	prevRunner, prevNow, prevEntries := preflightBDCommandRunner, preflightBDContextNow, preflightBDContexts
	preflightBDContexts = map[string]*preflightBDContextEntry{}
	preflightBDCommandRunner = func(string) beads.CommandRunner {
		return func(dir, name string, args ...string) ([]byte, error) {
			stub.mu.Lock()
			stub.lastDir, stub.lastArgs = dir, args
			stub.mu.Unlock()
			if name != "bd" {
				t.Errorf("probe ran %q, want bd", name)
			}
			return respond(stub.spawns.Add(1))
		}
	}
	preflightBDContextNow = stub.clock

	t.Cleanup(func() {
		preflightBDCommandRunner, preflightBDContextNow, preflightBDContexts = prevRunner, prevNow, prevEntries
	})
	return stub
}

// wantBDContext reads the scope through the reader under test and asserts the
// parsed answer. It takes the reader rather than the result so a test can call
// it on one expression (Go will not pass a multi-value call beside another arg).
func wantBDContext(t *testing.T, read func(string) (contract.PreflightBDContext, error), scope string) {
	t.Helper()
	got, err := read(scope)
	if err != nil {
		t.Fatalf("reader(%q) returned error: %v", scope, err)
	}
	want := contract.PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "0.52.1", SchemaVersion: 7}
	if got != want {
		t.Fatalf("bd context = %+v, want %+v", got, want)
	}
}

// The whole point of the memo: a gc CLI call opens several stores and each open
// ran its own preflight, forking one bd per open to ask what backend the
// process was already about to open.
func TestPreflightBDContextReaderProbesOncePerTTL(t *testing.T) {
	stub := stubBDContextProbe(t, nil)
	read := preflightBDContextReader("/city/estate")

	for i := 0; i < 3; i++ {
		wantBDContext(t, read, "/city/estate")
	}

	if got := stub.spawns.Load(); got != 1 {
		t.Fatalf("spawns = %d over three reads, want 1", got)
	}
	dir, args := stub.lastCall()
	if dir != "/city/estate" || len(args) != 2 || args[0] != "context" || args[1] != "--json" {
		t.Fatalf("probe ran bd %v in %q, want context --json in the scope", args, dir)
	}
}

// One scope's answer must never answer for another: the scope is what bd
// resolves, and a city can hold several.
func TestPreflightBDContextReaderKeyedByScope(t *testing.T) {
	stub := stubBDContextProbe(t, nil)
	read := preflightBDContextReader("/city/estate")

	wantBDContext(t, read, "/city/estate")
	wantBDContext(t, read, "/city/estate/packs/gc")
	wantBDContext(t, read, "/city/estate")

	if got := stub.spawns.Load(); got != 2 {
		t.Fatalf("spawns = %d, want one per distinct scope", got)
	}
}

// A city path written two ways is one city, not two caches.
func TestPreflightBDContextReaderNormalizesCityPath(t *testing.T) {
	stub := stubBDContextProbe(t, nil)

	wantBDContext(t, preflightBDContextReader("/city/estate"), "/city/estate")
	wantBDContext(t, preflightBDContextReader("/city/estate/"), "/city/estate")

	if got := stub.spawns.Load(); got != 1 {
		t.Fatalf("spawns = %d for one city spelled two ways, want 1", got)
	}
}

// Fail-closed is the preflight's contract: an unreachable or mid-upgrade bd has
// to keep failing every later check, so a failed probe must not be remembered.
func TestPreflightBDContextReaderNeverCachesAFailedProbe(t *testing.T) {
	boom := errors.New("bd not installed")
	stub := stubBDContextProbe(t, func(spawn int32) ([]byte, error) {
		if spawn == 1 {
			return nil, boom
		}
		return []byte(testBDContextJSON), nil
	})
	read := preflightBDContextReader("/city/estate")

	if _, err := read("/city/estate"); !errors.Is(err, boom) {
		t.Fatalf("first read error = %v, want %v", err, boom)
	}
	wantBDContext(t, read, "/city/estate")
	if got := stub.spawns.Load(); got != 2 {
		t.Fatalf("spawns = %d after a failed probe, want the failure re-probed", got)
	}

	wantBDContext(t, read, "/city/estate")
	if got := stub.spawns.Load(); got != 2 {
		t.Fatalf("spawns = %d after a success was cached, want no further probe", got)
	}
}

// The TTL is what bounds staleness in the long-lived processes (controller, API
// server) that can outlive a bd upgrade.
func TestPreflightBDContextReaderReprobesAfterTTL(t *testing.T) {
	stub := stubBDContextProbe(t, nil)
	read := preflightBDContextReader("/city/estate")

	wantBDContext(t, read, "/city/estate")

	stub.advance(preflightBDContextTTL - time.Second)
	wantBDContext(t, read, "/city/estate")
	if got := stub.spawns.Load(); got != 1 {
		t.Fatalf("spawns = %d inside the TTL, want the cached answer", got)
	}

	stub.advance(2 * time.Second)
	wantBDContext(t, read, "/city/estate")
	if got := stub.spawns.Load(); got != 2 {
		t.Fatalf("spawns = %d past the TTL, want a fresh probe", got)
	}
}

// Store opens for one scope arrive from several goroutines; the entry mutex is
// what turns that crowd into one spawn rather than a stampede of bd children.
// Failures travel on a channel: t.Fatalf may not be called off the test goroutine.
func TestPreflightBDContextReaderConcurrentReadersSpawnOnce(t *testing.T) {
	stub := stubBDContextProbe(t, nil)
	read := preflightBDContextReader("/city/estate")

	errCh := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := read("/city/estate")
			if err != nil {
				errCh <- err
				return
			}
			if got.Backend != "dolt" || got.SchemaVersion != 7 {
				errCh <- fmt.Errorf("concurrent reader got %+v", got)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent reader: %v", err)
	}

	if got := stub.spawns.Load(); got != 1 {
		t.Fatalf("spawns = %d for eight concurrent readers, want 1", got)
	}
}
