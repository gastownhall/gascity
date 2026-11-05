package storebindingtest

// Reference front doors.
//
// The harness needs a conforming implementation of its own so its
// broken-fake proofs have something to break. The canonical Beads adapters
// over an in-memory store are that reference: they are the substitution
// leg the design names, they need no provider, no CGO and no filesystem, and
// they keep this package's own tests free of any storage provider.
//
// The reference declares exactly the capabilities it has. beads.MemStore has
// no two-argument claim and its Tx is a straight pass-through with no
// rollback, so Claims and Transactions are false here. Declaring them would
// be the very capability-loss defect the suites exist to catch, and
// BrokenGraphStore proves that by declaring them anyway.

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// ReferenceCapability is what the Beads-over-memory reference honestly
// declares for every class it serves.
var ReferenceCapability = storebinding.ClassCapability{Available: true}

// ReferenceAdapters returns the canonical Beads adapters over one fresh
// in-memory store, with an in-memory nudge queue bound as the queue front
// door. Every class front door in the returned set shares that one store, so
// the set behaves like a single unified binding.
func ReferenceAdapters(tb TB) storebinding.BeadsAdapters {
	tb.Helper()
	adapters, err := storebinding.NewBeadsAdapters(
		beads.NewMemStore(),
		storebinding.BeadsAdapterIdentity{OpenerID: "storebindingtest", ComponentID: "reference", PhysicalID: "memory"},
		NewMemoryNudgeQueue(),
	)
	if err != nil {
		tb.Fatalf("storebindingtest: building the reference Beads adapters: %v", err)
	}
	return adapters
}

// ReferenceGraphStore returns a fresh reference Graph front door.
func ReferenceGraphStore(tb TB) storebinding.GraphStore { return ReferenceAdapters(tb).Graph }

// ReferenceSessionsStore returns a fresh reference Sessions front door.
func ReferenceSessionsStore(tb TB) storebinding.SessionsStore { return ReferenceAdapters(tb).Sessions }

// ReferenceOrdersStore returns a fresh reference Orders front door. The
// canonical adapter serves the orders and graph legs from one store, which is
// the unified topology the closed contract has to answer under.
func ReferenceOrdersStore(tb TB) storebinding.OrdersStore { return ReferenceAdapters(tb).Orders }

// ReferenceMessagingFrontDoors returns fresh reference Messaging front doors
// already bound to the matching Sessions address directory.
func ReferenceMessagingFrontDoors(tb TB) storebinding.MessagingFrontDoors {
	return ReferenceAdapters(tb).Messaging
}

// ReferenceNudgeFrontDoors returns a fresh in-memory queue and the canonical
// Beads-backed shadow projection.
func ReferenceNudgeFrontDoors(tb TB) storebinding.NudgeFrontDoors {
	return ReferenceAdapters(tb).Nudges
}

// ReferenceWorkTopology returns a two-scope Work topology whose HQ and rig
// members are distinct physical workspaces.
func ReferenceWorkTopology(tb TB) storebinding.WorkTopology {
	tb.Helper()
	topology, err := storebinding.NewWorkTopology(
		workspace(storebinding.HQScope(), "gc", "hq"),
		[]storebinding.Workspace{workspace(storebinding.RigScope("alpha"), "ga", "alpha")},
	)
	if err != nil {
		tb.Fatalf("storebindingtest: building the reference Work topology: %v", err)
	}
	return topology
}

func workspace(scope storebinding.WorkScope, prefix, physical string) storebinding.Workspace {
	return storebinding.Workspace{
		Scope:       scope,
		Store:       beads.NewMemStore(),
		Prefix:      prefix,
		OpenerID:    "storebindingtest",
		ComponentID: "work",
		PhysicalID:  physical,
	}
}

// ScopedWorkspace builds one Work topology member with a caller-chosen
// physical identity, so a consumer can compose scoped, unified and mixed
// topologies without reaching for the unexported constructor.
func ScopedWorkspace(scope storebinding.WorkScope, prefix, physical string) storebinding.Workspace {
	return workspace(scope, prefix, physical)
}

// SharedWorkspace builds one Work topology member that deliberately shares
// another member's physical identity — the unified shape, where several
// semantic scopes resolve to one opened database.
func SharedWorkspace(scope storebinding.WorkScope, prefix, physical string, store beads.Store) storebinding.Workspace {
	return storebinding.Workspace{
		Scope:       scope,
		Store:       store,
		Prefix:      prefix,
		OpenerID:    "storebindingtest",
		ComponentID: "work",
		PhysicalID:  physical,
	}
}
