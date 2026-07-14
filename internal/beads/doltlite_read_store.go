//go:build gascity_native_beads

package beads

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, CGO_ENABLED=0 safe
)

// DoltliteReadStore serves hot read paths in-process for bd/doltlite stores.
// Writes and less common operations delegate to the normal bd CLI store.
type DoltliteReadStore struct {
	*BdStore
	db              *sql.DB
	orderRunMu      sync.Mutex
	orderRunLastRun map[string]time.Time
	orderRunOpen    map[string]bool
	orderRunHash    string
	sessionMu       sync.Mutex
	sessionCache    []Bead
	sessionHash     string
	readyMu         sync.Mutex
	readyCache      map[string][]Bead
	readyHash       string
}

func (s *DoltliteReadStore) NeedsSessionTypeFallback() bool { return true }

type doltliteMetadata struct {
	Backend      string `json:"backend"`
	Database     string `json:"database"`
	DoltDatabase string `json:"dolt_database"`
}

type doltliteTableSet struct {
	issues string
	labels string
	deps   string
	// wisps marks the wisp-backed table set. Snapshots written before the
	// storage-flag columns existed have no per-row discriminator there, in
	// which case every row in the set is ephemeral.
	wisps bool
}

var (
	doltliteIssueTables = doltliteTableSet{issues: "issues", labels: "labels", deps: "dependencies"}
	doltliteWispTables  = doltliteTableSet{issues: "wisps", labels: "wisp_labels", deps: "wisp_dependencies", wisps: true}
)

// doltliteTableSetsForMode maps a TierMode to the storage tables that can hold
// matching rows. TierIssues spans both tables because non-ephemeral
// (no_history) wisps rows belong to the durable tier (query.go TierMode
// contract, #3444); queryIssueTable applies the per-row storage-flag filter.
func doltliteTableSetsForMode(mode TierMode) []doltliteTableSet {
	switch mode {
	case TierWisps:
		return []doltliteTableSet{doltliteWispTables}
	default: // TierIssues, TierBoth
		return []doltliteTableSet{doltliteIssueTables, doltliteWispTables}
	}
}

func (s *DoltliteReadStore) doltliteReadyIssueWhere(tables doltliteTableSet) (string, []any) {
	return doltliteReadyIssueWhere(tables, s.tableExists(doltliteWispTables.issues))
}

func doltliteReadyIssueWhere(tables doltliteTableSet, includeWispTargets bool) (string, []any) {
	typePredicate, args := doltliteIssueTypeNotInPredicate("i")
	blockingTypes := make([]string, 0, len(readyBlockingDependencyTypes))
	for typ := range readyBlockingDependencyTypes {
		blockingTypes = append(blockingTypes, typ)
	}
	sort.Strings(blockingTypes)
	blockingPlaceholders := strings.TrimRight(strings.Repeat("?,", len(blockingTypes)), ",")
	for _, typ := range blockingTypes {
		args = append(args, typ)
	}

	issueTarget := "COALESCE(NULLIF(d.depends_on_issue_id, ''), NULLIF(d.depends_on_id, ''), NULLIF(d.depends_on_external, ''), '')"
	wispTarget := "NULLIF(d.depends_on_wisp_id, '')"
	depType := "COALESCE(NULLIF(d.type, ''), 'blocks')"
	blockerJoins := "LEFT JOIN " + tables.issues + " blocker_issue ON blocker_issue.id = " + issueTarget
	blockerStatus := "COALESCE(blocker_issue.status, '')"
	if includeWispTargets {
		blockerJoins += "\n\t\t\tLEFT JOIN " + doltliteWispTables.issues + " blocker_wisp ON blocker_wisp.id = " + wispTarget
		blockerStatus = "CASE WHEN " + wispTarget + " IS NOT NULL THEN COALESCE(blocker_wisp.status, '') ELSE COALESCE(blocker_issue.status, '') END"
	}

	return strings.Join([]string{
		typePredicate,
		`NOT EXISTS (
				SELECT 1 FROM ` + tables.deps + ` d
				` + blockerJoins + `
				WHERE d.issue_id = i.id AND ` + depType + ` IN (` + blockingPlaceholders + `) AND ` + blockerStatus + ` != 'closed'
			)`,
	}, " AND "), args
}

func doltliteIssueTypeNotInPredicate(alias string) (string, []any) {
	excluded := make([]string, 0, len(readyExcludeTypes))
	for typ := range readyExcludeTypes {
		excluded = append(excluded, typ)
	}
	sort.Strings(excluded)
	placeholders := strings.TrimRight(strings.Repeat("?,", len(excluded)), ",")
	args := make([]any, 0, len(excluded))
	for _, typ := range excluded {
		args = append(args, typ)
	}
	return "COALESCE(" + alias + ".issue_type, '') NOT IN (" + placeholders + ")", args
}

func NewDoltliteReadStore(dir string, backing *BdStore) (*DoltliteReadStore, error) {
	meta, err := readDoltliteMetadata(dir)
	if err != nil {
		return nil, err
	}
	dbName := strings.TrimSpace(meta.DoltDatabase)
	if dbName == "" || dbName == "doltlite" {
		dbName = strings.TrimSpace(meta.Database)
	}
	if dbName == "" || dbName == "doltlite" {
		dbName = "hq"
	}
	dbPath := filepath.Join(dir, ".beads", "doltlite", dbName+".db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_busy_timeout=10000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DoltliteReadStore{BdStore: backing, db: db}, nil
}

func readDoltliteMetadata(dir string) (doltliteMetadata, error) {
	var meta doltliteMetadata
	data, err := os.ReadFile(filepath.Join(dir, ".beads", "metadata.json"))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	if !isDoltliteMetadata(meta.Backend, meta.Database) {
		return meta, fmt.Errorf("not a doltlite beads store")
	}
	return meta, nil
}

func (s *DoltliteReadStore) CloseStore() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *DoltliteReadStore) Get(id string) (Bead, error) {
	beads, err := s.queryIssues(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth}, "i.id = ?", []any{id}, 1)
	if err != nil {
		return Bead{}, err
	}
	if len(beads) == 0 {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
	}
	return beads[0], nil
}

func (s *DoltliteReadStore) GetSessionBead(id string) (Bead, error) {
	sessions, err := s.ListSessionBeads()
	if err == nil {
		for _, session := range sessions {
			if session.ID == id {
				return session, nil
			}
		}
	}
	beads, err := s.queryIssues(ListQuery{
		AllowScan:     true,
		IncludeClosed: true,
		SkipLabels:    true,
	}, "i.id = ?", []any{id}, 1)
	if err != nil {
		return Bead{}, err
	}
	if len(beads) == 0 {
		return Bead{}, fmt.Errorf("getting session bead %q: %w", id, ErrNotFound)
	}
	if beads[0].Type != "session" && beads[0].Type != "" {
		return Bead{}, fmt.Errorf("getting session bead %q: %w", id, ErrNotFound)
	}
	if beads[0].Type == "" {
		return s.Get(id)
	}
	return beads[0], nil
}

func (s *DoltliteReadStore) ListSessionBeads() ([]Bead, error) {
	hash, err := s.currentDoltHash()
	if err != nil {
		return nil, err
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if hash != "" && hash == s.sessionHash && s.sessionCache != nil {
		return cloneBeads(s.sessionCache), nil
	}
	rows, err := s.queryIssues(ListQuery{
		Type:       "session",
		SkipLabels: true,
	}, "", nil, 0)
	if err != nil {
		return nil, err
	}
	s.sessionCache = cloneBeads(rows)
	s.sessionHash = hash
	return rows, nil
}

func (s *DoltliteReadStore) List(query ListQuery) ([]Bead, error) {
	if err := query.Validate(); err != nil {
		return nil, err
	}
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("bd list: %w", ErrQueryRequiresScan)
	}
	return s.queryIssues(query, "", nil, query.Limit)
}

func (s *DoltliteReadStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = strings.TrimSpace(status[0])
	}
	return s.List(query)
}

func (s *DoltliteReadStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		Sort:          SortCreatedAsc,
	})
}

func (s *DoltliteReadStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

func (s *DoltliteReadStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{
		Assignee: assignee,
		Status:   status,
		Limit:    limit,
	})
}

func (s *DoltliteReadStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

func (s *DoltliteReadStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	rq := readyQueryFromArgs(query)
	cacheKey := fmt.Sprintf("%s\x00%d", rq.Assignee, rq.Limit)
	hash, err := s.currentDoltHash()
	if err != nil {
		return nil, err
	}
	s.readyMu.Lock()
	if hash != "" && hash == s.readyHash && s.readyCache != nil {
		if cached, ok := s.readyCache[cacheKey]; ok {
			s.readyMu.Unlock()
			return cloneBeads(cached), nil
		}
	}
	s.readyMu.Unlock()

	q := ListQuery{Status: "open", AllowScan: true, IncludeClosed: false, Limit: 0, SkipLabels: true}
	if rq.Assignee != "" {
		q.Assignee = rq.Assignee
	}
	if rq.Limit > 0 {
		q.Limit = rq.Limit
	}
	readyWhere, readyArgs := s.doltliteReadyIssueWhere(doltliteIssueTables)
	// The id tiebreaker keeps a LIMIT deterministic when rows share
	// (priority, created_at) — same bug class as queryIssueTable (#3208).
	//
	// This read is issues-only and does NOT honor a policy-expanded
	// rq.TierMode. A default (TierIssues) Ready is correctly history-backed per
	// the ReadyQuery.TierMode policy in query.go. But policy-aware callers
	// expand default Ready to TierBoth (bead_policy_store.go), and
	// NativeDoltStore.Ready / BdStore.Ready surface ready no-history/ephemeral
	// rows for that expansion while this path still returns issues-only — a
	// known backend parity gap, not behavior the policy endorses. It is
	// pre-existing (the internal Ready query was already issues-only before the
	// TierIssues List span), and out of scope for the #3444/#3449 List+Count
	// alignment because honoring rq.TierMode here also needs the wisp
	// dependency graph modeled in the ready blocker predicate. Tracked as
	// follow-up ga-4ax9bj.
	out, err := s.queryIssuesOrderedInTables(q, []doltliteTableSet{doltliteIssueTables}, readyWhere, readyArgs, q.Limit, "ORDER BY COALESCE(i.priority, 2) ASC, i.created_at ASC, i.id ASC")
	if err != nil {
		return nil, err
	}
	s.readyMu.Lock()
	if hash != "" {
		if hash != s.readyHash || s.readyCache == nil {
			s.readyHash = hash
			s.readyCache = make(map[string][]Bead)
		}
		s.readyCache[cacheKey] = cloneBeads(out)
	}
	s.readyMu.Unlock()
	return out, nil
}

func (s *DoltliteReadStore) LastOrderRun(name string) (time.Time, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.Time{}, nil
	}
	hash, err := s.currentDoltHash()
	if err != nil {
		return time.Time{}, err
	}
	s.orderRunMu.Lock()
	defer s.orderRunMu.Unlock()
	if s.orderRunLastRun == nil || hash == "" || hash != s.orderRunHash {
		lastRun, openRuns, err := s.loadOrderRuns()
		if err != nil {
			return time.Time{}, err
		}
		s.orderRunLastRun = lastRun
		s.orderRunOpen = openRuns
		s.orderRunHash = hash
	}
	return s.orderRunLastRun[name], nil
}

func (s *DoltliteReadStore) loadOrderRuns() (map[string]time.Time, map[string]bool, error) {
	rows, err := s.db.Query(`SELECT l.label, MAX(i.created_at), MAX(CASE WHEN i.status != 'closed' THEN 1 ELSE 0 END)
		FROM labels l
		JOIN issues i ON i.id = l.issue_id
		WHERE l.label >= 'order-run:' AND l.label < 'order-run;'
		GROUP BY l.label`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	lastRun := make(map[string]time.Time)
	openRuns := make(map[string]bool)
	for rows.Next() {
		var label string
		var createdRaw any
		var open int
		if err := rows.Scan(&label, &createdRaw, &open); err != nil {
			return nil, nil, err
		}
		name := strings.TrimPrefix(label, "order-run:")
		if name != "" {
			lastRun[name] = parseDBTime(createdRaw).Truncate(time.Second)
			openRuns[name] = open > 0
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return lastRun, openRuns, nil
}

func (s *DoltliteReadStore) HasOpenOrderRun(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, nil
	}
	hash, err := s.currentDoltHash()
	if err != nil {
		return false, err
	}
	s.orderRunMu.Lock()
	defer s.orderRunMu.Unlock()
	if s.orderRunOpen == nil || hash == "" || hash != s.orderRunHash {
		lastRun, openRuns, err := s.loadOrderRuns()
		if err != nil {
			return false, err
		}
		s.orderRunLastRun = lastRun
		s.orderRunOpen = openRuns
		s.orderRunHash = hash
	}
	return s.orderRunOpen[name], nil
}

func (s *DoltliteReadStore) currentDoltHash() (string, error) {
	var dataVersion int64
	if err := s.db.QueryRow("PRAGMA data_version").Scan(&dataVersion); err != nil {
		return "", fmt.Errorf("doltlite data version: %w", err)
	}

	issueCount, issueUpdated, err := s.tableFingerprint("issues", true)
	if err != nil {
		return "", fmt.Errorf("doltlite issues fingerprint: %w", err)
	}
	wispCount, wispUpdated, err := s.tableFingerprint("wisps", false)
	if err != nil {
		return "", fmt.Errorf("doltlite wisps fingerprint: %w", err)
	}
	labelCount, err := s.tableCount("labels", true)
	if err != nil {
		return "", fmt.Errorf("doltlite labels fingerprint: %w", err)
	}
	wispLabelCount, err := s.tableCount("wisp_labels", false)
	if err != nil {
		return "", fmt.Errorf("doltlite wisp labels fingerprint: %w", err)
	}
	depCount, err := s.tableCount("dependencies", true)
	if err != nil {
		return "", fmt.Errorf("doltlite dependencies fingerprint: %w", err)
	}
	wispDepCount, err := s.tableCount("wisp_dependencies", false)
	if err != nil {
		return "", fmt.Errorf("doltlite wisp dependencies fingerprint: %w", err)
	}

	return fmt.Sprintf("data=%d;issues=%d:%s;wisps=%d:%s;labels=%d:%d;deps=%d:%d",
		dataVersion, issueCount, issueUpdated, wispCount, wispUpdated, labelCount, wispLabelCount, depCount, wispDepCount), nil
}

func (s *DoltliteReadStore) tableFingerprint(table string, required bool) (int64, string, error) {
	if !s.tableExists(table) {
		if required {
			return 0, "", fmt.Errorf("missing table %q", table)
		}
		return 0, "", nil
	}
	var count int64
	var maxUpdated sql.NullString
	if err := s.db.QueryRow("SELECT COUNT(*), MAX(updated_at) FROM "+table).Scan(&count, &maxUpdated); err != nil {
		return 0, "", err
	}
	if !maxUpdated.Valid {
		return count, "", nil
	}
	return count, strings.TrimSpace(maxUpdated.String), nil
}

func (s *DoltliteReadStore) tableCount(table string, required bool) (int64, error) {
	if !s.tableExists(table) {
		if required {
			return 0, fmt.Errorf("missing table %q", table)
		}
		return 0, nil
	}
	var count int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *DoltliteReadStore) resetOrderRunCache() {
	s.orderRunMu.Lock()
	defer s.orderRunMu.Unlock()
	s.orderRunLastRun = nil
	s.orderRunOpen = nil
	s.orderRunHash = ""
	s.sessionMu.Lock()
	s.sessionCache = nil
	s.sessionHash = ""
	s.sessionMu.Unlock()
	s.readyMu.Lock()
	s.readyCache = nil
	s.readyHash = ""
	s.readyMu.Unlock()
}

func (s *DoltliteReadStore) Create(b Bead) (Bead, error) {
	created, err := s.BdStore.Create(b)
	if err == nil && hasOrderRunLabel(created.Labels) {
		s.resetOrderRunCache()
	}
	return created, err
}

func hasOrderRunLabel(labels []string) bool {
	for _, label := range labels {
		if strings.HasPrefix(label, "order-run:") {
			return true
		}
	}
	return false
}

func (s *DoltliteReadStore) Update(id string, opts UpdateOpts) error {
	err := s.BdStore.Update(id, opts)
	if err == nil {
		s.resetOrderRunCache()
	}
	return err
}

func (s *DoltliteReadStore) Close(id string) error {
	err := s.BdStore.Close(id)
	if err == nil {
		s.resetOrderRunCache()
	}
	return err
}

func (s *DoltliteReadStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	n, err := s.BdStore.CloseAll(ids, metadata)
	if err == nil && n > 0 {
		s.resetOrderRunCache()
	}
	return n, err
}

func (s *DoltliteReadStore) Reopen(id string) error {
	err := s.BdStore.Reopen(id)
	if err == nil {
		s.resetOrderRunCache()
	}
	return err
}

func (s *DoltliteReadStore) Delete(id string) error {
	err := s.BdStore.Delete(id)
	if err == nil {
		s.resetOrderRunCache()
	}
	return err
}

func (s *DoltliteReadStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	current, err := s.GetSessionBead(id)
	if err != nil {
		rows, queryErr := s.queryIssues(ListQuery{
			AllowScan:     true,
			IncludeClosed: true,
			SkipLabels:    true,
		}, "i.id = ?", []any{id}, 1)
		if queryErr != nil {
			return queryErr
		}
		if len(rows) == 0 {
			return fmt.Errorf("setting metadata on %q: %w", id, ErrNotFound)
		}
		current = rows[0]
	}
	changed := make(map[string]string, len(kvs))
	for k, v := range kvs {
		if current.Metadata[k] != v {
			changed[k] = v
		}
	}
	if len(changed) == 0 {
		return nil
	}
	err = s.BdStore.SetMetadataBatch(id, changed)
	if err == nil {
		s.resetOrderRunCache()
	}
	return err
}

func (s *DoltliteReadStore) SetMetadata(id, key, value string) error {
	return s.SetMetadataBatch(id, map[string]string{key: value})
}

func (s *DoltliteReadStore) DepAdd(id, dep, depType string) error {
	err := s.BdStore.DepAdd(id, dep, depType)
	if err == nil {
		s.resetOrderRunCache()
	}
	return err
}

func (s *DoltliteReadStore) DepRemove(id, dep string) error {
	err := s.BdStore.DepRemove(id, dep)
	if err == nil {
		s.resetOrderRunCache()
	}
	return err
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func cloneBeads(values []Bead) []Bead {
	if len(values) == 0 {
		return nil
	}
	out := make([]Bead, len(values))
	for i := range values {
		out[i] = cloneBead(values[i])
	}
	return out
}

func (s *DoltliteReadStore) DepList(id, direction string) ([]Dep, error) {
	if direction == "up" {
		return s.queryDeps(doltliteDependsOnExpr()+" = ?", id)
	}
	return s.queryDeps("issue_id = ?", id)
}

func (s *DoltliteReadStore) DepListBatch(ids []string) (map[string][]Dep, error) {
	result := make(map[string][]Dep, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		placeholders := strings.TrimRight(strings.Repeat("?,", end-start), ",")
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		for _, table := range []string{"dependencies", "wisp_dependencies"} {
			if table == "wisp_dependencies" && !s.tableExists(table) {
				continue
			}
			rows, err := s.db.Query(`SELECT issue_id, `+doltliteDependsOnExpr()+`, type FROM `+table+` WHERE issue_id IN (`+placeholders+`)`, args...)
			if err != nil {
				return result, err
			}
			for rows.Next() {
				dep, err := scanDep(rows)
				if err != nil {
					_ = rows.Close()
					return result, err
				}
				result[dep.IssueID] = append(result[dep.IssueID], dep)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return result, err
			}
			if err := rows.Close(); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func (s *DoltliteReadStore) dependencySnapshotForCache(ids []string) (map[string][]Dep, bool, error) {
	deps, err := s.DepListBatch(ids)
	if err != nil {
		return deps, false, err
	}
	return deps, true, nil
}

func (s *DoltliteReadStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	// Native DoltLite snapshots do not carry bd's denormalized is_blocked
	// projection, so cached ready intentionally keeps the nil fallback.
	return items, nil
}

func (s *DoltliteReadStore) queryDeps(where, value string) ([]Dep, error) {
	var deps []Dep
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		if table == "wisp_dependencies" && !s.tableExists(table) {
			continue
		}
		rows, err := s.db.Query(`SELECT issue_id, `+doltliteDependsOnExpr()+`, type FROM `+table+` WHERE `+where, value)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			dep, err := scanDep(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			deps = append(deps, dep)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return deps, nil
}

func doltliteDependsOnExpr() string {
	return "COALESCE(NULLIF(depends_on_id, ''), NULLIF(depends_on_issue_id, ''), NULLIF(depends_on_wisp_id, ''), NULLIF(depends_on_external, ''), '')"
}

func doltliteQualifiedDependsOnExpr(alias string) string {
	prefix := ""
	if strings.TrimSpace(alias) != "" {
		prefix = alias + "."
	}
	return "COALESCE(NULLIF(" + prefix + "depends_on_id, ''), NULLIF(" + prefix + "depends_on_issue_id, ''), NULLIF(" + prefix + "depends_on_wisp_id, ''), NULLIF(" + prefix + "depends_on_external, ''), '')"
}

func scanDep(rows interface{ Scan(...any) error }) (Dep, error) {
	var dep Dep
	var issueID, dependsOnID, depType sql.NullString
	if err := rows.Scan(&issueID, &dependsOnID, &depType); err != nil {
		return dep, err
	}
	dep.IssueID = issueID.String
	dep.DependsOnID = dependsOnID.String
	dep.Type = depType.String
	if dep.Type == "" {
		dep.Type = "blocks"
	}
	return dep, nil
}

func (s *DoltliteReadStore) queryIssues(query ListQuery, extraWhere string, extraArgs []any, limit int) ([]Bead, error) {
	return s.queryIssuesOrdered(query, extraWhere, extraArgs, limit, "")
}

func (s *DoltliteReadStore) queryIssuesOrdered(query ListQuery, extraWhere string, extraArgs []any, limit int, orderBy string) ([]Bead, error) {
	return s.queryIssuesOrderedInTables(query, doltliteTableSetsForMode(query.TierMode), extraWhere, extraArgs, limit, orderBy)
}

// queryIssuesOrderedInTables runs the query against an explicit list of table
// sets. Callers passing a custom orderBy must pass a single table set: the
// merged path re-sorts only when orderBy is empty.
func (s *DoltliteReadStore) queryIssuesOrderedInTables(query ListQuery, sets []doltliteTableSet, extraWhere string, extraArgs []any, limit int, orderBy string) ([]Bead, error) {
	if err := query.Validate(); err != nil {
		return nil, err
	}
	// A custom orderBy is applied per table in SQL, and the cross-table Go
	// re-sort below runs only for the empty-orderBy default order, so a custom
	// orderBy across more than one table set would concatenate independently
	// ordered tables. Reject that shape explicitly rather than silently
	// returning cross-table-unsorted results; the only custom-orderBy caller
	// (Ready) correctly passes a single issues-only table set (#3449 review).
	if orderBy != "" && len(sets) > 1 {
		return nil, fmt.Errorf("doltlite: custom orderBy requires a single table set, got %d", len(sets))
	}
	// A bounded multi-table read can resolve its exact global top-N by
	// selecting ids in one SQL statement and hydrating only those ids, instead
	// of reading every matching row from both tables and cutting in Go. This
	// keeps the default TierIssues bounded path O(limit) rather than spanning
	// full matching history (#3449 review).
	if doltliteCanSelectBoundedTopN(query, sets, extraWhere, limit, orderBy) {
		return s.queryBoundedTopN(query, sets, limit)
	}
	merged := make([]Bead, 0)
	seen := make(map[string]struct{})
	for _, tables := range sets {
		// A per-table SQL LIMIT is an exact prefix of the merged result only
		// for a single table set. Across multiple tables the cross-table id
		// dedupe below can drop a table's whole limited prefix (an id that
		// also appears in an earlier set), so a later unique row that belongs
		// in the global top-N may never be fetched. Read every matching row
		// for multi-table merges and let the Go sort+limit cut the exact
		// top-N (#3444); the bounded fast path above avoids this full read for
		// the common limited case.
		tableLimit := limit
		if len(sets) > 1 {
			tableLimit = 0
		}
		// A seek boundary is applied Go-side (filterDoltliteBeforeTimes) after
		// this fetch; a SQL LIMIT cut before that filter would silently drop
		// page rows, so seeked reads fetch unbounded and let the Go
		// filter+sort+limit below cut the exact page.
		if query.SeekAfter != nil {
			tableLimit = 0
		}
		rows, err := s.queryIssueTable(query, tables, extraWhere, extraArgs, tableLimit, orderBy)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if _, ok := seen[row.ID]; ok {
				continue
			}
			seen[row.ID] = struct{}{}
			merged = append(merged, row)
		}
	}
	if len(query.Metadata) > 0 {
		merged = filterDoltliteMetadata(merged, query.Metadata)
	}
	merged = filterDoltliteBeforeTimes(merged, query)
	if orderBy == "" {
		sortBeadsForQuery(merged, doltliteSortOrder(query.Sort))
	}
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// doltliteCanSelectBoundedTopN reports whether a bounded multi-table read can
// resolve its exact global top-N by selecting ids in one SQL statement (then
// hydrating only those ids). It is restricted to the shapes whose result is
// fully determined by SQL-expressible predicates and the standard
// (created_at, id) sort, so the SQL LIMIT is exact. A custom orderBy, caller
// extraWhere, or a ParentID/metadata/before-time filter all need post-SQL Go
// work that a bounded SQL selection could truncate incorrectly, so those fall
// back to the full-read merge.
func doltliteCanSelectBoundedTopN(query ListQuery, sets []doltliteTableSet, extraWhere string, limit int, orderBy string) bool {
	return limit > 0 &&
		len(sets) > 1 &&
		orderBy == "" &&
		extraWhere == "" &&
		query.ParentID == "" &&
		len(query.Metadata) == 0 &&
		query.CreatedBefore.IsZero() &&
		query.UpdatedBefore.IsZero() &&
		query.SeekAfter == nil
}

// queryBoundedTopN resolves a bounded multi-table read by selecting the exact
// global top-N ids in one SQL statement and hydrating only those ids.
func (s *DoltliteReadStore) queryBoundedTopN(query ListQuery, sets []doltliteTableSet, limit int) ([]Bead, error) {
	ids, err := s.selectBoundedTopNIDs(query, sets, limit)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []Bead{}, nil
	}
	merged, err := s.hydrateBeadsByID(query, sets, ids)
	if err != nil {
		return nil, err
	}
	sortBeadsForQuery(merged, doltliteSortOrder(query.Sort))
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// selectBoundedTopNIDs returns the ids of the exact global top-N rows for a
// bounded multi-table read, ordered by the query's (created_at, id) sort. It
// selects ids from the issues table plus the non-duplicate non-ephemeral wisps
// rows — anti-joined against the matching issues set exactly as List dedupes
// and as countDurableWisps counts — under one global ORDER BY/LIMIT, so the
// SQL bound stays O(limit) instead of hydrating full matching history (#3449).
func (s *DoltliteReadStore) selectBoundedTopNIDs(query ListQuery, sets []doltliteTableSet, limit int) ([]string, error) {
	// The issues-table predicates back both the issues union leg and the wisps
	// dedupe anti-join, so the wisp leg excludes exactly the ids a matching
	// issues row already contributes (List's "issues win" cross-table dedupe).
	issuesTQ, err := s.buildDoltliteTableQuery(query, doltliteIssueTables, "", nil)
	if err != nil {
		return nil, err
	}
	unionParts := make([]string, 0, len(sets))
	args := make([]any, 0)
	for _, tables := range sets {
		if tables.wisps && !s.tableExists(tables.issues) {
			continue
		}
		tq := issuesTQ
		if tables.wisps {
			tq, err = s.buildDoltliteTableQuery(query, tables, "", nil)
			if err != nil {
				return nil, err
			}
		}
		if tq.skipTable {
			continue
		}
		legWhere := append([]string{}, tq.where...)
		legArgs := append([]any{}, tq.args...)
		if tables.wisps && !issuesTQ.skipTable {
			antiJoin, antiArgs := doltliteMatchingIssuesAntiJoin(issuesTQ.where, issuesTQ.args)
			legWhere = append(legWhere, antiJoin)
			legArgs = append(legArgs, antiArgs...)
		}
		// Select created_at truncated to whole seconds, not the raw sub-second
		// text: the outer LIMIT must cut on the same second-granular key the Go
		// merge and post-hydration re-sort compare, or the bounded prefix can
		// diverge from the unbounded merge for same-second rows (#3449 review).
		leg := "SELECT i.id AS id, " + doltliteCreatedAtSortKey("i") + " AS created_at FROM " + tables.issues + " i"
		if len(legWhere) > 0 {
			leg += " WHERE " + strings.Join(legWhere, " AND ")
		}
		unionParts = append(unionParts, leg)
		args = append(args, legArgs...)
	}
	if len(unionParts) == 0 {
		return nil, nil
	}
	// The id tiebreaker matches sortBeadsForQuery's (created_at, id) total
	// order so the LIMIT cuts the same deterministic prefix the Go merge would
	// (#3208). created_at here is already the second-granular sort key.
	order := "DESC"
	if query.Sort == SortCreatedAsc {
		order = "ASC"
	}
	sqlText := "SELECT id FROM (" + strings.Join(unionParts, " UNION ALL ") +
		") ORDER BY created_at " + order + ", id " + order +
		fmt.Sprintf(" LIMIT %d", limit)
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// doltliteMaxHydrateIDsPerChunk bounds how many id placeholders a single
// hydrateBeadsByID query may bind. The native DoltLite driver
// (modernc.org/sqlite) caps a statement at SQLITE_MAX_VARIABLE_NUMBER = 32766
// bound parameters. A bounded read's id selection is limited only by the query
// limit, and the API derives that limit from an uncapped cursor offset
// (internal/api/pagination.go), so a deep page can select tens of thousands of
// ids. Hydrate the IN (...) clause in chunks well under the driver cap, leaving
// headroom for the query's own tier/status/label/assignee predicate parameters,
// so a deep cursor walk over a large rig cannot overflow the statement variable
// limit (#3449 review).
const doltliteMaxHydrateIDsPerChunk = 16000

// hydrateBeadsByID fetches full bead rows for ids from each table set, applying
// query's filters and deduping by id (issues-table rows win), exactly as the
// full-read merge does. The id set is hydrated in chunks bounded by
// doltliteMaxHydrateIDsPerChunk so a large bounded selection cannot exceed the
// SQLite bound-parameter limit. The dedupe spans every chunk and table set, so
// an issues-table row still wins over its wisp twin regardless of where the
// chunk boundaries fall.
func (s *DoltliteReadStore) hydrateBeadsByID(query ListQuery, sets []doltliteTableSet, ids []string) ([]Bead, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	merged := make([]Bead, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, tables := range sets {
		for start := 0; start < len(ids); start += doltliteMaxHydrateIDsPerChunk {
			end := start + doltliteMaxHydrateIDsPerChunk
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]
			placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
			args := make([]any, len(chunk))
			for i, id := range chunk {
				args[i] = id
			}
			rows, err := s.queryIssueTable(query, tables, "i.id IN ("+placeholders+")", args, 0, "")
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				if _, ok := seen[row.ID]; ok {
					continue
				}
				seen[row.ID] = struct{}{}
				merged = append(merged, row)
			}
		}
	}
	return merged, nil
}

func doltliteSortOrder(order SortOrder) SortOrder {
	if order == SortCreatedAsc {
		return SortCreatedAsc
	}
	return SortCreatedDesc
}

// doltliteCreatedAtSortKey returns the SQL expression for a row's created_at
// truncated to whole seconds for the given table alias, matching scanBead's
// parseDBTime(...).Truncate(time.Second). SQL LIMIT cuts that feed a bounded
// read must order by this key, not the raw sub-second created_at text: the Go
// merge and post-hydration re-sort compare the truncated CreatedAt, so cutting
// on raw sub-second precision could select a different same-second prefix than
// the unbounded merge (#3449 review). The first 19 chars span
// "YYYY-MM-DD?HH:MM:SS" for every layout parseTimeString accepts, where the 11th
// char is the date/time separator: RFC3339 stores 'T' (0x54) and
// time.Time.String() stores ' ' (0x20). Both are valid on-disk layouts, so a raw
// prefix sorts every space-separated row before every 'T' row that shares a date
// regardless of the actual time, cutting a different bounded prefix than the Go
// merge's instant comparison. Normalizing 'T' to ' ' makes the prefix sort by
// second exactly as the parsed, truncated time does for either layout (#3449
// post-merge review).
func doltliteCreatedAtSortKey(alias string) string {
	return "replace(substr(" + alias + ".created_at, 1, 19), 'T', ' ')"
}

// doltliteMatchingIssuesAntiJoin returns the "i.id NOT IN (SELECT i.id FROM
// issues i ...)" predicate and its args that drop every wisp whose durable
// issue twin the issues-table pass already contributes, reproducing List's
// "issues win" cross-table dedupe. The inner query re-aliases the issues table
// as i so its predicates resolve against issues, shadowing the outer wisps i.
// issuesWhere must be the issues-table pass before any post-List excludeTypes
// filter, because List dedupes wisps against the merged issues set before
// excludeTypes runs (#3449 review). Shared by the bounded List id selection and
// countDurableWisps so the dedupe shape has one source of truth.
func doltliteMatchingIssuesAntiJoin(issuesWhere []string, issuesArgs []any) (string, []any) {
	dedupe := "SELECT i.id FROM " + doltliteIssueTables.issues + " i"
	if len(issuesWhere) > 0 {
		dedupe += " WHERE " + strings.Join(issuesWhere, " AND ")
	}
	return "i.id NOT IN (" + dedupe + ")", append([]any{}, issuesArgs...)
}

// doltliteMetadataFilterPredicates narrows metadata queries in SQL without
// relying on SQLite JSON1, which is not available in every embedded build.
// scanBead still applies exact parseMetadata filtering to these candidates.
func doltliteMetadataFilterPredicates(filters map[string]string) ([]string, []any) {
	if len(filters) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(filters))
	for key := range filters {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	where := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys)*2)
	for _, key := range keys {
		patterns := doltliteMetadataLikePatterns(key, filters[key])
		clauses := make([]string, 0, len(patterns))
		for _, pattern := range patterns {
			clauses = append(clauses, "i.metadata LIKE ? ESCAPE '\\'")
			args = append(args, pattern)
		}
		where = append(where, "("+strings.Join(clauses, " OR ")+")")
	}
	return where, args
}

func doltliteMetadataLikePatterns(key, value string) []string {
	keyJSON, _ := json.Marshal(key)
	valueJSON, _ := json.Marshal(value)
	fragments := []string{
		string(keyJSON) + ":" + string(valueJSON),
		string(keyJSON) + ": " + string(valueJSON),
		string(keyJSON) + " :" + string(valueJSON),
		string(keyJSON) + " : " + string(valueJSON),
	}
	patterns := make([]string, 0, len(fragments))
	seen := make(map[string]struct{}, len(fragments))
	for _, fragment := range fragments {
		pattern := "%" + escapeDoltliteLikePattern(fragment) + "%"
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		patterns = append(patterns, pattern)
	}
	return patterns
}

func escapeDoltliteLikePattern(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func filterDoltliteMetadata(rows []Bead, filters map[string]string) []Bead {
	if len(filters) == 0 || len(rows) == 0 {
		return rows
	}
	out := rows[:0]
	for _, row := range rows {
		if matchesMetadata(row, filters) {
			out = append(out, row)
		}
	}
	return out
}

// doltliteTableQuery holds the resolved per-table-set SQL fragments shared by
// full-row reads (queryIssueTable) and id-only top-N selection
// (selectBoundedTopNIDs), so both apply identical filters from one source of
// truth and cannot drift.
type doltliteTableQuery struct {
	flags      doltliteStorageFlagExprs
	where      []string
	args       []any
	parentJoin string
	// skipTable reports that this table set cannot hold any rows for the
	// query's tier (e.g. a legacy ephemeral-only wisps table under TierIssues).
	skipTable bool
}

// buildDoltliteTableQuery resolves the storage-flag expressions, WHERE
// clauses, args, and parent join for one storage table set. It returns
// skipTable=true when the tier predicate excludes the whole table.
func (s *DoltliteReadStore) buildDoltliteTableQuery(query ListQuery, tables doltliteTableSet, extraWhere string, extraArgs []any) (doltliteTableQuery, error) {
	flags, err := s.storageFlagExprsFor(tables)
	if err != nil {
		return doltliteTableQuery{}, err
	}
	tierWhere, skipTable := doltliteTierPredicate(query.TierMode, tables, flags)
	if skipTable {
		return doltliteTableQuery{skipTable: true}, nil
	}
	where := []string{}
	args := []any{}
	if tierWhere != "" {
		where = append(where, tierWhere)
	}
	if !query.IncludeClosed && query.Status != "closed" {
		where = append(where, "i.status != 'closed'")
	}
	if query.Status != "" {
		where = append(where, "i.status = ?")
		args = append(args, query.Status)
	}
	if query.Type != "" {
		where = append(where, "i.issue_type = ?")
		args = append(args, query.Type)
	}
	if query.Assignee != "" {
		where = append(where, "i.assignee = ?")
		args = append(args, query.Assignee)
	}
	if len(query.Assignees) > 0 {
		assignees := compactStrings(query.Assignees)
		if len(assignees) == 0 {
			return doltliteTableQuery{skipTable: true}, nil
		}
		placeholders := strings.TrimRight(strings.Repeat("?,", len(assignees)), ",")
		where = append(where, "i.assignee IN ("+placeholders+")")
		for _, assignee := range assignees {
			args = append(args, assignee)
		}
	}
	if query.ParentID != "" {
		where = append(where, doltliteQualifiedDependsOnExpr("pc")+" = ?")
		args = append(args, query.ParentID)
	}
	if query.Label != "" {
		where = append(where, "EXISTS (SELECT 1 FROM "+tables.labels+" l WHERE l.issue_id = i.id AND l.label = ?)")
		args = append(args, query.Label)
	}
	if len(query.Metadata) > 0 {
		metadataWhere, metadataArgs := doltliteMetadataFilterPredicates(query.Metadata)
		where = append(where, metadataWhere...)
		args = append(args, metadataArgs...)
	}
	if !query.CreatedBefore.IsZero() {
		where = append(where, "julianday(i.created_at) < julianday(?)")
		args = append(args, doltliteSQLiteTime(query.CreatedBefore))
	}
	if !query.UpdatedBefore.IsZero() {
		where = append(where, "julianday(COALESCE(NULLIF(i.updated_at, ''), i.created_at)) < julianday(?)")
		args = append(args, doltliteSQLiteTime(query.UpdatedBefore))
	}
	if extraWhere != "" {
		where = append(where, extraWhere)
		args = append(args, extraArgs...)
	}
	parentJoin := " LEFT JOIN " + tables.deps + " pc ON pc.issue_id = i.id AND pc.type = 'parent-child'"
	return doltliteTableQuery{flags: flags, where: where, args: args, parentJoin: parentJoin}, nil
}

func (s *DoltliteReadStore) queryIssueTable(query ListQuery, tables doltliteTableSet, extraWhere string, extraArgs []any, limit int, orderBy string) ([]Bead, error) {
	if tables.wisps && !s.tableExists(tables.issues) {
		return nil, nil
	}
	tq, err := s.buildDoltliteTableQuery(query, tables, extraWhere, extraArgs)
	if err != nil {
		return nil, err
	}
	if tq.skipTable {
		return nil, nil
	}
	parentColumn := doltliteQualifiedDependsOnExpr("pc")
	sqlText := `SELECT i.id, COALESCE(i.title, ''), COALESCE(i.status, ''), COALESCE(i.issue_type, ''), i.priority, i.created_at,
		COALESCE(i.updated_at, ''), COALESCE(i.assignee, ''), COALESCE(i.description, ''), COALESCE(i.metadata, '{}'),
		` + parentColumn + `, ` + tq.flags.ephemeral + `, ` + tq.flags.noHistory + `
		FROM ` + tables.issues + ` i` + tq.parentJoin
	if len(tq.where) > 0 {
		sqlText += " WHERE " + strings.Join(tq.where, " AND ")
	}
	// The id tiebreaker matches sortBeadsForQuery's (created_at, id) total
	// order so a SQL LIMIT cuts a deterministic prefix even when rows share
	// a created_at timestamp (#3208). Order on the second-granular created_at
	// key, not the raw sub-second text, so a single-table bounded read cuts the
	// same prefix the Go re-sort over the truncated CreatedAt would (#3449
	// review).
	if orderBy != "" {
		sqlText += " " + orderBy
	} else if query.Sort == SortCreatedAsc {
		sqlText += " ORDER BY " + doltliteCreatedAtSortKey("i") + " ASC, i.id ASC"
	} else {
		sqlText += " ORDER BY " + doltliteCreatedAtSortKey("i") + " DESC, i.id DESC"
	}
	if limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.Query(sqlText, tq.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var beads []Bead
	for rows.Next() {
		b, err := scanBead(rows)
		if err != nil {
			return nil, err
		}
		beads = append(beads, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !query.SkipLabels {
		if err := s.hydrateLabels(beads, tables.labels); err != nil {
			return nil, err
		}
	}
	return beads, nil
}

// doltliteStorageFlagExprs holds SQL expressions yielding the per-row
// ephemeral and no_history storage flags for one storage table, accounting
// for snapshots whose schema predates those columns.
type doltliteStorageFlagExprs struct {
	ephemeral string
	noHistory string
	// hasColumns reports whether the table carries at least one storage-flag
	// column, i.e. whether per-row tier classification is possible.
	hasColumns bool
}

// storageFlagExprsFor resolves the storage-flag expressions for tables.
// Legacy snapshots wrote wisps tables without the flag columns; every row
// there is ephemeral, so the wisps fallback is the constant 1 while the
// issues-table fallback is the constant 0 (durable history rows). A probe
// failure is propagated rather than treated as an absent column, so a
// transient DB error cannot silently downgrade tier classification.
func (s *DoltliteReadStore) storageFlagExprsFor(tables doltliteTableSet) (doltliteStorageFlagExprs, error) {
	flags := doltliteStorageFlagExprs{ephemeral: "0", noHistory: "0"}
	if tables.wisps {
		flags.ephemeral = "1"
	}
	hasEphemeral, err := s.tableHasColumn(tables.issues, "ephemeral")
	if err != nil {
		return doltliteStorageFlagExprs{}, err
	}
	if hasEphemeral {
		flags.ephemeral = "COALESCE(i.ephemeral, 0)"
		flags.hasColumns = true
	}
	hasNoHistory, err := s.tableHasColumn(tables.issues, "no_history")
	if err != nil {
		return doltliteStorageFlagExprs{}, err
	}
	if hasNoHistory {
		flags.noHistory = "COALESCE(i.no_history, 0)"
		flags.hasColumns = true
	}
	return flags, nil
}

// doltliteTierPredicate translates query.go's TierMode row filter (Matches)
// into a SQL predicate for one storage table. It returns skipTable=true when
// the table cannot hold rows for the tier at all (a legacy wisps table is
// ephemeral-only, so the durable tier never reads it).
func doltliteTierPredicate(mode TierMode, tables doltliteTableSet, flags doltliteStorageFlagExprs) (string, bool) {
	switch mode {
	case TierWisps:
		if !flags.hasColumns {
			// Legacy wisps rows are all ephemeral; issues-table rows never
			// reach TierWisps because doltliteTableSetsForMode excludes them.
			return "", false
		}
		return "(" + flags.ephemeral + " = 1 OR " + flags.noHistory + " = 1)", false
	case TierBoth:
		return "", false
	default: // TierIssues keeps history and no-history rows, drops ephemeral.
		if tables.wisps && !flags.hasColumns {
			return "", true
		}
		if flags.ephemeral == "0" {
			return "", false
		}
		return flags.ephemeral + " = 0", false
	}
}

func doltliteSQLiteTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.999999999-07:00")
}

func filterDoltliteBeforeTimes(rows []Bead, query ListQuery) []Bead {
	if len(rows) == 0 || (query.CreatedBefore.IsZero() && query.UpdatedBefore.IsZero() && query.SeekAfter == nil) {
		return rows
	}
	out := rows[:0]
	for _, row := range rows {
		if !query.CreatedBefore.IsZero() && !row.CreatedAt.Before(query.CreatedBefore) {
			continue
		}
		if !query.UpdatedBefore.IsZero() && !beadUpdatedReferenceTime(row).Before(query.UpdatedBefore) {
			continue
		}
		// Exact Go-side seek: the compound (created_at, id) boundary is
		// resolved here rather than in SQL so the tie-break stays identical to
		// the in-memory sort, so the fetch above is a superset and this is
		// where the page boundary is enforced (before the Go limit).
		if query.SeekAfter != nil && !query.SeekAfter.After(row, query.Sort) {
			continue
		}
		out = append(out, row)
	}
	return out
}

func scanBead(rows interface{ Scan(...any) error }) (Bead, error) {
	var (
		b           Bead
		priority    sql.NullInt64
		createdRaw  any
		updatedRaw  any
		metadataRaw string
		ephemeral   int64
		noHistory   int64
	)
	if err := rows.Scan(&b.ID, &b.Title, &b.Status, &b.Type, &priority, &createdRaw, &updatedRaw, &b.Assignee, &b.Description, &metadataRaw, &b.ParentID, &ephemeral, &noHistory); err != nil {
		return b, err
	}
	if priority.Valid {
		p := int(priority.Int64)
		b.Priority = &p
	}
	b.Status = mapBdStatus(b.Status)
	b.CreatedAt = parseDBTime(createdRaw).Truncate(time.Second)
	b.UpdatedAt = parseDBTime(updatedRaw).Truncate(time.Second)
	b.Metadata = parseMetadata(metadataRaw)
	b.Ephemeral = ephemeral != 0
	b.NoHistory = noHistory != 0
	if b.From == "" {
		b.From = b.Metadata["from"]
	}
	return b, nil
}

func parseDBTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		return parseTimeString(t)
	case []byte:
		return parseTimeString(string(t))
	default:
		return time.Time{}
	}
}

func parseTimeString(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999 -0700 MST", // time.Time.String() — modernc default write format
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func parseMetadata(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return nil
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil
	}
	out := make(map[string]string, len(decoded))
	for k, v := range decoded {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
		} else {
			out[k] = strings.TrimSpace(string(v))
		}
	}
	return out
}

func (s *DoltliteReadStore) tableExists(name string) bool {
	var found string
	err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	return err == nil
}

// tableHasColumn reports whether the table's schema includes the named
// column. Snapshot schemas vary by writer generation (the storage-flag
// columns arrived with the upstream wisps/no-history migrations), so read
// paths probe before referencing them. A genuinely absent column reports
// (false, nil); an unexpected probe failure is returned so the caller fails
// the read instead of silently downgrading tier classification to legacy
// semantics, which would drop the whole wisps table from TierIssues and
// misclassify no-history rows (#3449 review).
func (s *DoltliteReadStore) tableHasColumn(table, column string) (bool, error) {
	var found string
	err := s.db.QueryRow(`SELECT name FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&found)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("doltlite probe %s.%s: %w", table, column, err)
	}
	return true, nil
}

func (s *DoltliteReadStore) hydrateLabels(beads []Bead, labelTable string) error {
	if len(beads) == 0 {
		return nil
	}
	byID := make(map[string]*Bead, len(beads))
	args := make([]any, 0, len(beads))
	for i := range beads {
		byID[beads[i].ID] = &beads[i]
		args = append(args, beads[i].ID)
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(args)), ",")
	rows, err := s.db.Query(`SELECT issue_id, label FROM `+labelTable+` WHERE issue_id IN (`+placeholders+`)`, args...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return err
		}
		if b := byID[id]; b != nil {
			b.Labels = append(b.Labels, label)
		}
	}
	for i := range beads {
		sort.Strings(beads[i].Labels)
	}
	return rows.Err()
}
