package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// leaseSpyProvider records the session row's lease holder at each Start and
// Stop: the lease must be held across every provider call that creates or
// destroys a runtime.
type leaseSpyProvider struct {
	*runtime.Fake
	mu      sync.Mutex // a wave's starts run concurrently
	store   beads.Store
	id      string
	starts  []string
	ttls    []string // the record's TTL at each Start
	watched []bool   // whether each Start's ctx ends at its lease's SafeUntil
	stops   []string
}

func (p *leaseSpyProvider) holder() string {
	b, _ := p.store.Get(p.id)
	return b.Metadata[sessionpkg.RuntimeLeaseHolderKey]
}

func (p *leaseSpyProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	b, _ := p.store.Get(p.id)
	p.mu.Lock()
	p.starts = append(p.starts, b.Metadata[sessionpkg.RuntimeLeaseHolderKey])
	p.ttls = append(p.ttls, b.Metadata[sessionpkg.RuntimeLeaseTTLKey])
	_, deadline := ctx.Deadline()
	p.watched = append(p.watched, deadline)
	p.mu.Unlock()
	return p.Fake.Start(ctx, name, cfg)
}

func (p *leaseSpyProvider) Stop(name string) error {
	holder := p.holder()
	p.mu.Lock()
	p.stops = append(p.stops, holder)
	p.mu.Unlock()
	return p.Fake.Stop(name)
}

// leaseStartFixture is one creating session for the legacy start path, in a
// city with a runtime dir.
type leaseStartFixture struct {
	store   beads.Store
	sp      *leaseSpyProvider
	cand    startCandidate
	cfg     *config.City
	desired map[string]TemplateParams
	city    string
	log     synchronizedBuffer // the async starts write it concurrently
}

func newLeaseStartFixture(t *testing.T, store beads.Store) *leaseStartFixture {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name": "worker", "template": "worker", "generation": "1",
			"continuation_epoch": "1", "instance_token": "tok-1", "pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	tp := TemplateParams{Command: "worker", SessionName: "worker", TemplateName: "worker"}
	return &leaseStartFixture{
		store:   store,
		sp:      &leaseSpyProvider{Fake: runtime.NewFake(), store: store, id: bead.ID},
		cand:    startCandidate{info: sessiontest.SeedBead(t, bead), tp: tp},
		cfg:     &config.City{Agents: []config.Agent{{Name: "worker"}}},
		desired: map[string]TemplateParams{"worker": tp},
		city:    t.TempDir(),
	}
}

func (f *leaseStartFixture) run(options ...startExecutionOption) int {
	clk := &clock.Fake{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	return executePlannedStartsTraced(context.Background(), []startCandidate{f.cand}, f.cfg, f.desired, f.sp, f.store,
		"test-city", f.city, clk, events.Discard, time.Minute, &f.log, &f.log, nil, options...)
}

func (f *leaseStartFixture) hold(t *testing.T) *sessionpkg.RuntimeLease {
	t.Helper()
	l, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(f.store), sessionpkg.RuntimeLeaseRequest{
		City: f.city, Name: "worker", ID: f.cand.info.ID, TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("holding the lease: %v", err)
	}
	return l
}

// TestLegacyStartDefersOnAHeldRuntimeLease: a start whose name another holder
// leases (an operator's attach, another host) writes nothing, not even
// PreWake, and the next pass starts under its own lease, released after the
// commit.
func TestLegacyStartDefersOnAHeldRuntimeLease(t *testing.T) {
	f := newLeaseStartFixture(t, beads.NewMemStore())
	held := f.hold(t)
	if got := f.run(); got != 0 {
		t.Fatalf("woken under a held lease = %d, want 0", got)
	}
	if !strings.Contains(f.log.String(), "outcome=deferred_by_runtime_lease") || len(f.sp.starts) != 0 {
		t.Fatalf("starts %q, log %q; want deferred with no Start", f.sp.starts, f.log.String())
	}
	if gen := mustGetBead(t, f.store, f.cand.info.ID).Metadata["generation"]; gen != "1" {
		t.Fatalf("generation under a held lease = %q, want PreWake not written", gen)
	}
	held.Release()
	if got := f.run(); got != 1 {
		t.Fatalf("woken = %d, want 1; log %q", got, f.log.String())
	}
	meta := mustGetBead(t, f.store, f.cand.info.ID).Metadata
	if len(f.sp.starts) != 1 || f.sp.starts[0] == "" || meta["generation"] != "2" {
		t.Fatalf("holders at Start %q, row %v; want one Start under the lease after PreWake", f.sp.starts, meta)
	}
	if !f.sp.watched[0] {
		t.Fatal("the legacy start's provider call did not run on its lease's Watch")
	}
	if f.sp.ttls[0] != "120" {
		t.Fatalf("lease TTL at Start = %q, want 120 (the 1m startup timeout plus the margin)", f.sp.ttls[0])
	}
	if meta[sessionpkg.RuntimeLeaseHolderKey] != "" || meta[sessionpkg.RuntimeLeaseEpochKey] != "2" {
		t.Fatalf("record after the commit = %v, want epoch 2 released", meta)
	}
}

// TestLegacyAsyncStartHoldsTheLeaseThroughItsCommit: an async start's
// goroutine owns the lease until its commit lands.
func TestLegacyAsyncStartHoldsTheLeaseThroughItsCommit(t *testing.T) {
	f := newLeaseStartFixture(t, beads.NewMemStore())
	var tracker asyncStartTracker
	if got := f.run(withAsyncStartExecution(), withAsyncStartLimiter(newAsyncStartLimiter(1)), withAsyncStartTracker(&tracker)); got != 1 {
		t.Fatalf("enqueued = %d, want 1", got)
	}
	if err := tracker.waitLaunchedStarts(context.Background(), hangBudget); err != nil {
		t.Fatal(err)
	}
	meta := mustGetBead(t, f.store, f.cand.info.ID).Metadata
	if len(f.sp.starts) != 1 || f.sp.starts[0] == "" || meta[sessionpkg.RuntimeLeaseHolderKey] != "" || meta["state"] == string(sessionpkg.StateCreating) {
		t.Fatalf("holders at Start %q, row %v; want the commit under the lease, then released", f.sp.starts, meta)
	}
}

// TestPreWakeIsACompareAndSwap (NEW-6): PreWake lands only on a row whose
// lifecycle facts are the ones read, still holding the start's lease.
func TestPreWakeIsACompareAndSwap(t *testing.T) {
	store := beads.NewMemStore()
	f := newLeaseStartFixture(t, store)
	front := sessionFrontDoor(store)
	clk := &clock.Fake{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	if err := store.SetMetadataBatch(f.cand.info.ID, map[string]string{"state": string(sessionpkg.StateSuspended)}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := preWakeCommitUnder(f.cand.info, front, clk, nil); !errors.Is(err, errPreWakeSuperseded) {
		t.Fatalf("PreWake over a moved row = %v, want errPreWakeSuperseded", err)
	}
	fresh, err := front.Get(f.cand.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := f.hold(t)
	stale.Release()
	if _, _, _, err := preWakeCommitUnder(fresh, front, clk, stale); !errors.Is(err, errPreWakeSuperseded) {
		t.Fatalf("PreWake under a released lease = %v, want errPreWakeSuperseded", err)
	}
	if gen := mustGetBead(t, store, f.cand.info.ID).Metadata["generation"]; gen != "1" {
		t.Fatalf("generation = %q, want no refused PreWake written", gen)
	}
	held := f.hold(t)
	defer held.Release()
	if _, _, _, err := preWakeCommitUnder(fresh, front, clk, held); err != nil {
		t.Fatalf("PreWake under the lease: %v", err)
	}
}

// TestReaperTakesTheNameFlockAndTheOwnersRecord (R-a): the closed-row reap
// stops nothing while the name's flock is held, and stops under the open
// owner row's record otherwise.
func TestReaperTakesTheNameFlockAndTheOwnersRecord(t *testing.T) {
	store := beads.NewMemStore()
	city := t.TempDir()
	mk := func(status string) beads.Bead {
		b, err := store.Create(beads.Bead{
			Type: sessionBeadType, Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{"session_name": "named-1", "state": "awake"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if status == "closed" {
			if err := store.Close(b.ID); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	old, owner := mk("closed"), mk("open")
	sp := &leaseSpyProvider{Fake: boundFake(t, old.ID), store: store, id: owner.ID}
	flock, err := sessionpkg.TryRuntimeLease(nil, sessionpkg.RuntimeLeaseRequest{City: city, Name: "named-1"})
	if err != nil {
		t.Fatal(err)
	}
	if n := reapRuntimesBoundToClosedBeads(store, newSessionBeadSnapshot([]beads.Bead{owner}), nil, sp, nil, city, io.Discard); n != 0 || len(sp.stops) != 0 {
		t.Fatalf("reaped %d under a held flock (stops %q), want none", n, sp.stops)
	}
	flock.Release()
	if n := reapRuntimesBoundToClosedBeads(store, newSessionBeadSnapshot([]beads.Bead{owner}), nil, sp, nil, city, io.Discard); n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
	meta := mustGetBead(t, store, owner.ID).Metadata
	if len(sp.stops) != 1 || sp.stops[0] == "" || meta[sessionpkg.RuntimeLeaseHolderKey] != "" || meta[sessionpkg.RuntimeLeaseEpochKey] != "1" {
		t.Fatalf("holders at Stop %q, owner %v; want the stop under the owner's record, released", sp.stops, meta)
	}
}

// takeoverProvider lets another host take the lease over mid-Start, then
// waits for the start's ctx to end, as a slow provider Start would.
type takeoverProvider struct {
	*runtime.Fake
	onStart func()
	cause   error // why the Start's ctx ended
}

func (p *takeoverProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.onStart()
	select {
	case <-ctx.Done():
		p.cause = context.Cause(ctx)
		return ctx.Err()
	case <-time.After(5 * time.Second):
	}
	return p.Fake.Start(ctx, name, cfg)
}

// TestLegacyStartCommitRefusedAfterATakeover: a start whose lease was taken
// over while its provider Start ran commits nothing.
func TestLegacyStartCommitRefusedAfterATakeover(t *testing.T) {
	prev := startLeaseWatchEvery
	startLeaseWatchEvery = 20 * time.Millisecond
	t.Cleanup(func() { startLeaseWatchEvery = prev })
	f := newLeaseStartFixture(t, beads.NewMemStore())
	f.sp = nil
	sp := &takeoverProvider{Fake: runtime.NewFake(), onStart: func() {
		if err := f.store.SetMetadataBatch(f.cand.info.ID, map[string]string{
			sessionpkg.RuntimeLeaseHolderKey: "other/1/n", sessionpkg.RuntimeLeaseEpochKey: "9",
		}); err != nil {
			t.Error(err)
		}
	}}
	clk := &clock.Fake{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	if got := executePlannedStartsTraced(context.Background(), []startCandidate{f.cand}, f.cfg, f.desired, sp, f.store,
		"test-city", f.city, clk, events.Discard, time.Minute, &f.log, &f.log, nil); got != 0 {
		t.Fatalf("woken after a takeover = %d, want 0", got)
	}
	if meta := mustGetBead(t, f.store, f.cand.info.ID).Metadata; meta["started_config_hash"] != "" || meta[sessionpkg.RuntimeLeaseHolderKey] != "other/1/n" {
		t.Fatalf("row after a takeover = %v, want no commit and the taker's record", meta)
	}
	if !errors.Is(sp.cause, sessionpkg.ErrRuntimeLeaseLost) {
		t.Fatalf("the provider Start's ctx ended with %v, want ErrRuntimeLeaseLost from the lease's Watch", sp.cause)
	}
}

// TestPrepareStartPreWakesUnderTheCandidatesLease: the start path's PreWake
// is the CAS under the candidate's lease; one taken over writes nothing.
func TestPrepareStartPreWakesUnderTheCandidatesLease(t *testing.T) {
	f := newLeaseStartFixture(t, beads.NewMemStore())
	f.cand.lease = f.hold(t)
	defer f.cand.lease.Release()
	if err := f.store.SetMetadataBatch(f.cand.info.ID, map[string]string{sessionpkg.RuntimeLeaseHolderKey: "other/1/n", sessionpkg.RuntimeLeaseEpochKey: "9"}); err != nil {
		t.Fatal(err)
	}
	clk := &clock.Fake{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	if _, err := prepareStartCandidateForCity(f.cand, f.city, "test-city", f.cfg, f.sp, f.store, clk, io.Discard, nil, dispatchOptionSources{}); !errors.Is(err, errPreWakeSuperseded) {
		t.Fatalf("prepare under a taken-over lease = %v, want errPreWakeSuperseded", err)
	}
	if gen := mustGetBead(t, f.store, f.cand.info.ID).Metadata["generation"]; gen != "1" {
		t.Fatalf("generation = %q, want no PreWake written", gen)
	}
}

// TestLegacyStartReleasesEachBatchsLeases: a synchronous batch's leases end
// at its commit, before the next batch starts.
func TestLegacyStartReleasesEachBatchsLeases(t *testing.T) {
	store := beads.NewMemStore()
	f := newLeaseStartFixture(t, store)
	first := f.cand.info.ID
	cands := []startCandidate{f.cand}
	for _, name := range []string{"w2", "w3", "w4"} {
		bead, err := store.Create(beads.Bead{
			Title: name, Type: sessionBeadType, Labels: []string{sessionBeadLabel},
			Metadata: creatingMeta(map[string]string{
				"session_name": name, "template": name, "generation": "1",
				"continuation_epoch": "1", "instance_token": "tok-" + name, "pending_create_claim": "true",
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		tp := TemplateParams{Command: name, SessionName: name, TemplateName: name}
		f.desired[name] = tp
		f.cfg.Agents = append(f.cfg.Agents, config.Agent{Name: name})
		cands = append(cands, startCandidate{info: sessiontest.SeedBead(t, bead), tp: tp})
	}
	f.sp.id = first
	clk := &clock.Fake{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	if got := executePlannedStartsTraced(context.Background(), cands, f.cfg, f.desired, f.sp, store,
		"test-city", f.city, clk, events.Discard, time.Minute, &f.log, &f.log, nil); got != 4 {
		t.Fatalf("woken = %d, want 4; log %q", got, f.log.String())
	}
	if len(f.sp.starts) != 4 || f.sp.starts[0] == "" || f.sp.starts[3] != "" {
		t.Fatalf("the first row's holder at each Start = %q, want held in its batch and released by the next", f.sp.starts)
	}
}

// TestLegacyStartReleasesLeasesOnAnEarlyReturn: a pass that returns between
// taking a lease and running its start releases it.
func TestLegacyStartReleasesLeasesOnAnEarlyReturn(t *testing.T) {
	f := newLeaseStartFixture(t, beads.NewMemStore())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := &clock.Fake{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	executePlannedStartsTraced(ctx, []startCandidate{f.cand}, f.cfg, f.desired, f.sp, f.store, "test-city", f.city,
		clk, events.Discard, time.Minute, &f.log, &f.log, nil, withTaskWorkDirResolver(func(startCandidate, *config.City) string {
			cancel()
			return ""
		}))
	if len(f.sp.starts) != 0 {
		t.Fatalf("a canceled pass started %q", f.sp.starts)
	}
	l, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(f.store), sessionpkg.RuntimeLeaseRequest{City: f.city, Name: "worker", ID: f.cand.info.ID, TTL: time.Minute})
	if err != nil {
		t.Fatalf("the canceled pass kept its lease: %v", err)
	}
	l.Release()
}

// takeoverOnFirstLeasedRead hands the row's lease record to another holder at
// the first read that sees one: the start's PreWake then finds it lost.
type takeoverOnFirstLeasedRead struct {
	beads.Store
	done bool
}

func (s *takeoverOnFirstLeasedRead) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err == nil && !s.done && b.Metadata[sessionpkg.RuntimeLeaseHolderKey] != "" {
		s.done = true
		_ = s.SetMetadataBatch(id, map[string]string{sessionpkg.RuntimeLeaseHolderKey: "other/1/n", sessionpkg.RuntimeLeaseEpochKey: "9"})
		return s.Store.Get(id)
	}
	return b, err
}

// TestRefusedPreWakeIsNoRestart: a PreWake refused because the lease was
// taken over keeps the row's in-flight marker, and is not counted as a
// circuit-breaker restart.
func TestRefusedPreWakeIsNoRestart(t *testing.T) {
	store := &takeoverOnFirstLeasedRead{Store: beads.NewMemStore()}
	f := newLeaseStartFixture(t, store)
	if err := store.SetMetadataBatch(f.cand.info.ID, map[string]string{"last_woke_at": "2026-10-09T11:00:00Z", sessionpkg.NamedSessionIdentityMetadata: "lease-cb-worker"}); err != nil {
		t.Fatal(err)
	}
	f.cand.info.ConfiguredNamedIdentity = "lease-cb-worker"
	f.cand.info.LastWokeAt = "2026-10-09T11:00:00Z"
	f.cfg.Daemon.SessionCircuitBreaker = true
	before := len(defaultSessionCircuitBreaker().entrySnapshot("lease-cb-worker").entryRestarts())
	if got := f.run(); got != 0 || len(f.sp.starts) != 0 {
		t.Fatalf("woken = %d, starts %q; want the takeover to refuse the start", got, f.sp.starts)
	}
	if !strings.Contains(f.log.String(), "pre-wake refused") {
		t.Fatalf("log %q, want the refused PreWake", f.log.String())
	}
	meta := mustGetBead(t, store.Store, f.cand.info.ID).Metadata
	if meta["last_woke_at"] != "2026-10-09T11:00:00Z" || meta["generation"] != "1" {
		t.Fatalf("row after a refused PreWake = %v, want its in-flight marker and generation kept", meta)
	}
	if after := len(defaultSessionCircuitBreaker().entrySnapshot("lease-cb-worker").entryRestarts()); after != before {
		t.Fatalf("circuit restarts %d -> %d, want a refused PreWake not counted", before, after)
	}
}

func (s sessionCircuitBreakerEntrySnapshot) entryRestarts() []time.Time {
	if s.entry == nil {
		return nil
	}
	return s.entry.restarts
}

// TestLegacyStartDefersOnAnotherHostsRecord: a start whose name's flock is
// free but whose row record another host holds defers, writing nothing: no
// PreWake, and no circuit-breaker restart.
func TestLegacyStartDefersOnAnotherHostsRecord(t *testing.T) {
	store := beads.NewMemStore()
	f := newLeaseStartFixture(t, store)
	if err := store.SetMetadataBatch(f.cand.info.ID, map[string]string{sessionpkg.NamedSessionIdentityMetadata: "lease-remote-worker"}); err != nil {
		t.Fatal(err)
	}
	f.cand.info.ConfiguredNamedIdentity = "lease-remote-worker"
	f.cfg.Daemon.SessionCircuitBreaker = true
	remote, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(store), sessionpkg.RuntimeLeaseRequest{City: t.TempDir(), Name: "worker", ID: f.cand.info.ID, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Release()
	before := len(defaultSessionCircuitBreaker().entrySnapshot("lease-remote-worker").entryRestarts())
	if got := f.run(); got != 0 || len(f.sp.starts) != 0 || !strings.Contains(f.log.String(), "outcome=deferred_by_runtime_lease") {
		t.Fatalf("woken = %d, starts %q, log %q; want deferred", got, f.sp.starts, f.log.String())
	}
	if gen := mustGetBead(t, store, f.cand.info.ID).Metadata["generation"]; gen != "1" {
		t.Fatalf("generation = %q, want no PreWake", gen)
	}
	if after := len(defaultSessionCircuitBreaker().entrySnapshot("lease-remote-worker").entryRestarts()); after != before {
		t.Fatalf("circuit restarts %d -> %d, want a deferred start not counted", before, after)
	}
}

// oneLostFenceStore loses the first PreWake's revision fence to another
// writer, as a racing non-lifecycle write would.
type oneLostFenceStore struct {
	beads.Store
	lost bool
}

func (s *oneLostFenceStore) ConditionalWritesModeSource() beads.Store { return s.Store }

func (s *oneLostFenceStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if _, prewake := opts.Metadata["instance_token"]; prewake && !s.lost {
		s.lost = true
		return &beads.PreconditionFailedError{ID: id, Expected: rev, Current: rev + 1}
	}
	return s.cw().UpdateIfMatch(id, rev, opts)
}

func (s *oneLostFenceStore) cw() beads.ConditionalWriter {
	w, _ := beads.ConditionalWriterFor(s.Store)
	return w
}

func (s *oneLostFenceStore) CloseIfMatch(id string, rev int64) error {
	return s.cw().CloseIfMatch(id, rev)
}

func (s *oneLostFenceStore) DeleteIfMatch(id string, rev int64) error {
	return s.cw().DeleteIfMatch(id, rev)
}

func (s *oneLostFenceStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	return s.cw().CompareAndSetMetadataKey(id, key, expected, next)
}

// TestPreWakeRetriesALostFence: a PreWake that loses its revision fence to a
// write that left the lifecycle facts alone re-reads and lands, as the start
// commit does.
func TestPreWakeRetriesALostFence(t *testing.T) {
	backing := openRequireSQLite(t, t.TempDir())
	store := &oneLostFenceStore{Store: backing}
	f := newLeaseStartFixture(t, backing)
	front := sessionFrontDoor(store)
	info, err := front.Get(f.cand.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock.Fake{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	if _, _, _, err := preWakeCommitUnder(info, front, clk, nil); err != nil || !store.lost {
		t.Fatalf("PreWake over one lost fence = %v (lost %v), want it to land on the retry", err, store.lost)
	}
	if gen := mustGetBead(t, backing, f.cand.info.ID).Metadata["generation"]; gen != "2" {
		t.Fatalf("generation = %q, want 2", gen)
	}
}

// TestRecoveryCommitTakesTheRuntimeLease: the pending-create recovery is a
// start's commit, so it writes nothing while another holder has the name's
// lease, and heals under its own lease, released, otherwise.
func TestRecoveryCommitTakesTheRuntimeLease(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker", "template": "worker", "pending_create_claim": "true",
			"state": "active", "state_reason": "creation_complete",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	city := t.TempDir()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	tp := TemplateParams{SessionName: "worker", TemplateName: "worker"}
	clk := &clock.Fake{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	held, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(store), sessionpkg.RuntimeLeaseRequest{City: city, Name: "worker", ID: bead.ID, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if ok, batch := recoverRunningPendingCreate(city, sessiontest.SeedBead(t, bead), tp, cfg, store, clk, nil); !ok || batch != nil {
		t.Fatalf("recovery under a held lease = (%v, %v), want deferred", ok, batch)
	}
	if claim := mustGetBead(t, store, bead.ID).Metadata["pending_create_claim"]; claim != "true" {
		t.Fatalf("pending_create_claim under a held lease = %q, want untouched", claim)
	}
	held.Release()
	if ok, _ := recoverRunningPendingCreate(city, sessiontest.SeedBead(t, mustGetBead(t, store, bead.ID)), tp, cfg, store, clk, nil); !ok {
		t.Fatal("recovery with a free lease did not heal")
	}
	meta := mustGetBead(t, store, bead.ID).Metadata
	if meta["pending_create_claim"] != "" || meta[sessionpkg.RuntimeLeaseHolderKey] != "" || meta[sessionpkg.RuntimeLeaseEpochKey] != "2" {
		t.Fatalf("row after the recovery = %v, want healed under epoch 2, released", meta)
	}
}

// TestAsyncStartReleasesItsLeaseBeforeTheFollowUp: the pass an async start's
// follow-up wakes finds the name's lease free.
func TestAsyncStartReleasesItsLeaseBeforeTheFollowUp(t *testing.T) {
	f := newLeaseStartFixture(t, beads.NewMemStore())
	var tracker asyncStartTracker
	var followUpErr error
	followUp := func() {
		l, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(f.store), sessionpkg.RuntimeLeaseRequest{City: f.city, Name: "worker", ID: f.cand.info.ID, TTL: time.Minute})
		if err == nil {
			l.Release()
		}
		followUpErr = err
	}
	if got := f.run(withAsyncStartExecution(), withAsyncStartLimiter(newAsyncStartLimiter(1)), withAsyncStartTracker(&tracker), withAsyncStartFollowUp(followUp)); got != 1 {
		t.Fatalf("enqueued = %d, want 1", got)
	}
	if err := tracker.waitLaunchedStarts(context.Background(), hangBudget); err != nil {
		t.Fatal(err)
	}
	if followUpErr != nil {
		t.Fatalf("lease at the follow-up: %v, want free", followUpErr)
	}
}

// TestReaperFallsBackToTheFlockAlone: a reap whose owner row's record cannot
// be taken for a reason other than busy (here the row closed) still stops
// the runtime under the name's flock, logged.
func TestReaperFallsBackToTheFlockAlone(t *testing.T) {
	store := beads.NewMemStore()
	city := t.TempDir()
	owner, err := store.Create(beads.Bead{
		Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{"session_name": "named-1", "state": "awake"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(owner.ID); err != nil {
		t.Fatal(err)
	}
	sp := boundFake(t, owner.ID)
	var log bytes.Buffer
	stopped, err := stopStillBoundClosedRuntimeLeased(store, city, "named-1", owner.ID, owner.ID, sp, false, &log)
	if !stopped || err != nil || sp.IsRunning("named-1") || !strings.Contains(log.String(), "under its flock alone") {
		t.Fatalf("reap = (%v, %v), running %v, log %q; want the stop under the flock alone", stopped, err, sp.IsRunning("named-1"), log.String())
	}
}

// TestReaperDecidesAgainUnderTheLease: a closed row reopened between the
// sweep's read and the reap's lease keeps its runtime.
func TestReaperDecidesAgainUnderTheLease(t *testing.T) {
	store := beads.NewMemStore()
	old, err := store.Create(beads.Bead{
		Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{"session_name": "named-1", "state": "awake"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := boundFake(t, old.ID)
	if stopped, err := stopStillBoundClosedRuntimeLeased(store, t.TempDir(), "named-1", "", old.ID, sp, false, io.Discard); stopped || err != nil || !sp.IsRunning("named-1") {
		t.Fatalf("reap of a reopened row = (%v, %v), running %v; want it spared", stopped, err, sp.IsRunning("named-1"))
	}
}

// TestLaunchDriftRelaunchTakesTheRuntimeLease: the warm-box relaunch defers,
// relaunching and writing nothing, while another holder has the name's lease,
// and relaunches under its own lease, released, otherwise.
func TestLaunchDriftRelaunchTakesTheRuntimeLease(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	tp := TemplateParams{Command: "claude", SessionName: "worker", TemplateName: "worker"}
	env.desiredState["worker"] = tp
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "claude"}); err != nil {
		t.Fatal(err)
	}
	b := env.createSessionBead("worker", "worker")
	env.markSessionActive(&b)
	agentCfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(b.ID))
	oldCfg := agentCfg
	oldCfg.Command = "stale-" + agentCfg.Command
	env.setSessionMetadata(&b, map[string]string{
		"started_config_hash":    runtime.CoreFingerprint(oldCfg),
		"started_provision_hash": runtime.ProvisionFingerprint(oldCfg),
		"started_launch_hash":    runtime.LaunchFingerprint(oldCfg),
	})
	city := t.TempDir()
	relaunch := func() (bool, map[string]string) {
		return relaunchAgentForLaunchDrift(context.Background(), env.sp, sessionFrontDoor(env.store), env.sessionInfo(b.ID), "worker",
			tp, city, env.cfg, env.store, dispatchOptionSources{}, runtime.CoreFingerprint(oldCfg), runtime.CoreFingerprint(agentCfg),
			runtime.ProvisionFingerprint(oldCfg), runtime.LaunchFingerprint(oldCfg), []string{"Command"}, env.rec, nil, &env.stdout, &env.stderr)
	}
	held, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(env.store), sessionpkg.RuntimeLeaseRequest{City: city, Name: "worker", ID: b.ID, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	before := mustGetBead(t, env.store, b.ID).Metadata["started_config_hash"]
	if ok, batch := relaunch(); !ok || batch != nil || env.sp.CountCalls("Relaunch", "worker") != 0 {
		t.Fatalf("relaunch under a held lease = (%v, %v), relaunches %d; want deferred", ok, batch, env.sp.CountCalls("Relaunch", "worker"))
	}
	if got := mustGetBead(t, env.store, b.ID).Metadata["started_config_hash"]; got != before {
		t.Fatalf("started_config_hash under a held lease = %q, want untouched", got)
	}
	held.Release()
	if ok, _ := relaunch(); !ok || env.sp.CountCalls("Relaunch", "worker") != 1 {
		t.Fatalf("relaunch with a free lease = %v, relaunches %d; stderr %q", ok, env.sp.CountCalls("Relaunch", "worker"), env.stderr.String())
	}
	if meta := mustGetBead(t, env.store, b.ID).Metadata; meta[sessionpkg.RuntimeLeaseHolderKey] != "" || meta[sessionpkg.RuntimeLeaseEpochKey] != "2" {
		t.Fatalf("row after the relaunch = %v, want relaunched under epoch 2, released", meta)
	}
}

// TestLaunchDriftRelaunchFallsBackOnANonBusyLeaseError: a lease failure other
// than busy (here the row closed) is no deferral: the relaunch falls back to
// the full restart, so the drift is not stuck.
func TestLaunchDriftRelaunchFallsBackOnANonBusyLeaseError(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	tp := TemplateParams{Command: "claude", SessionName: "worker", TemplateName: "worker"}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "claude"}); err != nil {
		t.Fatal(err)
	}
	b := env.createSessionBead("worker", "worker")
	info := env.sessionInfo(b.ID)
	if err := env.store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	ok, batch := relaunchAgentForLaunchDrift(context.Background(), env.sp, sessionFrontDoor(env.store), info, "worker",
		tp, t.TempDir(), env.cfg, env.store, dispatchOptionSources{}, "a", "b", "c", "d", []string{"Command"}, env.rec, nil, &env.stdout, &env.stderr)
	if ok || batch != nil || env.sp.CountCalls("Relaunch", "worker") != 0 || !strings.Contains(env.stderr.String(), "falling back to full restart") {
		t.Fatalf("relaunch on a closed row = (%v, %v), relaunches %d, stderr %q; want the full-restart fallback", ok, batch, env.sp.CountCalls("Relaunch", "worker"), env.stderr.String())
	}
}
