package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #9257: gc supervisor install reports which tier supplied each admitted name.
// Names and tiers only: the report never carries a value.
func TestSupervisorServiceEnvSourcesRecordTheTier(t *testing.T) {
	gcHome := t.TempDir()
	const secretValue = "s3cret-value-not-for-the-report"
	if err := os.WriteFile(filepath.Join(gcHome, "secrets.env"), []byte("GC_DOLT_PASSWORD="+secretValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_SUPERVISOR_ENV", "GC_DOLT_PASSWORD,PROVENANCE_TEST_VAR")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("PROVENANCE_TEST_VAR", "/provenance/bin")
	t.Setenv("ANTHROPIC_BASE_URL", "https://provenance.test")

	_, sources := supervisorServiceExtraEnvWithSources()

	// A shell-only routing name is refused, so it must not be admitted at all.
	if got, ok := sources["ANTHROPIC_BASE_URL"]; ok {
		t.Fatalf("ANTHROPIC_BASE_URL admitted from %q; a shell-only routing name must be refused", got)
	}
	if got := sources["PROVENANCE_TEST_VAR"]; got != "GC_SUPERVISOR_ENV" {
		t.Fatalf("PROVENANCE_TEST_VAR source = %q, want GC_SUPERVISOR_ENV", got)
	}
	if got := sources["GC_DOLT_PASSWORD"]; got != "secrets.env" {
		t.Fatalf("GC_DOLT_PASSWORD source = %q, want secrets.env", got)
	}

	var buf bytes.Buffer
	reportSupervisorEnvSources(&buf, sources)
	report := buf.String()
	if !strings.Contains(report, "service env PROVENANCE_TEST_VAR from GC_SUPERVISOR_ENV") || !strings.Contains(report, "service env GC_DOLT_PASSWORD from secrets.env") {
		t.Fatalf("report = %q, want the names with their tiers", report)
	}
	if strings.Contains(report, secretValue) || strings.Contains(report, "provenance.test") || strings.Contains(report, "/provenance/bin") {
		t.Fatalf("report leaked a value: %q", report)
	}
}
