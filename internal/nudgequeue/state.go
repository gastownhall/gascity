// Package nudgequeue manages the persisted deferred-nudge queue.
package nudgequeue

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// wakeSocketPathLimit caps the canonical socket path length below the
// platform sockaddr_un limit (108 bytes on Linux, 104 on macOS). Matches
// the controllerSocketPathLimit pattern in cmd/gc/controller.go.
const wakeSocketPathLimit = 100

// Reference links a queued nudge back to the object that produced it.
type Reference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// SourceSubmit is the Source of a user's submitted message that the session
// layer queued instead of delivering (a held or dead session, a running turn
// its submit keys would interrupt, a follow-up). It is still the user's
// prompt: it is delivered alone and verbatim, never folded into a deferred
// reminder or injected as hook context.
const SourceSubmit = "submit"

// Item is a persisted deferred nudge.
type Item struct {
	ID                string     `json:"id"`
	BeadID            string     `json:"bead_id,omitempty"`
	Agent             string     `json:"agent"`
	SessionID         string     `json:"session_id,omitempty"`
	ContinuationEpoch string     `json:"continuation_epoch,omitempty"`
	Source            string     `json:"source"`
	Message           string     `json:"message"`
	Reference         *Reference `json:"reference,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	DeliverAfter      time.Time  `json:"deliver_after"`
	ExpiresAt         time.Time  `json:"expires_at"`
	Attempts          int        `json:"attempts,omitempty"`
	// DeliveryBatch is the delivery key of the batch this item was claimed
	// in, pinned at claim time. Until the batch is acked, dead-lettered or
	// definitively rejected, its items are claimed and delivered together
	// under this key, so a redelivery after an unknown outcome is the same
	// delivery to a runtime that deduplicates by key. A rejection clears it,
	// and the next claim pins a new batch.
	DeliveryBatch string    `json:"delivery_batch,omitempty"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	ClaimedAt     time.Time `json:"claimed_at,omitempty"`
	LeaseUntil    time.Time `json:"lease_until,omitempty"`
	DeadAt        time.Time `json:"dead_at,omitempty"`
	// Shadow is the terminal record this item's gc:nudge shadow bead owes,
	// set when the queue commits the item's exit (a dead letter, an ack). The
	// shadow write happens after the queue lock is released.
	Shadow *ShadowDebt `json:"shadow,omitempty"`
}

// ShadowDebt is the terminal record an item's gc:nudge shadow bead owes: the
// queue committed the item's exit under the lock, and the shadow write happens
// after the lock is released. The queue is the authority; the shadow lags it
// and never leads it.
type ShadowDebt struct {
	// State, Reason and CommitBoundary are the terminal fields the shadow is
	// stamped with, and At its terminal time.
	State          string    `json:"state"`
	Reason         string    `json:"reason,omitempty"`
	CommitBoundary string    `json:"commit_boundary,omitempty"`
	At             time.Time `json:"at"`
	// Verify asks the payer to read the shadow first and write only if it is
	// not terminal: the debt of a dead letter an older binary wrote, whose
	// shadow state is unknown.
	Verify bool `json:"verify,omitempty"`
	// Paid marks a dead letter's debt as paid. A retired item is removed once
	// its debt is paid, so it never carries Paid.
	Paid bool `json:"paid,omitempty"`
	// Attempts counts failed payments.
	Attempts int `json:"attempts,omitempty"`
	// NotBefore is the lease of a payer working on the debt, or the backoff
	// after a failed payment. No other payer takes the debt before it.
	NotBefore time.Time `json:"not_before"`
	// Leased marks NotBefore as a payer's lease, not a backoff: a payer took
	// the debt and may be writing the shadow until NotBefore. NotBefore names
	// the lease: a payer records its payment only while the debt still
	// carries the NotBefore it took it with.
	Leased bool `json:"leased,omitempty"`
	// LastError is the last failed payment's error.
	LastError string `json:"last_error,omitempty"`
}

// Bucket names the queue bucket an item is in.
type Bucket string

// The buckets a queued item can be in. An item that has left the queue
// (delivered, withdrawn, or aged out) is in no bucket.
const (
	BucketPending  Bucket = "pending"
	BucketInFlight Bucket = "in_flight"
	BucketDead     Bucket = "dead"
)

// State is the persisted nudge queue snapshot.
type State struct {
	Pending  []Item `json:"pending,omitempty"`
	InFlight []Item `json:"in_flight,omitempty"`
	Dead     []Item `json:"dead,omitempty"`
	// Retired holds acked items until their shadow debt is paid. No verb
	// serves them: claims, lists, Lookup, DueKeys, Empty and the enqueue
	// dedupe ignore this bucket.
	Retired []Item `json:"retired,omitempty"`

	// DispatchSkips counts, by reason, how many times the supervisor
	// dispatch tick's per-session loop has silently skipped a target
	// without delivering (see dispatchAllQueuedNudges in cmd/gc). It is a
	// running total since this state file was first created; there is no
	// reset/rotation. Persisted here (rather than kept in-process) so it
	// stays visible to a `gc nudge status` invocation running in a
	// different process than the supervisor that incremented it.
	DispatchSkips map[string]int64 `json:"dispatch_skips,omitempty"`
}

// SortState orders items deterministically inside each queue bucket.
func SortState(state *State) {
	sort.SliceStable(state.Pending, func(i, j int) bool {
		if !state.Pending[i].DeliverAfter.Equal(state.Pending[j].DeliverAfter) {
			return state.Pending[i].DeliverAfter.Before(state.Pending[j].DeliverAfter)
		}
		if !state.Pending[i].CreatedAt.Equal(state.Pending[j].CreatedAt) {
			return state.Pending[i].CreatedAt.Before(state.Pending[j].CreatedAt)
		}
		return state.Pending[i].ID < state.Pending[j].ID
	})
	sort.SliceStable(state.InFlight, func(i, j int) bool {
		if !state.InFlight[i].LeaseUntil.Equal(state.InFlight[j].LeaseUntil) {
			return state.InFlight[i].LeaseUntil.Before(state.InFlight[j].LeaseUntil)
		}
		if !state.InFlight[i].ClaimedAt.Equal(state.InFlight[j].ClaimedAt) {
			return state.InFlight[i].ClaimedAt.Before(state.InFlight[j].ClaimedAt)
		}
		return state.InFlight[i].ID < state.InFlight[j].ID
	})
	sort.SliceStable(state.Dead, func(i, j int) bool {
		if !state.Dead[i].DeadAt.Equal(state.Dead[j].DeadAt) {
			return state.Dead[i].DeadAt.Before(state.Dead[j].DeadAt)
		}
		if !state.Dead[i].CreatedAt.Equal(state.Dead[j].CreatedAt) {
			return state.Dead[i].CreatedAt.Before(state.Dead[j].CreatedAt)
		}
		return state.Dead[i].ID < state.Dead[j].ID
	})
	sort.SliceStable(state.Retired, func(i, j int) bool {
		ai, aj := debtTime(state.Retired[i]), debtTime(state.Retired[j])
		if !ai.Equal(aj) {
			return ai.Before(aj)
		}
		if !state.Retired[i].CreatedAt.Equal(state.Retired[j].CreatedAt) {
			return state.Retired[i].CreatedAt.Before(state.Retired[j].CreatedAt)
		}
		return state.Retired[i].ID < state.Retired[j].ID
	})
}

func debtTime(item Item) time.Time {
	if item.Shadow == nil {
		return time.Time{}
	}
	return item.Shadow.At
}

// defaultLockWaitTimeout bounds how long WithState waits to acquire the
// queue's exclusive flock before giving up with ErrQueueBusy (ga-2kzci3
// FR1/FR2). Every holder's callback is pure (no store I/O, ga-fsp5w9), so a
// hold is a JSON round trip of the queue, and this budget is orders of
// magnitude above it; a waiter that times out points at a holder that broke
// that rule, not at a budget that is too small.
const defaultLockWaitTimeout = 8 * time.Second

// lockWaitTimeout is the wait budget WithState and UpdateIfChanged use:
// defaultLockWaitTimeout, unless a test shortened it (SetLockWaitForTest).
var lockWaitTimeout atomic.Int64

func init() { lockWaitTimeout.Store(int64(defaultLockWaitTimeout)) }

// SetLockWaitForTest sets the lock wait budget of WithState and
// UpdateIfChanged to d and returns the restore. It exists for tests that hold
// the queue lock on purpose and must not wait the production budget out;
// production code never calls it.
func SetLockWaitForTest(d time.Duration) (restore func()) {
	prev := lockWaitTimeout.Swap(int64(d))
	return func() { lockWaitTimeout.Store(prev) }
}

// ErrQueueBusy reports that the queue lock was not taken within the wait
// budget. Nothing was read or written; a retry may succeed.
var ErrQueueBusy = errors.New("nudge queue busy")

// lockTimeoutError is a lock wait that ran out its budget. Its text is the
// queue's historical timeout message; errors.Is matches it to ErrQueueBusy.
type lockTimeoutError struct{ wait time.Duration }

// Error is the historical timeout text.
func (e lockTimeoutError) Error() string {
	return fmt.Sprintf("locking nudge queue: timed out waiting %s for lock", e.wait)
}

// Is classifies the timeout as ErrQueueBusy.
func (e lockTimeoutError) Is(target error) bool { return target == ErrQueueBusy }

// WithState locks, loads, mutates, and atomically rewrites the queue state.
// The wait to acquire the lock is bounded to defaultLockWaitTimeout
// (ga-2kzci3 FR1/FR2); a caller that needs a different budget -- e.g. one
// that must keep cycling other work under contention -- can call
// withStateBounded directly instead.
func WithState(cityPath string, fn func(*State) error) error {
	return WithStateFor(cityPath, "", fn)
}

// WithStateFor is WithState naming the verb that holds the lock, for the lock
// observer (SetLockObserver).
func WithStateFor(cityPath, verb string, fn func(*State) error) error {
	return WithStateWithin(cityPath, verb, LockWait(), fn)
}

// WithStateWithin is WithStateFor with a caller-chosen wait for the lock: a
// caller that must finish by a deadline of its own (a hook's) waits no
// longer than that leaves. A wait of zero or less tries the lock once.
func WithStateWithin(cityPath, verb string, wait time.Duration, fn func(*State) error) error {
	return withStateBounded(cityPath, verb, wait, fn)
}

// LockWait is the wait budget WithState and UpdateIfChanged give the lock:
// defaultLockWaitTimeout, unless a test shortened it (SetLockWaitForTest).
func LockWait() time.Duration {
	return time.Duration(lockWaitTimeout.Load())
}

// The outcomes a LockSample reports.
const (
	// LockOutcomeOK is a hold whose callback and write succeeded.
	LockOutcomeOK = "ok"
	// LockOutcomeError is a hold (or a lock attempt) that failed.
	LockOutcomeError = "error"
	// LockOutcomeBusy is a wait that ran out its budget (ErrQueueBusy).
	LockOutcomeBusy = "busy"
)

// LockSample describes one acquisition of the queue lock: the verb that took
// it, how long it waited, how long it held it, and how it ended.
type LockSample struct {
	Verb    string
	Outcome string
	Wait    time.Duration
	Hold    time.Duration
}

var lockObserver atomic.Pointer[func(LockSample)]

// SetLockObserver installs fn to receive a LockSample for every acquisition
// of the queue lock in this process, or removes the observer when fn is nil.
// fn runs after the lock is released.
func SetLockObserver(fn func(LockSample)) {
	if fn == nil {
		lockObserver.Store(nil)
		return
	}
	lockObserver.Store(&fn)
}

func observeLock(sample LockSample) {
	if fn := lockObserver.Load(); fn != nil {
		(*fn)(sample)
	}
}

// nudgeQueueLockPollInterval is how often withStateBounded retries a
// non-blocking lock acquisition while waiting for its budget to expire.
const nudgeQueueLockPollInterval = 10 * time.Millisecond

// withStateBounded is WithState's bounded-wait implementation, callable
// directly by a caller that needs a different timeout budget than
// WithState's default -- e.g. the supervisor dispatch tick, which must keep
// cycling other sessions even when the queue is contended (ga-2kzci3
// FR1/FR2). flock offers no notification API, so the bound is enforced by
// polling LOCK_EX|LOCK_NB until either the lock is acquired or the budget
// elapses.
//
// fn runs between flock and unlock, so it must be pure: it reads and mutates
// the in-memory State and returns, with no bead-store call, store open,
// config load, census, socket or subprocess (ga-fsp5w9). A hold whose fn
// leaves the state's bytes unchanged writes nothing.
func withStateBounded(cityPath, verb string, waitTimeout time.Duration, fn func(*State) error) (err error) {
	start := time.Now()
	unlock, err := lockState(cityPath, waitTimeout)
	if err != nil {
		outcome := LockOutcomeError
		if errors.Is(err, ErrQueueBusy) {
			outcome = LockOutcomeBusy
		}
		observeLock(LockSample{Verb: verb, Outcome: outcome, Wait: time.Since(start)})
		return err
	}
	locked := time.Now()
	defer func() {
		unlock()
		outcome := LockOutcomeOK
		if err != nil {
			outcome = LockOutcomeError
		}
		observeLock(LockSample{Verb: verb, Outcome: outcome, Wait: locked.Sub(start), Hold: time.Since(locked)})
	}()

	state, before, err := loadStateBytes(cityPath)
	if err != nil {
		return err
	}
	if err := fn(&state); err != nil {
		return err
	}
	data, err := marshalState(state)
	if err != nil {
		return err
	}
	if before != nil && bytes.Equal(data, before) {
		return nil
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, StatePath(cityPath), data, 0o644); err != nil {
		return fmt.Errorf("write nudge queue: %w", err)
	}
	return nil
}

// UpdateIfChanged is WithState for a verb that is often a no-op. It first runs
// fn on a lock-free snapshot (the atomic rename makes a read a whole committed
// state); when that dry run leaves the state unchanged, it returns without
// taking the lock, linearized at the snapshot read. Otherwise it takes the
// lock and runs fn again on the state read under it, and that run's answer and
// writes are the authoritative ones.
//
// fn runs once or twice, so it must be pure (see withStateBounded) and must
// reset anything it captures (claimed items, buckets) at its start. A dry run
// that fails falls through to the locked run, whose error is the answer.
func UpdateIfChanged(cityPath string, fn func(*State) error) error {
	return UpdateIfChangedFor(cityPath, "", fn)
}

// UpdateIfChangedFor is UpdateIfChanged naming the verb, for the lock observer.
func UpdateIfChangedFor(cityPath, verb string, fn func(*State) error) error {
	return UpdateIfChangedWithin(cityPath, verb, LockWait(), fn)
}

// UpdateIfChangedWithin is UpdateIfChangedFor with a caller-chosen wait for
// the lock (see WithStateWithin).
func UpdateIfChangedWithin(cityPath, verb string, waitTimeout time.Duration, fn func(*State) error) error {
	if changed, err := DryRun(cityPath, fn); err == nil && !changed {
		return nil
	}
	return withStateBounded(cityPath, verb, waitTimeout, fn)
}

// DryRun runs fn on a lock-free snapshot of the queue (a whole committed
// state: writes are atomic renames) and reports whether fn changed it. It
// takes no lock and writes nothing. fn must be pure (see withStateBounded).
func DryRun(cityPath string, fn func(*State) error) (bool, error) {
	state, _, err := loadStateBytes(cityPath)
	if err != nil {
		return false, err
	}
	before, err := marshalState(state)
	if err != nil {
		return false, err
	}
	if err := fn(&state); err != nil {
		return false, err
	}
	after, err := marshalState(state)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(before, after), nil
}

// lockState takes the queue's exclusive flock within waitTimeout and returns
// its release.
func lockState(cityPath string, waitTimeout time.Duration) (func(), error) {
	dir := filepath.Dir(StatePath(cityPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating nudge queue dir: %w", err)
	}

	lockFile, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening nudge queue lock: %w", err)
	}

	deadline := time.Now().Add(waitTimeout)
	for {
		lockErr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			break
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
			lockFile.Close() //nolint:errcheck
			return nil, fmt.Errorf("locking nudge queue: %w", lockErr)
		}
		if !time.Now().Before(deadline) {
			lockFile.Close() //nolint:errcheck
			return nil, lockTimeoutError{wait: waitTimeout}
		}
		time.Sleep(nudgeQueueLockPollInterval)
	}
	return func() {
		syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
		lockFile.Close()                                   //nolint:errcheck
	}, nil
}

// marshalState is the exact bytes a write of state puts in state.json.
func marshalState(state State) ([]byte, error) {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal nudge queue: %w", err)
	}
	return append(data, '\n'), nil
}

// LoadState reads the persisted queue state from disk.
func LoadState(cityPath string) (State, error) {
	state, _, err := loadStateBytes(cityPath)
	return state, err
}

// loadStateBytes is LoadState that also returns the bytes it read: nil when
// the file is absent.
func loadStateBytes(cityPath string) (State, []byte, error) {
	data, err := os.ReadFile(StatePath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil, nil
	}
	if err != nil {
		return State{}, nil, fmt.Errorf("read nudge queue: %w", err)
	}
	if len(data) == 0 {
		return State{}, data, nil
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, nil, fmt.Errorf("parse nudge queue: %w", err)
	}
	SortState(&state)
	return state, data, nil
}

// StatePath returns the persisted queue state path for a city.
func StatePath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "state.json")
}

// LockPath returns the queue state lock path for a city.
func LockPath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "state.lock")
}

// WakeSocketPath returns the path to the supervisor nudge-dispatcher wake
// socket. Producers connect to this path after enqueue to trigger immediate
// dispatch; the supervisor listens on it when daemon.nudge_dispatcher is
// "supervisor".
//
// Preserves the legacy `<city>/.gc/runtime/nudges/wake.sock` location for
// short city paths but falls back to a deterministic short temp-path
// when the legacy pathname is too close to the platform sockaddr_un
// limit. Mirrors the controllerSocketPath pattern in cmd/gc/controller.go.
func WakeSocketPath(cityPath string) string {
	legacy := citylayout.RuntimePath(cityPath, "nudges", "wake.sock")
	if len(legacy) <= wakeSocketPathLimit {
		return legacy
	}
	canonical, err := filepath.Abs(cityPath)
	if err != nil {
		canonical = cityPath
	}
	canonical = filepath.Clean(canonical)
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join("/tmp", "gascity-nudge", fmt.Sprintf("%x.sock", sum[:16]))
}
