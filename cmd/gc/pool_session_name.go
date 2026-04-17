package main

import (
	"path"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// PoolSessionName derives the tmux session name for a pool worker session.
// Format: {basename(template)}-{beadID} (e.g., "claude-mc-xyz").
// Named sessions with an alias use the alias instead.
func PoolSessionName(template, beadID string) string {
	base := path.Base(template)
	return agent.SanitizeQualifiedNameForSession(base) + "-" + beadID
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
		// Check if any non-closed work bead is assigned to this session
		// via any identifier: bead ID, session name, or named identity (alias).
		if sessionHasAssignedWork(sb, assigneeHasWork) {
			continue
		}
		if err := store.SetMetadata(sb.ID, "state", "gc_swept"); err != nil {
			continue
		}
		if err := store.Close(sb.ID); err != nil {
			continue
		}
		closed = append(closed, sb.ID)
	}
	return closed
}

// releaseOrphanedPoolAssignments reopens active pool-routed work whose
// assignee no longer maps to any open session bead. This recovers attempts
// that were left in_progress after a pooled worker exited or was swept.
func releaseOrphanedPoolAssignments(
	store beads.Store,
	cfg *config.City,
	openSessionBeads []beads.Bead,
	assignedWorkBeads []beads.Bead,
) []string {
	if store == nil || cfg == nil || len(assignedWorkBeads) == 0 {
		return nil
	}

	openIdentifiers := make(map[string]struct{}, len(openSessionBeads)*3)
	for _, sb := range openSessionBeads {
		if sb.Status == "closed" {
			continue
		}
		if id := strings.TrimSpace(sb.ID); id != "" {
			openIdentifiers[id] = struct{}{}
		}
		if sn := strings.TrimSpace(sb.Metadata["session_name"]); sn != "" {
			openIdentifiers[sn] = struct{}{}
		}
		if ni := strings.TrimSpace(sb.Metadata["configured_named_identity"]); ni != "" {
			openIdentifiers[ni] = struct{}{}
		}
	}

	var released []string
	seen := make(map[string]struct{}, len(assignedWorkBeads))
	for _, wb := range assignedWorkBeads {
		if wb.Status != "open" && wb.Status != "in_progress" {
			continue
		}
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" {
			continue
		}
		if _, ok := openIdentifiers[assignee]; ok {
			continue
		}
		template := strings.TrimSpace(wb.Metadata["gc.routed_to"])
		if template == "" {
			continue
		}
		agentCfg := findAgentByTemplate(cfg, template)
		if agentCfg == nil || !isMultiSessionCfgAgent(agentCfg) {
			continue
		}
		if _, ok := seen[wb.ID]; ok {
			continue
		}
		seen[wb.ID] = struct{}{}

		if err := store.Update(wb.ID, beads.UpdateOpts{
			Assignee: stringPtr(""),
			Status:   stringPtr("open"),
		}); err != nil {
			continue
		}
		released = append(released, wb.ID)
	}
	return released
}

// sessionHasAssignedWork checks whether any work bead is assigned to this
// session bead via any of its identifiers: bead ID, session name, or
// named identity (alias).
func sessionHasAssignedWork(sb beads.Bead, assigneeHasWork map[string]bool) bool {
	if assigneeHasWork[sb.ID] {
		return true
	}
	if sn := strings.TrimSpace(sb.Metadata["session_name"]); sn != "" && assigneeHasWork[sn] {
		return true
	}
	if ni := strings.TrimSpace(sb.Metadata["configured_named_identity"]); ni != "" && assigneeHasWork[ni] {
		return true
	}
	return false
}

func stringPtr(s string) *string { return &s }
