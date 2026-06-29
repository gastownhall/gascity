package runproj

import "strings"

// DashboardSession is the dashboard-owned session projection that the run-health
// enrich layer joins lanes against. Port of the TypeScript DashboardSession in
// internal/api/dashboardspa/web/shared/src/dashboard-sessions.ts. Optional TS
// fields are modeled as pointers so an absent field (TS undefined) is
// distinguishable from an empty value — only Alias, Pool, LastActive, and
// Activity affect resolution/health, but the full shape is carried so the P2
// endpoint can unmarshal a /v0 sessions read directly.
type DashboardSession struct {
	ID            string   `json:"id"`
	Template      string   `json:"template"`
	SessionName   string   `json:"session_name"`
	Title         string   `json:"title"`
	Alias         *string  `json:"alias,omitempty"`
	State         string   `json:"state"`
	Reason        *string  `json:"reason,omitempty"`
	DisplayName   *string  `json:"display_name,omitempty"`
	CreatedAt     string   `json:"created_at"`
	LastActive    *string  `json:"last_active,omitempty"`
	Attached      bool     `json:"attached"`
	Rig           *string  `json:"rig,omitempty"`
	Pool          *string  `json:"pool,omitempty"`
	AgentKind     *string  `json:"agent_kind,omitempty"`
	Running       bool     `json:"running"`
	Model         *string  `json:"model,omitempty"`
	ContextPct    *float64 `json:"context_pct,omitempty"`
	ContextWindow *int     `json:"context_window,omitempty"`
	Activity      *string  `json:"activity,omitempty"`
	Provider      string   `json:"provider"`
}

// resolveSessionForTarget resolves a role/assignee/target label to the concrete
// session that carries it, or (zero, false) when none match. Port of TS
// resolveSessionForTarget: active sessions outrank non-active; within a tier,
// first match wins (deterministic given gc's recency-sorted iteration order).
func resolveSessionForTarget(target string, sessions []DashboardSession) (DashboardSession, bool) {
	if target == "" || len(sessions) == 0 {
		return DashboardSession{}, false
	}
	active := make([]DashboardSession, 0, len(sessions))
	for _, s := range sessions {
		if s.State == "active" {
			active = append(active, s)
		}
	}
	if s, ok := matchFirst(target, active); ok {
		return s, true
	}
	return matchFirst(target, sessions)
}

func matchFirst(target string, sessions []DashboardSession) (DashboardSession, bool) {
	for _, s := range sessions {
		if matchesSessionTarget(s, target) {
			return s, true
		}
	}
	return DashboardSession{}, false
}

// matchesSessionTarget reports whether session carries target in any of the four
// documented positions: exact alias, exact pool, last-segment of alias (split on
// '/' '.'), or last-segment of session_name (split on '__' '--'). Port of TS
// matchesSessionTarget.
func matchesSessionTarget(session DashboardSession, target string) bool {
	if session.Alias != nil && *session.Alias == target {
		return true
	}
	if session.Pool != nil && *session.Pool == target {
		return true
	}
	if session.Alias != nil && lastSegment(*session.Alias, []string{"/", "."}) == target {
		return true
	}
	if lastSegment(session.SessionName, []string{"__", "--"}) == target {
		return true
	}
	return false
}

// lastSegment returns the substring after the last occurrence of any separator
// in seps (whole-token match for multi-char separators), or value unchanged when
// no separator is present. Port of TS lastSegment.
func lastSegment(value string, seps []string) string {
	cut := -1
	sepLen := 0
	for _, sep := range seps {
		idx := strings.LastIndex(value, sep)
		if idx > cut {
			cut = idx
			sepLen = len(sep)
		}
	}
	if cut < 0 {
		return value
	}
	return value[cut+sepLen:]
}
