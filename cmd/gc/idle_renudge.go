package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// idleRenudgeMinAge is the minimum time a session must have been alive
// (or quiet since its last re-nudge) before detectIdleReloadSurvivors
// will return it as a target. The same value is used as the cooldown
// between successive re-nudges, so a busy reconciler tick cannot spam
// the same session every few seconds.
//
// 5 minutes leaves plenty of room for the spawn-time nudge to land,
// for the LLM to issue its first tool call, and for the work_query
// loop to claim a bead — typical fresh-spawn timelines are well under
// a minute. Reload-survivor sessions sit at the prompt indefinitely,
// so a longer threshold here is fine and avoids re-nudge noise on
// healthy-but-quiet workers.
const idleRenudgeMinAge = 5 * time.Minute

// idleRenudgeLastAtKey is the session bead metadata field that records
// the most recent idle re-nudge timestamp (RFC3339). The detector
// treats this and last_woke_at as equivalent anchors when computing
// session age, so successive ticks cannot fire faster than
// idleRenudgeMinAge apart.
const idleRenudgeLastAtKey = "last_idle_renudge_at"

// idleRenudgeTarget identifies an alive pool session that should be
// re-nudged because it appears idle while routed pool work waits.
type idleRenudgeTarget struct {
	SessionID   string
	SessionName string
	Template    string
	NudgeText   string
}

// detectIdleReloadSurvivors returns the alive ephemeral pool sessions
// that should be re-nudged this tick.
//
// The reload-survivor failure mode (ga-2ph follow-up): a polecat
// session was alive before a prompt-template update, so its on-spawn
// nudge fired against the old prompt and the LLM produced
// "Session started. Ready..." text instead of running gc hook. The
// supervisor cannot tell from session liveness alone that the LLM is
// idle — the tmux session is up, scale_check sees a slot occupied,
// and no work-aware code path nudges it. Detection criteria:
//
//   - state=active (alive in metadata; healState keeps this honest)
//   - ephemeral pool session (not named/manual/dependency-only)
//   - not held, quarantined, draining, or pending-create
//   - the agent has a Nudge string configured
//   - routed pool work for the agent's template (workSet[template])
//   - no open/in_progress work bead assigned by ID or session_name
//   - age >= idleRenudgeMinAge from the more recent of last_woke_at
//     or last_idle_renudge_at (so the spawn-time nudge has had its
//     chance and successive re-nudges respect a cooldown)
//
// Returns nil when the workSet has no entries — the common case on a
// reconciler tick with an empty queue, where the detector does no
// per-session work at all.
func detectIdleReloadSurvivors(
	sessions []beads.Bead,
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	workSet map[string]bool,
	clk clock.Clock,
) []idleRenudgeTarget {
	if cfg == nil || clk == nil || len(sessions) == 0 || len(workSet) == 0 {
		return nil
	}
	now := clk.Now()

	var targets []idleRenudgeTarget
	for _, session := range sessions {
		template, nudgeText, ok := idleRenudgeSessionEligible(session, cfg, workSet, now)
		if !ok {
			continue
		}
		if sessionHasInProgressAssignment(session, assignedWorkBeads) {
			continue
		}
		name := strings.TrimSpace(session.Metadata["session_name"])
		if name == "" {
			continue
		}
		targets = append(targets, idleRenudgeTarget{
			SessionID:   session.ID,
			SessionName: name,
			Template:    template,
			NudgeText:   nudgeText,
		})
	}
	return targets
}

// idleRenudgeSessionEligible reports whether session passes every
// non-assignment-related precondition. Returns (template, nudgeText,
// true) when eligible so callers don't have to re-derive either.
func idleRenudgeSessionEligible(
	session beads.Bead,
	cfg *config.City,
	workSet map[string]bool,
	now time.Time,
) (string, string, bool) {
	if session.Status == "closed" {
		return "", "", false
	}
	if sessionMetadataState(session) != "active" {
		return "", "", false
	}
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) == "true" {
		return "", "", false
	}
	if isDrainedSessionBead(session) {
		return "", "", false
	}
	if isNamedSessionBead(session) || isManualSessionBead(session) {
		return "", "", false
	}
	if !isEphemeralSessionBead(session) {
		return "", "", false
	}
	if session.Metadata["wait_hold"] != "" {
		return "", "", false
	}
	if held := session.Metadata["held_until"]; held != "" {
		if t, err := time.Parse(time.RFC3339, held); err == nil && now.Before(t) {
			return "", "", false
		}
	}
	if q := session.Metadata["quarantined_until"]; q != "" {
		if t, err := time.Parse(time.RFC3339, q); err == nil && now.Before(t) {
			return "", "", false
		}
	}

	template := normalizedSessionTemplate(session, cfg)
	if template == "" || !workSet[template] {
		return "", "", false
	}
	agent := findAgentByTemplate(cfg, template)
	if agent == nil || strings.TrimSpace(agent.Nudge) == "" {
		return "", "", false
	}

	last := idleRenudgeLastActivity(session)
	if last.IsZero() {
		return "", "", false
	}
	if now.Sub(last) < idleRenudgeMinAge {
		return "", "", false
	}
	return template, agent.Nudge, true
}

// idleRenudgeLastActivity returns the most recent of last_woke_at and
// last_idle_renudge_at as the session's "age anchor." Either value
// alone is sufficient; both being zero means the session has never
// reported activity (e.g., a half-initialized bead) and is not a
// re-nudge candidate.
func idleRenudgeLastActivity(session beads.Bead) time.Time {
	var last time.Time
	if v := strings.TrimSpace(session.Metadata["last_woke_at"]); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(last) {
			last = t
		}
	}
	if v := strings.TrimSpace(session.Metadata[idleRenudgeLastAtKey]); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(last) {
			last = t
		}
	}
	return last
}

// sessionHasInProgressAssignment reports whether any open or
// in_progress work bead is assigned to the given session (by bead ID
// or session_name). Closed beads do not count: a polecat that just
// finished work and drained-acked has no active assignment.
func sessionHasInProgressAssignment(session beads.Bead, workBeads []beads.Bead) bool {
	name := strings.TrimSpace(session.Metadata["session_name"])
	for _, wb := range workBeads {
		if wb.Status != "open" && wb.Status != "in_progress" {
			continue
		}
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" {
			continue
		}
		if assignee == session.ID || (name != "" && assignee == name) {
			return true
		}
	}
	return false
}

// renudgeIdleSessions delivers the configured nudge text to each target
// and stamps idleRenudgeLastAtKey on the bead so the cooldown applies.
// A Nudge failure is logged and the metadata is intentionally left
// unchanged so the next reconciler tick retries — a missing tmux
// session, for example, will heal to state=asleep on the same tick and
// drop out of detector candidates organically.
func renudgeIdleSessions(
	ctx context.Context,
	targets []idleRenudgeTarget,
	sp runtime.Provider,
	store beads.Store,
	clk clock.Clock,
	stderr io.Writer,
) {
	if len(targets) == 0 || sp == nil || store == nil || clk == nil {
		return
	}
	nowStr := clk.Now().UTC().Format(time.RFC3339)
	for _, t := range targets {
		if ctx != nil && ctx.Err() != nil {
			return
		}
		if err := sp.Nudge(t.SessionName, runtime.TextContent(t.NudgeText)); err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "idle re-nudge: deliver %s (%s): %v\n", t.SessionName, t.Template, err) //nolint:errcheck // best-effort stderr
			}
			continue
		}
		if err := store.SetMetadata(t.SessionID, idleRenudgeLastAtKey, nowStr); err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "idle re-nudge: record %s on %s: %v\n", idleRenudgeLastAtKey, t.SessionID, err) //nolint:errcheck // best-effort stderr
			}
		}
	}
}
