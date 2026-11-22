//go:build integration || dolt_integration

package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// flattenWindowCity is a flatten-eligible, no-remote real-Dolt city whose
// compact run is wrapped by a dolt shim that commits a same-count UPDATE from
// a second client right after the flatten call returns, before compact
// resumes (#8502).
type flattenWindowCity struct {
	doltPath  string
	root      string
	cityPath  string
	dataDir   string
	port      int
	shimPath  string
	fired     string
	writerLog string
}

func newFlattenWindowCity(t *testing.T) *flattenWindowCity {
	t.Helper()
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}
	c := &flattenWindowCity{doltPath: doltPath, root: repoRoot(t), cityPath: t.TempDir()}
	c.dataDir = filepath.Join(c.cityPath, ".beads", "dolt")
	dbDir := filepath.Join(c.dataDir, "beads")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	runDoltForCompactTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
	runDoltForCompactTest(t, doltPath, dbDir, "sql", "-q",
		"CREATE TABLE beads (id int primary key, name varchar(20)); INSERT INTO beads VALUES (1, 'first');")
	runDoltForCompactTest(t, doltPath, dbDir, "add", ".")
	runDoltForCompactTest(t, doltPath, dbDir, "commit", "-m", "seed first bead")
	for i := 2; i <= 4; i++ {
		runDoltForCompactTest(t, doltPath, dbDir, "sql", "-q", fmt.Sprintf("INSERT INTO beads VALUES (%d, 'b%d');", i, i))
		runDoltForCompactTest(t, doltPath, dbDir, "commit", "-Am", fmt.Sprintf("seed bead %d", i))
	}
	if err := os.WriteFile(filepath.Join(dbDir, ".compact-full-history"), nil, 0o644); err != nil {
		t.Fatalf("write .compact-full-history: %v", err)
	}

	port, pid := startRealDoltServerForCompactTest(t, doltPath, c.dataDir)
	c.port = port
	writeManagedRuntimeStateForScriptWithPID(t, c.cityPath, port, pid)
	waitForDoltServerQueryForCompactTest(t, doltPath, port)

	// The shim runs the real dolt and forwards its output unchanged; right
	// after the flatten call returns it commits a same-count UPDATE from a
	// second client, once.
	shimDir := t.TempDir()
	c.fired = filepath.Join(shimDir, "writer-fired")
	c.writerLog = filepath.Join(shimDir, "writer.log")
	shim := fmt.Sprintf(`#!/bin/sh
real=%q
"$real" "$@"
rc=$?
case "$*" in
*"DOLT_RESET('--soft'"*"DOLT_COMMIT('-Am', 'compaction: flatten history')"*)
  if [ ! -e %q ]; then
    : > %q
    DOLT_CLI_PASSWORD= "$real" --host 127.0.0.1 --port %d --user root --no-tls --use-db beads \
      sql -q "UPDATE beads SET name='x' WHERE id=1; CALL DOLT_COMMIT('-am','bd: update')" > %q 2>&1
  fi
  ;;
esac
exit $rc
`, doltPath, c.fired, c.fired, port, c.writerLog)
	c.shimPath = filepath.Join(shimDir, "dolt")
	if err := os.WriteFile(c.shimPath, []byte(shim), 0o755); err != nil {
		t.Fatalf("write dolt shim: %v", err)
	}
	return c
}

func (c *flattenWindowCity) compact(t *testing.T, doltPath string) (string, error) {
	t.Helper()
	return runCompactScriptForRealDoltTest(t, doltPath, c.root, c.cityPath, c.dataDir, c.port, nil,
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=1",
	)
}

func (c *flattenWindowCity) query(t *testing.T, q string) []string {
	t.Helper()
	return doltServerQueryForCompactTest(t, c.doltPath, c.port, q)
}

func (c *flattenWindowCity) cell(t *testing.T, q string) string {
	t.Helper()
	rows := c.query(t, q)
	if len(rows) != 1 {
		t.Fatalf("query %q: want one row, got %q", q, rows)
	}
	return strings.TrimSpace(rows[0])
}

func (c *flattenWindowCity) statePath(kind string) string {
	return filepath.Join(c.cityPath, ".gc", "runtime", "packs", "dolt", kind, "beads")
}

// runWriterRace runs compact through the shim and asserts the writer actually
// committed inside the window. It returns compact's output and error.
func (c *flattenWindowCity) runWriterRace(t *testing.T) (string, error) {
	t.Helper()
	out, err := c.compact(t, c.shimPath)
	t.Logf("compact (err=%v):\n%s", err, out)
	if _, statErr := os.Stat(c.fired); statErr != nil {
		t.Fatalf("writer shim never fired after the flatten (%v); test did not exercise the window:\n%s", statErr, out)
	}
	if n := c.cell(t, "SELECT COUNT(*) FROM dolt_log WHERE message = 'bd: update'"); n != "1" {
		writerOut, _ := os.ReadFile(c.writerLog)
		t.Fatalf("writer commit missing from history: %q\nwriter output:\n%s", n, writerOut)
	}
	return out, err
}

// A routine writer that commits a same-count UPDATE right after the flatten's
// DOLT_COMMIT returns must not quarantine the database: the flatten preserved
// every row, and verification reads the flatten commit itself (#8502).
func TestCompactRealDoltWriterCommitJustAfterFlattenIsNotQuarantined(t *testing.T) {
	c := newFlattenWindowCity(t)
	out, err := c.runWriterRace(t)
	if strings.Contains(out, "quarantine and investigate") || strings.Contains(out, "same_row_count_hash_drift") {
		t.Fatalf("a writer commit after the flatten quarantined the database:\n%s", out)
	}
	if err != nil {
		t.Fatalf("compact script failed: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(c.statePath("compact-quarantine")); !os.IsNotExist(statErr) {
		t.Fatalf("quarantine marker written (stat err=%v):\n%s", statErr, out)
	}

	flatten := c.cell(t, "SELECT commit_hash FROM dolt_log WHERE message = 'compaction: flatten history'")
	writer := c.cell(t, "SELECT commit_hash FROM dolt_log WHERE message = 'bd: update'")
	head := c.cell(t, "SELECT HASHOF('HEAD')")
	if flatten == writer {
		t.Fatalf("flatten commit %s equals writer commit", flatten)
	}
	if head != writer {
		t.Fatalf("HEAD=%s, want the writer commit %s left in place", head, writer)
	}
	if parent := c.cell(t, fmt.Sprintf("SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = '%s'", writer)); parent != flatten {
		t.Fatalf("writer commit parent=%s, want flatten commit %s", parent, flatten)
	}
	if name := c.cell(t, "SELECT name FROM beads WHERE id = 1"); name != "x" {
		t.Fatalf("writer's updated row name=%q, want x", name)
	}
	if rows := c.cell(t, "SELECT COUNT(*) FROM beads"); rows != "4" {
		t.Fatalf("beads row count=%s, want 4", rows)
	}
	for _, want := range []string{
		"flatten_HEAD=" + flatten,
		"final_verify_HEAD=" + writer,
		"deferring full GC until the next quiet run",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("compact output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "running DOLT_GC('--full'") {
		t.Fatalf("full GC ran while a later writer was active:\n%s", out)
	}
	if _, statErr := os.Stat(c.statePath("compact-pending-gc")); statErr != nil {
		t.Fatalf("pending-GC marker missing after later-writer defer (%v):\n%s", statErr, out)
	}
}

// The pending-GC marker a later-writer defer leaves behind is retried on the
// next eligible run: full GC runs and history is not flattened again.
func TestCompactRealDoltPendingGCAfterLaterWriterRetriesFullGCWithoutReflatten(t *testing.T) {
	c := newFlattenWindowCity(t)
	if out, err := c.runWriterRace(t); err != nil {
		t.Fatalf("first compact failed: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(c.statePath("compact-pending-gc")); statErr != nil {
		t.Fatalf("pending-GC marker missing after later-writer defer: %v", statErr)
	}
	commitsBefore := c.cell(t, "SELECT COUNT(*) FROM dolt_log")
	headBefore := c.cell(t, "SELECT HASHOF('HEAD')")

	out, err := c.compact(t, c.doltPath)
	t.Logf("retry compact (err=%v):\n%s", err, out)
	if err != nil {
		t.Fatalf("pending-GC retry failed: %v\n%s", err, out)
	}
	for _, want := range []string{"pending_gc=present — retrying DOLT_GC --full", "pending-GC retry duration="} {
		if !strings.Contains(out, want) {
			t.Fatalf("retry output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "flattening...") {
		t.Fatalf("pending-GC retry flattened again:\n%s", out)
	}
	if got := c.cell(t, "SELECT COUNT(*) FROM dolt_log"); got != commitsBefore {
		t.Fatalf("commit count changed across pending-GC retry: before=%s after=%s", commitsBefore, got)
	}
	if got := c.cell(t, "SELECT HASHOF('HEAD')"); got != headBefore {
		t.Fatalf("HEAD changed across pending-GC retry: before=%s after=%s", headBefore, got)
	}
	if _, statErr := os.Stat(c.statePath("compact-pending-gc")); !os.IsNotExist(statErr) {
		t.Fatalf("pending-GC marker not cleared after successful retry (stat err=%v)", statErr)
	}
}
