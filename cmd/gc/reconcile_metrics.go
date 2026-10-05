package main

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/workqueue"
)

// v2MetricsWindow bounds the latency and duration samples kept for the
// percentiles: the most recent reconciles, not the whole process lifetime.
const v2MetricsWindow = 1024

// v2DutyWindow is how far back the allocator duty cycle looks.
const v2DutyWindow = 5 * time.Minute

// v2Outcome is how one session reconcile ended.
type v2Outcome uint8

const (
	v2Succeeded v2Outcome = iota
	v2Failed
	v2Panicked
)

// v2Metrics records what the v2 runtime did: reconcile latency and duration,
// allocator passes, wakes and duty cycle, sweep and boot durations, resync
// requests, and the reason kinds the trace-only session controller saw. It is
// safe for concurrent use.
type v2Metrics struct {
	mu               sync.Mutex
	latency, work    sampleWindow      // enqueue-to-start (targets 3, 6); reconcile duration
	beadEvent        sampleWindow      // enqueue-to-start of reconciles a live bead event queued (target 3)
	resyncs          map[string]uint64 // resync lane wakes by reason
	reconciles       uint64
	failures, panics uint64
	sessionReasons   map[string]uint64 // reason kinds the trace-only session controller was handed
	allocatorWakes   map[string]uint64 // allocator pass wake reasons by kind
	allocStart       time.Time         // when the allocator lane started
	allocRecent      []allocPass       // passes that ended within v2DutyWindow, oldest first
	allocPasses      uint64
	allocFailures    uint64
	allocLast        time.Duration
	lastSweep        time.Duration
	superseded       uint64
	boot             time.Duration
}

// allocPass is one allocator pass, for the windowed duty cycle.
type allocPass struct {
	end time.Time
	ran time.Duration
}

// v2MetricsSnapshot is a point-in-time copy of v2Metrics.
type v2MetricsSnapshot struct {
	Reconciles, Failures, Panics uint64
	LatencyP50, LatencyP99       time.Duration
	BeadEventP50, BeadEventP99   time.Duration
	WorkP50, WorkP99             time.Duration
	ResyncRequests               map[string]uint64 // resync lane wakes by reason
	SessionReasons               map[string]uint64
	AllocatorWakes               map[string]uint64
	AllocatorPasses              uint64
	AllocatorFailures            uint64
	AllocatorLastPass            time.Duration
	AllocatorDuty                float64       // busy fraction over the last v2DutyWindow (target 4)
	LastSweep                    time.Duration // the last sweep to finish or be superseded
	SupersededSweeps             uint64        // sweeps a later resync replaced before they finished
	Boot                         time.Duration
}

// v2Stats is everything the runtime reports: its own metrics, the queue and
// the router.
type v2Stats struct {
	Metrics v2MetricsSnapshot
	Queue   workqueue.Stats
	Router  routerStats
}

func (rt *v2Runtime) stats() v2Stats {
	return v2Stats{Metrics: rt.metrics.snapshot(time.Now()), Queue: rt.sessions.Stats(), Router: rt.router.stats()}
}

// v2StuckReconcileAfter is how long a session reconcile may stay in flight
// before the reconcile_queue record alerts on stderr: twice the reload
// deadline, so a config reload has already aborted on it.
const v2StuckReconcileAfter = 2 * reloadReconcileDeadline

// What boot still waits on, as the reconcile_queue record reports it.
const (
	v2BootCensus    = "census"    // the boot pass has not read the sessions census
	v2BootCoverage  = "coverage"  // a boot key has not been reconciled once
	v2BootAllocator = "allocator" // the allocator has not completed a pass
	v2BootReady     = "ready"
	v2BootNoStore   = "no-store" // no bead store: boot never runs, and readiness proceeds (MAINT-003)
)

func (rt *v2Runtime) bootState() string {
	if rt.noStore.Load() {
		return v2BootNoStore
	}
	if rt.ready.Load() {
		return v2BootReady
	}
	cov := rt.bootCov.Load()
	if cov == nil {
		return v2BootCensus
	}
	select {
	case <-cov.Done():
	default:
		return v2BootCoverage
	}
	select {
	case <-rt.alloc.primed:
	default:
		return v2BootAllocator
	}
	return v2BootReady
}

// fsGateState is the worker FS gate's state: unarmed before readiness, else
// held or open. holds is the session queue's.
func (rt *v2Runtime) fsGateState(holds []string) string {
	rt.mu.Lock()
	armed := rt.fsArmed
	rt.mu.Unlock()
	switch {
	case !armed:
		return "unarmed"
	case slices.Contains(holds, v2HoldFSPressure):
		return "held"
	}
	return "open"
}

// v2QueueReport is what the reconcile_queue record keeps from one record to
// the next. mu covers the queue read too, so two records never interleave
// their adds.
type v2QueueReport struct {
	mu      sync.Mutex
	adds    map[string]uint64 // the queue's add counts at the last record
	alerted time.Time         // start of the in-flight reconcile last alerted on
}

// inFlight is how long the longest session reconcile in flight has run, or
// zero.
func inFlight(q workqueue.Stats, now time.Time) time.Duration {
	if q.OldestInFlight.IsZero() {
		return 0
	}
	return now.Sub(q.OldestInFlight)
}

// bootStatus is the startup watchdog's line on the v2 runtime: what boot
// waits on, and whether a reconcile holds it up.
func (rt *v2Runtime) bootStatus(now time.Time) string {
	q := rt.sessions.Stats()
	return fmt.Sprintf("boot=%s depth_hot=%d depth_resync=%d processing=%d longest_in_flight=%s",
		rt.bootState(), q.Depth[workqueue.LaneHot], q.Depth[workqueue.LaneResync], q.Processing, inFlight(q, now).Round(time.Millisecond))
}

// queueRecord is the reconcile_queue record of a maintenance tick, or of a
// boot patrol (engdocs/architecture/reconciler-v2.md#observability): the
// queue, the reconciles, the allocator and resync lanes, the router, what boot
// still waits on, and legacyEntries, the city runtime's count of refused
// legacy session entries, which must stay 0. Adds are counted since the last
// record; the rest is cumulative or current. A session reconcile in flight for
// v2StuckReconcileAfter or longer is alerted on stderr, once.
func (rt *v2Runtime) queueRecord(now time.Time, legacyEntries int64) map[string]any {
	m, r := rt.metrics.snapshot(now), rt.router.stats()
	ago := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return now.Sub(t).Milliseconds()
	}
	rep := &rt.report
	rep.mu.Lock()
	q := rt.sessions.Stats()
	adds := make(map[string]uint64, len(q.Adds))
	for kind, n := range q.Adds {
		if d := n - rep.adds[kind]; d > 0 {
			adds[kind] = d
		}
	}
	rep.adds = q.Adds
	running := inFlight(q, now)
	alert := running >= v2StuckReconcileAfter && !q.OldestInFlight.Equal(rep.alerted)
	if alert {
		rep.alerted = q.OldestInFlight
	}
	rep.mu.Unlock()
	if alert {
		fmt.Fprintf(rt.host.stderr, "v2 reconciler: a session reconcile has been running for %s, at least twice the %s reload deadline; config reloads abort until it returns\n", //nolint:errcheck // best-effort stderr
			running.Round(time.Second), reloadReconcileDeadline)
	}
	return map[string]any{
		"boot":                       rt.bootState(),
		"keys":                       q.Keys,
		"depth_hot":                  q.Depth[workqueue.LaneHot],
		"depth_resync":               q.Depth[workqueue.LaneResync],
		"oldest_hot_ms":              ago(q.Oldest[workqueue.LaneHot]),
		"oldest_resync_ms":           ago(q.Oldest[workqueue.LaneResync]),
		"processing":                 q.Processing,
		"longest_in_flight_ms":       running.Milliseconds(),
		"dirty":                      q.Dirty,
		"deferred":                   q.Deferred,
		"timers":                     q.Timers,
		"adds":                       adds,
		"dropped_adds":               q.Dropped,
		"reconciles":                 m.Reconciles,
		"reconcile_failures":         m.Failures,
		"reconcile_panics":           m.Panics,
		"latency_p50_ms":             m.LatencyP50.Milliseconds(),
		"latency_p99_ms":             m.LatencyP99.Milliseconds(),
		"bead_event_latency_p50_ms":  m.BeadEventP50.Milliseconds(),
		"bead_event_latency_p99_ms":  m.BeadEventP99.Milliseconds(),
		"session_reasons":            m.SessionReasons,
		"work_p50_ms":                m.WorkP50.Milliseconds(),
		"work_p99_ms":                m.WorkP99.Milliseconds(),
		"allocator_passes":           m.AllocatorPasses,
		"allocator_failures":         m.AllocatorFailures,
		"allocator_last_pass_ms":     m.AllocatorLastPass.Milliseconds(),
		"allocator_duty":             m.AllocatorDuty,
		"allocator_wakes":            m.AllocatorWakes,
		"allocator_wakes_suppressed": r.WakesSuppressed, // bead events the wake policy kept from the allocator (C9)
		"resync_sweep_ms":            m.LastSweep.Milliseconds(),
		"resync_superseded":          m.SupersededSweeps,
		"resync_requests":            m.ResyncRequests,
		"boot_ms":                    m.Boot.Milliseconds(),
		"router_events":              r.Events,
		"router_replays":             r.Replays,
		"router_undecodable":         r.Undecodable,
		"router_keys_out":            r.KeysOut,
		"router_unresolved":          r.Unresolved,
		"router_panics":              r.Panics, // recovered mapping panics; each dropped its trigger and forced a resync
		"holds":                      q.Holds,
		"fs_gate":                    rt.fsGateState(q.Holds),
		"legacy_session_entries":     legacyEntries,
	}
}

func newV2Metrics() *v2Metrics {
	return &v2Metrics{sessionReasons: make(map[string]uint64), allocatorWakes: make(map[string]uint64), resyncs: make(map[string]uint64)}
}

// recordReconcile records one reconcile. latency is sampled only when
// sampleLatency is set, and into the bead-event window too when beadEvent is.
func (m *v2Metrics) recordReconcile(latency time.Duration, sampleLatency, beadEvent bool, ran time.Duration, outcome v2Outcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconciles++
	switch outcome {
	case v2Failed:
		m.failures++
	case v2Panicked:
		m.panics++
	}
	if sampleLatency {
		m.latency.add(latency)
		if beadEvent {
			m.beadEvent.add(latency)
		}
	}
	m.work.add(ran)
}

// v2BeadEventWoke reports whether a live bead event queued the item: the
// item's first reason, whose add AddedAt dates. A reason merged in later
// would be timed from an older add of another kind, and a replay is not an
// event (F1). This is the bead-event half of target 3's evented inputs.
func v2BeadEventWoke(reasons []workqueue.Reason) bool {
	return len(reasons) > 0 && reasons[0].Kind == routeReasonEvent
}

func (m *v2Metrics) recordResyncRequest(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resyncs[reason]++
}

// recordTrace counts the reason kinds a trace-only controller was handed.
func (m *v2Metrics) recordTrace(into *map[string]uint64, reasons []workqueue.Reason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range reasons {
		(*into)[r.Kind]++
	}
}

func (m *v2Metrics) startAllocator(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allocStart = now
}

// recordAllocatorPass records a pass that ran for ran and ended at end.
func (m *v2Metrics) recordAllocatorPass(end time.Time, ran time.Duration, failed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allocPasses++
	if failed {
		m.allocFailures++
	}
	m.allocLast = ran
	keep := 0
	for keep < len(m.allocRecent) && (end.Sub(m.allocRecent[keep].end) > v2DutyWindow || len(m.allocRecent)-keep >= v2MetricsWindow) {
		keep++
	}
	m.allocRecent = append(m.allocRecent[keep:], allocPass{end: end, ran: ran})
}

func (m *v2Metrics) recordSweep(d time.Duration, superseded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSweep = d
	if superseded {
		m.superseded++
	}
}

func (m *v2Metrics) recordBoot(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.boot = d
}

func (m *v2Metrics) snapshot(now time.Time) v2MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := v2MetricsSnapshot{
		Reconciles:        m.reconciles,
		Failures:          m.failures,
		Panics:            m.panics,
		LatencyP50:        m.latency.quantile(0.50),
		LatencyP99:        m.latency.quantile(0.99),
		BeadEventP50:      m.beadEvent.quantile(0.50),
		BeadEventP99:      m.beadEvent.quantile(0.99),
		WorkP50:           m.work.quantile(0.50),
		WorkP99:           m.work.quantile(0.99),
		ResyncRequests:    maps.Clone(m.resyncs),
		SessionReasons:    maps.Clone(m.sessionReasons),
		AllocatorWakes:    maps.Clone(m.allocatorWakes),
		AllocatorPasses:   m.allocPasses,
		AllocatorFailures: m.allocFailures,
		AllocatorLastPass: m.allocLast,
		LastSweep:         m.lastSweep,
		SupersededSweeps:  m.superseded,
		Boot:              m.boot,
	}
	if m.allocStart.IsZero() {
		return s
	}
	// Offsets from the window's start: the lane's start or v2DutyWindow ago.
	elapsed := min(now.Sub(m.allocStart), v2DutyWindow)
	from := now.Add(-elapsed)
	if elapsed > 0 {
		var busy time.Duration
		for _, p := range m.allocRecent {
			end := p.end.Sub(from)
			busy += max(min(end, elapsed)-max(end-p.ran, 0), 0)
		}
		s.AllocatorDuty = float64(busy) / float64(elapsed)
	}
	return s
}

// sampleWindow keeps the last v2MetricsWindow durations.
type sampleWindow struct {
	samples []time.Duration
	next    int
}

func (w *sampleWindow) add(d time.Duration) {
	if len(w.samples) < v2MetricsWindow {
		w.samples = append(w.samples, d)
		return
	}
	w.samples[w.next] = d
	w.next = (w.next + 1) % v2MetricsWindow
}

// quantile is the nearest-rank q-quantile of the window, or zero if empty.
func (w *sampleWindow) quantile(q float64) time.Duration {
	if len(w.samples) == 0 {
		return 0
	}
	sorted := slices.Clone(w.samples)
	slices.Sort(sorted)
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}
