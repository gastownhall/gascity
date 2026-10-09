package session

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// terminalCloseTxStore is closeInterferenceStore plus a Tx that counts calls,
// records the commit message, and runs interfere between the Tx's metadata
// write and its Close. That is where the two writes of the non-atomic fallback
// split, so counting both arms proves which one CloseWithTerminalPatch took.
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

// TestCloseWithTerminalPatchAcrossBackends is the front-door half of the
// controller close regression. A writer that does not touch the close's
// premise (a nudge stamp) lands in the window between the close reading the
// row and its terminal write. On every store with the atomic close, the row
// must end closed with the caller's whole patch, through the fenced single
// write only (one lost fence, one retry, no Tx). On a store without the
// capability, the controller's single Tx is kept verbatim, commit message
// included; the interference is off there because that arm's residual race is
// documented, not fixed. TestCloseWithTerminalPatchRefusesWhenItsPremiseMoves
// covers a writer that does change the premise.
func TestCloseWithTerminalPatchAcrossBackends(t *testing.T) {
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

			closed, err := front.CloseWithTerminalPatch(decidedOn(t, backing, created.ID), failedCreateTerminalPatch(), "gc: close failed-create session "+created.ID, closeTestNow)
			if err != nil {
				t.Fatalf("CloseWithTerminalPatch: %v", err)
			}
			if !closed {
				t.Fatal("CloseWithTerminalPatch reported not-closed for an open session")
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
			if want := []string{"gc: close failed-create session " + created.ID}; tracing.atomicCalls != 1 || !reflect.DeepEqual(tracing.txMsgs, want) {
				t.Fatalf("atomic calls = %d, tx commit messages = %q, want 1 and %q (unsupported, then the controller's Tx)", tracing.atomicCalls, tracing.txMsgs, want)
			}
		})
	}
}

// TestCloseWithTerminalPatchRefusesWhenItsPremiseMoves is the regression for
// closing over a concurrent wake. The controller decided to close from an
// older read. A writer then changes a fact that decision rested on and wins
// the revision fence: a wake (state=awake), a wake request, or a new
// incarnation (generation, instance token). Before the fix, the retry re-read
// only the revision and closed the row anyway. On every store with the atomic
// close it must now write nothing and return ErrSessionCloseSuperseded,
// leaving the row to the writer that won; the caller's next pass decides
// again.
func TestCloseWithTerminalPatchRefusesWhenItsPremiseMoves(t *testing.T) {
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

				closed, err := front.CloseWithTerminalPatch(decidedOn(t, backing, created.ID), ClosePatch(closeTestNow, "dead-runtime"), "gc: close session "+created.ID, closeTestNow)
				if !errors.Is(err, ErrSessionCloseSuperseded) || closed {
					t.Fatalf("CloseWithTerminalPatch = (%v, %v), want (false, ErrSessionCloseSuperseded)", closed, err)
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

// TestCloseWithTerminalPatchLeavesAClosedRowAlone pins the atomic arm's
// idempotence: a row that is already closed is reported false and not
// rewritten, so a second closer cannot overwrite the first close's record.
func TestCloseWithTerminalPatchLeavesAClosedRowAlone(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-closed")
	front := NewStore(beads.SessionStore{Store: backing})
	if closed, err := front.CloseWithTerminalPatch(decidedOn(t, backing, created.ID), ClosePatch(closeTestNow, "orphaned"), "first", closeTestNow); err != nil || !closed {
		t.Fatalf("first close = (%v, %v), want (true, nil)", closed, err)
	}
	before, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	tracing := &terminalCloseTxStore{closeInterferenceStore: &closeInterferenceStore{Store: backing}}
	closed, err := NewStore(beads.SessionStore{Store: tracing}).CloseWithTerminalPatch(decidedOn(t, backing, created.ID), failedCreateTerminalPatch(), "second", closeTestNow)
	if err != nil || closed {
		t.Fatalf("second close = (%v, %v), want (false, nil) on a closed row", closed, err)
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

// TestCloseWithTerminalPatchReportsFalseWhenAnotherCloserWins proves the
// exactly-one-winner contract the controller's release cascade relies on: a
// competing closer that lands in the window wins the fence, and this call
// reports false without rewriting that closer's record.
func TestCloseWithTerminalPatchReportsFalseWhenAnotherCloserWins(t *testing.T) {
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
	closed, err := NewStore(beads.SessionStore{Store: tracing}).CloseWithTerminalPatch(decidedOn(t, backing, created.ID), failedCreateTerminalPatch(), "gc: close session", closeTestNow)
	if err != nil || closed {
		t.Fatalf("CloseWithTerminalPatch = (%v, %v), want (false, nil) after losing to a competing closer", closed, err)
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

// TestCloseWithTerminalPatchFallsBackToTheTxWhenRefusedAtCallTime covers a
// store that advertises the atomic close but refuses it with
// ErrConditionalWriteUnsupported (which contractually writes nothing): the
// controller's single Tx runs instead.
func TestCloseWithTerminalPatchFallsBackToTheTxWhenRefusedAtCallTime(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-refused")
	tracing := &terminalCloseTxStore{closeInterferenceStore: &closeInterferenceStore{Store: backing, unsupported: true}}
	closed, err := NewStore(beads.SessionStore{Store: tracing}).CloseWithTerminalPatch(decidedOn(t, backing, created.ID), failedCreateTerminalPatch(), "gc: close session", closeTestNow)
	if err != nil || !closed {
		t.Fatalf("CloseWithTerminalPatch = (%v, %v), want (true, nil) through the fallback", closed, err)
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

// TestCloseWithTerminalPatchYieldsToAKillFence covers #6749's kill fence on the
// atomic arm. `gc session kill` owns a fenced row until it lifts the fence, and
// lifecycle passes must not close it. The controller decided to close on an
// older read, so the fence can appear on the close's own read, or land in the
// window and win the revision fence. Either way the close must write nothing
// and return ErrSessionKillPending, instead of re-reading the fence and closing
// over it; the row stays the kill's.
func TestCloseWithTerminalPatchYieldsToAKillFence(t *testing.T) {
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

			closed, err := NewStore(beads.SessionStore{Store: tracing}).CloseWithTerminalPatch(decidedOn(t, backing, created.ID), ClosePatch(closeTestNow, "dead-runtime"), "gc: close session", closeTestNow.Add(time.Second))
			if !errors.Is(err, ErrSessionKillPending) || closed {
				t.Fatalf("CloseWithTerminalPatch = (%v, %v), want (false, ErrSessionKillPending)", closed, err)
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

// TestCloseWithTerminalPatchClosesPastAStaleKillFence: a fence older than
// KillPendingGrace is ordinary asleep state (the killing CLI died), so the
// controller's close proceeds.
func TestCloseWithTerminalPatchClosesPastAStaleKillFence(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-stale-kill")
	if err := backing.SetMetadataBatch(created.ID, map[string]string(KillPendingPatch(closeTestNow))); err != nil {
		t.Fatalf("stamping the kill fence: %v", err)
	}
	closed, err := NewStore(beads.SessionStore{Store: backing}).CloseWithTerminalPatch(decidedOn(t, backing, created.ID), ClosePatch(closeTestNow, "dead-runtime"), "gc: close session", closeTestNow.Add(KillPendingGrace+time.Second))
	if err != nil || !closed {
		t.Fatalf("CloseWithTerminalPatch = (%v, %v), want (true, nil) past the kill grace", closed, err)
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

// TestCloseWithTerminalPatchFallbackChecksTheRowFirst is the regression for
// the fallback arm closing over a row that moved after the caller's decision
// (mc-zndi7.65). The fallback ran its Tx with no read, so a wake, a new
// incarnation or a `gc session kill` fence that landed between the decision
// and the close was closed over, and an already-closed row was rewritten. It
// must now read the row first and write nothing in each of those cases, as the
// atomic arm does. An unchanged row still closes through the one Tx.
func TestCloseWithTerminalPatchFallbackChecksTheRowFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		// change runs on the backing after the decision, before the close.
		change     func(store beads.Store, id string) error
		wantClosed bool
		wantErr    error
	}{
		{name: "unchanged row", wantClosed: true},
		{name: "unrelated write", change: func(store beads.Store, id string) error {
			return store.SetMetadata(id, unrelatedKey, "2026-09-28T01:02:03Z")
		}, wantClosed: true},
		{name: "wake", change: func(store beads.Store, id string) error {
			return store.SetMetadata(id, "state", string(StateAwake))
		}, wantErr: ErrSessionCloseSuperseded},
		{name: "wake request", change: func(store beads.Store, id string) error {
			return store.SetMetadata(id, "wake_request", "api")
		}, wantErr: ErrSessionCloseSuperseded},
		{name: "new generation", change: func(store beads.Store, id string) error {
			return store.SetMetadata(id, "generation", "3")
		}, wantErr: ErrSessionCloseSuperseded},
		{name: "new instance token", change: func(store beads.Store, id string) error {
			return store.SetMetadata(id, "instance_token", "token-3")
		}, wantErr: ErrSessionCloseSuperseded},
		{name: "kill fence", change: func(store beads.Store, id string) error {
			return store.SetMetadataBatch(id, map[string]string(KillPendingPatch(closeTestNow)))
		}, wantErr: ErrSessionKillPending},
		{name: "already closed", change: func(store beads.Store, id string) error {
			if err := store.SetMetadataBatch(id, ClosePatch(closeTestNow, "orphaned")); err != nil {
				return err
			}
			return store.Close(id)
		}},
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

			closed, err := front.CloseWithTerminalPatch(decided, failedCreateTerminalPatch(), "gc: close session "+created.ID, closeTestNow.Add(time.Second))
			if closed != tc.wantClosed || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("CloseWithTerminalPatch = (%v, %v), want (%v, %v)", closed, err, tc.wantClosed, tc.wantErr)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !tc.wantClosed {
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
