package beads

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Writers census (plan §6.1 leg 1): the collapsed reconcile's regime invariant
// Q — quiescent ⇒ no fence value exceeds startSeq — rests on every fence VALUE
// being minted by a post-increment of mutationSeq under c.mu. This test proves
// the only sites that assign a fence map (index-assign a value, or replace the
// whole map) are the sanctioned ones, so a future bypass in reconcile (or a
// resurrected Branch B) fails the build. Extended per the council's V-soundness
// nit to also match whole-map replacement, not just indexed writes.
func TestReconcileFenceWritersCensus(t *testing.T) {
	files := packageGoFiles(t)

	indexAssign := regexp.MustCompile(`c\.(beadSeq|deletedSeq|localBeadAt)\[[^\]]+\]\s*=[^=]`)
	wholeAssign := regexp.MustCompile(`c\.(beadSeq|deletedSeq|localBeadAt)\s*=[^=]`)

	// Allowed enclosing functions for index-assignments (value minting / setting).
	allowedIndex := map[string]bool{
		"noteMutationLocked":      true, // beadSeq
		"noteLocalMutationLocked": true, // localBeadAt
		"tombstoneLocked":         true, // deletedSeq
	}
	// Allowed enclosing functions for whole-map replacement. Only prime()'s
	// own B-shaped rebuild remains after the Phase-2 collapse deleted reconcile
	// Branch B; if reconcile ever regrows a wholesale fence reset, this fails.
	allowedWhole := map[string]bool{
		"prime": true,
	}

	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		fn := ""
		funcRe := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z0-9_]+)`)
		for i, line := range strings.Split(string(src), "\n") {
			if m := funcRe.FindStringSubmatch(line); m != nil {
				fn = m[1]
			}
			if indexAssign.MatchString(line) && !allowedIndex[fn] {
				t.Errorf("%s:%d fence index-assignment in unsanctioned func %q: %s",
					filepath.Base(f), i+1, fn, strings.TrimSpace(line))
			}
			if wholeAssign.MatchString(line) && !allowedWhole[fn] {
				t.Errorf("%s:%d whole-map fence assignment in unsanctioned func %q: %s",
					filepath.Base(f), i+1, fn, strings.TrimSpace(line))
			}
		}
	}
}

func packageGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		t.Fatal("no package source files found")
	}
	return out
}

// Field-coverage census (plan §5.1 hardening): the oracle's end-state
// comparison must be structurally exhaustive. Every CachingStore and CacheStats
// field is either compared by the oracle or on a justified-exclusion list; a
// field added later that is neither fails this test, forcing a conscious
// classification instead of a silent oracle blind spot.
func TestMergeOracleFieldCoverage(t *testing.T) {
	comparedStore := map[string]bool{
		"beads": true, "deps": true, "depsComplete": true, "dirty": true,
		"beadSeq": true, "localBeadAt": true, "deletedSeq": true, "state": true,
		"readyProjectionLost": true, // compared as mergeEndState.readyLost
		"lastFreshAt":         true, "mutationSeq": true, "primePartialErr": true,
		"syncFailures": true, "circuitTripped": true,
		"stats": true, // stats compared field-wise below
	}
	excludedStore := map[string]bool{
		// observationRevision is a process-local publication fence, orthogonal to
		// the merge oracle's durable cache-state comparison.
		"observationRevision": true,
		"backing":             true, "idPrefix": true, "mu": true, "reconciling": true,
		"onChange": true, "problemf": true, "problemLog": true,
		"lastReconcileLogAt": true, "primeMu": true, "primeRunning": true,
		"primeCycle": true, "lastFullPrimeStartedAt": true, "primeRetryDelay": true,
		"lifecycleMu": true, "lifecycleWG": true, "cancelFn": true, "stopCh": true,
		"stopped": true, "latencyWindow": true, "latencyDriverActive": true,
		"applyEventBeforeCommitForTest": true,
		// readyProjectionDegraded is a one-way capability latch about the
		// BACKING STORE, set by applyReadyProjection before the seam runs and
		// never touched by mergeSnapshotLocked. It routes readiness reads to the
		// live backing (readyReadsMustGoLive); the merge end state does not
		// depend on it. Its own behavior is pinned by
		// TestDegradedProjectionSendsReadyToTheLiveBdVerdict.
		"readyProjectionDegraded": true,
		// The store-availability gate. All three are about BACKING-STORE
		// REACHABILITY, not durable cache content: availabilityGate is an
		// injected collaborator, unavailableSkipLogged dedupes one problem entry
		// per outage episode, and degradedReads is a monotonic counter of reads
		// served from last-good. None is written by mergeSnapshotLocked and the
		// merge end state does not depend on any of them; their behavior is
		// pinned by the caching_store_availability suite.
		"availabilityGate": true, "unavailableSkipLogged": true,
		"degradedReads": true,
	}
	assertFieldsClassified(t, reflect.TypeOf(CachingStore{}), comparedStore, excludedStore)

	comparedStats := map[string]bool{
		"LastFreshAt": true, "LastReconcileAt": true,
		"Adds": true, "Removes": true, "Updates": true,
	}
	excludedStats := map[string]bool{
		"TotalBeads": true, "TotalDeps": true, "LastReconcileMs": true,
		"ReconcileRecoveries": true, "ReconcileCloseDeferrals": true,
		"SyncFailures": true, "ProblemCount": true, "LastProblemAt": true,
		"LastProblem": true, "State": true, "StaggerOffsetMs": true,
		"CurrentReconcileInterval": true, "LatencyP95Ms": true, "CadenceDriver": true,
		// DegradedReads mirrors the gate counter above — an outage observable,
		// not durable cache content the merge oracle compares.
		"DegradedReads": true,
	}
	assertFieldsClassified(t, reflect.TypeOf(CacheStats{}), comparedStats, excludedStats)
}

func assertFieldsClassified(t *testing.T, ty reflect.Type, compared, excluded map[string]bool) {
	t.Helper()
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		if !compared[name] && !excluded[name] {
			t.Errorf("%s.%s is neither compared nor justified-excluded by the merge oracle — classify it (a seam-written field must be compared)", ty.Name(), name)
		}
		if compared[name] && excluded[name] {
			t.Errorf("%s.%s is in both compared and excluded sets", ty.Name(), name)
		}
	}
}
