package storeref

import (
	"errors"
	"runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// roleStore is a namedStore whose reads record the leg role on their stack,
// as the split-strict gate does (beads.LegRoleOnStack).
type roleStore struct {
	*namedStore
	roles []beads.LegRole
}

func newRoleStore(name string) *roleStore { return &roleStore{namedStore: newNamedStore(name)} }

func (s *roleStore) record() {
	pcs := make([]uintptr, 64)
	s.roles = append(s.roles, beads.LegRoleOnStack(pcs[:runtime.Callers(1, pcs)]))
}

func (s *roleStore) Get(id string) (beads.Bead, error) {
	s.record()
	return s.namedStore.Get(id)
}

func (s *roleStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.record()
	return s.MemStore.List(q)
}

func (s *roleStore) last(t *testing.T) beads.LegRole {
	t.Helper()
	if len(s.roles) == 0 {
		t.Fatalf("%s saw no read", s.name)
	}
	return s.roles[len(s.roles)-1]
}

// unionOf is a Union plan over legs, each named for its store.
func unionOf(legs ...PlanLeg) ResolvedPlan { return ResolvedPlan{Mode: ModeUnion, Legs: legs} }

func legOver(s *roleStore, role Role, onError ErrPolicy, gate beads.LegRole) PlanLeg {
	return PlanLeg{Leg: Leg{Ref: StoreRef(s.name), Store: s}, Role: role, OnError: onError, Gate: gate}
}

// TestTypedLegsReadThroughTheirGateRole: each typed executor hands its
// visitor a reader minted for the leg's gate role, so a federated leg's read
// carries the federated role, a relic leg's the relic role and a strict leg's
// none; a Sweep's writable handle reads with the role and writes to the leg's
// store.
func TestTypedLegsReadThroughTheirGateRole(t *testing.T) {
	strict, fed, relic := newRoleStore("strict"), newRoleStore("fed"), newRoleStore("relic")
	plan := unionOf(
		legOver(strict, RoleAuthority, PolicyFatal, beads.NoLegRole),
		legOver(fed, RoleFederationTail, PolicyFatal, beads.FederatedLegRole),
		legOver(relic, RoleRelic, PolicyPartialDegrade, beads.RelicLegRole),
	)
	want := map[*roleStore]beads.LegRole{strict: beads.NoLegRole, fed: beads.FederatedLegRole, relic: beads.RelicLegRole}
	byRef := map[StoreRef]*roleStore{"strict": strict, "fed": fed, "relic": relic}
	check := func(executor string, leg TypedLeg) {
		t.Helper()
		s := byRef[leg.Ref]
		if leg.Reader.Ref() != string(leg.Ref) || !beads.SameStore(leg.Reader, s) {
			t.Errorf("%s %s: reader %q is not over its leg's store", executor, leg.Ref, leg.Reader.Ref())
		}
		_, _ = leg.Reader.Get("x")
		if got := s.last(t); got != want[s] {
			t.Errorf("%s %s: a read carried %v, want %v", executor, leg.Ref, got, want[s])
		}
		_, _ = beads.LiveFor(leg.Reader).Get("x")
		if got := s.last(t); got != want[s] {
			t.Errorf("%s %s: a live read carried %v, want %v", executor, leg.Ref, got, want[s])
		}
		// The strict reader reads the same store with no role, whatever
		// the leg's gate role, as an unmarked read does today.
		if leg.Strict.Role() != beads.NoLegRole || leg.Strict.Ref() != string(leg.Ref) || !beads.SameStore(leg.Strict, s) {
			t.Errorf("%s %s: strict reader role %v, ref %q; want none, over the leg's store", executor, leg.Ref, leg.Strict.Role(), leg.Strict.Ref())
		}
		_, _ = leg.Strict.Get("x")
		if got := s.last(t); got != beads.NoLegRole {
			t.Errorf("%s %s: a strict read carried %v, want none", executor, leg.Ref, got)
		}
		_, _ = beads.LiveFor(leg.Strict).Get("x")
		if got := s.last(t); got != beads.NoLegRole {
			t.Errorf("%s %s: a strict live read carried %v, want none", executor, leg.Ref, got)
		}
	}
	EachTypedLeg(plan, func(leg TypedLeg) { check("EachTypedLeg", leg) })
	if _, err := TypedWalk(plan, func(leg TypedLeg) (bool, error) { check("TypedWalk", leg); return false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := TypedUnion(plan, nil, func(leg TypedLeg) ([]int, error) { check("TypedUnion", leg); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := Sweep(plan, func(leg TypedLeg, w beads.ResidentStore) error {
		check("Sweep", leg)
		s := byRef[leg.Ref]
		_, _ = w.List(beads.ListQuery{AllowScan: true})
		if got := s.last(t); got != want[s] {
			t.Errorf("Sweep %s: the writable handle's read carried %v, want %v", leg.Ref, got, want[s])
		}
		if !beads.SameStore(w, s) || w.Ref() != string(leg.Ref) {
			t.Errorf("Sweep %s: the writable handle is not over the leg's store", leg.Ref)
		}
		_, err := w.Create(beads.Bead{ID: "swept-" + s.name, Title: "swept"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range byRef {
		if _, err := s.MemStore.Get("swept-" + s.name); err != nil {
			t.Errorf("Sweep's write did not land on %s: %v", s.name, err)
		}
	}
}

// TestWalkOwnerMintsTheLegItStoppedOn: the handle WalkOwner returns is over
// the leg the visitor proved, not the first leg or the last; no proof is a
// zero handle.
func TestWalkOwnerMintsTheLegItStoppedOn(t *testing.T) {
	a, b, c := newRoleStore("a"), newRoleStore("b"), newRoleStore("c")
	b.seed(t, "ga-1")
	plan := unionOf(
		legOver(a, RoleAuthority, PolicyFatal, beads.FederatedLegRole),
		legOver(b, RoleFederationTail, PolicyFatal, beads.FederatedLegRole),
		legOver(c, RoleFederationTail, PolicyFatal, beads.FederatedLegRole),
	)
	holds := func(leg TypedLeg) (bool, error) {
		_, err := leg.Reader.Get("ga-1")
		if errors.Is(err, beads.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	owner, walked, err := WalkOwner(plan, holds)
	if err != nil {
		t.Fatal(err)
	}
	if !beads.SameStore(owner, b) || owner.Ref() != "b" || walked.StoppedAt != "b" || walked.Visited != 2 {
		t.Errorf("WalkOwner = %q over %s, walked %+v; want b", owner.Ref(), storeNameOf(owner.Raw()), walked)
	}
	if got := owner.Raw(); got != beads.Store(b) {
		t.Errorf("the owner wraps %T, want the leg's store itself (a strict resident)", got)
	}
	none, _, err := WalkOwner(plan, func(TypedLeg) (bool, error) { return false, nil })
	if err != nil || !beads.IsNilStore(none) {
		t.Errorf("no proof: %v, %v; want a zero handle", none.Ref(), err)
	}
	failing := newRoleStore("failing")
	failing.getErr = errors.New("down")
	_, _, err = WalkOwner(unionOf(legOver(failing, RoleAuthority, PolicyFatal, beads.FederatedLegRole)), holds)
	if err == nil {
		t.Error("a fatal leg's failure did not fail WalkOwner")
	}
}

// TestFirstOwnerWalkReadsThroughAFederatedLeg: the by-id walk's probes carry
// the federated role (the executor's own leg, which replaced the
// beads.FederatedGet marker), or the relic role where the plan declared one.
func TestFirstOwnerWalkReadsThroughAFederatedLeg(t *testing.T) {
	for gate, want := range map[beads.LegRole]beads.LegRole{
		beads.NoLegRole:        beads.FederatedLegRole,
		beads.FederatedLegRole: beads.FederatedLegRole,
		beads.RelicLegRole:     beads.RelicLegRole,
	} {
		s := newRoleStore("probe")
		_, _ = ResolveOwnerRow(ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{legOver(s, RoleResidenceProbe, PolicyFatal, gate)}}, "ga-1")
		if got := s.last(t); got != want {
			t.Errorf("gate %v: the probe carried %v, want %v", gate, got, want)
		}
	}
}

// TestPolicyFatalUnlessResolved: a hard error on a FatalUnlessResolved leg is
// remembered and the walk goes on; a later hit wins; with no hit the FIRST
// hard error is returned, never a not-found; a Fatal leg still stops the walk.
func TestPolicyFatalUnlessResolved(t *testing.T) {
	plan := func(legs ...*roleStore) ResolvedPlan {
		p := ResolvedPlan{Mode: ModeFirstOwner}
		for _, s := range legs {
			p.Legs = append(p.Legs, legOver(s, RoleAuthority, PolicyFatalUnlessResolved, beads.FederatedLegRole))
		}
		return p
	}
	graph, work, rig := newRoleStore("graph"), newRoleStore("work"), newRoleStore("rig")
	graph.getErr = errors.New("graph refused")
	work.seed(t, "ga-1")
	owner, err := ResolveOwnerRow(plan(graph, work), "ga-1")
	if err != nil || owner.Ref != "work" || !owner.Read || !beads.SameStore(owner.Resident, work) {
		t.Errorf("a later hit: %+v, %v; want the work leg's row", owner.Ref, err)
	}

	rig.getErr = errors.New("rig down")
	_, err = ResolveOwnerRow(plan(graph, rig, newRoleStore("empty")), "ga-1")
	if err == nil || !errors.Is(err, graph.getErr) || errors.Is(err, beads.ErrNotFound) {
		t.Errorf("no hit: %v, want the first hard error (the graph leg's)", err)
	}

	clean := newRoleStore("clean")
	if _, err := ResolveOwnerRow(plan(clean, newRoleStore("clean2")), "ga-1"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("all clean misses: %v, want not-found", err)
	}

	fatal := ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{legOver(graph, RoleAuthority, PolicyFatal, beads.FederatedLegRole), legOver(work, RoleV1, PolicyFatal, beads.FederatedLegRole)}}
	if _, err := ResolveOwnerRow(fatal, "ga-1"); !errors.Is(err, graph.getErr) {
		t.Errorf("a Fatal leg's error: %v, want it to stop the walk", err)
	}
}

// TestResolveOwnerPreferring: the first leg whose live row the predicate
// accepts wins over an earlier row it rejects; with none accepted the first
// row found wins; a read that fails before an accepted row is returned; no row
// is not-found. Every leg, the unprobed residual included, is read live.
func TestResolveOwnerPreferring(t *testing.T) {
	closed := func(b beads.Bead) bool { return b.Status == "closed" }
	open1, closed2, open3 := newRoleStore("open1"), newRoleStore("closed2"), newRoleStore("open3")
	for _, s := range []*roleStore{open1, closed2, open3} {
		s.seed(t, "ga-1")
	}
	if err := closed2.Close("ga-1"); err != nil {
		t.Fatal(err)
	}
	plan := func(legs ...*roleStore) ResolvedPlan {
		p := ResolvedPlan{Mode: ModeFirstOwner, ID: "ga-1"}
		for i, s := range legs {
			role := RoleResidenceProbe
			if i == len(legs)-1 {
				role = RoleWorkFallback
			}
			p.Legs = append(p.Legs, legOver(s, role, PolicyFatal, beads.NoLegRole))
		}
		return p
	}
	owner, err := ResolveOwnerPreferring(plan(open1, closed2, open3), "ga-1", closed)
	if err != nil || owner.Ref != "closed2" || owner.Bead.Status != "closed" || !beads.SameStore(owner.Resident, closed2) {
		t.Errorf("preferred: %q %q, %v; want the closed copy on closed2", owner.Ref, owner.Bead.Status, err)
	}
	if got := closed2.last(t); got != beads.FederatedLegRole {
		t.Errorf("the live probe carried %v, want federated", got)
	}

	owner, err = ResolveOwnerPreferring(plan(open1, open3), "ga-1", closed)
	if err != nil || owner.Ref != "open1" {
		t.Errorf("none preferred: %q, %v; want the first row (open1)", owner.Ref, err)
	}

	residualOnly := newRoleStore("residual")
	residualOnly.seed(t, "ga-1")
	if err := residualOnly.Close("ga-1"); err != nil {
		t.Fatal(err)
	}
	owner, err = ResolveOwnerPreferring(plan(open1, residualOnly), "ga-1", closed)
	if err != nil || owner.Ref != "residual" || !owner.Read {
		t.Errorf("the residual is read too: %q, %v", owner.Ref, err)
	}

	broken := newRoleStore("broken")
	broken.getErr = errors.New("down")
	if _, err := ResolveOwnerPreferring(plan(open1, broken, closed2), "ga-1", closed); !errors.Is(err, broken.getErr) {
		t.Errorf("a failed read before the preferred row: %v, want it returned", err)
	}
	if _, err := ResolveOwnerPreferring(plan(newRoleStore("e1"), newRoleStore("e2")), "ga-1", closed); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("no row: %v, want not-found", err)
	}
	if _, err := ResolveOwnerPreferring(unionOf(legOver(open1, RoleAuthority, PolicyFatal, beads.NoLegRole)), "ga-1", closed); err == nil {
		t.Error("a Union plan was accepted")
	}
}

// TestOwnerResidentAndWorkResidual: a resolution's Resident is the handle on
// the store it names, probed or the unprobed residual; a miss has none; and
// WorkResidual is the topology's work leg as a handle.
func TestOwnerResidentAndWorkResidual(t *testing.T) {
	f := newT1()
	f.legStore("class:gmnos").seed(t, "gcg-abc")
	owner, err := ResolveOwnerRow(mustPlan(t, ByID{ID: "gcg-abc"}, f.topo), "gcg-abc")
	if err != nil || !owner.Read || !beads.SameStore(owner.Resident, owner.Store) || owner.Resident.Ref() != "class:gmnos" {
		t.Errorf("probed owner: %+v, %v", owner.Resident, err)
	}
	residual, err := ResolveOwnerRow(mustPlan(t, ByID{ID: "ga-xyz"}, f.topo), "ga-xyz")
	if err != nil || residual.Read || !beads.SameStore(residual.Resident, f.work) || residual.Resident.Ref() != "" {
		t.Errorf("residual owner: %+v, %v", residual, err)
	}
	if got := WorkResidual(f.topo); !beads.SameStore(got, f.work) || got.Raw() != beads.Store(f.work) {
		t.Errorf("WorkResidual = %v", got.Ref())
	}
	if got := WorkResidual(Topology{}); !beads.IsNilStore(got) {
		t.Error("WorkResidual of no work store is a store")
	}
	if miss, _ := ResolveOwnerRow(ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{{Leg: Leg{Store: newNamedStore("x")}, Role: RoleShadow}}}, "ga-1"); !beads.IsNilStore(miss.Resident) {
		t.Error("a miss carries a resident handle")
	}
}

// TestResolveOwnerPreferringReadsPastTheCache (R4): a leg over a cache whose
// copy is stale (open) while its backing already reads closed answers the
// backing's closed row, because the probe reads live; a cached read would
// settle on the stale open copy of an earlier leg.
func TestResolveOwnerPreferringReadsPastTheCache(t *testing.T) {
	backing := newNamedStore("backing")
	backing.seed(t, "ga-1")
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := backing.Close("ga-1"); err != nil {
		t.Fatal(err)
	}
	if cached, err := cache.Get("ga-1"); err != nil || cached.Status == "closed" {
		t.Fatalf("fixture: the cache's copy is %q, %v; want a stale open copy", cached.Status, err)
	}
	open := newNamedStore("open")
	open.seed(t, "ga-1")
	plan := ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{
		{Leg: Leg{Ref: "open", Store: open}, Role: RoleResidenceProbe, OnError: PolicyFatal},
		{Leg: Leg{Ref: "cached", Store: cache}, Role: RoleWorkFallback, OnError: PolicyFatal},
	}}
	owner, err := ResolveOwnerPreferring(plan, "ga-1", func(b beads.Bead) bool { return b.Status == "closed" })
	if err != nil || owner.Ref != "cached" || owner.Bead.Status != "closed" {
		t.Errorf("ResolveOwnerPreferring = %q %q, %v; want the backing's closed row on the cached leg", owner.Ref, owner.Bead.Status, err)
	}
}
