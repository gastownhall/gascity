package main

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// recordingWriteWorkStore records every Update/UpdateIfMatch/SetMetadata call
// the work-assignment WRITE façade emits while delegating reads to a MemStore.
// It proves the façade writes the same fields as the raw ops it replaces (same
// UpdateOpts pointers/values, same metadata patches), and that a release
// computed from a snapshot reaches the store as a fenced write.
type recordingWriteWorkStore struct {
	*beads.MemStore
	listQueries []beads.ListQuery
	updates     []recordedUpdate
	metaSets    []recordedMetaSet
}

type recordedUpdate struct {
	id   string
	opts beads.UpdateOpts
	// fenced is set for an UpdateIfMatch, which carries revision.
	fenced   bool
	revision int64
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

// UpdateIfMatch records the fenced write, with the revision it is fenced on,
// and reports success without delegating, exactly as Update does.
func (s *recordingWriteWorkStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	s.updates = append(s.updates, recordedUpdate{id: id, opts: opts, fenced: true, revision: expectedRevision})
	return nil
}

func (s *recordingWriteWorkStore) SetMetadata(id, key, value string) error {
	s.metaSets = append(s.metaSets, recordedMetaSet{id: id, key: key, value: value})
	return nil
}

// The assertion is the enforcement of the comment below: on a signature drift
// the override stops satisfying the interface, MemStore's implementation is
// promoted in its place, and every assert here silently reroutes onto tier 1
// while still reporting green.
var (
	_ beads.ConditionalAssignmentReleaser = (*recordingWriteWorkStore)(nil)
	_ beads.ConditionalWriter             = (*recordingWriteWorkStore)(nil)
)

// ReleaseIfCurrent reports the conditional verb as unsupported so these tests
// pin the tier-2 fallback write — the fields must match the raw release ops,
// and the write must be fenced. The conditional fast path emits a different
// (metadata-only) write by design and is covered in
// work_assignment_release_race_test.go.
// Without this override the embedded MemStore would promote its own
// implementation and silently move every assert onto the other path.
func (s *recordingWriteWorkStore) ReleaseIfCurrent(_, _ string) (bool, error) {
	return false, beads.ErrConditionalReleaseUnsupported
}

// seedWriteWorkBead puts the bead the façade is about to release into the
// backing store with live state matching the caller's snapshot, and returns the
// seeded row. The release path re-reads the bead immediately before writing and
// fences the write on that read's revision (the dr-huhn no-clobber guard), so a
// snapshot with no bead behind it is correctly refused. Seeding goes straight to
// the MemStore because the recorder's own Update deliberately does not delegate.
func seedWriteWorkBead(t *testing.T, rec *recordingWriteWorkStore, item beads.Bead) beads.Bead {
	t.Helper()
	rec.HonorExplicitIDs = true
	if _, err := rec.Create(beads.Bead{ID: item.ID, Title: "work", Metadata: item.Metadata}); err != nil {
		t.Fatalf("seed Create(%s): %v", item.ID, err)
	}
	status, assignee := item.Status, item.Assignee
	// Qualified on MemStore deliberately: the recorder's own Update records
	// without delegating, so seeding through it would leave the store empty and
	// pollute the recorded ops the asserts read.
	if err := rec.MemStore.Update(item.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("seed Update(%s): %v", item.ID, err)
	}
	got, err := rec.Get(item.ID)
	if err != nil {
		t.Fatalf("seed Get(%s): %v", item.ID, err)
	}
	if got.Status != item.Status || got.Assignee != item.Assignee {
		t.Fatalf("seed(%s) failed: status=%q assignee=%q, want %q/%q",
			item.ID, got.Status, got.Assignee, item.Status, item.Assignee)
	}
	return got
}

// assertFencedOn fails unless got is a fenced write on seeded's revision.
func assertFencedOn(t *testing.T, got recordedUpdate, seeded beads.Bead) {
	t.Helper()
	if !got.fenced || got.revision != seeded.Revision {
		t.Fatalf("release write fenced=%v revision=%d, want an UpdateIfMatch on the re-read revision %d", got.fenced, got.revision, seeded.Revision)
	}
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
// already-open bead writes {Assignee:"", Metadata:<clearedAffinity>} with NO
// Status change (status reset is only for in_progress), the raw release op's
// fields, as one write fenced on the re-read revision.
func TestWorkAssignmentReleaseWorkBead_OpenStaysOpen(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	item := beads.Bead{ID: "w-open", Status: "open", Assignee: "agent-1"}
	seeded := seedWriteWorkBead(t, rec, item)
	if err := wa.ReleaseWorkBead(item, ""); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	if len(rec.updates) != 1 {
		t.Fatalf("expected 1 write, got %d: %#v", len(rec.updates), rec.updates)
	}
	got := rec.updates[0]
	assertFencedOn(t, got, seeded)
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
// in_progress bead is reset to open on release, as the raw op did, in a fenced
// write.
func TestWorkAssignmentReleaseWorkBead_InProgressResetsToOpen(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	item := beads.Bead{ID: "w-ip", Status: "in_progress", Assignee: "agent-1"}
	seeded := seedWriteWorkBead(t, rec, item)
	if err := wa.ReleaseWorkBead(item, ""); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	if len(rec.updates) != 1 {
		t.Fatalf("expected 1 write, got %d: %#v", len(rec.updates), rec.updates)
	}
	got := rec.updates[0]
	assertFencedOn(t, got, seeded)
	if got.opts.Status == nil || *got.opts.Status != "open" {
		t.Fatalf("Status = %v, want open", got.opts.Status)
	}
	if derefStr(got.opts.Assignee) != "" {
		t.Fatalf("Assignee = %q, want empty-string clear", derefStr(got.opts.Assignee))
	}
}

// TestWorkAssignmentReleaseWorkBead_RunTargetFallbackApplied asserts the
// run_target fallback (used by the retire/unclaim path) is written only when the
// bead has neither run_target nor routed_to, as the raw op did, in the same
// fenced write.
func TestWorkAssignmentReleaseWorkBead_RunTargetFallbackApplied(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	item := beads.Bead{ID: "w-route", Status: "in_progress", Assignee: "agent-1"}
	seeded := seedWriteWorkBead(t, rec, item)
	if err := wa.ReleaseWorkBead(item, "worker"); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	if len(rec.updates) != 1 {
		t.Fatalf("expected 1 write, got %d: %#v", len(rec.updates), rec.updates)
	}
	got := rec.updates[0]
	assertFencedOn(t, got, seeded)
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
	seedWriteWorkBead(t, rec, item)
	if err := wa.ReleaseWorkBead(item, "worker"); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	got := rec.updates[0]
	if _, ok := got.opts.Metadata[beadmeta.RunTargetMetadataKey]; ok {
		t.Fatalf("run_target fallback must be skipped when routed_to present, got %#v", got.opts.Metadata)
	}
}

// TestWorkAssignmentReassignWorkBead_ByteIdentical asserts reassign emits only
// Assignee=&new, the fields of the raw retire-reassign op, delivered as a
// write fenced on the snapshot. The bead is seeded live because the reassign
// is conditional on the snapshot: an unseeded fixture verifies as stale and
// emits no write at all.
func TestWorkAssignmentReassignWorkBead_ByteIdentical(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	item := beads.Bead{ID: "w-1", Status: "in_progress", Assignee: "retired-session"}
	seedWriteWorkBead(t, rec, item)
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if err := wa.ReassignWorkBead(item, "new-session"); err != nil {
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
	if !got.fenced {
		t.Fatalf("reassign = %#v, want a write fenced on the snapshot (UpdateIfMatch), not a blind Update", got)
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
	if err := wa.ReassignWorkBead(beads.Bead{ID: "x"}, "y"); err != nil {
		t.Fatalf("nil store ReassignWorkBead: %v", err)
	}
	if err := wa.ClearDetachedProbe("x"); err != nil { // must not panic
		t.Fatalf("nil store ClearDetachedProbe: %v", err)
	}
	if _, err := wa.OpenAssignedToBasic("a", "open"); err != nil {
		t.Fatalf("nil store OpenAssignedToBasic: %v", err)
	}
}
