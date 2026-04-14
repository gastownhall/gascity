package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// wispFreshness is an opaque snapshot of one session's most-recently-updated
// open wisp. It lives inside cmd/gc so raw bd JSON types (updated_at etc.)
// do not leak across the stuckTracker boundary.
type wispFreshness struct {
	id        string
	updatedAt time.Time
}

// freshnessEntry is the minimal JSON shape parsed from bd list --json.
// beads.Bead does not expose updated_at, so the stuck sweep parses the raw
// bd output rather than extending the Bead struct (rule-of-three not met).
type freshnessEntry struct {
	ID        string            `json:"id"`
	UpdatedAt string            `json:"updated_at"`
	Metadata  map[string]string `json:"metadata"`
}

// sweepWispFreshness queries bd for all in-progress wisps in one shellout and
// groups them by session_name, returning the most-recently-updated entry per
// session. One call per sweep (not per session) per the design brief.
//
// Returns an empty map and nil error when there are no in-progress wisps.
// On shellout or parse error the entire sweep fails open (whole-map fail);
// callers must treat the error as a signal to skip the tick.
func sweepWispFreshness(cityPath string, runner beads.CommandRunner) (map[string]wispFreshness, error) {
	out, err := runner(cityPath, "bd", "list", "--json", "--limit=0",
		"--status=in_progress", "--type=wisp")
	if err != nil {
		return nil, fmt.Errorf("listing in-progress wisps: %w", err)
	}
	var entries []freshnessEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("parsing wisp list: %w", err)
	}
	result := make(map[string]wispFreshness, len(entries))
	for _, e := range entries {
		sn := e.Metadata["session_name"]
		if sn == "" {
			continue
		}
		updated, err := time.Parse(time.RFC3339, e.UpdatedAt)
		if err != nil {
			continue
		}
		// Tiebreaker: when multiple open wisps map to one session (legal
		// during handoff), pick the most-recent updated_at.
		if cur, ok := result[sn]; ok && !updated.After(cur.updatedAt) {
			continue
		}
		result[sn] = wispFreshness{id: e.ID, updatedAt: updated}
	}
	return result, nil
}
