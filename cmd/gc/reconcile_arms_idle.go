package main

import (
	"cmp"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// decideRow's idle-timeout arm (CONTRACT v5 §4 A15, v5.9s; plan C7a-1):
// legacy's idle-timeout ladder (DecideIdleTimeout) on a live desired row,
// then the stop verb with the idle finalize. Idle timeout is a stop, not a
// park (v5.7, SESS-576): the finalize writes SleepPatch(idle-timeout) and
// nothing that holds the row down, so a row AL1 still wants is started
// again by A18 (one pass later than legacy, §12.2 #60). The decision rests
// on the lane's reads; the stop re-proves idleness fresh on the routed leaf
// before it kills (idleReproved).

// A15's stops: the idle timeout, and the assigned-work defer streak's
// forced stop (SESS-575).
const (
	decideIdleTimeout   = "idle-timeout"
	decideIdleExhausted = "idle-timeout:assigned-work-exhausted"
)

// causeIdleUnproven: the stop's fresh read does not prove the agent idle.
const causeIdleUnproven = "idle-unproven"

func isIdleReason(reason string) bool {
	return reason == decideIdleTimeout || reason == decideIdleExhausted
}

// armIdleTimeout is A15: a live desired row the ladder stops (idleLadder)
// is stopped, unless the pass observed it attached or its attach unknown
// (v2 never idle-kills an attached session, §12.2 #61), or its runtime no
// longer reads its own. A pending interaction is the ladder's hold, which
// leaves an active cancelable request to A19. The stop's legs, read fresh,
// catch a prompt, an attach and, but for the forced stop, a claim since
// the pass.
func armIdleTimeout(r *rowFacts) (intent, bool) {
	dec, ok := r.idleLadder()
	obs := r.w.Observed[r.k]
	switch {
	case !ok || dec.Action != session.TimerActionStop, obs.Attached || obs.Uncertain:
		return intent{}, false
	case !r.freshOwnAlive(): // the stop reads the runtime fresh
		return intent{}, false
	}
	it := intent{Kind: intentStop, Reason: decideIdleTimeout, Basis: rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}, Finish: idleTimeoutFinish}
	if dec.TraceReason == string(TraceReasonAssignedWorkExhausted) {
		it.Reason, it.Finish = decideIdleExhausted, idleExhaustedFinish
	}
	return it, true
}

// idleLadder is the row's ladder decision with the defer streak's forced
// stop: an assigned-work defer whose streak on one anchor bead is past the
// row's limit stops it all the same (SESS-575).
func (r *rowFacts) idleLadder() (session.TimerDecision, bool) {
	dec, s, ok := r.idleLadderRaw()
	if ok && deferredOnWork(dec) && r.w.idleView().defers[r.k] > s.Limit {
		dec = session.DecideAssignedWorkExhausted()
	}
	return dec, ok
}

// idleLadderRaw is legacy's ladder (DecideIdleTimeout, its gathers in its
// order) on the pass's facts: the trigger, a lifecycle blocker, a pending
// interaction (wait-hold exempt, pendingExempt), the pass's assigned work,
// a min-floor seat.
func (r *rowFacts) idleLadderRaw() (session.TimerDecision, idleSetting, bool) {
	s, ok := r.idleSetting()
	if !ok {
		return session.TimerDecision{}, s, false
	}
	info, now := r.row.Info, r.w.Now
	f := session.TimerFacts{Triggered: r.idleTriggered(s.Timeout)}
	if f.Triggered {
		f.Blocker = lifecycleTimerBlockerInfo(info, now)
	}
	dec := session.DecideIdleTimeout(f)
	for range 3 { // each gather once, as legacy's loop
		switch dec.Action {
		case session.TimerActionGatherPending:
			f.Pending = session.PendingNo
			if containsWakeReason(r.entry.WakeReasons, WakePending) && !pendingExempt(info, now) {
				f.Pending = session.PendingYes
			}
		case session.TimerActionGatherAssignedWork:
			f.AssignedWork = session.AssignedWorkNone
			if r.entry.AssignedWork != nil {
				f.AssignedWork = session.AssignedWorkHas
			}
		case session.TimerActionGatherMinFloor:
			f.MinFloor = session.MinFloorNo
			if isMinFloorExemptIdleSession(r.w.idleView().sessionInfos(r.w.Census), r.w.Env.Cfg, s.Template, info.ID) {
				f.MinFloor = session.MinFloorYes
			}
		default:
			return dec, s, true
		}
		dec = session.DecideIdleTimeout(f)
	}
	return dec, s, true
}

func deferredOnWork(dec session.TimerDecision) bool {
	return dec.Action == session.TimerActionDefer && dec.TraceReason == string(TraceReasonAssignedWork)
}

// idleSetting is the row's idle_timeout and defer limit, for a live desired
// row with a timeout: the pass's memo, else resolved.
func (r *rowFacts) idleSetting() (idleSetting, bool) {
	e := r.entry
	if e == nil || !e.InDesired || e.Liveness != livenessAlive || r.w.Env == nil || r.w.Env.Cfg == nil {
		return idleSetting{}, false
	}
	return r.w.idleSettingOf(r.k, r.row.Info)
}

// idleSetting is a row's resolved lifecycle settings for A15.
type idleSetting struct {
	Template string
	Timeout  time.Duration
	Limit    int
}

// idleSettingOf is k's settings, the pass's memo when it has them.
func (w *World) idleSettingOf(k rowKey, info session.Info) (idleSetting, bool) {
	if p := w.Idle; p != nil && p.settings != nil {
		if s, ok := p.settings[k]; ok {
			return s, s.Timeout > 0
		}
	}
	return resolveIdleSetting(w, info)
}

func resolveIdleSetting(w *World, info session.Info) (idleSetting, bool) {
	res, _ := w.Templates.lookup(info)
	if res.Err != nil || res.TP.TemplateName == "" || w.Env == nil || w.Env.Cfg == nil {
		return idleSetting{}, false
	}
	cfg, name := w.Env.Cfg, strings.TrimSpace(info.SessionName)
	s := idleSetting{Template: res.TP.TemplateName, Timeout: idleTimeoutFor(cfg, res.TP.TemplateName, name)}
	if s.Timeout > 0 {
		s.Limit = assignedWorkDeferLimitFor(cfg, res.TP.TemplateName, name)
	}
	return s, s.Timeout > 0
}

// idleTriggered is legacy's checkIdle: the content clock fired this pass
// (a Claude TUI over a non-ACP transport), or the lane's last activity is
// older than timeout.
func (r *rowFacts) idleTriggered(timeout time.Duration) bool {
	if r.w.idleView().fired[r.k] {
		return true
	}
	obs, ok := r.observation()
	return ok && obs.LastActivityKnown && r.w.Now.Sub(obs.LastActivity) > timeout
}

// contentRead is the lane's content read of the row's pane, for a row on
// the content clock.
func (r *rowFacts) contentRead() contentState {
	if !idleTrackerContentClockApplies(r.row.Info.Provider, r.row.Info.Transport) {
		return contentUnread
	}
	obs, _ := r.observation()
	return obs.Content
}

func (r *rowFacts) observation() (RuntimeObservation, bool) {
	if r.w.Obs == nil {
		return RuntimeObservation{}, false
	}
	return r.w.Obs.Observation(strings.TrimSpace(r.row.Info.SessionName), r.w.Now, r.w.ObsMaxAge)
}

// idleReproved is the idle stop's fresh proof on the routed leaf (v5.9s):
// a row the content clock fired for reads idle by a fresh snapshot; any
// other reads its fresh last activity older than its idle_timeout. Unknown
// never counts as idle.
func idleReproved(v txView) bool {
	if v.World.idleView().fired[v.It.Key] {
		return v.Fence.Content == contentIdle
	}
	s, ok := v.World.idleSettingOf(v.It.Key, v.Row)
	a := v.Fence.Activity
	return ok && a != nil && a.Reportable && a.Err == nil && !a.Last.IsZero() && v.Now.Sub(a.Last) > s.Timeout
}

// idleTimeoutFinish is the idle stop's finalize (D4): legacy's
// SleepPatch(idle-timeout), a sleep through the Sleep cell; never a close.
func idleTimeoutFinish(f finalizeFacts) finalizeChoice {
	return finalizeChoice{Patch: session.SleepPatch(f.Now, string(session.SleepReasonIdleTimeout)), AsSleep: true}
}

// idleExhaustedFinish is the forced stop's: SleepPatch(assigned-work
// exhausted).
func idleExhaustedFinish(f finalizeFacts) finalizeChoice {
	return finalizeChoice{Patch: session.SleepPatch(f.Now, string(session.SleepReasonAssignedWorkExhausted)), AsSleep: true}
}

// idleTimeoutFor is the row's idle_timeout as legacy's idle tracker
// resolves it (buildIdleTracker, checkIdle); zero is none.
func idleTimeoutFor(cfg *config.City, template, name string) time.Duration {
	t, _ := lifecycleSetting(cfg, template, name, func(a *config.Agent) (time.Duration, bool) {
		return a.IdleTimeoutDuration(), a.IdleTimeoutDuration() > 0
	})
	return t
}

// assignedWorkDeferLimitFor is the row's consecutive assigned-work defer
// limit as legacy's tracker resolves it (buildAssignedWorkDeferTracker,
// limitFor): a positive assigned_work_defer_limit, else
// defaultAssignedWorkDeferLimit.
func assignedWorkDeferLimitFor(cfg *config.City, template, name string) int {
	limit, ok := lifecycleSetting(cfg, template, name, func(a *config.Agent) (int, bool) {
		if a.AssignedWorkDeferLimit == nil {
			return 0, false
		}
		return *a.AssignedWorkDeferLimit, *a.AssignedWorkDeferLimit > 0
	})
	if !ok {
		return defaultAssignedWorkDeferLimit
	}
	return limit
}

// lifecycleSetting is a per-agent lifecycle setting (get: its value, and
// whether the agent sets it) resolved for the row named name of template as
// legacy's lifecycle trackers resolve theirs, pure: a configured named
// session's runtime name takes its agent's, but never mode always; an
// agent that expands into instances gives its to every row of its template
// (the template fallback), but an always named session's; a singleton agent
// to its one runtime name.
func lifecycleSetting[V any](cfg *config.City, template, name string, get func(*config.Agent) (V, bool)) (V, bool) {
	cityName := cfg.EffectiveCityName()
	var fallback V
	hasFallback, exempt := false, false
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		v, ok := get(a)
		if !ok {
			continue
		}
		if named := config.FindNamedSession(cfg, a.QualifiedName()); named != nil {
			if config.NamedSessionRuntimeName(cityName, cfg.Workspace, named.QualifiedName()) == name {
				if named.ModeOrDefault() != "always" {
					return v, true
				}
				exempt = true
			}
			if !a.SupportsInstanceExpansion() {
				continue
			}
		}
		switch {
		case a.SupportsInstanceExpansion():
			if a.SupportsGenericEphemeralSessions() && lifecycleTemplateFallbackKey(*a) == template {
				fallback, hasFallback = v, true
				exemptAlwaysNamedTemplateFallbacks(cfg, cityName, template, func(n string) { exempt = exempt || n == name })
			}
		case config.NamedSessionRuntimeName(cityName, cfg.Workspace, a.QualifiedName()) == name: // startupSessionName, through its exempted twin
			return v, true
		}
	}
	if exempt || !hasFallback {
		var none V
		return none, false
	}
	return fallback, true
}

// idlePass is A15's view of one pass, from planner memory (idleMemory):
// the rows' lifecycle settings, the assigned-work defer streaks this pass
// counted, the rows whose content clock fired, and the census's rows by ID
// for the min-floor exemption, built once.
type idlePass struct {
	settings map[rowKey]idleSetting
	defers   map[rowKey]int
	fired    map[rowKey]bool
	infos    map[string]session.Info
}

// idleView is the pass's view; a World without one (a test's) has none.
func (w *World) idleView() *idlePass {
	if w.Idle == nil {
		return &idlePass{}
	}
	return w.Idle
}

// sessionInfos are the sessions leg's canonical rows by ID, legacy's
// infoByID, for the min-floor exemption.
func (p *idlePass) sessionInfos(c *sessionCensus) map[string]session.Info {
	if p.infos != nil {
		return p.infos
	}
	leg := c.sessionsLeg()
	out := make(map[string]session.Info, len(c.Rows))
	for k, row := range c.Rows {
		if k.Leg == leg && row.DuplicateOf == "" {
			out[k.ID] = row.Info
		}
	}
	return out
}

// idleMemory is A15's planner memory: each row's consecutive assigned-work
// defer streak on one anchor bead (legacy's assignedWorkDeferTracker),
// counted at most once a patrol; each runtime's content-clock anchor, the
// pass since its pane last read idle (legacy's idleTracker.idleSince); and
// each row's last idle-timeout trace (a deferral or a stop).
type idleMemory struct {
	defers map[rowKey]idleDeferStreak
	since  map[idleIncarnation]time.Time
	traced map[rowKey]idleTraced
}

type idleDeferStreak struct {
	anchor string
	count  int
	at     time.Time // the count's last step
}

// idleIncarnation keys a content anchor by the row and the runtime
// incarnation the lane observes (on tmux its session, creation and pane
// pid; the row's token where a backend reports none): a restart or a
// relaunch in place starts a fresh idle clock (v5.9s).
type idleIncarnation struct {
	k           rowKey
	incarnation string
}

type idleTraced struct {
	line string
	at   time.Time
}

// update advances the memory on the pass's World and allocation, sets the
// pass's view on w, and returns the panes the lane reads for the content
// clock and the ladder's deferral traces (legacy's
// reconciler.session.idle_timeout), at most one a patrol per row and
// reason. Each row is isolated: a panic skips the row (P6).
func (m *idleMemory) update(w *World, a *allocDecision, patrol time.Duration, stderr io.Writer) (map[string]bool, []rowTrace) {
	if m.defers == nil {
		m.defers, m.since, m.traced = make(map[rowKey]idleDeferStreak), make(map[idleIncarnation]time.Time), make(map[rowKey]idleTraced)
	}
	p := &idlePass{settings: make(map[rowKey]idleSetting), defers: make(map[rowKey]int), fired: make(map[rowKey]bool)}
	w.Idle = p
	p.infos = p.sessionInfos(w.Census)
	live := make(map[idleIncarnation]bool)
	want := make(map[string]bool)
	var traces []rowTrace
	for _, row := range w.Census.Canonical() {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					fmt.Fprintf(stderr, "v2 planner: idle memory for row %s/%s panicked: %v\n", row.Key.Leg, row.Key.ID, rec) //nolint:errcheck // best-effort stderr
				}
			}()
			if t, ok := m.updateRow(w, a, row, patrol, live, want); ok {
				traces = append(traces, t)
			}
		}()
	}
	for k := range m.defers {
		if _, ok := p.defers[k]; !ok {
			delete(m.defers, k)
		}
	}
	for ki := range m.since {
		if !live[ki] {
			delete(m.since, ki)
		}
	}
	for k := range m.traced {
		if _, ok := w.Census.Rows[k]; !ok {
			delete(m.traced, k)
		}
	}
	return want, traces
}

// updateRow is update for one row: a live incarnation's pane read idle
// anchors once and fires past the row's idle_timeout, cleared as legacy's
// is, and any other read (busy, indeterminate, unread or erring) clears
// the anchor, so unknown never counts as idle; then a row the ladder
// defers on assigned work counts one more defer on the same anchor bead, a
// patrol after its last (a new bead starts at one), and any other row
// resets.
func (m *idleMemory) updateRow(w *World, a *allocDecision, row censusRow, patrol time.Duration, live map[idleIncarnation]bool, want map[string]bool) (rowTrace, bool) {
	p := w.Idle
	p.settings[row.Key], _ = resolveIdleSetting(w, row.Info) // memoized, a row with no timeout too
	r := &rowFacts{w: w, k: row.Key, entry: a.Snapshot.Entries[row.Key], row: row}
	s, ok := r.idleSetting()
	if !ok {
		return rowTrace{}, false
	}
	if idleTrackerContentClockApplies(row.Info.Provider, row.Info.Transport) {
		want[strings.TrimSpace(row.Info.SessionName)] = true
		obs, _ := r.observation()
		ki := idleIncarnation{row.Key, cmp.Or(obs.Incarnation, "token:"+strings.TrimSpace(row.Info.InstanceToken))}
		live[ki] = true
		at, anchored := m.since[ki]
		switch {
		case r.contentRead() != contentIdle:
			delete(m.since, ki)
		case !anchored:
			m.since[ki] = w.Now
		case w.Now.Sub(at) > s.Timeout:
			p.fired[row.Key] = true
			delete(m.since, ki)
		}
	}
	dec, _, _ := r.idleLadderRaw()
	if deferredOnWork(dec) {
		anchor := strings.TrimSpace(row.Info.CurrentlyProcessingBeadID)
		st, counted := m.defers[row.Key]
		switch {
		case !counted || st.anchor != anchor:
			st = idleDeferStreak{anchor: anchor, count: 1, at: w.Now}
		case w.Now.Sub(st.at) >= patrol:
			st.count, st.at = st.count+1, w.Now
		}
		m.defers[row.Key], p.defers[row.Key] = st, st.count
	}
	if dec.Action != session.TimerActionDefer {
		return rowTrace{}, false
	}
	reason, outcome := timerTraceCodes(dec)
	return m.trace(row, s.Template, string(reason), string(outcome), w.Now, patrol)
}

// trace is row's legacy idle-timeout record (reconciler.session.idle_timeout)
// with reason and outcome, unless the row was traced so within patrol.
func (m *idleMemory) trace(row censusRow, template, reason, outcome string, now time.Time, patrol time.Duration) (rowTrace, bool) {
	line := reason + "/" + outcome
	if t := m.traced[row.Key]; t.line == line && now.Sub(t.at) < patrol {
		return rowTrace{}, false
	}
	m.traced[row.Key] = idleTraced{line: line, at: now}
	return rowTrace{Key: row.Key, Site: TraceSiteReconcilerIdleTimeout, Template: template, SessionName: strings.TrimSpace(row.Info.SessionName), Reason: reason, Outcome: outcome}, true
}

// stopTraces are legacy's records of the idle stops the pass decided
// (reconciler.session.idle_timeout; mc-ensxp), with the codes legacy takes
// from the stop's decision through timerTraceCodes: idle_timeout/stop, or
// assigned_work_exhausted/stop_defer_exhausted for the forced stop
// (DecideAssignedWorkExhausted). The decision is the row's ladder on the
// pass's facts (idleLadder), the one armIdleTimeout stopped on. As legacy
// records them when it decides the kill, at most one a patrol per row and
// reason.
func (m *idleMemory) stopTraces(w *World, a *allocDecision, intents []intent, patrol time.Duration) []rowTrace {
	var out []rowTrace
	for _, it := range intents {
		if it.Kind != intentStop || !isIdleReason(it.Reason) {
			continue
		}
		row, ok := w.Census.Rows[it.Key]
		if !ok {
			continue
		}
		r := &rowFacts{w: w, k: it.Key, entry: a.Snapshot.Entries[it.Key], row: row}
		dec, ok := r.idleLadder()
		if !ok || dec.Action != session.TimerActionStop {
			continue
		}
		s, _ := w.idleSettingOf(it.Key, row.Info)
		reason, outcome := timerTraceCodes(dec)
		if t, ok := m.trace(row, s.Template, string(reason), string(outcome), w.Now, patrol); ok {
			out = append(out, t)
		}
	}
	return out
}
