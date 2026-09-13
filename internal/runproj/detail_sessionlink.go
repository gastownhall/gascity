package runproj

import (
	"regexp"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// sessionIDRe gates a value before it is fed to the supervisor session routes.
// It accepts a short lowercase store prefix (2-4 letters: gc-, td-, th-, mc-,
// ga-, gcg-, and the per-deployment four-letter city codes) followed by '-' and
// a lowercase-alnum/'-' body long enough for the "session-<32 hex>" ids some
// stores mint, while still rejecting empties, whitespace, uppercase, prefixless
// handles, and over-long pool-name prefixes (e.g. "polecat-…", "mystery-…").
// The body bound of 40 is exactly len("session-") + 32 hex digits — the longest
// id shape any store mints today; a longer minted form would silently regress
// to a bare link, so widen the bound (and TestSessionIDGateAcceptsMintedShapes)
// together with the minting side. It is applied to BOTH the provenance-trusted
// durable stamp and the name/assignee-derived fallback: the fallback path
// additionally re-checks the resolved id after the index lookup so an ambiguous
// match cannot leak an untrusted id. (The former strict gc/td/th-or-four-letter
// alternation rejected this city's gcg-session-<32hex> ids on the assignee-only
// path — ga-3cs9p.) This is the only session-id gate: the dashboard's own TS
// copy was removed so a stricter client-side alternation cannot re-gate ids the
// supervisor accepts.
//
// A value matching this gate WHOLE is accepted as-is: supervisorSessionIDFrom
// returns it before it ever consults supervisorSessionIDSuffixRe, so this gate —
// not the suffix regex — decides every handle that matches end to end.
// "bd-x-gc-335812" and "qa-agent-1" are therefore taken whole rather than
// reduced to a trailing "gc-335812" or rejected outright. Extraction is the
// fallback for handles this gate REJECTS — "polecat-gc-333573", and the
// qualified-identity assignees whose "__"/"--" separator encoding
// (agent.SanitizeQualifiedNameForSession) leaves a non-hyphen byte after the
// prefix letters — not a minimizer applied to every value. That diverges
// deliberately from work-in-flight.ts, whose assignee parser tightens its own
// id body to [a-z0-9] so it binds to the minimal trailing handle: this gate
// must admit the internal hyphen of "session-<32 hex>", so it cannot also
// minimize. The cost is bounded — a value taken whole that names no session
// yields a bare link plus by-id reads the retired-session miss cache backs off
// exponentially (retiredMissTTL) — and the narrowings weighed in review do not
// remove these values: requiring a digit in the body still admits both examples
// above, and tightening the suffix regex never runs for them.
var sessionIDRe = regexp.MustCompile(`^[a-z]{2,4}-[a-z0-9-]{1,40}$`)

// supervisorSessionIDSuffixRe extracts a trailing supervisor id from a
// pool-qualified handle (polecat-gc-333573 → gc-333573). Same id alphabet as
// sessionIDRe. It is consulted ONLY after the whole value fails sessionIDRe (see
// supervisorSessionIDFrom), so it never narrows a handle the gate already
// accepted.
var supervisorSessionIDSuffixRe = regexp.MustCompile(`(?:^|[-_/])([a-z]{2,4}-[a-z0-9-]{1,40})$`)

// RunSessions is the request-time session enrichment for a run-detail build.
//
// Live is the city's OPEN session listing (the default /v0 sessions read): those
// sessions resolve by id, by name/alias/title, and by template. Retired holds
// sessions the caller resolved INDIVIDUALLY by durable id — the closed seats of a
// finished run that the open-only listing no longer carries. Retired sessions are
// indexed by id ONLY, never by name or template: pool slot names are
// deterministic and REUSED, so a name match onto a closed session could attribute
// a step to the wrong session (a wrong transcript link — worse than a bare one).
// The caller bounds Retired by the run's own link count; it is never a scan of
// the city's closed sessions.
type RunSessions struct {
	Live    []DashboardSession
	Retired []DashboardSession
}

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

// buildRunSessionIndex indexes the session enrichment for link resolution. Port
// of TS buildRunSessionIndex (first-write-wins for the id/name maps). Live
// sessions are indexed first and by every key; retired sessions are added
// afterwards and by id only (see RunSessions), so a live session always wins an
// id collision and a retired one is never reachable through a recyclable name.
func buildRunSessionIndex(sessions RunSessions) runSessionIndex {
	idx := runSessionIndex{
		byID:       make(map[string]DashboardSession),
		byName:     make(map[string]DashboardSession),
		byTemplate: make(map[string][]DashboardSession),
	}
	for _, session := range sessions.Live {
		rememberSession(idx.byID, session.ID, session)
		rememberSession(idx.byName, derefString(session.Alias), session)
		rememberSession(idx.byName, session.Title, session)
		rememberSession(idx.byName, session.SessionName, session)
		if template := nonEmpty(session.Template); template != "" {
			idx.byTemplate[template] = append(idx.byTemplate[template], session)
		}
	}
	for _, session := range sessions.Retired {
		rememberSession(idx.byID, session.ID, session)
	}
	return idx
}

// SessionIDsForSnapshot returns the durable session ids a run's steps reference —
// the stamped gc.session_id (and its legacy/camelCase aliases) or, absent a stamp,
// the supervisor id derived from the assignee — deduped in first-seen bead order.
// Steps that have not started (pending/ready) and values the link resolver would
// reject are excluded, so the list is the set of CANDIDATE ids — pre-index
// resolution — that a by-id session lookup may turn from a bare link into a
// named one. They are candidates, not resolved ids: on the assignee/name
// fallback path the resolver can overwrite the link's SessionID with the id of
// an index-matched session (linkForSession), so a derived id here may name no
// session at all. That costs one bounded, miss-cached by-id read; the rendered
// link still carries the matched session's own id. It is bounded by the run's
// own bead count; the dashboard BFF resolves these individually against the
// session-by-id read instead of scanning the city's closed sessions.
func SessionIDsForSnapshot(snap RunSnapshot) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, bead := range snap.raw.beads {
		id := sessionIDForLookup(bead)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// sessionIDForLookup mirrors runSessionLinkFor's id selection without an index:
// the durable stamp when it passes the gate, else the assignee/legacy-derived
// supervisor id (the same fall-through the link path takes for a stamp that
// fails the gate), gated by sessionIDRe. "" means the step yields no index-
// independent link either way; TestSessionIDForLookupMirrorsLinkFallback pins
// the two paths together. The result is a CANDIDATE id: on the fallback branch
// runSessionLinkFor may still replace it with an index-matched session's own id,
// so the two paths agree on what to look up, not always on what is rendered.
func sessionIDForLookup(bead runSnapshotBead) string {
	status := presentationStatus(bead)
	if status == "pending" || status == "ready" {
		return ""
	}
	if stamped := stampedSessionID(bead); stamped != "" && sessionIDRe.MatchString(stamped) {
		return stamped
	}
	id := sessionIDFromBead(bead, nonEmpty(bead.assignee))
	if !sessionIDRe.MatchString(id) {
		return ""
	}
	return id
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
//     format-validated (sessionIDRe) so garbage cannot leak.
//
//  2. Legacy / direct fallback (no durable stamp): resolve via the index and
//     re-apply the sessionIDRe gate on the RESULT. Only an exact byID match on the
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
	if stamped := stampedSessionID(bead); stamped != "" && sessionIDRe.MatchString(stamped) {
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
// session bead id, independent of the session index. When that exact id is in
// the index — live, or retired and resolved by id (RunSessions.Retired) — we adopt
// its display fields; otherwise we keep the correct id and fall back to the
// step's own stamped display fields, so a closed step still resolves to the
// CORRECT session it ran on even when nothing else is known about it.
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
// unchanged (nil index / no match). The caller always re-applies the sessionIDRe
// gate, so an ambiguous match cannot leak an untrusted id.
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
