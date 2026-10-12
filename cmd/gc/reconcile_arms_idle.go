package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// decideRow's idle-timeout arm (CONTRACT v5 §4 A15; plan C7a-1): legacy's
// idle-timeout ladder (DecideIdleTimeout) on a live desired row, then the
// stop verb with the idle finalize. Idle timeout is a stop, not a park
// (v5.7, SESS-576): the finalize writes SleepPatch(idle-timeout) and nothing
// that holds the row down, so a row AL1 still wants is started again by
// A18.

// A15's stops: the idle timeout, and the assigned-work defer streak's
// forced stop (SESS-575).
const (
	decideIdleTimeout   = "idle-timeout"
	decideIdleExhausted = "idle-timeout:assigned-work-exhausted"
)

// armIdleTimeout is A15: a live desired row whose runtime has been idle
// past its idle_timeout, by its activity clock (the observation lane's last
// activity) or its content clock (idleMemory), is stopped unless the ladder
// defers it, in legacy's order: a lifecycle blocker (a hold, a quarantine,
// a pin), assigned work (the pass's), or a min-floor member. A row that has
// deferred on assigned work on the same bead more passes running than its
// limit is stopped all the same (SESS-575). A pending interaction, an
// attach and, but for the forced stop, work claimed since the pass are the
// stop's legs, read fresh; each refuses it.
func armIdleTimeout(r *rowFacts) (intent, bool) {
	timeout, ok := r.idleTimeout()
	if !ok || !r.idleTriggered(timeout) || lifecycleTimerBlockerInfo(r.row.Info, r.w.Now) != "" {
		return intent{}, false
	}
	cfg, info, res := r.w.Env.Cfg, r.row.Info, r.idleResolution()
	it := intent{Kind: intentStop, Reason: decideIdleTimeout, Basis: rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}, Finish: idleTimeoutFinish}
	switch {
	case r.entry.AssignedWork != nil && r.w.IdleDefers[r.k] <= assignedWorkDeferLimitFor(cfg, res.TP.TemplateName, strings.TrimSpace(info.SessionName)):
		return intent{}, false
	case r.entry.AssignedWork != nil:
		it.Reason, it.Finish = decideIdleExhausted, idleExhaustedFinish
	case isMinFloorExemptIdleSession(r.w.Census.sessionInfos(), cfg, res.TP.TemplateName, info.ID):
		return intent{}, false
	}
	if !r.freshOwnAlive() { // the stop reads the runtime fresh
		return intent{}, false
	}
	return it, true
}

// idleTimeout is a live desired row's idle_timeout, when it has one.
func (r *rowFacts) idleTimeout() (time.Duration, bool) {
	e := r.entry
	if e == nil || !e.InDesired || e.Liveness != livenessAlive || r.w.Env == nil || r.w.Env.Cfg == nil {
		return 0, false
	}
	res := r.idleResolution()
	if res.Err != nil || res.TP.TemplateName == "" {
		return 0, false
	}
	t := idleTimeoutFor(r.w.Env.Cfg, res.TP.TemplateName, strings.TrimSpace(r.row.Info.SessionName))
	return t, t > 0
}

func (r *rowFacts) idleResolution() templateResolution {
	res, _ := r.w.Templates.lookup(r.row.Info)
	return res
}

// idleTriggered is legacy's checkIdle: the content clock fired this pass
// (a Claude TUI over a non-ACP transport), or the lane's last activity is
// older than timeout.
func (r *rowFacts) idleTriggered(timeout time.Duration) bool {
	if r.w.ContentIdle[r.k] {
		return true
	}
	obs, ok := r.observation()
	return ok && obs.LastActivityKnown && r.w.Now.Sub(obs.LastActivity) > timeout
}

// idleWorkDefer reports the ladder's assigned-work defer for the row (a
// trigger, no blocker, the pass's assigned work), with the anchor bead
// legacy keys its streak on.
func (r *rowFacts) idleWorkDefer(timeout time.Duration) (string, bool) {
	if r.entry.AssignedWork == nil || !r.idleTriggered(timeout) || lifecycleTimerBlockerInfo(r.row.Info, r.w.Now) != "" {
		return "", false
	}
	return strings.TrimSpace(r.row.Info.CurrentlyProcessingBeadID), true
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

// sessionInfos are the sessions leg's canonical rows by ID, legacy's
// infoByID, for the min-floor exemption.
func (c *sessionCensus) sessionInfos() map[string]session.Info {
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
// defer streak on one anchor bead (legacy's assignedWorkDeferTracker), and
// each live incarnation's content-clock anchor, the pass since its pane
// last read idle (legacy's idleTracker.idleSince).
type idleMemory struct {
	defers map[rowKey]idleDeferStreak
	since  map[idleIncarnation]time.Time
}

type idleDeferStreak struct {
	anchor string
	count  int
}

type idleIncarnation struct {
	k   rowKey
	inc int64
}

// update advances the memory on the pass's World and allocation and
// returns its view: a row the ladder defers on assigned work counts one
// more defer on the same anchor bead (a new bead starts at one), any other
// row resets; a live incarnation's pane read idle anchors once, read busy
// or indeterminate clears, and an anchor older than the row's idle_timeout
// fires, cleared as legacy's is.
func (m *idleMemory) update(w *World, a *allocDecision) (map[rowKey]int, map[rowKey]bool, map[string]bool) {
	if m.defers == nil {
		m.defers, m.since = make(map[rowKey]idleDeferStreak), make(map[idleIncarnation]time.Time)
	}
	defers := make(map[rowKey]int)
	fired := make(map[rowKey]bool)
	live := make(map[idleIncarnation]bool)
	want := make(map[string]bool) // the panes the lane reads for the content clock
	for _, row := range w.Census.Canonical() {
		r := &rowFacts{w: w, k: row.Key, entry: a.Snapshot.Entries[row.Key], row: row}
		timeout, ok := r.idleTimeout()
		if !ok {
			continue
		}
		ki := idleIncarnation{row.Key, row.Incarnation}
		live[ki] = true
		if idleTrackerContentClockApplies(row.Info.Provider, row.Info.Transport) {
			want[strings.TrimSpace(row.Info.SessionName)] = true
		}
		switch content := r.contentRead(); content {
		case contentIdle:
			at, anchored := m.since[ki]
			switch {
			case !anchored:
				m.since[ki] = w.Now
			case w.Now.Sub(at) > timeout:
				fired[row.Key] = true
				delete(m.since, ki)
			}
		case contentBusy, contentIndeterminate:
			delete(m.since, ki)
		}
		w.ContentIdle = fired // the trigger reads this pass's fire
		if anchor, deferred := r.idleWorkDefer(timeout); deferred {
			st := m.defers[row.Key]
			if st.anchor != anchor {
				st = idleDeferStreak{anchor: anchor}
			}
			st.count++
			m.defers[row.Key], defers[row.Key] = st, st.count
		}
	}
	for k := range m.defers {
		if _, ok := defers[k]; !ok {
			delete(m.defers, k)
		}
	}
	for ki := range m.since {
		if !live[ki] {
			delete(m.since, ki)
		}
	}
	return defers, fired, want
}
