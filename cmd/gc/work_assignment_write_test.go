package main

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// recordingWriteWorkStore records every Update/SetMetadata call the
// work-assignment WRITE façade emits while delegating to a MemStore for real
// effects. It proves the façade emits byte-identical bead writes to the raw ops
// it replaces (same UpdateOpts pointers/values, same metadata patches).
type recordingWriteWorkStore struct {
	*beads.MemStore
	listQueries []beads.ListQuery
	updates     []recordedUpdate
	metaSets    []recordedMetaSet
}

type recordedUpdate struct {
	id   string
	opts beads.UpdateOpts
}

type recordedMetaSet struct {
	id    string
	key   string
	value string
}

func newRecordingWriteWorkStore() *recordingWriteWorkStore {
	return &recordingWriteWorkStore{MemStore: beads.NewMemStore()}
}

func (s *recordingWriteWorkStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.listQueries = append(s.listQueries, q)
	return s.MemStore.List(q)
}

// Update/SetMetadata record the exact op and report success WITHOUT delegating
// to the MemStore. The byte-identity asserts only need the captured op; the
// MemStore.Create path rewrites bead IDs to gc-N, so delegating would force the
// tests to round-trip generated IDs. Recording-only keeps the asserts pinned to
// the literal beads the façade was asked to write.
func (s *recordingWriteWorkStore) Update(id string, opts beads.UpdateOpts) error {
	s.updates = append(s.updates, recordedUpdate{id: id, opts: opts})
	return nil
}

func (s *recordingWriteWorkStore) SetMetadata(id, key, value string) error {
	s.metaSets = append(s.metaSets, recordedMetaSet{id: id, key: key, value: value})
	return nil
}

func derefStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestWorkAssignmentOpenAssignedToBasic_ByteIdenticalQuery asserts the no-flags
// List variant (used by releaseWorkFromClosedSessionBead) emits exactly
// {Assignee,Status} — no Live, no TierMode — matching the raw probe.
func TestWorkAssignmentOpenAssignedToBasic_ByteIdenticalQuery(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if _, err := wa.OpenAssignedToBasic("agent-1", "in_progress"); err != nil {
		t.Fatalf("OpenAssignedToBasic: %v", err)
	}
	want := beads.ListQuery{Assignee: "agent-1", Status: "in_progress"}
	if len(rec.listQueries) != 1 || !reflect.DeepEqual(rec.listQueries[0], want) {
		t.Fatalf("List query mismatch:\n got %#v\n want %#v", rec.listQueries, want)
	}
}

// TestWorkAssignmentReleaseWorkBead_OpenStaysOpen asserts that releasing an
// already-open bead emits Update{Assignee:"", Metadata:<clearedAffinity>} with
// NO Status change (status reset is only for in_progress), byte-identical to the
// raw release op.
func TestWorkAssignmentReleaseWorkBead_OpenStaysOpen(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	item := beads.Bead{ID: "w-open", Status: "open", Assignee: "agent-1"}
	if err := wa.ReleaseWorkBead(item, ""); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	if len(rec.updates) != 1 {
		t.Fatalf("expected 1 Update, got %d: %#v", len(rec.updates), rec.updates)
	}
	got := rec.updates[0]
	if got.id != "w-open" {
		t.Fatalf("Update id = %q, want w-open", got.id)
	}
	if derefStr(got.opts.Assignee) != "" {
		t.Fatalf("Assignee = %q, want empty-string clear", derefStr(got.opts.Assignee))
	}
	if got.opts.Status != nil {
		t.Fatalf("Status should be nil for an already-open bead, got %q", *got.opts.Status)
	}
	wantMeta := clearedSessionAffinityMetadata()
	if !reflect.DeepEqual(got.opts.Metadata, wantMeta) {
		t.Fatalf("Metadata mismatch:\n got %#v\n want %#v", got.opts.Metadata, wantMeta)
	}
}

// TestWorkAssignmentReleaseWorkBead_InProgressResetsToOpen asserts an
// in_progress bead is reset to open on release, byte-identical to the raw op.
func TestWorkAssignmentReleaseWorkBead_InProgressResetsToOpen(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	item := beads.Bead{ID: "w-ip", Status: "in_progress", Assignee: "agent-1"}
	if err := wa.ReleaseWorkBead(item, ""); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	got := rec.updates[0]
	if got.opts.Status == nil || *got.opts.Status != "open" {
		t.Fatalf("Status = %v, want open", got.opts.Status)
	}
	if derefStr(got.opts.Assignee) != "" {
		t.Fatalf("Assignee = %q, want empty-string clear", derefStr(got.opts.Assignee))
	}
}

// TestWorkAssignmentReleaseWorkBead_RunTargetFallbackApplied asserts the
// run_target fallback (used by the retire/unclaim path) is written only when the
// bead has neither run_target nor routed_to, byte-identical to the raw op.
func TestWorkAssignmentReleaseWorkBead_RunTargetFallbackApplied(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	item := beads.Bead{ID: "w-route", Status: "in_progress", Assignee: "agent-1"}
	if err := wa.ReleaseWorkBead(item, "worker"); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	got := rec.updates[0]
	if got.opts.Metadata[beadmeta.RunTargetMetadataKey] != "worker" {
		t.Fatalf("run_target fallback = %q, want worker", got.opts.Metadata[beadmeta.RunTargetMetadataKey])
	}
}

// TestWorkAssignmentReleaseWorkBead_RunTargetFallbackSkippedWhenRouted asserts
// the fallback is NOT applied when run_target/routed_to are already present.
func TestWorkAssignmentReleaseWorkBead_RunTargetFallbackSkippedWhenRouted(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	item := beads.Bead{
		ID:       "w-routed",
		Status:   "in_progress",
		Assignee: "agent-1",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "existing"},
	}
	if err := wa.ReleaseWorkBead(item, "worker"); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	got := rec.updates[0]
	if _, ok := got.opts.Metadata[beadmeta.RunTargetMetadataKey]; ok {
		t.Fatalf("run_target fallback must be skipped when routed_to present, got %#v", got.opts.Metadata)
	}
}

// TestWorkAssignmentReassignWorkBead_ByteIdentical asserts reassign emits only
// Update{Assignee:&new}, byte-identical to the raw retire-reassign op.
func TestWorkAssignmentReassignWorkBead_ByteIdentical(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if err := wa.ReassignWorkBead("w-1", "new-session"); err != nil {
		t.Fatalf("ReassignWorkBead: %v", err)
	}
	if len(rec.updates) != 1 {
		t.Fatalf("expected 1 Update, got %d", len(rec.updates))
	}
	got := rec.updates[0]
	if got.id != "w-1" || derefStr(got.opts.Assignee) != "new-session" {
		t.Fatalf("reassign = id %q assignee %q, want w-1/new-session", got.id, derefStr(got.opts.Assignee))
	}
	if got.opts.Status != nil || got.opts.Metadata != nil {
		t.Fatalf("reassign must not touch Status/Metadata, got %#v", got.opts)
	}
}

// TestWorkAssignmentClearDetachedProbe_ByteIdentical asserts the detached-probe
// clear emits SetMetadata(id, gc.detached, "") — the empty-string clear contract.
func TestWorkAssignmentClearDetachedProbe_ByteIdentical(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if err := wa.ClearDetachedProbe("w-1"); err != nil {
		t.Fatalf("ClearDetachedProbe: %v", err)
	}
	want := recordedMetaSet{id: "w-1", key: beadmeta.DetachedMetadataKey, value: ""}
	if len(rec.metaSets) != 1 || rec.metaSets[0] != want {
		t.Fatalf("SetMetadata mismatch:\n got %#v\n want %#v", rec.metaSets, want)
	}
}

// TestWorkAssignmentWrite_NilStoreSafe asserts the write methods tolerate a nil
// underlying store the same way the raw ops did (no panic, no write).
func TestWorkAssignmentWrite_NilStoreSafe(t *testing.T) {
	wa := workAssignmentForStore(beads.WorkStore{Store: nil})
	if err := wa.ReleaseWorkBead(beads.Bead{ID: "x"}, ""); err != nil {
		t.Fatalf("nil store ReleaseWorkBead: %v", err)
	}
	if err := wa.ReassignWorkBead("x", "y"); err != nil {
		t.Fatalf("nil store ReassignWorkBead: %v", err)
	}
	if err := wa.ClearDetachedProbe("x"); err != nil { // must not panic
		t.Fatalf("nil store ClearDetachedProbe: %v", err)
	}
	if _, err := wa.OpenAssignedToBasic("a", "open"); err != nil {
		t.Fatalf("nil store OpenAssignedToBasic: %v", err)
	}
}
