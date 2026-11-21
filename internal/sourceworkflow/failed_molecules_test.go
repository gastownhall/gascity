package sourceworkflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// strictCloseOrderStore rejects a CloseAll batch that closes a parent before
// an open child or a blocked bead before its open blocker, as a strict
// backing store does.
type strictCloseOrderStore struct {
	*beads.MemStore
}

func (s *strictCloseOrderStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closing := make(map[string]bool, len(ids))
	for _, id := range ids {
		kids, err := s.List(beads.ListQuery{ParentID: id, TierMode: beads.TierBoth})
		if err != nil {
			return 0, err
		}
		for _, kid := range kids {
			if kid.Status != "closed" && !closing[kid.ID] {
				return 0, fmt.Errorf("strict store: closing parent %s before open child %s", id, kid.ID)
			}
		}
		deps, err := s.DepList(id, "down")
		if err != nil {
			return 0, err
		}
		for _, d := range deps {
			if d.Type != "blocks" {
				continue
			}
			blocker, err := s.Get(d.DependsOnID)
			if err != nil {
				return 0, err
			}
			if blocker.Status != "closed" && !closing[blocker.ID] {
				return 0, fmt.Errorf("strict store: closing blocked %s before open blocker %s", id, blocker.ID)
			}
		}
		closing[id] = true
	}
	return s.MemStore.CloseAll(ids, metadata)
}

// failedMeta is metadata carrying the molecule_failed stamp.
func failedMeta() map[string]string {
	return map[string]string{beadmeta.MoleculeFailedMetadataKey: "true"}
}

// sweep runs one SweepFailedMolecules pass under budget over store, read
// through a federated leg and closed through a work-class target.
func sweep(store beads.Store, cutoff time.Time, budget FailedMoleculeSweepBudget) (FailedMoleculeSweep, error) {
	return SweepFailedMolecules(context.Background(), beadstest.FederatedLeg(store), beadstest.WorkMolecule(store), cutoff, budget)
}

// sweepAll runs one SweepFailedMolecules pass with no budget over store.
func sweepAll(store beads.Store, cutoff time.Time) (FailedMoleculeSweep, error) {
	return sweep(store, cutoff, FailedMoleculeSweepBudget{})
}

// failedWith is failedMeta plus the key/value pairs kv.
func failedWith(kv ...string) map[string]string {
	m := failedMeta()
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func mustCreate(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create(%q): %v", b.Title, err)
	}
	return created
}

func mustGet(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

// assertSwept fails unless every id is closed by the sweep and still stamped.
func assertSwept(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		b := mustGet(t, store, id)
		if b.Status != "closed" {
			t.Errorf("bead %s (%q) status = %q, want closed", id, b.Title, b.Status)
			continue
		}
		if got := b.Metadata["close_reason"]; got != FailedMoleculeSweepCloseReason {
			t.Errorf("bead %s close_reason = %q, want the sweep's", id, got)
		}
		if got := b.Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomeSkipped {
			t.Errorf("bead %s gc.outcome = %q, want skipped", id, got)
		}
		if b.Metadata[beadmeta.MoleculeFailedMetadataKey] != "true" {
			t.Errorf("bead %s lost molecule_failed on close", id)
		}
	}
}

// pastGrace is a cutoff every bead created so far is at or before.
func pastGrace() time.Time { return time.Now().Add(time.Hour) }

// AC-2: a failed v1 molecule has no gc.kind=workflow root and its steps link
// to it by ParentID alone; the sweep closes all of them in one ordered batch a
// strict store accepts.
func TestSweepFailedMoleculesClosesV1ParentLinkedChain(t *testing.T) {
	store := &strictCloseOrderStore{MemStore: beads.NewMemStore()}
	root := mustCreate(t, store, beads.Bead{Title: "v1 molecule", Type: "molecule", Metadata: failedMeta()})
	step1 := mustCreate(t, store, beads.Bead{Title: "step 1", ParentID: root.ID, Metadata: failedMeta()})
	step2 := mustCreate(t, store, beads.Bead{Title: "step 2", ParentID: root.ID, Metadata: failedMeta()})
	nested := mustCreate(t, store, beads.Bead{Title: "step 2.1", ParentID: step2.ID, Metadata: failedMeta()})
	if err := store.DepAdd(step2.ID, step1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	got, err := sweepAll(store, pastGrace())
	if err != nil {
		t.Fatalf("SweepFailedMolecules: %v", err)
	}
	if len(got.Chains) != 1 || got.Chains[0] != root.ID || got.Closed != 4 {
		t.Fatalf("sweep = %+v, want one chain %s with 4 beads closed", got, root.ID)
	}
	assertSwept(t, store, root.ID, step1.ID, step2.ID, nested.ID)
}

// assertOpen fails unless every id is still open.
func assertOpen(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if b := mustGet(t, store, id); b.Status != "open" {
			t.Errorf("bead %s (%q) status = %q, want open", id, b.Title, b.Status)
		}
	}
}

// AC-3: a failure stamped exactly at cutoff is past grace and one a
// nanosecond later is not; a zero UpdatedAt falls back to CreatedAt. A bead
// that did not fail, a controller-owned row and a chain whose root is
// stamped gc.gc_exempt=true are never touched.
func TestSweepFailedMoleculesHonorsGraceAndExemptions(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	long := cutoff.Add(-time.Hour)
	failed := map[string]string{beadmeta.MoleculeFailedMetadataKey: "true"}
	exempt := map[string]string{beadmeta.MoleculeFailedMetadataKey: "true", beadmeta.GCExemptMetadataKey: "true"}
	seed := []beads.Bead{
		{ID: "at-cutoff", Title: "stamped at cutoff", Status: "open", Metadata: failed, CreatedAt: long, UpdatedAt: cutoff},
		{ID: "inside", Title: "stamped 1ns after cutoff", Status: "open", Metadata: failed, CreatedAt: long, UpdatedAt: cutoff.Add(time.Nanosecond)},
		{ID: "created-past", Title: "no update stamp, created before cutoff", Status: "open", Metadata: failed, CreatedAt: cutoff.Add(-time.Minute).Add(time.Second)},
		{ID: "created-inside", Title: "no update stamp, created after cutoff", Status: "open", Metadata: failed, CreatedAt: cutoff.Add(time.Minute)},
		{ID: "not-failed", Title: "open bead that did not fail", Status: "open", CreatedAt: long.Add(time.Second), UpdatedAt: long},
		{ID: "session", Title: "controller-owned row", Type: "session", Status: "open", Metadata: failed, CreatedAt: long.Add(2 * time.Second), UpdatedAt: long},
		{ID: "exempt", Title: "exempt failed root", Status: "open", Metadata: exempt, CreatedAt: long.Add(3 * time.Second), UpdatedAt: long},
		{ID: "exempt-step", Title: "step of the exempt root", ParentID: "exempt", Status: "open", Metadata: failed, CreatedAt: long.Add(4 * time.Second), UpdatedAt: long},
	}
	store := beads.NewMemStoreFrom(0, seed, nil)

	got, err := sweepAll(store, cutoff)
	if err != nil {
		t.Fatalf("SweepFailedMolecules: %v", err)
	}
	if len(got.Chains) != 2 || got.Chains[0] != "at-cutoff" || got.Chains[1] != "created-past" || got.Closed != 2 {
		t.Fatalf("sweep = %+v, want chains [at-cutoff created-past], oldest first, with 2 beads closed", got)
	}
	assertSwept(t, store, "at-cutoff", "created-past")
	assertOpen(t, store, "inside", "created-inside", "not-failed", "session", "exempt", "exempt-step")
}

// AC-4: a failed sub-DAG whose gc.root_bead_id names an open workflow that
// did not fail belongs to that workflow and stays open; once that owner
// closes, its failed members are orphaned and swept, healthy members left
// alone. A failed member whose owner is gone is orphaned too.
func TestSweepFailedMoleculesResolvesOwners(t *testing.T) {
	store := &strictCloseOrderStore{MemStore: beads.NewMemStore()}
	owner := mustCreate(t, store, beads.Bead{Title: "live workflow", Metadata: map[string]string{beadmeta.KindMetadataKey: "workflow"}})
	ownedBy := func(kv ...string) map[string]string {
		m := map[string]string{beadmeta.RootBeadIDMetadataKey: owner.ID}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	healthy := mustCreate(t, store, beads.Bead{Title: "healthy step", ParentID: owner.ID, Metadata: ownedBy()})
	attach := mustCreate(t, store, beads.Bead{Title: "failed attach", ParentID: owner.ID, Metadata: ownedBy(beadmeta.MoleculeFailedMetadataKey, "true")})
	attachStep := mustCreate(t, store, beads.Bead{Title: "failed attach step", ParentID: attach.ID, Metadata: ownedBy(beadmeta.MoleculeFailedMetadataKey, "true")})
	stray := mustCreate(t, store, beads.Bead{Title: "member of a gone owner", Metadata: map[string]string{
		beadmeta.MoleculeFailedMetadataKey: "true",
		beadmeta.RootBeadIDMetadataKey:     "gc-gone",
	}})

	got, err := sweepAll(store, pastGrace())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(got.Chains) != 1 || got.Chains[0] != "gc-gone" || got.Closed != 1 {
		t.Fatalf("first sweep = %+v, want only the gone owner's member closed", got)
	}
	assertSwept(t, store, stray.ID)
	assertOpen(t, store, owner.ID, healthy.ID, attach.ID, attachStep.ID)

	if err := store.Close(owner.ID); err != nil {
		t.Fatalf("Close(owner): %v", err)
	}
	got, err = sweepAll(store, pastGrace())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(got.Chains) != 1 || got.Chains[0] != owner.ID || got.Closed != 2 {
		t.Fatalf("second sweep = %+v, want the closed owner's 2 failed members closed", got)
	}
	assertSwept(t, store, attach.ID, attachStep.ID)
	assertOpen(t, store, healthy.ID)
}

// AC-4: a failed member whose owner also failed but is still inside the grace
// window waits for its owner.
func TestSweepFailedMoleculesWaitsForOwnerInsideGrace(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{
			ID: "root", Title: "failed root inside grace", Status: "open", CreatedAt: cutoff.Add(-time.Hour), UpdatedAt: cutoff.Add(time.Minute),
			Metadata: map[string]string{beadmeta.MoleculeFailedMetadataKey: "true"},
		},
		{
			ID: "member", Title: "member past grace", Status: "open", CreatedAt: cutoff.Add(-time.Hour), UpdatedAt: cutoff.Add(-time.Hour),
			Metadata: map[string]string{beadmeta.MoleculeFailedMetadataKey: "true", beadmeta.RootBeadIDMetadataKey: "root"},
		},
	}, nil)

	got, err := sweepAll(store, cutoff)
	if err != nil {
		t.Fatalf("SweepFailedMolecules: %v", err)
	}
	if len(got.Chains) != 0 || got.Closed != 0 {
		t.Fatalf("sweep = %+v, want nothing closed while the owner is inside grace", got)
	}
	assertOpen(t, store, "root", "member")
}

// AC-7: with more sweepable chains than the cap, behind older failed sub-DAGs
// a live owner holds open, each sweep closes at most the cap, oldest first,
// the held sub-DAGs neither close nor use up the cap, and consecutive sweeps
// close every sweepable chain.
func TestSweepFailedMoleculesCapsChainsPerPassOldestFirst(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return cutoff.Add(-time.Hour).Add(time.Duration(i) * time.Second) }
	seed := []beads.Bead{{ID: "live", Title: "live owner that did not fail", Status: "open", CreatedAt: at(0), UpdatedAt: at(0)}}
	for i := 1; i <= 3; i++ {
		seed = append(seed, beads.Bead{
			ID: fmt.Sprintf("held-%d", i), Title: "failed sub-DAG of the live owner", Status: "open", CreatedAt: at(i), UpdatedAt: at(i),
			Metadata: map[string]string{beadmeta.MoleculeFailedMetadataKey: "true", beadmeta.RootBeadIDMetadataKey: "live"},
		})
	}
	var roots []string
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("root-%d", i)
		roots = append(roots, id)
		seed = append(seed, beads.Bead{ID: id, Title: "failed root", Status: "open", CreatedAt: at(10 + i), UpdatedAt: at(10 + i), Metadata: failedMeta()})
	}
	store := beads.NewMemStoreFrom(0, seed, nil)

	for _, want := range []struct {
		chains []string
		more   bool
	}{
		{roots[0:2], true},
		{roots[2:4], true},
		{roots[4:5], false},
		{nil, false},
	} {
		got, err := sweep(store, cutoff, FailedMoleculeSweepBudget{MaxChains: 2})
		if err != nil {
			t.Fatalf("SweepFailedMolecules: %v", err)
		}
		if fmt.Sprint(got.Chains) != fmt.Sprint(want.chains) || got.More != want.more || got.Closed != len(want.chains) {
			t.Fatalf("sweep = %+v, want chains %v with More=%v", got, want.chains, want.more)
		}
	}
	assertSwept(t, store, roots...)
	assertOpen(t, store, "live", "held-1", "held-2", "held-3")
}

// failCloseForStore fails every close batch that holds failFor and behaves
// as the inner store otherwise.
type failCloseForStore struct {
	*beads.MemStore
	failFor string
}

func (s *failCloseForStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	for _, id := range ids {
		if id == s.failFor {
			return 0, fmt.Errorf("injected close failure for %s", id)
		}
	}
	return s.MemStore.CloseAll(ids, metadata)
}

// AC-8: one chain's close failing does not stop the pass: the other chains
// still close, the joined error names the faulting chain's owner, and the
// next pass after the fault clears closes it.
func TestSweepFailedMoleculesContinuesPastAFailingChain(t *testing.T) {
	store := &failCloseForStore{MemStore: beads.NewMemStore()}
	faulting := mustCreate(t, store, beads.Bead{Title: "faulting chain", Type: "molecule", Metadata: failedMeta()})
	faultingStep := mustCreate(t, store, beads.Bead{Title: "faulting step", ParentID: faulting.ID, Metadata: failedMeta()})
	healthy := mustCreate(t, store, beads.Bead{Title: "healthy chain", Type: "molecule", Metadata: failedMeta()})
	healthyStep := mustCreate(t, store, beads.Bead{Title: "healthy step", ParentID: healthy.ID, Metadata: failedMeta()})
	store.failFor = faulting.ID

	got, err := sweepAll(store, pastGrace())
	if err == nil || !strings.Contains(err.Error(), "closing failed molecule "+faulting.ID) {
		t.Fatalf("sweep error = %v, want it to name the faulting owner %s", err, faulting.ID)
	}
	if fmt.Sprint(got.Chains) != fmt.Sprint([]string{healthy.ID}) || fmt.Sprint(got.ChainClosed) != "[2]" || got.Closed != 2 {
		t.Fatalf("sweep = %+v, want only the healthy chain %s closed with 2 beads", got, healthy.ID)
	}
	assertSwept(t, store, healthy.ID, healthyStep.ID)
	assertOpen(t, store, faulting.ID, faultingStep.ID)

	store.failFor = ""
	next, err := sweepAll(store, pastGrace())
	if err != nil {
		t.Fatalf("sweep after the fault cleared: %v", err)
	}
	if fmt.Sprint(next.Chains) != fmt.Sprint([]string{faulting.ID}) || next.Closed != 2 {
		t.Fatalf("sweep after the fault cleared = %+v, want the faulting chain %s closed", next, faulting.ID)
	}
	assertSwept(t, store, faulting.ID, faultingStep.ID)
}

// AC-2, AC-3, AC-4: a v1 step names its root by ParentID alone, and it belongs
// to that root even when the root is not swept this pass. An open root inside
// grace, or one stamped gc.gc_exempt in any spelling the wisp GC reads as true,
// keeps its steps open; a closed root keeps them only while it is exempt, and
// otherwise releases them to the sweep under the root's ID.
func TestSweepFailedMoleculesV1StepsFollowTheirRoot(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	past := cutoff.Add(-time.Hour)
	for _, tc := range []struct {
		name  string
		root  beads.Bead
		swept bool
	}{
		{"open root inside grace", beads.Bead{Status: "open", UpdatedAt: cutoff.Add(time.Minute), Metadata: failedMeta()}, false},
		{"open root exempt as on", beads.Bead{Status: "open", UpdatedAt: past, Metadata: failedWith(beadmeta.GCExemptMetadataKey, "on")}, false},
		{"closed root exempt", beads.Bead{Status: "closed", UpdatedAt: past, Metadata: failedWith(beadmeta.GCExemptMetadataKey, "true")}, false},
		{"closed root exempt as 1", beads.Bead{Status: "closed", UpdatedAt: past, Metadata: failedWith(beadmeta.GCExemptMetadataKey, "1")}, false},
		{"closed root not exempt", beads.Bead{Status: "closed", UpdatedAt: past, Metadata: failedMeta()}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tc.root
			root.ID, root.Title, root.CreatedAt = "root", "v1 root", past
			store := beads.NewMemStoreFrom(0, []beads.Bead{
				root,
				{ID: "step", Title: "v1 step", ParentID: "root", Status: "open", CreatedAt: past, UpdatedAt: past, Metadata: failedMeta()},
				{ID: "nested", Title: "v1 nested step", ParentID: "step", Status: "open", CreatedAt: past, UpdatedAt: past, Metadata: failedMeta()},
			}, nil)

			got, err := sweepAll(store, cutoff)
			if err != nil {
				t.Fatalf("SweepFailedMolecules: %v", err)
			}
			if !tc.swept {
				if len(got.Chains) != 0 || got.Closed != 0 {
					t.Fatalf("sweep = %+v, want nothing closed while the root holds its steps", got)
				}
				assertOpen(t, store, "step", "nested")
				return
			}
			if fmt.Sprint(got.Chains) != "[root]" || got.Closed != 2 {
				t.Fatalf("sweep = %+v, want the closed root's chain with its 2 steps closed", got)
			}
			assertSwept(t, store, "step", "nested")
		})
	}
}

// A failed v1 molecule attached under a live bead that never failed is its own
// chain: the attachment parent owns nothing, so the molecule is swept and the
// parent is left open.
func TestSweepFailedMoleculesAttachmentParentOwnsNothing(t *testing.T) {
	store := &strictCloseOrderStore{MemStore: beads.NewMemStore()}
	parent := mustCreate(t, store, beads.Bead{Title: "live work bead"})
	root := mustCreate(t, store, beads.Bead{Title: "attached v1 molecule", ParentID: parent.ID, Metadata: failedMeta()})
	step := mustCreate(t, store, beads.Bead{Title: "attached step", ParentID: root.ID, Metadata: failedMeta()})

	got, err := sweepAll(store, pastGrace())
	if err != nil {
		t.Fatalf("SweepFailedMolecules: %v", err)
	}
	if fmt.Sprint(got.Chains) != fmt.Sprint([]string{root.ID}) || got.Closed != 2 {
		t.Fatalf("sweep = %+v, want the attached molecule %s swept as its own chain", got, root.ID)
	}
	assertSwept(t, store, root.ID, step.ID)
	assertOpen(t, store, parent.ID)
}

// AC-3, AC-4: an orphaned chain waits until every failed member naming its
// gone or closed owner is past grace. Separate failed attachments can share
// an owner without having failed together, so an old member does not take a
// newer one's grace away.
func TestSweepFailedMoleculesOrphanedChainWaitsForEveryMember(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	member := func(id string, stamp time.Time) beads.Bead {
		return beads.Bead{
			ID: id, Title: "failed attach of a gone owner", Status: "open", CreatedAt: stamp, UpdatedAt: stamp,
			Metadata: failedWith(beadmeta.RootBeadIDMetadataKey, "gone"),
		}
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{member("old", cutoff.Add(-time.Hour)), member("recent", cutoff.Add(time.Minute))}, nil)

	got, err := sweepAll(store, cutoff)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(got.Chains) != 0 || got.Closed != 0 {
		t.Fatalf("first sweep = %+v, want nothing closed while a member is inside grace", got)
	}
	assertOpen(t, store, "old", "recent")

	got, err = sweepAll(store, cutoff.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("sweep past every member's grace: %v", err)
	}
	if fmt.Sprint(got.Chains) != "[gone]" || got.Closed != 2 {
		t.Fatalf("sweep past every member's grace = %+v, want the gone owner's 2 members closed", got)
	}
	assertSwept(t, store, "old", "recent")
}

// The sweep never scans: it reads the failed beads once per pass, and per
// top-level chain its root and the beads naming it by gc.root_bead_id, each a
// filter every backend pushes down. Steps linked only by ParentID come from
// that one discovery read, however many other beads the store holds.
func TestSweepFailedMoleculesReadsAreFiltersNotScans(t *testing.T) {
	inner := beads.NewMemStore()
	for i := 0; i < 200; i++ {
		mustCreate(t, inner, beads.Bead{Title: fmt.Sprintf("unrelated %d", i)})
	}
	for i := 0; i < 3; i++ {
		root := mustCreate(t, inner, beads.Bead{Title: "v2 root", Metadata: failedWith(beadmeta.KindMetadataKey, "workflow")})
		mustCreate(t, inner, beads.Bead{Title: "v2 step", ParentID: root.ID, Metadata: failedWith(beadmeta.RootBeadIDMetadataKey, root.ID)})
	}
	v1 := mustCreate(t, inner, beads.Bead{Title: "v1 root", Metadata: failedMeta()})
	v1Step := mustCreate(t, inner, beads.Bead{Title: "v1 step", ParentID: v1.ID, Metadata: failedMeta()})
	mustCreate(t, inner, beads.Bead{Title: "v1 nested step", ParentID: v1Step.ID, Metadata: failedMeta()})
	store := beadstest.NewOpRecordingStore(inner)

	got, err := sweepAll(store, pastGrace())
	if err != nil {
		t.Fatalf("SweepFailedMolecules: %v", err)
	}
	if len(got.Chains) != 4 || got.Closed != 9 {
		t.Fatalf("sweep = %+v, want 4 chains with 9 beads closed", got)
	}
	var lists, gets int
	for _, op := range store.Ops() {
		switch op.Kind {
		case beadstest.OpList:
			lists++
			if op.Query.AllowScan || len(op.Query.ParentIDs) > 0 || !op.Query.HasFilter() {
				t.Errorf("List %+v scans the store; every sweep read must be a pushed-down filter", op.Query)
			}
		case beadstest.OpGet:
			gets++
		}
	}
	if lists != 5 || gets != 4 {
		t.Errorf("sweep made %d Lists and %d Gets, want 5 Lists (discovery, then each chain's members) and 4 Gets (each chain's root)", lists, gets)
	}
}

// AC-7, AC-8: the cap counts the chains a pass attempts, a failed close
// included, so a store whose closes keep failing spends the cap instead of
// having every chain re-walked on every pass.
func TestSweepFailedMoleculesCapCountsAFailedClose(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &failCloseForStore{MemStore: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "faulting", Title: "faulting chain", Status: "open", CreatedAt: cutoff.Add(-2 * time.Hour), UpdatedAt: cutoff.Add(-2 * time.Hour), Metadata: failedMeta()},
		{ID: "healthy", Title: "healthy chain", Status: "open", CreatedAt: cutoff.Add(-time.Hour), UpdatedAt: cutoff.Add(-time.Hour), Metadata: failedMeta()},
	}, nil), failFor: "faulting"}

	got, err := sweep(store, cutoff, FailedMoleculeSweepBudget{MaxChains: 1})
	if err == nil || !strings.Contains(err.Error(), "closing failed molecule faulting") {
		t.Fatalf("sweep error = %v, want it to name the faulting chain", err)
	}
	if len(got.Chains) != 0 || !got.More {
		t.Fatalf("sweep = %+v, want no chain closed and More: the failed close spent the cap", got)
	}
	assertOpen(t, store, "faulting", "healthy")
}

// seedFailedRoots is a store holding one failed root per id, oldest first,
// every one past grace at cutoff.
func seedFailedRoots(cutoff time.Time, ids ...string) *beads.MemStore {
	return beads.NewMemStoreFrom(0, failedRoots(cutoff, ids...), nil)
}

// failedRoots is one failed root per id, oldest first, every one past grace
// at cutoff.
func failedRoots(cutoff time.Time, ids ...string) []beads.Bead {
	rows := make([]beads.Bead, 0, len(ids))
	for i, id := range ids {
		stamp := cutoff.Add(-time.Hour).Add(time.Duration(i) * time.Second)
		rows = append(rows, beads.Bead{ID: id, Title: "failed root", Status: "open", CreatedAt: stamp, UpdatedAt: stamp, Metadata: failedMeta()})
	}
	return rows
}

// A pass whose deadline has passed starts no chain after its first: the first
// always closes, so a caller sweeping several stores under one deadline makes
// progress in each, and More reports the chains left. The zero deadline
// places no bound.
func TestSweepFailedMoleculesStopsAfterItsFirstChainPastTheDeadline(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := seedFailedRoots(cutoff, "r1", "r2", "r3")
	passed := FailedMoleculeSweepBudget{Deadline: time.Now().Add(-time.Second)}

	for _, want := range []struct {
		budget FailedMoleculeSweepBudget
		chains string
		more   bool
	}{
		{passed, "[r1]", true},
		{passed, "[r2]", true},
		{FailedMoleculeSweepBudget{}, "[r3]", false},
	} {
		got, err := sweep(store, cutoff, want.budget)
		if err != nil {
			t.Fatalf("SweepFailedMolecules: %v", err)
		}
		if fmt.Sprint(got.Chains) != want.chains || got.More != want.more {
			t.Fatalf("sweep with deadline %v = %+v, want chains %s with More=%v", want.budget.Deadline, got, want.chains, want.more)
		}
	}
	assertSwept(t, store, "r1", "r2", "r3")
}

// A done ctx stops the pass before it reads past discovery, whether its first
// decision reads a chain's root or an outside owner: shutdown does not wait
// on the sweep, and More and Next report the bead it stopped at.
func TestSweepFailedMoleculesStartsNoChainOnceCtxIsDone(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	past := cutoff.Add(-time.Hour)
	for _, tc := range []struct {
		name  string
		seed  []beads.Bead
		first string
	}{
		{"a chain's root read", failedRoots(cutoff, "r1", "r2"), "r1"},
		{"an owner read", []beads.Bead{
			{ID: "root", Title: "closed v1 root", Status: "closed", CreatedAt: past, UpdatedAt: past, Metadata: failedMeta()},
			{ID: "step", Title: "v1 step", ParentID: "root", Status: "open", CreatedAt: past, UpdatedAt: past, Metadata: failedMeta()},
		}, "step"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beadstest.NewOpRecordingStore(beads.NewMemStoreFrom(0, tc.seed, nil))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			got, err := SweepFailedMolecules(ctx, beadstest.FederatedLeg(store), beadstest.WorkMolecule(store), cutoff, FailedMoleculeSweepBudget{})
			if err != nil {
				t.Fatalf("SweepFailedMolecules: %v", err)
			}
			if len(got.Chains) != 0 || got.Closed != 0 || !got.More || got.Next.ID != tc.first {
				t.Fatalf("sweep with a done ctx = %+v, want nothing closed, More, and Next %s", got, tc.first)
			}
			if reads := readsPastDiscovery(store); reads != 0 {
				t.Errorf("sweep with a done ctx made %d reads past discovery, want none", reads)
			}
		})
	}
}

// AC-4: an outside owner that never failed owns only the beads naming it by
// gc.root_bead_id. A failed molecule under it by ParentID alone tops its own
// chain, so when the owner closes and its named members are swept, that
// molecule's own gc.gc_exempt still keeps it open.
func TestSweepFailedMoleculesOwnerThatNeverFailedOwnsOnlyNamedMembers(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	past := cutoff.Add(-time.Hour)
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "wf", Title: "finished workflow", Status: "closed", CreatedAt: past, UpdatedAt: past, Metadata: map[string]string{beadmeta.KindMetadataKey: "workflow"}},
		{
			ID: "member", Title: "failed attach of the workflow", ParentID: "wf", Status: "open", CreatedAt: past, UpdatedAt: past,
			Metadata: failedWith(beadmeta.RootBeadIDMetadataKey, "wf"),
		},
		{
			ID: "exempt", Title: "exempt failed molecule under the workflow", ParentID: "wf", Status: "open", CreatedAt: past, UpdatedAt: past,
			Metadata: failedWith(beadmeta.GCExemptMetadataKey, "true"),
		},
	}, nil)

	got, err := sweepAll(store, cutoff)
	if err != nil {
		t.Fatalf("SweepFailedMolecules: %v", err)
	}
	if fmt.Sprint(got.Chains) != "[wf]" || got.Closed != 1 {
		t.Fatalf("sweep = %+v, want only the workflow's named member closed", got)
	}
	assertSwept(t, store, "member")
	assertOpen(t, store, "exempt")
}

// readsPastDiscovery counts the reads store recorded past the sweep's
// discovery List: by-ID reads, and Lists of the beads naming a root by
// gc.root_bead_id.
func readsPastDiscovery(store *beadstest.OpRecordingStore) int {
	reads := 0
	for _, op := range store.Ops() {
		switch {
		case op.Kind == beadstest.OpGet:
			reads++
		case op.Kind == beadstest.OpList && op.Query.Metadata[beadmeta.RootBeadIDMetadataKey] != "":
			reads++
		}
	}
	return reads
}

// faultyReadStore fails the Get of every ID in getFails and, when
// membersFail is set, every List of the beads naming a root by
// gc.root_bead_id: a store whose reads fail after the discovery List.
type faultyReadStore struct {
	*beads.MemStore
	getFails    map[string]bool
	membersFail bool
}

func (s *faultyReadStore) Get(id string) (beads.Bead, error) {
	if s.getFails[id] {
		return beads.Bead{}, fmt.Errorf("injected read fault for %s", id)
	}
	return s.MemStore.Get(id)
}

func (s *faultyReadStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if _, members := query.Metadata[beadmeta.RootBeadIDMetadataKey]; members && s.membersFail {
		return nil, errors.New("injected member list fault")
	}
	return s.MemStore.List(query)
}

// AC-4: a failed read of a chain's outside owner is no proof the owner is
// gone. The chain stays open and the joined error names the owner, both for a
// live workflow its failed attachment names by gc.root_bead_id and for a
// closed, exempt v1 root its step names by ParentID alone.
func TestSweepFailedMoleculesHoldsAChainWhoseOwnerReadFails(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	past := cutoff.Add(-time.Hour)
	for _, tc := range []struct {
		name  string
		seed  []beads.Bead
		owner string
		held  string
	}{
		{"live owner named by gc.root_bead_id", []beads.Bead{
			{ID: "wf", Title: "live workflow", Status: "open", CreatedAt: past, UpdatedAt: past, Metadata: map[string]string{beadmeta.KindMetadataKey: "workflow"}},
			{ID: "attach", Title: "failed attach", Status: "open", CreatedAt: past, UpdatedAt: past, Metadata: failedWith(beadmeta.RootBeadIDMetadataKey, "wf")},
		}, "wf", "attach"},
		{"closed exempt ParentID root", []beads.Bead{
			{ID: "root", Title: "closed exempt v1 root", Status: "closed", CreatedAt: past, UpdatedAt: past, Metadata: failedWith(beadmeta.GCExemptMetadataKey, "true")},
			{ID: "step", Title: "v1 step", ParentID: "root", Status: "open", CreatedAt: past, UpdatedAt: past, Metadata: failedMeta()},
		}, "root", "step"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &faultyReadStore{MemStore: beads.NewMemStoreFrom(0, tc.seed, nil), getFails: map[string]bool{tc.owner: true}}

			got, err := sweepAll(store, cutoff)
			if want := "resolving failed molecule owner " + tc.owner; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("sweep error = %v, want it to name the owner: %q", err, want)
			}
			if len(got.Chains) != 0 || got.Closed != 0 {
				t.Fatalf("sweep = %+v, want nothing closed while the owner cannot be read", got)
			}
			assertOpen(t, store, tc.held)
		})
	}
}

// staleSnapshotStore answers the first Get of id with fresh applied to the
// bead as stored: what a write landing between the sweep's discovery List and
// its fresh read of that bead shows.
type staleSnapshotStore struct {
	*beads.MemStore
	t     *testing.T
	id    string
	fresh func(t *testing.T, store *beads.MemStore, b beads.Bead) beads.Bead
	read  bool
}

func (s *staleSnapshotStore) Get(id string) (beads.Bead, error) {
	b, err := s.MemStore.Get(id)
	if err != nil || id != s.id || s.read {
		return b, err
	}
	s.read = true
	return s.fresh(s.t, s.MemStore, b), nil
}

// A top's chain closes on what its fresh read shows, not on the discovery
// snapshot. An operator's gc.gc_exempt stamp, a refreshed failure stamp, a
// close, a cleared molecule_failed stamp or a new link written after the
// discovery List keeps the top and its whole chain open this pass, and the
// next pass decides again.
func TestSweepFailedMoleculesRechecksTheTopItReadsFresh(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	past := cutoff.Add(-time.Hour)
	type freshFunc = func(*testing.T, *beads.MemStore, beads.Bead) beads.Bead
	set := func(kv ...string) freshFunc {
		return func(_ *testing.T, _ *beads.MemStore, b beads.Bead) beads.Bead {
			b.Metadata = maps.Clone(b.Metadata)
			for i := 0; i+1 < len(kv); i += 2 {
				b.Metadata[kv[i]] = kv[i+1]
			}
			return b
		}
	}
	for _, tc := range []struct {
		name  string
		fresh freshFunc
		swept bool
	}{
		{"unchanged", func(_ *testing.T, _ *beads.MemStore, b beads.Bead) beads.Bead { return b }, true},
		{"stamped gc.gc_exempt, failure stamp kept", set(beadmeta.GCExemptMetadataKey, "true"), false},
		{"failure stamp refreshed", func(_ *testing.T, _ *beads.MemStore, b beads.Bead) beads.Bead {
			b.UpdatedAt = cutoff.Add(time.Minute)
			return b
		}, false},
		{"closed", func(_ *testing.T, _ *beads.MemStore, b beads.Bead) beads.Bead {
			b.Status = "closed"
			return b
		}, false},
		{"molecule_failed cleared", set(beadmeta.MoleculeFailedMetadataKey, ""), false},
		{"named another root", set(beadmeta.RootBeadIDMetadataKey, "elsewhere"), false},
		{"reparented", func(_ *testing.T, _ *beads.MemStore, b beads.Bead) beads.Bead {
			b.ParentID = "elsewhere"
			return b
		}, false},
		{"gc.gc_exempt written to the store", func(t *testing.T, store *beads.MemStore, b beads.Bead) beads.Bead {
			if err := store.SetMetadata(b.ID, beadmeta.GCExemptMetadataKey, "true"); err != nil {
				t.Fatalf("stamping gc.gc_exempt: %v", err)
			}
			return mustGet(t, store, b.ID)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &staleSnapshotStore{MemStore: beads.NewMemStoreFrom(0, []beads.Bead{
				{ID: "root", Title: "failed root", Status: "open", CreatedAt: past, UpdatedAt: past, Metadata: failedMeta()},
				{ID: "member", Title: "failed member", Status: "open", CreatedAt: past, UpdatedAt: past, Metadata: failedWith(beadmeta.RootBeadIDMetadataKey, "root")},
				{ID: "step", Title: "failed v1 step", ParentID: "root", Status: "open", CreatedAt: past, UpdatedAt: past, Metadata: failedMeta()},
			}, nil), t: t, id: "root", fresh: tc.fresh}

			got, err := sweepAll(store, cutoff)
			if err != nil {
				t.Fatalf("SweepFailedMolecules: %v", err)
			}
			if tc.swept {
				if fmt.Sprint(got.Chains) != "[root]" || got.Closed != 3 {
					t.Fatalf("sweep = %+v, want the root's chain closed with 3 beads", got)
				}
				assertSwept(t, store, "root", "member", "step")
				return
			}
			if len(got.Chains) != 0 || got.Closed != 0 || got.More {
				t.Fatalf("sweep = %+v, want nothing closed and the pass finished", got)
			}
			assertOpen(t, store.MemStore, "root", "member", "step")
		})
	}
}

// AC-7, AC-8: a read that fails costs the cap as a failed close does, and
// past the deadline a pass that has read starts no decision that reads again.
// A store whose root, member or parent reads keep failing, or a backlog of
// live owners to read, therefore costs one pass a bounded number of reads,
// and the pass reports More and the bead it stopped at. A chain whose owner
// keeps it costs no cap, so under the cap alone every live owner is read.
func TestSweepFailedMoleculesBoundsTheReadsOfAPass(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return cutoff.Add(-time.Hour).Add(time.Duration(i) * time.Second) }
	var roots, steps, attachments []beads.Bead
	for i := 1; i <= 3; i++ {
		roots = append(roots, beads.Bead{ID: fmt.Sprintf("r%d", i), Title: "failed root", Status: "open", CreatedAt: at(i), UpdatedAt: at(i), Metadata: failedMeta()})
		steps = append(steps, beads.Bead{
			ID: fmt.Sprintf("s%d", i), Title: "failed v1 step", ParentID: fmt.Sprintf("p%d", i), Status: "open", CreatedAt: at(i), UpdatedAt: at(i), Metadata: failedMeta(),
		})
		attachments = append(attachments,
			beads.Bead{ID: fmt.Sprintf("w%d", i), Title: "live workflow", Status: "open", CreatedAt: at(0), UpdatedAt: at(0), Metadata: map[string]string{beadmeta.KindMetadataKey: "workflow"}},
			beads.Bead{
				ID: fmt.Sprintf("a%d", i), Title: "failed attach", Status: "open", CreatedAt: at(i), UpdatedAt: at(i),
				Metadata: failedWith(beadmeta.RootBeadIDMetadataKey, fmt.Sprintf("w%d", i)),
			})
	}
	failing := func(ids ...string) map[string]bool {
		m := make(map[string]bool, len(ids))
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	capped := FailedMoleculeSweepBudget{MaxChains: 1}
	late := FailedMoleculeSweepBudget{Deadline: time.Now().Add(-time.Second)}
	for _, tc := range []struct {
		name        string
		seed        []beads.Bead
		getFails    map[string]bool
		membersFail bool
		budget      FailedMoleculeSweepBudget
		reads       int
		err         string
		next        string
	}{
		{"root reads fail, capped", roots, failing("r1", "r2", "r3"), false, capped, 1, "closing failed molecule r1: reading root", "r2"},
		{"root reads fail, past the deadline", roots, failing("r1", "r2", "r3"), false, late, 1, "closing failed molecule r1: reading root", "r2"},
		{"member lists fail, capped", roots, nil, true, capped, 2, "closing failed molecule r1: listing members", "r2"},
		{"member lists fail, past the deadline", roots, nil, true, late, 2, "closing failed molecule r1: listing members", "r2"},
		{"parent reads fail, capped", steps, failing("p1", "p2", "p3"), false, capped, 1, "resolving failed molecule owner p1", "s2"},
		{"parent reads fail, past the deadline", steps, failing("p1", "p2", "p3"), false, late, 1, "resolving failed molecule owner p1", "s2"},
		{"live owners, past the deadline", attachments, nil, false, late, 1, "", "a2"},
		{"live owners, capped", attachments, nil, false, capped, 3, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beadstest.NewOpRecordingStore(&faultyReadStore{
				MemStore: beads.NewMemStoreFrom(0, tc.seed, nil), getFails: tc.getFails, membersFail: tc.membersFail,
			})

			got, err := sweep(store, cutoff, tc.budget)
			if tc.err == "" && err != nil {
				t.Fatalf("SweepFailedMolecules: %v", err)
			}
			if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("sweep error = %v, want it to contain %q", err, tc.err)
			}
			if got.Closed != 0 || got.More != (tc.next != "") || got.Next.ID != tc.next {
				t.Fatalf("sweep = %+v, want nothing closed, More=%v and Next %q", got, tc.next != "", tc.next)
			}
			if reads := readsPastDiscovery(store); reads != tc.reads {
				t.Errorf("sweep made %d reads past discovery, want %d", reads, tc.reads)
			}
		})
	}
}

// AC-7: a pass the budget stopped names the bead it stopped at, and a pass
// given that cursor starts there and goes round to the older beads after the
// newest. Live owners read one a pass past the deadline then hold a sweepable
// chain behind them back only until the passes reach it; restarting at the
// oldest every pass never would. A cursor whose bead is gone starts at the
// first bead after it.
func TestSweepFailedMoleculesResumesWhereThePassStopped(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return cutoff.Add(-time.Hour).Add(time.Duration(i) * time.Second) }
	seed := func() *beads.MemStore {
		var rows []beads.Bead
		for i := 1; i <= 3; i++ {
			rows = append(rows,
				beads.Bead{ID: fmt.Sprintf("w%d", i), Title: "live workflow", Status: "open", CreatedAt: at(0), UpdatedAt: at(0), Metadata: map[string]string{beadmeta.KindMetadataKey: "workflow"}},
				beads.Bead{
					ID: fmt.Sprintf("a%d", i), Title: "failed attach", Status: "open", CreatedAt: at(i), UpdatedAt: at(i),
					Metadata: failedWith(beadmeta.RootBeadIDMetadataKey, fmt.Sprintf("w%d", i)),
				})
		}
		rows = append(rows, beads.Bead{ID: "root", Title: "sweepable failed root", Status: "open", CreatedAt: at(4), UpdatedAt: at(4), Metadata: failedMeta()})
		return beads.NewMemStoreFrom(0, rows, nil)
	}
	late := func(from FailedMoleculeSweepCursor) FailedMoleculeSweepBudget {
		return FailedMoleculeSweepBudget{Deadline: time.Now().Add(-time.Second), From: from}
	}

	t.Run("threading Next", func(t *testing.T) {
		store := seed()
		var from FailedMoleculeSweepCursor
		for i, want := range []string{"a2", "a3", "root", "a1"} {
			if i == 3 {
				assertOpen(t, store, "root")
			}
			got, err := sweep(store, cutoff, late(from))
			if err != nil {
				t.Fatalf("pass %d: %v", i+1, err)
			}
			if !got.More || got.Next.ID != want {
				t.Fatalf("pass %d = %+v, want More and Next %s", i+1, got, want)
			}
			from = got.Next
		}
		assertSwept(t, store, "root")
		assertOpen(t, store, "a1", "a2", "a3")
	})

	t.Run("restarting at the oldest", func(t *testing.T) {
		store := seed()
		for i := 0; i < 4; i++ {
			got, err := sweep(store, cutoff, late(FailedMoleculeSweepCursor{}))
			if err != nil || got.Next.ID != "a2" {
				t.Fatalf("pass %d = %+v, %v; want it stopped at a2 again", i+1, got, err)
			}
		}
		assertOpen(t, store, "root")
	})

	t.Run("a cursor whose bead is gone", func(t *testing.T) {
		got, err := sweep(seed(), cutoff, late(FailedMoleculeSweepCursor{CreatedAt: at(2).Add(time.Millisecond), ID: "gone"}))
		if err != nil || !got.More || got.Next.ID != "root" {
			t.Fatalf("sweep = %+v, %v; want it to start at a3 and stop at root", got, err)
		}
	})
}

// The sweep reads and closes in one store: a leg over any other store is
// refused before anything is read.
func TestSweepFailedMoleculesRefusesALegOverAnotherStore(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := seedFailedRoots(cutoff, "r1")
	other := beadstest.NewOpRecordingStore(seedFailedRoots(cutoff, "r1"))

	got, err := SweepFailedMolecules(context.Background(), beadstest.FederatedLeg(other), beadstest.WorkMolecule(store), cutoff, FailedMoleculeSweepBudget{})
	if err == nil || !strings.Contains(err.Error(), "the read leg is not over the store the sweep closes in") {
		t.Fatalf("sweep error = %v, want the leg refused", err)
	}
	if len(got.Chains) != 0 || got.Closed != 0 || len(other.Ops()) != 0 {
		t.Fatalf("sweep = %+v with %d reads of the leg's store, want nothing read or closed", got, len(other.Ops()))
	}
	assertOpen(t, store, "r1")
}
