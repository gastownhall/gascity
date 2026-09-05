// Package stalldetect answers the "is the dispatcher wedged" operator
// question: which beads are sitting at status=in_progress with no event
// newer than a configurable threshold, and which pool are they parked
// on. Introduced by issue #5852 as the fourth of four `gc analyze`
// subcommands reading events.jsonl (the first, bead throughput, shipped
// in #5865/internal/beadthroughput and set the pattern this package
// follows).
//
// The package is a pure-data layer: it parses events.Event slices into
// a stall report. The CLI (cmd/gc/cmd_analyze_stall.go) handles IO,
// filtering, and presentation — the same split reliability
// (internal/reliability) and beadthroughput (internal/beadthroughput)
// established.
package stalldetect

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// unassignedPool is the group label for an in-progress bead with no
// assignee — a bead the store considers claimed/started but with no
// worker of record, which is itself a stall signature worth surfacing
// rather than dropping.
const unassignedPool = "unassigned"

// inProgressStatus is the bead status this report watches for staleness.
// Other statuses (open, closed) are not "in flight" in the sense the
// dispatcher-wedged signature cares about.
const inProgressStatus = "in_progress"

// Window restricts the events considered to a time range. Zero-valued
// fields disable the corresponding bound. Mirrors internal/beadthroughput's
// Window; kept as a separate type so this package has no dependency on
// sibling analyze packages and each owns its windowing independently.
type Window struct {
	Since time.Time
	Until time.Time
}

// Contains reports whether ts is within the window. A zero-valued bound
// disables that side of the check.
func (w Window) Contains(ts time.Time) bool {
	if !w.Since.IsZero() && ts.Before(w.Since) {
		return false
	}
	if !w.Until.IsZero() && ts.After(w.Until) {
		return false
	}
	return true
}

// Filter narrows the report to a specific pool. Empty disables the
// filter. Matching is case-insensitive, consistent with the sibling
// analyze packages' filters.
type Filter struct {
	Pool string
}

func (f Filter) matches(pool string) bool {
	if f.Pool == "" {
		return true
	}
	return strings.EqualFold(f.Pool, pool)
}

// Entry is one in-progress bead's last-event staleness.
type Entry struct {
	BeadID        string    `json:"bead_id"`
	Pool          string    `json:"pool"`
	Assignee      string    `json:"assignee"`
	LastEventType string    `json:"last_event_type"`
	LastEventAt   time.Time `json:"last_event_at"`
	AgeSeconds    float64   `json:"age_seconds"`
	Stalled       bool      `json:"stalled"`
}

// PoolSummary aggregates in-progress/stalled counts for one pool.
type PoolSummary struct {
	Pool             string  `json:"pool"`
	InProgress       int     `json:"in_progress"`
	Stalled          int     `json:"stalled"`
	OldestAgeSeconds float64 `json:"oldest_age_seconds"`
}

// Report is the top-level result of an analysis pass.
type Report struct {
	Window           Window        `json:"-"`
	Filter           Filter        `json:"-"`
	EvaluatedAt      time.Time     `json:"evaluated_at"`
	ThresholdSeconds float64       `json:"threshold_seconds"`
	Entries          []Entry       `json:"entries"`
	Pools            []PoolSummary `json:"pools"`
	TotalInProgress  int           `json:"total_in_progress"`
	TotalStalled     int           `json:"total_stalled"`
	// Skipped counts bead.created/bead.updated/bead.closed events whose
	// payload did not decode to a bead with an id — visible rather than
	// silently absorbed, matching beadthroughput.Report.Skipped.
	Skipped int `json:"skipped"`
}

// beadState tracks the latest known snapshot for one bead id as events
// are folded in order.
type beadState struct {
	status        string
	assignee      string
	snapshotAt    time.Time
	lastEventType string
	lastEventAt   time.Time
}

// Analyze produces a stall report from the supplied events: every bead
// last known to be status=in_progress, its last-event age relative to
// now, and whether that age meets or exceeds threshold. Events outside
// the window are dropped. Only events carrying a non-empty Subject
// contribute (Subject is the bead id for every event type this package
// cares about); bead.created/bead.updated/bead.closed events additionally
// carry a full bead snapshot used to track status and assignee, and a
// snapshot payload that fails to decode counts toward Report.Skipped.
//
// now is passed explicitly (not time.Now()) so the analysis is
// deterministic and testable; the CLI passes the wall-clock time or
// --until.
func Analyze(es []events.Event, win Window, now time.Time, threshold time.Duration, flt Filter) Report {
	states := make(map[string]*beadState)

	report := Report{
		Window:           win,
		Filter:           flt,
		EvaluatedAt:      now,
		ThresholdSeconds: threshold.Seconds(),
		// Non-nil so JSON output always emits "[]" rather than "null" for
		// an empty result — the result schema requires type "array".
		Entries: []Entry{},
		Pools:   []PoolSummary{},
	}

	for _, e := range es {
		if !win.Contains(e.Ts) {
			continue
		}
		subject := strings.TrimSpace(e.Subject)
		if subject == "" {
			continue
		}

		st, ok := states[subject]
		if !ok {
			st = &beadState{}
			states[subject] = st
		}
		if !e.Ts.Before(st.lastEventAt) {
			st.lastEventAt = e.Ts
			st.lastEventType = e.Type
		}

		if e.Type != events.BeadCreated && e.Type != events.BeadUpdated && e.Type != events.BeadClosed {
			continue
		}
		bead, decoded := beads.DecodeBeadEventPayload(e.Payload)
		if !decoded || strings.TrimSpace(bead.ID) == "" {
			report.Skipped++
			continue
		}
		if !e.Ts.Before(st.snapshotAt) {
			st.snapshotAt = e.Ts
			st.status = bead.Status
			st.assignee = bead.Assignee
		}
	}

	for beadID, st := range states {
		if st.status != inProgressStatus {
			continue
		}
		pool := poolForAssignee(st.assignee)
		if !flt.matches(pool) {
			continue
		}
		age := now.Sub(st.lastEventAt)
		stalled := age >= threshold
		report.Entries = append(report.Entries, Entry{
			BeadID:        beadID,
			Pool:          pool,
			Assignee:      st.assignee,
			LastEventType: st.lastEventType,
			LastEventAt:   st.lastEventAt,
			AgeSeconds:    age.Seconds(),
			Stalled:       stalled,
		})
	}

	sort.Slice(report.Entries, func(i, j int) bool {
		if report.Entries[i].AgeSeconds != report.Entries[j].AgeSeconds {
			return report.Entries[i].AgeSeconds > report.Entries[j].AgeSeconds
		}
		return report.Entries[i].BeadID < report.Entries[j].BeadID
	})

	report.Pools = summarizePools(report.Entries)
	report.TotalInProgress = len(report.Entries)
	for _, entry := range report.Entries {
		if entry.Stalled {
			report.TotalStalled++
		}
	}
	return report
}

// summarizePools aggregates entries into per-pool in-progress/stalled
// counts, sorted by descending stall count then ascending pool name for
// stable reading — the pools most likely to be wedged sort first.
func summarizePools(entries []Entry) []PoolSummary {
	byPool := make(map[string]*PoolSummary)
	for _, e := range entries {
		p, ok := byPool[e.Pool]
		if !ok {
			p = &PoolSummary{Pool: e.Pool}
			byPool[e.Pool] = p
		}
		p.InProgress++
		if e.Stalled {
			p.Stalled++
		}
		if e.AgeSeconds > p.OldestAgeSeconds {
			p.OldestAgeSeconds = e.AgeSeconds
		}
	}
	out := make([]PoolSummary, 0, len(byPool))
	for _, p := range byPool {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stalled != out[j].Stalled {
			return out[i].Stalled > out[j].Stalled
		}
		return out[i].Pool < out[j].Pool
	})
	return out
}

// poolForAssignee derives the pool name from a bead's assignee: a pool
// instance identity is "<pool>-<N>" (matchPoolInstanceBare's convention
// in internal/agentutil/resolve.go — "polecat-2" is instance 2 of pool
// "polecat"), so a trailing "-<positive integer>" is stripped. An
// assignee with no such suffix (a single-instance agent) is its own
// pool. An empty assignee groups under unassignedPool: an in-progress
// bead with nobody holding it is itself part of the wedged signature,
// not noise to drop.
func poolForAssignee(assignee string) string {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return unassignedPool
	}
	if i := strings.LastIndexByte(assignee, '-'); i > 0 && i < len(assignee)-1 {
		suffix := assignee[i+1:]
		if n, err := strconv.Atoi(suffix); err == nil && n >= 1 {
			return assignee[:i]
		}
	}
	return assignee
}
