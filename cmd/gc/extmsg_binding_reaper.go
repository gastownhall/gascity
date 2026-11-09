package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
)

// reapStaleExtmsgBindings reconciles external-message conversation bindings
// against live session identity on each reconciler tick. A binding stores the
// session bead ID it was created against; when that session crashes and
// respawns under the same name it gets a fresh bead ID, leaving the binding
// pointing at a dead session so inbound triage silently drops and a fresh bind
// is rejected as a conflict. The reaper re-points bindings at the respawned
// session and clears bindings whose session is gone.
//
// It runs after session beads have been synced for the tick so a respawned
// session's replacement bead is already visible. Errors are logged and
// swallowed so a binding-store hiccup never stalls the reconciler loop.
//
// The bindings are read and written in messaging, the city's messaging-class
// store; session identity and liveness are read from sessions.
func reapStaleExtmsgBindings(ctx context.Context, messaging beads.MailStore, sessions beads.SessionStore, now time.Time, stderr io.Writer) {
	if messaging == (beads.MailStore{}) || sessions == (beads.SessionStore{}) {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	stats, err := extmsg.ReapStaleBindings(ctx, messaging, sessions, now)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: reaping stale extmsg bindings: %v\n", err) //nolint:errcheck
		return
	}
	if stats.Reassigned > 0 || stats.Cleared > 0 {
		fmt.Fprintf(stderr, "session reconciler: extmsg bindings reaped (reassigned=%d cleared=%d scanned=%d)\n", //nolint:errcheck
			stats.Reassigned, stats.Cleared, stats.Scanned)
	}
}

// reapStaleExtmsgParticipants reconciles external-message group participants
// against live session identity on each reconciler tick — the participant-side
// companion to reapStaleExtmsgBindings. Group-participant routing self-heals at
// read time, but the group-owned transcript membership (keyed by session ID)
// does not, and a binding-less group participant whose session respawns is
// reached by no other backstop, so without this sweep its membership would stay
// stranded on the retired session bead. It runs on the same tick and after
// session beads have been synced. Errors are logged and swallowed so a
// participant-store hiccup never stalls the reconciler loop. Like
// reapStaleExtmsgBindings it reads participants from messaging and session
// identity from sessions.
func reapStaleExtmsgParticipants(ctx context.Context, messaging beads.MailStore, sessions beads.SessionStore, stderr io.Writer) {
	if messaging == (beads.MailStore{}) || sessions == (beads.SessionStore{}) {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	stats, err := extmsg.ReapStaleParticipants(ctx, messaging, sessions)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: reaping stale extmsg participants: %v\n", err) //nolint:errcheck
		return
	}
	if stats.Reassigned > 0 {
		fmt.Fprintf(stderr, "session reconciler: extmsg participants reaped (reassigned=%d scanned=%d)\n", //nolint:errcheck
			stats.Reassigned, stats.Scanned)
	}
}

// extmsgStoreOverSessions is the sessions store as the messaging store, for
// the session-retirement paths that still read and write extmsg's
// (messaging-class) bindings and participants through the sessions store they
// hold: one store on a single-store city, and one binding on the split city
// the routes serve, which relocates every class together. Two classes on two
// stores part them, as they did the controller's reapers before those took
// the city's messaging handle, so each use is a typed-store follow-up
// (mc-b4jn5.3) to route through that handle instead.
func extmsgStoreOverSessions(sessions beads.SessionStore) beads.MailStore {
	return beads.MailStore{Store: sessions}
}
