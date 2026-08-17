package runproj

import (
	"regexp"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// sessionIDRe gates a value before it is fed to the supervisor session routes.
// Port of TS SESSION_ID_RE (session-id.ts) — lowercase-only, case-sensitive. It
// stays strict (gc/td/th or exactly-four-letter prefixes) on the NAME/assignee
// fallback path, where an id derived from an ambiguous match must not leak.
var sessionIDRe = regexp.MustCompile(`^(gc|td|th|[a-z]{4})-[a-z0-9-]{1,32}$`)

// sessionBeadIDRe validates a DURABLE session bead id — the value gc hook --claim
// stamps from GC_SESSION_ID (a real session bead id by provenance) and the value
// direct routing stamps as gc.session_id. It accepts a short lowercase store prefix
// (2-4 letters, covering city stores like mc-, ga-, gcy- that sessionIDRe rejects)
// followed by '-' and a lowercase-alnum/'-' body, while still rejecting empties,
// whitespace, uppercase, prefixless handles, and over-long pool-name prefixes
// (e.g. "polecat-…", "mystery-…"). It is applied ONLY to the provenance-trusted
// durable id, never to a name/assignee-derived value.
var sessionBeadIDRe = regexp.MustCompile(`^[a-z]{2,4}-[a-z0-9-]{1,40}$`)

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

// runSessionLinkFor resolves a bead to a session link, or (zero, false) when none
// is usable. Port of TS runSessionLinkFor, hardened for durable pool-step
// attribution.
//
// Resolution precedence:
//
//  1. Durable stamp (authoritative, INDEX-INDEPENDENT): when the step carries a
//     gc.session_id stamped by gc hook --claim (or direct routing), that value is
//     a real session BEAD id by provenance — it identifies the session that ran
//     the step even after that session CLOSES and drops out of the active-only
//     session index. It is trusted directly and never overridden by a name/assignee
//     index match, because pool slot names are deterministic and REUSED: a byName
//     hit on a recycled slot would mis-resolve a closed step to a DIFFERENT live
//     session (a wrong transcript/diff link — worse than no link). The id is
//     format-validated (sessionBeadIDRe) so garbage cannot leak.
//
//  2. Legacy / direct fallback (no durable stamp): resolve via the index and keep
//     the STRICT sessionIDRe gate on the result. Only an exact byID match on the
//     durable stamp is trusted unconditionally; a name/assignee/template match must
//     still pass the gate, so a recycled-slot byName collision yields no link rather
//     than a wrong one.
//
// Streamability is decided downstream from the step's own status
// (detail_instances.go), so a closed step's link is inherently non-streamable
// regardless of index membership.
func runSessionLinkFor(bead runSnapshotBead, status string, ctx runSessionLinkContext) (RunSessionLink, bool) {
	if status == "pending" || status == "ready" {
		return RunSessionLink{}, false
	}
	if stamped := stampedSessionID(bead); stamped != "" && sessionBeadIDRe.MatchString(stamped) {
		return linkForStampedSessionID(stamped, bead, ctx), true
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

// stampedSessionID returns the durable session bead id a step carries in metadata
// (gc.session_id / its legacy and camelCase aliases), stamped at claim time or by
// direct routing. Unlike sessionIDFromBead it does NOT fall back to the transient
// assignee, so an empty result cleanly means "no durable stamp" — the signal that
// the resolver must use the ambiguous, gated name fallback instead.
func stampedSessionID(bead runSnapshotBead) string {
	for _, key := range []string{"session_id", beadmeta.SessionIDMetadataKey, beadmeta.SessionIDCamelMetadataKey} {
		if v := beadMeta(bead, key); v != "" {
			return v
		}
	}
	return ""
}

// stampedSessionName returns the durable session display name a step carries in
// metadata (gc.session_name / aliases), or "" when absent.
func stampedSessionName(bead runSnapshotBead) string {
	for _, key := range []string{"session_name", beadmeta.SessionNameMetadataKey, beadmeta.SessionNameCamelMetadataKey} {
		if v := beadMeta(bead, key); v != "" {
			return v
		}
	}
	return ""
}

// linkForStampedSessionID builds a session link straight from a durable stamped
// session bead id, independent of the active session index. When that exact id is
// still live in the index we adopt its display fields; otherwise (the session has
// closed and left the active-only index) we keep the correct id and fall back to
// the step's own stamped display fields — so a closed step still resolves to the
// CORRECT session it ran on.
func linkForStampedSessionID(sessionID string, bead runSnapshotBead, ctx runSessionLinkContext) RunSessionLink {
	name := stampedSessionName(bead)
	assignee := nonEmpty(bead.assignee)
	if ctx.sessionIndex != nil {
		if session, ok := ctx.sessionIndex.byID[sessionID]; ok {
			return linkForSession(session, RunSessionLink{SessionID: sessionID, SessionName: name, Assignee: assignee})
		}
	}
	return rawLinkFrom(sessionID, name, assignee)
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

// resolveRunSessionLink resolves rawLink against the session index for the
// name/assignee fallback path, returning the enriched link on a match or rawLink
// unchanged (nil index / no match). The caller always re-applies the strict
// sessionIDRe gate, so an ambiguous match cannot leak an untrusted id.
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
