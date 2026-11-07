package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/doltpool"
)

type managedDoltSQLHealthReport struct {
	QueryReady      bool
	ReadOnly        string
	ConnectionCount string
	// Database is the scope database the check was asked to prove. It is
	// empty when the caller named none, and then DatabaseReady and
	// DatabaseError carry no information.
	Database string
	// DatabaseReady is true only when Database is present, its default
	// branch head resolves, and (with the read-only check) a sample write
	// and read-back against it succeed. A server that answers SQL while the
	// city database is missing or unreadable must not read as healthy.
	DatabaseReady bool
	// DatabaseError says which database check failed when DatabaseReady is
	// false.
	DatabaseError string
}

// managedDoltHealthTarget names the scope database a health check must prove
// resolvable and readable. DataDir is the managed server's data directory and
// is set only when gc owns the server: an external endpoint's databases live
// somewhere this host cannot see.
type managedDoltHealthTarget struct {
	Database string
	DataDir  string
}

// managedDoltCommitHashPattern matches a Dolt commit hash as DOLT_HASHOF
// returns it: 32 characters of Dolt's base32 alphabet.
var managedDoltCommitHashPattern = regexp.MustCompile(`^[0-9a-v]{32}$`)

// managedDoltProbeDatabase is the legacy dedicated probe database name. The
// read-only probe no longer creates or writes to it: Dolt's autostats subsystem
// (statspro) randomly elects one server-wide database to host the on-disk
// stats backing store, and a tiny dedicated DB lost the lottery in production
// by accumulating stats noms it was never meant to hold. The probe now writes
// into a GC-owned table inside a discovered user database instead so it shares
// a backing store with real workload traffic. This constant remains so
// `gc dolt-state reset-probe` can still drop the legacy DB on demand and so
// `gc dolt-state init` can keep rejecting it as a user-supplied database name.
const managedDoltProbeDatabase = "__gc_probe"

const managedDoltProbeTable = "__gc_read_only_probe"

var errManagedDoltNoUserDatabase = errors.New("no user database available for managed Dolt read-only probe")

var (
	managedDoltQueryProbeDirectFn      = managedDoltQueryProbeDirect
	managedDoltReadOnlyStateDirectFn   = managedDoltReadOnlyStateDirect
	managedDoltDatabaseHeadDirectFn    = managedDoltDatabaseHeadDirect
	managedDoltProbeReadBackDirectFn   = managedDoltProbeReadBackDirect
	managedDoltConnectionCountDirectFn = managedDoltConnectionCountDirect
	managedDoltResetProbeDirectFn      = managedDoltResetProbeDirect
	managedDoltSQLCommandTimeout       = 5 * time.Second
)

// managedDoltSystemDatabases lists databases that the read-only probe must not
// pick as its write target. `__gc_probe` is included so existing legacy data
// is left in place while we migrate off of it.
var managedDoltSystemDatabases = map[string]struct{}{
	"information_schema":     {},
	"mysql":                  {},
	"dolt":                   {},
	"dolt_cluster":           {},
	"performance_schema":     {},
	"sys":                    {},
	managedDoltProbeDatabase: {},
}

// managedDoltReadOnlyProbeStatementsFor returns the read-only probe statements
// for db. Each invocation creates the persistent GC-owned probe table inside db
// (idempotent), rewrites a single row to test writability, and registers the
// table in dolt_ignore so history flattening can never first-commit it: a
// non-ignored table that lives only in the working set is committed by the
// compaction flatten's `DOLT_COMMIT -Am`, which drifts the database hash and
// quarantines GC for that database (hq June 2026, daa 2026-08-04). The
// registration is last so a read-only server still fails on the CREATE or
// REPLACE, which is what the read-only classification keys on, and it uses
// INSERT IGNORE so an operator's explicit `ignored = 0` override survives.
// The probe opens with USE because dolt_ignore is a session-root-backed system
// table: both probe paths (the dolt CLI over --host and the direct driver)
// connect with no default schema, and writing dolt_ignore through a qualified
// name on such a session fails with "no root value found in session" — USE is
// read-only, so the read-only classification still keys on the CREATE/REPLACE.
// db must be a real user database; the empty string returns nil so the caller
// can skip the probe entirely. The database identifier is backtick-quoted
// because Dolt derives DB names from repository directory names, which can start
// with a digit or contain other characters that need quoting.
func managedDoltReadOnlyProbeStatementsFor(db string) []string {
	db = strings.TrimSpace(db)
	if db == "" {
		return nil
	}
	quotedDB := managedDoltQuoteIdent(db)
	target := quotedDB + "." + managedDoltQuoteIdent(managedDoltProbeTable)
	return []string{
		"USE " + quotedDB,
		"CREATE TABLE IF NOT EXISTS " + target + " (k INT PRIMARY KEY)",
		"REPLACE INTO " + target + " VALUES (1)",
		"INSERT IGNORE INTO " + quotedDB + ".`dolt_ignore` (pattern, ignored) VALUES ('" + managedDoltProbeTable + "', 1)",
	}
}

// managedDoltQuoteIdent backtick-quotes a SQL identifier and escapes any
// embedded backticks by doubling them (MySQL convention).
func managedDoltQuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// managedDoltReadOnlyProbeSQLFor joins managedDoltReadOnlyProbeStatementsFor
// into a single semicolon-terminated SQL string suitable for passing to
// `dolt sql -q`.
func managedDoltReadOnlyProbeSQLFor(db string) string {
	stmts := managedDoltReadOnlyProbeStatementsFor(db)
	if len(stmts) == 0 {
		return ""
	}
	return strings.Join(stmts, "; ") + ";"
}

func managedDoltQueryProbe(host, port, user string) error {
	if managedDoltPassword() != "" {
		return managedDoltQueryProbeDirectFn(host, port, user)
	}
	_, err := runManagedDoltSQL(host, port, user, "-r", "csv", "-q", "SELECT COUNT(*) AS cnt FROM information_schema.SCHEMATA")
	if err == nil {
		return nil
	}
	if strings.TrimSpace(err.Error()) == "" {
		return fmt.Errorf("query probe failed")
	}
	return err
}

func managedDoltReadOnlyState(host, port, user string) (string, error) {
	return managedDoltReadOnlyStateFor(host, port, user, "")
}

// managedDoltReadOnlyStateFor runs the read-only write probe against database,
// or against the first user database on the server when database is empty.
func managedDoltReadOnlyStateFor(host, port, user, database string) (string, error) {
	database = strings.TrimSpace(database)
	if managedDoltPassword() != "" {
		return managedDoltReadOnlyStateDirectFn(host, port, user, database)
	}
	db := database
	if db == "" {
		var err error
		db, err = managedDoltSelectUserDatabase(host, port, user)
		if err != nil {
			return "unknown", err
		}
	}
	if db == "" {
		return "unknown", errManagedDoltNoUserDatabase
	}
	_, err := runManagedDoltSQL(host, port, user, "-q", managedDoltReadOnlyProbeSQLFor(db))
	if err == nil {
		return "false", nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "read only") || strings.Contains(msg, "read-only") {
		return "true", nil
	}
	return "unknown", err
}

// managedDoltSelectUserDatabase returns the first database from SHOW DATABASES
// that is not a system database. It returns "" when the server has no user database.
func managedDoltSelectUserDatabase(host, port, user string) (string, error) {
	dbs, err := managedDoltSelectUserDatabases(host, port, user)
	if err != nil || len(dbs) == 0 {
		return "", err
	}
	return dbs[0], nil
}

func managedDoltSelectUserDatabases(host, port, user string) ([]string, error) {
	out, err := runManagedDoltSQL(host, port, user, "-r", "csv", "-q", "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	return managedDoltUserDatabasesFromCSV(out)
}

// managedDoltFirstUserDatabaseFromCSV parses csv-format `SHOW DATABASES`
// output and returns the first non-system database, or "" when none exist.
func managedDoltFirstUserDatabaseFromCSV(out string) (string, error) {
	dbs, err := managedDoltUserDatabasesFromCSV(out)
	if err != nil || len(dbs) == 0 {
		return "", err
	}
	return dbs[0], nil
}

func managedDoltUserDatabasesFromCSV(out string) ([]string, error) {
	reader := csv.NewReader(strings.NewReader(out))
	reader.FieldsPerRecord = 1
	dbs := []string{}
	for {
		record, err := reader.Read()
		if err == io.EOF {
			return dbs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse SHOW DATABASES csv: %w", err)
		}
		dbs = append(dbs, managedDoltUserDatabases(record)...)
	}
}

// managedDoltFirstUserDatabase scans database names and returns the first non-system
// database, or "" when none exist.
func managedDoltFirstUserDatabase(lines []string) string {
	dbs := managedDoltUserDatabases(lines)
	if len(dbs) == 0 {
		return ""
	}
	return dbs[0]
}

func managedDoltUserDatabases(lines []string) []string {
	dbs := []string{}
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if strings.EqualFold(name, "Database") {
			continue
		}
		if _, system := managedDoltSystemDatabases[strings.ToLower(name)]; system {
			continue
		}
		dbs = append(dbs, name)
	}
	return dbs
}

func managedDoltConnectionCount(host, port, user string) (string, error) {
	if managedDoltPassword() != "" {
		return managedDoltConnectionCountDirectFn(host, port, user)
	}
	out, err := runManagedDoltSQL(host, port, user, "-r", "csv", "-q", "SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST")
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		_, parseErr := strconv.Atoi(line)
		if parseErr == nil {
			return line, nil
		}
		return "", fmt.Errorf("parse connection count %q: %w", line, parseErr)
	}
	return "", fmt.Errorf("parse connection count from %q", strings.TrimSpace(out))
}

// managedDoltHealthCheck probes the server SQL layer with the read-only
// check and no scope database. Callers that need to skip the read-only probe
// or prove a database use managedDoltHealthCheckFor.
func managedDoltHealthCheck(host, port, user string) (managedDoltSQLHealthReport, error) {
	return managedDoltHealthCheckFor(host, port, user, true, managedDoltHealthTarget{})
}

// managedDoltHealthCheckFor is managedDoltHealthCheck with a scope database
// to prove. A failure to reach the server SQL layer is an error; a failure of
// the named database is reported in-band through DatabaseReady and
// DatabaseError so the caller can tell "server down" from "server up, city
// database gone" and say which.
func managedDoltHealthCheckFor(host, port, user string, checkReadOnly bool, target managedDoltHealthTarget) (managedDoltSQLHealthReport, error) {
	if err := managedDoltQueryProbe(host, port, user); err != nil {
		return managedDoltSQLHealthReport{}, err
	}
	report := managedDoltSQLHealthReport{
		QueryReady: true,
		ReadOnly:   "false",
		Database:   strings.TrimSpace(target.Database),
	}
	if report.Database != "" {
		if err := managedDoltCheckDatabase(host, port, user, report.Database, target.DataDir); err != nil {
			report.DatabaseError = err.Error()
			if checkReadOnly {
				report.ReadOnly = "unknown"
			}
			report.ConnectionCount = managedDoltConnectionCountBestEffort(host, port, user)
			return report, nil
		}
		report.DatabaseReady = true
	}
	if checkReadOnly {
		state, err := managedDoltReadOnlyStateFor(host, port, user, report.Database)
		switch {
		case err == nil:
		case report.Database != "":
			report.DatabaseReady = false
			report.DatabaseError = fmt.Sprintf("sample write to database %q failed: %v", report.Database, err)
		case !errors.Is(err, errManagedDoltNoUserDatabase):
			return managedDoltSQLHealthReport{}, err
		}
		report.ReadOnly = state
		if report.DatabaseReady && state == "false" {
			if err := managedDoltProbeReadBack(host, port, user, report.Database); err != nil {
				report.DatabaseReady = false
				report.DatabaseError = fmt.Sprintf("sample read from database %q failed: %v", report.Database, err)
			}
		}
	}
	report.ConnectionCount = managedDoltConnectionCountBestEffort(host, port, user)
	return report, nil
}

func managedDoltConnectionCountBestEffort(host, port, user string) string {
	count, err := managedDoltConnectionCount(host, port, user)
	if err != nil {
		return ""
	}
	return count
}

// managedDoltCheckDatabase proves database exists and its default branch head
// resolves. When dataDir is set it first requires the database's on-disk
// repository, because a server can keep serving a database whose directory
// was removed or swapped underneath it.
func managedDoltCheckDatabase(host, port, user, database, dataDir string) error {
	if strings.TrimSpace(dataDir) != "" {
		if err := managedDoltDatabaseOnDisk(dataDir, database); err != nil {
			return err
		}
	}
	if _, err := managedDoltDatabaseHead(host, port, user, database); err != nil {
		return fmt.Errorf("database %q default branch head unresolvable: %w", database, err)
	}
	return nil
}

// managedDoltDatabaseOnDisk reports whether dataDir holds a Dolt repository
// for database. Dolt matches database names case-insensitively, so a
// directory that differs only in case counts.
func managedDoltDatabaseOnDisk(dataDir, database string) error {
	if managedDoltIsRepoDir(filepath.Join(dataDir, database)) {
		return nil
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("database %q: reading data dir %s: %w", database, dataDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.EqualFold(entry.Name(), database) && managedDoltIsRepoDir(filepath.Join(dataDir, entry.Name())) {
			return nil
		}
	}
	return fmt.Errorf("database %q absent from data dir %s", database, dataDir)
}

func managedDoltIsRepoDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".dolt"))
	return err == nil && info.IsDir()
}

// managedDoltDatabaseHead returns the commit hash of database's default
// branch head. A fresh session lands on the default branch, so USE fails
// when the database is missing from the catalog (errno 1049) or its default
// branch cannot be resolved (errno 1105).
func managedDoltDatabaseHead(host, port, user, database string) (string, error) {
	if managedDoltPassword() != "" {
		return managedDoltDatabaseHeadDirectFn(host, port, user, database)
	}
	out, err := runManagedDoltSQL(host, port, user, "-r", "csv", "-q",
		"USE "+managedDoltQuoteIdent(database)+"; SELECT DOLT_HASHOF('HEAD') AS head")
	if err != nil {
		return "", err
	}
	return managedDoltParseHead(out)
}

func managedDoltParseHead(out string) (string, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	head := strings.TrimSpace(lines[len(lines)-1])
	if !managedDoltCommitHashPattern.MatchString(head) {
		return "", fmt.Errorf("unexpected head %q", strings.TrimSpace(out))
	}
	return head, nil
}

// managedDoltProbeReadBack reads back the row the read-only probe just wrote
// into database, so the check covers the read path as well as the write.
func managedDoltProbeReadBack(host, port, user, database string) error {
	if managedDoltPassword() != "" {
		return managedDoltProbeReadBackDirectFn(host, port, user, database)
	}
	out, err := runManagedDoltSQL(host, port, user, "-r", "csv", "-q", managedDoltProbeReadBackSQLFor(database))
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return managedDoltCheckReadBackCount(strings.TrimSpace(lines[len(lines)-1]))
}

func managedDoltProbeReadBackSQLFor(database string) string {
	quotedDB := managedDoltQuoteIdent(database)
	return "USE " + quotedDB + "; SELECT COUNT(*) AS cnt FROM " + quotedDB + "." + managedDoltQuoteIdent(managedDoltProbeTable) + " WHERE k = 1"
}

func managedDoltCheckReadBackCount(text string) error {
	count, err := strconv.Atoi(text)
	if err != nil {
		return fmt.Errorf("parse probe row count %q: %w", text, err)
	}
	if count != 1 {
		return fmt.Errorf("probe row count = %d after write, want 1", count)
	}
	return nil
}

func managedDoltHealthCheckFields(report managedDoltSQLHealthReport) []string {
	if !report.QueryReady {
		return []string{"query_ready\tfalse"}
	}
	fields := []string{
		"query_ready\ttrue",
		"read_only\t" + report.ReadOnly,
		"connection_count\t" + report.ConnectionCount,
	}
	if report.Database == "" {
		return fields
	}
	fields = append(fields, "database\t"+managedDoltFieldValue(report.Database))
	if report.DatabaseReady {
		return append(fields, "database_ready\ttrue")
	}
	return append(fields,
		"database_ready\tfalse",
		"database_error\t"+managedDoltFieldValue(report.DatabaseError),
	)
}

// managedDoltFieldValue flattens a value onto one line of the tab-separated
// key/value report the provider script parses.
func managedDoltFieldValue(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func managedDoltPassword() string {
	return strings.TrimSpace(os.Getenv("GC_DOLT_PASSWORD"))
}

// managedDoltOpenDB returns the shared pooled server-level *sql.DB (no
// database selected) for a managed Dolt endpoint. The handle is owned by
// internal/doltpool — callers must NOT Close it.
func managedDoltOpenDB(host, port, user string) (*sql.DB, error) {
	host = managedDoltConnectHost(host)
	port = strings.TrimSpace(port)
	if port == "" {
		return nil, fmt.Errorf("missing port")
	}
	user = strings.TrimSpace(user)
	if user == "" {
		user = "root"
	}
	return doltpool.Open(host, port, user, managedDoltPassword(), "")
}

func managedDoltQueryProbeDirect(host, port, user string) error {
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	var cnt int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) AS cnt FROM information_schema.SCHEMATA").Scan(&cnt); err != nil {
		return err
	}
	return nil
}

func managedDoltReadOnlyStateDirect(host, port, user, database string) (string, error) {
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return "unknown", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return "unknown", err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return "unknown", err
	}
	defer conn.Close() //nolint:errcheck

	userDB := database
	if userDB == "" {
		userDB, err = managedDoltSelectUserDatabaseFromConn(ctx, conn)
		if err != nil {
			return "unknown", err
		}
	}
	if userDB == "" {
		return "unknown", errManagedDoltNoUserDatabase
	}
	for _, query := range managedDoltReadOnlyProbeStatementsFor(userDB) {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "read only") || strings.Contains(msg, "read-only") {
				return "true", nil
			}
			return "unknown", err
		}
	}
	return "false", nil
}

// managedDoltDatabaseConnDirect opens a pooled connection with database
// selected. The caller closes the returned connection and cancels its context.
func managedDoltDatabaseConnDirect(host, port, user, database string) (*sql.Conn, context.Context, context.CancelFunc, error) {
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := db.Conn(ctx)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "USE "+managedDoltQuoteIdent(database)); err != nil {
		conn.Close() //nolint:errcheck
		cancel()
		return nil, nil, nil, err
	}
	return conn, ctx, cancel, nil
}

func managedDoltDatabaseHeadDirect(host, port, user, database string) (string, error) {
	conn, ctx, cancel, err := managedDoltDatabaseConnDirect(host, port, user, database)
	if err != nil {
		return "", err
	}
	defer cancel()
	defer conn.Close() //nolint:errcheck
	var head string
	if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF('HEAD')").Scan(&head); err != nil {
		return "", err
	}
	return managedDoltParseHead(head)
}

func managedDoltProbeReadBackDirect(host, port, user, database string) error {
	conn, ctx, cancel, err := managedDoltDatabaseConnDirect(host, port, user, database)
	if err != nil {
		return err
	}
	defer cancel()
	defer conn.Close() //nolint:errcheck
	quotedDB := managedDoltQuoteIdent(database)
	var count int
	query := "SELECT COUNT(*) FROM " + quotedDB + "." + managedDoltQuoteIdent(managedDoltProbeTable) + " WHERE k = 1"
	if err := conn.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return err
	}
	return managedDoltCheckReadBackCount(strconv.Itoa(count))
}

func managedDoltSelectUserDatabaseFromConn(ctx context.Context, conn *sql.Conn) (string, error) {
	dbs, err := managedDoltSelectUserDatabasesFromConn(ctx, conn)
	if err != nil || len(dbs) == 0 {
		return "", err
	}
	return dbs[0], nil
}

func managedDoltSelectUserDatabasesFromConn(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return managedDoltUserDatabases(names), nil
}

func managedDoltConnectionCountDirect(host, port, user string) (string, error) {
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return "", err
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST").Scan(&count); err != nil {
		return "", err
	}
	return strconv.Itoa(count), nil
}

func managedDoltResetProbe(host, port, user string) error {
	if managedDoltPassword() != "" {
		return managedDoltResetProbeDirectFn(host, port, user)
	}
	if _, err := runManagedDoltSQL(host, port, user, "-q", "DROP DATABASE IF EXISTS "+managedDoltProbeDatabase); err != nil {
		return err
	}
	dbs, err := managedDoltSelectUserDatabases(host, port, user)
	if err != nil {
		return err
	}
	for _, db := range dbs {
		if _, err := runManagedDoltSQL(host, port, user, "-q", managedDoltDropProbeTableSQLFor(db)); err != nil {
			return err
		}
	}
	return nil
}

func managedDoltResetProbeDirect(host, port, user string) error {
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+managedDoltProbeDatabase); err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck
	dbs, err := managedDoltSelectUserDatabasesFromConn(ctx, conn)
	if err != nil {
		return err
	}
	for _, userDB := range dbs {
		if _, err := conn.ExecContext(ctx, managedDoltDropProbeTableSQLFor(userDB)); err != nil {
			return err
		}
	}
	return nil
}

func managedDoltDropProbeTableSQLFor(db string) string {
	return "DROP TABLE IF EXISTS " + managedDoltQuoteIdent(db) + "." + managedDoltQuoteIdent(managedDoltProbeTable)
}

func runManagedDoltSQL(host, port, user string, args ...string) (string, error) {
	return runManagedDoltSQLContext(context.Background(), host, port, user, args...)
}

// runManagedDoltSQLContext is the context-aware form of runManagedDoltSQL: the
// command is bounded by both managedDoltSQLCommandTimeout and the caller's ctx,
// so a hung server cannot outlive a canceled reconcile tick.
func runManagedDoltSQLContext(parent context.Context, host, port, user string, args ...string) (string, error) {
	host = managedDoltConnectHost(host)
	port = strings.TrimSpace(port)
	if port == "" {
		return "", fmt.Errorf("missing port")
	}
	user = strings.TrimSpace(user)
	if user == "" {
		user = "root"
	}
	// A configured password never goes in argv (world-readable
	// /proc/<pid>/cmdline); it travels only in the environment (owner-only
	// /proc/<pid>/environ). dolt reads DOLT_CLI_PASSWORD only when argv has no
	// --password, and a `default` CLI profile that stores a password injects its
	// own --password first, outranking the variable. So every caller with a
	// configured password takes the pooled driver connection (managedDoltOpenDB,
	// managedDoltOpenDatabase) instead of this runner. An empty password is
	// passed as an explicit empty --password, which discloses nothing and masks
	// that profile. The variable is exported either way so dolt never falls
	// back to an interactive password prompt.
	password := managedDoltPassword()
	baseArgs := []string{
		"--host", host,
		"--port", port,
		"--user", user,
	}
	if password == "" {
		baseArgs = append(baseArgs, "--password", "")
	}
	baseArgs = append(baseArgs, "--no-tls", "sql")
	ctx, cancel := context.WithTimeout(parent, managedDoltSQLCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dolt", append(baseArgs, args...)...)
	cmd.Env = withEnvValue(os.Environ(), "DOLT_CLI_PASSWORD", password)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "", fmt.Errorf("timed out after %s: %s", managedDoltSQLCommandTimeout, msg)
		}
		return "", fmt.Errorf("timed out after %s", managedDoltSQLCommandTimeout)
	}
	if err == nil {
		return string(out), nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return "", err
	}
	return "", fmt.Errorf("%s", msg)
}
