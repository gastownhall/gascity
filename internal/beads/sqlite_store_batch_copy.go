package beads

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// sqliteBatchQueryChunk bounds the ids bound into one IN list, below SQLite's
// default host-parameter limit.
const sqliteBatchQueryChunk = 500

// CreateWithForeignIDBatch creates rows keeping their explicit ids in ONE
// transaction: the CreateWithForeignID body for each row, with the same
// normalization, the same hard duplicate-id contract and the same claim-fence
// reset, so one bad row rolls the whole call back. It satisfies
// ForeignIDBatchCreator.
func (s *SQLiteStore) CreateWithForeignIDBatch(rows []Bead) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	for _, b := range rows {
		if strings.TrimSpace(b.ID) == "" {
			return fmt.Errorf("creating beads with foreign ids: empty id")
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return retryOnBusy(func() error {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite batch create: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		for _, b := range rows {
			stored, err := s.normalizeCreate(b)
			if err != nil {
				return err
			}
			if err := s.ensureCreateDoesNotExist(ctx, tx, stored.ID); err != nil {
				return err
			}
			if err := s.clearClaimFenceTx(ctx, tx, stored.ID); err != nil {
				return err
			}
			if err := s.upsertBeadTx(ctx, tx, stored); err != nil {
				return err
			}
			for _, dep := range depsFromBeadFields(stored) {
				if err := s.depAddTx(ctx, tx, dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
					return err
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite batch create: commit: %w", err)
		}
		return nil
	})
}

// DepAddBatch adds edges with their payloads in ONE transaction, each as
// DepAddWithMetadata adds it. It satisfies DepBatchWriter.
func (s *SQLiteStore) DepAddBatch(edges []DepWithPayload) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if len(edges) == 0 {
		return nil
	}
	return retryOnBusy(func() error {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite batch dep add: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		for _, e := range edges {
			if err := s.depAddWithMetadataTx(ctx, tx, e.IssueID, e.DependsOnID, e.Type, e.Payload); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

// DepRemoveBatch removes edges, and the payload sidecar of each, in ONE
// transaction, each as DepRemove removes it. It satisfies DepBatchRemover.
func (s *SQLiteStore) DepRemoveBatch(pairs []DepPair) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if len(pairs) == 0 {
		return nil
	}
	return retryOnBusy(func() error {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite batch dep remove: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		for _, p := range pairs {
			if _, err := tx.ExecContext(ctx, `DELETE FROM deps WHERE issue_id=? AND depends_on_id=?`, p.IssueID, p.DependsOnID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key GLOB ?`, sqliteGraphEdgeMetadataPairPrefix(p.IssueID, p.DependsOnID)+"*"); err != nil {
				return fmt.Errorf("clearing Graph dependency metadata %s -> %s: %w", p.IssueID, p.DependsOnID, err)
			}
		}
		return tx.Commit()
	})
}

// DepListBatch returns each held anchor's DOWN edges, in a stable order: the
// held anchors one query per chunk, then their edges through sqliteDepsFor,
// the read every row's edges take. An anchor the store does not hold gets no
// entry. It satisfies DependencyBatchLister, so a failed read returns no map.
func (s *SQLiteStore) DepListBatch(ids []string) (map[string][]Dep, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	out := make(map[string][]Dep, len(ids))
	for start := 0; start < len(ids); start += sqliteBatchQueryChunk {
		chunk := ids[start:min(start+sqliteBatchQueryChunk, len(ids))]
		args, in := sqliteInList(chunk)
		held, err := s.readDB.QueryContext(context.Background(), `SELECT id FROM beads WHERE id IN (`+in+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("listing the dep edges of %d anchor(s): %w", len(chunk), err)
		}
		for held.Next() {
			var id string
			if err := held.Scan(&id); err != nil {
				_ = held.Close()
				return nil, fmt.Errorf("listing the dep edges of %d anchor(s): %w", len(chunk), err)
			}
			out[id] = []Dep{}
		}
		if err := sqliteRowsDone(held); err != nil {
			return nil, fmt.Errorf("listing the dep edges of %d anchor(s): %w", len(chunk), err)
		}
	}
	edges, err := sqliteDepsFor(context.Background(), s.readDB, slices.Sorted(maps.Keys(out)))
	if err != nil {
		return nil, fmt.Errorf("listing the dep edges of %d anchor(s): %w", len(out), err)
	}
	for id, deps := range edges {
		out[id] = deps
	}
	return out, nil
}

// DepMetadataBatch reads the payload on every carrying DOWN edge of the given
// anchors: their edges, then their sidecars, under the type each edge has
// now, exactly as DepMetadata reads one. It satisfies DepMetadataBatchReader.
func (s *SQLiteStore) DepMetadataBatch(ids []string) (map[DepPair]string, error) {
	edges, err := s.DepListBatch(ids)
	if err != nil {
		return nil, err
	}
	keys := map[string]DepPair{}
	for _, deps := range edges {
		for _, d := range deps {
			keys[sqliteGraphEdgeMetadataKey(d.IssueID, d.DependsOnID, d.Type)] = DepPair{IssueID: d.IssueID, DependsOnID: d.DependsOnID}
		}
	}
	out := map[DepPair]string{}
	if len(keys) == 0 {
		return out, nil
	}
	all := make([]string, 0, len(keys))
	for k := range keys {
		all = append(all, k)
	}
	for start := 0; start < len(all); start += sqliteBatchQueryChunk {
		chunk := all[start:min(start+sqliteBatchQueryChunk, len(all))]
		args, in := sqliteInList(chunk)
		rows, err := s.readDB.QueryContext(context.Background(), `SELECT key, value FROM kv WHERE key IN (`+in+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("reading Graph dependency metadata of %d edge(s): %w", len(chunk), err)
		}
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("reading Graph dependency metadata of %d edge(s): %w", len(chunk), err)
			}
			if DepMetadataCarries(value) {
				out[keys[key]] = value
			}
		}
		if err := sqliteRowsDone(rows); err != nil {
			return nil, fmt.Errorf("reading Graph dependency metadata of %d edge(s): %w", len(chunk), err)
		}
	}
	return out, nil
}

// sqliteRowsDone ends a read of rows: the error that stopped the iteration,
// else the one closing them. A driver error part way through a result ends
// Next as the last row does, and closes the rows itself, so only Err reports
// it; checking Close alone takes the rows read before it for the whole
// answer.
func sqliteRowsDone(rows *sql.Rows) error {
	err := rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	return err
}

// sqliteInList binds ids as the arguments of an IN list.
func sqliteInList(ids []string) ([]any, string) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args, strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
}
