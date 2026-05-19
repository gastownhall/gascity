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
	"strconv"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

type managedDoltSQLHealthReport struct {
	QueryReady      bool
	ReadOnly        string
	ConnectionCount string
}

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
	"dolt_cluster":           {},
	"performance_schema":     {},
	"sys":                    {},
	managedDoltProbeDatabase: {},
}

// managedDoltReadOnlyProbeStatementsFor returns the read-only probe statements
// for db. Each invocation creates the persistent GC-owned probe table inside db
// (idempotent) and rewrites a single row to test writability. db must be a real
// user database; the empty string returns nil so the caller can skip the probe
// entirely. The database identifier is backtick-quoted because Dolt derives DB
// names from repository directory names, which can start with a digit or contain
// other characters that need quoting.
func managedDoltReadOnlyProbeStatementsFor(db string) []string {
	db = strings.TrimSpace(db)
	if db == "" {
		return nil
	}
	target := managedDoltQuoteIdent(db) + "." + managedDoltQuoteIdent(managedDoltProbeTable)
	return []string{
		"CREATE TABLE IF NOT EXISTS " + target + " (k INT PRIMARY KEY)",
		"REPLACE INTO " + target + " VALUES (1)",
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
	_, err := runManagedDoltSQL(host, port, user, "-q", "SELECT active_branch()")
	if err == nil {
		return nil
	}
	if strings.TrimSpace(err.Error()) == "" {
		return fmt.Errorf("query probe failed")
	}
	return err
}

func managedDoltReadOnlyState(host, port, user string) (string, error) {
	if managedDoltPassword() != "" {
		return managedDoltReadOnlyStateDirectFn(host, port, user)
	}
	db, err := managedDoltSelectUserDatabase(host, port, user)
	if err != nil {
		return "unknown", err
	}
	if db == "" {
		return "unknown", errManagedDoltNoUserDatabase
	}
	_, err = runManagedDoltSQL(host, port, user, "-q", managedDoltReadOnlyProbeSQLFor(db))
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

func managedDoltHealthCheck(host, port, user string, checkReadOnly bool) (managedDoltSQLHealthReport, error) {
	if err := managedDoltQueryProbe(host, port, user); err != nil {
		return managedDoltSQLHealthReport{}, err
	}
	report := managedDoltSQLHealthReport{
		QueryReady: true,
		ReadOnly:   "false",
	}
	if checkReadOnly {
		state, err := managedDoltReadOnlyState(host, port, user)
		if err != nil {
			if !errors.Is(err, errManagedDoltNoUserDatabase) {
				return managedDoltSQLHealthReport{}, err
			}
		}
		report.ReadOnly = state
	}
	if count, err := managedDoltConnectionCount(host, port, user); err == nil {
		report.ConnectionCount = count
	}
	return report, nil
}

func managedDoltHealthCheckFields(report managedDoltSQLHealthReport) []string {
	if !report.QueryReady {
		return []string{"query_ready\tfalse"}
	}
	return []string{
		"query_ready\ttrue",
		"read_only\t" + report.ReadOnly,
		"connection_count\t" + report.ConnectionCount,
	}
}

func managedDoltPassword() string {
	return strings.TrimSpace(os.Getenv("GC_DOLT_PASSWORD"))
}

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
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = managedDoltPassword()
	cfg.Net = "tcp"
	cfg.Addr = host + ":" + port
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	cfg.AllowNativePasswords = true
	return sql.Open("mysql", cfg.FormatDSN())
}

func managedDoltQueryProbeDirect(host, port, user string) error {
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	var branch sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&branch); err != nil {
		return err
	}
	return nil
}

func managedDoltReadOnlyStateDirect(host, port, user string) (string, error) {
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return "unknown", err
	}
	defer db.Close() //nolint:errcheck

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

	userDB, err := managedDoltSelectUserDatabaseFromConn(ctx, conn)
	if err != nil {
		return "unknown", err
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
	defer db.Close() //nolint:errcheck

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
	defer db.Close() //nolint:errcheck

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
	host = managedDoltConnectHost(host)
	port = strings.TrimSpace(port)
	if port == "" {
		return "", fmt.Errorf("missing port")
	}
	user = strings.TrimSpace(user)
	if user == "" {
		user = "root"
	}
	baseArgs := []string{
		"--host", host,
		"--port", port,
		"--user", user,
		"--password", managedDoltPassword(),
		"--no-tls",
		"sql",
	}
	ctx, cancel := context.WithTimeout(context.Background(), managedDoltSQLCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dolt", append(baseArgs, args...)...)
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
