package main

import (
	"path"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// PoolSessionName derives the tmux session name for a pool worker session.
// Format: {basename(template)}-{beadID} (e.g., "claude-mc-xyz").
// Named sessions with an alias use the alias instead.
func PoolSessionName(template, beadID string) string {
	base := path.Base(template)
	// Sanitize: replace "/" with "--" for tmux compatibility.
	base = strings.ReplaceAll(base, "/", "--")
	return base + "-" + beadID
}

// GCSweepSessionBeads closes open session beads that have no remaining
// assigned work beads (all assigned beads are closed). Returns the IDs
// of session beads that were closed.
func GCSweepSessionBeads(store beads.Store, sessionBeads []beads.Bead, allWorkBeads []beads.Bead) []string {
	// Index work beads by assignee.
	assigneeHasWork := make(map[string]bool)
	for _, wb := range allWorkBeads {
		if wb.Status == "closed" {
			continue
		}
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee != "" {
			assigneeHasWork[assignee] = true
		}
	}

	var closed []string
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		// If no non-closed work beads have this session as assignee, close it.
		if !assigneeHasWork[sb.ID] {
			if err := store.SetMetadata(sb.ID, "state", "gc_swept"); err != nil {
				continue
			}
			if err := store.Close(sb.ID); err != nil {
				continue
			}
			closed = append(closed, sb.ID)
		}
	}
	return closed
}
