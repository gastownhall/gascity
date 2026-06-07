package beads

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/deps"
)

const bdReadyProjectionMinVersion = "1.0.5"

type bdReadyProjectionRow struct {
	ID        string       `json:"id"`
	IsBlocked optionalBool `json:"is_blocked"`
}

func (s *BdStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	if len(items) == 0 {
		return items, nil
	}
	ids := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item.ID == "" || item.Status == "closed" || item.IsBlocked != nil {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		ids = append(ids, item.ID)
	}
	if len(ids) == 0 {
		return items, nil
	}
	enabled, err := s.bdReadyProjectionEnabled()
	if err != nil {
		return items, err
	}
	if !enabled {
		return items, nil
	}

	projection, err := s.fetchReadyProjection(ids)
	if err != nil {
		return items, err
	}
	enriched := make([]Bead, len(items))
	copy(enriched, items)
	var missing []string
	for i := range enriched {
		if enriched[i].ID == "" || enriched[i].Status == "closed" || enriched[i].IsBlocked != nil {
			continue
		}
		blocked, ok := projection[enriched[i].ID]
		if !ok {
			missing = append(missing, enriched[i].ID)
			continue
		}
		enriched[i].IsBlocked = cloneBoolPtr(&blocked)
	}
	if len(missing) > 0 {
		return items, fmt.Errorf("bd ready projection missing is_blocked for %d active beads (first %s)", len(missing), missing[0])
	}
	return enriched, nil
}

func (s *BdStore) bdReadyProjectionEnabled() (bool, error) {
	s.readyProjectionMu.Lock()
	defer s.readyProjectionMu.Unlock()
	if s.readyProjectionChecked {
		return s.readyProjectionEnabled, nil
	}
	out, err := s.runner(s.dir, "bd", "version")
	if err != nil {
		return false, fmt.Errorf("bd ready projection version gate: %w", err)
	}
	version, err := parseBDVersion(string(out))
	if err != nil {
		return false, fmt.Errorf("bd ready projection version gate: %w", err)
	}
	s.readyProjectionEnabled = deps.CompareVersions(version, bdReadyProjectionMinVersion) >= 0
	s.readyProjectionChecked = true
	return s.readyProjectionEnabled, nil
}

func (s *BdStore) fetchReadyProjection(ids []string) (map[string]bool, error) {
	result := make(map[string]bool, len(ids))
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		query := readyProjectionSQL(ids[start:end])
		out, err := s.runner(s.dir, "bd", "sql", query, "--json")
		if err != nil {
			return nil, fmt.Errorf("bd sql ready projection: %w", err)
		}
		var rows []bdReadyProjectionRow
		if err := json.Unmarshal(extractJSON(out), &rows); err != nil {
			return nil, fmt.Errorf("bd sql ready projection: parsing JSON: %w", err)
		}
		for _, row := range rows {
			if row.ID == "" || !row.IsBlocked.set {
				continue
			}
			result[row.ID] = row.IsBlocked.value
		}
	}
	return result, nil
}

func readyProjectionSQL(ids []string) string {
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(id, "'", "''")+"'")
	}
	inClause := strings.Join(quoted, ",")
	return "select id,is_blocked from issues where id in (" + inClause + ") " +
		"union all select id,is_blocked from wisps where id in (" + inClause + ")"
}
