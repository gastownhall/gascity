package main

import "strings"

// defaultStaleDatabasePrefixes mirrors
// /home/jaword/projects/beads/cmd/bd/dolt.go:staleDatabasePrefixes — the
// list of name prefixes that identify test/agent databases left behind by
// interrupted runs. The lists must converge (be-hjj-3 syncs the beads side).
//
// Convention:
//   - testdb_*: BEADS_TEST_MODE=1 FNV hash of temp paths
//   - doctest_*: doctor test helpers
//   - doctortest_*: doctor test helpers
//   - beads_pt*: orchestrator patrol_helpers_test.go random prefixes
//   - beads_vr*: orchestrator mail/router_test.go random prefixes
//   - beads_t[0-9a-f]*: protocol test random prefixes (t + 8 hex chars)
var defaultStaleDatabasePrefixes = []string{
	"testdb_", "doctest_", "doctortest_", "beads_pt", "beads_vr", "beads_t",
}

// systemDatabaseNames are the Dolt/MySQL system databases that SHOW
// DATABASES surfaces. The planner never targets these even if a stale
// prefix accidentally matches.
var systemDatabaseNames = map[string]bool{
	"information_schema": true,
	"mysql":              true,
	"performance_schema": true,
	"sys":                true,
	"dolt_cluster":       true,
}

// DoltDropPlan classifies a SHOW DATABASES result into to-drop, protected,
// and stale-but-spared sets. Pure logic; no I/O.
type DoltDropPlan struct {
	// ToDrop is the set of DB names whose prefix matches a stale entry and
	// which are not protected by the rig registry.
	ToDrop []string
	// Protected is the set of registered rig DB names that were observed in
	// the input list, in input order. The set is independent of whether a
	// name matches a stale prefix — it surfaces every registered rig that
	// currently exists on the server so callers can render a complete
	// PROTECTED section per designer Wireframe 1.
	Protected []string
	// Skipped records each stale-prefix-matched name that the planner
	// declined to drop, with the reason. The only reason populated today is
	// rig-protection.
	Skipped []DoltDropSkip
}

// DoltDropSkip is a single stale-but-spared database with the reason.
type DoltDropSkip struct {
	Name   string
	Reason string
}

// DropSkipReasonRigProtected marks a stale-matched DB held back because its
// name appears in the rig-protection list (architect 4.2 safety contract).
const DropSkipReasonRigProtected = "rig-protected"

// planDoltDrops classifies the names returned by SHOW DATABASES against the
// stale-prefix list and the rig-protection list. The protection check wins
// over the stale-prefix match: a registered rig DB is never a drop target,
// even if its name happens to start with a known stale prefix.
//
// Order of `allDBs` is preserved across ToDrop, Protected, and Skipped so
// human-readable rendering stays predictable.
func planDoltDrops(allDBs, stalePrefixes, protectedNames []string) DoltDropPlan {
	protected := map[string]bool{}
	for _, p := range protectedNames {
		protected[p] = true
	}

	plan := DoltDropPlan{}
	for _, name := range allDBs {
		if systemDatabaseNames[name] {
			continue
		}
		isProtected := protected[name]
		if isProtected {
			plan.Protected = append(plan.Protected, name)
		}
		if !hasAnyPrefix(name, stalePrefixes) {
			continue
		}
		if isProtected {
			plan.Skipped = append(plan.Skipped, DoltDropSkip{Name: name, Reason: DropSkipReasonRigProtected})
			continue
		}
		plan.ToDrop = append(plan.ToDrop, name)
	}
	return plan
}

func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
