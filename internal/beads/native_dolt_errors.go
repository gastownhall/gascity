package beads

import (
	"errors"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	beadslib "github.com/steveyegge/beads"
)

// Typed classification of a native read failure, ahead of the text table.
//
// # Why a table at all
//
// The read path's original question was binary: "is this the managed-Dolt
// hard-kill/rebind class, so reconnect-and-retry?" It answered by substring
// match over the error's text (nativeDoltTransientReadErrorSignatures), which is
// the right instrument for exactly one thing — a driver that reports the same
// physical condition with a different string on every release.
//
// It is the wrong instrument for everything else, and the proxied-native lane
// makes that concrete. Three of the failures a read over bd's proxy can take are
// facts a retry cannot move (the database is gone, the credentials are refused,
// the linked library's own breaker is open), and one of them — a serialization
// conflict wrapped in ErrCommitIndeterminate — is a shape where retrying is
// itself the hazard. A substring table cannot express any of those, because
// every one of them is identified by a SENTINEL or an error NUMBER that also
// appears inside errors the table already claims.
//
// So the classifier runs first, in a fixed order, and the text table survives as
// the backstop it always was.
//
// # The order, and why each rung is where it is
//
//  1. beadslib.ErrCommitIndeterminate (beads@v1.3.0 beads.go:206). A write whose
//     outcome is unknown must never be replayed automatically. PR2's native lane
//     serves reads only, so this rung is unreachable today — and it is FIRST
//     anyway, because the shape that makes it dangerous is an indeterminate
//     commit that ALSO carries a retryable signature (a 1213 deadlock under a
//     commit whose result never came back). Any later placement would let rung 2
//     claim it and replay a write that may have landed. PR3 inherits the order
//     rather than having to discover it.
//
//  2. A Dolt/MySQL serialization conflict (isNativeDoltSerializationConflict).
//     Known not to have committed, and the existing write path already replays
//     it; a read that hits one is retryable for the same reason. PROXIED LANE
//     ONLY — see "What the classifier does NOT decide".
//
//  3. beadslib.ErrCircuitOpen (beads.go:205). The library refused BEFORE the
//     wire, so there is nothing to reconnect: reconnecting would re-open a pool
//     against an endpoint the breaker has not re-armed for. The answer is to
//     wait the breaker's cooldown inside the read's own budget and ask again.
//     This rung exists because such an error usually has no transient signature
//     at all, so the text table returned it to the caller as if the endpoint had
//     said something about itself. PROXIED LANE ONLY — see "What the classifier
//     does NOT decide".
//
//  4. MySQL 1049 (unknown database) → database_gone, terminal. The proxy served
//     and answered that the database this scope names is not there. No retry, no
//     reconnect: a fresh pool would ask the same question.
//
//  5. MySQL 1045 (access denied) → access_denied, terminal. Same shape, about
//     credentials.
//
//  6. proxyendpoint.IsConnectionLevel (probe.go:356). The sentinel-and-type half
//     of "the endpoint was disturbed": io.EOF, ErrInvalidConn, ECONNRESET,
//     EPIPE, ECONNREFUSED, EHOSTUNREACH, ENETUNREACH, any net.Error or
//     *net.OpError — and, because IsIndeterminate runs first inside it, never a
//     timeout or a cancellation. Reconnect is the right response, which is what
//     the text table was reaching for.
//
//  7. The text table (isNativeDoltTransientReadError), unchanged, as the
//     backstop for a driver string none of the above catches.
//
// Rung 6 is the one small, deliberate widening of what the DIRECT native lane
// treats as transient: a bare io.EOF and the two unreachable-host errno values
// are sentinel-typed connection failures the substring table did not name. Every
// member added is a connection that demonstrably failed, which is precisely what
// the reconnect hook exists for. Rungs 2 and 3 are NOT widenings of that lane —
// they are gated on it; see the lane note below.
//
// # What the classifier does NOT decide
//
// It never decides whether a verdict is produced. classifyNativeDoltReadError
// names the endpoint fact; only a handle opened for the proxied lane turns that
// name into a *ProxiedVerdictError (see NativeDoltStore.proxiedReadVerdict). On
// a direct or hosted handle rungs 4 and 5 are still TERMINAL — which is exactly
// what happens today, since the text table matches neither, so "terminal" and
// "unclassified" are the same immediate return — and the error the caller
// receives is byte-identical to the one it receives now.
//
// # The lane gate, and why rungs 2 and 3 carry one (council B-F1 / C-F1)
//
// An earlier version of this comment claimed rung 6 was the ONLY widening of
// the direct lane. That was false, and the two counterexamples were live
// behavior changes on every existing hosted/managed-Dolt city — the lane this
// PR promises not to touch:
//
//   - Rung 3. On main, beadslib.ErrCircuitOpen's text
//     ("dolt circuit breaker is open: server appears down, failing fast") matched
//     none of the nine transient substrings, so the read returned it on the FIRST
//     pass, immediately. Classified as nativeReadCircuitOpen it slept a 5s
//     cooldown and asked again inside a 90s budget — ~18 passes, ~90 seconds of
//     hang per read — and then returned a DIFFERENT error, wrapped in
//     "native Dolt read retry budget exhausted".
//   - Rung 2. On main, isNativeDoltSerializationConflict was consulted on the
//     WRITE path only, and none of its strings is in the transient table, so an
//     Error 1213 on a read returned immediately. Classified as
//     nativeReadTransient every pass calls reconnect() — the injected hook,
//     which on a hosted city re-resolves the managed port with recovery enabled
//     and can restart the Dolt server — roughly 450 times inside one read's
//     budget. Reconnecting is also the wrong remedy: a 40001 says nothing about
//     the connection, and the library has already spent its own serialization
//     retry before gc sees it.
//
// The `reopen == nil` escape in withReadRetry protects neither: every
// production direct/hosted open installs a hook (cmd/gc/main.go,
// cmd/gc/api_state.go, internal/storebinding/beadsworkspace/engine.go), so it
// protects only bare test handles.
//
// So both rungs are gated on the LANE, exactly as rungs 4/5's verdict already
// is. On the direct lane the two errors fall through to the text table, match
// nothing, classify as nativeReadUnclassified and are returned verbatim on the
// first pass — the behavior main has. Rung 6 remains the one deliberate
// widening, and now that claim is true.
type nativeReadDisposition int

const (
	// nativeReadUnclassified means no rung matched: return the error as-is,
	// which is the read path's pre-existing default for anything that is not
	// transient.
	nativeReadUnclassified nativeReadDisposition = iota
	// nativeReadNonReplayable means the operation must not be re-run even
	// though it failed, because it may have taken effect.
	nativeReadNonReplayable
	// nativeReadTransient means reconnect and retry: the failure is about the
	// connection or a lost race, not about the data.
	nativeReadTransient
	// nativeReadCircuitOpen means wait the breaker's cooldown and retry the
	// SAME handle. Reconnecting is pointless: the refusal never reached a
	// socket.
	nativeReadCircuitOpen
	// nativeReadTerminal means stop: the endpoint answered with a fact about
	// the database or the credentials that a retry cannot change.
	nativeReadTerminal
)

// nativeReadCircuitCooldown is how long a read waits for the linked library's
// breaker to re-arm before asking again.
//
// Five seconds is chosen against the proxied read budget rather than against the
// breaker's own internals (which this package cannot see): a 10s proxied budget
// affords exactly one cooldown plus a retry, so a breaker that re-arms gets one
// free pass and one that does not costs the caller its budget and demotes. The
// wait is always clipped to the remaining budget, so this value can never extend
// a read.
//
// It is a var rather than a const for one reason: a test proving that the retry
// AFTER the cooldown succeeds would otherwise have to sit out five real seconds
// per run. Nothing in production writes it.
var nativeReadCircuitCooldown = 5 * time.Second

// nativeReadClass is one classification: what to do, and — for a terminal
// endpoint fact — which proxied verdict names it.
type nativeReadClass struct {
	disposition nativeReadDisposition
	// verdict names the endpoint fact for a terminal or circuit-open class. It
	// is a NAME, not a decision: a direct-lane handle discards it.
	verdict ProxiedVerdict
	// cooldown is the wait before the next pass, for nativeReadCircuitOpen.
	cooldown time.Duration
}

// MySQL error numbers the proxy's own answer can carry. They are matched on
// text because that is how the driver surfaces them through beadslib's wrapping
// (there is no exported typed error for either at the pinned version), and the
// match is on the full "Error 1049" token so an unrelated number containing the
// digits cannot claim it.
const (
	mysqlErrUnknownDatabase = "error 1049"
	mysqlErrAccessDenied    = "error 1045"
)

// nativeReadLane is which lane a classification is being made for.
//
// It is a named type rather than a bare bool so a call site reads as a lane
// rather than as an unexplained true, and so a caller cannot pass some other
// flag where the lane was meant.
type nativeReadLane bool

const (
	// directNativeLane is a handle against a database gc configured — a direct
	// or hosted managed-Dolt city. The rollout flag is off for it and its read
	// path must stay byte-identical to the one it has today.
	directNativeLane nativeReadLane = false
	// proxiedNativeLane is a handle against a database bd's proxy owns.
	proxiedNativeLane nativeReadLane = true
)

// readLane reports which lane this handle's reads are classified for. It is the
// same latch proxiedReadVerdict consults, read through one accessor so the two
// halves of "flag off is inert" cannot drift apart.
func (s *NativeDoltStore) readLane() nativeReadLane {
	if s != nil && s.proxiedReadVerdicts {
		return proxiedNativeLane
	}
	return directNativeLane
}

// classifyNativeDoltReadError classifies err for the native read path on lane.
// See the package-level commentary above for the order, the reason for each
// rung, and why two of them are lane-gated.
func classifyNativeDoltReadError(err error, lane nativeReadLane) nativeReadClass {
	if err == nil {
		return nativeReadClass{disposition: nativeReadUnclassified}
	}

	// 1. An indeterminate commit is never replayed, whatever else it looks like.
	// Both lanes: replaying a write whose outcome nobody knows is not a proxied
	// hazard, it is a hazard.
	if errors.Is(err, beadslib.ErrCommitIndeterminate) {
		return nativeReadClass{disposition: nativeReadNonReplayable, verdict: ProxiedVerdictWriteIndeterminate}
	}

	// 2. A lost serialization race committed nothing and is safe to repeat.
	// PROXIED LANE ONLY: on the direct lane this falls through to the text
	// table, matches nothing and returns on the first pass, which is what main
	// does. See the lane note above.
	if lane == proxiedNativeLane && isNativeDoltSerializationConflict(err) {
		return nativeReadClass{disposition: nativeReadTransient}
	}

	// 3. The breaker refused before the wire; there is no connection to remake.
	// PROXIED LANE ONLY, for the same reason: on a direct handle main returned
	// the breaker's error immediately and must keep doing so.
	if lane == proxiedNativeLane && errors.Is(err, beadslib.ErrCircuitOpen) {
		return nativeReadClass{
			disposition: nativeReadCircuitOpen,
			verdict:     ProxiedVerdictCircuitOpen,
			cooldown:    nativeReadCircuitCooldown,
		}
	}

	// 4/5. The endpoint answered about the database or the credentials.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, mysqlErrUnknownDatabase):
		return nativeReadClass{disposition: nativeReadTerminal, verdict: ProxiedVerdictDatabaseGone}
	case strings.Contains(msg, mysqlErrAccessDenied):
		return nativeReadClass{disposition: nativeReadTerminal, verdict: ProxiedVerdictAccessDenied}
	}

	// 6. The sentinel-and-type half of "the endpoint was disturbed".
	if proxyendpoint.IsConnectionLevel(err) {
		return nativeReadClass{disposition: nativeReadTransient}
	}

	// 7. The text table, unchanged, as the backstop.
	if isNativeDoltTransientReadError(err) {
		return nativeReadClass{disposition: nativeReadTransient}
	}
	return nativeReadClass{disposition: nativeReadUnclassified}
}

// proxiedReadVerdict turns a classified endpoint fact into the typed refusal the
// ProxiedStore wrapper demotes on — and into NOTHING on a direct or hosted
// handle.
//
// This is the single place the lanes diverge, and it is a deliberate narrowing
// of the change P2-08 makes: the classification table above is shared, so a
// direct native store gains the correct terminality for a 1049 (it stops instead
// of falling through the text table to the same immediate return), while the
// error its caller receives stays exactly the error it receives today. A verdict
// string on a direct scope would be a message about a proxy that is not there.
func (s *NativeDoltStore) proxiedReadVerdict(verdict ProxiedVerdict, cause error) error {
	if s == nil || !s.proxiedReadVerdicts || verdict == ProxiedVerdictNone {
		return cause
	}
	return NewProxiedVerdictError(verdict, "native read over bd's proxy", cause)
}

// proxiedReadBudgetVerdict is the read-path half of the trap the plan's §1 names:
// withReadRetry wraps an exhausted budget in nativeReadRetryBudgetError, which is
// an UNTYPED error, so a wrapper that demotes on errors.As(*ProxiedVerdictError)
// would sit on a dead handle forever while every read spent the whole budget.
//
// A budget that ran out says nothing about the endpoint, so the verdict is
// budget_exhausted and it is explicitly NON-TERMINAL: this open stands down, the
// next one re-admits. The original budget error is preserved as the cause, so
// every existing test and log that matches on its text still matches.
func (s *NativeDoltStore) proxiedReadBudgetVerdict(cause error) error {
	if s == nil || !s.proxiedReadVerdicts {
		return cause
	}
	return NewNonTerminalProxiedVerdictError(ProxiedVerdictBudgetExhausted,
		"a native read over bd's proxy spent its whole budget", cause)
}
