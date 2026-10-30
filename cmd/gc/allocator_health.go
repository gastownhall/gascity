package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// The allocator's episode read (P3 spec §4.2). Provider health (I7) and
// runtime suspension (I9) are read directly each pass, as legacy reads them
// (loadProviderHealthSnapshot, loadSuspensionState): two small files, and
// passes are at least 100ms apart.

// readStartupHealthEpisodes reads the startup-health episodes (#46) through
// the sessions store, keyed by their session name, which is the key legacy
// loads them by (startupHealthEpisodeKey(info, name)). A cached store serves
// them through its dirty-row overlay. A failed read returns the error and no
// episodes; consumers fail open on it, as legacy does on a load error
// (SESS-607).
func readStartupHealthEpisodes(store beads.Store) (map[string]session.StartupHealthEpisode, error) {
	rows, err := store.List(beads.ListQuery{Type: session.StartupHealthEpisodeType})
	if err != nil {
		return nil, err
	}
	// Legacy loads the first of ListByMetadata's order: the newest created,
	// ties by the largest bead ID.
	beads.SortBeads(rows, beads.SortCreatedDesc)
	episodes := make(map[string]session.StartupHealthEpisode, len(rows))
	for _, b := range rows {
		ep := session.StartupHealthEpisodeFromMetadata(b.Metadata)
		if _, dup := episodes[ep.SessionName]; !dup {
			episodes[ep.SessionName] = ep
		}
	}
	return episodes, nil
}
