// Package reliability correlates session-lifecycle events against
// per-session attributes (model, prompt version, rig) to surface
// reliability trends. Introduced by issue #1254 (1c) as a read-only
// consumer of existing events; no new event emission.
//
// The package is a pure-data layer: it parses events.Event slices into
// grouped reports. The CLI (cmd/gc/cmd_analyze_reliability.go) handles
// IO, filtering, and presentation.
package reliability

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// Lifecycle event types tracked by the reliability report. Mirrors the
// constants in internal/events but kept local so this package compiles
// with only an events import.
const (
	eventSessionCrashed     = "session.crashed"
	eventSessionQuarantined = "session.quarantined"
	eventSessionIdleKilled  = "session.idle_killed"
	eventSessionDraining    = "session.draining"
	eventWorkerOperation    = "worker.operation"
)

// LifecycleKind names a tracked session-lifecycle event class. Strongly
// typed so callers can pattern-match without comparing strings.
type LifecycleKind int

const (
	// LifecycleUnknown means the event type didn't match a tracked kind.
	LifecycleUnknown LifecycleKind = iota
	// LifecycleCrashed corresponds to events.SessionCrashed.
	LifecycleCrashed
	// LifecycleQuarantined corresponds to events.SessionQuarantined.
	LifecycleQuarantined
	// LifecycleIdleKilled corresponds to events.SessionIdleKilled.
	LifecycleIdleKilled
	// LifecycleDraining corresponds to events.SessionDraining.
	LifecycleDraining
)

// String returns the canonical event-type label for a kind.
func (k LifecycleKind) String() string {
	switch k {
	case LifecycleCrashed:
		return "crashed"
	case LifecycleQuarantined:
		return "quarantined"
	case LifecycleIdleKilled:
		return "idle_killed"
	case LifecycleDraining:
		return "draining"
	default:
		return "unknown"
	}
}

// classifyType maps an events.Event.Type to a LifecycleKind.
func classifyType(eventType string) LifecycleKind {
	switch eventType {
	case eventSessionCrashed:
		return LifecycleCrashed
	case eventSessionQuarantined:
		return LifecycleQuarantined
	case eventSessionIdleKilled:
		return LifecycleIdleKilled
	case eventSessionDraining:
		return LifecycleDraining
	default:
		return LifecycleUnknown
	}
}

// SessionAttrs are the descriptive attributes the reliability report
// groups by. Sourced from worker.operation event payloads.
type SessionAttrs struct {
	Model         string
	PromptVersion string
	AgentName     string // qualified, e.g. "rig/polecat-1"
	Provider      string
}

// Rig parses agent name into the rig portion. For "rig/polecat-1" it
// returns "rig"; for "mayor" (no slash) it returns "" (city-level).
func (a SessionAttrs) Rig() string {
	if i := strings.IndexByte(a.AgentName, '/'); i > 0 {
		return a.AgentName[:i]
	}
	return ""
}

// GroupKey is the (model, prompt_version, rig) tuple the report groups by.
// Empty fields are valid keys ("no model observed", "version unknown",
// "city-level"); they appear in their own buckets so operators can spot
// missing instrumentation.
type GroupKey struct {
	Model         string
	PromptVersion string
	Rig           string
}

// Group reports per-(model, version, rig) reliability counts.
type Group struct {
	Key            GroupKey `json:"key"`
	Sessions       int      `json:"sessions"`
	Crashed        int      `json:"crashed"`
	Quarantined    int      `json:"quarantined"`
	IdleKilled     int      `json:"idle_killed"`
	Drained        int      `json:"drained"`
	UnhealthyTotal int      `json:"unhealthy_total"`
}

// CrashRate returns Crashed / Sessions or 0 if Sessions is zero.
// Returned as a fraction (0.05 = 5%).
func (g Group) CrashRate() float64 {
	if g.Sessions == 0 {
		return 0
	}
	return float64(g.Crashed) / float64(g.Sessions)
}

// UnhealthyRate is (crashed + quarantined + idle_killed + drained) / sessions.
// Returns 0 when Sessions is zero.
func (g Group) UnhealthyRate() float64 {
	if g.Sessions == 0 {
		return 0
	}
	return float64(g.UnhealthyTotal) / float64(g.Sessions)
}

// Window restricts the events considered to a time range. Zero-valued
// fields disable the corresponding bound.
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

// Filter narrows the event set to specific (model, rig) values when set.
// Empty fields disable the corresponding filter.
type Filter struct {
	Model string
	Rig   string
}

// Report is the top-level result of an analysis pass.
type Report struct {
	Window  Window  `json:"-"`
	Filter  Filter  `json:"-"`
	Groups  []Group `json:"groups"`
	Total   Group   `json:"total"`
	Skipped int     `json:"skipped"` // events without enough attribute data to group
}

// workerOperationPayload is the minimal structural subset of
// api.WorkerOperationEventPayload that this package consumes. Decoupling
// it from the api package avoids a downstream import (api → reliability
// would create a cycle once 1c gets surfaced via the supervisor API).
type workerOperationPayload struct {
	SessionID     string `json:"session_id"`
	Model         string `json:"model"`
	AgentName     string `json:"agent_name"`
	PromptVersion string `json:"prompt_version"`
	Provider      string `json:"provider"`
}

// Analyze produces a reliability report from the supplied events.
//
// The two passes are:
//
//  1. Build a session-attribute map from worker.operation event payloads.
//     The most recent (highest Seq) payload per session wins, since
//     attributes can change across reruns (model swap, prompt edit).
//  2. Walk lifecycle events, look up their session's attributes, and
//     bucket by GroupKey.
//
// Events outside the window are dropped silently. Events with a
// LifecycleUnknown type are dropped silently. Events whose session has
// no attribute payload yet contribute to Report.Skipped — they would
// produce a (model="", version="", rig="") bucket otherwise, which
// hides instrumentation gaps rather than surfacing them.
func Analyze(es []events.Event, win Window, flt Filter) Report {
	attrs := buildSessionAttrs(es)
	sessionsByGroup := make(map[GroupKey]map[string]struct{})
	groups := make(map[GroupKey]*Group)
	report := Report{Window: win, Filter: flt}

	keep := func(g GroupKey) bool {
		if flt.Model != "" && !strings.EqualFold(g.Model, flt.Model) {
			return false
		}
		if flt.Rig != "" && !strings.EqualFold(g.Rig, flt.Rig) {
			return false
		}
		return true
	}

	addSession := func(key GroupKey, sessionID string) {
		set, ok := sessionsByGroup[key]
		if !ok {
			set = make(map[string]struct{})
			sessionsByGroup[key] = set
		}
		set[sessionID] = struct{}{}
	}

	groupFor := func(key GroupKey) *Group {
		g, ok := groups[key]
		if !ok {
			g = &Group{Key: key}
			groups[key] = g
		}
		return g
	}

	for _, e := range es {
		if !win.Contains(e.Ts) {
			continue
		}
		if e.Type == eventWorkerOperation {
			if a, ok := attrs[e.Subject]; ok {
				key := GroupKey{Model: a.Model, PromptVersion: a.PromptVersion, Rig: a.Rig()}
				if keep(key) {
					addSession(key, e.Subject)
				}
			}
			continue
		}
		kind := classifyType(e.Type)
		if kind == LifecycleUnknown {
			continue
		}
		a, ok := attrs[e.Subject]
		if !ok {
			report.Skipped++
			continue
		}
		key := GroupKey{Model: a.Model, PromptVersion: a.PromptVersion, Rig: a.Rig()}
		if !keep(key) {
			continue
		}
		g := groupFor(key)
		switch kind {
		case LifecycleCrashed:
			g.Crashed++
		case LifecycleQuarantined:
			g.Quarantined++
		case LifecycleIdleKilled:
			g.IdleKilled++
		case LifecycleDraining:
			g.Drained++
		}
		g.UnhealthyTotal++
		addSession(key, e.Subject)
	}

	// Materialize Sessions counts and totals.
	for key, g := range groups {
		g.Sessions = len(sessionsByGroup[key])
	}
	// A session with worker.operation events but no lifecycle events
	// also counts toward Sessions in its group.
	for key, set := range sessionsByGroup {
		g := groupFor(key)
		if g.Sessions == 0 {
			g.Sessions = len(set)
		}
	}

	report.Groups = sortedGroups(groups)
	report.Total = totalGroup(report.Groups)
	return report
}

// buildSessionAttrs walks events and records the latest worker.operation
// payload attributes per session. Subsequent events with higher Seq for
// the same session override earlier ones.
func buildSessionAttrs(es []events.Event) map[string]SessionAttrs {
	type seqEntry struct {
		seq   uint64
		attrs SessionAttrs
	}
	latest := make(map[string]seqEntry)
	for _, e := range es {
		if e.Type != eventWorkerOperation {
			continue
		}
		if e.Subject == "" || len(e.Payload) == 0 {
			continue
		}
		var p workerOperationPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if cur, ok := latest[e.Subject]; ok && cur.seq >= e.Seq {
			continue
		}
		latest[e.Subject] = seqEntry{
			seq: e.Seq,
			attrs: SessionAttrs{
				Model:         p.Model,
				PromptVersion: p.PromptVersion,
				AgentName:     p.AgentName,
				Provider:      p.Provider,
			},
		}
	}
	out := make(map[string]SessionAttrs, len(latest))
	for k, v := range latest {
		out[k] = v.attrs
	}
	return out
}

// sortedGroups returns the report groups sorted deterministically:
// descending unhealthy total, then ascending model/version/rig for
// stable reading.
func sortedGroups(groups map[GroupKey]*Group) []Group {
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UnhealthyTotal != out[j].UnhealthyTotal {
			return out[i].UnhealthyTotal > out[j].UnhealthyTotal
		}
		if out[i].Key.Model != out[j].Key.Model {
			return out[i].Key.Model < out[j].Key.Model
		}
		if out[i].Key.PromptVersion != out[j].Key.PromptVersion {
			return out[i].Key.PromptVersion < out[j].Key.PromptVersion
		}
		return out[i].Key.Rig < out[j].Key.Rig
	})
	return out
}

// totalGroup sums counts across all groups. The Key is left zero-valued
// since the total spans every key combination.
func totalGroup(groups []Group) Group {
	var t Group
	for _, g := range groups {
		t.Sessions += g.Sessions
		t.Crashed += g.Crashed
		t.Quarantined += g.Quarantined
		t.IdleKilled += g.IdleKilled
		t.Drained += g.Drained
		t.UnhealthyTotal += g.UnhealthyTotal
	}
	return t
}
