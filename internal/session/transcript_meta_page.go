package session

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
)

// PersistedTranscriptMetaSnapshot loads authoritative session records once for
// a supervisor lifetime and sorts them in stable lexical ID order. Closed
// records are included because historical transcript correlation is still
// valuable after a session has ended.
//
// Store pagination is currently keyed by (created_at, id), not the required
// lexical session ID. This method therefore takes one authoritative typed
// snapshot then sorts it locally. The snapshot read is not bounded; the worker
// applies its exact-key resolution, provider directory traversal, and writes in
// bounded batches. Keep that distinction explicit until the session store grows
// a lexical-ID keyset query.
//
// This intentionally uses the same type+label union as every session-domain
// list, so type-only canonical records and repairable label-only records both
// participate. It does not apply Manager.EnrichInfo: runtime probes are not
// needed to resolve a persisted provider session key and would make the
// archival pass fleet-size dependent.
func (m *Manager) PersistedTranscriptMetaSnapshot() ([]Info, error) {
	if m == nil {
		return nil, nil
	}
	infos, err := m.PersistedStore().ListAll(ListAllOptions{
		IncludeClosed: true,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, fmt.Errorf("listing persisted transcript-metadata sessions: %w", err)
	}

	result := make([]Info, 0, len(infos))
	for _, info := range infos {
		if info.ID == "" {
			continue
		}
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// KeyedTranscriptPaths resolves an already-persisted page using only stable
// provider keys. It is the batched counterpart of KeyedTranscriptPath: callers
// that process a page avoid N independent Codex directory scans while retaining
// its exact-key, ambiguity-refusing semantics. No fallback provider is supplied,
// so legacy records without a persisted provider identity remain skipped.
func (m *Manager) KeyedTranscriptPaths(infos []Info, searchPaths []string) map[string]string {
	return ResolveKeyedTranscriptPaths(infos, searchPaths, "")
}
