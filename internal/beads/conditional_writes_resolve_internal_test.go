package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// stampedNoCASStore is a purpose-built minimal store shape for the seam matrix:
// it carries a mode stamp but does not implement ConditionalWriter — the
// NativeDoltStore/exec.Store shape. It embeds the Store interface only to
// satisfy the seam's parameter type; no Store method is ever invoked by the
// seam. This is a seam-matrix double, not a conformance-store wrapper (the
// §7.3 interface-stripping ban targets conformance fakes that hide optional
// interfaces of a real store; here capability absence IS the shape under test).
type stampedNoCASStore struct {
	Store
	condWritesStamp
}

// casOnlyStore implements ConditionalWriter (by embedding the interface) and
// carries a stamp, but does NOT implement the capability prober — the
// vacuously-capable cell, mirroring rollout's nil-predicate rule.
type casOnlyStore struct {
	Store
	ConditionalWriter
	condWritesStamp
}

// probeCountingStore counts prober consultations so the Off-is-zero-cost cell
// can assert the prober was never reached.
type probeCountingStore struct {
	Store
	ConditionalWriter
	condWritesStamp
	probes  atomic.Int32
	capable bool
	reason  string
}

func (p *probeCountingStore) probeConditionalWriteCapability() (bool, string) {
	p.probes.Add(1)
	return p.capable, p.reason
}

func TestCondWritesStampZeroValueIsUnset(t *testing.T) {
	t.Parallel()
	var s condWritesStamp
	mode, defaulted := s.conditionalWritesMode()
	if mode != gate.ModeUnset || defaulted {
		t.Fatalf("zero stamp = (%q, %v), want (ModeUnset, false)", mode, defaulted)
	}
}

func TestCondWritesStampStampAndRead(t *testing.T) {
	t.Parallel()
	var s condWritesStamp
	if !s.stampConditionalWritesMode(gate.Auto, false) {
		t.Fatal("a stamp-owning store must report landed=true")
	}
	if mode, defaulted := s.conditionalWritesMode(); mode != gate.Auto || defaulted {
		t.Fatalf("after stamp(Auto,false) = (%q, %v), want (auto, false)", mode, defaulted)
	}
	s.stampConditionalWritesMode(gate.Off, true)
	if mode, defaulted := s.conditionalWritesMode(); mode != gate.Off || !defaulted {
		t.Fatalf("after stamp(Off,true) = (%q, %v), want (off, true)", mode, defaulted)
	}
}

func TestCondWritesStampDegradeOnce(t *testing.T) {
	t.Parallel()
	var s condWritesStamp
	if !s.noteConditionalDegradeOnce() {
		t.Fatal("first noteConditionalDegradeOnce = false, want true")
	}
	if s.noteConditionalDegradeOnce() {
		t.Fatal("second noteConditionalDegradeOnce = true, want false")
	}
}

func TestCondWritesStampDegradeOnceConcurrent(t *testing.T) {
	t.Parallel()
	var s condWritesStamp
	const n = 16
	var firsts atomic.Int32
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.noteConditionalDegradeOnce() {
				firsts.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := firsts.Load(); got != 1 {
		t.Fatalf("%d goroutines observed first-degrade, want exactly 1", got)
	}
}

func TestResolveConditionalWriterLegacyCells(t *testing.T) {
	t.Parallel()
	assertLegacy := func(t *testing.T, store Store) {
		t.Helper()
		w, diag, err := ResolveConditionalWriter(store)
		if w != nil || diag != nil || err != nil {
			t.Fatalf("ResolveConditionalWriter = (%v, %v, %v), want (nil, nil, nil)", w, diag, err)
		}
	}
	t.Run("nil store", func(t *testing.T) {
		t.Parallel()
		assertLegacy(t, nil)
	})
	t.Run("unstamped store is ModeUnset legacy", func(t *testing.T) {
		t.Parallel()
		assertLegacy(t, NewMemStore())
	})
	t.Run("stamped off", func(t *testing.T) {
		t.Parallel()
		mem := NewMemStore()
		mem.stampConditionalWritesMode(gate.Off, false)
		assertLegacy(t, mem)
	})
	t.Run("stamped explicit unset", func(t *testing.T) {
		t.Parallel()
		mem := NewMemStore()
		mem.stampConditionalWritesMode(gate.ModeUnset, true)
		assertLegacy(t, mem)
	})
	t.Run("off never consults the prober", func(t *testing.T) {
		t.Parallel()
		mem := NewMemStore()
		pcs := &probeCountingStore{Store: mem, ConditionalWriter: mem, capable: true}
		pcs.stampConditionalWritesMode(gate.Off, false)
		assertLegacy(t, pcs)
		if got := pcs.probes.Load(); got != 0 {
			t.Fatalf("prober consulted %d times under off, want 0 (off is zero-cost)", got)
		}
	})
}

func TestResolveConditionalWriterAutoCapableReturnsOuterStoreWriter(t *testing.T) {
	t.Parallel()
	mem := NewMemStore()
	mem.stampConditionalWritesMode(gate.Auto, false)
	w, diag, err := ResolveConditionalWriter(mem)
	if err != nil || diag != nil {
		t.Fatalf("auto∧capable: diag=%v err=%v, want nil/nil", diag, err)
	}
	if got, ok := w.(*MemStore); !ok || got != mem {
		t.Fatalf("auto∧capable writer = %T(%p), want the resolved store itself (%p)", w, w, mem)
	}
}

func TestResolveConditionalWriterAutoIncapableDegradesLoud(t *testing.T) {
	t.Parallel()
	mem := NewMemStore()
	mem.DisableConditionalWrites = true
	mem.stampConditionalWritesMode(gate.Auto, false)

	for _, call := range []string{"first", "second"} {
		w, diag, err := ResolveConditionalWriter(mem)
		if err != nil {
			t.Fatalf("%s call: err = %v, want nil (auto degrades, never errors)", call, err)
		}
		if w != nil {
			t.Fatalf("%s call: writer = %v, want nil", call, w)
		}
		if diag == nil {
			t.Fatalf("%s call: diagnostic = nil, want loud degrade diagnostic on every call", call)
		}
		if diag.PreflightGate != "conditional_writes" {
			t.Fatalf("%s call: PreflightGate = %q, want %q", call, diag.PreflightGate, "conditional_writes")
		}
		if diag.Store != "MemStore" {
			t.Fatalf("%s call: diag.Store = %q, want MemStore", call, diag.Store)
		}
		if !strings.Contains(diag.PreflightReason, "mode=auto") {
			t.Fatalf("%s call: PreflightReason = %q, want mode=auto in the reason", call, diag.PreflightReason)
		}
		if !strings.Contains(diag.PreflightReason, "disabled") {
			t.Fatalf("%s call: PreflightReason = %q, want the prober's reason", call, diag.PreflightReason)
		}
	}
}

func TestResolveConditionalWriterRequireCapableReturnsWriter(t *testing.T) {
	t.Parallel()
	mem := NewMemStore()
	mem.stampConditionalWritesMode(gate.Require, false)
	w, diag, err := ResolveConditionalWriter(mem)
	if err != nil || diag != nil || w == nil {
		t.Fatalf("require∧capable = (%v, %v, %v), want (writer, nil, nil)", w, diag, err)
	}
}

func TestResolveConditionalWriterRequireIncapableRefusesClosed(t *testing.T) {
	t.Parallel()
	mem := NewMemStore()
	mem.DisableConditionalWrites = true
	mem.stampConditionalWritesMode(gate.Require, false)

	w, diag, err := ResolveConditionalWriter(mem)
	if w != nil {
		t.Fatalf("require∧incapable writer = %v, want nil (fail closed)", w)
	}
	if diag == nil || diag.PreflightGate != "conditional_writes" {
		t.Fatalf("require∧incapable diag = %+v, want conditional_writes diagnostic alongside the error", diag)
	}
	if err == nil {
		t.Fatal("require∧incapable err = nil, want typed refusal")
	}
	if !IsConditionalWritesRequired(err) {
		t.Fatalf("IsConditionalWritesRequired(%v) = false, want true", err)
	}
	var cre *ConditionalWritesRequiredError
	if !errors.As(err, &cre) {
		t.Fatalf("errors.As(%T) failed", err)
	}
	if cre.StoreKind != "MemStore" {
		t.Fatalf("StoreKind = %q, want MemStore", cre.StoreKind)
	}
	if cre.Reason == "" {
		t.Fatal("Reason is empty, want the prober's reason")
	}
	wantPrefix := "conditional_writes refused: store=MemStore mode=require reason="
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("Error() = %q, want prefix %q (the §12.3 refusal grammar)", err.Error(), wantPrefix)
	}
}

func TestResolveConditionalWriterFileStorePromotion(t *testing.T) {
	t.Parallel()
	t.Run("capable through promotion, stamp survives reload", func(t *testing.T) {
		t.Parallel()
		fs, err := OpenFileStore(fsys.OSFS{}, t.TempDir()+"/beads.json")
		if err != nil {
			t.Fatalf("OpenFileStore: %v", err)
		}
		fs.stampConditionalWritesMode(gate.Auto, false)
		// A write runs the reload-before-write path; the stamp must survive
		// because reloadFromDisk mutates the embedded MemStore in place.
		if _, err := fs.Create(Bead{Title: "t"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		w, diag, resolveErr := ResolveConditionalWriter(fs)
		if resolveErr != nil || diag != nil || w == nil {
			t.Fatalf("FileStore auto∧capable after write = (%v, %v, %v), want (writer, nil, nil)", w, diag, resolveErr)
		}
	})
	t.Run("degrade reports FileStore kind", func(t *testing.T) {
		t.Parallel()
		fs, err := OpenFileStore(fsys.OSFS{}, t.TempDir()+"/beads.json")
		if err != nil {
			t.Fatalf("OpenFileStore: %v", err)
		}
		fs.DisableConditionalWrites = true
		fs.stampConditionalWritesMode(gate.Auto, false)
		w, diag, resolveErr := ResolveConditionalWriter(fs)
		if w != nil || resolveErr != nil || diag == nil {
			t.Fatalf("FileStore auto∧disabled = (%v, %v, %v), want (nil, diag, nil)", w, diag, resolveErr)
		}
		if diag.Store != "FileStore" {
			t.Fatalf("diag.Store = %q, want FileStore (not the embedded MemStore)", diag.Store)
		}
	})
}

func TestResolveConditionalWriterInterfaceAbsentStore(t *testing.T) {
	t.Parallel()
	t.Run("auto degrades", func(t *testing.T) {
		t.Parallel()
		s := &stampedNoCASStore{Store: NewMemStore()}
		s.stampConditionalWritesMode(gate.Auto, false)
		w, diag, err := ResolveConditionalWriter(s)
		if w != nil || err != nil || diag == nil {
			t.Fatalf("auto∧no-interface = (%v, %v, %v), want (nil, diag, nil)", w, diag, err)
		}
		if !strings.Contains(diag.PreflightReason, "does not implement conditional writes") {
			t.Fatalf("PreflightReason = %q, want the interface-absent reason", diag.PreflightReason)
		}
	})
	t.Run("require refuses", func(t *testing.T) {
		t.Parallel()
		s := &stampedNoCASStore{Store: NewMemStore()}
		s.stampConditionalWritesMode(gate.Require, false)
		w, diag, err := ResolveConditionalWriter(s)
		if w != nil || diag == nil || !IsConditionalWritesRequired(err) {
			t.Fatalf("require∧no-interface = (%v, %v, %v), want (nil, diag, typed refusal)", w, diag, err)
		}
	})
}

func TestResolveConditionalWriterVacuouslyCapableWithoutProber(t *testing.T) {
	t.Parallel()
	mem := NewMemStore()
	s := &casOnlyStore{Store: mem, ConditionalWriter: mem}
	s.stampConditionalWritesMode(gate.Auto, false)
	w, diag, err := ResolveConditionalWriter(s)
	if err != nil || diag != nil || w == nil {
		t.Fatalf("auto∧CAS-without-prober = (%v, %v, %v), want (writer, nil, nil) — vacuously capable", w, diag, err)
	}
}

func TestResolveConditionalWriterProberConsultedOncePerCall(t *testing.T) {
	t.Parallel()
	mem := NewMemStore()
	pcs := &probeCountingStore{Store: mem, ConditionalWriter: mem, capable: false, reason: "scripted incapable"}
	pcs.stampConditionalWritesMode(gate.Auto, false)
	for i := 1; i <= 2; i++ {
		if _, diag, _ := ResolveConditionalWriter(pcs); diag == nil {
			t.Fatalf("call %d: want degrade diagnostic", i)
		}
		if got := pcs.probes.Load(); got != int32(i) {
			t.Fatalf("after call %d: prober consulted %d times, want %d", i, got, i)
		}
	}
}

func TestConditionalWritesRequiredErrorIdentity(t *testing.T) {
	t.Parallel()
	refusal := error(&ConditionalWritesRequiredError{StoreKind: "MemStore", Reason: "r"})
	wrapped := fmt.Errorf("resolving: %w", refusal)
	if !IsConditionalWritesRequired(wrapped) {
		t.Fatal("wrapped refusal not detected by IsConditionalWritesRequired")
	}
	for name, err := range map[string]error{
		"precondition": &PreconditionFailedError{ID: "b-1", Expected: 1, Current: 2},
		"gate refusal": &GateRefusalError{ID: "b-1", Verb: "close"},
		"exhaustion":   &CASRetriesExhaustedError{ID: "b-1", Key: "k", Attempts: 4},
		"unsupported":  ErrConditionalWriteUnsupported,
	} {
		if IsConditionalWritesRequired(err) {
			t.Fatalf("IsConditionalWritesRequired(%s) = true, want false", name)
		}
	}
	if IsPreconditionFailed(refusal) || IsGateRefusal(refusal) || IsCASRetriesExhausted(refusal) || IsConditionalWriteUnsupported(refusal) {
		t.Fatal("refusal matched an unrelated typed-error helper")
	}
	var nilErr *ConditionalWritesRequiredError
	if got := nilErr.Error(); got != "<nil>" {
		t.Fatalf("nil receiver Error() = %q, want <nil>", got)
	}
}

func TestConditionalStoreKindFallsBackToTypeName(t *testing.T) {
	t.Parallel()
	s := &stampedNoCASStore{Store: NewMemStore()}
	if got := conditionalStoreKind(s); !strings.Contains(got, "stampedNoCASStore") {
		t.Fatalf("conditionalStoreKind = %q, want the %%T fallback naming the concrete type", got)
	}
}

// TestConditionalWritesStampConcurrentStampAndResolve exposes the stamp's
// mutex to the race detector: stores are not always stamped strictly before
// sharing (the t3bridge watcher path), so a stamp write racing a seam resolve
// must be race-clean. Runs under the -race Conditional gate; dropping the
// stamp mutex fails here.
func TestConditionalWritesStampConcurrentStampAndResolve(t *testing.T) {
	t.Parallel()
	mem := NewMemStore()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		mode := gate.Auto
		if i%2 == 0 {
			mode = gate.Off
		}
		go func() {
			defer wg.Done()
			mem.stampConditionalWritesMode(mode, false)
		}()
		go func() {
			defer wg.Done()
			// Every interleaving is legal (auto→writer, off→legacy); the
			// assertion is the race detector plus outcome coherence.
			w, diag, err := ResolveConditionalWriter(mem)
			if err != nil {
				t.Errorf("resolve errored under concurrent stamping: %v", err)
			}
			if w == nil && diag != nil {
				t.Error("capable store degraded under concurrent stamping")
			}
		}()
	}
	wg.Wait()
}

// resolveTargetWrapper is a purpose-built wrapper double that declares its
// resolution target — the beadPolicyStore shape (interface embedding blocks
// both the unexported carrier and ConditionalWriter promotion, so the wrapper
// consents to resolution against its inner store instead).
type resolveTargetWrapper struct {
	Store
	target Store
}

func (w *resolveTargetWrapper) ConditionalWritesResolveTarget() Store { return w.target }

func TestResolveConditionalWriterFollowsDeclaredResolveTarget(t *testing.T) {
	t.Parallel()
	t.Run("single wrapper resolves the inner store", func(t *testing.T) {
		t.Parallel()
		mem := NewMemStore()
		mem.stampConditionalWritesMode(gate.Auto, false)
		w := &resolveTargetWrapper{Store: mem, target: mem}
		writer, diag, err := ResolveConditionalWriter(w)
		if err != nil || diag != nil {
			t.Fatalf("resolve through wrapper = diag %v err %v, want nil/nil", diag, err)
		}
		if got, ok := writer.(*MemStore); !ok || got != mem {
			t.Fatalf("writer = %T, want the INNER stamped store", writer)
		}
	})
	t.Run("nested wrappers follow to the innermost target", func(t *testing.T) {
		t.Parallel()
		mem := NewMemStore()
		mem.DisableConditionalWrites = true
		mem.stampConditionalWritesMode(gate.Require, false)
		inner := &resolveTargetWrapper{Store: mem, target: mem}
		outer := &resolveTargetWrapper{Store: inner, target: inner}
		_, diag, err := ResolveConditionalWriter(outer)
		if diag == nil || !IsConditionalWritesRequired(err) {
			t.Fatalf("nested resolve = (diag %v, err %v), want the inner store's require refusal", diag, err)
		}
	})
	t.Run("class wrappers pass through", func(t *testing.T) {
		t.Parallel()
		mem := NewMemStore()
		mem.stampConditionalWritesMode(gate.Auto, false)
		writer, diag, err := ResolveConditionalWriter(GraphStore{Store: mem})
		if err != nil || diag != nil || writer == nil {
			t.Fatalf("resolve through GraphStore class wrapper = (%v, %v, %v), want the store's writer", writer, diag, err)
		}
	})
	t.Run("self-referential target terminates as legacy", func(t *testing.T) {
		t.Parallel()
		w := &resolveTargetWrapper{Store: NewMemStore()}
		w.target = w
		writer, diag, err := ResolveConditionalWriter(w)
		if writer != nil || diag != nil || err != nil {
			t.Fatalf("cyclic target = (%v, %v, %v), want bounded legacy resolution", writer, diag, err)
		}
	})
	t.Run("nil target terminates on the wrapper", func(t *testing.T) {
		t.Parallel()
		w := &resolveTargetWrapper{Store: NewMemStore(), target: nil}
		writer, diag, err := ResolveConditionalWriter(w)
		if writer != nil || diag != nil || err != nil {
			t.Fatalf("nil target = (%v, %v, %v), want legacy (wrapper itself carries no stamp)", writer, diag, err)
		}
	})
}

func TestResolveConditionalWriterDegradeEmissionLatchedOnce(t *testing.T) {
	t.Parallel()
	var fired []ConditionalWritesDegrade
	mem := NewMemStore()
	mem.DisableConditionalWrites = true
	mem.stampConditionalWritesMode(gate.Auto, false)
	mem.setConditionalWritesDegradeCallback(func(d ConditionalWritesDegrade) { fired = append(fired, d) })

	for range 3 {
		if _, diag, _ := ResolveConditionalWriter(mem); diag == nil {
			t.Fatal("want degrade diagnostic on every resolve")
		}
	}
	if len(fired) != 1 {
		t.Fatalf("degrade callback fired %d times over 3 resolves, want exactly 1 (latched per store)", len(fired))
	}
	if fired[0].StoreKind != "MemStore" || fired[0].Mode != "auto" || !strings.Contains(fired[0].Reason, "disabled") {
		t.Fatalf("degrade notification = %+v, want kind/mode/reason populated", fired[0])
	}
}

func TestResolveConditionalWriterRequireRefusalDoesNotEmit(t *testing.T) {
	t.Parallel()
	var fired int
	mem := NewMemStore()
	mem.DisableConditionalWrites = true
	mem.stampConditionalWritesMode(gate.Require, false)
	mem.setConditionalWritesDegradeCallback(func(ConditionalWritesDegrade) { fired++ })

	if _, _, err := ResolveConditionalWriter(mem); !IsConditionalWritesRequired(err) {
		t.Fatal("want typed refusal")
	}
	if fired != 0 {
		t.Fatalf("refusal fired the degrade callback %d times, want 0 (refusals are typed errors, not events)", fired)
	}
}

func TestFactoryInjectsDegradeCallback(t *testing.T) {
	t.Parallel()
	var fired int
	mem := NewMemStore()
	mem.DisableConditionalWrites = true
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:                   "/city",
		Provider:                    "file",
		ConditionalWrites:           gate.Auto,
		OpenFileStore:               func() (Store, error) { return mem, nil },
		OnConditionalWritesDegraded: func(ConditionalWritesDegrade) { fired++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, _, _ = ResolveConditionalWriter(result.Store)
	}
	if fired != 1 {
		t.Fatalf("factory-injected callback fired %d times, want exactly 1", fired)
	}
}

func TestCachingStoreForwardsDegradeCallbackToBacking(t *testing.T) {
	t.Parallel()
	var fired int
	mem := NewMemStore()
	mem.DisableConditionalWrites = true
	cache := NewCachingStoreForTest(mem, nil)
	cache.stampConditionalWritesMode(gate.Auto, false)
	cache.setConditionalWritesDegradeCallback(func(ConditionalWritesDegrade) { fired++ })

	_, _, _ = ResolveConditionalWriter(cache) // degrade via the cache
	_, _, _ = ResolveConditionalWriter(mem)   // degrade via the backing: SAME latch
	if fired != 1 {
		t.Fatalf("callback fired %d times across cache+backing resolves, want 1 (one shared latch)", fired)
	}
}
