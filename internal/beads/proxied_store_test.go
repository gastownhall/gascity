package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	beadslib "github.com/steveyegge/beads"
)

// recordingLeaf wraps a Store and records the name of every method called
// through it, so a test can assert which LEAF answered rather than only what the
// answer was. A split store whose reads and writes both produced the right value
// from the wrong leaf would pass every value assertion in this file.
type recordingLeaf struct {
	Store
	mu    sync.Mutex
	calls []string
}

func (r *recordingLeaf) note(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
}

func (r *recordingLeaf) took() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.calls...)
	r.calls = nil
	return out
}

func (r *recordingLeaf) Get(id string) (Bead, error) {
	r.note("Get")
	return r.Store.Get(id)
}

func (r *recordingLeaf) List(query ListQuery) ([]Bead, error) {
	r.note("List")
	return r.Store.List(query)
}

func (r *recordingLeaf) Ready(query ...ReadyQuery) ([]Bead, error) {
	r.note("Ready")
	return r.Store.Ready(query...)
}

func (r *recordingLeaf) DepList(id, direction string) ([]Dep, error) {
	r.note("DepList")
	return r.Store.DepList(id, direction)
}

func (r *recordingLeaf) Ping() error {
	r.note("Ping")
	return r.Store.Ping()
}

func (r *recordingLeaf) Create(b Bead) (Bead, error) {
	r.note("Create")
	return r.Store.Create(b)
}

func (r *recordingLeaf) Update(id string, opts UpdateOpts) error {
	r.note("Update")
	return r.Store.Update(id, opts)
}

func (r *recordingLeaf) Close(id string) error {
	r.note("Close")
	return r.Store.Close(id)
}

func (r *recordingLeaf) SetMetadata(id, key, value string) error {
	r.note("SetMetadata")
	return r.Store.SetMetadata(id, key, value)
}

func (r *recordingLeaf) SetLocalString(id, key, value string) error {
	r.note("SetLocalString")
	return r.Store.SetLocalString(id, key, value)
}

func (r *recordingLeaf) GetLocalString(id, key string) (string, error) {
	r.note("GetLocalString")
	return r.Store.GetLocalString(id, key)
}

// The forwards below exist because recordingLeaf embeds the Store INTERFACE,
// which strips every optional capability — the same stripping the production
// wrapper must not do. Without them this double would make the wrapper look
// capability-blind and the parity test would be asserting the double's defect.
func (r *recordingLeaf) GraphApplyHandle() (GraphApplyStore, bool) {
	return GraphApplyFor(r.Store)
}

func (r *recordingLeaf) stampConditionalWritesMode(mode gate.Mode, defaulted bool) bool {
	carrier, ok := r.Store.(conditionalWritesModeCarrier)
	return ok && carrier.stampConditionalWritesMode(mode, defaulted)
}

func (r *recordingLeaf) conditionalWritesMode() (gate.Mode, bool) {
	if carrier, ok := r.Store.(conditionalWritesModeCarrier); ok {
		return carrier.conditionalWritesMode()
	}
	return gate.ModeUnset, false
}

func (r *recordingLeaf) noteConditionalDegradeOnce() bool {
	carrier, ok := r.Store.(conditionalWritesModeCarrier)
	return ok && carrier.noteConditionalDegradeOnce()
}

func (r *recordingLeaf) setConditionalWritesDegradeCallback(cb func(ConditionalWritesDegrade)) {
	if carrier, ok := r.Store.(conditionalWritesModeCarrier); ok {
		carrier.setConditionalWritesDegradeCallback(cb)
	}
}

func (r *recordingLeaf) fireConditionalWritesDegradeOnce(d ConditionalWritesDegrade) {
	if carrier, ok := r.Store.(conditionalWritesModeCarrier); ok {
		carrier.fireConditionalWritesDegradeOnce(d)
	}
}

func (r *recordingLeaf) localSidecarHandle() *localSidecar {
	if carrier, ok := r.Store.(localSidecarCarrier); ok {
		return carrier.localSidecarHandle()
	}
	return nil
}

// newSplitFixture builds a ProxiedStore whose two leaves share one in-memory
// ledger, with the native (read) leaf read-only latched exactly as the proxied
// opener latches it.
func newSplitFixture(t *testing.T) (*ProxiedStore, *NativeDoltStore, *recordingLeaf) {
	t.Helper()
	storage := &nativeDoltMemStorage{store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true}}

	native := newNativeDoltStoreForTest(storage, WithProxiedReadOnly())
	native.idPrefix = "prx"
	writeLeaf := newNativeDoltStoreForTest(storage)
	writeLeaf.idPrefix = "prx"
	bd := &recordingLeaf{Store: writeLeaf}

	store, err := NewProxiedStore(native, bd, PinForTest("/scope", "", "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}
	return store, native, bd
}

// TestNewProxiedStoreRefusesAnUnadmittedPin is the second half of the gate P2-05
// built: Pin's fields are unexported and only Admit sets admitted, so the split
// store cannot be constructed around a pin nobody checked.
func TestNewProxiedStoreRefusesAnUnadmittedPin(t *testing.T) {
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	if _, err := NewProxiedStore(native, NewMemStore(), Pin{}); err == nil {
		t.Fatal("NewProxiedStore accepted the zero Pin; the admission gate is bypassable")
	}
	if _, err := NewProxiedStore(nil, NewMemStore(), PinForTest("/scope", "", "beads")); err == nil {
		t.Fatal("NewProxiedStore accepted a nil native leaf")
	}
	if _, err := NewProxiedStore(native, nil, PinForTest("/scope", "", "beads")); err == nil {
		t.Fatal("NewProxiedStore accepted a nil bd leaf")
	}
}

// TestProxiedStoreReadsNativeWritesBd is the routing claim itself, asserted on
// WHICH LEAF answered rather than on the answer: both leaves see the same ledger
// in this fixture, so every value assertion would pass with the routing reversed.
func TestProxiedStoreReadsNativeWritesBd(t *testing.T) {
	store, native, bd := newSplitFixture(t)

	created, err := store.Create(Bead{Title: "written through bd", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := bd.took(); len(got) != 1 || got[0] != "Create" {
		t.Fatalf("bd leaf calls after Create = %v, want [Create]", got)
	}

	for _, read := range []struct {
		name string
		call func() error
	}{
		{"Get", func() error { _, err := store.Get(created.ID); return err }},
		{"List", func() error { _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth}); return err }},
		{"Ready", func() error { _, err := store.Ready(); return err }},
		{"DepList", func() error { _, err := store.DepList(created.ID, "down"); return err }},
	} {
		if err := read.call(); err != nil {
			t.Fatalf("%s: %v", read.name, err)
		}
		if got := bd.took(); len(got) != 0 {
			t.Errorf("%s reached the bd leaf (%v); every read belongs to the native leaf", read.name, got)
		}
	}

	// And the mutations keep going to bd even though the native leaf is right
	// there and technically capable — it is read-only latched, so a routing slip
	// would refuse rather than write, but the routing is what makes the refusal
	// unreachable.
	if err := store.Update(created.ID, UpdateOpts{Title: strPtr("retitled")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := store.SetMetadata(created.ID, "k", "v"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := store.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := bd.took(); strings.Join(got, ",") != "Update,SetMetadata,Close" {
		t.Fatalf("bd leaf calls = %v, want [Update SetMetadata Close]", got)
	}
	if native.ReadOnly() != true {
		t.Fatal("the fixture's read leaf is not read-only latched, so a mis-routed write would succeed silently")
	}
}

func strPtr(s string) *string { return &s }

// TestProxiedStoreGetFallsBackToBdLeafForWispIDs is H1.
//
// NativeDoltStore.Get is a SearchIssues over the issues table with no
// IncludeEphemeral, so a wisp is invisible to it; BdStore.Get falls back to
// `bd query --ephemeral` for a bead-shaped id. A proxied scope reads through
// BdStore TODAY, so losing the wisp would be a regression on this lane rather
// than parity inherited from the direct one.
func TestProxiedStoreGetFallsBackToBdLeafForWispIDs(t *testing.T) {
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithProxiedReadOnly())
	wisp := Bead{ID: "prx-wisp-0001", Title: "auto-handoff mail", Type: "task"}
	bd := &recordingLeaf{Store: &wispOnlyStore{wisp: wisp}}
	store, err := NewProxiedStore(native, bd, PinForTest("/scope", "", "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}

	got, err := store.Get(wisp.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v — the native leaf cannot see the wisps table, so the miss must fall through", wisp.ID, err)
	}
	if got.ID != wisp.ID {
		t.Fatalf("Get.ID = %q, want %q", got.ID, wisp.ID)
	}
	if calls := bd.took(); len(calls) != 1 || calls[0] != "Get" {
		t.Fatalf("bd leaf calls = %v, want exactly one Get — the fallback costs ONE fork, and only on a miss", calls)
	}

	t.Run("a hit never reaches the bd leaf", func(t *testing.T) {
		// Seed the ISSUES table the native leaf can see, through a write leaf
		// pointed at the same storage.
		seeded, err := newNativeDoltStoreForTest(native.storage.(*nativeDoltMemStorage)).Create(Bead{Title: "in the issues table", Type: "task"})
		if err != nil {
			t.Fatalf("seeding the native ledger: %v", err)
		}
		if _, err := store.Get(seeded.ID); err != nil {
			t.Fatalf("Get(%s): %v", seeded.ID, err)
		}
		if calls := bd.took(); len(calls) != 0 {
			t.Fatalf("a native HIT reached the bd leaf (%v); the fallback must be gated on the miss", calls)
		}
	})

	t.Run("a non-bead name never buys a supplemental query", func(t *testing.T) {
		// BdStore gates its own wisp query on isWispQueryableID for this reason:
		// callers pass session names ("rig/agent.name") through Get, and those
		// must not leak into a supplemental lookup.
		if _, err := store.Get("rig/agent.name"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(session name) = %v, want ErrNotFound", err)
		}
		if calls := bd.took(); len(calls) != 0 {
			t.Fatalf("a non-bead name reached the bd leaf (%v)", calls)
		}
	})
}

// wispOnlyStore is a bd leaf that holds exactly one wisp-tier bead, standing in
// for the `bd query --ephemeral` fallback BdStore.Get performs.
type wispOnlyStore struct {
	Store
	wisp Bead
}

func (s *wispOnlyStore) Get(id string) (Bead, error) {
	if id == s.wisp.ID {
		return s.wisp, nil
	}
	return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
}

// TestProxiedStoreLocalStringsSetThenGetAcrossLeaves is H2.
//
// localSidecar.ensureLoadedLocked latches loaded=true on first use and never
// re-reads, so two instances over one file do not merely duplicate a read — they
// diverge permanently. BdStore and NativeDoltStore each construct their own over
// <scope>/.beads/local-strings.json, so without the wrapper's surgery a
// SetLocalString through one leaf would be invisible through the other for the
// life of the process.
func TestProxiedStoreLocalStringsSetThenGetAcrossLeaves(t *testing.T) {
	scope := t.TempDir()
	sidecarPath := filepath.Join(scope, ".beads", "local-strings.json")

	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithProxiedReadOnly())
	native.localStrings = newLocalSidecar(sidecarPath)
	bdLeaf := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	bdLeaf.localStrings = newLocalSidecar(sidecarPath)

	// Force BOTH sidecars to latch before anything is written, which is what makes
	// this a real regression test rather than a lucky ordering: after this, a
	// second instance can never learn about the first's writes by re-reading.
	if _, err := native.GetLocalString("prx-1", "synced_at"); err != nil {
		t.Fatalf("priming the native sidecar: %v", err)
	}
	if _, err := bdLeaf.GetLocalString("prx-1", "synced_at"); err != nil {
		t.Fatalf("priming the bd sidecar: %v", err)
	}

	store, err := NewProxiedStore(native, bdLeaf, PinForTest(scope, "", "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}

	if err := store.SetLocalString("prx-1", "synced_at", "2026-09-22T00:00:00Z"); err != nil {
		t.Fatalf("SetLocalString: %v", err)
	}
	got, err := store.GetLocalString("prx-1", "synced_at")
	if err != nil {
		t.Fatalf("GetLocalString: %v", err)
	}
	if got != "2026-09-22T00:00:00Z" {
		t.Fatalf("GetLocalString = %q, want the value just written", got)
	}

	// The load-bearing assertion: the NATIVE leaf, asked directly, sees it too.
	// That can only be true if the two leaves hold the same object.
	direct, err := native.GetLocalString("prx-1", "synced_at")
	if err != nil {
		t.Fatalf("native GetLocalString: %v", err)
	}
	if direct != "2026-09-22T00:00:00Z" {
		t.Fatalf("the native leaf reads %q through its own sidecar; the two leaves are still two objects", direct)
	}
	if native.localStrings != bdLeaf.localStrings {
		t.Fatal("the leaves hold different *localSidecar values")
	}
}

// TestProxiedStoreRelocatedClassReadsRefuseLikeBdStore is H5.
//
// A relocated coordination class is served from a store the bd ledger's metadata
// never mentions, so a bd-ledger read scoped to one of its ids runs successfully,
// matches nothing, and returns an empty answer that is indistinguishable from a
// true negative. BdStore refuses instead (guardRelocatedClassIDs); native_dolt_
// store.go has no equivalent, so the split would turn that typed refusal into a
// silent not-found.
func TestProxiedStoreRelocatedClassReadsRefuseLikeBdStore(t *testing.T) {
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithProxiedReadOnly())
	var forks int
	countingRunner := func(string, string, ...string) ([]byte, error) {
		forks++
		return nil, errors.New("bd: no issue found matching the id")
	}
	bd := NewBdStoreWithPrefix(t.TempDir(), countingRunner, "prx",
		WithBdStoreRelocatedClasses(RelocatedClass{Class: "graph", IDPrefix: "gcg", Location: "the infra binding"}))

	store, err := NewProxiedStore(native, bd, PinForTest("/scope", "", "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}

	for _, read := range []struct {
		name string
		call func() error
	}{
		{"Get", func() error { _, err := store.Get("gcg-1234"); return err }},
		{"DepList", func() error { _, err := store.DepList("gcg-1234", "down"); return err }},
		{"DepMetadata", func() error { _, _, err := store.DepMetadata("gcg-1234", "prx-1"); return err }},
	} {
		err := read.call()
		if !errors.Is(err, ErrBdSQLClassRelocated) {
			t.Errorf("%s(gcg-1234) = %v, want the relocated-class refusal — a silent not-found here reads as a true negative", read.name, err)
		}
	}
	if forks != 0 {
		t.Errorf("the guard let %d bd fork(s) through; it must refuse before anything is spent", forks)
	}

	t.Run("an id this ledger does serve is untouched", func(t *testing.T) {
		// A plain miss, not a refusal — and H1's wisp fallback is what spends the
		// one bd fork here, which is the documented cost of a miss.
		if _, err := store.Get("prx-1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(prx-1) = %v, want a plain ErrNotFound; the guard must be narrow", err)
		}
	})
}

// TestProxiedStoreAtomicTxFollowsWriteLeaf is H3.
//
// NativeDoltStore.AtomicTx is true; BdStore stages and implements no
// AtomicTxStore at all. Tx runs on the bd leaf, so answering with the native
// leaf's true would promise callers a rollback the split cannot perform — and the
// Store.Tx contract says a caller who needs one either requires such a store or
// sequences its writes to stay recoverable, so the lie would be acted on.
func TestProxiedStoreAtomicTxFollowsWriteLeaf(t *testing.T) {
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithProxiedReadOnly())
	if !native.AtomicTx() {
		t.Fatal("the fixture's native leaf does not claim atomic Tx, so this test proves nothing")
	}
	bd := NewBdStoreWithPrefix(t.TempDir(), func(string, string, ...string) ([]byte, error) {
		return nil, errors.New("no bd here")
	}, "prx")
	if _, ok := Store(bd).(AtomicTxStore); ok {
		t.Fatal("BdStore now claims AtomicTxStore; this hazard's premise changed")
	}

	store, err := NewProxiedStore(native, bd, PinForTest("/scope", "", "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}
	if store.AtomicTx() {
		t.Fatal("AtomicTx reported true while Tx runs on a staged, non-atomic leaf")
	}
	if StoreSupportsAtomicTx(store) {
		t.Fatal("StoreSupportsAtomicTx(wrapper) is true; a caller needing all-or-nothing would act on it")
	}
}

// TestProxiedStoreCapabilityParityAndNoGraphApplyClaim is H4 plus the whole
// discovery surface.
//
// The negative half is the constraint: *NativeDoltStore declares
// StorageGraphApplyStore and EphemeralGraphApplyStore, and cmd/gc's
// wrapStoreWithBeadPolicies resolves beads.GraphApplyFor(store) at WRAP time and
// keeps the answer — so a claim on the wrapper would route every policy-selected
// ephemeral graph create into a native WRITE on a database bd owns.
func TestProxiedStoreCapabilityParityAndNoGraphApplyClaim(t *testing.T) {
	store, _, _ := newSplitFixture(t)

	t.Run("does not claim graph-apply", func(t *testing.T) {
		if _, ok := Store(store).(GraphApplyStore); ok {
			t.Error("the wrapper claims GraphApplyStore")
		}
		if _, ok := Store(store).(StorageGraphApplyStore); ok {
			t.Error("the wrapper claims StorageGraphApplyStore; wrapStoreWithBeadPolicies would route ephemeral graph creates native")
		}
		if _, ok := Store(store).(EphemeralGraphApplyStore); ok {
			t.Error("the wrapper claims EphemeralGraphApplyStore")
		}
	})

	t.Run("hands out the write leaf's applier", func(t *testing.T) {
		// The fixture's write leaf is graph-apply capable, so GraphApplyFor must
		// resolve through the handle provider rather than fail.
		applier, ok := GraphApplyFor(store)
		if !ok {
			t.Fatal("GraphApplyFor(wrapper) found nothing; policy-selected graph creates would lose their applier")
		}
		if applier.(Store) == Store(store) {
			t.Fatal("GraphApplyFor resolved to the wrapper itself")
		}
		writeApplier, ok := GraphApplyFor(store.writeLeaf())
		if !ok || applier != writeApplier {
			t.Fatal("GraphApplyFor did not resolve to the WRITE leaf's applier")
		}
		if _, isNative := applier.(*NativeDoltStore); isNative && applier == GraphApplyStore(store.nativeLeaf()) {
			t.Fatal("GraphApplyFor resolved to the READ leaf; a graph create would be a native write")
		}
	})

	t.Run("does not claim the dependency-snapshot shortcut", func(t *testing.T) {
		// CachingStore.fetchDepsForBeads tries cacheDependencySnapshotStore FIRST
		// and listDependencyCompletenessStore second. Neither leaf implements the
		// snapshot, so claiming it would preempt the completeness answer below and
		// fabricate an empty dependency snapshot.
		if _, ok := Store(store).(cacheDependencySnapshotStore); ok {
			t.Error("the wrapper claims dependencySnapshotForCache with nothing to forward to")
		}
		if !store.listIncludesCompleteDependencies() {
			t.Error("the completeness answer did not follow the native read leaf, which answers true")
		}
	})

	t.Run("forwards the discovery capabilities production asserts", func(t *testing.T) {
		for name, ok := range map[string]bool{
			"Counter":                          asserted[Counter](store),
			"BatchDeleter":                     asserted[BatchDeleter](store),
			"ForeignIDCreator":                 asserted[ForeignIDCreator](store),
			"StorageCreateStore":               asserted[StorageCreateStore](store),
			"ConditionalAssignmentReleaser":    asserted[ConditionalAssignmentReleaser](store),
			"ConditionalWriterHandleProvider":  asserted[ConditionalWriterHandleProvider](store),
			"ConditionalWritesResolveTargeter": asserted[ConditionalWritesResolveTargeter](store),
			"DepMetadataReader":                asserted[DepMetadataReader](store),
			"ParentProjectionWaiter":           asserted[ParentProjectionWaiter](store),
			"RowWitness":                       asserted[RowWitness](store),
			"AtomicTxStore":                    asserted[AtomicTxStore](store),
			"GraphApplyHandleProvider":         asserted[GraphApplyHandleProvider](store),
			"listDependencyCompletenessStore":  asserted[listDependencyCompletenessStore](store),
			"readyProjectionEnrichmentStore":   asserted[readyProjectionEnrichmentStore](store),
			"conditionalWritesModeCarrier":     asserted[conditionalWritesModeCarrier](store),
		} {
			if !ok {
				t.Errorf("the wrapper does not satisfy %s; production discovers it by direct type assertion", name)
			}
		}
	})

	t.Run("does not claim what neither leaf can serve", func(t *testing.T) {
		if asserted[ContextReadyReader](store) {
			t.Error("the wrapper claims ReadyContext; neither BdStore nor NativeDoltStore implements it")
		}
	})

	t.Run("resolves conditional writes on the write leaf", func(t *testing.T) {
		if target := store.ConditionalWritesResolveTarget(); target != store.writeLeaf() {
			t.Errorf("resolve target = %T, want the write leaf", target)
		}
		if !store.stampConditionalWritesMode(gate.Require, false) {
			t.Fatal("the conditional-writes stamp did not land on the write leaf")
		}
		mode, defaulted := store.conditionalWritesMode()
		if mode != gate.Require || defaulted {
			t.Errorf("stamped mode = %s (defaulted %v), want require", mode, defaulted)
		}
		// Both leaves carry it: PR3 moves writes onto the native leaf, and a leaf
		// that was never stamped would resolve unset -> legacy exactly when
		// enforcement was supposed to start.
		if nativeMode, _ := store.nativeLeaf().conditionalWritesMode(); nativeMode != gate.Require {
			t.Errorf("native leaf mode = %s, want the stamp to reach both leaves", nativeMode)
		}
	})

	t.Run("Backing is the read leaf", func(t *testing.T) {
		if store.Backing() != Store(store.nativeLeaf()) { //nolint:staticcheck // comparing interface identity is the point
			t.Error("Backing is not the read leaf; ReadyLive would send every live readiness read through bd")
		}
	})
}

// CountIssues gives the in-memory native storage fixture the one method
// NativeDoltStore.Count reaches. It counts the whole ledger, mirroring
// SearchIssues' own approximation (that method also ignores the filter and lets
// the store narrow in Go), which is enough for a routing assertion and is not a
// claim about filter parity.
func (s *nativeDoltMemStorage) CountIssues(_ context.Context, _ string, _ beadslib.IssueFilter) (int64, error) {
	beads, err := s.store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		return 0, err
	}
	return int64(len(beads)), nil
}

// asserted reports whether store satisfies T, as a type assertion written once.
func asserted[T any](store Store) bool {
	_, ok := store.(T)
	return ok
}

// TestProxiedStoreDemoteIsOneWay pins the terminality rule.
//
// A TERMINAL verdict is a fact about the database, the record or the policy, so
// re-pinning could only re-learn it and the latch is permanent for this handle.
// A NON-terminal stand-down is the endpoint in motion, and the design's guard
// tick answers a generation change by RE-PINNING rather than demoting (U22) — so
// latching those permanently would convert every ordinary bd proxy restart into a
// store that forks for the rest of the process.
func TestProxiedStoreDemoteIsOneWay(t *testing.T) {
	t.Run("a terminal verdict is permanent", func(t *testing.T) {
		store, _, bd := newSplitFixture(t)
		store.standDown(NewSchemaSkewVerdictError(ProxiedSkewLaneMain, ProxiedSkewDirAhead, "database moved"))
		if !store.Demoted() {
			t.Fatal("standDown left the store native")
		}
		if verdict := store.Verdict(); verdict == nil || verdict.Verdict != ProxiedVerdictSchemaSkew {
			t.Fatalf("verdict = %v, want schema_skew", verdict)
		}
		if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth}); err != nil {
			t.Fatalf("a demoted store must keep serving from bd: %v", err)
		}
		if got := bd.took(); len(got) != 1 || got[0] != "List" {
			t.Fatalf("bd leaf calls after demotion = %v, want [List]", got)
		}
		replacement := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		if store.repin(replacement, PinForTest("/scope", "", "beads")) {
			t.Fatal("repin succeeded after a TERMINAL stand-down; demotion is not one-way")
		}
		if !store.Demoted() {
			t.Fatal("the store promoted itself after a terminal verdict")
		}
	})

	t.Run("a non-terminal stand-down can be re-pinned", func(t *testing.T) {
		store, _, _ := newSplitFixture(t)
		store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))
		if !store.Demoted() {
			t.Fatal("standDown left the store native")
		}
		replacement := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		if !store.repin(replacement, PinForTest("/scope", "", "beads")) {
			t.Fatal("repin refused after a NON-terminal stand-down; the guard tick could never re-pin")
		}
		if store.Demoted() {
			t.Fatal("repin did not install the replacement")
		}
		if store.Verdict() != nil {
			t.Fatal("repin left the stale verdict in place")
		}
	})

	t.Run("a read that returns a verdict stands the store down", func(t *testing.T) {
		native := newNativeDoltStoreForTest(deadSearchStorage(errors.New("begin read tx: Error 1049 (42000): Unknown database 'beads'")))
		native.proxiedReadVerdicts = true
		bd := &recordingLeaf{Store: NewMemStore()}
		store, err := NewProxiedStore(native, bd, PinForTest("/scope", "", "beads"))
		if err != nil {
			t.Fatalf("NewProxiedStore: %v", err)
		}
		if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth}); err == nil {
			t.Fatal("the failing read reported success")
		}
		if !store.Demoted() {
			t.Fatal("a database_gone verdict did not stand the native leaf down")
		}
		// And the next read is served, from bd, without another failure.
		if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth}); err != nil {
			t.Fatalf("the demoted store did not serve from bd: %v", err)
		}
	})
}

// TestProxiedStoreMutationRepinsAcrossProxyRestart is H6.
//
// bd restarts a stopped proxy — after an operator `bd dolt stop`, after an idle
// expiry — as a NEW generation on a new port, and the write that triggered the
// restart succeeds while the native pool keeps pointing at the old one. Every
// later read would then be served by a socket nobody validated.
func TestProxiedStoreMutationRepinsAcrossProxyRestart(t *testing.T) {
	root := t.TempDir()
	writeRecord := func(t *testing.T, pid, port int, birth string) {
		t.Helper()
		rootID, err := proxyendpoint.RootID(root)
		if err != nil {
			t.Fatalf("RootID: %v", err)
		}
		body, err := json.Marshal(proxyendpoint.Record{
			PID: pid, Port: port, UpstreamID: "upstream", Schema: proxyendpoint.SchemaV2,
			Kind: proxyendpoint.RecordKind, Birth: birth, RootID: rootID, ControlPort: port + 1,
		})
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		if err := os.WriteFile(proxyendpoint.PIDPath(root), body, 0o600); err != nil {
			t.Fatalf("write record: %v", err)
		}
	}

	t.Run("a generation change across a write stands the native leaf down", func(t *testing.T) {
		writeRecord(t, 4001, 45123, proxyendpoint.BirthToken("boot", "111"))
		storage := &nativeDoltMemStorage{store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true}}
		native := newNativeDoltStoreForTest(storage, WithProxiedReadOnly())
		restarting := &restartingLeaf{Store: newNativeDoltStoreForTest(storage), onWrite: func() {
			// bd's child restarted the proxy while the write was in flight.
			writeRecord(t, 4002, 45987, proxyendpoint.BirthToken("boot", "222"))
		}}
		store, err := NewProxiedStore(native, restarting, PinForTest("/scope", root, "beads"))
		if err != nil {
			t.Fatalf("NewProxiedStore: %v", err)
		}

		if _, err := store.Create(Bead{Title: "the write that restarted the proxy", Type: "task"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if !store.Demoted() {
			t.Fatal("the native leaf kept serving a generation that no longer exists")
		}
		verdict := store.Verdict()
		if verdict == nil || verdict.Verdict != ProxiedVerdictProxyGone {
			t.Fatalf("verdict = %v, want proxy_gone", verdict)
		}
		if verdict.Terminal() {
			t.Error("a generation change must be NON-terminal: the guard tick re-pins rather than demoting")
		}
	})

	t.Run("a steady generation leaves the native leaf alone", func(t *testing.T) {
		writeRecord(t, 4001, 45123, proxyendpoint.BirthToken("boot", "111"))
		storage := &nativeDoltMemStorage{store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true}}
		native := newNativeDoltStoreForTest(storage, WithProxiedReadOnly())
		store, err := NewProxiedStore(native, newNativeDoltStoreForTest(storage), PinForTest("/scope", root, "beads"))
		if err != nil {
			t.Fatalf("NewProxiedStore: %v", err)
		}
		if _, err := store.Create(Bead{Title: "ordinary write", Type: "task"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if store.Demoted() {
			t.Fatal("an ordinary write demoted the store")
		}
	})

	t.Run("an unreadable record is not treated as a change", func(t *testing.T) {
		writeRecord(t, 4001, 45123, proxyendpoint.BirthToken("boot", "111"))
		storage := &nativeDoltMemStorage{store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true}}
		native := newNativeDoltStoreForTest(storage, WithProxiedReadOnly())
		removing := &restartingLeaf{Store: newNativeDoltStoreForTest(storage), onWrite: func() {
			// bd removes the record on an orderly stop and rewrites it on start,
			// so an absent record mid-write is as likely to be that window as a
			// move. Demoting on it would drop a healthy store for a file that
			// reappears a millisecond later.
			_ = os.Remove(proxyendpoint.PIDPath(root))
		}}
		store, err := NewProxiedStore(native, removing, PinForTest("/scope", root, "beads"))
		if err != nil {
			t.Fatalf("NewProxiedStore: %v", err)
		}
		if _, err := store.Create(Bead{Title: "write across a record rewrite", Type: "task"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if store.Demoted() {
			t.Fatal("an unreadable record demoted the store")
		}
	})
}

// restartingLeaf runs a side effect in the middle of every write, standing in for
// bd's child restarting the proxy as part of servicing the command.
type restartingLeaf struct {
	Store
	onWrite func()
}

func (r *restartingLeaf) Create(b Bead) (Bead, error) {
	created, err := r.Store.Create(b)
	r.onWrite()
	return created, err
}

// TestProxiedStoreCloseStoreReleasesBothLeaves pins that closing the wrapper
// closes the native pool rather than leaking a live Dolt connection for the
// process lifetime.
func TestProxiedStoreCloseStoreReleasesBothLeaves(t *testing.T) {
	store, native, _ := newSplitFixture(t)
	if err := store.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	if _, err := native.Get("prx-1"); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("the native leaf is still open after CloseStore: %v", err)
	}
	if !store.Demoted() {
		t.Fatal("CloseStore left the wrapper pointing at a closed native leaf")
	}
}

// TestProxiedStoreCountAndDepListBatchRouteToTheReadLeaf covers the two optional
// read capabilities whose answers differ by leaf: only the native leaf can Count,
// and only the bd leaf has a batched dep read.
func TestProxiedStoreCountAndDepListBatchRouteToTheReadLeaf(t *testing.T) {
	store, _, _ := newSplitFixture(t)
	if _, err := store.Create(Bead{Title: "counted", Type: "task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	n, err := store.Count(context.Background(), ListQuery{AllowScan: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("Count through the native read leaf: %v", err)
	}
	if n != 1 {
		t.Fatalf("Count = %d, want 1", n)
	}

	t.Run("DepListBatch falls back through the wrapper", func(t *testing.T) {
		batch, err := store.DepListBatch([]string{"prx-1"})
		if err != nil {
			t.Fatalf("DepListBatch: %v", err)
		}
		if _, ok := batch["prx-1"]; !ok {
			t.Fatalf("DepListBatch = %#v, want an entry per id", batch)
		}
	})

	t.Run("a demoted store reports Count unsupported", func(t *testing.T) {
		store.standDown(NewSchemaSkewVerdictError(ProxiedSkewLaneMain, ProxiedSkewDirAhead, "demoted"))
		if _, err := store.Count(context.Background(), ListQuery{AllowScan: true}); !errors.Is(err, ErrCountUnsupported) {
			t.Fatalf("Count on a demoted store = %v, want ErrCountUnsupported so callers fall back to List", err)
		}
	})
}

// conditionalRestartLeaf is a write leaf that carries the conditional-write
// capabilities AND restarts bd's proxy on every one of them, which is the shape
// H6 exists for: the bd child finds the proxy stopped and brings it back as a
// NEW generation on a new port while the write itself succeeds.
//
// It implements the methods explicitly rather than embedding a store that has
// them, because an embedded Store interface strips the concrete leaf's own
// methods — which is exactly why the capability HANDLES exist in the first
// place.
type conditionalRestartLeaf struct {
	Store
	onWrite func()
}

func (l *conditionalRestartLeaf) UpdateIfMatch(string, int64, UpdateOpts) error {
	l.onWrite()
	return nil
}
func (l *conditionalRestartLeaf) CloseIfMatch(string, int64) error  { l.onWrite(); return nil }
func (l *conditionalRestartLeaf) DeleteIfMatch(string, int64) error { l.onWrite(); return nil }

func (l *conditionalRestartLeaf) CompareAndSetMetadataKey(string, string, string, string) (bool, error) {
	l.onWrite()
	return true, nil
}

func (l *conditionalRestartLeaf) CloseWithMetadataIfMatch(string, int64, map[string]string) (Bead, error) {
	l.onWrite()
	return Bead{}, nil
}

// TestProxiedStoreBracketsTheConditionalWriteHandles is council B-F3.
//
// H6's bracket ran only for the mutations the wrapper implements as METHODS.
// Four capability families are exposed as HANDLES instead, and every one of
// them used to resolve to the bd leaf object, which the caller then drove
// directly: UpdateIfMatch, CloseIfMatch, DeleteIfMatch,
// CompareAndSetMetadataKey and CloseWithMetadataIfMatch never re-entered the
// wrapper. Their callers are real — internal/molecule, internal/storebinding —
// and a one-shot gc command has no guard tick either, so a CAS that restarted
// bd's proxy left the handle's pool on the previous generation with nothing
// left to notice.
//
// Each row asserts the DEMOTION, not the write: the write succeeds either way,
// and it is the bracket that is the subject.
func TestProxiedStoreBracketsTheConditionalWriteHandles(t *testing.T) {
	root := t.TempDir()
	writeRecord := func(t *testing.T, pid, port int, birth string) {
		t.Helper()
		rootID, err := proxyendpoint.RootID(root)
		if err != nil {
			t.Fatalf("RootID: %v", err)
		}
		body, err := json.Marshal(proxyendpoint.Record{
			PID: pid, Port: port, UpstreamID: "upstream", Schema: proxyendpoint.SchemaV2,
			Kind: proxyendpoint.RecordKind, Birth: birth, RootID: rootID, ControlPort: port + 1,
		})
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		if err := os.WriteFile(proxyendpoint.PIDPath(root), body, 0o600); err != nil {
			t.Fatalf("write record: %v", err)
		}
	}

	newSplit := func(t *testing.T, restart bool) *ProxiedStore {
		t.Helper()
		writeRecord(t, 4001, 45123, proxyendpoint.BirthToken("boot", "111"))
		storage := &nativeDoltMemStorage{store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true}}
		native := newNativeDoltStoreForTest(storage, WithProxiedReadOnly())
		leaf := &conditionalRestartLeaf{Store: newNativeDoltStoreForTest(storage), onWrite: func() {
			if restart {
				writeRecord(t, 4002, 45987, proxyendpoint.BirthToken("boot", "222"))
			}
		}}
		store, err := NewProxiedStore(native, leaf, PinForTest("/scope", root, "beads"))
		if err != nil {
			t.Fatalf("NewProxiedStore: %v", err)
		}
		return store
	}

	cases := []struct {
		name string
		call func(t *testing.T, store *ProxiedStore)
	}{
		{
			name: "UpdateIfMatch",
			call: func(t *testing.T, store *ProxiedStore) {
				writer, ok := ConditionalWriterFor(store)
				if !ok {
					t.Fatal("ConditionalWriterFor did not resolve through the wrapper")
				}
				if err := writer.UpdateIfMatch("gc-1", 1, UpdateOpts{}); err != nil {
					t.Fatalf("UpdateIfMatch: %v", err)
				}
			},
		},
		{
			name: "CloseIfMatch",
			call: func(t *testing.T, store *ProxiedStore) {
				writer, _ := ConditionalWriterFor(store)
				if err := writer.CloseIfMatch("gc-1", 1); err != nil {
					t.Fatalf("CloseIfMatch: %v", err)
				}
			},
		},
		{
			name: "DeleteIfMatch",
			call: func(t *testing.T, store *ProxiedStore) {
				writer, _ := ConditionalWriterFor(store)
				if err := writer.DeleteIfMatch("gc-1", 1); err != nil {
					t.Fatalf("DeleteIfMatch: %v", err)
				}
			},
		},
		{
			name: "CompareAndSetMetadataKey",
			call: func(t *testing.T, store *ProxiedStore) {
				writer, ok := store.MetadataCASWriterHandle()
				if !ok {
					t.Fatal("MetadataCASWriterHandle did not resolve")
				}
				if _, err := writer.CompareAndSetMetadataKey("gc-1", "k", "", "v"); err != nil {
					t.Fatalf("CompareAndSetMetadataKey: %v", err)
				}
			},
		},
		{
			name: "CloseWithMetadataIfMatch",
			call: func(t *testing.T, store *ProxiedStore) {
				closer, ok := store.AtomicConditionalCloserHandle()
				if !ok {
					t.Fatal("AtomicConditionalCloserHandle did not resolve")
				}
				if _, err := closer.CloseWithMetadataIfMatch("gc-1", 1, map[string]string{"k": "v"}); err != nil {
					t.Fatalf("CloseWithMetadataIfMatch: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+" across a proxy restart stands the native leaf down", func(t *testing.T) {
			store := newSplit(t, true)
			tc.call(t, store)
			if !store.Demoted() {
				t.Fatalf("%s restarted bd's proxy and the native leaf kept serving the previous "+
					"generation; nothing else would have noticed on a one-shot store", tc.name)
			}
			verdict := store.Verdict()
			if verdict == nil || verdict.Verdict != ProxiedVerdictProxyGone {
				t.Fatalf("verdict = %v, want proxy_gone", verdict)
			}
			if verdict.Terminal() {
				t.Error("a generation change must be NON-terminal: the guard tick re-pins rather than demoting")
			}
		})

		t.Run(tc.name+" on a steady generation leaves the native leaf alone", func(t *testing.T) {
			store := newSplit(t, false)
			tc.call(t, store)
			if store.Demoted() {
				t.Fatalf("%s demoted the store without a generation change; the bracket is firing on nothing", tc.name)
			}
		})
	}
}

// TestProxiedStoreConditionalResolveTargetIsTheDocumentedGap makes the OTHER
// half of council B-F3 executable rather than prose.
//
// The three handles above are bracketed, and beads.ConditionalWriterFor reaches
// them because it does not follow the resolve target. MetadataCASWriterFor,
// AtomicConditionalCloserFor and CachingStore.conditionalBacking() DO follow it
// — and ProxiedStore.ConditionalWritesResolveTarget has to keep answering "the
// bd leaf", because a wrapper that answered "me" would have to carry the
// capability prober, the state inspector and conditionalStoreKind's identity as
// well as the stamp surface it already carries: a hand-written capability leaf,
// which is the shape splittest/strict_store.go warns about and the reason this
// store is a wrapper at all.
//
// So this row pins the SCOPE of the bracket, not a wish. If a later change
// closes the gap this test fails, which is the point: the deviation stops being
// true and the comment that records it has to go with it.
func TestProxiedStoreConditionalResolveTargetIsTheDocumentedGap(t *testing.T) {
	storage := &nativeDoltMemStorage{store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true}}
	native := newNativeDoltStoreForTest(storage, WithProxiedReadOnly())
	bd := &conditionalRestartLeaf{Store: newNativeDoltStoreForTest(storage), onWrite: func() {}}
	store, err := NewProxiedStore(native, bd, PinForTest("/scope", "", "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}

	if got := store.ConditionalWritesResolveTarget(); got != Store(bd) {
		t.Fatalf("ConditionalWritesResolveTarget() = %T, want the bd leaf: CachingStore.conditionalBacking "+
			"follows it, and a target that cannot carry the stamp collapses the seam to unset->legacy", got)
	}
	// Both resolvers follow the target, so they reach the leaf rather than the
	// bracketing adapters. Asserted on the CONCRETE type: the adapters are
	// values of this package's own unexported types, so "is it the adapter" is
	// a question the test can ask directly.
	if writer, ok := MetadataCASWriterFor(store); ok {
		if _, bracketed := writer.(proxiedMetadataCASWriter); bracketed {
			t.Fatal("MetadataCASWriterFor now reaches the bracketing adapter; the gap recorded on " +
				"ConditionalWritesResolveTarget and in ProxiedStore's H6 note is closed, so both comments must go")
		}
	}
	if closer, ok := AtomicConditionalCloserFor(store); ok {
		if _, bracketed := closer.(proxiedAtomicConditionalCloser); bracketed {
			t.Fatal("AtomicConditionalCloserFor now reaches the bracketing adapter; the gap recorded on " +
				"ConditionalWritesResolveTarget and in ProxiedStore's H6 note is closed, so both comments must go")
		}
	}
	// The path that IS bracketed, so the two halves are asserted together and
	// neither can be read as the whole story.
	writer, ok := ConditionalWriterFor(store)
	if !ok {
		t.Fatal("ConditionalWriterFor did not resolve through the wrapper")
	}
	if _, bracketed := writer.(proxiedConditionalWriter); !bracketed {
		t.Fatalf("ConditionalWriterFor resolved to %T, want the bracketing adapter", writer)
	}
}

// TestStandDownKeepsTheTerminalReason is council A-F8.
//
// standDown wrote s.verdict unconditionally and only OR-ed the terminal flag,
// so a later NON-terminal stand-down overwrote the reason that had already
// decided the handle's fate. Both later paths are reachable: withMutation's
// generation bracket still runs after a demotion (it does not consult s.native)
// and checkGeneration's markPoolStale arm produces a non-terminal proxy_gone.
//
// The handle stays demoted either way, so this was never a resurrection. What
// it was is LiveProxiedDiagnostic and the doctor payload reporting
// "verdict=proxy_gone terminal=false" for a city that actually failed the
// schema gate — an operator told to wait for a proxy to come back, about a
// database whose schema moved.
func TestStandDownKeepsTheTerminalReason(t *testing.T) {
	store, _, _ := newSplitFixture(t)

	store.standDown(NewSchemaSkewVerdictError(ProxiedSkewLaneIgnored, ProxiedSkewDirBehind,
		"the pinned database moved while this handle was open"))
	store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone,
		"the proxy generation changed across create", nil))

	verdict := store.Verdict()
	if verdict == nil {
		t.Fatal("the store reports no verdict after two stand-downs")
	}
	if verdict.Verdict != ProxiedVerdictSchemaSkew {
		t.Fatalf("verdict = %q, want schema_skew: the terminal fact that demoted this handle was "+
			"overwritten, so doctor tells an operator to wait for a proxy that is not the problem",
			verdict.Verdict)
	}
	if !verdict.Terminal() {
		t.Error("the recorded verdict lost its terminality")
	}
	if !store.Demoted() {
		t.Error("the store promoted itself")
	}
}
