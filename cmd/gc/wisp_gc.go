package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// wispGC performs mechanical garbage collection of closed molecules that
// have exceeded their TTL. Follows the nil-guard tracker pattern used by
// crashTracker and idleTracker: nil means disabled.
type wispGC interface {
	// shouldRun returns true if enough time has elapsed since the last run.
	shouldRun(now time.Time) bool

	// runGC lists closed molecules, deletes those older than TTL, and returns
	// the count of purged entries. Errors from individual deletes are
	// best-effort (logged but not fatal); the returned error is for list
	// failures.
	runGC(cityPath string, now time.Time) (int, error)
}

// memoryWispGC is the production implementation of wispGC.
type memoryWispGC struct {
	interval time.Duration
	ttl      time.Duration
	lastRun  time.Time
	runner   beads.CommandRunner
}

// newWispGC creates a wisp GC tracker. Returns nil if disabled (interval or
// TTL is zero). Callers nil-guard before use.
func newWispGC(interval, ttl time.Duration, runner beads.CommandRunner) wispGC {
	if interval <= 0 || ttl <= 0 {
		return nil
	}
	return &memoryWispGC{
		interval: interval,
		ttl:      ttl,
		runner:   runner,
	}
}

func (m *memoryWispGC) shouldRun(now time.Time) bool {
	return now.Sub(m.lastRun) >= m.interval
}

// gcEntry is the JSON structure returned by bd list --json for a bead.
type gcEntry struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Status    string `json:"status"`
	Type      string `json:"type"`
}

func (m *memoryWispGC) runGC(cityPath string, now time.Time) (int, error) {
	m.lastRun = now

	// List closed molecules.
	out, err := m.runner(cityPath, "bd", "list", "--json", "--limit=0", "--status=closed", "--type=molecule")
	if err != nil {
		return 0, fmt.Errorf("listing closed molecules: %w", err)
	}

	var entries []gcEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return 0, fmt.Errorf("parsing molecule list: %w", err)
	}

	cutoff := now.Add(-m.ttl)
	purged := 0
	for _, e := range entries {
		created, err := time.Parse(time.RFC3339, e.CreatedAt)
		if err != nil {
			continue // skip unparseable timestamps
		}
		if created.Before(cutoff) {
			_, delErr := m.runner(cityPath, "bd", "delete", e.ID, "--force")
			if delErr != nil {
				continue // best-effort: skip failed deletes
			}
			purged++
		}
	}

	// Purge expired closed tracking beads.
	trackOut, trackErr := m.runner(cityPath, "bd", "list", "--json",
		"--label="+labelOrderTracking, "--all", "--limit=0")
	if trackErr == nil {
		var trackEntries []gcEntry
		if err := json.Unmarshal(trackOut, &trackEntries); err == nil {
			for _, e := range trackEntries {
				if e.Status != "closed" {
					continue
				}
				created, err := time.Parse(time.RFC3339, e.CreatedAt)
				if err != nil {
					continue
				}
				if created.Before(cutoff) {
					_, delErr := m.runner(cityPath, "bd", "delete", e.ID, "--force")
					if delErr == nil {
						purged++
					}
				}
			}
		}
	}

	return purged, nil
}
