package rig

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// A fresh add must not inherit a runtime suspend/resume override recorded
// for an earlier rig of the same name. Left in place, a stale explicit resume
// silently beats the new rig's suspended_on_start = true.
func TestProvisionFreshAddClearsStaleRuntimeSuspensionOverride(t *testing.T) {
	deps, req, _ := provisionToWritePhase(t)
	req.StartSuspended = true

	resumed := false
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, deps.CityPath, req.Name, &resumed); err != nil {
		t.Fatalf("seeding stale resume: %v", err)
	}

	got, result, err := Provision(deps, req)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", result.Warnings)
	}

	st, err := suspensionstate.Load(fsys.OSFS{}, deps.CityPath)
	if err != nil {
		t.Fatalf("Load suspension state: %v", err)
	}
	if v, ok := suspensionstate.ExplicitRig(st, req.Name); ok {
		t.Fatalf("stale runtime override survived fresh add: suspended=%v", v)
	}
	if !suspensionstate.EffectiveRigSuspended(st, req.Name, got.EffectiveSuspendedOnStart()) {
		t.Fatal("freshly added rig with suspended_on_start = true is not effectively suspended")
	}
}
