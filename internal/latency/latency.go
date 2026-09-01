// Package latency answers the recurring "why is this slow" operator
// question over chain/convoy execution: how long ready work sits before a
// pool claims it, how long a defined step sits before it starts, and how
// often a formula's step gets redefined (bounced) before it finally runs.
// Introduced by issue #5852 as the second of four `gc analyze` subcommands
// reading events.jsonl.
//
// The package is a pure-data layer: it parses events.Event slices into a
// grouped report. The CLI (cmd/gc/cmd_analyze_latency.go) handles IO,
// filtering, and presentation — the same split reliability
// (internal/reliability) established for #1254.
//
// # Grounding and scope limitations
//
// All three metrics are derived strictly from fields that exist on
// events.Event and its registered payloads today (internal/events/events.go,
// internal/events/execution_payloads.go, internal/api/event_payloads.go).
// No event carries a "pool" field directly, and no event carries a formula
// name directly — both are reconstructed from bead/root metadata that other
// packages (internal/graphroute, internal/runproj) already treat as the
// source of truth for those concepts:
//
//   - "Pool" (metric 1) is beadmeta.RoutedToMetadataKey ("gc.routed_to"), the
//     same field internal/graphroute.ApplyGraphRouteBinding stamps on a
//     pool-routed step ("Pool-routed step: the pool decides which slot runs
//     it") and the same field BeadDeadAssigneeReopenedPayload's RoutedTo
//     documents as "the RoutedTo pool". It is read from the bead.created/
//     bead.updated snapshot payload, not from a dedicated claim event — Gas
//     City has no event marking a successful claim (bead.claim_rejected and
//     bead.claim_released are the exception paths, not the common case), so
//     "claimed" is inferred as the first bead.updated/bead.created snapshot
//     where Assignee transitions from empty to non-empty for a bead that was
//     previously seen with a non-empty gc.routed_to and an empty Assignee.
//   - "Formula" (metrics 2 and 3) is read from the workflow root bead's
//     metadata (beadmeta.FormulaMetadataKey / beadmeta.FormulaNameMetadataKey),
//     resolved via the execution event's RunID (the workflow root's bead id)
//     — the same two keys, in the same priority order, that
//     internal/runproj/detail_formulaname.go's runFormulaMetadataNameRoot
//     reads for the run-detail view. Only the root carries this metadata
//     (internal/formula/compile.go stamps it once, on the root step), so a
//     step bead itself is never checked for it.
//   - Gate "bounce" (metric 3) is inferred from execution.step_defined,
//     which internal/executionevent/projector.go documents as recording
//     "one physical native execution-step occurrence" — a retried step gets
//     a fresh physical step bead (a new Subject) but keeps the same
//     (RunID, StepID) pair. More than one execution.step_defined sharing a
//     (RunID, StepID) is therefore read as that step having been redefined
//     — bounced — one or more times. This is an inference from the
//     documented step-identity contract, not a dedicated
//     "gate failed and retried" event; no such event exists today.
package latency

import (
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// unknownFormula is the group label used when a run's workflow root never
// surfaced a formula name in the observed event window (root bead.created/
// bead.updated event missing, or missing both metadata keys).
const unknownFormula = "unknown"

// Window restricts the events considered to a time range. Zero-valued
// fields disable the corresponding bound. Kept as a package-local type so
// this package has no dependency on sibling analyze packages and each owns
// its windowing independently.
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

// Filter narrows the report to a specific pool and/or formula. Empty
// fields disable the corresponding filter. Matching is case-insensitive,
// consistent with reliability's filters.
type Filter struct {
	Pool    string
	Formula string
}

func (f Filter) matchesPool(pool string) bool {
	return f.Pool == "" || strings.EqualFold(f.Pool, pool)
}

func (f Filter) matchesFormula(formula string) bool {
	return f.Formula == "" || strings.EqualFold(f.Formula, formula)
}

// DurationStats summarizes a set of millisecond duration samples.
type DurationStats struct {
	Count int   `json:"count"`
	MinMs int64 `json:"min_ms"`
	MaxMs int64 `json:"max_ms"`
	AvgMs int64 `json:"avg_ms"`
	P50Ms int64 `json:"p50_ms"`
	P95Ms int64 `json:"p95_ms"`
}

// computeDurationStats builds a DurationStats from unsorted millisecond
// samples. Percentiles use nearest-rank on the sorted sample set, rounding
// the fractional index to the nearest sample — deterministic and free of
// interpolation, though for very small sample sets (e.g. two samples) P50
// can round up to the higher sample rather than a true median.
func computeDurationStats(samplesMs []int64) DurationStats {
	if len(samplesMs) == 0 {
		return DurationStats{}
	}
	sorted := append([]int64(nil), samplesMs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	return DurationStats{
		Count: len(sorted),
		MinMs: sorted[0],
		MaxMs: sorted[len(sorted)-1],
		AvgMs: sum / int64(len(sorted)),
		P50Ms: nearestRank(sorted, 0.50),
		P95Ms: nearestRank(sorted, 0.95),
	}
}

// nearestRank returns the nearest-rank percentile of sorted (ascending,
// non-empty). percentile is in [0,1].
func nearestRank(sorted []int64, percentile float64) int64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	rank := int(percentile*float64(len(sorted)-1) + 0.5)
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// ClaimWaitGroup reports claim-wait duration stats for one pool.
type ClaimWaitGroup struct {
	Pool  string        `json:"pool"`
	Stats DurationStats `json:"stats"`
}

// GateQueueWaitGroup reports execution.step_defined -> execution.step_started
// gap duration stats for one (formula, step) pair.
type GateQueueWaitGroup struct {
	Formula string        `json:"formula"`
	StepID  string        `json:"step_id"`
	Stats   DurationStats `json:"stats"`
}

// GateBounceGroup reports how often a formula's steps were redefined
// (bounced) before finally running.
type GateBounceGroup struct {
	Formula string `json:"formula"`
	// Definitions is the total count of execution.step_defined occurrences
	// observed for this formula (one per physical step bead, including
	// retries).
	Definitions int `json:"definitions"`
	// Bounces is Definitions minus the count of distinct (RunID, StepID)
	// pairs — i.e. every redefinition beyond a step's first.
	Bounces int `json:"bounces"`
	// BounceRate is Bounces / Definitions, 0 when Definitions is 0.
	BounceRate float64 `json:"bounce_rate"`
}

// Report is the top-level result of an analysis pass.
type Report struct {
	Window Window `json:"-"`
	Filter Filter `json:"-"`

	ClaimWait     []ClaimWaitGroup     `json:"claim_wait"`
	GateQueueWait []GateQueueWaitGroup `json:"gate_queue_wait"`
	GateBounce    []GateBounceGroup    `json:"gate_bounce"`

	// Skipped counts bead.created/bead.updated/bead.closed events (consumed
	// while reconstructing claim-wait pool assignment and resolving formula
	// names) whose payload did not decode to a bead with an id.
	Skipped int `json:"skipped"`
}

// Analyze produces a latency report from the supplied events.
func Analyze(es []events.Event, win Window, flt Filter) Report {
	report := Report{Window: win, Filter: flt}

	beadEvents, skipped := decodeBeadLifecycleEvents(es)
	formulaByRun := buildFormulaIndex(beadEvents)

	report.ClaimWait = analyzeClaimWait(beadEvents, win, flt)
	report.GateQueueWait = analyzeGateQueueWait(es, win, flt, formulaByRun)
	report.GateBounce = analyzeGateBounce(es, win, flt, formulaByRun)
	report.Skipped = skipped
	return report
}

// decodedBeadEvent pairs a bead.created/bead.updated/bead.closed event with
// its decoded bead snapshot, so the payload is parsed once and shared by
// both consumers (claim-wait reconstruction and formula-name resolution)
// instead of decoding — and double-counting decode failures for — the same
// event twice.
type decodedBeadEvent struct {
	event events.Event
	bead  beads.Bead
}

// decodeBeadLifecycleEvents decodes every bead.created/bead.updated/
// bead.closed event's payload. Events whose payload does not decode to a
// bead with an id are dropped and counted in the returned skipped total.
func decodeBeadLifecycleEvents(es []events.Event) ([]decodedBeadEvent, int) {
	decoded := make([]decodedBeadEvent, 0, len(es))
	skipped := 0
	for _, e := range es {
		switch e.Type {
		case events.BeadCreated, events.BeadUpdated, events.BeadClosed:
		default:
			continue
		}
		b, ok := beads.DecodeBeadEventPayload(e.Payload)
		if !ok {
			skipped++
			continue
		}
		decoded = append(decoded, decodedBeadEvent{event: e, bead: b})
	}
	return decoded, skipped
}

// buildFormulaIndex resolves a workflow root bead id -> formula name map
// from decoded bead snapshots, checking beadmeta.FormulaMetadataKey then
// beadmeta.FormulaNameMetadataKey (the same priority runproj's run-detail
// resolution uses). Only beads that ever carry one of those keys are
// indexed; everything else resolves to unknownFormula at lookup time.
func buildFormulaIndex(beadEvents []decodedBeadEvent) map[string]string {
	index := make(map[string]string)
	for _, de := range beadEvents {
		name := strings.TrimSpace(de.bead.Metadata[beadmeta.FormulaMetadataKey])
		if name == "" {
			name = strings.TrimSpace(de.bead.Metadata[beadmeta.FormulaNameMetadataKey])
		}
		if name != "" {
			index[de.bead.ID] = name
		}
	}
	return index
}

func formulaForRun(formulaByRun map[string]string, runID string) string {
	if name, ok := formulaByRun[runID]; ok && name != "" {
		return name
	}
	return unknownFormula
}

// analyzeClaimWait reconstructs, per bead, the interval between a bead
// becoming pool-routed (gc.routed_to set, Assignee empty) and being
// claimed (Assignee becomes non-empty), and buckets the resulting
// durations by pool. See the package doc for why this is the best
// available proxy given the current event vocabulary.
func analyzeClaimWait(beadEvents []decodedBeadEvent, win Window, flt Filter) []ClaimWaitGroup {
	byBead := make(map[string][]decodedBeadEvent)
	for _, de := range beadEvents {
		if de.event.Subject != "" {
			byBead[de.event.Subject] = append(byBead[de.event.Subject], de)
		}
	}

	samplesByPool := make(map[string][]int64)
	for _, evs := range byBead {
		sort.Slice(evs, func(i, j int) bool { return evs[i].event.Seq < evs[j].event.Seq })

		var pendingPool string
		var pendingSince time.Time
		havePending := false

		for _, de := range evs {
			routedTo := strings.TrimSpace(de.bead.Metadata[beadmeta.RoutedToMetadataKey])
			assignee := strings.TrimSpace(de.bead.Assignee)
			ts := de.event.Ts

			switch {
			case !havePending && assignee == "" && routedTo != "":
				pendingPool = routedTo
				pendingSince = ts
				havePending = true
			case havePending && assignee != "":
				if !ts.Before(pendingSince) && win.Contains(ts) && flt.matchesPool(pendingPool) {
					ms := ts.Sub(pendingSince).Milliseconds()
					samplesByPool[pendingPool] = append(samplesByPool[pendingPool], ms)
				}
				havePending = false
			case havePending && assignee == "" && routedTo == "":
				// Reopened/rerouted away from a pool before ever being
				// claimed (e.g. a dead-assignee reopen clearing routing) —
				// abandon the pending wait rather than reporting a bogus
				// duration against a pool the bead no longer targets.
				havePending = false
			case havePending && assignee == "" && routedTo != "" && routedTo != pendingPool:
				// Rerouted to a different pool before ever being claimed —
				// restart the wait clock against the new pool rather than
				// attributing the whole span to the stale one.
				pendingPool = routedTo
				pendingSince = ts
			}
		}
	}

	groups := make([]ClaimWaitGroup, 0, len(samplesByPool))
	for pool, samples := range samplesByPool {
		groups = append(groups, ClaimWaitGroup{Pool: pool, Stats: computeDurationStats(samples)})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Stats.Count != groups[j].Stats.Count {
			return groups[i].Stats.Count > groups[j].Stats.Count
		}
		return groups[i].Pool < groups[j].Pool
	})
	return groups
}

// stepKey identifies one semantic step within one execution run.
type stepKey struct {
	runID  string
	stepID string
}

// analyzeGateQueueWait pairs each execution.step_defined event with the
// execution.step_started event sharing the same Subject (physical step
// bead) — the two lifecycle facts for one physical step attempt share a
// Subject by construction (internal/executionevent.LifecycleEvent stamps
// Subject from the same step bead ProjectCurrent/EmitLifecycle read) — and
// buckets the gap by (formula, step id).
func analyzeGateQueueWait(es []events.Event, win Window, flt Filter, formulaByRun map[string]string) []GateQueueWaitGroup {
	defined := make(map[string]events.Event) // subject -> step_defined event
	started := make(map[string]events.Event) // subject -> step_started event
	for _, e := range es {
		if e.Subject == "" {
			continue
		}
		switch e.Type {
		case events.ExecutionStepDefined:
			defined[e.Subject] = e
		case events.ExecutionStepStarted:
			started[e.Subject] = e
		}
	}

	type groupKey struct{ formula, stepID string }
	samples := make(map[groupKey][]int64)
	for subject, defEvent := range defined {
		startEvent, ok := started[subject]
		if !ok {
			continue
		}
		if startEvent.Ts.Before(defEvent.Ts) {
			continue
		}
		if !win.Contains(startEvent.Ts) {
			continue
		}
		formula := formulaForRun(formulaByRun, defEvent.RunID)
		if !flt.matchesFormula(formula) {
			continue
		}
		key := groupKey{formula: formula, stepID: defEvent.StepID}
		samples[key] = append(samples[key], startEvent.Ts.Sub(defEvent.Ts).Milliseconds())
	}

	groups := make([]GateQueueWaitGroup, 0, len(samples))
	for key, ms := range samples {
		groups = append(groups, GateQueueWaitGroup{Formula: key.formula, StepID: key.stepID, Stats: computeDurationStats(ms)})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Stats.Count != groups[j].Stats.Count {
			return groups[i].Stats.Count > groups[j].Stats.Count
		}
		if groups[i].Formula != groups[j].Formula {
			return groups[i].Formula < groups[j].Formula
		}
		return groups[i].StepID < groups[j].StepID
	})
	return groups
}

// analyzeGateBounce counts, per formula, how many execution.step_defined
// occurrences exist for each (RunID, StepID) pair. A pair with more than
// one occurrence had its step redefined — bounced — after the first
// attempt; see the package doc for why this is read as a gate
// failed-and-retried signal.
func analyzeGateBounce(es []events.Event, win Window, flt Filter, formulaByRun map[string]string) []GateBounceGroup {
	definitionsByStep := make(map[stepKey]int)
	formulaByStep := make(map[stepKey]string)
	for _, e := range es {
		if e.Type != events.ExecutionStepDefined {
			continue
		}
		if !win.Contains(e.Ts) {
			continue
		}
		key := stepKey{runID: e.RunID, stepID: e.StepID}
		definitionsByStep[key]++
		if _, ok := formulaByStep[key]; !ok {
			formulaByStep[key] = formulaForRun(formulaByRun, e.RunID)
		}
	}

	totalsByFormula := make(map[string]struct{ definitions, steps int })
	for key, count := range definitionsByStep {
		formula := formulaByStep[key]
		if !flt.matchesFormula(formula) {
			continue
		}
		t := totalsByFormula[formula]
		t.definitions += count
		t.steps++
		totalsByFormula[formula] = t
	}

	groups := make([]GateBounceGroup, 0, len(totalsByFormula))
	for formula, t := range totalsByFormula {
		bounces := t.definitions - t.steps
		rate := 0.0
		if t.definitions > 0 {
			rate = float64(bounces) / float64(t.definitions)
		}
		groups = append(groups, GateBounceGroup{
			Formula:     formula,
			Definitions: t.definitions,
			Bounces:     bounces,
			BounceRate:  rate,
		})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Bounces != groups[j].Bounces {
			return groups[i].Bounces > groups[j].Bounces
		}
		return groups[i].Formula < groups[j].Formula
	})
	return groups
}
