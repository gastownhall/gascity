package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestManagedDoltReadOnlyProbeDoesNotDropProbeDatabase(t *testing.T) {
	for _, query := range append(append([]string{}, managedDoltReadOnlyProbeStatements[:]...), managedDoltReadOnlyProbeSQL) {
		assertNoManagedDoltProbeDrop(t, "read-only probe", query)
	}
	assertManagedDoltProbeWrites(t, "joined read-only probe", managedDoltReadOnlyProbeSQL)
	foundWriteStatement := false
	for _, query := range managedDoltReadOnlyProbeStatements {
		if strings.Contains(query, "REPLACE INTO __gc_probe.__probe VALUES (1)") {
			foundWriteStatement = true
		}
	}
	if !foundWriteStatement {
		t.Fatal("read-only probe statements must include a write to __gc_probe.__probe")
	}
}

func assertNoManagedDoltProbeDrop(t *testing.T, label, text string) {
	t.Helper()
	dropProbeDatabase := regexp.MustCompile("(?i)\\bDROP\\s+DATABASE\\s+(IF\\s+EXISTS\\s+)?`?__gc_probe`?")
	dropProbeTable := regexp.MustCompile("(?i)\\bDROP\\s+TABLE\\s+(IF\\s+EXISTS\\s+)?(`?__gc_probe`?\\.)?`?__probe`?")
	if dropProbeDatabase.MatchString(text) {
		t.Fatalf("%s must not drop __gc_probe: %s", label, text)
	}
	if dropProbeTable.MatchString(text) {
		t.Fatalf("%s must keep __gc_probe.__probe stable: %s", label, text)
	}
}

func assertManagedDoltProbeWrites(t *testing.T, label, text string) {
	t.Helper()
	if !strings.Contains(text, "REPLACE INTO __gc_probe.__probe VALUES (1)") {
		t.Fatalf("%s must write to __gc_probe.__probe: %s", label, text)
	}
}

func TestManagedDoltHealthCheckWithPasswordUsesDirectHelpers(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$INVOCATION_FILE\"\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_DOLT_PASSWORD", "secret")

	oldQuery := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	defer func() {
		managedDoltQueryProbeDirectFn = oldQuery
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltConnectionCountDirectFn = oldConnCount
	}()

	calledQuery := false
	calledReadOnly := false
	calledConnCount := false
	managedDoltQueryProbeDirectFn = func(host, port, user string) error {
		calledQuery = true
		if host != "0.0.0.0" || port != "3311" || user != "root" {
			t.Fatalf("query direct args = %q %q %q", host, port, user)
		}
		return nil
	}
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) {
		calledReadOnly = true
		return "false", nil
	}
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) {
		calledConnCount = true
		return "7", nil
	}

	report, err := managedDoltHealthCheck("0.0.0.0", "3311", "root", true)
	if err != nil {
		t.Fatalf("managedDoltHealthCheck() error = %v", err)
	}
	if !calledQuery || !calledReadOnly || !calledConnCount {
		t.Fatalf("direct helper calls = query:%v readOnly:%v connCount:%v", calledQuery, calledReadOnly, calledConnCount)
	}
	if !report.QueryReady || report.ReadOnly != "false" || report.ConnectionCount != "7" {
		t.Fatalf("managedDoltHealthCheck() = %+v", report)
	}
	if invocation, err := os.ReadFile(invocationFile); err == nil && strings.TrimSpace(string(invocation)) != "" {
		t.Fatalf("dolt argv should not be used when GC_DOLT_PASSWORD is set: %s", string(invocation))
	}
}

func TestManagedDoltHealthCheckWithPasswordPropagatesReadOnlyProbeErrors(t *testing.T) {
	t.Setenv("GC_DOLT_PASSWORD", "secret")

	oldQuery := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	defer func() {
		managedDoltQueryProbeDirectFn = oldQuery
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltConnectionCountDirectFn = oldConnCount
	}()

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error {
		return nil
	}
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) {
		return "unknown", errors.New("read-only probe failed")
	}
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) {
		t.Fatal("connection count should not run after read-only probe failure")
		return "", nil
	}

	_, err := managedDoltHealthCheck("127.0.0.1", "3311", "root", true)
	if err == nil {
		t.Fatal("managedDoltHealthCheck() error = nil, want read-only probe failure")
	}
	if !strings.Contains(err.Error(), "read-only probe failed") {
		t.Fatalf("managedDoltHealthCheck() error = %v, want read-only probe failure", err)
	}
}

func TestRunManagedDoltSQLTimesOut(t *testing.T) {
	binDir := t.TempDir()
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldTimeout := managedDoltSQLCommandTimeout
	managedDoltSQLCommandTimeout = 50 * time.Millisecond
	defer func() { managedDoltSQLCommandTimeout = oldTimeout }()

	_, err := runManagedDoltSQL("127.0.0.1", "3311", "root", "-q", "SELECT 1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("runManagedDoltSQL() error = %v, want timeout", err)
	}
}
