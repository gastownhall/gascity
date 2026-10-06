package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// Create-effect tests run effects against MemStores with a recording
// settler. Each test mints the plan's token the way the planner does at
// submit, submits the plan, waits for the executor to drain (wg), then reads
// the effect's one settlement and the stores. The effects read time.Now, so
// tests under synctest move it with advance.

// createHarness is one executor with a recorded settle.
type createHarness struct {
	x *createEffects

	mu      sync.Mutex
	tokens  map[string]string
	settled []createSettlement
}

func newCreateHarness(t *testing.T, edit func(*createEffectHost)) *createHarness {
	t.Helper()
	h := &createHarness{tokens: make(map[string]string)}
	host := createEffectHost{
		cityPath: t.TempDir(),
		cityName: "test-city",
		settle: func(s createSettlement) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.settled = append(h.settled, s)
		},
		withLocks: func(_ string, _ []string, fn func() error) error { return fn() },
		verify: func(worktree.Spec) (worktree.Report, error) {
			t.Error("worktree.Verify called for a plan without a worktree spec")
			return worktree.Report{}, errors.New("unexpected verify")
		},
	}
	if edit != nil {
		edit(&host)
	}
	x, err := newCreateEffects(host)
	if err != nil {
		t.Fatalf("newCreateEffects: %v", err)
	}
	h.x = x
	return h
}

// createHarnessRev is the ConfigRev every harness plan is decided under.
const createHarnessRev = "rev-1"

// reserve mints create entry id's instance token, as the planner does at
// submit (S-8), and returns it. submit stamps it on the entry's plans.
func (h *createHarness) reserve(t *testing.T, id string) string {
	t.Helper()
	token := session.NewInstanceToken()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tokens[id] = token
	return token
}

// submit stamps each plan without a token with its entry's minted token and
// createHarnessRev, then submits them.
func (h *createHarness) submit(pass *createPass, plans ...createPlan) bool {
	h.mu.Lock()
	stamped := make([]createPlan, len(plans))
	for i, p := range plans {
		if p.Token == "" {
			p.Token, p.ConfigRev = h.tokens[p.EntryID], createHarnessRev
		}
		stamped[i] = p
	}
	h.mu.Unlock()
	return h.x.submit(pass, stamped...)
}

// runAll submits plans and waits until every effect returned.
func (h *createHarness) runAll(t *testing.T, pass *createPass, plans ...createPlan) {
	t.Helper()
	if !h.submit(pass, plans...) {
		t.Fatal("submit refused")
	}
	h.x.wg.Wait()
}

func (h *createHarness) settlements() []createSettlement {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]createSettlement(nil), h.settled...)
}

// settlementOf returns create entry id's settlement; an effect settles
// exactly once (C5.6).
func (h *createHarness) settlementOf(t *testing.T, id string) createSettlement {
	t.Helper()
	var out []createSettlement
	for _, s := range h.settlements() {
		if s.EntryID == id {
			out = append(out, s)
		}
	}
	if len(out) != 1 {
		t.Fatalf("settlements of %s = %+v, want exactly one", id, out)
	}
	return out[0]
}

// entry returns create entry c1's settlement, the entry of every
// single-plan test.
func (h *createHarness) entry(t *testing.T) createSettlement {
	t.Helper()
	return h.settlementOf(t, "c1")
}

// assertNoRefusal fails if any settlement refuses its identity.
func (h *createHarness) assertNoRefusal(t *testing.T) {
	t.Helper()
	for _, s := range h.settlements() {
		if s.Stage != "" {
			t.Fatalf("settlement %+v refuses its identity, want no refusal", s)
		}
	}
}

// assertRefused checks that plan's entry settled refusing its identity with
// cause under createHarnessRev.
func (h *createHarness) assertRefused(t *testing.T, plan createPlan, cause string) {
	t.Helper()
	s := h.settlementOf(t, plan.EntryID)
	if s.Stage != cause || s.Identity != plan.identity().key() || s.ConfigRev != createHarnessRev {
		t.Fatalf("settlement %+v, want identity %q refused with cause %q under %q", s, plan.identity().key(), cause, createHarnessRev)
	}
}

// refusesWork reports whether entry c1's settlement refuses spec's evidence.
func (h *createHarness) refusesWork(t *testing.T, spec worktree.Spec) bool {
	t.Helper()
	w := h.entry(t).Work
	return w != nil && w.Refused && w.BeadID == spec.BeadID && w.Fingerprint == specFingerprint(spec)
}

// assertFailedNoWrite checks entry c1 settled a failure without a row, which
// clears from the in-flight map at its settlement.
func assertFailedNoWrite(t *testing.T, h *createHarness) {
	t.Helper()
	s := h.entry(t)
	if s.Landed || s.Ambiguous || s.Err == nil {
		t.Fatalf("settlement %+v, want a failure without a row", s)
	}
	if m := settledCreateEntry(t, s); len(m.view().Entries) != 0 {
		t.Fatalf("in-flight entries after the settlement = %+v, want none", m.view().Entries)
	}
}

// settledCreateEntry is an in-flight map holding s's create entry, recorded
// at submit with the plan's token and settled by s.
func settledCreateEntry(t *testing.T, s createSettlement) *inflightMap {
	t.Helper()
	m := newInflightMap()
	if !m.add(inflightEntry{Kind: inflightCreate, Token: s.Token, Identity: s.Identity, Leg: "sessions"}) {
		t.Fatal("add refused")
	}
	m.settle(s.settlement())
	return m
}

func sessionRows(t *testing.T, store beads.Store) []session.Info {
	t.Helper()
	infos, err := sessionFrontDoor(store).ListAll(session.ListAllOptions{})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	return infos
}

// workerCity is a transient-slot pool of max.
func workerCity(maxSessions int) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(maxSessions)}},
	}
}

func workerPlan(cfg *config.City, entryID string, slot int) createPlan {
	_, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(&cfg.Agents[0], slot)
	return createPlan{EntryID: entryID, Template: cfg.Agents[0].QualifiedName(), QualifiedInstance: qualifiedInstance, Slot: poolSlot}
}

// aliasQueryFailStore fails the first alias-keyed query, which is the
// locked alias reservation check: the re-census before it and the identifier
// checks after it answer, so only the alias check cannot.
type aliasQueryFailStore struct {
	beads.Store
	failed bool
}

func (s *aliasQueryFailStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if _, ok := q.Metadata["alias"]; ok && !s.failed {
		s.failed = true
		return nil, errors.New("alias index unavailable")
	}
	return s.Store.List(q)
}

// probePanicProvider panics on every method: an effect must not touch it
// beyond transport capability checks, which only type-assert it.
type probePanicProvider struct{ runtime.Provider }

func (probePanicProvider) IsRunning(name string) bool {
	panic("create effect probed runtime " + name)
}

// Kills: the fenced live re-census dropped (POOL-051, R8). A same-identity
// holder written on a rig leg after planning is visible only to the re-census
// the effect runs under the identifier locks.
func TestCreateEffect_LockedReservationRejectsLateForeignHolder(t *testing.T) {
	primary, foreign := beads.NewMemStore(), beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig", Path: t.TempDir()}},
		Agents:    []config.Agent{{Name: "worker", Dir: "rig", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
	}
	plan := workerPlan(cfg, "c1", 1)
	runtimeName := poolRuntimeSessionName(cfg, plan.QualifiedInstance, plan.Template, true)
	h := newCreateHarness(t, func(host *createEffectHost) {
		host.withLocks = func(_ string, identifiers []string, fn func() error) error {
			if !containsString(identifiers, runtimeName) {
				t.Errorf("identifier locks = %v, want the exact runtime name %q", identifiers, runtimeName)
			}
			seedGuardedPoolSessionHolder(t, foreign, "late holder", "rig/late-writer", "", runtimeName)
			return fn()
		}
	})
	h.reserve(t, "c1")
	pass := &createPass{cfg: cfg, store: primary, rigStores: map[string]beads.Store{"rig": foreign}}

	h.runAll(t, pass, plan)

	assertFailedNoWrite(t, h)
	if rows := sessionRows(t, primary); len(rows) != 0 {
		t.Fatalf("primary rows = %+v, want none beside a late foreign holder", rows)
	}
}

// Kills: a second generation minted beside an open unconfirmed create of the
// same slot identity (ga-vcjr9): two live boxes for one slot. The holder is a
// failed create whose runtime teardown is unconfirmed, with a bead-scoped
// name, so only the identity lease can see it.
func TestCreateEffect_IdentityLeaseRefusesSecondGeneration(t *testing.T) {
	store := beads.NewMemStore()
	cfg := workerCity(2)
	plan := workerPlan(cfg, "c1", 1)
	holder, err := store.Create(beads.Bead{
		Title: plan.QualifiedInstance, Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template": "worker", "agent_name": plan.QualifiedInstance, "pool_slot": "1",
			"session_name": PoolSessionName("worker", "gc-old"), "pool_managed": "true",
			"state": string(session.StateFailedCreate),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := newCreateHarness(t, nil)
	h.reserve(t, "c1")

	h.runAll(t, &createPass{cfg: cfg, store: store}, plan)

	assertFailedNoWrite(t, h)
	if rows := sessionRows(t, store); len(rows) != 1 || rows[0].ID != holder.ID {
		t.Fatalf("rows = %+v, want only the unconfirmed holder %s", rows, holder.ID)
	}
}

// Kills removing the planner's identity-lease pre-check without the effect
// fence covering it (OPTION1 F1, S1-3). The planner now plans a create for an
// identity a later-leg copy of a pending row still holds; the create effect
// refuses it under the identifier locks through
// ensurePoolIdentityNotHeldByOpenRow (errPoolSessionNameUnavailable), so the
// refusal settles with cause fence, writes nothing, and the next pass
// advances a slot (F3).
func TestHeldIdentityRefusedUnderCreateLocks(t *testing.T) {
	started := ago(time.Minute)
	canonical := poolRow("gc-1", "worker", 1, "start-pending", "pending_create_claim", "true", "pending_create_started_at", started)
	relic := poolRow("gc-1", "worker", 1, "start-pending", "pending_create_claim", "true", "pending_create_started_at", started,
		"agent_name", "worker-2", "pool_slot", "")
	d := newAllocFixture(t, &config.City{Agents: []config.Agent{allocPoolAgent("worker", 2)}}).
		sessions(canonical).rigLeg(relic).demand("worker", "w-1", "w-2").decide()
	if !slices.ContainsFunc(d.Plans, func(p allocPlan) bool { return p.Plan.qualifiedInstance == "worker-2" }) {
		t.Fatalf("plans %+v trace %v, want a create for worker-2: the lease is the effect's to refuse", d.Plans, d.Trace)
	}

	store := beads.NewMemStore()
	cfg := workerCity(2)
	plan := workerPlan(cfg, "c1", 2)
	if _, err := store.Create(beads.Bead{
		Title: plan.QualifiedInstance, Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template": "worker", "agent_name": plan.QualifiedInstance, "pool_managed": "true",
			"session_name": PoolSessionName("worker", "gc-old"), "state": string(session.StateStartPending), "pending_create_claim": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	h := newCreateHarness(t, nil)
	h.reserve(t, "c1")

	h.runAll(t, &createPass{cfg: cfg, store: store}, plan)

	assertFailedNoWrite(t, h)
	h.assertRefused(t, plan, createStageFence)
	if rows := sessionRows(t, store); len(rows) != 1 {
		t.Fatalf("rows = %+v, want only the holder", rows)
	}
}

// Kills: alias contract changes. A proven collision creates the row without
// its public alias (deferred); an alias query that cannot answer is not a
// proof and fails the create closed.
func TestCreateEffect_ProvenAliasCollisionCreatesWithoutAlias_QueryErrorFailsClosed(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name: "worker", Dir: "rig", StartCommand: "true", MaxActiveSessions: intPtr(2),
			NamepoolNames: []string{"furiosa", "nux"},
		}},
	}
	plan := workerPlan(cfg, "c1", 1)
	if plan.QualifiedInstance != "rig/furiosa" {
		t.Fatalf("fixture instance = %q, want rig/furiosa", plan.QualifiedInstance)
	}

	t.Run("proven collision", func(t *testing.T) {
		store := beads.NewMemStore()
		holder := seedGuardedPoolSessionHolder(t, store, "alias holder", "rig/manual", "rig/furiosa", "manual-furiosa")
		h := newCreateHarness(t, nil)
		h.reserve(t, "c1")

		h.runAll(t, &createPass{cfg: cfg, store: store, planning: []session.Info{holder}}, plan)

		e := h.entry(t)
		if !e.Landed || e.RowID == "" {
			t.Fatalf("settlement = %+v, want landed with the new row", e)
		}
		var created session.Info
		for _, row := range sessionRows(t, store) {
			if row.ID == e.RowID {
				created = row
			}
		}
		if created.ID == "" || created.Alias != "" || created.AgentName != "rig/furiosa" {
			t.Fatalf("created row = %+v, want rig/furiosa without its held alias", created)
		}
	})

	t.Run("query error", func(t *testing.T) {
		mem := beads.NewMemStore()
		h := newCreateHarness(t, nil)
		h.reserve(t, "c1")

		h.runAll(t, &createPass{cfg: cfg, store: &aliasQueryFailStore{Store: mem}}, plan)

		assertFailedNoWrite(t, h)
		if rows := sessionRows(t, mem); len(rows) != 0 {
			t.Fatalf("rows = %+v, want none when the alias query cannot answer", rows)
		}
	})
}

// Kills: the pass's planning reservations dropped from the fenced check
// (C7.1 tier 1). A name the census the pass planned against still holds
// stays reserved for the effect even when no store shows its holder any more.
func TestCreateEffect_PlanningReservationRefusesItsName(t *testing.T) {
	store := beads.NewMemStore()
	cfg := workerCity(2)
	cfg.Agents[0].TmuxAlias = "crew"
	h := newCreateHarness(t, nil)
	h.reserve(t, "c1")
	planning := []session.Info{{ID: "gc-planned", SessionNameMetadata: "crew", MetadataState: "active"}}

	h.runAll(t, &createPass{cfg: cfg, store: store, planning: planning}, workerPlan(cfg, "c1", 1))

	assertFailedNoWrite(t, h)
	if rows := sessionRows(t, store); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none while the planning census holds the name", rows)
	}
}

// Kills: a create without the city identifier flock (C7.1 tier 2).
func TestCreateEffect_LockFailureFailsClosed(t *testing.T) {
	store := beads.NewMemStore()
	cfg := workerCity(2)
	var stderr strings.Builder
	h := newCreateHarness(t, func(host *createEffectHost) {
		host.withLocks = func(string, []string, func() error) error { return errors.New("flock: city lock dir unwritable") }
		host.stderr = &stderr
	})
	h.reserve(t, "c1")

	h.runAll(t, &createPass{cfg: cfg, store: store}, workerPlan(cfg, "c1", 1))

	assertFailedNoWrite(t, h)
	if rows := sessionRows(t, store); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none without the flock", rows)
	}
	if !strings.Contains(stderr.String(), "flock: city lock dir unwritable") {
		t.Fatalf("stderr = %q, want the lock failure reported", stderr.String())
	}
}

// Kills: IsRunning reintroduced into the effect (C7.3, POOL-052). Legacy
// probes the provider before a canonical-singleton create; the allocator
// decided singleton occupancy at plan time from the observation cache, so the
// effect creates without asking.
func TestCreateEffect_NeverProbesProvider(t *testing.T) {
	cfg := workerCity(1) // max 1: the canonical singleton identity
	if !cfg.Agents[0].UsesCanonicalSingletonPoolIdentity() {
		t.Fatal("fixture is not a canonical singleton")
	}
	plan := workerPlan(cfg, "c1", 0)

	// Control: legacy refuses while a runtime holds the singleton name.
	legacyStore := beads.NewMemStore()
	fake := runtime.NewFake()
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, fake, time.Now().UTC(), legacyStore, io.Discard)
	bp.sessionBeads = newSessionBeadSnapshot(nil)
	ids, err := derivePoolSessionIdentifiers(cfg, plan.Template, poolSessionCreateIdentity{AgentName: plan.QualifiedInstance}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Start(context.Background(), ids.sessionName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if _, err := createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], plan.Template, plan.QualifiedInstance, plan.Slot, nil); !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("control: legacy create = %v, want the provider probe to refuse", err)
	}

	store := beads.NewMemStore()
	h := newCreateHarness(t, nil)
	h.reserve(t, "c1")

	h.runAll(t, &createPass{cfg: cfg, sp: probePanicProvider{}, store: store}, plan)

	if e := h.entry(t); !e.Landed || e.RowID == "" {
		t.Fatalf("settlement = %+v, want landed without a provider probe", e)
	}
	if rows := sessionRows(t, store); len(rows) != 1 || rows[0].AgentName != "worker" {
		t.Fatalf("rows = %+v, want the canonical singleton row", rows)
	}
}

// Kills: starting into an unverified work dir; retrying a bad work item
// forever (#34, POOL-055, C6.5a). Evidence that fails verification writes
// nothing and settles refusing that evidence, which a bead's new evidence
// escapes.
func TestCreateEffect_WorktreeEvidenceFailureNoWriteAndWorkBackoff(t *testing.T) {
	store := beads.NewMemStore()
	cfg := workerCity(3)
	root := t.TempDir()
	spec := worktree.Spec{
		RepoDir: filepath.Join(root, "repo"), Path: filepath.Join(root, "wt"), Root: root, Branch: "b",
		BeadID: "w-1", StoreRef: "city:city", Generation: "g1",
	}
	plan := workerPlan(cfg, "c1", 1)
	plan.Metadata = map[string]string{"gc.trigger_bead_id": "w-1"}
	plan.WorktreeSpec = &spec
	var verified []worktree.Spec
	h := newCreateHarness(t, func(host *createEffectHost) {
		host.verify = func(s worktree.Spec) (worktree.Report, error) {
			verified = append(verified, s)
			return worktree.Report{}, errors.New("branch b not checked out")
		}
	})
	h.reserve(t, "c1")

	h.runAll(t, &createPass{cfg: cfg, store: store}, plan)

	assertFailedNoWrite(t, h)
	if rows := sessionRows(t, store); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none for unverified evidence", rows)
	}
	if !reflect.DeepEqual(verified, []worktree.Spec{spec}) {
		t.Fatalf("verified = %+v, want exactly the plan's spec", verified)
	}
	if !h.refusesWork(t, spec) {
		t.Fatal("the settlement does not refuse the evidence that failed")
	}
	next := spec
	next.Generation = "g2"
	if h.refusesWork(t, next) {
		t.Fatal("the settlement refuses a new generation of the bead's evidence")
	}
	// The work item is throttled, not the slot: no identity refusal.
	h.assertNoRefusal(t)
}

// Kills: a plan-only create written without its verified work dir (P3-1
// obligation). The plan-only planner leaves gc.work_dir off and carries the
// spec; the effect verifies it and stamps both work-dir keys, as legacy's
// poolTriggerMetadata does outside planOnly.
func TestCreateEffect_VerifiesPlanOnlyWorktreeSpecAndStampsWorkDir(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(3)}}}
	root := t.TempDir()
	spec := worktree.Spec{
		RepoDir: filepath.Join(root, "repo"), Path: filepath.Join(root, "wt"), Root: root, Branch: "b",
		BeadID: "w-1", StoreRef: "city:city",
	}
	planner := newPlanOnlyHarness(t, cfg, nil)
	_, _, planned, err := selectOrPlanPoolSessionBead(planner.bp, &cfg.Agents[0], "worker", nil,
		SessionRequest{WorkBeadID: "w-1", WorkStoreRef: "city", WorktreeSpec: &spec}, time.Time{}, map[string]bool{}, map[int]bool{})
	if err != nil || planned == nil || planned.worktreeSpec == nil {
		t.Fatalf("plan-only select = (%+v, %v), want a plan carrying the worktree spec", planned, err)
	}
	plan := createPlanOf("c1", "worker", *planned)
	store := beads.NewMemStore()
	h := newCreateHarness(t, func(host *createEffectHost) {
		host.verify = func(s worktree.Spec) (worktree.Report, error) { return worktree.Report{Path: s.Path}, nil }
	})
	h.reserve(t, "c1")

	h.runAll(t, &createPass{cfg: cfg, store: store}, plan)

	if w := h.entry(t).Work; w == nil || w.Refused || w.BeadID != "w-1" {
		t.Fatalf("work verdict = %+v, want w-1 verified, so the planner forgets an earlier refusal", w)
	}

	rows, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v (%v), want one", rows, err)
	}
	if got := rows[0].Metadata; got["gc.work_dir"] != spec.Path || got["work_dir"] != spec.Path || got["gc.trigger_bead_id"] != "w-1" {
		t.Fatalf("row metadata = %v, want the trigger and both work-dir keys at %q", got, spec.Path)
	}
	if _, ok := plan.Metadata["gc.work_dir"]; ok {
		t.Fatal("the effect stamped the plan's shared metadata map")
	}
}

// failingCreateStore refuses every Create: the write may or may not have
// landed, as on a connection error.
type failingCreateStore struct{ beads.Store }

func (failingCreateStore) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, errors.New("connection reset during create")
}

// Kills: clearing an unproven create (CONTRACT v5 P5). A landed create
// settles with its row and the plan's token and clears at once; a failure
// before the write settles with no row; an error from the write itself is
// ambiguous, so its entry clears only when a census row carries its token,
// or at the hard bound.
func TestCreateEffect_CommitsMarkerOrFailsNoWrite_AmbiguousLeavesMarker(t *testing.T) {
	cfg := workerCity(2)

	t.Run("committed", func(t *testing.T) {
		store := beads.NewMemStore()
		startedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.FixedZone("x", 3600))
		h := newCreateHarness(t, func(host *createEffectHost) { host.now = func() time.Time { return startedAt } })
		token := h.reserve(t, "c1")

		h.runAll(t, &createPass{cfg: cfg, store: store}, workerPlan(cfg, "c1", 1))

		e := h.entry(t)
		rows := sessionRows(t, store)
		if len(rows) != 1 || !e.Landed || e.Ambiguous || e.RowID != rows[0].ID || e.Token != token {
			t.Fatalf("settlement = %+v rows = %+v, want landed with the row and the plan's token", e, rows)
		}
		if rows[0].InstanceToken != token {
			t.Fatalf("row token = %q, want the plan's %q", rows[0].InstanceToken, token)
		}
		raw, err := store.Get(rows[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := raw.Metadata["pending_create_started_at"]; got != "2026-10-03T11:00:00Z" {
			t.Fatalf("pending_create_started_at = %q, want the effect's clock in UTC", got)
		}
		if m := settledCreateEntry(t, e); len(m.view().Entries) != 0 {
			t.Fatalf("in-flight entries after a landed create = %+v, want none: the census shows its row", m.view().Entries)
		}
	})

	t.Run("failed before the write", func(t *testing.T) {
		store := beads.NewMemStore()
		h := newCreateHarness(t, nil)
		h.reserve(t, "c1")
		plan := createPlan{EntryID: "c1", Template: "ghost", QualifiedInstance: "ghost-1", Slot: 1}

		h.runAll(t, &createPass{cfg: cfg, store: store}, plan)

		assertFailedNoWrite(t, h)
		h.assertRefused(t, plan, createStageStalePlan)
	})

	t.Run("ambiguous write", func(t *testing.T) {
		h := newCreateHarness(t, nil)
		token := h.reserve(t, "c1")

		h.runAll(t, &createPass{cfg: cfg, store: failingCreateStore{Store: beads.NewMemStore()}}, workerPlan(cfg, "c1", 1))

		e := h.entry(t)
		if e.Landed || !e.Ambiguous || e.RowID != "" || e.Token != token {
			t.Fatalf("settlement = %+v, want ambiguous with only the token as its marker", e)
		}
		m := settledCreateEntry(t, e)
		if got := m.clearVisible(inflightCensus{}, e.At.Add(time.Second)); len(got) != 0 || len(m.view().Entries) != 1 {
			t.Fatalf("an ambiguous create cleared by a later read without its token: %+v", got)
		}
		if got := m.clearVisible(inflightCensus{Tokens: map[string]bool{token: true}}, e.At); len(got) != 1 || got[0].HardBound {
			t.Fatalf("clears with the token's row = %+v, want one by marker", got)
		}
		h.assertNoRefusal(t)
	})
}

// refusingCreateStore refuses every Create with err, writing nothing.
type refusingCreateStore struct {
	beads.Store
	err error
}

func (s refusingCreateStore) Create(beads.Bead) (beads.Bead, error) { return beads.Bead{}, s.err }

// firstListFailStore fails the first List: the locked live re-census of
// the leg (a transient leg read failure, a Dolt outage or a bd timeout).
type firstListFailStore struct {
	beads.Store
	failed bool
}

func (s *firstListFailStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if !s.failed {
		s.failed = true
		return nil, errors.New("leg unavailable")
	}
	return s.Store.List(q)
}

// Kills: a read failure under the locks recorded as fence (F3). Only a taken
// name is fence, which moves the planner's request to the next slot; a
// failed live re-census or alias query proves nothing about the name and is
// fence-read, which stalls it. A taken name stays fence
// (TestCreateEffect_NoWriteFailureBacksOffIdentity).
func TestCreateEffect_LockedReadFailureBacksOffFenceRead(t *testing.T) {
	cfg := workerCity(60)
	for name, store := range map[string]beads.Store{
		"re-census read error": &firstListFailStore{Store: beads.NewMemStore()},
		"alias query error":    &aliasQueryFailStore{Store: beads.NewMemStore()},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCreateHarness(t, nil)
			h.reserve(t, "c1")
			plan := workerPlan(cfg, "c1", 1)
			h.runAll(t, &createPass{cfg: cfg, store: store}, plan)
			assertFailedNoWrite(t, h)
			h.assertRefused(t, plan, createStageFenceRead)
		})
	}
}

// Kills: a create the store provably refused settled as ambiguous (latent 1
// from the P3-6b review): it would hold the token until resolved and skip
// the create backoff, so the planner re-plans the identity at pass rate. A gate
// refusal, a code-less not-found, a lost fence, a store that cannot fence, a
// closed store and a SQLite write out of busy retries fail the entry with no
// row and back off the identity, as fence-read: none proves the name taken
// (F3).
func TestCreateEffect_RefusedWriteFailsNoWriteAndBacksOff(t *testing.T) {
	cfg := workerCity(2)
	for name, err := range map[string]error{
		"gate refusal":                   &beads.GateRefusalError{Verb: "create", Code: "policy"},
		"code-less not found":            fmt.Errorf("bd create: %w", beads.ErrNotFound),
		"precondition failed":            &beads.PreconditionFailedError{Expected: 1, Current: 2},
		"conditional writes unsupported": beads.ErrConditionalWriteUnsupported,
		"sqlite store closed":            fmt.Errorf("sqlite store: %w", beads.ErrStoreClosed),
		"native Dolt store closed":       fmt.Errorf("native Dolt store: %w", beads.ErrStoreClosed),
		"sqlite busy retries exhausted":  fmt.Errorf("sqlite create: begin tx: %w", beads.ErrSQLiteBusyExhausted),
	} {
		t.Run(name, func(t *testing.T) {
			mem := beads.NewMemStore()
			h := newCreateHarness(t, nil)
			h.reserve(t, "c1")
			plan := workerPlan(cfg, "c1", 1)
			h.runAll(t, &createPass{cfg: cfg, store: refusingCreateStore{Store: mem, err: err}}, plan)
			assertFailedNoWrite(t, h)
			h.assertRefused(t, plan, createStageFenceRead)
			if rows := sessionRows(t, mem); len(rows) != 0 {
				t.Fatalf("rows = %+v, want none", rows)
			}
		})
	}
}

// gateLocker parks every effect inside the identifier locks until release,
// counting how many are inside at once.
type gateLocker struct {
	release chan struct{}

	mu        sync.Mutex
	inside    int
	maxInside int
}

func (g *gateLocker) withLocks(_ string, _ []string, fn func() error) error {
	g.mu.Lock()
	g.inside++
	g.maxInside = max(g.maxInside, g.inside)
	g.mu.Unlock()
	<-g.release
	defer func() {
		g.mu.Lock()
		g.inside--
		g.mu.Unlock()
	}()
	return fn()
}

func (g *gateLocker) counts() (inside, maxInside int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inside, g.maxInside
}

// Kills: unbounded create fan-out (POOL-053, C1.10).
func TestCreateEffect_ParallelBoundedAtEight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := beads.NewMemStore()
		cfg := workerCity(30)
		gate := &gateLocker{release: make(chan struct{})}
		h := newCreateHarness(t, func(host *createEffectHost) { host.withLocks = gate.withLocks })
		var plans []createPlan
		for i := 1; i <= 20; i++ {
			id := fmt.Sprintf("c%02d", i)
			h.reserve(t, id)
			plans = append(plans, workerPlan(cfg, id, i))
		}
		pass := &createPass{cfg: cfg, store: store}

		h.submit(pass, plans[:5]...)
		h.submit(pass, plans[5:]...)
		synctest.Wait()
		if inside, _ := gate.counts(); inside != createEffectParallelism {
			t.Fatalf("effects inside the locks = %d, want %d", inside, createEffectParallelism)
		}
		close(gate.release)
		h.x.wg.Wait()

		if _, maxInside := gate.counts(); maxInside != createEffectParallelism {
			t.Fatalf("max concurrent effects = %d, want %d", maxInside, createEffectParallelism)
		}
		if rows := sessionRows(t, store); len(rows) != 20 {
			t.Fatalf("rows = %d, want 20", len(rows))
		}
		for _, e := range h.settlements() {
			if !e.Landed {
				t.Fatalf("settlement %+v, want every create landed", e)
			}
		}
	})
}

// Kills: leaked effect goroutines, and plans run after shutdown began (C1.8).
// Shutdown stops admission, drops queued plans unsettled, and waits for the
// effects in flight; with an expired context it returns without them.
func TestCreateEffect_JoinedAtShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := beads.NewMemStore()
		cfg := workerCity(30)
		gate := &gateLocker{release: make(chan struct{})}
		h := newCreateHarness(t, func(host *createEffectHost) { host.withLocks = gate.withLocks })
		var plans []createPlan
		for i := 1; i <= 10; i++ {
			id := fmt.Sprintf("c%02d", i)
			h.reserve(t, id)
			plans = append(plans, workerPlan(cfg, id, i))
		}
		pass := &createPass{cfg: cfg, store: store}
		h.submit(pass, plans...)
		synctest.Wait()

		expired, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := h.x.shutdown(expired); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown with effects in flight past its deadline = %v, want DeadlineExceeded", err)
		}
		done := make(chan error, 1)
		go func() { done <- h.x.shutdown(context.Background()) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("shutdown returned %v while effects were in flight", err)
		default:
		}
		h.reserve(t, "late")
		if h.submit(pass, workerPlan(cfg, "late", 20)) {
			t.Fatal("submit accepted a plan after shutdown began")
		}

		close(gate.release)
		if err := <-done; err != nil {
			t.Fatalf("shutdown = %v, want nil once effects finished", err)
		}
		settled := h.settlements()
		for _, e := range settled {
			if !e.Landed {
				t.Fatalf("settlement %+v, want only the landed effects that were in flight", e)
			}
		}
		if len(settled) != createEffectParallelism {
			t.Fatalf("settlements = %d, want %d: plans dropped at shutdown never run", len(settled), createEffectParallelism)
		}
		if rows := sessionRows(t, store); len(rows) != createEffectParallelism {
			t.Fatalf("rows = %d, want %d", len(rows), createEffectParallelism)
		}
	})
}

// normalizedSessionRows reads every session row with the per-create random
// and clock fields blanked, so two creates of the same plan compare equal.
func normalizedSessionRows(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	rows, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		rows[i].CreatedAt, rows[i].UpdatedAt = time.Time{}, time.Time{}
		for _, key := range []string{"instance_token", "pending_create_started_at"} {
			if rows[i].Metadata[key] != "" {
				rows[i].Metadata[key] = "<" + key + ">"
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

// Kills: pool and dependency-floor create metadata drifting from legacy. The
// legacy planner creates on one store; the plan-only planner plans the same
// request on an identical store and the effect creates it on a third.
func TestCreateEffect_PoolMetadataByteIdenticalToLegacyPlannerCreate(t *testing.T) {
	request := SessionRequest{WorkBeadID: "w-1", WorkStoreRef: "city", WorkPack: "pk", BrainParentSID: "s-parent"}
	cases := []struct {
		name       string
		agent      config.Agent
		dependency bool
		rig        bool
	}{
		{name: "transient slot", agent: config.Agent{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(3)}},
		{name: "namepool alias", agent: config.Agent{Name: "worker", Dir: "rig", StartCommand: "true", MaxActiveSessions: intPtr(2), NamepoolNames: []string{"furiosa", "nux"}}},
		{name: "tmux alias", agent: config.Agent{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2), TmuxAlias: "crew"}},
		{name: "templated tmux alias", agent: config.Agent{Name: "worker", Dir: "rig", StartCommand: "true", MaxActiveSessions: intPtr(2), TmuxAlias: "{{.CityName}}-{{.Rig}}-crew"}, rig: true},
		{name: "canonical singleton", agent: config.Agent{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(1)}},
		{name: "dependency floor", agent: config.Agent{Name: "db", StartCommand: "true", MaxActiveSessions: intPtr(3)}, dependency: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{tc.agent}}
			if tc.rig {
				cfg.Rigs = []config.Rig{{Name: "rig", Path: t.TempDir()}}
			}
			template := cfg.Agents[0].QualifiedName()

			legacy := newLegacyHarness(t, cfg, nil)
			legacyStore := beads.NewMemStore()
			legacy.bp.beadStore = legacyStore
			legacy.bp.rigs = cfg.Rigs // as newAgentBuildParams sets them
			if tc.dependency {
				if _, _, err := selectOrCreateDependencyPoolSessionBeadWithSlot(legacy.bp, &cfg.Agents[0], template); err != nil {
					t.Fatalf("legacy dependency create: %v", err)
				}
			} else {
				_, _, planned, err := selectOrPlanPoolSessionBead(legacy.bp, &cfg.Agents[0], template, nil, request, time.Time{}, map[string]bool{}, map[int]bool{})
				if err != nil || planned == nil {
					t.Fatalf("legacy plan = (%+v, %v)", planned, err)
				}
				if _, err := executePlannedPoolSessionBeadCreate(legacy.bp, &cfg.Agents[0], template, *planned); err != nil {
					t.Fatalf("legacy create: %v", err)
				}
			}

			planner := newPlanOnlyHarness(t, cfg, nil)
			planner.bp.cityPath = legacy.bp.cityPath // work dirs derive from it
			planner.bp.rigs = cfg.Rigs
			var planned *poolSessionCreatePlan
			var err error
			if tc.dependency {
				_, _, planned, err = selectOrPlanDependencyPoolSessionBead(planner.bp, &cfg.Agents[0], template, time.Time{})
			} else {
				_, _, planned, err = selectOrPlanPoolSessionBead(planner.bp, &cfg.Agents[0], template, nil, request, time.Time{}, map[string]bool{}, map[int]bool{})
			}
			if err != nil || planned == nil {
				t.Fatalf("plan-only plan = (%+v, %v)", planned, err)
			}
			// The effect creates with the plan's slot, as legacy does, and
			// checks it against the pool slot: the planner must derive both
			// alike (createIdentity.agentIn).
			if planned.slot != planned.poolSlot {
				t.Fatalf("plan slot %d, pool slot %d: the effect would refuse the plan", planned.slot, planned.poolSlot)
			}
			effectStore := beads.NewMemStore()
			h := newCreateHarness(t, func(host *createEffectHost) {
				host.cityPath, host.cityName = legacy.bp.cityPath, legacy.bp.cityName
				host.lookPath = legacy.bp.lookPath
			})
			h.reserve(t, "c1")
			h.runAll(t, &createPass{cfg: cfg, sp: runtime.NewFake(), store: effectStore}, createPlanOf("c1", template, *planned))

			want, got := normalizedSessionRows(t, legacyStore), normalizedSessionRows(t, effectStore)
			if len(want) != 1 || !reflect.DeepEqual(got, want) {
				t.Fatalf("effect rows differ from legacy:\n got  %+v\n want %+v", got, want)
			}
			if tc.rig && want[0].Metadata["session_name"] != "city-rig-crew" {
				t.Fatalf("session_name = %q, want the template rendered with city and rig", want[0].Metadata["session_name"])
			}
		})
	}
}

// guardedCreateWorld is one legacy guarded-create call: build params and the
// arguments legacy passes. build returns a fresh world each call, so the
// refactored and the frozen function each run on their own stores.
type guardedCreateWorld struct {
	bp                *agentBuildParams
	agent             *config.Agent
	template          string
	qualifiedInstance string
	slot              int
	metadata          map[string]string
	locks             poolSessionIdentifierLockFunc
	stores            []beads.Store
	fake              *runtime.Fake
}

// guardedCreateOutcome is everything a legacy guarded create can affect.
type guardedCreateOutcome struct {
	Err       string
	ID        string
	Rows      [][]beads.Bead
	Writeback []string
	Locks     [][]string
	Calls     []runtime.Call
}

func (w guardedCreateWorld) outcome(t *testing.T, create func(*guardedCreateWorld, poolSessionIdentifierLockFunc) (session.Info, error)) guardedCreateOutcome {
	t.Helper()
	var out guardedCreateOutcome
	recorder := func(cityPath string, identifiers []string, fn func() error) error {
		out.Locks = append(out.Locks, append([]string(nil), identifiers...))
		return w.locks(cityPath, identifiers, fn)
	}
	info, err := create(&w, recorder)
	if err != nil {
		out.Err = err.Error()
	}
	out.ID = info.ID
	for _, store := range w.stores {
		out.Rows = append(out.Rows, normalizedSessionRows(t, store))
	}
	for _, row := range w.bp.sessionBeads.OpenInfos() {
		out.Writeback = append(out.Writeback, row.ID+"/"+row.SessionNameMetadata+"/"+row.Alias)
	}
	if w.fake != nil {
		out.Calls = w.fake.Calls
	}
	return out
}

func passthroughLocks(_ string, _ []string, fn func() error) error { return fn() }

// Kills: the effect-local view changing legacy. Every fixture runs through
// the refactored legacy entry (view from bp) and through the frozen
// pre-refactor function, on separate identical worlds; the error, the rows,
// the primary writeback, the lock sets and the provider calls must match.
func TestCreatePoolSessionBeadWithGuardedAliasMatchesPreRefactor(t *testing.T) {
	newBP := func(t *testing.T, cfg *config.City, store beads.Store) (*agentBuildParams, *runtime.Fake) {
		fake := runtime.NewFake()
		bp := newAgentBuildParams("test-city", t.TempDir(), cfg, fake, time.Now().UTC(), store, io.Discard)
		bp.sessionBeads = newSessionBeadSnapshot(nil)
		return bp, fake
	}
	basic := func(t *testing.T, cfg *config.City, store beads.Store, slot int) guardedCreateWorld {
		bp, fake := newBP(t, cfg, store)
		_, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(&cfg.Agents[0], slot)
		return guardedCreateWorld{
			bp: bp, agent: &cfg.Agents[0], template: cfg.Agents[0].QualifiedName(), qualifiedInstance: qualifiedInstance,
			slot: poolSlot, locks: passthroughLocks, stores: []beads.Store{store}, fake: fake,
		}
	}
	rigCity := func(t *testing.T, agent config.Agent) *config.City {
		agent.Dir = "rig"
		return &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Rigs:      []config.Rig{{Name: "rig", Path: t.TempDir()}},
			Agents:    []config.Agent{agent},
		}
	}
	withForeign := func(t *testing.T, w guardedCreateWorld, foreign beads.Store) guardedCreateWorld {
		primeGuardedPoolCrossStoreCensus(t, w.bp, map[string]beads.Store{"rig": foreign})
		w.stores = append(w.stores, foreign)
		return w
	}
	namepool := config.Agent{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2), NamepoolNames: []string{"furiosa", "nux"}}
	plain := config.Agent{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}

	// want names the branch each fixture must reach: a substring of the
	// error, or "" for a create.
	type fixture struct {
		want  string
		build func(t *testing.T) guardedCreateWorld
	}
	cases := map[string]fixture{
		"transient slot": {"", func(t *testing.T) guardedCreateWorld {
			return basic(t, workerCity(2), beads.NewMemStore(), 1)
		}},
		"trigger metadata": {"", func(t *testing.T) guardedCreateWorld {
			w := basic(t, workerCity(2), beads.NewMemStore(), 2)
			w.metadata = map[string]string{"gc.trigger_bead_id": "w-1", "gc.work_dir": "/w"}
			return w
		}},
		"tmux alias": {"", func(t *testing.T) guardedCreateWorld {
			cfg := workerCity(2)
			cfg.Agents[0].TmuxAlias = "crew"
			return basic(t, cfg, beads.NewMemStore(), 2)
		}},
		"templated tmux alias": {"", func(t *testing.T) guardedCreateWorld {
			agent := plain
			agent.TmuxAlias = "{{.CityName}}-{{.Rig}}-crew"
			return basic(t, rigCity(t, agent), beads.NewMemStore(), 2)
		}},
		"suspended rig leg": {"", func(t *testing.T) guardedCreateWorld {
			w := basic(t, rigCity(t, plain), beads.NewMemStore(), 1)
			w.bp.sessionCensusRigStores = map[string]beads.Store{"rig": &toggleListFailStore{Store: beads.NewMemStore(), fail: true}}
			w.bp.sessionCensusSuspendedRigPaths = map[string]bool{filepath.Clean(w.bp.city.Rigs[0].Path): true}
			return w
		}},
		"canonical singleton idle": {"", func(t *testing.T) guardedCreateWorld {
			return basic(t, workerCity(1), beads.NewMemStore(), 0)
		}},
		"canonical singleton running": {"still occupies singleton", func(t *testing.T) guardedCreateWorld {
			w := basic(t, workerCity(1), beads.NewMemStore(), 0)
			ids, err := derivePoolSessionIdentifiers(w.bp.city, w.template, poolSessionCreateIdentity{AgentName: w.qualifiedInstance}, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := w.fake.Start(context.Background(), ids.sessionName, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			w.fake.Calls = nil
			return w
		}},
		"planning holder gone from the stores": {"unavailable", func(t *testing.T) guardedCreateWorld {
			cfg := workerCity(2)
			cfg.Agents[0].TmuxAlias = "crew"
			w := basic(t, cfg, beads.NewMemStore(), 1)
			w.bp.sessionOccupancyInfos = []session.Info{{ID: "gc-gone", SessionNameMetadata: "crew", MetadataState: "active"}}
			return w
		}},
		"proven alias collision": {"", func(t *testing.T) guardedCreateWorld {
			store := beads.NewMemStore()
			seedGuardedPoolSessionHolder(t, store, "alias holder", "rig/manual", "rig/furiosa", "manual-furiosa")
			return basic(t, rigCity(t, namepool), store, 1)
		}},
		"foreign alias collision": {"", func(t *testing.T) guardedCreateWorld {
			foreign := beads.NewMemStore()
			seedGuardedPoolSessionHolder(t, foreign, "foreign alias holder", "rig/manual", "rig/furiosa", "manual-furiosa")
			return withForeign(t, basic(t, rigCity(t, namepool), beads.NewMemStore(), 1), foreign)
		}},
		"late foreign exact-name holder": {"unavailable", func(t *testing.T) guardedCreateWorld {
			foreign := beads.NewMemStore()
			w := withForeign(t, basic(t, rigCity(t, plain), beads.NewMemStore(), 1), foreign)
			name := poolRuntimeSessionName(w.bp.city, w.qualifiedInstance, w.template, true)
			w.locks = func(_ string, _ []string, fn func() error) error {
				seedGuardedPoolSessionHolder(t, foreign, "late holder", "rig/manual", "", name)
				return fn()
			}
			return w
		}},
		"foreign recensus error": {"cross-store list failed", func(t *testing.T) guardedCreateWorld {
			foreign := &toggleListFailStore{Store: beads.NewMemStore()}
			w := withForeign(t, basic(t, rigCity(t, plain), beads.NewMemStore(), 1), foreign)
			foreign.fail = true
			w.stores = w.stores[:1]
			return w
		}},
		"alias query error": {"alias index unavailable", func(t *testing.T) guardedCreateWorld {
			mem := beads.NewMemStore()
			w := basic(t, rigCity(t, namepool), &aliasQueryFailStore{Store: mem}, 1)
			w.stores = []beads.Store{mem}
			return w
		}},
		"lock failure": {"flock failed", func(t *testing.T) guardedCreateWorld {
			w := basic(t, workerCity(2), beads.NewMemStore(), 1)
			w.locks = func(string, []string, func() error) error { return errors.New("flock failed") }
			return w
		}},
		"identity lease held": {"held by open session", func(t *testing.T) guardedCreateWorld {
			store := beads.NewMemStore()
			if _, err := store.Create(beads.Bead{
				Title: "worker-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{
					"template": "worker", "agent_name": "worker-1", "pool_slot": "1", "pool_managed": "true",
					"session_name": "worker-gc-old", "state": string(session.StateStartPending), "pending_create_claim": "true",
				},
			}); err != nil {
				t.Fatal(err)
			}
			return basic(t, workerCity(2), store, 1)
		}},
		"create write error": {"connection reset during create", func(t *testing.T) guardedCreateWorld {
			mem := beads.NewMemStore()
			w := basic(t, workerCity(2), failingCreateStore{Store: mem}, 1)
			w.stores = []beads.Store{mem}
			return w
		}},
		"no store": {"session store unavailable", func(t *testing.T) guardedCreateWorld {
			w := basic(t, workerCity(2), nil, 1)
			w.stores = nil
			return w
		}},
		"unsupported transport": {"cannot route tmux sessions", func(t *testing.T) guardedCreateWorld {
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city", Provider: "opencode"},
				Session:   config.SessionConfig{Provider: config.SessionTransportACP},
				Providers: map[string]config.ProviderSpec{"opencode": {
					Command: "echo", ACPCommand: "echo", PromptMode: "none", SupportsACP: boolPtr(true),
				}},
				Agents: []config.Agent{{Name: "worker", Provider: "opencode", Session: config.SessionTransportTmux, MaxActiveSessions: intPtr(1)}},
			}
			store := beads.NewMemStore()
			bp := newAgentBuildParams("test-city", t.TempDir(), cfg, &acpOnlyDesiredStateProvider{Fake: runtime.NewFake()}, time.Now().UTC(), store, io.Discard)
			bp.sessionBeads = newSessionBeadSnapshot(nil)
			return guardedCreateWorld{
				bp: bp, agent: &cfg.Agents[0], template: "worker", qualifiedInstance: "worker",
				locks: passthroughLocks, stores: []beads.Store{store},
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tc.build(t).outcome(t, func(w *guardedCreateWorld, locks poolSessionIdentifierLockFunc) (session.Info, error) {
				return createPoolSessionBeadWithGuardedAliasUsingLock(poolCreateViewOf(w.bp), w.agent, w.template, w.qualifiedInstance, w.slot, w.metadata, locks)
			})
			want := tc.build(t).outcome(t, func(w *guardedCreateWorld, locks poolSessionIdentifierLockFunc) (session.Info, error) {
				return createPoolSessionBeadWithGuardedAliasUsingLockPreRefactor(w.bp, w.agent, w.template, w.qualifiedInstance, w.slot, w.metadata, locks)
			})
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("refactored legacy create differs from pre-refactor:\n got  %+v\n want %+v", got, want)
			}
			if reached := (tc.want == "" && got.Err == "" && got.ID != "") || (tc.want != "" && strings.Contains(got.Err, tc.want)); !reached {
				t.Fatalf("fixture outcome = (id %q, err %q), want %q", got.ID, got.Err, tc.want)
			}
		})
	}
}

// closedNameOwner seeds a closed manual session row that once owned
// sessionName. Explicit names stay owned after close, and the census (open
// rows) never shows the owner, so every create of that name is refused
// inside the fence: the doomed create AM-N8 throttles.
func closedNameOwner(t *testing.T, store beads.Store, sessionName string) {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title: "old manual", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:someone"},
		Metadata: map[string]string{
			"session_name": sessionName, "agent_name": "someone", "template": "someone",
			"state": "asleep", "manual_session": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
}

// Kills (AM-N8, S4): a doomed create that settles without refusing its
// identity, so the planner re-plans it at pass rate; a refusal on a landed or
// an ambiguous create; a parallel failure that settles other than once. The
// refusal record's doubling, cap and ConfigRev pruning are the table's own
// (allocator_backoff_test.go). The planner applies a settlement to the
// in-flight map and the table in one step (C5.12), so no pass can see the
// failure without its refusal.
func TestCreateEffect_NoWriteFailureRefusesIdentity(t *testing.T) {
	cfg := workerCity(60)
	cfg.Agents[0].TmuxAlias = "crew"
	doomed := beads.NewMemStore()
	closedNameOwner(t, doomed, "crew")
	h := newCreateHarness(t, nil)
	attempt := func(id string, store beads.Store) createSettlement {
		t.Helper()
		h.reserve(t, id)
		h.runAll(t, &createPass{cfg: cfg, store: store}, workerPlan(cfg, id, 1))
		return h.settlementOf(t, id)
	}
	h.reserve(t, "c1")
	h.runAll(t, &createPass{cfg: cfg, store: doomed}, workerPlan(cfg, "c1", 1))
	assertFailedNoWrite(t, h)
	h.assertRefused(t, workerPlan(cfg, "c1", 1), createStageFence)
	if s := attempt("ok", beads.NewMemStore()); !s.Landed || s.Stage != "" {
		t.Fatalf("landed settlement %+v, want landed with no refusal", s)
	}
	if s := attempt("ambiguous", failingCreateStore{Store: beads.NewMemStore()}); !s.Ambiguous || s.Stage != "" {
		t.Fatalf("ambiguous settlement %+v, want ambiguous with no refusal", s)
	}

	// 40 doomed effects fail in parallel; each settles once, refusing its
	// identity. Slot i's runtime name is crew-i.
	var plans []createPlan
	for i := 1; i <= 40; i++ {
		if i > 1 {
			closedNameOwner(t, doomed, fmt.Sprintf("crew-%d", i))
		}
		id := fmt.Sprintf("p%02d", i)
		h.reserve(t, id)
		plans = append(plans, workerPlan(cfg, id, i))
	}
	h.runAll(t, &createPass{cfg: cfg, store: doomed}, plans...)
	for _, p := range plans {
		h.assertRefused(t, p, createStageFence)
	}
}

// Kills: the plan's own reservation fencing its create (C7.2). The in-flight
// map holds the entry's reservation and the pass hands the census rows, so the
// create commits with its alias. The control shows why createPass.planning
// must hold census rows only: the plan's own reservation, shaped as a row,
// refuses the create.
func TestCreateEffect_OwnReservationNeverFencesItself(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", Dir: "rig", StartCommand: "true", MaxActiveSessions: intPtr(2), NamepoolNames: []string{"furiosa", "nux"}}},
	}
	plan := workerPlan(cfg, "c1", 1)
	other := session.Info{ID: "gc-other", AgentName: "rig/nux", Alias: "rig/nux", Template: plan.Template, SessionNameMetadata: "nux-box", MetadataState: "active"}

	store := beads.NewMemStore()
	h := newCreateHarness(t, nil)
	h.reserve(t, "c1")
	h.runAll(t, &createPass{cfg: cfg, store: store, planning: []session.Info{other}}, plan)
	e := h.entry(t)
	rows := sessionRows(t, store)
	if !e.Landed || len(rows) != 1 || rows[0].Alias != plan.QualifiedInstance {
		t.Fatalf("settlement %+v rows %+v, want landed with alias %q", e, rows, plan.QualifiedInstance)
	}

	control := newCreateHarness(t, nil)
	control.reserve(t, "c1")
	own := session.Info{Alias: plan.QualifiedInstance, AgentName: plan.QualifiedInstance, Template: plan.Template}
	controlStore := beads.NewMemStore()
	control.runAll(t, &createPass{cfg: cfg, store: controlStore, planning: []session.Info{other, own}}, plan)
	if rows := sessionRows(t, controlStore); len(rows) == 1 && rows[0].Alias == plan.QualifiedInstance {
		t.Fatalf("control: the own reservation in planning did not fence the create: %+v", rows)
	}
}

// prefixedMemStore pre-mints row IDs from the instance token, as the bd
// stores do (poolSessionExplicitBeadID).
type prefixedMemStore struct{ *beads.MemStore }

func newPrefixedMemStore() prefixedMemStore {
	m := beads.NewMemStore()
	m.HonorExplicitIDs = true
	return prefixedMemStore{MemStore: m}
}

func (prefixedMemStore) IDPrefix() string { return "gc" }

// prefixedFailingCreateStore pre-mints IDs and fails every Create.
type prefixedFailingCreateStore struct{ prefixedMemStore }

func (prefixedFailingCreateStore) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, errors.New("connection reset during create")
}

// panicAfterCreateStore writes the row, then panics.
type panicAfterCreateStore struct{ prefixedMemStore }

func (s panicAfterCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	if _, err := s.prefixedMemStore.Create(b); err != nil {
		return beads.Bead{}, err
	}
	panic("store driver crashed after the insert")
}

// Kills: the pre-minted row ID dropped from a commit or from an ambiguous
// marker (the census then finds the row only by its token).
func TestCreateEffect_PreMintedRowIDIsTheMarker(t *testing.T) {
	cfg := workerCity(2)
	t.Run("committed", func(t *testing.T) {
		store := newPrefixedMemStore()
		h := newCreateHarness(t, nil)
		token := h.reserve(t, "c1")
		h.runAll(t, &createPass{cfg: cfg, store: store}, workerPlan(cfg, "c1", 1))
		if e := h.entry(t); !e.Landed || e.RowID != "gc-session-"+token || e.Token != token {
			t.Fatalf("settlement %+v, want landed with row gc-session-%s", e, token)
		}
	})
	t.Run("ambiguous write", func(t *testing.T) {
		h := newCreateHarness(t, nil)
		token := h.reserve(t, "c1")
		h.runAll(t, &createPass{cfg: cfg, store: prefixedFailingCreateStore{newPrefixedMemStore()}}, workerPlan(cfg, "c1", 1))
		if e := h.entry(t); !e.Ambiguous || e.RowID != "gc-session-"+token || e.Token != token {
			t.Fatalf("settlement %+v, want ambiguous with row gc-session-%s", e, token)
		}
		h.assertNoRefusal(t)
	})
}

// setMarkerFailStore lets Create through, then fails the follow-up write of
// the bead-scoped session_name with err.
type setMarkerFailStore struct {
	beads.Store
	err error
}

func (s setMarkerFailStore) SetMetadata(id, key, value string) error {
	if key == "session_name" {
		return s.err
	}
	return s.Store.SetMetadata(id, key, value)
}

func (s setMarkerFailStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs["session_name"]; ok {
		return s.err
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s setMarkerFailStore) Update(id string, opts beads.UpdateOpts) error {
	if _, ok := opts.Metadata["session_name"]; ok {
		return s.err
	}
	return s.Store.Update(id, opts)
}

// Kills: a failure after the row landed (the session_name follow-up of a
// store that pre-mints no ID) read as no write: a refusal for a row
// that exists. A refusal class from that follow-up (a store closed after the
// Create, a not-found) proves only that the follow-up wrote nothing.
func TestCreateEffect_FailureAfterTheWriteIsAmbiguous(t *testing.T) {
	cfg := workerCity(2)
	for name, err := range map[string]error{
		"connection lost": errors.New("dolt: connection lost during set"),
		"store closed":    fmt.Errorf("sqlite store: %w", beads.ErrStoreClosed),
		"not found":       fmt.Errorf("bd update: %w", beads.ErrNotFound),
	} {
		t.Run(name, func(t *testing.T) {
			mem := beads.NewMemStore()
			h := newCreateHarness(t, nil)
			token := h.reserve(t, "c1")
			h.runAll(t, &createPass{cfg: cfg, store: setMarkerFailStore{Store: mem, err: err}}, workerPlan(cfg, "c1", 1))
			all, err := mem.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if err != nil || len(all) != 1 {
				t.Fatalf("rows = %+v (%v), want the one row the create wrote", all, err)
			}
			if e := h.entry(t); !e.Ambiguous || e.RowID != all[0].ID || e.Token != token {
				t.Fatalf("settlement %+v, want ambiguous with row %s", e, all[0].ID)
			}
			h.assertNoRefusal(t)
		})
	}
}

// Kills: busy text alone read as a write that committed nothing. Only the
// SQLite store's exhausted retries prove that (ErrSQLiteBusyExhausted); the
// same text from another writer (a bd subprocess) stays ambiguous.
func TestCreateEffect_UnprovenBusyWriteIsAmbiguous(t *testing.T) {
	cfg := workerCity(2)
	h := newCreateHarness(t, nil)
	h.reserve(t, "c1")
	busy := errors.New("bd create: database is locked (5) (SQLITE_BUSY)")
	h.runAll(t, &createPass{cfg: cfg, store: refusingCreateStore{Store: beads.NewMemStore(), err: busy}}, workerPlan(cfg, "c1", 1))
	if e := h.entry(t); !e.Ambiguous {
		t.Fatalf("settlement %+v, want ambiguous", e)
	}
	h.assertNoRefusal(t)
}

// Kills (ME7): a refusal fingerprinted by anything but the plan's ConfigRev,
// the revision the planner decided it under; the planner prunes the record
// when that revision changes.
func TestCreateEffect_RefusalCarriesThePlanConfigRev(t *testing.T) {
	cfg := workerCity(60)
	cfg.Agents[0].TmuxAlias = "crew"
	doomed := beads.NewMemStore()
	closedNameOwner(t, doomed, "crew")
	h := newCreateHarness(t, nil)
	plan := workerPlan(cfg, "c1", 1)
	plan.Token, plan.ConfigRev = session.NewInstanceToken(), "rev-entry"
	h.runAll(t, &createPass{cfg: cfg, store: doomed}, plan)
	if s := h.entry(t); s.ConfigRev != "rev-entry" || s.Stage != createStageFence {
		t.Fatalf("settlement %+v, want fence under the plan's ConfigRev rev-entry", s)
	}
}

// Kills: a panic before the row write settled as ambiguous (an entry held
// until a later read for a create that wrote nothing); a panic after it
// settled as no write (a refusal for a row that exists); the pre-minted row
// ID lost on the panic path; a panic that skips the settle and strands the
// entry running.
func TestCreateEffect_PanicSettlesByWhetherTheWriteBegan(t *testing.T) {
	cfg := workerCity(2)
	t.Run("before the write", func(t *testing.T) {
		// The locked alias check panics, inside the fence.
		cfg := &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents:    []config.Agent{{Name: "worker", Dir: "rig", StartCommand: "true", MaxActiveSessions: intPtr(2), NamepoolNames: []string{"furiosa", "nux"}}},
		}
		store := beads.NewMemStore()
		h := newCreateHarness(t, nil)
		h.reserve(t, "c1")
		plan := workerPlan(cfg, "c1", 1)
		h.runAll(t, &createPass{cfg: cfg, store: aliasPanicStore{Store: store}}, plan)
		assertFailedNoWrite(t, h)
		h.assertRefused(t, plan, createStagePanic)
		if rows := sessionRows(t, store); len(rows) != 0 {
			t.Fatalf("rows = %+v, want none", rows)
		}
	})
	t.Run("in the lock", func(t *testing.T) {
		h := newCreateHarness(t, func(host *createEffectHost) {
			host.withLocks = func(string, []string, func() error) error { panic("lock dir vanished") }
		})
		h.reserve(t, "c1")
		h.runAll(t, &createPass{cfg: cfg, store: beads.NewMemStore()}, workerPlan(cfg, "c1", 1))
		assertFailedNoWrite(t, h)
	})
	t.Run("after the write", func(t *testing.T) {
		store := panicAfterCreateStore{newPrefixedMemStore()}
		h := newCreateHarness(t, nil)
		token := h.reserve(t, "c1")
		h.runAll(t, &createPass{cfg: cfg, store: store}, workerPlan(cfg, "c1", 1))
		rowID := "gc-session-" + token
		if e := h.entry(t); !e.Ambiguous || e.RowID != rowID || e.Token != token {
			t.Fatalf("settlement %+v, want ambiguous with row %s", e, rowID)
		}
		if _, err := store.Get(rowID); err != nil {
			t.Fatalf("the row the panicking create wrote: %v", err)
		}
		h.assertNoRefusal(t)
	})
}

// aliasPanicStore panics on an alias-keyed List: the locked alias check,
// which runs on the effect's goroutine.
type aliasPanicStore struct{ beads.Store }

func (s aliasPanicStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if _, ok := q.Metadata["alias"]; ok {
		panic("alias index driver crashed")
	}
	return s.Store.List(q)
}

// panicWriter panics on every write.
type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("stderr closed") }

// Kills: a panicking settle or log sink killing the worker: the executor
// loses a worker, or the process dies.
func TestCreateEffect_PanickingSinksStillSettle(t *testing.T) {
	cfg := workerCity(20)
	panicked := false
	h := newCreateHarness(t, func(host *createEffectHost) {
		record := host.settle
		host.settle = func(s createSettlement) {
			record(s)
			if s.EntryID == "doomed" {
				panicked = true
				panic("settlement queue closed")
			}
		}
		host.stderr = panicWriter{}
	})
	h.reserve(t, "ok")
	h.reserve(t, "doomed")
	pass := &createPass{cfg: cfg, store: beads.NewMemStore()}
	h.runAll(t, pass, workerPlan(cfg, "ok", 1), createPlan{EntryID: "doomed", Template: "ghost", QualifiedInstance: "ghost-1", Slot: 1})
	if e := h.settlementOf(t, "ok"); !e.Landed {
		t.Fatalf("ok: %+v, want landed", e)
	}
	if e := h.settlementOf(t, "doomed"); e.Landed || !panicked {
		t.Fatalf("doomed: %+v (sink panicked %v), want failed through a panicking sink", e, panicked)
	}
	h.reserve(t, "next")
	h.runAll(t, pass, workerPlan(cfg, "next", 2))
	if e := h.settlementOf(t, "next"); !e.Landed {
		t.Fatalf("next: %+v, want landed by a live executor", e)
	}
}

// Kills: a worker that exits without giving its slot back, so a later
// submit, once every slot was used, starts no worker and its plan never runs.
func TestCreateEffect_WorkersRestartAfterTheQueueDrains(t *testing.T) {
	cfg := workerCity(30)
	store := beads.NewMemStore()
	h := newCreateHarness(t, nil)
	pass := &createPass{cfg: cfg, store: store}
	for round := 0; round < 2; round++ {
		var plans []createPlan
		for i := 1; i <= createEffectParallelism; i++ {
			id := fmt.Sprintf("r%d-%d", round, i)
			h.reserve(t, id)
			plans = append(plans, workerPlan(cfg, id, round*createEffectParallelism+i))
		}
		h.runAll(t, pass, plans...)
		for _, p := range plans {
			if e := h.settlementOf(t, p.EntryID); !e.Landed {
				t.Fatalf("round %d: %s = %+v, want landed", round, p.EntryID, e)
			}
		}
	}
}

// Kills: the effect's transport validation dropped (C7.2's view keeps
// legacy's capability check): a create the provider cannot carry is written
// and fails only at start.
func TestCreateEffect_ValidatesTransportWithLookPath(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Provider: "opencode"},
		Session:   config.SessionConfig{Provider: config.SessionTransportACP},
		Providers: map[string]config.ProviderSpec{"opencode": {Command: "echo", ACPCommand: "echo", PromptMode: "none", SupportsACP: boolPtr(true)}},
		Agents:    []config.Agent{{Name: "worker", Provider: "opencode", Session: config.SessionTransportTmux, MaxActiveSessions: intPtr(2)}},
	}
	store := beads.NewMemStore()
	h := newCreateHarness(t, func(host *createEffectHost) {
		host.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	})
	h.reserve(t, "c1")
	plan := workerPlan(cfg, "c1", 1)
	h.runAll(t, &createPass{cfg: cfg, sp: &acpOnlyDesiredStateProvider{Fake: runtime.NewFake()}, store: store}, plan)
	assertFailedNoWrite(t, h)
	h.assertRefused(t, plan, createStagePrepare)
	if rows := sessionRows(t, store); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none for an unsupported transport", rows)
	}
}

// Kills: identifier locks taken without the city path, which fence this
// process only (C7.1 tier 2), and an executor whose outcomes reach no one.
// The executor refuses a host without either, and the effect hands its city
// path to the real flock, which fails closed.
func TestCreateEffect_LocksFailClosedWithoutTheCityPath(t *testing.T) {
	for name, host := range map[string]createEffectHost{
		"no city path": {settle: func(createSettlement) {}},
		"no settle":    {cityPath: t.TempDir()},
	} {
		if _, err := newCreateEffects(host); err == nil {
			t.Errorf("%s: newCreateEffects accepted the host", name)
		}
	}

	cityPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cityPath, []byte("blocks the lock dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := workerCity(2)
	store := beads.NewMemStore()
	h := newCreateHarness(t, func(host *createEffectHost) {
		host.cityPath = cityPath
		host.withLocks = nil // session.WithCitySessionIdentifierLocks
	})
	h.reserve(t, "c1")
	plan := workerPlan(cfg, "c1", 1)
	h.runAll(t, &createPass{cfg: cfg, store: store}, plan)
	assertFailedNoWrite(t, h)
	h.assertRefused(t, plan, createStageLock)
	if rows := sessionRows(t, store); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none without the city flock", rows)
	}
}

// Kills: the suspended rigs dropped from the effect's re-census: a suspended
// rig's leg is read, and its failure refuses every create.
func TestCreateEffect_ReCensusSkipsSuspendedRigs(t *testing.T) {
	rigPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig", Path: rigPath}},
		Agents:    []config.Agent{{Name: "worker", Dir: "rig", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
	}
	for name, suspended := range map[string]map[string]bool{"suspended": {filepath.Clean(rigPath): true}, "serving": nil} {
		t.Run(name, func(t *testing.T) {
			h := newCreateHarness(t, nil)
			h.reserve(t, "c1")
			rig := &toggleListFailStore{Store: beads.NewMemStore(), fail: true}
			h.runAll(t, &createPass{cfg: cfg, store: beads.NewMemStore(), rigStores: map[string]beads.Store{"rig": rig}, suspendedRigPaths: suspended}, workerPlan(cfg, "c1", 1))
			if e := h.entry(t); e.Landed != (suspended != nil) {
				t.Fatalf("entry %+v: a %s rig's failing leg decided the create wrongly", e, name)
			}
		})
	}
}

// Kills: a plan whose slot and pool slot disagree created with legacy's slot
// (they agree only because the planner and the effect derive both from one
// function, poolDesiredRequestIdentity), or a plan for an identity config no
// longer has.
func TestCreateEffect_RefusesAPlanConfigDoesNotDerive(t *testing.T) {
	cfg := workerCity(1) // canonical singleton: instance "worker", pool slot 0
	for name, plan := range map[string]createPlan{
		"singleton with a slot":    {EntryID: "c1", Template: "worker", QualifiedInstance: "worker", Slot: 1},
		"instance of another slot": {EntryID: "c1", Template: "worker", QualifiedInstance: "worker-2", Slot: 0},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			h := newCreateHarness(t, nil)
			h.reserve(t, "c1")
			h.runAll(t, &createPass{cfg: cfg, store: store}, plan)
			assertFailedNoWrite(t, h)
			h.assertRefused(t, plan, createStageStalePlan)
			if rows := sessionRows(t, store); len(rows) != 0 {
				t.Fatalf("rows = %+v, want none", rows)
			}
		})
	}
}

// Kills: concurrent effects for one slot identity under real city flocks
// minting two rows.
func TestCreateEffect_ConcurrentSameIdentityRealFlocksOneRow(t *testing.T) {
	for i := 0; i < 10; i++ {
		cfg := workerCity(3)
		store := beads.NewMemStore()
		h := newCreateHarness(t, func(host *createEffectHost) { host.withLocks = nil })
		var plans []createPlan
		for j := 0; j < 4; j++ {
			id := fmt.Sprintf("c%d", j)
			h.reserve(t, id)
			plans = append(plans, workerPlan(cfg, id, 1))
		}
		h.runAll(t, &createPass{cfg: cfg, store: store}, plans...)
		committed := 0
		for _, e := range h.settlements() {
			if e.Landed {
				committed++
			}
		}
		if rows := sessionRows(t, store); len(rows) != 1 || committed != 1 {
			t.Fatalf("iteration %d: rows=%d committed=%d, want exactly one", i, len(rows), committed)
		}
	}
}

// Kills: shared state in parallel effects across templates under real flocks
// and transport validation (run under -race).
func TestCreateEffect_ParallelTemplatesRealFlocks(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(20)},
			{Name: "crew", StartCommand: "true", MaxActiveSessions: intPtr(20), NamepoolNames: []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		},
	}
	store := beads.NewMemStore()
	h := newCreateHarness(t, func(host *createEffectHost) {
		host.withLocks = nil
		host.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	})
	var plans []createPlan
	for j := 1; j <= 8; j++ {
		for ai := range cfg.Agents {
			id := fmt.Sprintf("c-%d-%d", ai, j)
			h.reserve(t, id)
			_, qi, ps := poolDesiredRequestIdentity(&cfg.Agents[ai], j)
			plans = append(plans, createPlan{EntryID: id, Template: cfg.Agents[ai].QualifiedName(), QualifiedInstance: qi, Slot: ps})
		}
	}
	pass := &createPass{cfg: cfg, store: store}
	var wg sync.WaitGroup
	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			h.submit(pass, plans[k*4:(k+1)*4]...)
		}(k)
	}
	wg.Wait()
	h.x.wg.Wait()
	if rows := sessionRows(t, store); len(rows) != len(plans) {
		t.Fatalf("rows = %d, want %d", len(rows), len(plans))
	}
}

// Kills an effect goroutine mutating shared planner state (C1.11): the host
// holds no planner-owned table, and every outcome, a panic included, reaches
// the planner as exactly one settlement through the settle spy.
func TestCreateEffectSettlesOnlyByMessage(t *testing.T) {
	shared := map[reflect.Type]bool{
		reflect.TypeFor[*intentLedger](): true, reflect.TypeFor[*backoffTable](): true,
		reflect.TypeFor[*inflightMap](): true, reflect.TypeFor[v2Enqueuer](): true,
	}
	host := reflect.TypeFor[createEffectHost]()
	for i := range host.NumField() {
		if f := host.Field(i); shared[f.Type] {
			t.Errorf("createEffectHost.%s is planner state (%v): effects report by settlement only", f.Name, f.Type)
		}
	}

	cfg := workerCity(3)
	cfg.Agents[0].TmuxAlias = "crew"
	doomed := beads.NewMemStore()
	closedNameOwner(t, doomed, "crew")
	h := newCreateHarness(t, nil)
	for _, id := range []string{"landed", "refused", "panicked"} {
		h.reserve(t, id)
	}
	h.runAll(t, &createPass{cfg: workerCity(3), store: beads.NewMemStore()}, workerPlan(cfg, "landed", 1))
	h.runAll(t, &createPass{cfg: cfg, store: doomed}, workerPlan(cfg, "refused", 1))
	h.runAll(t, &createPass{cfg: cfg, store: aliasPanicStore{Store: beads.NewMemStore()}}, workerPlan(cfg, "panicked", 2))
	if got := len(h.settlements()); got != 3 {
		t.Fatalf("settlements = %d, want one per effect", got)
	}
	if s := h.settlementOf(t, "landed"); !s.Landed || s.Err != nil {
		t.Fatalf("landed: %+v", s)
	}
	if s := h.settlementOf(t, "refused"); s.Stage != createStageFence || s.Err == nil {
		t.Fatalf("refused: %+v, want a fence refusal", s)
	}
	if s := h.settlementOf(t, "panicked"); s.Stage != createStagePanic {
		t.Fatalf("panicked: %+v, want a panic refusal", s)
	}
}

// Kills a create that still takes its token from anywhere but the plan the
// planner minted it into (S-8): the row and the settlement carry the plan's
// token, and a plan without one writes nothing rather than minting a token
// its entry's marker would never match.
func TestCreateEffectUsesPlanToken(t *testing.T) {
	cfg := workerCity(2)
	store := beads.NewMemStore()
	h := newCreateHarness(t, nil)
	plan := workerPlan(cfg, "c1", 1)
	plan.Token, plan.ConfigRev = session.NewInstanceToken(), "rev-plan"
	h.runAll(t, &createPass{cfg: cfg, store: store}, plan)
	rows := sessionRows(t, store)
	if s := h.entry(t); !s.Landed || s.Token != plan.Token || len(rows) != 1 || rows[0].InstanceToken != plan.Token || rows[0].ID != s.RowID {
		t.Fatalf("settlement %+v rows %+v, want the row written with the plan's token %q", s, rows, plan.Token)
	}

	tokenless := beads.NewMemStore()
	bare := newCreateHarness(t, nil)
	bare.runAll(t, &createPass{cfg: cfg, store: tokenless}, workerPlan(cfg, "c1", 1))
	if s := bare.entry(t); s.Landed || s.Ambiguous || s.Err == nil {
		t.Fatalf("settlement %+v, want a failure without a row", s)
	}
	if rows := sessionRows(t, tokenless); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none for a plan without a token", rows)
	}
}

// Kills a fence/fence-read misclassification (F3, allocator_create.go's
// settle): fence moves the planner's request to the next slot, fence-read
// stalls it. Only a pool create whose locked checks proved its name taken is
// fence; a locked read failure is fence-read; a named create's locked
// failure is always fence.
func TestCreateSettlementFenceVersusFenceRead(t *testing.T) {
	cfg := workerCity(60)
	cfg.Agents[0].TmuxAlias = "crew"
	taken := beads.NewMemStore()
	closedNameOwner(t, taken, "crew")
	squatted := beads.NewMemStore()
	aliasSquatter(t, squatted)
	for name, tc := range map[string]struct {
		cfg   *config.City
		store beads.Store
		plan  func(*config.City) createPlan
		want  string
	}{
		"pool name taken":         {cfg, taken, func(c *config.City) createPlan { return workerPlan(c, "c1", 1) }, createStageFence},
		"pool re-census error":    {cfg, &firstListFailStore{Store: beads.NewMemStore()}, func(c *config.City) createPlan { return workerPlan(c, "c1", 1) }, createStageFenceRead},
		"pool store refused":      {cfg, refusingCreateStore{Store: beads.NewMemStore(), err: beads.ErrStoreClosed}, func(c *config.City) createPlan { return workerPlan(c, "c1", 1) }, createStageFenceRead},
		"named alias held":        {mayorCity(), squatted, func(c *config.City) createPlan { return namedPlan(t, c, "c1", "mayor") }, createStageFence},
		"named identity read err": {mayorCity(), namedListFailStore{Store: beads.NewMemStore()}, func(c *config.City) createPlan { return namedPlan(t, c, "c1", "mayor") }, createStageFence},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCreateHarness(t, nil)
			h.reserve(t, "c1")
			plan := tc.plan(tc.cfg)
			h.runAll(t, &createPass{cfg: tc.cfg, store: tc.store}, plan)
			assertFailedNoWrite(t, h)
			h.assertRefused(t, plan, tc.want)
		})
	}
}

// partialLegStore answers every List with its rows and a partial-result
// error: some entries on the leg could not be read.
type partialLegStore struct{ beads.Store }

func (s partialLegStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(q)
	if err != nil {
		return rows, err
	}
	return rows, &beads.PartialResultError{Op: "bd list", Err: errors.New("1 entry failed to parse")}
}

// Kills a create that trusts a partial leg (C7.2; B1's owed confirmation):
// the census keeps a partial leg's rows, but the create effect's locked live
// re-census fails closed on any partial leg, writing nothing (fence-read),
// for a pool create on a rig leg and a named create on the sessions leg.
// The control lands with the same stores read completely.
func TestCreateEffectFenceFailsClosedOnPartialLeg(t *testing.T) {
	rigCfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig", Path: t.TempDir()}},
		Agents:    []config.Agent{{Name: "worker", Dir: "rig", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
	}
	for _, partial := range []bool{true, false} {
		rig := beads.Store(beads.NewMemStore())
		if partial {
			rig = partialLegStore{Store: rig}
		}
		store := beads.NewMemStore()
		h := newCreateHarness(t, nil)
		h.reserve(t, "c1")
		plan := workerPlan(rigCfg, "c1", 1)
		h.runAll(t, &createPass{cfg: rigCfg, store: store, rigStores: map[string]beads.Store{"rig": rig}}, plan)
		if !partial {
			if s := h.entry(t); !s.Landed {
				t.Fatalf("control: settlement %+v, want landed", s)
			}
			continue
		}
		assertFailedNoWrite(t, h)
		h.assertRefused(t, plan, createStageFenceRead)
		if rows := sessionRows(t, store); len(rows) != 0 {
			t.Fatalf("rows = %+v, want none past a partial rig leg", rows)
		}
	}

	cfg := mayorCity()
	mem := beads.NewMemStore()
	h := newNamedHarness(t, t.TempDir(), nil)
	h.reserve(t, "c1")
	plan := namedPlan(t, cfg, "c1", "mayor")
	h.runAll(t, &createPass{cfg: cfg, store: partialLegStore{Store: mem}}, plan)
	assertFailedNoWrite(t, h)
	if all, err := mem.List(beads.ListQuery{AllowScan: true, IncludeClosed: true}); err != nil || len(all) != 0 {
		t.Fatalf("rows = %+v (%v), want none past a partial sessions leg", all, err)
	}
}
