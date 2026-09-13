package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

func boolPtrTest(b bool) *bool { return &b }

// TestSuspendRigInState_AlreadySuspendedReturnsFalse covers the no-op
// branch — calling suspend on an already explicit-suspended rig
// should return false so callers know they can skip the disk write.
func TestSuspendRigInState_AlreadySuspendedReturnsFalse(t *testing.T) {
	st := suspensionstate.State{
		Rigs: map[string]suspensionstate.Override{"foo": {Suspended: boolPtrTest(true)}},
	}
	if suspendRigInState(&st, "foo") {
		t.Error("suspendRigInState on already-suspended rig should return false")
	}
}

// TestSuspendRigInState_NotSuspendedReturnsTrue covers the mutating
// branch and confirms the state is updated.
func TestSuspendRigInState_NotSuspendedReturnsTrue(t *testing.T) {
	st := suspensionstate.State{}
	if !suspendRigInState(&st, "foo") {
		t.Fatal("suspendRigInState on fresh state should return true")
	}
	if !suspensionstate.IsRigSuspended(st, "foo") {
		t.Error("foo should be suspended after suspendRigInState")
	}
}

// TestSuspendRigInState_ExplicitResumeIsUpgraded covers the case where
// the rig has an explicit resume on file — suspending must overwrite
// the &false with &true and report mutation.
func TestSuspendRigInState_ExplicitResumeIsUpgraded(t *testing.T) {
	st := suspensionstate.State{
		Rigs: map[string]suspensionstate.Override{"foo": {Suspended: boolPtrTest(false)}},
	}
	if !suspendRigInState(&st, "foo") {
		t.Fatal("suspendRigInState must overwrite explicit-resume with suspend")
	}
	if !suspensionstate.IsRigSuspended(st, "foo") {
		t.Error("foo should be suspended after suspendRigInState")
	}
}

// TestResumeRigInState_AlreadyResumedReturnsFalse covers the no-op
// branch — calling resume on a rig with explicit resume already
// recorded is a signal to skip the disk write.
func TestResumeRigInState_AlreadyResumedReturnsFalse(t *testing.T) {
	st := suspensionstate.State{
		Rigs: map[string]suspensionstate.Override{"foo": {Suspended: boolPtrTest(false)}},
	}
	if resumeRigInState(&st, "foo") {
		t.Error("resumeRigInState on already explicit-resumed rig should return false")
	}
}

// TestResumeRigInState_SuspendedKeepsEntryAsExplicitResume confirms
// resume records an explicit &false (not just removes the entry) so
// the override sticks even when suspended_on_start = true.
func TestResumeRigInState_SuspendedKeepsEntryAsExplicitResume(t *testing.T) {
	st := suspensionstate.State{
		Rigs: map[string]suspensionstate.Override{"foo": {Suspended: boolPtrTest(true)}},
	}
	if !resumeRigInState(&st, "foo") {
		t.Fatal("resumeRigInState on suspended rig should return true")
	}
	if v, ok := suspensionstate.ExplicitRig(st, "foo"); !ok || v {
		t.Errorf("foo should be explicit-resume after resumeRigInState; got (%v, %v)", v, ok)
	}
}

// TestResumeRigInState_FreshStateRecordsExplicitResume — running
// resume against a rig with no entry must record an explicit &false
// so a later "gc start" does not let suspended_on_start reassert.
func TestResumeRigInState_FreshStateRecordsExplicitResume(t *testing.T) {
	st := suspensionstate.State{}
	if !resumeRigInState(&st, "foo") {
		t.Fatal("resumeRigInState on fresh state should record explicit resume and return true")
	}
	if v, ok := suspensionstate.ExplicitRig(st, "foo"); !ok || v {
		t.Errorf("foo should be explicit-resume; got (%v, %v)", v, ok)
	}
}

// TestIsRigSuspendedInState_TrueAndFalse exercises both branches in
// the trivial wrapper.
func TestIsRigSuspendedInState_TrueAndFalse(t *testing.T) {
	st := suspensionstate.State{
		Rigs: map[string]suspensionstate.Override{"foo": {Suspended: boolPtrTest(true)}},
	}
	if !isRigSuspendedInState(st, "foo") {
		t.Error("isRigSuspendedInState should return true for suspended rig")
	}
	if isRigSuspendedInState(st, "bar") {
		t.Error("isRigSuspendedInState should return false for absent rig")
	}
}

// TestBuildEffectiveSuspendedRigNames_RuntimeOverridesConfig covers
// the merge rule: a runtime explicit-resume must defeat the rig's
// SuspendedOnStart, and a runtime explicit-suspend must defeat
// SuspendedOnStart=false.
func TestBuildEffectiveSuspendedRigNames_RuntimeOverridesConfig(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "resumed-but-default-suspended", SuspendedOnStart: true},
			{Name: "suspended-but-default-resumed"},
			{Name: "default-suspended", SuspendedOnStart: true},
		},
	}
	st := suspensionstate.State{
		Rigs: map[string]suspensionstate.Override{
			"resumed-but-default-suspended": {Suspended: boolPtrTest(false)},
			"suspended-but-default-resumed": {Suspended: boolPtrTest(true)},
		},
	}
	got := buildEffectiveSuspendedRigNames(cfg, st)

	if got["resumed-but-default-suspended"] {
		t.Error("explicit resume must beat suspended_on_start=true")
	}
	if !got["suspended-but-default-resumed"] {
		t.Error("explicit suspend must beat suspended_on_start=false")
	}
	if !got["default-suspended"] {
		t.Error("suspended_on_start=true with no runtime override must mark rig suspended")
	}
}

// TestBuildEffectiveSuspendedRigNames_LegacySuspendedIsAlias pins the
// migration behavior: the deprecated `[[rig]] suspended = true` field
// is honored as an alias for `suspended_on_start`. Existing city.toml
// files with `suspended = true` continue to start their rigs
// suspended after upgrade.
func TestBuildEffectiveSuspendedRigNames_LegacySuspendedIsAlias(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "legacy", Suspended: true},
			{Name: "new", SuspendedOnStart: true},
			{Name: "neither"},
		},
	}
	got := buildEffectiveSuspendedRigNames(cfg, suspensionstate.State{})
	if !got["legacy"] {
		t.Error("legacy [[rig]] suspended = true must alias suspended_on_start so the rig still starts suspended")
	}
	if !got["new"] {
		t.Error("explicit suspended_on_start = true must mark the rig suspended")
	}
	if got["neither"] {
		t.Error("rig with neither field set must not be suspended")
	}
}

// TestBuildEffectiveSuspendedRigNames_NilRuntimeMap defends against a
// nil State.Rigs (e.g. fresh city or test setup) — the helper must
// not panic and must still honor SuspendedOnStart.
func TestBuildEffectiveSuspendedRigNames_NilRuntimeMap(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "alpha", SuspendedOnStart: true}},
	}
	got := buildEffectiveSuspendedRigNames(cfg, suspensionstate.State{})
	if !got["alpha"] {
		t.Error("alpha should be effective-suspended via SuspendedOnStart even when runtime map is nil")
	}
}

// TestLoadAndSaveSuspensionState_RoundTrip pins the wrapper-level
// behavior so future refactors can't drop the suspensionstate
// Save/Load calls or change the persisted location.
func TestLoadAndSaveSuspensionState_RoundTrip(t *testing.T) {
	cityDir := t.TempDir()
	st := suspensionstate.State{
		Rigs: map[string]suspensionstate.Override{"foo": {Suspended: boolPtrTest(true)}},
	}
	if err := saveSuspensionState(fsys.OSFS{}, cityDir, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadSuspensionState(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !suspensionstate.IsRigSuspended(got, "foo") {
		t.Error("round-tripped state should preserve foo suspended")
	}
}
