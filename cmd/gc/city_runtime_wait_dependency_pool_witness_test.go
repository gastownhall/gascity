package main

import (
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// sessionWaitDependencyPoolWitnessFixture builds one certified sole pool member
// and the lease the shipped keyed wait-dependency pool resume certifies against,
// so the witness predicate can be exercised on its own.
type sessionWaitDependencyPoolWitnessFixture struct {
	cr       *CityRuntime
	cfg      *config.City
	store    beads.Store
	snapshot controllerSessionStartSnapshot
	info     sessionpkg.Info
	lease    sessionWaitDependencyStartLease
}

func newSessionWaitDependencyPoolWitnessFixture(t *testing.T, maxActiveSessions int, occupied bool) sessionWaitDependencyPoolWitnessFixture {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Prefix: "hq"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(maxActiveSessions),
		}},
	}
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	cs := coherentSessionStartControllerStateForTest(cfg, provider, store, rollout.Auto)
	cs.cityPath = cityPath
	cs.cityName = "test-city"
	cr := &CityRuntime{
		cityPath:             cityPath,
		cityName:             "test-city",
		cfg:                  cfg,
		sp:                   provider,
		cs:                   cs,
		rec:                  events.Discard,
		poolMembershipShadow: newPoolMembershipIndex(),
		stdout:               io.Discard,
		stderr:               io.Discard,
	}
	if !cr.poolMembershipShadow.publishRebuild(0, newPoolMembershipState()) {
		t.Fatal("publish empty pool membership")
	}
	work, err := store.Create(beads.Bead{
		Title:    "open routed work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create routed work: %v", err)
	}
	info := newSessionWaitDependencyPoolWitnessMember(t, store, cfg, work.ID, 1, occupied)
	if err := cr.poolMembershipShadow.replace(cfg, info); err != nil {
		t.Fatalf("publish pool member: %v", err)
	}
	observation, memberIDs, exact := cr.poolMembershipShadow.observeMemberIDs("worker")
	if !exact || len(memberIDs) != 1 || memberIDs[0] != info.ID {
		t.Fatalf("sole pool membership = (%+v, %v, %t), want one certified member", observation, memberIDs, exact)
	}
	wantOccupied := 0
	if occupied {
		wantOccupied = 1
	}
	if observation.occupied != wantOccupied {
		t.Fatalf("member occupancy = %d, want %d", observation.occupied, wantOccupied)
	}
	snapshot, release, err := cs.acquireSessionStartSnapshot()
	if err != nil {
		t.Fatalf("acquire start snapshot: %v", err)
	}
	t.Cleanup(release)
	// The shared wake-candidate gate already applies the identity-model
	// exclusion: a max==1 pool IS the canonical singleton, whose rows ride
	// other families. Everything else is a candidate regardless of arm.
	wantCandidate := maxActiveSessions != 1
	if got := waitDependencyConfiguredTemplateEligible(info, cfg, provider, snapshot.CityName, store, time.Time{}); got != wantCandidate {
		t.Fatalf("configured-template wake candidate = %t, want %t", got, wantCandidate)
	}
	return sessionWaitDependencyPoolWitnessFixture{
		cr: cr, cfg: cfg, store: store, snapshot: snapshot, info: info,
		lease: sessionWaitDependencyStartLease{
			WaitID:                 "wait-1",
			SessionID:              info.ID,
			DepIDs:                 []string{"dependency-1"},
			DepMode:                "all",
			IndexGeneration:        1,
			ControllerGeneration:   snapshot.Generation,
			PoolTarget:             "worker",
			PoolMembershipRevision: observation.revision,
		},
	}
}

func newSessionWaitDependencyPoolWitnessMember(
	t *testing.T,
	store beads.Store,
	cfg *config.City,
	workID string,
	slot int,
	occupied bool,
) sessionpkg.Info {
	t.Helper()
	info, err := createPoolSessionBeadWithAlias(store, "worker", cfg, nil, time.Now().UTC(), poolSessionCreateIdentity{
		AgentName: fmt.Sprintf("worker-%d", slot),
		Slot:      slot,
		Metadata: map[string]string{
			beadmeta.TriggerBeadIDMetadataKey:       workID,
			beadmeta.TriggerBeadStoreRefMetadataKey: "city:test-city",
		},
	}, "")
	if err != nil {
		t.Fatalf("create pool member in slot %d: %v", slot, err)
	}
	state := string(sessionpkg.StateAsleep)
	if occupied {
		state = string(sessionpkg.StateActive)
	}
	if err := store.SetMetadataBatch(info.ID, map[string]string{
		"state":                     state,
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}); err != nil {
		t.Fatalf("set pool member %d lifecycle state: %v", slot, err)
	}
	info, err = sessionFrontDoor(store).Get(info.ID)
	if err != nil {
		t.Fatalf("read pool member in slot %d: %v", slot, err)
	}
	return info
}

// TestSessionWaitDependencyPoolWitnessAppliesTheUniformPredicateContract pins
// the Q1 resolution recorded on ga-f7v2ft.116. The shipped keyed
// wait-dependency pool resume was unreachable on BOTH arms: the witness
// demanded reason == EligibleAgentCap, which the unlimited arm (max=-1, reason
// Eligible) can never satisfy, and it demanded a completely unoccupied pool,
// which a resumed WORKING member -- the member still holding its own open
// trigger -- can never satisfy either. Under the uniform contract eligibility
// is supported() at every pool-family site, capacity is a separate explicit
// check that never double-counts the resumed member's own occupancy, and the
// only identity-model exclusion is the canonical singleton, named honestly.
func TestSessionWaitDependencyPoolWitnessAppliesTheUniformPredicateContract(t *testing.T) {
	for _, test := range []struct {
		name              string
		maxActiveSessions int
		occupied          bool
	}{
		{name: "unlimited idle member", maxActiveSessions: -1},
		{name: "unlimited working member", maxActiveSessions: -1, occupied: true},
		{name: "bounded idle member", maxActiveSessions: 2},
		{name: "bounded working member", maxActiveSessions: 2, occupied: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSessionWaitDependencyPoolWitnessFixture(t, test.maxActiveSessions, test.occupied)
			if !fixture.cr.sessionWaitDependencyPoolWitnessCurrent(fixture.snapshot, fixture.info, fixture.lease) {
				t.Fatal("sole pool member did not witness its own keyed resume")
			}
		})
	}

	t.Run("canonical singleton still refuses", func(t *testing.T) {
		fixture := newSessionWaitDependencyPoolWitnessFixture(t, 1, false)
		if fixture.cr.sessionWaitDependencyPoolWitnessCurrent(fixture.snapshot, fixture.info, fixture.lease) {
			t.Fatal("canonical singleton pool witnessed a resume that rides another family")
		}
	})

	t.Run("occupancy held by another member still refuses", func(t *testing.T) {
		fixture := newSessionWaitDependencyPoolWitnessFixture(t, 2, false)
		sibling := newSessionWaitDependencyPoolWitnessMember(t, fixture.store, fixture.cfg, "other-work", 2, true)
		if err := fixture.cr.poolMembershipShadow.replace(fixture.cfg, sibling); err != nil {
			t.Fatalf("publish sibling pool member: %v", err)
		}
		observation, _, exact := fixture.cr.poolMembershipShadow.observeMemberIDs("worker")
		if !exact || observation.members != 2 || observation.occupied != 1 {
			t.Fatalf("two-member pool observation = %+v (exact=%t), want two members with one occupied", observation, exact)
		}
		lease := fixture.lease
		lease.PoolMembershipRevision = observation.revision
		if fixture.cr.sessionWaitDependencyPoolWitnessCurrent(fixture.snapshot, fixture.info, lease) {
			t.Fatal("a pool whose OTHER member holds occupancy witnessed a resume")
		}
	})
}
