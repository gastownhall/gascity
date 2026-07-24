package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Inert-recovery pacing (ga-qox / incident ci-emg). Deliberately unhurried: the
// recovery action is a continuation nudge into the SAME session, so a little
// latency is fine and keeps the lane far from anything that reads as churn.
const (
	// inertRecoveryQuietGrace is how long a session must be quiet (no new pane
	// activity) before it is inspected. It excludes actively-producing sessions
	// from the peek and gives a self-healing CLI a moment to recover on its own.
	inertRecoveryQuietGrace = 20 * time.Second
	// inertRecoveryGrace is the observe-before-first-nudge window measured from
	// the first sighting of a failure episode.
	inertRecoveryGrace = 60 * time.Second
	// inertRecoveryBackoff is the wait between continuation nudges.
	inertRecoveryBackoff = 2 * time.Minute
	// inertRecoveryMaxAttempts caps continuation nudges per episode, after which
	// the lane gives up quietly (manual re-nudge remains the escape hatch) so
	// retries can never storm.
	inertRecoveryMaxAttempts = 3
	// inertRecoveryPeekLines bounds the pane capture the classifier inspects.
	inertRecoveryPeekLines = 40
)

// inertRecoveryContinuationFallback is the continuation nudge used when the
// session's agent has no configured Nudge. It re-engages the agent in place —
// preserving the conversation and its work context — without naming any role.
const inertRecoveryContinuationFallback = "Your previous turn ended on a provider transport error before the work was finished, and the connection is back. Re-check your hook for assigned work and continue where you left off."

// recoverInertSessions is a reconcile-tick backstop that rescues sessions left
// alive but inert after a provider transport failure (ga-qox / incident ci-emg):
// a DNS/WebSocket/HTTPS/stream drop can abort an interactive turn and return the
// CLI to its idle prompt with the process still alive, so the reconciler keeps
// reporting the session awake and its durable work — and any critical always-on
// role — silently stalls until a human intervenes.
//
// It is provider-neutral and role-neutral: eligibility is "the orchestrator
// wants this session running" (desiredNames), not any role name; the failure is
// recognized from provider error strings on the pane, not a provider name in Go.
// Attachment is deliberately NOT an exemption — the incident's crashed session
// was attached, and the human was the one waiting on the dead turn.
//
// It is churn-free and storm-proof by construction:
//   - The recovery action is a continuation NUDGE, never a restart.
//   - The observe→nudge→backoff→give-up state machine is persisted on the
//     session bead, so a controller restart cannot replay it.
//   - Peeks are bounded by a quiet gate and an in-memory activity checkpoint, so
//     an ordinary completed turn triggers at most one inspection.
//   - Nudges are capped per episode; the count is persisted before delivery, so
//     a provider error still advances the backoff and a metadata error sends no
//     unaccounted nudge.
func recoverInertSessions(
	sp runtime.Provider,
	cfg *config.City,
	sessStore beads.SessionStore,
	sessionBeads []beads.Bead,
	desiredNames map[string]bool,
	checkpoint map[string]time.Time,
	now time.Time,
	stdout io.Writer,
) {
	if sp == nil || cfg == nil || sessStore.Store == nil || checkpoint == nil {
		return // hot reconcile path: never panic on a half-built dependency
	}

	live := make(map[string]bool, len(sessionBeads))
	for i := range sessionBeads {
		s := &sessionBeads[i]
		name := strings.TrimSpace(s.Metadata["session_name"])
		if name == "" {
			continue
		}
		live[name] = true

		// Eligibility: only rescue a session the orchestrator wants running.
		// Assigned work is a sufficient reason to be desired but not necessary —
		// an always-on named session with no assignee bead is still desired.
		if !desiredNames[name] || !sp.IsRunning(name) {
			// Death or removal from desired state ends the episode. Clear the
			// durable marker now so a later process using the same session bead
			// starts with a fresh attempt budget, without needing a pane read.
			clearInertRecoveryMarker(sessStore, s, stdout)
			delete(checkpoint, name)
			continue
		}

		la, err := sp.GetLastActivity(name)
		if err != nil || la.IsZero() {
			continue // provider does not report activity: not the interactive incident
		}
		if now.Sub(la) < inertRecoveryQuietGrace {
			continue // still producing output; let it settle (and self-recover)
		}

		markedFingerprint := strings.TrimSpace(s.Metadata[sessionpkg.InertRecoveryFingerprintKey])
		marked := markedFingerprint != ""
		attempts := atoiOr0(s.Metadata[sessionpkg.InertRecoveryAttemptsKey])
		lastActionAt := parseRFC3339OrZero(s.Metadata[sessionpkg.InertRecoveryAtKey])
		// Activity checkpoint: one completed turn → at most one inspection. Skip
		// re-peeking unchanged ordinary output. A marked episode re-inspects only
		// when its grace/backoff deadline is due; an exhausted episode waits for
		// new activity. Any activity change bypasses the checkpoint so recovery
		// can be detected and the marker cleared.
		if checkpoint[name].Equal(la) && !inertRecoveryReinspectionDue(marked, attempts, lastActionAt, now) {
			continue
		}

		pane, err := sp.Peek(name, inertRecoveryPeekLines)
		if err != nil {
			continue
		}
		checkpoint[name] = la
		recoverable, fingerprint := runtime.ClassifyInertTransportFailure(pane)

		facts := sessionpkg.InertRecoveryFacts{
			Alive:             true,
			Eligible:          true,
			TransportFailure:  recoverable,
			Fingerprint:       fingerprint,
			MarkedFingerprint: markedFingerprint,
			Attempts:          attempts,
			LastActionAt:      lastActionAt,
			Now:               now,
			Grace:             inertRecoveryGrace,
			Backoff:           inertRecoveryBackoff,
			MaxAttempts:       inertRecoveryMaxAttempts,
		}

		switch sessionpkg.DecideInertRecovery(facts) {
		case sessionpkg.RecoverObserve:
			if !applyInertRecoveryMarker(sessStore, s, sessionpkg.InertRecoveryObservePatch(fingerprint, now), stdout) {
				// The activity checkpoint must not hide an episode whose durable
				// observation marker failed. Reinspect the same pane next tick.
				delete(checkpoint, name)
			}
		case sessionpkg.RecoverNudge:
			// The marker is the storm-prevention boundary: persist the attempt and
			// cooldown BEFORE delivery. If persistence is unavailable, send no
			// unaccounted nudge that a controller restart or next tick could replay.
			if !applyInertRecoveryMarker(sessStore, s, sessionpkg.InertRecoveryNudgePatch(fingerprint, attempts+1, now), stdout) {
				continue
			}
			nudgeErr := sp.Nudge(name, runtime.TextContent(inertContinuationNudge(cfg, *s)))
			if nudgeErr != nil {
				fmt.Fprintf(stdout, "inert-recovery: nudge %s failed after transport failure %q (attempt %d/%d): %v\n", name, fingerprint, attempts+1, inertRecoveryMaxAttempts, nudgeErr) //nolint:errcheck // best-effort telemetry
			} else {
				fmt.Fprintf(stdout, "inert-recovery: nudged %s to resume after transport failure %q (attempt %d/%d)\n", name, fingerprint, attempts+1, inertRecoveryMaxAttempts) //nolint:errcheck // best-effort telemetry
			}
		case sessionpkg.RecoverClear:
			clearInertRecoveryMarker(sessStore, s, stdout)
		case sessionpkg.RecoverNone, sessionpkg.RecoverWait, sessionpkg.RecoverExhausted:
			// Hold: no store write, no action.
		}
	}

	// Bound the in-memory checkpoint to sessions still present this tick.
	for name := range checkpoint {
		if !live[name] {
			delete(checkpoint, name)
		}
	}
}

func inertRecoveryReinspectionDue(marked bool, attempts int, lastActionAt, now time.Time) bool {
	if !marked || attempts >= inertRecoveryMaxAttempts {
		return false
	}
	wait := inertRecoveryBackoff
	if attempts <= 0 {
		wait = inertRecoveryGrace
	}
	return lastActionAt.IsZero() || now.Sub(lastActionAt) >= wait
}

// inertContinuationNudge resolves the continuation nudge for a session: the
// agent's own configured Nudge (which re-engages it with its work) when set,
// otherwise a generic, role-neutral resume message.
func inertContinuationNudge(cfg *config.City, session beads.Bead) string {
	if n := claimNudgeFor(cfg, session); n != "" {
		return n
	}
	return inertRecoveryContinuationFallback
}

// applyInertRecoveryMarker persists the inert-recovery state machine onto the
// session bead and mirrors it into the in-memory snapshot so the rest of this
// tick reads the just-written values. It reports whether the durable write
// succeeded; callers must not perform an action whose retry bound depends on a
// marker that was not stored.
func applyInertRecoveryMarker(sessStore beads.SessionStore, s *beads.Bead, patch sessionpkg.MetadataPatch, stdout io.Writer) bool {
	kvs := map[string]string(patch)
	if err := sessStore.SetMetadataBatch(s.ID, kvs); err != nil {
		fmt.Fprintf(stdout, "inert-recovery: marking %s failed: %v\n", s.ID, err) //nolint:errcheck // best-effort
		return false
	}
	if s.Metadata == nil {
		s.Metadata = make(map[string]string, len(kvs))
	}
	for k, v := range kvs {
		s.Metadata[k] = v
	}
	return true
}

// clearInertRecoveryMarker wipes the marker once a session is no longer inert on
// a transport failure, so the next episode starts a fresh grace clock. It is a
// no-op (no store write) when there is nothing to clear, so steady-state ticks
// stay silent.
func clearInertRecoveryMarker(sessStore beads.SessionStore, s *beads.Bead, stdout io.Writer) {
	if s.Metadata[sessionpkg.InertRecoveryFingerprintKey] == "" &&
		s.Metadata[sessionpkg.InertRecoveryAttemptsKey] == "" &&
		s.Metadata[sessionpkg.InertRecoveryAtKey] == "" {
		return
	}
	kvs := map[string]string(sessionpkg.InertRecoveryClearPatch())
	if err := sessStore.SetMetadataBatch(s.ID, kvs); err != nil {
		fmt.Fprintf(stdout, "inert-recovery: clearing %s failed: %v\n", s.ID, err) //nolint:errcheck // best-effort
		return
	}
	for k := range kvs {
		delete(s.Metadata, k)
	}
}
