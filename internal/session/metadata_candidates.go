package session

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// ExactMetadataSessionCandidates returns session beads matching any exact
// metadata filter. Each filter must contain exactly one key/value pair; empty
// filters are ignored. Results are deduplicated by bead ID in query order.
func ExactMetadataSessionCandidates(store beads.Store, includeClosed bool, filters ...map[string]string) ([]beads.Bead, error) {
	return exactMetadataSessionCandidates(store, includeClosed, "", filters...)
}

// ExactMetadataSessionCandidatesWithStatus returns session beads matching any
// exact metadata filter and the requested bead status.
func ExactMetadataSessionCandidatesWithStatus(store beads.Store, status string, filters ...map[string]string) ([]beads.Bead, error) {
	return exactMetadataSessionCandidates(store, false, strings.TrimSpace(status), filters...)
}

// ExactMetadataSessionCandidatesInfo is the session.Info-projecting sibling of
// ExactMetadataSessionCandidates: it returns the projected session.Info of each
// candidate bead, applying the codec once here at the store edge so no raw bead
// escapes. It shares the dedup / query order / IsSessionBeadOrRepairable
// semantics of ExactMetadataSessionCandidates. It is the typed feed for the
// named-session retire lane, which needs only Info fields per candidate.
func ExactMetadataSessionCandidatesInfo(store beads.Store, includeClosed bool, filters ...map[string]string) ([]Info, error) {
	candidates, err := exactMetadataSessionCandidates(store, includeClosed, "", filters...)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(candidates))
	for _, b := range candidates {
		out = append(out, infoFromPersistedBead(b))
	}
	return out, nil
}

func exactMetadataSessionCandidates(store beads.Store, includeClosed bool, status string, filters ...map[string]string) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	seenQueries := make(map[string]bool, len(filters))
	seenBeads := make(map[string]bool)
	candidates := make([]beads.Bead, 0, len(filters))
	for _, filter := range filters {
		if len(filter) != 1 {
			continue
		}
		var key, value string
		for k, v := range filter {
			key = strings.TrimSpace(k)
			value = strings.TrimSpace(v)
		}
		if key == "" || value == "" {
			continue
		}
		queryKey := key + "\x00" + value
		if seenQueries[queryKey] {
			continue
		}
		seenQueries[queryKey] = true
		query := beads.ListQuery{
			Metadata: map[string]string{key: value},
		}
		if status != "" {
			query.Status = status
		} else {
			query.IncludeClosed = includeClosed
		}
		items, err := store.List(query)
		if err != nil {
			return nil, err
		}
		for _, b := range items {
			if seenBeads[b.ID] || !IsSessionBeadOrRepairable(b) {
				continue
			}
			RepairEmptyType(store, &b)
			seenBeads[b.ID] = true
			candidates = append(candidates, b)
		}
	}
	return candidates, nil
}
