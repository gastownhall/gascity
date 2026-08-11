package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/deps"
	"github.com/gastownhall/gascity/internal/fsys"
)

const bdReadyProjectionMinVersion = "1.0.5"

// ErrReadyProjectionUnsupported is the named degraded state a cache enters when
// its backing store cannot serve the ready projection AT ALL, as opposed to
// having failed to serve it this cycle.
//
// It is a degrade rather than a partial result because the enrichment is an
// optimization: IsBlocked==nil is the documented fallback and cachedBeadReady
// derives readiness from dependencies instead, so the snapshot is whole. The
// distinction is load-bearing — a cache that folds this into primePartialErr
// declines every cache-only read for the life of the process.
//
// It is reported exactly once per store: the verdict is latched, so the first
// prime or reconcile after the discovery carries the cause onto the problem log
// and an operator notice, and every later cycle degrades quietly.
var ErrReadyProjectionUnsupported = errors.New("ready projection unsupported by this bead store")

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
	// Probe the capability once per process. Operators must restart gc after
	// changing bd versions or re-pointing a scope at another backend to
	// re-evaluate ready-projection support.
	if s.readyProjectionChecked {
		return s.readyProjectionEnabled, nil
	}
	if reason := s.readyProjectionBackendRefusal(); reason != nil {
		s.disableReadyProjectionLocked(reason)
		return false, fmt.Errorf("bd ready projection backend gate: %w: %w", ErrReadyProjectionUnsupported, reason)
	}
	// A bd that predates the projection is not a degraded state to announce:
	// the feature never existed for it, the pinned toolchain version is
	// something operators already manage through deps.env, and the absence
	// costs only the enrichment. The verdicts below this comment are about the
	// LEDGER, and those do announce themselves.
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

// readyProjectionBackendRefusal reports why this scope's backend cannot answer
// `bd sql`, or nil when nothing on disk says it cannot.
//
// The version gate above asks the wrong question. `bd sql` is a raw-database
// escape hatch, and whether bd implements it is a property of the BACKEND it
// opened, not of the bd release: bd refuses it outright unless it holds a SQL
// session ("'bd sql' is not yet supported in embedded mode", cmd/bd/sql.go).
// gc's own composition root already names which backends this build implements
// — reads their metadata shape, projects their environment, manages their
// runtime (contract.RegisteredBackends). A scope served through the linked
// beads library under any other name is one gc knows nothing about, so
// assuming its `bd sql` works, and that its schema carries gc's issues/wisps
// projection, is a guess. Withholding the call is the honest default and costs
// that scope one optimization whose absence is already the documented benign
// state (IsBlocked==nil falls back to dependency-derived readiness).
//
// A scope naming a registered backend — including metadata that names none, and
// including metadata gc cannot read — reaches nil here and takes exactly the
// path it took before, so the Dolt cities are untouched. Nothing on disk proves
// a Dolt SQL server is REACHABLE, only that gc implements the backend; a bd
// that then refuses anyway is caught by the runtime latch in
// fetchReadyProjection.
func (s *BdStore) readyProjectionBackendRefusal() error {
	_, _, err := contract.LoadMetadataState(fsys.OSFS{}, filepath.Join(s.dir, ".beads", "metadata.json"))
	if err == nil || !errors.Is(err, contract.ErrUnknownBackend) {
		return nil
	}
	return err
}

// disableReadyProjectionLocked latches the projection off for the life of this
// store and states the reason once. Callers hold readyProjectionMu.
//
// The latch is one-way for the same reason the conditional-release one is: the
// ledger in front of the process cannot grow a capability mid-run, and
// re-probing per cycle costs a guaranteed-failing subprocess on every cache
// prime and every reconcile — which is exactly the defect this closes.
func (s *BdStore) disableReadyProjectionLocked(reason error) {
	s.readyProjectionEnabled = false
	s.readyProjectionChecked = true
	_, _ = fmt.Fprintf(s.noticeWriter(),
		"gc: ready-projection enrichment disabled for %s: %v\n"+
			"gc: cached ready falls back to dependency-derived readiness; no work is lost and no further bd sql is spent.\n",
		s.dir, reason)
}

// latchReadyProjectionUnsupported records bd's own refusal of `bd sql`, so no
// later cycle spends the call again.
func (s *BdStore) latchReadyProjectionUnsupported(cause error) {
	s.readyProjectionMu.Lock()
	defer s.readyProjectionMu.Unlock()
	if s.readyProjectionChecked && !s.readyProjectionEnabled {
		return
	}
	s.disableReadyProjectionLocked(cause)
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
		if isBdSQLUnsupportedInEmbeddedMode(err) {
			// Belt-and-braces to the backend gate: a scope whose metadata does
			// not name its backend, or names one gc implements while bd opened
			// it some other way, only learns this from bd's own answer. It is a
			// permanent property of the ledger, so it is latched rather than
			// re-discovered — the unlatched version of this call cost
			// maintainer-city a failing 6-16s subprocess on every prime and
			// every reconcile, indefinitely.
			s.latchReadyProjectionUnsupported(err)
			return nil, fmt.Errorf("bd sql ready projection: %w: %w", ErrReadyProjectionUnsupported, err)
		}
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
