package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/resilience"
	"github.com/gastownhall/gascity/internal/runtime"
)

// fleetBreakerScope is the breaker.state_changed scope of the fleet start
// breaker.
const fleetBreakerScope = "fleet"

const (
	// fleetFailureWindow is how long an automatic-start failure counts toward
	// tripping the breaker.
	fleetFailureWindow = 120 * time.Second
	// fleetTripSessions and fleetTripTemplates are how many distinct sessions
	// and distinct templates must fail inside fleetFailureWindow to trip it. A
	// single template failing is that template's problem, not the fleet's.
	fleetTripSessions  = 3
	fleetTripTemplates = 2
	// fleetStateCacheStale is how long the runtime's state cache may go
	// without a successful refresh, while sessions are expected running,
	// before the breaker trips.
	fleetStateCacheStale = 60 * time.Second
	// fleetProbeGrace is added to startup_timeout to make the half-open probe
	// window: a probe that never resolves cannot block a new one longer.
	fleetProbeGrace = 60 * time.Second
	// fleetConfirmForgetFactor bounds the start-confirmation tracker: starts
	// never observed again are dropped after this many thresholds.
	fleetConfirmForgetFactor = 10

	// fleetReasonFailures and fleetReasonStateCache are the reasons a fleet
	// breaker opens.
	fleetReasonFailures   = "failures"
	fleetReasonStateCache = "state-cache"
)

// fleetBreakerSettings backs off 15s, 30s, 60s, 2m, 4m, then 5m (full jitter
// within each cap). HalfOpenInterval is set per guard from startup_timeout.
// ConsecutiveFailures is unused: the guard trips the breaker itself once the
// failure window holds enough distinct sessions and templates.
var fleetBreakerSettings = resilience.Settings{
	Enabled:                true,
	OpenBase:               15 * time.Second,
	OpenMax:                5 * time.Minute,
	TripDecay:              2 * time.Minute,
	IgnoreSuccessWhileOpen: true,
}

// fleetOutcome is what one automatic start says about fleet start health.
type fleetOutcome int

const (
	// fleetInconclusive counts nothing: a capacity refusal, a deferral, a
	// start that never reached the provider, or an unresolved collision.
	fleetInconclusive fleetOutcome = iota
	// fleetStartOK is a start that returned without error; it is only
	// confirmed once the session is observed alive for stabilityThreshold.
	fleetStartOK
	// fleetStartFailed is an observed start failure.
	fleetStartFailed
)

// classifyFleetOutcome maps a start result onto its fleet outcome. A provider
// capacity refusal is classified first: it belongs to the endpoint breaker and
// says nothing about the fleet.
func classifyFleetOutcome(r startResult) fleetOutcome {
	if runtime.IsProviderCapacity(r.err) || !r.providerStartCalled {
		return fleetInconclusive
	}
	switch r.outcome {
	case TraceOutcomeDeferred, TraceOutcomeSessionInitializing, TraceOutcomeCanceled, TraceOutcomePanicRecovered:
		return fleetInconclusive
	case TraceOutcomeDeadlineExceeded:
		return fleetStartFailed
	}
	if r.err != nil {
		if errors.Is(r.err, runtime.ErrSessionExists) || errors.Is(r.err, runtime.ErrRuntimeUnavailable) {
			return fleetInconclusive
		}
		return fleetStartFailed
	}
	if r.diedDuringStartup {
		return fleetStartFailed
	}
	if r.outcome == TraceOutcomeSuccess {
		return fleetStartOK
	}
	return fleetInconclusive
}

// resolveStartFleet resolves the attempt's fleet ticket from its result and
// flushes the transitions that caused. A nil ticket is unguarded.
func resolveStartFleet(result startResult, rec events.Recorder, stderr io.Writer) {
	ticket := result.prepared.fleetTicket
	if ticket == nil {
		return
	}
	ticket.Resolve(classifyFleetOutcome(result))
	ticket.guard.Flush(rec, stderr)
}

// abandonFleetTicket resolves a ticket whose start never reached the provider
// as inconclusive. Safe to defer: it is a no-op once the ticket is resolved.
func abandonFleetTicket(ticket *fleetTicket, guard *fleetStartGuard, rec events.Recorder, stderr io.Writer) {
	if ticket == nil {
		return
	}
	ticket.Resolve(fleetInconclusive)
	guard.Flush(rec, stderr)
}

// confirmVerdict is a start-confirmation tracker's reading of one session.
type confirmVerdict int

const (
	// confirmUntracked means the session has no start being confirmed.
	confirmUntracked confirmVerdict = iota
	// confirmPending means the session is alive but not yet for the threshold.
	confirmPending
	// confirmAlive means the session stayed alive for the threshold.
	confirmAlive
	// confirmDied means the session died before the threshold.
	confirmDied
)

// startConfirmTracker confirms started sessions: alive for a threshold after
// Begin confirms, dead before it fails. It is safe for concurrent use.
type startConfirmTracker struct {
	now       func() time.Time
	threshold time.Duration

	mu      sync.Mutex
	started map[string]time.Time
}

// newStartConfirmTracker returns a tracker on the given clock.
func newStartConfirmTracker(now func() time.Time, threshold time.Duration) *startConfirmTracker {
	return &startConfirmTracker{now: now, threshold: threshold, started: make(map[string]time.Time)}
}

// Begin starts confirming id from now. Starts never observed again are
// dropped once they are fleetConfirmForgetFactor thresholds old.
func (t *startConfirmTracker) Begin(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for other, at := range t.started {
		if now.Sub(at) > fleetConfirmForgetFactor*t.threshold {
			delete(t.started, other)
		}
	}
	t.started[id] = now
}

// Observe reads id's liveness and returns its verdict; a confirmed or died
// session is forgotten. A session first seen dead after the threshold is
// forgotten without a verdict: it did not die inside the confirmation window
// as far as anyone observed.
func (t *startConfirmTracker) Observe(id string, alive bool) confirmVerdict {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.started[id]
	if !ok {
		return confirmUntracked
	}
	aged := t.now().Sub(at) >= t.threshold
	switch {
	case alive && aged:
		delete(t.started, id)
		return confirmAlive
	case alive:
		return confirmPending
	case aged:
		delete(t.started, id)
		return confirmUntracked
	default:
		delete(t.started, id)
		return confirmDied
	}
}

// Forget drops id without a verdict.
func (t *startConfirmTracker) Forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.started, id)
}

// fleetStatus is the fleet breaker's state as operators see it.
type fleetStatus struct {
	State         resilience.State
	Trips         int
	BackoffCap    time.Duration
	NextProbeAt   time.Time
	OpenSince     time.Time
	ProbeInFlight bool
	// Reason is why the breaker last opened: "failures" or "state-cache".
	Reason string
}

// fleetFailure is one counted automatic-start failure.
type fleetFailure struct {
	at       time.Time
	session  string
	template string
}

// fleetStart is a start being confirmed: whose it is and whether it is a probe.
type fleetStart struct {
	template string
	probe    bool
	at       time.Time
}

// fleetStartGuard is the fleet-wide breaker over automatic session starts. It
// wraps one resilience breaker and adds what the start path needs on top: the
// failure window that trips it, probe ownership, start confirmation, and a
// transition queue drained outside every lock. State is process-local. A nil
// guard admits everything: the kill switch is today's behavior.
//
// Lock order: flushMu, then probeMu, then a breaker's lock, then mu. The
// breaker's transition callback takes mu, so no breaker method is ever called
// while mu is held.
type fleetStartGuard struct {
	registry         *resilience.Registry
	now              func() time.Time
	halfOpenInterval time.Duration
	confirm          *startConfirmTracker

	probeMu sync.Mutex
	probe   *fleetTicket

	// flushMu serializes flushes across drain and emit, so transitions are
	// emitted in the order the breaker made them.
	flushMu sync.Mutex

	mu          sync.Mutex
	failures    []fleetFailure
	starts      map[string]fleetStart
	reason      string
	openSince   time.Time
	transitions []resilience.Transition
	// deferred counts starts the gate kept from being attempted since the
	// last tick record.
	deferred int
}

// newFleetStartGuard returns a guard on the given clock whose half-open probe
// window is startupTimeout plus a grace.
func newFleetStartGuard(now func() time.Time, startupTimeout time.Duration) *fleetStartGuard {
	settings := fleetBreakerSettings
	settings.Now = now
	settings.HalfOpenInterval = startupTimeout + fleetProbeGrace
	g := &fleetStartGuard{
		registry:         resilience.NewRegistry(settings),
		now:              now,
		halfOpenInterval: settings.HalfOpenInterval,
		confirm:          newStartConfirmTracker(now, stabilityThreshold),
		starts:           make(map[string]fleetStart),
	}
	g.registry.SetOnStateChange(g.onTransition)
	return g
}

func (g *fleetStartGuard) breaker() *resilience.Breaker {
	return g.registry.Breaker(fleetBreakerScope, resilience.OpClassSessionStart)
}

// onTransition runs under the breaker's lock. It touches only guard state
// under g.mu and never calls back into a breaker; I/O happens in Flush.
func (g *fleetStartGuard) onTransition(t resilience.Transition) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.transitions = append(g.transitions, t)
	switch {
	case t.From == resilience.StateClosed:
		g.openSince = t.At
	case t.To == resilience.StateClosed:
		g.openSince = time.Time{}
	}
}

// Eligible reports whether a start could be admitted now: closed, or a probe
// is due. It is read-only and never consumes the probe. A nil guard is always
// eligible.
func (g *fleetStartGuard) Eligible() bool {
	if g == nil {
		return true
	}
	b := g.breaker()
	return b.Available() || b.ProbeDue()
}

// NoteDeferred counts a start the gate kept from being attempted, for the
// per-tick record.
func (g *fleetStartGuard) NoteDeferred() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deferred++
}

// Admit is the commitment point for one automatic start of sessionID, an
// instance of template. It returns a ticket that the caller must Resolve
// exactly once, or false when the start must be deferred. While half-open,
// exactly one admission is the probe. A nil guard admits without a ticket.
func (g *fleetStartGuard) Admit(sessionID, template string) (*fleetTicket, bool) {
	if g == nil {
		return nil, true
	}
	g.probeMu.Lock()
	allowed, probe := g.breaker().AllowProbe()
	var t *fleetTicket
	if allowed {
		t = &fleetTicket{guard: g, sessionID: sessionID, template: template, probe: probe}
		if probe {
			g.probe = t
		}
	}
	g.probeMu.Unlock()
	if !allowed {
		g.NoteDeferred()
		return nil, false
	}
	return t, true
}

// fleetTicket is one admitted automatic start. Resolve it exactly once; later
// calls are no-ops. A nil ticket is unguarded.
type fleetTicket struct {
	guard     *fleetStartGuard
	sessionID string
	template  string
	probe     bool

	once sync.Once
}

// Probe reports whether the ticket is the half-open probe.
func (t *fleetTicket) Probe() bool { return t != nil && t.probe }

// Resolve applies the attempt's outcome. Idempotent and nil-safe.
func (t *fleetTicket) Resolve(o fleetOutcome) {
	if t == nil {
		return
	}
	t.once.Do(func() { t.guard.resolve(t, o) })
}

// resolve applies a ticket's outcome to the breaker. A probe resolves the
// half-open state: failure re-opens it, a started session is confirmed later
// (ObserveLiveness), and an inconclusive end frees the probe.
func (g *fleetStartGuard) resolve(t *fleetTicket, o fleetOutcome) {
	switch o {
	case fleetStartFailed:
		if t.probe {
			g.dropProbe(t)
			g.breaker().RecordFailure()
			return
		}
		g.noteFailure(t.sessionID, t.template)
	case fleetStartOK:
		g.dropProbe(t)
		g.beginConfirm(t.sessionID, t.template, t.probe)
	default:
		g.releaseProbe(t)
	}
}

// beginConfirm starts watching a started session for stabilityThreshold.
func (g *fleetStartGuard) beginConfirm(sessionID, template string, probe bool) {
	g.confirm.Begin(sessionID)
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	// A start whose session is never observed again (closed, renamed) would
	// stay here for the life of the controller; drop it once the tracker has.
	for id, st := range g.starts {
		if now.Sub(st.at) > fleetConfirmForgetFactor*stabilityThreshold {
			delete(g.starts, id)
		}
	}
	g.starts[sessionID] = fleetStart{template: template, probe: probe, at: now}
}

// noteFailure records an automatic-start failure and trips the breaker when
// the window holds fleetTripSessions distinct sessions across
// fleetTripTemplates distinct templates. Failures while the breaker is not
// closed are stragglers and count for nothing.
func (g *fleetStartGuard) noteFailure(sessionID, template string) {
	b := g.breaker()
	if b.State() != resilience.StateClosed {
		return
	}
	now := g.now()
	g.mu.Lock()
	kept := g.failures[:0]
	for _, f := range g.failures {
		if now.Sub(f.at) < fleetFailureWindow {
			kept = append(kept, f)
		}
	}
	kept = append(kept, fleetFailure{at: now, session: sessionID, template: template})
	g.failures = kept
	sessions, templates := make(map[string]struct{}), make(map[string]struct{})
	for _, f := range g.failures {
		sessions[f.session] = struct{}{}
		templates[f.template] = struct{}{}
	}
	trip := len(sessions) >= fleetTripSessions && len(templates) >= fleetTripTemplates
	if trip {
		g.failures = nil
		g.reason = fleetReasonFailures
	}
	g.mu.Unlock()
	if trip {
		b.Trip()
	}
}

// releaseProbe frees t's probe so the next Admit gets a new one. It only
// releases the probe t still owns.
func (g *fleetStartGuard) releaseProbe(t *fleetTicket) {
	if !t.probe {
		return
	}
	g.probeMu.Lock()
	defer g.probeMu.Unlock()
	if g.probe == t {
		g.probe = nil
		g.breaker().ReleaseProbe()
	}
}

// dropProbe forgets t's probe ownership when its outcome resolves the
// half-open state itself.
func (g *fleetStartGuard) dropProbe(t *fleetTicket) {
	if !t.probe {
		return
	}
	g.probeMu.Lock()
	defer g.probeMu.Unlock()
	if g.probe == t {
		g.probe = nil
	}
}

// ObserveLiveness feeds one session's liveness to the start confirmation. A
// probe's session that stays alive for stabilityThreshold closes the breaker;
// one that dies inside it re-opens the breaker. Another session dying inside
// it is a start failure. A nil guard ignores it.
func (g *fleetStartGuard) ObserveLiveness(sessionID string, alive bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	tracking := len(g.starts) > 0
	g.mu.Unlock()
	if !tracking {
		return
	}
	verdict := g.confirm.Observe(sessionID, alive)
	if verdict == confirmUntracked {
		// The tracker aged the start out without a verdict.
		g.mu.Lock()
		delete(g.starts, sessionID)
		g.mu.Unlock()
		return
	}
	if verdict != confirmAlive && verdict != confirmDied {
		return
	}
	g.mu.Lock()
	start, ok := g.starts[sessionID]
	delete(g.starts, sessionID)
	g.mu.Unlock()
	if !ok {
		return
	}
	b := g.breaker()
	switch {
	case verdict == confirmAlive && start.probe:
		b.RecordSuccess()
	case verdict == confirmDied && start.probe:
		if b.State() == resilience.StateHalfOpen {
			b.RecordFailure()
		}
	case verdict == confirmDied:
		g.noteFailure(sessionID, start.template)
	}
}

// ForgetSession drops any start being confirmed for sessionID. The reconciler
// calls it for a session it is stopping, draining or closing, or that has left
// the desired set: its runtime ending is then the reconciler's own doing, not a
// start that failed. A session whose runtime ends with no such cause inside
// stabilityThreshold is a failed start and is not forgotten.
func (g *fleetStartGuard) ForgetSession(sessionID string) {
	if g == nil {
		return
	}
	g.confirm.Forget(sessionID)
	g.mu.Lock()
	delete(g.starts, sessionID)
	g.mu.Unlock()
}

// NoteStateCache feeds the age of the runtime's last successful state
// refresh; known is false when the runtime keeps no such cache. A cache that
// has gone fleetStateCacheStale without a refresh while sessions are expected
// running trips the breaker: the reconciler is acting on a view it cannot
// refresh.
func (g *fleetStartGuard) NoteStateCache(age time.Duration, known bool, expected int) {
	if g == nil || !known || expected < 1 || age <= fleetStateCacheStale {
		return
	}
	b := g.breaker()
	if b.State() != resilience.StateClosed {
		return
	}
	g.mu.Lock()
	g.reason = fleetReasonStateCache
	g.mu.Unlock()
	b.Trip()
}

// Reset closes the breaker and forgets its failure window and the starts it
// was confirming. It reports whether the breaker was open or half-open. The
// close is queued as a transition like any other.
func (g *fleetStartGuard) Reset() bool {
	if g == nil {
		return false
	}
	g.probeMu.Lock()
	defer g.probeMu.Unlock()
	prev := g.breaker().State()
	g.probe = nil
	g.registry.Remove(fleetBreakerScope, resilience.OpClassSessionStart)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures = nil
	clear(g.starts)
	g.openSince = time.Time{}
	if prev == resilience.StateClosed {
		return false
	}
	g.transitions = append(g.transitions, resilience.Transition{
		Scope:   fleetBreakerScope,
		OpClass: resilience.OpClassSessionStart,
		From:    prev,
		To:      resilience.StateClosed,
		At:      g.now(),
	})
	return true
}

// Status returns the breaker's current state. A nil guard reports closed.
func (g *fleetStartGuard) Status() fleetStatus {
	if g == nil {
		return fleetStatus{}
	}
	st := g.breaker().Status()
	now := g.now()
	out := fleetStatus{State: st.State, Trips: st.Trips, BackoffCap: st.BackoffCap}
	switch st.State {
	case resilience.StateOpen:
		out.NextProbeAt = st.Deadline
	case resilience.StateHalfOpen:
		probeExpires := st.LastProbeAt.Add(g.halfOpenInterval)
		out.ProbeInFlight = !st.LastProbeAt.IsZero() && now.Before(probeExpires)
		out.NextProbeAt = now
		if out.ProbeInFlight {
			out.NextProbeAt = probeExpires
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out.OpenSince = g.openSince
	if st.State != resilience.StateClosed {
		out.Reason = g.reason
	}
	return out
}

// Flush emits queued transitions: a breaker.state_changed event for each, and
// one stderr line per episode (when it opens and when it closes, not for the
// re-opens and probes between). It runs outside every lock.
func (g *fleetStartGuard) Flush(rec events.Recorder, stderr io.Writer) {
	if g == nil {
		return
	}
	g.flushMu.Lock()
	defer g.flushMu.Unlock()
	g.mu.Lock()
	pending := g.transitions
	g.transitions = nil
	reason := g.reason
	g.mu.Unlock()
	for _, t := range pending {
		if stderr != nil {
			switch {
			case t.From == resilience.StateClosed && t.To == resilience.StateOpen:
				fmt.Fprintf(stderr, "session reconciler: fleet start breaker opened (%s; backoff %s): automatic session starts are limited to probes until sessions stay up; clear with `gc breaker reset`\n", reason, t.Backoff.Round(time.Millisecond)) //nolint:errcheck
			case t.To == resilience.StateClosed:
				fmt.Fprintf(stderr, "session reconciler: fleet start breaker closed: automatic session starts resumed\n") //nolint:errcheck
			}
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
			Message: fmt.Sprintf("fleet start breaker %s→%s", t.From, t.To),
			Payload: payload,
		})
	}
}

// recordTick closes one reconciler tick: it records the breaker's state to the
// trace while an episode is under way (open, half-open, or deferring starts),
// resets the deferred count, and flushes queued transitions. Nil-safe.
func (g *fleetStartGuard) recordTick(trace *sessionReconcilerTraceCycle, rec events.Recorder, stderr io.Writer) {
	if g == nil {
		return
	}
	st := g.Status()
	g.mu.Lock()
	deferred := g.deferred
	g.deferred = 0
	g.mu.Unlock()
	if trace != nil && (st.State != resilience.StateClosed || deferred > 0) {
		trace.RecordControllerOperation(TraceSiteFleetStartBreaker, TraceReasonFleetStartBreakerOpen, endpointStateOutcome(st.State), "fleet_start_breaker", 0, traceRecordPayload{
			"state":           st.State.String(),
			"reason":          st.Reason,
			"trips":           st.Trips,
			"backoff_cap_ms":  st.BackoffCap.Milliseconds(),
			"next_probe_at":   traceTime(st.NextProbeAt),
			"open_since":      traceTime(st.OpenSince),
			"deferred":        deferred,
			"probe_in_flight": st.ProbeInFlight,
		})
	}
	g.Flush(rec, stderr)
}
