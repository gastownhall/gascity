package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

// The pending-create rollback (rollbackPendingCreateClears) used to fold its
// failed-create close into one store.Tx together with its pre-close clears and
// the post-close session_name clear. That Tx is atomic on native Dolt and
// SQLite, but FileStore (and a cache over it) runs it as separate writes. A
// wake landing between the failed-create metadata and the Close left the row
// status=closed state=awake. The tests below pin the fenced atomic close on
// every store that provides it, the session_name clear strictly after that
// close, the rollback fences, and the unchanged Tx on stores without the
// capability. The race harness (controllerCloseRaceStore,
// controllerCloseBackends, wakeStampOnce) is shared with
// session_beads_close_atomic_test.go.

var rollbackAtomicNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// createRollbackPendingSession seeds a pending create with an explicit session
// name, so the rollback also has its post-close session_name clear to make.
func createRollbackPendingSession(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":              "worker-1",
			"session_name_explicit":     "true",
			"template":                  "worker",
			"state":                     string(session.StateCreating),
			"generation":                "1",
			"instance_token":            "tok-1",
			"pending_create_claim":      "true",
			"pending_create_started_at": rollbackAtomicNow.Add(-time.Minute).Format(time.RFC3339),
			"last_woke_at":              rollbackAtomicNow.Add(-time.Minute).Format(time.RFC3339),
			"sleep_intent":              "idle",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

// rollbackObserved is the Info a rollback caller decided on.
func rollbackObserved(t *testing.T, store beads.Store, id string) session.Info {
	t.Helper()
	info, _, err := sessionFrontDoor(store).GetPersistedResponse(id)
	if err != nil {
		t.Fatalf("GetPersistedResponse: %v", err)
	}
	return info
}

// rollbackVariant is one of the three pending-create rollback helpers, which
// share rollbackPendingCreateClears, and the terminal metadata it must leave.
type rollbackVariant struct {
	name string
	// run reports whether the helper says it applied the rollback. The
	// terminal-error helper returns nothing, so it reports true and the test
	// judges it by the row.
	run  func(info session.Info, store beads.Store, stderr *bytes.Buffer) bool
	want map[string]string
}

func rollbackVariants(now time.Time) []rollbackVariant {
	want := func(preClose map[string]string) map[string]string {
		out := map[string]string{"last_woke_at": ""}
		for k, v := range preClose {
			out[k] = v
		}
		for k, v := range failedCreateClosePatch(now) {
			out[k] = v
		}
		out["session_name"] = ""
		return out
	}
	const reason = "provider says no"
	return []rollbackVariant{
		{
			name: "rollbackPendingCreate",
			run: func(info session.Info, store beads.Store, stderr *bytes.Buffer) bool {
				return rollbackPendingCreate(info, sessionFrontDoor(store), now, stderr) != nil
			},
			want: want(nil),
		},
		{
			name: "rollbackPendingCreateClearingClaim",
			run: func(info session.Info, store beads.Store, stderr *bytes.Buffer) bool {
				return rollbackPendingCreateClearingClaim(info, sessionFrontDoor(store), now, stderr) != nil
			},
			want: want(nil),
		},
		{
			name: "rollbackPendingCreateMarkingTerminal",
			run: func(info session.Info, store beads.Store, stderr *bytes.Buffer) bool {
				rollbackPendingCreateMarkingTerminal(info, sessionFrontDoor(store), now, reason, stderr)
				return true
			},
			want: want(providerTerminalErrorPatch(reason, now)),
		},
	}
}

func assertRolledBackRow(t *testing.T, got beads.Bead, want map[string]string) {
	t.Helper()
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	for key, value := range want {
		if got.Metadata[key] != value {
			t.Errorf("metadata[%q] = %q, want %q (a closed row must carry its failed-create state, never state=awake)", key, got.Metadata[key], value)
		}
	}
}

// TestRollbackPendingCreateKeepsAConcurrentWakeOutOfTheClosedRow is the
// regression. A wake stamps state=awake right before the rollback's terminal
// write. Before the fix, on FileStore and a cache over it, the stamp landed
// between the failed-create metadata and the Close inside the rollback's Tx,
// and the row came to rest status=closed state=awake. Now every store with the
// atomic close commits the rollback's close as one fenced write: the stamp
// wins the fence, the rollback re-reads (awake with the claim still held is
// still its pending create) and retries, and the row ends closed with its
// failed-create metadata. A store without the capability keeps the Tx.
func TestRollbackPendingCreateKeepsAConcurrentWakeOutOfTheClosedRow(t *testing.T) {
	for _, variant := range rollbackVariants(rollbackAtomicNow) {
		for _, backend := range controllerCloseBackends() {
			t.Run(variant.name+"/"+backend.name, func(t *testing.T) {
				store, backing := backend.open(t)
				created := createRollbackPendingSession(t, store)
				race := &controllerCloseRaceStore{Store: store, interfereInTx: backend.txSplits}
				if _, ok := beads.AtomicConditionalCloserFor(race); ok != backend.atomic {
					t.Fatalf("AtomicConditionalCloserFor(%s) = %v, want %v", backend.name, ok, backend.atomic)
				}
				if backend.atomic {
					race.interfere = wakeStampOnce(backing)
				}
				info := rollbackObserved(t, race, created.ID)

				var stderr bytes.Buffer
				if !variant.run(info, race, &stderr) {
					t.Fatalf("%s did not apply: %s", variant.name, stderr.String())
				}
				got, err := backing.Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				assertRolledBackRow(t, got, variant.want)
				if backend.atomic {
					if race.atomicCalls != 2 || race.txCalls != 0 {
						t.Fatalf("atomic/tx calls = %d/%d, want 2/0 (one lost fence, one retry, no split write)", race.atomicCalls, race.txCalls)
					}
				} else if race.atomicCalls != 0 || race.txCalls != 1 {
					t.Fatalf("atomic/tx calls = %d/%d, want 0/1 (the rollback's Tx, unchanged)", race.atomicCalls, race.txCalls)
				}
			})
		}
	}
}

// rollbackOrderStore records the order of the rollback's writes. It carries
// the atomic close only when its backing does.
type rollbackOrderStore struct {
	beads.Store
	backing beads.Store
	t       *testing.T
	mu      sync.Mutex
	ops     []string
	// afterClose runs once, right after a successful atomic close.
	afterClose func(id string)
}

func (s *rollbackOrderStore) record(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, op)
}

func (s *rollbackOrderStore) CloseWithMetadataIfMatch(id string, revision int64, metadata map[string]string) (beads.Bead, error) {
	closer, ok := beads.AtomicConditionalCloserFor(s.Store)
	if !ok {
		return beads.Bead{}, beads.ErrConditionalWriteUnsupported
	}
	if _, clears := metadata["session_name"]; clears {
		s.t.Errorf("the atomic close carries session_name=%q: the name must be released only after the close", metadata["session_name"])
	}
	closed, err := closer.CloseWithMetadataIfMatch(id, revision, metadata)
	if err != nil {
		return closed, err
	}
	s.record("close")
	row, getErr := s.backing.Get(id)
	if getErr != nil {
		s.t.Errorf("Get after close: %v", getErr)
	} else if row.Status != "closed" || row.Metadata["session_name"] != "worker-1" {
		s.t.Errorf("right after the close: status %q session_name %q, want closed and still named", row.Status, row.Metadata["session_name"])
	}
	if s.afterClose != nil {
		hook := s.afterClose
		s.afterClose = nil
		hook(id)
	}
	return closed, nil
}

func (s *rollbackOrderStore) AtomicConditionalCloserHandle() (beads.AtomicConditionalCloser, bool) {
	if _, ok := beads.AtomicConditionalCloserFor(s.Store); !ok {
		return nil, false
	}
	return s, true
}

func (s *rollbackOrderStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs["session_name"]; ok {
		s.record("clear-session_name")
	} else {
		s.record("metadata")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *rollbackOrderStore) Close(id string) error {
	s.record("plain-close")
	return s.Store.Close(id)
}

// TestRollbackPendingCreateReleasesTheNameOnlyAfterTheClose pins the
// name-release ordering on the atomic arm: the session_name clear is its own
// write, strictly after the close has committed, and never part of it.
func TestRollbackPendingCreateReleasesTheNameOnlyAfterTheClose(t *testing.T) {
	for _, backend := range controllerCloseBackends() {
		if !backend.atomic {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := createRollbackPendingSession(t, store)
			order := &rollbackOrderStore{Store: store, backing: backing, t: t}
			info := rollbackObserved(t, order, created.ID)

			var stderr bytes.Buffer
			batch := rollbackPendingCreate(info, sessionFrontDoor(order), rollbackAtomicNow, &stderr)
			if batch == nil {
				t.Fatalf("rollbackPendingCreate did not apply: %s", stderr.String())
			}
			if got := strings.Join(order.ops, ","); got != "close,clear-session_name" {
				t.Fatalf("write order = %s, want close,clear-session_name", got)
			}
			if name, ok := batch["session_name"]; !ok || name != "" {
				t.Fatalf("mirrored batch = %v, want it to carry the session_name clear", batch)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			assertRolledBackRow(t, got, rollbackVariants(rollbackAtomicNow)[0].want)
		})
	}
}

// TestRollbackPendingCreateKeepsTheNameWhenTheCloseFails pins the other half
// of the ordering: a close that does not land releases nothing. A writer that
// wins every fence exhausts the bounded retries, and the row stays this
// incarnation's open pending create, with its name, its claim and its
// in-flight lease, and the confirmed outcome reports the failure.
func TestRollbackPendingCreateKeepsTheNameWhenTheCloseFails(t *testing.T) {
	for _, backend := range controllerCloseBackends() {
		if !backend.atomic {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := createRollbackPendingSession(t, store)
			race := &controllerCloseRaceStore{Store: store}
			race.interfere = func(id string) error {
				return backing.SetMetadata(id, "state", string(session.StateAwake))
			}
			info := rollbackObserved(t, race, created.ID)

			var stderr bytes.Buffer
			if got := rollbackPendingCreateConfirmed(info, info, sessionFrontDoor(race), rollbackAtomicNow, &stderr); got != pendingCreateRollbackFailed {
				t.Fatalf("outcome = %v, want pendingCreateRollbackFailed", got)
			}
			if !strings.Contains(stderr.String(), "lost the revision fence") {
				t.Fatalf("stderr = %q, want the exhausted fence reported", stderr.String())
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "open" || got.Metadata["session_name"] != "worker-1" ||
				got.Metadata["pending_create_claim"] != "true" || got.Metadata["last_woke_at"] == "" {
				t.Fatalf("row = status %q session_name %q claim %q last_woke_at %q, want it untouched",
					got.Status, got.Metadata["session_name"], got.Metadata["pending_create_claim"], got.Metadata["last_woke_at"])
			}
			if race.atomicCalls != 3 || race.txCalls != 0 {
				t.Fatalf("atomic/tx calls = %d/%d, want 3/0 (bounded retries, never a split write)", race.atomicCalls, race.txCalls)
			}
		})
	}
}

// TestRollbackPendingCreateLeavesAReopenedRowItsName covers the post-close
// step's condition: it clears session_name only while the row still reads
// closed. A row reopened between the close and the clear keeps its name, and
// the mirrored batch does not claim the clear.
func TestRollbackPendingCreateLeavesAReopenedRowItsName(t *testing.T) {
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	created := createRollbackPendingSession(t, store)
	order := &rollbackOrderStore{Store: store, backing: store, t: t}
	order.afterClose = func(id string) {
		open := "open"
		if err := store.Update(id, beads.UpdateOpts{Status: &open}); err != nil {
			t.Errorf("reopen: %v", err)
		}
	}
	info := rollbackObserved(t, order, created.ID)

	var stderr bytes.Buffer
	batch := rollbackPendingCreate(info, sessionFrontDoor(order), rollbackAtomicNow, &stderr)
	if batch == nil {
		t.Fatalf("rollbackPendingCreate did not report its close: %s", stderr.String())
	}
	if _, ok := batch["session_name"]; ok {
		t.Fatalf("mirrored batch = %v, want no session_name clear for a row that was reopened", batch)
	}
	if got := strings.Join(order.ops, ","); got != "close" {
		t.Fatalf("write order = %s, want only the close", got)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" || got.Metadata["session_name"] != "worker-1" {
		t.Fatalf("reopened row = status %q session_name %q, want open and still named", got.Status, got.Metadata["session_name"])
	}
}

// TestRollbackPendingCreateFencesHoldOnTheAtomicClose pins #6744's rollback
// fences on the atomic arm. The rollback decides on an older read, and a
// writer from another process can move the row on before the close writes.
// That writer wins the revision fence, and the re-read must pass
// PendingCreateLease.CanRollback again, so the rollback never closes a row that
// is no longer the pending create it observed. The confirmed outcome then
// reports the row as moved on.
func TestRollbackPendingCreateFencesHoldOnTheAtomicClose(t *testing.T) {
	cases := []struct {
		name    string
		move    map[string]string
		outcome pendingCreateRollbackOutcome
		check   func(t *testing.T, got beads.Bead)
	}{
		{
			name:    "new incarnation",
			move:    map[string]string{"instance_token": "tok-2", "generation": "2"},
			outcome: pendingCreateRollbackSuperseded,
			check: func(t *testing.T, got beads.Bead) {
				if got.Metadata["instance_token"] != "tok-2" || got.Metadata["pending_create_claim"] != "true" {
					t.Fatalf("new incarnation = token %q claim %q, want it untouched", got.Metadata["instance_token"], got.Metadata["pending_create_claim"])
				}
			},
		},
		{
			name:    "start completed",
			move:    map[string]string{"state": string(session.StateActive), "pending_create_claim": ""},
			outcome: pendingCreateRollbackSuperseded,
			check: func(t *testing.T, got beads.Bead) {
				if got.Metadata["state"] != string(session.StateActive) {
					t.Fatalf("state = %q, want the completed start left active", got.Metadata["state"])
				}
			},
		},
		{
			name:    "kill fence",
			move:    map[string]string(session.KillPendingPatch(rollbackAtomicNow)),
			outcome: pendingCreateRollbackSuperseded,
			check: func(t *testing.T, got beads.Bead) {
				if got.Metadata["state_reason"] != session.KillPendingReason {
					t.Fatalf("state_reason = %q, want the kill fence left in place", got.Metadata["state_reason"])
				}
			},
		},
	}
	for _, tc := range cases {
		for _, backend := range controllerCloseBackends() {
			if !backend.atomic {
				continue
			}
			t.Run(tc.name+"/"+backend.name, func(t *testing.T) {
				store, backing := backend.open(t)
				created := createRollbackPendingSession(t, store)
				race := &controllerCloseRaceStore{Store: store}
				fired := false
				race.interfere = func(id string) error {
					if fired {
						return nil
					}
					fired = true
					return backing.SetMetadataBatch(id, tc.move)
				}
				info := rollbackObserved(t, race, created.ID)

				var stderr bytes.Buffer
				if got := rollbackPendingCreateConfirmed(info, info, sessionFrontDoor(race), rollbackAtomicNow, &stderr); got != tc.outcome {
					t.Fatalf("outcome = %v, want %v (stderr %q)", got, tc.outcome, stderr.String())
				}
				got, err := backing.Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if got.Status != "open" || got.Metadata["session_name"] != "worker-1" || got.Metadata["state"] == string(session.StateFailedCreate) {
					t.Fatalf("row = status %q state %q session_name %q, want it left open and named", got.Status, got.Metadata["state"], got.Metadata["session_name"])
				}
				tc.check(t, got)
				if race.atomicCalls != 1 || race.txCalls != 0 {
					t.Fatalf("atomic/tx calls = %d/%d, want 1/0 (the lost fence, then the refusal)", race.atomicCalls, race.txCalls)
				}
			})
		}
	}
}

// TestRollbackPendingCreateRefusesAStaleObservationBeforeWriting pins the
// entry fence on the atomic arm: when the row already moved on before the
// rollback read it, nothing is written. On a bare store nothing is attempted.
func TestRollbackPendingCreateRefusesAStaleObservationBeforeWriting(t *testing.T) {
	for _, backend := range controllerCloseBackends() {
		if !backend.atomic {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := createRollbackPendingSession(t, store)
			race := &controllerCloseRaceStore{Store: store}
			stale := rollbackObserved(t, race, created.ID)
			if err := backing.SetMetadataBatch(created.ID, map[string]string{"instance_token": "tok-2", "generation": "2"}); err != nil {
				t.Fatalf("SetMetadataBatch: %v", err)
			}

			var stderr bytes.Buffer
			if batch := rollbackPendingCreate(stale, sessionFrontDoor(race), rollbackAtomicNow, &stderr); batch != nil {
				t.Fatalf("rollbackPendingCreate applied on a stale observation: %v", batch)
			}
			// A cache that has not seen the other process's write still
			// serves the old row, so the rollback's own read passes the
			// fence check. The revision fence then refuses the close, and the
			// re-read from the backing is refused before a second attempt.
			wantAtomic := 0
			if store != backing {
				wantAtomic = 1
			}
			if race.atomicCalls != wantAtomic || race.txCalls != 0 {
				t.Fatalf("atomic/tx calls = %d/%d, want %d/0", race.atomicCalls, race.txCalls, wantAtomic)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "open" || got.Metadata["session_name"] != "worker-1" {
				t.Fatalf("row = status %q session_name %q, want it untouched", got.Status, got.Metadata["session_name"])
			}
		})
	}
}

// TestRollbackPendingCreateFallsBackToTheTxWhenTheCloseIsRefused covers a
// store that advertises the atomic close but refuses it at call time
// (conditional writes disabled). The refusal writes nothing, and the rollback
// keeps its historical Tx.
func TestRollbackPendingCreateFallsBackToTheTxWhenTheCloseIsRefused(t *testing.T) {
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	created := createRollbackPendingSession(t, store)
	store.DisableConditionalWrites = true
	race := &controllerCloseRaceStore{Store: store}
	info := rollbackObserved(t, race, created.ID)

	var stderr bytes.Buffer
	if rollbackPendingCreate(info, sessionFrontDoor(race), rollbackAtomicNow, &stderr) == nil {
		t.Fatalf("rollbackPendingCreate did not apply through the fallback: %s", stderr.String())
	}
	if race.atomicCalls != 1 || race.txCalls != 1 {
		t.Fatalf("atomic/tx calls = %d/%d, want 1/1 (refused, then the Tx)", race.atomicCalls, race.txCalls)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertRolledBackRow(t, got, rollbackVariants(rollbackAtomicNow)[0].want)
}

// TestRollbackPendingCreateUnderRealConcurrentWritersNeverRestsClosedAwake
// races real goroutines: several rollbacks of the same pending create and a
// writer that keeps stamping state=awake on it while it is open. The writer is
// fenced, as the session front door's writers are, so it can only write an
// open row. The Tx's back-to-back writes almost never let a free-running writer
// in, so right before each terminal write (the Tx's Close, or the atomic
// close) the rollback also hands the writer one turn and waits for that stamp
// attempt. On every store with the atomic close, the row must end closed with
// its failed-create metadata and its name released, and exactly one rollback
// may report the close. Before the fix the writer landed in the split Tx and
// the row came to rest status=closed state=awake. Run it with -race.
func TestRollbackPendingCreateUnderRealConcurrentWritersNeverRestsClosedAwake(t *testing.T) {
	for _, backend := range controllerCloseBackends() {
		if !backend.atomic {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := createRollbackPendingSession(t, store)
			writer, ok := backing.(beads.ConditionalWriter)
			if !ok {
				t.Fatalf("%T is not a ConditionalWriter", backing)
			}
			// turns hands the writer one stamp attempt; the writer closes the
			// reply when the attempt is done. writerDone releases every turn
			// request once the writer has stopped.
			turns := make(chan chan struct{})
			writerDone := make(chan struct{})
			race := &controllerCloseRaceStore{Store: store, interfereInTx: backend.txSplits}
			race.interfere = func(string) error {
				reply := make(chan struct{})
				select {
				case turns <- reply:
					<-reply
				case <-writerDone:
				}
				return nil
			}
			info := rollbackObserved(t, race, created.ID)

			// The writer stops after maxStamps successful stamps, so the
			// rollbacks cannot be starved forever.
			const rollbacks, maxStamps = 6, 8
			stamps := 0
			wins := make([]bool, rollbacks)
			start := make(chan struct{})
			stopWriter := make(chan struct{})
			var writers, rolling sync.WaitGroup
			var writerErr error
			writers.Add(1)
			go func() {
				defer writers.Done()
				defer close(writerDone)
				<-start
				for {
					var reply chan struct{}
					select {
					case <-stopWriter:
						return
					case reply = <-turns:
					default:
					}
					stop, err := func() (bool, error) {
						if reply != nil {
							defer close(reply)
						}
						cur, err := backing.Get(created.ID)
						if err != nil {
							return true, err
						}
						if cur.Status == "closed" || stamps == maxStamps {
							return true, nil
						}
						err = writer.UpdateIfMatch(created.ID, cur.Revision, beads.UpdateOpts{
							Metadata: map[string]string{"state": string(session.StateAwake)},
						})
						var precondition *beads.PreconditionFailedError
						switch {
						case err == nil:
							stamps++
						case !errors.As(err, &precondition):
							return true, err
						}
						return false, nil
					}()
					if stop {
						writerErr = err
						return
					}
				}
			}()
			for i := range rollbacks {
				rolling.Add(1)
				go func() {
					defer rolling.Done()
					<-start
					var stderr bytes.Buffer
					wins[i] = rollbackPendingCreate(info, sessionFrontDoor(race), rollbackAtomicNow, &stderr) != nil
				}()
			}
			close(start)
			rolling.Wait()
			close(stopWriter)
			writers.Wait()
			if writerErr != nil {
				t.Fatalf("writer: %v", writerErr)
			}

			winners := 0
			for _, won := range wins {
				if won {
					winners++
				}
			}
			// A writer that keeps winning the fence can exhaust every
			// rollback's bounded retries. The row is then still this
			// incarnation's open pending create, and the next pass rolls it
			// back.
			if winners == 0 {
				var stderr bytes.Buffer
				if rollbackPendingCreate(info, sessionFrontDoor(race), rollbackAtomicNow, &stderr) == nil {
					t.Fatalf("follow-up rollback after the writer stopped failed: %s", stderr.String())
				}
				winners = 1
			}
			if winners != 1 {
				t.Fatalf("%d rollbacks reported the close, want exactly 1", winners)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			assertRolledBackRow(t, got, rollbackVariants(rollbackAtomicNow)[0].want)
		})
	}
}
