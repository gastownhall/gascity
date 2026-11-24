package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

var errTestStoreTimeout = errors.New("store timed out")

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
	pinHookStoreLegsSerial(t) // asserts the serial call sequence
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
	pinHookStoreLegsSerial(t) // asserts the serial call sequence
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
	pinHookStoreLegsSerial(t) // asserts the serial call sequence
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

// pinHookStoreLegsSerial reads store legs one at a time, in store order, for
// the rest of t: tests that assert the order of reads, or record them in a
// shared slice, pin it.
func pinHookStoreLegsSerial(t *testing.T) {
	t.Helper()
	prev := hookStoreLegConcurrency
	hookStoreLegConcurrency = 1
	t.Cleanup(func() { hookStoreLegConcurrency = prev })
}

// TestBestStoreWithWorkOverlapsStoreLegs pins the store-leg concurrency of
// #5533 deterministically: each of five legs blocks until all five have
// entered, so the selection finishes only when every leg is in flight at once.
// Read one store at a time, the first leg waits alone for a second that never
// starts.
func TestBestStoreWithWorkOverlapsStoreLegs(t *testing.T) {
	const barrierTimeout = 5 * time.Second
	stores := []hookStore{{dir: "city"}, {dir: "sl"}, {dir: "syst"}, {dir: "te"}, {dir: "wr"}}
	var entered sync.WaitGroup
	entered.Add(len(stores))
	allIn := make(chan struct{})
	go func() {
		entered.Wait()
		close(allIn)
	}()
	run := func(_ context.Context, _, dir string, _ []string) (string, error) {
		entered.Done()
		select {
		case <-allIn:
			return `[]`, nil
		case <-time.After(barrierTimeout):
			return "", fmt.Errorf("leg %s waited %s alone: the other legs were not started while it ran", dir, barrierTimeout)
		}
	}
	out, selected, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("bestStoreWithWork: %v", err)
	}
	if strings.TrimSpace(out) != "[]" || !isZeroHookStore(selected) {
		t.Fatalf("out = %q, store = %+v; want the no-work answer []", out, selected)
	}
}

// hookLegAnswer is one store leg's fixed read result in
// TestBestStoreWithWorkSelectionIndependentOfCompletionOrder.
type hookLegAnswer struct {
	out string
	err error
}

// hookSelection is what bestStoreWithWork decided, reduced to what a caller
// acts on: the output, the store it came from, the class of error and the
// failed stores passed to noted, in the order they were noted.
type hookSelection struct {
	out   string
	store string
	err   string
	noted []string
}

func hookSelectionErrClass(err error) string {
	if err == nil {
		return "nil"
	}
	var secondary *hookSecondaryStoreFailure
	if errors.As(err, &secondary) {
		dirs := make([]string, 0, len(secondary.failures))
		for _, f := range secondary.failures {
			dirs = append(dirs, f.dir)
		}
		return "secondary:" + strings.Join(dirs, ",")
	}
	return "own:" + err.Error()
}

// TestBestStoreWithWorkSelectionIndependentOfCompletionOrder pins #5533's
// selection invariant: reading the store legs concurrently must not change what
// bestStoreWithWork answers. For each set of fixed per-store results, the
// answer read one store at a time is the reference, and the same results
// arriving in store order, reversed, and in a seeded shuffle must give the same
// output, store, error class and noted stores. Where a store answers early (own
// in-progress work), the legs still running must be canceled.
func TestBestStoreWithWorkSelectionIndependentOfCompletionOrder(t *testing.T) {
	const (
		inProgress = `[{"id":"ci-1","priority":3,"status":"in_progress","assignee":"me"}]`
		rigResume  = `[{"id":"va-9","priority":3,"status":"in_progress","assignee":"me"}]`
		p0         = `[{"id":"va-1","priority":0}]`
		p1         = `[{"id":"va-2","priority":1}]`
		p1b        = `[{"id":"vb-2","priority":1}]`
		p1c        = `[{"id":"ci-2","priority":1}]`
		p2         = `[{"id":"ci-3","priority":2}]`
		empty      = `[]`
		plain      = "va-5 some plain-text row"
	)
	errOwn := errors.New("own store read failed")
	errRig := errors.New("rig store read failed")
	errDown := fmt.Errorf("%w: %w", errHookRigRuntimeUnavailable, errTestStoreTimeout)
	plainStores := []hookStore{{dir: "city"}, {dir: "riga", rig: "riga"}, {dir: "rigb", rig: "rigb"}, {dir: "rigc", rig: "rigc"}}
	// The federated reader pinned to the primary: the extras run their own
	// single-store command (hookFederationPinnedToPrimary).
	pinnedStores := []hookStore{{dir: "city"}, {dir: "riga", rig: "riga", command: "single"}, {dir: "rigb", rig: "rigb", command: "single"}, {dir: "rigc", rig: "rigc", command: "single"}}

	rows := []struct {
		name         string
		stores       []hookStore
		answers      map[string]hookLegAnswer
		shortCircuit bool
	}{
		{name: "primary in-progress short-circuit", stores: plainStores, shortCircuit: true, answers: map[string]hookLegAnswer{
			"city": {out: inProgress}, "riga": {out: p0}, "rigb": {out: empty}, "rigc": {err: errRig},
		}},
		{name: "rank winner", stores: plainStores, answers: map[string]hookLegAnswer{
			"city": {out: p2}, "riga": {out: p1}, "rigb": {out: p0}, "rigc": {out: empty},
		}},
		{name: "pinned tie", stores: plainStores, answers: map[string]hookLegAnswer{
			"city": {out: p1c}, "riga": {out: p1}, "rigb": {out: p1b}, "rigc": {out: empty},
		}},
		{name: "unrankable first-hit", stores: plainStores, answers: map[string]hookLegAnswer{
			"city": {out: empty}, "riga": {out: plain}, "rigb": {out: p0}, "rigc": {out: p1},
		}},
		{name: "served with a failed secondary", stores: plainStores, answers: map[string]hookLegAnswer{
			"city": {out: p1c}, "riga": {err: errRig}, "rigb": {out: empty}, "rigc": {err: errDown},
		}},
		{name: "primary error with work in a rig", stores: plainStores, answers: map[string]hookLegAnswer{
			"city": {err: errOwn}, "riga": {out: empty}, "rigb": {out: p1}, "rigc": {out: empty},
		}},
		{name: "primary error with no work anywhere", stores: plainStores, answers: map[string]hookLegAnswer{
			"city": {err: errOwn}, "riga": {out: empty}, "rigb": {err: errRig}, "rigc": {out: empty},
		}},
		{name: "secondary failures only", stores: plainStores, answers: map[string]hookLegAnswer{
			"city": {out: empty}, "riga": {err: errRig}, "rigb": {out: empty}, "rigc": {err: errRig},
		}},
		{name: "down managed rig", stores: plainStores, answers: map[string]hookLegAnswer{
			"city": {out: empty}, "riga": {err: errDown}, "rigb": {out: empty}, "rigc": {out: empty},
		}},
		{name: "federated-pinned primary failure", stores: pinnedStores, answers: map[string]hookLegAnswer{
			"city": {err: errOwn}, "riga": {out: p0}, "rigb": {err: errRig}, "rigc": {out: empty},
		}},
		{name: "federated-pinned primary failure with a resume answer", stores: pinnedStores, shortCircuit: true, answers: map[string]hookLegAnswer{
			"city": {err: errOwn}, "riga": {out: empty}, "rigb": {out: rigResume}, "rigc": {out: p0},
		}},
	}

	const step = 15 * time.Millisecond
	type schedule struct {
		name  string
		delay func(i, n int) time.Duration
	}
	shuffled := rand.New(rand.NewSource(5533)).Perm(len(plainStores))
	schedules := []schedule{
		{name: "store order", delay: func(i, _ int) time.Duration { return time.Duration(i) * step }},
		{name: "reversed", delay: func(i, n int) time.Duration { return time.Duration(n-1-i) * step }},
		{name: "seeded shuffle", delay: func(i, _ int) time.Duration { return time.Duration(shuffled[i]) * step }},
	}

	selectWith := func(t *testing.T, stores []hookStore, answers map[string]hookLegAnswer, delay func(i, n int) time.Duration) hookSelection {
		t.Helper()
		index := make(map[string]int, len(stores))
		for i, st := range stores {
			index[st.dir] = i
		}
		run := func(ctx context.Context, _, dir string, _ []string) (string, error) {
			select {
			case <-time.After(delay(index[dir], len(stores))):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			a, ok := answers[dir]
			if !ok {
				t.Errorf("leg read unknown store %q", dir)
			}
			return a.out, a.err
		}
		var noted []string
		out, st, err := bestStoreWithWork(context.Background(), "q", stores, stores[0], run, func(f *hookStoreReadError) {
			noted = append(noted, f.dir)
		})
		return hookSelection{out: out, store: st.dir, err: hookSelectionErrClass(err), noted: noted}
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			withHookTieBreakClock(t, time.Unix(0, 1))
			prev := hookStoreLegConcurrency
			hookStoreLegConcurrency = 1
			want := selectWith(t, row.stores, row.answers, func(int, int) time.Duration { return 0 })
			hookStoreLegConcurrency = prev

			for _, sched := range schedules {
				got := selectWith(t, row.stores, row.answers, sched.delay)
				if got.out != want.out || got.store != want.store || got.err != want.err || strings.Join(got.noted, ",") != strings.Join(want.noted, ",") {
					t.Errorf("%s: selection = %+v, want the one-store-at-a-time %+v", sched.name, got, want)
				}
			}

			if !row.shortCircuit {
				return
			}
			// The answering store is fast and every later leg would take far
			// longer: an early answer must cancel them, not wait them out.
			const slow = 5 * time.Second
			answering := 0
			for i, st := range row.stores {
				if st.dir == want.store {
					answering = i
				}
			}
			later := len(row.stores) - answering - 1
			canceled := make(chan string, later)
			run := func(ctx context.Context, _, dir string, _ []string) (string, error) {
				i := 0
				for j, st := range row.stores {
					if st.dir == dir {
						i = j
					}
				}
				a := row.answers[dir]
				if i <= answering {
					return a.out, a.err
				}
				select {
				case <-time.After(slow):
					return a.out, a.err
				case <-ctx.Done():
					canceled <- dir
					return "", ctx.Err()
				}
			}
			start := time.Now()
			_, st, err := bestStoreWithWork(context.Background(), "q", row.stores, row.stores[0], run)
			if err != nil || st.dir != want.store {
				t.Fatalf("early answer = (%q, %v), want %q", st.dir, err, want.store)
			}
			if elapsed := time.Since(start); elapsed >= slow {
				t.Fatalf("early answer took %s: it waited for the later legs", elapsed)
			}
			// Cancellation reaches the later legs asynchronously.
			timeout := time.After(slow)
			for n := 0; n < later; n++ {
				select {
				case <-canceled:
				case <-timeout:
					t.Fatalf("%d of %d later legs observed ctx.Done() after the early answer", n, later)
				}
			}
		})
	}
}
