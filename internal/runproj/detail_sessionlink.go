package runproj

import (
	"regexp"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// sessionIDRe gates a value before it is fed to the supervisor session routes.
// Port of TS SESSION_ID_RE (session-id.ts) — lowercase-only, case-sensitive.
var sessionIDRe = regexp.MustCompile(`^(gc|td|th|[a-z]{4})-[a-z0-9-]{1,32}$`)

// supervisorSessionIDSuffixRe extracts a trailing supervisor id from a
// pool-qualified handle. Port of the TS suffix match in supervisorSessionIdFrom.
var supervisorSessionIDSuffixRe = regexp.MustCompile(`(?:^|[-_/])((?:gc|td|th|[a-z]{4})-[a-z0-9-]{1,32})$`)

// runSessionIndex indexes sessions by id, name, and template for run-link
// resolution. Port of TS RunSessionIndex.
type runSessionIndex struct {
	byID       map[string]DashboardSession
	byName     map[string]DashboardSession
	byTemplate map[string][]DashboardSession
}

// runSessionLinkContext carries the session index (and scope) for link
// resolution. Port of TS RunSessionLinkContext. A nil index mirrors undefined.
type runSessionLinkContext struct {
	sessionIndex *runSessionIndex
	scopeRef     string
}

// buildRunSessionIndex indexes a session list for link resolution. Port of TS
// buildRunSessionIndex (first-write-wins for the id/name maps).
func buildRunSessionIndex(sessions []DashboardSession) runSessionIndex {
	idx := runSessionIndex{
		byID:       make(map[string]DashboardSession),
		byName:     make(map[string]DashboardSession),
		byTemplate: make(map[string][]DashboardSession),
	}
	for _, session := range sessions {
		rememberSession(idx.byID, session.ID, session)
		rememberSession(idx.byName, derefString(session.Alias), session)
		rememberSession(idx.byName, session.Title, session)
		rememberSession(idx.byName, session.SessionName, session)
		if template := nonEmpty(session.Template); template != "" {
			idx.byTemplate[template] = append(idx.byTemplate[template], session)
		}
	}
	return idx
}

// runSessionLinkFor resolves a bead to a streamable session link, or (zero,
// false) when none is usable. Port of TS runSessionLinkFor.
func runSessionLinkFor(bead runSnapshotBead, status string, ctx runSessionLinkContext) (RunSessionLink, bool) {
	if status == "pending" || status == "ready" {
		return RunSessionLink{}, false
	}
	assignee := nonEmpty(bead.assignee)
	sessionID := sessionIDFromBead(bead, assignee)
	sessionName := sessionNameFromBead(bead, assignee, sessionID)
	if sessionID == "" && sessionName == "" {
		return RunSessionLink{}, false
	}
	rawLink := rawLinkFrom(sessionID, sessionName, assignee)
	link := resolveRunSessionLink(rawLink, ctx.sessionIndex)
	if !sessionIDRe.MatchString(link.SessionID) {
		return RunSessionLink{}, false
	}
	return link, true
}

// sessionIDFromBead resolves the supervisor session id from a bead. Port of TS
// sessionIdFromBead ("" mirrors undefined).
func sessionIDFromBead(bead runSnapshotBead, assignee string) string {
	rawSessionID := beadMeta(bead, "session_id")
	if rawSessionID == "" {
		rawSessionID = beadMeta(bead, beadmeta.SessionIDMetadataKey)
	}
	if rawSessionID == "" {
		rawSessionID = beadMeta(bead, beadmeta.SessionIDCamelMetadataKey)
	}
	if rawSessionID == "" {
		rawSessionID = assignee
	}
	if supervisor := supervisorSessionIDFrom(rawSessionID); supervisor != "" {
		return supervisor
	}
	return rawSessionID
}

// sessionNameFromBead resolves the session display name from a bead. Port of TS
// sessionNameFromBead.
func sessionNameFromBead(bead runSnapshotBead, assignee, sessionID string) string {
	if v := beadMeta(bead, "session_name"); v != "" {
		return v
	}
	if v := beadMeta(bead, beadmeta.SessionNameMetadataKey); v != "" {
		return v
	}
	if v := beadMeta(bead, beadmeta.SessionNameCamelMetadataKey); v != "" {
		return v
	}
	if assignee != "" {
		return assignee
	}
	return sessionID
}

func rawLinkFrom(sessionID, sessionName, assignee string) RunSessionLink {
	name := sessionName
	if name == "" {
		name = sessionID
	}
	id := sessionID
	if id == "" {
		id = sessionName
	}
	resolvedAssignee := assignee
	if resolvedAssignee == "" {
		resolvedAssignee = name
	}
	return RunSessionLink{SessionID: id, SessionName: name, Assignee: resolvedAssignee}
}

// supervisorSessionIDFrom extracts a supervisor session id from a raw handle.
// Port of TS supervisorSessionIdFrom ("" mirrors undefined).
func supervisorSessionIDFrom(value string) string {
	clean := nonEmpty(value)
	if clean == "" {
		return ""
	}
	if sessionIDRe.MatchString(clean) {
		return clean
	}
	m := supervisorSessionIDSuffixRe.FindStringSubmatch(clean)
	if m == nil {
		return ""
	}
	suffix := m[1]
	if suffix == "" || !sessionIDRe.MatchString(suffix) {
		return ""
	}
	return suffix
}

func resolveRunSessionLink(rawLink RunSessionLink, sessionIndex *runSessionIndex) RunSessionLink {
	if sessionIndex == nil {
		return rawLink
	}
	session, ok := resolveRunSessionSummary(rawLink, *sessionIndex)
	if !ok {
		return rawLink
	}
	return linkForSession(session, rawLink)
}

func resolveRunSessionSummary(link RunSessionLink, sessionIndex runSessionIndex) (DashboardSession, bool) {
	for _, candidate := range []string{link.SessionID, link.SessionName, link.Assignee} {
		key := nonEmpty(candidate)
		if key == "" {
			continue
		}
		if session, ok := sessionIndex.byID[key]; ok {
			return session, true
		}
		if session, ok := sessionIndex.byName[key]; ok {
			return session, true
		}
		if session, ok := uniquePreferredSession(sessionIndex.byTemplate[key]); ok {
			return session, true
		}
	}
	return DashboardSession{}, false
}

func linkForSession(session DashboardSession, rawLink RunSessionLink) RunSessionLink {
	// sessionName: nonEmpty(alias) ?? nonEmpty(title) ?? nonEmpty(session_name) ??
	// nonEmpty(template) ?? rawLink.sessionName. The `??` chain returns the first
	// trimmed-non-empty value, else rawLink.sessionName verbatim (not trimmed).
	sessionName := rawLink.SessionName
	for _, v := range []string{derefString(session.Alias), session.Title, session.SessionName, session.Template} {
		if t := nonEmpty(v); t != "" {
			sessionName = t
			break
		}
	}

	// assignee: rawLink.assignee || nonEmpty(template) || nonEmpty(alias) ||
	// nonEmpty(title) || nonEmpty(session_name) || session.id. The `||` chain
	// takes rawLink.assignee verbatim when non-empty (JS-truthy), then the first
	// trimmed-non-empty value, else session.id verbatim.
	assignee := session.ID
	switch {
	case rawLink.Assignee != "":
		assignee = rawLink.Assignee
	default:
		for _, v := range []string{session.Template, derefString(session.Alias), session.Title, session.SessionName} {
			if t := nonEmpty(v); t != "" {
				assignee = t
				break
			}
		}
	}

	return RunSessionLink{SessionID: session.ID, SessionName: sessionName, Assignee: assignee}
}

func uniquePreferredSession(sessions []DashboardSession) (DashboardSession, bool) {
	if len(sessions) == 0 {
		return DashboardSession{}, false
	}
	var active []DashboardSession
	for _, s := range sessions {
		if s.State == "active" || s.Running {
			active = append(active, s)
		}
	}
	if len(active) == 1 {
		return active[0], true
	}
	if len(sessions) == 1 {
		return sessions[0], true
	}
	return DashboardSession{}, false
}

func rememberSession(store map[string]DashboardSession, key string, session DashboardSession) {
	clean := nonEmpty(key)
	if clean == "" {
		return
	}
	if _, ok := store[clean]; ok {
		return
	}
	store[clean] = session
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
