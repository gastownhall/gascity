package main

import (
	"bytes"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// twoRigSplitEnv is the split fixture with a serving rig (e.rig, rig-A) and a
// second, SUSPENDED rig (rig-B) whose store the legs leave out.
func twoRigSplitEnv(t *testing.T) (e splitEnv, suspended beads.Store, legs WorkLegs) {
	t.Helper()
	e = newSplitEnvWithRig(t, true)
	suspended = beads.NewMemStoreFrom(500, nil, nil)
	e.cfg.Rigs = append(e.cfg.Rigs, config.Rig{Name: "rig-B", Path: filepath.Join(e.cityPath, "rigs", "rig-B"), Suspended: true})
	legs = workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: e.work}), map[string]beads.Store{e.rigName: e.rig, "rig-B": suspended})
	if got := legs.suspendedRigs(); len(got) != 1 || got[0] != "rig-B" {
		t.Fatalf("suspendedRigs = %v, want [rig-B]", got)
	}
	return e, suspended, legs
}

func runGCWithLegs(t *testing.T, legs wispGCLegs, graph beads.Store, now time.Time) string {
	t.Helper()
	var logs bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()
	withCloseAbandonedTTL(t, 5*time.Minute, func() {
		wg := newTestGC()
		if err := wg.pass(legs, graph, now); err != nil {
			t.Logf("runGC: %v", err)
		}
	})
	return logs.String()
}

func stepless(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func wantStatus(t *testing.T, store beads.Store, id, want, why string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got.Status != want {
		t.Errorf("%s status=%q, want %q: %s", id, got.Status, want, why)
	}
}

// 2(a): the work arm reads the root's own leg and the graph store only, so
// another rig's suspension does not hold its abandoned stepless root open.
func TestWispGCWorkArmIgnoresOtherRigsSuspension(t *testing.T) {
	e, _, legs := twoRigSplitEnv(t)
	root := stepless(t, e.rig, beads.Bead{Title: "junk v1 molecule", Type: "molecule"})
	withWorkMoleculesEnforced(t, func() {
		withCloseAbandonedEnforced(t, func() {
			runGCWithLegs(t, legs, e.graphStore(), time.Now().Add(30*time.Minute))
		})
	})
	wantStatus(t, e.rig, root.ID, "closed", "rig-B's suspension says nothing about a rig-A molecule")
}

// 2(b): an attached root-only wisp that records its source is decided by that
// source: left open only while the source is live or its rig is suspended.
func TestWispGCHolderGuardAsksTheRecordedSource(t *testing.T) {
	e, suspended, legs := twoRigSplitEnv(t)
	graph := e.graphStore()
	doneSrc := stepless(t, e.rig, beads.Bead{Title: "done source", Type: "task"})
	if err := e.rig.Close(doneSrc.ID); err != nil {
		t.Fatal(err)
	}
	liveSrc := stepless(t, e.rig, beads.Bead{Title: "live source", Type: "task"})
	parkedSrc := stepless(t, suspended, beads.Bead{Title: "source in a suspended rig", Type: "task"})
	attached := func(src beads.Bead, rig string) beads.Bead {
		return stepless(t, graph, beads.Bead{Title: "attached wisp", Type: "molecule", Metadata: map[string]string{
			beadmeta.KindMetadataKey:           beadmeta.KindWisp,
			beadmeta.SourceBeadIDMetadataKey:   src.ID,
			beadmeta.SourceStoreRefMetadataKey: "rig:" + rig,
		}})
	}
	toDone := attached(doneSrc, e.rigName)
	toLive := attached(liveSrc, e.rigName)
	toParked := attached(parkedSrc, "rig-B")
	withCloseAbandonedEnforced(t, func() {
		runGCWithLegs(t, legs, graph, time.Now().Add(30*time.Minute))
	})
	wantStatus(t, graph, toDone.ID, "closed", "its recorded source is closed, whatever rig-B holds")
	wantStatus(t, graph, toLive.ID, "open", "its recorded source is live")
	wantStatus(t, graph, toParked.ID, "open", "its recorded source sits in a suspended rig")
}

// 2(c) and 2(d): a root with no source pointer is held open by a suspended rig
// only when it is molecule-typed (it may be a pre-pointer attached wisp); a
// task-typed standalone wisp is not. The pass logs one line, not one per root.
func TestWispGCSuspensionHoldsOnlyMoleculeTypedRootsWithoutAPointer(t *testing.T) {
	e, _, legs := twoRigSplitEnv(t)
	graph := e.graphStore()
	mol1 := stepless(t, graph, beads.Bead{Title: "legacy attached 1", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}})
	mol2 := stepless(t, graph, beads.Bead{Title: "legacy attached 2", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}})
	// A task-typed stepless root the closer considers: a graph.v2 workflow root
	// with no source pointer (sling stamps one on every attached launch).
	task := stepless(t, graph, beads.Bead{Title: "standalone workflow root", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
	}})
	var out string
	withCloseAbandonedEnforced(t, func() {
		out = runGCWithLegs(t, legs, graph, time.Now().Add(30*time.Minute))
	})
	wantStatus(t, graph, mol1.ID, "open", "a molecule-typed root without a pointer may have its source in the suspended rig")
	wantStatus(t, graph, mol2.ID, "open", "a molecule-typed root without a pointer may have its source in the suspended rig")
	wantStatus(t, graph, task.ID, "closed", "a task-typed root without a source pointer is not an attached wisp")
	if n := strings.Count(out, "suspended rig(s)"); n != 1 {
		t.Errorf("suspension logged %d time(s) this pass, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "left 2 stepless root(s) open") {
		t.Errorf("summary line does not count both held roots:\n%s", out)
	}
}

// M9: the work arm's abandoned close also needs GC_WISP_GC_CLOSE_ABANDONED.
func TestWispGCWorkArmCloseNeedsTheCloseAbandonedOptIn(t *testing.T) {
	e, _, legs := twoRigSplitEnv(t)
	root := stepless(t, e.rig, beads.Bead{Title: "junk v1 molecule", Type: "molecule"})
	withWorkMoleculesEnforced(t, func() {
		runGCWithLegs(t, legs, e.graphStore(), time.Now().Add(30*time.Minute))
	})
	wantStatus(t, e.rig, root.ID, "open", "GC_WISP_GC_CLOSE_ABANDONED is not set")
}

// M12 and M13: the work arm only takes ROOT molecules that classify as WORK.
// A graph-marked molecule on a work leg is not its business, and a child
// molecule (ParentID set) is not a root: a stepless child has no molecule_id
// holder to protect it, so closing it would break its parent.
func TestWispGCWorkArmTakesOnlyWorkClassRootMolecules(t *testing.T) {
	e, _, legs := twoRigSplitEnv(t)
	parent := stepless(t, e.rig, beads.Bead{Title: "parent molecule", Type: "molecule"})
	child := stepless(t, e.rig, beads.Bead{Title: "child molecule", Type: "molecule", ParentID: parent.ID})
	graphMarked := stepless(t, e.rig, beads.Bead{Title: "graph-marked molecule", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}})
	roots, err := enumerateWorkClassMoleculeRoots(e.rig, "open", "in_progress")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roots {
		if r.ID == child.ID || r.ID == graphMarked.ID {
			t.Errorf("enumerateWorkClassMoleculeRoots returned %s (type=%s parent=%q)", r.ID, r.Type, r.ParentID)
		}
	}
	withWorkMoleculesEnforced(t, func() {
		withCloseAbandonedEnforced(t, func() {
			runGCWithLegs(t, legs, e.graphStore(), time.Now().Add(30*time.Minute))
		})
	})
	wantStatus(t, e.rig, child.ID, "open", "a child molecule is not a root")
	wantStatus(t, e.rig, graphMarked.ID, "open", "a graph-class molecule is not the work arm's")
}

// templateLeakingStore models bd's --include-templates: a both-tiers or
// wisps-tier read also returns the formula proto rows, which carry no
// template label here.
type templateLeakingStore struct {
	*beads.MemStore
	proto beads.Bead
}

func (s templateLeakingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(q)
	if err == nil && (q.TierMode == beads.TierBoth || q.TierMode == beads.TierWisps) {
		rows = append(rows, s.proto)
	}
	return rows, err
}

// M5: the work arm reads the issues tier only, so bd's template rows a
// both-tiers read would include are never candidates.
func TestWispGCWorkArmReadsTheIssuesTierOnly(t *testing.T) {
	store := templateLeakingStore{MemStore: beads.NewMemStore(), proto: beads.Bead{ID: "proto-1", Title: "mol-proto", Type: "molecule", Status: "closed"}}
	roots, err := enumerateWorkClassMoleculeRoots(store, "closed")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roots {
		if r.ID == "proto-1" {
			t.Fatal("a template row a both-tiers read returns was enumerated")
		}
	}
}

// unreadableMemberStore fails the live read of one bead.
type unreadableMemberStore struct {
	batchDeleteWorkStore
	failID string
}

func (s unreadableMemberStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, errors.New("store unavailable")
	}
	return s.batchDeleteWorkStore.Get(id)
}

// M10: a closure member whose row cannot be read counts as open: the purge
// fails closed.
func TestWispGCPurgeCountsAnUnreadableMemberAsOpen(t *testing.T) {
	inner := newBatchDeleteWorkStore()
	root := stepless(t, inner, beads.Bead{Title: "v1 poured", Type: "molecule"})
	member := stepless(t, inner, beads.Bead{Title: "dep-only member", Type: "step"})
	if err := inner.DepAdd(member.ID, root.ID, "parent-child"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{member.ID, root.ID} {
		if err := inner.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	store := unreadableMemberStore{batchDeleteWorkStore: inner, failID: member.ID}
	live, err := inner.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	purged, _ := purgeExpiredBeadClosures(store, []beads.Bead{live}, time.Now().Add(time.Hour), 0)
	if purged != 0 {
		t.Fatalf("purged %d closure(s) with an unreadable member, want 0", purged)
	}
	if _, err := inner.Get(root.ID); err != nil {
		t.Fatalf("root %s was deleted although a member could not be read: %v", root.ID, err)
	}
}

// M15: a refused closure is not charged to the delete cap: with a cap of 1, a
// refusal ahead of a collectible root still lets that root be purged.
func TestWispGCPurgeRefusalIsNotChargedToTheCap(t *testing.T) {
	store := newBatchDeleteWorkStore()
	refused := stepless(t, store, beads.Bead{Title: "held", Type: "molecule"})
	held := stepless(t, store, beads.Bead{Title: "reopened step", Type: "step", ParentID: refused.ID})
	ok := stepless(t, store, beads.Bead{Title: "collectible", Type: "molecule"})
	for _, id := range []string{refused.ID, ok.ID} {
		if err := store.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	_ = held
	entries, err := closedWispGCEntries(store)
	if err != nil {
		t.Fatal(err)
	}
	purged, err := purgeExpiredBeadClosures(store, entries, time.Now().Add(time.Hour), 1)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1: the refusal must not consume the only cap slot", purged)
	}
	if _, err := store.Get(ok.ID); err == nil {
		t.Fatalf("collectible root %s survived", ok.ID)
	}
}

// Should-fix 5: an unread mail or nudge under a closed root does not hold its
// closure, the same carve-out the strand guard makes.
func TestWispGCPurgeIgnoresALingeringNotification(t *testing.T) {
	store := newBatchDeleteWorkStore()
	root := stepless(t, store, beads.Bead{Title: "v1 poured", Type: "molecule"})
	stepless(t, store, beads.Bead{Title: "unread mail", Type: "message", ParentID: root.ID})
	if err := store.Close(root.ID); err != nil {
		t.Fatal(err)
	}
	live, err := store.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	purged, err := purgeExpiredBeadClosures(store, []beads.Bead{live}, time.Now().Add(time.Hour), 0)
	if err != nil || purged != 1 {
		t.Fatalf("purge = (%d, %v), want the closure purged despite the open notification", purged, err)
	}
}

// Should-fix 6: the work arm's purge clock is the root's last update (its
// close), not its creation: a long-lived molecule closed just now is kept.
func TestWispGCWorkArmPurgeAgesOnTheClose(t *testing.T) {
	old := time.Now().Add(-72 * time.Hour)
	closedNow := beads.Bead{ID: "x", Type: "molecule", Status: "closed", CreatedAt: old, UpdatedAt: time.Now()}
	if !workArmPurgeAge(closedNow).Equal(closedNow.UpdatedAt) {
		t.Fatalf("work arm purge age = %v, want the last update %v", workArmPurgeAge(closedNow), closedNow.UpdatedAt)
	}
	if workArmPurgeAge(closedNow).Before(time.Now().Add(-time.Hour)) {
		t.Fatal("a molecule closed just now reads as past a one-hour retention")
	}
}
