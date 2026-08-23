package storebinding_test

// The substitution proof for the binding GC actually ships.
//
// The settled architecture names one shape: a single canonical Beads engine,
// projected into every storage class through NewBeadsAdapters. This file runs
// the shared class corpus against that shape over SQLite. It was the evidence
// the per-class stores could be deleted, and now that they are
// it is the only place the
// six front doors are proven end to end over a real engine — so it lives in
// the tree and runs on every `go test ./internal/storebinding/...`, not in an
// overlay probe somebody has to remember to re-run.
//
// It is an EXTERNAL test package because the corpus lives in
// internal/storebinding/storebindingtest, which imports storebinding; an in-package test
// importing it would close an import cycle.
//
// HONEST CAPABILITIES. The engine underneath is beads.OpenSQLiteStore, which
// has a real SQL transaction and a real two-argument compare-and-swap claim,
// so this leg declares Transactions and Claims true and the guarded
// assertions run. The in-memory reference deliberately declares them false
// (see storebindingtest.ReferenceCapability); declaring a capability the engine
// lacks is the exact defect storebindingtest.BrokenGraphStore exists to catch, so
// the declaration below is a claim this file has to earn, not a formality.

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/storebindingtest"
)

// beadsOverSQLiteCapability is what this binding declares for every class it
// serves. See the capability note above.
var beadsOverSQLiteCapability = storebinding.ClassCapability{
	Available:    true,
	Transactions: true,
	Claims:       true,
}

// beadStoreCloser is the close half of an opened engine. beads.Store is a pure
// data contract with no Close, so the concrete engines carry it separately.
type beadStoreCloser interface{ CloseStore() error }

// openBeadsOverSQLiteEngine opens one SQLite bead engine below a per-assertion
// temp root and closes it when the assertion ends.
func openBeadsOverSQLiteEngine(tb storebindingtest.TB) beads.Store {
	tb.Helper()
	dir := tb.TempDir()
	store, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		tb.Fatalf("opening the SQLite bead engine below %s: %v", dir, err)
	}
	closer, ok := store.(beadStoreCloser)
	if !ok {
		tb.Fatalf("the SQLite bead engine %T cannot be closed; every assertion would leak a handle", store)
	}
	tb.Cleanup(func() {
		if err := closer.CloseStore(); err != nil {
			tb.Errorf("closing the SQLite bead engine below %s: %v", dir, err)
		}
	})
	return store
}

// openBeadsOverSQLite projects one fresh SQLite engine into every storage
// class, with the Beads-backed nudge queue bound as the queue front door.
// Every front door in the returned set is a view of that one engine, which is
// the unified topology the closed contracts have to answer under.
func openBeadsOverSQLite(tb storebindingtest.TB) storebinding.BeadsAdapters {
	tb.Helper()
	store := openBeadsOverSQLiteEngine(tb)
	queue, err := storebinding.NewBeadsNudgeQueue(beads.NudgesStore{Store: store})
	if err != nil {
		tb.Fatalf("binding the beads nudge queue: %v", err)
	}
	adapters, err := storebinding.NewBeadsAdapters(
		store,
		storebinding.BeadsAdapterIdentity{OpenerID: "conformance", ComponentID: "beads", PhysicalID: "sqlite"},
		queue,
	)
	if err != nil {
		tb.Fatalf("projecting the SQLite engine into the class front doors: %v", err)
	}
	return adapters
}

// beadsOverSQLiteTopology composes a two-scope Work topology whose HQ and rig
// members are distinct SQLite engines, so the grouping assertions run over
// real handles rather than in-memory stand-ins.
func beadsOverSQLiteTopology(tb storebindingtest.TB) storebinding.WorkTopology {
	tb.Helper()
	workspace := func(scope storebinding.WorkScope, prefix, physical string) storebinding.Workspace {
		return storebinding.Workspace{
			Scope:       scope,
			Store:       openBeadsOverSQLiteEngine(tb),
			Prefix:      prefix,
			OpenerID:    "conformance",
			ComponentID: "work",
			PhysicalID:  physical,
		}
	}
	topology, err := storebinding.NewWorkTopology(
		workspace(storebinding.HQScope(), "gc", "hq"),
		[]storebinding.Workspace{workspace(storebinding.RigScope("alpha"), "ga", "alpha")},
	)
	if err != nil {
		tb.Fatalf("composing the SQLite Work topology: %v", err)
	}
	return topology
}

// TestBeadsOverSQLiteRunsTheClassCorpus is the deletion license: all six
// storage-class front doors of one SQLite-backed canonical Beads binding
// answer the same corpus the deployed per-class stores answer.
func TestBeadsOverSQLiteRunsTheClassCorpus(t *testing.T) {
	t.Run("work", func(t *testing.T) {
		storebindingtest.RunWorkTopologyTests(storebindingtest.Wrap(t), storebindingtest.WorkTopologySuite{
			NewTopology:            beadsOverSQLiteTopology,
			WantPhysicalWorkspaces: 2,
		})
	})
	t.Run("graph", func(t *testing.T) {
		storebindingtest.RunGraphStoreTests(storebindingtest.Wrap(t), storebindingtest.GraphSuite{
			NewStore:   func(tb storebindingtest.TB) storebinding.GraphStore { return openBeadsOverSQLite(tb).Graph },
			Capability: beadsOverSQLiteCapability,
		})
	})
	t.Run("sessions", func(t *testing.T) {
		storebindingtest.RunSessionsStoreTests(storebindingtest.Wrap(t), storebindingtest.SessionsSuite{
			NewStore:   func(tb storebindingtest.TB) storebinding.SessionsStore { return openBeadsOverSQLite(tb).Sessions },
			Capability: beadsOverSQLiteCapability,
		})
	})
	t.Run("messaging", func(t *testing.T) {
		storebindingtest.RunMessagingFrontDoorTests(storebindingtest.Wrap(t), storebindingtest.MessagingSuite{
			NewFrontDoors: func(tb storebindingtest.TB) storebinding.MessagingFrontDoors {
				return openBeadsOverSQLite(tb).Messaging
			},
			Capability: beadsOverSQLiteCapability,
		})
	})
	t.Run("orders", func(t *testing.T) {
		storebindingtest.RunOrdersStoreTests(storebindingtest.Wrap(t), storebindingtest.OrdersSuite{
			NewStore:   func(tb storebindingtest.TB) storebinding.OrdersStore { return openBeadsOverSQLite(tb).Orders },
			Capability: beadsOverSQLiteCapability,
		})
	})
	t.Run("nudges", func(t *testing.T) {
		storebindingtest.RunNudgeFrontDoorTests(storebindingtest.Wrap(t), storebindingtest.NudgesSuite{
			NewFrontDoors: func(tb storebindingtest.TB) storebinding.NudgeFrontDoors { return openBeadsOverSQLite(tb).Nudges },
			Capability:    beadsOverSQLiteCapability,
		})
	})
}

// TestBeadsOverSQLiteRunsTheMailProviderTable runs the exhaustive shared
// mail.Provider table against the Messaging leg. RunMessagingFrontDoorTests
// covers the bare class contract; this is the rest of the mail surface, and
// without it the Messaging front door is the one class whose corpus pass is
// only skin deep.
func TestBeadsOverSQLiteRunsTheMailProviderTable(t *testing.T) {
	storebindingtest.RunMailProviderTests(t, storebindingtest.MessagingSuite{
		NewFrontDoors: func(tb storebindingtest.TB) storebinding.MessagingFrontDoors {
			return openBeadsOverSQLite(tb).Messaging
		},
		Capability: beadsOverSQLiteCapability,
	})
}

// TestBeadsOverSQLiteBindsTheBeadsBackedQueue pins WHICH queue the corpus
// above actually exercised.
//
// This assertion exists because its absence already produced a misleading
// green: an earlier out-of-tree probe passed the Nudges suite while
// storebindingtest's in-memory queue was silently bound, so the corpus proved
// nothing about the Beads-backed queue at all. NewBeadsAdapters takes the
// queue as a variadic argument, so dropping it is a compiling, passing
// mistake. Comparing dynamic types — rather than asserting "not nil" or
// matching a type NAME — is what makes a substituted queue a failure.
func TestBeadsOverSQLiteBindsTheBeadsBackedQueue(t *testing.T) {
	bound := openBeadsOverSQLite(storebindingtest.Wrap(t)).Nudges.Queue
	if bound == nil {
		t.Fatal("the Nudges front doors carry no queue")
	}

	reference, err := storebinding.NewBeadsNudgeQueue(beads.NudgesStore{Store: openBeadsOverSQLiteEngine(storebindingtest.Wrap(t))})
	if err != nil {
		t.Fatalf("building the reference beads nudge queue: %v", err)
	}
	got, want := reflect.TypeOf(bound), reflect.TypeOf(reference)
	if memory := reflect.TypeOf(storebindingtest.NewMemoryNudgeQueue()); got == memory {
		t.Fatalf("the harness's in-memory queue (%s) was substituted for the durable one; every Nudges assertion above passed against a queue that keeps nothing", memory)
	}
	if got != want {
		t.Fatalf("the corpus ran against a %s queue, want the Beads-backed %s; the class corpus proves nothing about the queue it did not run", got, want)
	}
}
