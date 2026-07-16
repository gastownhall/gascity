package beads

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// TestInspectConditionalWritesIsSideEffectFree pins the inspector's core
// contract: inspecting an unprobed BdStore reports probe=unprobed and runs
// ZERO bd subprocesses — a status poll must never pay the four-verb probe
// tax or mutate the capability memo.
func TestInspectConditionalWritesIsSideEffectFree(t *testing.T) {
	calls := 0
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		calls++
		return nil, errors.New("inspector must not run bd")
	}
	s := NewBdStore("/city", runner)
	s.stampConditionalWritesMode(gate.Require, false)

	insp := InspectConditionalWrites(s)
	if calls != 0 {
		t.Fatalf("inspection ran %d bd subprocesses, want 0", calls)
	}
	if insp.Mode != gate.Require {
		t.Errorf("Mode = %q, want require", insp.Mode)
	}
	if insp.StoreKind != BeadsStoreNameBdStore {
		t.Errorf("StoreKind = %q, want %q", insp.StoreKind, BeadsStoreNameBdStore)
	}
	if insp.Probe != ConditionalWriteProbeUnprobed {
		t.Errorf("Probe = %q, want unprobed", insp.Probe)
	}
	if insp.Latch != ConditionalWriteLatchUnlatched {
		t.Errorf("Latch = %q, want unlatched", insp.Latch)
	}
	if !insp.Capable {
		t.Error("unprobed store with no definitive incapable verdict should report Capable=true")
	}

	// The memo must be untouched: the first real capability check still probes.
	s.condWriteMu.Lock()
	probed := s.condWriteProbed
	s.condWriteMu.Unlock()
	if probed {
		t.Error("inspection set the probe memo")
	}
}

// TestInspectConditionalWritesReadsLatchedState pins the §12.6 skew story:
// probe memo and runtime latch are reported independently, so an in-place bd
// upgrade after a runtime latch is legible as probe=capable latch=incapable.
func TestInspectConditionalWritesReadsLatchedState(t *testing.T) {
	s := NewBdStore("/city", func(string, string, ...string) ([]byte, error) {
		return nil, errors.New("no probe expected")
	})
	s.stampConditionalWritesMode(gate.Auto, false)
	s.condWriteMu.Lock()
	s.condWriteProbed, s.condWriteCapable = true, true
	s.condWriteMu.Unlock()
	s.markConditionalWritesUnsupported()

	insp := InspectConditionalWrites(s)
	if insp.Probe != ConditionalWriteProbeCapable {
		t.Errorf("Probe = %q, want capable (memoized verdict survives the latch)", insp.Probe)
	}
	if insp.Latch != ConditionalWriteLatchIncapable {
		t.Errorf("Latch = %q, want incapable (runtime latch)", insp.Latch)
	}
	if insp.Capable {
		t.Error("latched store must report Capable=false (the latch is authoritative)")
	}
	if insp.Reason == "" {
		t.Error("incapable verdicts must carry a reason")
	}
}

// TestInspectConditionalWritesProbeIncapableReason distinguishes "bd too old"
// from "bd broken" via the memoized probe error.
func TestInspectConditionalWritesProbeIncapableReason(t *testing.T) {
	s := NewBdStore("/city", func(string, string, ...string) ([]byte, error) {
		return nil, errors.New("no probe expected")
	})
	s.stampConditionalWritesMode(gate.Auto, false)
	s.condWriteMu.Lock()
	s.condWriteProbed, s.condWriteCapable = true, false
	s.condWriteProbeErr = errors.New("exec: bd: not found")
	s.condWriteMu.Unlock()

	insp := InspectConditionalWrites(s)
	if insp.Probe != ConditionalWriteProbeIncapable {
		t.Errorf("Probe = %q, want incapable", insp.Probe)
	}
	if insp.Capable {
		t.Error("probe-incapable store must report Capable=false")
	}
	if want := "exec: bd: not found"; !strings.Contains(insp.Reason, want) {
		t.Errorf("Reason = %q, want the memoized probe error (%q)", insp.Reason, want)
	}
}

// TestInspectConditionalWritesMemAndWrappers pins the instant-probe stores
// (Mem/File report their instance toggle with no latch machinery) and the
// wrapper walk (CachingStore inspects THROUGH to its backing).
func TestInspectConditionalWritesMemAndWrappers(t *testing.T) {
	m := NewMemStore()
	m.stampConditionalWritesMode(gate.Auto, false)
	insp := InspectConditionalWrites(m)
	if insp.Probe != ConditionalWriteProbeCapable || insp.Latch != ConditionalWriteLatchUnlatched || !insp.Capable {
		t.Errorf("MemStore inspection = %+v, want probe=capable latch=unlatched capable=true", insp)
	}

	m.DisableConditionalWrites = true
	insp = InspectConditionalWrites(m)
	if insp.Probe != ConditionalWriteProbeIncapable || insp.Capable {
		t.Errorf("disabled MemStore inspection = %+v, want probe=incapable capable=false", insp)
	}

	// CachingStore reports its backing's state and kind resolution follows
	// the same walk the write path uses.
	backing := NewMemStore()
	backing.stampConditionalWritesMode(gate.Require, false)
	c := NewCachingStore(backing, nil)
	cInsp := InspectConditionalWrites(c)
	if cInsp.Mode != gate.Require {
		t.Errorf("cache-wrapped Mode = %q, want require (backing stamp)", cInsp.Mode)
	}
	if cInsp.Probe != ConditionalWriteProbeCapable || !cInsp.Capable {
		t.Errorf("cache-wrapped inspection = %+v, want backing's capable state", cInsp)
	}
}

// TestInspectConditionalWritesUnstamped pins the legacy path: no stamp means
// ModeUnset, and the write path never fences, so capability detail is moot
// but must not panic or probe.
func TestInspectConditionalWritesUnstamped(t *testing.T) {
	insp := InspectConditionalWrites(NewMemStore())
	if insp.Mode != gate.ModeUnset {
		t.Errorf("Mode = %q, want unset", insp.Mode)
	}
	if insp := InspectConditionalWrites(nil); insp.StoreKind != "<nil>" {
		t.Errorf("nil store StoreKind = %q, want <nil>", insp.StoreKind)
	}
}
