package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// Create-effect tests run effects against MemStores with a real intent
// ledger. Each test reserves the create entry the way the allocator does,
// submits the plan, waits for the executor to drain (wg), then reads the
// ledger's verdict and the stores. The ledger and the effects read time.Now,
// so tests under synctest move both with advance.

// createHarness is one executor with a recorded enqueue.
type createHarness struct {
	ledger  *intentLedger
	backoff *backoffTable
	x       *createEffects

	mu       sync.Mutex
	enqueued [][]reconcilekey.Key
}

func newCreateHarness(t *testing.T, edit func(*createEffectHost)) *createHarness {
	t.Helper()
	h := &createHarness{ledger: newIntentLedger(time.Now), backoff: newBackoffTable()}
	host := createEffectHost{
		cityPath: t.TempDir(),
		cityName: "test-city",
		ledger:   h.ledger,
		backoff:  h.backoff,
		enqueue: func(_ string, keys ...reconcilekey.Key) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.enqueued = append(h.enqueued, keys)
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

// createHarnessRev is the ConfigRev every harness entry is reserved under.
const createHarnessRev = "rev-1"

// reserve records create entry id as the allocator would, and returns its
// pre-minted token.
func (h *createHarness) reserve(t *testing.T, id string) string {
	t.Helper()
	token := session.NewInstanceToken()
	if !h.ledger.Reserve(ledgerEntry{
		ID: id, Kind: kindCreate, Key: rowKey{Leg: "sessions"}, ConfigRev: createHarnessRev,
		ReservedAt: time.Now(), Marker: ledgerMarker{InstanceToken: token},
	}) {
		t.Fatalf("reserve %s refused", id)
	}
	return token
}

// runAll submits plans and waits until every effect returned.
func (h *createHarness) runAll(t *testing.T, pass *createPass, plans ...createPlan) {
	t.Helper()
	if !h.x.submit(pass, plans...) {
		t.Fatal("submit refused")
	}
	h.x.wg.Wait()
}

// entry returns create entry c1, the entry of every single-plan test.
func (h *createHarness) entry(t *testing.T) ledgerEntry {
	t.Helper()
	e, ok := ledgerEntryOf(h.ledger, "c1")
	if !ok {
		t.Fatal("entry c1 left the ledger")
	}
	return e
}

// createBackoffs returns the table's create and named records, by key.
func (h *createHarness) createBackoffs() map[string]backoffRecord {
	out := make(map[string]backoffRecord)
	for k, r := range h.backoff.Snapshot() {
		if !strings.HasPrefix(k, "work:") {
			out[k] = r
		}
	}
	return out
}

// assertNoCreateBackoff fails if any create backoff was recorded.
func (h *createHarness) assertNoCreateBackoff(t *testing.T) {
	t.Helper()
	if got := h.createBackoffs(); len(got) != 0 {
		t.Fatalf("create backoffs = %+v, want none", got)
	}
}

// assertCreateBackoff checks that plan's identity holds a create backoff
// live on the effect's clock, with cause, under createHarnessRev, and
// returns it.
func (h *createHarness) assertCreateBackoff(t *testing.T, plan createPlan, cause string) backoffRecord {
	t.Helper()
	key := createBackoffKey(plan.identity().key())
	r, ok := h.backoff.Snapshot()[key]
	if !ok || !r.live(h.x.host.now()) || r.Cause != cause || r.Fingerprint != createHarnessRev {
		t.Fatalf("create backoff for %q = %+v (present %v), want live with cause %q", key, r, ok, cause)
	}
	return r
}

// refusesWork reports whether the table holds a live work record for
// spec's evidence.
func (h *createHarness) refusesWork(spec worktree.Spec) bool {
	r, ok := h.backoff.Snapshot()[workBackoffKey(spec.BeadID)]
	return ok && r.live(h.x.host.now()) && r.Fingerprint == specFingerprint(spec) && r.Cause == createStageWorktree
}

func (h *createHarness) enqueues() [][]reconcilekey.Key {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]reconcilekey.Key(nil), h.enqueued...)
}

// assertFailedNoWrite checks C5.4(2) for create entry c1: a create that
// wrote nothing clears at the next pass, whatever the census.
func assertFailedNoWrite(t *testing.T, h *createHarness) {
	t.Helper()
	e := h.entry(t)
	if e.State != ledgerFailed || e.WroteRow {
		t.Fatalf("entry c1 = state %d wroteRow %v, want failed without a row", e.State, e.WroteRow)
	}
	if got := e.clearVerdict(ledgerCensus{}, time.Now()); got != clearUnwritten {
		t.Fatalf("entry c1 clear verdict = %d, want clearUnwritten", got)
	}
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
	if got := h.enqueues(); len(got) != 1 || !reflect.DeepEqual(got[0], []reconcilekey.Key{reconcilekey.Allocator()}) {
		t.Fatalf("enqueues = %v, want one allocator wake", got)
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
		if e.State != ledgerCommitted || e.Marker.RowID == "" {
			t.Fatalf("entry = %+v, want committed with the new row", e)
		}
		var created session.Info
		for _, row := range sessionRows(t, store) {
			if row.ID == e.Marker.RowID {
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

	if e := h.entry(t); e.State != ledgerCommitted || e.Marker.RowID == "" {
		t.Fatalf("entry = %+v, want committed without a provider probe", e)
	}
	if rows := sessionRows(t, store); len(rows) != 1 || rows[0].AgentName != "worker" {
		t.Fatalf("rows = %+v, want the canonical singleton row", rows)
	}
}

// Kills: starting into an unverified work dir; retrying a bad work item
// forever (#34, POOL-055, C6.5a). Evidence that fails verification writes
// nothing and records a work backoff that a bead's new evidence escapes.
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
	if !h.refusesWork(spec) {
		t.Fatal("no work backoff refuses the evidence that failed")
	}
	next := spec
	next.Generation = "g2"
	if h.refusesWork(next) {
		t.Fatal("the work backoff refuses a new generation of the bead's evidence")
	}
	// The work item is throttled, not the slot: no create backoff.
	h.assertNoCreateBackoff(t)
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
	stale := spec
	stale.Generation = "old"
	h.backoff.Refuse(workBackoffKey(stale.BeadID), time.Now(), time.Time{}, createStageWorktree, specFingerprint(stale))
	h.reserve(t, "c1")

	h.runAll(t, &createPass{cfg: cfg, store: store}, plan)

	if _, ok := h.backoff.Snapshot()[workBackoffKey(stale.BeadID)]; ok {
		t.Fatal("a successful verify left the bead's work backoff standing")
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

// Kills: clearing an unproven create (C5.4, C5.15). A landed create commits
// with its row and pre-minted token; a failure before the write fails with
// no row; an error from the write itself is ambiguous and commits with the
// token as its marker, so it clears only through the census: by its marker,
// or by a sessions-leg read started after it settled (C5.4(3)).
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
		if len(rows) != 1 || e.State != ledgerCommitted || !e.WroteRow ||
			e.Marker != (ledgerMarker{RowID: rows[0].ID, InstanceToken: token}) || e.Key.ID != rows[0].ID {
			t.Fatalf("entry = %+v rows = %+v, want committed with the row and the pre-minted token", e, rows)
		}
		if rows[0].InstanceToken != token {
			t.Fatalf("row token = %q, want the entry's pre-minted %q", rows[0].InstanceToken, token)
		}
		raw, err := store.Get(rows[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := raw.Metadata["pending_create_started_at"]; got != "2026-10-03T11:00:00Z" {
			t.Fatalf("pending_create_started_at = %q, want the effect's clock in UTC", got)
		}
		if e.Ambiguous || e.clearVerdict(startedCensus(nil, e.SettledAt.Add(time.Second)), time.Now()) != clearKeep {
			t.Fatal("a committed create cleared before the census showed its row")
		}
		census := ledgerCensusOf(map[rowKey]ledgerRow{{Leg: "sessions", ID: rows[0].ID}: {InstanceToken: token}})
		if got := e.clearVerdict(census, time.Now()); got != clearWritten {
			t.Fatalf("clear verdict with the row = %d, want clearWritten", got)
		}
		want := [][]reconcilekey.Key{{reconcilekey.Allocator(), reconcilekey.Session(rows[0].ID)}}
		if got := h.enqueues(); !reflect.DeepEqual(got, want) {
			t.Fatalf("enqueues = %v, want %v", got, want)
		}
	})

	t.Run("failed before the write", func(t *testing.T) {
		store := beads.NewMemStore()
		h := newCreateHarness(t, nil)
		h.reserve(t, "c1")
		plan := createPlan{EntryID: "c1", Template: "ghost", QualifiedInstance: "ghost-1", Slot: 1}

		h.runAll(t, &createPass{cfg: cfg, store: store}, plan)

		assertFailedNoWrite(t, h)
		h.assertCreateBackoff(t, plan, createStageStalePlan)
	})

	t.Run("ambiguous write", func(t *testing.T) {
		h := newCreateHarness(t, nil)
		token := h.reserve(t, "c1")

		h.runAll(t, &createPass{cfg: cfg, store: failingCreateStore{Store: beads.NewMemStore()}}, workerPlan(cfg, "c1", 1))

		e := h.entry(t)
		if e.State != ledgerCommitted || !e.WroteRow || !e.Ambiguous || e.Marker != (ledgerMarker{InstanceToken: token}) {
			t.Fatalf("entry = %+v, want committed as ambiguous with only the token as its marker", e)
		}
		if got := e.clearVerdict(startedCensus(nil, e.SettledAt), time.Now()); got != clearKeep {
			t.Fatalf("an ambiguous create cleared by a read that started at its settle: %d", got)
		}
		if got := e.clearVerdict(startedCensus(nil, e.SettledAt.Add(time.Second)), time.Now()); got != clearUnwritten {
			t.Fatalf("clear verdict after a later read without the token = %d, want clearUnwritten", got)
		}
		census := ledgerCensusOf(map[rowKey]ledgerRow{{Leg: "sessions", ID: "gc-late"}: {InstanceToken: token}})
		if got := e.clearVerdict(census, time.Now()); got != clearWritten {
			t.Fatalf("clear verdict with the token's row = %d, want clearWritten", got)
		}
		if got := h.enqueues(); !reflect.DeepEqual(got, [][]reconcilekey.Key{{reconcilekey.Allocator()}}) {
			t.Fatalf("enqueues = %v, want one allocator wake", got)
		}
		h.assertNoCreateBackoff(t)
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
			h.assertCreateBackoff(t, plan, createStageFenceRead)
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
			h.assertCreateBackoff(t, plan, createStageFenceRead)
			if rows := sessionRows(t, mem); len(rows) != 0 {
				t.Fatalf("rows = %+v, want none", rows)
			}
		})
	}
}

// Kills: running an effect whose entry the allocator released first (C5.1):
// the lost CAS means no effect and no write.
func TestCreateEffect_ReleasedEntryRunsNoEffect(t *testing.T) {
	store := beads.NewMemStore()
	cfg := workerCity(2)
	h := newCreateHarness(t, nil)
	h.reserve(t, "c1")
	if _, ok := h.ledger.Release("c1"); !ok {
		t.Fatal("release refused")
	}

	h.runAll(t, &createPass{cfg: cfg, store: store}, workerPlan(cfg, "c1", 1))

	if rows := sessionRows(t, store); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none for a released entry", rows)
	}
	if got := h.enqueues(); len(got) != 0 {
		t.Fatalf("enqueues = %v, want none", got)
	}
	h.assertNoCreateBackoff(t)
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

		h.x.submit(pass, plans[:5]...)
		h.x.submit(pass, plans[5:]...)
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
		for _, e := range h.ledger.View() {
			if e.State != ledgerCommitted {
				t.Fatalf("entry %s state %d, want every create committed", e.ID, e.State)
			}
		}
	})
}

// Kills: leaked effect goroutines, and plans run after shutdown began (C1.8).
// Shutdown stops admission, leaves queued plans reserved, and waits for the
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
		h.x.submit(pass, plans...)
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
		if h.x.submit(pass, workerPlan(cfg, "late", 20)) {
			t.Fatal("submit accepted a plan after shutdown began")
		}

		close(gate.release)
		if err := <-done; err != nil {
			t.Fatalf("shutdown = %v, want nil once effects finished", err)
		}
		states := map[ledgerState]int{}
		for _, e := range h.ledger.View() {
			states[e.State]++
		}
		if states[ledgerCommitted] != createEffectParallelism || states[ledgerReserved] != 11-createEffectParallelism {
			t.Fatalf("entry states = %v, want %d committed and the rest still reserved", states, createEffectParallelism)
		}
		if rows := sessionRows(t, store); len(rows) != createEffectParallelism {
			t.Fatalf("rows = %d, want %d", len(rows), createEffectParallelism)
		}
		h.assertNoCreateBackoff(t) // plans dropped at shutdown were never issued
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

// Kills (AM-N8, S4): a doomed create re-planned at pass rate; a backoff that
// does not double, is uncapped, clears early or never; a commit that leaves
// the backoff; a backoff on a lost issue CAS or on an ambiguous write; a
// failed entry visible to a pass (ledger read before the table) without its
// backoff; a backoff that survives a ConfigRev change.
func TestCreateEffect_NoWriteFailureBacksOffIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := workerCity(60)
		cfg.Agents[0].TmuxAlias = "crew"
		doomed := beads.NewMemStore()
		closedNameOwner(t, doomed, "crew")
		h := newCreateHarness(t, nil)
		attempt := func(id string, store beads.Store) createPlan {
			t.Helper()
			h.reserve(t, id)
			plan := workerPlan(cfg, id, 1)
			h.runAll(t, &createPass{cfg: cfg, store: store}, plan)
			return plan
		}

		for i, want := range []time.Duration{10, 20, 40, 80, 160, 300, 300} {
			plan := attempt(fmt.Sprintf("c%d", i), doomed)
			if e, _ := ledgerEntryOf(h.ledger, plan.EntryID); e.State != ledgerFailed || e.WroteRow {
				t.Fatalf("attempt %d: entry %+v, want failed without a row", i+1, e)
			}
			v := h.assertCreateBackoff(t, plan, createStageFence)
			if got := time.Until(v.Until); got != want*time.Second || v.Consecutive != i+1 {
				t.Fatalf("attempt %d: backoff for %v (consecutive %d), want %v (%d)", i+1, got, v.Consecutive, want*time.Second, i+1)
			}
			key := createBackoffKey(plan.identity().key())
			advance(time.Until(v.Until) - time.Nanosecond)
			if !h.backoff.Snapshot()[key].live(time.Now()) {
				t.Fatalf("attempt %d: backoff cleared before Until", i+1)
			}
			advance(time.Nanosecond)
			if h.backoff.Snapshot()[key].live(time.Now()) {
				t.Fatalf("attempt %d: backoff still live at Until", i+1)
			}
		}

		// A commit for the identity resets its backoff.
		attempt("ok", beads.NewMemStore())
		h.assertNoCreateBackoff(t)
		plan := attempt("again", doomed)
		if v := h.assertCreateBackoff(t, plan, createStageFence); time.Until(v.Until) != backoffBase || v.Consecutive != 1 {
			t.Fatalf("after a commit: backoff %+v, want 10s and consecutive 1", v)
		}

		// A ConfigRev change drops the backoff.
		h.backoff.Prune(createHarnessRev, ledgerCensus{}, nil)
		h.assertCreateBackoff(t, plan, createStageFence)
		h.backoff.Prune("rev-2", ledgerCensus{}, nil)
		h.assertNoCreateBackoff(t)

		// A lost issue CAS and an ambiguous write record nothing.
		h.reserve(t, "released")
		if _, ok := h.ledger.Release("released"); !ok {
			t.Fatal("release refused")
		}
		h.runAll(t, &createPass{cfg: cfg, store: doomed}, workerPlan(cfg, "released", 1))
		attempt("ambiguous", failingCreateStore{Store: beads.NewMemStore()})
		h.assertNoCreateBackoff(t)

		// Every failed create a pass can see carries its backoff: 40 doomed
		// effects fail in parallel while a pass reads the ledger. Slot i's
		// runtime name is crew-i.
		var plans []createPlan
		for i := 1; i <= 40; i++ {
			if i > 1 {
				closedNameOwner(t, doomed, fmt.Sprintf("crew-%d", i))
			}
			id := fmt.Sprintf("p%02d", i)
			h.reserve(t, id)
			plans = append(plans, workerPlan(cfg, id, i))
		}
		identities := make(map[string]string, len(plans))
		for _, p := range plans {
			identities[p.EntryID] = createBackoffKey(p.identity().key())
		}
		stop, observed := make(chan struct{}), make(chan error, 1)
		go func() {
			for {
				entries := h.ledger.View()
				recs := h.backoff.Snapshot()
				for _, e := range entries {
					if key, ok := identities[e.ID]; ok && e.State == ledgerFailed && !recs[key].live(time.Now()) {
						observed <- fmt.Errorf("entry %s failed without its backoff", e.ID)
						return
					}
				}
				select {
				case <-stop:
					observed <- nil
					return
				default:
				}
			}
		}()
		h.runAll(t, &createPass{cfg: cfg, store: doomed}, plans...)
		close(stop)
		if err := <-observed; err != nil {
			t.Fatal(err)
		}
		if got := len(h.createBackoffs()); got != len(plans) {
			t.Fatalf("create backoffs = %d, want one per doomed identity (%d)", got, len(plans))
		}
	})
}

// Kills: the plan's own reservation fencing its create (C7.2). The ledger
// holds the entry's reservation and the pass hands the census rows, so the
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
	if e.State != ledgerCommitted || len(rows) != 1 || rows[0].Alias != plan.QualifiedInstance {
		t.Fatalf("entry %+v rows %+v, want committed with alias %q", e, rows, plan.QualifiedInstance)
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
		want := ledgerMarker{RowID: "gc-session-" + token, InstanceToken: token}
		if e := h.entry(t); e.State != ledgerCommitted || e.Marker != want {
			t.Fatalf("entry %+v, want committed with marker %+v", e, want)
		}
	})
	t.Run("ambiguous write", func(t *testing.T) {
		h := newCreateHarness(t, nil)
		token := h.reserve(t, "c1")
		h.runAll(t, &createPass{cfg: cfg, store: prefixedFailingCreateStore{newPrefixedMemStore()}}, workerPlan(cfg, "c1", 1))
		want := ledgerMarker{RowID: "gc-session-" + token, InstanceToken: token}
		if e := h.entry(t); e.State != ledgerCommitted || !e.WroteRow || e.Marker != want {
			t.Fatalf("entry %+v, want ambiguous commit with marker %+v", e, want)
		}
		h.assertNoCreateBackoff(t)
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
// store that pre-mints no ID) read as no write: a create backoff for a row
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
			want := ledgerMarker{RowID: all[0].ID, InstanceToken: token}
			if e := h.entry(t); e.State != ledgerCommitted || !e.WroteRow || !e.Ambiguous || e.Marker != want {
				t.Fatalf("entry %+v, want ambiguous commit with marker %+v", e, want)
			}
			h.assertNoCreateBackoff(t)
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
	if e := h.entry(t); e.State != ledgerCommitted || !e.Ambiguous {
		t.Fatalf("entry %+v, want an ambiguous commit", e)
	}
	h.assertNoCreateBackoff(t)
}

// Kills (ME7, and a backoff fingerprinted by anything but the entry): a
// no-write failure's create backoff under a fingerprint other than the
// ConfigRev its entry was reserved under. The pass carries no revision, so
// only the entry's can reach the record, and Prune keeps it exactly while
// the pass's revision is the entry's.
func TestCreateEffect_NoWriteBackoffFingerprintIsTheEntryConfigRev(t *testing.T) {
	cfg := workerCity(60)
	cfg.Agents[0].TmuxAlias = "crew"
	doomed := beads.NewMemStore()
	closedNameOwner(t, doomed, "crew")
	h := newCreateHarness(t, nil)
	if !h.ledger.Reserve(ledgerEntry{
		ID: "c1", Kind: kindCreate, Key: rowKey{Leg: "sessions"}, ConfigRev: "rev-entry",
		ReservedAt: time.Now(), Marker: ledgerMarker{InstanceToken: session.NewInstanceToken()},
	}) {
		t.Fatal("reserve refused")
	}
	plan := workerPlan(cfg, "c1", 1)
	h.runAll(t, &createPass{cfg: cfg, store: doomed}, plan)
	key := createBackoffKey(plan.identity().key())
	if r := h.backoff.Snapshot()[key]; r.Fingerprint != "rev-entry" || r.Cause != createStageFence {
		t.Fatalf("create backoff = %+v, want fence under the entry's ConfigRev rev-entry", r)
	}
	h.backoff.Prune("rev-entry", ledgerCensus{}, nil)
	if _, ok := h.backoff.Snapshot()[key]; !ok {
		t.Fatal("prune under the entry's ConfigRev dropped its backoff")
	}
	h.backoff.Prune("rev-next", ledgerCensus{}, nil)
	if _, ok := h.backoff.Snapshot()[key]; ok {
		t.Fatal("the backoff survived a ConfigRev change")
	}
}

// Kills (M19, deterministic): the ledger settling a failed create before its
// backoff is recorded. The ledger reads its clock under its lock at every
// settle, so the hook sees the table as a pass that reads the ledger first
// and the table second sees it once the failed entry is visible.
func TestCreateEffect_BackoffRecordedBeforeTheLedgerSettles(t *testing.T) {
	cfg := workerCity(60)
	cfg.Agents[0].TmuxAlias = "crew"
	doomed := beads.NewMemStore()
	closedNameOwner(t, doomed, "crew")
	h := newCreateHarness(t, nil)
	plan := workerPlan(cfg, "c1", 1)
	key := createBackoffKey(plan.identity().key())
	var atSettle []backoffRecord
	h.ledger.now = func() time.Time {
		atSettle = append(atSettle, h.backoff.Snapshot()[key])
		return time.Now()
	}
	h.reserve(t, "c1")
	h.runAll(t, &createPass{cfg: cfg, store: doomed}, plan)
	if len(atSettle) != 1 || !atSettle[0].live(time.Now()) || atSettle[0].Cause != createStageFence {
		t.Fatalf("create backoff when the ledger settled = %+v, want the fence record already live", atSettle)
	}
}

// Kills: a panic before the row write settled as ambiguous (an entry held
// until a later read for a create that wrote nothing); a panic after it
// settled as no write (a create backoff for a row that exists); the pre-minted row ID
// lost on the panic path; a panic that skips the settle and strands the
// entry issued.
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
		h.assertCreateBackoff(t, plan, createStagePanic)
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
		if e := h.entry(t); e.State != ledgerCommitted || !e.WroteRow || e.Marker != (ledgerMarker{RowID: rowID, InstanceToken: token}) {
			t.Fatalf("entry %+v, want ambiguous commit with row %s", e, rowID)
		}
		if _, err := store.Get(rowID); err != nil {
			t.Fatalf("the row the panicking create wrote: %v", err)
		}
		h.assertNoCreateBackoff(t)
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

// Kills: a panicking wake or log sink killing the worker before or instead
// of settling: the entry stays issued, and the executor loses a worker.
func TestCreateEffect_PanickingSinksStillSettle(t *testing.T) {
	cfg := workerCity(20)
	h := newCreateHarness(t, func(host *createEffectHost) {
		host.enqueue = func(string, ...reconcilekey.Key) { panic("router stopped") }
		host.stderr = panicWriter{}
	})
	h.reserve(t, "ok")
	h.reserve(t, "doomed")
	pass := &createPass{cfg: cfg, store: beads.NewMemStore()}
	h.runAll(t, pass, workerPlan(cfg, "ok", 1), createPlan{EntryID: "doomed", Template: "ghost", QualifiedInstance: "ghost-1", Slot: 1})
	if e, _ := ledgerEntryOf(h.ledger, "ok"); e.State != ledgerCommitted {
		t.Fatalf("ok: %+v, want committed", e)
	}
	if e, _ := ledgerEntryOf(h.ledger, "doomed"); e.State != ledgerFailed {
		t.Fatalf("doomed: %+v, want failed", e)
	}
	h.reserve(t, "next")
	h.runAll(t, pass, workerPlan(cfg, "next", 2))
	if e, _ := ledgerEntryOf(h.ledger, "next"); e.State != ledgerCommitted {
		t.Fatalf("next: %+v, want committed by a live executor", e)
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
			if e, _ := ledgerEntryOf(h.ledger, p.EntryID); e.State != ledgerCommitted {
				t.Fatalf("round %d: %s = %+v, want committed", round, p.EntryID, e)
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
	h.assertCreateBackoff(t, plan, createStagePrepare)
	if rows := sessionRows(t, store); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none for an unsupported transport", rows)
	}
}

// Kills: identifier locks taken without the city path, which fence this
// process only (C7.1 tier 2). The executor refuses a host without one, and
// the effect hands its city path to the real flock, which fails closed.
func TestCreateEffect_LocksFailClosedWithoutTheCityPath(t *testing.T) {
	ledger := newIntentLedger(time.Now)
	for name, host := range map[string]createEffectHost{
		"no city path": {ledger: ledger, backoff: newBackoffTable()},
		"no backoff":   {ledger: ledger, cityPath: t.TempDir()},
		"no ledger":    {backoff: newBackoffTable(), cityPath: t.TempDir()},
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
	h.assertCreateBackoff(t, plan, createStageLock)
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
			if e := h.entry(t); (e.State == ledgerCommitted) != (suspended != nil) {
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
			h.assertCreateBackoff(t, plan, createStageStalePlan)
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
		for _, e := range h.ledger.View() {
			if e.State == ledgerCommitted {
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
			h.x.submit(pass, plans[k*4:(k+1)*4]...)
		}(k)
	}
	wg.Wait()
	h.x.wg.Wait()
	if rows := sessionRows(t, store); len(rows) != len(plans) {
		t.Fatalf("rows = %d, want %d", len(rows), len(plans))
	}
}
