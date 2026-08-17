package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

var errTestStoreTimeout = errors.New("store timed out")

// TestAppendRigHookStoresExcludesSuspendedRig pins AC3 of the custom
// scale_check fan-out work (ga-drb140): buildDesiredStateWithSessionBeads'
// activeStores already excludes a suspended rig's store from scale_check
// (build_desired_state.go), so the store set backing "gc hook" work_query
// must exclude it too -- otherwise scale_check and work_query disagree on
// which stores exist for the same city-scoped agent, and hook can route
// work into a rig that scale_check will never fan out to (or vice versa).
func TestAppendRigHookStoresExcludesSuspendedRig(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "riga", Path: filepath.Join(cityPath, "riga")},
			{Name: "rigb", Path: filepath.Join(cityPath, "rigb"), SuspendedOnStart: true},
		},
	}
	a := &config.Agent{Name: "worker"}
	base := []hookStore{{dir: "own"}}

	got := appendRigHookStores(base, cityPath, cfg, a, nil)

	if len(got) != len(base)+1 {
		t.Fatalf("len(got) = %d, want %d: a suspended rig's store must be excluded from the "+
			"hook work_query store set, the same way activeStores already excludes it from "+
			"scale_check (got=%+v)", len(got), len(base)+1, got)
	}
	for _, s := range got {
		if strings.Contains(s.dir, "rigb") {
			t.Fatalf("appendRigHookStores included a store for suspended rig rigb: %+v", got)
		}
	}
}

// TestRigScopedHookRig is the core of the rig-scope hook fix: a rig-scoped agent
// ("<rig>/<name>") must resolve to its own rig so the hook also queries that
// rig's store, where its routed work lives. City-scoped identities (no "/") and
// unknown rigs resolve to "" so no spurious store is added.
func TestRigScopedHookRig(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{{Name: "voxist-web"}, {Name: "voxist-api"}}}
	cases := []struct {
		name     string
		identity string
		want     string
	}{
		{"rig-scoped known rig", "voxist-web/voxist.executor", "voxist-web"},
		{"rig-scoped other known rig", "voxist-api/voxist.reviewer", "voxist-api"},
		{"rig-scoped unknown rig", "hq/voxist.executor", ""},
		{"city-scoped (no slash)", "voxist.architect", ""},
		{"empty identity", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rigScopedHookRig(cfg, tc.identity); got != tc.want {
				t.Fatalf("rigScopedHookRig(%q) = %q, want %q", tc.identity, got, tc.want)
			}
		})
	}
	if got := rigScopedHookRig(nil, "voxist-web/x"); got != "" {
		t.Fatalf("rigScopedHookRig(nil, ...) = %q, want \"\"", got)
	}
}

// TestAppendOneRigHookStoreSkipsUnknownInput guards the best-effort contract:
// an unknown rig, empty rig, or nil cfg/agent must leave the store list
// unchanged (and must not reach hookQueryEnv), so a stray GC_AGENT prefix can
// never add a bogus store or wedge the hook.
func TestAppendOneRigHookStoreSkipsUnknownInput(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{{Name: "voxist-web"}}}
	a := &config.Agent{Name: "voxist.executor"}
	base := []hookStore{{dir: "own"}}

	for _, tc := range []struct {
		name    string
		cfg     *config.City
		agent   *config.Agent
		rigName string
	}{
		{"unknown rig", cfg, a, "nope"},
		{"empty rig", cfg, a, ""},
		{"nil cfg", nil, a, "voxist-web"},
		{"nil agent", cfg, nil, "voxist-web"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := appendOneRigHookStore(base, t.TempDir(), tc.cfg, tc.agent, tc.rigName, nil)
			if len(got) != len(base) {
				t.Fatalf("appendOneRigHookStore added a store for %s: len=%d, want %d", tc.name, len(got), len(base))
			}
		})
	}
}

func TestBestStoreWithWorkReturnsTheOnlyStoreThatHasWork(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}, {dir: "rigb"}}
	var calls []string
	run := func(_, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		if dir == "riga" {
			return `[{"id":"va-1"}]`, nil
		}
		return `[]`, nil
	}
	out, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[{"id":"va-1"}]` {
		t.Fatalf("out = %q, want riga work", out)
	}
	if gotStore.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga", gotStore.dir)
	}
	// Every store is consulted: selection is a comparison, not a first hit.
	if len(calls) != 3 || calls[0] != "city" || calls[1] != "riga" || calls[2] != "rigb" {
		t.Fatalf("calls = %v, want [city riga rigb]", calls)
	}
}

// TestBestStoreWithWorkPrefersHigherPriorityInALaterStore is the regression this
// selection change exists for: the agent's own store is first in the slice and
// has ready work, so first-hit selection returned it and the rig-routed P0 was
// unreachable no matter how urgent it was.
func TestBestStoreWithWorkPrefersHigherPriorityInALaterStore(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"ci-1","priority":2}]`, nil
		}
		return `[{"id":"va-1","priority":0}]`, nil
	}
	out, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotStore.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga (P0 must beat the own store's P2)", gotStore.dir)
	}
	if out != `[{"id":"va-1","priority":0}]` {
		t.Fatalf("out = %q, want riga work", out)
	}
}

// TestBestStoreWithWorkDoesNotInvertTheBug guards the other direction: a
// higher-priority candidate in the agent's OWN store must still win. A fix
// that simply preferred the federated store would pass the regression test
// above and be just as wrong.
//
// (An "equal priority keeps slice order" case used to live here too,
// asserting that a tie always resolved to the own store. That assertion WAS
// the permanent-starvation bug ga-kbbg9a exists to fix — see
// TestBestStoreWithWorkRotatesExactTies and
// TestBestStoreWithWorkRepeatedTiesVisitEveryStoreOverTime below for its
// replacement.)
func TestBestStoreWithWorkDoesNotInvertTheBug(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"ci-1","priority":1}]`, nil
		}
		return `[{"id":"va-1","priority":3}]`, nil
	}
	_, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotStore.dir != "city" {
		t.Fatalf("store.dir = %q, want city (P1 in own store beats rig P3)", gotStore.dir)
	}
}

// withHookTieBreakClock pins bestStoreWithWork's tie-break clock for the
// duration of the test, restoring the real clock on cleanup.
func withHookTieBreakClock(t *testing.T, now time.Time) {
	t.Helper()
	orig := hookTieBreakClock
	hookTieBreakClock = func() time.Time { return now }
	t.Cleanup(func() { hookTieBreakClock = orig })
}

// TestBestStoreWithWorkRotatesExactTies is the fix ga-kbbg9a exists for: an
// exact rank tie resolved to the same store on every call because the
// selection loop only replaced its incumbent on a STRICT improvement — ties
// left the first-seen store (stores[0], the agent's own store) as the
// incumbent forever, starving every other tied store regardless of how many
// hook calls ran. Pinning the tie-break clock to two different instants
// proves the winner now depends on the clock rather than always being the
// first store in the slice.
func TestBestStoreWithWorkRotatesExactTies(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"ci-1","priority":1}]`, nil
		}
		return `[{"id":"va-1","priority":1}]`, nil
	}

	withHookTieBreakClock(t, time.Unix(0, 0))
	_, gotCity, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotCity.dir != "city" {
		t.Fatalf("store.dir = %q, want city at tie-break clock offset 0", gotCity.dir)
	}

	withHookTieBreakClock(t, time.Unix(0, 1))
	_, gotRiga, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotRiga.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga at tie-break clock offset 1 — an exact tie must not always resolve to the same store", gotRiga.dir)
	}
}

// TestBestStoreWithWorkRepeatedTiesVisitEveryStoreOverTime is the direct
// regression test for the reported symptom: gc hook run repeatedly against an
// UNCHANGED four-way exact tie (mirroring the bead's live measurement — city,
// gascity, cairn, and beads all at tier=routed/P1) must eventually surface
// every tied store, not just the first one in the slice, forever.
func TestBestStoreWithWorkRepeatedTiesVisitEveryStoreOverTime(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "gascity"}, {dir: "cairn"}, {dir: "beads"}}
	run := func(_, dir string, _ []string) (string, error) {
		return `[{"id":"tied-` + dir + `","priority":1}]`, nil
	}

	orig := hookTieBreakClock
	defer func() { hookTieBreakClock = orig }()

	seen := map[string]bool{}
	for i := 0; i < len(stores); i++ {
		offset := int64(i)
		hookTieBreakClock = func() time.Time { return time.Unix(0, offset) }
		_, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
		if err != nil {
			t.Fatalf("call %d: err: %v", i, err)
		}
		seen[gotStore.dir] = true
	}

	if len(seen) <= 1 {
		t.Fatalf("repeated calls against an unchanged tie only ever selected %v — starvation is still permanent", seen)
	}
}

// TestBestStoreWithWorkDoesNotRotateOnACoResidentDuplicateID is the
// regression test for a gap in ga-kbbg9a's own rotation: a bead migrated
// with `gc storage migrate` (copies, never deletes) is visible as ready from
// MORE than one store under the SAME id — the exact same row, not two tied
// pieces of work. An id-blind tie-break can rotate onto a later store ahead
// of the primary even though nothing about the work differs, which breaks
// the rig-first-city-last fan-out order TestClassEscalationWaitsForEveryWorkLeg
// and TestClassEscalationStillReachesABindingOnlyBead
// (hook_claim_class_fanout_test.go) depend on. Two DIFFERENT ids at the same
// rank must still rotate (TestBestStoreWithWorkRotatesExactTies) — only a
// shared id must not.
func TestBestStoreWithWorkDoesNotRotateOnACoResidentDuplicateID(t *testing.T) {
	stores := []hookStore{{dir: "riga"}, {dir: "city"}}
	run := func(_, _ string, _ []string) (string, error) {
		// Same id, same rank, from every store: a co-resident duplicate, not
		// two different pieces of work.
		return `[{"id":"dup-1","assignee":"worker-1"}]`, nil
	}

	// Offset 1 is exactly the clock value TestBestStoreWithWorkRotatesExactTies
	// uses to prove rotation moves off the first store; here the tied
	// candidates share an id, so it must NOT move off riga.
	withHookTieBreakClock(t, time.Unix(0, 1))
	_, got, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga: a tie between two copies of the SAME bead id must keep slice order, not rotate", got.dir)
	}
}

// TestHookTieBreakIndex pins the rotation formula directly: it must stay
// within [0, n) and must not collapse to a constant across varying clock
// values, which is exactly what would silently reintroduce the starvation
// bug.
func TestHookTieBreakIndex(t *testing.T) {
	for _, tc := range []struct {
		n    int
		nano int64
		want int
	}{
		{2, 0, 0},
		{2, 1, 1},
		{2, 2, 0},
		{4, 0, 0},
		{4, 3, 3},
		{4, 4, 0},
	} {
		got := hookTieBreakIndex(tc.n, time.Unix(0, tc.nano))
		if got != tc.want {
			t.Fatalf("hookTieBreakIndex(%d, unix-nano %d) = %d, want %d", tc.n, tc.nano, got, tc.want)
		}
	}
}

// TestBestStoreWithWorkRanksTierAheadOfPriority pins that priority is compared
// WITHIN a tier, never across one: the three-tier work_query means crash
// recovery and pre-assigned work outrank routed work regardless of number.
func TestBestStoreWithWorkRanksTierAheadOfPriority(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cityRow string
		rigRow  string
		wantDir string
	}{
		{
			name:    "in_progress in a rig store beats a routed P0 in the own store",
			cityRow: `[{"id":"ci-1","priority":0}]`,
			rigRow:  `[{"id":"va-1","priority":3,"status":"in_progress","assignee":"me"}]`,
			wantDir: "riga",
		},
		{
			name:    "assigned beats routed at worse priority",
			cityRow: `[{"id":"ci-1","priority":0}]`,
			rigRow:  `[{"id":"va-1","priority":3,"assignee":"me"}]`,
			wantDir: "riga",
		},
		{
			name:    "within the routed tier, priority decides",
			cityRow: `[{"id":"ci-1","priority":3}]`,
			rigRow:  `[{"id":"va-1","priority":0}]`,
			wantDir: "riga",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stores := []hookStore{{dir: "city"}, {dir: "riga"}}
			run := func(_, dir string, _ []string) (string, error) {
				if dir == "city" {
					return tc.cityRow, nil
				}
				return tc.rigRow, nil
			}
			_, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if gotStore.dir != tc.wantDir {
				t.Fatalf("store.dir = %q, want %q", gotStore.dir, tc.wantDir)
			}
		})
	}
}

// TestBestStoreWithWorkShortCircuitsOwnInProgress pins the resume carve-out:
// this session's own interrupted work is unconditional, so the primary store's
// in_progress row is taken without consulting any federated store at all.
func TestBestStoreWithWorkShortCircuitsOwnInProgress(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	var calls []string
	run := func(_, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		if dir == "city" {
			return `[{"id":"ci-1","priority":3,"status":"in_progress","assignee":"me"}]`, nil
		}
		return `[{"id":"va-1","priority":0,"status":"in_progress","assignee":"me"}]`, nil
	}
	_, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotStore.dir != "city" {
		t.Fatalf("store.dir = %q, want city (own in_progress work is unconditional)", gotStore.dir)
	}
	if len(calls) != 1 || calls[0] != "city" {
		t.Fatalf("calls = %v, want [city] — the resume path must not query rig stores", calls)
	}
}

// TestBestStoreWithWorkDegradesToFirstHitOnUnrankableOutput pins the degradation
// rule: a work_query that does not emit a JSON array of objects cannot be
// compared, so selection falls back to the pre-existing first-hit behavior
// rather than reordering on a comparison that was never made.
func TestBestStoreWithWorkDegradesToFirstHitOnUnrankableOutput(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return "va-1 some plain-text row", nil
		}
		return `[{"id":"va-2","priority":0}]`, nil
	}
	_, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotStore.dir != "city" {
		t.Fatalf("store.dir = %q, want city (unrankable output degrades to first-hit)", gotStore.dir)
	}
}

// TestBestHookCandidateRank exercises the ranking primitive directly, including
// the wire-shape distinction that motivates hookDefaultCandidatePriority: bd's
// priority is *int with omitempty, so an ABSENT priority must not be read as P0.
func TestBestHookCandidateRank(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready string
		want  hookCandidateRank
		ok    bool
	}{
		{"routed with priority", `[{"id":"a","priority":1}]`, hookCandidateRank{hookTierRouted, 1}, true},
		{"absent priority is not P0", `[{"id":"a"}]`, hookCandidateRank{hookTierRouted, hookDefaultCandidatePriority}, true},
		{"assignee lifts the tier", `[{"id":"a","assignee":"me","priority":3}]`, hookCandidateRank{hookTierAssigned, 3}, true},
		{"in_progress is the top tier", `[{"id":"a","assignee":"me","status":"in_progress","priority":3}]`, hookCandidateRank{hookTierInProgress, 3}, true},
		{"blank assignee stays routed", `[{"id":"a","assignee":"  ","priority":1}]`, hookCandidateRank{hookTierRouted, 1}, true},
		{"best of several rows wins", `[{"id":"a","priority":3},{"id":"b","priority":0}]`, hookCandidateRank{hookTierRouted, 0}, true},
		{"empty array is unrankable", `[]`, hookCandidateRank{}, false},
		{"non-JSON is unrankable", `not json`, hookCandidateRank{}, false},
		{"array of non-objects is unrankable", `["a"]`, hookCandidateRank{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := bestHookCandidateRank(tc.ready)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("rank = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBestStoreWithWorkReturnsLastWhenNoneHasWork(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, _ string, _ []string) (string, error) { return `[]`, nil }
	out, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[]` {
		t.Fatalf("out = %q, want []", out)
	}
	if gotStore.dir != "" || len(gotStore.env) != 0 {
		t.Fatalf("store = %#v, want zero value when no work is found", gotStore)
	}
}

func TestBestStoreWithWorkSurfacesOwnStoreErrorWhenNoWork(t *testing.T) {
	// The agent's own store (first) timing out must be surfaced even if a
	// federated rig store returns no work — otherwise emitCityWorkQueryFailure
	// never fires and a transient timeout is silently downgraded to "no work".
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return "", errTestStoreTimeout
		}
		return `[]`, nil
	}
	if _, _, err := bestStoreWithWork("q", stores, stores[0], run); !errors.Is(err, errTestStoreTimeout) {
		t.Fatalf("own-store error must be surfaced when no store has work; got %v", err)
	}
}

func TestBestStoreWithWorkIgnoresRigStoreErrorWhenOwnStoreHasNoWork(t *testing.T) {
	// A flaky federated rig store must not wedge the hook: when the agent's own
	// store is healthy (no work), a rig-store error is best-effort and dropped.
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[]`, nil
		}
		return "", errTestStoreTimeout
	}
	out, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("rig-store error must not surface when own store is healthy; got %v", err)
	}
	if out != `[]` {
		t.Fatalf("out = %q, want city store's no-work output", out)
	}
	if gotStore.dir != "" || len(gotStore.env) != 0 {
		t.Fatalf("store = %#v, want zero value when no work is found", gotStore)
	}
}

func TestBestStoreWithWorkSkipsStoreWithOnlyUnreadyRows(t *testing.T) {
	// A store whose only row is dep-blocked is NOT a hit; federation moves on.
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"x","blocked_by":[{"status":"open"}]}]`, nil
		}
		return `[{"id":"va-2"}]`, nil
	}
	out, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[{"id":"va-2"}]` {
		t.Fatalf("out = %q, want riga work (city row was unready)", out)
	}
	if gotStore.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga", gotStore.dir)
	}
}

// TestClaimStoreWithFallbackFallsBackWhenSelectedStoreRerunsEmpty pins the
// post-merge fix for the bundled gc hook --claim change: when the
// discovery-selected store loses its claimable row before the claim, the claim
// must re-select across the federated stores instead of draining as "no work"
// while a later store still has ready routed work.
func TestClaimStoreWithFallbackFallsBackWhenSelectedStoreRerunsEmpty(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	selected := stores[0]
	var calls []string
	run := func(_, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		switch len(calls) {
		case 1: // claim-time re-validation of the selected store: now empty.
			return `[]`, nil
		case 2: // federated re-selection: own store still empty.
			return `[]`, nil
		case 3: // later store still has ready routed work.
			return `[{"id":"va-3"}]`, nil
		default:
			t.Fatalf("unexpected call %d to %q", len(calls), dir)
			return "", nil
		}
	}

	out, gotStore, err := claimStoreWithFallback("q", stores, selected, stores[0], "", run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[{"id":"va-3"}]` {
		t.Fatalf("out = %q, want later-store work", out)
	}
	if gotStore.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga", gotStore.dir)
	}
	if len(calls) != 3 || calls[0] != "city" || calls[1] != "city" || calls[2] != "riga" {
		t.Fatalf("calls = %v, want [city city riga]", calls)
	}
}

// TestClaimStoreWithFallbackUsesSelectedStoreWhenStillReady covers the common
// path: when the selected store still reports ready work at claim time, the
// claim acts on that store's fresh output without a redundant federated rescan.
func TestClaimStoreWithFallbackUsesSelectedStoreWhenStillReady(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	selected := stores[0]
	var calls []string
	run := func(_, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		return `[{"id":"va-1"}]`, nil
	}

	out, gotStore, err := claimStoreWithFallback("q", stores, selected, stores[0], "", run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[{"id":"va-1"}]` {
		t.Fatalf("out = %q, want selected-store work", out)
	}
	if gotStore.dir != "city" {
		t.Fatalf("store.dir = %q, want city", gotStore.dir)
	}
	if len(calls) != 1 || calls[0] != "city" {
		t.Fatalf("calls = %v, want a single [city] re-validation", calls)
	}
}
