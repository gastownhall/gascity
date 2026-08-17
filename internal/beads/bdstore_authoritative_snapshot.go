package beads

import "fmt"

var _ AuthoritativeSnapshotter = (*BdStore)(nil)

// AuthoritativeSnapshot reads the complete BdStore corpus with strict reads of
// both backing tiers, then point-authority hydrates only rows that can
// have come from the wisps table. Upstream SearchIssues prefers a wisp when an
// ID exists in both tables, while bd show (and BdStore.Get) prefers the issues
// table. Ephemeral and no-history are the routing flags carried by every row
// written to wisps through supported bd writers and migrations; re-reading
// just those IDs through GetBatch repairs that precedence without issuing bd
// show for the usually much larger durable history. (A row inserted directly
// into wisps with both routing flags false violates bd's storage contract and
// cannot be identified through its public list/query surface.)
//
// Both tier reads surface any command or partial-decode failure. In particular,
// this path must not inherit ordinary List's compatibility fallback that treats
// an unsupported `bd query` as an empty wisp tier: recovery cannot prove a
// complete projection in that case. Any error aborts before authority
// hydration or recovery can consume a row.
func (s *BdStore) AuthoritativeSnapshot() ([]Bead, error) {
	query := ListQuery{
		AllowScan:     true,
		IncludeClosed: true,
		SkipLabels:    true,
		TierMode:      TierBoth,
	}
	primary, err := s.listViaBDList(query)
	if err != nil {
		return nil, fmt.Errorf("reading bd snapshot primary census: %w", err)
	}
	if _, err := validateAuthoritativeSnapshotRows(primary); err != nil {
		return nil, fmt.Errorf("validating bd snapshot primary census: %w", err)
	}
	wisps, err := s.listEphemeralStrictSnapshot()
	if err != nil {
		return nil, fmt.Errorf("reading bd snapshot wisp census: %w", err)
	}
	if _, err := validateAuthoritativeSnapshotRows(wisps); err != nil {
		return nil, fmt.Errorf("validating bd snapshot wisp census: %w", err)
	}
	rows, err := mergeListTierResults(query, "bd authoritative snapshot", primary, nil, wisps, nil)
	if err != nil {
		return nil, fmt.Errorf("merging bd snapshot tiers: %w", err)
	}

	suspectIDs := make([]string, 0)
	for _, row := range rows {
		if row.Ephemeral || row.NoHistory {
			suspectIDs = append(suspectIDs, row.ID)
		}
	}
	if len(suspectIDs) == 0 {
		return validateAuthoritativeSnapshotRows(rows)
	}

	authoritative, err := s.GetBatch(suspectIDs)
	if err != nil {
		return nil, fmt.Errorf("hydrating bd snapshot tier authority: %w", err)
	}
	byID := make(map[string]Bead, len(authoritative))
	for _, row := range authoritative {
		byID[row.ID] = row
	}
	for i := range rows {
		if row, ok := byID[rows[i].ID]; ok {
			rows[i] = row
		}
	}
	return validateAuthoritativeSnapshotRows(rows)
}

// listEphemeralStrictSnapshot is the all-wisp read used only by the strict
// recovery projection. Ordinary List deliberately tolerates old bd versions
// without `query`; recovery instead fails closed because silently treating an
// unreadable tier as empty could omit a completed graph step forever.
func (s *BdStore) listEphemeralStrictSnapshot() ([]Bead, error) {
	out, err := s.runBDTransientRead("query", "--json", "ephemeral=true", "--all", "--limit", "0")
	if err != nil {
		return nil, fmt.Errorf("bd query (strict snapshot wisps): %w", err)
	}
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	rows := make([]Bead, len(issues))
	for i := range issues {
		rows[i] = issues[i].toBead()
		rows[i].Ephemeral = true
		rows[i].NoHistory = false
	}
	if parseErr != nil {
		if len(rows) > 0 {
			return rows, &PartialResultError{Op: "bd query strict snapshot", Err: parseErr}
		}
		return nil, fmt.Errorf("bd query strict snapshot: %w", parseErr)
	}
	return rows, nil
}
