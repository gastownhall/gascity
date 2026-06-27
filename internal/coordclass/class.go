// Package coordclass defines the single work-vs-infrastructure boundary for
// Gas City's bead substrate.
//
// Everything in Gas City currently persists through one beads.Store
// (internal/beads/beads.go), backed by bd/Dolt. The "work vs infrastructure
// split" initiative carves that one store into persistence classes, each
// destined for its own typed provider module so a faster backend can be
// swapped in later:
//
//   - WORK beads — the real backlog (tasks, epics, bugs, features,
//     merge-requests, user convoys). These stay in bd, git-synced and
//     human-visible.
//   - INFRASTRUCTURE data — everything that merely uses beads as a convenient
//     store: the formula-v2 graph engine, sessions, mail, order dispatch, and
//     the nudge queue.
//
// A [Class] names WHO owns the data — the subsystem responsible for it — not
// how or where it is stored. This is deliberate and is the one axis the whole
// split turns on:
//
//   - Class is the OWNERSHIP/ROUTING axis: work, graph, messaging, sessions,
//     orders, nudges. Each value maps to one owning subsystem and (eventually)
//     one typed module.
//   - beads.StorageClass (history / no_history / ephemeral) is a SEPARATE,
//     ORTHOGONAL axis: the physical tier a bead lives in. "Ephemeral" is a
//     storage tier, NOT a class — graph nodes, mail, and trackers are all
//     ephemeral at times. A bead has both a Class and a StorageClass; the two
//     never collapse into one enum.
//
// The split is deliberately NOT type-shaped: extmsg records hide under
// type=task, nudges under type=chore, synthetic convoys under type=convoy, and
// formula graphs embed work-typed bug/epic steps inside infra graphs. So the
// classification cannot be a type filter — it is the runtime function
// [Classify], the one place the boundary is decided. The router write path, the
// Ready/claim federation, and the wire-layer List federation all consult this
// single function so the boundary can never drift between subsystems.
//
// Not every infrastructure bead is a routed class. agent/role/rig identity
// records have NO Go creation site (packs create them via bd prompts), so
// gascity never routes a Create for them; they are read-only records, not a
// Class here. If a pack ever owns them in Go, a "registry" class can be added.
//
// This package is a leaf: its production code imports only internal/beads and
// internal/beadmeta. The contract strings it matches on (session/wait/
// order-tracking/nudge labels, the mail message type, the extmsg label prefix)
// are mirrored here with their canonical source cited; guard_test.go pins them
// against the exported constants where those are importable.
package coordclass

// Class is the owning subsystem a bead belongs to — the unit of routing for the
// coordination-store split. Each non-Work class is destined for its own typed
// provider module, with bd as the first (identity) implementation. It is
// orthogonal to beads.StorageClass (the physical tier): a bead has both.
type Class int

const (
	// ClassWork is the real backlog: tasks, epics, bugs, features,
	// merge-requests, and user/sling convoys. Owner: the bd backlog. This is the
	// zero value so any bead not matched by an explicit infrastructure arm
	// defaults to work.
	ClassWork Class = iota

	// ClassGraph is the formula-v2 execution engine's topology and control lane:
	// molecule/step/gate/scope/run beads, every gc.kind control bead, wisp
	// roots, convergence beads, spec sidecars, and synthetic (graph.v2 input /
	// drain-unit) convoys. Owner: internal/dispatch + internal/molecule +
	// internal/formula. This is the bead explosion the split primarily targets.
	ClassGraph

	// ClassMessaging is mail (type=message) and the extmsg families (type=task
	// carrying a gc:extmsg-* label). Owner: internal/mail + internal/extmsg.
	ClassMessaging

	// ClassSessions is session lifecycle beads (type=session) and durable
	// session waits (type=gate + gc:wait). Owner: internal/session.
	ClassSessions

	// ClassOrders is order-dispatch tracking beads (the order-tracking /
	// order-run records that gate repeat order firing). Owner: internal/orders
	// + the order-dispatch path.
	ClassOrders

	// ClassNudges is the durability mirror of the nudge queue (type=chore +
	// gc:nudge). Owner: the nudge queue subsystem. The live queue is a
	// flock-guarded file; these beads are its persistent shadow.
	ClassNudges
)

// String returns the stable lowercase name of the class. The names are part of
// the routing/config contract (e.g. per-class backend selection in city.toml)
// and must not change without a migration.
func (c Class) String() string {
	switch c {
	case ClassWork:
		return "work"
	case ClassGraph:
		return "graph"
	case ClassMessaging:
		return "messaging"
	case ClassSessions:
		return "sessions"
	case ClassOrders:
		return "orders"
	case ClassNudges:
		return "nudges"
	default:
		return "unknown"
	}
}

// IsInfrastructure reports whether the class is an infrastructure class (i.e.
// anything other than ClassWork). Infrastructure classes are the ones eligible
// to move behind a non-bd provider.
func (c Class) IsInfrastructure() bool {
	return c != ClassWork
}

// Classes returns every class in a stable order, ClassWork first. Used to drive
// deterministic fan-out order in the router and exhaustive table tests.
func Classes() []Class {
	return []Class{ClassWork, ClassGraph, ClassMessaging, ClassSessions, ClassOrders, ClassNudges}
}
