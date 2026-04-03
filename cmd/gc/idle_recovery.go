package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// idleRecovery detects pool sessions that are alive but idle at the CLI
// prompt (not processing). This catches sessions that were interrupted
// (by Claude Code bugs, stray signals, etc.) and are now stuck.
//
// On each call it does a single capture-pane per alive pool session.
// If the session is idle:
//   - Has assigned work → nudge it to resume
//   - No assigned work  → set GC_DRAIN_ACK to clean it up
//
// The grace period prevents acting on sessions that are briefly idle
// between tool calls (inter-turn gap).
type idleRecovery struct {
	firstIdle map[string]time.Time // session name → first observed idle
	grace     time.Duration        // how long to wait before acting
}

func newIdleRecovery(grace time.Duration) *idleRecovery {
	return &idleRecovery{
		firstIdle: make(map[string]time.Time),
		grace:     grace,
	}
}

// recoverIdleSessions checks alive pool sessions for idle-at-prompt state.
// Called once per reconciler tick.
func (ir *idleRecovery) recoverIdleSessions(
	sp runtime.Provider,
	sessions []beads.Bead,
	assignedWorkBeads []beads.Bead,
	now time.Time,
	stdout io.Writer,
) {
	// Build lookup: session name → has assigned work
	workBySession := buildWorkBySession(sessions, assignedWorkBeads)

	// Track which sessions we visited so we can prune stale entries.
	visited := make(map[string]bool, len(sessions))

	for i := range sessions {
		s := &sessions[i]
		if s.Metadata["pool_managed"] != "true" {
			continue // only pool sessions
		}
		name := s.Metadata["session_name"]
		if name == "" || !sp.IsRunning(name) {
			continue
		}
		visited[name] = true

		idle := isSessionIdleAtPrompt(sp, name)
		if !idle {
			delete(ir.firstIdle, name)
			continue
		}

		// Track when we first saw it idle.
		if _, ok := ir.firstIdle[name]; !ok {
			ir.firstIdle[name] = now
			continue
		}

		// Check grace period.
		if now.Sub(ir.firstIdle[name]) < ir.grace {
			continue
		}

		// Session has been idle past the grace period. Act on it.
		if workBySession[name] {
			// Has work — nudge it to resume.
			content := runtime.TextContent(
				"You were interrupted. Check your current work with " +
					"`bd list --assignee=\"$GC_SESSION_NAME\" --status=in_progress` " +
					"and resume where you left off.",
			)
			if err := sp.Nudge(name, content); err != nil {
				fmt.Fprintf(stdout, "idle-recovery: nudge %s failed: %v\n", name, err) //nolint:errcheck
			} else {
				fmt.Fprintf(stdout, "idle-recovery: nudged %s (has assigned work)\n", name) //nolint:errcheck
			}
		} else {
			// No work — drain it.
			_ = sp.SetMeta(name, "GC_DRAIN_ACK", "1")
			fmt.Fprintf(stdout, "idle-recovery: set GC_DRAIN_ACK on %s (no assigned work)\n", name) //nolint:errcheck
		}

		// Reset timer so we don't spam every tick.
		delete(ir.firstIdle, name)
	}

	// Prune entries for sessions no longer tracked.
	for name := range ir.firstIdle {
		if !visited[name] {
			delete(ir.firstIdle, name)
		}
	}
}

// buildWorkBySession returns a set of session names that have in_progress
// or open work assigned to them.
func buildWorkBySession(sessions []beads.Bead, workBeads []beads.Bead) map[string]bool {
	// Build reverse lookup: any identifier → session name
	idToName := make(map[string]string, len(sessions)*2)
	for _, s := range sessions {
		name := s.Metadata["session_name"]
		if name == "" {
			continue
		}
		idToName[s.ID] = name
		idToName[name] = name
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

// isSessionIdleAtPrompt does a single capture-pane and checks whether
// Claude CLI is at its idle prompt (❯) with no busy indicator.
func isSessionIdleAtPrompt(sp runtime.Provider, name string) bool {
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
	promptPrefix := "❯"
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, promptPrefix) {
			return true
		}
	}
	return false
}
