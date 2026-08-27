package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// exactSessionDuplicateNamedWinner re-derives D-DUP's condition for one exact
// key from durable state and returns the row this key must be retired in favor
// of. It is both the seam's dispatch guard and the handler's own
// pre-mutation re-verification, so the two can never disagree about which row
// loses.
//
// The predicate is legacy's, verbatim: an open configured-named row,
// continuity-eligible, whose stored identity still resolves to a named-session
// spec, and which is NOT the winner of its identity's duplicate set under
// namedSessionWinsCanonicalRepairInfo. The cheap durable rungs run first, so the
// bounded sibling list below is paid only by admissions for configured named
// sessions that could plausibly be duplicates — the same footing as the
// per-session probe D-DEADLINE's guard pays (DETECTOR.md §2, declared reads).
//
// Every failure fails CLOSED (not a loser): a store blip must never archive a
// row and re-point its work.
func exactSessionDuplicateNamedWinner(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
) (sessionpkg.Info, bool) {
	// Revision 0 is the legacy-at-0 residual (DETECTOR.md §3b): a row no
	// conditional writer has ever fenced. Refuse it and let the first
	// unconditional write self-heal it.
	if params.Store == nil || params.Config == nil || response.Revision == 0 {
		return sessionpkg.Info{}, false
	}
	identity := exactSessionDuplicateNamedIdentity(params, info)
	if identity == "" {
		return sessionpkg.Info{}, false
	}
	spec, ok := findNamedSessionSpec(params.Config, params.CityName, identity)
	if !ok {
		return sessionpkg.Info{}, false
	}
	rows, err := sessionFrontDoor(params.Store).ListAllForReconcile(sessionpkg.ListAllOptions{})
	if err != nil {
		return sessionpkg.Info{}, false
	}
	siblings := make([]sessionpkg.Info, 0, 2)
	for i := range rows {
		if exactSessionDuplicateNamedIdentity(params, rows[i].Info) == identity {
			siblings = append(siblings, rows[i].Info)
		}
	}
	if len(siblings) < 2 {
		return sessionpkg.Info{}, false
	}
	// The sweep's pinned order (session name, then bead ID) seats the incumbent;
	// feeding the handler the same order keeps detector and handler answering
	// from identical inputs even though the winner rule is a total order.
	sort.SliceStable(siblings, func(i, j int) bool {
		a := strings.TrimSpace(siblings[i].SessionNameMetadata)
		b := strings.TrimSpace(siblings[j].SessionNameMetadata)
		if a != b {
			return a < b
		}
		return siblings[i].ID < siblings[j].ID
	})
	winner := detectorDuplicateWinner(siblings, spec.SessionName)
	if winner.ID == "" || winner.ID == info.ID {
		return sessionpkg.Info{}, false
	}
	return winner, true
}

// exactSessionDuplicateNamedIdentity is the handler-side spelling of
// detectorDuplicateNamedIdentity: same rungs, same order, over params instead of
// the sweep input.
func exactSessionDuplicateNamedIdentity(params exactSessionStartParams, info sessionpkg.Info) string {
	if info.Closed || !isNamedSessionInfo(info) || !sessionpkg.NamedSessionInfoContinuityEligible(info) {
		return ""
	}
	identity := namedSessionIdentityInfo(info)
	if identity == "" {
		return ""
	}
	if _, ok := findNamedSessionSpec(params.Config, params.CityName, identity); !ok {
		return ""
	}
	return identity
}

// exactSessionDuplicateNamedCandidate is the seam's guard: a predicate over the
// durable row, never over admission.Source.
func exactSessionDuplicateNamedCandidate(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
) bool {
	_, ok := exactSessionDuplicateNamedWinner(params, info, response)
	return ok
}

// reconcileExactSessionDuplicateNamedRetire retires ONE duplicate named session
// row by exact key. It adds no second retire implementation: it re-derives the
// winner, then hands exactly the (winner, loser) pair to the fleet pass's own
// retire body (retireDuplicateConfiguredNamedSessionRows, session_beads.go), so
// the stop-before-mutate, the front-door archive batch, and the work/wait/nudge
// re-point are the same bytes legacy writes. Narrowing the feed to two rows is
// what turns a fleet phase into one fenced effect per key.
//
// A stop failure inside that body is a refusal by construction — it archives
// nothing and re-points nothing — so the handler proves the effect landed by
// re-reading the row, and reports the refusal rather than claiming a retire that
// did not happen. The condition is level-triggered: the next sweep re-detects.
//
// It always answers exactSessionStartKeyedOwner: unlike the start and deadline
// families there is no auto-mode legacy handoff to fall back to, because the
// legacy yield here is the exclusion predicate, not an owner transfer. The owner
// return stays because it is the seam's contract shape.
func reconcileExactSessionDuplicateNamedRetire(
	admission sessionStartAdmission,
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) (exactSessionStartOwner, error) { //nolint:unparam // owner is the detector-family seam's contract shape; this family never hands back to legacy
	if clk == nil {
		clk = clock.Real{}
	}
	stderr := params.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	winner, ok := exactSessionDuplicateNamedWinner(params, info, response)
	if !ok {
		// Re-derived away between admission and dispatch. Release the key with
		// zero effect; the row is the authority, the detector's reason was a hint.
		return exactSessionStartKeyedOwner, nil
	}

	startedAt := time.Now()
	retireDuplicateConfiguredNamedSessionRows(
		params.CityPath, params.Store, params.RigStores, params.Provider, params.Config, params.CityName,
		[]sessionpkg.ReconcileSession{{Info: winner}, {Info: info, Revision: response.Revision}},
		nil, clk.Now().UTC(), stderr,
	)

	latest, _, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if readErr != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("confirming duplicate named session %q retired: %w", info.ID, readErr)
	}
	if !exactSessionDuplicateNamedRetired(latest) {
		recordExactSessionDuplicateNamedTrace(params, admission, info, winner, 0, false)
		return exactSessionStartKeyedOwner, fmt.Errorf(
			"retiring duplicate named session %q in favor of %q: runtime stop refused the mutation", info.ID, winner.ID)
	}
	recordExactSessionDuplicateNamedTrace(params, admission, info, winner, time.Since(startedAt), true)
	return exactSessionStartKeyedOwner, nil
}

// exactSessionDuplicateNamedRetired reads the retire's durable signature off the
// row: RetireNamedSessionPatch archives the state and frees the session name.
func exactSessionDuplicateNamedRetired(info sessionpkg.Info) bool {
	return strings.TrimSpace(info.MetadataState) == string(sessionpkg.BaseStateArchived) &&
		strings.TrimSpace(info.SessionNameMetadata) == ""
}

// recordExactSessionDuplicateNamedTrace fires the SAME legacy site the fleet
// phase fires, with effect_owner=keyed and the honest effect_applied, so the
// WD.15 parity join can separate the legacy, keyed, and detector-shadow
// populations on a shared cycle.
func recordExactSessionDuplicateNamedTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	winner sessionpkg.Info,
	duration time.Duration,
	applied bool,
) {
	if params.Trace == nil {
		return
	}
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_duplicate_named_retire", time.Now().UTC(), params.Config)
	if cycle == nil {
		return
	}
	template := normalizedSessionTemplateInfo(info, params.Config)
	outcome := TraceOutcomeApplied
	if !applied {
		outcome = TraceOutcomeSkipped
	}
	cycle.recordKeyedEffect(
		TraceSiteSessionReconcileHealRetire,
		detectorReasonDuplicateNamed,
		outcome,
		"exact_session_duplicate_named_retire",
		template,
		info.ID,
		info.SessionNameMetadata,
		duration,
		map[string]any{
			"admission":         string(admission.Source),
			"admission_version": admission.Version,
			"generation":        params.Generation,
			"instance_token":    info.InstanceToken,
			"winner_id":         winner.ID,
			"session_identity":  namedSessionIdentityInfo(info),
			"effect_owner":      detectorKeyedEffectOwner,
			"effect_applied":    applied,
		},
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact duplicate named retire trace: %v\n", err) //nolint:errcheck // tracing is observational
	}
}

// healExactSessionAdmissionTimers clears elapsed held_until / quarantined_until
// at wake admission and returns the row re-read at its post-heal revision.
//
// This is where expired-timer heal lives now (DETECTOR.md §3, D-DUP entry:
// "expired hold/quarantine timer heal does NOT get a detector"). The admission
// path already re-reads the authoritative row, so the clear costs one write on
// exactly the rows that have an elapsed timer and creates no second heal path.
// It reports false — and touches nothing — when no timer has elapsed, so an
// ordinary admission pays neither a write nor an extra read.
//
// A failed post-heal re-read returns a ZERO revision, which every downstream
// fence reads as "refuse": the clear already landed, so carrying the pre-heal
// revision forward would fence against a row that no longer exists at it.
func healExactSessionAdmissionTimers(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) (sessionpkg.Info, sessionpkg.PersistedResponse, bool) {
	if clk == nil {
		clk = clock.Real{}
	}
	if params.Store == nil || !exactSessionHasElapsedLifecycleTimer(info, clk.Now()) {
		return info, response, false
	}
	healed := healExpiredTimersInfo(info, sessionFrontDoor(params.Store), clk)
	latest, latestResponse, err := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if err != nil || latest.ID != info.ID {
		return healed, sessionpkg.PersistedResponse{}, true
	}
	return latest, latestResponse, true
}

// exactSessionHasElapsedLifecycleTimer mirrors healExpiredTimersInfo's own
// elapsed test exactly, so the gate can never claim a clear the fold would
// decline to make.
func exactSessionHasElapsedLifecycleTimer(info sessionpkg.Info, now time.Time) bool {
	return exactSessionLifecycleTimerElapsed(info.HeldUntil, now) ||
		exactSessionLifecycleTimerElapsed(info.QuarantinedUntil, now)
}

func exactSessionLifecycleTimerElapsed(raw string, now time.Time) bool {
	if raw == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, raw)
	return err == nil && !t.IsZero() && now.After(t)
}
