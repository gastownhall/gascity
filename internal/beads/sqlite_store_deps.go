package beads

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The SQLite engine keeps a bead's edges in the deps table and its fields in
// bead_json, which is written at create and on every field update but never by
// DepAdd, DepRemove or a graph apply. A row decoded from bead_json alone
// therefore carries the edges it was created with, or none, rather than the
// edges it has. Every read that returns rows replaces the decoded dependency
// fields with the table's, so a row carries its complete edge set — the same
// convention bd and the native Dolt store follow, and the one a CachingStore
// relies on when it installs a row (listIncludesCompleteDependencies).

// sqliteDepsQueryChunk bounds the ids in one IN (...) list, well under
// SQLite's host-parameter limit.
const sqliteDepsQueryChunk = 500

var _ listDependencyCompletenessStore = (*SQLiteStore)(nil)

type sqliteQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// listIncludesCompleteDependencies declares that every row this store returns
// carries its full outgoing edge set, so a cache installs them as read and
// skips a DepList per row on prime and re-scan.
func (s *SQLiteStore) listIncludesCompleteDependencies() bool {
	return true
}

// hydrateSQLiteDeps sets each bead's Dependencies to its outgoing edges in the
// deps table and clears Needs, the create-time shorthand those edges came from.
func hydrateSQLiteDeps(ctx context.Context, q sqliteQueryer, items []Bead) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	deps, err := sqliteDepsFor(ctx, q, ids)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].Dependencies = deps[items[i].ID]
		items[i].Needs = nil
	}
	return nil
}

// sqliteDepsFor returns the outgoing edges of each id, in a stable order.
func sqliteDepsFor(ctx context.Context, q sqliteQueryer, ids []string) (map[string][]Dep, error) {
	out := make(map[string][]Dep, len(ids))
	for start := 0; start < len(ids); start += sqliteDepsQueryChunk {
		chunk := ids[start:min(start+sqliteDepsQueryChunk, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := q.QueryContext(ctx,
			`SELECT issue_id, depends_on_id, dep_type FROM deps WHERE issue_id IN (`+
				strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+
				`) ORDER BY issue_id, depends_on_id, dep_type`,
			args...)
		if err != nil {
			// A canceled or expired read can surface as the driver's own
			// "interrupted" error; report the cause a caller classifies.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("reading dependency edges: %w", err)
		}
		for rows.Next() {
			var d Dep
			if err := rows.Scan(&d.IssueID, &d.DependsOnID, &d.Type); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("reading dependency edges: %w", err)
			}
			out[d.IssueID] = append(out[d.IssueID], d)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("reading dependency edges: %w", err)
		}
	}
	return out, nil
}
