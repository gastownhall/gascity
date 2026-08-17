package beadstest

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestRecordingStoreRecordsCreate(t *testing.T) {
	rs := NewRecordingStore(nil)
	in := beads.Bead{
		Title:     "order:rig/agent",
		Type:      "task",
		Labels:    []string{"order-run:rig/agent", "order-tracking"},
		NoHistory: true,
		Metadata:  map[string]string{"state": "queued"},
	}
	created, err := rs.Create(in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	calls := rs.CallsForOp("Create")
	if len(calls) != 1 {
		t.Fatalf("want 1 Create call, got %d", len(calls))
	}
	c := calls[0]
	if c.ID != created.ID {
		t.Errorf("recorded ID = %q, want assigned id %q", c.ID, created.ID)
	}
	if !reflect.DeepEqual(c.Bead, in) {
		t.Errorf("recorded bead != input\n got  %#v\n want %#v", c.Bead, in)
	}
}

func TestRecordingStoreDeepCopiesArguments(t *testing.T) {
	rs := NewRecordingStore(nil)
	created, err := rs.Create(beads.Bead{Title: "subject"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	meta := map[string]string{"state": "asleep", "last_woke_at": ""}
	if err := rs.SetMetadataBatch(created.ID, meta); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	// Mutating the caller's map after the call must not rewrite history.
	meta["state"] = "MUTATED"
	delete(meta, "last_woke_at")

	calls := rs.CallsForOp("SetMetadataBatch")
	if len(calls) != 1 {
		t.Fatalf("want 1 SetMetadataBatch call, got %d", len(calls))
	}
	got := calls[0].Metadata
	want := map[string]string{"state": "asleep", "last_woke_at": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recorded metadata = %#v, want %#v (deep copy must isolate from caller mutation)", got, want)
	}
}

func TestRecordingStoreRecordsEmptyStringClearVerbatim(t *testing.T) {
	rs := NewRecordingStore(nil)
	created, err := rs.Create(beads.Bead{Title: "subject", Metadata: map[string]string{"state": "active"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := rs.SetMetadata(created.ID, "state", ""); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	calls := rs.CallsForOp("SetMetadata")
	if len(calls) != 1 {
		t.Fatalf("want 1 SetMetadata call, got %d", len(calls))
	}
	if calls[0].Key != "state" || calls[0].Value != "" {
		t.Errorf("recorded (key,value) = (%q,%q), want (state,\"\") — empty-clear must be captured verbatim",
			calls[0].Key, calls[0].Value)
	}
}

func TestRecordingStoreRecordsMutatingOpsInOrder(t *testing.T) {
	rs := NewRecordingStore(nil)
	a, _ := rs.Create(beads.Bead{Title: "a"})
	if err := rs.Update(a.ID, beads.UpdateOpts{Labels: []string{"x"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := rs.Close(a.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	gotOps := make([]string, 0, 3)
	for _, c := range rs.Calls() {
		gotOps = append(gotOps, c.Op)
	}
	wantOps := []string{"Create", "Update", "Close"}
	if !reflect.DeepEqual(gotOps, wantOps) {
		t.Errorf("recorded ops = %v, want %v", gotOps, wantOps)
	}
}

func TestRecordingStoreReadsPassThroughUnrecorded(t *testing.T) {
	rs := NewRecordingStore(nil)
	created, _ := rs.Create(beads.Bead{Title: "subject"})
	if _, err := rs.Get(created.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := rs.List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	// Only the Create should have been recorded; reads are pass-through.
	if got := len(rs.Calls()); got != 1 {
		t.Errorf("recorded %d calls, want 1 (reads must not be recorded)", got)
	}
}

func TestRecordingStoreReset(t *testing.T) {
	rs := NewRecordingStore(nil)
	if _, err := rs.Create(beads.Bead{Title: "subject"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	rs.Reset()
	if got := len(rs.Calls()); got != 0 {
		t.Errorf("after Reset, recorded %d calls, want 0", got)
	}
}
