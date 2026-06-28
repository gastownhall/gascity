package session

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// recordingInfoStore seeds a session bead into a recording-fake store and
// returns the InfoStore front door plus the recorder, so a test can assert the
// typed write method emitted byte-identical bead writes.
func recordingInfoStore(t *testing.T, b beads.Bead) (*InfoStore, *beadstest.RecordingStore) {
	t.Helper()
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	rec := beadstest.NewRecordingStore(mem)
	return NewInfoStore(beads.SessionStore{Store: rec}), rec
}

// TestApplyPatchByteIdenticalToSetMetaBatch proves ApplyPatch emits exactly one
// SetMetadataBatch with the patch verbatim — the byte-identical replacement for
// setMetaBatch(store, id, patch).
func TestApplyPatchByteIdenticalToSetMetaBatch(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingInfoStore(t, b)

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

// TestApplyPatchEmptyIsNoOp proves an empty patch emits no write (matching
// setMetaBatch's len==0 short-circuit).
func TestApplyPatchEmptyIsNoOp(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", nil)
	is, rec := recordingInfoStore(t, b)

	if err := is.ApplyPatch("s-1", MetadataPatch{}); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if got := len(rec.Calls()); got != 0 {
		t.Errorf("empty patch emitted %d calls, want 0", got)
	}
}

// TestSleepEmitsSleepPatch proves the typed Sleep method emits exactly the bead
// write that SleepPatch produces — the same write the reconciler raw op did.
func TestSleepEmitsSleepPatch(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingInfoStore(t, b)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := is.Sleep("s-1", "idle-timeout", now); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	calls := rec.CallsForOp("SetMetadataBatch")
	if len(calls) != 1 {
		t.Fatalf("want 1 SetMetadataBatch, got %d", len(calls))
	}
	want := map[string]string(SleepPatch(now, "idle-timeout"))
	if !reflect.DeepEqual(calls[0].Metadata, want) {
		t.Errorf("Sleep batch = %#v, want %#v", calls[0].Metadata, want)
	}
}

// TestSetWaitHoldClearWritesEmptyStrings proves clearing wait-hold emits the
// empty-string writes the raw cmd_wait.go clear did.
func TestSetWaitHoldClearWritesEmptyStrings(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"wait_hold": "x", "sleep_intent": "x"})
	is, rec := recordingInfoStore(t, b)

	if err := is.SetWaitHold("s-1", false, ""); err != nil {
		t.Fatalf("SetWaitHold: %v", err)
	}
	calls := rec.CallsForOp("SetMetadataBatch")
	if len(calls) != 1 {
		t.Fatalf("want 1 SetMetadataBatch, got %d", len(calls))
	}
	want := map[string]string{"wait_hold": "", "sleep_intent": ""}
	if !reflect.DeepEqual(calls[0].Metadata, want) {
		t.Errorf("clear batch = %#v, want %#v", calls[0].Metadata, want)
	}
}

// TestCloseEmitsClosePatchThenClose proves Close stamps ClosePatch metadata and
// then closes the bead — the byte-identical replacement for closeBead's
// SetMetadataBatch(ClosePatch)+Close, WITHOUT any work-reassignment side effect
// (that is Phase 6).
func TestCloseEmitsClosePatchThenClose(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingInfoStore(t, b)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	closed, err := is.Close("s-1", "gc_swept", now)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Fatalf("Close reported not-closed for an open bead")
	}

	gotOps := opsOf(rec.Calls())
	wantOps := []string{"SetMetadataBatch", "Close"}
	if !reflect.DeepEqual(gotOps, wantOps) {
		t.Fatalf("Close ops = %v, want %v", gotOps, wantOps)
	}
	want := map[string]string(ClosePatch(now, "gc_swept"))
	if !reflect.DeepEqual(rec.CallsForOp("SetMetadataBatch")[0].Metadata, want) {
		t.Errorf("close patch = %#v, want %#v", rec.CallsForOp("SetMetadataBatch")[0].Metadata, want)
	}
}

// TestCloseAlreadyClosedIsNoOp proves Close on a closed bead emits no writes.
func TestCloseAlreadyClosedIsNoOp(t *testing.T) {
	b := sessionBeadFixture("s-1", "closed", nil)
	is, rec := recordingInfoStore(t, b)

	closed, err := is.Close("s-1", "gc_swept", time.Now())
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
	is, _ := recordingInfoStore(t, b)

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
	is, rec := recordingInfoStore(t, b)

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
	is, rec := recordingInfoStore(t, b)

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
	is, rec := recordingInfoStore(t, b)

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

// TestCloseWithoutReasonEmitsSingleClose proves CloseWithoutReason emits exactly
// one Close op and no metadata write — byte-identical to closeBead's raw
// store.Close(id) after it stamps ClosePatch separately.
func TestCloseWithoutReasonEmitsSingleClose(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingInfoStore(t, b)

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

func opsOf(calls []beadstest.RecordedCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Op)
	}
	return out
}
