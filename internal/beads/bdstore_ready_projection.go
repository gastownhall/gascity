package beads

import (
	"encoding/json"
	"fmt"

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
		// Message and nudge beads are notifications, not dependency-blocked ready
		// work, and bd's denormalized is_blocked column can flap NULL<->false for
		// them. Enriching those rows makes the CachingStore reconciler re-emit
		// bead.updated on every cycle (an event flood that starves gc-hook work
		// queries). Leave their IsBlocked at bd's nil fallback so the reconcile
		// diff converges.
		if skipBDReadyProjectionEnrichment(item) {
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
	for i := range enriched {
		if skipBDReadyProjectionEnrichment(enriched[i]) {
			continue
		}
		blocked, ok := projection[enriched[i].ID]
		if !ok {
			continue
		}
		enriched[i].IsBlocked = cloneBoolPtr(&blocked)
	}
	return enriched, nil
}

func skipBDReadyProjectionEnrichment(item Bead) bool {
	return item.ID == "" ||
		item.Status == "closed" ||
		item.IsBlocked != nil ||
		item.Type == "message" ||
		beadHasLabel(item, "gc:nudge")
}

func (s *BdStore) bdReadyProjectionEnabled() (bool, error) {
	s.readyProjectionMu.Lock()
	defer s.readyProjectionMu.Unlock()
	// Probe the bd version once per process. Operators must restart gc after
	// changing bd versions to re-evaluate ready-projection support.
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
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		// An id under a relocated class's reserved prefix is not a row of this
		// ledger, so bd's answer about it would be an empty that reads as
		// "unblocked-unknown". Drop it from the request rather than refusing the
		// batch: this projection is handed EVERY active bead by cache
		// prime/reconcile, and a whole-batch refusal cost every other row its
		// is_blocked on every cycle, permanently — the call sites only
		// recordProblem and continue, and the refusal is a pure function of
		// config, so it never healed. A dropped id keeps its last cached value
		// (preserveCachedReadyProjectionLocked), which is the documented benign
		// state and exactly what its absence produced before this guard existed.
		//
		// The drop is silent on purpose. Nothing should mint a reserved class
		// prefix into this ledger — only the relocated class engine mints under
		// one, and a migration preserves the copied rows' original work-prefix
		// ids — so this is a should-never-happen that costs one row an
		// optimization, and a per-cycle log for it would be the reconcile-path
		// noise skipBDReadyProjectionEnrichment above exists to avoid.
		if len(s.relocatedClassesForID(id)) > 0 {
			continue
		}
		wanted[id] = struct{}{}
	}
	if len(wanted) == 0 {
		return result, nil
	}

	// bd exposes this as an active-row projection: the SQL filters out closed
	// rows so cache prime/reconcile cost stays O(active work) instead of
	// scanning unbounded closed issue/wisp history every cycle. The ids
	// argument is a cache-side allow-list so callers can keep their requested
	// surface bounded. A row that races closed between the list snapshot and
	// this fetch drops out of the projection; the reconciler preserves its last
	// cached is_blocked (preserveCachedReadyProjectionLocked) so the absence
	// does not flap a spurious bead.updated.
	out, err := s.runner(s.dir, "bd", "sql", readyProjectionSQL(), "--json")
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
		if _, ok := wanted[row.ID]; !ok {
			continue
		}
		result[row.ID] = row.IsBlocked.value
	}
	return result, nil
}

func readyProjectionSQL() string {
	return "select id,is_blocked from issues where status <> 'closed' union all select id,is_blocked from wisps where status <> 'closed'"
}
