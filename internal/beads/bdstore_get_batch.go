package beads

import (
	"fmt"
	"strconv"
	"strings"
)

// Keep each external command well below platform argument limits while still
// amortizing the process startup cost that GetBatch exists to avoid. The wisp
// query language repeats every ID in an OR expression, so its expression is
// the tighter of the two command shapes and determines the shared chunk size.
const (
	bdGetBatchChunkSize  = 128
	bdGetBatchChunkBytes = 16 * 1024
)

var _ BatchGetter = (*BdStore)(nil)

// GetBatch returns the same authoritative rows as repeated Get calls while
// batching the external bd reads. The routed bd-show result (including a
// durable no-history row) wins; only IDs proven absent from that primary read
// are queried in the wisp tier. Results are all-or-nothing and follow stable
// first-request order.
func (s *BdStore) GetBatch(ids []string) ([]Bead, error) {
	unique, err := uniqueBatchGetIDs(ids)
	if err != nil {
		return nil, fmt.Errorf("getting beads batch: %w", err)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	byID := make(map[string]Bead, len(unique))
	for start := 0; start < len(unique); {
		end := bdGetBatchChunkEnd(unique, start)
		chunk := unique[start:end]
		rows, readErr := s.getBatchPrimary(chunk)
		if readErr != nil {
			return nil, readErr
		}
		if err := addBdBatchRows(byID, rows, "bd get batch primary"); err != nil {
			return nil, err
		}
		start = end
	}

	missingWisps := make([]string, 0, len(unique)-len(byID))
	for _, id := range unique {
		if _, found := byID[id]; !found && isWispQueryableID(id) {
			missingWisps = append(missingWisps, id)
		}
	}
	for start := 0; start < len(missingWisps); {
		end := bdGetBatchChunkEnd(missingWisps, start)
		chunk := missingWisps[start:end]
		rows, readErr := s.getBatchWisps(chunk)
		if readErr != nil {
			return nil, readErr
		}
		if err := addBdBatchRows(byID, rows, "bd get batch wisps"); err != nil {
			return nil, err
		}
		start = end
	}

	ordered := make([]Bead, len(unique))
	for i, id := range unique {
		row, found := byID[id]
		if !found {
			return nil, fmt.Errorf("getting bead %q in batch: %w", id, ErrNotFound)
		}
		ordered[i] = row
	}
	return ordered, nil
}

func bdGetBatchChunkEnd(ids []string, start int) int {
	end := start
	bytes := 0
	for end < len(ids) && end-start < bdGetBatchChunkSize {
		// Eight bytes covers the larger wisp form's "id=" and " OR "
		// delimiters (and exceeds show argv's per-ID separator overhead).
		cost := len(ids[end]) + 8
		if end > start && bytes+cost > bdGetBatchChunkBytes {
			break
		}
		bytes += cost
		end++
	}
	return end
}

// getBatchPrimary uses multi-ID show so every ID follows the same local-first,
// prefix-routed resolution path as Get and every returned Bead carries the
// complete dependency payload. bd show emits all rows it finds even when some
// requested IDs are absent; a wholly absent chunk is reported as not found.
func (s *BdStore) getBatchPrimary(ids []string) ([]Bead, error) {
	rows, err := s.runBatchShow(ids)
	if err != nil {
		if bdBatchShowProvesAllNotFound(err, ids) {
			return nil, nil
		}
		return nil, fmt.Errorf("bd get batch primary: %w", err)
	}
	if len(rows) == 0 {
		// Preserve point Get's successful-empty behavior: it returns
		// ErrNotFound without consulting the wisp tier.
		return nil, fmt.Errorf("bd get batch primary show: empty result: %w", ErrNotFound)
	}

	found := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		found[row.ID] = struct{}{}
	}
	missing := make([]string, 0, len(ids)-len(found))
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return rows, nil
	}

	// A successful multi-show suppresses each omitted ID's stderr and exits
	// zero as long as any other ID was found. Re-probe only the omissions so
	// wisp fallback is authorized solely when every omitted ID has explicit
	// not-found evidence; routed/backend failures must fail the whole batch.
	verifiedRows, verifyErr := s.runBatchShow(missing)
	if verifyErr != nil {
		if bdBatchShowProvesAllNotFound(verifyErr, missing) {
			return rows, nil
		}
		return nil, fmt.Errorf("bd get batch primary absence verification: %w", verifyErr)
	}
	orderedVerified, validateErr := validateBatchGetRows(missing, verifiedRows)
	if validateErr != nil {
		return nil, &PartialResultError{
			Op:  "bd get batch primary absence verification",
			Err: validateErr,
		}
	}
	return append(rows, orderedVerified...), nil
}

func (s *BdStore) runBatchShow(ids []string) ([]Bead, error) {
	args := make([]string, 0, len(ids)+2)
	args = append(args, "show", "--json")
	args = append(args, ids...)
	out, err := s.runBDTransientRead(args...)
	if err != nil {
		return nil, err
	}
	return decodeBdBatchRows(out, ids, "bd get batch primary show", false)
}

// bdBatchShowProvesAllNotFound classifies bd show's per-ID stderr. Multi-ID
// show can mix not-found and backend failures in one aggregate error, so a
// broad substring test is unsafe: every requested ID must have one recognized
// not-found line and every line must be classified.
func bdBatchShowProvesAllNotFound(err error, ids []string) bool {
	if err == nil || len(ids) == 0 {
		return false
	}
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(ids))
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	for lineIndex, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			return false
		}
		if lineIndex == 0 {
			var ok bool
			line, ok = stripBDShowExitPrefix(line)
			if !ok {
				return false
			}
		}

		matchedID := ""
		for id := range wanted {
			fetchMarker := "Error fetching " + id + ":"
			issueMarker := "Issue " + id + " not found"
			switch {
			case strings.HasPrefix(line, fetchMarker):
				detail := strings.TrimSpace(strings.TrimPrefix(line, fetchMarker))
				if !isClassifiedBDShowNotFoundDetail(detail, id) {
					return false
				}
				matchedID = id
			case line == issueMarker:
				matchedID = id
			}
			if matchedID != "" {
				break
			}
		}
		if matchedID == "" {
			return false
		}
		if _, duplicate := seen[matchedID]; duplicate {
			return false
		}
		seen[matchedID] = struct{}{}
	}
	return len(seen) == len(wanted)
}

func stripBDShowExitPrefix(line string) (string, bool) {
	if strings.HasPrefix(line, "Error fetching ") || strings.HasPrefix(line, "Issue ") {
		return line, true
	}
	markerIndex := strings.Index(line, "Error fetching ")
	if markerIndex < 0 {
		markerIndex = strings.Index(line, "Issue ")
	}
	if markerIndex < 0 {
		return "", false
	}
	prefix := strings.TrimSpace(line[:markerIndex])
	if !strings.HasPrefix(prefix, "exit status ") || !strings.HasSuffix(prefix, ":") {
		return "", false
	}
	code := strings.TrimSuffix(strings.TrimPrefix(prefix, "exit status "), ":")
	exitCode, err := strconv.Atoi(code)
	if err != nil || exitCode <= 0 {
		return "", false
	}
	return line[markerIndex:], true
}

func isClassifiedBDShowNotFoundDetail(detail, id string) bool {
	detail = strings.ToLower(detail)
	id = strings.ToLower(id)
	known := []string{
		"not found",
		"not found: issue " + id,
		"get issue: not found",
		"get issue: not found: issue " + id,
		"no issue found matching " + strconv.Quote(id),
		"issue not found",
		"issue " + id + " not found",
	}
	for _, candidate := range known {
		if detail == candidate {
			return true
		}
	}
	return false
}

// getBatchWisps queries exactly the primary-absent IDs in one compound wisp
// expression. IDs reach this path only after isWispQueryableID accepts them,
// matching Get's query-interpolation guard.
func (s *BdStore) getBatchWisps(ids []string) ([]Bead, error) {
	clauses := make([]string, len(ids))
	for i, id := range ids {
		clauses[i] = "id=" + id
	}
	expression := "ephemeral=true AND " + clauses[0]
	if len(clauses) > 1 {
		expression = "ephemeral=true AND (" + strings.Join(clauses, " OR ") + ")"
	}
	out, err := s.runBDTransientRead("query", "--json", expression, "--all", "--limit", "0")
	if err != nil {
		if isBdQueryUnsupported(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("bd get batch wisps: %w", err)
	}
	return decodeBdBatchRows(out, ids, "bd get batch wisps", true)
}

// decodeBdBatchRows applies strict response validation before any decoded row
// can escape. parseIssuesTolerant is reused for bd's array/envelope variants,
// but a tolerant partial parse is never authoritative enough for GetBatch.
func decodeBdBatchRows(out []byte, ids []string, op string, forceEphemeral bool) ([]Bead, error) {
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	if parseErr != nil {
		if len(issues) > 0 {
			return nil, &PartialResultError{Op: op, Err: parseErr}
		}
		return nil, fmt.Errorf("%s: %w", op, parseErr)
	}

	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(issues))
	rows := make([]Bead, 0, len(issues))
	for i := range issues {
		row := issues[i].toBead()
		if forceEphemeral {
			row.Ephemeral = true
		}
		if strings.TrimSpace(row.ID) == "" {
			return nil, fmt.Errorf("%s: malformed result at index %d: empty bead ID", op, i)
		}
		if _, ok := wanted[row.ID]; !ok {
			unexpectedErr := ErrIDCollision
			if forceEphemeral {
				// Point Get treats a non-exact wisp query row as absence; the
				// collision sentinel is reserved for bd show's fuzzy resolver.
				unexpectedErr = ErrNotFound
			}
			return nil, fmt.Errorf("%s: unexpected bead %q for requested IDs %q: %w", op, row.ID, ids, unexpectedErr)
		}
		if _, duplicate := seen[row.ID]; duplicate {
			return nil, fmt.Errorf("%s: duplicate bead %q", op, row.ID)
		}
		seen[row.ID] = struct{}{}
		rows = append(rows, row)
	}
	return rows, nil
}

func addBdBatchRows(byID map[string]Bead, rows []Bead, op string) error {
	for _, row := range rows {
		if _, duplicate := byID[row.ID]; duplicate {
			return fmt.Errorf("%s: duplicate bead %q across responses", op, row.ID)
		}
		byID[row.ID] = row
	}
	return nil
}
