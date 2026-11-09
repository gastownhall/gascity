package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// mergeCrossEdgeRecords returns existing, a bead's gc.migrated_cross_edges
// record, with each of add appended that it does not already hold (same
// direction, type and other end), and whether anything was appended. A writer
// that runs again therefore records nothing twice. A record that does not
// parse is refused rather than overwritten: it may hold the only trace of an
// earlier import.
func mergeCrossEdgeRecords(existing string, add []rigCrossEdgeRecord) (string, bool, error) {
	var records []rigCrossEdgeRecord
	if strings.TrimSpace(existing) != "" {
		if err := json.Unmarshal([]byte(existing), &records); err != nil {
			return existing, false, fmt.Errorf("its %s record does not parse (%w); fix or remove it, then run this again", beadmeta.MigratedCrossEdgesMetadataKey, err)
		}
	}
	type key struct{ dir, typ, other string }
	have := make(map[key]bool, len(records)+len(add))
	for _, r := range records {
		have[key{r.Dir, r.Type, r.Other}] = true
	}
	changed := false
	for _, r := range add {
		k := key{r.Dir, r.Type, r.Other}
		if have[k] {
			continue
		}
		have[k] = true
		records = append(records, r)
		changed = true
	}
	if !changed {
		return existing, false, nil
	}
	raw, err := json.Marshal(records)
	if err != nil {
		return existing, false, fmt.Errorf("encoding its %s record: %w", beadmeta.MigratedCrossEdgesMetadataKey, err)
	}
	return string(raw), true, nil
}
