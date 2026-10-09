package session

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

var leaseT0 = time.Date(2099, 10, 9, 12, 0, 0, 0, time.UTC) // in the future, so KeepAlive's real deadline never fires first

const leaseTTL = 4 * time.Minute

// openLeaseStore opens a SQLite store under dir in conditional_writes=require. Every
// process that opens the same dir shares its rows and revisions.
func openLeaseStore(t *testing.T, dir string) beads.Store {
	t.Helper()
	opened, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	s := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = s.CloseStore() })
	if err := beads.StampOpenedStore(s, "SQLiteStore", gate.Require, nil, nil); err != nil {
		t.Fatalf("StampOpenedStore: %v", err)
	}
	return s
}

// leaseFixture is one session row in a CAS-capable store.
type leaseFixture struct {
	store beads.Store
	front *Store
	id    string
}

func newLeaseFixture(t *testing.T) leaseFixture {
	t.Helper()
	store := openLeaseStore(t, t.TempDir())
	created := seedPatchFenceSession(t, store, "s-lease")
	return leaseFixture{store: store, front: NewStore(beads.SessionStore{Store: store}), id: created.ID}
}

// onHost runs the test as host until it ends.
func onHost(t *testing.T, host string) {
	t.Helper()
	prev := runtimeLeaseHostname
	runtimeLeaseHostname = func() (string, error) { return host, nil }
	t.Cleanup(func() { runtimeLeaseHostname = prev })
}

func (f leaseFixture) req(city string, now time.Time) RuntimeLeaseRequest {
	return RuntimeLeaseRequest{City: city, Name: "s-lease", ID: f.id, TTL: leaseTTL, Now: func() time.Time { return now }}
}

func (f leaseFixture) meta(t *testing.T) map[string]string {
	t.Helper()
	b, err := f.store.Get(f.id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return b.Metadata
}

func TestRuntimeLeaseRecordFree(t *testing.T) {
	now := leaseT0
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for _, c := range []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"never held", map[string]string{}, true},
		{"released", map[string]string{RuntimeLeaseEpochKey: "4"}, true},
		{"held on another host", map[string]string{RuntimeLeaseHolderKey: "host-b/7/n", RuntimeLeaseExpiresKey: at(time.Minute)}, false},
		{"expired", map[string]string{RuntimeLeaseHolderKey: "host-b/7/n", RuntimeLeaseExpiresKey: at(0)}, true},
		{"expiry unparseable", map[string]string{RuntimeLeaseHolderKey: "host-b/7/n", RuntimeLeaseExpiresKey: "soon"}, true},
		{"expiry at the skew bound", map[string]string{RuntimeLeaseHolderKey: "host-b/7/n", RuntimeLeaseExpiresKey: at(leaseTTL + RuntimeLeaseMargin)}, false},
		{"expiry past the skew bound", map[string]string{RuntimeLeaseHolderKey: "host-b/7/n", RuntimeLeaseExpiresKey: at(leaseTTL + RuntimeLeaseMargin + time.Second)}, true},
		{"held on this host, flock free: dead", map[string]string{RuntimeLeaseHolderKey: "host-a/7/n", RuntimeLeaseExpiresKey: at(time.Minute)}, true},
		{"host prefix is not the host", map[string]string{RuntimeLeaseHolderKey: "host-ab/7/n", RuntimeLeaseExpiresKey: at(time.Minute)}, false},
	} {
		if got := parseRuntimeLease(c.meta).free(now, leaseTTL, "host-a"); got != c.want {
			t.Errorf("%s: free = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRuntimeLeaseExcludesOnThisHostAndKeepsTheEpoch: a second holder on the
// host is refused by the flock with the holder's diagnostic, release clears
// the record but keeps the epoch, and the next acquire takes epoch+1.
func TestRuntimeLeaseExcludesOnThisHostAndKeepsTheEpoch(t *testing.T) {
	onHost(t, "host-a")
	f, city := newLeaseFixture(t), t.TempDir()
	first, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil {
		t.Fatalf("first TryRuntimeLease: %v", err)
	}
	if !first.Fenced() || first.Epoch() != 1 || !first.Expires().Equal(leaseT0.Add(leaseTTL)) {
		t.Fatalf("first lease fenced=%v epoch=%d expires=%v", first.Fenced(), first.Epoch(), first.Expires())
	}
	if m := f.meta(t); !strings.HasPrefix(m[RuntimeLeaseHolderKey], "host-a/") || m[RuntimeLeaseEpochKey] != "1" {
		t.Fatalf("record = %v", m)
	}
	_, err = TryRuntimeLease(f.front, f.req(city, leaseT0))
	var busy *RuntimeLeaseBusyError
	if !errors.As(err, &busy) || !busy.Local || !strings.Contains(busy.Holder, "pid ") {
		t.Fatalf("second TryRuntimeLease = %v, want a local busy naming the holder", err)
	}
	first.Release()
	first.Release()
	if m := f.meta(t); m[RuntimeLeaseHolderKey] != "" || m[RuntimeLeaseExpiresKey] != "" || m[RuntimeLeaseEpochKey] != "1" {
		t.Fatalf("released record = %v, want holder cleared and epoch kept", m)
	}
	second, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil || second.Epoch() != 2 {
		t.Fatalf("after release: %v, epoch %v", err, second)
	}
	second.Release()
}

// TestRuntimeLeaseNameFlockOnly: a lease with no row (the closed-row reaper)
// still excludes every same-host holder of the name.
func TestRuntimeLeaseNameFlockOnly(t *testing.T) {
	f, city := newLeaseFixture(t), t.TempDir()
	reaper, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: city, Name: "s-lease"})
	if err != nil || reaper.Epoch() != 0 {
		t.Fatalf("flock-only lease: %v", err)
	}
	if _, err := TryRuntimeLease(f.front, f.req(city, leaseT0)); !errors.Is(err, ErrRuntimeLeaseBusy) {
		t.Fatalf("row lease under the reaper = %v, want busy", err)
	}
	if m := f.meta(t); m[RuntimeLeaseHolderKey] != "" {
		t.Fatalf("a refused lease wrote the record: %v", m)
	}
	reaper.Release()
	if _, err := TryRuntimeLease(nil, RuntimeLeaseRequest{Name: "s-lease"}); err == nil {
		t.Fatal("a lease without a city was taken")
	}
}

// TestRuntimeLeaseAcrossHosts: two hosts share only the store. The second
// host is refused until the record expires, then takes epoch+1, and the first
// holder's late write and release are refused by the epoch.
func TestRuntimeLeaseAcrossHosts(t *testing.T) {
	f := newLeaseFixture(t)
	onHost(t, "host-b")
	stale, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0))
	if err != nil {
		t.Fatalf("host-b TryRuntimeLease: %v", err)
	}
	stalled, cancel := stale.KeepAlive(context.Background(), 10*time.Millisecond)
	defer cancel()

	onHost(t, "host-a")
	cityA := t.TempDir()
	_, err = TryRuntimeLease(f.front, f.req(cityA, leaseT0.Add(leaseTTL-time.Second)))
	var busy *RuntimeLeaseBusyError
	if !errors.As(err, &busy) || busy.Local || !strings.HasPrefix(busy.Holder, "host-b/") || !busy.Expires.Equal(leaseT0.Add(leaseTTL)) {
		t.Fatalf("host-a before expiry = %v, want busy on host-b's record", err)
	}
	taker, err := TryRuntimeLease(f.front, f.req(cityA, leaseT0.Add(leaseTTL)))
	if err != nil || taker.Epoch() != 2 {
		t.Fatalf("host-a at expiry: %v, %v; want epoch 2", err, taker)
	}
	defer taker.Release()

	wrote, err := stale.UpdateMetadataFenced(3, func(Info, PersistedResponse) MetadataPatch {
		return MetadataPatch{"state": "awake"}
	})
	if wrote || !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("stale holder's write = %v, %v; want refused as lost", wrote, err)
	}
	stale.Release()
	if m := f.meta(t); m["state"] == "awake" || m[RuntimeLeaseEpochKey] != "2" || m[RuntimeLeaseHolderKey] != taker.holder {
		t.Fatalf("record after the stale holder = %v, want host-a's epoch 2 untouched", m)
	}
	select {
	case <-stalled.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("KeepAlive did not cancel the stale holder's call")
	}
	wrote, err = taker.UpdateMetadataFenced(3, func(Info, PersistedResponse) MetadataPatch {
		return MetadataPatch{"state": "awake"}
	})
	if !wrote || err != nil {
		t.Fatalf("holder's write = %v, %v", wrote, err)
	}
}

// TestRuntimeLeaseTakesOverADeadHolderOnThisHost: a record whose holder is on
// this host, with the flock free, is taken at once, before its expiry.
func TestRuntimeLeaseTakesOverADeadHolderOnThisHost(t *testing.T) {
	onHost(t, "host-a")
	f := newLeaseFixture(t)
	if err := f.front.ApplyPatch(f.id, MetadataPatch{
		RuntimeLeaseHolderKey:  "host-a/99999/dead",
		RuntimeLeaseEpochKey:   "6",
		RuntimeLeaseExpiresKey: leaseT0.Add(time.Minute).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	l, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0))
	if err != nil || l.Epoch() != 7 {
		t.Fatalf("takeover: %v, %v; want epoch 7", err, l)
	}
	l.Release()
}

// TestRuntimeLeaseWait: a waiter gets a lease released within its bound, and
// a busy error once the bound passes.
func TestRuntimeLeaseWait(t *testing.T) {
	f, city := newLeaseFixture(t), t.TempDir()
	held, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := WaitRuntimeLease(context.Background(), f.front, f.req(city, leaseT0), 500*time.Millisecond); !errors.Is(err, ErrRuntimeLeaseBusy) {
		t.Fatalf("Wait past the bound = %v, want busy", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("Wait took %v past a 500ms bound", waited)
	}
	time.AfterFunc(300*time.Millisecond, held.Release)
	got, err := WaitRuntimeLease(context.Background(), f.front, f.req(city, leaseT0), 5*time.Second)
	if err != nil || got.Epoch() != 2 {
		t.Fatalf("Wait over a release: %v, %v", err, got)
	}
	got.Release()
}

// noCASStore declares a stamped source but cannot write conditionally.
type noCASStore struct{ beads.Store }

func (s noCASStore) ConditionalWritesModeSource() beads.Store { return s.Store }

func TestRuntimeLeaseWithoutConditionalWrites(t *testing.T) {
	t.Run("require refuses", func(t *testing.T) {
		store := noCASStore{openLeaseStore(t, t.TempDir())}
		created := seedPatchFenceSession(t, store, "s-lease")
		f := leaseFixture{store: store, front: NewStore(beads.SessionStore{Store: store}), id: created.ID}
		city := t.TempDir()
		if _, err := TryRuntimeLease(f.front, f.req(city, leaseT0)); !errors.Is(err, ErrRuntimeLeaseNoCAS) {
			t.Fatalf("require without CAS = %v, want ErrRuntimeLeaseNoCAS", err)
		}
		l, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: city, Name: "s-lease"})
		if err != nil {
			t.Fatalf("the refused lease kept its flock: %v", err)
		}
		l.Release()
	})
	t.Run("off is best-effort and warns once", func(t *testing.T) {
		var buf bytes.Buffer
		prev := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(prev)
		noCASWarning = sync.Once{}
		store := beads.NewMemStore()
		created := seedPatchFenceSession(t, store, "s-lease")
		f := leaseFixture{store: store, front: NewStore(beads.SessionStore{Store: store}), id: created.ID}
		city := t.TempDir()
		for i := 1; i <= 2; i++ {
			l, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
			if err != nil || l.Fenced() || f.meta(t)[RuntimeLeaseEpochKey] != strconv.Itoa(i) {
				t.Fatalf("off-mode lease %d: %v, %v, record %v", i, err, l, f.meta(t))
			}
			l.Release()
		}
		if n := strings.Count(buf.String(), "no conditional writes (conditional_writes is off)"); n != 1 {
			t.Fatalf("warned %d times, want once: %q", n, buf.String())
		}
	})
}

// racingStore runs race once, right after the next read of the row: another
// host's acquire landing between the lease's read and its write.
type racingStore struct {
	*rowWriteRecorder
	race func()
}

func (s *racingStore) Get(id string) (beads.Bead, error) {
	b, err := s.rowWriteRecorder.Get(id)
	if race := s.race; race != nil {
		s.race = nil
		race()
	}
	return b, err
}

// TestRuntimeLeaseAcquireIsFencedAtTheReadRevision: an acquire that lands
// between the read and the write wins; the lease re-reads and is refused.
func TestRuntimeLeaseAcquireIsFencedAtTheReadRevision(t *testing.T) {
	onHost(t, "host-a")
	backing := openLeaseStore(t, t.TempDir())
	created := seedPatchFenceSession(t, backing, "s-lease")
	store := &racingStore{rowWriteRecorder: &rowWriteRecorder{Store: backing}}
	store.race = func() {
		if err := backing.SetMetadataBatch(created.ID, map[string]string{
			RuntimeLeaseHolderKey:  "host-b/7/n",
			RuntimeLeaseEpochKey:   "1",
			RuntimeLeaseExpiresKey: leaseT0.Add(time.Minute).Format(time.RFC3339),
		}); err != nil {
			t.Error(err)
		}
	}
	f := leaseFixture{store: backing, front: NewStore(beads.SessionStore{Store: store}), id: created.ID}
	if _, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0)); !errors.Is(err, ErrRuntimeLeaseBusy) {
		t.Fatalf("acquire over a racing acquire = %v, want busy", err)
	}
	if m := f.meta(t); !strings.HasPrefix(m[RuntimeLeaseHolderKey], "host-b/") {
		t.Fatalf("record = %v, want host-b's racing acquire kept", m)
	}
}

// TestRuntimeLeaseKeepAliveEndsAtExpiry: a Call under the lease is canceled
// once the record lapses, even if nobody has taken it yet.
func TestRuntimeLeaseKeepAliveEndsAtExpiry(t *testing.T) {
	f := newLeaseFixture(t)
	l, err := TryRuntimeLease(f.front, RuntimeLeaseRequest{City: t.TempDir(), Name: "s-lease", ID: f.id, TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	ctx, cancel := l.KeepAlive(context.Background(), time.Hour)
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("KeepAlive ended with %v, want its expiry deadline", ctx.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("KeepAlive outlived the record's expiry")
	}
}
