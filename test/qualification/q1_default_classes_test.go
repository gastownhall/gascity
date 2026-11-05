package qualification_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// q1ClassWrite is one class's smallest complete round trip: a write performed
// through that class's own front door, returning the bead id the write landed
// on. Every case is a real domain operation, not a raw bead create, so the
// class's own codec (types, labels, metadata) is what gets persisted.
type q1ClassWrite struct {
	Class coordclass.Class
	Write func(t *testing.T, adapters storebinding.BeadsAdapters) string
}

// q1ClassWrites is the per-class write corpus. Sessions, Messaging, Orders and
// Nudges go through their domain front doors; Graph writes a convergence root,
// which is graph-class by type alone; Work writes an ordinary task, the
// residual class everything unmatched falls into.
func q1ClassWrites() []q1ClassWrite {
	return []q1ClassWrite{
		{
			Class: coordclass.ClassWork,
			Write: func(t *testing.T, adapters storebinding.BeadsAdapters) string {
				t.Helper()
				created, err := adapters.Work.Create(beads.Bead{Title: "q1 work item", Type: "task"})
				if err != nil {
					t.Fatalf("creating a work bead: %v", err)
				}
				return created.ID
			},
		},
		{
			Class: coordclass.ClassGraph,
			Write: func(t *testing.T, adapters storebinding.BeadsAdapters) string {
				t.Helper()
				created, err := adapters.Graph.Create(beads.Bead{Title: "q1 convergence root", Type: "convergence"})
				if err != nil {
					t.Fatalf("creating a graph bead: %v", err)
				}
				return created.ID
			},
		},
		{
			Class: coordclass.ClassSessions,
			Write: func(t *testing.T, adapters storebinding.BeadsAdapters) string {
				t.Helper()
				id, err := adapters.Sessions.CreateSession(session.CreateSpec{Title: "q1-agent", AgentName: "q1-agent"})
				if err != nil {
					t.Fatalf("creating a session: %v", err)
				}
				return id
			},
		},
		{
			Class: coordclass.ClassMessaging,
			Write: func(t *testing.T, adapters storebinding.BeadsAdapters) string {
				t.Helper()
				message, err := adapters.Messaging.Mail.Send("q1-sender", "q1-agent", "q1 subject", "q1 body")
				if err != nil {
					t.Fatalf("sending mail: %v", err)
				}
				return message.ID
			},
		},
		{
			Class: coordclass.ClassOrders,
			Write: func(t *testing.T, adapters storebinding.BeadsAdapters) string {
				t.Helper()
				run, err := adapters.Orders.CreateRun("q1/patrol", orders.RunOpts{})
				if err != nil {
					t.Fatalf("creating an order run: %v", err)
				}
				return run.ID
			},
		},
		{
			Class: coordclass.ClassNudges,
			Write: func(t *testing.T, adapters storebinding.BeadsAdapters) string {
				t.Helper()
				now := time.Now().UTC()
				id, created, err := adapters.Nudges.Shadows.Save(nudgequeue.Item{
					ID:           "q1-nudge-1",
					Agent:        "q1-agent",
					Source:       "q1",
					Message:      "q1 nudge",
					CreatedAt:    now,
					DeliverAfter: now,
					ExpiresAt:    now.Add(time.Hour),
				})
				if err != nil {
					t.Fatalf("saving a nudge shadow: %v", err)
				}
				if !created {
					t.Fatal("saving a fresh nudge shadow reported no create")
				}
				return id
			},
		},
	}
}

// TestQ1DefaultCityServesEverySemanticClassFromOneBinding is the behavioral
// core of this suite and the observable form of the compatibility contract.
//
// A default city projects ONE canonical Beads binding into all six class front
// doors. Each front door performs its own domain write; the persisted effect is
// then read back from the canonical engine — a read path the front door's
// caller never writes through — and classified from the persisted fields alone.
// A front door that quietly kept its writes elsewhere, or that persisted a bead
// some other class owns, fails here rather than at the point where two classes
// stop agreeing in production.
func TestQ1DefaultCityServesEverySemanticClassFromOneBinding(t *testing.T) {
	engine := q1Engine(t)
	adapters := q1DefaultProjection(t, engine, "q1-hq")

	if adapters.Identity != q1Identity("q1-hq") {
		t.Fatalf("the projection carries identity %+v, want %+v", adapters.Identity, q1Identity("q1-hq"))
	}

	for _, write := range q1ClassWrites() {
		t.Run(write.Class.String(), func(t *testing.T) {
			id := write.Write(t, adapters)
			if id == "" {
				t.Fatal("the class front door reported no bead id")
			}
			// The independent read path: the canonical engine itself. The
			// domain front doors above never write through this handle.
			persisted, err := engine.Get(id)
			if err != nil {
				t.Fatalf("reading %s back from the canonical engine: %v; the %s front door did not persist into the reserved work binding",
					id, err, write.Class)
			}
			if got := coordclass.Classify(persisted); got != write.Class {
				t.Errorf("the bead the %s front door persisted classifies as %s (type %q, labels %v)",
					write.Class, got, persisted.Type, persisted.Labels)
			}
		})
	}
}

// TestQ1DefaultCityClassesReadEachOthersWrites states the same fact from the
// other direction, and is what makes the assertion above evidence about the
// BINDING rather than about each front door in isolation: because all six
// classes resolve to one binding, a bead written through one class's front door
// is readable through another's.
//
// The relocated control is the mutation this suite has to survive. It builds
// the identical assertion over TWO engines — exactly what a city that moved the
// sessions class to its own binding would have — and requires it to fail. If
// the cross-class read still succeeded there, the passing leg above would be
// telling us nothing about which binding served the write.
func TestQ1DefaultCityClassesReadEachOthersWrites(t *testing.T) {
	t.Run("one binding", func(t *testing.T) {
		adapters := q1DefaultProjection(t, q1Engine(t), "q1-hq")

		sessionID, err := adapters.Sessions.CreateSession(session.CreateSpec{Title: "q1-agent", AgentName: "q1-agent"})
		if err != nil {
			t.Fatalf("creating a session: %v", err)
		}
		if _, err := adapters.Graph.Get(sessionID); err != nil {
			t.Errorf("the graph front door cannot read the session bead %s: %v; the two classes are not on one binding", sessionID, err)
		}

		run, err := adapters.Orders.CreateRun("q1/patrol", orders.RunOpts{})
		if err != nil {
			t.Fatalf("creating an order run: %v", err)
		}
		if _, err := adapters.Graph.Get(run.ID); err != nil {
			t.Errorf("the graph front door cannot read the order-tracking bead %s: %v", run.ID, err)
		}

		message, err := adapters.Messaging.Mail.Send("q1-sender", "q1-agent", "q1 subject", "q1 body")
		if err != nil {
			t.Fatalf("sending mail: %v", err)
		}
		if _, err := adapters.Graph.Get(message.ID); err != nil {
			t.Errorf("the graph front door cannot read the mail bead %s: %v", message.ID, err)
		}

		// Messaging resolves its addressing through the Sessions directory it
		// was bound to at composition. On one binding, mail sent to the agent
		// that owns the session above lands in that agent's inbox.
		inbox, err := adapters.Messaging.Mail.Inbox("q1-agent")
		if err != nil {
			t.Fatalf("reading the inbox: %v", err)
		}
		if !q1ContainsMessage(inbox, message.ID) {
			t.Errorf("mail %s is not in the recipient's inbox; messaging and sessions do not agree on one binding", message.ID)
		}
	})

	t.Run("relocated sessions", func(t *testing.T) {
		hq := q1DefaultProjection(t, q1Engine(t), "q1-hq")
		relocated := q1DefaultProjection(t, q1Engine(t), "q1-sessions")

		sessionID, err := relocated.Sessions.CreateSession(session.CreateSpec{Title: "q1-agent", AgentName: "q1-agent"})
		if err != nil {
			t.Fatalf("creating a session on the relocated binding: %v", err)
		}
		if _, err := hq.Graph.Get(sessionID); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("the graph front door read session bead %s from a DIFFERENT binding (err=%v); the one-binding assertions above are vacuous",
				sessionID, err)
		}
	})
}

// TestQ1DefaultBindingKeepsTransactionsClaimsAndErrorsExact is the capability leg for
// the reserved binding: the capabilities a default city's classes are served
// with today — atomic transactions, compare-and-swap claims, readiness, and the
// typed not-found error — behave exactly as they do now, and every durable
// effect is confirmed against the canonical engine rather than against the
// front door that produced it.
//
// These are the capabilities the plan declares as requirements for the classes
// on the reserved binding. Declaring them and then serving a binding that
// silently lacks them is the substitution defect the whole class corpus exists
// to catch; this leg pins it for the one binding Q1 ships.
func TestQ1DefaultBindingKeepsTransactionsClaimsAndErrorsExact(t *testing.T) {
	engine := q1Engine(t)
	adapters := q1DefaultProjection(t, engine, "q1-hq")

	t.Run("a committed transaction is durable", func(t *testing.T) {
		var id string
		if err := adapters.Graph.Tx("q1 commit", func(tx storebinding.GraphTx) error {
			created, err := tx.Create(beads.Bead{Title: "q1 committed", Type: "task"})
			if err != nil {
				return err
			}
			id = created.ID
			return nil
		}); err != nil {
			t.Fatalf("committing a graph transaction: %v", err)
		}
		if _, err := engine.Get(id); err != nil {
			t.Errorf("the committed bead %s is not in the canonical engine: %v", id, err)
		}
	})

	t.Run("an aborted transaction leaves nothing behind", func(t *testing.T) {
		abort := errors.New("q1 abort")
		var id string
		err := adapters.Graph.Tx("q1 abort", func(tx storebinding.GraphTx) error {
			created, createErr := tx.Create(beads.Bead{Title: "q1 aborted", Type: "task"})
			if createErr != nil {
				return createErr
			}
			id = created.ID
			return abort
		})
		if !errors.Is(err, abort) {
			t.Fatalf("an aborting graph transaction returned %v, want the caller's error", err)
		}
		if id == "" {
			t.Fatal("the aborted transaction never created anything, so rollback is untested")
		}
		if _, err := engine.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("the rolled-back bead %s is still in the canonical engine (err=%v); the transaction was not atomic", id, err)
		}
	})

	t.Run("claims are compare-and-swap, and a conflict is not an error", func(t *testing.T) {
		created, err := adapters.Graph.Create(beads.Bead{Title: "q1 claimable", Type: "task"})
		if err != nil {
			t.Fatalf("creating a claimable bead: %v", err)
		}
		claimed, ok, err := adapters.Graph.Claim(created.ID, "worker-a")
		if err != nil || !ok {
			t.Fatalf("claiming %s for worker-a returned ok=%t err=%v", created.ID, ok, err)
		}
		if claimed.Assignee != "worker-a" {
			t.Errorf("the claimed bead reports assignee %q, want worker-a", claimed.Assignee)
		}
		if persisted, err := engine.Get(created.ID); err != nil || persisted.Assignee != "worker-a" {
			t.Errorf("the canonical engine reports assignee %q (err=%v), want worker-a", persisted.Assignee, err)
		}
		if _, ok, err := adapters.Graph.Claim(created.ID, "worker-a"); err != nil || !ok {
			t.Errorf("re-claiming for the same holder returned ok=%t err=%v, want an idempotent success", ok, err)
		}
		if _, ok, err := adapters.Graph.Claim(created.ID, "worker-b"); err != nil || ok {
			t.Errorf("claiming a held bead for worker-b returned ok=%t err=%v, want a conflict reported as ok=false with no error", ok, err)
		}
		if released, err := adapters.Graph.ReleaseIfCurrent(created.ID, "worker-b"); err != nil || released {
			t.Errorf("releasing on behalf of a non-holder returned released=%t err=%v, want false with no error", released, err)
		}
		if released, err := adapters.Graph.ReleaseIfCurrent(created.ID, "worker-a"); err != nil || !released {
			t.Errorf("releasing on behalf of the holder returned released=%t err=%v, want true", released, err)
		}
	})

	t.Run("readiness reports the open backlog", func(t *testing.T) {
		created, err := adapters.Graph.Create(beads.Bead{Title: "q1 ready", Type: "task"})
		if err != nil {
			t.Fatalf("creating a ready bead: %v", err)
		}
		ready, err := adapters.Graph.Ready()
		if err != nil {
			t.Fatalf("reading the ready set: %v", err)
		}
		if !q1ContainsBead(ready, created.ID) {
			t.Errorf("the ready set does not contain the open bead %s", created.ID)
		}
		if err := adapters.Graph.Close(created.ID); err != nil {
			t.Fatalf("closing %s: %v", created.ID, err)
		}
		ready, err = adapters.Graph.Ready()
		if err != nil {
			t.Fatalf("re-reading the ready set: %v", err)
		}
		if q1ContainsBead(ready, created.ID) {
			t.Errorf("the ready set still contains the closed bead %s", created.ID)
		}
	})

	t.Run("a missing bead is the typed not-found error", func(t *testing.T) {
		if _, err := adapters.Graph.Get("gc-does-not-exist"); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("reading a missing bead returned %v, want a %v", err, beads.ErrNotFound)
		}
	})
}

// TestQ1DefaultCityResolvesWorkIDsAcrossScopes covers the by-ID selection rows
// of the enumeration surface against live stores, in the shape a default city has: every scope's
// Work store is a real engine, and the reserved binding serves them all.
//
// Exact prefix comes first and never touches a store. An ID no prefix claims
// falls through to the residence probe, which reads. The probe's two failure
// modes — the same ID resident in two scopes, and an ID resident in none — are
// typed errors, not a first-match guess.
func TestQ1DefaultCityResolvesWorkIDsAcrossScopes(t *testing.T) {
	hq := q1Engine(t)
	alpha := q1Engine(t)
	beta := q1Engine(t)
	topology := q1Topology(t,
		q1Workspace(storebinding.HQScope(), hq, "gc", "hq"),
		q1Workspace(storebinding.RigScope("alpha"), alpha, "ga", "alpha"),
		q1Workspace(storebinding.RigScope("beta"), beta, "gb", "beta"),
	)

	t.Run("exact prefix", func(t *testing.T) {
		// Resident in alpha, but prefixed for HQ: prefix selection wins and no
		// store is read, so a probe-first implementation would answer alpha.
		q1CreateWithID(t, alpha, "gc-prefixed")
		scope, err := topology.ScopeForID("gc-prefixed")
		if err != nil {
			t.Fatalf("selecting gc-prefixed: %v", err)
		}
		if scope != storebinding.HQScope() {
			t.Errorf("gc-prefixed selected %s, want hq", scope)
		}
	})

	t.Run("residence probe", func(t *testing.T) {
		q1CreateWithID(t, beta, "legacy-resident")
		scope, err := topology.ScopeForID("legacy-resident")
		if err != nil {
			t.Fatalf("probing legacy-resident: %v", err)
		}
		if scope != storebinding.RigScope("beta") {
			t.Errorf("legacy-resident resolved to %s, want rig:beta", scope)
		}
	})

	t.Run("duplicate residence", func(t *testing.T) {
		q1CreateWithID(t, alpha, "legacy-duplicated")
		q1CreateWithID(t, beta, "legacy-duplicated")
		_, err := topology.ScopeForID("legacy-duplicated")
		if !errors.Is(err, storebinding.ErrDuplicateWorkResidence) {
			t.Fatalf("an ID resident in two scopes resolved with %v, want a %v", err, storebinding.ErrDuplicateWorkResidence)
		}
		var duplicate *storebinding.DuplicateWorkResidenceError
		if !errors.As(err, &duplicate) {
			t.Fatalf("duplicate residence reported %T, want *storebinding.DuplicateWorkResidenceError", err)
		}
		if got := q1ScopeNames(duplicate.Candidates); got != "rig:alpha,rig:beta" {
			t.Errorf("duplicate residence named candidates %s, want rig:alpha,rig:beta", got)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := topology.ScopeForID("legacy-absent")
		if !errors.Is(err, storebinding.ErrWorkResidenceNotFound) {
			t.Fatalf("an ID resident nowhere resolved with %v, want a %v", err, storebinding.ErrWorkResidenceNotFound)
		}
	})

	t.Run("scoped topology keeps every workspace distinct", func(t *testing.T) {
		if got := len(topology.PhysicalWorkspaces()); got != 3 {
			t.Errorf("three distinct engines grouped into %d physical workspaces, want 3", got)
		}
		if _, err := topology.ForScope(storebinding.RigScope("absent")); !errors.Is(err, storebinding.ErrWorkScopeNotFound) {
			t.Errorf("ForScope on an unconfigured rig returned %v, want a %v", err, storebinding.ErrWorkScopeNotFound)
		}
	})
}

// TestQ1SharedWorkspaceOpensAggregatesAndClosesOnce is the pinning contract over live handles.
// Two semantic scopes served by one physical ledger group into a single
// physical workspace — the unit that is opened and closed — while both scopes
// remain separately addressable and observe each other's writes, because they
// are the same ledger.
//
// The scoped topology above is this case's control: distinct ledgers must group
// three times and must NOT share writes.
func TestQ1SharedWorkspaceOpensAggregatesAndClosesOnce(t *testing.T) {
	shared := q1Engine(t)
	topology := q1Topology(t,
		q1Workspace(storebinding.HQScope(), shared, "gc", "shared-ledger"),
		q1Workspace(storebinding.RigScope("alpha"), shared, "ga", "shared-ledger"),
	)

	physical := topology.PhysicalWorkspaces()
	if len(physical) != 1 {
		t.Fatalf("two scopes on one ledger grouped into %d physical workspaces, want 1", len(physical))
	}
	if got := q1ScopeNames(physical[0].Scopes); got != "hq,rig:alpha" {
		t.Errorf("the shared physical workspace retains scopes %s, want hq,rig:alpha", got)
	}
	if len(topology.MigrationWorkspaces()) != 1 {
		t.Errorf("a shared ledger schedules %d migration workspaces, want 1", len(topology.MigrationWorkspaces()))
	}
	if len(topology.All()) != 2 {
		t.Errorf("a shared ledger exposes %d semantic scopes, want 2", len(topology.All()))
	}

	hq, err := topology.ForScope(storebinding.HQScope())
	if err != nil {
		t.Fatalf("resolving the HQ scope: %v", err)
	}
	rig, err := topology.ForScope(storebinding.RigScope("alpha"))
	if err != nil {
		t.Fatalf("resolving the rig scope: %v", err)
	}
	created, err := hq.Store.Create(beads.Bead{Title: "q1 shared write", Type: "task"})
	if err != nil {
		t.Fatalf("writing through the HQ scope: %v", err)
	}
	if _, err := rig.Store.Get(created.ID); err != nil {
		t.Errorf("the rig scope cannot read %s written through the HQ scope: %v; a shared ledger must aggregate once", created.ID, err)
	}
}

// q1Workspace builds one Work workspace over a live engine.
func q1Workspace(scope storebinding.WorkScope, store beads.Store, prefix, physical string) storebinding.Workspace {
	return storebinding.Workspace{
		Scope:       scope,
		Store:       store,
		Prefix:      prefix,
		OpenerID:    "beads",
		ComponentID: "beads",
		PhysicalID:  physical,
	}
}

// q1Topology composes a Work topology whose first workspace is HQ.
func q1Topology(t *testing.T, hq storebinding.Workspace, rigs ...storebinding.Workspace) storebinding.WorkTopology {
	t.Helper()
	topology, err := storebinding.NewWorkTopology(hq, rigs)
	if err != nil {
		t.Fatalf("composing the work topology: %v", err)
	}
	return topology
}

// q1CreateWithID persists a bead under an explicit id so residence probes have
// something deterministic to find.
func q1CreateWithID(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if _, err := store.Create(beads.Bead{ID: id, Title: id, Type: "task"}); err != nil {
		t.Fatalf("creating bead %s: %v", id, err)
	}
	if _, err := store.Get(id); err != nil {
		t.Fatalf("bead %s is not resident after create: %v", id, err)
	}
}

// q1ContainsBead reports whether id is among the listed beads.
func q1ContainsBead(items []beads.Bead, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

// q1ContainsMessage reports whether id is among the listed messages.
func q1ContainsMessage(messages []mail.Message, id string) bool {
	for _, message := range messages {
		if message.ID == id {
			return true
		}
	}
	return false
}
