package main

// The CONTROLLER half of bead.* emission on a split city (ga-f7v2ft.164 step 2).
//
// class_store_emit_test.go covers the one-shot CLI's side. This file covers the
// side the .162 investigation found still silent: the controller opens its
// relocated binding through openStorageRoutes, which handed back the bare
// engine, so every session-class write it made emitted nothing. The cost is not
// the missing rows — it is admitSessionStartEvent, which never fired for a
// session bead on a split city, leaving keyed admission at patrol-tick cadence
// (soak item S4).
//
// The tests below drive the WHOLE chain the controller runs in production —
// store write → emission → recorder → the bead-event watcher → admission — on
// both topologies, because the split city's claim is a comparison: it must
// behave like the single-store city the campaign certified.

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// splitCityControllerState builds the controller state a converged split city
// runs with: the relocated binding serving every infrastructure class, the work
// ledger behind it, and the live recorder the supervisor hands both.
//
// It goes through storageBootGate and the provider's own EngineOpener, exactly
// as the controller's boot does, because the defect lived in that path and a
// hand-built route map cannot reproduce it.
func splitCityControllerState(t *testing.T) (*controllerState, *events.Fake) {
	t.Helper()
	routes, work, cfg, cityPath := serveConvergedSplitCity(t, "auto")
	ep := events.NewFake()
	cs := &controllerState{
		cfg:           cfg,
		cityPath:      cityPath,
		cityName:      "split-city",
		cityBeadStore: wrapWithCachingStore(context.Background(), work, ep, false),
		storageRoutes: routes.withControllerEmission(ep),
		eventProv:     ep,
		pokeCh:        make(chan struct{}, 1),
	}
	return cs, ep
}

// singleStoreControllerState is the control topology: no [storage] section, so
// the session class resolves to the work ledger the controller wrapped in its
// own CachingStore. This is the composition the campaign certified, and the one
// the split city has to match.
func singleStoreControllerState(t *testing.T) (*controllerState, *events.Fake) {
	t.Helper()
	ep := events.NewFake()
	cs := &controllerState{
		cfg:           &config.City{},
		cityPath:      t.TempDir(),
		cityName:      "single-store-city",
		cityBeadStore: wrapWithCachingStore(context.Background(), beads.NewMemStore(), ep, false),
		eventProv:     ep,
		pokeCh:        make(chan struct{}, 1),
	}
	return cs, ep
}

// admitSessionStartFromAWrite creates one session bead through the controller's
// own session-class front door and returns the id the keyed session-start
// admission was handed, or "" when nothing arrived before the deadline.
//
// Nothing here is injected: the write goes to whatever store the class resolver
// returns, the event has to be emitted by that store, the recorder has to carry
// it, and the controller's real watcher has to pick it up. That is the whole
// claim — a split city's session write reaches keyed admission at event
// cadence, not at the next patrol tick.
func admitSessionStartFromAWrite(t *testing.T, cs *controllerState) string {
	t.Helper()
	admitted := make(chan string, 4)
	if err := cs.installSessionStartEventAdmission(func(id string) {
		select {
		case admitted <- id:
		default:
		}
	}); err != nil {
		t.Fatalf("installing session-start event admission: %v", err)
	}
	t.Cleanup(cs.stopSessionStartEventAdmission)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs.startBeadEventWatcher(ctx)

	created, err := cs.SessionsBeadStore().Create(beads.Bead{
		Title:  "keyed session",
		Type:   session.BeadType,
		Status: "open",
	})
	if err != nil {
		t.Fatalf("creating a session bead on the session-class store: %v", err)
	}

	select {
	case id := <-admitted:
		if id != created.ID {
			t.Fatalf("admission was handed %q, want the bead just written (%s)", id, created.ID)
		}
		return id
	case <-time.After(10 * time.Second):
		return ""
	}
}

// TestSingleStoreSessionWriteReachesKeyedAdmission is the control, and it must
// pass before and after the fix: the certified single-store composition already
// carries a session write all the way to keyed admission. Without it, the split
// assertion below proves only that the test harness works.
func TestSingleStoreSessionWriteReachesKeyedAdmission(t *testing.T) {
	cs, _ := singleStoreControllerState(t)
	if admitSessionStartFromAWrite(t, cs) == "" {
		t.Fatal("a session write on a single-store city never reached keyed session-start admission; the control topology is broken, so nothing below can be read")
	}
}

// TestSplitCitySessionWriteReachesKeyedAdmission is ga-f7v2ft.161 Q4 / .162
// defect 3, stated as the thing an operator actually loses: on a split city the
// session-class store emitted no bead.*, so admitSessionStartEvent never fired
// for a session row and keyed admission ran at patrol cadence.
func TestSplitCitySessionWriteReachesKeyedAdmission(t *testing.T) {
	cs, _ := splitCityControllerState(t)
	if admitSessionStartFromAWrite(t, cs) == "" {
		t.Fatal("a session write on a SPLIT city never reached keyed session-start admission: the relocated session-class store is event-silent, so admission waits for the next patrol tick")
	}
}

// TestSplitCitySessionWriteEmitsABeadEvent is the emission half on its own, so a
// failure says which link broke: the row has to exist on the recorder, carry the
// canonical decodable payload, and name the bead that was written.
func TestSplitCitySessionWriteEmitsABeadEvent(t *testing.T) {
	cs, ep := splitCityControllerState(t)

	created, err := cs.SessionsBeadStore().Create(beads.Bead{
		Title:  "relocated session",
		Type:   session.BeadType,
		Status: "open",
	})
	if err != nil {
		t.Fatalf("creating a session bead on the relocated store: %v", err)
	}

	recorded, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("listing recorded events: %v", err)
	}
	for _, evt := range recorded {
		if evt.Type != events.BeadCreated || evt.Subject != created.ID {
			continue
		}
		bead, ok := beads.DecodeBeadEventPayload(evt.Payload)
		if !ok {
			t.Fatalf("the relocated store's bead.created payload does not decode: %s", string(evt.Payload))
		}
		if bead.ID != created.ID || bead.Type != session.BeadType {
			t.Fatalf("payload = %+v, want the session bead just written (%s)", bead, created.ID)
		}
		return
	}
	t.Fatalf("no bead.created for %s among %d recorded events: a relocated session-class write is event-silent", created.ID, len(recorded))
}

// TestSplitCityEmissionCarriesTheStoreLayerActor pins the one field that decides
// what ELSE the event does. The controller's work ledger emits its own mutations
// as the store layer, which is what keeps a routine write from poking the tick
// and from being read as externally-authored routed-work demand. A relocated
// store emitting under any other actor would make a split city's session writes
// behave differently from the identical writes on a single-store city.
func TestSplitCityEmissionCarriesTheStoreLayerActor(t *testing.T) {
	cs, ep := splitCityControllerState(t)

	created, err := cs.SessionsBeadStore().Create(beads.Bead{
		Title:  "relocated session",
		Type:   session.BeadType,
		Status: "open",
	})
	if err != nil {
		t.Fatalf("creating a session bead on the relocated store: %v", err)
	}
	recorded, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("listing recorded events: %v", err)
	}
	found := false
	for _, evt := range recorded {
		if evt.Subject != created.ID {
			continue
		}
		found = true
		if evt.Actor != beadStoreLayerActor {
			t.Errorf("relocated emission actor = %q, want %q — the single-store city's own store layer emits under that actor, and the split city must not diverge", evt.Actor, beadStoreLayerActor)
		}
	}
	if !found {
		t.Fatalf("no event at all for %s", created.ID)
	}
}

// TestControllerRoutesCarryAnEmitTarget is the wiring assertion the stale
// comment at class_store.go used to stand in for. Emission is required plumbing
// on a relocated class, so the controller's routes must carry a target — a
// runtime that opened a binding and wired nothing is the defect, not a
// configuration.
func TestControllerRoutesCarryAnEmitTarget(t *testing.T) {
	cs, _ := splitCityControllerState(t)
	sessions := cs.SessionsBeadStore().Store
	if !storeEmitsBeadEvents(sessions) {
		t.Fatalf("the controller's session-class store (%T) reports no bead.* emission; relocated writes would be silent", sessions)
	}
}

// TestNewCityRuntimeWiresEmissionIntoARelocatedClass is the PRODUCTION SEAM.
// The fixtures above prove the mechanism; this proves the boot path actually
// reaches for it, on the constructor both production boot paths consume. Delete
// the one line in newCityRuntime and this is the test that goes red.
func TestNewCityRuntimeWiresEmissionIntoARelocatedClass(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	source := stubInfraMigrationSource(t)

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the migration reported %s: %s", got.Outcome, log.String())
	}

	sp := runtime.NewFake()
	cr, err := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "split-city",
		Cfg:      cfg,
		SP:       sp,
		Dops:     newDrainOps(sp),
		Rec:      events.NewFake(),
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	if err != nil {
		t.Fatalf("building a converged split city: %v", err)
	}
	// Only the binding is released: shutting the whole runtime down exercises
	// machinery this test never started.
	t.Cleanup(func() { _ = cr.storageRoutes.close() })

	sessions := cr.sessionsBeadStore().Store
	if sessions == source {
		t.Fatal("the session class still resolves to the work store on a converged city")
	}
	if !storeEmitsBeadEvents(sessions) {
		t.Fatalf("newCityRuntime served the session class from %T with no emission wired: every session-class write on this city is silent, and keyed admission waits for the patrol tick", sessions)
	}
}

// TestEmissionWrapperDoesNotHideAnIncapableBacking is the hazard the wrapper
// introduces, closed. Wrapping the store the session class resolves to puts a
// layer between the .162 requirement and the engine that answers it — and the
// wrapper forwards the whole fenced trio, so asking IT produces a vacuous yes.
// A store that cannot fence must still be FATAL through the wrapper, or the fix
// for event silence would have silently un-done the fix for silent unfencing.
func TestEmissionWrapperDoesNotHideAnIncapableBacking(t *testing.T) {
	backing := beads.NewMemStore()
	backing.DisableConditionalWrites = true

	if _, err := beads.RequiredConditionalWriter(backing); err == nil {
		t.Fatal("control: the bare incapable store satisfied the requirement, so nothing below discriminates")
	}

	routes := splitClassRoutes(backing).withControllerEmission(events.NewFake())
	sessions := resolveSessionStore(routes, beads.NewMemStore(), nil, t.TempDir(), nil)
	if _, err := beads.RequiredConditionalWriter(sessions); err == nil {
		t.Error("an incapable backing satisfied the session-class requirement through the emitting wrapper")
	}
	if insp := beads.InspectConditionalWrites(sessions); insp.Capable || insp.StoreKind != "MemStore" {
		t.Errorf("inspection through the wrapper = %+v, want the BACKING reported incapable", insp)
	}

	cs := &controllerState{rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Off))}
	cs.storageRoutes = routes
	cs.cityBeadStore = beads.NewMemStore()
	row, ok := conditionalWritesRow(cs.ConditionalWritesStatus().Stores, sessionClassStoreID)
	if !ok || row.Capable || !strings.Contains(row.Reason, "FATAL") {
		t.Errorf("the §12.5 session-class row = %+v (present=%t), want the FATAL verdict the fence check produces", row, ok)
	}
}

// TestRequiredWriterOnAWrappedStoreStillEmits is the control for the test above,
// and the reason the capability question is pointed down rather than the WRITE.
// A caller that takes the required writer and fences with it must still produce
// an event — redirecting resolution instead would have handed it the bare engine
// and made every fenced write, terminal close included, silent again.
func TestRequiredWriterOnAWrappedStoreStillEmits(t *testing.T) {
	cs, ep := splitCityControllerState(t)
	sessions := cs.SessionsBeadStore().Store

	created, err := sessions.Create(beads.Bead{Title: "fenced session", Type: session.BeadType, Status: "open"})
	if err != nil {
		t.Fatalf("creating a session bead: %v", err)
	}
	fresh, err := sessions.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading the session bead: %v", err)
	}

	writer, err := beads.RequiredConditionalWriter(sessions)
	if err != nil {
		t.Fatalf("the relocated session-class store cannot fence: %v", err)
	}
	title := "healed"
	if err := writer.UpdateIfMatch(fresh.ID, fresh.Revision, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("fenced update: %v", err)
	}

	recorded, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("listing recorded events: %v", err)
	}
	for _, evt := range recorded {
		if evt.Type == events.BeadUpdated && evt.Subject == created.ID {
			return
		}
	}
	t.Fatalf("a fenced update through the required writer emitted nothing (%d events): the capability lookup redirected the WRITE, not just the question", len(recorded))
}

// TestWiringRoutesTwiceDoesNotDoubleTheEmission pins the invariant the
// per-process rule rests on. A wrapper around a wrapper emits every row twice,
// and on the reconcile path — where the runtime re-writes rows it just read —
// that is not a duplicate, it is a flood.
func TestWiringRoutesTwiceDoesNotDoubleTheEmission(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewMemStore()
	ep := events.NewFake()

	routes := splitClassRoutes(leaf).withControllerEmission(ep).withCLIEmission(cityPath)
	bead := seedClassBead(t, leaf, "twice")
	if err := resolveGraphStore(routes, beads.NewMemStore(), nil, cityPath, nil).Close(bead.ID); err != nil {
		t.Fatalf("closing through twice-wired routes: %v", err)
	}

	recorded, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("listing recorded events: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("one close produced %d recorded event(s), want 1", len(recorded))
	}
	if journal := beadEvents(readCityJournal(t, cityPath)); len(journal) != 0 {
		t.Fatalf("the second wiring attempt installed a target anyway and appended %d journal row(s): %s", len(journal), eventSummary(journal))
	}
}

// TestUnwiredRoutesEmitNothing is the control for the assertion above: the
// emission check has to be able to answer no, or "wired" means nothing. Routes
// straight out of the open seam, before any process claims them, carry no
// target.
func TestUnwiredRoutesEmitNothing(t *testing.T) {
	routes, work, cfg, cityPath := serveConvergedSplitCity(t, "auto")
	sessions := resolveSessionStore(routes, work, cfg, cityPath, nil)
	if storeEmitsBeadEvents(sessions) {
		t.Fatalf("a store nobody wired (%T) reports emission; the wiring check cannot fail, so it proves nothing", sessions)
	}
}
