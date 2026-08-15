package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// leaseRenewingMemStore is a MemStore that also records lease renewals, with
// per-bead error injection. It stands in for a BdStore in sweep tests.
type leaseRenewingMemStore struct {
	*beads.MemStore
	renews   []string
	renewErr map[string]error
}

func (s *leaseRenewingMemStore) RenewLease(id, holder string) error {
	s.renews = append(s.renews, id+"/"+holder)
	if err := s.renewErr[id]; err != nil {
		return err
	}
	return nil
}

func newLeaseRenewingMemStore() *leaseRenewingMemStore {
	return &leaseRenewingMemStore{MemStore: beads.NewMemStore(), renewErr: map[string]error{}}
}

// seedInProgressBead creates a bead held in_progress by assignee, the shape a
// hook-claimed work bead has while a session works it.
func seedInProgressBead(t *testing.T, store beads.Store, assignee string) string {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}
	return b.ID
}

// --- leaseRenewalInterval ---

// TestLeaseRenewalIntervalIsOneThirdOfTTL pins the cadence derivation: renew
// at one third of the lease TTL so a renewal can miss twice before the lease
// lapses (gas-76r acceptance: interval strictly shorter than the window, with
// margin, derived from the single configured value).
func TestLeaseRenewalIntervalIsOneThirdOfTTL(t *testing.T) {
	if got := leaseRenewalInterval(5 * time.Minute); got != 100*time.Second {
		t.Errorf("leaseRenewalInterval(5m) = %v, want 100s", got)
	}
	if got := leaseRenewalInterval(90 * time.Second); got != 30*time.Second {
		t.Errorf("leaseRenewalInterval(90s) = %v, want 30s", got)
	}
}

// TestLeaseRenewalIntervalDegeneratesToEveryPass proves a nonsense TTL (zero
// or negative) falls back to renewing on every pass rather than disabling
// renewal: with no meaningful window to derive a cadence from, renewing too
// often is safe and renewing never is the bug this exists to fix.
func TestLeaseRenewalIntervalDegeneratesToEveryPass(t *testing.T) {
	if got := leaseRenewalInterval(0); got != 0 {
		t.Errorf("leaseRenewalInterval(0) = %v, want 0", got)
	}
	if got := leaseRenewalInterval(-time.Minute); got != 0 {
		t.Errorf("leaseRenewalInterval(-1m) = %v, want 0", got)
	}
}

// --- sweepClaimLeaseRenewals ---

// TestSweepClaimLeaseRenewalsRenewsOnlyRunningHolders proves the sweep renews
// exactly the in_progress beads whose assignee is a running session: a bead
// held by a session that is not running must NOT be renewed (its lease going
// stale is what lets bd reclaim recover dead workers' beads), and unassigned
// or open beads carry no lease to renew.
func TestSweepClaimLeaseRenewalsRenewsOnlyRunningHolders(t *testing.T) {
	store := newLeaseRenewingMemStore()
	held := seedInProgressBead(t, store, "sess-running")
	seedInProgressBead(t, store, "sess-ghost")
	if _, err := store.Create(beads.Bead{Title: "unclaimed", Type: "task"}); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	res := sweepClaimLeaseRenewals(leaseSweepConfig{
		targets:   []leaseSweepTarget{{label: "rig", store: store}},
		running:   map[string]bool{"sess-running": true},
		stderr:    &stderr,
		logPrefix: "test",
	})

	if res.renewed != 1 {
		t.Errorf("renewed = %d, want 1", res.renewed)
	}
	want := held + "/sess-running"
	if len(store.renews) != 1 || store.renews[0] != want {
		t.Errorf("renew calls = %v, want [%s]", store.renews, want)
	}
}

// TestSweepClaimLeaseRenewalsSkipsStoresWithoutCapability proves a store with
// no lease semantics (e.g. the native store) is skipped silently: capability
// absence is not a failure and must not spam the controller log every pass.
func TestSweepClaimLeaseRenewalsSkipsStoresWithoutCapability(t *testing.T) {
	store := beads.NewMemStore()
	seedInProgressBead(t, store, "sess-running")

	var stderr bytes.Buffer
	res := sweepClaimLeaseRenewals(leaseSweepConfig{
		targets:   []leaseSweepTarget{{label: "rig", store: store}},
		running:   map[string]bool{"sess-running": true},
		stderr:    &stderr,
		logPrefix: "test",
	})

	if res.renewed != 0 {
		t.Errorf("renewed = %d, want 0", res.renewed)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// TestSweepClaimLeaseRenewalsContinuesPastRenewalError proves one failed
// renewal (holder lost the lease, bead closed) is logged loudly and does not
// stop the rest of the sweep — the other live sessions' leases still renew.
func TestSweepClaimLeaseRenewalsContinuesPastRenewalError(t *testing.T) {
	store := newLeaseRenewingMemStore()
	failing := seedInProgressBead(t, store, "sess-a")
	store.renewErr[failing] = errors.New("lease not found for holder")
	seedInProgressBead(t, store, "sess-b")

	var stderr bytes.Buffer
	res := sweepClaimLeaseRenewals(leaseSweepConfig{
		targets:   []leaseSweepTarget{{label: "rig", store: store}},
		running:   map[string]bool{"sess-a": true, "sess-b": true},
		stderr:    &stderr,
		logPrefix: "test",
	})

	if res.renewed != 1 {
		t.Errorf("renewed = %d, want 1", res.renewed)
	}
	if len(store.renews) != 2 {
		t.Errorf("renew calls = %v, want both beads attempted", store.renews)
	}
	if !strings.Contains(stderr.String(), failing) {
		t.Errorf("stderr %q does not mention failing bead %s", stderr.String(), failing)
	}
}

// TestSweepClaimLeaseRenewalsTreatsUnsupportedAsStoreSkip proves a renewal
// that reports ErrLeaseRenewalUnsupported (a lease-capable wrapper over a
// lease-less backing store, e.g. CachingStore over the native store) skips the
// store without logging: it is the same capability absence as the store not
// implementing LeaseRenewer at all.
func TestSweepClaimLeaseRenewalsTreatsUnsupportedAsStoreSkip(t *testing.T) {
	store := newLeaseRenewingMemStore()
	id := seedInProgressBead(t, store, "sess-a")
	store.renewErr[id] = beads.ErrLeaseRenewalUnsupported

	var stderr bytes.Buffer
	res := sweepClaimLeaseRenewals(leaseSweepConfig{
		targets:   []leaseSweepTarget{{label: "rig", store: store}},
		running:   map[string]bool{"sess-a": true},
		stderr:    &stderr,
		logPrefix: "test",
	})

	if res.renewed != 0 {
		t.Errorf("renewed = %d, want 0", res.renewed)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// --- runLeaseRenewalWatchdog ---

// leaseWatchdogRuntime builds the minimal CityRuntime the watchdog reads,
// configured with the default 5m claim-lease TTL. TTL variation itself is
// covered by the leaseRenewalInterval tests.
func leaseWatchdogRuntime(stores map[string]beads.Store) (*CityRuntime, *bytes.Buffer) {
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cfg:                 &config.City{Daemon: config.DaemonConfig{ClaimLeaseTTL: "5m"}},
		standaloneRigStores: stores,
		stderr:              &stderr,
		logPrefix:           "test",
	}
	return cr, &stderr
}

func runningSnapshot(sessionNames ...string) *sessionBeadSnapshot {
	infos := make([]sessionpkg.Info, 0, len(sessionNames))
	for _, name := range sessionNames {
		infos = append(infos, sessionpkg.Info{
			ID:                  "sb-" + name,
			SessionName:         name,
			SessionNameMetadata: name,
			State:               sessionpkg.StateActive,
		})
	}
	return newSessionBeadSnapshotFromInfos(infos)
}

// TestRunLeaseRenewalWatchdogSkipsSessionsWithoutLiveRuntime proves only
// sessions with a live runtime (StateActive) get their held leases renewed. An
// asleep or suspended session is not working its bead; keeping its lease alive
// would hide it from bd reclaim exactly the way the bug hid dead workers.
func TestRunLeaseRenewalWatchdogSkipsSessionsWithoutLiveRuntime(t *testing.T) {
	store := newLeaseRenewingMemStore()
	seedInProgressBead(t, store, "sess-asleep")
	cr, _ := leaseWatchdogRuntime(map[string]beads.Store{"rig": store})
	snapshot := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{
		{ID: "sb-asleep", SessionName: "sess-asleep", State: sessionpkg.StateAsleep},
	})

	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	if got := cr.runLeaseRenewalWatchdog(t0, snapshot); got != 0 {
		t.Fatalf("renewed = %d, want 0 for a session with no live runtime", got)
	}
	if len(store.renews) != 0 {
		t.Errorf("renew calls = %v, want none", store.renews)
	}
}

// TestRunLeaseRenewalWatchdogGatesToDerivedCadence proves the watchdog runs at
// most once per derived interval (TTL/3): the patrol tick fires every ~30s but
// renewals land on the lease cadence, not the tick cadence, so heartbeat
// writes stay a small fraction of the TTL as bd's guidance requires.
func TestRunLeaseRenewalWatchdogGatesToDerivedCadence(t *testing.T) {
	store := newLeaseRenewingMemStore()
	seedInProgressBead(t, store, "sess-a")
	cr, _ := leaseWatchdogRuntime(map[string]beads.Store{"rig": store})
	snapshot := runningSnapshot("sess-a")

	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	if got := cr.runLeaseRenewalWatchdog(t0, snapshot); got != 1 {
		t.Fatalf("first pass renewed = %d, want 1", got)
	}
	if got := cr.runLeaseRenewalWatchdog(t0.Add(30*time.Second), snapshot); got != 0 {
		t.Fatalf("pass inside interval renewed = %d, want 0 (gated)", got)
	}
	if got := cr.runLeaseRenewalWatchdog(t0.Add(101*time.Second), snapshot); got != 1 {
		t.Fatalf("pass after interval renewed = %d, want 1", got)
	}
	if len(store.renews) != 2 {
		t.Errorf("renew calls = %v, want exactly 2", store.renews)
	}
}

// --- acceptance regression (gas-76r) ---

// leaseTrackingStore models bd's actual lease semantics: a claim or heartbeat
// stamps lease_expires_at = now + TTL. The simulation reads the lease the way
// bd reclaim would.
type leaseTrackingStore struct {
	*beads.MemStore
	ttl          time.Duration
	now          func() time.Time
	leaseExpires map[string]time.Time
}

func (s *leaseTrackingStore) RenewLease(id, holder string) error {
	if strings.TrimSpace(holder) == "" {
		return errors.New("empty holder")
	}
	s.leaseExpires[id] = s.now().Add(s.ttl)
	return nil
}

// TestLeaseStaysValidAcrossPatrolTicksForARunningSession is the gas-76r
// regression test: it drives the REAL controller cadence — a 30s patrol tick
// calling the watchdog, which renews on its own derived TTL/3 schedule — for
// four full lease windows, and asserts the lease of a bead held by a running
// session NEVER reads expired at any tick. Before the fix, renewals arrived
// every ~16-22 minutes against a 5-minute window, so a live agent's lease read
// expired ~70% of the time; hand-feeding heartbeats every 30s from the test
// would prove nothing about the machinery (the bead's own words).
func TestLeaseStaysValidAcrossPatrolTicksForARunningSession(t *testing.T) {
	const ttl = 5 * time.Minute
	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	now := t0

	store := &leaseTrackingStore{
		MemStore:     beads.NewMemStore(),
		ttl:          ttl,
		now:          func() time.Time { return now },
		leaseExpires: map[string]time.Time{},
	}
	id := seedInProgressBead(t, store, "sess-live")
	// The claim itself starts the lease, exactly as bd update --claim does.
	store.leaseExpires[id] = t0.Add(ttl)

	cr, stderr := leaseWatchdogRuntime(map[string]beads.Store{"rig": store})
	snapshot := runningSnapshot("sess-live")

	// Four lease windows of 30s patrol ticks (20 simulated minutes).
	for tick := 0; tick <= 40; tick++ {
		now = t0.Add(time.Duration(tick) * 30 * time.Second)
		cr.runLeaseRenewalWatchdog(now, snapshot)
		if !store.leaseExpires[id].After(now) {
			t.Fatalf("lease expired at tick %d (%s): lease_expires_at=%s while session running",
				tick, now.Format(time.RFC3339), store.leaseExpires[id].Format(time.RFC3339))
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// TestLeaseLapsesWhenSessionStopsRunning is the inverse guarantee: renewals
// key on the session actually running, so a dead worker's lease still goes
// stale and bd reclaim can recover its bead. Renewing unconditionally would
// defeat the reclaim safety net from the other side.
func TestLeaseLapsesWhenSessionStopsRunning(t *testing.T) {
	const ttl = 5 * time.Minute
	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	now := t0

	store := &leaseTrackingStore{
		MemStore:     beads.NewMemStore(),
		ttl:          ttl,
		now:          func() time.Time { return now },
		leaseExpires: map[string]time.Time{},
	}
	id := seedInProgressBead(t, store, "sess-dead")
	store.leaseExpires[id] = t0.Add(ttl)

	cr, _ := leaseWatchdogRuntime(map[string]beads.Store{"rig": store})
	empty := runningSnapshot() // session no longer running

	for tick := 0; tick <= 12; tick++ {
		now = t0.Add(time.Duration(tick) * 30 * time.Second)
		cr.runLeaseRenewalWatchdog(now, empty)
	}
	if store.leaseExpires[id].After(now) {
		t.Fatalf("lease still valid at %s for a session that stopped running; reclaim could never fire",
			now.Format(time.RFC3339))
	}
}

// TestLeaseRenewalRenewsAnAliasAssignedHolder proves holder matching speaks the
// confined session-class assignee vocabulary, not the derived runtime
// SessionName. Claims are stamped alias-first, so a pool worker whose bead is
// assigned to its alias ("nux") must still have its lease renewed; matching on
// Info.SessionName — which falls back to sessionNameFor(ID) — misses it and
// silently renews nothing. Same class the SkipsLiveSessionAssignedByAlias
// regressions guard elsewhere in the tree.
func TestLeaseRenewalRenewsAnAliasAssignedHolder(t *testing.T) {
	store := newLeaseRenewingMemStore()
	seedInProgressBead(t, store, "nux")
	cr, _ := leaseWatchdogRuntime(map[string]beads.Store{"rig": store})
	snapshot := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{
		ID:                  "sb-pool-1",
		Alias:               "nux",
		SessionNameMetadata: "claude-mc-xyz",
		State:               sessionpkg.StateActive,
	}})

	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	if got := cr.runLeaseRenewalWatchdog(t0, snapshot); got != 1 {
		t.Fatalf("renewed = %d, want 1 for a bead assigned to the holder's alias", got)
	}
}

// TestNilSessionSnapshotIsRetriedNotTreatedAsSuccess proves a failed
// sessions-class read is not stamped as a complete sweep.
// loadSessionBeadSnapshot returns nil both when the store is unavailable and
// when the query errors; treating that as "no running sessions" would spend a
// full cadence of lease margin on a transient failure instead of taking the
// backoff path this watchdog already has.
func TestNilSessionSnapshotIsRetriedNotTreatedAsSuccess(t *testing.T) {
	store := newLeaseRenewingMemStore()
	seedInProgressBead(t, store, "sess-a")
	cr, _ := leaseWatchdogRuntime(map[string]beads.Store{"rig": store})

	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	if got := cr.runLeaseRenewalWatchdog(t0, nil); got != 0 {
		t.Fatalf("renewed = %d, want 0 from a failed snapshot read", got)
	}
	if !cr.leaseRenewalLastSuccess.IsZero() {
		t.Fatal("leaseRenewalLastSuccess stamped by a failed sessions-class read")
	}
	if cr.leaseRenewalFailures != 1 {
		t.Fatalf("leaseRenewalFailures = %d, want 1", cr.leaseRenewalFailures)
	}

	// The retry rides the failure backoff, not a full cadence: gated inside
	// the retry delay, and eligible immediately after it.
	interval := leaseRenewalInterval(5 * time.Minute)
	retry := leaseRenewalRetryDelay(1, interval)
	if retry <= 0 || retry >= interval {
		t.Fatalf("retry delay = %v, want >0 and < the %v cadence", retry, interval)
	}
	if got := cr.runLeaseRenewalWatchdog(t0.Add(retry-time.Second), runningSnapshot("sess-a")); got != 0 {
		t.Fatalf("renewed = %d inside the retry delay, want 0 (gated)", got)
	}
	if got := cr.runLeaseRenewalWatchdog(t0.Add(retry), runningSnapshot("sess-a")); got != 1 {
		t.Fatalf("renewed = %d after the retry delay, want 1", got)
	}
}
