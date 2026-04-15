package main

import (
	"path"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

var sessionKeepaliveMetadataKeys = []string{
	"bugflow.main_session_name",
}

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
// active work beads, including workflow beads that explicitly retain
// ownership via metadata. Returns the IDs of session beads that were closed.
func GCSweepSessionBeads(store beads.Store, sessionBeads []beads.Bead, allBeads []beads.Bead) []string {
	activeSessionRefs := indexActiveSessionRefs(allBeads)

	var closed []string
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		// Keep sessions alive for both direct assignment edges and known
		// metadata-based workflow ownership edges.
		if sessionHasActiveWorkRef(sb, activeSessionRefs) {
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

func indexActiveSessionRefs(allBeads []beads.Bead) map[string]bool {
	activeSessionRefs := make(map[string]bool)
	for _, bead := range allBeads {
		if bead.Status == "closed" {
			continue
		}
		addActiveSessionRef(activeSessionRefs, bead.Assignee)
		for _, key := range sessionKeepaliveMetadataKeys {
			addActiveSessionRef(activeSessionRefs, bead.Metadata[key])
		}
	}
	return activeSessionRefs
}

func addActiveSessionRef(activeSessionRefs map[string]bool, ref string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return
	}
	activeSessionRefs[ref] = true
}

// sessionHasActiveWorkRef checks whether any active bead references this
// session bead via any of its identifiers: bead ID, session name, or
// named identity (alias).
func sessionHasActiveWorkRef(sb beads.Bead, activeSessionRefs map[string]bool) bool {
	if activeSessionRefs[sb.ID] {
		return true
	}
	if sn := strings.TrimSpace(sb.Metadata["session_name"]); sn != "" && activeSessionRefs[sn] {
		return true
	}
	if ni := strings.TrimSpace(sb.Metadata["configured_named_identity"]); ni != "" && activeSessionRefs[ni] {
		return true
	}
	return false
}
