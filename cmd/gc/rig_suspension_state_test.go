package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rigstate"
)

// TestSuspendRigInState_AlreadySuspendedReturnsFalse covers the no-op
// branch — calling suspend on an already-suspended rig should return
// false so callers know they can skip the disk write.
func TestSuspendRigInState_AlreadySuspendedReturnsFalse(t *testing.T) {
	st := rigstate.SuspensionState{
		Rigs: map[string]rigstate.RigOverride{"foo": {Suspended: true}},
	}
	if suspendRigInState(&st, "foo") {
		t.Error("suspendRigInState on already-suspended rig should return false")
	}
}

// TestSuspendRigInState_NotSuspendedReturnsTrue covers the mutating
// branch and confirms the state is updated.
func TestSuspendRigInState_NotSuspendedReturnsTrue(t *testing.T) {
	st := rigstate.SuspensionState{Rigs: map[string]rigstate.RigOverride{}}
	if !suspendRigInState(&st, "foo") {
		t.Fatal("suspendRigInState on fresh state should return true")
	}
	if !rigstate.IsSuspended(st, "foo") {
		t.Error("foo should be suspended after suspendRigInState")
	}
}

// TestResumeRigInState_NotSuspendedReturnsFalse covers the no-op branch
// — calling resume on a rig that is not suspended is a signal to skip
// the disk write.
func TestResumeRigInState_NotSuspendedReturnsFalse(t *testing.T) {
	st := rigstate.SuspensionState{Rigs: map[string]rigstate.RigOverride{}}
	if resumeRigInState(&st, "foo") {
		t.Error("resumeRigInState on not-suspended rig should return false")
	}
}

// TestResumeRigInState_SuspendedReturnsTrue covers the mutating branch
// — confirming both the return value and that the rig entry is removed
// from the state map.
func TestResumeRigInState_SuspendedReturnsTrue(t *testing.T) {
	st := rigstate.SuspensionState{
		Rigs: map[string]rigstate.RigOverride{"foo": {Suspended: true}},
	}
	if !resumeRigInState(&st, "foo") {
		t.Fatal("resumeRigInState on suspended rig should return true")
	}
	if _, ok := st.Rigs["foo"]; ok {
		t.Error("foo entry should be removed from state after resume")
	}
}

// TestIsRigSuspendedInState_TrueAndFalse exercises both branches in
// the trivial wrapper to lock the contract with rigstate.IsSuspended.
func TestIsRigSuspendedInState_TrueAndFalse(t *testing.T) {
	st := rigstate.SuspensionState{
		Rigs: map[string]rigstate.RigOverride{"foo": {Suspended: true}},
	}
	if !isRigSuspendedInState(st, "foo") {
		t.Error("isRigSuspendedInState should return true for suspended rig")
	}
	if isRigSuspendedInState(st, "bar") {
		t.Error("isRigSuspendedInState should return false for absent rig")
	}
}

// TestBuildMergedSuspendedRigNames_RuntimeOnly covers the case where a
// rig is suspended in the runtime JSON but not city.toml — the runtime
// state alone should be enough.
func TestBuildMergedSuspendedRigNames_RuntimeOnly(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "alpha"}, {Name: "beta"}},
	}
	rs := rigstate.SuspensionState{
		Rigs: map[string]rigstate.RigOverride{"alpha": {Suspended: true}},
	}
	got := buildMergedSuspendedRigNames(cfg, rs)
	if !got["alpha"] {
		t.Error("alpha should be in merged suspended set (runtime)")
	}
	if got["beta"] {
		t.Error("beta should not be in merged suspended set")
	}
}

// TestBuildMergedSuspendedRigNames_LegacyOnly covers the legacy
// city.toml suspended=true field, which must still be honored so older
// configs don't silently lose suspension on upgrade.
func TestBuildMergedSuspendedRigNames_LegacyOnly(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Suspended: true},
			{Name: "beta"},
		},
	}
	got := buildMergedSuspendedRigNames(cfg, rigstate.SuspensionState{Rigs: map[string]rigstate.RigOverride{}})
	if !got["alpha"] {
		t.Error("alpha should be in merged suspended set (legacy city.toml)")
	}
	if got["beta"] {
		t.Error("beta should not be in merged suspended set")
	}
}

// TestBuildMergedSuspendedRigNames_BothSources covers union semantics —
// suspended from either source should be present.
func TestBuildMergedSuspendedRigNames_BothSources(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Suspended: true},
			{Name: "beta"},
			{Name: "gamma"},
		},
	}
	rs := rigstate.SuspensionState{
		Rigs: map[string]rigstate.RigOverride{"beta": {Suspended: true}},
	}
	got := buildMergedSuspendedRigNames(cfg, rs)
	for _, name := range []string{"alpha", "beta"} {
		if !got[name] {
			t.Errorf("%s should be in merged suspended set", name)
		}
	}
	if got["gamma"] {
		t.Error("gamma should not be in merged suspended set")
	}
}

// TestBuildMergedSuspendedRigNames_NilRuntimeMap defends against a nil
// SuspensionState.Rigs (e.g. fresh city or test setup) — the helper
// must not panic and must still surface legacy city.toml suspensions.
func TestBuildMergedSuspendedRigNames_NilRuntimeMap(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "alpha", Suspended: true}},
	}
	got := buildMergedSuspendedRigNames(cfg, rigstate.SuspensionState{})
	if !got["alpha"] {
		t.Error("alpha should be present even when runtime map is nil")
	}
}

// TestLoadAndSaveRigSuspensionState_RoundTrip pins the wrapper-level
// behavior so future refactors can't drop the rigstate.Save/Load calls
// or change the persisted location.
func TestLoadAndSaveRigSuspensionState_RoundTrip(t *testing.T) {
	cityDir := t.TempDir()
	st := rigstate.SuspensionState{
		Rigs: map[string]rigstate.RigOverride{"foo": {Suspended: true}},
	}
	if err := saveRigSuspensionState(fsys.OSFS{}, cityDir, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadRigSuspensionState(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !rigstate.IsSuspended(got, "foo") {
		t.Error("round-tripped state should preserve foo suspended")
	}
}
