package main

import (
	"bytes"
	"errors"
	"path/filepath"
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

// TestLegacyPoolTriggerStampCanonicalizesBareStoreRefs is the write-side half of
// ga-2oboq. The legacy demand collector names the HQ store "city" and a rig
// store by its bare rig name, and that spelling reaches the member row through
// SessionRequest.WorkStoreRef. Both stamp sites -- the create stamp
// (poolTriggerMetadata) and the reconcile bind (bindPoolSessionTriggerBead) --
// convert it to the canonical workflow ref, so rows written from here on carry
// the spelling the keyed seams and the agent's GC_TRIGGER_WORK_STORE_REF
// environment already speak. They MUST move together: canonicalizing only one
// would make the other rewrite the row back on the next tick.
func TestLegacyPoolTriggerStampCanonicalizesBareStoreRefs(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "packs", Path: filepath.Join(cityPath, "rigs", "packs")}},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}

	t.Run("create stamp", func(t *testing.T) {
		for _, test := range []struct{ name, ref, want string }{
			{name: "bare city", ref: "city", want: "city:test-city"},
			{name: "bare rig", ref: "packs", want: "rig:packs"},
			{name: "already canonical", ref: "rig:packs", want: "rig:packs"},
			{name: "unknown ref left verbatim", ref: "not-a-store", want: "not-a-store"},
		} {
			t.Run(test.name, func(t *testing.T) {
				var stderr bytes.Buffer
				bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), beads.NewMemStore(), &stderr)
				metadata := poolTriggerMetadata(bp, &cfg.Agents[0], "worker", SessionRequest{
					WorkBeadID:   "wb-1",
					WorkStoreRef: test.ref,
				})
				if got := metadata[beadmeta.TriggerBeadStoreRefMetadataKey]; got != test.want {
					t.Fatalf("created trigger store ref = %q, want %q", got, test.want)
				}
			})
		}
	})

	t.Run("bind heals a legacy row once and never flips it back", func(t *testing.T) {
		mem := beads.NewMemStore()
		bead := triggerClusterSessionBead()
		bead.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = "city"
		created, err := mem.Create(bead)
		if err != nil {
			t.Fatalf("create legacy-stamped session bead: %v", err)
		}
		rec := beadstest.NewRecordingStore(mem)
		info, err := sessionFrontDoor(rec).Get(created.ID)
		if err != nil {
			t.Fatalf("read legacy-stamped session bead: %v", err)
		}
		var stderr bytes.Buffer
		bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), rec, &stderr)
		request := SessionRequest{WorkBeadID: "wb-A", WorkStoreRef: "city", BrainParentSID: "brain-A"}

		bound, err := bindPoolSessionTriggerBead(bp, &cfg.Agents[0], "worker", info, request)
		if err != nil {
			t.Fatalf("bind legacy-stamped session bead: %v", err)
		}
		if bound.TriggerBeadStoreRef != "city:test-city" {
			t.Fatalf("bound trigger store ref = %q, want the canonical %q", bound.TriggerBeadStoreRef, "city:test-city")
		}
		if updates := len(rec.CallsForOp("Update")); updates != 1 {
			t.Fatalf("Update ops on the healing bind = %d, want 1", updates)
		}

		// The same legacy request against the now-canonical row must be a no-op.
		// A stamp site left un-canonicalized would rewrite it back to "city" here.
		rebound, err := bindPoolSessionTriggerBead(bp, &cfg.Agents[0], "worker", bound, request)
		if err != nil {
			t.Fatalf("re-bind canonical session bead: %v", err)
		}
		if rebound.TriggerBeadStoreRef != "city:test-city" {
			t.Fatalf("re-bound trigger store ref = %q, want the canonical spelling preserved", rebound.TriggerBeadStoreRef)
		}
		if updates := len(rec.CallsForOp("Update")); updates != 1 {
			t.Fatalf("Update ops after the idempotent re-bind = %d, want the healing write only", updates)
		}
	})
}
