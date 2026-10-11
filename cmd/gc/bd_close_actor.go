package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

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
//
// The session's own identities are only its session bead id (GC_SESSION_ID)
// and the actor its bd child runs under. The session name and alias are not:
// for tmux_alias pools and legacy rows they are a shared chair a successor
// session takes over (#6324), so a claim recorded under one may be a
// predecessor's, and closing it still needs --force.
//
// getenv reads GC_SESSION_ID; effectiveActor is the BEADS_ACTOR the bd child
// will actually run under (its command env, which can differ from the process
// env), so both the identity set and the "already the actor" short-circuit
// judge what bd sees.
func closeActorForOwnClaim(bdArgs []string, targets map[string]beads.Bead, getenv func(string) string, effectiveActor string) string {
	ids, isClose := workRecordCloseTargets(bdArgs)
	if !isClose {
		return ""
	}
	sessionID := strings.TrimSpace(getenv("GC_SESSION_ID"))
	if sessionID == "" {
		return "" // not running as a session
	}
	effectiveActor = strings.TrimSpace(effectiveActor)
	own := map[string]bool{sessionID: true}
	if effectiveActor != "" {
		own[effectiveActor] = true
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
	if actor == effectiveActor {
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
