package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

var errTestStoreTimeout = errors.New("store timed out")

// withHookStoreLegConcurrency pins the store-leg fan-out width for one test
// and restores the production default on cleanup. Tests that assert read
// order or count reads through a shared slice pin it to 1: at width 1 the
// fan-out is exactly the pre-concurrency serial behavior, one store at a
// time, so those assertions keep judging the selection, not the scheduling.
func withHookStoreLegConcurrency(t *testing.T, n int) {
	t.Helper()
	prev := hookStoreLegConcurrency
	hookStoreLegConcurrency = n
	t.Cleanup(func() { hookStoreLegConcurrency = prev })
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
	withHookStoreLegConcurrency(t, 1)
	stores := []hookStore{{dir: "city"}, {dir: "riga"}, {dir: "rigb"}}
	var calls []string
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		if dir == "riga" {
			return `[{"id":"va-1"}]`, nil
		}
		return `[]`, nil
	}
	out, gotStore, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
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
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"ci-1","priority":2}]`, nil
		}
		return `[{"id":"va-1","priority":0}]`, nil
	}
	out, gotStore, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
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
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"ci-1","priority":1}]`, nil
		}
		return `[{"id":"va-1","priority":3}]`, nil
	}
	_, gotStore, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
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
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"ci-1","priority":1}]`, nil
		}
		return `[{"id":"va-1","priority":1}]`, nil
	}

	withHookTieBreakClock(t, time.Unix(0, 0))
	_, gotCity, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotCity.dir != "city" {
		t.Fatalf("store.dir = %q, want city at tie-break clock offset 0", gotCity.dir)
	}

	withHookTieBreakClock(t, time.Unix(0, 1))
	_, gotRiga, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
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
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		return `[{"id":"tied-` + dir + `","priority":1}]`, nil
	}

	orig := hookTieBreakClock
	defer func() { hookTieBreakClock = orig }()

	seen := map[string]bool{}
	for i := 0; i < len(stores); i++ {
		offset := int64(i)
		hookTieBreakClock = func() time.Time { return time.Unix(0, offset) }
		_, gotStore, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
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
	run := func(_ context.Context, _, _ string, _ []string) (string, error) {
		// Same id, same rank, from every store: a co-resident duplicate, not
		// two different pieces of work.
		return `[{"id":"dup-1","assignee":"worker-1"}]`, nil
	}

	// Offset 1 is exactly the clock value TestBestStoreWithWorkRotatesExactTies
	// uses to prove rotation moves off the first store; here the tied
	// candidates share an id, so it must NOT move off riga.
	withHookTieBreakClock(t, time.Unix(0, 1))
	_, got, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
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
			name:    "assigned in one store does not beat routed at better priority in another",
			cityRow: `[{"id":"ci-1","priority":0}]`,
			rigRow:  `[{"id":"va-1","priority":3,"assignee":"me"}]`,
			wantDir: "city",
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
			run := func(_ context.Context, _, dir string, _ []string) (string, error) {
				if dir == "city" {
					return tc.cityRow, nil
				}
				return tc.rigRow, nil
			}
			_, gotStore, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if gotStore.dir != tc.wantDir {
				t.Fatalf("store.dir = %q, want %q", gotStore.dir, tc.wantDir)
			}
		})
	}
}

// TestBestStoreWithWorkTreatsAssignedTierAsRoutedAcrossStores is the direct
// regression test for ga-t922vm. TestBestStoreWithWorkRanksTierAheadOfPriority
// above correctly pins that tier dominates priority WITHIN a single store's
// ranking (that mirrors bd's own three-tier work_query order). But
// bestStoreWithWork was reusing that exact same tier comparison ACROSS
// stores too — so a merely assigned-but-open bead in the primary store
// permanently outranked routed rig work at any priority, and #5491's tie
// rotation never got a chance to fire because the two candidates were never
// at equal rank. Confirmed live: gm-j3o0fo (tier=assigned, priority=1) beat
// ga-4twfqq (tier=routed, priority=1) on 10 consecutive gc hook runs even
// though the rig bead was P0-equivalent urgent and routed specifically to
// this agent's rig.
//
// Here the primary store's assigned bead is P3 and the rig's routed bead is
// P0 — the rig bead must win once assigned and routed are tie-equivalent
// across stores.
func TestBestStoreWithWorkTreatsAssignedTierAsRoutedAcrossStores(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"ci-1","priority":3,"assignee":"x"}]`, nil
		}
		return `[{"id":"va-1","priority":0}]`, nil
	}
	_, gotStore, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotStore.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga (routed P0 in a rig store must not lose to an assigned-but-open P3 in the primary store)", gotStore.dir)
	}
}

// TestBestStoreWithWorkShortCircuitsOwnInProgress pins the resume carve-out:
// this session's own interrupted work is unconditional, so the primary store's
// in_progress row is taken without consulting any federated store at all.
func TestBestStoreWithWorkShortCircuitsOwnInProgress(t *testing.T) {
	withHookStoreLegConcurrency(t, 1)
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	var calls []string
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		if dir == "city" {
			return `[{"id":"ci-1","priority":3,"status":"in_progress","assignee":"me"}]`, nil
		}
		return `[{"id":"va-1","priority":0,"status":"in_progress","assignee":"me"}]`, nil
	}
	_, gotStore, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
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
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		if dir == "city" {
			return "va-1 some plain-text row", nil
		}
		return `[{"id":"va-2","priority":0}]`, nil
	}
	_, gotStore, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
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
	run := func(_ context.Context, _, _ string, _ []string) (string, error) { return `[]`, nil }
	out, gotStore, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
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
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		if dir == "city" {
			return "", errTestStoreTimeout
		}
		return `[]`, nil
	}
	if _, _, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run); !errors.Is(err, errTestStoreTimeout) {
		t.Fatalf("own-store error must be surfaced when no store has work; got %v", err)
	}
}

func TestBestStoreWithWorkSurfacesRigStoreErrorWhenOwnStoreHasNoWork(t *testing.T) {
	// The agent's own store is healthy and empty, but a rig store could not be
	// read, so "no work" is not established. This used to drop the rig error as
	// best-effort and answer the city store's "[]", which drained seats whose
	// work sat in the unread rig. The error now surfaces as a secondary-store
	// failure: a failed query to the caller, but not this session's own store
	// failing (hookStoreFailureReport emits it under the rig store's name).
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[]`, nil
		}
		return "", errTestStoreTimeout
	}
	out, gotStore, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
	var secondary *hookSecondaryStoreFailure
	if !errors.As(err, &secondary) {
		t.Fatalf("err = %v, want a *hookSecondaryStoreFailure", err)
	}
	if !errors.Is(err, errTestStoreTimeout) {
		t.Fatalf("err = %v, want it to wrap the rig store's own error", err)
	}
	if out != "" {
		t.Fatalf("out = %q, want no output alongside the failure", out)
	}
	if gotStore.dir != "" || len(gotStore.env) != 0 {
		t.Fatalf("store = %#v, want zero value when no work is found", gotStore)
	}
}

func TestBestStoreWithWorkOwnStoreErrorTakesPrecedenceOverRigStoreError(t *testing.T) {
	// Both stores fail: the agent's own store's error is the one surfaced, as
	// itself, so emitCityWorkQueryFailure still classifies it.
	errRig := errors.New("rig store down")
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		if dir == "city" {
			return "", errTestStoreTimeout
		}
		return "", errRig
	}
	_, _, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
	if !errors.Is(err, errTestStoreTimeout) {
		t.Fatalf("err = %v, want the own store's error", err)
	}
	var secondary *hookSecondaryStoreFailure
	if errors.As(err, &secondary) {
		t.Fatalf("err = %v, want the own store's error surfaced as itself, not as a secondary failure", err)
	}
}

// TestHookStoreFailureReportEmitsEachFailedStoreOnce pins the event half of a
// query that failed on secondary stores: each failed store is emitted on its
// own, as itself, once per invocation however many failed reads name it; a
// failure the answer did not stop on is a stderr line, once, and no event; and
// the agent's own store's failure is still emitted as the query's error.
func TestHookStoreFailureReportEmitsEachFailedStoreOnce(t *testing.T) {
	var emitted []error
	emit := func(_ string, err error) { emitted = append(emitted, err) }
	var stderr strings.Builder
	report := newHookStoreFailureReport("gc hook --claim", &stderr, emit)
	riga := &hookStoreReadError{dir: "riga", rig: "riga", err: errTestStoreTimeout}
	rigb := &hookStoreReadError{dir: "rigb", rig: "rigb", err: errTestStoreTimeout}
	rigc := &hookStoreReadError{dir: "rigc", rig: "rigc", err: errTestStoreTimeout}
	secondary := &hookSecondaryStoreFailure{failures: []*hookStoreReadError{riga, rigb}}

	report.failed("q", secondary)
	report.failed("q", hookClaimRetryWindowClosed(secondary, &hookClaimOps{}))
	report.failed("q", nil)
	if len(emitted) != 2 || !errors.Is(emitted[0], riga) || !errors.Is(emitted[1], rigb) {
		t.Fatalf("emitted %v, want riga's and rigb's failures, once each", emitted)
	}

	report.note(rigc)
	report.note(rigc)
	if len(emitted) != 2 {
		t.Fatalf("emitted %v; a failure the answer did not stop on is not a failed query", emitted)
	}
	if got := strings.Count(stderr.String(), `rig "rigc"`); got != 1 {
		t.Fatalf("stderr names rigc %d times, want once: %q", got, stderr.String())
	}

	report.failed("q", errTestStoreTimeout)
	if len(emitted) != 3 || !errors.Is(emitted[2], errTestStoreTimeout) {
		t.Fatalf("emitted %v, want the own store's failure emitted as itself", emitted)
	}
}

func TestBestStoreWithWorkSkipsStoreWithOnlyUnreadyRows(t *testing.T) {
	// A store whose only row is dep-blocked is NOT a hit; federation moves on.
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"x","blocked_by":[{"status":"open"}]}]`, nil
		}
		return `[{"id":"va-2"}]`, nil
	}
	out, gotStore, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
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
	withHookStoreLegConcurrency(t, 1)
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	selected := stores[0]
	var calls []string
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
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

	out, gotStore, err := claimStoreWithFallback(context.Background(), "q", stores, selected, stores[0], "", run)
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
	withHookStoreLegConcurrency(t, 1)
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	selected := stores[0]
	var calls []string
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		return `[{"id":"va-1"}]`, nil
	}

	out, gotStore, err := claimStoreWithFallback(context.Background(), "q", stores, selected, stores[0], "", run)
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

// TestBestStoreWithWorkOverlapsStoreLegs proves the fan-out actually overlaps:
// every one of five store legs blocks until all five have entered, so the only
// way the query can answer at all is for the legs to run concurrently. If they
// are serial, the first leg's 5s barrier timeout fires and names the leg that
// waited alone. The rows all tie so the winner is the tie-break clock's choice;
// the assertion is the barrier, not the winner.
func TestBestStoreWithWorkOverlapsStoreLegs(t *testing.T) {
	// Pin the production width explicitly: this test asserts overlap AT the
	// width the hook really runs at, not a wider-than-default accident.
	withHookStoreLegConcurrency(t, 8)
	const barrierTimeout = 5 * time.Second
	stores := make([]hookStore, 5)
	for i := range stores {
		stores[i] = hookStore{dir: fmt.Sprintf("rig-%d", i)}
	}
	entered := make(chan string, len(stores))
	release := make(chan struct{})
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		select {
		case entered <- dir:
		case <-time.After(barrierTimeout):
			t.Errorf("leg %q waited alone for %s: store legs did not overlap", dir, barrierTimeout)
			return "", nil
		}
		select {
		case <-release:
			return `[{"id":"tied-` + dir + `","priority":1}]`, nil
		case <-time.After(barrierTimeout):
			t.Errorf("leg %q never released: the barrier never assembled", dir)
			return "", nil
		}
	}
	go func() {
		seen := make([]string, 0, len(stores))
		for len(seen) < len(stores) {
			select {
			case d := <-entered:
				seen = append(seen, d)
			case <-time.After(barrierTimeout):
				t.Errorf("only %d of %d store legs started (%v): the fan-out is serial, not overlapping", len(seen), len(stores), seen)
				close(release)
				return
			}
		}
		close(release)
	}()

	started := time.Now()
	out, _, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= barrierTimeout {
		t.Fatalf("discovery took %s, at or over the %s barrier timeout: the legs did not overlap", elapsed.Round(time.Millisecond), barrierTimeout)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), `[{"id":"tied-`) {
		t.Fatalf("out = %q, want one tied store row", out)
	}
}

// hookSelectionSchedule is one ordering of the store legs' per-store delays:
// which leg is made slow, which fast. The answers are fixed; the schedule
// decides only WHEN each answer is ready relative to the others, so a
// selection that depends on completion order produces different answers
// under different schedules.
type hookSelectionSchedule struct {
	name string
	// delaysFor assigns a delay to leg i of stores, in sweep order.
	delaysFor func(stores []hookStore) []time.Duration
	width     int
}

// hookSelectionRow is one fixed set of store answers and the selection the
// stores' ORDER alone must produce, whatever order the answers arrive in.
type hookSelectionRow struct {
	name    string
	stores  func() []hookStore
	answers map[string]hookStoreLegResult
	wantOut string
	wantDir string
	// wantErrClass matches a rendered error class (see hookSelectionErrClass).
	// "" wants a nil error.
	wantErrClass string
	wantNoted    []string
}

// hookSelectionErrClass renders err into a comparable value with no pointers
// in it: nil errors are "ok", and a hookSecondaryStoreFailure is named as the
// aggregate it is. The text is deterministic for a given set of failed stores in
// consumption order, which is exactly the order under test.
func hookSelectionErrClass(err error) string {
	if err == nil {
		return "ok"
	}
	var secondary *hookSecondaryStoreFailure
	if errors.As(err, &secondary) {
		return "secondary failure naming " + strconv.Itoa(len(secondary.failures)) + " store(s): " + err.Error()
	}
	return "err: " + err.Error()
}

// runHookSelectionRow runs one row's fixture under one schedule and returns
// the selection's whole observable outcome, plus canceledLegs, which receives
// one token per leg that was still in flight and observed the shared context
// cancel (the early-return cancellation edge). The wakeups are asynchronous,
// so a caller asserting on cancellation waits on the channel rather than
// reading a counter at return time.
func runHookSelectionRow(t *testing.T, row hookSelectionRow, schedule hookSelectionSchedule) (out string, dir string, errClass string, noted []string, canceledLegs <-chan struct{}) {
	t.Helper()
	withHookStoreLegConcurrency(t, schedule.width)
	stores := row.stores()
	delays := schedule.delaysFor(stores)
	canceled := make(chan struct{}, len(stores))
	run := func(ctx context.Context, _, dir string, _ []string) (string, error) {
		select {
		case <-time.After(delays[hookStoreIndex(stores, dir)]):
		case <-ctx.Done():
			canceled <- struct{}{}
			res := row.answers[dir]
			return res.out, res.err
		}
		res := row.answers[dir]
		return res.out, res.err
	}
	out, selected, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run, func(failure *hookStoreReadError) {
		noted = append(noted, failure.Error())
	})
	return out, selected.dir, hookSelectionErrClass(err), noted, canceled
}

func hookStoreIndex(stores []hookStore, dir string) int {
	for i := range stores {
		if stores[i].dir == dir {
			return i
		}
	}
	return 0
}

// TestBestStoreWithWorkSelectionIndependentOfCompletionOrder pins the whole
// behavioral contract of the concurrent fan-out (#5533, AC-2): the stores have
// no obligation order between them — ranking compares their answers after the
// fact — so the legs overlap, but every answer is consumed strictly in store
// order and the selection a set of store answers produces must not depend on
// WHEN each answer was ready. Each row encodes today's serial answers (the
// width-1 sweep IS the serial behavior), and every overlapping sweep of the
// same fixture must reproduce them byte for byte: same output, same store,
// same error class, same noted failures — however the legs' delays are
// permuted.
func TestBestStoreWithWorkSelectionIndependentOfCompletionOrder(t *testing.T) {
	// Pin the tie-break clock: a rotating tie reads the clock, and this test
	// compares one invocation's answer across several schedulings, so wall
	// clock would vary between them for no reason.
	prevClock := hookTieBreakClock
	hookTieBreakClock = func() time.Time { return time.Unix(1800000000, 0) }
	t.Cleanup(func() { hookTieBreakClock = prevClock })

	const (
		workCityInProg = `[{"id":"ci-1","priority":3,"status":"in_progress","assignee":"me"}]`
		workRigaP0     = `[{"id":"va-1","priority":0}]`
		workCityP2     = `[{"id":"ci-2","priority":2}]`
		resumeRiga     = `[{"id":"res-1","priority":2,"status":"in_progress","assignee":"me"}]`
	)

	threeStores := func() []hookStore {
		return []hookStore{
			{dir: "city"},
			{dir: "riga", rig: "riga"},
			{dir: "rigb", rig: "rigb"},
		}
	}

	rows := []hookSelectionRow{
		{
			name:    "primary in-progress short-circuit",
			stores:  threeStores,
			answers: map[string]hookStoreLegResult{"city": {out: workCityInProg}, "riga": {out: workRigaP0}, "rigb": {out: "[]"}},
			wantOut: workCityInProg,
			wantDir: "city",
		},
		{
			name:    "rank winner across stores",
			stores:  threeStores,
			answers: map[string]hookStoreLegResult{"city": {out: workCityP2}, "riga": {out: workRigaP0}, "rigb": {out: "[]"}},
			wantOut: workRigaP0,
			wantDir: "riga",
		},
		{
			// A migrated bead is ready from more than one store under the SAME
			// id: the same row, not two tied pieces of work, and the first-seen
			// store keeps it (hookTiedIDSeen) whatever order they answer in.
			name:   "pinned tie (same migrated id)",
			stores: threeStores,
			answers: map[string]hookStoreLegResult{
				"city": {out: `[{"id":"moved-1","priority":1}]`},
				"riga": {out: `[{"id":"moved-1","priority":1}]`},
				"rigb": {out: "[]"},
			},
			wantOut: `[{"id":"moved-1","priority":1}]`,
			wantDir: "city",
		},
		{
			name:   "unrankable output degrades to first-hit",
			stores: threeStores,
			answers: map[string]hookStoreLegResult{
				"city": {out: "va-1 some plain-text row"},
				"riga": {out: workRigaP0},
				"rigb": {out: "[]"},
			},
			wantOut: "va-1 some plain-text row",
			wantDir: "city",
		},
		{
			name:    "primary read error with work in a rig",
			stores:  threeStores,
			answers: map[string]hookStoreLegResult{"city": {err: errTestStoreTimeout}, "riga": {out: workRigaP0}, "rigb": {out: "[]"}},
			wantOut: workRigaP0,
			wantDir: "riga",
		},
		{
			name:         "primary read error with no work anywhere",
			stores:       threeStores,
			answers:      map[string]hookStoreLegResult{"city": {err: errTestStoreTimeout}, "riga": {out: "[]"}, "rigb": {out: "[]"}},
			wantOut:      "",
			wantDir:      "",
			wantErrClass: "err: " + errTestStoreTimeout.Error(),
		},
		{
			name:         "secondary failure only names the unread store",
			stores:       threeStores,
			answers:      map[string]hookStoreLegResult{"city": {out: "[]"}, "riga": {err: errTestStoreTimeout}, "rigb": {out: "[]"}},
			wantOut:      "",
			wantDir:      "",
			wantErrClass: "secondary failure naming 1 store(s):",
		},
		{
			// A rig whose managed Dolt runtime is down does not hold the answer
			// up: it is passed to noted and the invocation answers without it.
			name:   "down managed rig is noted, not fatal",
			stores: threeStores,
			answers: map[string]hookStoreLegResult{
				"city": {out: "[]"},
				"riga": {out: "[]"},
				"rigb": {err: fmt.Errorf("%w: %w", errHookRigRuntimeUnavailable, errTestStoreTimeout)},
			},
			wantOut: "[]",
			wantDir: "",
			wantNoted: []string{
				`work query against rig "rigb" store rigb failed: ` + errHookRigRuntimeUnavailable.Error() + ": " + errTestStoreTimeout.Error(),
			},
		},
		{
			// The federated reader pinned to the primary, primary fails: every
			// extra leg is a partial view, so no answer may come from them and
			// the primary's error is the invocation's answer.
			name: "federated-pinned primary failure without a resume answer",
			stores: func() []hookStore {
				stores := threeStores()
				stores[1].command = "q-single-store"
				stores[2].command = "q-single-store"
				return stores
			},
			answers:      map[string]hookStoreLegResult{"city": {err: errTestStoreTimeout}, "riga": {out: workRigaP0}, "rigb": {out: "[]"}},
			wantOut:      "",
			wantDir:      "",
			wantErrClass: "err: " + errTestStoreTimeout.Error(),
		},
		{
			// ...with the single exception that a resume row is this session's
			// own work: no federated view could overturn it, and refusing it
			// would regress crash recovery whenever the primary is down.
			name: "federated-pinned primary failure keeps the resume answer",
			stores: func() []hookStore {
				stores := threeStores()
				stores[1].command = "q-single-store"
				stores[2].command = "q-single-store"
				return stores
			},
			answers: map[string]hookStoreLegResult{"city": {err: errTestStoreTimeout}, "riga": {out: resumeRiga}, "rigb": {out: "[]"}},
			wantOut: resumeRiga,
			wantDir: "riga",
		},
	}

	// The schedules: per-leg delays in store order, reversed, and seeded-random
	// (the permutation is fixed by the seed so a failure reproduces), once at
	// the production width and once at width 1, which is exactly the serial
	// behavior and therefore the oracle every other sweep must match.
	ascDelays := func(n int) []time.Duration {
		delays := make([]time.Duration, n)
		for i := range delays {
			delays[i] = time.Duration(i) * 30 * time.Millisecond
		}
		return delays
	}
	schedules := []hookSelectionSchedule{
		{
			name:  "delays in store order",
			width: 8,
			delaysFor: func(stores []hookStore) []time.Duration {
				return ascDelays(len(stores))
			},
		},
		{
			name:  "delays reversed",
			width: 8,
			delaysFor: func(stores []hookStore) []time.Duration {
				delays := ascDelays(len(stores))
				for i, j := 0, len(delays)-1; i < j; i, j = i+1, j-1 {
					delays[i], delays[j] = delays[j], delays[i]
				}
				return delays
			},
		},
		{
			name:  "delays shuffled (seeded)",
			width: 8,
			delaysFor: func(stores []hookStore) []time.Duration {
				delays := ascDelays(len(stores))
				rnd := rand.New(rand.NewSource(5533)) //nolint:gosec // a fixed seed is the point: a failure must reproduce
				rnd.Shuffle(len(delays), func(i, j int) { delays[i], delays[j] = delays[j], delays[i] })
				return delays
			},
		},
		{
			name:  "serial (width 1), delays in store order",
			width: 1,
			delaysFor: func(stores []hookStore) []time.Duration {
				return ascDelays(len(stores))
			},
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			type selectionObservation struct {
				out, dir, errClass string
				noted              []string
			}
			observed := make([]selectionObservation, len(schedules))
			for i, schedule := range schedules {
				out, dir, errClass, noted, _ := runHookSelectionRow(t, row, schedule)
				observed[i] = selectionObservation{out: out, dir: dir, errClass: errClass, noted: noted}
			}
			serial := observed[len(observed)-1] // the width-1 sweep: today's serial behavior
			for i, schedule := range schedules {
				obs := observed[i]
				if obs.out != row.wantOut {
					t.Errorf("%s: out = %q, want %q", schedule.name, obs.out, row.wantOut)
				}
				if obs.dir != row.wantDir {
					t.Errorf("%s: store.dir = %q, want %q", schedule.name, obs.dir, row.wantDir)
				}
				if row.wantErrClass == "" && obs.errClass != "ok" {
					t.Errorf("%s: errClass = %q, want no error", schedule.name, obs.errClass)
				}
				if row.wantErrClass != "" && !strings.HasPrefix(obs.errClass, row.wantErrClass) {
					t.Errorf("%s: errClass = %q, want prefix %q", schedule.name, obs.errClass, row.wantErrClass)
				}
				if fmt.Sprintf("%v", obs.noted) != fmt.Sprintf("%v", row.wantNoted) {
					t.Errorf("%s: noted = %v, want %v", schedule.name, obs.noted, row.wantNoted)
				}
				if i != len(observed)-1 && (obs.out != serial.out || obs.dir != serial.dir || obs.errClass != serial.errClass || fmt.Sprintf("%v", obs.noted) != fmt.Sprintf("%v", serial.noted)) {
					t.Errorf("%s: selection ({out %q dir %q err %q}, noted %v) differs from the serial answer ({out %q dir %q err %q}, noted %v)",
						schedule.name, obs.out, obs.dir, obs.errClass, obs.noted, serial.out, serial.dir, serial.errClass, serial.noted)
				}
			}
			// The early-return edge: in the short-circuit row, the legs still
			// in flight when the primary answered must observe the shared
			// context cancel. At width 1 nothing is in flight, so the sweep
			// that must show it is an overlapping one whose slow legs are
			// still sleeping when the primary answers.
			if row.name == "primary in-progress short-circuit" {
				canceling := hookSelectionSchedule{
					name:  "overlapping, primary first",
					width: 8,
					delaysFor: func(stores []hookStore) []time.Duration {
						delays := ascDelays(len(stores))
						for i := 1; i < len(delays); i++ {
							delays[i] = 250 * time.Millisecond
						}
						return delays
					},
				}
				_, _, _, _, canceled := runHookSelectionRow(t, row, canceling)
				sawCancel := 0
				deadline := time.After(2 * time.Second)
			awaitCancel:
				for {
					select {
					case <-canceled:
						sawCancel++
					case <-deadline:
						break awaitCancel
					}
				}
				if sawCancel == 0 {
					t.Errorf("no leg observed ctx.Done() after the short-circuit return: the legs still in flight were not canceled")
				}
			}
		})
	}
}
