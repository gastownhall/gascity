package session

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// recordingStore seeds a session bead into a recording-fake store and
// returns the Store front door plus the recorder, so a test can assert the
// typed write method emitted byte-identical bead writes.
func recordingStore(t *testing.T, b beads.Bead) (*Store, *beadstest.RecordingStore) {
	t.Helper()
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	rec := beadstest.NewRecordingStore(mem)
	return NewStore(beads.SessionStore{Store: rec}), rec
}

// TestApplyPatchByteIdenticalToSetMetaBatch proves ApplyPatch emits exactly one
// SetMetadataBatch with the patch verbatim — the byte-identical replacement for
// setMetaBatch(store, id, patch).
func TestApplyPatchByteIdenticalToSetMetaBatch(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	patch := MetadataPatch{"state": "asleep", "last_woke_at": "", "sleep_reason": "max-age"}
	if err := is.ApplyPatch("s-1", patch); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}

	calls := rec.CallsForOp("SetMetadataBatch")
	if len(calls) != 1 {
		t.Fatalf("want 1 SetMetadataBatch, got %d", len(calls))
	}
	if calls[0].ID != "s-1" {
		t.Errorf("target id = %q, want s-1", calls[0].ID)
	}
	want := map[string]string{"state": "asleep", "last_woke_at": "", "sleep_reason": "max-age"}
	if !reflect.DeepEqual(calls[0].Metadata, want) {
		t.Errorf("batch = %#v, want %#v", calls[0].Metadata, want)
	}
}

// TestApplyPatchInfoPersistsAndFoldsEqualsReprojection proves ApplyPatchInfo
// persists the patch byte-identically (one SetMetadataBatch) AND returns the
// LOCAL fold — never a re-Get — and that the folded Info equals a full
// reprojection of the patched bead. This is the write-returns-Info contract the
// reconciler cuts over to in WI-5 W1.
func TestApplyPatchInfoPersistsAndFoldsEqualsReprojection(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{
		"state":                "creating",
		"pending_create_claim": "true",
		"last_woke_at":         "2026-01-01T00:00:00Z",
	})
	is, rec := recordingStore(t, b)

	pre, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get (pre): %v", err)
	}

	patch := MetadataPatch{"state": "asleep", "pending_create_claim": "", "last_woke_at": ""}
	got, err := is.ApplyPatchInfo(pre, patch)
	if err != nil {
		t.Fatalf("ApplyPatchInfo: %v", err)
	}

	// The persist must be a single byte-identical SetMetadataBatch.
	calls := rec.CallsForOp("SetMetadataBatch")
	if len(calls) != 1 {
		t.Fatalf("want 1 SetMetadataBatch, got %d", len(calls))
	}
	if calls[0].ID != "s-1" || !reflect.DeepEqual(calls[0].Metadata, map[string]string(patch)) {
		t.Errorf("persist = (%q, %#v), want (s-1, %#v)", calls[0].ID, calls[0].Metadata, map[string]string(patch))
	}

	// The returned Info is the local fold pre.ApplyPatch(patch)...
	if want := pre.ApplyPatch(patch); !reflect.DeepEqual(got, want) {
		t.Errorf("ApplyPatchInfo fold diverged from pre.ApplyPatch\n got=%+v\nwant=%+v", got, want)
	}
	// ...which is byte-identical to a full reprojection of the patched bead.
	if want := infoFromPersistedBead(reprojectBead(b, patch)); !reflect.DeepEqual(got, want) {
		t.Errorf("ApplyPatchInfo fold diverged from full reprojection\n got=%+v\nwant=%+v", got, want)
	}
}

// TestApplyPatchInfoEmptyIsNoOp proves an empty patch persists nothing and
// returns the input Info unchanged (matching ApplyPatch's len==0 short-circuit).
func TestApplyPatchInfoEmptyIsNoOp(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	pre, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get (pre): %v", err)
	}
	got, err := is.ApplyPatchInfo(pre, MetadataPatch{})
	if err != nil {
		t.Fatalf("ApplyPatchInfo: %v", err)
	}
	if !reflect.DeepEqual(got, pre) {
		t.Errorf("empty patch changed Info\n got=%+v\nwant=%+v", got, pre)
	}
	if n := len(rec.Calls()); n != 0 {
		t.Errorf("empty patch emitted %d calls, want 0", n)
	}
}

// TestApplyPatchInfoWriteErrorReturnsInputUnchanged proves that when the persist
// fails, ApplyPatchInfo returns the INPUT Info unchanged (no fold) plus the
// error — so a caller that ignores the error keeps a snapshot consistent with
// the store, and a caller that checks it can bail.
func TestApplyPatchInfoWriteErrorReturnsInputUnchanged(t *testing.T) {
	// A store with no such bead: SetMetadataBatch on a missing id errors.
	is := NewStore(seedSessionStore(t))
	pre := infoFromPersistedBead(sessionBeadFixture("missing", "open", map[string]string{"state": "active"}))

	got, err := is.ApplyPatchInfo(pre, MetadataPatch{"state": "asleep"})
	if err == nil {
		t.Fatal("ApplyPatchInfo(missing): want store error, got nil")
	}
	if !reflect.DeepEqual(got, pre) {
		t.Errorf("write error must return the input Info unchanged\n got=%+v\nwant=%+v", got, pre)
	}
}

// TestApplyPatchEmptyIsNoOp proves an empty patch emits no write (matching
// setMetaBatch's len==0 short-circuit).
func TestApplyPatchEmptyIsNoOp(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", nil)
	is, rec := recordingStore(t, b)

	if err := is.ApplyPatch("s-1", MetadataPatch{}); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if got := len(rec.Calls()); got != 0 {
		t.Errorf("empty patch emitted %d calls, want 0", got)
	}
}

// TestGetReflectsApplyPatch proves the store-authoritative refresh guarantee the
// session reconciler cuts over to in front-door Step 6: after a mutation
// persisted through ApplyPatch, a re-Get returns an Info reflecting that
// mutation. During the lockstep-coexistence phase (Steps 3-5) refreshSessionInfo
// refreshes the snapshot from the raw working copy instead (byte-identical by
// construction, and preserving the reconciler's deliberate intra-tick raw/store
// divergences like the reset_committed_at hiding); Step 6 removes the raw working
// set and makes Get the sole source. This test pins that Get sees a persisted
// write — the guarantee that Step-6 cutover depends on.
func TestGetReflectsApplyPatch(t *testing.T) {
	// A creating, still-claimed pending-create session — the shape whose lease
	// fields (state / pending_create_claim / last_woke_at) a heal-with-rollback
	// flips, and whose staleness would flip pendingCreateSessionStillLeasedInfo
	// in the reconciler's post-heal switch.
	b := sessionBeadFixture("s-1", "open", map[string]string{
		"state":                "creating",
		"pending_create_claim": "true",
		"last_woke_at":         "2026-01-01T00:00:00Z",
	})
	is, _ := recordingStore(t, b)

	pre, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get (pre): %v", err)
	}
	if pre.MetadataState != "creating" || !pre.PendingCreateClaim {
		t.Fatalf("pre Get = state %q claim %v, want creating/true", pre.MetadataState, pre.PendingCreateClaim)
	}

	// Persist a heal-shaped rollback: clear the claim and drop the lease markers.
	if err := is.ApplyPatch("s-1", MetadataPatch{
		"state":                "asleep",
		"pending_create_claim": "",
		"last_woke_at":         "",
	}); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}

	// The re-Get must reflect the persisted write — the refresh guarantee.
	post, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get (post): %v", err)
	}
	if post.MetadataState != "asleep" {
		t.Errorf("post MetadataState = %q, want asleep", post.MetadataState)
	}
	if post.PendingCreateClaim {
		t.Errorf("post PendingCreateClaim = true, want false (cleared)")
	}
	if post.LastWokeAt != "" {
		t.Errorf("post LastWokeAt = %q, want empty (cleared)", post.LastWokeAt)
	}
}

// TestSleepEmitsSleepPatch proves the typed Sleep method writes the row
// SleepPatch produces, keeping an operator's user-hold intent and a wait's
// intent the row carries (ApplySleep, D-J 3).
func TestSleepEmitsSleepPatch(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for intent, want := range map[string]string{"": "", "idle-stop-pending": "", "user-hold": "user-hold", "wait-hold": "wait-hold"} {
		b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active", "sleep_intent": intent})
		is, _ := recordingStore(t, b)
		if err := is.Sleep("s-1", "idle-timeout", now); err != nil {
			t.Fatalf("Sleep: %v", err)
		}
		got, err := is.Get("s-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.MetadataState != "asleep" || got.SleepReason != "idle-timeout" || got.SleptAt != now.Format(time.RFC3339) || got.SleepIntent != want {
			t.Errorf("intent %q: state=%q sleep_reason=%q slept_at=%q sleep_intent=%q, want SleepPatch with sleep_intent %q",
				intent, got.MetadataState, got.SleepReason, got.SleptAt, got.SleepIntent, want)
		}
	}
}

// TestSetWaitHoldClearWritesEmptyStrings proves clearing a wait hold writes
// the WaitEnd cell's move, fenced on a fresh read: the wait and its own
// intent, as empty strings, and no other key.
func TestSetWaitHoldClearWritesEmptyStrings(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"wait_hold": "x", "sleep_intent": "wait-hold"})
	is, rec := recordingStore(t, b)

	if err := is.SetWaitHold("s-1", false, ""); err != nil {
		t.Fatalf("SetWaitHold: %v", err)
	}
	calls := rec.CallsForOp("Update")
	if len(calls) != 1 {
		t.Fatalf("want 1 Update, got %d", len(calls))
	}
	want := map[string]string{"wait_hold": "", "sleep_intent": ""}
	if !reflect.DeepEqual(calls[0].Opts.Metadata, want) {
		t.Errorf("clear batch = %#v, want %#v", calls[0].Opts.Metadata, want)
	}
}

// TestCloseEmitsClosePatchThenClose proves Close stamps ClosePatch metadata and
// then closes the bead, in one transaction on a store without the atomic
// close (CommitClose), WITHOUT any work-reassignment side effect (that is
// Phase 6).
func TestCloseEmitsClosePatchThenClose(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	closed, err := is.Close(infoFromPersistedBead(b), "gc_swept", now)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Fatalf("Close reported not-closed for an open bead")
	}
	got, err := rec.Get("s-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	for k, v := range ClosePatch(now, "gc_swept") {
		if got.Metadata[k] != v {
			t.Errorf("metadata[%q] = %q, want %q", k, got.Metadata[k], v)
		}
	}
}

// TestCloseAlreadyClosedIsNoOp proves Close on a closed bead emits no writes.
func TestCloseAlreadyClosedIsNoOp(t *testing.T) {
	b := sessionBeadFixture("s-1", "closed", nil)
	is, rec := recordingStore(t, b)

	closed, err := is.Close(infoFromPersistedBead(b), "gc_swept", time.Now())
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed {
		t.Errorf("Close reported closed for an already-closed bead")
	}
	if got := len(rec.Calls()); got != 0 {
		t.Errorf("Close on closed bead emitted %d writes, want 0", got)
	}
}

// TestGetStateProjectsState proves GetState returns the persisted state/closed
// without raw beads crossing the boundary.
func TestGetStateProjectsState(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "asleep"})
	is, _ := recordingStore(t, b)

	state, closed, err := is.GetState("s-1")
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state != StateAsleep || closed {
		t.Errorf("GetState = (%q, %v), want (asleep, false)", state, closed)
	}
}

// TestSetMarkerEmitsSingleKeySetMetadata proves SetMarker emits exactly one
// single-key SetMetadata op — byte-identical to the raw store.SetMetadata
// single-key write it replaces (stranded marker, sleep_intent clear, cmd_stop
// sleep_reason), NOT a SetMetadataBatch.
func TestSetMarkerEmitsSingleKeySetMetadata(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	if err := is.SetMarker("s-1", "sleep_reason", "city-stop"); err != nil {
		t.Fatalf("SetMarker: %v", err)
	}
	gotOps := opsOf(rec.Calls())
	if !reflect.DeepEqual(gotOps, []string{"SetMetadata"}) {
		t.Fatalf("SetMarker ops = %v, want [SetMetadata]", gotOps)
	}
	c := rec.CallsForOp("SetMetadata")[0]
	if c.ID != "s-1" || c.Key != "sleep_reason" || c.Value != "city-stop" {
		t.Errorf("SetMarker call = (%q,%q,%q), want (s-1,sleep_reason,city-stop)", c.ID, c.Key, c.Value)
	}
}

// TestSetMarkerEmptyValueClears proves SetMarker writes an empty string verbatim
// (the empty-string-clear contract) via a single SetMetadata op.
func TestSetMarkerEmptyValueClears(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"sleep_intent": "idle-stop-pending"})
	is, rec := recordingStore(t, b)

	if err := is.SetMarker("s-1", "sleep_intent", ""); err != nil {
		t.Fatalf("SetMarker: %v", err)
	}
	c := rec.CallsForOp("SetMetadata")
	if len(c) != 1 || c[0].Key != "sleep_intent" || c[0].Value != "" {
		t.Fatalf("SetMarker clear = %#v, want one SetMetadata(sleep_intent,\"\")", c)
	}
}

// TestRecordCurrentBeadEmitsSingleKeySetMetadata proves RecordCurrentBead emits
// a single-key SetMetadata of CurrentBeadIDKey — byte-identical to
// recordCurrentBeadIDOnWake's raw store.SetMetadata write (NOT a batch).
func TestRecordCurrentBeadEmitsSingleKeySetMetadata(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", nil)
	is, rec := recordingStore(t, b)

	if err := is.RecordCurrentBead("s-1", "gcg-42"); err != nil {
		t.Fatalf("RecordCurrentBead: %v", err)
	}
	gotOps := opsOf(rec.Calls())
	if !reflect.DeepEqual(gotOps, []string{"SetMetadata"}) {
		t.Fatalf("RecordCurrentBead ops = %v, want [SetMetadata]", gotOps)
	}
	c := rec.CallsForOp("SetMetadata")[0]
	if c.ID != "s-1" || c.Key != CurrentBeadIDKey || c.Value != "gcg-42" {
		t.Errorf("RecordCurrentBead call = (%q,%q,%q), want (s-1,%q,gcg-42)", c.ID, c.Key, c.Value, CurrentBeadIDKey)
	}
}

// TestReserveCurrentClaimWritesTheClaimKey proves the reserve writes
// beadmeta.CurrentClaimBeadIDMetadataKey — deliberately a DIFFERENT key from
// RecordCurrentBead's, so the self-claim lane and the reconciler's wake-time
// assignment lane cannot clobber each other.
func TestReserveCurrentClaimWritesTheClaimKey(t *testing.T) {
	is, mem, _ := currentClaimFixture(t, "")

	outcome, err := is.ReserveCurrentClaim("s-1", "", "gcg-42")
	if err != nil {
		t.Fatalf("ReserveCurrentClaim: %v", err)
	}
	if outcome != CurrentClaimSwapped {
		t.Fatalf("ReserveCurrentClaim = %q, want %q", outcome, CurrentClaimSwapped)
	}
	if got := currentClaimPointer(t, mem); got != "gcg-42" {
		t.Errorf("pointer = %q, want gcg-42", got)
	}
	b, err := mem.Get("s-1")
	if err != nil {
		t.Fatalf("reading session bead: %v", err)
	}
	if v, ok := b.Metadata[CurrentBeadIDKey]; ok {
		t.Errorf("ReserveCurrentClaim wrote the reconciler's %q key (%q)", CurrentBeadIDKey, v)
	}
}

// TestDropCurrentClaimClearsAnyValue proves the cascades' unfenced clear empties
// the pointer whatever it names through one single-key SetMetadata, and writes
// nothing when it is already empty.
func TestDropCurrentClaimClearsAnyValue(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: "gcg-42"})
	is, rec := recordingStore(t, b)

	if err := is.DropCurrentClaim("s-1"); err != nil {
		t.Fatalf("DropCurrentClaim: %v", err)
	}
	c := rec.CallsForOp("SetMetadata")
	if len(c) != 1 || c[0].Key != beadmeta.CurrentClaimBeadIDMetadataKey || c[0].Value != "" {
		t.Fatalf("clear = %#v, want one SetMetadata(%s,\"\")", c, beadmeta.CurrentClaimBeadIDMetadataKey)
	}
	if n := len(rec.CallsForOp("SetMetadataBatch")); n != 0 {
		t.Errorf("DropCurrentClaim emitted %d batch writes, want a single-key write", n)
	}
	got, err := is.CurrentClaimBeadID("s-1")
	if err != nil {
		t.Fatalf("CurrentClaimBeadID: %v", err)
	}
	if got != "" {
		t.Errorf("CurrentClaimBeadID after drop = %q, want empty", got)
	}

	if err := is.DropCurrentClaim("s-1"); err != nil {
		t.Fatalf("DropCurrentClaim (empty): %v", err)
	}
	if n := len(rec.CallsForOp("SetMetadata")); n != 1 {
		t.Errorf("dropping an empty pointer wrote; SetMetadata calls = %d, want 1", n)
	}
}

// TestCurrentClaimBeadIDReadsTheStampedValue proves the read half of the
// back-channel returns the stamped id verbatim, and empty when nothing is
// stamped.
func TestCurrentClaimBeadIDReadsTheStampedValue(t *testing.T) {
	is, _ := recordingStore(t, sessionBeadFixture("s-1", "open",
		map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: "gcg-42"}))
	got, err := is.CurrentClaimBeadID("s-1")
	if err != nil {
		t.Fatalf("CurrentClaimBeadID: %v", err)
	}
	if got != "gcg-42" {
		t.Errorf("CurrentClaimBeadID = %q, want gcg-42", got)
	}

	bare, _ := recordingStore(t, sessionBeadFixture("s-2", "open", nil))
	got, err = bare.CurrentClaimBeadID("s-2")
	if err != nil {
		t.Fatalf("CurrentClaimBeadID (unstamped): %v", err)
	}
	if got != "" {
		t.Errorf("CurrentClaimBeadID (unstamped) = %q, want empty", got)
	}
}

// TestCurrentClaimRefusesANonSessionBead proves both halves route through
// validatedBead: a present-but-non-session bead is refused with
// ErrSessionNotFound BEFORE any write, so bd's fuzzy id resolver cannot redirect
// the claim stamp onto a foreign bead.
func TestCurrentClaimRefusesANonSessionBead(t *testing.T) {
	mem := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "wb-1", Status: "open", Title: "work"}}, nil)
	rec := beadstest.NewRecordingStore(mem)
	is := NewStore(beads.SessionStore{Store: rec})

	if _, err := is.ReserveCurrentClaim("wb-1", "", "gcg-42"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ReserveCurrentClaim on a non-session bead = %v, want ErrSessionNotFound", err)
	}
	if err := is.DropCurrentClaim("wb-1"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("DropCurrentClaim on a non-session bead = %v, want ErrSessionNotFound", err)
	}
	if n := len(rec.CallsForOp("SetMetadata")); n != 0 {
		t.Errorf("refused stamp still wrote %d times", n)
	}
	if _, err := is.CurrentClaimBeadID("wb-1"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("CurrentClaimBeadID on a non-session bead = %v, want ErrSessionNotFound", err)
	}
}

// TestCloseWithoutReasonEmitsSingleClose proves CloseWithoutReason emits exactly
// one Close op and no metadata write — byte-identical to closeBead's raw
// store.Close(id) after it stamps ClosePatch separately.
func TestCloseWithoutReasonEmitsSingleClose(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	if err := is.CloseWithoutReason("s-1"); err != nil {
		t.Fatalf("CloseWithoutReason: %v", err)
	}
	gotOps := opsOf(rec.Calls())
	if !reflect.DeepEqual(gotOps, []string{"Close"}) {
		t.Fatalf("CloseWithoutReason ops = %v, want [Close]", gotOps)
	}
	if rec.CallsForOp("Close")[0].ID != "s-1" {
		t.Errorf("Close target = %q, want s-1", rec.CallsForOp("Close")[0].ID)
	}
}

// TestSetStatusOpenEmitsStatusOnlyUpdate proves SetStatusOpen emits exactly one
// Update with Status="open" and the runtime lease clear (a reopened row holds
// no lease), and nothing else.
func TestSetStatusOpenEmitsStatusOnlyUpdate(t *testing.T) {
	b := sessionBeadFixture("s-1", "closed", map[string]string{"state": "archived"})
	is, rec := recordingStore(t, b)

	if err := is.SetStatusOpen("s-1"); err != nil {
		t.Fatalf("SetStatusOpen: %v", err)
	}
	gotOps := opsOf(rec.Calls())
	if !reflect.DeepEqual(gotOps, []string{"Update"}) {
		t.Fatalf("SetStatusOpen ops = %v, want [Update]", gotOps)
	}
	c := rec.CallsForOp("Update")[0]
	if c.ID != "s-1" {
		t.Errorf("Update target = %q, want s-1", c.ID)
	}
	if c.Opts.Status == nil || *c.Opts.Status != "open" {
		t.Errorf("Update Status = %v, want open", c.Opts.Status)
	}
	if c.Opts.Type != nil || c.Opts.Labels != nil || !reflect.DeepEqual(c.Opts.Metadata, map[string]string(RuntimeLeaseClearPatch())) {
		t.Errorf("Update set fields beyond Status and the runtime lease clear: %#v", c.Opts)
	}
}

// TestRepairTypeEmitsTypeOnlyUpdate proves RepairType emits exactly one Update
// with only Type set to the canonical session bead type — byte-identical to the
// raw store.Update(id, UpdateOpts{Type: &"session"}) empty-type repair write.
func TestRepairTypeEmitsTypeOnlyUpdate(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", nil)
	b.Type = ""
	is, rec := recordingStore(t, b)

	if err := is.RepairType("s-1"); err != nil {
		t.Fatalf("RepairType: %v", err)
	}
	gotOps := opsOf(rec.Calls())
	if !reflect.DeepEqual(gotOps, []string{"Update"}) {
		t.Fatalf("RepairType ops = %v, want [Update]", gotOps)
	}
	c := rec.CallsForOp("Update")[0]
	if c.Opts.Type == nil || *c.Opts.Type != BeadType {
		t.Errorf("Update Type = %v, want %q", c.Opts.Type, BeadType)
	}
	if c.Opts.Status != nil || c.Opts.Metadata != nil {
		t.Errorf("Update set fields beyond Type: %#v", c.Opts)
	}
}

// TestCircuitResetGenerationReturnsPersistedValue proves the typed read returns
// the persisted reset-generation metadata value verbatim — equivalent to the raw
// store.Get(id) + read .Metadata[SessionCircuitResetGenerationMetadataKey] that
// loadPersistedSessionCircuitResetGeneration performed.
func TestCircuitResetGenerationReturnsPersistedValue(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{
		SessionCircuitResetGenerationMetadataKey: "7",
	})
	is, rec := recordingStore(t, b)

	got, err := is.CircuitResetGeneration("s-1")
	if err != nil {
		t.Fatalf("CircuitResetGeneration: %v", err)
	}
	if want := b.Metadata[SessionCircuitResetGenerationMetadataKey]; got != want {
		t.Errorf("CircuitResetGeneration = %q, want %q (the raw read value)", got, want)
	}
	// The read confines a single Get; it must not emit any mutating bead op.
	if mutating := opsOf(rec.Calls()); len(mutating) != 0 {
		t.Errorf("CircuitResetGeneration emitted mutating ops %v, want none", mutating)
	}
}

// TestCircuitResetGenerationEmptyWhenUnset proves an unset key reads back as the
// empty string (not an error) — matching the raw map read on a bead that never
// stamped the generation.
func TestCircuitResetGenerationEmptyWhenUnset(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, _ := recordingStore(t, b)

	got, err := is.CircuitResetGeneration("s-1")
	if err != nil {
		t.Fatalf("CircuitResetGeneration: %v", err)
	}
	if got != "" {
		t.Errorf("CircuitResetGeneration on unset key = %q, want \"\"", got)
	}
}

// TestCircuitResetGenerationSurfacesStoreError proves a missing bead surfaces the
// bare store error (the caller owns its diagnostic wrapping), matching the raw
// store.Get error path the front door replaces.
func TestCircuitResetGenerationSurfacesStoreError(t *testing.T) {
	store := seedSessionStore(t)
	is := NewStore(store)
	if _, err := is.CircuitResetGeneration("missing"); err == nil {
		t.Fatal("CircuitResetGeneration(missing): want store error, got nil")
	}
}

// TestPersistedMarkersReturnsVerbatimValues proves the typed read returns the
// persisted session_name / continuation_epoch / sleep_reason /
// continuation_reset_pending metadata values verbatim — equivalent to the raw store.Get(id) + read .Metadata[...] the
// cmd_wait registration and retry/clear paths performed. It performs no bead
// validation (matching the raw reads) and emits no mutating op.
func TestPersistedMarkersReturnsVerbatimValues(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{
		"__title":                    "My Session",
		"session_name":               "polecat-1",
		"continuation_epoch":         "4",
		"sleep_reason":               "wait-hold",
		"continuation_reset_pending": "true",
	})
	is, rec := recordingStore(t, b)

	got, err := is.PersistedMarkers("s-1")
	if err != nil {
		t.Fatalf("PersistedMarkers: %v", err)
	}
	want := PersistedMarkers{
		Title:                    "My Session",
		SessionName:              "polecat-1",
		ContinuationEpoch:        "4",
		SleepReason:              "wait-hold",
		ContinuationResetPending: "true",
	}
	if got != want {
		t.Errorf("PersistedMarkers = %#v, want %#v (the raw read values)", got, want)
	}
	// The read confines a single Get; it must not emit any mutating bead op.
	if mutating := opsOf(rec.Calls()); len(mutating) != 0 {
		t.Errorf("PersistedMarkers emitted mutating ops %v, want none", mutating)
	}
}

// TestPersistedMarkersEmptyWhenUnset proves unset keys read back as empty
// strings (not an error), matching the raw map read on a bead that never
// stamped those markers.
func TestPersistedMarkersEmptyWhenUnset(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, _ := recordingStore(t, b)

	got, err := is.PersistedMarkers("s-1")
	if err != nil {
		t.Fatalf("PersistedMarkers: %v", err)
	}
	if got != (PersistedMarkers{}) {
		t.Errorf("PersistedMarkers on unset keys = %#v, want zero value", got)
	}
}

// TestPersistedMarkersSurfacesStoreError proves a missing bead surfaces the bare
// store error (the caller owns its diagnostic wrapping), matching the raw
// store.Get error path the front door replaces.
func TestPersistedMarkersSurfacesStoreError(t *testing.T) {
	store := seedSessionStore(t)
	is := NewStore(store)
	if _, err := is.PersistedMarkers("missing"); err == nil {
		t.Fatal("PersistedMarkers(missing): want store error, got nil")
	}
}

func opsOf(calls []beadstest.RecordedCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Op)
	}
	return out
}

// updateFailStore is a beads.Store whose Update always fails; every other op
// delegates to the embedded store. It proves UpdateMetadataInfo's all-or-nothing
// contract: a rejected Update must leave both the durable row and the caller's
// Info untouched.
type updateFailStore struct {
	beads.Store
	err error
}

func (s updateFailStore) Update(string, beads.UpdateOpts) error { return s.err }

// TestUpdateMetadataInfoEmitsSingleUpdateWithFullPatch pins the one-operation
// contract for the pool trigger/provenance cluster (council finding 1): the whole
// patch is written in exactly ONE Store.Update carrying the full metadata map —
// NOT decomposed into per-key SetMetadata / SetMetadataBatch ops, whose per-key
// decomposition on exec:/partial-write backends could commit a mixed provenance
// row. On success the returned Info equals the local fold pre.ApplyPatch(patch).
func TestUpdateMetadataInfoEmitsSingleUpdateWithFullPatch(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	pre, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	patch := MetadataPatch{
		beadmeta.TriggerBeadIDMetadataKey:       "gcg-123",
		beadmeta.TriggerBeadStoreRefMetadataKey: "rig-a",
		beadmeta.BrainParentSIDMetadataKey:      "sid-parent",
		beadmeta.PackMetadataKey:                "packs/x",
		beadmeta.PackWorkspaceMetadataKey:       "ws-1",
		beadmeta.WorkDirMetadataKey:             "/work/dir",
	}

	got, err := is.UpdateMetadataInfo(pre, patch)
	if err != nil {
		t.Fatalf("UpdateMetadataInfo: %v", err)
	}

	updates := rec.CallsForOp("Update")
	if len(updates) != 1 {
		t.Fatalf("want exactly 1 Update op, got %d (all ops: %v)", len(updates), opsOf(rec.Calls()))
	}
	if updates[0].ID != "s-1" {
		t.Errorf("Update target id = %q, want s-1", updates[0].ID)
	}
	if !reflect.DeepEqual(updates[0].Opts.Metadata, map[string]string(patch)) {
		t.Errorf("Update metadata = %#v, want the FULL patch %#v", updates[0].Opts.Metadata, map[string]string(patch))
	}
	// One-operation contract: no per-key decomposition.
	if n := len(rec.CallsForOp("SetMetadata")); n != 0 {
		t.Errorf("SetMetadata ops = %d, want 0 (one-Update contract)", n)
	}
	if n := len(rec.CallsForOp("SetMetadataBatch")); n != 0 {
		t.Errorf("SetMetadataBatch ops = %d, want 0 (one-Update contract)", n)
	}
	// Success folds the patch onto Info, byte-identical to a local ApplyPatch.
	if want := pre.ApplyPatch(patch); !reflect.DeepEqual(got, want) {
		t.Errorf("returned Info = %#v, want local fold %#v", got, want)
	}
	if got.TriggerBeadID != "gcg-123" || got.Pack != "packs/x" || got.WorkDirCanonical != "/work/dir" {
		t.Errorf("returned Info did not fold the trigger cluster: %+v", got)
	}
}

// TestUpdateMetadataInfoFailedWritePersistsNothingAndReturnsInputUnchanged proves
// the all-or-nothing guarantee: when the single Update fails, NOTHING is persisted
// (the durable row keeps its pre-write metadata) and the returned Info is the
// INPUT unchanged, so a log-and-continue caller never advances onto a half-applied
// provenance cluster (council finding 1).
func TestUpdateMetadataInfoFailedWritePersistsNothingAndReturnsInputUnchanged(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	is := NewStore(beads.SessionStore{Store: updateFailStore{Store: mem, err: errors.New("update rejected")}})

	pre, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	patch := MetadataPatch{
		beadmeta.TriggerBeadIDMetadataKey:       "gcg-123",
		beadmeta.TriggerBeadStoreRefMetadataKey: "rig-a",
		beadmeta.BrainParentSIDMetadataKey:      "sid-parent",
	}

	got, err := is.UpdateMetadataInfo(pre, patch)
	if err == nil {
		t.Fatal("UpdateMetadataInfo: want error on failed Update, got nil")
	}
	// Returned Info is the input UNCHANGED — no partial fold.
	if !reflect.DeepEqual(got, pre) {
		t.Errorf("returned Info = %#v, want INPUT unchanged %#v", got, pre)
	}
	// Nothing persisted: the durable row still has none of the cluster keys.
	after, err := mem.Get("s-1")
	if err != nil {
		t.Fatalf("Get after failed update: %v", err)
	}
	for k := range patch {
		if v := after.Metadata[k]; v != "" {
			t.Errorf("durable row key %q = %q after failed Update, want unset (all-or-nothing)", k, v)
		}
	}
}

// TestStoreBacked: a front door over a store is backed; a nil door and a door
// over the zero handle are not.
func TestStoreBacked(t *testing.T) {
	if !NewStore(beadstest.Sessions(beads.NewMemStore())).Backed() {
		t.Error("a door over a store reports unbacked")
	}
	if NewStore(beads.SessionStore{}).Backed() {
		t.Error("a door over the zero handle reports backed")
	}
	var nilDoor *Store
	if nilDoor.Backed() {
		t.Error("a nil door reports backed")
	}
}

// currentClaimFixture seeds one session bead whose current-claim pointer reads
// pointer ("" leaves the key absent) into a MemStore, which offers metadata
// CAS, behind an OpRecordingStore so a test can count the ops a writer issued.
func currentClaimFixture(t *testing.T, pointer string) (*Store, *beads.MemStore, *beadstest.OpRecordingStore) {
	t.Helper()
	var meta map[string]string
	if pointer != "" {
		meta = map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: pointer}
	}
	mem := beads.NewMemStoreFrom(1, []beads.Bead{sessionBeadFixture("s-1", "open", meta)}, nil)
	rec := beadstest.NewOpRecordingStore(mem)
	return NewStore(beads.SessionStore{Store: rec}), mem, rec
}

// currentClaimPointer reads the raw pointer straight off the backing store.
func currentClaimPointer(t *testing.T, mem *beads.MemStore) string {
	t.Helper()
	b, err := mem.Get("s-1")
	if err != nil {
		t.Fatalf("reading session bead: %v", err)
	}
	return b.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey]
}

// currentClaimWrites counts the metadata writes a recorder saw, the
// compare-and-set included.
func currentClaimWrites(rec *beadstest.OpRecordingStore) map[string]int {
	out := map[string]int{}
	for _, op := range rec.Ops() {
		switch op.Kind {
		case beadstest.OpSetMetadata, beadstest.OpSetMetadataBatch, beadstest.OpCompareAndSetMetadataKey:
			out[op.Kind]++
		}
	}
	return out
}

// TestReserveCurrentClaimSwapThenConflict is AC-1: two reserves that both read
// the pointer as "X" race on a store that offers metadata CAS. The first swaps
// it to its bead; the second is told it lost and writes nothing, so the pointer
// keeps naming the first claim.
func TestReserveCurrentClaimSwapThenConflict(t *testing.T) {
	is, mem, rec := currentClaimFixture(t, "X")

	first, err := is.ReserveCurrentClaim("s-1", "X", "A")
	if err != nil {
		t.Fatalf("first ReserveCurrentClaim: %v", err)
	}
	if first != CurrentClaimSwapped {
		t.Fatalf("first reserve = %q, want %q", first, CurrentClaimSwapped)
	}
	rec.Reset()
	second, err := is.ReserveCurrentClaim("s-1", "X", "B")
	if err != nil {
		t.Fatalf("second ReserveCurrentClaim: %v", err)
	}
	if second != CurrentClaimConflict {
		t.Fatalf("second reserve = %q, want %q", second, CurrentClaimConflict)
	}
	if w := currentClaimWrites(rec); w[beadstest.OpSetMetadata] != 0 || w[beadstest.OpSetMetadataBatch] != 0 {
		t.Errorf("losing reserve wrote unconditionally: %v", w)
	}
	if got := currentClaimPointer(t, mem); got != "A" {
		t.Fatalf("pointer = %q, want A (the winner's stamp)", got)
	}
}

// TestReserveCurrentClaimRefusesEmptyBeadID pins that a reserve must name the
// bead it claims: clearing the pointer is ClearCurrentClaim's job, so an empty
// reserve cannot launder a clear past the expected-value fence.
func TestReserveCurrentClaimRefusesEmptyBeadID(t *testing.T) {
	is, mem, rec := currentClaimFixture(t, "X")

	if _, err := is.ReserveCurrentClaim("s-1", "X", ""); err == nil {
		t.Fatal("ReserveCurrentClaim with an empty bead id = nil error, want a refusal")
	}
	if writes := currentClaimWrites(rec); len(writes) != 0 {
		t.Fatalf("issued writes: %v, want none", writes)
	}
	if p := currentClaimPointer(t, mem); p != "X" {
		t.Fatalf("pointer = %q, want %q", p, "X")
	}
}

// casInterleavingStore runs between before the first compare-and-set of the
// current-claim key to trigger lands, standing in for a second claim process
// that read the same pointer and swapped first. It offers the metadata CAS
// capability itself, forwarding to the wrapped MemStore.
type casInterleavingStore struct {
	beads.Store
	trigger string
	between func()
	fired   bool
}

func (s *casInterleavingStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if !s.fired && key == beadmeta.CurrentClaimBeadIDMetadataKey && next == s.trigger {
		s.fired = true
		s.between()
	}
	return s.Store.(beads.MetadataCASWriter).CompareAndSetMetadataKey(id, key, expected, next)
}

// TestReserveCurrentClaimInterleavedClaimersExactlyOneWins is the #6110
// reproduction: claimer A has read the pointer and is about to write when
// claimer B reads the same value and writes first. Exactly one of them may be
// told its stamp landed, and the pointer names that one.
func TestReserveCurrentClaimInterleavedClaimersExactlyOneWins(t *testing.T) {
	mem := beads.NewMemStoreFrom(1, []beads.Bead{sessionBeadFixture("s-1", "open", nil)}, nil)
	wrapped := &casInterleavingStore{Store: mem, trigger: "gcg-A"}
	is := NewStore(beads.SessionStore{Store: wrapped})

	var outB CurrentClaimOutcome
	var errB error
	wrapped.between = func() { outB, errB = is.ReserveCurrentClaim("s-1", "", "gcg-B") }
	outA, errA := is.ReserveCurrentClaim("s-1", "", "gcg-A")
	if !wrapped.fired {
		t.Fatal("interleaving hook never fired; the test did not exercise the race")
	}
	if errA != nil || errB != nil {
		t.Fatalf("reserve errors: A=%v B=%v", errA, errB)
	}
	if outA != CurrentClaimConflict || outB != CurrentClaimSwapped {
		t.Fatalf("outcomes A=%q B=%q, want A conflict and B swapped", outA, outB)
	}
	if got := currentClaimPointer(t, mem); got != "gcg-B" {
		t.Fatalf("pointer = %q, want gcg-B (the one claimer told it won)", got)
	}
}

// TestReserveCurrentClaimConcurrentSingleWinner races sixteen reserves that all
// read the pointer as "X": exactly one swaps, every other one conflicts, and the
// pointer names the winner.
func TestReserveCurrentClaimConcurrentSingleWinner(t *testing.T) {
	is, mem, _ := currentClaimFixture(t, "X")
	const n = 16
	outcomes := make([]CurrentClaimOutcome, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i], errs[i] = is.ReserveCurrentClaim("s-1", "X", fmt.Sprintf("b%d", i))
		}(i)
	}
	wg.Wait()
	winner := ""
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("reserve %d: %v", i, errs[i])
		}
		switch outcomes[i] {
		case CurrentClaimSwapped:
			if winner != "" {
				t.Fatalf("two reserves swapped: %s and b%d", winner, i)
			}
			winner = fmt.Sprintf("b%d", i)
		case CurrentClaimConflict:
		default:
			t.Fatalf("reserve %d = %q, want swapped or conflict", i, outcomes[i])
		}
	}
	if winner == "" {
		t.Fatal("no reserve swapped")
	}
	if got := currentClaimPointer(t, mem); got != winner {
		t.Fatalf("pointer = %q, want the winner %s", got, winner)
	}
}

// TestClearCurrentClaimLeavesLaterStamp is AC-2: a rollback that clears the
// pointer against its own stamp "A" after another claim replaced it with "B" is
// told it lost, writes nothing, and leaves the later winner's stamp in place.
func TestClearCurrentClaimLeavesLaterStamp(t *testing.T) {
	is, mem, rec := currentClaimFixture(t, "B")

	outcome, err := is.ClearCurrentClaim("s-1", "A")
	if err != nil {
		t.Fatalf("ClearCurrentClaim: %v", err)
	}
	if outcome != CurrentClaimConflict {
		t.Fatalf("clear = %q, want %q", outcome, CurrentClaimConflict)
	}
	if w := currentClaimWrites(rec); w[beadstest.OpSetMetadata] != 0 || w[beadstest.OpSetMetadataBatch] != 0 {
		t.Errorf("losing clear wrote unconditionally: %v", w)
	}
	if got := currentClaimPointer(t, mem); got != "B" {
		t.Fatalf("pointer = %q, want B (the later winner's stamp)", got)
	}
}

// TestClearCurrentClaimClearsOwnStamp is the control for AC-2: a clear whose
// expected value is still the pointer empties it.
func TestClearCurrentClaimClearsOwnStamp(t *testing.T) {
	is, mem, _ := currentClaimFixture(t, "A")

	outcome, err := is.ClearCurrentClaim("s-1", "A")
	if err != nil {
		t.Fatalf("ClearCurrentClaim: %v", err)
	}
	if outcome != CurrentClaimSwapped {
		t.Fatalf("clear = %q, want %q", outcome, CurrentClaimSwapped)
	}
	if got := currentClaimPointer(t, mem); got != "" {
		t.Fatalf("pointer = %q, want empty", got)
	}
}

// TestClearCurrentClaimRefusesEmptyExpected: a clear must name the stamp it
// removes, or it is the unconditional clear that erases a later winner.
func TestClearCurrentClaimRefusesEmptyExpected(t *testing.T) {
	is, mem, rec := currentClaimFixture(t, "B")

	if _, err := is.ClearCurrentClaim("s-1", " "); err == nil {
		t.Fatal("ClearCurrentClaim with an empty expected value succeeded, want an error")
	}
	if w := currentClaimWrites(rec); len(w) != 0 {
		t.Errorf("refused clear issued writes: %v", w)
	}
	if got := currentClaimPointer(t, mem); got != "B" {
		t.Fatalf("pointer = %q, want B", got)
	}
}

// TestReserveCurrentClaimUnchangedWritesNothing is AC-3: the claim path
// re-serves an adopted bead on every hook tick, so a reserve for the bead the
// pointer already names reports CurrentClaimUnchanged and issues no write of
// any kind — not even a compare-and-set — whatever the expected value.
func TestReserveCurrentClaimUnchangedWritesNothing(t *testing.T) {
	for _, expected := range []string{"A", "X", ""} {
		t.Run("expected="+expected, func(t *testing.T) {
			is, mem, rec := currentClaimFixture(t, "A")

			outcome, err := is.ReserveCurrentClaim("s-1", expected, "A")
			if err != nil {
				t.Fatalf("ReserveCurrentClaim: %v", err)
			}
			if outcome != CurrentClaimUnchanged {
				t.Fatalf("reserve = %q, want %q", outcome, CurrentClaimUnchanged)
			}
			if w := currentClaimWrites(rec); len(w) != 0 {
				t.Errorf("unchanged reserve issued writes: %v", w)
			}
			if got := currentClaimPointer(t, mem); got != "A" {
				t.Fatalf("pointer = %q, want A", got)
			}
		})
	}
}

// legacyCurrentClaimFixture builds the session store over a backing store that
// cannot fence the pointer: either the wrapper hides the metadata-CAS
// capability entirely, or the CAS it offers refuses with
// beads.ErrConditionalWriteUnsupported (DoltLite's read store refuses every
// conditional verb).
func legacyCurrentClaimFixture(t *testing.T, pointer string, refusingCAS bool) (*Store, *beads.MemStore, *beadstest.OpRecordingStore) {
	t.Helper()
	var meta map[string]string
	if pointer != "" {
		meta = map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: pointer}
	}
	mem := beads.NewMemStoreFrom(1, []beads.Bead{sessionBeadFixture("s-1", "open", meta)}, nil)
	rec := beadstest.NewOpRecordingStore(mem)
	if refusingCAS {
		mem.DisableConditionalWrites = true
		return NewStore(beads.SessionStore{Store: rec}), mem, rec
	}
	// An interface-embedding wrapper promotes no optional capability and
	// declares no resolution target, so the CAS is out of reach.
	return NewStore(beads.SessionStore{Store: struct{ beads.Store }{rec}}), mem, rec
}

// TestReserveCurrentClaimLegacyStore is AC-4: on a store without metadata CAS
// the reserve still compares against the caller's expected value, writing one
// single-key SetMetadata when the pointer holds it and nothing when it does not.
func TestReserveCurrentClaimLegacyStore(t *testing.T) {
	for _, tc := range []struct {
		name        string
		refusingCAS bool
	}{
		{name: "no CAS capability"},
		{name: "CAS refuses as unsupported", refusingCAS: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("pointer holds expected", func(t *testing.T) {
				is, mem, rec := legacyCurrentClaimFixture(t, "X", tc.refusingCAS)
				got, err := is.ReserveCurrentClaim("s-1", "X", "A")
				if err != nil {
					t.Fatalf("ReserveCurrentClaim: %v", err)
				}
				if got != CurrentClaimLegacy {
					t.Fatalf("reserve = %q, want %q", got, CurrentClaimLegacy)
				}
				writes := currentClaimWrites(rec)
				if writes[beadstest.OpSetMetadata] != 1 || writes[beadstest.OpSetMetadataBatch] != 0 {
					t.Fatalf("issued writes: %v, want exactly one SetMetadata", writes)
				}
				if p := currentClaimPointer(t, mem); p != "A" {
					t.Fatalf("pointer = %q, want %q", p, "A")
				}
			})
			t.Run("pointer moved off expected", func(t *testing.T) {
				is, mem, rec := legacyCurrentClaimFixture(t, "B", tc.refusingCAS)
				got, err := is.ReserveCurrentClaim("s-1", "X", "A")
				if err != nil {
					t.Fatalf("ReserveCurrentClaim: %v", err)
				}
				if got != CurrentClaimConflict {
					t.Fatalf("reserve = %q, want %q", got, CurrentClaimConflict)
				}
				writes := currentClaimWrites(rec)
				if writes[beadstest.OpSetMetadata] != 0 || writes[beadstest.OpSetMetadataBatch] != 0 {
					t.Fatalf("issued writes: %v, want none", writes)
				}
				if p := currentClaimPointer(t, mem); p != "B" {
					t.Fatalf("pointer = %q, want %q", p, "B")
				}
			})
		})
	}
}

// TestClearCurrentClaimLegacyStore pins the rollback clear on a store without
// metadata CAS: it writes the empty value with one SetMetadata only while the
// raw stored value still equals the stamp it was asked to remove, and a stale
// clear writes nothing and leaves the later stamp.
func TestClearCurrentClaimLegacyStore(t *testing.T) {
	for _, tc := range []struct {
		name        string
		refusingCAS bool
	}{
		{name: "no CAS capability"},
		{name: "CAS refuses as unsupported", refusingCAS: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("pointer holds own stamp", func(t *testing.T) {
				is, mem, rec := legacyCurrentClaimFixture(t, "A", tc.refusingCAS)
				got, err := is.ClearCurrentClaim("s-1", "A")
				if err != nil {
					t.Fatalf("ClearCurrentClaim: %v", err)
				}
				if got != CurrentClaimLegacy {
					t.Fatalf("clear = %q, want %q", got, CurrentClaimLegacy)
				}
				writes := currentClaimWrites(rec)
				if writes[beadstest.OpSetMetadata] != 1 || writes[beadstest.OpSetMetadataBatch] != 0 {
					t.Fatalf("issued writes: %v, want exactly one SetMetadata", writes)
				}
				if p := currentClaimPointer(t, mem); p != "" {
					t.Fatalf("pointer = %q, want empty", p)
				}
			})
			t.Run("later stamp", func(t *testing.T) {
				is, mem, rec := legacyCurrentClaimFixture(t, "B", tc.refusingCAS)
				got, err := is.ClearCurrentClaim("s-1", "A")
				if err != nil {
					t.Fatalf("ClearCurrentClaim: %v", err)
				}
				if got != CurrentClaimConflict {
					t.Fatalf("clear = %q, want %q", got, CurrentClaimConflict)
				}
				writes := currentClaimWrites(rec)
				if writes[beadstest.OpSetMetadata] != 0 || writes[beadstest.OpSetMetadataBatch] != 0 {
					t.Fatalf("issued writes: %v, want none", writes)
				}
				if p := currentClaimPointer(t, mem); p != "B" {
					t.Fatalf("pointer = %q, want %q", p, "B")
				}
			})
		})
	}
}

// TestReserveCurrentClaimRawExpected pins that expected is compared against the
// raw stored value, on both the fenced and the legacy path: a pointer stored as
// " X" is matched by " X", not by its trimmed form.
func TestReserveCurrentClaimRawExpected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) (*Store, *beads.MemStore)
	}{
		{name: "fenced", build: func(t *testing.T) (*Store, *beads.MemStore) {
			is, mem, _ := currentClaimFixture(t, " X")
			return is, mem
		}},
		{name: "legacy", build: func(t *testing.T) (*Store, *beads.MemStore) {
			is, mem, _ := legacyCurrentClaimFixture(t, " X", false)
			return is, mem
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is, mem := tc.build(t)
			if got, err := is.ReserveCurrentClaim("s-1", "X", "A"); err != nil || got != CurrentClaimConflict {
				t.Fatalf("reserve with trimmed expected = %q, %v; want %q", got, err, CurrentClaimConflict)
			}
			if got, err := is.ReserveCurrentClaim("s-1", " X", "A"); err != nil || (got != CurrentClaimSwapped && got != CurrentClaimLegacy) {
				t.Fatalf("reserve with raw expected = %q, %v; want a write", got, err)
			}
			if p := currentClaimPointer(t, mem); p != "A" {
				t.Fatalf("pointer = %q, want %q", p, "A")
			}
		})
	}
}
