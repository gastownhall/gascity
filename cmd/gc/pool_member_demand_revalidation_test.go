package main

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// poolMemberDemandCity is a one-pool city whose worker template is the routed
// target in every case below.
func poolMemberDemandCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(4),
		}},
	}
}

// poolMemberDemandPlan is the plan the legacy pool builder produces for one
// routed work item: the trigger provenance is the durable claim both builders
// stamp at member creation.
func poolMemberDemandPlan(workID, storeRef string) poolSessionCreatePlan {
	metadata := map[string]string{}
	if workID != "" {
		metadata[beadmeta.TriggerBeadIDMetadataKey] = workID
		metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = storeRef
	}
	if len(metadata) == 0 {
		metadata = nil
	}
	return poolSessionCreatePlan{
		qualifiedInstance: "worker-1",
		slot:              1,
		poolSlot:          1,
		metadata:          metadata,
	}
}

// seedKeyedRoutedWorkPoolMember writes the member the keyed routed-work
// allocation materializes, carrying the same durable claim.
func seedKeyedRoutedWorkPoolMember(t *testing.T, store beads.Store, workID, storeRef string) string {
	t.Helper()
	id, err := sessionFrontDoor(store).CreateSession(session.CreateSpec{
		Title:     "worker",
		AgentName: "worker",
		Metadata: map[string]string{
			"template":                              "worker",
			"session_name":                          "worker-keyed",
			"state":                                 string(session.StateStartPending),
			"pool_slot":                             "1",
			poolManagedMetadataKey:                  boolMetadata(true),
			"pending_create_claim":                  "true",
			beadmeta.TriggerBeadIDMetadataKey:       workID,
			beadmeta.TriggerBeadStoreRefMetadataKey: storeRef,
		},
	})
	if err != nil {
		t.Fatalf("seed keyed pool member: %v", err)
	}
	return id
}

// TestPlannedPoolMemberSkipsWorkAlreadyClaimedByKeyedAllocation is the
// ga-f7v2ft.126 red. An ordinary poke now runs a full legacy tick inside the
// keyed routed-work allocation window, and the legacy pool builder plans its
// member from a per-tick snapshot taken BEFORE the keyed allocation
// materialized. Without a re-validation at the member-creation boundary the same
// routed bead gets two members — two worker rows in creating, two live agent
// sessions, real tmux processes and real model spend for one work item.
func TestPlannedPoolMemberSkipsWorkAlreadyClaimedByKeyedAllocation(t *testing.T) {
	store := beads.NewMemStore()
	cfg := poolMemberDemandCity()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	// The snapshot the plan was selected from predates the keyed allocation.
	bp.sessionBeads = newSessionBeadSnapshot(nil)

	const workID = "sk-5gd"
	const storeRef = "city:test-city"
	keyedMember := seedKeyedRoutedWorkPoolMember(t, store, workID, storeRef)

	info, err := executePlannedPoolSessionBeadCreate(bp, &cfg.Agents[0], "worker", poolMemberDemandPlan(workID, storeRef))
	if !errors.Is(err, errPoolMemberAlreadyClaimed) || info.ID != "" {
		t.Fatalf("materializing a member for already-claimed work = %+v / %v, want errPoolMemberAlreadyClaimed", info, err)
	}

	members := livePoolMembersForWork(t, store, cfg, workID, storeRef)
	if len(members) != 1 || members[0].ID != keyedMember {
		t.Fatalf("live members for %s = %+v, want exactly the keyed allocation %q", workID, members, keyedMember)
	}
}

// TestPlannedPoolMemberSeesClaimAcrossStoreRefSpelling is the second half of the
// ga-f7v2ft.126 red, observed on the v59 routed-work leg: the two builders reach
// the trigger provenance by different routes and stamp different spellings of
// the same city scope ("city" vs "city:<name>"). With the store ref matched as
// exact metadata equality each side is blind to the other's claim and BOTH
// materialize a member for one work item — the duplicate survives the
// re-validation that exists to stop it.
func TestPlannedPoolMemberSeesClaimAcrossStoreRefSpelling(t *testing.T) {
	store := beads.NewMemStore()
	cfg := poolMemberDemandCity()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = newSessionBeadSnapshot(nil)

	const workID = "g5-wnb"
	keyedMember := seedKeyedRoutedWorkPoolMember(t, store, workID, "city:gctest-5e65d1f5")

	// The legacy plan spells the same city scope as the bare "city" label.
	info, err := executePlannedPoolSessionBeadCreate(bp, &cfg.Agents[0], "worker", poolMemberDemandPlan(workID, "city"))
	if !errors.Is(err, errPoolMemberAlreadyClaimed) || info.ID != "" {
		t.Fatalf("materializing a member across store-ref spellings = %+v / %v, want errPoolMemberAlreadyClaimed", info, err)
	}
	members := livePoolMembersForWork(t, store, cfg, workID, "city")
	if len(members) != 1 || members[0].ID != keyedMember {
		t.Fatalf("live members for %s = %+v, want exactly the keyed allocation %q", workID, members, keyedMember)
	}
}

// TestRoutedWorkClaimKeepsRigScopesDistinct pins the limit of that widening: a
// rig-scoped claim never satisfies a different rig's demand.
func TestRoutedWorkClaimKeepsRigScopesDistinct(t *testing.T) {
	store := beads.NewMemStore()
	cfg := poolMemberDemandCity()
	const workID = "g5-rig"
	seedKeyedRoutedWorkPoolMember(t, store, workID, "rig:frontend")

	claims, err := routedWorkPoolSessionClaims(store, cfg, routedWorkPoolAllocationHint{
		WorkID:      workID,
		PoolTarget:  "worker",
		SourceStore: "rig:backend",
	})
	if err != nil || len(claims) != 0 {
		t.Fatalf("claims for a different rig scope = %+v / %v, want none", claims, err)
	}
	same, err := routedWorkPoolSessionClaims(store, cfg, routedWorkPoolAllocationHint{
		WorkID:      workID,
		PoolTarget:  "worker",
		SourceStore: "rig:frontend",
	})
	if err != nil || len(same) != 1 {
		t.Fatalf("claims for the owning rig scope = %+v / %v, want exactly one", same, err)
	}
}

// TestPlannedPoolMemberMaterializesUnclaimedRoutedWork is the positive: with no
// live member claiming the work, the legacy builder still materializes one.
func TestPlannedPoolMemberMaterializesUnclaimedRoutedWork(t *testing.T) {
	store := beads.NewMemStore()
	cfg := poolMemberDemandCity()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = newSessionBeadSnapshot(nil)

	const workID = "sk-18x"
	const storeRef = "city:test-city"
	info, err := executePlannedPoolSessionBeadCreate(bp, &cfg.Agents[0], "worker", poolMemberDemandPlan(workID, storeRef))
	if err != nil || info.ID == "" {
		t.Fatalf("materializing a member for unclaimed work = %+v / %v, want a created member", info, err)
	}
	members := livePoolMembersForWork(t, store, cfg, workID, storeRef)
	if len(members) != 1 || members[0].ID != info.ID {
		t.Fatalf("live members for %s = %+v, want exactly the created member %q", workID, members, info.ID)
	}
}

// TestPlannedPoolMemberReplacesRuntimeMissingClaimant is the false positive the
// claim check must not produce. A member that went asleep because its runtime
// vanished no longer serves its work item; the fleet retires it and materializes
// a replacement for the SAME work. Counting the dead row as a live claim wedges
// that replacement forever (caught by
// TestControlDispatcherTickRepairsRigRouteAndRestartsRuntimeMissingDispatcher).
func TestPlannedPoolMemberReplacesRuntimeMissingClaimant(t *testing.T) {
	store := beads.NewMemStore()
	cfg := poolMemberDemandCity()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = newSessionBeadSnapshot(nil)

	const workID = "g5-dead"
	const storeRef = "city:test-city"
	dead := seedKeyedRoutedWorkPoolMember(t, store, workID, storeRef)
	if err := sessionFrontDoor(store).ApplyPatch(dead, session.MetadataPatch{
		"state":                string(session.StateAsleep),
		"sleep_reason":         string(session.SleepReasonRuntimeMissing),
		"pending_create_claim": "",
	}); err != nil {
		t.Fatalf("mark seeded member runtime-missing: %v", err)
	}

	info, err := executePlannedPoolSessionBeadCreate(bp, &cfg.Agents[0], "worker", poolMemberDemandPlan(workID, storeRef))
	if err != nil || info.ID == "" {
		t.Fatalf("materializing a replacement for a runtime-missing member = %+v / %v, want a created member", info, err)
	}
}

// TestPlannedPoolMemberMaterializesFloorRefill keeps the floor path ungated: a
// min-floor refill carries no work item, so it never consults the claim index.
func TestPlannedPoolMemberMaterializesFloorRefill(t *testing.T) {
	store := beads.NewMemStore()
	cfg := poolMemberDemandCity()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = newSessionBeadSnapshot(nil)

	// A live member for some OTHER work item must not shadow the floor refill.
	seedKeyedRoutedWorkPoolMember(t, store, "sk-other", "city:test-city")

	info, err := executePlannedPoolSessionBeadCreate(bp, &cfg.Agents[0], "worker", poolMemberDemandPlan("", ""))
	if err != nil || info.ID == "" {
		t.Fatalf("materializing a floor refill = %+v / %v, want a created member", info, err)
	}
}

func livePoolMembersForWork(t *testing.T, store beads.Store, cfg *config.City, workID, storeRef string) []session.Info {
	t.Helper()
	members, err := routedWorkPoolSessionClaims(store, cfg, routedWorkPoolAllocationHint{
		WorkID:      workID,
		PoolTarget:  "worker",
		SourceStore: storeRef,
	})
	if err != nil {
		t.Fatalf("read live pool members for %s: %v", workID, err)
	}
	return members
}
