package storebindingtest

// Bare Sessions class conformance, over the closed
// storebinding.SessionsStore contract only.

import (
	"errors"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// SessionsSuite configures one bare Sessions class conformance run.
type SessionsSuite struct {
	// NewStore returns a fresh, empty Sessions front door per assertion.
	NewStore func(TB) storebinding.SessionsStore
	// Capability is what the provider declares for the Sessions class.
	Capability storebinding.ClassCapability
}

// RunSessionsStoreTests runs the bare Sessions class conformance suite.
func RunSessionsStoreTests(r Runner, suite SessionsSuite) {
	r.Helper()
	if suite.NewStore == nil {
		r.Fatalf("storebindingtest: SessionsSuite.NewStore is required")
	}

	assertClassDeclaredAvailable(r, "Sessions", suite.Capability)

	r.Run("CreateGetRoundTrip", func(r Runner) {
		store := suite.NewStore(r)
		created := mustCreateSession(r, store, "polly", map[string]string{
			"state":        string(session.StateAwake),
			"session_name": "gc-polly",
			"generation":   "3",
		})
		if created.ID == "" {
			r.Fatalf("CreateSessionInfo returned an empty ID")
		}
		got, err := store.Get(created.ID)
		if err != nil {
			r.Fatalf("Get(%q): %v", created.ID, err)
		}
		if got.ID != created.ID {
			r.Errorf("Get ID = %q, want %q", got.ID, created.ID)
		}
		if got.SessionName != "gc-polly" {
			r.Errorf("SessionName = %q, want gc-polly", got.SessionName)
		}
		if got.Generation != "3" {
			r.Errorf("Generation = %q, want 3", got.Generation)
		}
		if got.MetadataState != string(session.StateAwake) {
			r.Errorf("MetadataState = %q, want %q", got.MetadataState, session.StateAwake)
		}
	})

	r.Run("GetUnknownIsNotFound", func(r Runner) {
		store := suite.NewStore(r)
		if _, err := store.Get("gcs-does-not-exist"); !errors.Is(err, beads.ErrNotFound) {
			r.Fatalf("Get(unknown) = %v, want a beads.ErrNotFound chain", err)
		}
	})

	r.Run("ApplyPatchClearsWithEmptyString", func(r Runner) {
		store := suite.NewStore(r)
		created := mustCreateSession(r, store, "patchy", map[string]string{
			"sleep_reason": "maintenance",
			"session_name": "gc-patchy",
		})
		if err := store.ApplyPatch(created.ID, session.MetadataPatch{"sleep_reason": ""}); err != nil {
			r.Fatalf("ApplyPatch: %v", err)
		}
		got, err := store.Get(created.ID)
		if err != nil {
			r.Fatalf("Get after patch: %v", err)
		}
		if got.SleepReason != "" {
			r.Errorf("SleepReason = %q after an empty-string patch, want it cleared", got.SleepReason)
		}
		if got.SessionName != "gc-patchy" {
			r.Errorf("SessionName = %q, want the untouched marker preserved", got.SessionName)
		}
	})

	r.Run("SetStateIsReadBack", func(r Runner) {
		store := suite.NewStore(r)
		created := mustCreateSession(r, store, "stateful", map[string]string{"state": string(session.StateAwake)})
		if err := store.SetState(created.ID, session.StateAsleep, "conformance"); err != nil {
			r.Fatalf("SetState: %v", err)
		}
		state, closed, err := store.GetState(created.ID)
		if err != nil {
			r.Fatalf("GetState: %v", err)
		}
		if state != session.StateAsleep {
			r.Errorf("State = %q, want %q", state, session.StateAsleep)
		}
		if closed {
			r.Errorf("an open session reported closed")
		}
	})

	r.Run("ListAllIncludesCreatedSessions", func(r Runner) {
		store := suite.NewStore(r)
		first := mustCreateSession(r, store, "one", nil)
		second := mustCreateSession(r, store, "two", nil)
		listed, err := store.ListAll(session.ListAllOptions{})
		if err != nil {
			r.Fatalf("ListAll: %v", err)
		}
		ids := sessionIDs(listed)
		if !containsID(ids, first.ID) || !containsID(ids, second.ID) {
			r.Fatalf("ListAll = %v, want both %q and %q", ids, first.ID, second.ID)
		}
	})

	r.Run("CloseIsWonExactlyOnce", func(r Runner) {
		store := suite.NewStore(r)
		created := mustCreateSession(r, store, "closable", nil)
		now := time.Now().UTC()
		closed, err := store.Close(created.ID, "done", now)
		if err != nil {
			r.Fatalf("Close: %v", err)
		}
		if !closed {
			r.Fatalf("the first Close of an open session reported no transition")
		}
		again, err := store.Close(created.ID, "done", now)
		if err != nil {
			r.Fatalf("second Close: %v", err)
		}
		if again {
			r.Fatalf("a second Close also reported a transition; the close is not owned once")
		}
		_, wasClosed, err := store.GetState(created.ID)
		if err != nil {
			r.Fatalf("GetState after close: %v", err)
		}
		if !wasClosed {
			r.Errorf("the session did not report closed after Close")
		}
	})

	if suite.Capability.Transactions {
		r.Run("TransactionRollsBackEntirely", func(r Runner) {
			store := suite.NewStore(r)
			// sleep_reason (not last_woke_at) is the representative durable
			// field here deliberately: last_woke_at is clone-local-routed by
			// Store.ApplyPatch (see session/store.go's localOnlyMetadataKeys),
			// and no production Tx caller ever patches it (ClosePatch and the
			// reconciler's Tx call sites both leave it alone), so it is not a
			// safe stand-in for "an arbitrary durable field" in a generic
			// Tx-atomicity proof. sleep_reason carries no such special routing.
			survivor := mustCreateSession(r, store, "survivor", map[string]string{
				"state":        string(session.StateAwake),
				"sleep_reason": "before",
			})
			boom := errors.New("conformance rollback")
			err := store.Tx("rollback", func(tx session.Tx) error {
				if err := tx.ApplyPatch(survivor.ID, session.MetadataPatch{"sleep_reason": ""}); err != nil {
					return err
				}
				if err := tx.CloseWithoutReason(survivor.ID); err != nil {
					return err
				}
				return boom
			})
			if !errors.Is(err, boom) {
				r.Fatalf("Tx = %v, want the callback failure", err)
			}
			got, err := store.Get(survivor.ID)
			if err != nil {
				r.Fatalf("Get after a failed transaction: %v", err)
			}
			// The rollback lane's whole invariant is that a session bead never
			// reports closed without the terminal metadata that belongs with
			// the close. A store that keeps HALF a failed transaction produces
			// exactly that row, so both halves are asserted.
			if got.Closed {
				r.Errorf("the session is closed after a failed transaction; the close was not rolled back")
			}
			if got.SleepReason != "before" {
				r.Errorf("sleep_reason = %q after a failed transaction, want the rolled-back %q", got.SleepReason, "before")
			}
		})

		r.Run("TransactionCommitsEveryWrite", func(r Runner) {
			store := suite.NewStore(r)
			created := mustCreateSession(r, store, "committed", map[string]string{
				"state":        string(session.StateAwake),
				"sleep_reason": "before",
			})
			if err := store.Tx("commit", func(tx session.Tx) error {
				if err := tx.ApplyPatch(created.ID, session.MetadataPatch{"sleep_reason": ""}); err != nil {
					return err
				}
				return tx.CloseWithoutReason(created.ID)
			}); err != nil {
				r.Fatalf("Tx: %v", err)
			}
			got, err := store.Get(created.ID)
			if err != nil {
				r.Fatalf("Get after a committed transaction: %v", err)
			}
			if !got.Closed {
				r.Errorf("the session is open after a committed transaction closed it")
			}
			if got.SleepReason != "" {
				r.Errorf("sleep_reason = %q after a committed transaction cleared it, want it cleared", got.SleepReason)
			}
		})
	}

	r.Run("WaitLifecycleIsDurable", func(r Runner) {
		store := suite.NewStore(r)
		owner := mustCreateSession(r, store, "waiter", nil)
		now := time.Now().UTC()
		wait, err := store.CreateWait(session.WaitSpec{
			SessionID: owner.ID,
			Kind:      "deps",
			DepIDs:    []string{"gcg-1", "gcg-2"},
			DepMode:   "all",
			Note:      "conformance wait",
			Now:       now,
		})
		if err != nil {
			r.Fatalf("CreateWait: %v", err)
		}
		if wait.ID == "" {
			r.Fatalf("CreateWait returned an empty wait ID")
		}
		got, err := store.GetWait(wait.ID)
		if err != nil {
			r.Fatalf("GetWait: %v", err)
		}
		if got.SessionID != owner.ID {
			r.Errorf("wait SessionID = %q, want %q", got.SessionID, owner.ID)
		}
		if len(got.DepIDs) != 2 {
			r.Errorf("wait DepIDs = %v, want the two registered dependencies", got.DepIDs)
		}
		waits, err := store.WaitsForSession(owner.ID)
		if err != nil {
			r.Fatalf("WaitsForSession: %v", err)
		}
		if len(waits) != 1 || waits[0].ID != wait.ID {
			r.Fatalf("WaitsForSession = %+v, want exactly the registered wait %q", waits, wait.ID)
		}
		if err := store.MarkWaitReady(wait.ID, now); err != nil {
			r.Fatalf("MarkWaitReady: %v", err)
		}
		ready, err := store.GetWait(wait.ID)
		if err != nil {
			r.Fatalf("GetWait after MarkWaitReady: %v", err)
		}
		if ready.State != "ready" {
			r.Errorf("wait State = %q after MarkWaitReady, want ready", ready.State)
		}
	})
}

func mustCreateSession(r Runner, store storebinding.SessionsStore, agent string, metadata map[string]string) session.Info {
	r.Helper()
	info, err := store.CreateSessionInfo(session.CreateSpec{Title: agent, AgentName: agent, Metadata: metadata})
	if err != nil {
		r.Fatalf("CreateSessionInfo(%q): %v", agent, err)
	}
	return info
}

func sessionIDs(list []session.Info) []string {
	ids := make([]string, 0, len(list))
	for _, info := range list {
		ids = append(ids, info.ID)
	}
	return ids
}
