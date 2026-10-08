package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The v2 pass record and alerts (OBS1; CONTRACT v5 R6, P2, P5, P6, D6; Gate
// S). The planner emits C2a's pass metrics as the reconcile.pass record when
// its content changes and at every patrol, never once a pass. Field names
// are wire names (mdash). Alerts go to stderr and to the event log as
// reconciler.alert, and the record counts them.

// stopOutstandingAge is when a signaled stop counts as outstanding (v5 D6).
const stopOutstandingAge = 30 * time.Minute

// Alert kinds: reconciler.alert's "alert" and the record's "alerts" keys.
const (
	alertNamedDuplicate = "named-duplicate"        // a named identity with more than one open row (B3)
	alertAmbiguousBound = "ambiguous-create-bound" // P5's hard bound cleared an ambiguous create
	alertBootRefused    = "boot-refused"           // C11, or C0.7 (C4b)
	alertBootGateClosed = "boot-gate-closed"       // P2's gate closed for longer than 2 × patrol
	alertPassPanic      = "pass-panic"             // P6
	alertEffectDeadline = "effect-deadline"        // an effect settled at its deadline
	alertUnknownState   = "unknown-state"          // a state legacy does not know was written (I-legacy broke)
)

// passObserver is the planner's memory for alerting once and emitting on
// change.
type passObserver struct {
	gateClosedSince time.Time
	gateAlerted     bool
	duplicates      map[string]bool   // the last pass's named-duplicate alerts
	unknown         map[rowKey]string // rows alerted for an unknown state, with it
	lastKey         string
	lastEmit        time.Time
}

// alert writes an operator alert to stderr, records it as a
// reconciler.alert event on subject, and counts it.
func (p *planner) alert(kind, subject, msg string) {
	p.metrics.count(&p.metrics.alerts, kind)
	fmt.Fprintf(p.stderr, "v2 planner: alert %s: %s\n", kind, msg) //nolint:errcheck // best-effort stderr
	if p.rec == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"alert": kind})
	p.record(events.Event{Type: events.ReconcilerAlert, Ts: p.clock.Now().UTC(), Actor: "gc", Subject: subject, Message: msg, Payload: payload})
}

// alertCleared alerts on each ambiguous create the hard bound cleared (P5),
// not one the census showed. C4b calls it with clearVisible's records.
func (p *planner) alertCleared(recs []clearRecord) {
	for _, c := range recs {
		if c.HardBound {
			e := c.Entry
			p.alert(alertAmbiguousBound, e.Identity, fmt.Sprintf("ambiguous create %s on leg %q (token %s) never reached the census; cleared at the %s bound", e.Identity, e.Leg, e.Token, inflightHardBound))
		}
	}
}

// observeSettlement counts one drained settlement, and alerts when it
// settled at its deadline. A late landing's lone event is not counted.
func (p *planner) observeSettlement(s settlement) {
	if s.Kind == "" {
		return
	}
	p.metrics.count(&p.metrics.settled, settledKey(s))
	if phase := idleRespawnPhases[s.Kind]; phase != "" && s.Outcome == settledLanded && s.Reason == idleRespawnDrainReason {
		p.metrics.count(&p.metrics.series, phase)
	}
	if s.Outcome == settledFailed && strings.TrimPrefix(s.Cause, causeFinalizePrefix) == causeDeadline {
		p.alert(alertEffectDeadline, s.Key.ID+s.Token, fmt.Sprintf("%s effect for %s settled at its deadline", s.Kind, s.Key.ID+s.Token))
	}
}

// settledKey is a settlement's "settled" key: kind/outcome[/cause].
func settledKey(s settlement) string {
	k := s.Kind + "/" + settleOutcomeNames[s.Outcome]
	if s.Cause != "" {
		k += "/" + s.Cause
	}
	return k
}

var settleOutcomeNames = map[settleOutcome]string{
	settledLanded: "landed", settledFailed: "failed", settledRefused: "refused", settledAmbiguous: "ambiguous", settledNoop: "noop",
}

// idleRespawnPhases are the idle-respawn series (C6d, Gate S) a landed
// settlement counts in by kind, when it carries the legacy drain reason
// C6d's begin, void and stop intents carry.
var idleRespawnPhases = map[string]string{
	intentDrainBegin: "idle_respawn_begun", intentDrainBeginFresh: "idle_respawn_begun",
	intentDrainVoid: "idle_respawn_voided", intentStop: "idle_respawn_completed",
}

// soakSeries are passMetrics.series' counters, always in the record.
var soakSeries = []string{"idle_respawn_begun", "idle_respawn_voided", "idle_respawn_completed", "stop_escalations"}

// observeWorld fills c's gauges from w, and alerts once on each named
// duplicate the allocation reports and each row with a state legacy does
// not know, for as long as it lasts.
func (p *planner) observeWorld(w *World, duplicates []string, c *passCounts) {
	rows := w.Census.Canonical()
	c.Rows, c.InputAges = len(rows), w.InputAges
	unknown := make(map[rowKey]string)
	for _, row := range rows {
		if stopOutstanding(row.Info, w.Now) {
			c.StopOutstanding++
		}
		if !row.UnknownState {
			continue
		}
		state := row.Info.MetadataState
		unknown[row.Key] = state
		if prev, seen := p.obs.unknown[row.Key]; !seen || prev != state {
			p.alert(alertUnknownState, row.Key.ID, fmt.Sprintf("session %s on leg %q has state %q, which legacy does not know", row.Key.ID, row.Key.Leg, state))
		}
	}
	p.obs.unknown = unknown
	dups := make(map[string]bool, len(duplicates))
	for _, d := range duplicates {
		dups[d] = true
		if !p.obs.duplicates[d] {
			p.alert(alertNamedDuplicate, "", d)
		}
	}
	p.obs.duplicates = dups
}

// stopOutstanding reports a signaled stop (legacy's stop-pending projection)
// signaled at least stopOutstandingAge before now.
func stopOutstanding(info session.Info, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(info.DrainAt))
	return isDrainAckStopPendingInfo(info) && err == nil && now.Sub(at) >= stopOutstandingAge
}

// inputAges is the age at now of the census (its stalest leg cache), the
// inventory, and each K1 source kind (its stalest source).
func inputAges(now time.Time, legs map[string]beads.Store, obs *ObservationSnapshot, rec *externalReadsRecording) map[string]time.Duration {
	ages := make(map[string]time.Duration)
	stalest := func(key string, at time.Time) {
		if d, ok := ages[key]; !at.IsZero() && (!ok || now.Sub(at) > d) {
			ages[key] = now.Sub(at)
		}
	}
	for _, s := range legs {
		if c, ok := demandLabelKey(s).(interface{ Stats() beads.CacheStats }); ok {
			stalest("census", c.Stats().LastFreshAt)
		}
	}
	if obs != nil {
		stalest("inventory", obs.At)
	}
	if rec != nil {
		for k, src := range rec.Sources {
			stalest("k1/"+sourceKindNames[k.kind], src.EndedAt)
		}
	}
	return ages
}

var sourceKindNames = map[sourceKind]string{sourceDemand: "demand", sourceClosedNamed: "closed_named", sourceScaleCheck: "scale_check"}

// observePass runs after every pass: it alerts once when the boot gate stays
// closed past 2 × patrol, and emits the record when its content changed or a
// patrol has passed. A panic here is logged.
func (p *planner) observePass(now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(p.stderr, "v2 planner: observing the pass panicked: %v\n", r) //nolint:errcheck // best-effort stderr
		}
	}()
	switch b := p.boot; {
	case b.open():
		p.obs.gateClosedSince, p.obs.gateAlerted = time.Time{}, false
	case p.obs.gateClosedSince.IsZero():
		p.obs.gateClosedSince = now
	case !p.obs.gateAlerted && now.Sub(p.obs.gateClosedSince) > 2*p.patrol():
		p.obs.gateAlerted = true
		p.alert(alertBootGateClosed, "", fmt.Sprintf("boot gate closed since %s (cache primed %t, inventory complete %t, recording seen %t)", p.obs.gateClosedSince.UTC().Format(time.RFC3339), b.CachePrimed, b.InventoryComplete, b.RecordingSeen))
	}
	if p.emitRecord == nil {
		return
	}
	fields, key := p.passFields(now)
	if key == p.obs.lastKey && now.Sub(p.obs.lastEmit) < p.patrol() {
		return
	}
	p.obs.lastKey, p.obs.lastEmit = key, now
	p.emitRecord(fields)
}

// passFields is the record at now and its content key: what the last pass
// saw and did, and the counters that move only when something happens.
// Timings, ages and wakes ride along and never make a record due.
func (p *planner) passFields(now time.Time) (map[string]any, string) {
	m, c := p.metrics.snapshot(now), p.last.Result.Counts
	effects := "trace-only"
	if p.effects != nil {
		effects = "real"
	}
	gate := "closed"
	if p.boot.open() {
		gate = "open"
	}
	ages := make(map[string]int64, len(c.InputAges))
	for k, d := range c.InputAges {
		ages[k] = d.Milliseconds()
	}
	content := map[string]any{
		"effects": effects, "boot_gate": gate, "rows": c.Rows, "panicked": p.last.Panicked,
		"admitted": c.Admitted, "deferred": deferralKeys(c.Deferred), "in_flight": c.InFlight,
		"settled": m.Settled, "alerts": m.Alerts, "stops_outstanding_30m": c.StopOutstanding,
		"stale_self_rekeys": m.Settled[intentRekey+"/landed"],
	}
	for _, k := range soakSeries {
		content[k] = m.Series[k]
	}
	key := fmt.Sprint(content) // fmt sorts map keys
	fields := map[string]any{
		"passes": m.Passes, "panics": m.Panics, "duration_ms": m.LastDuration.Milliseconds(),
		"duration_p99_ms": m.DurationP99.Milliseconds(), "gap_p99_ms": m.GapP99.Milliseconds(), "duty_1m": m.Duty,
		"wakes": m.Wakes, "admitted_total": m.Admitted, "deferred_total": deferralKeys(m.Deferred), "input_age_ms": ages,
	}
	for k, v := range content {
		fields[k] = v
	}
	return fields, key
}

// deferralKeys keys deferred counts by "kind/cause".
func deferralKeys[N int | uint64](in map[deferral]N) map[string]N {
	out := make(map[string]N, len(in))
	for d, n := range in {
		out[d.Kind+"/"+d.Cause] = n
	}
	return out
}

// count adds one to (*m)[key] under the metrics lock.
func (pm *passMetrics) count(m *map[string]uint64, key string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	(*m)[key]++
}

// emitPassRecord adds the boot state and the inventory lane's last pass to
// a planner record, and traces it at reconcile.pass.
func (rt *plannerRuntime) emitPassRecord(fields map[string]any) {
	if rt.host.beginTrace == nil {
		return
	}
	fields["boot"] = rt.bootState()
	if rt.host.inventoryFields != nil {
		maps.Copy(fields, rt.host.inventoryFields())
	}
	t := rt.host.beginTrace("v2-pass")
	t.RecordControllerOperation(TraceSiteReconcilePass, TraceReasonRetained, TraceOutcomeComplete, "v2_pass", 0, fields)
	t.end(TraceCompletionCompleted, traceRecordPayload{"phase": "pass_record"})
}

// v2PassCommand asks the controller socket for v2PassStatus.
const v2PassCommand = "v2-pass"

// v2PassStatus is the controller's answer to v2PassCommand, which doctor
// prints. Zero passes: no planner, or no pass yet.
type v2PassStatus struct {
	Passes        uint64 `json:"passes"`
	LastPassAgeMS int64  `json:"last_pass_age_ms"`
}

// v2PassStatus reads the planner's metrics at now.
func (w *controllerWake) v2PassStatus(now time.Time) v2PassStatus {
	if w == nil || w.planner == nil {
		return v2PassStatus{}
	}
	m := w.planner.metrics.snapshot(now)
	return v2PassStatus{Passes: m.Passes, LastPassAgeMS: now.Sub(m.LastEnd).Milliseconds()}
}

// queryV2PassStatus asks the city's controller; one that does not answer,
// or predates the command, is an error.
func queryV2PassStatus(cityPath string) (v2PassStatus, error) {
	var st v2PassStatus
	resp, err := sendControllerCommandWithTimeouts(cityPath, v2PassCommand, 500*time.Millisecond, 500*time.Millisecond, 2*time.Second)
	if err == nil {
		err = json.Unmarshal(resp, &st)
	}
	return st, err
}
