// Package doltread provides bounded read-only SQL views over Beads Dolt tables.
package doltread

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beads"
)

// Config describes a resolved Dolt SQL target for one Beads scope.
type Config struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
}

// Reader serves supported active Beads list queries from bounded SQL reads.
type Reader struct {
	db *sql.DB
}

// Open creates a Reader with a small connection pool.
func Open(cfg Config) (*Reader, error) {
	db, err := sql.Open("mysql", buildDSN(cfg))
	if err != nil {
		return nil, fmt.Errorf("open indexed dolt reader: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Second)
	return &Reader{db: db}, nil
}

// New returns a Reader backed by db. Tests use this to avoid opening a network
// connection.
func New(db *sql.DB) *Reader {
	return &Reader{db: db}
}

func buildDSN(cfg Config) string {
	user := strings.TrimSpace(cfg.User)
	if user == "" {
		user = "root"
	}
	mysqlCfg := mysql.NewConfig()
	mysqlCfg.User = user
	mysqlCfg.Passwd = cfg.Password
	mysqlCfg.Net = "tcp"
	mysqlCfg.Addr = fmt.Sprintf("%s:%d", strings.TrimSpace(cfg.Host), cfg.Port)
	mysqlCfg.DBName = strings.TrimSpace(cfg.Database)
	mysqlCfg.ParseTime = true
	mysqlCfg.Timeout = 10 * time.Second
	mysqlCfg.ReadTimeout = 10 * time.Second
	mysqlCfg.WriteTimeout = 10 * time.Second
	mysqlCfg.AllowNativePasswords = true
	return mysqlCfg.FormatDSN()
}

// Close closes the underlying SQL pool.
func (r *Reader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

// ListIndexed returns supported rows with label and dependency enrichment.
// Broad closed/history/full-text query shapes are intentionally unsupported and
// must fall back to Beads CLI.
func (r *Reader) ListIndexed(ctx context.Context, query beads.ListQuery) (beads.IndexedListResult, error) {
	if r == nil || r.db == nil {
		return beads.IndexedListResult{}, fmt.Errorf("indexed dolt reader unavailable")
	}
	if err := validateSupported(query); err != nil {
		return beads.IndexedListResult{}, err
	}

	switch query.TierMode {
	case beads.TierWisps:
		return r.listTier(ctx, query, tierWisps, true)
	case beads.TierBoth:
		issuesQ := query
		issuesQ.TierMode = beads.TierIssues
		wispsQ := query
		wispsQ.TierMode = beads.TierWisps

		applyTierLimit := query.Limit > 0
		issues, err := r.listTier(ctx, issuesQ, tierIssues, applyTierLimit)
		if err != nil {
			return issues, err
		}
		wisps, err := r.listTier(ctx, wispsQ, tierWisps, applyTierLimit)
		if err != nil {
			return wisps, err
		}
		return mergeTierResults(query, issues, wisps), nil
	default:
		return r.listTier(ctx, query, tierIssues, true)
	}
}

// CountIndexed returns a cheap aggregate count for supported Beads table
// filters. Unlike ListIndexed it does not hydrate labels or dependencies, so
// broad all-status counts are safe for status/health summaries.
func (r *Reader) CountIndexed(ctx context.Context, query beads.ListQuery) (int, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("indexed dolt reader unavailable")
	}
	if err := validateSupportedStatus(query); err != nil {
		return 0, err
	}
	switch query.TierMode {
	case beads.TierWisps:
		return r.countTier(ctx, query, tierWisps)
	case beads.TierBoth:
		issues, err := r.countTier(ctx, query, tierIssues)
		if err != nil {
			return 0, err
		}
		wisps, err := r.countTier(ctx, query, tierWisps)
		if err != nil {
			return 0, err
		}
		return issues + wisps, nil
	default:
		return r.countTier(ctx, query, tierIssues)
	}
}

func validateSupported(query beads.ListQuery) error {
	if err := validateSupportedStatus(query); err != nil {
		return err
	}
	if query.IncludeClosed || query.Status == "closed" {
		if !isBoundedHistoryQuery(query) {
			return fmt.Errorf("%w: broad closed/history reads", beads.ErrIndexedListUnsupported)
		}
	}
	return nil
}

func validateSupportedStatus(query beads.ListQuery) error {
	switch query.Status {
	case "", "open", "in_progress", "closed":
	default:
		return fmt.Errorf("%w: raw status %q", beads.ErrIndexedListUnsupported, query.Status)
	}
	return nil
}

func isBoundedHistoryQuery(query beads.ListQuery) bool {
	if len(query.Metadata) > 0 {
		return true
	}
	if query.Type == "session" || query.Label == "gc:session" {
		return true
	}
	if query.ParentID != "" {
		return true
	}
	if query.Limit <= 0 {
		return false
	}
	return query.Label != "" ||
		query.Type != "" ||
		query.Assignee != "" ||
		query.ParentID != "" ||
		!query.CreatedBefore.IsZero()
}

type tierSpec struct {
	beadTable  string
	labelTable string
	depTable   string
	ephemeral  bool
}

var (
	tierIssues = tierSpec{beadTable: "issues", labelTable: "labels", depTable: "dependencies"}
	tierWisps  = tierSpec{beadTable: "wisps", labelTable: "wisp_labels", depTable: "wisp_dependencies", ephemeral: true}
)

func (r *Reader) listTier(ctx context.Context, query beads.ListQuery, tier tierSpec, applyLimit bool) (beads.IndexedListResult, error) {
	sqlText, args := buildListSQL(query, tier, applyLimit)
	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return beads.IndexedListResult{}, fmt.Errorf("indexed list %s: %w", tier.beadTable, err)
	}
	defer rows.Close() //nolint:errcheck // best-effort cleanup

	items := make([]beads.Bead, 0)
	for rows.Next() {
		item, err := scanBead(rows.Scan, tier.ephemeral)
		if err != nil {
			return beads.IndexedListResult{}, fmt.Errorf("indexed list scan %s: %w", tier.beadTable, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return beads.IndexedListResult{}, fmt.Errorf("indexed list rows %s: %w", tier.beadTable, err)
	}

	ids := beadIDs(items)
	labelsCoverage := true
	if !query.SkipLabels || query.Label != "" {
		labelsByID, err := r.loadLabels(ctx, tier.labelTable, ids)
		if err != nil {
			return beads.IndexedListResult{Beads: items, LabelsCoverage: false}, err
		}
		for i := range items {
			items[i].Labels = labelsByID[items[i].ID]
		}
	}
	depsByID, err := r.loadDependencies(ctx, tier.depTable, ids)
	if err != nil {
		return beads.IndexedListResult{Beads: items, LabelsCoverage: labelsCoverage}, err
	}
	for i := range items {
		items[i].Dependencies = depsByID[items[i].ID]
		for _, dep := range items[i].Dependencies {
			if dep.Type == "parent-child" {
				items[i].ParentID = dep.DependsOnID
				break
			}
		}
	}

	items = beads.ApplyListQuery(items, query)
	return beads.IndexedListResult{
		Beads:              items,
		DepsByID:           depsByID,
		DependencyCoverage: true,
		LabelsCoverage:     labelsCoverage,
	}, nil
}

func (r *Reader) countTier(ctx context.Context, query beads.ListQuery, tier tierSpec) (int, error) {
	sqlText, args := buildCountSQL(query, tier)
	var count int
	if err := r.db.QueryRowContext(ctx, sqlText, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("indexed count %s: %w", tier.beadTable, err)
	}
	return count, nil
}

func buildWhereSQL(query beads.ListQuery, tier tierSpec) ([]string, []any) {
	where := []string{}
	args := []any{}

	switch {
	case query.Status == "closed":
		where = append(where, "b.status = ?")
		args = append(args, "closed")
	case query.Status == "open":
		where = append(where, "b.status NOT IN ('closed', 'in_progress')")
	case query.Status == "in_progress":
		where = append(where, "b.status = ?")
		args = append(args, "in_progress")
	case !query.IncludeClosed:
		where = append(where, "b.status <> 'closed'")
	}
	if query.Type != "" {
		where = append(where, "b.issue_type = ?")
		args = append(args, query.Type)
	}
	if query.ExcludeType != "" {
		where = append(where, "b.issue_type <> ?")
		args = append(args, query.ExcludeType)
	}
	if query.Assignee != "" {
		where = append(where, "b.assignee = ?")
		args = append(args, query.Assignee)
	}
	if query.Label != "" {
		where = append(where, "EXISTS (SELECT 1 FROM "+tier.labelTable+" l WHERE l.issue_id = b.id AND l.label = ?)")
		args = append(args, query.Label)
	}
	if query.ParentID != "" {
		where = append(where, "EXISTS (SELECT 1 FROM "+tier.depTable+" d WHERE d.issue_id = b.id AND d.type = 'parent-child' AND "+dependencyTargetExpr("d")+" = ?)")
		args = append(args, query.ParentID)
	}
	if !query.CreatedBefore.IsZero() {
		where = append(where, "b.created_at < ?")
		args = append(args, query.CreatedBefore)
	}
	if len(query.Metadata) > 0 {
		keys := make([]string, 0, len(query.Metadata))
		for key := range query.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			where = append(where, "JSON_UNQUOTE(JSON_EXTRACT(b.metadata, ?)) = ?")
			args = append(args, metadataJSONPath(key), query.Metadata[key])
		}
	}
	if len(where) == 0 {
		where = append(where, "1=1")
	}
	return where, args
}

func buildFromWhereSQL(query beads.ListQuery, tier tierSpec) (string, []string, []any) {
	from := tier.beadTable + " b"
	whereQuery := query
	if query.Label != "" {
		from = tier.labelTable + " l JOIN " + tier.beadTable + " b ON b.id = l.issue_id"
		whereQuery.Label = ""
	}
	if query.ParentID != "" {
		if query.Label == "" {
			from = tier.depTable + " d JOIN " + tier.beadTable + " b ON b.id = d.issue_id AND d.type = 'parent-child'"
		} else {
			from += " JOIN " + tier.depTable + " d ON d.issue_id = b.id AND d.type = 'parent-child'"
		}
		whereQuery.ParentID = ""
	}
	where, args := buildWhereSQL(whereQuery, tier)
	if query.Label != "" {
		where = append(where, "l.label = ?")
		args = append(args, query.Label)
	}
	if query.ParentID != "" {
		where = append(where, dependencyTargetExpr("d")+" = ?")
		args = append(args, query.ParentID)
	}
	return from, where, args
}

func buildListSQL(query beads.ListQuery, tier tierSpec, applyLimit bool) (string, []any) {
	from, where, args := buildFromWhereSQL(query, tier)
	order := "DESC"
	if query.Sort == beads.SortCreatedAsc {
		order = "ASC"
	}
	text := "SELECT b.id, b.title, b.description, b.status, b.priority, b.issue_type, b.assignee, b.created_at, b.external_ref, b.metadata FROM " +
		from + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY b.created_at " + order + ", b.id " + order
	if applyLimit && query.Limit > 0 {
		text += " LIMIT ?"
		args = append(args, query.Limit)
	}
	return text, args
}

func buildCountSQL(query beads.ListQuery, tier tierSpec) (string, []any) {
	from, where, args := buildFromWhereSQL(query, tier)
	text := "SELECT COUNT(*) FROM " + from + " WHERE " + strings.Join(where, " AND ")
	return text, args
}

func scanBead(scan func(dest ...any) error, ephemeral bool) (beads.Bead, error) {
	var item beads.Bead
	var description, assignee, externalRef sql.NullString
	var priority sql.NullInt64
	var metadataJSON []byte

	if err := scan(
		&item.ID,
		&item.Title,
		&description,
		&item.Status,
		&priority,
		&item.Type,
		&assignee,
		&item.CreatedAt,
		&externalRef,
		&metadataJSON,
	); err != nil {
		return beads.Bead{}, err
	}
	item.Status = normalizeStatus(item.Status)
	item.Description = description.String
	item.Assignee = assignee.String
	item.Ref = externalRef.String
	item.Ephemeral = ephemeral
	if priority.Valid {
		p := int(priority.Int64)
		item.Priority = &p
	}
	if len(metadataJSON) > 0 {
		item.Metadata = parseMetadata(metadataJSON)
	}
	return item, nil
}

func parseMetadata(data []byte) map[string]string {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		if s, ok := value.(string); ok {
			out[key] = s
			continue
		}
		if encoded, err := json.Marshal(value); err == nil {
			out[key] = string(encoded)
		}
	}
	return out
}

func normalizeStatus(status string) string {
	switch status {
	case "closed":
		return "closed"
	case "in_progress":
		return "in_progress"
	default:
		return "open"
	}
}

func (r *Reader) loadLabels(ctx context.Context, table string, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	query, args := inQuery("SELECT issue_id, label FROM "+table+" WHERE issue_id IN ", ids)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return out, fmt.Errorf("indexed labels %s: %w", table, err)
	}
	defer rows.Close() //nolint:errcheck // best-effort cleanup
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return out, fmt.Errorf("indexed labels scan %s: %w", table, err)
		}
		out[id] = append(out[id], label)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("indexed labels rows %s: %w", table, err)
	}
	return out, nil
}

func (r *Reader) loadDependencies(ctx context.Context, table string, ids []string) (map[string][]beads.Dep, error) {
	out := make(map[string][]beads.Dep, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	query, args := inQuery("SELECT issue_id, "+dependencyTargetExpr("")+" AS depends_on_id, type FROM "+table+" WHERE issue_id IN ", ids)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return out, fmt.Errorf("indexed dependencies %s: %w", table, err)
	}
	defer rows.Close() //nolint:errcheck // best-effort cleanup
	for rows.Next() {
		var dep beads.Dep
		if err := rows.Scan(&dep.IssueID, &dep.DependsOnID, &dep.Type); err != nil {
			return out, fmt.Errorf("indexed dependencies scan %s: %w", table, err)
		}
		if dep.IssueID == "" || dep.DependsOnID == "" {
			continue
		}
		out[dep.IssueID] = append(out[dep.IssueID], dep)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("indexed dependencies rows %s: %w", table, err)
	}
	return out, nil
}

func dependencyTargetExpr(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return "COALESCE(" + prefix + "depends_on_issue_id, " + prefix + "depends_on_wisp_id, " + prefix + "depends_on_external)"
}

func metadataJSONPath(key string) string {
	escaped := strings.ReplaceAll(key, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `$."` + escaped + `"`
}

func beadIDs(items []beads.Bead) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.ID != "" {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

func inQuery(prefix string, ids []string) (string, []any) {
	args := make([]any, len(ids))
	parts := make([]string, len(ids))
	for i, id := range ids {
		args[i] = id
		parts[i] = "?"
	}
	return prefix + "(" + strings.Join(parts, ",") + ")", args
}

func mergeTierResults(query beads.ListQuery, results ...beads.IndexedListResult) beads.IndexedListResult {
	seen := map[string]struct{}{}
	items := []beads.Bead{}
	depsByID := map[string][]beads.Dep{}
	labelsCoverage := true
	dependencyCoverage := true
	for _, result := range results {
		labelsCoverage = labelsCoverage && result.LabelsCoverage
		dependencyCoverage = dependencyCoverage && result.DependencyCoverage
		for id, deps := range result.DepsByID {
			depsByID[id] = deps
		}
		for _, item := range result.Beads {
			if _, ok := seen[item.ID]; ok {
				continue
			}
			seen[item.ID] = struct{}{}
			items = append(items, item)
		}
	}
	sortBeads(items, query.Sort)
	if query.Limit > 0 && len(items) > query.Limit {
		items = items[:query.Limit]
	}
	return beads.IndexedListResult{
		Beads:              items,
		DepsByID:           depsByID,
		DependencyCoverage: dependencyCoverage,
		LabelsCoverage:     labelsCoverage,
	}
}

func sortBeads(items []beads.Bead, order beads.SortOrder) {
	desc := order != beads.SortCreatedAsc
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			if desc {
				return items[i].ID > items[j].ID
			}
			return items[i].ID < items[j].ID
		}
		if desc {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
}
