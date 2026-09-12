package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The cases codex round 15 found (evidence 07-codex-r15.md).

// queryCapturingStore records the list queries the sweep makes.
type queryCapturingStore struct {
	beads.Store
	queries []beads.ListQuery
}

func (s *queryCapturingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, q)
	return s.Store.List(q)
}

// TestSweepUnmailedParksMailsARelocatedParkFromItsOwnStore: a bead migrated
// into a relocated class store keeps a retained, unparked copy in the work
// store. The sweep finds the ACTIVE park in the class store and mails it
// from there — the park's store rides with it, never re-resolved to the
// retained copy — and lists every store on both tiers, so an ephemeral
// row in a relocated store is found too.
func TestSweepUnmailedParksMailsARelocatedParkFromItsOwnStore(t *testing.T) {
	const id = "gg-relocated"
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	now := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	retained := beads.NewMemStoreFrom(1, []beads.Bead{{ID: id, Title: "retained copy", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}}}, nil)
	class := &queryCapturingStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{{ID: id, Title: "active copy", Type: "task", Status: "in_progress", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "worker", beadmeta.ParkedAtMetadataKey: now.Format(time.RFC3339), beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkIDMetadataKey: "cafe",
	}}}, nil)}
	var mails []parkedWorkNotice
	policy := &workStartFailurePolicy{
		workStore:         retained,
		extraStores:       []beads.Store{class},
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		notify:            func(n parkedWorkNotice) error { mails = append(mails, n); return nil },
	}
	policy.sweepUnmailedParks(now)
	policy.awaitParkMailRetries()
	if len(mails) != 1 || mails[0].BeadID != id || mails[0].Title != "active copy" {
		t.Fatalf("the active park is mailed once, from its own store: %+v", mails)
	}
	active, _ := class.Get(id)
	if readWorkStartFailureState(active.Metadata).ParkMailedAt.IsZero() {
		t.Fatalf("the class store's row is the one stamped: %v", active.Metadata)
	}
	copyRow, _ := retained.Get(id)
	if copyRow.Metadata[beadmeta.ParkMailedAtMetadataKey] != "" || copyRow.Metadata[beadmeta.ParkedAtMetadataKey] != "" {
		t.Fatalf("the retained copy is untouched: %v", copyRow.Metadata)
	}
	if len(class.queries) == 0 || class.queries[0].TierMode != beads.FederatedReadTier || !class.queries[0].AllowScan {
		t.Fatalf("the sweep lists every store on both tiers: %+v", class.queries)
	}
	// Again: nothing owed, no second mail.
	policy.sweepUnmailedParks(now.Add(parkMailRetryEvery))
	policy.awaitParkMailRetries()
	if len(mails) != 1 {
		t.Fatalf("a stamped park is not re-sent: %d", len(mails))
	}
}

// TestMailParkRereadsTheLiveRow: the pre-send re-read under the
// single-flight slot is live — a cache that still serves "parked,
// unmailed" after another process lifted the park must not produce a
// PARKED notice for a bead the operator already unparked.
func TestMailParkRereadsTheLiveRow(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	now := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	backing := beads.NewMemStore()
	caching := beads.NewCachingStoreForTest(backing, nil)
	work, err := caching.Create(beads.Bead{Title: "parked", Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "worker", beadmeta.ParkedAtMetadataKey: now.Format(time.RFC3339), beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkIDMetadataKey: "cafe",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := caching.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The operator unparks through another process: the backing moves, the
	// cache does not.
	if err := backing.SetMetadataBatch(work.ID, map[string]string{beadmeta.ParkedAtMetadataKey: "", beadmeta.ParkReasonMetadataKey: "", beadmeta.ParkFailuresMetadataKey: "", beadmeta.ParkIDMetadataKey: ""}); err != nil {
		t.Fatal(err)
	}
	wrapped := &beadPolicyStore{Store: caching, cfg: cfg}
	cached, _ := wrapped.Get(work.ID)
	if !readWorkStartFailureState(cached.Metadata).Parked() {
		t.Fatal("fixture: the cache still serves the park")
	}
	var mails []parkedWorkNotice
	policy := &workStartFailurePolicy{
		workStore:         wrapped,
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		notify:            func(n parkedWorkNotice) error { mails = append(mails, n); return nil },
	}
	policy.retryUnmailedParks([]beads.Bead{cached}, []string{"city"})
	policy.awaitParkMailRetries()
	policy.mailPark(wrapped, work.ID, "worker", true)
	if len(mails) != 0 {
		t.Fatalf("a park lifted behind the cache is not announced: %+v", mails)
	}
}

// TestDeferredStartDoesNotSpendACircuitBreakerAttempt: a named holder whose
// work is deferred at start time makes no attempt — the deferral gate runs
// before the circuit breaker records a restart — so a holder whose work is
// parked (or unreadable) cannot trip its breaker with zero provider calls.
// Unparked, the same holder starts and the attempt is recorded.
func TestDeferredStartDoesNotSpendACircuitBreakerAttempt(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Daemon: config.DaemonConfig{
			SessionCircuitBreaker:            true,
			SessionCircuitBreakerMaxRestarts: intPtrCircuit(1),
			SessionCircuitBreakerWindow:      "30m",
		},
		Agents: []config.Agent{circuitTestAgent("template-a")},
		NamedSessions: []config.NamedSession{{
			Name:     "session-a",
			Template: "template-a",
			Mode:     "always",
		}},
	}
	cb := breakerAt(30*time.Minute, 1)
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()
	work, err := env.store.Create(beads.Bead{Title: "parked work", Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "template-a", beadmeta.ParkedAtMetadataKey: env.clk.Now().UTC().Format(time.RFC3339), beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkIDMetadataKey: "cafe", beadmeta.ParkMailedAtMetadataKey: env.clk.Now().UTC().Format(time.RFC3339),
	}})
	if err != nil {
		t.Fatal(err)
	}
	policy := &workStartFailurePolicy{workStore: env.store, limitFor: func(string) int { return 5 }, stderr: &env.stderr}
	env.startOptions = append(env.startOptions, withWorkStartFailurePolicy(policy))
	env.addDesired("session-a", "template-a", false)
	b := createCircuitTestNamedSession(t, env, "asleep")
	if err := env.store.SetMetadataBatch(b.ID, map[string]string{beadmeta.TriggerBeadIDMetadataKey: work.ID, beadmeta.TriggerBeadStoreRefMetadataKey: "city"}); err != nil {
		t.Fatal(err)
	}
	restarts := func() (int, string) {
		total, ids := 0, ""
		for _, snap := range cb.Snapshot(env.clk.Now().UTC()) {
			total += snap.RestartCount
			ids += snap.Identity + " "
		}
		return total, ids
	}
	for i := 0; i < 3; i++ {
		b, _ = env.store.Get(b.ID)
		if woken := env.reconcile([]beads.Bead{b}); woken != 0 {
			t.Fatalf("woken = %d, want 0 while the work is parked", woken)
		}
		env.clk.Time = env.clk.Time.Add(time.Second)
	}
	if env.sp.IsRunning("session-a") {
		t.Fatalf("the holder's work is parked: never started\nstderr:\n%s", env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "outcome=work_deferred") {
		t.Fatalf("the deferral is said:\n%s", env.stderr.String())
	}
	if got, ids := restarts(); got != 0 {
		t.Fatalf("a deferred start spends no breaker attempt, restart count = %d (%s)\nstderr:\n%s", got, ids, env.stderr.String())
	}
	for _, snap := range cb.Snapshot(env.clk.Now().UTC()) {
		if cb.IsOpen(snap.Identity, env.clk.Now().UTC()) {
			t.Fatalf("the breaker must stay closed with zero attempts: %s", snap.Identity)
		}
	}
	// Control: unparked, the start is made and the attempt recorded.
	row, _ := env.store.Get(work.ID)
	if err := env.store.SetMetadataBatch(work.ID, workStartFailureClearPatch(row.Metadata)); err != nil {
		t.Fatal(err)
	}
	b, _ = env.store.Get(b.ID)
	if woken := env.reconcile([]beads.Bead{b}); woken != 1 {
		t.Fatalf("control: unparked, the holder starts (woken = %d)\nstderr:\n%s", woken, env.stderr.String())
	}
	if got, ids := restarts(); got != 1 {
		t.Fatalf("control: a real attempt is recorded, restart count = %d (%s)", got, ids)
	}
}
