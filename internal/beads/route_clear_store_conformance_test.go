package beads_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// This is the second of the two regression tests Risk R1 requires (design
// ga-cm2o5t.1 secs 3, 13.1), alongside the end-to-end reroute test: a
// repo-wide static scan enumerating every non-test call site that writes
// beadmeta.RoutedToMetadataKey, requiring each one to be on this explicit,
// reviewed allow-list.
//
// A raw text scan cannot tell whether a given call site's Store traces back
// to a composition root wrapped with WithRouteChangeClearing -- that would
// need full data-flow analysis. So this test enumerates matches and demands
// each one carry a reviewed justification, same shape as the worker-boundary
// denylist in cmd/gc/worker_boundary_import_test.go: it is a tripwire against
// silent, unreviewed new write sites, not a runtime wrap-verification.
//
// A site is exempt from wrapping only when it can never leave stale
// executor-identity stamps behind. Two proven exemption shapes recur:
//
//   - creation-time-only: the write lands in an in-memory *beads.Bead /
//     recipe / metadata map before that bead's first store.Create. No prior
//     bead -- and so no prior stamps -- exist yet.
//   - deferred-activation: the write supplies the first real gc.routed_to for
//     a bead that was fenced (type="gate", real key deleted, not merely
//     stale) from its own creation until this activation call. The bead was
//     never dispatchable before the write, so it never received live stamps.
//
// Everything else that writes a possibly-different route onto a bead able to
// have been dispatched under a prior route is in scope and must eventually
// route through a wrapped Store.
func TestRoutedToMetadataWriteSitesAreWrappedOrAllowlisted(t *testing.T) {
	repoRoot := findRepoRoot(t)

	allowed := make(map[string]routedToSite, len(routedToAllowlist))
	for _, s := range routedToAllowlist {
		key := allowlistKey(s.path, s.line)
		if _, dup := allowed[key]; dup {
			t.Fatalf("duplicate allow-list entry %s", key)
		}
		allowed[key] = s
	}
	seen := make(map[string]bool, len(routedToAllowlist))
	var unlisted []string

	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			if name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", path, readErr)
		}
		if isGeneratedGoFile(data) {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return fmt.Errorf("relativizing %s: %w", path, relErr)
		}
		rel = filepath.ToSlash(rel)

		for lineNo, line := range strings.Split(string(data), "\n") {
			lineNo++ // 1-indexed
			matched := false
			for _, p := range routedToPatterns {
				if p.MatchString(line) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			key := allowlistKey(rel, lineNo)
			if _, ok := allowed[key]; ok {
				seen[key] = true
				continue
			}
			unlisted = append(unlisted, fmt.Sprintf("%s: %s", key, strings.TrimSpace(line)))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", repoRoot, err)
	}

	for _, s := range routedToAllowlist {
		key := allowlistKey(s.path, s.line)
		if !seen[key] {
			t.Errorf("stale allow-list entry %s: no beadmeta.RoutedToMetadataKey write pattern found at that location anymore -- update the line number or remove this entry\n  recorded reason: %s", key, s.reason)
		}
	}
	if len(unlisted) > 0 {
		sort.Strings(unlisted)
		t.Errorf("found %d beadmeta.RoutedToMetadataKey write call site(s) not on the reviewed allow-list in route_clear_store_conformance_test.go:\n%s\n\nEach one must either route through a Store wrapped with beads.WithRouteChangeClearing, or be added to routedToAllowlist with a reviewed justification (design ga-cm2o5t.1 sec 13.1, Risk R1).", len(unlisted), strings.Join(unlisted, "\n"))
	}
}

// routedToPatterns are the three line-level regexes that flag a candidate
// non-test write call site for beadmeta.RoutedToMetadataKey.
var routedToPatterns = []*regexp.Regexp{
	// Direct Store-method calls -- unambiguous writes.
	regexp.MustCompile(`\.SetMetadata(Batch)?\(.*beadmeta\.RoutedToMetadataKey`),
	// Composite-literal map-key position. NOT unambiguous: matches both
	// write-shaped structs (UpdateOpts.Metadata, Bead.Metadata) and
	// read/filter-shaped structs (ListQuery.Metadata). Every match is
	// individually triaged in the allow-list below.
	regexp.MustCompile(`beadmeta\.RoutedToMetadataKey\s*:`),
	// Map-index assignment, dotted or bare receiver, excluding == / !=.
	regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.]*\[beadmeta\.RoutedToMetadataKey\]\s*=[^=]`),
}

type routedToSite struct {
	path   string // relative to repo root, forward slashes
	line   int    // 1-indexed
	reason string
}

// routedToAllowlist is the full, reviewed inventory of every non-test
// beadmeta.RoutedToMetadataKey write-pattern match in the repo as of this
// test's authorship (ga-cm2o5t.1.1). Composition-root numbers (#N) refer to
// the Sec 15 Integrations table on parent design ga-cm2o5t.1.
var routedToAllowlist = []routedToSite{
	// --- In scope: genuine reroute writes to pre-existing beads, mapped to
	// an already-documented Sec 15 composition root. Needs
	// WithRouteChangeClearing wrapped in during the GREEN phase. ---
	{
		path:   "cmd/gc/build_desired_state.go",
		line:   5202,
		reason: "write to a pre-existing bead (canonicalize routed_to spelling on assignee-change repair pass); Root #5 (build_desired_state.go reconciliation loop) -- wrap that root's store construction in GREEN phase",
	},
	{
		path:   "cmd/gc/build_desired_state.go",
		line:   5262,
		reason: "write to a pre-existing bead (canonicalizeLegacyBoundUnassignedRoutedWork); Root #5 (build_desired_state.go reconciliation loop) -- wrap that root's store construction in GREEN phase",
	},
	{
		path:   "cmd/gc/build_desired_state.go",
		line:   6037,
		reason: "write to a pre-existing bead (controlDispatcherRouteRepair.persist, repairControlDispatcherRoutesForStoreScope); Root #5 (build_desired_state.go reconciliation loop) -- wrap that root's store construction in GREEN phase",
	},
	{
		path:   "cmd/gc/build_desired_state.go",
		line:   6064,
		reason: "in-memory mirror of the line-6037 persisted write (applyRouteRepairInMemory), applied only after the durable write at line 6037 already succeeded; same Root #5 -- wrap that root's store construction in GREEN phase",
	},
	{
		path:   "cmd/gc/cmd_convoy_dispatch.go",
		line:   2110,
		reason: "write to a pre-existing bead; Root #7 (cmd_convoy_dispatch.go openStoreAtForCity) -- wrap that root's store construction in GREEN phase",
	},
	{
		path:   "cmd/gc/cmd_github.go",
		line:   328,
		reason: "write to a pre-existing bead (refreshGitHubPRRepairBead refreshes route across GitHub re-evaluations of the same PR/head -- one of the flows Risk R1 names for e2e coverage); Root #7 (openGitHubPRRepairStore, transitively via cmd_convoy_dispatch.go per design sec 15) -- wrap that root's store construction in GREEN phase",
	},
	{
		path:   "cmd/gc/cmd_sling.go",
		line:   777,
		reason: "write to a pre-existing bead; Root #3 (cmd_sling.go slingDeps.Store) -- wrap that root's store construction in GREEN phase",
	},
	{
		path:   "cmd/gc/doctor_hold_label_routed_to.go",
		line:   137,
		reason: "write to a pre-existing bead; Root #6 (cmd_doctor.go openStoreForCity/storeFactory, shared by all doctor checks) -- wrap that root's store construction in GREEN phase",
	},
	{
		path:   "cmd/gc/doctor_routed_to_checks.go",
		line:   105,
		reason: "write to a pre-existing bead ((*v2RoutedToNamespaceCheck).Fix rewrites gc.routed_to to its canonical binding-qualified form); Root #6 (cmd_doctor.go openStoreForCity/storeFactory) -- wrap that root's store construction in GREEN phase",
	},
	{
		path:   "cmd/gc/doctor_run_target_backfill.go",
		line:   113,
		reason: "write to a pre-existing bead; Root #6 (cmd_doctor.go openStoreForCity/storeFactory) -- wrap that root's store construction in GREEN phase",
	},
	{
		path:   "internal/api/handler_sling.go",
		line:   510,
		reason: "write to a pre-existing bead; Root #4 (internal/api/handler_sling.go:510, API sling composition root per design sec 15) -- wrap that root's store construction in GREEN phase",
	},

	// --- In scope: genuine reroute writes to pre-existing beads. RESOLVED
	// (previously tracked as candidate Root #9): route_recovery_lane.go,
	// detached_orphan_lane.go, pool_detached_orphan_sweep.go, and
	// demand_serve_predicate.go share CityRuntime store plumbing rooted in a
	// routeRecoveryLane singleton (cmd/gc/route_recovery.go,
	// routeRecoveryLaneOf). Traced in full (ga-cm2o5t.1.1 RED phase): under
	// the common controller-managed path (CityRuntime.cs != nil), the stores
	// for all four sites resolve via controllerState.CityBeadStore/BeadStores,
	// whose only assignment sites (api_state.go constructor and reload path)
	// funnel through wrapWithCachingStore -- the same function that is
	// Root #1. Under the standalone path (CityRuntime.cs == nil), both the
	// standalone city store (openCityStoreAt chain) and standalone rig stores
	// (openStandaloneRigStores calling openStoreAtForCity per rig) converge on
	// the identical terminal function openStoreResultAtForCityWithConfig in
	// main.go -- the same shared factory chain already used by Root #7
	// (cmd_convoy_dispatch.go). So these four sites are not an independent
	// ninth root: wrapping Root #1 and Root #7 in the GREEN phase covers all
	// four. ---
	{
		path:   "cmd/gc/demand_serve_predicate.go",
		line:   175,
		reason: "write to a pre-existing bead (collapseSlotSuffixedRoutedWork persists the base route for a live slot-suffix route -- the FR-2 pool-slot-suffix-collapse scenario the collapsingNormalizer unit test already covers as a normalized no-op); resolves to Root #1 (controller-managed) or Root #7 (standalone), not an independent root -- see block comment above; wrap those roots in GREEN phase",
	},
	{
		path:   "cmd/gc/detached_orphan_lane.go",
		line:   430,
		reason: "write to a pre-existing bead (restoreDetachedOrphanRoute); resolves to Root #1 (controller-managed) or Root #7 (standalone), not an independent root -- see block comment above; wrap those roots in GREEN phase",
	},
	{
		path:   "cmd/gc/pool_detached_orphan_sweep.go",
		line:   136,
		reason: "write to a pre-existing bead (sweepDetachedHandoffOrphansWithRouteStore); resolves to Root #1 (controller-managed) or Root #7 (standalone), not an independent root -- see block comment above; wrap those roots in GREEN phase",
	},
	{
		path:   "cmd/gc/route_recovery_lane.go",
		line:   859,
		reason: "write to a pre-existing, long-lived, flap-tracked bead (routeRecoveryLane restore path, via SetMetadataBatch); resolves to Root #1 (controller-managed) or Root #7 (standalone), not an independent root -- see block comment above; wrap those roots in GREEN phase",
	},

	// --- Out of scope: creation-time-only. Each mutates an in-memory
	// *beads.Bead / recipe / metadata map strictly before that bead's first
	// store.Create (or, for the resume/idempotent path, the mutation is
	// discarded entirely rather than applied to a pre-existing bead). No
	// prior bead -- and so no prior stamps -- can exist at mutation time. ---
	{
		path:   "cmd/gc/cmd_order.go",
		line:   900,
		reason: "creation-time-only: labels/routes the bead moments after molecule.Instantiate creates it, in the same call -- no prior stamps possible",
	},
	{
		path:   "cmd/gc/cmd_github.go",
		line:   377,
		reason: "creation-time-only: githubPRRepairMetadata builds the metadata map passed directly to store.Create (cmd_github.go:293) -- no prior bead exists yet",
	},
	{
		path:   "cmd/gc/order_dispatch.go",
		line:   2299,
		reason: "creation-time-only: identical fresh-creation labeling pattern to cmd_order.go:900 -- no prior stamps possible",
	},
	{
		path:   "internal/dispatch/control.go",
		line:   1291,
		reason: "creation-time-only: applyAttemptStepRoute mutates *formula.RecipeStep.Metadata in-memory before molecule.Attach creates the retry-attempt bead (spawnNextAttempt) -- no prior bead exists yet",
	},
	{
		path:   "internal/dispatch/control.go",
		line:   1308,
		reason: "creation-time-only: same applyAttemptStepRoute pattern as line 1291 -- mutates *formula.RecipeStep.Metadata before molecule.Attach -- no prior bead exists yet",
	},
	{
		path:   "internal/dispatch/control.go",
		line:   1350,
		reason: "creation-time-only: applyAttemptControlStepRoute mutates *formula.RecipeStep.Metadata before molecule.Attach -- no prior bead exists yet",
	},
	{
		path:   "internal/graphroute/graphroute.go",
		line:   193,
		reason: "creation-time-only: mutates *formula.RecipeStep.Metadata before molecule.Instantiate/InstantiateFragment; routing is discarded entirely (not applied to the pre-existing bead) on the idempotent-resume path -- no prior stamps possible",
	},
	{
		path:   "internal/graphroute/graphroute.go",
		line:   243,
		reason: "creation-time-only: same ApplyGraphRouting pattern as line 193 -- no prior stamps possible",
	},
	{
		path:   "internal/graphroute/graphroute.go",
		line:   591,
		reason: "creation-time-only: same ApplyGraphRouting pattern as line 193 -- no prior stamps possible",
	},
	{
		path:   "internal/graphroute/graphroute.go",
		line:   720,
		reason: "creation-time-only: same ApplyGraphRouting pattern as line 193 -- no prior stamps possible",
	},

	// --- Out of scope: deferred-activation. Each write supplies the first
	// real gc.routed_to for a bead/wisp fenced (type forced non-dispatchable,
	// real key deleted via deferBeadMetadataValue, not merely stale) since
	// its own creation. Never dispatchable before this call, so never
	// received live-executor stamps to clear. ---
	{
		path:   "cmd/gc/convergence_store.go",
		line:   249,
		reason: "deferred-activation: activateDeferredAssignees writes the first real gc.routed_to for a fenced wisp (molecule.DeferredRoutedToMetadataKey pending activation) via ActivateWisp; the wisp is not dispatchable before activation, so it never received live-executor stamps to clear -- same mechanism as internal/molecule/molecule.go:1486",
	},
	{
		path:   "internal/molecule/molecule.go",
		line:   1486,
		reason: "deferred-activation: deferredRoutingActivationUpdate writes the first real gc.routed_to for a bead fenced (type=\"gate\", real key deleted via deferBeadMetadataValue, not merely stale) since its own creation in the same Instantiate/InstantiateFragment call (fenceGraphWorkflowBead at molecule.go:993) or a crash-recovery re-activation of that same never-dispatched state (activateAttachCandidate). The bead is never dispatchable before activation, so it never received live-executor stamps to clear",
	},

	// --- Not writes at all: false positives of the ambiguous composite-
	// literal pattern. ListQuery.Metadata (internal/beads/query.go) is a
	// read/query filter field, not a Store mutation. ---
	{
		path:   "cmd/gc/doctor_pool_idle_routed_work_check.go",
		line:   164,
		reason: "not a write: beads.ListQuery{Metadata: ...} filters a Live.List read by current gc.routed_to value -- ListQuery.Metadata is a query filter field (internal/beads/query.go), not a Store mutation",
	},
	{
		path:   "cmd/gc/doctor_routed_to_checks.go",
		line:   166,
		reason: "not a write: store.List(beads.ListQuery{Metadata: ...}) inside collect(), which scans (reads) beads whose gc.routed_to names a bound alias -- the actual write for this check is line 105, already allow-listed under Root #6",
	},
	{
		path:   "cmd/gc/wisp_step_inject.go",
		line:   243,
		reason: "not a write: resolveMoleculeRootViaRoutedBridge calls workStore.List(beads.ListQuery{Metadata: ...}) to find an already-routed bridge bead -- a pure lookup, no write anywhere in this function",
	},
}

func allowlistKey(path string, line int) string {
	return fmt.Sprintf("%s:%d", path, line)
}

// isGeneratedGoFile reports whether data begins (within its first few lines)
// with a standard "Code generated ... DO NOT EDIT" marker, so generated
// clients/stubs never need their own allow-list entries.
func isGeneratedGoFile(data []byte) bool {
	lines := strings.SplitN(string(data), "\n", 20)
	for _, l := range lines {
		if strings.Contains(l, "Code generated") && strings.Contains(l, "DO NOT EDIT") {
			return true
		}
	}
	return false
}

// findRepoRoot walks up from this test file's own directory to the nearest
// ancestor containing go.mod.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod above %s", filepath.Dir(currentFile))
		}
		dir = parent
	}
}
