package main

import (
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
// allocator passes and duty cycle, sweep and boot durations, and the reason
// kinds the trace-only controllers saw. It is safe for concurrent use.
type v2Metrics struct {
	mu               sync.Mutex
	latency, work    sampleWindow // enqueue-to-start (targets 3, 6); reconcile duration
	reconciles       uint64
	failures, panics uint64
	sessionReasons   map[string]uint64
	allocatorReasons map[string]uint64
	allocStart       time.Time   // when the allocator lane started
	allocRecent      []allocPass // passes that ended within v2DutyWindow, oldest first
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
	WorkP50, WorkP99             time.Duration
	SessionReasons               map[string]uint64
	AllocatorReasons             map[string]uint64
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

func newV2Metrics() *v2Metrics {
	return &v2Metrics{sessionReasons: make(map[string]uint64), allocatorReasons: make(map[string]uint64)}
}

// recordReconcile records one reconcile. latency is sampled only when
// sampleLatency is set.
func (m *v2Metrics) recordReconcile(latency time.Duration, sampleLatency bool, ran time.Duration, outcome v2Outcome) {
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
	}
	m.work.add(ran)
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
		WorkP50:           m.work.quantile(0.50),
		WorkP99:           m.work.quantile(0.99),
		SessionReasons:    maps.Clone(m.sessionReasons),
		AllocatorReasons:  maps.Clone(m.allocatorReasons),
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
