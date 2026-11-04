package storeref

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type liveOwnerBacking struct {
	beads.Store
	getErr   error
	getCalls int
}

func (s *liveOwnerBacking) Get(id string) (beads.Bead, error) {
	s.getCalls++
	if s.getErr != nil {
		return beads.Bead{}, s.getErr
	}
	return s.Store.Get(id)
}

func liveOwnerCache(t *testing.T, populated bool) (*beads.MemStore, *liveOwnerBacking, *beads.CachingStore) {
	t.Helper()
	base := beads.NewMemStore()
	base.HonorExplicitIDs = true
	if populated {
		if _, err := base.Create(beads.Bead{ID: "work-input", Title: "Owned current input", Type: "task"}); err != nil {
			t.Fatal(err)
		}
	}
	backing := &liveOwnerBacking{Store: base}
	cache := beads.NewCachingStore(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	return base, backing, cache
}

func liveOwnerPlan(binding, work beads.Store) ResolvedPlan {
	legs := []PlanLeg{}
	if binding != nil {
		legs = append(legs, PlanLeg{Leg: Leg{Ref: ClassRef(infraClasses), Store: binding}, Role: RoleResidenceProbe, OnError: PolicyFatal})
	}
	legs = append(legs, PlanLeg{Leg: Leg{Ref: WorkRef, Store: work}, Role: RoleWorkFallback, OnError: PolicyFatal})
	return ResolvedPlan{Mode: ModeFirstOwner, ID: "work-input", Legs: legs}
}

func TestResolveOwnerRowLiveReadsWorkResidualBeforeReturningOwner(t *testing.T) {
	base, backing, cache := liveOwnerCache(t, true)
	if err := base.Close("work-input"); err != nil {
		t.Fatal(err)
	}
	plan := liveOwnerPlan(nil, cache)
	legacy, err := ResolveOwnerRow(plan, "work-input")
	if err != nil || legacy.Read || backing.getCalls != 0 {
		t.Fatalf("legacy residual behavior changed: %+v err=%v reads=%d", legacy, err, backing.getCalls)
	}
	owner, err := ResolveOwnerRowLive(plan, "work-input")
	if err != nil || !owner.Read || owner.Store != cache || owner.Bead.Status != "closed" || backing.getCalls != 1 {
		t.Fatalf("live residual answered from stale cache: %+v err=%v reads=%d", owner, err, backing.getCalls)
	}
}

func TestResolveOwnerRowLiveDoesNotLetCachedMissSelectOldWorkTwin(t *testing.T) {
	bindingBase, binding, bindingCache := liveOwnerCache(t, true)
	_, work, workCache := liveOwnerCache(t, true)
	// Keep a real cached tombstone while an out-of-band writer recreates the
	// same durable identity in the canonical binding.
	if err := bindingCache.Delete("work-input"); err != nil {
		t.Fatal(err)
	}
	binding.getCalls = 0
	if _, err := bindingBase.Create(beads.Bead{ID: "work-input", Title: "Current relocated input", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	if err := bindingBase.Close("work-input"); err != nil {
		t.Fatal(err)
	}
	plan := liveOwnerPlan(bindingCache, workCache)
	legacy, err := ResolveOwnerRow(plan, "work-input")
	if err != nil || legacy.Store != workCache || legacy.Read {
		t.Fatalf("fixture lacks the cached-negative residency path: %+v %v", legacy, err)
	}
	owner, err := ResolveOwnerRowLive(plan, "work-input")
	if err != nil || owner.Store != bindingCache || owner.Bead.Status != "closed" || !owner.Read {
		t.Fatalf("cached binding miss selected a stale work twin: %+v %v", owner, err)
	}
	if binding.getCalls != 1 || work.getCalls != 0 {
		t.Fatalf("owner was not selected by current authoritative evidence: binding=%d work=%d", binding.getCalls, work.getCalls)
	}
}

func TestResolveOwnerRowLiveDoesNotLetCachedGhostHideCurrentWorkOwner(t *testing.T) {
	bindingBase, binding, bindingCache := liveOwnerCache(t, true)
	workBase, work, workCache := liveOwnerCache(t, true)
	if err := bindingBase.Delete("work-input"); err != nil {
		t.Fatal(err)
	}
	if err := workBase.Close("work-input"); err != nil {
		t.Fatal(err)
	}
	owner, err := ResolveOwnerRowLive(liveOwnerPlan(bindingCache, workCache), "work-input")
	if err != nil || owner.Store != workCache || owner.Bead.Status != "closed" || binding.getCalls != 1 || work.getCalls != 1 {
		t.Fatalf("cached binding ghost won residency: %+v err=%v binding=%d work=%d", owner, err, binding.getCalls, work.getCalls)
	}
}

func TestResolveOwnerRowLiveUnavailableProbeNeverFallsBack(t *testing.T) {
	_, binding, bindingCache := liveOwnerCache(t, true)
	_, work, workCache := liveOwnerCache(t, true)
	boom := errors.New("current binding read unavailable")
	binding.getErr = boom
	owner, err := ResolveOwnerRowLive(liveOwnerPlan(bindingCache, workCache), "work-input")
	if !errors.Is(err, boom) || owner.Read || binding.getCalls != 1 || work.getCalls != 0 {
		t.Fatalf("unknown binding became cached success or work fallback: %+v err=%v binding=%d work=%d", owner, err, binding.getCalls, work.getCalls)
	}
}

func TestResolveOwnerRowLiveMissingAndWrongPlanRemainDistinct(t *testing.T) {
	_, binding, bindingCache := liveOwnerCache(t, false)
	_, work, workCache := liveOwnerCache(t, false)
	plan := liveOwnerPlan(bindingCache, workCache)
	if _, err := ResolveOwnerRowLive(plan, "other-id"); err == nil || binding.getCalls != 0 || work.getCalls != 0 {
		t.Fatalf("wrong-ID plan performed reads: err=%v binding=%d work=%d", err, binding.getCalls, work.getCalls)
	}
	if _, err := ResolveOwnerRowLive(plan, "work-input"); !errors.Is(err, beads.ErrNotFound) || binding.getCalls != 1 || work.getCalls != 1 {
		t.Fatalf("complete live miss was not a proven not-found: err=%v binding=%d work=%d", err, binding.getCalls, work.getCalls)
	}
}
