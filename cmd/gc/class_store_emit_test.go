package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
)

// The three gates this seam has to hold, and the control that keeps the second
// one honest:
//
//  1. a mutation through the class front door appends exactly ONE bead.* event
//     to the city's journal (TestClassStoreDoorMutation…);
//  2. the controller/runtime side of the same resolver appends NONE
//     (TestControllerClassRoutes…), and that is asserted against a fixture
//     PROVEN able to observe an event, so "zero" cannot pass by accident;
//  3. what lands is readable by a journal CURSOR consumer — the shape the
//     event-delta lanes read (TestClassStoreEmissionIsVisibleToACursorConsumer).
//
// # Those three prove the WRAPPER; two more prove the WIRING
//
// The three above hand the funnel routes the TEST has already wrapped. That is
// the right shape for asserting emission SEMANTICS — it isolates them from
// migration, providers and plan resolution — but it means the shipped injection
// never runs, and a review caught exactly what that costs: rewriting the one
// production call as withCLIEmission("") left all three green while emission was
// entirely dead in production, because an empty city path early-returns
// unwrapped.
//
// So the two gates at the bottom of this file never say withCLIEmission at all.
// They stand up a city that has really converged onto a binding and enter
// through the real constructors — resolveCLIStorageRoutes for the one-shot side
// (TestOneShotCLIWritesEmitBeadEventsOnAMigratedCity) and openStorageRoutes for
// the controller's (TestControllerRoutesFromOpenStorageRoutesCarryNoEmitTarget)
// — so a mutation at either injection point reddens a DIFFERENT one of them.
// The lesson generalizes: a gate that constructs the thing under test cannot
// also be the gate that proves it is constructed.

// splitClassRoutes builds the routes a split city resolves: every
// infrastructure class served by one binding store, work left alone. It is the
// shape openStorageRoutes produces, so a test that does NOT call
// withCLIEmission is holding the CONTROLLER's routes.
func splitClassRoutes(class beads.Store) *storageRoutes {
	routes := &storageRoutes{stores: make(map[coordclass.Class]beads.Store), binding: "infra"}
	for _, c := range coordclass.Classes() {
		if c.IsInfrastructure() {
			routes.stores[c] = class
		}
	}
	return routes
}

// cityJournalPath is where a city's events land, and where the emitter is
// expected to append.
func cityJournalPath(cityPath string) string {
	return filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")
}

// readCityJournal returns every event in the city's log, in sequence order.
func readCityJournal(t *testing.T, cityPath string) []events.Event {
	t.Helper()
	path := cityJournalPath(cityPath)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	rec, err := events.NewFileRecorder(path, io.Discard, events.WithoutStartupSweep())
	if err != nil {
		t.Fatalf("opening the city journal: %v", err)
	}
	defer rec.Close() //nolint:errcheck // read-only in a test
	all, err := rec.List(events.Filter{})
	if err != nil {
		t.Fatalf("reading the city journal: %v", err)
	}
	return all
}

// beadEvents keeps only the bead.* rows, which are the ones this seam is about.
func beadEvents(all []events.Event) []events.Event {
	var out []events.Event
	for _, e := range all {
		if strings.HasPrefix(e.Type, "bead.") {
			out = append(out, e)
		}
	}
	return out
}

// seedClassBead writes a graph-class bead straight into the leaf store, which
// is deliberately NOT the emitting wrapper: a fixture that emitted while
// seeding would make every "exactly one event" assertion meaningless.
func seedClassBead(t *testing.T, leaf beads.Store, stepID string) beads.Bead {
	t.Helper()
	bead, err := leaf.Create(beads.Bead{
		Title:  "step",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:          "check",
			beadmeta.RootBeadIDMetadataKey:    "gcg-root-1",
			beadmeta.StepIDMetadataKey:        stepID,
			beadmeta.SessionIDMetadataKey:     "sess-1",
			beadmeta.LogicalBeadIDMetadataKey: "logical-1",
		},
	})
	if err != nil {
		t.Fatalf("seeding the class store: %v", err)
	}
	return bead
}

// GATE 1. A mutation through the class front door the one-shot commands open
// appends exactly one bead.* event, carrying the fresh snapshot and the
// correlation ids a run projection folds on.
//
// This is the production symptom: on a split city a worker's close of a
// graph-class step landed in the binding and appended nothing, so the
// event-sourced run view rendered the step "Running" forever.
//
// Every relocated class is a row, not just graph. resolveClassStore discarded
// its recorder for ALL of them, and the supported split moves all five at once
// — a seam that only covered graph would leave a mail write, a nudge
// terminalization and an order close just as dark as before.
func TestClassStoreDoorMutationEmitsExactlyOneBeadEvent(t *testing.T) {
	for _, row := range []struct {
		name  string
		close func(t *testing.T, cityPath, id string)
	}{
		{
			name: "graph, through the by-id class front door",
			close: func(t *testing.T, cityPath, id string) {
				door, routed, err := openBdByIDClassFrontDoor(cityPath)
				if err != nil {
					t.Fatalf("opening the class front door: %v", err)
				}
				if !routed {
					t.Fatal("the class front door reported no relocated class on a split city")
				}
				if err := door.Graph.Close(id); err != nil {
					t.Fatalf("closing %s through the class front door: %v", id, err)
				}
			},
		},
		{name: "orders", close: closeThroughClassResolver(resolveOrderStore)},
		{name: "nudges", close: closeThroughClassResolver(resolveNudgesStore)},
		{name: "messaging", close: closeThroughClassResolver(resolveMailMessagesStore)},
		{name: "sessions", close: closeThroughClassResolver(resolveSessionStore)},
	} {
		t.Run(row.name, func(t *testing.T) {
			cityPath := t.TempDir()
			leaf := beads.NewMemStore()
			seedCLIStorageRoutes(t, cityPath, splitClassRoutes(leaf).withCLIEmission(cityPath))

			bead := seedClassBead(t, leaf, "implement")
			if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 0 {
				t.Fatalf("seeding the leaf store emitted %d bead event(s); the fixture must be silent", len(got))
			}

			row.close(t, cityPath, bead.ID)

			got := beadEvents(readCityJournal(t, cityPath))
			if len(got) != 1 {
				t.Fatalf("a class-store close appended %d bead event(s), want exactly 1: %s", len(got), eventSummary(got))
			}
			evt := got[0]
			if evt.Type != events.BeadClosed {
				t.Errorf("event type = %q, want %q", evt.Type, events.BeadClosed)
			}
			if evt.Subject != bead.ID {
				t.Errorf("event subject = %q, want %q", evt.Subject, bead.ID)
			}
			if evt.RunID != "gcg-root-1" {
				t.Errorf("event run id = %q, want the run chain's root %q", evt.RunID, "gcg-root-1")
			}
			if evt.StepID != "implement" {
				t.Errorf("event step id = %q, want %q", evt.StepID, "implement")
			}
			if evt.SessionID != "sess-1" {
				t.Errorf("event session id = %q, want %q", evt.SessionID, "sess-1")
			}
			snapshot, ok := beads.DecodeBeadEventPayload(evt.Payload)
			if !ok {
				t.Fatalf("the payload does not decode through the shared bead decoder: %s", evt.Payload)
			}
			if snapshot.ID != bead.ID {
				t.Errorf("payload id = %q, want %q", snapshot.ID, bead.ID)
			}
			if !strings.EqualFold(snapshot.Status, "closed") {
				t.Errorf("payload status = %q, want closed: a fold that reads this decides the step is still running", snapshot.Status)
			}
		})
	}
}

// closeThroughClassResolver closes a bead through one class's CLI resolver,
// entered the way a one-shot command enters it: through the memoized funnel,
// not through routes the test hands in directly.
func closeThroughClassResolver(resolve func(*storageRoutes, beads.Store, *config.City, string, events.Recorder) beads.Store) func(*testing.T, string, string) {
	return func(t *testing.T, cityPath, id string) {
		t.Helper()
		store := resolve(cliStorageRoutes(cityPath), beads.NewMemStore(), nil, cityPath, nil)
		if err := store.Close(id); err != nil {
			t.Fatalf("closing %s through the class resolver: %v", id, err)
		}
	}
}

// AtomicConditionalCloserFor is a hard capability gate, not a rollout seam. The
// emitting class-store wrapper carries CloseWithMetadataIfMatch structurally for
// every engine — TestEmittingClassStoreKeepsEveryEngineCapability forces that,
// because *NativeDoltStore has it — so a bare type assertion would advertise
// atomic close even over a backing (the sqlite CLI engine) that cannot honor it.
// The wrapper's AtomicConditionalCloserHandle keeps discovery honest: yes only
// when the resolved backing truly provides atomic close, and the closer it hands
// back is the emitting wrapper itself, so a DISCOVERED atomic close still appends
// bead.closed rather than silently going dark on the binding.
func TestEmittingClassStoreAtomicCloseHonorsBackingCapability(t *testing.T) {
	t.Run("an atomic backing yields an emitting closer that appends bead.closed", func(t *testing.T) {
		cityPath := t.TempDir()
		leaf := beads.NewAtomicCloseMemStore()
		wrapped := splitClassRoutes(leaf).withCLIEmission(cityPath).stores[coordclass.ClassGraph]

		closer, ok := beads.AtomicConditionalCloserFor(wrapped)
		if !ok {
			t.Fatal("AtomicConditionalCloserFor(wrapper over an atomic backing) = unavailable, want the emitting wrapper's closer")
		}

		bead := seedClassBead(t, leaf, "atomic-close")
		if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 0 {
			t.Fatalf("seeding the leaf emitted %d bead event(s); the fixture must be silent", len(got))
		}

		closed, err := closer.CloseWithMetadataIfMatch(bead.ID, bead.Revision, map[string]string{"state": "drained"})
		if err != nil {
			t.Fatalf("CloseWithMetadataIfMatch: %v", err)
		}
		if !strings.EqualFold(closed.Status, "closed") || closed.Metadata["state"] != "drained" {
			t.Fatalf("returned bead = %#v, want a closed row carrying the merged metadata", closed)
		}

		got := beadEvents(readCityJournal(t, cityPath))
		if len(got) != 1 {
			t.Fatalf("a discovered atomic close appended %d bead event(s), want exactly 1: %s", len(got), eventSummary(got))
		}
		if got[0].Type != events.BeadClosed || got[0].Subject != bead.ID {
			t.Errorf("event = %q on %q, want %q on %q", got[0].Type, got[0].Subject, events.BeadClosed, bead.ID)
		}
		snapshot, ok := beads.DecodeBeadEventPayload(got[0].Payload)
		if !ok || !strings.EqualFold(snapshot.Status, "closed") {
			t.Errorf("payload = %s, want a decodable closed snapshot", got[0].Payload)
		}
	})

	t.Run("a non-atomic backing refuses discovery even though the wrapper carries the method", func(t *testing.T) {
		cityPath := t.TempDir()
		wrapped := splitClassRoutes(beads.NewMemStore()).withCLIEmission(cityPath).stores[coordclass.ClassGraph]

		if _, ok := any(wrapped).(beads.AtomicConditionalCloser); !ok {
			t.Fatal("the emitting wrapper must carry CloseWithMetadataIfMatch structurally; the engine-parity gate requires it")
		}
		if closer, ok := beads.AtomicConditionalCloserFor(wrapped); ok || closer != nil {
			t.Fatalf("AtomicConditionalCloserFor(wrapper over a non-atomic backing) = (%v, %v), want (nil, false): the seam is a hard capability gate, and a bare type assertion would answer yes here", closer, ok)
		}
	})
}

// GATE 2 (the control). The CONTROLLER's routes — the ones openStorageRoutes
// builds, which never carry an emit target — stay silent under a
// reconcile-shaped absorption, even when the resolver is handed a live
// recorder. The controller reaches its own emission through the CachingStore;
// a second emitter on this path is a double-emit, and on the reconcile path it
// is the cache-reconcile flood.
//
// The control is falsifiable rather than vacuous: the same mutations through
// the CLI-emitting twin of the same routes DO land in the same journal, so a
// fixture that could not observe an event fails here before it can certify
// silence.
func TestControllerClassRoutesStayEventSilentUnderReconcileShapedWrites(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewMemStore()
	controllerRoutes := splitClassRoutes(leaf)

	if controllerRoutes.emitCityPath != "" {
		t.Fatalf("controller-shaped routes carry emit target %q; only the one-shot funnel may set one", controllerRoutes.emitCityPath)
	}

	// A live recorder, exactly as the controller passes one to the resolvers.
	rec, err := events.NewFileRecorder(cityJournalPath(cityPath), io.Discard)
	if err != nil {
		t.Fatalf("opening the city journal: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	absorbed := seedClassBead(t, leaf, "absorb")
	store := resolveGraphStore(controllerRoutes, beads.NewMemStore(), nil, cityPath, rec)

	// The reconcile shape: the runtime re-writes rows it just read.
	closed := "closed"
	if err := store.Update(absorbed.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("reconcile-shaped update: %v", err)
	}
	if err := store.SetMetadata(absorbed.ID, "gc.absorbed", "1"); err != nil {
		t.Fatalf("reconcile-shaped metadata write: %v", err)
	}

	if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 0 {
		t.Fatalf("the controller's class routes appended %d bead event(s), want 0: %s", len(got), eventSummary(got))
	}

	// The control's own control: prove this fixture CAN see an emission, so the
	// zero above is a fact about the controller path and not about the harness.
	emitting := splitClassRoutes(leaf).withCLIEmission(cityPath)
	observable := seedClassBead(t, leaf, "observable")
	if err := resolveGraphStore(emitting, beads.NewMemStore(), nil, cityPath, nil).Close(observable.ID); err != nil {
		t.Fatalf("closing through the emitting twin: %v", err)
	}
	witness := beadEvents(readCityJournal(t, cityPath))
	if len(witness) != 1 || witness[0].Subject != observable.ID {
		t.Fatalf("the emitting twin appended %d bead event(s) for %s, want exactly 1: this fixture cannot observe emission, so the silence assertion above proves nothing", len(witness), observable.ID)
	}
}

// The structural half of gate 2: the emit target is injected in exactly one
// place, ON the resolved routes, WITH the city path. "The controller never
// emits" is a claim about the call graph, and a second injection site is how a
// claim like that stops being true without anybody noticing.
//
// It checks the argument and the receiver, not just the name, because the first
// cut of this guard checked neither and a mutant survived it: rewriting the
// shipped call as withCLIEmission("") kept it green while production emission
// was entirely dead (the empty path early-returns unwrapped). A syntactic guard
// a semantic mutant walks past is worse than none, since it reads as coverage.
// The gates that actually prove the wiring are the two production-seam tests
// below; this one keeps the SHAPE from drifting.
func TestClassStoreEmitTargetHasExactlyOneInjectionSite(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading cmd/gc: %v", err)
	}
	fset := token.NewFileSet()
	sites := map[string]int{}
	var shape []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "withCLIEmission" {
				return true
			}
			sites[name]++
			// The receiver must be the routes value the gate just resolved,
			// and the argument must be the city path the funnel was asked
			// about — never a literal, which is how an injection gets
			// silently neutered while still reading as one.
			if _, ok := sel.X.(*ast.Ident); !ok {
				shape = append(shape, fmt.Sprintf("%s: receiver is %T, want the resolved routes identifier", name, sel.X))
			}
			if len(call.Args) != 1 {
				shape = append(shape, fmt.Sprintf("%s: %d argument(s), want exactly the city path", name, len(call.Args)))
				return true
			}
			arg, ok := call.Args[0].(*ast.Ident)
			if !ok {
				shape = append(shape, fmt.Sprintf("%s: argument is %T, want the cityPath identifier (a literal cannot be the city the funnel was asked about)", name, call.Args[0]))
				return true
			}
			if arg.Name != "cityPath" {
				shape = append(shape, fmt.Sprintf("%s: argument is %q, want cityPath", name, arg.Name))
			}
			return true
		})
	}
	if len(sites) != 1 || sites["cli_storage_routes.go"] != 1 {
		t.Fatalf("withCLIEmission is called from %v, want exactly one call in cli_storage_routes.go: the controller path is untouched only while the one-shot funnel is the sole injector", sites)
	}
	if len(shape) > 0 {
		t.Fatalf("the injection does not have the shape that makes it one:\n  %s", strings.Join(shape, "\n  "))
	}
}

// GATE 3. What the door mutation appends is readable by a journal CURSOR
// consumer — read from a recorded position forward, which is how the
// event-delta lanes consume bead.* — with the run/step correlation on the
// envelope rather than buried in the payload.
func TestClassStoreEmissionIsVisibleToACursorConsumer(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewMemStore()
	seedCLIStorageRoutes(t, cityPath, splitClassRoutes(leaf).withCLIEmission(cityPath))

	consumer, err := events.NewFileRecorder(cityJournalPath(cityPath), io.Discard)
	if err != nil {
		t.Fatalf("opening the journal consumer: %v", err)
	}
	t.Cleanup(func() { _ = consumer.Close() })

	// Something already in the log, so the cursor is a real position and not 0.
	consumer.Record(events.Event{Type: "city.started", Actor: "test"})
	cursor, err := consumer.LatestSeq()
	if err != nil {
		t.Fatalf("reading the cursor: %v", err)
	}
	if cursor == 0 {
		t.Fatal("cursor is 0 after a recorded event")
	}

	bead := seedClassBead(t, leaf, "verify")
	door, routed, err := openBdByIDClassFrontDoor(cityPath)
	if err != nil || !routed {
		t.Fatalf("opening the class front door: routed=%v err=%v", routed, err)
	}
	if err := door.Graph.Close(bead.ID); err != nil {
		t.Fatalf("closing %s through the class front door: %v", bead.ID, err)
	}

	delta, err := consumer.List(events.Filter{AfterSeq: cursor})
	if err != nil {
		t.Fatalf("reading the delta: %v", err)
	}
	got := beadEvents(delta)
	if len(got) != 1 {
		t.Fatalf("a consumer resuming at seq %d saw %d bead event(s), want exactly 1: %s", cursor, len(got), eventSummary(got))
	}
	if got[0].Seq <= cursor {
		t.Errorf("delta event seq = %d, want > cursor %d: a row at or below the cursor is invisible to a resuming lane", got[0].Seq, cursor)
	}
	if got[0].RunID == "" || got[0].StepID == "" {
		t.Errorf("delta event carries run=%q step=%q; a delta lane keys on both", got[0].RunID, got[0].StepID)
	}
}

// A close is a close only when the bead was open. eventexport drops
// bead.updated, so a close edge that rode bead.updated would never reach a
// consumer — and a metadata write to an already-closed bead that rode
// bead.closed would re-close it in every fold that replays the log.
func TestClassStoreEmissionPromotesToClosedOnlyOnTheTransition(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewMemStore()
	routes := splitClassRoutes(leaf).withCLIEmission(cityPath)
	store := resolveGraphStore(routes, beads.NewMemStore(), nil, cityPath, nil)

	bead := seedClassBead(t, leaf, "transition")
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := store.SetMetadata(bead.ID, "gc.note", "after"); err != nil {
		t.Fatalf("metadata write on a closed bead: %v", err)
	}

	got := beadEvents(readCityJournal(t, cityPath))
	if len(got) != 2 {
		t.Fatalf("got %d bead event(s), want 2: %s", len(got), eventSummary(got))
	}
	if got[0].Type != events.BeadClosed {
		t.Errorf("first event = %q, want %q", got[0].Type, events.BeadClosed)
	}
	if got[1].Type != events.BeadUpdated {
		t.Errorf("second event = %q, want %q: a metadata write to a closed bead is not a close", got[1].Type, events.BeadUpdated)
	}
}

// Create and delete are the other two lifecycle edges a fold needs. Delete has
// to carry the PRE-delete snapshot, because there is nothing to read after.
func TestClassStoreEmissionCoversCreateAndDelete(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewMemStore()
	routes := splitClassRoutes(leaf).withCLIEmission(cityPath)
	store := resolveGraphStore(routes, beads.NewMemStore(), nil, cityPath, nil)

	created, err := store.Create(beads.Bead{Title: "fresh", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Delete(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	got := beadEvents(readCityJournal(t, cityPath))
	if len(got) != 2 {
		t.Fatalf("got %d bead event(s), want 2: %s", len(got), eventSummary(got))
	}
	if got[0].Type != events.BeadCreated || got[0].Subject != created.ID {
		t.Errorf("first event = %q/%q, want %q/%q", got[0].Type, got[0].Subject, events.BeadCreated, created.ID)
	}
	if got[1].Type != events.BeadDeleted || got[1].Subject != created.ID {
		t.Errorf("second event = %q/%q, want %q/%q", got[1].Type, got[1].Subject, events.BeadDeleted, created.ID)
	}
	snapshot, ok := beads.DecodeBeadEventPayload(got[1].Payload)
	if !ok || snapshot.Title != "fresh" {
		t.Errorf("bead.deleted payload = %s, want the pre-delete snapshot", got[1].Payload)
	}
}

// A city that relocates nothing must reach none of this: the resolver hands
// back the caller's own store VALUE, and no journal appears.
func TestSingleStoreCityIsUntouchedByTheEmitSeam(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()

	var none *storageRoutes
	if got := none.withCLIEmission(cityPath); got != nil {
		t.Fatalf("withCLIEmission on nil routes = %v, want nil: a city that relocates nothing has no class store to wrap", got)
	}
	store := resolveGraphStore(none.withCLIEmission(cityPath), work, nil, cityPath, nil)
	if store != beads.Store(work) {
		t.Fatal("the resolver did not return the caller's own store on a city that relocates nothing")
	}
	if _, err := store.Create(beads.Bead{Title: "work", Type: "task"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := os.Stat(cityJournalPath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("a single-store city grew an event log at %s", cityJournalPath(cityPath))
	}
}

// Store identity has to survive the wrap: callers dedup scan candidates in a
// map keyed by beads.Store, so a fresh wrapper per resolution would turn one
// store into N and re-scan the binding once per class.
func TestEmittingClassStoreIsIdentityStableAcrossResolutions(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewMemStore()
	routes := splitClassRoutes(leaf).withCLIEmission(cityPath)

	graph := resolveGraphStore(routes, beads.NewMemStore(), nil, cityPath, nil)
	again := resolveGraphStore(routes, beads.NewMemStore(), nil, cityPath, nil)
	if graph != again {
		t.Error("two resolutions of the same class returned different store values")
	}
	orders := resolveOrderStore(routes, beads.NewMemStore(), nil, cityPath, nil)
	if graph != orders {
		t.Error("two classes served by one binding resolved to different store values; the store-identity dedup counts them twice")
	}
	if graph == beads.Store(leaf) {
		t.Error("the resolved class store is the un-augmented leaf; nothing would emit")
	}
}

// Wrapping a store is how capabilities get lost, and a lost capability is
// silent: the caller's type assertion simply stops matching and it takes a
// slower or weaker path. Both engines a binding can be served from are pinned.
func TestEmittingClassStoreKeepsEveryEngineCapability(t *testing.T) {
	cityPath := t.TempDir()
	wrapped := splitClassRoutes(beads.NewMemStore()).withCLIEmission(cityPath).stores[coordclass.ClassGraph]
	wrapper := reflect.TypeOf(wrapped)

	for _, engine := range []reflect.Type{
		reflect.TypeOf(&beads.SQLiteStore{}),
		reflect.TypeOf(&beads.NativeDoltStore{}),
	} {
		var missing []string
		for i := 0; i < engine.NumMethod(); i++ {
			method := engine.Method(i)
			got, ok := wrapper.MethodByName(method.Name)
			if !ok {
				missing = append(missing, method.Name)
				continue
			}
			// Compare the signatures without their receivers.
			if got.Type.String() != strings.Replace(method.Type.String(), engine.String(), wrapper.String(), 1) {
				missing = append(missing, method.Name+" (signature differs)")
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("the emitting class store drops %v from %s; every one is a capability assertion that stops matching", missing, engine)
		}
	}
}

func TestEmittingClassStoreForwardsAtomicCommentImportAndSnapshot(t *testing.T) {
	cityPath := t.TempDir()
	leaf := &updateCASAtomicCommentStore{MemStore: beads.NewMemStore()}
	created := seedClassBead(t, leaf, "comment-step")
	wrapped := splitClassRoutes(leaf).withCLIEmission(cityPath).stores[coordclass.ClassGraph]
	reader, ok := beads.IssueGraphSnapshotFor(wrapped)
	if !ok {
		t.Fatal("emitting wrapper dropped atomic graph snapshot")
	}
	rows, edges, err := reader.IssueGraphSnapshot(beads.ListQuery{AllowScan: true})
	if err != nil || len(rows) != 1 || rows[0].ID != created.ID || len(edges) != 1 {
		t.Fatalf("snapshot = (%v, %v, %v), want exact backing graph", rows, edges, err)
	}
	importer, ok := beads.AtomicCommentImporterFor(wrapped)
	if !ok {
		t.Fatal("emitting wrapper dropped atomic comment import")
	}
	comments := []beads.ImportedComment{{ExternalID: "IC_1", Author: "github-human", Text: "comment", CreatedAt: time.Now().UTC()}}
	if err := importer.UpdateWithCommentsIfMatch(created.ID, created.Revision, beads.UpdateOpts{}, comments); err != nil {
		t.Fatal(err)
	}
	if len(leaf.imported) != 1 {
		t.Fatalf("imported %d comments, want 1", len(leaf.imported))
	}
	if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 1 || got[0].Type != events.BeadUpdated {
		t.Fatalf("events = %v, want exactly one committed update", got)
	}
}

func TestEmittingClassStoreDoesNotAdvertiseUnsupportedSyncCapabilities(t *testing.T) {
	leaf := &struct{ beads.Store }{Store: beads.NewMemStore()}
	wrapped := splitClassRoutes(leaf).withCLIEmission(t.TempDir()).stores[coordclass.ClassGraph]
	if _, ok := beads.IssueGraphSnapshotFor(wrapped); ok {
		t.Fatal("advertised snapshot over an unsupported backing")
	}
	if _, ok := beads.AtomicCommentImporterFor(wrapped); ok {
		t.Fatal("advertised atomic comment import over an unsupported backing")
	}
}

type exactStatusCommentStore struct{ *updateCASAtomicCommentStore }

func (s *exactStatusCommentStore) Get(id string) (beads.Bead, error) {
	row, err := s.MemStore.Get(id)
	if row.Status == "blocked" {
		row.Status = "open"
	}
	return row, err
}

func (s *exactStatusCommentStore) ReadUpdateCASBead(id string) (beads.Bead, error) {
	return s.MemStore.Get(id)
}

func TestEmittingClassStorePreservesExactStatusForCommentCASReadback(t *testing.T) {
	cityPath := t.TempDir()
	leaf := &exactStatusCommentStore{&updateCASAtomicCommentStore{MemStore: beads.NewMemStore()}}
	created := seedClassBead(t, leaf, "exact-status")
	wrapped := splitClassRoutes(leaf).withCLIEmission(cityPath).stores[coordclass.ClassGraph]
	comments := []beads.ImportedComment{{ExternalID: "IC_1", Author: "github-human", Text: "comment", CreatedAt: time.Now().UTC()}}
	blocked := "blocked"
	result, err := beads.ApplyUpdateCASWithComments(wrapped, created.ID, created.Revision, beads.UpdateOpts{Status: &blocked}, comments)
	if err != nil || result.Outcome != beads.UpdateCASUpdated {
		t.Fatalf("blocked update = %+v, %v; want exact committed readback", result, err)
	}
	replay, err := beads.ApplyUpdateCASWithComments(wrapped, created.ID, created.Revision, beads.UpdateOpts{Status: &blocked}, comments)
	if err != nil || replay.Outcome != beads.UpdateCASAlreadyApplied {
		t.Fatalf("blocked replay = %+v, %v", replay, err)
	}
	open := "open"
	conflict, err := beads.ApplyUpdateCASWithComments(wrapped, created.ID, created.Revision, beads.UpdateOpts{Status: &open}, comments)
	if err != nil || conflict.Outcome != beads.UpdateCASConflict {
		t.Fatalf("stale open request = %+v, %v; must not normalize blocked to open", conflict, err)
	}
	if len(leaf.imported) != 1 {
		t.Fatalf("imported %d comments, want one", len(leaf.imported))
	}
	if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 1 || got[0].Type != events.BeadUpdated {
		t.Fatalf("events = %v, want one committed update", got)
	}
}

// Emission is best-effort by contract: the mutation has already committed when
// the event is written, so a journal that cannot be opened must not turn a
// landed close into a failed command.
func TestClassStoreEmissionFailureDoesNotFailTheMutation(t *testing.T) {
	cityPath := t.TempDir()
	// A regular file where the runtime directory belongs: the recorder's
	// MkdirAll fails, and every open after it.
	if err := os.WriteFile(filepath.Join(cityPath, citylayout.RuntimeRoot), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seeding the blocked runtime root: %v", err)
	}
	leaf := beads.NewMemStore()
	routes := splitClassRoutes(leaf).withCLIEmission(cityPath)
	store := resolveGraphStore(routes, beads.NewMemStore(), nil, cityPath, nil)

	var warned []string
	restore := classStoreEmitWarn
	classStoreEmitWarn = func(err error) { warned = append(warned, err.Error()) }
	t.Cleanup(func() { classStoreEmitWarn = restore })

	bead := seedClassBead(t, leaf, "besteffort")
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("close returned %v; emission is best-effort and must not fail a committed mutation", err)
	}
	if got, err := leaf.Get(bead.ID); err != nil || !strings.EqualFold(got.Status, "closed") {
		t.Fatalf("the close did not land: bead=%+v err=%v", got, err)
	}
	if len(warned) == 0 {
		t.Error("a journal that cannot be opened produced no diagnostic; a dropped terminal event must not be silent")
	}
}

// The controller's payload carries dependency edges; a class store's row does
// not carry them in its bead JSON, so the emitter hydrates them. Without this a
// consumer holding both shapes reads the CLI's payload as an edge REMOVAL.
func TestClassStoreEmissionHydratesDependencyEdges(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewMemStore()
	store := resolveGraphStore(splitClassRoutes(leaf).withCLIEmission(cityPath), beads.NewMemStore(), nil, cityPath, nil)

	blocker := seedClassBead(t, leaf, "blocker")
	blocked := seedClassBead(t, leaf, "blocked")
	if err := store.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("dep add: %v", err)
	}

	got := beadEvents(readCityJournal(t, cityPath))
	if len(got) != 1 || got[0].Type != events.BeadUpdated {
		t.Fatalf("got %d bead event(s) %s, want one bead.updated", len(got), eventSummary(got))
	}
	snapshot, ok := beads.DecodeBeadEventPayload(got[0].Payload)
	if !ok {
		t.Fatalf("payload does not decode: %s", got[0].Payload)
	}
	if len(snapshot.Dependencies) != 1 || snapshot.Dependencies[0].DependsOnID != blocker.ID {
		t.Errorf("payload dependencies = %+v, want the edge just added to %s", snapshot.Dependencies, blocker.ID)
	}
}

// A post-write read miss must never produce a bare-id payload. An empty
// snapshot does not say less than a full one — it CLOBBERS the fold, because a
// projection applying it overwrites title, status and run membership with
// nothing. The one exception is a close, whose transition is proven committed.
func TestClassStoreEmissionNeverEmitsABareIDPayload(t *testing.T) {
	cityPath := t.TempDir()
	leaf := &readMissAfterWriteStore{MemStore: beads.NewMemStore()}
	store := resolveGraphStore(splitClassRoutes(leaf).withCLIEmission(cityPath), beads.NewMemStore(), nil, cityPath, nil)

	var warned []string
	restore := classStoreEmitWarn
	classStoreEmitWarn = func(err error) { warned = append(warned, err.Error()) }
	t.Cleanup(func() { classStoreEmitWarn = restore })

	bead := seedClassBead(t, leaf, "readmiss")
	leaf.missing = true

	if err := store.SetMetadata(bead.ID, "gc.note", "x"); err != nil {
		t.Fatalf("metadata write: %v", err)
	}
	if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 0 {
		t.Fatalf("a refresh miss emitted %s; a bare-id payload detaches the bead from its run", eventSummary(got))
	}
	if len(warned) == 0 {
		t.Error("a skipped emission produced no diagnostic; a dropped row must not be silent")
	}

	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := beadEvents(readCityJournal(t, cityPath))
	if len(got) != 1 || got[0].Type != events.BeadClosed {
		t.Fatalf("got %s, want one bead.closed: a committed close is a fact and must survive a refresh miss", eventSummary(got))
	}
	snapshot, ok := beads.DecodeBeadEventPayload(got[0].Payload)
	if !ok || !strings.EqualFold(snapshot.Status, "closed") {
		t.Errorf("bead.closed payload = %s, want a closed status from the write's own transition", got[0].Payload)
	}
}

// readMissAfterWriteStore models a store whose read-after-write lags: the
// mutation lands and the refresh finds nothing.
type readMissAfterWriteStore struct {
	*beads.MemStore
	missing bool
}

func (s *readMissAfterWriteStore) Get(id string) (beads.Bead, error) {
	if s.missing {
		return beads.Bead{}, beads.ErrNotFound
	}
	return s.MemStore.Get(id)
}

// A landed conditional release is a state change a fold has to see: the bead
// went from claimed to claimable, and a reader that missed it keeps showing an
// owner who has walked away.
func TestClassStoreEmissionCoversConditionalRelease(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewMemStore()
	store := resolveGraphStore(splitClassRoutes(leaf).withCLIEmission(cityPath), beads.NewMemStore(), nil, cityPath, nil)

	bead := seedClassBead(t, leaf, "release")
	assignee := "worker-1"
	inProgress := "in_progress"
	if err := leaf.Update(bead.ID, beads.UpdateOpts{Assignee: &assignee, Status: &inProgress}); err != nil {
		t.Fatalf("claiming the bead: %v", err)
	}

	releaser, ok := store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		t.Fatalf("the emitting class store dropped ConditionalAssignmentReleaser (%T)", store)
	}
	released, err := releaser.ReleaseIfCurrent(bead.ID, assignee)
	if err != nil || !released {
		t.Fatalf("release: released=%v err=%v", released, err)
	}
	got := beadEvents(readCityJournal(t, cityPath))
	if len(got) != 1 || got[0].Type != events.BeadUpdated || got[0].Subject != bead.ID {
		t.Fatalf("got %s, want one bead.updated for %s", eventSummary(got), bead.ID)
	}

	// A release that does not fire changes nothing, and must say nothing.
	if _, err := releaser.ReleaseIfCurrent(bead.ID, "someone-else"); err != nil {
		t.Fatalf("no-op release: %v", err)
	}
	if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 1 {
		t.Fatalf("a release that did not fire appended a row: %s", eventSummary(got))
	}
}

// A landed atomic fenced close — merge metadata and close in one revision-guarded
// transaction — is a terminal transition a fold has to see. The capability is
// discovered through AtomicConditionalCloserFor, and it must resolve to the
// EMITTING wrapper: a bare backing would close silently and leave the run view
// rendering the step running forever, the exact silence this seam ends.
func TestClassStoreEmissionCoversAtomicMetadataClose(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewAtomicCloseMemStore()
	store := resolveGraphStore(splitClassRoutes(leaf).withCLIEmission(cityPath), beads.NewMemStore(), nil, cityPath, nil)

	bead := seedClassBead(t, leaf, "finalize")
	seeded, err := leaf.Get(bead.ID)
	if err != nil {
		t.Fatalf("reading the seeded revision: %v", err)
	}

	closer, ok := beads.AtomicConditionalCloserFor(store)
	if !ok {
		t.Fatalf("AtomicConditionalCloserFor did not discover the capability on the emitting wrapper (%T)", store)
	}
	closed, err := closer.CloseWithMetadataIfMatch(bead.ID, seeded.Revision, map[string]string{"gc.outcome": "pass"})
	if err != nil {
		t.Fatalf("atomic fenced close: %v", err)
	}
	if !beadStatusIsClosed(closed.Status) {
		t.Errorf("returned bead status = %q, want closed", closed.Status)
	}
	if closed.Metadata["gc.outcome"] != "pass" {
		t.Errorf("returned bead metadata = %v, want the merged gc.outcome=pass", closed.Metadata)
	}

	got := beadEvents(readCityJournal(t, cityPath))
	if len(got) != 1 || got[0].Type != events.BeadClosed || got[0].Subject != bead.ID {
		t.Fatalf("got %s, want one bead.closed for %s", eventSummary(got), bead.ID)
	}
	// The committed row IS the payload, emitted without a re-read, so it carries
	// the merged metadata and the closed status the transaction established.
	snapshot, ok := beads.DecodeBeadEventPayload(got[0].Payload)
	if !ok || snapshot.Metadata["gc.outcome"] != "pass" || !beadStatusIsClosed(snapshot.Status) {
		t.Errorf("bead.closed payload = %s, want the committed closed row with gc.outcome=pass", got[0].Payload)
	}

	// A fence that no longer matches commits nothing, and must say nothing: the
	// first close bumped the revision, so replaying the stale one is refused.
	if _, err := closer.CloseWithMetadataIfMatch(bead.ID, seeded.Revision, map[string]string{"gc.outcome": "fail"}); err == nil {
		t.Fatal("a stale-revision fenced close reported success")
	}
	if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 1 {
		t.Fatalf("a fenced close that did not fire appended a row: %s", eventSummary(got))
	}
}

// Atomic close is a hard capability gate, not a rollout seam. The wrapper is
// forced to carry CloseWithMetadataIfMatch structurally for every engine
// (TestEmittingClassStoreKeepsEveryEngineCapability), so a bare type assertion
// would advertise the capability even over a backing that cannot honor the
// all-or-nothing close. AtomicConditionalCloserFor must consult the resolved
// backing and answer no when it lacks the atomic terminal write.
func TestEmittingClassStoreRefusesAtomicCloseOverANonAtomicBacking(t *testing.T) {
	cityPath := t.TempDir()
	// Plain MemStore deliberately does not expose the atomic terminal close.
	store := resolveGraphStore(splitClassRoutes(beads.NewMemStore()).withCLIEmission(cityPath), beads.NewMemStore(), nil, cityPath, nil)
	if _, ok := beads.AtomicConditionalCloserFor(store); ok {
		t.Fatal("the emitting wrapper advertised atomic close over a backing that cannot honor it")
	}
}

// The recorder reports its own trouble — a flock timeout that dropped a row, a
// short write — to a stderr sink rather than an error return. That sink is
// wired into the warn path, because a dropped terminal event nobody hears about
// is the exact failure this seam exists to end.
func TestClassStoreEmitWarnWriterFunnelsRecorderDiagnostics(t *testing.T) {
	var warned []string
	restore := classStoreEmitWarn
	classStoreEmitWarn = func(err error) { warned = append(warned, err.Error()) }
	t.Cleanup(func() { classStoreEmitWarn = restore })

	writer := classStoreEmitWarnWriter{}
	msg := []byte("events: append failed: no space left on device\n")
	n, err := writer.Write(msg)
	if err != nil || n != len(msg) {
		t.Fatalf("Write = (%d, %v), want (%d, nil): a recorder stderr sink must report a full write", n, err, len(msg))
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "no space left on device") {
		t.Fatalf("warnings = %v, want the recorder's own diagnostic", warned)
	}
	if n, err := writer.Write([]byte("   \n")); err != nil || n != 4 {
		t.Fatalf("blank Write = (%d, %v), want (4, nil)", n, err)
	}
	if len(warned) != 1 {
		t.Errorf("a blank diagnostic produced a warning: %v", warned)
	}
}

// eventSummary renders a journal slice for a failure message.
func eventSummary(all []events.Event) string {
	if len(all) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(all))
	for _, e := range all {
		payload, _ := json.Marshal(e.Subject)
		parts = append(parts, e.Type+"/"+string(payload))
	}
	return strings.Join(parts, ", ")
}

// The dispatcher's serve loop must not wake on its OWN emissions. Its
// control-bead writes now append bead.* rows, and an unfiltered wake buys a
// heavy ready re-scan and a controller poke per dispatch burst — the loop
// paying itself to re-read what it just wrote.
func TestWorkflowServeWakeIgnoresSelfActorEvents(t *testing.T) {
	prevDebounce := workflowServeWakeDebounce
	workflowServeWakeDebounce = time.Millisecond
	t.Cleanup(func() { workflowServeWakeDebounce = prevDebounce })

	prevActor := workflowServeSelfActor
	t.Cleanup(func() { workflowServeSelfActor = prevActor })

	deliver := func(actor string) chan workflowWatchResult {
		ch := make(chan workflowWatchResult, 1)
		ch <- workflowWatchResult{evt: events.Event{Type: events.BeadClosed, Subject: "gcg-1", Actor: actor}}
		return ch
	}

	workflowServeSelfActor = func() (string, bool) { return "dispatcher-1", true }
	if wake, err := waitForRelevantWorkflowWakeWithTrace(deliver("dispatcher-1"), 15*time.Millisecond, -1); err != nil || wake {
		t.Fatalf("self-actor event: wake=%v err=%v, want no wake", wake, err)
	}
	if wake, err := waitForRelevantWorkflowWakeWithTrace(deliver("worker-7"), time.Second, -1); err != nil || !wake {
		t.Fatalf("foreign-actor event: wake=%v err=%v, want a wake", wake, err)
	}

	// The fallback identity is shared by every foreign CLI writer, so it must
	// never filter: suppressing a wake there would strand ready work.
	workflowServeSelfActor = func() (string, bool) { return "human", false }
	if wake, err := waitForRelevantWorkflowWakeWithTrace(deliver("human"), time.Second, -1); err != nil || !wake {
		t.Fatalf("unusable identity: wake=%v err=%v, want a wake with no self-filter", wake, err)
	}
}

// class_store_emit.go is on the claim-CAS allowlist
// (turn_bound_claim_invariants_test.go) as a FORWARDER, not as a pull path.
// That exception is only true while the file's single claim call is the one
// inside its own Claim method, so it is checked rather than promised.
func TestClassStoreEmitOnlyForwardsClaims(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "class_store_emit.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing class_store_emit.go: %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "Claim" {
				return true
			}
			if fn.Name.Name != "Claim" {
				t.Errorf("%s claims; this file may only forward a claim from its own Claim method, and the allowlist entry says so", fn.Name.Name)
			}
			return true
		})
	}
}

// The claim capability is why the wrapper carries Claim at all: the hook's
// binding probe is a type assertion, and a wrapper that dropped the method
// would degrade `gc hook --claim` to "this binding cannot claim" on every split
// city — silently, because that degrade is a designed fallback.
func TestEmittingClassStoreStillSatisfiesTheHookClaimRoute(t *testing.T) {
	cityPath := t.TempDir()
	wrapped := splitClassRoutes(beads.NewMemStore()).withCLIEmission(cityPath).stores[coordclass.ClassGraph]
	if _, err := newHookClaimClassRoute(wrapped); err != nil {
		t.Fatalf("the hook claim route refused the emitting class store: %v", err)
	}
}

// PRODUCTION SEAM, gate 1. The gates above seed the funnel memo with routes the
// TEST already wrapped, which proves the wrapper and proves nothing about the
// wiring: replacing the shipped injection with withCLIEmission("") leaves them
// all green while production emission is entirely dead.
//
// This one never says withCLIEmission. It stands up a city that has converged
// onto its binding and enters through the same doors a one-shot command enters
// through — the `gc mail` provider root and the by-id class front door — so the
// only thing that can put an emit target on the store is resolveCLIStorageRoutes
// itself. It is red against a neutered injection, and red against no injection.
func TestOneShotCLIWritesEmitBeadEventsOnAMigratedCity(t *testing.T) {
	t.Run("mail send through the gc mail provider root", func(t *testing.T) {
		cityPath, _ := migratedOneShotCLICity(t)
		captureCLIStorageStderr(t)

		sender, code := openCityMailProvider(io.Discard, "gc mail send")
		if sender == nil {
			t.Fatalf("openCityMailProvider returned no provider (code=%d)", code)
		}
		sent, err := sender.Send("worker", "mayor", "PR ready", "please review the auth PR")
		if err != nil {
			t.Fatalf("gc mail send: %v", err)
		}

		created := 0
		for _, evt := range beadEvents(readCityJournal(t, cityPath)) {
			if evt.Subject == sent.ID && evt.Type == events.BeadCreated {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("a one-shot mail send on a migrated city appended %d bead.created row(s) for %s, want exactly 1: the shipped injection at resolveCLIStorageRoutes is what puts the emit target on the messaging class store",
				created, sent.ID)
		}
	})

	t.Run("graph close through the by-id class front door", func(t *testing.T) {
		cityPath, _ := migratedOneShotCLICity(t)
		captureCLIStorageStderr(t)

		door, routed, err := openBdByIDClassFrontDoor(cityPath)
		if err != nil {
			t.Fatalf("opening the class front door: %v", err)
		}
		if !routed {
			t.Fatal("a migrated city reported no relocated class at the by-id front door")
		}
		step, err := door.Graph.Create(beads.Bead{
			Title:  "implement",
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:       "check",
				beadmeta.RootBeadIDMetadataKey: "gcg-root-e2e",
				beadmeta.StepIDMetadataKey:     "implement",
			},
		})
		if err != nil {
			t.Fatalf("creating a graph-class step through the front door: %v", err)
		}
		if err := door.Graph.Close(step.ID); err != nil {
			t.Fatalf("closing %s through the front door: %v", step.ID, err)
		}

		var got []string
		for _, evt := range beadEvents(readCityJournal(t, cityPath)) {
			if evt.Subject == step.ID {
				got = append(got, evt.Type)
			}
		}
		want := []string{events.BeadCreated, events.BeadClosed}
		if !slices.Equal(got, want) {
			t.Fatalf("the one-shot graph lifecycle of %s appended %v, want %v: without the shipped injection the run projection never sees the step begin or end",
				step.ID, got, want)
		}
	})
}

// PRODUCTION SEAM, gate 2 (the control). Routes built by the REAL
// openStorageRoutes — the controller's constructor — carry no emit target and
// serve no emitting store, so a wrap added there is caught here rather than
// discovered as a double row in a city's log.
//
// The type assertion is the load-bearing half. A behavioral "the city journal
// stayed empty" would pass a mutant that wrapped with the BINDING root instead
// of the city path, because those events land somewhere this test never looks.
func TestControllerRoutesFromOpenStorageRoutesCarryNoEmitTarget(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)

	plan, err := resolveCityStoragePlan(cityPath, cfg)
	if err != nil {
		t.Fatalf("resolving the storage plan: %v", err)
	}
	routes, err := openStorageRoutes(plan, mustResolveInfraTarget(t, cityPath, cfg))
	if err != nil {
		t.Fatalf("opening the controller's storage routes: %v", err)
	}
	defer routes.close() //nolint:errcheck // the test asserts on the routes, not on the close

	if routes.emitCityPath != "" {
		t.Errorf("openStorageRoutes set an emit target %q; only the one-shot funnel may set one", routes.emitCityPath)
	}
	served := 0
	for class, store := range routes.stores {
		served++
		if _, emitting := store.(*emittingClassStore); emitting {
			t.Errorf("the controller's %s store is an emitting wrapper; the controller emits through its own CachingStore and a second emitter is a double row in the log", class)
		}
	}
	if served == 0 {
		t.Fatal("the controller's routes serve no class; this gate has lost its subject")
	}

	// And behaviorally, for the emit target this file would use.
	store := resolveGraphStore(routes, beads.NewMemStore(), cfg, cityPath, nil)
	step, err := store.Create(beads.Bead{Title: "controller write", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("creating through the controller's class store: %v", err)
	}
	if err := store.Close(step.ID); err != nil {
		t.Fatalf("closing through the controller's class store: %v", err)
	}
	if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 0 {
		t.Fatalf("the controller's class routes appended %d bead event(s), want 0: %s", len(got), eventSummary(got))
	}
}
