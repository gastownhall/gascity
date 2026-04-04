package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// idleNudger detects sessions that are alive and idle at the CLI prompt
// but have open or in_progress work assigned. This catches sessions stuck
// after a Claude Code "Interrupted" bug — they land at the ❯ prompt with
// work still assigned and sit there forever.
//
// On each tick it checks alive sessions. If a session has been idle at the
// prompt for longer than the grace period AND has assigned work, it nudges
// the session to resume.
//
// It does NOT drain idle sessions without work — that's handled by the
// existing idle sleep timers (sleep_after_idle, idle_timeout) via
// ComputeAwakeSet.
type idleNudger struct {
	firstIdle map[string]time.Time // session name → first observed idle
	grace     time.Duration        // how long to wait before nudging
}

const defaultIdleNudgeGrace = 2 * time.Minute

func newIdleNudger() *idleNudger {
	return &idleNudger{
		firstIdle: make(map[string]time.Time),
		grace:     defaultIdleNudgeGrace,
	}
}

// nudgeIdleSessions checks alive sessions for idle-at-prompt state with
// assigned work. Called once per reconciler tick.
func (in *idleNudger) nudgeIdleSessions(
	sp runtime.Provider,
	sessions []beads.Bead,
	assignedWorkBeads []beads.Bead,
	now time.Time,
	stdout io.Writer,
) {
	workBySession := buildSessionWorkLookup(sessions, assignedWorkBeads)

	visited := make(map[string]bool, len(sessions))

	for i := range sessions {
		s := &sessions[i]
		name := s.Metadata["session_name"]
		if name == "" || !sp.IsRunning(name) {
			continue
		}
		visited[name] = true

		// Only nudge sessions that have assigned work.
		if !workBySession[name] {
			delete(in.firstIdle, name)
			continue
		}

		idle := isIdleAtPrompt(sp, name)
		if !idle {
			delete(in.firstIdle, name)
			continue
		}

		// Track when we first saw it idle.
		if _, ok := in.firstIdle[name]; !ok {
			in.firstIdle[name] = now
			continue
		}

		// Check grace period.
		if now.Sub(in.firstIdle[name]) < in.grace {
			continue
		}

		// Session has assigned work and has been idle past grace. Nudge it.
		content := runtime.TextContent(
			"You were interrupted. Check your current work with " +
				"`bd list --assignee=\"$GC_SESSION_NAME\" --status=in_progress` " +
				"and resume where you left off. If no in_progress work, check " +
				"`bd ready --assignee=\"$GC_SESSION_NAME\"` for assigned ready work.",
		)
		if err := sp.Nudge(name, content); err != nil {
			fmt.Fprintf(stdout, "idle-nudge: nudge %s failed: %v\n", name, err) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "idle-nudge: nudged %s (idle with assigned work)\n", name) //nolint:errcheck
		}

		// Reset timer so we don't spam every tick.
		delete(in.firstIdle, name)
	}

	// Prune entries for sessions no longer tracked.
	for name := range in.firstIdle {
		if !visited[name] {
			delete(in.firstIdle, name)
		}
	}
}

// buildSessionWorkLookup returns a set of session names that have open or
// in_progress work assigned to them via any identifier (bead ID, session
// name, or alias).
func buildSessionWorkLookup(sessions []beads.Bead, workBeads []beads.Bead) map[string]bool {
	// Build reverse lookup: any identifier → session name
	idToName := make(map[string]string, len(sessions)*3)
	for _, s := range sessions {
		name := s.Metadata["session_name"]
		if name == "" {
			continue
		}
		idToName[s.ID] = name
		idToName[name] = name
		if alias := strings.TrimSpace(s.Metadata["configured_named_identity"]); alias != "" {
			idToName[alias] = name
		}
	}

	result := make(map[string]bool)
	for _, wb := range workBeads {
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" {
			continue
		}
		if wb.Status != "open" && wb.Status != "in_progress" {
			continue
		}
		if name, ok := idToName[assignee]; ok {
			result[name] = true
		}
	}
	return result
}

// isIdleAtPrompt does a single capture-pane and checks whether Claude CLI
// is at its idle prompt (❯) with no busy indicator.
func isIdleAtPrompt(sp runtime.Provider, name string) bool {
	output, err := sp.Peek(name, 10)
	if err != nil {
		return false
	}

	lines := strings.Split(output, "\n")

	// If busy indicator is present, not idle.
	for _, line := range lines {
		if strings.Contains(line, "esc to interrupt") {
			return false
		}
	}

	// Check for prompt prefix in the captured lines.
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "❯") {
			return true
		}
	}
	return false
}
