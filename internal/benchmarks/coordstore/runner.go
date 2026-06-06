package coordstore

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Runner drives a workload against a StoreAdapter and collects latency metrics.
type Runner struct {
	adapter StoreAdapter
	wl      WorkloadConfig
	seed    SeedResult
	rng     *rand.Rand

	// rssReader overrides the default readRSSBytes function for testing.
	// When nil, readRSSBytes is used.
	rssReader func() (uint64, bool)

	seedMu    sync.RWMutex
	resultsMu sync.Mutex
	results   map[string]*OperationResult // op name → result

	purgeMu           sync.Mutex
	lastTerminalPurge time.Time
}

// NewRunner creates a Runner for the given adapter and workload.
// seed must be the result of a prior Seeder.Seed call.
func NewRunner(adapter StoreAdapter, wl WorkloadConfig, seed SeedResult) *Runner {
	return &Runner{
		adapter: adapter,
		wl:      wl,
		seed:    seed,
		rng:     rand.New(rand.NewPCG(42, 0xbeefdead)),
		results: make(map[string]*OperationResult),
	}
}

// Run drives the workload for wl.Duration with wl.Concurrency goroutines.
// Progress is written to w.
func (r *Runner) Run(ctx context.Context, w io.Writer) (Scorecard, error) {
	wl := r.wl
	if len(r.seed.MainOpenIDs)+len(r.seed.WispOpenIDs) == 0 {
		return Scorecard{}, fmt.Errorf("runner: empty seed — call Seeder.Seed first")
	}

	// Build the per-second operation schedule as a weighted list.
	schedule := r.buildSchedule()
	if len(schedule) == 0 {
		return Scorecard{}, fmt.Errorf("runner: workload has no operations (all rates are zero)")
	}
	if err := r.primeTerminalRetention(ctx); err != nil {
		return Scorecard{}, err
	}

	var (
		totalOps    atomic.Int64
		totalErrors atomic.Int64
	)

	deadline := time.Now().Add(wl.Duration)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	r.startTerminalPurgeClock(time.Now())

	// Start the memory sampler before the workload so its peak captures the
	// full run, including the hot working set under load.
	mem := newMemSampler(200 * time.Millisecond)
	mem.readRSS = r.rssReader
	if wl.MaxRSSBytes > 0 || wl.MaxRSSGrowthBytesPerSec > 0 {
		mem.maxRSSBytes = wl.MaxRSSBytes
		mem.maxRSSGrowthBytesPerSec = wl.MaxRSSGrowthBytesPerSec
		mem.abortCh = make(chan string, 1)
	}
	mem.start()

	var (
		leakFinding string
		wg          sync.WaitGroup
	)
	for range wl.Concurrency {
		seedA := r.rng.Uint64()
		seedB := r.rng.Uint64()
		wg.Add(1)
		go func(seedA, seedB uint64) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seedA, seedB))
			for {
				if ctx.Err() != nil {
					return
				}
				op := schedule[rng.IntN(len(schedule))]
				start := time.Now()
				err := r.execOp(ctx, op, rng)
				elapsed := time.Since(start)

				totalOps.Add(1)
				if err != nil && ctx.Err() == nil {
					totalErrors.Add(1)
				}
				r.record(op, elapsed, err)

				// Yield to let other goroutines run; realistic agents do work
				// between requests. A tiny sleep also prevents CPU-spin benchmarks
				// from overwhelming the store with unrealistic concurrency.
				if ctx.Err() == nil {
					sleepNs := int64(time.Millisecond) + rng.Int64N(int64(4*time.Millisecond))
					time.Sleep(time.Duration(sleepNs))
				}
			}
		}(seedA, seedB)
	}

	// Progress ticker; also drains the memory guard abort channel.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	start := time.Now()
progressLoop:
	for {
		select {
		case <-ctx.Done():
			break progressLoop
		case finding := <-mem.abortCh:
			leakFinding = finding
			fmt.Fprintf(w, "  [LEAK DETECTED] %s — aborting run\n", finding) //nolint:errcheck
			cancel()
			break progressLoop
		case t := <-ticker.C:
			fmt.Fprintf(w, "  [%s elapsed] ops=%d errors=%d\n", //nolint:errcheck
				FormatDuration(t.Sub(start)), totalOps.Load(), totalErrors.Load())
		}
	}

	wg.Wait()
	dur := time.Since(start)

	memReport := mem.stop()

	// Build throughput map.
	r.resultsMu.Lock()
	defer r.resultsMu.Unlock()

	throughput := make(map[string]float64, len(r.results))
	for op, res := range r.results {
		if dur > 0 {
			throughput[op] = float64(res.Samples) / dur.Seconds()
		}
	}

	// Map EphemeralFilterScan to the mail-poll throughput target.
	if tput, ok := throughput["EphemeralFilterScan"]; ok {
		throughput["MailPoll"] = tput
		r.results["MailPoll"] = r.results["EphemeralFilterScan"]
	}

	sc := Score(
		"", wl.Name, dur,
		int(totalOps.Load()), int(totalErrors.Load()),
		r.results, throughput, memReport,
	)
	if leakFinding != "" {
		sc.LeakAborted = true
		sc.LeakFinding = leakFinding
	}

	fmt.Fprintf(w, "  workload %q done: %d ops in %s, %d errors\n", //nolint:errcheck
		wl.Name, totalOps.Load(), FormatDuration(dur), totalErrors.Load())

	return sc, nil
}

// HistogramSnapshot returns a deep copy of the operation histograms collected
// so far.
func (r *Runner) HistogramSnapshot() map[string]*OperationResult {
	r.resultsMu.Lock()
	defer r.resultsMu.Unlock()

	out := make(map[string]*OperationResult, len(r.results))
	for name, res := range r.results {
		if res == nil {
			continue
		}
		cp := *res
		cp.H = res.H.clone()
		out[name] = &cp
	}
	return out
}

// SeedSnapshot returns a read-only copy of the runner's live seed population.
// Slices are cloned so callers (tests) can inspect post-run population sizes
// without racing the workload goroutines. Exposed for steady-state population
// assertions (design ga-sftyt NFR-3); the mutable r.seed is never exported.
func (r *Runner) SeedSnapshot() SeedResult {
	r.seedMu.RLock()
	defer r.seedMu.RUnlock()
	return SeedResult{
		MainOpenIDs:   append([]string(nil), r.seed.MainOpenIDs...),
		MainClosedIDs: append([]string(nil), r.seed.MainClosedIDs...),
		WispOpenIDs:   append([]string(nil), r.seed.WispOpenIDs...),
		DepEdges:      append([]Dep(nil), r.seed.DepEdges...),
	}
}

// opTag identifies which operation a goroutine will execute.
type opTag int

const (
	opMailPoll opTag = iota
	opPointRead
	opFilterScan
	opCreate
	opUpdate
	opSetMetadataBatch
	opBatchGet
	opReady
	opDepOp
	opRecentScan
	opPurgeTerminal
	opClose
	opDeleteWisp
	opPurgeExpired
)

// buildSchedule creates a weighted list of operations proportional to their
// rates. Each goroutine picks uniformly from this list, producing the
// desired traffic mix.
func (r *Runner) buildSchedule() []opTag {
	wl := r.wl
	type entry struct {
		op    opTag
		rate  float64
		valid bool
	}
	entries := []entry{
		{opMailPoll, wl.MailPollRate, len(r.seed.WispOpenIDs) > 0},
		{opPointRead, wl.PointReadRate, len(r.seed.MainOpenIDs) > 0},
		{opFilterScan, wl.FilterScanRate, len(r.seed.MainOpenIDs) > 0},
		{opCreate, wl.CreateRate, true},
		{opUpdate, wl.UpdateRate, len(r.seed.MainOpenIDs) > 0},
		{opSetMetadataBatch, wl.SetMetadataRate, len(r.seed.MainOpenIDs) > 0},
		{opBatchGet, wl.BatchGetRate, len(r.seed.MainOpenIDs) >= 16},
		{opReady, wl.ReadyRate, len(r.seed.MainOpenIDs) > 0},
		{opDepOp, wl.DepOpRate, len(r.seed.MainOpenIDs) >= 2},
		{opRecentScan, wl.RecentScanRate, true},
		{opPurgeTerminal, wl.PurgeTerminalRate, wl.PurgeTerminalOlderThan > 0},
		{opClose, wl.CloseRate, wl.CloseRate > 0 && len(r.seed.MainOpenIDs) > 100},
		{opDeleteWisp, wl.WispDeleteRate, wl.WispDeleteRate > 0 && len(r.seed.WispOpenIDs) > 100},
		{opPurgeExpired, wl.PurgeExpiredRate, wl.PurgeExpiredRate > 0},
	}

	var schedule []opTag
	for _, e := range entries {
		if !e.valid || e.rate <= 0 {
			continue
		}
		// Scale to integer weight: 1 unit = 0.1/s; min 1.
		weight := int(e.rate*10 + 0.5)
		if weight < 1 {
			weight = 1
		}
		for range weight {
			schedule = append(schedule, e.op)
		}
	}
	return schedule
}

// execOp executes a single operation and returns any error.
func (r *Runner) execOp(ctx context.Context, op opTag, rng *rand.Rand) error {
	seed := &r.seed
	a := r.adapter

	switch op {
	case opMailPoll:
		// FR-8: mail-poll hot path — filter scan of ephemeral tier by type+assignee.
		assignee := MailAssignees[rng.IntN(len(MailAssignees))]
		_, err := a.FilterScan(ctx, Query{
			Type:     "message",
			Status:   "open",
			Assignee: assignee,
			Tier:     TierEphemeral,
		})
		if err != nil {
			return r.recordOpErr("EphemeralFilterScan", err)
		}
		return nil

	case opPointRead:
		// FR-3: point read by ID.
		r.seedMu.RLock()
		if len(seed.MainOpenIDs) == 0 {
			r.seedMu.RUnlock()
			return nil
		}
		id := seed.MainOpenIDs[rng.IntN(len(seed.MainOpenIDs))]
		r.seedMu.RUnlock()
		_, err := a.Get(ctx, id)
		return r.recordOpErr("Get", err)

	case opFilterScan:
		// FR-2: main-tier filter scan by assignee.
		assignee := MailAssignees[rng.IntN(len(MailAssignees))]
		_, err := a.FilterScan(ctx, Query{
			Assignee: assignee,
			Tier:     TierMain,
		})
		return r.recordOpErr("FilterScan", err)

	case opCreate:
		// FR-1: create — alternates between main and wisp tier.
		ephemeral := rng.Float32() < 0.5
		assignee := MailAssignees[rng.IntN(len(MailAssignees))]
		rType := "task"
		var expiresAt time.Time
		if ephemeral {
			rType = "message"
			if rng.Float32() < 0.37 {
				rType = "order-tracking"
				expiresAt = time.Now().Add(24 * time.Hour)
			}
		}
		created, err := a.Create(ctx, Record{
			Title:     fmt.Sprintf("bench-%d", rng.Int64()),
			Status:    "open",
			Type:      rType,
			Assignee:  assignee,
			Ephemeral: ephemeral,
			ExpiresAt: expiresAt,
		})
		if err != nil {
			return r.recordOpErr("Create", err)
		}
		// Track newly created IDs for follow-up operations.
		r.seedMu.Lock()
		if ephemeral {
			seed.WispOpenIDs = append(seed.WispOpenIDs, created.ID)
		} else {
			seed.MainOpenIDs = append(seed.MainOpenIDs, created.ID)
		}
		r.seedMu.Unlock()
		return nil

	case opUpdate:
		// FR-1: update status field.
		r.seedMu.RLock()
		if len(seed.MainOpenIDs) == 0 {
			r.seedMu.RUnlock()
			return nil
		}
		id := seed.MainOpenIDs[rng.IntN(len(seed.MainOpenIDs))]
		r.seedMu.RUnlock()
		err := a.Update(ctx, id, Update{Status: "in_progress"})
		if IsNotFound(err) {
			return nil // raced with a delete; not an error
		}
		return r.recordOpErr("Update", err)

	case opSetMetadataBatch:
		// FR-5: intra-record multi-key atomic write (session state transition).
		r.seedMu.RLock()
		if len(seed.MainOpenIDs) == 0 {
			r.seedMu.RUnlock()
			return nil
		}
		id := seed.MainOpenIDs[rng.IntN(len(seed.MainOpenIDs))]
		r.seedMu.RUnlock()
		err := a.SetMetadataBatch(ctx, id, map[string]string{
			"gc.session_state":  "running",
			"gc.session_pid":    fmt.Sprintf("%d", rng.Int64()%100000),
			"gc.session_pane":   fmt.Sprintf("pane-%d", rng.IntN(20)),
			"gc.last_heartbeat": time.Now().UTC().Format(time.RFC3339),
		})
		if IsNotFound(err) {
			return nil
		}
		return r.recordOpErr("SetMetadataBatch", err)

	case opBatchGet:
		// FR-4: batch fetch 16 records.
		r.seedMu.RLock()
		openIDs := append([]string(nil), seed.MainOpenIDs...)
		r.seedMu.RUnlock()
		if len(openIDs) < 16 {
			return nil
		}
		perm := rng.Perm(len(openIDs))
		batchSize := 16
		if len(perm) < batchSize {
			batchSize = len(perm)
		}
		ids := make([]string, batchSize)
		for i := range batchSize {
			ids[i] = openIDs[perm[i]]
		}
		_, err := a.BatchGet(ctx, ids)
		return r.recordOpErr("BatchGet", err)

	case opReady:
		// FR-9: ready semantics query.
		assignee := MailAssignees[rng.IntN(len(MailAssignees))]
		_, err := a.Ready(ctx, ReadyQuery{Assignee: assignee, Limit: 10})
		return r.recordOpErr("Ready", err)

	case opDepOp:
		// FR-10: add a dep edge, then remove it.
		r.seedMu.RLock()
		if len(seed.MainOpenIDs) < 2 {
			r.seedMu.RUnlock()
			return nil
		}
		i := rng.IntN(len(seed.MainOpenIDs))
		j := rng.IntN(len(seed.MainOpenIDs))
		if i == j {
			j = (j + 1) % len(seed.MainOpenIDs)
		}
		fromID := seed.MainOpenIDs[i]
		toID := seed.MainOpenIDs[j]
		r.seedMu.RUnlock()
		if err := a.DepAdd(ctx, fromID, toID, "blocks"); err != nil {
			return r.recordOpErr("DepAdd", err)
		}
		return r.recordOpErr("DepRemove", a.DepRemove(ctx, fromID, toID))

	case opRecentScan:
		// FR-18: recent records by created_at DESC.
		_, err := a.RecentScan(ctx, 50)
		return r.recordOpErr("RecentScan", err)

	case opPurgeTerminal:
		if !r.takeTerminalPurgeSlot(time.Now()) {
			return nil
		}
		_, err := a.PurgeTerminal(ctx, r.wl.PurgeTerminalOlderThan)
		return r.recordOpErr("PurgeTerminal", err)

	case opClose:
		// Lifecycle (design ga-sftyt): close an open main-tier task (Update ->
		// closed) and pop it from the open pool. Closed IDs are tracked (capped at
		// 50k so the tracker itself stays bounded) for future retention ops; the
		// record stays resident, modeling long-lived closed tasks. A floor guard
		// keeps the open pool from draining below 100. The ID is removed under the
		// lock so no other goroutine closes it; the adapter is called unlocked.
		r.seedMu.Lock()
		if len(seed.MainOpenIDs) <= 100 {
			r.seedMu.Unlock()
			return nil
		}
		idx := rng.IntN(len(seed.MainOpenIDs))
		id := seed.MainOpenIDs[idx]
		last := len(seed.MainOpenIDs) - 1
		seed.MainOpenIDs[idx] = seed.MainOpenIDs[last]
		seed.MainOpenIDs = seed.MainOpenIDs[:last]
		r.seedMu.Unlock()

		if err := a.Update(ctx, id, Update{Status: "closed"}); err != nil {
			if IsNotFound(err) {
				return nil // raced with a concurrent close
			}
			return r.recordOpErr("Close", err)
		}
		r.seedMu.Lock()
		if len(seed.MainClosedIDs) < 50000 {
			seed.MainClosedIDs = append(seed.MainClosedIDs, id)
		}
		r.seedMu.Unlock()
		return nil

	case opDeleteWisp:
		// Lifecycle (design ga-sftyt): delete an open ephemeral record (mail
		// archival / order cancellation) and pop it from the open pool. Wisp
		// deletes ≈ wisp creates so the ephemeral population plateaus. Floor guard
		// keeps the pool from draining below 100.
		r.seedMu.Lock()
		if len(seed.WispOpenIDs) <= 100 {
			r.seedMu.Unlock()
			return nil
		}
		idx := rng.IntN(len(seed.WispOpenIDs))
		id := seed.WispOpenIDs[idx]
		last := len(seed.WispOpenIDs) - 1
		seed.WispOpenIDs[idx] = seed.WispOpenIDs[last]
		seed.WispOpenIDs = seed.WispOpenIDs[:last]
		r.seedMu.Unlock()

		if err := a.Delete(ctx, id); err != nil {
			if IsNotFound(err) {
				return nil // raced with PurgeExpired or another goroutine
			}
			return r.recordOpErr("DeleteWisp", err)
		}
		return nil

	case opPurgeExpired:
		// Lifecycle (design ga-sftyt): TTL sweep of expired order-tracking wisps.
		// Removes records by their already-past ExpiresAt; it must NOT touch the
		// seed trackers, which hold live IDs only.
		_, err := a.PurgeExpired(ctx)
		return r.recordOpErr("PurgeExpired", err)
	}
	return nil
}

func (r *Runner) takeTerminalPurgeSlot(now time.Time) bool {
	if r.wl.PurgeTerminalRate <= 0 || r.wl.PurgeTerminalOlderThan <= 0 {
		return false
	}
	interval := time.Duration(float64(time.Second) / r.wl.PurgeTerminalRate)
	r.purgeMu.Lock()
	defer r.purgeMu.Unlock()
	if r.lastTerminalPurge.IsZero() || now.Sub(r.lastTerminalPurge) >= interval {
		r.lastTerminalPurge = now
		return true
	}
	return false
}

func (r *Runner) startTerminalPurgeClock(now time.Time) {
	if r.wl.PurgeTerminalRate <= 0 || r.wl.PurgeTerminalOlderThan <= 0 {
		return
	}
	r.purgeMu.Lock()
	defer r.purgeMu.Unlock()
	if r.lastTerminalPurge.IsZero() {
		r.lastTerminalPurge = now
	}
}

func (r *Runner) primeTerminalRetention(ctx context.Context) error {
	if r.wl.PurgeTerminalRate <= 0 || r.wl.PurgeTerminalOlderThan <= 0 {
		return nil
	}
	for {
		purged, err := r.adapter.PurgeTerminal(ctx, r.wl.PurgeTerminalOlderThan)
		if err != nil {
			return r.recordOpErr("PurgeTerminal", err)
		}
		if purged < TerminalPurgeBatchSize {
			return nil
		}
	}
}

// record adds a latency sample to the named operation's histogram.
func (r *Runner) record(op opTag, d time.Duration, err error) {
	name := opName(op)
	r.resultsMu.Lock()
	defer r.resultsMu.Unlock()
	res, ok := r.results[name]
	if !ok {
		res = &OperationResult{Op: name, H: &Histogram{}}
		r.results[name] = res
	}
	res.Samples++
	res.H.Add(d)
	if err != nil {
		res.Errors++
	}
}

// recordOpErr is a helper for operations where we check errors post-call.
func (r *Runner) recordOpErr(_ string, err error) error {
	if IsNotFound(err) {
		return nil // treat not-found as benign during workload
	}
	return err
}

func opName(op opTag) string {
	switch op {
	case opMailPoll:
		return "EphemeralFilterScan"
	case opPointRead:
		return "Get"
	case opFilterScan:
		return "FilterScan"
	case opCreate:
		return "Create"
	case opUpdate:
		return "Update"
	case opSetMetadataBatch:
		return "SetMetadataBatch"
	case opBatchGet:
		return "BatchGet"
	case opReady:
		return "Ready"
	case opDepOp:
		return "DepAdd"
	case opRecentScan:
		return "RecentScan"
	case opPurgeTerminal:
		return "PurgeTerminal"
	case opClose:
		return "Close"
	case opDeleteWisp:
		return "DeleteWisp"
	case opPurgeExpired:
		return "PurgeExpired"
	}
	return "unknown"
}

// memGuardWarmUp is the period after a run starts before the growth-rate
// watchdog begins checking. This absorbs the initial allocation surge that
// happens when the store opens and the first batch of ops runs.
const memGuardWarmUp = 30 * time.Second

// memSampler periodically samples process memory during a workload run and
// tracks baseline, peak, and steady HeapInuse/RSS plus total bytes allocated.
// When MaxRSSBytes or MaxRSSGrowthBytesPerSec are set, a breach sends a
// finding string on abortCh (buffered, capacity 1).
type memSampler struct {
	interval time.Duration
	stopCh   chan struct{}
	doneCh   chan struct{}

	// readRSS is the RSS reader function. Defaults to readRSSBytes when nil.
	readRSS func() (uint64, bool)

	// memory guard settings
	maxRSSBytes             uint64
	maxRSSGrowthBytesPerSec uint64
	abortCh                 chan string // non-nil when any guard is active

	// timing state for growth-rate guard
	startedAt time.Time
	warmUpAt  time.Time
	warmUpRSS uint64

	startTotalAlloc uint64
	report          MemReport
}

func newMemSampler(interval time.Duration) *memSampler {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	return &memSampler{
		interval: interval,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// rss returns the current RSS using the injected reader or the default.
func (m *memSampler) rss() (uint64, bool) {
	if m.readRSS != nil {
		return m.readRSS()
	}
	return readRSSBytes()
}

// start records the baseline allocation counter and launches the sampling
// goroutine. The first sample is taken immediately so a fast workload still
// produces a non-zero peak.
func (m *memSampler) start() {
	m.startedAt = time.Now()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	m.startTotalAlloc = ms.TotalAlloc
	m.report.HeapInuseBaseline = ms.HeapInuse
	m.report.HeapInusePeak = ms.HeapInuse
	if rss, ok := m.rss(); ok {
		m.report.RSSBaseline = rss
		m.report.RSSPeak = rss
	}
	m.report.Sampled = true

	go func() {
		defer close(m.doneCh)
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.sampleOnce()
			case <-m.stopCh:
				return
			}
		}
	}()
}

// sampleOnce updates the running peaks from one observation and checks
// memory guard conditions, sending a finding to abortCh on breach.
func (m *memSampler) sampleOnce() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if ms.HeapInuse > m.report.HeapInusePeak {
		m.report.HeapInusePeak = ms.HeapInuse
	}
	rss, rssOK := m.rss()
	if rssOK && rss > m.report.RSSPeak {
		m.report.RSSPeak = rss
	}
	m.report.Sampled = true

	if m.abortCh == nil || !rssOK {
		return
	}

	now := time.Now()

	// Absolute ceiling check.
	if m.maxRSSBytes > 0 && rss > m.maxRSSBytes {
		finding := fmt.Sprintf("RSS %s exceeds ceiling %s at T+%s (baseline %s)",
			FormatBytes(rss), FormatBytes(m.maxRSSBytes),
			FormatDuration(now.Sub(m.startedAt)),
			FormatBytes(m.report.RSSBaseline))
		select {
		case m.abortCh <- finding:
		default:
		}
		return
	}

	// Growth-rate check: only active after the warm-up period.
	if m.maxRSSGrowthBytesPerSec > 0 {
		// Record the warm-up checkpoint on first sample past the warm-up window.
		if m.warmUpAt.IsZero() && !m.startedAt.IsZero() && now.Sub(m.startedAt) >= memGuardWarmUp {
			m.warmUpAt = now
			m.warmUpRSS = rss
		}
		if !m.warmUpAt.IsZero() {
			elapsed := now.Sub(m.warmUpAt).Seconds()
			if elapsed > 0 && rss > m.warmUpRSS {
				growthRate := float64(rss-m.warmUpRSS) / elapsed
				if growthRate > float64(m.maxRSSGrowthBytesPerSec) {
					finding := fmt.Sprintf(
						"RSS growth rate %.1f B/s exceeds limit %s/s at T+%s (warm-up RSS %s, current RSS %s)",
						growthRate, FormatBytes(m.maxRSSGrowthBytesPerSec),
						FormatDuration(now.Sub(m.startedAt)),
						FormatBytes(m.warmUpRSS), FormatBytes(rss))
					select {
					case m.abortCh <- finding:
					default:
					}
				}
			}
		}
	}
}

// stop halts sampling and waits for the sampler goroutine to exit before
// taking the final sample, so m.report is only ever touched by one goroutine
// at a time. It then computes the alloc delta and returns the report.
func (m *memSampler) stop() MemReport {
	close(m.stopCh)
	<-m.doneCh

	// Safe to mutate m.report here: the sampler goroutine has exited.
	m.sampleOnce()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	m.report.HeapInuseSteady = ms.HeapInuse
	if ms.HeapInuse > m.report.HeapInusePeak {
		m.report.HeapInusePeak = ms.HeapInuse
	}
	if rss, ok := m.rss(); ok {
		m.report.RSSSteady = rss
		if rss > m.report.RSSPeak {
			m.report.RSSPeak = rss
		}
	}
	if ms.TotalAlloc >= m.startTotalAlloc {
		m.report.AllocDelta = ms.TotalAlloc - m.startTotalAlloc
	}
	m.report.HeapInuseDelta = heapInuseDelta(m.report.HeapInuseBaseline, m.report.HeapInusePeak)
	return m.report
}

func heapInuseDelta(baseline, peak uint64) uint64 {
	if peak >= baseline {
		return peak - baseline
	}
	return 0
}

// readRSSBytes reads the resident set size from /proc/self/status (VmRSS).
// Returns false on platforms without procfs or on read errors.
func readRSSBytes() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		// Expected form: "VmRSS:   123456 kB"
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// CorrectnessChecker validates that a StoreAdapter satisfies the 18 FRs
// through targeted functional tests. Returns a list of failures; nil means all checks passed.
func CorrectnessChecker(ctx context.Context, a StoreAdapter) []string {
	var failures []string
	check := func(name string, err error) {
		if err != nil {
			failures = append(failures, fmt.Sprintf("[%s] %v", name, err))
		}
	}

	// FR-1: CRUD
	created, err := a.Create(ctx, Record{Title: "correctness-test", Type: "task", Status: "open"})
	check("FR-1/Create", err)
	if err != nil {
		return failures
	}

	got, err := a.Get(ctx, created.ID)
	check("FR-1/Get", err)
	if err == nil && got.Title != "correctness-test" {
		failures = append(failures, "FR-1/Get: title mismatch")
	}

	check("FR-1/Update", a.Update(ctx, created.ID, Update{Status: "in_progress"}))

	got2, err := a.Get(ctx, created.ID)
	check("FR-6/ReadAfterWrite", err)
	if err == nil && got2.Status != "in_progress" {
		failures = append(failures, "FR-6/ReadAfterWrite: status not updated")
	}

	// FR-2: Filter scan
	results, err := a.FilterScan(ctx, Query{Assignee: created.Assignee, Tier: TierMain})
	check("FR-2/FilterScan", err)
	_ = results

	// FR-4: Batch fetch
	batch, err := a.BatchGet(ctx, []string{created.ID})
	check("FR-4/BatchGet", err)
	if err == nil && len(batch) != 1 {
		failures = append(failures, fmt.Sprintf("FR-4/BatchGet: expected 1 record, got %d", len(batch)))
	}

	// FR-5: SetMetadataBatch
	check("FR-5/SetMetadataBatch", a.SetMetadataBatch(ctx, created.ID, map[string]string{
		"k1": "v1",
		"k2": "v2",
	}))
	got3, err := a.Get(ctx, created.ID)
	check("FR-5/VerifyMetadata", err)
	if err == nil {
		if got3.Metadata["k1"] != "v1" || got3.Metadata["k2"] != "v2" {
			failures = append(failures, "FR-5/SetMetadataBatch: metadata not stored")
		}
	}

	// FR-7 + FR-8: Ephemeral tier
	wisp, err := a.Create(ctx, Record{
		Title:     "wisp-test",
		Type:      "message",
		Status:    "open",
		Assignee:  "test-agent",
		Ephemeral: true,
	})
	check("FR-7/CreateEphemeral", err)
	if err == nil {
		wispResults, err := a.FilterScan(ctx, Query{
			Type:     "message",
			Status:   "open",
			Assignee: "test-agent",
			Tier:     TierEphemeral,
		})
		check("FR-8/EphemeralFilterScan", err)
		if err == nil {
			found := false
			for _, w := range wispResults {
				if w.ID == wisp.ID {
					found = true
					break
				}
			}
			if !found {
				failures = append(failures, "FR-8/EphemeralFilterScan: wisp not found in ephemeral tier scan")
			}
		}

		// Verify wisp not visible on main-tier scan.
		mainResults, err := a.FilterScan(ctx, Query{Type: "message", Status: "open", Assignee: "test-agent", Tier: TierMain})
		check("FR-7/TierIsolation-main", err)
		if err == nil {
			for _, m := range mainResults {
				if m.ID == wisp.ID {
					failures = append(failures, "FR-7/TierIsolation: ephemeral record visible on main-tier scan")
					break
				}
			}
		}
	}

	// FR-10: Dependency graph
	dep1, err := a.Create(ctx, Record{Title: "dep-from", Type: "task", Status: "open"})
	check("FR-10/DepFromCreate", err)
	dep2, err := a.Create(ctx, Record{Title: "dep-to", Type: "task", Status: "open"})
	check("FR-10/DepToCreate", err)
	if err == nil {
		check("FR-10/DepAdd", a.DepAdd(ctx, dep1.ID, dep2.ID, "blocks"))
		deps, err := a.DepList(ctx, dep1.ID, "down")
		check("FR-10/DepList", err)
		if err == nil && len(deps) == 0 {
			failures = append(failures, "FR-10/DepList: no deps returned after DepAdd")
		}
		check("FR-10/DepRemove", a.DepRemove(ctx, dep1.ID, dep2.ID))
		depsAfter, err := a.DepList(ctx, dep1.ID, "down")
		check("FR-10/DepListAfterRemove", err)
		if err == nil && len(depsAfter) != 0 {
			failures = append(failures, "FR-10/DepList: dep still present after DepRemove")
		}
	}

	// FR-17: FK cascade on delete
	if err == nil {
		// Add dep, then delete the record — dep must disappear.
		if err2 := a.DepAdd(ctx, dep1.ID, dep2.ID, "blocks"); err2 == nil {
			if err3 := a.Delete(ctx, dep1.ID); err3 == nil {
				cascaded, err4 := a.DepList(ctx, dep1.ID, "down")
				check("FR-17/CascadeCheck", err4)
				if err4 == nil && len(cascaded) != 0 {
					failures = append(failures, "FR-17/Cascade: dep edges not removed on delete")
				}
			}
		}
	}

	// FR-12: TTL expiry
	past := time.Now().Add(-time.Second)
	expiring, err := a.Create(ctx, Record{
		Title:     "ttl-test",
		Type:      "order-tracking",
		Status:    "open",
		Ephemeral: true,
		ExpiresAt: past,
	})
	check("FR-12/CreateExpiring", err)
	if err == nil {
		purged, err := a.PurgeExpired(ctx)
		check("FR-12/PurgeExpired", err)
		if err == nil && purged == 0 {
			failures = append(failures, "FR-12/PurgeExpired: expected ≥1 purged, got 0")
		}
		// Verify the expired record is gone.
		_, getErr := a.Get(ctx, expiring.ID)
		if getErr == nil {
			failures = append(failures, "FR-12/PurgeExpired: expired record still accessible after purge")
		}
	}

	// Terminal retention: old terminal main-tier records are purged without
	// deleting recent terminal records or old active records.
	old := time.Now().Add(-2 * time.Hour)
	recentRef := time.Now().Add(-30 * time.Minute)
	oldTerminal, err := a.Create(ctx, Record{Title: "terminal-old", Type: "task", Status: "closed", CreatedAt: old, UpdatedAt: old})
	check("TerminalRetention/CreateOldTerminal", err)
	recentTerminal, err := a.Create(ctx, Record{Title: "terminal-recent", Type: "task", Status: "closed", CreatedAt: old, UpdatedAt: recentRef})
	check("TerminalRetention/CreateRecentTerminal", err)
	oldActive, err := a.Create(ctx, Record{Title: "terminal-active", Type: "task", Status: "open", CreatedAt: old, UpdatedAt: old})
	check("TerminalRetention/CreateOldActive", err)
	if err == nil {
		purged, err := a.PurgeTerminal(ctx, time.Hour)
		check("TerminalRetention/PurgeTerminal", err)
		if err == nil && purged == 0 {
			failures = append(failures, "TerminalRetention/PurgeTerminal: expected ≥1 purged, got 0")
		}
		_, getErr := a.Get(ctx, oldTerminal.ID)
		if getErr == nil {
			failures = append(failures, "TerminalRetention/PurgeTerminal: old terminal record still accessible after purge")
		}
		if _, getErr := a.Get(ctx, recentTerminal.ID); getErr != nil {
			failures = append(failures, "TerminalRetention/PurgeTerminal: recent terminal record was purged")
		}
		if _, getErr := a.Get(ctx, oldActive.ID); getErr != nil {
			failures = append(failures, "TerminalRetention/PurgeTerminal: old active record was purged")
		}
	}

	// FR-18: Range scan by recency
	recent, err := a.RecentScan(ctx, 10)
	check("FR-18/RecentScan", err)
	if err == nil && len(recent) == 0 {
		failures = append(failures, "FR-18/RecentScan: no results")
	}
	if len(recent) >= 2 {
		if recent[0].CreatedAt.Before(recent[1].CreatedAt) {
			failures = append(failures, "FR-18/RecentScan: not ordered by created_at DESC")
		}
	}

	// FR-15: PrimeScan
	count, err := a.PrimeScan(ctx)
	check("FR-15/PrimeScan", err)
	if err == nil && count == 0 {
		failures = append(failures, "FR-15/PrimeScan: returned 0 records")
	}

	return failures
}
