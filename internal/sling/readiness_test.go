package sling

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// testBlockedBead is the bead under test in this file: the one being routed,
// whose readiness the sling has to resolve before it will route.
const testBlockedBead = "BL-42"

// blockedStore seeds a store holding testBlockedBead plus the given dependency
// edges. Statuses are keyed by bead ID so a test can close a blocker by seeding
// it closed, exactly as the store's own Ready() cross-references status.
func blockedStore(deps []beads.Dep, statuses map[string]string) *beads.MemStore {
	seed := []beads.Bead{{ID: testBlockedBead, Title: testBlockedBead, Type: "task", Status: "open", Metadata: map[string]string{}}}
	for id, status := range statuses {
		seed = append(seed, beads.Bead{
			ID: id, Title: "blocker " + id, Type: "task", Status: status, Metadata: map[string]string{},
		})
	}
	return beads.NewMemStoreFrom(0, seed, deps)
}

func TestOpenBlockersReportsOnlyUnclosedReadyBlockingDeps(t *testing.T) {
	store := blockedStore([]beads.Dep{
		{IssueID: "BL-42", DependsOnID: "BL-open", Type: "blocks"},
		{IssueID: "BL-42", DependsOnID: "BL-waits", Type: "waits-for"},
		{IssueID: "BL-42", DependsOnID: "BL-done", Type: "blocks"},
		{IssueID: "BL-42", DependsOnID: "BL-related", Type: "relates-to"},
		{IssueID: "BL-42", DependsOnID: "BL-tracked", Type: "tracks"},
	}, map[string]string{
		"BL-open":    "open",
		"BL-waits":   "in_progress",
		"BL-done":    "closed",
		"BL-related": "open",
		"BL-tracked": "open",
	})

	blockers, err := OpenBlockers("BL-42", store)
	if err != nil {
		t.Fatalf("OpenBlockers error: %v", err)
	}
	got := BlockerIDs(blockers)
	want := []string{"BL-open", "BL-waits"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("OpenBlockers = %v, want %v (closed and informational edges must not block)", got, want)
	}
	if blockers[1].Status != "in_progress" {
		t.Errorf("blocker status = %q, want in_progress", blockers[1].Status)
	}
}

// A dep edge pointing at a bead that is not in the store still blocks: Ready()
// resolves its status to the empty string, which is not "closed". This is the
// inverted/dangling-edge shape that made the original incident undiagnosable.
func TestOpenBlockersTreatsMissingDependencyAsBlocking(t *testing.T) {
	store := blockedStore([]beads.Dep{
		{IssueID: "BL-42", DependsOnID: "BL-ghost", Type: "blocks"},
	}, nil)

	blockers, err := OpenBlockers("BL-42", store)
	if err != nil {
		t.Fatalf("OpenBlockers error: %v", err)
	}
	if len(blockers) != 1 || blockers[0].ID != "BL-ghost" {
		t.Fatalf("OpenBlockers = %v, want [BL-ghost]", BlockerIDs(blockers))
	}
	if !strings.Contains(blockers[0].String(), "not found in store") {
		t.Errorf("Blocker.String() = %q, want it to say the target is missing", blockers[0].String())
	}
}

func TestOpenBlockersReportsDuplicateEdgesOnce(t *testing.T) {
	store := blockedStore([]beads.Dep{
		{IssueID: "BL-42", DependsOnID: "BL-open", Type: "blocks"},
		{IssueID: "BL-42", DependsOnID: "BL-open", Type: "waits-for"},
	}, map[string]string{"BL-open": "open"})

	blockers, err := OpenBlockers("BL-42", store)
	if err != nil {
		t.Fatalf("OpenBlockers error: %v", err)
	}
	if len(blockers) != 1 {
		t.Fatalf("OpenBlockers = %v, want a single entry for BL-open", BlockerIDs(blockers))
	}
}

// TestDoSlingRefusesBlockedBead is the regression test for the reported bug:
// gc sling wrote gc.routed_to and printed a success line (convoy id, "Slung
// ... ->", controller poke) for a bead that no ready query could ever surface,
// so the operator waited on a dispatch that could not result in a claim.
func TestDoSlingRefusesBlockedBead(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "gastown.polecat", Dir: "flinders", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.Store = blockedStore([]beads.Dep{
		{IssueID: "BL-42", DependsOnID: "BL-blocker", Type: "blocks"},
	}, map[string]string{"BL-blocker": "open"})

	_, err := DoSling(testOpts(a, "BL-42"), deps, nil)
	if err == nil {
		t.Fatal("DoSling error = nil, want a refusal: the bead is blocked and can never be claimed")
	}
	var blockedErr *BlockedError
	if !errors.As(err, &blockedErr) {
		t.Fatalf("DoSling error = %T (%v), want *BlockedError", err, err)
	}
	if blockedErr.BeadID != "BL-42" {
		t.Errorf("BlockedError.BeadID = %q, want BL-42", blockedErr.BeadID)
	}
	if got := BlockerIDs(blockedErr.Blockers); len(got) != 1 || got[0] != "BL-blocker" {
		t.Errorf("BlockedError.Blockers = %v, want [BL-blocker]", got)
	}
	msg := err.Error()
	for _, want := range []string{"REFUSED", "BL-42", "BL-blocker", a.QualifiedName(), "--force"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	// The refusal must land before routing, so no route is written and no
	// controller poke starts a clock the operator thinks is running.
	if len(runner.calls) != 0 {
		t.Errorf("runner calls = %v, want none: the sling must refuse before routing", runner.calls)
	}
	bead, getErr := deps.Store.Get("BL-42")
	if getErr != nil {
		t.Fatalf("Get(BL-42) error: %v", getErr)
	}
	if routed := bead.Metadata["gc.routed_to"]; routed != "" {
		t.Errorf("gc.routed_to = %q, want empty: a refused sling must not route", routed)
	}
}

func TestDoSlingRoutesReadyBead(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	// Same graph shape as the refusal test, except the blocker is closed.
	deps.Store = blockedStore([]beads.Dep{
		{IssueID: "BL-42", DependsOnID: "BL-blocker", Type: "blocks"},
	}, map[string]string{"BL-blocker": "closed"})

	result, err := DoSling(testOpts(a, "BL-42"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.BeadID != "BL-42" {
		t.Errorf("BeadID = %q, want BL-42", result.BeadID)
	}
	if len(result.BeadWarnings) != 0 {
		t.Errorf("BeadWarnings = %v, want none for a ready bead", result.BeadWarnings)
	}
}

// --force is the escape hatch the refusal points operators at: it routes the
// blocked bead instead of refusing. It is also documented to suppress warnings
// and to dispatch even when the bead does not resolve in the local store, so
// it must not pay for the readiness lookup at all.
func TestDoSlingForceRoutesBlockedBead(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	store := &depCountingStore{MemStore: blockedStore([]beads.Dep{
		{IssueID: "BL-42", DependsOnID: "BL-blocker", Type: "blocks"},
	}, map[string]string{"BL-blocker": "open"})}
	deps.Store = store

	opts := testOpts(a, "BL-42")
	opts.Force = true
	result, err := DoSling(opts, deps, nil)
	if err != nil {
		t.Fatalf("DoSling --force error: %v", err)
	}
	if result.BeadID != "BL-42" {
		t.Errorf("BeadID = %q, want BL-42", result.BeadID)
	}
	if store.depListCalls != 0 {
		t.Errorf("DepList calls under --force = %d, want 0", store.depListCalls)
	}
}

// depCountingStore counts DepList calls so a test can assert that a code path
// does not pay for the readiness lookup.
type depCountingStore struct {
	*beads.MemStore
	depListCalls int
}

func (s *depCountingStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.depListCalls++
	return s.MemStore.DepList(id, direction)
}

func TestDoSlingDryRunWarnsBlockedBeadWithoutFailing(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.Store = blockedStore([]beads.Dep{
		{IssueID: "BL-42", DependsOnID: "BL-blocker", Type: "blocks"},
	}, map[string]string{"BL-blocker": "open"})

	opts := testOpts(a, "BL-42")
	opts.DryRun = true
	result, err := DoSling(opts, deps, nil)
	if err != nil {
		t.Fatalf("DoSling --dry-run error: %v", err)
	}
	if !result.DryRun {
		t.Fatal("result.DryRun = false, want true")
	}
	joined := strings.Join(result.BeadWarnings, "\n")
	if !strings.Contains(joined, "BL-blocker") || !strings.Contains(joined, "would not result in a claim") {
		t.Errorf("dry-run warnings %q must name the blockers and stay conditional", joined)
	}
}

// The readiness check only makes sense where an existing bead is what gets
// routed. Inline text and formula slings mint new beads/molecules that cannot
// carry blockers; --on routes the workflow root rather than the source bead,
// so the source bead's blockers do not decide the root's readiness; and
// --force both suppresses warnings and tolerates a bead that does not resolve
// locally. --dry-run stays in scope and downgrades to a warning.
func TestShouldCheckBlockedScope(t *testing.T) {
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	base := func() SlingOpts { return testOpts(a, "BL-42") }

	tests := []struct {
		name string
		opts SlingOpts
		want bool
	}{
		{"plain bead", base(), true},
		{"dry run", func() SlingOpts { o := base(); o.DryRun = true; return o }(), true},
		{"force", func() SlingOpts { o := base(); o.Force = true; return o }(), false},
		{"formula", func() SlingOpts { o := base(); o.IsFormula = true; return o }(), false},
		{"on formula", func() SlingOpts { o := base(); o.OnFormula = "mol-do-work"; return o }(), false},
		{"inline text", func() SlingOpts { o := base(); o.InlineText = true; return o }(), false},
		{"no bead", func() SlingOpts { o := base(); o.BeadOrFormula = ""; return o }(), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldCheckBlocked(tc.opts); got != tc.want {
				t.Errorf("shouldCheckBlocked(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// A store fault while resolving blockers is not proof that the bead is
// blocked. The sling must warn and proceed rather than refuse legitimate work.
func TestCheckBlockedWarnsAndProceedsOnStoreFault(t *testing.T) {
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := SlingDeps{Store: failingDepStore{}}
	var result SlingResult

	if err := checkBlocked(testOpts(a, "BL-42"), deps, a, &result); err != nil {
		t.Fatalf("checkBlocked error = %v, want nil: a store fault must not refuse the sling", err)
	}
	joined := strings.Join(result.BeadWarnings, "\n")
	if !strings.Contains(joined, "could not verify") || !strings.Contains(joined, "BL-42") {
		t.Errorf("warnings = %q, want an unverified-readiness warning naming BL-42", joined)
	}
}

// failingDepStore is a beads.Store whose DepList always fails, standing in for
// a store fault during the readiness check.
type failingDepStore struct {
	beads.Store
}

func (failingDepStore) DepList(string, string) ([]beads.Dep, error) {
	return nil, errors.New("store unreachable")
}
