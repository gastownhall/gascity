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
//     directory excludes same-host holders, the closed-row reaper included,
//     and dies with its process.
//   - A record on the open session row (holder, epoch, expiry) excludes
//     holders on other hosts. It is taken by conditional write at the row's
//     revision, so a write fenced by HoldsMeta at that revision is refused
//     once anyone else acquires. The epoch is the fencing token: it only
//     grows, and release keeps it.
//
// The record never renews (no revision churn), so its TTL covers a whole
// start (RuntimeLeaseTTL). A crashed holder on this host is taken over at once:
// the recorded host is ours and its flock is free, so the holder is dead. A
// crashed holder on another host blocks until its expiry. Without conditional
// writes the record is best-effort and only the flock excludes, which is
// warned once per process.
//
// Lock order: the name flock, then the row record (its write briefly takes the
// session mutation lock), then identifier flocks, then the session mutation
// lock. A holder never waits on another lease while holding one.

// The lease record's row keys. They are lease class: a holder's own acquire
// and release write them, so no lifecycle premise may compare them.
const (
	RuntimeLeaseHolderKey  = "runtime_lease_holder"
	RuntimeLeaseEpochKey   = "runtime_lease_epoch"
	RuntimeLeaseExpiresKey = "runtime_lease_expires_at"
)

// RuntimeLeaseMargin is the slack RuntimeLeaseTTL adds to the startup timeout,
// and the slack past the TTL beyond which a recorded expiry is malformed.
const RuntimeLeaseMargin = 60 * time.Second

// runtimeLeaseAttempts bounds the record's CAS retries against other writers.
const runtimeLeaseAttempts = 3

// runtimeLeaseWaitPoll is WaitRuntimeLease's retry cadence.
const runtimeLeaseWaitPoll = 200 * time.Millisecond

// RuntimeLeaseTTL is the record's lifetime for a city whose starts are
// bounded by startupTimeout: one start plus RuntimeLeaseMargin.
func RuntimeLeaseTTL(startupTimeout time.Duration) time.Duration {
	return startupTimeout + RuntimeLeaseMargin
}

var (
	// ErrRuntimeLeaseBusy reports that another holder has the lease; see
	// RuntimeLeaseBusyError.
	ErrRuntimeLeaseBusy = errors.New("runtime lease busy")
	// ErrRuntimeLeaseLost reports that the lease's epoch or holder moved: a
	// write fenced on it was refused.
	ErrRuntimeLeaseLost = errors.New("runtime lease lost")
	// ErrRuntimeLeaseNoCAS reports a require-mode store that cannot fence, or
	// one that refused a conditional write at call time.
	ErrRuntimeLeaseNoCAS = errors.New("runtime lease: conditional writes unavailable")
)

// RuntimeLeaseBusyError names who holds a busy lease. Local means the holder
// is a live process on this host (its flock is held); Holder is then the lock
// file's diagnostic body, and otherwise the row's recorded holder.
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
	Name string // the runtime name
	// ID is the open row whose record is taken. Empty takes the flock alone,
	// for a stopper of a runtime no open row owns (the closed-row reaper).
	ID  string
	TTL time.Duration
	Now func() time.Time // nil reads the wall clock
}

// RuntimeLease is a held lease. Release it exactly once; Release is
// idempotent.
type RuntimeLease struct {
	store   *Store
	id      string
	name    string
	holder  string
	epoch   int64
	expires time.Time
	fenced  bool
	now     func() time.Time
	lock    *os.File
	once    sync.Once
}

// Epoch is the lease's fencing token; zero for a flock-only lease.
func (l *RuntimeLease) Epoch() int64 { return l.epoch }

// Expires is when the record lapses; a deadline under the lease must end by it.
func (l *RuntimeLease) Expires() time.Time { return l.expires }

// Fenced reports whether the record was taken by conditional write. False
// means it is best-effort and only same-host holders are excluded.
func (l *RuntimeLease) Fenced() bool { return l.fenced }

// HoldsMeta reports whether row metadata still records this lease. A write
// decided under it and fenced at that read's revision cannot land after
// another holder acquires.
func (l *RuntimeLease) HoldsMeta(meta map[string]string) bool {
	if l.id == "" {
		return true
	}
	rec := parseRuntimeLease(meta)
	return rec.holder == l.holder && rec.epoch == l.epoch
}

// runtimeLeaseHostname is the host half of a holder; tests simulate a second
// host by replacing it.
var runtimeLeaseHostname = os.Hostname

var noCASWarning sync.Once

// TryRuntimeLease takes req's lease without waiting, or returns a
// RuntimeLeaseBusyError. The controller and reapers use it and defer on busy.
func TryRuntimeLease(s *Store, req RuntimeLeaseRequest) (*RuntimeLease, error) {
	if strings.TrimSpace(req.City) == "" || strings.TrimSpace(req.Name) == "" {
		return nil, fmt.Errorf("runtime lease: city %q and name %q are required", req.City, req.Name)
	}
	now := req.Now
	if now == nil {
		now = time.Now
	}
	host, err := runtimeLeaseHostname()
	if err != nil {
		return nil, fmt.Errorf("runtime lease: hostname: %w", err)
	}
	lock, err := lockRuntimeNameFile(req.City, req.Name, now())
	if err != nil {
		return nil, err
	}
	l := &RuntimeLease{store: s, id: req.ID, name: req.Name, now: now, lock: lock}
	if req.ID == "" {
		return l, nil
	}
	l.holder = host + "/" + strconv.Itoa(os.Getpid()) + "/" + runtimeLeaseNonce()
	if err := l.acquireRecord(host, req.TTL); err != nil {
		unlockRuntimeNameFile(lock)
		return nil, err
	}
	return l, nil
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
// once the record is free (runtimeLeaseRecord.free). The flock is already held,
// so a recorded holder on this host is dead.
func (l *RuntimeLease) acquireRecord(host string, ttl time.Duration) error {
	writer, diag, err := beads.ResolveConditionalWriter(l.store.store)
	if err != nil {
		return fmt.Errorf("%w: session %q: %w", ErrRuntimeLeaseNoCAS, l.id, err)
	}
	if l.fenced = writer != nil; !l.fenced {
		noCASWarning.Do(func() {
			reason := "conditional_writes is off"
			if diag != nil {
				reason = diag.PreflightReason
			}
			log.Printf("WARNING: runtime lease: the session store has no conditional writes (%s); "+
				"lease records are best-effort and only same-host starters and stoppers are excluded", reason)
		})
	}
	return WithSessionMutationLock(l.id, func() error {
		for attempt := 0; attempt < runtimeLeaseAttempts; attempt++ {
			bead, err := l.store.validatedBead(l.id)
			if err != nil {
				return err
			}
			now := l.now()
			rec := parseRuntimeLease(bead.Metadata)
			if !rec.free(now, ttl, host) {
				return &RuntimeLeaseBusyError{Name: l.name, Holder: rec.holder, Expires: rec.expires}
			}
			l.epoch, l.expires = rec.epoch+1, now.Add(ttl).UTC().Truncate(time.Second)
			patch := map[string]string{
				RuntimeLeaseHolderKey:  l.holder,
				RuntimeLeaseEpochKey:   strconv.FormatInt(l.epoch, 10),
				RuntimeLeaseExpiresKey: l.expires.Format(time.RFC3339),
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
		return fmt.Errorf("runtime lease: session %q: lost the revision fence %d times", l.id, runtimeLeaseAttempts)
	})
}

// UpdateMetadataFenced is Store.UpdateMetadataFenced under the lease: decide
// runs only on a re-read row that still records it, and the write is fenced at
// that row's revision. A moved record writes nothing and returns
// ErrRuntimeLeaseLost.
func (l *RuntimeLease) UpdateMetadataFenced(attempts int, decide func(Info, PersistedResponse) MetadataPatch) (bool, error) {
	lost := false
	wrote, err := l.store.UpdateMetadataFenced(l.id, attempts, func(info Info, p PersistedResponse) MetadataPatch {
		if lost = !l.HoldsMeta(p.Metadata); lost {
			return nil
		}
		return decide(info, p)
	})
	if err == nil && lost {
		err = fmt.Errorf("%w: session %q", ErrRuntimeLeaseLost, l.id)
	}
	return wrote, err
}

// KeepAlive returns ctx canceled at the record's expiry, or once the row
// stops recording this lease, checked every interval by read only. A provider
// Call runs on it, so a holder that lost its record stops calling. A failed
// read cancels nothing.
func (l *RuntimeLease) KeepAlive(ctx context.Context, every time.Duration) (context.Context, context.CancelFunc) {
	if l.id == "" {
		return context.WithCancel(ctx)
	}
	ctx, cancel := context.WithDeadline(ctx, l.expires)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if bead, err := l.store.validatedBead(l.id); err == nil && !l.HoldsMeta(bead.Metadata) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

// Release clears the record if it is still this lease's (keeping the epoch),
// then drops the flock. A failed clear leaves the record to expire; same-host
// contenders take it over at once regardless.
func (l *RuntimeLease) Release() {
	l.once.Do(func() {
		if l.id != "" {
			_, _ = l.UpdateMetadataFenced(runtimeLeaseAttempts, func(Info, PersistedResponse) MetadataPatch {
				return MetadataPatch{RuntimeLeaseHolderKey: "", RuntimeLeaseExpiresKey: ""}
			})
		}
		unlockRuntimeNameFile(l.lock)
	})
}

// runtimeLeaseRecord is the row's lease record.
type runtimeLeaseRecord struct {
	holder     string
	epoch      int64
	expires    time.Time
	expiresBad bool
}

func parseRuntimeLease(meta map[string]string) runtimeLeaseRecord {
	rec := runtimeLeaseRecord{holder: strings.TrimSpace(meta[RuntimeLeaseHolderKey])}
	rec.epoch, _ = strconv.ParseInt(strings.TrimSpace(meta[RuntimeLeaseEpochKey]), 10, 64)
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(meta[RuntimeLeaseExpiresKey]))
	rec.expires, rec.expiresBad = at, err != nil
	return rec
}

// free reports whether a contender on host, holding the name's flock, may take
// the record: nobody holds it, it expired, its expiry is malformed (unparseable,
// or further out than one TTL plus the margin: clock skew or a bogus write),
// or its holder is on this host, where the free flock proves it dead.
func (r runtimeLeaseRecord) free(now time.Time, ttl time.Duration, host string) bool {
	switch {
	case r.holder == "", r.expiresBad, !now.Before(r.expires), r.expires.Sub(now) > ttl+RuntimeLeaseMargin:
		return true
	}
	h, _, _ := strings.Cut(r.holder, "/")
	return h == host
}

// lockRuntimeNameFile takes the name's flock without blocking. The file's body
// records the holder for a contender's busy diagnostic; it is never a fence.
func lockRuntimeNameFile(city, name string, now time.Time) (*os.File, error) {
	sum := sha256.Sum256([]byte(name))
	path := filepath.Join(citylayout.SessionNameLocksDir(city), "runtime-"+hex.EncodeToString(sum[:])+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("runtime lease: creating lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runtime lease: opening lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close() //nolint:errcheck // closing after a refused lock
		if errors.Is(err, syscall.EWOULDBLOCK) {
			body, _ := os.ReadFile(path)
			return nil, &RuntimeLeaseBusyError{Name: name, Holder: strings.TrimSpace(string(body)), Local: true}
		}
		return nil, fmt.Errorf("runtime lease: locking %q: %w", name, err)
	}
	body := fmt.Sprintf("pid %d (%s), since %s\n", os.Getpid(), filepath.Base(os.Args[0]), now.UTC().Format(time.RFC3339))
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(body), 0)
	}
	return f, nil
}

func unlockRuntimeNameFile(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // close releases it too
	f.Close()                                   //nolint:errcheck // best-effort cleanup
}

func runtimeLeaseNonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
