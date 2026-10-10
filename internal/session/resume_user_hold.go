package session

import (
	"fmt"
	"strings"
	"time"
)

// This file is the interim consume of an operator's user hold by an
// operator's own resume (ResumeOperator: Attach, and Start, Submit or Send
// carrying it): a strict subset of CONTRACT v5.9 D8 rules 2-5. The runtime
// lease (L1b) fences it against the controller's kills; D8 B replaces it with
// the lease-fenced consume.

// resumeReplacing is the policy an interrupt's restart runs under for a
// caller that is not an operator's resume: the hold was decided before the
// interrupt's stop, so it starts without re-reading it (as ResumeOperator
// does), but it never consumes one.
const resumeReplacing ResumePolicy = -1

// restartPolicy is the policy an interrupt's restart runs under for a caller
// that submitted under policy.
func restartPolicy(policy ResumePolicy) ResumePolicy {
	if policy == ResumeOperator {
		return ResumeOperator
	}
	return resumeReplacing
}

// liveUserHoldPremise is userHoldPremise for a running row: only an operator's
// resume of an active or awake row consumes it, never a draining or
// stop-pending one, whose stop is already the controller's.
func liveUserHoldPremise(meta map[string]string, policy ResumePolicy, now time.Time) map[string]string {
	switch State(strings.TrimSpace(meta["state"])) {
	case StateActive, StateAwake:
	default:
		return nil
	}
	if policy != ResumeOperator {
		return nil
	}
	return userHoldPremise(meta, now)
}

// userHoldPremiseKeys are the facts an operator's resume read before Start.
// A write to any of them since (a newer suspend, kill, wait or quarantine, or
// another start) refuses the consume. state is left out: legacy's heal moves
// it under a dormant row.
var userHoldPremiseKeys = []string{
	"held_until", "sleep_intent", "quarantined_until", "wait_hold", "suspended_at", "generation", "instance_token",
}

// userHoldPremise is the premise of consuming meta's user hold (HoldUser),
// or nil when meta carries none.
func userHoldPremise(meta map[string]string, now time.Time) map[string]string {
	if Holds(meta, now).In&HoldUser == 0 {
		return nil
	}
	premise := make(map[string]string, len(userHoldPremiseKeys))
	for _, k := range userHoldPremiseKeys {
		premise[k] = meta[k]
	}
	return premise
}

// consumeUserHoldPatch records the resumed runtime and drops the hold it
// consumed, with the wake request it satisfies and the pending-create claim.
func consumeUserHoldPatch() MetadataPatch {
	patch := ClearWakeRequestPatch()
	patch["state"] = string(StateActive)
	patch["state_reason"] = "creation_complete"
	for _, k := range []string{"held_until", "sleep_intent", "sleep_reason", "slept_at", "suspended_at", "pending_create_claim", "pending_create_started_at"} {
		patch[k] = ""
	}
	return patch
}

// consumeUserHold is the confirmation of a resumed runtime whose row the
// operator held: one fenced write, decided from a fresh read, while the row is
// open and premise still holds. A refused write over a row already active,
// with no hold that blocks a consume (HoldSet.BlocksConsume), at the same
// generation and token is converged; otherwise it returns an
// ErrStateSync error, so the caller never stops the runtime it launched.
func (m *Manager) consumeUserHold(id string, premise map[string]string, now time.Time) error {
	converged := false
	ok, err := m.PersistedStore().UpdateMetadataFenced(id, 3, func(_ Info, p PersistedResponse) MetadataPatch {
		open := p.Status != "closed"
		state := State(strings.TrimSpace(p.Metadata["state"]))
		converged = open && (state == StateActive || state == StateAwake) && !Holds(p.Metadata, now).BlocksConsume() &&
			p.Metadata["generation"] == premise["generation"] && p.Metadata["instance_token"] == premise["instance_token"]
		for k, v := range premise {
			if p.Metadata[k] != v {
				return nil
			}
		}
		if !open {
			return nil
		}
		return consumeUserHoldPatch()
	})
	switch {
	case err != nil:
		return fmt.Errorf("%w: consuming the user hold: %w", ErrStateSync, err)
	case ok || converged:
		return nil
	}
	return fmt.Errorf("%w: the user hold on session %s changed during the resume; the runtime is left running", ErrStateSync, id)
}
