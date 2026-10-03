package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/resilience"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// endpointKey groups sessions that share serving capacity. "" = unguarded
// (fail open).
type endpointKey string

// resolvedEndpointKey returns the capacity group for a start: the selected
// [upstreams] name when the agent picks one, else the leaf provider name,
// else the provider recorded on the session bead. It never uses the builtin
// family: two providers that share an ancestor need not share an endpoint,
// and over-grouping would let one endpoint's refusals block the other.
func resolvedEndpointKey(tp TemplateParams, info sessionpkg.Info) endpointKey {
	if u := strings.TrimSpace(tp.Upstream); u != "" {
		return endpointKey("upstream:" + u)
	}
	if tp.ResolvedProvider != nil {
		if p := strings.TrimSpace(tp.ResolvedProvider.Name); p != "" {
			return endpointKey("provider:" + p)
		}
	}
	if p := strings.TrimSpace(info.Provider); p != "" {
		return endpointKey("provider:" + p)
	}
	return ""
}

// capacityBreakerSettings trips on the first refusal and backs off 5s, 10s,
// 20s, 40s, then 60s (full jitter within each cap). HalfOpenInterval is only a
// backstop: tickets resolve probes. The values are provisional.
var capacityBreakerSettings = resilience.Settings{
	Enabled:                true,
	ConsecutiveFailures:    1,
	OpenBase:               5 * time.Second,
	OpenMax:                60 * time.Second,
	HalfOpenInterval:       10 * time.Minute,
	TripDecay:              2 * time.Minute,
	IgnoreSuccessWhileOpen: true,
}

// capacityValveThreshold is how many refusals a template may take while other
// templates get in, with no other template refused, before its refusals are
// treated as its own failures again.
const capacityValveThreshold = 3

// capacityBackstopRefusals and capacityBackstopCloses are the valve's absolute
// backstop: a template refused this many times in a row, with no success of
// its own, while the endpoint closed at least capacityBackstopCloses times
// (others got in), goes back to normal failure accounting even when other
// templates were refused too. It keeps a bad template from holding its rows
// forever under saturation. Both are conservative: a healthy template under
// steady saturation still succeeds often enough to reset its streak.
const (
	capacityBackstopRefusals = 10
	capacityBackstopCloses   = 5
)

// capacityEndpointForgetAfter is how long an endpoint may go unused by any
// session before recordTick forgets it, so a key orphaned by a config reload
// does not stay non-closed forever.
const capacityEndpointForgetAfter = 10 * time.Minute

// capacityValveMemory bounds how long a session's refusal history is kept
// without a new refusal.
const capacityValveMemory = time.Hour

// breakerStateChangedEventType is the event emitted on every capacity
// breaker transition. Its payload is breakerStateChangedPayload. Like
// events.ProviderHealthGateAlert it is not in events.KnownEventTypes yet, so
// subscribers receive it through the custom-event envelope.
const breakerStateChangedEventType = "breaker.state_changed"

// breakerStateChangedPayload is the breaker.state_changed payload. Its shape
// matches events.BreakerStateChangedPayload from gastownhall/gascity#5953.
type breakerStateChangedPayload struct {
	Scope     string `json:"scope"`
	OpClass   string `json:"op_class"`
	From      string `json:"from"`
	To        string `json:"to"`
	Failures  int    `json:"failures,omitempty"`
	BackoffMs int64  `json:"backoff_ms,omitempty"`
}

// startVerdict is what a start attempt says about its endpoint's capacity.
type startVerdict int

const (
	verdictNotAttempted startVerdict = iota // provider Start never called (prepare error, later gate, limiter)
	verdictInconclusive                     // deferred/initializing/canceled/deadline/panic/other error/warm reuse
	verdictCapacity                         // runtime.IsProviderCapacity(err)
	verdictSuccess                          // err==nil, provider Start actually called, outcome success/converged/exists
)

// classifyStartVerdict maps a start result onto its capacity verdict. Only a
// start that actually reached the provider can prove capacity is free; a warm
// reuse touches no endpoint and is inconclusive.
func classifyStartVerdict(r startResult) startVerdict {
	if runtime.IsProviderCapacity(r.err) {
		return verdictCapacity
	}
	if r.err != nil || !r.providerStartCalled {
		return verdictInconclusive
	}
	switch r.outcome {
	case TraceOutcomeSuccess, TraceOutcomeStartErrorConverged, TraceOutcomeSessionExistsConverged, TraceOutcomeSessionExists:
		return verdictSuccess
	default:
		return verdictInconclusive
	}
}

// endpointStatus is one endpoint's breaker state as the reconciler sees it.
type endpointStatus struct {
	Key           endpointKey
	State         resilience.State
	Trips         int
	BackoffCap    time.Duration
	NextProbeAt   time.Time
	OpenSince     time.Time
	ProbeInFlight bool
}

// endpointCapacityGuard gates reconciler starts per serving endpoint. It
// wraps one resilience breaker per endpointKey and adds what the start path
// needs on top: probe ownership, probe rotation among refused sessions, the
// poison valve, per-tick deferral counts, and a transition queue drained
// outside every lock. State is process-local.
//
// Lock order: probeMu may be held while calling into a breaker; mu is taken
// by the breaker's transition callback (under the breaker's lock), so no
// breaker method is ever called while mu is held. flushMu is outermost and is
// never held while calling into a breaker.
type endpointCapacityGuard struct {
	registry *resilience.Registry
	now      func() time.Time

	probeMu sync.Mutex
	probes  map[endpointKey]*capacityTicket

	// flushMu serializes flushes across drain and emit, so transitions are
	// emitted in the order the breakers made them.
	flushMu sync.Mutex

	mu          sync.Mutex
	endpoints   map[endpointKey]*endpointCapacityState
	transitions []resilience.Transition
}

// endpointCapacityState is the guard's per-endpoint bookkeeping.
type endpointCapacityState struct {
	openSince time.Time
	// successes, refusalSeq and closes count every success, refusal and close
	// on the endpoint; refusalsBy splits refusals by template. The poison
	// valve orders events by them.
	successes, refusalSeq, closes int
	refusalsBy                    map[string]int
	// lastSeen is when a session routed to the endpoint was last looked up
	// (wake gate, admission, hold check); recordTick forgets endpoints that
	// no session has used for capacityEndpointForgetAfter.
	lastSeen time.Time
	// refusals counts this open episode's refusals per session ID, for probe
	// rotation. Cleared when the endpoint closes.
	refusals map[string]int
	// valve holds each refused template's valve history. It is keyed by
	// template (per endpoint), so replacement beads keep the count and two
	// seats of one template cannot mask each other.
	valve         map[string]capacityValveState
	deferredWake  int
	deferredAdmit int
}

type capacityValveState struct {
	lastRefusalAt time.Time
	// successes is the endpoint's success count, and otherRefusals its count
	// of other templates' refusals, just after this template's previous
	// refusal. A success of the template itself deletes its history, so any
	// later success is another template's.
	successes, otherRefusals int
	// count is the refusals that followed another template's success with no
	// other template refused.
	count int
	// streak is the consecutive probe refusals with no success of this
	// template; streakCloses is the endpoint's close count when it began.
	streak, streakCloses int
}

// newEndpointCapacityGuard returns a guard on the given clock.
func newEndpointCapacityGuard(now func() time.Time) *endpointCapacityGuard {
	settings := capacityBreakerSettings
	settings.Now = now
	g := &endpointCapacityGuard{
		registry:  resilience.NewRegistry(settings),
		now:       now,
		probes:    make(map[endpointKey]*capacityTicket),
		endpoints: make(map[endpointKey]*endpointCapacityState),
	}
	g.registry.SetOnStateChange(g.onTransition)
	return g
}

func (g *endpointCapacityGuard) breaker(k endpointKey) *resilience.Breaker {
	return g.registry.Breaker(string(k), resilience.OpClassSessionStart)
}

// stateLocked returns k's bookkeeping, creating it. Caller must hold g.mu.
func (g *endpointCapacityGuard) stateLocked(k endpointKey) *endpointCapacityState {
	st := g.endpoints[k]
	if st == nil {
		st = &endpointCapacityState{
			refusalsBy: make(map[string]int),
			refusals:   make(map[string]int),
			valve:      make(map[string]capacityValveState),
		}
		g.endpoints[k] = st
	}
	return st
}

// onTransition runs under the breaker's lock. It touches only guard state
// under g.mu and never calls back into a breaker; I/O happens in flush.
func (g *endpointCapacityGuard) onTransition(t resilience.Transition) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.transitions = append(g.transitions, t)
	st := g.stateLocked(endpointKey(t.Scope))
	switch {
	case t.From == resilience.StateClosed:
		st.openSince = t.At
	case t.To == resilience.StateClosed:
		st.openSince = time.Time{}
		st.closes++
		clear(st.refusals)
	}
}

// Eligible reports whether k could admit a start now: closed, or a probe is
// due. It is read-only and never consumes the probe. A nil guard or an empty
// key is always eligible.
func (g *endpointCapacityGuard) Eligible(k endpointKey) (bool, endpointStatus) {
	if g == nil || k == "" {
		return true, endpointStatus{Key: k}
	}
	g.noteSeen(k)
	b := g.breaker(k)
	return b.Available() || b.ProbeDue(), g.status(k, b.Status())
}

// noteDeferredWake counts a wake the reconciler skipped because k was not
// eligible, for the per-tick record.
func (g *endpointCapacityGuard) noteDeferredWake(k endpointKey) {
	if g == nil || k == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stateLocked(k).deferredWake++
}

// Admit is the commitment point for one start of sessionID, an instance of
// template, on k. It returns a ticket that the caller must Resolve exactly
// once, or false when the start must be deferred. While half-open, exactly
// one admission is the probe. A nil guard or an empty key admits without a
// ticket.
func (g *endpointCapacityGuard) Admit(k endpointKey, sessionID, template string) (*capacityTicket, bool) {
	if g == nil || k == "" {
		return nil, true
	}
	g.noteSeen(k)
	g.probeMu.Lock()
	allowed, probe := g.breaker(k).AllowProbe()
	var t *capacityTicket
	if allowed {
		t = &capacityTicket{guard: g, key: k, sessionID: sessionID, template: template, probe: probe}
		if probe {
			g.probes[k] = t
		}
	}
	g.probeMu.Unlock()
	if !allowed {
		g.mu.Lock()
		g.stateLocked(k).deferredAdmit++
		g.mu.Unlock()
		return nil, false
	}
	return t, true
}

// HoldsPendingCreate reports whether never-started pending creates on k must
// keep their rows: while k is refusing and for pendingCreateNeverStartedTimeout
// after it last closed, so queued demand gets a fresh window after an outage.
func (g *endpointCapacityGuard) HoldsPendingCreate(k endpointKey) bool {
	if g == nil || k == "" {
		return false
	}
	g.noteSeen(k)
	st := g.breaker(k).Status()
	if st.State != resilience.StateClosed {
		return true
	}
	return !st.ClosedAt.IsZero() && g.now().Sub(st.ClosedAt) < pendingCreateNeverStartedTimeout
}

// noteSeen records that a session routed to k was just looked up.
func (g *endpointCapacityGuard) noteSeen(k endpointKey) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stateLocked(k).lastSeen = g.now()
}

// endpointHoldForRows returns a hold check for rows seen without template
// params (the stale-session reaper runs before desired state is built). The
// endpoint comes from config: the agent's selected upstream, else the provider
// recorded on the row, which is already the resolved leaf.
func endpointHoldForRows(cfg *config.City, g *endpointCapacityGuard) func(sessionpkg.Info) bool {
	return func(info sessionpkg.Info) bool {
		var tp TemplateParams
		if agent := findAgentByTemplate(cfg, info.Template); agent != nil {
			tp.Upstream = agent.Upstream
		}
		return g.HoldsPendingCreate(resolvedEndpointKey(tp, info))
	}
}

// refusalsInEpisode is how many times sessionID was refused during k's
// current open episode. Probe rotation ranks by it, fewest first.
func (g *endpointCapacityGuard) refusalsInEpisode(k endpointKey, sessionID string) int {
	if g == nil || k == "" {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.endpoints[k]; st != nil {
		return st.refusals[sessionID]
	}
	return 0
}

// Snapshot returns every known endpoint's status, sorted by key.
func (g *endpointCapacityGuard) Snapshot() []endpointStatus {
	if g == nil {
		return nil
	}
	statuses := g.registry.Statuses()
	out := make([]endpointStatus, 0, len(statuses))
	for key, st := range statuses {
		out = append(out, g.status(endpointKey(key.Scope), st))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (g *endpointCapacityGuard) status(k endpointKey, st resilience.Status) endpointStatus {
	now := g.now()
	out := endpointStatus{Key: k, State: st.State, Trips: st.Trips, BackoffCap: st.BackoffCap}
	switch st.State {
	case resilience.StateOpen:
		out.NextProbeAt = st.Deadline
	case resilience.StateHalfOpen:
		probeExpires := st.LastProbeAt.Add(capacityBreakerSettings.HalfOpenInterval)
		out.ProbeInFlight = !st.LastProbeAt.IsZero() && now.Before(probeExpires)
		out.NextProbeAt = now
		if out.ProbeInFlight {
			out.NextProbeAt = probeExpires
		}
	}
	g.mu.Lock()
	if es := g.endpoints[k]; es != nil {
		out.OpenSince = es.openSince
	}
	g.mu.Unlock()
	return out
}

// resolve applies a ticket's verdict to its breaker and reports whether the
// poison valve tripped for its session.
func (g *endpointCapacityGuard) resolve(t *capacityTicket, v startVerdict) bool {
	b := g.breaker(t.key)
	now := g.now()
	valve := false
	switch v {
	case verdictSuccess:
		g.dropProbe(t)
		b.RecordSuccess()
		g.mu.Lock()
		st := g.stateLocked(t.key)
		st.successes++
		delete(st.valve, t.template)
		g.mu.Unlock()
	case verdictCapacity:
		g.mu.Lock()
		valve = g.stateLocked(t.key).noteRefusal(t.sessionID, t.template, t.probe, now)
		g.mu.Unlock()
		if valve {
			// Others keep getting in and this template does not: its refusal
			// is not evidence about the endpoint.
			g.releaseProbe(t)
		} else {
			g.dropProbe(t)
			b.RecordFailure()
		}
	default:
		g.releaseProbe(t)
	}
	t.stateAfter = b.State()
	return valve
}

// noteRefusal records a refusal of sessionID, an instance of template, and
// reports whether the valve tripped for the template. It trips on either:
//   - capacityValveThreshold refusals that each followed another template's
//     success with no other template refused since the template's previous
//     refusal (a first refusal never counts), so outages and steady
//     saturation, where other templates are refused too, never count; or
//   - the backstop: capacityBackstopRefusals probe refusals in a row with no
//     success of the template while the endpoint closed
//     capacityBackstopCloses times. Only probe refusals count: the herd a
//     close admits is refused by saturation, not by the template.
//
// Caller must hold g.mu.
func (st *endpointCapacityState) noteRefusal(sessionID, template string, probe bool, now time.Time) bool {
	st.refusals[sessionID]++
	for key, v := range st.valve {
		if now.Sub(v.lastRefusalAt) > capacityValveMemory {
			delete(st.valve, key)
		}
	}
	prev, seen := st.valve[template]
	st.refusalSeq++
	st.refusalsBy[template]++
	otherRefusals := st.refusalSeq - st.refusalsBy[template]
	v := prev
	if seen && st.successes > prev.successes && otherRefusals == prev.otherRefusals {
		v.count++
	}
	if probe {
		if v.streak == 0 {
			v.streakCloses = st.closes
		}
		v.streak++
	}
	v.lastRefusalAt = now
	v.successes, v.otherRefusals = st.successes, otherRefusals
	if v.count >= capacityValveThreshold ||
		(v.streak >= capacityBackstopRefusals && st.closes-v.streakCloses >= capacityBackstopCloses) {
		delete(st.valve, template)
		return true
	}
	st.valve[template] = v
	return false
}

// releaseProbe frees t's probe so the next Admit gets a new one. It only
// releases the probe t still owns, never a newer probe admitted after the
// breaker moved on.
func (g *endpointCapacityGuard) releaseProbe(t *capacityTicket) {
	if !t.probe {
		return
	}
	g.probeMu.Lock()
	defer g.probeMu.Unlock()
	if g.probes[t.key] == t {
		delete(g.probes, t.key)
		g.breaker(t.key).ReleaseProbe()
	}
}

// dropProbe forgets t's probe ownership when its verdict resolves the
// half-open state itself.
func (g *endpointCapacityGuard) dropProbe(t *capacityTicket) {
	if !t.probe {
		return
	}
	g.probeMu.Lock()
	defer g.probeMu.Unlock()
	if g.probes[t.key] == t {
		delete(g.probes, t.key)
	}
}

// flush emits queued breaker transitions: one stderr line and one
// breaker.state_changed event each. It runs outside every lock.
func (g *endpointCapacityGuard) flush(rec events.Recorder, stderr io.Writer) {
	if g == nil {
		return
	}
	g.flushMu.Lock()
	defer g.flushMu.Unlock()
	g.mu.Lock()
	pending := g.transitions
	g.transitions = nil
	g.mu.Unlock()
	for _, t := range pending {
		if stderr != nil {
			fmt.Fprintf(stderr, "session reconciler: endpoint %s capacity breaker %s→%s (backoff %s)\n", t.Scope, t.From, t.To, t.Backoff.Round(time.Millisecond)) //nolint:errcheck
		}
		if rec == nil {
			continue
		}
		payload, err := json.Marshal(breakerStateChangedPayload{
			Scope:     t.Scope,
			OpClass:   t.OpClass,
			From:      t.From.String(),
			To:        t.To.String(),
			Failures:  t.Failures,
			BackoffMs: t.Backoff.Milliseconds(),
		})
		if err != nil {
			continue
		}
		rec.Record(events.Event{
			Type:    breakerStateChangedEventType,
			Ts:      t.At.UTC(),
			Actor:   "gc",
			Subject: t.Scope,
			Message: fmt.Sprintf("endpoint %s capacity breaker %s→%s", t.Scope, t.From, t.To),
			Payload: payload,
		})
	}
}

// recordTick emits one baseline trace record per endpoint that is not closed
// or deferred starts this tick, resets the per-tick deferral counts, and
// flushes queued transitions.
func (g *endpointCapacityGuard) recordTick(trace *sessionReconcilerTraceCycle, rec events.Recorder, stderr io.Writer) {
	if g == nil {
		return
	}
	g.forgetUnusedEndpoints()
	statuses := g.Snapshot()
	type deferrals struct{ wake, admit int }
	counts := make(map[endpointKey]deferrals)
	g.mu.Lock()
	for k, st := range g.endpoints {
		if st.deferredWake > 0 || st.deferredAdmit > 0 {
			counts[k] = deferrals{wake: st.deferredWake, admit: st.deferredAdmit}
		}
		st.deferredWake, st.deferredAdmit = 0, 0
	}
	g.mu.Unlock()
	if trace != nil {
		for _, st := range statuses {
			d := counts[st.Key]
			if st.State == resilience.StateClosed && d.wake == 0 && d.admit == 0 {
				continue
			}
			trace.RecordControllerOperation(TraceSiteEndpointCapacityBreaker, TraceReasonEndpointCapacityOpen, endpointStateOutcome(st.State), "endpoint_capacity_breaker", 0, traceRecordPayload{
				"endpoint":        string(st.Key),
				"state":           st.State.String(),
				"trips":           st.Trips,
				"backoff_cap_ms":  st.BackoffCap.Milliseconds(),
				"next_probe_at":   traceTime(st.NextProbeAt),
				"open_since":      traceTime(st.OpenSince),
				"deferred_wake":   d.wake,
				"deferred_admit":  d.admit,
				"probe_in_flight": st.ProbeInFlight,
			})
		}
	}
	g.flush(rec, stderr)
}

// forgetUnusedEndpoints drops every endpoint no session has used for
// capacityEndpointForgetAfter: its breaker, bookkeeping, and probe.
func (g *endpointCapacityGuard) forgetUnusedEndpoints() {
	now := g.now()
	var forget []endpointKey
	g.mu.Lock()
	for k, st := range g.endpoints {
		if now.Sub(st.lastSeen) >= capacityEndpointForgetAfter {
			forget = append(forget, k)
			delete(g.endpoints, k)
		}
	}
	g.mu.Unlock()
	if len(forget) == 0 {
		return
	}
	g.probeMu.Lock()
	for _, k := range forget {
		delete(g.probes, k)
		g.registry.Remove(string(k), resilience.OpClassSessionStart)
	}
	g.probeMu.Unlock()
}

func endpointStateOutcome(s resilience.State) TraceOutcomeCode {
	switch s {
	case resilience.StateOpen:
		return TraceOutcomeOpen
	case resilience.StateHalfOpen:
		return TraceOutcomeHalfOpen
	default:
		return TraceOutcomeClosed
	}
}

func traceTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// capacityTicket is one admitted start on a guarded endpoint. Resolve it
// exactly once; later calls are no-ops. A nil ticket is unguarded.
type capacityTicket struct {
	guard     *endpointCapacityGuard
	key       endpointKey
	sessionID string
	template  string
	probe     bool

	once       sync.Once
	valve      bool
	stateAfter resilience.State
}

// Resolve applies the attempt's verdict to the endpoint breaker and reports
// whether the poison valve returned this session to normal failure
// accounting. Idempotent and nil-safe.
func (t *capacityTicket) Resolve(v startVerdict) (valveTripped bool) {
	if t == nil {
		return false
	}
	t.once.Do(func() { t.valve = t.guard.resolve(t, v) })
	return t.valve
}

// resolveStartCapacity resolves the attempt's ticket from its result, flushes
// the transitions that caused, and settles capacityRefused: a refusal takes
// the capacity commit arm only when a guard admitted it and the poison valve
// did not trip. Without a ticket the legacy failure arms run, because then no
// breaker throttles retries.
func resolveStartCapacity(result startResult, rec events.Recorder, stderr io.Writer) startResult {
	ticket := result.prepared.capacityTicket
	refused := runtime.IsProviderCapacity(result.err)
	result.capacityValve = ticket.Resolve(classifyStartVerdict(result))
	result.capacityRefused = refused && ticket != nil && !result.capacityValve
	if ticket != nil {
		ticket.guard.flush(rec, stderr)
	}
	return result
}

// abandonCapacityTicket resolves a ticket whose start never reached the
// provider. Safe to defer: it is a no-op once the ticket is resolved.
func abandonCapacityTicket(ticket *capacityTicket, rec events.Recorder, stderr io.Writer) {
	if ticket == nil {
		return
	}
	ticket.Resolve(verdictNotAttempted)
	ticket.guard.flush(rec, stderr)
}

// sortCandidatesByProbeRotation stably reorders each endpoint's candidates,
// within the slots that endpoint already holds, so sessions refused more often
// this episode go last. Durable wake order is deliberately unchanged by a
// refusal, so without this the oldest refused session would be chosen as the
// probe on every window. Rotating within an endpoint's own slots never demotes
// its sessions behind another endpoint's backlog.
func sortCandidatesByProbeRotation(candidates []startCandidate, g *endpointCapacityGuard) {
	if g == nil {
		return
	}
	slots := make(map[endpointKey][]int)
	for i, c := range candidates {
		if k := resolvedEndpointKey(c.tp, c.info); k != "" {
			slots[k] = append(slots[k], i)
		}
	}
	for k, idx := range slots {
		if len(idx) < 2 {
			continue
		}
		members := make([]startCandidate, len(idx))
		ranks := make(map[string]int, len(idx))
		for j, i := range idx {
			members[j] = candidates[i]
			ranks[members[j].info.ID] = g.refusalsInEpisode(k, members[j].info.ID)
		}
		sort.SliceStable(members, func(a, b int) bool {
			return ranks[members[a].info.ID] < ranks[members[b].info.ID]
		})
		for j, i := range idx {
			candidates[i] = members[j]
		}
	}
}
