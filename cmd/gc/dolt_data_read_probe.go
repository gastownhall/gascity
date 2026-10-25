package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// The data-read probe closes the blind spot behind ga-vrq5pu: dolt-health and
// beads-health stayed green 60s before a total outage because every probe read
// only the catalog (information_schema.SCHEMATA) or a table the probe itself had
// just written. A dolt-ignored table (wisps, events, ...) whose prolly tree had
// lost chunks was never read, so "empty chunk returned from ChunkStore" and
// "chunk <hash> required but not found" surfaced first in bd, not in the probe.
//
// The probe issues `SELECT * ... LIMIT 1` against each pre-existing dolt-ignored
// table. A bare COUNT(*) is not enough: the planner answers it from the root
// node's cardinality without fetching a leaf. An unordered full-scan read has to
// materialize the first row, which means fetching the leftmost leaf chunk.

// managedDoltIgnoredTablePatterns are the table names (or LIKE patterns, when
// they contain %) that bd keeps out of Dolt history through dolt_ignore. They
// are the tables whose chunks live only in the working set, so no commit-graph
// operation ever touches them and nothing but a direct read would notice a loss.
var managedDoltIgnoredTablePatterns = []string{
	"wisps",
	"wisp_%",
	"local_metadata",
	"leases",
	"events",
	"bd_events_journal",
	"bd_events_seq",
	"repo_mtimes",
	"ignored_schema_migrations",
}

// managedDoltDataReadBudget bounds one whole probe (discovery plus one batch per
// database) so the 30s patrol cannot pile up behind a wedged server.
const managedDoltDataReadBudget = 20 * time.Second

// managedDoltTableRef names one table in one database.
type managedDoltTableRef struct {
	DB    string
	Table string
}

// managedDoltDataReadOps is the I/O the data-read probe needs: table discovery
// across the server and one batch of reads per database.
type managedDoltDataReadOps struct {
	listTables func() ([]managedDoltTableRef, error)
	readBatch  func(db string, stmts []string) error
}

// managedDoltDataReadResult is the probe outcome. Err is a chunk-level read
// failure (the data is unreadable). Warning is a read that failed for another
// reason, which proves nothing about the data, so it must never be acted on as
// data loss. Both carry the underlying error text.
type managedDoltDataReadResult struct {
	Err     string
	Warning string
}

// managedDoltDataReadProbeFn is the seam managedDoltHealthCheck calls.
var managedDoltDataReadProbeFn = managedDoltDataReadProbe

type managedDoltDataReadErrorKind int

const (
	managedDoltDataReadErrorOther managedDoltDataReadErrorKind = iota
	managedDoltDataReadErrorAbsent
	managedDoltDataReadErrorChunk
)

// classifyManagedDoltDataReadError sorts a read error: a table or database that
// is not there is healthy (the table is created lazily and may be dropped), a
// chunk-store signature is data loss, and anything else is inconclusive.
func classifyManagedDoltDataReadError(err error) managedDoltDataReadErrorKind {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "empty chunk returned"),
		strings.Contains(msg, "required but not found"):
		return managedDoltDataReadErrorChunk
	case strings.Contains(msg, "table not found"),
		strings.Contains(msg, "doesn't exist"),
		strings.Contains(msg, "does not exist"),
		strings.Contains(msg, "database not found"):
		return managedDoltDataReadErrorAbsent
	}
	return managedDoltDataReadErrorOther
}

// managedDoltIgnoredTableDiscoverySQL lists the existing dolt-ignored tables in
// every user database. Names containing % are LIKE patterns whose underscores
// are escaped so they stay literal.
func managedDoltIgnoredTableDiscoverySQL() string {
	var names, likes []string
	for _, p := range managedDoltIgnoredTablePatterns {
		if strings.Contains(p, "%") {
			likes = append(likes, "table_name LIKE '"+strings.ReplaceAll(p, "_", `\\_`)+"'")
			continue
		}
		names = append(names, "'"+p+"'")
	}
	var systemDBs []string
	for _, db := range []string{"information_schema", "mysql", "dolt", "dolt_cluster", "performance_schema", "sys"} {
		systemDBs = append(systemDBs, "'"+db+"'")
	}
	match := append([]string{"table_name IN (" + strings.Join(names, ", ") + ")"}, likes...)
	return "SELECT table_schema, table_name FROM information_schema.tables" +
		" WHERE table_type = 'BASE TABLE'" +
		" AND table_schema NOT IN (" + strings.Join(systemDBs, ", ") + ")" +
		" AND (" + strings.Join(match, " OR ") + ")" +
		" ORDER BY table_schema, table_name"
}

// managedDoltLeafReadStatementsFor returns one leaf-fetching read per table in
// db: an unaggregated, single-row SELECT of an existing table.
func managedDoltLeafReadStatementsFor(db string, tables []string) []string {
	quotedDB := managedDoltQuoteIdent(db)
	stmts := make([]string, 0, len(tables))
	for _, table := range tables {
		stmts = append(stmts, "SELECT * FROM "+quotedDB+"."+managedDoltQuoteIdent(table)+" LIMIT 1")
	}
	return stmts
}

// managedDoltDataReadProbeWith runs the probe over ops. Databases are probed in
// discovery order, one batch each; the first chunk-level error wins and is
// returned at once, over any earlier inconclusive one. System databases and
// absent tables are healthy.
func managedDoltDataReadProbeWith(ops managedDoltDataReadOps) managedDoltDataReadResult {
	refs, err := ops.listTables()
	if err != nil {
		return managedDoltDataReadResult{Warning: "discovering dolt-ignored tables: " + err.Error()}
	}
	var dbs []string
	byDB := map[string][]string{}
	for _, ref := range refs {
		if _, system := managedDoltSystemDatabases[strings.ToLower(ref.DB)]; system {
			continue
		}
		if _, seen := byDB[ref.DB]; !seen {
			dbs = append(dbs, ref.DB)
		}
		byDB[ref.DB] = append(byDB[ref.DB], ref.Table)
	}
	var result managedDoltDataReadResult
	for _, db := range dbs {
		err := ops.readBatch(db, managedDoltLeafReadStatementsFor(db, byDB[db]))
		if err == nil {
			continue
		}
		switch classifyManagedDoltDataReadError(err) {
		case managedDoltDataReadErrorAbsent:
		case managedDoltDataReadErrorChunk:
			return managedDoltDataReadResult{Err: db + ": " + err.Error()}
		default:
			if result.Warning == "" {
				result.Warning = db + ": " + err.Error()
			}
		}
	}
	return result
}

// managedDoltDataReadProbe probes the server at host:port with the CLI runner,
// or the pooled driver when a password is configured (a password never goes in
// argv).
func managedDoltDataReadProbe(host, port, user string) managedDoltDataReadResult {
	ctx, cancel := context.WithTimeout(context.Background(), managedDoltDataReadBudget)
	defer cancel()
	if managedDoltPassword() != "" {
		return managedDoltDataReadProbeWith(managedDoltDataReadOpsDirect(ctx, host, port, user))
	}
	return managedDoltDataReadProbeWith(managedDoltDataReadOpsCLI(ctx, host, port, user))
}

// managedDoltDataReadOpsCLI backs the probe with `dolt sql`: one spawn for
// discovery and one per database.
func managedDoltDataReadOpsCLI(ctx context.Context, host, port, user string) managedDoltDataReadOps {
	return managedDoltDataReadOps{
		listTables: func() ([]managedDoltTableRef, error) {
			out, err := runManagedDoltSQLContext(ctx, host, port, user, "-r", "csv", "-q", managedDoltIgnoredTableDiscoverySQL())
			if err != nil {
				return nil, err
			}
			return managedDoltTableRefsFromCSV(out)
		},
		readBatch: func(_ string, stmts []string) error {
			_, err := runManagedDoltSQLContext(ctx, host, port, user, "-q", strings.Join(stmts, "; ")+";")
			return err
		},
	}
}

// managedDoltDataReadOpsDirect backs the probe with the pooled driver. Rows are
// iterated, not just executed: the leaf is fetched when the first row is read.
func managedDoltDataReadOpsDirect(ctx context.Context, host, port, user string) managedDoltDataReadOps {
	return managedDoltDataReadOps{
		listTables: func() ([]managedDoltTableRef, error) {
			db, err := managedDoltOpenDB(host, port, user)
			if err != nil {
				return nil, err
			}
			rows, err := db.QueryContext(ctx, managedDoltIgnoredTableDiscoverySQL())
			if err != nil {
				return nil, err
			}
			defer rows.Close() //nolint:errcheck
			var refs []managedDoltTableRef
			for rows.Next() {
				var ref managedDoltTableRef
				if err := rows.Scan(&ref.DB, &ref.Table); err != nil {
					return nil, err
				}
				refs = append(refs, ref)
			}
			return refs, rows.Err()
		},
		readBatch: func(_ string, stmts []string) error {
			db, err := managedDoltOpenDB(host, port, user)
			if err != nil {
				return err
			}
			conn, err := db.Conn(ctx)
			if err != nil {
				return err
			}
			defer conn.Close() //nolint:errcheck
			for _, stmt := range stmts {
				rows, err := conn.QueryContext(ctx, stmt)
				if err != nil {
					return err
				}
				_ = rows.Next() // LIMIT 1: reading the row is what fetches the leaf
				err = rows.Err()
				_ = rows.Close()
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// managedDoltTableRefsFromCSV parses the csv output of the discovery query,
// skipping its header row.
func managedDoltTableRefsFromCSV(out string) ([]managedDoltTableRef, error) {
	reader := csv.NewReader(strings.NewReader(out))
	reader.FieldsPerRecord = 2
	var refs []managedDoltTableRef
	for first := true; ; first = false {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return refs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse dolt-ignored table list csv: %w", err)
		}
		if first {
			continue
		}
		refs = append(refs, managedDoltTableRef{DB: record[0], Table: record[1]})
	}
}
