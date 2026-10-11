package main

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// failUpdateStore is a beads.Store whose Update always fails; every other op
// delegates. It lets the trigger-bind fail-on-write test prove the cluster commits
// all-or-nothing.
type failUpdateStore struct {
	beads.Store
	err error
}

func (s failUpdateStore) Update(string, beads.UpdateOpts) error { return s.err }

// triggerClusterSessionBead builds a pool session bead carrying a full
// trigger/provenance cluster, so a clear reconciles every cluster key at once.
func triggerClusterSessionBead() beads.Bead {
	return beads.Bead{
		Title:  "claude-1",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                          "s-claude",
			"template":                              "city/claude",
			beadmeta.TriggerBeadIDMetadataKey:       "wb-A",
			beadmeta.TriggerBeadStoreRefMetadataKey: "rig-a",
			beadmeta.BrainParentSIDMetadataKey:      "brain-A",
		},
	}
}

// TestBindPoolSessionTriggerBead_ClearEmitsSingleUpdate pins the one-operation
// contract at the pool trigger bind/clear call site (council finding 1): dropping
// the trigger/provenance cluster must persist through exactly ONE Store.Update
// carrying the FULL patch — not a per-key SetMetadata / SetMetadataBatch
// decomposition that could commit a mixed provenance row on exec:/partial-write
// backends. The returned Info folds the patch on success.
func TestBindPoolSessionTriggerBead_ClearEmitsSingleUpdate(t *testing.T) {
	mem := beads.NewMemStore()
	created, err := mem.Create(triggerClusterSessionBead())
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	rec := beadstest.NewRecordingStore(mem)

	info, err := sessionFrontDoor(rec).Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "city"}}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), rec, &stderr)

	// Clear: repointing to no work bead drops the whole trigger/provenance cluster.
	bound, err := bindPoolSessionTriggerBead(bp, &config.Agent{Name: "claude"}, "city/claude", info, SessionRequest{WorkBeadID: ""})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	updates := rec.CallsForOp("Update")
	if len(updates) != 1 {
		t.Fatalf("want exactly 1 Update op, got %d (all ops: %#v)", len(updates), rec.Calls())
	}
	if updates[0].ID != created.ID {
		t.Errorf("Update target = %q, want %q", updates[0].ID, created.ID)
	}
	wantPatch := map[string]string{
		beadmeta.TriggerBeadIDMetadataKey:       "",
		beadmeta.TriggerBeadStoreRefMetadataKey: "",
		beadmeta.BrainParentSIDMetadataKey:      "",
	}
	if !reflect.DeepEqual(updates[0].Opts.Metadata, wantPatch) {
		t.Errorf("Update metadata = %#v, want the FULL cluster clear %#v", updates[0].Opts.Metadata, wantPatch)
	}
	// One-operation contract: no per-key decomposition.
	if n := len(rec.CallsForOp("SetMetadata")); n != 0 {
		t.Errorf("SetMetadata ops = %d, want 0 (one-Update contract)", n)
	}
	if n := len(rec.CallsForOp("SetMetadataBatch")); n != 0 {
		t.Errorf("SetMetadataBatch ops = %d, want 0 (one-Update contract)", n)
	}
	// Success folds the cluster clear onto the returned Info.
	if bound.TriggerBeadID != "" || bound.TriggerBeadStoreRef != "" || bound.BrainParentSID != "" {
		t.Errorf("bound Info retained cluster after clear: %+v", bound)
	}
}

// TestBindPoolSessionTriggerBead_FailedWritePersistsNothing proves the bind/clear
// is all-or-nothing by construction (council finding 1): when the single Update
// fails, NOTHING is persisted (the durable cluster is untouched) and the returned
// Info is the INPUT unchanged, so the caller's log-and-continue path never
// advances onto a half-applied provenance cluster.
func TestBindPoolSessionTriggerBead_FailedWritePersistsNothing(t *testing.T) {
	mem := beads.NewMemStore()
	created, err := mem.Create(triggerClusterSessionBead())
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	fail := failUpdateStore{Store: mem, err: errors.New("update rejected")}

	info, err := sessionFrontDoor(fail).Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "city"}}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), fail, &stderr)

	bound, err := bindPoolSessionTriggerBead(bp, &config.Agent{Name: "claude"}, "city/claude", info, SessionRequest{WorkBeadID: ""})
	if err == nil {
		t.Fatal("bind: want error on failed Update, got nil")
	}
	// Returned Info is the input UNCHANGED — no partial fold.
	if !reflect.DeepEqual(bound, info) {
		t.Errorf("bound Info = %+v, want INPUT unchanged %+v", bound, info)
	}
	// Nothing persisted: the durable cluster keeps its pre-write values.
	after, err := mem.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after failed update: %v", err)
	}
	for k, want := range map[string]string{
		beadmeta.TriggerBeadIDMetadataKey:       "wb-A",
		beadmeta.TriggerBeadStoreRefMetadataKey: "rig-a",
		beadmeta.BrainParentSIDMetadataKey:      "brain-A",
	} {
		if got := after.Metadata[k]; got != want {
			t.Errorf("durable cluster key %q = %q after failed Update, want %q (all-or-nothing)", k, got, want)
		}
	}
}
