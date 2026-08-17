package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func quarantinedBead(id, reason string) beads.Bead {
	return beads.Bead{ID: id, Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.RunTargetMetadataKey:             routeRecoveryTestPool,
		beadmeta.RouteQuarantineMetadataKey:       "true",
		beadmeta.RouteQuarantineReasonMetadataKey: reason,
	}}
}

// TestRouteRecoveryQuarantineCheckSurfacesMarkedBeads is the operator-visibility
// half of the relic-quarantine contract: a bead the lane could not converge must
// never be silently dropped, so the advisory names it and its reason.
func TestRouteRecoveryQuarantineCheckSurfacesMarkedBeads(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		quarantinedBead("T-relic", routeRecoveryQuarantineRecheckFailed),
		unroutedWorkBead("T-fine"),
	}, nil)
	check := newRouteRecoveryQuarantineCheck(&config.City{}, t.TempDir(), func(string) (beads.Store, error) { return store, nil })

	result := check.Run(&doctor.CheckContext{})
	if result.Status == doctor.StatusOK {
		t.Fatalf("check reported OK with a quarantined bead present: %+v", result)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "T-relic") || !strings.Contains(joined, routeRecoveryQuarantineRecheckFailed) {
		t.Fatalf("details %q name neither the bead nor its reason", joined)
	}
	// Control: a bead that is merely unrouted is not an advisory. Without this
	// the check could warn about everything and still "surface" the relic.
	if strings.Contains(joined, "T-fine") {
		t.Fatalf("details %q include an unquarantined bead", joined)
	}
}

// TestRouteRecoveryQuarantineCheckFixLiftsTheMarker pins the un-quarantine path.
// Quarantine that could not be lifted would be a silent terminal drop for a bead
// somebody actually wants routed.
func TestRouteRecoveryQuarantineCheckFixLiftsTheMarker(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		quarantinedBead("T-relic", routeRecoveryQuarantineRestoreFlap),
	}, nil)
	check := newRouteRecoveryQuarantineCheck(&config.City{}, t.TempDir(), func(string) (beads.Store, error) { return store, nil })

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if reason := quarantineReason(t, store, "T-relic"); reason != "" {
		t.Fatalf("quarantine reason after --fix = %q, want cleared", reason)
	}
	if result := check.Run(&doctor.CheckContext{}); result.Status != doctor.StatusOK {
		t.Fatalf("check after --fix = %+v, want OK", result)
	}
	// Control: the lifted bead is still a repair candidate. Lifting must re-arm
	// the lane, not retire the bead.
	report := newRouteRecoveryLane().backstopLeg(planeLeg{store: store})
	if report.restored != 1 {
		t.Fatalf("backstop restored %d after the quarantine was lifted, want 1", report.restored)
	}
}

// TestRouteRecoveryQuarantineCheckReportsAFlapAsASiblingLaneBug pins the remedy
// text for the flap reason: the defect is in whatever clears gc.routed_to, and
// pointing the operator at the bead instead would send them to the wrong lane.
func TestRouteRecoveryQuarantineCheckReportsAFlapAsASiblingLaneBug(t *testing.T) {
	flap := beads.NewMemStoreFrom(0, []beads.Bead{quarantinedBead("T-flap", routeRecoveryQuarantineRestoreFlap)}, nil)
	relic := beads.NewMemStoreFrom(0, []beads.Bead{quarantinedBead("T-relic", routeRecoveryQuarantineRecheckFailed)}, nil)
	remedyFor := func(store beads.Store) string {
		check := newRouteRecoveryQuarantineCheck(&config.City{}, t.TempDir(), func(string) (beads.Store, error) { return store, nil })
		return check.Run(&doctor.CheckContext{}).FixHint
	}
	flapRemedy, relicRemedy := remedyFor(flap), remedyFor(relic)
	if !strings.Contains(flapRemedy, "clearing gc.routed_to") {
		t.Fatalf("flap remedy %q does not name the clearing lane", flapRemedy)
	}
	// Control: the two reasons produce DIFFERENT remedies, so the assertion
	// above is reading the reason and not a constant.
	if flapRemedy == relicRemedy {
		t.Fatalf("both reasons produced the same remedy %q", flapRemedy)
	}
}
