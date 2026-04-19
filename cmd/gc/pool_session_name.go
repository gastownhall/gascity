package main

import (
	"path"
	"strings"
	"time"

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
// open/in-progress work beads anywhere — primary store OR any attached
// rig store. Work-bead assignment is verified by a live cross-store
// query inside closeSessionBeadIfUnassigned, so the caller does not
// pass a work snapshot — that pattern was retired to prevent pre-close
// tick snapshots from poisoning close decisions. Returns the IDs of
// session beads that were closed.
func GCSweepSessionBeads(store beads.Store, rigStores map[string]beads.Store, sessionBeads []beads.Bead) []string {
	var closed []string
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		if !closeSessionBeadIfUnassigned(store, rigStores, sb, "gc_swept", time.Now().UTC(), nil) {
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
		if agentCfg == nil || !agentCfg.SupportsGenericEphemeralSessions() {
			continue
		}
		if assigneePreservesNamedSessionRoute(cfg, template, assignee) {
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

func assigneePreservesNamedSessionRoute(cfg *config.City, template, assignee string) bool {
	if cfg == nil {
		return false
	}
	spec, ok := findNamedSessionSpec(cfg, cfg.EffectiveCityName(), assignee)
	if !ok {
		return false
	}
	return namedSessionBackingTemplate(spec) == template
}

func stringPtr(s string) *string { return &s }
