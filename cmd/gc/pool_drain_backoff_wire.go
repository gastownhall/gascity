package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// sessionHadClaims reports whether any work bead is attributed to this
// session — measured by Assignee matching the session's identity. Closed
// beads count: if a session ever owned work, it is not a zero-claim drain
// even if the work has since wrapped up.
//
// Returns true on store error to fail safe — the detector should not fire
// on transient query failures.
func sessionHadClaims(store beads.Store, session beads.Bead) bool {
	if store == nil {
		return true
	}
	candidates := sessionClaimIdentities(session)
	if len(candidates) == 0 {
		return true
	}
	for _, identity := range candidates {
		beadList, err := store.List(beads.ListQuery{
			Assignee:      identity,
			IncludeClosed: true,
		})
		if err != nil {
			return true
		}
		for _, b := range beadList {
			if b.Type == sessionBeadType {
				continue
			}
			return true
		}
	}
	return false
}

// sessionClaimIdentities returns the set of identity strings that may
// appear in a work bead's Assignee field for this session. Workers may
// claim using GC_SESSION_NAME, GC_AGENT, or a configured named identity;
// returning all of them keeps the lookup robust to that variation.
func sessionClaimIdentities(session beads.Bead) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	add(session.Metadata["session_name"])
	add(session.Metadata["agent_name"])
	add(session.Metadata["alias"])
	add(session.Metadata["configured_named_identity"])
	return out
}

// poolHasLiveClaim reports whether any work bead routed to this pool is
// currently held by a worker — in_progress with non-empty Assignee. This
// is the Q6 rate-condition input: while progress is in flight somewhere
// in the pool, in-flight zero-claim drain-acks are treated as race-loser
// noise and the circuit-breaker does not fire.
//
// We check live in-progress state rather than a recency window because
// the bead model has no UpdatedAt. If the pool finished a burst and is
// now idle, scale_check should return 0 demand anyway — when it doesn't,
// the detector is correct to throttle.
//
// Returns true on store error to fail safe.
func poolHasLiveClaim(store beads.Store, template string) bool {
	if store == nil || template == "" {
		return true
	}
	beadList, err := store.List(beads.ListQuery{
		Status:   "in_progress",
		Metadata: map[string]string{"gc.routed_to": template},
	})
	if err != nil {
		return true
	}
	for _, b := range beadList {
		if b.Type == sessionBeadType {
			continue
		}
		if strings.TrimSpace(b.Assignee) == "" {
			continue
		}
		return true
	}
	return false
}

// recordPoolDrainAck wires a completed drain into the detector. It is the
// onComplete callback installed on drainTracker. Only pool-managed
// ephemeral sessions feed the detector — named/manual sessions have
// different lifecycle semantics and are not part of the spawn-storm
// pathology.
func recordPoolDrainAck(detector *poolDrainBackoff, store beads.Store, cfg *config.City, session beads.Bead) {
	if detector == nil {
		return
	}
	if !isEphemeralSessionBead(session) || isManualSessionBead(session) || isNamedSessionBead(session) {
		return
	}
	template := normalizedSessionTemplate(session, cfg)
	if template == "" {
		return
	}
	hadClaims := sessionHadClaims(store, session)
	detector.RecordDrainAck(template, hadClaims)
}

// applyPoolDrainBackoff suppresses scale_check counts for pools that the
// detector has flagged. Mutates scaleCheckCounts in place. The Q6
// rate-condition (any in-progress pool-routed claim) is checked per pool
// against the live store before suppression is applied.
//
// Called from the demand-snapshot path after scale_check runs and before
// pool desired states are computed.
func applyPoolDrainBackoff(
	detector *poolDrainBackoff,
	store beads.Store,
	scaleCheckCounts map[string]int,
) {
	if detector == nil || len(scaleCheckCounts) == 0 {
		return
	}
	for template, count := range scaleCheckCounts {
		if count <= 0 {
			continue
		}
		hasClaim := poolHasLiveClaim(store, template)
		suppress, _ := detector.Evaluate(template, hasClaim)
		if suppress {
			scaleCheckCounts[template] = 0
		}
	}
}
