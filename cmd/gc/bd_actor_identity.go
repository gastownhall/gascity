package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// projectBdActorForMutation keeps the scalar BEADS_ACTOR interface compatible
// with the session identity set that gc uses for ownership decisions. bd can
// compare one actor string at a time; when a command targets work already
// assigned to this session, pass bd the exact stored spelling instead of the
// runtime spelling that led gc to the same session.
//
// Fresh --claim operations use the same actor selector as hook claims. For
// close, heartbeat, and ordinary updates, projection is deliberately narrower:
// every known non-empty target assignee must be one of this session's
// identities, and all owned targets must have one exact spelling. Otherwise
// the ambient actor is preserved so bd's own ownership guard rejects a foreign
// or mixed-owner mutation.
func projectBdActorForMutation(args []string, ambientActor, claimActor string, identities []string, targets map[string]beads.Bead) string {
	ambientActor = strings.TrimSpace(ambientActor)
	claimActor = strings.TrimSpace(claimActor)
	if bdMutationIsClaim(args) {
		if ids, ok, ambiguous := bdMutationWriteIDs(args); ok && !ambiguous && len(ids) == 1 {
			if bead, present := targets[ids[0]]; present {
				assignee := strings.TrimSpace(bead.Assignee)
				if hookClaimHasIdentity(assignee, identities) {
					return assignee
				}
			}
		}
		if claimActor != "" {
			return claimActor
		}
		return ambientActor
	}

	ids, ok, ambiguous := bdMutationWriteIDs(args)
	if !ok || ambiguous || len(ids) == 0 || len(targets) == 0 {
		return ambientActor
	}

	ownedSpelling := ""
	for _, id := range ids {
		bead, present := targets[id]
		if !present {
			return ambientActor
		}
		assignee := strings.TrimSpace(bead.Assignee)
		if assignee == "" {
			continue
		}
		if !hookClaimHasIdentity(assignee, identities) {
			return ambientActor
		}
		if ownedSpelling == "" {
			ownedSpelling = assignee
			continue
		}
		if ownedSpelling != assignee {
			return ambientActor
		}
	}
	if ownedSpelling != "" {
		return ownedSpelling
	}
	return ambientActor
}

// projectBdActorInEnv applies projectBdActorForMutation to the complete
// environment that gc is about to hand to the bd subprocess. The session
// identity candidates intentionally mirror hookClaim's candidate set, while
// the claim actor retains alias-first behavior for named holders and uses the
// durable session bead ID for unaliased pool holders.
func projectBdActorInEnv(args []string, env []string, targets map[string]beads.Bead) []string {
	ambientActor := bdEnvValue(env, "BEADS_ACTOR")
	identities := bdSessionIdentityCandidates(env)
	claimActor := bdSessionClaimActor(env)
	projected := projectBdActorForMutation(args, ambientActor, claimActor, identities, targets)
	if projected == ambientActor || projected == "" {
		return env
	}
	return bdEnvWithValue(env, "BEADS_ACTOR", projected)
}

func bdSessionClaimActor(env []string) string {
	return firstNonEmptyHookValue(
		bdEnvValue(env, "GC_ALIAS"),
		bdEnvValue(env, "GC_SESSION_ID"),
		bdEnvValue(env, "BEADS_ACTOR"),
		bdEnvValue(env, "GC_AGENT"),
		bdEnvValue(env, "GC_SESSION_NAME"),
	)
}

func bdSessionIdentityCandidates(env []string) []string {
	return hookClaimIdentityCandidates(
		bdEnvValue(env, "GC_SESSION_ID"),
		bdEnvValue(env, "GC_SESSION_NAME"),
		bdEnvValue(env, "GC_ALIAS"),
		bdEnvValue(env, "GC_AGENT"),
		bdEnvValue(env, "BEADS_ACTOR"),
	)
}

func bdEnvValue(env []string, key string) string {
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == key {
			return value
		}
	}
	return ""
}

func bdEnvWithValue(env []string, key, value string) []string {
	out := append([]string(nil), env...)
	for i, entry := range out {
		name, _, ok := strings.Cut(entry, "=")
		if ok && name == key {
			out[i] = key + "=" + value
			return out
		}
	}
	return append(out, key+"="+value)
}
