package session

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// startCommitPatch is the shape of the start commit: confirm the session live
// and release the pending-create claim.
var startCommitPatch = MetadataPatch{"state": string(StateActive), "pending_create_claim": ""}

func seedPendingStart(t *testing.T, store beads.Store, name string) beads.Bead {
	t.Helper()
	created, err := store.Create(sessionBeadFixture(name, "open", map[string]string{
		"state":                string(StateCreating),
		"pending_create_claim": "true",
		"generation":           "2",
		"instance_token":       "token-2",
		"session_name":         name,
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

// killPendingStart is what a `gc session kill` from another process writes:
// it clears the pending-create claim, so the start commit's verdict refuses.
func killPendingStart(store beads.Store, id string) error {
	return store.SetMetadataBatch(id, map[string]string(KillPendingPatch(closeTestNow)))
}

func assertKillSurvived(t *testing.T, store beads.Store, id string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != string(StateAsleep) || got.Metadata["state_reason"] != KillPendingReason {
		t.Fatalf("row = state %q state_reason %q, want the kill's asleep/%s (row %v)", got.Metadata["state"], got.Metadata["state_reason"], KillPendingReason, got.Metadata)
	}
}

// TestCommitStartedIfCurrentRefusesOverAnOutOfBandKill is the stale-read
// regression for the start commit. A kill lands on the backing store after the
// start was prepared. Through a cache, the commit's re-read still serves the
// pre-kill row, so before the fix the verdict passed and the unconditional
// write stamped state=active over the kill. On every store kind the commit
// must now write nothing.
func TestCommitStartedIfCurrentRefusesOverAnOutOfBandKill(t *testing.T) {
	for _, backend := range patchFenceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := seedPendingStart(t, store, "s-kill")
			front := NewStore(beads.SessionStore{Store: store})
			prepared, err := front.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := killPendingStart(backing, created.ID); err != nil {
				t.Fatalf("kill: %v", err)
			}

			applied, err := front.CommitStartedIfCurrent(context.Background(), prepared, startCommitPatch)
			if err != nil {
				t.Fatalf("CommitStartedIfCurrent: %v", err)
			}
			if applied {
				t.Fatal("start commit reported applied over a concurrent kill")
			}
			assertKillSurvived(t, backing, created.ID)
		})
	}
}

// TestCommitStartedIfCurrentFencesAWriteAfterTheReread proves the revision
// fence: a kill that lands after the re-read passed the verdict must still
// win, and the retry must re-run the verdict rather than re-send the patch.
func TestCommitStartedIfCurrentFencesAWriteAfterTheReread(t *testing.T) {
	for _, backend := range patchFenceBackends() {
		if !backend.fenced {
			continue // the documented residual window without conditional writes
		}
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := seedPendingStart(t, store, "s-window")
			prepared, err := NewStore(beads.SessionStore{Store: store}).Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			racing := &afterGetStore{Store: store, afterGet: func(id string) {
				if err := killPendingStart(backing, id); err != nil {
					t.Errorf("kill: %v", err)
				}
			}}
			front := NewStore(beads.SessionStore{Store: racing})

			applied, err := front.CommitStartedIfCurrent(context.Background(), prepared, startCommitPatch)
			if err != nil {
				t.Fatalf("CommitStartedIfCurrent: %v", err)
			}
			if applied {
				t.Fatal("start commit reported applied although a kill won the revision fence")
			}
			assertKillSurvived(t, backing, created.ID)
		})
	}
}

// TestCommitStartedIfCurrentRedecidesPastAnUnrelatedWrite pins that the fence
// re-decides rather than giving up: a write to a key the verdict does not read
// wins the fence once, the retry re-reads, still passes, and commits without
// clobbering that key.
func TestCommitStartedIfCurrentRedecidesPastAnUnrelatedWrite(t *testing.T) {
	for _, backend := range patchFenceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := seedPendingStart(t, store, "s-unrelated")
			prepared, err := NewStore(beads.SessionStore{Store: store}).Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			racing := &afterGetStore{Store: store, afterGet: func(id string) {
				if err := backing.SetMetadata(id, "last_nudge_delivered_at", "2026-09-28T01:02:03Z"); err != nil {
					t.Errorf("unrelated write: %v", err)
				}
			}}
			front := NewStore(beads.SessionStore{Store: racing})

			applied, err := front.CommitStartedIfCurrent(context.Background(), prepared, startCommitPatch)
			if err != nil || !applied {
				t.Fatalf("CommitStartedIfCurrent = (%v, %v), want applied", applied, err)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Metadata["state"] != string(StateActive) || got.Metadata["pending_create_claim"] != "" ||
				got.Metadata["last_nudge_delivered_at"] != "2026-09-28T01:02:03Z" {
				t.Fatalf("row = %v, want the start committed with the unrelated write kept", got.Metadata)
			}
		})
	}
}

// everyGetStore runs afterGet after every read, so a concurrent writer wins
// every fence the front door tries.
type everyGetStore struct {
	beads.Store
	afterGet func(id string)
}

func (s *everyGetStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	s.afterGet(id)
	return b, err
}

func (s *everyGetStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// TestCommitStartedIfCurrentBoundsALosingFence pins the exhaustion contract: a
// writer that wins every fence leaves nothing written and returns the
// precondition error, so the caller fails the commit and retries next pass.
func TestCommitStartedIfCurrentBoundsALosingFence(t *testing.T) {
	store := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	created := seedPendingStart(t, store, "s-starved")
	prepared, err := NewStore(beads.SessionStore{Store: store}).Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	writes := 0
	racing := &everyGetStore{Store: store, afterGet: func(id string) {
		writes++
		if err := store.SetMetadata(id, "test_nudge_seq", strconv.Itoa(writes)); err != nil {
			t.Errorf("unrelated write: %v", err)
		}
	}}

	applied, err := NewStore(beads.SessionStore{Store: racing}).CommitStartedIfCurrent(context.Background(), prepared, startCommitPatch)
	if applied || !beads.IsPreconditionFailed(err) {
		t.Fatalf("CommitStartedIfCurrent = (%v, %v), want the precondition error", applied, err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != string(StateCreating) || got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("row = %v, want the pending create untouched", got.Metadata)
	}
}

// TestCommitStartedIfCurrentRequireModeFailsClosed: under require, a store
// that cannot fence refuses the commit instead of writing unconditionally.
func TestCommitStartedIfCurrentRequireModeFailsClosed(t *testing.T) {
	mem := beads.NewMemStore()
	store := stampedMemStore(t, gate.Require, mem)
	created := seedPendingStart(t, store, "s-require")
	front := NewStore(beads.SessionStore{Store: store})
	prepared, err := front.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	mem.DisableConditionalWrites = true

	applied, err := front.CommitStartedIfCurrent(context.Background(), prepared, startCommitPatch)
	if applied || !beads.IsConditionalWritesRequired(err) {
		t.Fatalf("CommitStartedIfCurrent = (%v, %v), want the require-mode refusal", applied, err)
	}
	got, err := mem.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != string(StateCreating) {
		t.Fatalf("state = %q, want the row untouched", got.Metadata["state"])
	}
}

// TestCommitStartedIfCurrentRaceAgainstAKill runs the start commit and an
// out-of-band kill concurrently. Whatever the interleaving, the kill must be
// the surviving fact: either the commit landed first and the kill overwrote
// it, or the kill landed first and the commit refused. The commit goes through
// a cache over the stamped store, as the controller's does, and the kill hits
// the backing, as another process's does.
func TestCommitStartedIfCurrentRaceAgainstAKill(t *testing.T) {
	for _, backend := range patchFenceBackends() {
		if !backend.fenced {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			for i := 0; i < 25; i++ {
				created := seedPendingStart(t, store, "s-race-"+strconv.Itoa(i))
				front := NewStore(beads.SessionStore{Store: store})
				prepared, err := front.Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				start := make(chan struct{})
				var wg sync.WaitGroup
				var commitErr, killErr error
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					_, commitErr = front.CommitStartedIfCurrent(context.Background(), prepared, startCommitPatch)
				}()
				go func() {
					defer wg.Done()
					<-start
					killErr = killPendingStart(backing, created.ID)
				}()
				close(start)
				wg.Wait()
				if commitErr != nil && !beads.IsPreconditionFailed(commitErr) {
					t.Fatalf("CommitStartedIfCurrent: %v", commitErr)
				}
				if killErr != nil {
					t.Fatalf("kill: %v", killErr)
				}
				assertKillSurvived(t, backing, created.ID)
			}
		})
	}
}

func stampedMemStore(t *testing.T, mode gate.Mode, mem *beads.MemStore) beads.Store {
	t.Helper()
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: mode,
		OpenFileStore:     func() (beads.Store, error) { return mem, nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	return result.Store
}

// suspendDuringStart is what an operator's `gc session suspend` that lands
// while a start runs leaves for the commit (mc-z1v39, J16 B): the user hold,
// with the row healed awake once the start's runtime is listed, so the lease
// verdict alone would commit.
func suspendDuringStart(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if _, err := NewStore(beads.SessionStore{Store: store}).OperatorSuspend(id, closeTestNow); err != nil {
		t.Fatalf("OperatorSuspend: %v", err)
	}
	if err := store.SetMetadataBatch(id, map[string]string{"state": string(StateAwake)}); err != nil {
		t.Fatalf("heal: %v", err)
	}
}

// TestCommitStartedRefusesAnOperatorSuspendDuringTheStart (mc-z1v39,
// CONTRACT S1 v5.6, §12.2 row 65): with StartCommitRefusingHolds, a start
// commit whose re-read finds an operator's suspend refuses with
// ErrStartCommitHeld and writes nothing, whether the suspend landed before
// the re-read or after it (the revision fence re-decides); without the
// option the commit lands, as recoverRunningPendingCreate's does. Kills: the
// hold check dropped, or read off the prepared row instead of the re-read.
func TestCommitStartedRefusesAnOperatorSuspendDuringTheStart(t *testing.T) {
	for _, backend := range patchFenceBackends() {
		for _, c := range []struct {
			name       string
			afterRead  bool
			refuseHeld bool
		}{
			{name: "before the re-read", refuseHeld: true},
			{name: "after the re-read", afterRead: true, refuseHeld: true},
			{name: "without the option"},
		} {
			if c.afterRead && !backend.fenced {
				continue // the documented residual window without conditional writes
			}
			t.Run(backend.name+"/"+c.name, func(t *testing.T) {
				store, backing := backend.open(t)
				created := seedPendingStart(t, store, "s-held")
				prepared, err := NewStore(beads.SessionStore{Store: store}).Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				reads := store
				if c.afterRead {
					reads = &afterGetStore{Store: store, afterGet: func(id string) { suspendDuringStart(t, backing, id) }}
				} else {
					suspendDuringStart(t, backing, created.ID)
				}
				var opts []StartCommitOpt
				if c.refuseHeld {
					opts = append(opts, StartCommitRefusingHolds())
				}
				applied, err := NewStore(beads.SessionStore{Store: reads}).CommitStartedIfCurrentUnder(context.Background(), prepared, startCommitPatch, nil, opts...)
				got, gerr := backing.Get(created.ID)
				if gerr != nil {
					t.Fatalf("Get: %v", gerr)
				}
				switch {
				case !c.refuseHeld && (err != nil || !applied):
					t.Fatalf("CommitStartedIfCurrentUnder = %v, %v; want it applied without the option", applied, err)
				case c.refuseHeld && (applied || !errors.Is(err, ErrStartCommitHeld)):
					t.Fatalf("CommitStartedIfCurrentUnder = %v, %v; want ErrStartCommitHeld", applied, err)
				case c.refuseHeld && (got.Metadata["state"] != string(StateAwake) || got.Metadata["pending_create_claim"] != "true"):
					t.Fatalf("row %v; want nothing written", got.Metadata)
				}
			})
		}
	}
}
