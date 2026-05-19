package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/doltauth"
	"github.com/gastownhall/gascity/internal/fsys"
)

type workflowSQLStoreCandidate struct {
	info workflowStoreInfo
	path string
}

func workflowSQLCandidatesForWorkflowID(
	state State,
	workflowID, requestedScopeKind, requestedScopeRef string,
) []workflowSQLStoreCandidate {
	requestedScopeKind = strings.TrimSpace(requestedScopeKind)
	requestedScopeRef = strings.TrimSpace(requestedScopeRef)
	if requestedScopeKind != "" && requestedScopeRef != "" {
		return workflowSQLStoreCandidates(state, requestedScopeKind, requestedScopeRef)
	}

	if prefix := beadPrefix(strings.TrimSpace(workflowID)); prefix != "" {
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

	// Query 1: All workflow beads (root + children by gc.root_bead_id metadata)
	beadRows, err := db.Query(`
		SELECT
			i.id, i.title, i.status, i.issue_type, i.assignee,
			i.description, i.created_at, i.updated_at,
			i.metadata
		FROM issues i
		WHERE i.id = ?
		   OR JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$."gc.root_bead_id"')) = ?
		ORDER BY i.created_at
	`, rootID, rootID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("beads query: %w", err)
	}
	defer beadRows.Close() //nolint:errcheck // best-effort cleanup

	var workflowBeads []beads.Bead
	beadIndex := make(map[string]beads.Bead)
	beadIDs := make([]string, 0, 100)

	for beadRows.Next() {
		var b beads.Bead
		var assignee, description sql.NullString
		var metadataJSON []byte
		var createdAt, updatedAt time.Time

		if err := beadRows.Scan(
			&b.ID, &b.Title, &b.Status, &b.Type, &assignee,
			&description, &createdAt, &updatedAt,
			&metadataJSON,
		); err != nil {
			return nil, nil, nil, fmt.Errorf("bead scan: %w", err)
		}

		b.Assignee = assignee.String
		b.Description = description.String
		b.CreatedAt = createdAt

		// Parse JSON metadata
		if len(metadataJSON) > 0 {
			b.Metadata = make(map[string]string)
			var raw map[string]interface{}
			if json.Unmarshal(metadataJSON, &raw) == nil {
				for k, v := range raw {
					if s, ok := v.(string); ok {
						b.Metadata[k] = s
					} else {
						// Non-string values: marshal back to string
						if encoded, err := json.Marshal(v); err == nil {
							b.Metadata[k] = string(encoded)
						}
					}
				}
			}
		}

		workflowBeads = append(workflowBeads, b)
		beadIndex[b.ID] = b
		beadIDs = append(beadIDs, b.ID)
	}
	if err := beadRows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("bead rows: %w", err)
	}

	if len(beadIDs) == 0 {
		return nil, nil, nil, fmt.Errorf("no beads found for workflow %s", rootID)
	}

	// Query 2: All deps between workflow beads
	// Use subquery instead of IN (?,?,...) — dolt handles subqueries much
	// faster than large parameter lists (13s vs 46ms for 95 IDs).
	depRows, err := db.Query(`
		SELECT d.issue_id, d.depends_on_id, d.type
		FROM dependencies d
		WHERE d.issue_id IN (
			SELECT i.id FROM issues i
			WHERE i.id = ? OR JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$."gc.root_bead_id"')) = ?
		)
		AND d.depends_on_id IN (
			SELECT i.id FROM issues i
			WHERE i.id = ? OR JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$."gc.root_bead_id"')) = ?
		)
	`, rootID, rootID, rootID, rootID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("deps query: %w", err)
	}
	defer depRows.Close() //nolint:errcheck // best-effort cleanup

	depMap := make(map[string][]beads.Dep)
	for depRows.Next() {
		var d beads.Dep
		if err := depRows.Scan(&d.IssueID, &d.DependsOnID, &d.Type); err != nil {
			return nil, nil, nil, fmt.Errorf("dep scan: %w", err)
		}
		depMap[d.IssueID] = append(depMap[d.IssueID], d)
	}
	if err := depRows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("dep rows: %w", err)
	}

	// Query 3: Labels for workflow beads
	labelRows, err := db.Query(`
		SELECT l.issue_id, l.label
		FROM labels l
		WHERE l.issue_id IN (
			SELECT i.id FROM issues i
			WHERE i.id = ? OR JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$."gc.root_bead_id"')) = ?
		)
	`, rootID, rootID)
	if err != nil {
		// Non-fatal — labels are optional
		return workflowBeads, beadIndex, depMap, nil
	}
	defer labelRows.Close() //nolint:errcheck // best-effort cleanup

	labelMap := make(map[string][]string)
	for labelRows.Next() {
		var issueID, label string
		if err := labelRows.Scan(&issueID, &label); err != nil {
			continue
		}
		labelMap[issueID] = append(labelMap[issueID], label)
	}

	// Attach labels to beads
	for i := range workflowBeads {
		if labels, ok := labelMap[workflowBeads[i].ID]; ok {
			workflowBeads[i].Labels = labels
			beadIndex[workflowBeads[i].ID] = workflowBeads[i]
		}
	}

	return workflowBeads, beadIndex, depMap, nil
}

// tryFullWorkflowSQL does the entire workflow snapshot via SQL — root
// discovery, bead fetch, dep fetch, and graph build. Falls back to nil
// error only on full success so the caller can use the slow path on any failure.
func (s *Server) tryFullWorkflowSQL(workflowID, fallbackScopeKind, fallbackScopeRef string, snapshotIndex uint64) (*workflowSnapshotResponse, error) {
	candidates := workflowSQLCandidatesForWorkflowID(
		s.state,
		workflowID,
		fallbackScopeKind,
		fallbackScopeRef,
	)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no sql workflow stores")
	}

	type sqlWorkflowRootMatch struct {
		candidate workflowSQLStoreCandidate
		root      beads.Bead
	}
	matches := make([]sqlWorkflowRootMatch, 0, len(candidates))
	for _, candidate := range candidates {
		host, port, database, user, password, err := resolveDoltConnection(s.state.CityPath(), candidate.path)
		if err != nil {
			continue
		}
		root, ok, err := workflowSQLFindRoot(user, password, host, port, database, workflowID)
		if err != nil || !ok {
			continue
		}
		matches = append(matches, sqlWorkflowRootMatch{candidate: candidate, root: root})
	}
	if len(matches) == 0 {
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

	scopeKind := fallbackScopeKind
	scopeRef := fallbackScopeRef
	if sk := strings.TrimSpace(root.Metadata["gc.scope_kind"]); sk != "" {
		scopeKind = sk
	}
	if sr := strings.TrimSpace(root.Metadata["gc.scope_ref"]); sr != "" {
		scopeRef = sr
	}

	storeRef := chosen.info.ref
	beadResponses := make([]workflowBeadResponse, 0, len(workflowBeads))
	for _, bead := range workflowBeads {
		beadResponses = append(beadResponses, workflowBeadResponse{
			ID:            bead.ID,
			Title:         bead.Title,
			Status:        workflowStatus(bead),
			Kind:          workflowKind(bead),
			StepRef:       strings.TrimSpace(bead.Metadata["gc.step_ref"]),
			Attempt:       workflowAttempt(bead),
			LogicalBeadID: strings.TrimSpace(bead.Metadata["gc.logical_bead_id"]),
			ScopeRef:      strings.TrimSpace(bead.Metadata["gc.scope_ref"]),
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

func workflowSQLFindRoot(user, password, host string, port int, database, workflowID string) (beads.Bead, bool, error) {
	if root, ok, err := workflowSQLGetBead(user, password, host, port, database, workflowID); err != nil {
		return beads.Bead{}, false, err
	} else if ok {
		if isWorkflowRoot(root) && matchesWorkflowID(root, workflowID) {
			return root, true, nil
		}
		if beadPrefix(workflowID) != "" {
			return beads.Bead{}, false, nil
		}
	}
	if beadPrefix(workflowID) != "" {
		return beads.Bead{}, false, nil
	}

	db, err := openWorkflowSQLDB(user, password, host, port, database)
	if err != nil {
		return beads.Bead{}, false, err
	}
	defer db.Close() //nolint:errcheck // best-effort cleanup

	row := db.QueryRow(`
		SELECT
			i.id, i.title, i.status, i.issue_type, i.assignee,
			i.description, i.created_at, i.updated_at,
			i.metadata
		FROM issues i
		WHERE JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$."gc.kind"')) = 'workflow'
		  AND JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$."gc.workflow_id"')) = ?
		ORDER BY i.created_at
		LIMIT 1
	`, workflowID)
	bead, ok, err := workflowSQLScanBead(row.Scan)
	if err != nil || !ok {
		return beads.Bead{}, ok, err
	}
	return bead, true, nil
}

func workflowSQLGetBead(user, password, host string, port int, database, id string) (beads.Bead, bool, error) {
	db, err := openWorkflowSQLDB(user, password, host, port, database)
	if err != nil {
		return beads.Bead{}, false, err
	}
	defer db.Close() //nolint:errcheck // best-effort cleanup

	row := db.QueryRow(`
		SELECT
			i.id, i.title, i.status, i.issue_type, i.assignee,
			i.description, i.created_at, i.updated_at,
			i.metadata
		FROM issues i
		WHERE i.id = ?
		LIMIT 1
	`, id)
	return workflowSQLScanBead(row.Scan)
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
		User:      user,
		Passwd:    password,
		Net:       "tcp",
		Addr:      fmt.Sprintf("%s:%d", host, port),
		DBName:    database,
		ParseTime: true,
		Timeout:   10 * time.Second,
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
