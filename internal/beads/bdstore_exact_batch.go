package beads

import (
	"encoding/json"
	"fmt"
)

// GetExactBatch reads ids with a single `bd show --json <ids...>` and returns
// the beads bd answered for exactly the requested ID. Every id bd did not
// return verbatim is listed in unresolved, in input order: it may be absent,
// only reachable through Get's wisp fallback, or a substring collision (bd
// resolved it to a different bead). Callers that must tell those apart
// resolve the unresolved ids one at a time with Get.
//
// It exists for bulk mutation guards: one bd fork for a batch instead of one
// or two per id. A bd error that only reports missing beads is not an error
// here (every id is then unresolved); any other failure is returned.
func (s *BdStore) GetExactBatch(ids []string) (found map[string]Bead, unresolved []string, err error) {
	found = make(map[string]Bead, len(ids))
	if len(ids) == 0 {
		return found, nil, nil
	}
	args := append([]string{"show", "--json"}, ids...)
	out, runErr := s.runBDTransientRead(args...)
	if runErr != nil {
		if !isBdBeadNotFound(runErr) {
			return nil, nil, fmt.Errorf("getting beads %v: %w", ids, runErr)
		}
		return found, append([]string(nil), ids...), nil
	}
	var issues []bdIssue
	if err := json.Unmarshal(extractJSON(out), &issues); err != nil {
		return nil, nil, fmt.Errorf("bd show: parsing JSON: %w", err)
	}
	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}
	for _, issue := range issues {
		bead := issue.toBead()
		if requested[bead.ID] {
			found[bead.ID] = bead
		}
	}
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			unresolved = append(unresolved, id)
		}
	}
	return found, unresolved, nil
}
