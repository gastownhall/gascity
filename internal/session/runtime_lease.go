package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
)

// The runtime lease serializes every starter and stopper of one runtime
// name (ARCH-RESTRUCTURE R2, invariant I-LEASE). It has two layers:
//
//   - A flock per (city, runtime name) under the city's session-name lock
//     directory excludes holders that share the flock (one kernel, one lock
//     file), the closed-row reaper included, and dies with its process.
//   - A record on the open session row excludes holders on other hosts. It is
//     per row, so across hosts it relies on one open row per runtime name. It
//     is taken by conditional write at the row's revision, and with
//     conditional writes a write fenced by HoldsMeta at that revision is
//     refused once anyone else acquires. The epoch is the fencing token: it
//     only grows, and release keeps it.
//
// A closed row holds no lease: acquire refuses one, and a holder that reads
// its row closed has lost its lease (UpdateMetadataFenced, Watch), so no
// close path needs to clear the record before it closes. An atomic close
// clears it in the same write; a reopen clears it (SetStatusOpen,
// RuntimeLeaseClearPatch, ClearRuntimeLeaseForReopen before a generic
// reopen), so a reopened row never hands its old holder back its lease.
//
// The record never renews (no revision churn), so its TTL covers a whole
// start (RuntimeLeaseTTL), and is at least RuntimeLeaseMargin. A crashed
// holder that shared the contender's flock is taken over at once: the record
// names the flock (boot ID, lock file identity, and the random token its
// holder wrote into the lock file) the contender now holds, so its holder is
// dead. The token changes only when a record is written. It keeps a cloned
// kernel (a VM snapshot sharing the boot ID and the disk) from reading a
// record taken after the clone as its own. Cloning a host's memory (VM
// snapshot restore or live clone) while it holds a lease is unsupported: the
// clone duplicates the holder. Any other crashed
// holder blocks until its expiry. Expiry compares wall clocks across hosts,
// which must agree within RuntimeLeaseSkewAllowance; a holder's calls end by
// SafeUntil. Without conditional writes the record is best-effort, only the
// flock excludes, and a warning is logged once per city.
//
// Lock order: the name flock, then the row record, then identifier flocks,
// then the session mutation lock. The record's write takes no lock: the flock
// already excludes this process's other holders, and callers that hold the
// session mutation lock may take a lease with TryRuntimeLease, which never
// waits. A holder never waits on another lease while holding one.

// The lease record's row keys, a settled format every version reads. They are
// lease class: a holder's own acquire and release write them, so no lifecycle
// premise may compare them.
const (
	RuntimeLeaseHolderKey  = "runtime_lease_holder"     // host/pid/nonce
	RuntimeLeaseEpochKey   = "runtime_lease_epoch"      // decimal, only grows
	RuntimeLeaseExpiresKey = "runtime_lease_expires_at" // RFC3339 UTC, holder's clock
	RuntimeLeaseTTLKey     = "runtime_lease_ttl"        // the holder's TTL, whole seconds
	RuntimeLeaseFlockKey   = "runtime_lease_flock"      // boot ID/st_dev/st_ino/lock file token of its flock, "" without a boot ID
)

// RuntimeLeaseMargin is the slack RuntimeLeaseTTL adds to the startup timeout,
// and the slack past a record's own TTL beyond which its expiry is malformed.
const RuntimeLeaseMargin = 60 * time.Second

// RuntimeLeaseSkewAllowance is how far hosts' clocks may disagree. A holder
// stops its calls that long before its record expires (SafeUntil).
const RuntimeLeaseSkewAllowance = 10 * time.Second

// runtimeLeaseAttempts bounds the record's CAS retries against other writers.
const runtimeLeaseAttempts = 3

// runtimeLeaseWaitPoll is WaitRuntimeLease's retry cadence.
const runtimeLeaseWaitPoll = 200 * time.Millisecond

// RuntimeLeaseTTL is the record's lifetime for a city whose starts are
// bounded by startupTimeout: one start plus RuntimeLeaseMargin. A negative
// startupTimeout (a misconfigured city) counts as zero, so the TTL is never
// below the margin.
func RuntimeLeaseTTL(startupTimeout time.Duration) time.Duration {
	return max(startupTimeout, 0) + RuntimeLeaseMargin
}

var (
	// ErrRuntimeLeaseBusy reports that another holder has the lease; see
	// RuntimeLeaseBusyError.
	ErrRuntimeLeaseBusy = errors.New("runtime lease busy")
	// ErrRuntimeLeaseLost reports that the lease's epoch or holder moved: a
	// write under it was refused, or its Watch context canceled.
	ErrRuntimeLeaseLost = errors.New("runtime lease lost")
	// ErrRuntimeLeaseExpired cancels a Watch context at SafeUntil.
	ErrRuntimeLeaseExpired = errors.New("runtime lease expired")
	// ErrRuntimeLeaseReleased cancels a Watch context at Release.
	ErrRuntimeLeaseReleased = errors.New("runtime lease released")
	// ErrRuntimeLeaseNoCAS reports a require-mode store that cannot fence, or
	// one that refused a conditional write at call time.
	ErrRuntimeLeaseNoCAS = errors.New("runtime lease: conditional writes unavailable")
	// ErrRuntimeLeaseRowClosed refuses a record on a closed row.
	ErrRuntimeLeaseRowClosed = errors.New("runtime lease: the session row is closed")
	// ErrRuntimeLeaseAfterRelease refuses a write under a released lease.
	ErrRuntimeLeaseAfterRelease = errors.New("runtime lease: written after release")
	// ErrRuntimeLeaseRenamed refuses a record on a row whose session_name is
	// not the runtime name asked for.
	ErrRuntimeLeaseRenamed = errors.New("runtime lease: the row's runtime name moved")
	// ErrRuntimeLeaseContention reports a record write that lost its
	// revision fence on every attempt.
	ErrRuntimeLeaseContention = errors.New("runtime lease: contended")
	// ErrRuntimeLeaseLocalFS reports the name's lock file or its directory
	// failing on the local filesystem.
	ErrRuntimeLeaseLocalFS = errors.New("runtime lease: local lock file")
)

// RuntimeLeaseBusyError names who holds a busy lease. Local means the holder
// shares this process's flock and is alive; Holder is then the lock file's
// diagnostic body, and otherwise the row's recorded holder.
type RuntimeLeaseBusyError struct {
	Name    string
	Holder  string
	Expires time.Time // zero for a local holder
	Local   bool
}

func (e *RuntimeLeaseBusyError) Error() string {
	if e.Local {
		return fmt.Sprintf("runtime %q is busy: held on this host by %s", e.Name, e.Holder)
	}
	return fmt.Sprintf("runtime %q is busy: held by %s until %s", e.Name, e.Holder, e.Expires.UTC().Format(time.RFC3339))
}

func (e *RuntimeLeaseBusyError) Unwrap() error { return ErrRuntimeLeaseBusy }

// RuntimeLeaseRequest names the lease to take.
type RuntimeLeaseRequest struct {
	City string // the city root; required, its runtime dir holds the flocks
	Name string // the runtime name; with ID, the row's session_name
	// ID is the open row whose record is taken. Empty takes the flock alone,
	// for a stopper of a runtime no open row owns (the closed-row reaper).
	ID string
	// TTL is the record's lifetime, RuntimeLeaseTTL of the startup timeout;
	// required with ID, and at least RuntimeLeaseMargin.
	TTL time.Duration
	now func() time.Time // test hook; nil reads the wall clock
}

// RuntimeLease is a held lease. Release ends it; Release is idempotent.
type RuntimeLease struct {
	store    *Store
	id       string
	name     string
	holder   string
	epoch    int64
	expires  time.Time
	fenced   bool
	now      func() time.Time
	lock     *os.File
	token    string // this acquire's lock file token
	prev     string // the token the lock file held before it
	once     sync.Once
	mu       sync.Mutex
	watchers []context.CancelCauseFunc // nil once released
	released bool
}

// Epoch is the lease's fencing token; zero for a flock-only lease.
func (l *RuntimeLease) Epoch() int64 { return l.epoch }

// Expires is when the record lapses on the holder's clock.
func (l *RuntimeLease) Expires() time.Time { return l.expires }

// SafeUntil is Expires less RuntimeLeaseSkewAllowance: work under the lease,
// a provider call or an effect's deadline, must end by it.
func (l *RuntimeLease) SafeUntil() time.Time { return l.expires.Add(-RuntimeLeaseSkewAllowance) }

// Fenced reports whether the record was taken by conditional write. False
// means it is best-effort and only holders sharing the flock are excluded.
func (l *RuntimeLease) Fenced() bool { return l.fenced }

// HoldsMeta reports whether row metadata still records this lease: its
// holder and its epoch. With conditional writes, a write decided under it and
// fenced at that read's revision cannot land after another holder acquires;
// without them only the read guards it, and a takeover between the read and
// the write is not seen.
func (l *RuntimeLease) HoldsMeta(meta map[string]string) bool {
	if l.id == "" {
		return true
	}
	rec := parseRuntimeLease(meta)
	return rec.holder == l.holder && rec.epoch == l.epoch
}

// runtimeLeaseHostname is the host half of a holder, for diagnostics.
var runtimeLeaseHostname = os.Hostname

var noCASWarned = struct {
	sync.Mutex
	cities map[string]bool
}{cities: map[string]bool{}}

// TryRuntimeLease takes req's lease without waiting, or returns a
// RuntimeLeaseBusyError. The controller and reapers use it and defer on busy.
func TryRuntimeLease(s *Store, req RuntimeLeaseRequest) (*RuntimeLease, error) {
	req.Name = strings.TrimSpace(req.Name)
	if strings.TrimSpace(req.City) == "" || req.Name == "" {
		return nil, fmt.Errorf("runtime lease: city %q and name %q are required", req.City, req.Name)
	}
	if req.ID != "" && (s == nil || req.TTL < runtimeLeaseMinTTL) {
		return nil, fmt.Errorf("runtime lease: session %q: a store and a TTL of at least %v (got %v) are required", req.ID, runtimeLeaseMinTTL, req.TTL)
	}
	now := req.now
	if now == nil {
		now = time.Now
	}
	lock, token, prev, err := lockRuntimeNameFile(req.City, req.Name, now())
	if err != nil {
		return nil, err
	}
	l := &RuntimeLease{store: s, id: req.ID, name: req.Name, now: now, lock: lock, token: token, prev: prev}
	if req.ID == "" {
		return l, nil
	}
	l.holder = newRuntimeLeaseHolder()
	if err := l.acquireRecord(req.City, req.TTL); err != nil {
		writeRuntimeLeaseLockBody(lock, prev, now()) // no record written: keep the previous token
		unlockRuntimeNameFile(lock)
		return nil, err
	}
	return l, nil
}

// TakeRecord adds the record on req's open row to a flock-only lease on the
// same name, taken as TryRuntimeLease takes it. The legacy start holds the
// name's flock first and takes the record only just before PreWake, so a start
// deferred before then writes nothing. A failure leaves the lease flock-only.
func (l *RuntimeLease) TakeRecord(s *Store, req RuntimeLeaseRequest) error {
	switch {
	case l.id != "":
		return fmt.Errorf("runtime lease: runtime %q already holds session %q's record", l.name, l.id)
	case strings.TrimSpace(req.Name) != l.name || req.ID == "" || s == nil || req.TTL < runtimeLeaseMinTTL:
		return fmt.Errorf("runtime lease: runtime %q: a record needs its own name, a session, a store and a TTL of at least %v (got %q, %q, %v)", l.name, runtimeLeaseMinTTL, req.Name, req.ID, req.TTL)
	}
	l.store, l.id, l.holder = s, req.ID, newRuntimeLeaseHolder()
	if req.now != nil {
		l.now = req.now
	}
	if err := l.acquireRecord(req.City, req.TTL); err != nil {
		l.store, l.id, l.holder, l.epoch, l.expires = nil, "", "", 0, time.Time{}
		return err
	}
	return nil
}

// RuntimeLeaseHeld reports whether a live holder has req's lease: the name's
// flock is busy (a holder on this host), or the open row's record is one a
// contender could not take (another host's, unexpired). A dead holder's lease
// is not held: its flock died with it and its record names that flock, or the
// record expired. It writes no record, leaves the lock file's token as it
// found it, and takes the flock only for the read, so a start that reads its
// row's in-flight marker unheld knows that start was abandoned (mc-5a1ma).
func RuntimeLeaseHeld(s *Store, req RuntimeLeaseRequest) (bool, error) {
	now := req.now
	if now == nil {
		now = time.Now
	}
	lock, _, prev, err := lockRuntimeNameFile(req.City, strings.TrimSpace(req.Name), now())
	if errors.Is(err, ErrRuntimeLeaseBusy) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer unlockRuntimeNameFile(lock)
	if req.ID == "" {
		return false, nil
	}
	bead, err := s.freshBead(req.ID)
	if err != nil {
		return false, err
	}
	if bead.Status == "closed" {
		return false, nil
	}
	free, _ := parseRuntimeLease(bead.Metadata).free(now(), runtimeLeaseFlockIdentity(lock, prev))
	return !free, nil
}

// WaitRuntimeLease retries TryRuntimeLease until it succeeds, fails for
// another reason, ctx ends, or bound passes, which returns the last busy error.
// Operators use it (CONTRACT O3: attach waits 10s). It never blocks in the
// kernel, so the wait is bounded.
func WaitRuntimeLease(ctx context.Context, s *Store, req RuntimeLeaseRequest, bound time.Duration) (*RuntimeLease, error) {
	deadline := time.Now().Add(bound)
	for {
		l, err := TryRuntimeLease(s, req)
		if err == nil || !errors.Is(err, ErrRuntimeLeaseBusy) || !time.Now().Add(runtimeLeaseWaitPoll).Before(deadline) {
			return l, err
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), err)
		case <-time.After(runtimeLeaseWaitPoll):
		}
	}
}

// acquireRecord writes this lease onto the row at a fresh read's revision,
// once the record there is free (runtimeLeaseRecord.free).
func (l *RuntimeLease) acquireRecord(city string, ttl time.Duration) error {
	writer, diag, err := beads.ResolveConditionalWriter(l.store.store)
	if err != nil {
		return fmt.Errorf("%w: session %q: %w", ErrRuntimeLeaseNoCAS, l.id, err)
	}
	if l.fenced = writer != nil; !l.fenced {
		warnRuntimeLeaseNoCAS(city, diag)
	}
	mine, held := runtimeLeaseFlockIdentity(l.lock, l.token), runtimeLeaseFlockIdentity(l.lock, l.prev)
	for attempt := 0; attempt < runtimeLeaseAttempts; attempt++ {
		bead, err := l.store.freshBead(l.id)
		if err != nil {
			return err
		}
		if bead.Status == "closed" {
			return fmt.Errorf("%w: session %q", ErrRuntimeLeaseRowClosed, l.id)
		}
		if name := infoFromPersistedBead(bead).SessionName; name != l.name {
			return fmt.Errorf("%w: runtime %q is not session %q's runtime %q", ErrRuntimeLeaseRenamed, l.name, l.id, name)
		}
		now := l.now()
		rec := parseRuntimeLease(bead.Metadata)
		free, malformed := rec.free(now, held)
		if !free {
			return &RuntimeLeaseBusyError{Name: l.name, Holder: rec.holder, Expires: rec.expires}
		}
		if malformed {
			log.Printf("runtime lease: session %q: taking a malformed record (holder %q, expires %q, ttl %q)", l.id,
				rec.holder, bead.Metadata[RuntimeLeaseExpiresKey], bead.Metadata[RuntimeLeaseTTLKey])
		}
		l.epoch, l.expires = rec.epoch+1, now.Add(ttl).UTC().Truncate(time.Second)
		writeRuntimeLeaseLockBody(l.lock, l.token, now)
		patch := map[string]string{
			RuntimeLeaseHolderKey:  l.holder,
			RuntimeLeaseEpochKey:   strconv.FormatInt(l.epoch, 10),
			RuntimeLeaseExpiresKey: l.expires.Format(time.RFC3339),
			RuntimeLeaseTTLKey:     strconv.FormatInt(int64(ttl.Round(time.Second)/time.Second), 10),
			RuntimeLeaseFlockKey:   mine,
		}
		if writer == nil {
			return l.store.ApplyPatch(l.id, patch)
		}
		err = writer.UpdateIfMatch(l.id, bead.Revision, beads.UpdateOpts{Metadata: patch})
		switch {
		case err == nil:
			return nil
		case errors.Is(err, beads.ErrConditionalWriteUnsupported):
			return fmt.Errorf("%w: session %q: %w", ErrRuntimeLeaseNoCAS, l.id, err)
		case !beads.IsPreconditionFailed(err):
			return fmt.Errorf("runtime lease: session %q: %w", l.id, err)
		}
	}
	return fmt.Errorf("%w: session %q: lost the revision fence %d times", ErrRuntimeLeaseContention, l.id, runtimeLeaseAttempts)
}

func warnRuntimeLeaseNoCAS(city string, diag *beads.BeadsDiagnostic) {
	noCASWarned.Lock()
	defer noCASWarned.Unlock()
	if noCASWarned.cities[city] {
		return
	}
	noCASWarned.cities[city] = true
	reason := "conditional_writes is off"
	if diag != nil {
		reason = diag.PreflightReason
	}
	log.Printf("WARNING: runtime lease: city %s: the session store has no conditional writes (%s); "+
		"lease records are best-effort and only same-host starters and stoppers are excluded", city, reason)
}

// UpdateMetadataFenced writes the patch decide returns from a fresh read of
// the row, only while that row is open and still records the lease
// (HoldsMeta). With conditional writes the write is fenced at that read's
// revision, and a lost fence re-reads, up to attempts times. A moved record
// or a closed row writes nothing and returns ErrRuntimeLeaseLost; a released
// lease writes nothing and returns ErrRuntimeLeaseAfterRelease.
func (l *RuntimeLease) UpdateMetadataFenced(attempts int, decide func(Info, PersistedResponse) MetadataPatch) (bool, error) {
	l.mu.Lock()
	released := l.released
	l.mu.Unlock()
	if released {
		return false, fmt.Errorf("%w: session %q", ErrRuntimeLeaseAfterRelease, l.id)
	}
	return l.updateMetadataFenced(attempts, decide)
}

func (l *RuntimeLease) updateMetadataFenced(attempts int, decide func(Info, PersistedResponse) MetadataPatch) (bool, error) {
	writer, _, err := beads.ResolveConditionalWriter(l.store.store)
	if err != nil {
		return false, fmt.Errorf("updating session %q: %w", l.id, err)
	}
	for attempt := 0; attempt < attempts; attempt++ {
		bead, err := l.store.freshBead(l.id)
		if err != nil {
			return false, err
		}
		if !l.holdsRow(bead) {
			return false, fmt.Errorf("%w: session %q", ErrRuntimeLeaseLost, l.id)
		}
		patch := decide(infoFromPersistedBead(bead), PersistedResponseFromBead(bead))
		if len(patch) == 0 {
			return false, nil
		}
		if writer == nil {
			if err := l.store.ApplyPatch(l.id, patch); err != nil {
				return false, err
			}
			return true, nil
		}
		err = writer.UpdateIfMatch(l.id, bead.Revision, beads.UpdateOpts{Metadata: map[string]string(patch)})
		switch {
		case err == nil:
			return true, nil
		case !beads.IsPreconditionFailed(err):
			return false, fmt.Errorf("updating session %q: %w", l.id, err)
		}
	}
	return false, nil
}

// Watch returns ctx canceled at SafeUntil (cause ErrRuntimeLeaseExpired), at
// Release (ErrRuntimeLeaseReleased), or once a fresh read every interval finds
// the row closed or no longer recording the lease (ErrRuntimeLeaseLost). It never writes
// and never renews. A provider Call runs on it, so a holder that lost its
// record stops calling. A failed read cancels nothing.
func (l *RuntimeLease) Watch(ctx context.Context, every time.Duration) (context.Context, context.CancelFunc, error) {
	if every <= 0 {
		return nil, nil, fmt.Errorf("runtime lease: watch interval %v must be positive", every)
	}
	lost, cancelLost := context.WithCancelCause(ctx)
	l.mu.Lock()
	if l.released {
		cancelLost(ErrRuntimeLeaseReleased)
	} else {
		l.watchers = append(l.watchers, cancelLost)
	}
	l.mu.Unlock()
	if l.id == "" {
		return lost, func() { cancelLost(context.Canceled) }, nil
	}
	watched, cancelWatched := context.WithDeadlineCause(lost, time.Now().Add(l.SafeUntil().Sub(l.now())), ErrRuntimeLeaseExpired)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-watched.Done():
				return
			case <-t.C:
				if bead, err := l.store.freshBead(l.id); err == nil && !l.holdsRow(bead) {
					cancelLost(ErrRuntimeLeaseLost)
					return
				}
			}
		}
	}()
	return watched, func() { cancelWatched(); cancelLost(context.Canceled) }, nil
}

// Release cancels the lease's Watch contexts, clears the record if it is
// still this lease's on an open row (keeping the epoch), then
// drops the flock. A failed clear is logged and leaves the record to expire.
// A nil lease releases nothing.
func (l *RuntimeLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.mu.Lock()
		l.released = true
		for _, cancel := range l.watchers {
			cancel(ErrRuntimeLeaseReleased)
		}
		l.watchers = nil
		l.mu.Unlock()
		if l.id != "" {
			_, err := l.updateMetadataFenced(runtimeLeaseAttempts, func(Info, PersistedResponse) MetadataPatch {
				return RuntimeLeaseClearPatch()
			})
			if err != nil && !errors.Is(err, ErrRuntimeLeaseLost) {
				log.Printf("runtime lease: session %q: clearing the record at release: %v", l.id, err)
			}
		}
		unlockRuntimeNameFile(l.lock)
	})
}

// holdsRow reports whether the row bead is open and records this lease.
func (l *RuntimeLease) holdsRow(bead beads.Bead) bool {
	return bead.Status != "closed" && l.HoldsMeta(bead.Metadata)
}

// ClearRuntimeLeaseForReopen clears the runtime lease record on session bead
// id before a generic reopen (the bead API, the bd bridge) flips it open, as
// SetStatusOpen's own clear does: a reopened row holds no lease. A bead that is
// not a session, or holds no record, is left alone.
func ClearRuntimeLeaseForReopen(store beads.Store, id string) error {
	b, err := store.Get(id)
	if err != nil || !IsSessionBeadOrRepairable(b) || strings.TrimSpace(b.Metadata[RuntimeLeaseHolderKey]) == "" {
		return nil
	}
	return store.SetMetadataBatch(id, map[string]string(RuntimeLeaseClearPatch()))
}

// RuntimeLeaseClearPatch clears a record's holder, keeping its epoch. An
// atomic close carries it, and every reopen writes it with the status flip.
func RuntimeLeaseClearPatch() MetadataPatch {
	return MetadataPatch{RuntimeLeaseHolderKey: "", RuntimeLeaseExpiresKey: "", RuntimeLeaseTTLKey: "", RuntimeLeaseFlockKey: ""}
}

// withRuntimeLeaseCleared is patch plus RuntimeLeaseClearPatch, patch's own
// keys winning.
func withRuntimeLeaseCleared(patch MetadataPatch) MetadataPatch {
	out := RuntimeLeaseClearPatch()
	for k, v := range patch {
		out[k] = v
	}
	return out
}

// freshBead reads the session row past any cache.
func (s *Store) freshBead(id string) (beads.Bead, error) {
	if s == nil || s.store.Store == nil {
		return beads.Bead{}, fmt.Errorf("loading session %q: %w", id, beads.ErrNotFound)
	}
	b, err := beads.HandlesFor(s.store.Store).Live.Get(id)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("loading session %q: %w", id, err)
	}
	if strings.TrimSpace(b.ID) == "" || !IsSessionBeadOrRepairable(b) {
		return beads.Bead{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return b, nil
}

// runtimeLeaseRecord is the row's lease record.
type runtimeLeaseRecord struct {
	holder  string
	epoch   int64
	expires time.Time
	ttl     time.Duration
	flock   string
	bad     bool // expiry or TTL unparseable or not positive
}

func parseRuntimeLease(meta map[string]string) runtimeLeaseRecord {
	rec := runtimeLeaseRecord{holder: strings.TrimSpace(meta[RuntimeLeaseHolderKey]), flock: strings.TrimSpace(meta[RuntimeLeaseFlockKey])}
	rec.epoch, _ = strconv.ParseInt(strings.TrimSpace(meta[RuntimeLeaseEpochKey]), 10, 64)
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(meta[RuntimeLeaseExpiresKey]))
	secs, ttlErr := strconv.ParseInt(strings.TrimSpace(meta[RuntimeLeaseTTLKey]), 10, 64)
	rec.expires, rec.ttl = at, time.Duration(secs)*time.Second
	rec.bad = err != nil || ttlErr != nil || secs <= 0
	return rec
}

// free reports whether a contender holding the flock identified by flock may
// take the record: nobody holds it, it expired, its holder recorded that same
// flock (which the contender now holds, so the holder is dead), or it is
// malformed (unparseable, or expiring further out than its own TTL plus the
// margin: clock skew or a bogus write), which is reported.
func (r runtimeLeaseRecord) free(now time.Time, flock string) (free, malformed bool) {
	switch {
	case r.holder == "":
		return true, false
	case r.bad, r.expires.Sub(now) > r.ttl+RuntimeLeaseMargin:
		return true, true
	case !now.Before(r.expires):
		return true, false
	}
	return r.flock != "" && r.flock == flock, false
}

// runtimeLeaseFlockIdentity names the flock f holds as written by the holder
// whose lock file token is token: this boot's kernel ID, the lock file's
// device and inode, and the token. It is "" where the boot ID or the token is
// unknown, so no takeover is immediate.
func runtimeLeaseFlockIdentity(f *os.File, token string) string {
	boot := runtimeLeaseBootID()
	if boot == "" || f == nil || token == "" {
		return ""
	}
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s/%d/%d/%s", boot, st.Dev, st.Ino, token)
}

// runtimeLeaseMinTTL is RuntimeLeaseMargin; the cross-process tests shorten it.
var runtimeLeaseMinTTL = RuntimeLeaseMargin

// runtimeLeaseBootID is this boot's kernel ID, "" where unknown.
var runtimeLeaseBootID = sync.OnceValue(func() string { return strings.TrimSpace(readBootID()) })

// lockRuntimeNameFile takes the name's flock without blocking. It returns the
// lock file's token from before this acquire (prev) and the new random token
// it wrote (token); the rest of the body names the holder for a contender's
// busy diagnostic.
func lockRuntimeNameFile(city, name string, now time.Time) (f *os.File, token, prev string, err error) {
	sum := sha256.Sum256([]byte(name))
	path := filepath.Join(citylayout.SessionNameLocksDir(city), "runtime-"+hex.EncodeToString(sum[:])+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, "", "", fmt.Errorf("%w: creating lock dir: %w", ErrRuntimeLeaseLocalFS, err)
	}
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, "", "", fmt.Errorf("%w: opening lock: %w", ErrRuntimeLeaseLocalFS, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close() //nolint:errcheck // closing after a refused lock
		if errors.Is(err, syscall.EWOULDBLOCK) {
			body, _ := os.ReadFile(path)
			_, holder, _ := strings.Cut(string(body), "\n")
			return nil, "", "", &RuntimeLeaseBusyError{Name: name, Holder: strings.TrimSpace(holder), Local: true}
		}
		return nil, "", "", fmt.Errorf("%w: locking %q: %w", ErrRuntimeLeaseLocalFS, name, err)
	}
	old := make([]byte, 128)
	n, _ := f.ReadAt(old, 0)
	if line, _, _ := strings.Cut(string(old[:n]), "\n"); strings.HasPrefix(line, "token ") {
		prev = strings.TrimPrefix(line, "token ")
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	token = hex.EncodeToString(b[:])
	writeRuntimeLeaseLockBody(f, prev, now)
	return f, token, prev, nil
}

// writeRuntimeLeaseLockBody writes the lock file's body: the token a row
// record names, and the holder for a contender's busy diagnostic. The token
// changes only when a record is written (rotateLockToken), so a flock-only
// holder (the reaper) or a failed acquire never destroys the proof that the
// previous record's holder is dead.
func writeRuntimeLeaseLockBody(f *os.File, token string, now time.Time) {
	body := fmt.Sprintf("token %s\npid %d (%s), since %s\n", token, os.Getpid(), filepath.Base(os.Args[0]), now.UTC().Format(time.RFC3339))
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(body), 0)
	}
}

func unlockRuntimeNameFile(f *os.File) {
	if f == nil {
		return
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // close releases it too
	f.Close()                                   //nolint:errcheck // best-effort cleanup
}

// newRuntimeLeaseHolder names one acquire: host/pid/nonce.
func newRuntimeLeaseHolder() string {
	host, err := runtimeLeaseHostname()
	if err != nil {
		host = "unknown"
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return host + "/" + strconv.Itoa(os.Getpid()) + "/" + hex.EncodeToString(b[:])
}
