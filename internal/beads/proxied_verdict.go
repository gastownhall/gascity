package beads

import (
	"errors"
	"fmt"
	"strings"
)

// ProxiedVerdict names WHY gc did not (or may no longer) serve reads natively
// over bd's proxy data port.
//
// It is the vocabulary of the proxied-native lane, and it is deliberately a
// closed set of strings rather than free text: the verdict rides on the store
// open diagnostic and out through `gc doctor --json`, so an operator reading a
// fallback has a value to match on, and the acceptance matrix has something to
// assert that is not message prose.
//
// Every verdict except ProxiedVerdictWriteIndeterminate is produced by PR2's
// read path. That one is reserved: it is the shape a PR3 write would need when
// bd's child exits without telling gc whether its commit landed, and naming it
// here fixes the terminality table before there is code to argue with.
type ProxiedVerdict string

const (
	// ProxiedVerdictNone is the zero value: no verdict, the native lane holds.
	ProxiedVerdictNone ProxiedVerdict = ""

	// ProxiedVerdictSchemaSkew reports that the database's migration cursors
	// are not exactly the ones this binary's linked beads library pins. The
	// error carries which lane (main/ignored) and which direction
	// (ahead/behind), because the two directions are different hazards: BEHIND
	// is a database the library would migrate on open, AHEAD is one it would
	// issue old-shape SQL against.
	ProxiedVerdictSchemaSkew ProxiedVerdict = "schema_skew"

	// ProxiedVerdictProxyGone reports that the ownership record names a proxy
	// that is no longer running (or the record has been removed). bd owns the
	// lifecycle, so the recovery is one provider `probe` and a re-admission,
	// never a spawn.
	ProxiedVerdictProxyGone ProxiedVerdict = "proxy_gone"

	// ProxiedVerdictDraining reports that the proxy is shutting down or
	// refusing new connections while the same generation is still live. A
	// long-lived open waits it out; a one-shot takes BdStore for this command.
	ProxiedVerdictDraining ProxiedVerdict = "draining"

	// ProxiedVerdictBackendUnreachable reports that the endpoint accepted the
	// connection but never completed the MySQL greeting, so nothing is known
	// about the database behind it yet.
	ProxiedVerdictBackendUnreachable ProxiedVerdict = "backend_unreachable"

	// ProxiedVerdictProxyZombie reports a listening socket whose backend stays
	// unreachable across the whole escalation ladder — one probe, one recover
	// per generation, and still no greeting. gc stops asking.
	ProxiedVerdictProxyZombie ProxiedVerdict = "proxy_zombie"

	// ProxiedVerdictDatabaseGone reports MySQL 1049: the proxy serves, but the
	// database the scope names is not there.
	ProxiedVerdictDatabaseGone ProxiedVerdict = "database_gone"

	// ProxiedVerdictCircuitOpen reports that the linked library's own circuit
	// breaker is open, so the connection is refused before it reaches the wire.
	ProxiedVerdictCircuitOpen ProxiedVerdict = "circuit_open"

	// ProxiedVerdictNotOurs reports an ownership record that fails validation
	// against the scope gc resolved — a foreign root_id, a copied proxy.pid.
	// It is decided from the record alone: no dial is ever spent on it.
	ProxiedVerdictNotOurs ProxiedVerdict = "not_ours"

	// ProxiedVerdictNoOwnershipRecord reports that the scope has no readable
	// ownership record in the "ready" state.
	ProxiedVerdictNoOwnershipRecord ProxiedVerdict = "no_ownership_record"

	// ProxiedVerdictLegacySchema reports a proxy record written by a bd whose
	// record schema predates the fields admission reads.
	ProxiedVerdictLegacySchema ProxiedVerdict = "legacy_schema"

	// ProxiedVerdictIdlePolicyFinite reports the deliberate PR2 deviation: a
	// scope whose proxy has a FINITE idle timeout does not get a long-lived
	// native handle, because gc cannot hold one across an idle expiry it does
	// not own. Such a scope keeps BdStore for its controller store. See the
	// PR2 plan Q1 — this is doctor-visible on purpose, not silent.
	ProxiedVerdictIdlePolicyFinite ProxiedVerdict = "idle_policy_finite"

	// ProxiedVerdictPrefixMismatch reports that the bd leaf's configured id
	// prefix and the database's own issue_prefix row disagree, so the two
	// leaves of a split store would not agree on which beads are foreign.
	ProxiedVerdictPrefixMismatch ProxiedVerdict = "prefix_mismatch"

	// ProxiedVerdictAccessDenied reports MySQL 1045: the proxy answered and
	// rejected the credentials gc projected.
	ProxiedVerdictAccessDenied ProxiedVerdict = "access_denied"

	// ProxiedVerdictBudgetExhausted reports that admission or a read spent its
	// whole wall-clock budget without reaching a decision. It says nothing
	// about the endpoint, so it is never terminal.
	ProxiedVerdictBudgetExhausted ProxiedVerdict = "budget_exhausted"

	// ProxiedVerdictSchemaUnverified reports a probe that answered "served"
	// without having evaluated the ignored lane's cursor reality (council pr2
	// D-F11). The gate needs that reality to know which cursor the linked
	// library will act on, and an unevaluated one is not evidence, so it is
	// refused rather than believed. It is not terminal: it describes the
	// session that produced the result, not the database, so the open takes the
	// bd front door and a later open asks again. In production only the probe
	// session marks a reality evaluated, so this verdict names a Session
	// implementation that skipped the check.
	ProxiedVerdictSchemaUnverified ProxiedVerdict = "schema_unverified"

	// ProxiedVerdictHeadMoved reports that the database changed across gc's
	// own library open: the HEAD hash the admitting probe session read is not
	// the one a re-read sees once the open has returned — or, on the
	// dolt_ignore'd plane HEAD cannot see, the ignored lane's effective cursor
	// is no longer the one admission found (council pr2 E-S4). The detail
	// says which.
	//
	// PR2's native lane serves reads only, so an open that moves HEAD may be
	// gc writing to bd's database — the hazard the schema gate exists for, in
	// the shapes that gate cannot see. It is not terminal, because gc cannot
	// tell a commit ITS open minted from one another bd client made in the same
	// window, and another process's ordinary write must not permanently demote
	// this scope. It is the one verdict logged at WARN, at every site that
	// meets it — the factory, the read path's reopen and the guard's recovery
	// (proxied_incident_log.go): it is an incident, not an expected refusal.
	// See ProxiedOpenUnmoved.
	ProxiedVerdictHeadMoved ProxiedVerdict = "head_moved"

	// ProxiedVerdictWriteIndeterminate is RESERVED and never produced in PR2.
	// PR2's native lane serves reads only, so there is no write whose outcome
	// could be unknown. It is declared now so the terminality table is complete
	// before PR3 inherits it: an indeterminate write is the one verdict that
	// must never be retried automatically.
	ProxiedVerdictWriteIndeterminate ProxiedVerdict = "write_indeterminate"
)

// The two lanes and two directions a schema_skew verdict can name.
const (
	ProxiedSkewLaneMain    = "main"
	ProxiedSkewLaneIgnored = "ignored"
	ProxiedSkewDirAhead    = "ahead"
	ProxiedSkewDirBehind   = "behind"
)

// String renders the verdict for a message or a JSON field.
func (v ProxiedVerdict) String() string { return string(v) }

// Terminal reports the default terminality of a verdict: whether a store that
// took this verdict should stop asking for this open's lifetime.
//
// "Terminal" here is scoped to ONE store handle, not to the scope or the
// process. A terminal verdict demotes a handle to BdStore one-way — the wrapper
// never promotes — but the next open re-runs admission from scratch, so fixing
// the cause (restoring the schema, restarting the proxy) is a fresh open away.
//
// The split is by whether re-asking could plausibly answer differently WITHOUT
// somebody changing something:
//
//   - Not terminal: the endpoint is in motion (proxy_gone, draining,
//     backend_unreachable, circuit_open), gc simply ran out of clock
//     (budget_exhausted), gc saw something it cannot attribute to itself
//     (head_moved — any bd client may commit inside the same window), or the
//     evidence was never gathered (schema_unverified — a fact about the
//     session, not the database).
//     Retrying inside the escalation ladder is the point.
//   - Terminal: a fact about the database, the record, or the policy that a
//     retry cannot move (schema_skew, not_ours, legacy_schema, database_gone,
//     access_denied, prefix_mismatch, idle_policy_finite, no_ownership_record),
//     or a state gc has already escalated to exhaustion (proxy_zombie), or a
//     write whose outcome is unknown (write_indeterminate — the one case where
//     automatic retry is itself the hazard).
func (v ProxiedVerdict) Terminal() bool {
	switch v {
	case ProxiedVerdictNone,
		ProxiedVerdictProxyGone,
		ProxiedVerdictDraining,
		ProxiedVerdictBackendUnreachable,
		ProxiedVerdictCircuitOpen,
		ProxiedVerdictBudgetExhausted,
		ProxiedVerdictHeadMoved,
		ProxiedVerdictSchemaUnverified:
		return false
	default:
		return true
	}
}

// ProxiedVerdictError is the typed refusal of the proxied-native lane.
//
// It is what admission, the read path and the guard tick all return, and the
// whole lane is built on the promise that it survives wrapping: the native read
// path wraps a failed read's error in reconnect and budget context with %w
// before the wrapper sees it, so a verdict that could only be recovered by
// string matching would be lost exactly where it decides whether to demote.
// Hence Unwrap plus an Is that matches on the verdict.
type ProxiedVerdictError struct {
	// Verdict is the closed-set reason.
	Verdict ProxiedVerdict
	// Lane and Dir qualify ProxiedVerdictSchemaSkew and are empty otherwise:
	// which migration lane drifted, and in which direction the DATABASE sits
	// relative to this binary's pinned library.
	Lane string
	Dir  string
	// Detail is operator-facing context — the port, the generation, the
	// cursor pair. It is never parsed.
	Detail string
	// Err is the underlying cause, if any.
	Err error

	// terminal overrides the verdict's default terminality table when non-nil.
	// The one production override is admission's bounded drain expiring: that
	// returns draining NON-terminal so the caller demotes for this open only.
	terminal *bool
}

// NewProxiedVerdictError builds a verdict error whose terminality comes from
// the verdict table.
func NewProxiedVerdictError(verdict ProxiedVerdict, detail string, err error) *ProxiedVerdictError {
	return &ProxiedVerdictError{Verdict: verdict, Detail: detail, Err: err}
}

// NewNonTerminalProxiedVerdictError builds a verdict error that is explicitly
// NOT terminal regardless of the table. It exists for the one shape the table
// cannot express: a verdict whose default is terminal but which this particular
// caller reached by running out of clock rather than by learning a fact.
func NewNonTerminalProxiedVerdictError(verdict ProxiedVerdict, detail string, err error) *ProxiedVerdictError {
	notTerminal := false
	return &ProxiedVerdictError{Verdict: verdict, Detail: detail, Err: err, terminal: &notTerminal}
}

// NewSchemaSkewVerdictError builds the schema_skew verdict with its lane and
// direction, which are the only two facts a reader needs to decide whether the
// database or the binary is the thing that moved.
func NewSchemaSkewVerdictError(lane, dir, detail string) *ProxiedVerdictError {
	return &ProxiedVerdictError{Verdict: ProxiedVerdictSchemaSkew, Lane: lane, Dir: dir, Detail: detail}
}

// Terminal reports whether this refusal ends the native lane for the handle
// that took it. It is the verdict table unless the constructor overrode it.
func (e *ProxiedVerdictError) Terminal() bool {
	if e == nil {
		return false
	}
	if e.terminal != nil {
		return *e.terminal
	}
	return e.Verdict.Terminal()
}

// Error renders the refusal in the diagnostic grammar the rest of the package
// uses: the machine-matchable verdict first, then its qualifiers, then prose.
func (e *ProxiedVerdictError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString("proxied native refused: verdict=")
	b.WriteString(string(e.Verdict))
	if e.Lane != "" {
		b.WriteString(" lane=")
		b.WriteString(e.Lane)
	}
	if e.Dir != "" {
		b.WriteString(" dir=")
		b.WriteString(e.Dir)
	}
	if !e.Terminal() {
		b.WriteString(" terminal=false")
	}
	if e.Detail != "" {
		fmt.Fprintf(&b, " detail=%q", e.Detail)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the cause so errors.Is/As reach past the verdict.
func (e *ProxiedVerdictError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is lets a caller write errors.Is(err, &ProxiedVerdictError{Verdict: v}) to
// ask "is this that verdict", without reaching for errors.As and a nil check.
// A target with the empty verdict matches any verdict — that is the "is this a
// proxied refusal at all" question.
func (e *ProxiedVerdictError) Is(target error) bool {
	other, ok := target.(*ProxiedVerdictError)
	if !ok || e == nil || other == nil {
		return false
	}
	if other.Verdict == ProxiedVerdictNone {
		return true
	}
	if other.Verdict != e.Verdict {
		return false
	}
	if other.Lane != "" && other.Lane != e.Lane {
		return false
	}
	if other.Dir != "" && other.Dir != e.Dir {
		return false
	}
	return true
}

// ProxiedVerdictOf extracts the verdict error from anywhere in err's chain.
//
// This is the accessor the read path and the wrapper use, and it is the reason
// the type carries Unwrap: withReadRetry wraps a failed read twice with %w
// before returning it, so a caller that type-asserted the top-level error would
// see a *fmt.wrapError and demote nothing.
func ProxiedVerdictOf(err error) (*ProxiedVerdictError, bool) {
	var verdictErr *ProxiedVerdictError
	if errors.As(err, &verdictErr) {
		return verdictErr, true
	}
	return nil, false
}
