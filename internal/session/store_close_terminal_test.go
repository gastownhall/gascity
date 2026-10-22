package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// terminalCloseTxStore is closeInterferenceStore plus a Tx that counts calls,
// records the commit message, and runs interfere between the Tx's metadata
// write and its Close. That is where the two writes of the non-atomic fallback
// split, so counting both arms proves which one CommitClose took.
type terminalCloseTxStore struct {
	*closeInterferenceStore
	txMsgs []string
}

func (s *terminalCloseTxStore) Tx(commitMsg string, fn func(beads.Tx) error) error {
	s.txMsgs = append(s.txMsgs, commitMsg)
	return s.Store.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&interferingTx{Tx: tx, interfere: s.interfere})
	})
}

type interferingTx struct {
	beads.Tx
	interfere func(id string) error
}

func (tx *interferingTx) Close(id string) error {
	if tx.interfere != nil {
		if err := tx.interfere(id); err != nil {
			return err
		}
	}
	return tx.Tx.Close(id)
}

// failedCreateTerminalPatch mirrors the controller's failed-create close:
// ClosePatch plus clears the plain Close does not write.
func failedCreateTerminalPatch() MetadataPatch {
	patch := ClosePatch(closeTestNow, string(StateFailedCreate))
	patch["pending_create_claim"] = ""
	patch["sleep_intent"] = ""
	return patch
}

// TestCommitCloseAcrossBackends is the front-door half of the
// controller close regression. A writer that does not touch the close's
// premise (a nudge stamp) lands in the window between the close reading the
// row and its terminal write. On every store with the atomic close, the row
// must end closed with the caller's whole patch, through the fenced single
// write only (one lost fence, one retry, no Tx). On a store without the
// capability, the controller's single Tx is kept verbatim, commit message
// included; the interference is off there because that arm's residual race is
// documented, not fixed. TestCommitCloseRefusesWhenItsPremiseMoves
// covers a writer that does change the premise.
func TestCommitCloseAcrossBackends(t *testing.T) {
	for _, backend := range sessionCloseBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created, err := store.Create(sessionBeadFixture("s-"+backend.name, "open", map[string]string{
				"state":                string(StateCreating),
				"pending_create_claim": "true",
				"sleep_intent":         "idle",
			}))
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			tracing := &terminalCloseTxStore{closeInterferenceStore: &closeInterferenceStore{Store: store}}
			if backend.atomic {
				tracing.interfere = unrelatedWriteOnce(backing)
			}
			front := NewStore(beads.SessionStore{Store: tracing})

			res, err := front.CommitClose(Decide(decidedOn(t, backing, created.ID), FactsLegacyClose), failedCreateTerminalPatch(), closeTestNow)
			if err != nil || res != CommitLanded {
				t.Fatalf("CommitClose = (%v, %v), want landed", res, err)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "closed" {
				t.Fatalf("status = %q, want closed", got.Status)
			}
			for key, want := range failedCreateTerminalPatch() {
				if got.Metadata[key] != want {
					t.Errorf("metadata[%q] = %q, want %q (the whole terminal patch lands with the close)", key, got.Metadata[key], want)
				}
			}
			if backend.atomic {
				if tracing.atomicCalls != 2 || len(tracing.txMsgs) != 0 || tracing.plainCloseCalls != 0 {
					t.Fatalf("atomic/tx/plain close calls = %d/%d/%d, want 2/0/0 (one lost fence, one retry, no split write)",
						tracing.atomicCalls, len(tracing.txMsgs), tracing.plainCloseCalls)
				}
				return
			}
			// The tracing wrapper carries the method, so it is asked once and
			// answers unsupported (the backing lacks the capability).
			if want := []string{"gc: close session " + created.ID}; tracing.atomicCalls != 1 || !reflect.DeepEqual(tracing.txMsgs, want) {
				t.Fatalf("atomic calls = %d, tx commit messages = %q, want 1 and %q (unsupported, then the controller's Tx)", tracing.atomicCalls, tracing.txMsgs, want)
			}
		})
	}
}

// TestCommitCloseRefusesWhenItsPremiseMoves is the regression for
// closing over a concurrent wake. The controller decided to close from an
// older read. A writer then changes a fact that decision rested on and wins
// the revision fence: a wake (state=awake), a wake request, or a new
// incarnation (generation, instance token). Before the fix, the retry re-read
// only the revision and closed the row anyway. On every store with the atomic
// close it must now write nothing and return ErrSessionCloseSuperseded,
// leaving the row to the writer that won; the caller's next pass decides
// again.
func TestCommitCloseRefusesWhenItsPremiseMoves(t *testing.T) {
	for _, change := range []struct {
		name  string
		patch map[string]string
	}{
		{name: "wake", patch: map[string]string{"state": string(StateAwake)}},
		{name: "wake request", patch: map[string]string{"wake_request": "api"}},
		{name: "new generation", patch: map[string]string{"generation": "3"}},
		{name: "new instance token", patch: map[string]string{"instance_token": "token-3"}},
	} {
		for _, backend := range sessionCloseBackends() {
			if !backend.atomic {
				continue // the documented two-write residual race
			}
			t.Run(change.name+"/"+backend.name, func(t *testing.T) {
				store, backing := backend.open(t)
				created, err := store.Create(sessionBeadFixture("s-premise", "open", map[string]string{
					"state":          string(StateAsleep),
					"generation":     "2",
					"instance_token": "token-2",
				}))
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				tracing := &terminalCloseTxStore{closeInterferenceStore: &closeInterferenceStore{Store: store}}
				fired := false
				tracing.interfere = func(id string) error {
					if fired {
						return nil
					}
					fired = true
					return backing.SetMetadataBatch(id, change.patch)
				}
				front := NewStore(beads.SessionStore{Store: tracing})

				res, err := front.CommitClose(Decide(decidedOn(t, backing, created.ID), FactsLegacyClose), ClosePatch(closeTestNow, "dead-runtime"), closeTestNow)
				if err != nil || res != CommitMoved {
					t.Fatalf("CommitClose = (%v, %v), want moved", res, err)
				}
				if tracing.atomicCalls != 1 || len(tracing.txMsgs) != 0 {
					t.Fatalf("atomic/tx calls = %d/%d, want 1/0 (one lost fence, no retry, no split write)", tracing.atomicCalls, len(tracing.txMsgs))
				}
				got, err := backing.Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if got.Status != "open" || got.Metadata["close_reason"] != "" {
					t.Fatalf("row = status %q close_reason %q, want it open with no terminal metadata", got.Status, got.Metadata["close_reason"])
				}
				for key, want := range change.patch {
					if got.Metadata[key] != want {
						t.Fatalf("metadata[%q] = %q, want the winning writer's %q", key, got.Metadata[key], want)
					}
				}
			})
		}
	}
}

// TestCommitCloseLeavesAClosedRowAlone pins the atomic arm's
// idempotence: a row that is already closed is reported false and not
// rewritten, so a second closer cannot overwrite the first close's record.
func TestCommitCloseLeavesAClosedRowAlone(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-closed")
	front := NewStore(beads.SessionStore{Store: backing})
	if closed, err := front.CommitClose(Decide(decidedOn(t, backing, created.ID), FactsLegacyClose), ClosePatch(closeTestNow, "orphaned"), closeTestNow); err != nil || closed != CommitLanded {
		t.Fatalf("first close = (%v, %v), want landed", closed, err)
	}
	before, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	tracing := &terminalCloseTxStore{closeInterferenceStore: &closeInterferenceStore{Store: backing}}
	closed, err := NewStore(beads.SessionStore{Store: tracing}).CommitClose(Decide(decidedOn(t, backing, created.ID), FactsLegacyClose), failedCreateTerminalPatch(), closeTestNow)
	if err != nil || closed != CommitClosed {
		t.Fatalf("second close = (%v, %v), want closed on a closed row", closed, err)
	}
	if tracing.atomicCalls != 0 || len(tracing.txMsgs) != 0 {
		t.Fatalf("atomic/tx calls = %d/%d, want 0/0 on a closed row", tracing.atomicCalls, len(tracing.txMsgs))
	}
	after, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(before.Metadata, after.Metadata) || before.Revision != after.Revision {
		t.Fatalf("closed row rewritten: before rev %d %v, after rev %d %v", before.Revision, before.Metadata, after.Revision, after.Metadata)
	}
}

// TestCommitCloseReportsFalseWhenAnotherCloserWins proves the
// exactly-one-winner contract the controller's release cascade relies on: a
// competing closer that lands in the window wins the fence, and this call
// reports false without rewriting that closer's record.
func TestCommitCloseReportsFalseWhenAnotherCloserWins(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-lost")
	tracing := &terminalCloseTxStore{closeInterferenceStore: &closeInterferenceStore{Store: backing}}
	fired := false
	tracing.interfere = func(id string) error {
		if fired {
			return nil
		}
		fired = true
		if err := backing.SetMetadataBatch(id, ClosePatch(closeTestNow, "orphaned")); err != nil {
			return err
		}
		return backing.Close(id)
	}
	closed, err := NewStore(beads.SessionStore{Store: tracing}).CommitClose(Decide(decidedOn(t, backing, created.ID), FactsLegacyClose), failedCreateTerminalPatch(), closeTestNow)
	if err != nil || closed != CommitClosed {
		t.Fatalf("CommitClose = (%v, %v), want closed after losing to a competing closer", closed, err)
	}
	got, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" || got.Metadata["state"] != "orphaned" {
		t.Fatalf("row = status %q state %q, want the winning closer's closed/orphaned record", got.Status, got.Metadata["state"])
	}
	if tracing.atomicCalls != 1 || len(tracing.txMsgs) != 0 {
		t.Fatalf("atomic/tx calls = %d/%d, want 1/0 (lost fence, re-read closed, no fallback)", tracing.atomicCalls, len(tracing.txMsgs))
	}
}

// TestCommitCloseFallsBackToTheTxWhenRefusedAtCallTime covers a
// store that advertises the atomic close but refuses it with
// ErrConditionalWriteUnsupported (which contractually writes nothing): the
// controller's single Tx runs instead.
func TestCommitCloseFallsBackToTheTxWhenRefusedAtCallTime(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-refused")
	tracing := &terminalCloseTxStore{closeInterferenceStore: &closeInterferenceStore{Store: backing, unsupported: true}}
	closed, err := NewStore(beads.SessionStore{Store: tracing}).CommitClose(Decide(decidedOn(t, backing, created.ID), FactsLegacyClose), failedCreateTerminalPatch(), closeTestNow)
	if err != nil || closed != CommitLanded {
		t.Fatalf("CommitClose = (%v, %v), want landed through the fallback", closed, err)
	}
	if tracing.atomicCalls != 1 || len(tracing.txMsgs) != 1 {
		t.Fatalf("atomic/tx calls = %d/%d, want 1/1 (refused, then the Tx)", tracing.atomicCalls, len(tracing.txMsgs))
	}
	got, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" || got.Metadata["state"] != string(StateFailedCreate) {
		t.Fatalf("row = status %q state %q, want closed/%s", got.Status, got.Metadata["state"], StateFailedCreate)
	}
}

// TestCloseReportsAKillFence: Store.Close, CommitClose's bool form, refuses a
// row a fresh kill fence holds with ErrSessionKillPending.
func TestCloseReportsAKillFence(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-killed")
	if err := backing.SetMetadataBatch(created.ID, map[string]string(KillPendingPatch(closeTestNow))); err != nil {
		t.Fatal(err)
	}
	closed, err := NewStore(beads.SessionStore{Store: backing}).Close(decidedOn(t, backing, created.ID), "dead-runtime", closeTestNow.Add(time.Second))
	if closed || !errors.Is(err, ErrSessionKillPending) {
		t.Fatalf("Close = (%v, %v), want (false, ErrSessionKillPending)", closed, err)
	}
}

// TestCommitCloseYieldsToAKillFence covers #6749's kill fence on the
// atomic arm. `gc session kill` owns a fenced row until it lifts the fence, and
// lifecycle passes must not close it. The controller decided to close on an
// older read, so the fence can appear on the close's own read, or land in the
// window and win the revision fence. Either way the close must write nothing
// and return ErrSessionKillPending, instead of re-reading the fence and closing
// over it; the row stays the kill's.
func TestCommitCloseYieldsToAKillFence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		inWindow    bool
		wantAtomics int
	}{
		{name: "fence already on the row", inWindow: false, wantAtomics: 0},
		{name: "fence lands in the window", inWindow: true, wantAtomics: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := beads.NewAtomicCloseMemStore()
			created := seedOpenSession(t, backing, "s-killed")
			stampKill := func(id string) error {
				return backing.SetMetadataBatch(id, map[string]string(KillPendingPatch(closeTestNow)))
			}
			tracing := &terminalCloseTxStore{closeInterferenceStore: &closeInterferenceStore{Store: backing}}
			if tc.inWindow {
				fired := false
				tracing.interfere = func(id string) error {
					if fired {
						return nil
					}
					fired = true
					return stampKill(id)
				}
			} else if err := stampKill(created.ID); err != nil {
				t.Fatalf("stamping the kill fence: %v", err)
			}

			closed, err := NewStore(beads.SessionStore{Store: tracing}).CommitClose(Decide(decidedOn(t, backing, created.ID), FactsLegacyClose), ClosePatch(closeTestNow, "dead-runtime"), closeTestNow.Add(time.Second))
			if err != nil || closed != CommitKillFenced {
				t.Fatalf("CommitClose = (%v, %v), want kill-fenced", closed, err)
			}
			if tracing.atomicCalls != tc.wantAtomics || len(tracing.txMsgs) != 0 {
				t.Fatalf("atomic/tx calls = %d/%d, want %d/0", tracing.atomicCalls, len(tracing.txMsgs), tc.wantAtomics)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "open" || !IsKillPendingInfo(infoFromPersistedBead(got), closeTestNow.Add(time.Second)) {
				t.Fatalf("row = status %q state %q state_reason %q, want it left open under the kill fence", got.Status, got.Metadata["state"], got.Metadata["state_reason"])
			}
		})
	}
}

// TestCommitCloseClosesPastAStaleKillFence: a fence older than
// KillPendingGrace is ordinary asleep state (the killing CLI died), so the
// controller's close proceeds.
func TestCommitCloseClosesPastAStaleKillFence(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-stale-kill")
	if err := backing.SetMetadataBatch(created.ID, map[string]string(KillPendingPatch(closeTestNow))); err != nil {
		t.Fatalf("stamping the kill fence: %v", err)
	}
	closed, err := NewStore(beads.SessionStore{Store: backing}).CommitClose(Decide(decidedOn(t, backing, created.ID), FactsLegacyClose), ClosePatch(closeTestNow, "dead-runtime"), closeTestNow.Add(KillPendingGrace+time.Second))
	if err != nil || closed != CommitLanded {
		t.Fatalf("CommitClose = (%v, %v), want landed past the kill grace", closed, err)
	}
}

// fallbackTxStore stands in for a store without the atomic close (BdStore, the
// exec store, a legacy SQLite layout): embedding only beads.Store hides any
// capability the backing has. It counts Tx calls, so a test can tell whether
// the fallback's write ran.
type fallbackTxStore struct {
	beads.Store
	txCalls int
}

func (s *fallbackTxStore) Tx(commitMsg string, fn func(beads.Tx) error) error {
	s.txCalls++
	return s.Store.Tx(commitMsg, fn)
}

// TestCommitCloseFallbackChecksTheRowFirst is the regression for
// the fallback arm closing over a row that moved after the caller's decision
// (mc-zndi7.65). The fallback ran its Tx with no read, so a wake, a new
// incarnation or a `gc session kill` fence that landed between the decision
// and the close was closed over, and an already-closed row was rewritten. It
// must now read the row first and write nothing in each of those cases, as the
// atomic arm does. An unchanged row still closes through the one Tx.
func TestCommitCloseFallbackChecksTheRowFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		// change runs on the backing after the decision, before the close.
		change func(store beads.Store, id string) error
		want   CommitResult
	}{
		{name: "unchanged row", want: CommitLanded},
		{name: "unrelated write", change: func(store beads.Store, id string) error {
			return store.SetMetadata(id, unrelatedKey, "2026-09-28T01:02:03Z")
		}, want: CommitLanded},
		{name: "wake", change: func(store beads.Store, id string) error {
			return store.SetMetadata(id, "state", string(StateAwake))
		}, want: CommitMoved},
		{name: "wake request", change: func(store beads.Store, id string) error {
			return store.SetMetadata(id, "wake_request", "api")
		}, want: CommitMoved},
		{name: "new generation", change: func(store beads.Store, id string) error {
			return store.SetMetadata(id, "generation", "3")
		}, want: CommitMoved},
		{name: "new instance token", change: func(store beads.Store, id string) error {
			return store.SetMetadata(id, "instance_token", "token-3")
		}, want: CommitMoved},
		{name: "kill fence", change: func(store beads.Store, id string) error {
			return store.SetMetadataBatch(id, map[string]string(KillPendingPatch(closeTestNow)))
		}, want: CommitKillFenced},
		{name: "already closed", change: func(store beads.Store, id string) error {
			if err := store.SetMetadataBatch(id, ClosePatch(closeTestNow, "orphaned")); err != nil {
				return err
			}
			return store.Close(id)
		}, want: CommitClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := beads.NewMemStore()
			created, err := backing.Create(sessionBeadFixture("s-fallback", "open", map[string]string{
				"state":          string(StateAsleep),
				"generation":     "2",
				"instance_token": "token-2",
			}))
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			decided := decidedOn(t, backing, created.ID)
			if tc.change != nil {
				if err := tc.change(backing, created.ID); err != nil {
					t.Fatalf("change: %v", err)
				}
			}
			before, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			fallback := &fallbackTxStore{Store: backing}
			front := NewStore(beads.SessionStore{Store: fallback})
			if _, ok := beads.AtomicConditionalCloserFor(beads.SessionStore{Store: fallback}); ok {
				t.Fatal("fallbackTxStore advertises the atomic close; the test would not reach the fallback")
			}

			closed, err := front.CommitClose(Decide(decided, FactsLegacyClose), failedCreateTerminalPatch(), closeTestNow.Add(time.Second))
			if closed != tc.want || err != nil {
				t.Fatalf("CommitClose = (%v, %v), want %v", closed, err, tc.want)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if tc.want != CommitLanded {
				if fallback.txCalls != 0 {
					t.Fatalf("tx calls = %d, want 0 (refused on the read, nothing written)", fallback.txCalls)
				}
				if !reflect.DeepEqual(before.Metadata, got.Metadata) || before.Status != got.Status {
					t.Fatalf("row rewritten: before %s %v, after %s %v", before.Status, before.Metadata, got.Status, got.Metadata)
				}
				return
			}
			if fallback.txCalls != 1 {
				t.Fatalf("tx calls = %d, want 1 (the controller's single Tx)", fallback.txCalls)
			}
			if got.Status != "closed" {
				t.Fatalf("status = %q, want closed", got.Status)
			}
			for key, want := range failedCreateTerminalPatch() {
				if got.Metadata[key] != want {
					t.Errorf("metadata[%q] = %q, want %q", key, got.Metadata[key], want)
				}
			}
		})
	}
}

// unconfirmedTxStore is a store whose Tx reports a bd staged Tx's read-back
// timeout: every write it applied committed, but the store stopped waiting to
// read the last one back, so the Tx answers ErrWriteUnconfirmed. apply decides
// which of the callback's writes landed first: all of them (the close landed,
// the post-close update's read-back lagged) or the metadata only (the
// pre-close update's read-back lagged, so the close never ran). nil applies
// nothing and fails the Tx outright, as a refused write does.
type unconfirmedTxStore struct {
	beads.Store
	apply func(tx beads.Tx, id string, patch map[string]string) error
	fail  error
}

func (s *unconfirmedTxStore) Tx(_ string, fn func(beads.Tx) error) error {
	rec := &recordingTx{}
	if err := fn(rec); err != nil {
		return err
	}
	if s.apply == nil {
		return s.fail
	}
	if err := s.Store.Tx("applied", func(tx beads.Tx) error { return s.apply(tx, rec.id, rec.patch) }); err != nil {
		return err
	}
	return fmt.Errorf("updating bead %q: %w: waiting for tx update projection: %w", rec.id, beads.ErrWriteUnconfirmed, context.DeadlineExceeded)
}

// recordingTx captures the close Tx's staged metadata and target.
type recordingTx struct {
	beads.Tx
	id    string
	patch map[string]string
}

func (r *recordingTx) SetMetadataBatch(id string, kvs map[string]string) error {
	r.id, r.patch = id, kvs
	return nil
}

func (r *recordingTx) Close(id string) error {
	r.id = id
	return nil
}

func applyMetadataAndClose(tx beads.Tx, id string, patch map[string]string) error {
	if err := tx.SetMetadataBatch(id, patch); err != nil {
		return err
	}
	return tx.Close(id)
}

func applyMetadataOnly(tx beads.Tx, id string, patch map[string]string) error {
	return tx.SetMetadataBatch(id, patch)
}

// TestCommitCloseLandsWhenAnUnconfirmedTxLeftTheRowClosed is the regression
// for POST /session/{id}/close answering 500 "waiting for tx update
// projection: context deadline exceeded" for a close that had landed. A Tx
// whose last read-back timed out is not a failed close: CommitClose re-reads
// the row, and a closed row is the close landing.
func TestCommitCloseLandsWhenAnUnconfirmedTxLeftTheRowClosed(t *testing.T) {
	backing := beads.NewMemStore()
	created := seedOpenSession(t, backing, "s-unconfirmed")
	front := NewStore(beads.SessionStore{Store: &unconfirmedTxStore{Store: backing, apply: applyMetadataAndClose}})

	res, err := front.CommitClose(Decide(decidedOn(t, backing, created.ID), FactsLegacyClose), failedCreateTerminalPatch(), closeTestNow)
	if err != nil || res != CommitLanded {
		t.Fatalf("CommitClose = (%v, %v), want landed: the row reads closed", res, err)
	}
	got, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}

// TestCommitCloseReportsAnUnconfirmedTxThatLeftTheRowOpen: when the re-read
// shows the row still open (the read-back that timed out came before the
// close), the close did not land, and CommitClose keeps ErrWriteUnconfirmed so
// the caller can answer "retry" rather than "failed".
func TestCommitCloseReportsAnUnconfirmedTxThatLeftTheRowOpen(t *testing.T) {
	backing := beads.NewMemStore()
	created := seedOpenSession(t, backing, "s-unconfirmed-open")
	front := NewStore(beads.SessionStore{Store: &unconfirmedTxStore{Store: backing, apply: applyMetadataOnly}})

	res, err := front.CommitClose(Decide(decidedOn(t, backing, created.ID), FactsLegacyClose), failedCreateTerminalPatch(), closeTestNow)
	if !errors.Is(err, beads.ErrWriteUnconfirmed) || res == CommitLanded {
		t.Fatalf("CommitClose = (%v, %v), want ErrWriteUnconfirmed and not landed", res, err)
	}
}

// TestCommitCloseReportsAFailedTx: a Tx the store refused committed nothing
// and stays an error; the re-read is only for an unconfirmed write.
func TestCommitCloseReportsAFailedTx(t *testing.T) {
	backing := beads.NewMemStore()
	created := seedOpenSession(t, backing, "s-failed")
	refused := errors.New("updating bead: exit status 1: disk full")
	front := NewStore(beads.SessionStore{Store: &unconfirmedTxStore{Store: backing, fail: refused}})

	res, err := front.CommitClose(Decide(decidedOn(t, backing, created.ID), FactsLegacyClose), failedCreateTerminalPatch(), closeTestNow)
	if !errors.Is(err, refused) || errors.Is(err, beads.ErrWriteUnconfirmed) || res == CommitLanded {
		t.Fatalf("CommitClose = (%v, %v), want the refused write's error", res, err)
	}
	got, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("row closed after a refused Tx")
	}
}
