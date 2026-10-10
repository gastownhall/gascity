package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

var leaseT0 = time.Date(2099, 10, 9, 12, 0, 0, 0, time.UTC)

const leaseTTL = 4 * time.Minute

// openLeaseStore opens a SQLite store under dir in conditional_writes=require.
// Every process that opens the same dir shares its rows and revisions.
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
	return leaseFixtureOver(t, openLeaseStore(t, t.TempDir()))
}

func leaseFixtureOver(t *testing.T, store beads.Store) leaseFixture {
	t.Helper()
	created := seedPatchFenceSession(t, store, "s-lease")
	return leaseFixture{store: store, front: NewStore(beads.SessionStore{Store: store}), id: created.ID}
}

// onHost names the holders the test takes host, for diagnostics.
func onHost(t *testing.T, host string) {
	t.Helper()
	prev := runtimeLeaseHostname
	runtimeLeaseHostname = func() (string, error) { return host, nil }
	t.Cleanup(func() { runtimeLeaseHostname = prev })
}

func (f leaseFixture) req(city string, now time.Time) RuntimeLeaseRequest {
	return f.reqTTL(city, now, leaseTTL)
}

func (f leaseFixture) reqTTL(city string, now time.Time, ttl time.Duration) RuntimeLeaseRequest {
	return RuntimeLeaseRequest{City: city, Name: "s-lease", ID: f.id, TTL: ttl, now: func() time.Time { return now }}
}

func (f leaseFixture) meta(t *testing.T) map[string]string {
	t.Helper()
	b, err := f.store.Get(f.id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return b.Metadata
}

func (f leaseFixture) write(t *testing.T, patch MetadataPatch) {
	t.Helper()
	if err := f.front.ApplyPatch(f.id, patch); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeLeaseTTL(t *testing.T) {
	for _, c := range []struct{ startup, want time.Duration }{{3 * time.Minute, 4 * time.Minute}, {time.Hour, time.Hour + time.Minute}} {
		if got := RuntimeLeaseTTL(c.startup); got != c.want {
			t.Errorf("RuntimeLeaseTTL(%v) = %v, want %v (no cap)", c.startup, got, c.want)
		}
	}
}

func TestRuntimeLeaseRecordFree(t *testing.T) {
	now := leaseT0
	rec := func(expires time.Duration, ttl string, flock string) map[string]string {
		return map[string]string{
			RuntimeLeaseHolderKey: "host-b/7/n", RuntimeLeaseExpiresKey: now.Add(expires).Format(time.RFC3339),
			RuntimeLeaseTTLKey: ttl, RuntimeLeaseFlockKey: flock,
		}
	}
	for _, c := range []struct {
		name            string
		meta            map[string]string
		free, malformed bool
	}{
		{"never held", map[string]string{}, true, false},
		{"released", map[string]string{RuntimeLeaseEpochKey: "4"}, true, false},
		{"held", rec(time.Minute, "240", "boot/1/2"), false, false},
		{"held under the flock we hold: dead", rec(time.Minute, "240", "boot/1/9"), true, false},
		{"held, no boot ID on either side", rec(time.Minute, "240", ""), false, false},
		{"expired", rec(0, "240", "boot/1/2"), true, false},
		{"expiry unparseable", map[string]string{RuntimeLeaseHolderKey: "h/1/n", RuntimeLeaseExpiresKey: "soon", RuntimeLeaseTTLKey: "240"}, true, true},
		{"TTL missing", rec(time.Minute, "", "boot/1/2"), true, true},
		{"TTL zero", rec(time.Minute, "0", "boot/1/2"), true, true},
		{"expiry at its own TTL plus margin", rec(leaseTTL+RuntimeLeaseMargin, "240", ""), false, false},
		{"expiry past its own TTL plus margin", rec(leaseTTL+RuntimeLeaseMargin+time.Second, "240", ""), true, true},
		{"a longer-TTL holder than the contender", rec(50*time.Minute, "3600", ""), false, false},
		{"a shorter-TTL holder than the contender", rec(30*time.Second, "60", ""), false, false},
	} {
		free, malformed := parseRuntimeLease(c.meta).free(now, "boot/1/9")
		if free != c.free || malformed != c.malformed {
			t.Errorf("%s: free, malformed = %v, %v; want %v, %v", c.name, free, malformed, c.free, c.malformed)
		}
	}
	if free, _ := parseRuntimeLease(rec(time.Minute, "240", "")).free(now, ""); free {
		t.Error("a record and a contender both without a boot ID read as one flock")
	}
}

func TestRuntimeLeaseHoldsMetaNeedsHolderAndEpoch(t *testing.T) {
	l := &RuntimeLease{id: "s", holder: "h/1/a", epoch: 3}
	for _, c := range []struct {
		holder, epoch string
		want          bool
	}{{"h/1/a", "3", true}, {"h/1/a", "4", false}, {"h/1/b", "3", false}} {
		if got := l.HoldsMeta(map[string]string{RuntimeLeaseHolderKey: c.holder, RuntimeLeaseEpochKey: c.epoch}); got != c.want {
			t.Errorf("HoldsMeta(%s, %s) = %v, want %v", c.holder, c.epoch, got, c.want)
		}
	}
}

// TestRuntimeLeaseExcludesSharersAndKeepsTheEpoch: a second holder of the
// flock is refused with the holder's diagnostic, release clears the record
// but keeps the epoch, and the next acquire takes epoch+1.
func TestRuntimeLeaseExcludesSharersAndKeepsTheEpoch(t *testing.T) {
	onHost(t, "host-a")
	f, city := newLeaseFixture(t), t.TempDir()
	first, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil {
		t.Fatalf("first TryRuntimeLease: %v", err)
	}
	if !first.Fenced() || first.Epoch() != 1 || !first.Expires().Equal(leaseT0.Add(leaseTTL)) || !first.SafeUntil().Equal(leaseT0.Add(leaseTTL-RuntimeLeaseSkewAllowance)) {
		t.Fatalf("first lease fenced=%v epoch=%d expires=%v", first.Fenced(), first.Epoch(), first.Expires())
	}
	m := f.meta(t)
	if !strings.HasPrefix(m[RuntimeLeaseHolderKey], "host-a/") || m[RuntimeLeaseEpochKey] != "1" || m[RuntimeLeaseTTLKey] != "240" ||
		(runtimeLeaseBootID() != "" && m[RuntimeLeaseFlockKey] != lockFileIdentity(t, first)) {
		t.Fatalf("record = %v", m)
	}
	_, err = TryRuntimeLease(f.front, f.req(city, leaseT0))
	var busy *RuntimeLeaseBusyError
	if !errors.As(err, &busy) || !busy.Local || !strings.Contains(busy.Holder, "pid ") {
		t.Fatalf("second TryRuntimeLease = %v, want a local busy naming the holder", err)
	}
	first.Release()
	first.Release()
	if m := f.meta(t); m[RuntimeLeaseHolderKey] != "" || m[RuntimeLeaseExpiresKey] != "" || m[RuntimeLeaseFlockKey] != "" || m[RuntimeLeaseEpochKey] != "1" {
		t.Fatalf("released record = %v, want holder cleared and epoch kept", m)
	}
	second, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil || second.Epoch() != 2 {
		t.Fatalf("after release: %v, epoch %v", err, second)
	}
	second.Release()
}

// TestRuntimeLeaseNameFlockOnly: a lease with no row (the closed-row reaper)
// still excludes every holder of the name's flock.
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

// TestRuntimeLeaseRefusesBadRequests: a row lease needs a store, a positive
// TTL, an open row, and the row's own runtime name.
func TestRuntimeLeaseRefusesBadRequests(t *testing.T) {
	f, city := newLeaseFixture(t), t.TempDir()
	for _, c := range []struct {
		name string
		s    *Store
		req  RuntimeLeaseRequest
	}{
		{"zero TTL", f.front, f.reqTTL(city, leaseT0, 0)},
		{"TTL below the margin", f.front, f.reqTTL(city, leaseT0, RuntimeLeaseMargin-time.Second)},
		{"negative TTL", f.front, f.reqTTL(city, leaseT0, -time.Second)},
		{"no store", nil, f.req(city, leaseT0)},
		{"another runtime's name", f.front, RuntimeLeaseRequest{City: city, Name: "s-other", ID: f.id, TTL: leaseTTL}},
	} {
		if _, err := TryRuntimeLease(c.s, c.req); err == nil || errors.Is(err, ErrRuntimeLeaseBusy) {
			t.Errorf("%s: TryRuntimeLease = %v, want refused", c.name, err)
		}
	}
	trimmed := f.req(city, leaseT0)
	trimmed.Name = " s-lease "
	l, err := TryRuntimeLease(f.front, trimmed)
	if err != nil {
		t.Fatalf("a padded name: %v", err)
	}
	l.Release()
	if err := f.store.Close(f.id); err != nil {
		t.Fatal(err)
	}
	if _, err := TryRuntimeLease(f.front, f.req(city, leaseT0)); !errors.Is(err, ErrRuntimeLeaseRowClosed) {
		t.Fatalf("a closed row = %v, want ErrRuntimeLeaseRowClosed", err)
	}
}

// TestRuntimeLeaseOnAClosedRow: a close clears the record, and a release
// after the close writes nothing.
func TestRuntimeLeaseOnAClosedRow(t *testing.T) {
	f, city := newLeaseFixture(t), t.TempDir()
	l, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := f.front.Get(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if closed, err := f.front.Close(expected, "drained", leaseT0); !closed || err != nil {
		t.Fatalf("Close = %v, %v", closed, err)
	}
	if m := f.meta(t); m[RuntimeLeaseHolderKey] != "" || m[RuntimeLeaseEpochKey] != "1" {
		t.Fatalf("closed row's record = %v, want the holder cleared", m)
	}
	f.write(t, MetadataPatch{RuntimeLeaseHolderKey: l.holder})
	l.Release()
	if m := f.meta(t); m[RuntimeLeaseHolderKey] != l.holder {
		t.Fatalf("release wrote the closed row: %v", m)
	}
}

// TestRuntimeLeaseAcrossHosts: holders that share only the store (separate
// lock namespaces, the same hostname). The second is refused until the record
// expires, then takes epoch+1; the first's late write and release are refused
// by the epoch, and its Watch context ends as lost.
func TestRuntimeLeaseAcrossHosts(t *testing.T) {
	f := newLeaseFixture(t)
	onHost(t, "host-b")
	stale, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0))
	if err != nil {
		t.Fatalf("host-b TryRuntimeLease: %v", err)
	}
	stalled, cancel, err := stale.Watch(context.Background(), 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
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
	select {
	case <-stalled.Done():
		if !errors.Is(context.Cause(stalled), ErrRuntimeLeaseLost) {
			t.Fatalf("Watch ended with %v, want ErrRuntimeLeaseLost", context.Cause(stalled))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not cancel the stale holder's call")
	}
	stale.Release()
	if m := f.meta(t); m["state"] == "awake" || m[RuntimeLeaseEpochKey] != "2" || m[RuntimeLeaseHolderKey] != taker.holder {
		t.Fatalf("record after the stale holder = %v, want host-a's epoch 2 untouched", m)
	}
	wrote, err = taker.UpdateMetadataFenced(3, func(Info, PersistedResponse) MetadataPatch {
		return MetadataPatch{"state": "awake"}
	})
	if !wrote || err != nil {
		t.Fatalf("holder's write = %v, %v", wrote, err)
	}
}

// TestRuntimeLeaseMismatchedTTLs: each record is judged by its own TTL, so a
// short-TTL contender waits out a long-TTL holder, and a long-TTL contender
// takes a short-TTL holder's record only at its expiry.
func TestRuntimeLeaseMismatchedTTLs(t *testing.T) {
	for _, c := range []struct {
		name              string
		holder, contender time.Duration
		busyAt, freeAt    time.Duration
	}{
		{"long holder, short contender", time.Hour, time.Minute, 50 * time.Minute, time.Hour},
		{"short holder, long contender", time.Minute, time.Hour, 50 * time.Second, time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newLeaseFixture(t)
			held, err := TryRuntimeLease(f.front, f.reqTTL(t.TempDir(), leaseT0, c.holder))
			if err != nil {
				t.Fatal(err)
			}
			defer held.Release()
			city := t.TempDir()
			if _, err := TryRuntimeLease(f.front, f.reqTTL(city, leaseT0.Add(c.busyAt), c.contender)); !errors.Is(err, ErrRuntimeLeaseBusy) {
				t.Fatalf("contender before the holder's expiry = %v, want busy", err)
			}
			l, err := TryRuntimeLease(f.front, f.reqTTL(city, leaseT0.Add(c.freeAt), c.contender))
			if err != nil {
				t.Fatalf("contender at the holder's expiry: %v", err)
			}
			l.Release()
		})
	}
}

// TestRuntimeLeaseTakesOverADeadSharer: a record naming the flock the
// contender now holds is taken at once; one naming another lock file, or no
// flock identity, waits for its expiry.
func TestRuntimeLeaseTakesOverADeadSharer(t *testing.T) {
	if runtimeLeaseBootID() == "" {
		t.Skip("no boot ID on this platform")
	}
	f, city := newLeaseFixture(t), t.TempDir()
	probe, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	flock := lockFileIdentity(t, probe)
	probe.Release()
	dead := func(flock string) MetadataPatch {
		return MetadataPatch{
			RuntimeLeaseHolderKey: "host-a/99999/dead", RuntimeLeaseEpochKey: "6", RuntimeLeaseTTLKey: "240",
			RuntimeLeaseExpiresKey: leaseT0.Add(time.Minute).Format(time.RFC3339), RuntimeLeaseFlockKey: flock,
		}
	}
	f.write(t, dead(flock))
	if _, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0)); !errors.Is(err, ErrRuntimeLeaseBusy) {
		t.Fatalf("a contender in another lock namespace = %v, want busy until expiry", err)
	}
	// A flock-only holder (the reaper) in between leaves the proof intact.
	reaper, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: city, Name: "s-lease"})
	if err != nil {
		t.Fatal(err)
	}
	reaper.Release()
	l, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil || l.Epoch() != 7 {
		t.Fatalf("takeover: %v, %v; want epoch 7", err, l)
	}
	flock = lockFileIdentity(t, l)
	l.Release()
	boot, rest, _ := strings.Cut(flock, "/")
	dev, rest, _ := strings.Cut(rest, "/")
	ino, token, _ := strings.Cut(rest, "/")
	for _, c := range []struct{ name, flock string }{
		{"no flock identity", ""},
		{"our device and inode under another boot", "another-boot/" + rest},
		{"another device", boot + "/1" + dev + "/" + ino + "/" + token},
		{"another holder's token (a kernel cloned before it)", boot + "/" + dev + "/" + ino + "/0123"},
		{"no token", boot + "/" + dev + "/" + ino + "/"},
	} {
		f.write(t, dead(c.flock))
		// The lock file as an older binary leaves it: no token line.
		if err := os.WriteFile(l.lock.Name(), []byte("pid 1 (gc), since then\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := TryRuntimeLease(f.front, f.req(city, leaseT0)); !errors.Is(err, ErrRuntimeLeaseBusy) {
			t.Errorf("a record with %s = %v, want busy until expiry", c.name, err)
		}
	}
}

// lockFileIdentity is the identity l's acquire recorded: the boot ID, the
// lock file's device and inode as stat reports them, and l's token, which a
// record write rotated into the lock file.
func lockFileIdentity(t *testing.T, l *RuntimeLease) string {
	t.Helper()
	fi, err := l.lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%s/%d/%d/%s", runtimeLeaseBootID(), st.Dev, st.Ino, l.token)
}

// TestRuntimeLeaseTakesAMalformedRecordLoudly: a malformed record is taken,
// and the taking is logged.
func TestRuntimeLeaseTakesAMalformedRecordLoudly(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	f := newLeaseFixture(t)
	f.write(t, MetadataPatch{RuntimeLeaseHolderKey: "host-b/1/n", RuntimeLeaseExpiresKey: leaseT0.Add(24 * time.Hour).Format(time.RFC3339), RuntimeLeaseTTLKey: "240"})
	l, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	if !strings.Contains(buf.String(), "taking a malformed record") {
		t.Fatalf("log = %q, want the malformed takeover named", buf.String())
	}
}

// TestRuntimeLeaseWait: a waiter gets a lease released within its bound, a
// busy error once the bound passes, and its ctx's error once ctx ends.
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
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := WaitRuntimeLease(ctx, f.front, f.req(city, leaseT0), time.Minute); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("Wait past its ctx = %v after %v, want the ctx's error promptly", err, time.Since(start))
	}
	time.AfterFunc(300*time.Millisecond, held.Release)
	got, err := WaitRuntimeLease(context.Background(), f.front, f.req(city, leaseT0), 5*time.Second)
	if err != nil || got.Epoch() != 2 {
		t.Fatalf("Wait over a release: %v, %v", err, got)
	}
	got.Release()
}

// TestRuntimeLeaseWatch: Watch needs a positive interval, ends at SafeUntil
// and at Release with their causes, and a failed read cancels nothing.
func TestRuntimeLeaseWatch(t *testing.T) {
	lowerRuntimeLeaseMinTTL(t)
	backing := openLeaseStore(t, t.TempDir())
	store := &failingReadStore{rowWriteRecorder: &rowWriteRecorder{Store: backing}}
	f := leaseFixtureOver(t, store)
	l, err := TryRuntimeLease(f.front, f.reqTTL(t.TempDir(), leaseT0, RuntimeLeaseSkewAllowance+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Watch(context.Background(), 0); err == nil {
		t.Fatal("Watch took a zero interval")
	}
	store.fail.Store(true)
	expiring, cancel, err := l.Watch(context.Background(), 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	select {
	case <-expiring.Done():
		if !errors.Is(context.Cause(expiring), ErrRuntimeLeaseExpired) {
			t.Fatalf("Watch ended with %v, want ErrRuntimeLeaseExpired (a failed read must cancel nothing)", context.Cause(expiring))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch outlived SafeUntil")
	}
	store.fail.Store(false)
	held, cancelHeld, err := l.Watch(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelHeld()
	l.Release()
	if !errors.Is(context.Cause(held), ErrRuntimeLeaseReleased) {
		t.Fatalf("Watch after Release = %v, want ErrRuntimeLeaseReleased", context.Cause(held))
	}
}

// failingReadStore fails every read while fail is set.
type failingReadStore struct {
	*rowWriteRecorder
	fail atomic.Bool
}

func (s *failingReadStore) Get(id string) (beads.Bead, error) {
	if s.fail.Load() {
		return beads.Bead{}, errors.New("store unreachable")
	}
	return s.rowWriteRecorder.Get(id)
}

// noCASStore declares a stamped source but cannot write conditionally.
type noCASStore struct{ beads.Store }

func (s noCASStore) ConditionalWritesModeSource() beads.Store { return s.Store }

// brokenCASStore fails every conditional write with a store error.
type brokenCASStore struct{ *rowWriteRecorder }

func (s brokenCASStore) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	return errors.New("disk on fire")
}

func TestRuntimeLeaseStoreFailures(t *testing.T) {
	t.Run("require refuses", func(t *testing.T) {
		f := leaseFixtureOver(t, noCASStore{openLeaseStore(t, t.TempDir())})
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
	t.Run("a failed write is an error, not busy", func(t *testing.T) {
		f := leaseFixtureOver(t, brokenCASStore{&rowWriteRecorder{Store: openLeaseStore(t, t.TempDir())}})
		city := t.TempDir()
		if _, err := TryRuntimeLease(f.front, f.req(city, leaseT0)); err == nil || errors.Is(err, ErrRuntimeLeaseBusy) || !strings.Contains(err.Error(), "disk on fire") {
			t.Fatalf("acquire over a failing store = %v, want its error", err)
		}
		l, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: city, Name: "s-lease"})
		if err != nil {
			t.Fatalf("the failed lease kept its flock: %v", err)
		}
		l.Release()
	})
	t.Run("off is best-effort and warns once per city", func(t *testing.T) {
		var buf bytes.Buffer
		prev := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(prev)
		f := leaseFixtureOver(t, beads.NewMemStore())
		cities := []string{t.TempDir(), t.TempDir()}
		for i := 1; i <= 4; i++ {
			l, err := TryRuntimeLease(f.front, f.req(cities[i%2], leaseT0))
			if err != nil || l.Fenced() || f.meta(t)[RuntimeLeaseEpochKey] != strconv.Itoa(i) {
				t.Fatalf("off-mode lease %d: %v, %v, record %v", i, err, l, f.meta(t))
			}
			l.Release()
		}
		for _, city := range cities {
			if n := strings.Count(buf.String(), "city "+city+": the session store has no conditional writes (conditional_writes is off)"); n != 1 {
				t.Fatalf("warned %d times for %s, want once: %q", n, city, buf.String())
			}
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
	backing := openLeaseStore(t, t.TempDir())
	created := seedPatchFenceSession(t, backing, "s-lease")
	store := &racingStore{rowWriteRecorder: &rowWriteRecorder{Store: backing}}
	store.race = func() {
		if err := backing.SetMetadataBatch(created.ID, map[string]string{
			RuntimeLeaseHolderKey: "host-b/7/n", RuntimeLeaseEpochKey: "1", RuntimeLeaseTTLKey: "240",
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

// TestRuntimeLeaseUnderTheSessionMutationLock: a caller holding the row's
// session mutation lock (every Manager start path) can take the lease.
func TestRuntimeLeaseUnderTheSessionMutationLock(t *testing.T) {
	f := newLeaseFixture(t)
	done := make(chan error, 1)
	go func() {
		done <- WithSessionMutationLock(f.id, func() error {
			l, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0))
			l.Release()
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("TryRuntimeLease deadlocked under the session mutation lock")
	}
}

// TestRuntimeLeaseReadsPastTheCache: a takeover written by another process
// reaches the backing store only; the lease's fresh reads still see it.
func TestRuntimeLeaseReadsPastTheCache(t *testing.T) {
	backing := openLeaseStore(t, t.TempDir())
	created := seedPatchFenceSession(t, backing, "s-lease")
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	cached := leaseFixture{store: backing, front: NewStore(beads.SessionStore{Store: cache}), id: created.ID}
	stale, err := TryRuntimeLease(cached.front, cached.req(t.TempDir(), leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	watched, cancel, err := stale.Watch(context.Background(), 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	direct := leaseFixture{store: backing, front: NewStore(beads.SessionStore{Store: backing}), id: created.ID}
	taker, err := TryRuntimeLease(direct.front, direct.req(t.TempDir(), leaseT0.Add(leaseTTL)))
	if err != nil {
		t.Fatal(err)
	}
	defer taker.Release()
	select {
	case <-watched.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Watch read a cached row and missed the takeover")
	}
	if _, err := stale.UpdateMetadataFenced(3, func(Info, PersistedResponse) MetadataPatch { return MetadataPatch{"state": "awake"} }); !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("stale write through the cache = %v, want ErrRuntimeLeaseLost", err)
	}
}

// lowerRuntimeLeaseMinTTL lets the test take leases shorter than the margin.
func lowerRuntimeLeaseMinTTL(t *testing.T) {
	t.Helper()
	prev := runtimeLeaseMinTTL
	runtimeLeaseMinTTL = time.Second
	t.Cleanup(func() { runtimeLeaseMinTTL = prev })
}

// closeHookStore runs hook once, at the next Close, before closing: a
// contender acting between a close's metadata write and its status flip.
type closeHookStore struct {
	beads.Store
	hook func()
}

func (s *closeHookStore) Close(id string) error {
	if hook := s.hook; hook != nil {
		s.hook = nil
		hook()
	}
	return s.Store.Close(id)
}

// TestRuntimeLeaseAcrossANonAtomicClose: on a store with no atomic close the
// lease is not freed before the row closes, a closed row is lost to its
// holder, and a reopen hands it to nobody.
func TestRuntimeLeaseAcrossANonAtomicClose(t *testing.T) {
	store := &closeHookStore{Store: beads.NewMemStore()}
	f := leaseFixtureOver(t, store)
	held, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	watched, cancel, err := held.Watch(context.Background(), 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	city := t.TempDir()
	store.hook = func() {
		if _, err := TryRuntimeLease(f.front, f.req(city, leaseT0)); !errors.Is(err, ErrRuntimeLeaseBusy) {
			t.Errorf("a contender between the close's write and its status flip = %v, want busy", err)
		}
	}
	expected, err := f.front.Get(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if closed, err := f.front.Close(expected, "drained", leaseT0); !closed || err != nil {
		t.Fatalf("Close = %v, %v", closed, err)
	}
	if _, err := TryRuntimeLease(f.front, f.req(city, leaseT0)); !errors.Is(err, ErrRuntimeLeaseRowClosed) {
		t.Fatalf("a contender after the close = %v, want ErrRuntimeLeaseRowClosed", err)
	}
	if _, err := held.UpdateMetadataFenced(3, func(Info, PersistedResponse) MetadataPatch { return MetadataPatch{"state": "awake"} }); !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("the holder's write to its closed row = %v, want ErrRuntimeLeaseLost", err)
	}
	select {
	case <-watched.Done():
		if !errors.Is(context.Cause(watched), ErrRuntimeLeaseLost) {
			t.Fatalf("Watch ended with %v, want ErrRuntimeLeaseLost", context.Cause(watched))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch kept a closed row's lease")
	}
	if err := f.front.SetStatusOpen(f.id); err != nil {
		t.Fatal(err)
	}
	if m := f.meta(t); m[RuntimeLeaseHolderKey] != "" || m[RuntimeLeaseEpochKey] != "1" {
		t.Fatalf("reopened row = %v, want no holder and epoch 1", m)
	}
	if _, err := held.UpdateMetadataFenced(3, func(Info, PersistedResponse) MetadataPatch { return MetadataPatch{"state": "awake"} }); !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("the old holder's write after the reopen = %v, want ErrRuntimeLeaseLost", err)
	}
	l, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil || l.Epoch() != 2 {
		t.Fatalf("a contender after the reopen: %v, %v", err, l)
	}
	l.Release()
	held.Release()
}

// TestRuntimeLeaseAfterRelease: a released lease writes nothing, and Watch on
// it is dead from the start.
func TestRuntimeLeaseAfterRelease(t *testing.T) {
	f := newLeaseFixture(t)
	l, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	if wrote, err := l.UpdateMetadataFenced(3, func(Info, PersistedResponse) MetadataPatch { return MetadataPatch{"state": "awake"} }); wrote || !errors.Is(err, ErrRuntimeLeaseAfterRelease) {
		t.Fatalf("write after release = %v, %v; want ErrRuntimeLeaseAfterRelease", wrote, err)
	}
	ctx, cancel, err := l.Watch(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if !errors.Is(context.Cause(ctx), ErrRuntimeLeaseReleased) {
		t.Fatalf("Watch after release = %v, want dead with ErrRuntimeLeaseReleased", context.Cause(ctx))
	}
}

// TestRuntimeLeaseOnAMalformedName: a row whose name is not a valid session
// name is still leased by its own name, so its cleanup is not blocked.
func TestRuntimeLeaseOnAMalformedName(t *testing.T) {
	store := openLeaseStore(t, t.TempDir())
	created := seedPatchFenceSession(t, store, "bad name:1")
	front := NewStore(beads.SessionStore{Store: store})
	l, err := TryRuntimeLease(front, RuntimeLeaseRequest{City: t.TempDir(), Name: "bad name:1", ID: created.ID, TTL: leaseTTL})
	if err != nil {
		t.Fatalf("leasing a malformed-name row by its name: %v", err)
	}
	l.Release()
}

// TestFencedWriteReportsNothingWrittenOnError: a failed unconditional write
// reports wrote=false.
func TestFencedWriteReportsNothingWrittenOnError(t *testing.T) {
	store := &failingWriteStore{Store: beads.NewMemStore()}
	f := leaseFixtureOver(t, store)
	l, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	store.fail = true
	if wrote, err := l.UpdateMetadataFenced(3, func(Info, PersistedResponse) MetadataPatch { return MetadataPatch{"state": "awake"} }); wrote || err == nil {
		t.Fatalf("failed write = %v, %v; want false and the error", wrote, err)
	}
	store.fail = false
}

// failingWriteStore fails metadata writes while fail is set.
type failingWriteStore struct {
	beads.Store
	fail bool
}

func (s *failingWriteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if s.fail {
		return errors.New("disk full")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// TestRuntimeLeaseHeld: only a live holder holds a lease. A holder on this
// host holds the name's flock; another host's unexpired record holds; a
// record that expired, or whose holder's flock this host now holds (it died),
// is free, so a start it names was abandoned (mc-5a1ma). The probe writes
// nothing and keeps the lock file's proof for the takeover that follows.
func TestRuntimeLeaseHeld(t *testing.T) {
	f, city := newLeaseFixture(t), t.TempDir()
	held := func(now time.Time) bool {
		t.Helper()
		got, err := RuntimeLeaseHeld(f.front, f.req(city, now))
		if err != nil {
			t.Fatalf("RuntimeLeaseHeld: %v", err)
		}
		return got
	}
	if held(leaseT0) {
		t.Fatal("a row nobody leased reads held")
	}
	live, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	if !held(leaseT0) {
		t.Fatal("a live holder on this host reads unheld")
	}
	if got, err := RuntimeLeaseHeld(nil, RuntimeLeaseRequest{City: city, Name: "s-lease"}); err != nil || !got {
		t.Fatalf("the name's flock alone under a live holder = %v, %v; want held", got, err)
	}
	live.Release()

	remote := MetadataPatch{
		RuntimeLeaseHolderKey: "host-b/7/n", RuntimeLeaseEpochKey: "4", RuntimeLeaseTTLKey: "240",
		RuntimeLeaseExpiresKey: leaseT0.Add(time.Minute).Format(time.RFC3339), RuntimeLeaseFlockKey: "another-boot/1/2/0123",
	}
	f.write(t, remote)
	before := f.meta(t)
	if !held(leaseT0) {
		t.Fatal("another host's unexpired record reads unheld")
	}
	if held(leaseT0.Add(time.Minute)) {
		t.Fatal("another host's expired record reads held; it must be reclaimable")
	}
	if after := f.meta(t); after[RuntimeLeaseHolderKey] != before[RuntimeLeaseHolderKey] || after[RuntimeLeaseEpochKey] != "4" {
		t.Fatalf("the probe wrote the record: %v", after)
	}

	if runtimeLeaseBootID() != "" {
		dead, err := TryRuntimeLease(f.front, f.req(city, leaseT0.Add(2*time.Minute)))
		if err != nil {
			t.Fatal(err)
		}
		flock := lockFileIdentity(t, dead)
		dead.Release()
		remote[RuntimeLeaseHolderKey], remote[RuntimeLeaseFlockKey] = "host-a/99999/dead", flock
		f.write(t, remote)
		if held(leaseT0) {
			t.Fatal("a dead holder on this host (its flock free, its record naming it) reads held")
		}
		// The probe kept the lock file's token: the takeover is immediate.
		l, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
		if err != nil {
			t.Fatalf("takeover after the probe: %v; want the dead holder taken over", err)
		}
		l.Release()
	}

	if err := f.store.Close(f.id); err != nil {
		t.Fatal(err)
	}
	if held(leaseT0) {
		t.Fatal("a closed row reads held")
	}
}

// TestRuntimeLeaseFailedAcquireKeepsTheProof: an acquire whose record write
// fails restores the lock file's token, so a dead holder's record is still
// taken over at once afterwards.
func TestRuntimeLeaseFailedAcquireKeepsTheProof(t *testing.T) {
	if runtimeLeaseBootID() == "" {
		t.Skip("no boot ID on this platform")
	}
	store := &failingWriteStore{Store: beads.NewMemStore()}
	f, city := leaseFixtureOver(t, store), t.TempDir()
	dead, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	flock := lockFileIdentity(t, dead)
	dead.Release()
	f.write(t, MetadataPatch{
		RuntimeLeaseHolderKey: "host-a/99999/dead", RuntimeLeaseEpochKey: "1", RuntimeLeaseTTLKey: "240",
		RuntimeLeaseExpiresKey: leaseT0.Add(time.Minute).Format(time.RFC3339), RuntimeLeaseFlockKey: flock,
	})
	store.fail = true
	if _, err := TryRuntimeLease(f.front, f.req(city, leaseT0)); err == nil || errors.Is(err, ErrRuntimeLeaseBusy) {
		t.Fatalf("acquire over a failing write = %v, want the write's error", err)
	}
	store.fail = false
	l, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil || l.Epoch() != 2 {
		t.Fatalf("takeover after a failed acquire: %v, %v; want the dead holder taken over", err, l)
	}
	l.Release()
}

// TestRuntimeLeaseRecordTakenAfterAClone: a kernel cloned before a record
// was taken still has the lock file's older token, so it reads the record's
// holder as alive and waits, however its flock and boot ID look.
func TestRuntimeLeaseRecordTakenAfterAClone(t *testing.T) {
	if runtimeLeaseBootID() == "" {
		t.Skip("no boot ID on this platform")
	}
	f, city := newLeaseFixture(t), t.TempDir()
	before, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	t0 := before.token
	before.Release()
	after, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	record := f.meta(t)
	if after.token == t0 || record[RuntimeLeaseFlockKey] != lockFileIdentity(t, after) {
		t.Fatalf("tokens %q then %q, record %q: want a fresh token recorded", t0, after.token, record[RuntimeLeaseFlockKey])
	}
	unlockRuntimeNameFile(after.lock) // the clone's kernel does not share the holder's flock
	if err := os.WriteFile(after.lock.Name(), []byte("token "+t0+"\npid 1 (gc), since then\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := TryRuntimeLease(f.front, f.req(city, leaseT0)); !errors.Is(err, ErrRuntimeLeaseBusy) {
		t.Fatalf("the clone's contender = %v, want busy until expiry", err)
	}
}

// TestClearRuntimeLeaseForReopen: a generic reopen of a session bead clears
// its record first, keeping the epoch; other beads are left alone.
func TestClearRuntimeLeaseForReopen(t *testing.T) {
	f := newLeaseFixture(t)
	f.write(t, MetadataPatch{RuntimeLeaseHolderKey: "h/1/n", RuntimeLeaseEpochKey: "3", RuntimeLeaseTTLKey: "240"})
	if err := ClearRuntimeLeaseForReopen(f.store, f.id); err != nil {
		t.Fatal(err)
	}
	if m := f.meta(t); m[RuntimeLeaseHolderKey] != "" || m[RuntimeLeaseTTLKey] != "" || m[RuntimeLeaseEpochKey] != "3" {
		t.Fatalf("record after the clear = %v", m)
	}
	other, err := f.store.Create(beads.Bead{Title: "task", Metadata: map[string]string{RuntimeLeaseHolderKey: "h/1/n"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ClearRuntimeLeaseForReopen(f.store, other.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := f.store.Get(other.ID); b.Metadata[RuntimeLeaseHolderKey] != "h/1/n" {
		t.Fatalf("a non-session bead was touched: %v", b.Metadata)
	}
}
