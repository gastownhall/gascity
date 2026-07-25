package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const managedDoltInventoryTimeout = 10 * time.Second

type managedDoltInventoryQuery func(context.Context, string) (string, error)

// Test seam. Production leaves this nil and uses runManagedDoltSQL.
var managedDoltInventoryQueryFn managedDoltInventoryQuery

type managedDoltInventoryRow struct {
	Database       string
	SchemaVersion  string
	ContentHash    string
	Remotes        string
	RemoteBranches string
	OriginMain     string
}

func managedDoltInventory(host, port, user string) ([]managedDoltInventoryRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), managedDoltInventoryTimeout)
	defer cancel()
	return managedDoltInventoryContext(ctx, host, port, user)
}

func managedDoltInventoryContext(ctx context.Context, host, port, user string) ([]managedDoltInventoryRow, error) {
	query := managedDoltInventoryQueryFn
	if query == nil {
		query = func(queryCtx context.Context, sql string) (string, error) {
			return runManagedDoltSQLContext(queryCtx, host, port, user, "-r", "csv", "-q", sql)
		}
	}
	out, err := query(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	dbs, err := managedDoltUserDatabasesFromCSV(out)
	if err != nil {
		return nil, err
	}
	sort.Strings(dbs)
	rows := make([]managedDoltInventoryRow, 0, len(dbs))
	for _, db := range dbs {
		row, err := managedDoltInventoryDatabase(ctx, query, db)
		if err != nil {
			return nil, fmt.Errorf("inventory database %q: %w", db, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func managedDoltInventoryDatabase(ctx context.Context, query managedDoltInventoryQuery, db string) (managedDoltInventoryRow, error) {
	row := managedDoltInventoryRow{Database: db, SchemaVersion: "absent", ContentHash: "absent", Remotes: "absent", RemoteBranches: "absent", OriginMain: "absent"}
	literal := strings.ReplaceAll(db, "'", "''")
	shapeSQL := "SELECT EXISTS(SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA='" + literal + "' AND TABLE_NAME='schema_migrations') AS schema_table, " +
		"EXISTS(SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='" + literal + "' AND TABLE_NAME='issues' AND COLUMN_NAME='content_hash') AS content_hash"
	out, err := query(ctx, shapeSQL)
	if err != nil {
		return row, err
	}
	shape, err := inventoryCSVRows(out, 2)
	if err != nil {
		return row, fmt.Errorf("parse schema shape: %w", err)
	}
	if len(shape) != 1 {
		return row, fmt.Errorf("parse schema shape: got %d rows, want 1", len(shape))
	}
	if shape[0][1] == "1" {
		row.ContentHash = "present"
	}
	quoted := managedDoltQuoteIdent(db)
	if shape[0][0] == "1" {
		out, err = query(ctx, "SELECT MAX(version) AS schema_version FROM "+quoted+".schema_migrations")
		if err != nil {
			return row, err
		}
		values, parseErr := inventoryCSVRows(out, 1)
		if parseErr != nil {
			return row, parseErr
		}
		if len(values) == 1 {
			version := strings.TrimSpace(values[0][0])
			if version != "" && !strings.EqualFold(version, "null") {
				row.SchemaVersion = version
			}
		}
	}
	out, err = query(ctx, "SELECT name FROM "+quoted+".dolt_remotes ORDER BY name")
	if err != nil {
		return row, err
	}
	remotes, err := inventoryCSVRows(out, 1)
	if err != nil {
		return row, err
	}
	if len(remotes) > 0 {
		names := make([]string, 0, len(remotes))
		for _, r := range remotes {
			names = append(names, r[0])
		}
		row.Remotes = strings.Join(names, ",")
	}
	out, err = query(ctx, "SELECT name FROM "+quoted+".dolt_remote_branches ORDER BY name")
	if err != nil {
		return row, err
	}
	branches, err := inventoryCSVRows(out, 1)
	if err != nil {
		return row, err
	}
	if len(branches) > 0 {
		names := make([]string, 0, len(branches))
		for _, branch := range branches {
			remote, name, ok := parseDoltRemoteBranchName(branch[0])
			if !ok {
				continue
			}
			names = append(names, remote+"/"+name)
			if remote == "origin" && name == "main" {
				row.OriginMain = "present"
			}
		}
		row.RemoteBranches = strings.Join(names, ",")
	}
	return row, nil
}

func parseDoltRemoteBranchName(full string) (remote, branch string, ok bool) {
	parts := strings.Split(strings.TrimSpace(full), "/")
	if len(parts) < 3 || parts[0] != "remotes" || parts[1] == "" {
		return "", "", false
	}
	branch = strings.Join(parts[2:], "/")
	if branch == "" {
		return "", "", false
	}
	return parts[1], branch, true
}

func inventoryCSVRows(out string, fields int) ([][]string, error) {
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = fields
	if _, err := r.Read(); err != nil {
		return nil, err
	}
	var rows [][]string
	for {
		record, err := r.Read()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		rows = append(rows, record)
	}
}

func managedDoltInventoryFields(rows []managedDoltInventoryRow) []string {
	fields := make([]string, 0, len(rows))
	for _, row := range rows {
		fields = append(fields, strings.Join([]string{"database", row.Database, "schema_version", row.SchemaVersion, "content_hash", row.ContentHash, "remotes", row.Remotes, "remote_branches", row.RemoteBranches, "origin_main", row.OriginMain}, "\t"))
	}
	return fields
}
