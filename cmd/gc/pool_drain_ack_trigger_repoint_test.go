package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestRoutedWorkPoolDrainAckSurvivesLegacyTriggerRepoint is the ga-f7v2ft.131
// repro.
//
// The keyed drain-ack sweep rebuilds its lease FROM the member row every tick
// (newRoutedWorkPoolDrainAckLease, pool_allocation_controller.go:181 —
// WorkID: info.TriggerBeadID), and the effect boundary then requires that work
// to be closed (pool_allocation_controller.go:295-301). Between the worker's
// ack and the keyed stop, the row is still state=active, so the legacy pool
// builder may re-point it to the next ready work item — that is exactly the
// reassign arm of computePoolTriggerBindingPatch
// (build_desired_state.go:3022-3025), and re-targeting a freed member is the
// intended system response to the drained member's trigger closing.
//
// After that re-point every rebuilt lease names a DIFFERENT, genuinely OPEN
// trigger, so the guard refuses with got_id == want_id and status=open for the
// whole finalize budget and forever after. The instrumented journey signature
// (refused=work_not_closed[got_id=X got_status=open want_id=X store=city:...])
// is that refusal, not a stale read: this test reproduces it with no store,
// cache, or Dolt session involved at all.
func TestRoutedWorkPoolDrainAckSurvivesLegacyTriggerRepoint(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)

	// The sibling member's routed work: a second, genuinely open trigger in the
	// same store, which is what legacy re-targets a freed member onto.
	sibling, err := fixture.workStore.Create(beads.Bead{
		Title:    "sibling routed work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": fixture.template},
	})
	if err != nil {
		t.Fatalf("create sibling routed work: %v", err)
	}

	// Legacy re-points the acknowledged member onto the still-open sibling work
	// while the drain-ack is pending and the row is still active.
	if err := sessionFrontDoor(fixture.store).ApplyPatch(fixture.info.ID, sessionpkg.MetadataPatch{
		beadmeta.TriggerBeadIDMetadataKey: sibling.ID,
	}); err != nil {
		t.Fatalf("legacy re-points the drained member: %v", err)
	}
	repointed, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read re-pointed member: %v", err)
	}
	if repointed.TriggerBeadID != sibling.ID {
		t.Fatalf("re-pointed member trigger = %q, want %q", repointed.TriggerBeadID, sibling.ID)
	}

	// The sweep rebuilds the lease from the row, exactly as it does every tick.
	lease, agentDrainAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, repointed)
	if err != nil {
		t.Fatalf("rebuild drain acknowledgement lease: %v", err)
	}
	if !agentDrainAck {
		t.Fatal("rebuilt lease is not an agent drain acknowledgement; the ack evidence was lost")
	}

	// The acknowledged work is still closed and the ack is unchanged, so the
	// drain must still finalize. Today the rebuilt lease names the sibling's
	// open trigger and the effect boundary refuses forever.
	if lease.WorkID != fixture.work.ID {
		t.Fatalf("rebuilt drain acknowledgement lease names work %q, want the acknowledged work %q: "+
			"a drain acknowledgement is about the unit of work the agent finished, not whatever "+
			"trigger the row carries when the sweep next runs (ga-f7v2ft.131)",
			lease.WorkID, fixture.work.ID)
	}
	authorized, _, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, repointed, lease)
	if err != nil {
		t.Fatalf("authorize drain acknowledgement after re-point: %v", err)
	}
	if !authorized {
		t.Fatal("drain acknowledgement is refused after a legacy trigger re-point; the acknowledged drain can never finalize (ga-f7v2ft.131)")
	}
}

// TestRoutedWorkPoolDrainAckSurvivesCrossStoreTriggerRepoint is the cross-store
// arm of the same defect. The acknowledged work lives in a rig store; legacy
// re-points the member onto city-store work. The acknowledgement is about the
// rig work, so both halves of the lease — the work id AND the store it must be
// read from — have to come from the ack, or the effect boundary would look for
// a rig bead in the city store.
func TestRoutedWorkPoolDrainAckSurvivesCrossStoreTriggerRepoint(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixtureWithOptions(t, routedWorkPoolAuthorizationFixtureOptions{rigName: "packs"})

	cityWork, err := fixture.store.Create(beads.Bead{
		Title:    "city routed work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": fixture.template},
	})
	if err != nil {
		t.Fatalf("create city routed work: %v", err)
	}
	if err := sessionFrontDoor(fixture.store).ApplyPatch(fixture.info.ID, sessionpkg.MetadataPatch{
		beadmeta.TriggerBeadIDMetadataKey:       cityWork.ID,
		beadmeta.TriggerBeadStoreRefMetadataKey: "city:test-city",
	}); err != nil {
		t.Fatalf("legacy re-points the drained member across stores: %v", err)
	}
	repointed, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read re-pointed member: %v", err)
	}

	lease, agentDrainAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, repointed)
	if err != nil || !agentDrainAck {
		t.Fatalf("rebuild drain acknowledgement lease = (%+v, %t, %v), want an admitted lease", lease, agentDrainAck, err)
	}
	if lease.WorkID != fixture.work.ID || lease.SourceStore != "rig:packs" {
		t.Fatalf("rebuilt lease = work %q store %q, want the acknowledged rig work %q in %q",
			lease.WorkID, lease.SourceStore, fixture.work.ID, "rig:packs")
	}
	authorized, _, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, repointed, lease)
	if err != nil || !authorized {
		t.Fatalf("cross-store drain acknowledgement authorization = (%t, %v), want true", authorized, err)
	}
}

// TestRoutedWorkPoolDrainAckMixedVersionTriggerEvidence pins both directions of
// the version skew the stamp introduces.
//
// The old-controller direction is only reachable as a lease built by the
// fallback path — the pre-stamp controller binary is not in this process — so it
// is exercised as an in-flight lease carrying no stamp provenance against a
// session whose acknowledgement does.
func TestRoutedWorkPoolDrainAckMixedVersionTriggerEvidence(t *testing.T) {
	t.Run("unstamped ack still finalizes when the row is unchanged", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		clearAckTriggerStamp(&fixture)

		lease, agentDrainAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
		if err != nil || !agentDrainAck {
			t.Fatalf("rebuild unstamped lease = (%+v, %t, %v), want an admitted lease", lease, agentDrainAck, err)
		}
		if lease.TriggerFromAck || lease.WorkID != fixture.work.ID {
			t.Fatalf("unstamped lease = %+v, want the row's trigger %q in fallback mode", lease, fixture.work.ID)
		}
		authorized, _, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, fixture.info, lease)
		if err != nil || !authorized {
			t.Fatalf("unstamped drain acknowledgement authorization = (%t, %v), want true", authorized, err)
		}
	})

	// The documented residual: an acknowledgement from an agent CLI that predates
	// the stamp carries no evidence of what it acknowledged, so a legacy re-point
	// still strands it. That refusal is bounded by the WD.6 deadline release
	// (ga-f7v2ft.112 round-4 ruling 1b), not by this seam.
	t.Run("unstamped ack still loses a re-pointed row", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		clearAckTriggerStamp(&fixture)
		repointed := repointDrainedMemberOntoSibling(t, fixture)

		lease, agentDrainAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, repointed)
		if err != nil || !agentDrainAck {
			t.Fatalf("rebuild unstamped lease = (%+v, %t, %v), want an admitted lease", lease, agentDrainAck, err)
		}
		if lease.WorkID == fixture.work.ID {
			t.Fatalf("unstamped lease named the acknowledged work %q; a pre-stamp ack carries no such evidence", lease.WorkID)
		}
		authorized, _, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, repointed, lease)
		if err != nil || authorized {
			t.Fatalf("unstamped re-pointed authorization = (%t, %v), want a clean refusal", authorized, err)
		}
	})

	t.Run("stamped ack still finalizes an in-flight fallback lease", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		lease := fixture.lease
		lease.TriggerFromAck = false

		authorized, _, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, fixture.info, lease)
		if err != nil || !authorized {
			t.Fatalf("fallback-mode lease against a stamped acknowledgement = (%t, %v), want true", authorized, err)
		}
	})
}

// TestRoutedWorkPoolDrainAckIgnoresLoneTriggerStamp pins both-or-neither. A
// half-written pair carries no usable binding — one of the two halves the
// effect boundary needs is missing — so it must read as no stamp at all rather
// than as a partially trusted one.
func TestRoutedWorkPoolDrainAckIgnoresLoneTriggerStamp(t *testing.T) {
	for _, test := range []struct{ name, drop string }{
		{name: "store ref stamp missing", drop: reconcilerDrainAckTriggerStoreRefKey},
		{name: "bead id stamp missing", drop: reconcilerDrainAckTriggerBeadIDKey},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
			if err := fixture.provider.RemoveMeta(fixture.info.SessionName, test.drop); err != nil {
				t.Fatalf("drop %s: %v", test.drop, err)
			}
			repointed := repointDrainedMemberOntoSibling(t, fixture)

			lease, agentDrainAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, repointed)
			if err != nil || !agentDrainAck {
				t.Fatalf("rebuild half-stamped lease = (%+v, %t, %v), want an admitted lease", lease, agentDrainAck, err)
			}
			if lease.TriggerFromAck {
				t.Fatalf("half-stamped lease claimed acknowledgement provenance: %+v", lease)
			}
			if lease.WorkID != strings.TrimSpace(repointed.TriggerBeadID) {
				t.Fatalf("half-stamped lease work = %q, want the row's %q (full fallback)",
					lease.WorkID, repointed.TriggerBeadID)
			}
		})
	}
}

// repointDrainedMemberOntoSibling is the legacy re-point at the heart of
// ga-f7v2ft.131: a second, genuinely open routed work item bound onto a member
// that has already acknowledged its drain.
func repointDrainedMemberOntoSibling(t *testing.T, fixture routedWorkPoolDrainAckAuthorizationFixture) sessionpkg.Info {
	t.Helper()
	sibling, err := fixture.workStore.Create(beads.Bead{
		Title:    "sibling routed work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": fixture.template},
	})
	if err != nil {
		t.Fatalf("create sibling routed work: %v", err)
	}
	if err := sessionFrontDoor(fixture.store).ApplyPatch(fixture.info.ID, sessionpkg.MetadataPatch{
		beadmeta.TriggerBeadIDMetadataKey: sibling.ID,
	}); err != nil {
		t.Fatalf("legacy re-points the drained member: %v", err)
	}
	repointed, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read re-pointed member: %v", err)
	}
	return repointed
}
