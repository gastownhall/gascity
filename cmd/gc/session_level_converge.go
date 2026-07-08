package main

import (
	"strings"
	"time"
)

// Level-triggered session convergence core (S19, approach b — Stage 1 slice).
//
// The reconciler's Phase 1 today applies session side effects (identity
// stamping, prompt priming, firstStart classification, rollback) edge-triggered
// and path-dependent: each arrival path (fresh spawn, adoption, resume, drift
// relaunch, rollback) stamps its own subset, so any new path silently ships a
// missing side effect (#3872 / #3849 / #2073). This file replaces the *decision*
// with a pure observe -> diff -> act core:
//
//   observe : build durableFacts (persisted bead metadata) + runtimeFacts
//             (observed, non-persisted signals) up front.
//   diff    : deriveConvergeActions turns (durable x runtime) into an ordered,
//             idempotent action list — the same every tick regardless of how the
//             session arrived.
//   act     : the reconciler executes the actions under the existing
//             alias/identifier locks and the worker boundary.
//
// This package decides WHICH actions are needed; it performs no I/O. That keeps
// identity heal, prompt delivery, firstStart, and rollback table-testable
// without a tmux or provider boot — the whole point of the level-triggered shape.
//
// Stage 1 amends the s19-b spike to the S19 spec §2 two-phase priming
// vocabulary: a write-ahead attempt marker plus an explicit confirmation, so a
// crash between the two never permanently loses the startup prompt (the s19-b
// spike's at-most-once flaw). Nothing reads the action list yet; it is exercised
// only by tests until Stage 3 wires the act loop.
//
// Naming note: this is unrelated to `gc converge` (convergence loops); this is
// per-tick session-lifecycle convergence.

// primeReattemptInterval bounds delivery-attempt backoff for the startup prompt:
// once a delivery has been declared (priming_attempted_at stamped) but not yet
// confirmed, no fresh attempt is emitted until this interval has elapsed. This
// is mechanical transport backoff, not judgment. Default is 2× the reconcile
// tick (patrol) interval, floored at 60s; the default patrol interval is 30s, so
// the floor governs.
const primeReattemptInterval = 60 * time.Second

// sessTranscriptState is a tri-state observation of whether the runtime
// transcript backing a session's resume key currently exists on disk. Unknown
// means the converger did not (or could not) probe — a caller that cannot probe
// must pass sessTranscriptUnknown so its decision matches the durable-only
// legacy signal exactly.
type sessTranscriptState int

const (
	// sessTranscriptUnknown means the transcript was not probed.
	sessTranscriptUnknown sessTranscriptState = iota
	// sessTranscriptPresent means the keyed transcript exists on disk.
	sessTranscriptPresent
	// sessTranscriptAbsent means the keyed transcript is known to be gone.
	sessTranscriptAbsent
)

// durableFacts are the persisted (session-bead metadata) signals the converger
// reads. Building these once, up front, is the "observe" half of the durable
// world; every field maps to a single bead-metadata key so there is one source
// of truth per fact rather than a precedence ladder. Time enters as a fact
// (now, primingAttemptedAt) so the core stays clock-free and table-testable.
type durableFacts struct {
	// primedAt mirrors "primed_at": non-empty means the startup prompt delivery
	// was durably CONFIRMED (stamped only after a delivery mechanism reported
	// success). It makes "live but never primed" detectable by any observer,
	// unlike the runtime-only GC_STARTUP_PROMPT_DELIVERED.
	primedAt string
	// primingAttemptedAt mirrors "priming_attempted_at": the write-ahead attempt
	// marker, stamped BEFORE any delivery I/O. Zero value means no attempt was
	// ever declared. Its age gates re-attempts via primeReattemptInterval.
	primingAttemptedAt time.Time
	// primedPromptHash mirrors "prompt_hash": the hash of the rendered prompt the
	// markers refer to. When it differs from currentPromptHash a stale attempt is
	// re-eligible (the template's prompt changed under the session).
	primedPromptHash string
	// currentPromptHash is the hash of the currently-rendered startup prompt. It
	// is a template-derived fact captured by the observe step, not persisted.
	currentPromptHash string
	// canonicalIdentity is the one canonical qualified instance name persisted on
	// the session bead. "" means no canonical identity record exists yet, so the
	// legacy read ladders would be consulted — the converger heals that by
	// stamping the record.
	canonicalIdentity string
	// promptConfigured is true when the resolved template carries a startup
	// prompt to deliver. When false there is nothing to prime and priming actions
	// are never emitted (P5).
	promptConfigured bool
	// absent means the durable world says this session should not be running (the
	// bead was closed or a pending create was rolled back). A lingering live
	// runtime under an absent durable intent is the #2073 pane-leak shape.
	absent bool
	// now is the tick's wall clock, captured once by the observe step so the core
	// reads no clock itself.
	now time.Time
}

// runtimeFacts are the observed, non-persisted signals the converger reads —
// the "observe" half of the runtime world. No I/O happens here; the caller has
// already probed.
type runtimeFacts struct {
	// observed reports whether the runtime was actually probed this tick. When
	// false, NO live-only or start/teardown action may be emitted (C5 observed
	// gating); only durable-only heals are permitted.
	observed bool
	// live is true when a matching runtime session is present and alive.
	live bool
	// primedEnv is true when the live runtime env carries
	// GC_STARTUP_PROMPT_DELIVERED — the prompt was delivered this runtime but may
	// not yet be durably recorded.
	primedEnv bool
}

// sessConvergeAction is one idempotent side effect the reconciler must apply to
// bring the runtime and durable worlds into agreement. Emitting an action is a
// pure decision; the reconciler performs the effect.
type sessConvergeAction int

const (
	// actionRollbackRuntimeToAbsent tears down a runtime that lingers under an
	// absent durable intent (rollback / closed bead), killing the pane leak. It
	// dominates every other action and is never emitted when the runtime was not
	// observed.
	actionRollbackRuntimeToAbsent sessConvergeAction = iota
	// actionStampCanonicalIdentity persists the canonical identity record when it
	// is absent, collapsing the legacy read ladders into a single healed field. It
	// is a durable-only heal, permitted even when the runtime was not observed.
	actionStampCanonicalIdentity
	// actionStampPrimedFromRuntime persists primed_at from an observed-live
	// runtime that already carries GC_STARTUP_PROMPT_DELIVERED but lacks the
	// durable confirmation. It heals the durable world with NO nudge — the prompt
	// already landed this incarnation.
	actionStampPrimedFromRuntime
	// actionAttemptPrime is the two-phase delivery: stamp the write-ahead attempt
	// marker, deliver via worker.Handle.Nudge, then stamp the confirmation. It is
	// emitted only for an observed-live, prompt-bearing, unconfirmed session whose
	// last attempt is stale enough to retry (attemptEligible).
	actionAttemptPrime
)

// deriveFirstStart decides whether a launch should be treated as a first start
// (fresh session via --session-id) rather than a resume (--resume), as a pure
// function of the durable start hash and the observed transcript state.
//
// Legacy behavior was durable-only: firstStart == (started_config_hash == "").
// Stage 1 preserves that EXACTLY — the comparison is the raw `== ""` the legacy
// site used, with no TrimSpace (M10 / D4). Passing sessTranscriptUnknown
// reproduces the legacy signal byte-for-byte, so existing call sites stay
// behavior-preserving until they opt in to probing the transcript. The TrimSpace
// variant is adopted deliberately at Stage 4 with its own test.
//
// S19 intent: firstStart is "no durable hash AND/OR no runtime transcript". A
// present hash whose transcript is known-absent is the #3849 crash-loop shape —
// the bead claims a prior start but the conversation a resume would target is
// gone, so --resume hard-fails and the pane dies every tick. Classifying it as a
// first start makes the launch use --session-id and breaks the loop by
// construction. That branch is inert until a real probe is passed (Stage 4).
func deriveFirstStart(startedConfigHash string, transcript sessTranscriptState) bool {
	if startedConfigHash == "" {
		return true
	}
	if transcript == sessTranscriptAbsent {
		return true
	}
	return false
}

// attemptEligible reports whether a fresh startup-prompt delivery attempt may be
// declared for a session that is live, prompt-bearing, and not yet confirmed
// (primed_at == ""). It is pure delivery backoff (transport, not judgment):
//
//   - no attempt has ever been declared, or
//   - the last attempt is at least primeReattemptInterval old (bounded retry),
//     or
//   - the markers refer to a different prompt than the one currently rendered
//     (the template's prompt changed, so the old attempt is stale).
func attemptEligible(d durableFacts) bool {
	if d.primingAttemptedAt.IsZero() {
		return true
	}
	if d.now.Sub(d.primingAttemptedAt) >= primeReattemptInterval {
		return true
	}
	if d.primedPromptHash != d.currentPromptHash {
		return true
	}
	return false
}

// deriveConvergeActions is the pure diff: given the durable and runtime facts
// for one session, it returns the ordered, idempotent action list needed to
// converge them. The order is load-bearing:
//
//   - Rollback dominates: an absent durable intent with an observed-live runtime
//     means the only correct action is to converge the runtime to absent;
//     identity/priming on a doomed runtime would be wasted work. Never emitted
//     when the runtime was not observed (C5).
//   - Otherwise, heal the canonical identity record first (so downstream readers
//     see one agreed identity), then handle priming.
//   - Priming is two-phase (§2). When the runtime already carries the delivered
//     env marker we only heal the durable world (actionStampPrimedFromRuntime,
//     no nudge). Otherwise we attempt a fresh delivery — but only when the last
//     attempt is stale enough (attemptEligible), so a crash between the
//     write-ahead attempt stamp and the confirmation re-delivers at most one
//     duplicate per interval instead of nudging every tick forever.
//
// The function is total and side-effect-free, so (durable x runtime) -> actions
// is fully table-testable. Re-running it after its actions have durably landed
// yields the empty list (C2 idempotence).
func deriveConvergeActions(d durableFacts, r runtimeFacts) []sessConvergeAction {
	if d.absent {
		// Rollback dominates, but only on an observed-live runtime: an unobserved
		// runtime tells us nothing to tear down (C5).
		if r.observed && r.live {
			return []sessConvergeAction{actionRollbackRuntimeToAbsent}
		}
		return nil
	}

	var actions []sessConvergeAction

	// Identity heal is a durable-only fact; permitted even when the runtime was
	// not observed this tick.
	if strings.TrimSpace(d.canonicalIdentity) == "" {
		actions = append(actions, actionStampCanonicalIdentity)
	}

	// Priming requires an observed-live runtime, a prompt-bearing template, and no
	// durable confirmation yet (P5 gates on promptConfigured).
	if r.observed && r.live && d.promptConfigured && d.primedAt == "" {
		switch {
		case r.primedEnv:
			// The runtime already got the prompt this incarnation; persist the
			// confirmation without re-nudging.
			actions = append(actions, actionStampPrimedFromRuntime)
		case attemptEligible(d):
			actions = append(actions, actionAttemptPrime)
		}
	}

	return actions
}
