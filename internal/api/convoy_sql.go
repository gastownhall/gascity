package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltauth"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/sling"
)

type workflowSQLStoreCandidate struct {
	info workflowStoreInfo
	path string
}

type workflowSQLTableSet struct {
	beads  string
	labels string
	deps   string
}

var (
	workflowSQLIssueTables = workflowSQLTableSet{beads: "issues", labels: "labels", deps: "dependencies"}
	workflowSQLWispTables  = workflowSQLTableSet{beads: "wisps", labels: "wisp_labels", deps: "wisp_dependencies"}
)

func workflowSQLCandidatesForWorkflowID(
	state State,
	workflowID, requestedScopeKind, requestedScopeRef string,
) []workflowSQLStoreCandidate {
	requestedScopeKind = strings.TrimSpace(requestedScopeKind)
	requestedScopeRef = strings.TrimSpace(requestedScopeRef)
	if requestedScopeKind != "" && requestedScopeRef != "" {
		return workflowSQLStoreCandidates(state, requestedScopeKind, requestedScopeRef)
	}

	if prefix := workflowSQLWorkflowIDPrefix(state.Config(), workflowID); prefix != "" {
		if candidate, ok := workflowSQLRouteCandidate(state, prefix); ok {
			return []workflowSQLStoreCandidate{candidate}
		}
		return nil
	}

	return workflowSQLStoreCandidates(state, "", "")
}

// workflowSQLSnapshot fetches all workflow beads and deps via direct SQL,
// bypassing the N+1 bd subprocess calls. Returns beads, a bead index, and
// a pre-fetched dep map. Connects to the dolt server on the given port
// using the given database name.
func workflowSQLSnapshot(user, password, host string, port int, database, rootID string) ([]beads.Bead, map[string]beads.Bead, map[string][]beads.Dep, error) {
	dsn := buildDoltDSN(user, password, host, port, database)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sql open: %w", err)
	}
	defer db.Close() //nolint:errcheck // best-effort cleanup
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(30 * time.Second)

	tableSets, err := workflowSQLAvailableTableSets(db)
	if err != nil {
		return nil, nil, nil, err
	}

	workflowBeads, beadIndex, err := workflowSQLQueryWorkflowBeads(db, tableSets, rootID)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(workflowBeads) == 0 {
		return nil, nil, nil, fmt.Errorf("no beads found for workflow %s", rootID)
	}

	depMap, err := workflowSQLQueryWorkflowDeps(db, tableSets, rootID)
	if err != nil {
		return nil, nil, nil, err
	}

	if err := workflowSQLHydrateWorkflowLabels(db, tableSets, rootID, workflowBeads, beadIndex); err != nil {
		return workflowBeads, beadIndex, depMap, nil
	}

	return workflowBeads, beadIndex, depMap, nil
}

func workflowSQLQueryWorkflowBeads(db *sql.DB, tableSets []workflowSQLTableSet, rootID string) ([]beads.Bead, map[string]beads.Bead, error) {
	workflowBeads := make([]beads.Bead, 0, 100)
	beadIndex := make(map[string]beads.Bead)
	for _, tables := range tableSets {
		rows, err := db.Query(`
			SELECT
				i.id, i.title, i.status, i.issue_type, i.assignee,
				i.description, i.created_at, i.updated_at,
				i.metadata
			FROM `+tables.beads+` i
			WHERE i.id = ?
			   OR JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$."gc.root_bead_id"')) = ?
			ORDER BY i.created_at
		`, rootID, rootID)
		if err != nil {
			return nil, nil, fmt.Errorf("beads query %s: %w", tables.beads, err)
		}
		for rows.Next() {
			bead, ok, err := workflowSQLScanBead(rows.Scan)
			if err != nil {
				_ = rows.Close()
				return nil, nil, fmt.Errorf("bead scan %s: %w", tables.beads, err)
			}
			if !ok {
				continue
			}
			if _, seen := beadIndex[bead.ID]; seen {
				continue
			}
			workflowBeads = append(workflowBeads, bead)
			beadIndex[bead.ID] = bead
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("bead rows %s: %w", tables.beads, err)
		}
		if err := rows.Close(); err != nil {
			return nil, nil, fmt.Errorf("bead rows close %s: %w", tables.beads, err)
		}
	}
	sort.SliceStable(workflowBeads, func(i, j int) bool {
		return workflowBeads[i].CreatedAt.Before(workflowBeads[j].CreatedAt)
	})
	return workflowBeads, beadIndex, nil
}

func workflowSQLQueryWorkflowDeps(db *sql.DB, tableSets []workflowSQLTableSet, rootID string) (map[string][]beads.Dep, error) {
	depMap := make(map[string][]beads.Dep)
	subquery := workflowSQLWorkflowIDsSubquery(tableSets)
	subqueryArgs := workflowSQLWorkflowIDsSubqueryArgs(tableSets, rootID)
	for _, tables := range tableSets {
		exists, err := workflowSQLTableExists(db, tables.deps)
		if err != nil {
			return nil, fmt.Errorf("check dep table %s: %w", tables.deps, err)
		}
		if !exists {
			continue
		}
		dependsOnExpr, err := workflowSQLDependsOnExpr(db, tables.deps, "d")
		if err != nil {
			return nil, err
		}
		args := make([]any, 0, len(subqueryArgs)*2)
		args = append(args, subqueryArgs...)
		args = append(args, subqueryArgs...)
		rows, err := db.Query(`
			SELECT d.issue_id, `+dependsOnExpr+`, COALESCE(NULLIF(d.type, ''), 'blocks')
			FROM `+tables.deps+` d
			WHERE d.issue_id IN (`+subquery+`)
			  AND `+dependsOnExpr+` IN (`+subquery+`)
		`, args...)
		if err != nil {
			return nil, fmt.Errorf("deps query %s: %w", tables.deps, err)
		}
		for rows.Next() {
			var issueID, dependsOnID, depType sql.NullString
			if err := rows.Scan(&issueID, &dependsOnID, &depType); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("dep scan %s: %w", tables.deps, err)
			}
			dep := workflowSQLDepFromRow(issueID, dependsOnID, depType)
			depMap[dep.IssueID] = append(depMap[dep.IssueID], dep)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("dep rows %s: %w", tables.deps, err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("dep rows close %s: %w", tables.deps, err)
		}
	}
	return depMap, nil
}

func workflowSQLHydrateWorkflowLabels(db *sql.DB, tableSets []workflowSQLTableSet, rootID string, workflowBeads []beads.Bead, beadIndex map[string]beads.Bead) error {
	labelMap := make(map[string][]string)
	subquery := workflowSQLWorkflowIDsSubquery(tableSets)
	subqueryArgs := workflowSQLWorkflowIDsSubqueryArgs(tableSets, rootID)
	for _, tables := range tableSets {
		exists, err := workflowSQLTableExists(db, tables.labels)
		if err != nil {
			return fmt.Errorf("check label table %s: %w", tables.labels, err)
		}
		if !exists {
			continue
		}
		rows, err := db.Query(`
			SELECT l.issue_id, l.label
			FROM `+tables.labels+` l
			WHERE l.issue_id IN (`+subquery+`)
		`, subqueryArgs...)
		if err != nil {
			return fmt.Errorf("labels query %s: %w", tables.labels, err)
		}
		for rows.Next() {
			var issueID, label string
			if err := rows.Scan(&issueID, &label); err != nil {
				continue
			}
			labelMap[issueID] = append(labelMap[issueID], label)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("label rows %s: %w", tables.labels, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("label rows close %s: %w", tables.labels, err)
		}
	}
	for i := range workflowBeads {
		if labels, ok := labelMap[workflowBeads[i].ID]; ok {
			workflowBeads[i].Labels = labels
			beadIndex[workflowBeads[i].ID] = workflowBeads[i]
		}
	}
	return nil
}

func workflowSQLAvailableTableSets(db *sql.DB) ([]workflowSQLTableSet, error) {
	issueExists, err := workflowSQLTableExists(db, workflowSQLIssueTables.beads)
	if err != nil {
		return nil, fmt.Errorf("check table %s: %w", workflowSQLIssueTables.beads, err)
	}
	wispExists, err := workflowSQLTableExists(db, workflowSQLWispTables.beads)
	if err != nil {
		return nil, fmt.Errorf("check table %s: %w", workflowSQLWispTables.beads, err)
	}
	tableSets := make([]workflowSQLTableSet, 0, 2)
	if issueExists {
		tableSets = append(tableSets, workflowSQLIssueTables)
	}
	if wispExists {
		tableSets = append(tableSets, workflowSQLWispTables)
	}
	if len(tableSets) == 0 {
		return nil, fmt.Errorf("no workflow bead SQL tables")
	}
	return tableSets, nil
}

func workflowSQLTableExists(db *sql.DB, table string) (bool, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*)
		FROM information_schema.tables
		WHERE table_schema = DATABASE()
		  AND table_name = ?
	`, table).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func workflowSQLExistingColumns(db *sql.DB, table string, candidates []string) (map[string]bool, error) {
	if len(candidates) == 0 {
		return map[string]bool{}, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(candidates)), ",")
	args := make([]any, 0, len(candidates)+1)
	args = append(args, table)
	for _, column := range candidates {
		args = append(args, column)
	}
	rows, err := db.Query(`
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = DATABASE()
		  AND table_name = ?
		  AND column_name IN (`+placeholders+`)
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // best-effort cleanup
	columns := make(map[string]bool, len(candidates))
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		columns[column] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

func workflowSQLDependsOnExpr(db *sql.DB, table, alias string) (string, error) {
	candidates := []string{"depends_on_id", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"}
	columns, err := workflowSQLExistingColumns(db, table, candidates)
	if err != nil {
		return "", fmt.Errorf("read dep columns %s: %w", table, err)
	}
	expr, err := workflowSQLDependsOnExprFromColumns(alias, columns)
	if err != nil {
		return "", fmt.Errorf("dependency target columns %s: %w", table, err)
	}
	return expr, nil
}

func workflowSQLDependsOnExprFromColumns(alias string, columns map[string]bool) (string, error) {
	prefix := ""
	if strings.TrimSpace(alias) != "" {
		prefix = alias + "."
	}
	parts := make([]string, 0, 4)
	for _, column := range []string{"depends_on_id", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"} {
		if columns[column] {
			parts = append(parts, "NULLIF("+prefix+column+", '')")
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("no dependency target columns")
	}
	return "COALESCE(" + strings.Join(parts, ", ") + ", '')", nil
}

func workflowSQLWorkflowIDsSubquery(tableSets []workflowSQLTableSet) string {
	parts := make([]string, 0, len(tableSets))
	for _, tables := range tableSets {
		parts = append(parts, `
			SELECT i.id FROM `+tables.beads+` i
			WHERE i.id = ? OR JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$."gc.root_bead_id"')) = ?
		`)
	}
	return strings.Join(parts, " UNION ")
}

func workflowSQLWorkflowIDsSubqueryArgs(tableSets []workflowSQLTableSet, rootID string) []any {
	args := make([]any, 0, len(tableSets)*2)
	for range tableSets {
		args = append(args, rootID, rootID)
	}
	return args
}

func workflowSQLDepFromRow(issueID, dependsOnID, depType sql.NullString) beads.Dep {
	typ := depType.String
	if typ == "" {
		typ = "blocks"
	}
	return beads.Dep{
		IssueID:     issueID.String,
		DependsOnID: dependsOnID.String,
		Type:        typ,
	}
}

// tryFullWorkflowSQL does the entire workflow snapshot via SQL — root
// discovery, bead fetch, dep fetch, and graph build. Falls back to nil
// error only on full success so the caller can use the slow path on any failure.
// errNoSQLWorkflowStores is the benign "this deployment has no SQL-backed
// workflow store to consult" outcome — distinct from a SQL store that exists
// but could not be reached or did not contain the workflow. The caller
// (buildWorkflowSnapshot) uses errors.Is to keep the routine no-SQL fallback
// quiet while still surfacing genuine fast-path failures (gascity#2940).
var errNoSQLWorkflowStores = errors.New("no sql workflow stores")

func (s *Server) tryFullWorkflowSQL(workflowID, fallbackScopeKind, fallbackScopeRef string, snapshotIndex uint64) (*workflowSnapshotResponse, error) {
	candidates := workflowSQLCandidatesForWorkflowID(
		s.state,
		workflowID,
		fallbackScopeKind,
		fallbackScopeRef,
	)
	if len(candidates) == 0 {
		return nil, errNoSQLWorkflowStores
	}

	type sqlWorkflowRootMatch struct {
		candidate workflowSQLStoreCandidate
		root      beads.Bead
	}
	matches := make([]sqlWorkflowRootMatch, 0, len(candidates))
	// Retain the first genuine probe failure (Dolt unreachable, query error)
	// so a fully-failed sweep surfaces the real cause rather than a synthetic
	// "not found" — that real cause is exactly what the #2940 fallback log
	// needs to be actionable. A clean miss (ok == false, err == nil) is not a
	// failure: the workflow simply isn't in that store.
	var firstProbeErr error
	for _, candidate := range candidates {
		host, port, database, user, password, err := resolveDoltConnection(s.state.CityPath(), candidate.path)
		if err != nil {
			if firstProbeErr == nil {
				firstProbeErr = fmt.Errorf("resolve dolt connection for %s: %w", candidate.info.ref, err)
			}
			continue
		}
		root, ok, err := workflowSQLFindRoot(s.state.Config(), user, password, host, port, database, workflowID)
		if err != nil {
			if firstProbeErr == nil {
				firstProbeErr = fmt.Errorf("sql find root in %s: %w", candidate.info.ref, err)
			}
			continue
		}
		if !ok {
			continue
		}
		matches = append(matches, sqlWorkflowRootMatch{candidate: candidate, root: root})
	}
	if len(matches) == 0 {
		if firstProbeErr != nil {
			return nil, firstProbeErr
		}
		return nil, fmt.Errorf("workflow %q not found in sql stores", workflowID)
	}

	cityScopeRef := workflowCityScopeRef(s.state.CityName())
	workflowMatches := make([]workflowRootMatch, 0, len(matches))
	for _, match := range matches {
		workflowMatches = append(workflowMatches, workflowRootMatch{
			info: match.candidate.info,
			root: match.root,
		})
	}
	selected, ok := selectWorkflowRootMatch(workflowMatches, fallbackScopeKind, fallbackScopeRef, cityScopeRef)
	if !ok {
		return nil, fmt.Errorf("sql root match selection failed")
	}

	var chosen workflowSQLStoreCandidate
	foundCandidate := false
	for _, match := range matches {
		if match.root.ID == selected.root.ID && match.candidate.info.ref == selected.info.ref {
			chosen = match.candidate
			foundCandidate = true
			break
		}
	}
	if !foundCandidate {
		return nil, fmt.Errorf("sql root match candidate missing")
	}

	host, port, database, user, password, err := resolveDoltConnection(s.state.CityPath(), chosen.path)
	if err != nil {
		return nil, err
	}

	workflowBeads, beadIndex, depMap, err := workflowSQLSnapshot(user, password, host, port, database, selected.root.ID)
	if err != nil {
		return nil, err
	}
	if len(workflowBeads) == 0 {
		return nil, fmt.Errorf("no beads found")
	}

	root, ok := beadIndex[selected.root.ID]
	if !ok {
		return nil, fmt.Errorf("root bead not found in SQL results")
	}

	store := &prefetchedDepStore{deps: depMap}

	// Collect physical deps only — logical nodes are computed by real-world app.
	workflowDeps, partial := collectWorkflowDeps(store, beadIndex)

	scopeKind, scopeRef := workflowSQLSnapshotScope(root, chosen.info, fallbackScopeKind, fallbackScopeRef)

	storeRef := chosen.info.ref
	beadResponses := make([]workflowBeadResponse, 0, len(workflowBeads))
	for _, bead := range workflowBeads {
		beadResponses = append(beadResponses, workflowBeadResponse{
			ID:            bead.ID,
			Title:         bead.Title,
			Status:        workflowStatus(bead),
			Kind:          workflowKind(bead),
			StepRef:       strings.TrimSpace(bead.Metadata[beadmeta.StepRefMetadataKey]),
			Attempt:       workflowAttempt(bead),
			LogicalBeadID: strings.TrimSpace(bead.Metadata[beadmeta.LogicalBeadIDMetadataKey]),
			ScopeRef:      strings.TrimSpace(bead.Metadata[beadmeta.ScopeRefMetadataKey]),
			Assignee:      strings.TrimSpace(bead.Assignee),
			Metadata:      cloneStringMap(bead.Metadata),
		})
	}

	snapshot := &workflowSnapshotResponse{
		WorkflowID:        resolvedWorkflowID(root),
		RootBeadID:        root.ID,
		RootStoreRef:      storeRef,
		ScopeKind:         scopeKind,
		ScopeRef:          scopeRef,
		Beads:             beadResponses,
		Deps:              workflowDeps,
		LogicalNodes:      []LogicalNode{},
		LogicalEdges:      []workflowDepResponse{},
		ScopeGroups:       []ScopeGroup{},
		Partial:           partial,
		ResolvedRootStore: storeRef,
		StoresScanned:     []string{storeRef},
		SnapshotVersion:   snapshotIndex,
	}
	if snapshotIndex > 0 {
		snapshot.SnapshotEventSeq = &snapshotIndex
	}
	return snapshot, nil
}

func workflowSQLSnapshotScope(root beads.Bead, info workflowStoreInfo, fallbackScopeKind, fallbackScopeRef string) (string, string) {
	scopeKind := strings.TrimSpace(fallbackScopeKind)
	scopeRef := strings.TrimSpace(fallbackScopeRef)
	if scopeKind == "" {
		scopeKind = strings.TrimSpace(info.scopeKind)
	}
	if scopeRef == "" {
		scopeRef = strings.TrimSpace(info.scopeRef)
	}
	if sk := strings.TrimSpace(root.Metadata[beadmeta.ScopeKindMetadataKey]); sk != "" {
		scopeKind = sk
	}
	if sr := strings.TrimSpace(root.Metadata[beadmeta.ScopeRefMetadataKey]); sr != "" {
		scopeRef = sr
	}
	return scopeKind, scopeRef
}

// tryWorkflowSQL attempts to resolve the dolt port and database for the
// city and fetch the workflow snapshot via direct SQL. Returns a non-nil
// error if SQL is not available (caller should fall back to bd subprocess).
func (s *Server) tryWorkflowSQL(info workflowStoreInfo, rootID string) ([]beads.Bead, map[string]beads.Bead, map[string][]beads.Dep, error) {
	storePath, ok := workflowStorePath(s.state, info)
	if !ok {
		return nil, nil, nil, fmt.Errorf("no store path for %s", info.ref)
	}

	host, port, database, user, password, err := resolveDoltConnection(s.state.CityPath(), storePath)
	if err != nil {
		return nil, nil, nil, err
	}

	return workflowSQLSnapshot(user, password, host, port, database, rootID)
}

func workflowSQLStoreCandidates(state State, requestedScopeKind, requestedScopeRef string) []workflowSQLStoreCandidate {
	requestedScopeKind = strings.TrimSpace(requestedScopeKind)
	requestedScopeRef = strings.TrimSpace(requestedScopeRef)
	if requestedScopeKind != "" && requestedScopeRef != "" {
		if info, ok := workflowStoreByRef(state, requestedScopeKind+":"+requestedScopeRef); ok {
			if path, ok := workflowStorePath(state, info); ok {
				return []workflowSQLStoreCandidate{{info: info, path: path}}
			}
		}
		return nil
	}

	stores := workflowStores(state)
	candidates := make([]workflowSQLStoreCandidate, 0, len(stores))
	for _, info := range stores {
		if path, ok := workflowStorePath(state, info); ok {
			candidates = append(candidates, workflowSQLStoreCandidate{
				info: info,
				path: path,
			})
		}
	}
	return candidates
}

func workflowSQLRouteCandidate(state State, prefix string) (workflowSQLStoreCandidate, bool) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return workflowSQLStoreCandidate{}, false
	}
	cfg := state.Config()
	if cfg == nil {
		return workflowSQLStoreCandidate{}, false
	}
	candidates := workflowSQLStoreCandidates(state, "", "")
	if len(candidates) == 0 {
		return workflowSQLStoreCandidate{}, false
	}

	for _, rig := range cfg.Rigs {
		rigPath := resolveScopeRoot(state.CityPath(), rig.Path)
		if rigPath == "" {
			continue
		}
		storePath, ok := resolveRoutePrefix(rigPath, prefix)
		if !ok {
			continue
		}
		cleanStorePath := filepath.Clean(storePath)
		for _, candidate := range candidates {
			if filepath.Clean(candidate.path) == cleanStorePath {
				return candidate, true
			}
		}
	}

	return workflowSQLStoreCandidate{}, false
}

func workflowStorePath(state State, info workflowStoreInfo) (string, bool) {
	switch strings.TrimSpace(info.scopeKind) {
	case "city":
		cityPath := strings.TrimSpace(state.CityPath())
		return cityPath, cityPath != ""
	case "rig":
		cfg := state.Config()
		if cfg == nil {
			return "", false
		}
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Name) != info.scopeRef {
				continue
			}
			rigPath := resolveScopeRoot(state.CityPath(), rig.Path)
			if rigPath == "" {
				return "", false
			}
			return rigPath, true
		}
	}
	return "", false
}

func workflowSQLFindRoot(cfg *config.City, user, password, host string, port int, database, workflowID string) (beads.Bead, bool, error) {
	db, err := openWorkflowSQLDB(user, password, host, port, database)
	if err != nil {
		return beads.Bead{}, false, err
	}
	defer db.Close() //nolint:errcheck // best-effort cleanup

	tableSets, err := workflowSQLAvailableTableSets(db)
	if err != nil {
		return beads.Bead{}, false, err
	}

	hasWorkflowIDPrefix := workflowSQLWorkflowIDPrefix(cfg, workflowID) != ""
	if root, ok, err := workflowSQLGetBeadFromTables(db, tableSets, workflowID); err != nil {
		return beads.Bead{}, false, err
	} else if ok {
		if isWorkflowRoot(root) && matchesWorkflowID(root, workflowID) {
			return root, true, nil
		}
		if hasWorkflowIDPrefix {
			return beads.Bead{}, false, nil
		}
	}
	if hasWorkflowIDPrefix {
		return beads.Bead{}, false, nil
	}

	return workflowSQLFindRootByWorkflowID(db, tableSets, workflowID)
}

func workflowSQLWorkflowIDPrefix(cfg *config.City, workflowID string) string {
	return sling.BeadPrefixForCity(cfg, strings.TrimSpace(workflowID))
}

func workflowSQLGetBeadFromTables(db *sql.DB, tableSets []workflowSQLTableSet, id string) (beads.Bead, bool, error) {
	for _, tables := range tableSets {
		row := db.QueryRow(`
			SELECT
				i.id, i.title, i.status, i.issue_type, i.assignee,
				i.description, i.created_at, i.updated_at,
				i.metadata
			FROM `+tables.beads+` i
			WHERE i.id = ?
			LIMIT 1
		`, id)
		bead, ok, err := workflowSQLScanBead(row.Scan)
		if err != nil {
			return beads.Bead{}, false, fmt.Errorf("get bead %s from %s: %w", id, tables.beads, err)
		}
		if ok {
			return bead, true, nil
		}
	}
	return beads.Bead{}, false, nil
}

func workflowSQLFindRootByWorkflowID(db *sql.DB, tableSets []workflowSQLTableSet, workflowID string) (beads.Bead, bool, error) {
	matches := make([]beads.Bead, 0, len(tableSets))
	for _, tables := range tableSets {
		row := db.QueryRow(`
			SELECT
				i.id, i.title, i.status, i.issue_type, i.assignee,
				i.description, i.created_at, i.updated_at,
				i.metadata
			FROM `+tables.beads+` i
			WHERE JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$."gc.kind"')) = 'workflow'
			  AND JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$."gc.workflow_id"')) = ?
			ORDER BY i.created_at
			LIMIT 1
		`, workflowID)
		bead, ok, err := workflowSQLScanBead(row.Scan)
		if err != nil {
			return beads.Bead{}, false, fmt.Errorf("find workflow %s in %s: %w", workflowID, tables.beads, err)
		}
		if ok {
			matches = append(matches, bead)
		}
	}
	if len(matches) == 0 {
		return beads.Bead{}, false, nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].CreatedAt.Before(matches[j].CreatedAt)
	})
	return matches[0], true, nil
}

func openWorkflowSQLDB(user, password, host string, port int, database string) (*sql.DB, error) {
	dsn := buildDoltDSN(user, password, host, port, database)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(30 * time.Second)
	return db, nil
}

func workflowSQLScanBead(scan func(dest ...any) error) (beads.Bead, bool, error) {
	var b beads.Bead
	var assignee, description sql.NullString
	var metadataJSON []byte
	var createdAt, updatedAt time.Time

	if err := scan(
		&b.ID, &b.Title, &b.Status, &b.Type, &assignee,
		&description, &createdAt, &updatedAt,
		&metadataJSON,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return beads.Bead{}, false, nil
		}
		return beads.Bead{}, false, err
	}

	b.Assignee = assignee.String
	b.Description = description.String
	b.CreatedAt = createdAt
	b.UpdatedAt = updatedAt

	if len(metadataJSON) > 0 {
		b.Metadata = make(map[string]string)
		var raw map[string]interface{}
		if json.Unmarshal(metadataJSON, &raw) == nil {
			for k, v := range raw {
				if s, ok := v.(string); ok {
					b.Metadata[k] = s
				} else if encoded, err := json.Marshal(v); err == nil {
					b.Metadata[k] = string(encoded)
				}
			}
		}
	}

	return b, true, nil
}

// resolveDoltConnection reads the canonical beads contract and returns the
// resolved connection target for a city or rig scope.
func resolveDoltConnection(cityRoot, scopeRoot string) (string, int, string, string, string, error) {
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityRoot, scopeRoot)
	if err != nil {
		return "", 0, "", "", "", err
	}
	port, err := strconv.Atoi(strings.TrimSpace(target.Port))
	if err != nil {
		return "", 0, "", "", "", fmt.Errorf("parse dolt port %q: %w", target.Port, err)
	}
	host := strings.TrimSpace(target.Host)
	if host == "" {
		return "", 0, "", "", "", fmt.Errorf("missing dolt host for %s", scopeRoot)
	}
	auth := doltauth.Resolve(doltauth.AuthScopeRoot(cityRoot, scopeRoot, target), strings.TrimSpace(target.User), host, port)
	return host, port, target.Database, auth.User, auth.Password, nil
}

func buildDoltDSN(user, password, host string, port int, database string) string {
	user = strings.TrimSpace(user)
	if user == "" {
		user = "root"
	}
	cfg := mysql.Config{
		User:                 user,
		Passwd:               password,
		Net:                  "tcp",
		Addr:                 fmt.Sprintf("%s:%d", host, port),
		DBName:               database,
		AllowNativePasswords: true,
		ParseTime:            true,
		Timeout:              10 * time.Second,
	}
	return cfg.FormatDSN()
}

// prefetchedDepStore wraps a pre-fetched dep map to satisfy the beads.Store
// interface for collectWorkflowDeps, which calls store.DepList().
type prefetchedDepStore struct {
	beads.Store // embed nil Store — only DepList is called
	deps        map[string][]beads.Dep
}

func (s *prefetchedDepStore) DepList(id, direction string) ([]beads.Dep, error) {
	if direction == "down" {
		return s.deps[id], nil
	}
	// "up" direction — reverse lookup
	var result []beads.Dep
	for _, deps := range s.deps {
		for _, d := range deps {
			if d.DependsOnID == id {
				result = append(result, d)
			}
		}
	}
	return result, nil
}
