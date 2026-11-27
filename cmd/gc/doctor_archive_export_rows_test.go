package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archiveRowsTestCity builds a city with one rig whose .beads/metadata.json
// names dolt database db, and clears the env that relocates the archive.
func archiveRowsTestCity(t *testing.T, db string) (cityDir, rigDir string) {
	t.Helper()
	cityDir = t.TempDir()
	rigDir = filepath.Join(cityDir, "rigs", "alpha")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"backend":"dolt","dolt_mode":"server","dolt_database":"` + db + `"}`
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GC_JSONL_ARCHIVE_REPO", "GC_PACK_STATE_DIR", "GC_CITY_RUNTIME_DIR"} {
		t.Setenv(key, "")
	}
	return cityDir, rigDir
}

// writeArchive creates an archive repo (a directory with .git) holding n issue
// records for db at <repo>/<db>/issues.jsonl.
func writeArchive(t *testing.T, repo, db string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, db), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, db, "issues.jsonl"), []byte(strings.Repeat("{\"id\":\"x\"}\n", n)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestArchiveExportRowsResolvesTheRigDatabaseFromMetadata: the rig's database
// name comes from its .beads/metadata.json, and names the archive directory.
// The lookup used to hand the rig directory itself to the metadata reader,
// which cannot read a directory, so the archive was never consulted.
func TestArchiveExportRowsResolvesTheRigDatabaseFromMetadata(t *testing.T) {
	cityDir, rigDir := archiveRowsTestCity(t, "alpha_db")
	writeArchive(t, filepath.Join(cityDir, ".gc", "runtime", "packs", "core", "jsonl-archive"), "alpha_db", 4)

	n, ok, err := archiveExportRowsFor(cityDir)(rigDir)
	if err != nil || !ok || n != 4 {
		t.Fatalf("archiveExportRowsFor = %d, %v, %v; want 4, true, nil", n, ok, err)
	}
}

// TestArchiveExportRowsReadsOnlyTheRepoTheExporterWrites: jsonl-export writes
// to the first candidate that is a git repo. A stale legacy archive beside it,
// however many rows it holds, is not evidence.
func TestArchiveExportRowsReadsOnlyTheRepoTheExporterWrites(t *testing.T) {
	cityDir, rigDir := archiveRowsTestCity(t, "alpha_db")
	writeArchive(t, filepath.Join(cityDir, ".gc", "runtime", "packs", "core", "jsonl-archive"), "alpha_db", 1)
	writeArchive(t, filepath.Join(cityDir, ".gc", "jsonl-archive"), "alpha_db", 9)

	n, ok, err := archiveExportRowsFor(cityDir)(rigDir)
	if err != nil || !ok || n != 1 {
		t.Fatalf("archiveExportRowsFor = %d, %v, %v; want the current repo's 1 row, not the legacy repo's 9", n, ok, err)
	}
}

// TestArchiveExportRowsWithoutAnArchiveRepo: with no candidate that is a git
// repo the exporter has written nothing yet, so there is no evidence.
func TestArchiveExportRowsWithoutAnArchiveRepo(t *testing.T) {
	cityDir, rigDir := archiveRowsTestCity(t, "alpha_db")

	n, ok, err := archiveExportRowsFor(cityDir)(rigDir)
	if err != nil || ok || n != 0 {
		t.Fatalf("archiveExportRowsFor = %d, %v, %v; want 0, false, nil", n, ok, err)
	}
}
