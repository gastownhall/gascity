package main

import (
	"context"
	"database/sql"
	"fmt"
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

const managedDoltProbeDatabase = "__gc_probe"

var (
	managedDoltQueryProbeDirectFn      = managedDoltQueryProbeDirect
	managedDoltReadOnlyStateDirectFn   = managedDoltReadOnlyStateDirect
	managedDoltConnectionCountDirectFn = managedDoltConnectionCountDirect
	managedDoltResetProbeDirectFn      = managedDoltResetProbeDirect
	managedDoltSQLCommandTimeout       = 5 * time.Second
)

// The probe database is intentionally persistent. Dropping Dolt databases leaves
// .dolt_dropped_databases backups, so the health check keeps a stable table and
// rewrites a single row to test writability.
var managedDoltReadOnlyProbeStatements = [...]string{
	"CREATE DATABASE IF NOT EXISTS " + managedDoltProbeDatabase,
	"CREATE TABLE IF NOT EXISTS " + managedDoltProbeDatabase + ".__probe (k INT PRIMARY KEY)",
	"REPLACE INTO " + managedDoltProbeDatabase + ".__probe VALUES (1)",
}

var managedDoltReadOnlyProbeSQL = strings.Join(managedDoltReadOnlyProbeStatements[:], "; ") + ";"

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
	_, err := runManagedDoltSQL(host, port, user, "-q", managedDoltReadOnlyProbeSQL)
	if err == nil {
		return "false", nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "read only") || strings.Contains(msg, "read-only") {
		return "true", nil
	}
	return "unknown", err
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
			return managedDoltSQLHealthReport{}, err
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

	for _, query := range managedDoltReadOnlyProbeStatements {
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
	_, err := runManagedDoltSQL(host, port, user, "-q", "DROP DATABASE IF EXISTS "+managedDoltProbeDatabase)
	return err
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
	_, err = db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+managedDoltProbeDatabase)
	return err
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
