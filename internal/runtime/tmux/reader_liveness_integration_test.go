//go:build integration && linux

package tmux

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// readerLivenessConfig is a relayed, capture-armed emitter session whose
// liveness names the emitter, so a start waits for it below the relay.
func readerLivenessConfig(t *testing.T) runtime.Config {
	emit := captureEmitterBin(t)
	return runtime.Config{
		Command:       emitterCommand(emit, "report state=working", "hang"),
		WorkDir:       t.TempDir(),
		ProcessNames:  []string{filepath.Base(emit)},
		ProgramStatus: runtime.ProgramStatusArm{Capture: true, Relay: true},
	}
}

// On real tmux (scratch server osc7501x-liv-<rand>), a relay-wrapped start
// whose host cannot take a full process snapshot (every ps fails) still
// finds its agent below gc-osc7501 at once, reading only the pane's subtree,
// and its liveness probes verify the agent rather than reporting the table
// unread.
func TestRelayedStartFindsAgentWhileProcessSnapshotFails(t *testing.T) {
	p := captureProvider(t, newScratchServer(t, "liv"), captureCompanionBin(t))
	cfg := readerLivenessConfig(t)
	const name = "gc-ps-liv-degraded"
	readsSince := shimProcessTableTools(t, true)

	start := time.Now()
	if err := p.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start under a failing process snapshot: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	// Before the subtree read, this start waited out its 30 s command wait
	// and took ~51 s.
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("Start took %v under a failing process snapshot, want no wait on the snapshot", took)
	}
	readsSince()
	if err := p.tm.WaitForCommand(context.Background(), name, supportedShells, 5*time.Second); err != nil {
		t.Fatalf("WaitForCommand under a failing process snapshot: %v (Start took %v)", err, time.Since(start))
	}
	if running, err := p.tm.runtimeRunning(name, cfg.ProcessNames); !running || err != nil {
		t.Fatalf("runtimeRunning = (%v, %v) under a failing process snapshot, want the relayed agent verified", running, err)
	}
	if reads := readsSince(); len(reads) != 0 {
		t.Errorf("the wrapped looks ran ps/pgrep %d times %q, want none", len(reads), reads)
	}
}

// On real tmux, a pane armed after the state cache's process table was
// scanned (as during a start, whose readiness readings take the table the
// cache held before the arm) reads its record, unverified, from the
// inventory, not reader_lost; once the table is scanned again it verifies
// the reader. The arm here is a capture respawn, which leaves the cache's
// table in place.
func TestFreshPaneReadsLiveTapBeforeTheNextSnapshot(t *testing.T) {
	p := captureProvider(t, newScratchServer(t, "liv"), captureCompanionBin(t))
	// A table that stays put until invalidated.
	p.cache = NewStateCache(&tmuxFetcher{tm: p.tm}, time.Hour)
	const name = "gc-ps-liv-fresh"
	cfg := readerLivenessConfig(t)
	if err := p.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	first := armedNonce(t, p, name)
	eventually(t, "the first tap to run", func() bool { return tapRunning(t, first) })
	if r, err := p.ProgramStatus(name); err != nil || r.Nonce != first {
		t.Fatalf("priming the cache: ProgramStatus = (%+v, %v), want a reading under %s", r, err, first)
	}

	// The arm, after the cached scan.
	if err := p.tm.respawnPaneWithWorkDir(name, cfg.WorkDir, cfg.Command, cfg.ProgramStatus); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	nonce := armedNonce(t, p, name)
	if nonce == first {
		t.Fatalf("respawn kept nonce %q", first)
	}
	eventually(t, "the new tap to run", func() bool { return tapRunning(t, nonce) })

	var reading runtime.ProgramStatus
	eventually(t, "the working record in the inventory", func() bool {
		entries, err := p.RuntimeInventory(context.Background())
		if err != nil {
			t.Fatalf("RuntimeInventory: %v", err)
		}
		reading = entries[name].Program
		if reading.Validity == runtime.ProgramStatusReaderLost {
			t.Fatalf("fresh pane reads %+v from a table scanned before its arm, want its tap's liveness unknown, not reader_lost", reading)
		}
		return reading.Running()
	})
	if reading.ReaderVerified || reading.Nonce != nonce {
		t.Fatalf("fresh pane reads %+v, want the working record under %s, unverified by the older table", reading, nonce)
	}

	p.cache.Invalidate()
	if r, err := p.ProgramStatus(name); err != nil || !r.Running() || !r.ReaderVerified {
		t.Fatalf("after a fresh scan: ProgramStatus = (%+v, %v), want a verified working reading", r, err)
	}
}
