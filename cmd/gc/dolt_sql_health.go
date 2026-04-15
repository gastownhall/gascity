package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type managedDoltSQLHealthReport struct {
	QueryReady      bool
	ReadOnly        string
	ConnectionCount string
}

func managedDoltQueryProbe(host, port, user string) error {
	_, err := runManagedDoltSQL(host, port, user, "-q", "SELECT active_branch()")
	if err == nil {
		return nil
	}
	if strings.TrimSpace(err.Error()) == "" {
		return fmt.Errorf("query probe failed")
	}
	return err
}

func managedDoltReadOnly(host, port, user string) bool {
	state, err := managedDoltReadOnlyState(host, port, user)
	return err == nil && state == "true"
}

func managedDoltReadOnlyState(host, port, user string) (string, error) {
	_, err := runManagedDoltSQL(host, port, user, "-q", "CREATE DATABASE IF NOT EXISTS __gc_probe; USE __gc_probe; CREATE TABLE IF NOT EXISTS __probe (k INT PRIMARY KEY); REPLACE INTO __probe VALUES (1); DROP TABLE __probe; DROP DATABASE __gc_probe;")
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
		if err == nil {
			report.ReadOnly = state
		}
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
		"--password", os.Getenv("GC_DOLT_PASSWORD"),
		"--no-tls",
		"sql",
	}
	cmd := exec.Command("dolt", append(baseArgs, args...)...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return "", err
	}
	return "", fmt.Errorf("%s", msg)
}
