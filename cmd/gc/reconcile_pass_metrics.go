package main

import (
	"maps"
	"math"
	"slices"
	"sync"
	"time"
)

// passMetricsWindow bounds the duration and gap samples kept for the
// percentiles: the most recent passes, not the whole process lifetime.
const passMetricsWindow = 1024

// passDutyWindow is how far back the planner's duty cycle looks: Gate S
// reads its p99 over the records.
const passDutyWindow = time.Minute

// passCounts is what one pass reports for the metrics: intents admitted by
// kind, intents deferred by kind and cause, the in-flight depth by kind once
// the pass has submitted, the age of each input it read, the census rows,
// and the signaled stops older than stopOutstandingAge.
type passCounts struct {
	Admitted        map[string]int
	Deferred        map[deferral]int
	InFlight        map[string]int
	InputAges       map[string]time.Duration
	Rows            int
	StopOutstanding int
}

// deferral is why admission held an intent back.
type deferral struct{ Kind, Cause string }

// passMetrics records what the planner did: pass durations, the idle gap
// before each pass, the duty cycle, the wakes by reason and the intents each
// pass admitted and deferred. OBS1 publishes its snapshot. It is safe for
// concurrent use: markDirty counts wakes from any goroutine.
type passMetrics struct {
	mu        sync.Mutex
	started   time.Time   // the first pass's start, the duty window's floor
	durations passSamples // how long each pass ran
	gaps      passSamples // from the end of one pass to the start of the next
	recent    []passSpan  // passes that ended within passDutyWindow, oldest first
	lastEnd   time.Time
	last      time.Duration
	passes    uint64
	panics    uint64
	wakes     map[string]uint64
	admitted  map[string]uint64
	deferred  map[deferral]uint64
	inFlight  map[string]int
	inputAges map[string]time.Duration
	// The pass record's counters (reconcile_observe_v2.go).
	settled map[string]uint64 // by settledKey
	alerts  map[string]uint64 // by alert kind
	series  map[string]uint64 // soakSeries
}

// passSpan is one pass, for the windowed duty cycle.
type passSpan struct {
	end time.Time
	ran time.Duration
}

// passMetricsSnapshot is a point-in-time copy of passMetrics.
type passMetricsSnapshot struct {
	Passes, Panics           uint64
	DurationP50, DurationP99 time.Duration
	GapP50, GapP99           time.Duration
	LastDuration             time.Duration
	Duty                     float64 // busy fraction over the last passDutyWindow
	Wakes                    map[string]uint64
	Admitted                 map[string]uint64
	Deferred                 map[deferral]uint64
	InFlight                 map[string]int           // after the last pass
	InputAges                map[string]time.Duration // as the last pass read them
	LastEnd                  time.Time
	Settled, Alerts, Series  map[string]uint64
}

func newPassMetrics() *passMetrics {
	return &passMetrics{
		wakes: make(map[string]uint64), admitted: make(map[string]uint64), deferred: make(map[deferral]uint64),
		settled: make(map[string]uint64), alerts: make(map[string]uint64), series: make(map[string]uint64),
	}
}

func (m *passMetrics) recordWake(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wakes[reason]++
}

// recordPass records a pass that started at start and ran for ran.
func (m *passMetrics) recordPass(start time.Time, ran time.Duration, panicked bool, c passCounts) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started.IsZero() {
		m.started = start
	} else {
		m.gaps.add(max(start.Sub(m.lastEnd), 0))
	}
	end := start.Add(ran)
	m.passes++
	if panicked {
		m.panics++
	}
	m.durations.add(ran)
	m.last, m.lastEnd = ran, end
	keep := 0
	for keep < len(m.recent) && (end.Sub(m.recent[keep].end) > passDutyWindow || len(m.recent)-keep >= passMetricsWindow) {
		keep++
	}
	m.recent = append(m.recent[keep:], passSpan{end: end, ran: ran})
	for k, n := range c.Admitted {
		m.admitted[k] += uint64(n)
	}
	for k, n := range c.Deferred {
		m.deferred[k] += uint64(n)
	}
	m.inFlight, m.inputAges = maps.Clone(c.InFlight), maps.Clone(c.InputAges)
}

func (m *passMetrics) snapshot(now time.Time) passMetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := passMetricsSnapshot{
		Passes:       m.passes,
		Panics:       m.panics,
		DurationP50:  m.durations.quantile(0.50),
		DurationP99:  m.durations.quantile(0.99),
		GapP50:       m.gaps.quantile(0.50),
		GapP99:       m.gaps.quantile(0.99),
		LastDuration: m.last,
		Wakes:        maps.Clone(m.wakes),
		Admitted:     maps.Clone(m.admitted),
		Deferred:     maps.Clone(m.deferred),
		InFlight:     maps.Clone(m.inFlight),
		InputAges:    maps.Clone(m.inputAges),
		LastEnd:      m.lastEnd,
		Settled:      maps.Clone(m.settled),
		Alerts:       maps.Clone(m.alerts),
		Series:       maps.Clone(m.series),
	}
	if m.started.IsZero() {
		return s
	}
	// Offsets from the window's start: the first pass or passDutyWindow ago.
	elapsed := min(now.Sub(m.started), passDutyWindow)
	from := now.Add(-elapsed)
	if elapsed > 0 {
		var busy time.Duration
		for _, p := range m.recent {
			end := p.end.Sub(from)
			busy += max(min(end, elapsed)-max(end-p.ran, 0), 0)
		}
		s.Duty = float64(busy) / float64(elapsed)
	}
	return s
}

// passSamples keeps the last passMetricsWindow durations.
type passSamples struct {
	samples []time.Duration
	next    int
}

func (w *passSamples) add(d time.Duration) {
	if len(w.samples) < passMetricsWindow {
		w.samples = append(w.samples, d)
		return
	}
	w.samples[w.next] = d
	w.next = (w.next + 1) % passMetricsWindow
}

// quantile is the nearest-rank q-quantile of the window, or zero if empty.
func (w *passSamples) quantile(q float64) time.Duration {
	if len(w.samples) == 0 {
		return 0
	}
	sorted := slices.Clone(w.samples)
	slices.Sort(sorted)
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}
