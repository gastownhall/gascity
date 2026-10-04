package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// sessionCloseIdentityKeys are the environment variables that carry the
// identities a running session is known by: the session bead id its claims
// record as assignee, its runtime session name, its alias, and the actor bd
// stamps on its writes.
var sessionCloseIdentityKeys = []string{"GC_SESSION_ID", "GC_SESSION_NAME", "GC_ALIAS", "BEADS_ACTOR"}

// closeActorForOwnClaim returns the actor a session's close of its own claimed
// work should run under, or "" to leave the actor alone.
//
// bd authorizes a close by comparing the bead's assignee with the actor as
// strings. A pool session claims work under its session bead id but acts under
// its session name (BEADS_ACTOR), so its close of a bead it holds is refused
// ("assignee is <bead id>, actor is <session name>; reclaim or use --force"),
// and workers learn to force every close. When every assigned target is held
// by one identity of this session, closing under that exact identity is the
// same principal speaking, and bd's check passes without --force. A bead held
// by anyone else keeps the session's own actor, so bd still refuses it.
func closeActorForOwnClaim(bdArgs []string, targets map[string]beads.Bead, getenv func(string) string) string {
	ids, isClose := workRecordCloseTargets(bdArgs)
	if !isClose {
		return ""
	}
	own := make(map[string]bool, len(sessionCloseIdentityKeys))
	for _, key := range sessionCloseIdentityKeys {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			own[v] = true
		}
	}
	if !own[strings.TrimSpace(getenv("GC_SESSION_ID"))] {
		return "" // not running as a session
	}
	actor := ""
	for _, id := range ids {
		bead, ok := targets[id]
		if !ok {
			return "" // unread target: leave bd's check to decide
		}
		assignee := strings.TrimSpace(bead.Assignee)
		if assignee == "" {
			continue // unassigned beads pass bd's check under any actor
		}
		if !own[assignee] || (actor != "" && actor != assignee) {
			return ""
		}
		actor = assignee
	}
	if actor == strings.TrimSpace(getenv("BEADS_ACTOR")) {
		return ""
	}
	return actor
}

// withEnvValue returns env with key set to value, replacing every prior entry.
func withEnvValue(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok && name == key {
			continue
		}
		out = append(out, entry)
	}
	return append(out, key+"="+value)
}
