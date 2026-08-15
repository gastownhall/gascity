package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// --- retry pacing after a failed sweep (review item 1) ---

// TestLeaseRenewalRetryDelayStartsWellInsideTheMargin proves the first retry
// after a failed sweep lands at a small fraction of the normal cadence, not a
// full cadence later. Gating on the ATTEMPT rather than the last success is the
// defect: it spent a full TTL/3 of margin on every failure, so two consecutive
// failures put the third attempt a full TTL from the last success — the expiry
// boundary itself, with nothing left for tick jitter or sweep runtime.
func TestLeaseRenewalRetryDelayStartsWellInsideTheMargin(t *testing.T) {
	interval := leaseRenewalInterval(5 * time.Minute) // 100s
	first := leaseRenewalRetryDelay(1, interval)
	if first <= 0 || first >= interval {
		t.Fatalf("first retry delay = %v, want >0 and < the %v cadence", first, interval)
	}
	if first > interval/4 {
		t.Errorf("first retry delay = %v, want a small fraction of %v", first, interval)
	}
}

// TestLeaseRenewalRetryDelayBacksOffButNeverExceedsTheCadence proves repeated
// failures back off (a persistently broken bd is not retried every tick) while
// the delay stays capped at the normal cadence, so a failing store can never be
// retried LESS often than a healthy one.
func TestLeaseRenewalRetryDelayBacksOffButNeverExceedsTheCadence(t *testing.T) {
	interval := leaseRenewalInterval(5 * time.Minute)
	prev := time.Duration(0)
	for failures := 1; failures <= 12; failures++ {
		got := leaseRenewalRetryDelay(failures, interval)
		if got < prev {
			t.Errorf("retry delay for %d failures = %v, want >= previous %v", failures, got, prev)
		}
		if got > interval {
			t.Fatalf("retry delay for %d failures = %v, want <= cadence %v", failures, got, interval)
		}
		prev = got
	}
}

// TestFailedRenewalIsRetriedWellBeforeTheLeaseExpires is the acceptance test
// for review item 1: with EVERY renewal failing, the watchdog must still make
// several attempts inside one lease TTL. Under the attempt-gated behavior it
// managed only TTL/(TTL/3) = 3 attempts, the last landing exactly on the expiry
// boundary; gating on the last SUCCESS with backoff retries far sooner.
func TestFailedRenewalIsRetriedWellBeforeTheLeaseExpires(t *testing.T) {
	const ttl = 5 * time.Minute
	store := newLeaseRenewingMemStore()
	id := seedInProgressBead(t, store, "sess-a")
	store.renewErr[id] = errors.New("bd unreachable")

	cr, _ := leaseWatchdogRuntime(map[string]beads.Store{"rig": store})
	snapshot := runningSnapshot("sess-a")

	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	// One full TTL of 30s patrol ticks.
	for tick := 0; tick*30 < int(ttl.Seconds()); tick++ {
		cr.runLeaseRenewalWatchdog(t0.Add(time.Duration(tick)*30*time.Second), snapshot)
	}

	// Attempt-gated pacing yields 3 attempts across a TTL. Success-gated pacing
	// with backoff must do materially better than that.
	if len(store.renews) <= 3 {
		t.Errorf("attempts within one %v TTL = %d, want > 3 (retries must not each cost a full cadence)", ttl, len(store.renews))
	}
}

// TestSuccessfulSweepStillHoldsTheDerivedCadence proves the faster retry path
// applies ONLY after a failure: a healthy sweep still renews at TTL/3, so the
// fix does not turn every patrol tick into a bd subprocess storm.
func TestSuccessfulSweepStillHoldsTheDerivedCadence(t *testing.T) {
	store := newLeaseRenewingMemStore()
	seedInProgressBead(t, store, "sess-a")
	cr, _ := leaseWatchdogRuntime(map[string]beads.Store{"rig": store})
	snapshot := runningSnapshot("sess-a")

	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	for tick := 0; tick <= 6; tick++ { // 3 minutes of 30s ticks
		cr.runLeaseRenewalWatchdog(t0.Add(time.Duration(tick)*30*time.Second), snapshot)
	}
	// 180s at a 100s cadence = 2 renewals (t=0, t=120).
	if len(store.renews) != 2 {
		t.Errorf("renew calls = %v, want exactly 2 across 3 minutes at a 100s cadence", store.renews)
	}
}

// --- bounded sweep (review item 2) ---

// clockedLeaseStore charges a fixed cost to every renewal against a test clock,
// standing in for the bd subprocess the real sweep spawns per bead.
type clockedLeaseStore struct {
	*beads.MemStore
	renews   []string
	cost     time.Duration
	advance  func(time.Duration)
	renewErr map[string]error
}

func (s *clockedLeaseStore) RenewLease(id, _ string) error {
	s.renews = append(s.renews, id)
	s.advance(s.cost)
	if err := s.renewErr[id]; err != nil {
		return err
	}
	return nil
}

// TestSweepStopsAtTheWallClockBudget proves one sweep cannot monopolize the
// reconciler tick: with a per-renewal cost that would blow the budget, the
// sweep stops early rather than running one bd subprocess per in-progress bead
// synchronously on the tick's critical path (review item 2).
func TestSweepStopsAtTheWallClockBudget(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store := &clockedLeaseStore{
		MemStore: beads.NewMemStore(),
		cost:     500 * time.Millisecond,
		advance:  func(d time.Duration) { now = now.Add(d) },
		renewErr: map[string]error{},
	}
	running := map[string]bool{}
	for i := 0; i < 40; i++ {
		holder := "sess-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		seedInProgressBead(t, store, holder)
		running[holder] = true
	}

	var stderr bytes.Buffer
	res := sweepClaimLeaseRenewals(leaseSweepConfig{
		targets:   []leaseSweepTarget{{label: "rig", store: store}},
		running:   running,
		budget:    2 * time.Second,
		now:       func() time.Time { return now },
		stderr:    &stderr,
		logPrefix: "test",
	})

	if !res.truncated {
		t.Errorf("res.truncated = false, want true when the budget is exhausted")
	}
	if len(store.renews) >= 40 {
		t.Errorf("renewed %d of 40 beads, want the sweep bounded by its budget", len(store.renews))
	}
	// A silent cap reads as "covered everything"; the operator must see it.
	if !strings.Contains(stderr.String(), "budget") {
		t.Errorf("stderr = %q, want the truncation reported", stderr.String())
	}
}

// TestBudgetTruncatedSweepResumesWhereItStopped proves the bound does not
// starve the beads it skipped: the next sweep resumes from the cursor rather
// than re-renewing the same prefix forever, so every live holder's lease is
// still renewed within a bounded number of ticks.
func TestBudgetTruncatedSweepResumesWhereItStopped(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store := &clockedLeaseStore{
		MemStore: beads.NewMemStore(),
		cost:     400 * time.Millisecond,
		advance:  func(d time.Duration) { now = now.Add(d) },
		renewErr: map[string]error{},
	}
	running := map[string]bool{}
	for i := 0; i < 12; i++ {
		holder := "sess-" + string(rune('a'+i))
		seedInProgressBead(t, store, holder)
		running[holder] = true
	}

	sweep := func(cursor string) leaseSweepResult {
		var stderr bytes.Buffer
		return sweepClaimLeaseRenewals(leaseSweepConfig{
			targets:   []leaseSweepTarget{{label: "rig", store: store}},
			running:   running,
			cursor:    cursor,
			budget:    time.Second,
			now:       func() time.Time { return now },
			stderr:    &stderr,
			logPrefix: "test",
		})
	}

	first := sweep("")
	if !first.truncated {
		t.Fatalf("first sweep was not truncated; the test cannot prove resumption")
	}
	firstRound := append([]string(nil), store.renews...)

	second := sweep(first.cursor)
	secondRound := store.renews[len(firstRound):]
	if len(secondRound) == 0 {
		t.Fatal("second sweep renewed nothing")
	}
	for _, id := range secondRound {
		for _, seen := range firstRound {
			if id == seen {
				t.Fatalf("second sweep re-renewed %s instead of resuming after the cursor", id)
			}
		}
	}
	_ = second
}

// TestSweepWithoutBudgetPressureReportsComplete proves the common case — few
// enough holders to renew well inside the budget — is not reported as
// truncated, so the watchdog does not treat a healthy fleet as perpetually
// behind and retry every tick.
func TestSweepWithoutBudgetPressureReportsComplete(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store := &clockedLeaseStore{
		MemStore: beads.NewMemStore(),
		cost:     time.Millisecond,
		advance:  func(d time.Duration) { now = now.Add(d) },
		renewErr: map[string]error{},
	}
	seedInProgressBead(t, store, "sess-a")
	seedInProgressBead(t, store, "sess-b")

	var stderr bytes.Buffer
	res := sweepClaimLeaseRenewals(leaseSweepConfig{
		targets:   []leaseSweepTarget{{label: "rig", store: store}},
		running:   map[string]bool{"sess-a": true, "sess-b": true},
		budget:    2 * time.Second,
		now:       func() time.Time { return now },
		stderr:    &stderr,
		logPrefix: "test",
	})

	if res.truncated {
		t.Errorf("res.truncated = true, want false when the sweep finishes inside its budget")
	}
	if res.renewed != 2 {
		t.Errorf("res.renewed = %d, want 2", res.renewed)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}
