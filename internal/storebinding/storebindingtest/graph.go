package storebindingtest

// Bare Graph class conformance.
//
// Every assertion here rides ONLY the closed storebinding.GraphStore contract:
// no raw beads.Store, no provider type, no physical layout. That is what makes
// the suite portable across the canonical Beads adapters, the SQLite class
// store, and a private provider that this repository never sees.
//
// Capability-guarded assertions (transactions, claims) run only when the
// provider DECLARES the capability. A declaration is a promise, not a hint: a
// provider that declares transactions and does not roll back fails
// TransactionRollsBackEntirely rather than skipping it. Capability loss is a
// conformance failure; silence is what lets it ship.

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// GraphSuite configures one bare Graph class conformance run.
type GraphSuite struct {
	// NewStore returns a fresh, empty Graph front door per assertion.
	NewStore func(TB) storebinding.GraphStore
	// Capability is what the provider declares for the Graph class. Guarded
	// assertions run exactly when the matching flag is set.
	Capability storebinding.ClassCapability
}

// RunGraphStoreTests runs the bare Graph class conformance suite.
func RunGraphStoreTests(r Runner, suite GraphSuite) {
	r.Helper()
	if suite.NewStore == nil {
		r.Fatalf("storebindingtest: GraphSuite.NewStore is required")
	}

	assertClassDeclaredAvailable(r, "Graph", suite.Capability)

	r.Run("CreateGetRoundTrip", func(r Runner) {
		store := suite.NewStore(r)
		created := mustCreateBead(r, store, beads.Bead{
			Title:    "compose the binding",
			Type:     "task",
			Status:   "open",
			Labels:   []string{"storage"},
			Metadata: beads.StringMap{"plan_key": "conformance"},
		})
		if created.ID == "" {
			r.Fatalf("Create returned an empty ID")
		}
		got, err := store.Get(created.ID)
		if err != nil {
			r.Fatalf("Get(%q): %v", created.ID, err)
		}
		if got.ID != created.ID {
			r.Errorf("Get ID = %q, want %q", got.ID, created.ID)
		}
		if got.Title != "compose the binding" {
			r.Errorf("Title = %q, want %q", got.Title, "compose the binding")
		}
		if got.Status != "open" {
			r.Errorf("Status = %q, want open", got.Status)
		}
		if got.Metadata["plan_key"] != "conformance" {
			r.Errorf("Metadata[plan_key] = %q, want conformance", got.Metadata["plan_key"])
		}
	})

	r.Run("GetUnknownIsNotFound", func(r Runner) {
		store := suite.NewStore(r)
		_, err := store.Get("gcg-does-not-exist")
		if !errors.Is(err, beads.ErrNotFound) {
			r.Fatalf("Get(unknown) = %v, want a beads.ErrNotFound chain", err)
		}
	})

	r.Run("ListFiltersByStatus", func(r Runner) {
		store := suite.NewStore(r)
		open := mustCreateBead(r, store, beads.Bead{Title: "open work", Type: "task", Status: "open"})
		closed := mustCreateBead(r, store, beads.Bead{Title: "finished work", Type: "task", Status: "open"})
		if err := store.Close(closed.ID); err != nil {
			r.Fatalf("Close(%q): %v", closed.ID, err)
		}
		listed, err := store.List(beads.ListQuery{Status: "open"})
		if err != nil {
			r.Fatalf("List: %v", err)
		}
		ids := beadIDs(listed)
		if !containsID(ids, open.ID) {
			r.Errorf("List(open) = %v, want it to contain the open bead %q", ids, open.ID)
		}
		if containsID(ids, closed.ID) {
			r.Errorf("List(open) = %v, want it to exclude the closed bead %q", ids, closed.ID)
		}
	})

	r.Run("UpdateIfMatchRejectsStaleRevision", func(r Runner) {
		store := suite.NewStore(r)
		created := mustCreateBead(r, store, beads.Bead{Title: "revisioned", Type: "task", Status: "open"})
		before := mustGetBead(r, store, created.ID)
		title := "renamed once"
		if err := store.UpdateIfMatch(created.ID, before.Revision, beads.UpdateOpts{Title: &title}); err != nil {
			r.Fatalf("UpdateIfMatch at the current revision: %v", err)
		}
		after := mustGetBead(r, store, created.ID)
		if after.Title != title {
			r.Fatalf("Title = %q, want %q — the matched update did not apply", after.Title, title)
		}
		if after.Revision == before.Revision {
			r.Fatalf("revision %d did not move across a committed update; every later CAS is vacuous", after.Revision)
		}
		stale := "renamed from a stale snapshot"
		if err := store.UpdateIfMatch(created.ID, before.Revision, beads.UpdateOpts{Title: &stale}); err == nil {
			r.Fatalf("UpdateIfMatch at a stale revision succeeded; the write is not conditional")
		}
		if latest := mustGetBead(r, store, created.ID); latest.Title != title {
			r.Errorf("Title = %q after a rejected CAS, want the committed %q", latest.Title, title)
		}
	})

	r.Run("CompareAndSetMetadataKeyHasOneWinner", func(r Runner) {
		store := suite.NewStore(r)
		created := mustCreateBead(r, store, beads.Bead{Title: "contended key", Type: "task", Status: "open"})
		won, err := store.CompareAndSetMetadataKey(created.ID, "owner", "", "first")
		if err != nil {
			r.Fatalf("CompareAndSetMetadataKey: %v", err)
		}
		if !won {
			r.Fatalf("the first CAS from the absent value lost; nothing can ever win")
		}
		lost, err := store.CompareAndSetMetadataKey(created.ID, "owner", "", "second")
		if err != nil {
			r.Fatalf("CompareAndSetMetadataKey (second): %v", err)
		}
		if lost {
			r.Fatalf("a second CAS from the same absent value also won; the key has two owners")
		}
		if got := mustGetBead(r, store, created.ID); got.Metadata["owner"] != "first" {
			r.Errorf("Metadata[owner] = %q, want first", got.Metadata["owner"])
		}
	})

	r.Run("DependenciesGateReadiness", func(r Runner) {
		store := suite.NewStore(r)
		blocker := mustCreateBead(r, store, beads.Bead{Title: "blocker", Type: "task", Status: "open"})
		blocked := mustCreateBead(r, store, beads.Bead{Title: "blocked", Type: "task", Status: "open"})
		if err := store.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
			r.Fatalf("DepAdd: %v", err)
		}
		deps, err := store.DepList(blocked.ID, "blocked-by")
		if err != nil {
			r.Fatalf("DepList: %v", err)
		}
		if len(deps) != 1 || deps[0].DependsOnID != blocker.ID {
			r.Fatalf("DepList(blocked-by) = %+v, want one edge onto %q", deps, blocker.ID)
		}
		ready, err := store.Ready()
		if err != nil {
			r.Fatalf("Ready: %v", err)
		}
		if containsID(beadIDs(ready), blocked.ID) {
			r.Errorf("Ready = %v, want it to exclude the blocked bead %q", beadIDs(ready), blocked.ID)
		}
		if !containsID(beadIDs(ready), blocker.ID) {
			r.Errorf("Ready = %v, want it to contain the unblocked bead %q", beadIDs(ready), blocker.ID)
		}
		if err := store.Close(blocker.ID); err != nil {
			r.Fatalf("Close(blocker): %v", err)
		}
		unblocked, err := store.Ready()
		if err != nil {
			r.Fatalf("Ready after the blocker closed: %v", err)
		}
		if !containsID(beadIDs(unblocked), blocked.ID) {
			r.Errorf("Ready = %v, want the released bead %q once its blocker closed", beadIDs(unblocked), blocked.ID)
		}
	})

	r.Run("ReadyContextHonorsCancellation", func(r Runner) {
		store := suite.NewStore(r)
		mustCreateBead(r, store, beads.Bead{Title: "ready work", Type: "task", Status: "open"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.ReadyContext(ctx); !errors.Is(err, context.Canceled) {
			r.Fatalf("ReadyContext on a canceled context = %v, want context.Canceled", err)
		}
	})

	r.Run("CloseAndReopenOwnership", func(r Runner) {
		store := suite.NewStore(r)
		created := mustCreateBead(r, store, beads.Bead{Title: "cycle me", Type: "task", Status: "open"})
		if err := store.Close(created.ID); err != nil {
			r.Fatalf("Close: %v", err)
		}
		if got := mustGetBead(r, store, created.ID); got.Status != "closed" {
			r.Fatalf("Status = %q after Close, want closed", got.Status)
		}
		if err := store.Reopen(created.ID); err != nil {
			r.Fatalf("Reopen: %v", err)
		}
		if got := mustGetBead(r, store, created.ID); got.Status != "open" {
			r.Fatalf("Status = %q after Reopen, want open", got.Status)
		}
		second := mustCreateBead(r, store, beads.Bead{Title: "batch me", Type: "task", Status: "open"})
		count, err := store.CloseAll([]string{created.ID, second.ID}, map[string]string{"closed_by": "conformance"})
		if err != nil {
			r.Fatalf("CloseAll: %v", err)
		}
		if count != 2 {
			r.Errorf("CloseAll closed %d beads, want 2", count)
		}
	})

	r.Run("DeleteRemovesTheBead", func(r Runner) {
		store := suite.NewStore(r)
		created := mustCreateBead(r, store, beads.Bead{Title: "transient", Type: "task", Status: "open"})
		if err := store.Delete(created.ID); err != nil {
			r.Fatalf("Delete: %v", err)
		}
		if _, err := store.Get(created.ID); !errors.Is(err, beads.ErrNotFound) {
			r.Fatalf("Get after Delete = %v, want a beads.ErrNotFound chain", err)
		}
	})

	r.Run("PingReportsLiveness", func(r Runner) {
		store := suite.NewStore(r)
		if err := store.Ping(); err != nil {
			r.Fatalf("Ping on an open store: %v", err)
		}
	})

	if suite.Capability.Claims {
		r.Run("ClaimIsCompareAndSwap", func(r Runner) {
			store := suite.NewStore(r)
			created := mustCreateBead(r, store, beads.Bead{Title: "claimable", Type: "task", Status: "open"})
			claimed, ok, err := store.Claim(created.ID, "alice")
			if err != nil {
				r.Fatalf("Claim by the first holder: %v", err)
			}
			if !ok {
				r.Fatalf("Claim on an unassigned open bead lost; nothing can ever claim it")
			}
			if claimed.Assignee != "alice" {
				r.Errorf("Assignee = %q after a won claim, want alice", claimed.Assignee)
			}
			repeat, ok, err := store.Claim(created.ID, "alice")
			if err != nil {
				r.Fatalf("Claim by the same holder: %v", err)
			}
			if !ok {
				r.Fatalf("re-claim by the current holder reported a conflict; the claim is not idempotent")
			}
			if repeat.Revision != claimed.Revision {
				r.Errorf("revision moved from %d to %d on a same-holder re-claim; a no-op wrote", claimed.Revision, repeat.Revision)
			}
			if _, ok, err := store.Claim(created.ID, "bob"); err != nil {
				r.Fatalf("Claim by a second holder: %v", err)
			} else if ok {
				r.Fatalf("a second holder also won the claim; the bead has two owners")
			}
			if released, err := store.ReleaseIfCurrent(created.ID, "bob"); err != nil {
				r.Fatalf("ReleaseIfCurrent by a non-holder: %v", err)
			} else if released {
				r.Fatalf("a non-holder released the claim")
			}
			if released, err := store.ReleaseIfCurrent(created.ID, "alice"); err != nil {
				r.Fatalf("ReleaseIfCurrent by the holder: %v", err)
			} else if !released {
				r.Fatalf("the holder could not release its own claim")
			}
			if _, ok, err := store.Claim(created.ID, "bob"); err != nil {
				r.Fatalf("Claim after release: %v", err)
			} else if !ok {
				r.Fatalf("the bead stayed unclaimable after its holder released it")
			}
		})
	}

	if suite.Capability.Transactions {
		r.Run("TransactionRollsBackEntirely", func(r Runner) {
			store := suite.NewStore(r)
			survivor := mustCreateBead(r, store, beads.Bead{Title: "committed earlier", Type: "task", Status: "open"})
			boom := errors.New("conformance rollback")
			var doomed string
			err := store.Tx("rollback", func(tx storebinding.GraphTx) error {
				created, err := tx.Create(beads.Bead{Title: "must not survive", Type: "task", Status: "open"})
				if err != nil {
					return err
				}
				doomed = created.ID
				if err := tx.Close(survivor.ID); err != nil {
					return err
				}
				return boom
			})
			if !errors.Is(err, boom) {
				r.Fatalf("Tx = %v, want the callback failure", err)
			}
			if doomed != "" {
				if _, err := store.Get(doomed); !errors.Is(err, beads.ErrNotFound) {
					r.Errorf("the bead created inside a failed transaction survived (Get = %v); the transaction is not atomic", err)
				}
			}
			if got := mustGetBead(r, store, survivor.ID); got.Status != "open" {
				r.Errorf("Status = %q after a failed transaction closed it, want the rolled-back open", got.Status)
			}
		})

		r.Run("TransactionCommitsEveryWrite", func(r Runner) {
			store := suite.NewStore(r)
			var created string
			if err := store.Tx("commit", func(tx storebinding.GraphTx) error {
				bead, err := tx.Create(beads.Bead{Title: "committed", Type: "task", Status: "open"})
				if err != nil {
					return err
				}
				created = bead.ID
				return tx.SetMetadataBatch(bead.ID, map[string]string{"phase": "committed"})
			}); err != nil {
				r.Fatalf("Tx: %v", err)
			}
			got := mustGetBead(r, store, created)
			if got.Metadata["phase"] != "committed" {
				r.Errorf("Metadata[phase] = %q after a committed transaction, want committed", got.Metadata["phase"])
			}
		})
	}
}

// assertClassDeclaredAvailable pins the composition-level promise: a provider
// that hands out a class front door while declaring the class unavailable has
// published a set no planner should have accepted.
func assertClassDeclaredAvailable(r Runner, class string, capability storebinding.ClassCapability) {
	r.Run("ClassIsDeclaredAvailable", func(r Runner) {
		if !capability.Available {
			r.Fatalf("the %s front door was handed out while the provider declares the class unavailable (%s)",
				class, describeCapability(capability))
		}
	})
}

func mustCreateBead(r Runner, store storebinding.GraphStore, bead beads.Bead) beads.Bead {
	r.Helper()
	created, err := store.Create(bead)
	if err != nil {
		r.Fatalf("Create(%q): %v", bead.Title, err)
	}
	return created
}

func mustGetBead(r Runner, store storebinding.GraphStore, id string) beads.Bead {
	r.Helper()
	got, err := store.Get(id)
	if err != nil {
		r.Fatalf("Get(%q): %v", id, err)
	}
	return got
}

func beadIDs(list []beads.Bead) []string {
	ids := make([]string, 0, len(list))
	for _, bead := range list {
		ids = append(ids, bead.ID)
	}
	return ids
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// describeCapability renders a class capability for failure messages.
func describeCapability(capability storebinding.ClassCapability) string {
	return fmt.Sprintf("available=%t transactions=%t claims=%t",
		capability.Available, capability.Transactions, capability.Claims)
}
