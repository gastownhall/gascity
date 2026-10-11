package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWorkDirExportFixture(t *testing.T, rels ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range rels {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRuntimeWorkdirExportListsSurvivingEntries(t *testing.T) {
	dir := writeWorkDirExportFixture(t,
		".gc/controller.token",
		".gc/settings.json",
		".beads/doltlite/hq.db",
		".beads/metadata.json",
		"src/main.go",
	)
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"workdir-export", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workdir-export: %v; stderr: %s", err, stderr.String())
	}
	want := ".beads\n.beads/metadata.json\n.gc\n.gc/settings.json\nsrc\nsrc/main.go\n"
	if stdout.String() != want {
		t.Fatalf("stdout =\n%s\nwant\n%s", stdout.String(), want)
	}
}

func TestRuntimeWorkdirExportNullSeparated(t *testing.T) {
	dir := writeWorkDirExportFixture(t, ".gc/events.jsonl", "a")
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"workdir-export", "--null", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workdir-export --null: %v; stderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), ".gc\x00a\x00"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRuntimeWorkdirExportMissingDirFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"workdir-export", filepath.Join(t.TempDir(), "missing")})
	if err := cmd.Execute(); err == nil {
		t.Fatal("workdir-export of a missing dir succeeded")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty on failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc runtime workdir-export") {
		t.Fatalf("stderr = %q, want a gc runtime workdir-export error", stderr.String())
	}
}
