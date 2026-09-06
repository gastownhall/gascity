package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// bindingOrphanReconcileEnv is the maintainer-city shape the orphan-close
// tie-break wedges on (ga-b0o6a): a split city whose infrastructure classes are
// relocated into one binding, a rig-scoped pool seat whose runtime is gone, and
// the seat's claim resident in the BINDING rather than in the rig work ledger
// its gc.routed_to prefix names.
//
// The rig ledger is deliberately empty. Every lane that resolves an owner store
// from the route lands there, finds nothing, and leaves the claim held — while
// the close guard, which reads the binding leg, correctly sees the work and
// refuses to close the seat.
type bindingOrphanReconcileEnv struct {
	cityPath string
	cfg      *config.City
	binding  beads.Store
	rig      beads.Store
	sp       *runtime.Fake
	session  beads.Bead
	work     beads.Bead
}

func newBindingOrphanReconcileEnv(t *testing.T) *bindingOrphanReconcileEnv {
	t.Helper()
	cityPath := t.TempDir()
	writeCityTOML(t, cityPath, "test-city", "gc.implementation-worker")
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Session:   config.SessionConfig{Provider: "fake"},
		Rigs:      []config.Rig{{Name: "beads", Path: filepath.Join(cityPath, "beads")}},
		Agents: []config.Agent{{
			Name:              "gc.implementation-worker",
			Dir:               "beads",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}

	// Type + label matter: the reconciler resolves a seat's liveness through
	// session.ResolveSessionRecordByExactID, which only recognizes a typed (or
	// labeled and untyped) bead as a session record.
	session, err := binding.Create(beads.Bead{
		Title:  "gc.implementation-worker",
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"session_name":       bindingOrphanSeat,
			"template":           bindingOrphanTemplate,
			"agent_name":         "gc.implementation-worker",
			"state":              "asleep",
			"generation":         "1",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := binding.Create(beads.Bead{
		Title:    "graph step held by a dead seat",
		Status:   "open",
		Assignee: bindingOrphanSeat,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: bindingOrphanTemplate,
			"gc.root_store_ref":          "rig:beads",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	return &bindingOrphanReconcileEnv{
		cityPath: cityPath,
		cfg:      cfg,
		binding:  binding,
		rig:      beads.NewMemStore(),
		sp:       runtime.NewFake(),
		session:  session,
		work:     work,
	}
}

func (e *bindingOrphanReconcileEnv) rigStores() map[string]beads.Store {
	return map[string]beads.Store{"beads": e.rig}
}

// reconcile runs one reconcile pass over the orphaned seat. Empty desired state
// plus a provider that never started the session is a confirmed orphan.
func (e *bindingOrphanReconcileEnv) reconcile(t *testing.T, options ...startExecutionOption) {
	t.Helper()
	reconcileSessionBeadsAtPath(
		context.Background(), e.cityPath, []beads.Bead{e.session}, map[string]TemplateParams{}, map[string]bool{},
		e.cfg, e.sp, e.binding, nil, []beads.Bead{e.work}, e.rigStores(), nil, newDrainTracker(),
		map[string]int{}, false, nil, "test-city",
		nil, clock.Real{}, events.NewFake(), time.Minute, 0,
		io.Discard, io.Discard,
		options...,
	)
}

// tick runs the controller's own bead-reconcile tick, so the aligned stores must
// travel the whole production route: DesiredStateResult.AssignedWorkStores →
// wake filter → start option → orphan close.
func (e *bindingOrphanReconcileEnv) tick(t *testing.T) {
	t.Helper()
	cr := &CityRuntime{
		cityPath:            e.cityPath,
		cityName:            "test-city",
		cfg:                 e.cfg,
		sp:                  e.sp,
		standaloneCityStore: e.binding,
		standaloneRigStores: e.rigStores(),
		sessionDrains:       newDrainTracker(),
		rec:                 events.NewFake(),
		logPrefix:           "gc",
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	cr.beadReconcileTick(context.Background(), DesiredStateResult{
		State:                 map[string]TemplateParams{},
		AssignedWorkBeads:     []beads.Bead{e.work},
		AssignedWorkStores:    []beads.Store{e.binding},
		AssignedWorkStoreRefs: []string{""},
	}, newSessionBeadSnapshot([]beads.Bead{e.session}), nil, false)
}

func (e *bindingOrphanReconcileEnv) get(t *testing.T, id string) beads.Bead {
	t.Helper()
	got, err := e.binding.Get(id)
	if err != nil {
		t.Fatalf("binding.Get(%s): %v", id, err)
	}
	return got
}

// TestSessionReconcilerOrphanCloseReleasesBindingResidentWorkThroughAlignedStores
// is the end-to-end repair. The controller tick already knows which leg it read
// each assigned-work row through; carrying that leg to the orphan-close
// tie-break is what lets a binding-resident claim be released at all.
//
// Without it the tie-break asks the rig ledger named by gc.routed_to, gets "no
// such bead", skips the release, and the close guard — which reads the binding —
// keeps refusing. The seat then wedges for as long as the city runs (ga-jrnou,
// 86 hours in the case that surfaced it).
func TestSessionReconcilerOrphanCloseReleasesBindingResidentWorkThroughAlignedStores(t *testing.T) {
	env := newBindingOrphanReconcileEnv(t)

	env.tick(t)

	work := env.get(t, env.work.ID)
	if work.Assignee != "" || work.Status != "open" {
		t.Fatalf("claim = status %q assignee %q, want open/unassigned — a confirmed-orphaned seat must not keep holding a binding-resident claim", work.Status, work.Assignee)
	}
	session := env.get(t, env.session.ID)
	if session.Status != "closed" {
		t.Fatalf("session bead status = %q, want closed — once the held work is released the close guard stops refusing, so the orphan closes in the same tick", session.Status)
	}
}

// TestSessionReconcilerOrphanCloseWithAlignedStoresOptionReleasesHeldWork pins
// the option itself, independent of the controller wiring above: the reconciler
// releases through the supplied leg and closes the seat in the same pass.
func TestSessionReconcilerOrphanCloseWithAlignedStoresOptionReleasesHeldWork(t *testing.T) {
	env := newBindingOrphanReconcileEnv(t)

	env.reconcile(t, withAssignedWorkStores([]beads.Store{env.binding}))

	work := env.get(t, env.work.ID)
	if work.Assignee != "" || work.Status != "open" {
		t.Fatalf("claim = status %q assignee %q, want open/unassigned", work.Status, work.Assignee)
	}
	session := env.get(t, env.session.ID)
	if session.Status != "closed" {
		t.Fatalf("session bead status = %q, want closed", session.Status)
	}
}

// TestSessionReconcilerOrphanCloseWithoutAlignedStoresLeavesBindingResidentWorkHeld
// is the mutation probe: it documents the behavior every caller that supplies
// no aligned stores still gets, and it is exactly the wedge. If this test ever
// starts releasing, the tie-break has grown a second owner-store resolver and
// the aligned leg is no longer what the tests above are measuring.
func TestSessionReconcilerOrphanCloseWithoutAlignedStoresLeavesBindingResidentWorkHeld(t *testing.T) {
	env := newBindingOrphanReconcileEnv(t)

	env.reconcile(t)

	work := env.get(t, env.work.ID)
	if work.Assignee != bindingOrphanSeat || work.Status != "open" {
		t.Fatalf("claim = status %q assignee %q, want the claim still held — with no aligned leg the routed prefix names the empty rig ledger", work.Status, work.Assignee)
	}
	session := env.get(t, env.session.ID)
	if session.Status == "closed" {
		t.Fatalf("session bead closed while still holding work — the close guard must keep refusing until the claim is actually released")
	}
}

// TestSessionReconcilerOrphanCloseIgnoresMisalignedAssignedWorkStores pins the
// alignment invariant at the call site. A slice that does not describe this
// tick's assigned-work snapshot is not a smaller truth, it is a different one:
// indexing it would release a claim through a store that never held it. The
// site drops to the documented fallback instead — no panic, no release.
func TestSessionReconcilerOrphanCloseIgnoresMisalignedAssignedWorkStores(t *testing.T) {
	env := newBindingOrphanReconcileEnv(t)

	env.reconcile(t, withAssignedWorkStores([]beads.Store{env.binding, env.rig}))

	work := env.get(t, env.work.ID)
	if work.Assignee != bindingOrphanSeat || work.Status != "open" {
		t.Fatalf("claim = status %q assignee %q, want the claim untouched — a misaligned store slice must never be indexed", work.Status, work.Assignee)
	}
}
