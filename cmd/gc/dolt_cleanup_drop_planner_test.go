package main

import (
	"testing"
)

func TestPlanDoltDrops_FiltersByStalePrefixes(t *testing.T) {
	all := []string{"hq", "beads", "testdb_abc", "doctest_x", "user_data"}
	stale := []string{"testdb_", "doctest_"}
	protected := []string{"hq", "beads"}

	plan := planDoltDrops(all, stale, protected)

	wantDrop := []string{"testdb_abc", "doctest_x"}
	if !equalStringSlice(plan.ToDrop, wantDrop) {
		t.Errorf("ToDrop = %v, want %v", plan.ToDrop, wantDrop)
	}
	// Protected enumerates every registered rig DB present in the input,
	// regardless of stale-prefix match. This drives the human PROTECTED
	// section ("these rigs exist on the server; we won't touch them").
	wantProtected := []string{"hq", "beads"}
	if !equalStringSlice(plan.Protected, wantProtected) {
		t.Errorf("Protected = %v, want %v", plan.Protected, wantProtected)
	}
}

func TestPlanDoltDrops_RefusesProtectedEvenWhenStalePrefixMatches(t *testing.T) {
	// Critical safety contract: a registered rig DB whose name happens to
	// match a stale prefix must NOT be dropped. Protection wins.
	all := []string{"testdb_unsafe", "testdb_safe"}
	stale := []string{"testdb_"}
	protected := []string{"testdb_unsafe"} // some operator chose this name

	plan := planDoltDrops(all, stale, protected)

	wantDrop := []string{"testdb_safe"}
	if !equalStringSlice(plan.ToDrop, wantDrop) {
		t.Errorf("ToDrop = %v, want %v", plan.ToDrop, wantDrop)
	}

	// The protected-but-stale-matching name must show up in Skipped with a
	// reason that documents why we refused.
	foundSkip := false
	for _, s := range plan.Skipped {
		if s.Name == "testdb_unsafe" && s.Reason == "rig-protected" {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Errorf("expected Skipped entry for testdb_unsafe with reason=rig-protected; got %+v", plan.Skipped)
	}
}

func TestPlanDoltDrops_IgnoresSystemDatabases(t *testing.T) {
	// Dolt's SHOW DATABASES includes information_schema, mysql,
	// performance_schema, sys, dolt_cluster — none of these are stale DBs
	// and the planner must never attempt to drop them.
	all := []string{
		"information_schema", "mysql", "performance_schema", "sys", "dolt_cluster",
		"testdb_real",
	}
	stale := []string{"testdb_"}
	protected := []string{}

	plan := planDoltDrops(all, stale, protected)

	wantDrop := []string{"testdb_real"}
	if !equalStringSlice(plan.ToDrop, wantDrop) {
		t.Errorf("ToDrop = %v, want %v", plan.ToDrop, wantDrop)
	}
}

func TestPlanDoltDrops_EmptyInputsProduceEmptyPlan(t *testing.T) {
	plan := planDoltDrops(nil, nil, nil)
	if len(plan.ToDrop) != 0 {
		t.Errorf("ToDrop = %v, want empty", plan.ToDrop)
	}
	if len(plan.Skipped) != 0 {
		t.Errorf("Skipped = %v, want empty", plan.Skipped)
	}
	if len(plan.Protected) != 0 {
		t.Errorf("Protected = %v, want empty", plan.Protected)
	}
}

func TestDefaultStaleDatabasePrefixes_MirrorsBeadsCleanDatabases(t *testing.T) {
	// be-hjj-3 is the beads-side bead that converges these prefixes; until
	// then we mirror beads/cmd/bd/dolt.go:staleDatabasePrefixes.
	want := []string{"testdb_", "doctest_", "doctortest_", "beads_pt", "beads_vr", "beads_t"}
	if !equalStringSlice(defaultStaleDatabasePrefixes, want) {
		t.Errorf("defaultStaleDatabasePrefixes = %v, want %v", defaultStaleDatabasePrefixes, want)
	}
}
