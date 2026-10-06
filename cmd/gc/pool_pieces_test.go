package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/worktree"
)

// The P3-1 pieces are shared by legacy and the v2 allocator. Every test here
// pins one of two things: legacy decides exactly as it did before the
// extraction (against the frozen copies in pool_pieces_prerefactor_test.go),
// or a v2-only mode (bead-ID keying, planOnly) holds its contract.

const poolPiecesSeeds = 400

// awakeSeeds is larger because one awake differential costs microseconds and
// the shared-name collisions that tell the keyings apart are rare.
const awakeSeeds = 20000

// computeAwakeSetChecked is ComputeAwakeSet for fixtures. Each call also runs
// awakeKeyingDiff, so every fixture is a differential case for both keyings.
func computeAwakeSetChecked(t *testing.T, input AwakeInput) map[string]AwakeDecision {
	t.Helper()
	if diff := awakeKeyingDiff(input); diff != "" {
		t.Fatalf("awake keying differential: %s", diff)
	}
	return ComputeAwakeSet(input)
}

// awakeKeyingDiff reports how ComputeAwakeSet disagrees with the pre-refactor
// algorithm, or, when session names are unique, how bead-ID keying disagrees
// with session-name keying. It returns "" when they agree.
func awakeKeyingDiff(input AwakeInput) string {
	if diff := awakePreRefactorDiff(input); diff != "" {
		return diff
	}
	names := make(map[string]bool, len(input.SessionBeads))
	for _, b := range input.SessionBeads {
		if names[b.SessionName] {
			return ""
		}
		names[b.SessionName] = true
	}
	byName := ComputeAwakeSet(input)
	byID := computeAwakeSetKeyed(awakeInputKeyedByID(input), awakeKeyBeadID)
	if len(byID) != len(byName) {
		return fmt.Sprintf("bead-ID keying has %d decisions, session-name keying %d", len(byID), len(byName))
	}
	for _, b := range input.SessionBeads {
		if got, want := byID[b.ID], byName[b.SessionName]; got != want {
			return fmt.Sprintf("bead %s (%s): by ID %+v, by name %+v", b.ID, b.SessionName, got, want)
		}
	}
	return ""
}

// awakePreRefactorDiff reports how ComputeAwakeSet disagrees with the
// pre-refactor algorithm, or "" when they agree. Where the legacy result is
// map-order dependent, both sides are normalized first.
func awakePreRefactorDiff(input AwakeInput) string {
	got, want := ComputeAwakeSet(input), computeAwakeSetPreRefactor(input)
	if legacyAwakeMapOrderDependent(input) {
		got, want = normalizeScaledReasons(got), normalizeScaledReasons(want)
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Sprintf("name-keyed result differs from pre-refactor:\n got=%+v\nwant=%+v", got, want)
	}
	return ""
}

// legacyAwakeMapOrderDependent reports whether session-name keying is
// nondeterministic for input: rows of two templates that the scale_check pass
// ranges over (a Go map) share a session name, so whichever template comes
// last in map order picks the reason, scaled:demand or scaled:creating. The
// work_query pass also ranges over a map, but it writes one reason. The
// pre-refactor code has the same nondeterminism; normalizeScaledReasons
// removes it. Bead-ID keying avoids it (BEHAVIORS POOL-082, #8).
func legacyAwakeMapOrderDependent(input AwakeInput) bool {
	templatesByName := map[string]string{}
	for _, b := range input.SessionBeads {
		if input.ScaleCheckCounts[b.Template] <= 0 && !input.WorkSet[b.Template] {
			continue
		}
		if seen, ok := templatesByName[b.SessionName]; ok && seen != b.Template {
			return true
		}
		templatesByName[b.SessionName] = b.Template
	}
	return false
}

// normalizeScaledReasons folds scaled:creating into scaled:demand, the only
// difference map order can make (legacyAwakeMapOrderDependent).
func normalizeScaledReasons(m map[string]AwakeDecision) map[string]AwakeDecision {
	out := make(map[string]AwakeDecision, len(m))
	for k, d := range m {
		if d.Reason == "scaled:creating" {
			d.Reason = "scaled:demand"
		}
		out[k] = d
	}
	return out
}

// awakeInputKeyedByID re-keys the runtime maps from session name to bead ID.
func awakeInputKeyedByID(input AwakeInput) AwakeInput {
	rekey := func(byName map[string]bool) map[string]bool {
		if byName == nil {
			return nil
		}
		byID := make(map[string]bool)
		for _, b := range input.SessionBeads {
			if v, ok := byName[b.SessionName]; ok {
				byID[b.ID] = v
			}
		}
		return byID
	}
	input.RunningSessions = rekey(input.RunningSessions)
	input.AttachedSessions = rekey(input.AttachedSessions)
	input.PendingSessions = rekey(input.PendingSessions)
	return input
}

// randAwakeInput builds an awake input over a small vocabulary so that named
// fallbacks, shared names, aliases and every override collide often.
func randAwakeInput(r *rand.Rand, uniqueNames bool) AwakeInput {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	pick := func(xs ...string) string { return xs[r.IntN(len(xs))] }
	coin := func() bool { return r.IntN(3) == 0 }
	when := func() time.Time {
		switch r.IntN(6) {
		case 0:
			return now.Add(-2 * time.Hour)
		case 1:
			return now.Add(time.Hour)
		default:
			return time.Time{}
		}
	}
	templates := []string{"t0", "rig/t1", "t2", "t3"}
	in := AwakeInput{Now: now}
	if coin() {
		in.ChatIdleTimeout = 30 * time.Minute
	}
	if coin() {
		in.ManualGracePeriod = time.Minute
	}
	for _, tmpl := range templates {
		if r.IntN(5) == 0 {
			continue
		}
		a := AwakeAgent{QualifiedName: tmpl, Suspended: r.IntN(6) == 0, MinActiveSessions: r.IntN(3)}
		if coin() {
			a.SleepAfterIdle = time.Minute
		}
		in.Agents = append(in.Agents, a)
	}
	// n1's runtime name equals its identity, so a named session with no row
	// desires a name a plain row may carry.
	named := []AwakeNamedSession{
		{Identity: "rig/n0", Template: "rig/t1", RuntimeName: "rig--n0"},
		{Identity: "n1", Template: "t2", RuntimeName: "n1"},
	}
	for _, ns := range named {
		if r.IntN(4) == 0 {
			continue
		}
		ns.Mode = pick("always", "on_demand")
		in.NamedSessions = append(in.NamedSessions, ns)
		in.NamedSessionDemand = setRand(r, in.NamedSessionDemand, ns.Identity)
		in.NamedSessionRoutedDemand = setRand(r, in.NamedSessionRoutedDemand, ns.Identity)
		in.NamedSessionWorkQ = setRand(r, in.NamedSessionWorkQ, ns.Identity)
	}
	sharedNames := []string{"s-a", "s-b", "rig--n0", "n1", ""}
	used := map[string]bool{}
	n := r.IntN(9)
	for i := 0; i < n; i++ {
		name := pick(sharedNames...)
		if uniqueNames {
			if used[name] {
				name = "s-" + strconv.Itoa(i)
			}
			used[name] = true
		}
		b := AwakeSessionBead{
			ID:                        "b" + strconv.Itoa(i),
			SessionName:               name,
			Template:                  pick(append(templates, "t9")...),
			State:                     pick("active", "active", "creating", "start-pending", "asleep", "drained", "closed", "suspended", "failed-create"),
			SleepReason:               pick("", "", "city-stop", "idle-timeout", "drained"),
			ManualSession:             coin(),
			PendingCreate:             coin(),
			ExplicitWake:              coin(),
			DependencyOnly:            r.IntN(5) == 0,
			NamedIdentity:             pick("", "", "rig/n0", "n1"),
			Alias:                     pick("", "", "al-0", "rig/n0"),
			ConfiguredNamedSession:    coin(),
			Pinned:                    coin(),
			Drained:                   r.IntN(5) == 0,
			WaitHold:                  r.IntN(5) == 0,
			HeldUntil:                 when(),
			QuarantinedUntil:          when(),
			IdleSince:                 when(),
			CreatedAt:                 now.Add(-time.Duration(r.IntN(120)) * time.Second),
			RestartRequested:          coin(),
			ContinuationResetPending:  coin(),
			CurrentlyProcessingBeadID: pick("", "w0", "w1"),
			PostCreateProtected:       coin(),
		}
		in.SessionBeads = append(in.SessionBeads, b)
		in.RunningSessions = setRand(r, in.RunningSessions, name)
		in.AttachedSessions = setRand(r, in.AttachedSessions, name)
		in.PendingSessions = setRand(r, in.PendingSessions, name)
		in.ReadyWaitSet = setRand(r, in.ReadyWaitSet, b.ID)
	}
	assignees := []string{"", "al-0", "rig/n0", "n1", "s-a", "rig--n0"}
	for _, b := range in.SessionBeads {
		assignees = append(assignees, b.ID, b.SessionName)
	}
	for i := r.IntN(6); i > 0; i-- {
		in.WorkBeads = append(in.WorkBeads, AwakeWorkBead{
			ID:       "w" + strconv.Itoa(r.IntN(3)),
			Assignee: pick(assignees...),
			Status:   pick("open", "in_progress", "closed"),
			Ready:    coin(),
			Blocked:  coin(),
		})
	}
	for _, tmpl := range templates {
		if c := r.IntN(4); c > 0 {
			if in.ScaleCheckCounts == nil {
				in.ScaleCheckCounts = map[string]int{}
			}
			in.ScaleCheckCounts[tmpl] = c - 1
		}
		in.WorkSet = setRand(r, in.WorkSet, tmpl)
	}
	return in
}

func setRand(r *rand.Rand, m map[string]bool, k string) map[string]bool {
	if r.IntN(3) != 0 {
		return m
	}
	if m == nil {
		m = map[string]bool{}
	}
	m[k] = r.IntN(2) == 0
	return m
}

func TestComputeAwakeSetMatchesPreRefactor(t *testing.T) {
	for seed := uint64(0); seed < awakeSeeds; seed++ {
		r := rand.New(rand.NewPCG(seed, 1))
		in := randAwakeInput(r, r.IntN(2) == 0)
		if diff := awakePreRefactorDiff(in); diff != "" {
			t.Fatalf("seed %d: %s\ninput=%+v", seed, diff, in)
		}
	}
}

func TestComputeAwakeSetKeyedByIDMatchesNameKeyedWhenNamesUnique(t *testing.T) {
	// Every compute_awake_set_test.go fixture runs through both keyings via
	// computeAwakeSetChecked; this adds randomized inputs.
	for seed := uint64(0); seed < awakeSeeds; seed++ {
		in := randAwakeInput(rand.New(rand.NewPCG(seed, 2)), true)
		if diff := awakeKeyingDiff(in); diff != "" {
			t.Fatalf("seed %d: %s\ninput=%+v", seed, diff, in)
		}
	}
}

func TestComputeAwakeSetKeyedByIDKeepsRowsThatShareAName(t *testing.T) {
	// Enterprise slot-scoped names: one live owner and stale creating rows
	// share a session_name (BEHAVIORS POOL-082, #8).
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	rows := []AwakeSessionBead{
		{ID: "gc-live", SessionName: "rig--worker-2-pool", Template: "rig/worker", State: "active"},
		{ID: "gc-stale-1", SessionName: "rig--worker-2-pool", Template: "rig/worker", State: "creating", HeldUntil: now.Add(time.Hour)},
		{ID: "gc-stale-2", SessionName: "rig--worker-2-pool", Template: "rig/worker", State: "asleep"},
	}
	in := AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: "rig/worker"}},
		SessionBeads: rows,
		WorkBeads:    []AwakeWorkBead{{ID: "w1", Assignee: "gc-live", Status: "in_progress"}},
		Now:          now,
	}
	byName := ComputeAwakeSet(in)
	if len(byName) != 1 {
		t.Fatalf("session-name keying: %d decisions, want the rows collapsed into 1: %+v", len(byName), byName)
	}
	in.AttachedSessions = map[string]bool{"gc-stale-2": true}
	byID := computeAwakeSetKeyed(in, awakeKeyBeadID)
	want := map[string]AwakeDecision{
		"gc-live":    {ShouldWake: true, Reason: "assigned-work", HasAssignedWork: true, AssignedWorkBeadID: "w1", AssignedWorkClaimed: true},
		"gc-stale-1": {Reason: "held"},
		"gc-stale-2": {ShouldWake: true, Reason: "attached"},
	}
	if !reflect.DeepEqual(byID, want) {
		t.Fatalf("bead-ID keying:\n got=%+v\nwant=%+v", byID, want)
	}
}

// awakeBothKeyings runs in through both keyings (names in the fixtures are
// unique, so the runtime maps are left nil) and returns the bead-ID result.
func awakeBothKeyings(t *testing.T, in AwakeInput) (byName, byID map[string]AwakeDecision) {
	t.Helper()
	return computeAwakeSetChecked(t, in), computeAwakeSetKeyed(in, awakeKeyBeadID)
}

func TestComputeAwakeSetMinActiveCountsAsleepRowWithAnotherWake(t *testing.T) {
	// The explicitly woken asleep row covers min_active=1, so the city-stop
	// row stays asleep.
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	in := AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "worker", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{
			{ID: "gc-a", SessionName: "s-a", Template: "worker", State: "asleep", ExplicitWake: true},
			{ID: "gc-b", SessionName: "s-b", Template: "worker", State: "asleep", SleepReason: "city-stop"},
		},
		Now: now,
	}
	byName, byID := awakeBothKeyings(t, in)
	for _, got := range []map[string]AwakeDecision{{"gc-a": byName["s-a"], "gc-b": byName["s-b"]}, byID} {
		want := map[string]AwakeDecision{"gc-a": {ShouldWake: true, Reason: "explicit-wake"}, "gc-b": {}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decisions = %+v, want %+v", got, want)
		}
	}
}

func TestComputeAwakeSetKeyedByIDNamedResolvesToIdentityRow(t *testing.T) {
	// Two rows carry the named session's runtime name; only the second claims
	// its identity. Keyed by bead ID, only that row is the named session.
	in := AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "mayor"}},
		NamedSessions: []AwakeNamedSession{{Identity: "mayor", Template: "mayor", RuntimeName: "mayor", Mode: "always"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "gc-1", SessionName: "mayor", Template: "mayor", State: "asleep"},
			{ID: "gc-2", SessionName: "mayor", Template: "mayor", State: "asleep", NamedIdentity: "mayor"},
		},
		Now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
	}
	got := computeAwakeSetKeyed(in, awakeKeyBeadID)
	want := map[string]AwakeDecision{"gc-1": {}, "gc-2": {ShouldWake: true, Reason: "named-always"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bead-ID keying:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestComputeAwakeSetKeyedByIDUnresolvedNamedDesiresEveryRowWithItsName(t *testing.T) {
	// No row claims the identity, so the desire is made under the name, as
	// legacy does, and lands on every row carrying it.
	in := AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "t"}},
		NamedSessions: []AwakeNamedSession{{Identity: "n1", Template: "t", RuntimeName: "n1", Mode: "always"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "gc-1", SessionName: "n1", Template: "t", State: "asleep"},
			{ID: "gc-2", SessionName: "other", Template: "t", State: "asleep"},
			{ID: "gc-3", SessionName: "n1", Template: "t", State: "asleep"},
		},
		Now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
	}
	got := computeAwakeSetKeyed(in, awakeKeyBeadID)
	want := map[string]AwakeDecision{
		"gc-1": {ShouldWake: true, Reason: "named-always"},
		"gc-2": {},
		"gc-3": {ShouldWake: true, Reason: "named-always"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bead-ID keying:\n got=%+v\nwant=%+v", got, want)
	}
	if byName := ComputeAwakeSet(in); byName["n1"] != want["gc-1"] {
		t.Fatalf("session-name keying for n1 = %+v, want %+v", byName["n1"], want["gc-1"])
	}
}

// planOnlyStore counts every call. With t set it fails the test on a write;
// without, it passes writes through (the legacy controls).
type planOnlyStore struct {
	beads.Store
	t     *testing.T
	calls int
}

func (s *planOnlyStore) write(op string) error {
	s.calls++
	if s.t == nil {
		return nil
	}
	s.t.Helper()
	s.t.Errorf("store %s under planOnly", op)
	return errPlanOnlyEffect
}

func (s *planOnlyStore) Create(b beads.Bead) (beads.Bead, error) {
	if err := s.write("Create"); err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Create(b)
}

func (s *planOnlyStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.write("Update"); err != nil {
		return err
	}
	return s.Store.Update(id, opts)
}

func (s *planOnlyStore) Close(id string) error {
	if err := s.write("Close"); err != nil {
		return err
	}
	return s.Store.Close(id)
}

func (s *planOnlyStore) Reopen(id string) error {
	if err := s.write("Reopen"); err != nil {
		return err
	}
	return s.Store.Reopen(id)
}

func (s *planOnlyStore) SetMetadata(id, key, value string) error {
	if err := s.write("SetMetadata"); err != nil {
		return err
	}
	return s.Store.SetMetadata(id, key, value)
}

func (s *planOnlyStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.write("SetMetadataBatch"); err != nil {
		return err
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *planOnlyStore) SetLocalString(id, key, value string) error {
	if err := s.write("SetLocalString"); err != nil {
		return err
	}
	return s.Store.SetLocalString(id, key, value)
}

func (s *planOnlyStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := s.write("DepAdd"); err != nil {
		return err
	}
	return s.Store.DepAdd(issueID, dependsOnID, depType)
}

func (s *planOnlyStore) DepRemove(issueID, dependsOnID string) error {
	if err := s.write("DepRemove"); err != nil {
		return err
	}
	return s.Store.DepRemove(issueID, dependsOnID)
}

func (s *planOnlyStore) Delete(id string) error {
	if err := s.write("Delete"); err != nil {
		return err
	}
	return s.Store.Delete(id)
}

func (s *planOnlyStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if err := s.write("CloseAll"); err != nil {
		return 0, err
	}
	return s.Store.CloseAll(ids, metadata)
}

func (s *planOnlyStore) Tx(msg string, fn func(beads.Tx) error) error {
	if err := s.write("Tx"); err != nil {
		return err
	}
	return s.Store.Tx(msg, fn)
}

func (s *planOnlyStore) Get(id string) (beads.Bead, error) {
	s.calls++
	return s.Store.Get(id)
}

func (s *planOnlyStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.calls++
	return s.Store.List(q)
}

// planOnlyHarness is build params whose every effect channel is observable:
// store calls, fsys calls, the city directory, provider resolution (lookPath)
// and worktree verification.
type planOnlyHarness struct {
	bp       *agentBuildParams
	store    *planOnlyStore
	fs       *fsys.Fake
	lookPath int
}

func newPlanOnlyHarness(t *testing.T, cfg *config.City, rows []beads.Bead) *planOnlyHarness {
	t.Helper()
	h := &planOnlyHarness{
		store: &planOnlyStore{Store: beads.NewMemStoreFrom(1, rows, nil), t: t},
		fs:    fsys.NewFake(),
	}
	h.bp = &agentBuildParams{
		city:                             cfg,
		cityName:                         "city",
		cityPath:                         t.TempDir(),
		workspace:                        &cfg.Workspace,
		agents:                           cfg.Agents,
		providers:                        cfg.Providers,
		fs:                               h.fs,
		sp:                               runtime.NewFake(),
		beadStore:                        h.store,
		sessionBeads:                     newSessionBeadSnapshot(rows),
		sessionSnapshotComplete:          true,
		sessionSnapshotCompletenessKnown: true,
		realizeProbe:                     &poolRealizeProbe{},
		beadNames:                        map[string]string{},
		stderr:                           io.Discard,
		planOnly:                         true,
		lookPath: func(name string) (string, error) {
			h.lookPath++
			return "/usr/bin/" + name, nil
		},
	}
	return h
}

// newLegacyHarness is the same harness with planOnly off, for controls that
// prove a fixture reaches the effect planOnly suppresses.
func newLegacyHarness(t *testing.T, cfg *config.City, rows []beads.Bead) *planOnlyHarness {
	t.Helper()
	h := newPlanOnlyHarness(t, cfg, rows)
	h.bp.planOnly = false
	h.store.t = nil
	return h
}

// assertNoEffects fails unless nothing reached a store, the filesystem, the
// city directory, provider resolution or worktree verification.
func (h *planOnlyHarness) assertNoEffects(t *testing.T) {
	t.Helper()
	if h.store.calls != 0 {
		t.Errorf("store calls = %d, want 0", h.store.calls)
	}
	if len(h.fs.Calls) != 0 {
		t.Errorf("fsys calls = %+v, want none", h.fs.Calls)
	}
	if entries := dirEntries(t, h.bp.cityPath); len(entries) != 0 {
		t.Errorf("city dir gained %v, want nothing", entries)
	}
	if h.lookPath != 0 {
		t.Errorf("provider resolutions (lookPath calls) = %d, want 0", h.lookPath)
	}
	if n := h.bp.realizeProbe.worktreeVerifies.Load(); n != 0 {
		t.Errorf("worktree.Verify calls = %d, want 0", n)
	}
}

func dirEntries(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root {
			out = append(out, strings.TrimPrefix(path, root))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func singletonPhantomRow() beads.Bead {
	return wpoolSessionBead("gm-1", "open", "mayor-1", []string{"agent:mayor-1"}, map[string]string{
		"template": "mayor", "agent_name": "mayor-1", "alias": "mayor-1",
		"pool_slot": "1", "session_name": "s-mayor-1", "pool_managed": "true", "state": "active",
	})
}

func TestSelectOrPlanPoolSessionBeadPlanOnlyWritesNothing(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}}}
	row := singletonPhantomRow()

	// Control: legacy collapses the phantom identity with a write under the
	// alias lock, so the fixture reaches the normalization write.
	legacy := newLegacyHarness(t, cfg, []beads.Bead{row})
	if _, _, _, err := selectOrPlanPoolSessionBead(legacy.bp, &cfg.Agents[0], "mayor", nil, SessionRequest{}, time.Time{}, map[string]bool{}, map[int]bool{}); err != nil {
		t.Fatalf("legacy select: %v", err)
	}
	if legacy.store.calls == 0 {
		t.Fatal("control: legacy selection made no store call; the fixture does not reach the normalization write")
	}

	h := newPlanOnlyHarness(t, cfg, []beads.Bead{row})
	info, slot, plan, err := selectOrPlanPoolSessionBead(h.bp, &cfg.Agents[0], "mayor", nil, SessionRequest{}, time.Time{}, map[string]bool{}, map[int]bool{})
	if err != nil || plan != nil {
		t.Fatalf("planOnly select = (plan %+v, err %v), want reuse", plan, err)
	}
	if want := sessiontest.SeedBead(t, row); !reflect.DeepEqual(info, want) || slot != 0 {
		t.Fatalf("planOnly select = (%+v, slot %d), want the census row unchanged at slot 0", info, slot)
	}
	h.assertNoEffects(t)
}

func TestPoolTriggerMetadataPlanOnlySkipsWorktreeVerify(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(3)}}}
	cityRoot := t.TempDir()
	request := SessionRequest{
		WorkBeadID:   "w-1",
		WorkStoreRef: "city",
		WorktreeSpec: &worktree.Spec{
			RepoDir: filepath.Join(cityRoot, "repo"), Path: filepath.Join(cityRoot, "wt"), Branch: "b",
			Root: cityRoot, BeadID: "w-1", StoreRef: "city:city",
		},
	}

	legacy := newLegacyHarness(t, cfg, nil)
	if _, err := poolTriggerMetadata(legacy.bp, &cfg.Agents[0], "worker-1", request); err == nil || legacy.bp.realizeProbe.worktreeVerifies.Load() != 1 {
		t.Fatalf("control: legacy verifies = %d (err %v), want one failed worktree.Verify", legacy.bp.realizeProbe.worktreeVerifies.Load(), err)
	}

	h := newPlanOnlyHarness(t, cfg, nil)
	metadata, err := poolTriggerMetadata(h.bp, &cfg.Agents[0], "worker-1", request)
	if err != nil {
		t.Fatalf("planOnly poolTriggerMetadata: %v", err)
	}
	if metadata["gc.trigger_bead_id"] != "w-1" || metadata["gc.work_dir"] != "" {
		t.Fatalf("planOnly metadata = %v, want the trigger without a verified work dir", metadata)
	}
	h.assertNoEffects(t)

	// The evidence checks that need no filesystem still fail closed, before
	// the plan-only skip.
	worktreeErr := request
	worktreeErr.WorktreeError = "two owners"
	otherBead := request
	otherBead.WorkBeadID = "w-2"
	otherStore := request
	otherStore.WorkStoreRef = "r1"
	for name, bad := range map[string]SessionRequest{"worktree error": worktreeErr, "bead mismatch": otherBead, "store mismatch": otherStore} {
		if _, err := poolTriggerMetadata(h.bp, &cfg.Agents[0], "worker-1", bad); !errors.Is(err, errPoolTriggerWorktreeEvidence) {
			t.Errorf("planOnly poolTriggerMetadata with %s = %v, want errPoolTriggerWorktreeEvidence", name, err)
		}
	}
	h.assertNoEffects(t)
}

func TestSelectOrPlanPoolSessionBeadPlanOnlyCarriesWorktreeSpec(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(3)}}}
	cityRoot := t.TempDir()
	spec := worktree.Spec{
		RepoDir: filepath.Join(cityRoot, "repo"), Path: filepath.Join(cityRoot, "wt"), Branch: "b",
		Root: cityRoot, BeadID: "w-1", StoreRef: "city:city",
	}
	request := SessionRequest{WorkBeadID: "w-1", WorkStoreRef: "city", WorktreeSpec: &spec}

	h := newPlanOnlyHarness(t, cfg, nil)
	_, _, plan, err := selectOrPlanPoolSessionBead(h.bp, &cfg.Agents[0], "worker", nil, request, time.Time{}, map[string]bool{}, map[int]bool{})
	if err != nil || plan == nil {
		t.Fatalf("planOnly select = (plan %+v, err %v), want a create plan", plan, err)
	}
	if plan.worktreeSpec == nil || !reflect.DeepEqual(*plan.worktreeSpec, spec) || plan.worktreeSpec == request.WorktreeSpec {
		t.Fatalf("plan worktreeSpec = %+v, want a copy of %+v", plan.worktreeSpec, spec)
	}
	if plan.metadata["gc.trigger_bead_id"] != "w-1" || plan.metadata["gc.work_dir"] != "" {
		t.Fatalf("plan metadata = %v, want the trigger without an unverified work dir", plan.metadata)
	}
	h.assertNoEffects(t)

	// A request whose work dir needs no verification carries no spec.
	request.WorktreeSpec = nil
	_, _, plan, err = selectOrPlanPoolSessionBead(h.bp, &cfg.Agents[0], "worker", nil, request, time.Time{}, map[string]bool{}, map[int]bool{})
	if err != nil || plan == nil || plan.worktreeSpec != nil {
		t.Fatalf("planOnly select without a spec = (plan %+v, err %v), want a plan without a worktree spec", plan, err)
	}
}

func TestDependencyFloorPlanOnlyReturnsPlanNoCreate(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "db", MaxActiveSessions: intPtr(3)}}}
	h := newPlanOnlyHarness(t, cfg, nil)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	info, slot, plan, err := selectOrPlanDependencyPoolSessionBead(h.bp, &cfg.Agents[0], "db", now)
	if err != nil {
		t.Fatalf("planOnly dependency floor: %v", err)
	}
	want := &poolSessionCreatePlan{qualifiedInstance: "db-1", slot: 1, poolSlot: 1}
	if !reflect.DeepEqual(plan, want) || slot != 1 || info.ID != "" {
		t.Fatalf("planOnly dependency floor = (%+v, slot %d, plan %+v), want plan %+v", info, slot, plan, want)
	}
	h.assertNoEffects(t)

	// The legacy wrapper creates what the plan describes.
	store := beads.NewMemStore()
	legacy := &agentBuildParams{
		city: cfg, cityPath: t.TempDir(), agents: cfg.Agents, beadStore: store,
		sessionBeads: newSessionBeadSnapshot(nil), sessionSnapshotComplete: true, sessionSnapshotCompletenessKnown: true,
	}
	created, createdSlot, err := selectOrCreateDependencyPoolSessionBeadWithSlot(legacy, &cfg.Agents[0], "db")
	if err != nil || created.ID == "" || createdSlot != 1 || created.AgentName != "db-1" {
		t.Fatalf("legacy dependency floor = (%+v, slot %d, %v), want a created db-1 row at slot 1", created, createdSlot, err)
	}
}

func TestPlanOnlyBuildParamsRefuseEffects(t *testing.T) {
	// "hookprov" makes resolution call lookPath; install_agent_hooks makes
	// it and installAgentSideEffects write hook files through bp.fs.
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{"hookprov": {Command: "hookprov"}},
		Agents: []config.Agent{{
			Name: "worker", Provider: "hookprov", MaxActiveSessions: intPtr(3),
			InstallAgentHooks: []string{"gemini"},
		}},
	}
	agent := &cfg.Agents[0]
	row := wpoolSessionBead("gw-1", "open", "worker-1", nil, map[string]string{
		"template": "worker", "agent_name": "worker-1", "session_name": "s-worker-1",
		"pool_managed": "true", "pool_slot": "1", "state": "active",
	})
	tp := TemplateParams{WorkDir: "/work/worker-1", SessionName: "s-worker-1", IsACP: true}

	// Control: legacy resolution resolves the provider and installs hooks.
	legacy := newLegacyHarness(t, cfg, nil)
	installAgentSideEffects(legacy.bp, agent, tp, io.Discard)
	if len(legacy.fs.Calls) == 0 {
		t.Fatal("control: legacy installAgentSideEffects wrote no hook file")
	}
	if _, err := resolveTemplatePrepared(legacy.bp, agent, "worker-1", nil); err != nil || legacy.lookPath == 0 {
		t.Fatalf("control: legacy resolveTemplatePrepared lookPath calls = %d (err %v), want a provider resolution", legacy.lookPath, err)
	}

	h := newPlanOnlyHarness(t, cfg, []beads.Bead{row})
	if _, err := resolveTemplatePrepared(h.bp, agent, "worker-1", nil); !errors.Is(err, errPlanOnlyEffect) {
		t.Errorf("resolveTemplatePrepared = %v, want errPlanOnlyEffect", err)
	}
	if _, err := resolveTemplateForSessionBeadInfo(h.bp, agent, "worker-1", nil, sessiontest.SeedBead(t, row)); !errors.Is(err, errPlanOnlyEffect) {
		t.Errorf("resolveTemplateForSessionBeadInfo = %v, want errPlanOnlyEffect", err)
	}
	installAgentSideEffects(h.bp, agent, tp, io.Discard)
	if _, err := createPoolSessionBeadWithGuardedAlias(h.bp, agent, "worker", "worker-2", 2, nil); !errors.Is(err, errPlanOnlyEffect) {
		t.Errorf("createPoolSessionBeadWithGuardedAlias = %v, want errPlanOnlyEffect", err)
	}
	if _, err := bindPoolSessionTriggerBead(h.bp, agent, "worker-1", sessiontest.SeedBead(t, row), SessionRequest{WorkBeadID: "w-9"}); !errors.Is(err, errPlanOnlyEffect) {
		t.Errorf("bindPoolSessionTriggerBead = %v, want errPlanOnlyEffect", err)
	}
	h.assertNoEffects(t)
}

func TestEndpointKeyForAgentMatchesResolvedEndpointKey(t *testing.T) {
	providers := map[string]config.ProviderSpec{"p1": {Command: "p1"}, "wsp": {Command: "wsp"}}
	lookPath := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	cases := []struct {
		name  string
		ws    config.Workspace
		agent config.Agent
		info  session.Info
		want  endpointKey
	}{
		{name: "upstream", agent: config.Agent{Name: "a", Provider: "p1", Upstream: " broker "}, want: "upstream:broker"},
		{name: "upstream with start_command", agent: config.Agent{Name: "a", StartCommand: "run", Upstream: "broker"}, want: "upstream:broker"},
		{name: "agent provider", agent: config.Agent{Name: "a", Provider: "p1"}, info: session.Info{Provider: "row"}, want: "provider:p1"},
		{name: "workspace provider", ws: config.Workspace{Provider: "wsp"}, agent: config.Agent{Name: "a"}, want: "provider:wsp"},
		{name: "agent provider over workspace", ws: config.Workspace{Provider: "wsp"}, agent: config.Agent{Name: "a", Provider: "p1"}, want: "provider:p1"},
		{name: "workspace start_command keeps provider", ws: config.Workspace{Provider: "wsp", StartCommand: "run"}, agent: config.Agent{Name: "a"}, want: "provider:wsp"},
		{name: "start_command falls to row provider", agent: config.Agent{Name: "a", Provider: "p1", StartCommand: "run"}, info: session.Info{Provider: "row"}, want: "provider:row"},
		{name: "workspace start_command without provider", ws: config.Workspace{StartCommand: "run"}, agent: config.Agent{Name: "a"}, info: session.Info{Provider: "row"}, want: "provider:row"},
		{name: "row provider only", agent: config.Agent{Name: "a"}, info: session.Info{Provider: " row "}, want: "provider:row"},
		{name: "unguarded", agent: config.Agent{Name: "a"}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{Workspace: tc.ws, Providers: providers, Agents: []config.Agent{tc.agent}}
			agent := &cfg.Agents[0]
			resolved, _ := config.ResolveProvider(agent, &cfg.Workspace, cfg.Providers, lookPath)
			resolvedKey := resolvedEndpointKey(TemplateParams{Upstream: agent.Upstream, ResolvedProvider: resolved}, tc.info)
			got := endpointKeyForAgent(cfg, agent, tc.info)
			if got != resolvedKey || got != tc.want {
				t.Fatalf("endpointKeyForAgent = %q, resolvedEndpointKey = %q, want %q", got, resolvedKey, tc.want)
			}
		})
	}
}

// demandFixture is a random city for the demand-pass differentials.
type demandFixture struct {
	cityPath          string
	cfg               *config.City
	store             beads.Store
	rigStores         map[string]beads.Store
	suspendedRigPaths map[string]bool
	sessions          []session.Info
}

func randDemandFixture(t *testing.T, r *rand.Rand) demandFixture {
	t.Helper()
	cityPath := t.TempDir()
	coin := func() bool { return r.IntN(3) == 0 }
	f := demandFixture{cityPath: cityPath, cfg: &config.City{}, rigStores: map[string]beads.Store{}, suspendedRigPaths: map[string]bool{}}
	if r.IntN(6) != 0 {
		f.store = beads.NewMemStore()
	}
	for _, rig := range []string{"r1", "r2"} {
		path := filepath.Join(cityPath, rig)
		f.cfg.Rigs = append(f.cfg.Rigs, config.Rig{Name: rig, Path: path})
		if r.IntN(4) != 0 {
			f.rigStores[rig] = beads.NewMemStore()
		}
		if coin() {
			f.suspendedRigPaths[filepath.Clean(path)] = true
		}
	}
	for i := r.IntN(7); i >= 0; i-- {
		a := config.Agent{
			Name:              "a" + strconv.Itoa(i),
			Dir:               []string{"", "", "r1", "r2"}[r.IntN(4)],
			Suspended:         r.IntN(6) == 0,
			MinActiveSessions: intPtr(r.IntN(2)),
		}
		if i == 0 && coin() {
			a.Name = config.ControlDispatcherAgentName
		}
		if coin() {
			a.ScaleCheck = "echo 3"
		}
		switch r.IntN(4) {
		case 0:
			a.MaxActiveSessions = intPtr(0)
		case 1:
			a.MaxActiveSessions = intPtr(1)
		case 2:
			a.MaxActiveSessions = intPtr(3)
		}
		f.cfg.Agents = append(f.cfg.Agents, a)
		if coin() {
			f.cfg.NamedSessions = append(f.cfg.NamedSessions, config.NamedSession{
				Template: a.Name, Dir: a.Dir, Mode: []string{"", "always", "on_demand"}[r.IntN(3)],
			})
		}
		for j := r.IntN(3); j > 0; j-- {
			f.sessions = append(f.sessions, session.Info{
				ID:            fmt.Sprintf("s-%d-%d", i, j),
				Template:      a.QualifiedName(),
				PoolManaged:   coin(),
				MetadataState: []string{"active", "asleep", "creating", "closed"}[r.IntN(4)],
			})
		}
	}
	return f
}

func TestBuildDemandTargetsGoldenAgainstInlineLoop(t *testing.T) {
	for seed := uint64(0); seed < poolPiecesSeeds; seed++ {
		f := randDemandFixture(t, rand.New(rand.NewPCG(seed, 3)))
		var gotErr, wantErr bytes.Buffer
		got := buildDemandTargets("city", f.cityPath, f.cfg, f.store, f.rigStores, f.suspendedRigPaths, f.sessions, controllerQueryRuntimeEnv, &gotErr)
		want := buildDemandTargetsPreRefactor("city", f.cityPath, f.cfg, f.store, f.rigStores, f.suspendedRigPaths, f.sessions, &wantErr)
		// The frozen loop predates the custom scale_check fan-out and has no
		// probes to compare; they are pinned in pool_scale_check_fanout_test.go
		// and, against the lane's pools, by TestScaleCheckLaneWorkMatchesBuildDemandTargets.
		for i := range got.pendingPools {
			got.pendingPools[i].probes = nil
		}
		if !reflect.DeepEqual(got, want) || gotErr.String() != wantErr.String() {
			t.Fatalf("seed %d: buildDemandTargets differs from the inline loop:\n got=%+v\nwant=%+v\nstderr got=%q want=%q", seed, got, want, gotErr.String(), wantErr.String())
		}
	}
}

func TestComputeNamedSessionDemandMatchesInlineBlock(t *testing.T) {
	for seed := uint64(0); seed < poolPiecesSeeds; seed++ {
		r := rand.New(rand.NewPCG(seed, 4))
		f := randDemandFixture(t, r)
		var identities, templates []string
		for i := range f.cfg.NamedSessions {
			identities = append(identities, f.cfg.NamedSessions[i].QualifiedName())
		}
		for i := range f.cfg.Agents {
			templates = append(templates, f.cfg.Agents[i].QualifiedName())
		}
		assignees := append([]string{"", "other"}, identities...)
		for _, id := range identities {
			assignees = append(assignees, config.NamedSessionRuntimeName("city", f.cfg.Workspace, id))
		}
		var work []beads.Bead
		var refs []string
		ready := map[storeScopedBeadKey]bool{}
		for i := r.IntN(6); i > 0; i-- {
			b := beads.Bead{ID: "w" + strconv.Itoa(i), Assignee: assignees[r.IntN(len(assignees))], Status: []string{"open", "in_progress", "closed"}[r.IntN(3)]}
			ref := []string{"", "city", "r1", "r2"}[r.IntN(4)]
			work, refs = append(work, b), append(refs, ref)
			if r.IntN(2) == 0 {
				ready[storeScopedBeadKey{StoreRef: ref, ID: b.ID}] = true
			}
		}
		if r.IntN(3) == 0 && len(refs) > 0 {
			refs = refs[:len(refs)-1] // a short ref slice, as legacy tolerates
		}
		defaults, counts := map[string]bool{}, map[string]int{}
		for _, id := range identities {
			if r.IntN(3) == 0 {
				defaults[id] = true
			}
		}
		for _, tmpl := range templates {
			counts[tmpl] = r.IntN(3)
		}
		var gotErr, wantErr bytes.Buffer
		got := computeNamedSessionDemand("city", f.cityPath, f.cfg, f.store, f.suspendedRigPaths, defaults, work, refs, ready, counts, &gotErr)
		want := computeNamedSessionDemandPreRefactor("city", f.cityPath, f.cfg, f.store, f.suspendedRigPaths, defaults, work, refs, ready, counts, &wantErr)
		// The block logs while ranging over a map, so compare log lines as a set.
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(sortedLines(gotErr.String()), sortedLines(wantErr.String())) {
			t.Fatalf("seed %d: computeNamedSessionDemand differs from the inline block:\n got=%+v\nwant=%+v\nstderr got=%q want=%q", seed, got, want, gotErr.String(), wantErr.String())
		}
	}
}

// overlayFixture is a random open-session census plus the desired state the
// config and pool passes left for the overlay.
type overlayFixture struct {
	cfg               *config.City
	rows              []beads.Bead
	desired           map[string]TemplateParams
	suspendedRigPaths map[string]bool
	poolPartial       map[string]bool
	namedPartial      map[string]bool
}

func randOverlayFixture(r *rand.Rand, cityPath string) overlayFixture {
	pick := func(xs ...string) string { return xs[r.IntN(len(xs))] }
	coin := func() bool { return r.IntN(3) == 0 }
	rigPath := filepath.Join(cityPath, "r1")
	f := overlayFixture{
		cfg: &config.City{
			Rigs: []config.Rig{{Name: "r1", Path: rigPath}},
			Agents: []config.Agent{
				{Name: "claude", StartCommand: "echo", MaxActiveSessions: intPtr(5)},
				{Name: "mayor", StartCommand: "echo", MaxActiveSessions: intPtr(1)},
				{Name: "helper", StartCommand: "echo"},
				{Name: "worker", Dir: "r1", StartCommand: "echo", MaxActiveSessions: intPtr(2)},
			},
			NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "always"}},
		},
		desired:           map[string]TemplateParams{},
		suspendedRigPaths: map[string]bool{},
		poolPartial:       map[string]bool{},
		namedPartial:      map[string]bool{},
	}
	if coin() {
		f.suspendedRigPaths[filepath.Clean(rigPath)] = true
	}
	templates := []string{"claude", "mayor", "helper", "r1/worker", "unknown", ""}
	for _, tmpl := range templates[:4] {
		if coin() {
			f.poolPartial[tmpl] = true
		}
		if coin() {
			f.namedPartial[tmpl] = true
		}
	}
	names := []string{"s-0", "s-1", "s-2", "mayor", "claude-1", ""}
	now := time.Now()
	for i := r.IntN(10); i > 0; i-- {
		meta := map[string]string{"template": pick(templates...), "session_name": pick(names...)}
		set := func(k string, vals ...string) {
			if v := pick(vals...); v != "" {
				meta[k] = v
			}
		}
		set("state", "", "active", "creating", "start-pending", "asleep", "drained", "failed-create", "stopped")
		set("pool_managed", "", "true")
		set("pool_slot", "", "1", "2")
		set("pending_create_claim", "", "true")
		set("manual_session", "", "true")
		set("session_origin", "", "manual", "ephemeral", "named")
		set("configured_named_identity", "", "mayor")
		set("configured_named_session", "", "true")
		set("dependency_only", "", "true")
		set("agent_name", "", "mayor", "mayor-1", "claude-2", "r1/worker-1")
		set("alias", "", "mayor", "claude-1")
		set("sleep_reason", "", "drained")
		created := now.Add(-time.Hour)
		if coin() {
			created = now.Add(time.Hour)
		}
		f.rows = append(f.rows, beads.Bead{
			ID: "gc-" + strconv.Itoa(i), Type: session.BeadType, Status: pick("open", "open", "open", "closed"),
			Labels: []string{session.LabelSession}, Metadata: meta, CreatedAt: created,
		})
	}
	for i := r.IntN(4); i > 0; i-- {
		f.desired[pick(names...)] = TemplateParams{
			TemplateName:            pick(templates[:4]...),
			InstanceName:            pick("", "mayor", "claude-1"),
			Alias:                   pick("", "mayor"),
			DependencyOnly:          coin(),
			ConfiguredNamedIdentity: pick("", "", "mayor"),
		}
	}
	return f
}

func runOverlay(t *testing.T, f overlayFixture, cityPath string, discover func(*agentBuildParams, *config.City, map[string]TemplateParams, map[string]bool, map[string]bool, map[string]bool, io.Writer) map[string]bool) ([]string, map[string]bool, string) {
	t.Helper()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("city", cityPath, f.cfg, runtime.NewFake(), time.Time{}, nil, io.Discard)
	bp.sessionBeads = newSessionBeadSnapshot(f.rows)
	bp.beadNames = map[string]string{}
	desired := copyDesired(f.desired)
	roots := discover(bp, f.cfg, desired, f.suspendedRigPaths, f.poolPartial, f.namedPartial, &stderr)
	return sortedTemplateParamKeys(desired), roots, stderr.String()
}

func copyDesired(m map[string]TemplateParams) map[string]TemplateParams {
	out := make(map[string]TemplateParams, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedLines(s string) []string {
	lines := strings.Split(s, "\n")
	sort.Strings(lines)
	return lines
}

func sortedTemplateParamKeys(m map[string]TemplateParams) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestDiscoverSessionBeadsMatchesPreRefactor(t *testing.T) {
	cityPath := t.TempDir()
	for seed := uint64(0); seed < poolPiecesSeeds; seed++ {
		f := randOverlayFixture(rand.New(rand.NewPCG(seed, 5)), cityPath)
		gotKeys, gotRoots, gotErr := runOverlay(t, f, cityPath, discoverSessionBeadsWithRoots)
		wantKeys, wantRoots, wantErr := runOverlay(t, f, cityPath, discoverSessionBeadsWithRootsPreRefactor)
		if !reflect.DeepEqual(gotKeys, wantKeys) || !reflect.DeepEqual(gotRoots, wantRoots) || gotErr != wantErr {
			t.Fatalf("seed %d: overlay differs from pre-refactor:\n desired got=%v want=%v\n roots got=%v want=%v\n stderr got=%q want=%q", seed, gotKeys, wantKeys, gotRoots, wantRoots, gotErr, wantErr)
		}
	}
}
