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

const (
	quarantineSignalStatusNotEmitted = "not_emitted_by_production"
	quarantineSignalStatusObserved   = "observed"
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
	case events.SessionCrashed:
		return LifecycleCrashed
	case events.SessionQuarantined:
		return LifecycleQuarantined
	case events.SessionIdleKilled:
		return LifecycleIdleKilled
	case events.SessionDraining:
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
	AgentName     string // qualified, e.g. "rig/worker-1"
	Provider      string
}

// Rig parses agent name into the rig portion. For "rig/worker-1" it
// returns "rig"; for "coordinator" (no slash) it returns "" (city-level).
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
	Model         string `json:"model"`
	PromptVersion string `json:"prompt_version"`
	Rig           string `json:"rig"`
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
	Window           Window          `json:"-"`
	Filter           Filter          `json:"-"`
	Groups           []Group         `json:"groups"`
	Total            Group           `json:"total"`
	Skipped          int             `json:"skipped"` // events without enough attribute data to group
	AmbiguousAliases int             `json:"ambiguous_aliases"`
	Instrumentation  Instrumentation `json:"instrumentation"`
}

// Instrumentation summarizes whether the source event stream can support
// the requested reliability dimensions.
type Instrumentation struct {
	WorkerOperations       int    `json:"worker_operations"`
	MissingModel           int    `json:"missing_model"`
	MissingPromptVersion   int    `json:"missing_prompt_version"`
	QuarantineSignalStatus string `json:"quarantine_signal_status,omitempty"`
}

// workerOperationPayload is the minimal structural subset of
// api.WorkerOperationEventPayload that this package consumes. Decoupling
// it from the api package avoids a downstream import (api → reliability
// would create a cycle once 1c gets surfaced via the supervisor API).
type workerOperationPayload struct {
	SessionID     string `json:"session_id"`
	SessionName   string `json:"session_name"`
	Model         string `json:"model"`
	AgentName     string `json:"agent_name"`
	PromptVersion string `json:"prompt_version"`
	Provider      string `json:"provider"`
}

type sessionLifecyclePayload struct {
	SessionID string `json:"session_id"`
}

type sessionRecord struct {
	id    string
	seq   uint64
	attrs SessionAttrs
}

// Analyze produces a reliability report from the supplied events.
//
// The two passes are:
//
//  1. Build a session-attribute index from worker.operation event payloads.
//     For each lifecycle event, the latest payload at or before that event's
//     Seq wins, since attributes can change across reruns (model swap, prompt
//     edit) and future payloads must not rebucket past lifecycle events. The
//     index includes the worker-operation session ID plus payload aliases such
//     as session_name and agent_name because current lifecycle emitters do not
//     all use the session ID as Event.Subject. Display aliases that only reuse
//     the same qualified agent name resolve to the latest eligible record;
//     display aliases spanning multiple qualified agents count as ambiguous.
//  2. Walk lifecycle events, look up their session's attributes through that
//     alias index, and bucket by GroupKey.
//
// Events outside the window are dropped silently. Events with a
// LifecycleUnknown type are dropped silently. Events whose session has
// no attribute payload yet contribute to Report.Skipped — they would
// produce a (model="", version="", rig="") bucket otherwise, which
// hides instrumentation gaps rather than surfacing them.
func Analyze(es []events.Event, win Window, flt Filter) Report {
	sessionIndex := buildSessionIndex(es)
	sessionsByGroup := make(map[GroupKey]map[string]struct{})
	workerSessions := make(map[string]sessionRecord)
	groups := make(map[GroupKey]*Group)
	report := Report{Window: win, Filter: flt, Instrumentation: analyzeInstrumentation(es, win)}

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
		if e.Type == events.WorkerOperation {
			if rec, status := sessionIndex.lookup(e.Subject, e.Seq); status == lookupFound {
				if cur, ok := workerSessions[rec.id]; !ok || cur.seq < rec.seq {
					workerSessions[rec.id] = rec
				}
			}
			continue
		}
		kind := classifyType(e.Type)
		if kind == LifecycleUnknown {
			continue
		}
		rec, status := sessionIndex.lookup(lifecycleLookupSubject(e), e.Seq)
		if status == lookupAmbiguous {
			report.AmbiguousAliases++
			continue
		}
		if status == lookupMiss {
			report.Skipped++
			continue
		}
		key := GroupKey{Model: rec.attrs.Model, PromptVersion: rec.attrs.PromptVersion, Rig: rec.attrs.Rig()}
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
		addSession(key, rec.id)
	}

	for _, rec := range workerSessions {
		key := GroupKey{Model: rec.attrs.Model, PromptVersion: rec.attrs.PromptVersion, Rig: rec.attrs.Rig()}
		if keep(key) {
			addSession(key, rec.id)
		}
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
	report.Total = totalGroup(report.Groups, sessionsByGroup)
	return report
}

type lookupStatus int

const (
	lookupMiss lookupStatus = iota
	lookupFound
	lookupAmbiguous
)

type sessionIndex struct {
	records map[string][]sessionRecord
}

func (idx sessionIndex) lookup(subject string, maxSeq uint64) (sessionRecord, lookupStatus) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return sessionRecord{}, lookupMiss
	}
	records := idx.records[subject]
	if len(records) == 0 {
		return sessionRecord{}, lookupMiss
	}
	i := sort.Search(len(records), func(i int) bool {
		return records[i].seq > maxSeq
	})
	if i == 0 {
		return sessionRecord{}, lookupMiss
	}
	if recordsAmbiguous(records[:i]) {
		return sessionRecord{}, lookupAmbiguous
	}
	return records[i-1], lookupFound
}

// buildSessionIndex walks events and records worker.operation payload history
// per subject alias. Ambiguity is resolved at lookup time so a later
// cross-rig collision does not invalidate earlier lifecycle events for
// the same display name.
func buildSessionIndex(es []events.Event) sessionIndex {
	records := make(map[string][]sessionRecord)
	for _, e := range es {
		if e.Type != events.WorkerOperation {
			continue
		}
		if len(e.Payload) == 0 {
			continue
		}
		var p workerOperationPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		sessionID := firstNonEmpty(p.SessionID, e.Subject, p.SessionName)
		if sessionID == "" {
			continue
		}
		rec := sessionRecord{
			id:  sessionID,
			seq: e.Seq,
			attrs: SessionAttrs{
				Model:         p.Model,
				PromptVersion: p.PromptVersion,
				AgentName:     p.AgentName,
				Provider:      p.Provider,
			},
		}
		for _, alias := range workerOperationAliases(e, p, sessionID) {
			records[alias] = append(records[alias], rec)
		}
	}
	for alias := range records {
		sort.Slice(records[alias], func(i, j int) bool {
			return records[alias][i].seq < records[alias][j].seq
		})
	}
	return sessionIndex{records: records}
}

func lifecycleLookupSubject(e events.Event) string {
	if len(e.Payload) == 0 {
		return e.Subject
	}
	var p sessionLifecyclePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return e.Subject
	}
	if sessionID := strings.TrimSpace(p.SessionID); sessionID != "" {
		return sessionID
	}
	return e.Subject
}

func recordsAmbiguous(records []sessionRecord) bool {
	sessionIDs := make(map[string]struct{})
	owners := make(map[string]struct{})
	for _, rec := range records {
		sessionIDs[rec.id] = struct{}{}
		owners[recordOwnerKey(rec)] = struct{}{}
	}
	return len(sessionIDs) > 1 && len(owners) > 1
}

func recordOwnerKey(rec sessionRecord) string {
	if agentName := strings.TrimSpace(rec.attrs.AgentName); agentName != "" {
		return "agent:" + agentName
	}
	return "session:" + rec.id
}

func analyzeInstrumentation(es []events.Event, win Window) Instrumentation {
	out := Instrumentation{QuarantineSignalStatus: quarantineSignalStatusNotEmitted}
	for _, e := range es {
		if e.Type == events.SessionQuarantined {
			// This is a stream-level feature signal, not a windowed metric.
			out.QuarantineSignalStatus = quarantineSignalStatusObserved
		}
		if e.Type != events.WorkerOperation || !win.Contains(e.Ts) || len(e.Payload) == 0 {
			continue
		}
		var p workerOperationPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		out.WorkerOperations++
		if strings.TrimSpace(p.Model) == "" {
			out.MissingModel++
		}
		if strings.TrimSpace(p.PromptVersion) == "" {
			out.MissingPromptVersion++
		}
	}
	return out
}

func workerOperationAliases(e events.Event, p workerOperationPayload, sessionID string) []string {
	return compactStrings(
		sessionID,
		e.Subject,
		p.SessionID,
		p.SessionName,
		p.AgentName,
		unqualifiedName(p.AgentName),
		unqualifiedName(p.SessionName),
	)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func compactStrings(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

func unqualifiedName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexByte(name, '/'); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return name
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

// totalGroup sums event counts across all groups and de-duplicates session
// IDs for the total denominator. The Key is left zero-valued since the
// total spans every key combination.
func totalGroup(groups []Group, sessionsByGroup map[GroupKey]map[string]struct{}) Group {
	var t Group
	for _, g := range groups {
		t.Crashed += g.Crashed
		t.Quarantined += g.Quarantined
		t.IdleKilled += g.IdleKilled
		t.Drained += g.Drained
		t.UnhealthyTotal += g.UnhealthyTotal
	}
	uniqueSessions := make(map[string]struct{})
	for _, sessions := range sessionsByGroup {
		for sessionID := range sessions {
			uniqueSessions[sessionID] = struct{}{}
		}
	}
	t.Sessions = len(uniqueSessions)
	return t
}
