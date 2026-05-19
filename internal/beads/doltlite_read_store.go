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

	_ "github.com/mattn/go-sqlite3"
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
	poolDemandMu    sync.Mutex
	poolDemandCache map[string]int
	poolDemandHash  string
}

func (s *DoltliteReadStore) NeedsSessionTypeFallback() bool { return true }

type doltliteMetadata struct {
	Backend      string `json:"backend"`
	Database     string `json:"database"`
	DoltDatabase string `json:"dolt_database"`
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
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&_busy_timeout=10000")
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
	if strings.TrimSpace(meta.Backend) != "doltlite" && strings.TrimSpace(meta.Database) != "doltlite" {
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
	beads, err := s.queryIssues(ListQuery{AllowScan: true, IncludeClosed: true}, "i.id = ?", []any{id}, 1)
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
		SkipParent:    true,
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
		SkipParent: true,
	}, "", nil, 0)
	if err != nil {
		return nil, err
	}
	s.sessionCache = cloneBeads(rows)
	s.sessionHash = hash
	return rows, nil
}

func (s *DoltliteReadStore) List(query ListQuery) ([]Bead, error) {
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

	q := ListQuery{Status: "open", AllowScan: true, IncludeClosed: false, Limit: 0, SkipLabels: true, SkipParent: true}
	if rq.Assignee != "" {
		q.Assignee = rq.Assignee
	}
	if rq.Limit > 0 {
		q.Limit = rq.Limit
	}
	candidateLimit := q.Limit
	if candidateLimit > 0 {
		candidateLimit *= 4
		if candidateLimit < 100 {
			candidateLimit = 100
		}
	}
	candidates, err := s.queryIssues(q, `i.issue_type NOT IN ('merge-request','gate','molecule','message','session','agent','role','rig')`, nil, candidateLimit)
	if err != nil {
		return nil, err
	}
	blocked, err := s.blockedIssueIDs(candidates)
	if err != nil {
		return nil, err
	}
	out := candidates[:0]
	for _, b := range candidates {
		if blocked[b.ID] {
			continue
		}
		if !IsReadyExcludedType(b.Type) {
			out = append(out, b)
			if q.Limit > 0 && len(out) >= q.Limit {
				break
			}
		}
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

func (s *DoltliteReadStore) blockedIssueIDs(candidates []Bead) (map[string]bool, error) {
	blocked := make(map[string]bool)
	if len(candidates) == 0 {
		return blocked, nil
	}
	for start := 0; start < len(candidates); start += 500 {
		end := start + 500
		if end > len(candidates) {
			end = len(candidates)
		}
		placeholders := strings.TrimRight(strings.Repeat("?,", end-start), ",")
		args := make([]any, 0, end-start)
		for _, candidate := range candidates[start:end] {
			args = append(args, candidate.ID)
		}
		rows, err := s.db.Query(`SELECT d.issue_id
			FROM dependencies d
			JOIN issues blocker ON blocker.id = d.depends_on_id
			WHERE d.type = 'blocks'
			AND blocker.status != 'closed'
			AND d.issue_id IN (`+placeholders+`)`, args...)
		if err != nil {
			return blocked, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return blocked, err
			}
			blocked[id] = true
		}
		if err := rows.Close(); err != nil {
			return blocked, err
		}
	}
	return blocked, nil
}

func (s *DoltliteReadStore) PoolDemandCount(template string) (int, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return 0, nil
	}
	hash, err := s.currentDoltHash()
	if err != nil {
		return 0, err
	}
	s.poolDemandMu.Lock()
	if hash != "" && hash == s.poolDemandHash && s.poolDemandCache != nil {
		if count, ok := s.poolDemandCache[template]; ok {
			s.poolDemandMu.Unlock()
			return count, nil
		}
	}
	s.poolDemandMu.Unlock()
	query := `SELECT COUNT(*) FROM issues i
		WHERE json_extract(i.metadata, '$."gc.routed_to"') = ?
		AND (i.assignee IS NULL OR i.assignee = '')
		AND (
			(i.status = 'in_progress')
			OR (i.status = 'open' AND i.issue_type = 'molecule')
			OR (
				i.status = 'open'
				AND i.issue_type NOT IN ('merge-request','gate','molecule','message','session','agent','role','rig')
				AND NOT EXISTS (
					SELECT 1 FROM dependencies d
					JOIN issues blocker ON blocker.id = d.depends_on_id
					WHERE d.issue_id = i.id AND d.type = 'blocks' AND blocker.status != 'closed'
				)
			)
		)`
	var count int
	if err := s.db.QueryRow(query, template).Scan(&count); err != nil {
		return 0, err
	}
	s.poolDemandMu.Lock()
	if hash != "" {
		if hash != s.poolDemandHash || s.poolDemandCache == nil {
			s.poolDemandHash = hash
			s.poolDemandCache = make(map[string]int)
		}
		s.poolDemandCache[template] = count
	}
	s.poolDemandMu.Unlock()
	return count, nil
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

	var issueCount int64
	var issueMaxUpdated sql.NullString
	if err := s.db.QueryRow("SELECT COUNT(*), MAX(updated_at) FROM issues").Scan(&issueCount, &issueMaxUpdated); err != nil {
		return "", fmt.Errorf("doltlite issues fingerprint: %w", err)
	}

	var labelCount int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM labels").Scan(&labelCount); err != nil {
		return "", fmt.Errorf("doltlite labels fingerprint: %w", err)
	}

	var depCount int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM dependencies").Scan(&depCount); err != nil {
		return "", fmt.Errorf("doltlite dependencies fingerprint: %w", err)
	}

	updated := ""
	if issueMaxUpdated.Valid {
		updated = strings.TrimSpace(issueMaxUpdated.String)
	}
	return fmt.Sprintf("data=%d;issues=%d:%s;labels=%d;deps=%d", dataVersion, issueCount, updated, labelCount, depCount), nil
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
	s.poolDemandMu.Lock()
	s.poolDemandCache = nil
	s.poolDemandHash = ""
	s.poolDemandMu.Unlock()
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
			SkipParent:    true,
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

// DefaultWorkQueryHasReadyWork mirrors config.Agent.EffectiveWorkQuery for the
// built-in bd query without spawning bd. It is intentionally narrow: custom
// work_query commands still execute through the configured shell path.
func (s *DoltliteReadStore) DefaultWorkQueryHasReadyWork(targets []string, identities []string, includeRouted bool) (bool, error) {
	for _, identity := range compactStrings(identities) {
		ok, err := s.existsIssue(`i.status = 'in_progress' AND i.assignee = ?`, identity)
		if ok || err != nil {
			return ok, err
		}
		ok, err = s.existsReadyIssue(`i.assignee = ?`, identity)
		if ok || err != nil {
			return ok, err
		}
	}
	if !includeRouted {
		return false, nil
	}
	for _, target := range compactStrings(targets) {
		ok, err := s.existsReadyIssue(`json_extract(i.metadata, '$."gc.routed_to"') = ? AND (i.assignee IS NULL OR i.assignee = '')`, target)
		if ok || err != nil {
			return ok, err
		}
		ok, err = s.existsIssue(`i.status = 'open' AND i.issue_type = 'molecule' AND json_extract(i.metadata, '$."gc.routed_to"') = ? AND (i.assignee IS NULL OR i.assignee = '')`, target)
		if ok || err != nil {
			return ok, err
		}
	}
	return false, nil
}

func compactStrings(values []string) []string {
	out := values[:0]
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

func sqliteJSONPath(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "$"
	}
	if strings.IndexFunc(key, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) == -1 {
		return "$." + key
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		return "$"
	}
	return "$." + string(encoded)
}

func (s *DoltliteReadStore) existsReadyIssue(extraWhere string, args ...any) (bool, error) {
	return s.existsIssue(`i.status = 'open'
		AND i.issue_type NOT IN ('merge-request','gate','molecule','message','session','agent','role','rig')
		AND NOT EXISTS (
			SELECT 1 FROM dependencies d
			JOIN issues blocker ON blocker.id = d.depends_on_id
			WHERE d.issue_id = i.id AND d.type = 'blocks' AND blocker.status != 'closed'
		)
		AND `+extraWhere, args...)
}

func (s *DoltliteReadStore) existsIssue(where string, args ...any) (bool, error) {
	var found int
	err := s.db.QueryRow(`SELECT 1 FROM issues i WHERE `+where+` LIMIT 1`, args...).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *DoltliteReadStore) DepList(id, direction string) ([]Dep, error) {
	if direction == "up" {
		return s.queryDeps("depends_on_id = ?", id)
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
		rows, err := s.db.Query(`SELECT issue_id, depends_on_id, type FROM dependencies WHERE issue_id IN (`+placeholders+`)`, args...)
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
		if err := rows.Close(); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *DoltliteReadStore) queryDeps(where, value string) ([]Dep, error) {
	rows, err := s.db.Query(`SELECT issue_id, depends_on_id, type FROM dependencies WHERE `+where, value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deps []Dep
	for rows.Next() {
		dep, err := scanDep(rows)
		if err != nil {
			return nil, err
		}
		deps = append(deps, dep)
	}
	return deps, rows.Err()
}

func scanDep(rows interface{ Scan(...any) error }) (Dep, error) {
	var dep Dep
	if err := rows.Scan(&dep.IssueID, &dep.DependsOnID, &dep.Type); err != nil {
		return dep, err
	}
	if dep.Type == "" {
		dep.Type = "blocks"
	}
	return dep, nil
}

func (s *DoltliteReadStore) queryIssues(query ListQuery, extraWhere string, extraArgs []any, limit int) ([]Bead, error) {
	where := []string{}
	args := []any{}
	needParent := !query.SkipParent || query.ParentID != ""
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
	if query.ParentID != "" {
		where = append(where, "pc.depends_on_id = ?")
		args = append(args, query.ParentID)
	}
	if query.Label != "" {
		where = append(where, "EXISTS (SELECT 1 FROM labels l WHERE l.issue_id = i.id AND l.label = ?)")
		args = append(args, query.Label)
	}
	for k, v := range query.Metadata {
		where = append(where, "json_extract(i.metadata, ?) = ?")
		args = append(args, sqliteJSONPath(k), v)
	}
	if !query.CreatedBefore.IsZero() {
		where = append(where, "i.created_at < ?")
		args = append(args, query.CreatedBefore.Format(time.RFC3339Nano))
	}
	if extraWhere != "" {
		where = append(where, extraWhere)
		args = append(args, extraArgs...)
	}
	parentColumn := "''"
	parentJoin := ""
	if needParent {
		parentColumn = "COALESCE(pc.depends_on_id, '')"
		parentJoin = " LEFT JOIN dependencies pc ON pc.issue_id = i.id AND pc.type = 'parent-child'"
	}
	sqlText := `SELECT i.id, i.title, i.status, i.issue_type, i.priority, i.created_at,
		COALESCE(i.assignee, ''), i.description, COALESCE(i.metadata, '{}'),
		` + parentColumn + `
		FROM issues i` + parentJoin
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	if query.Sort == SortCreatedAsc {
		sqlText += " ORDER BY i.created_at ASC"
	} else {
		sqlText += " ORDER BY i.created_at DESC"
	}
	if limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.Query(sqlText, args...)
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
		if err := s.hydrateLabels(beads); err != nil {
			return nil, err
		}
	}
	return beads, nil
}

func scanBead(rows interface{ Scan(...any) error }) (Bead, error) {
	var (
		b           Bead
		priority    sql.NullInt64
		createdRaw  any
		metadataRaw string
	)
	if err := rows.Scan(&b.ID, &b.Title, &b.Status, &b.Type, &priority, &createdRaw, &b.Assignee, &b.Description, &metadataRaw, &b.ParentID); err != nil {
		return b, err
	}
	if priority.Valid {
		p := int(priority.Int64)
		b.Priority = &p
	}
	b.Status = mapBdStatus(b.Status)
	b.CreatedAt = parseDBTime(createdRaw).Truncate(time.Second)
	b.Metadata = parseMetadata(metadataRaw)
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

func (s *DoltliteReadStore) hydrateLabels(beads []Bead) error {
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
	rows, err := s.db.Query(`SELECT issue_id, label FROM labels WHERE issue_id IN (`+placeholders+`)`, args...)
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
