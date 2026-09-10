//go:build integration || dolt_integration

package dolt_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The two-clone race of gp-c04p on a real dolt: a hub remote, one clone per
// city, the same automatic change written by both, then `gc dolt pull` on
// one side and a pull back on the other. Run with
// `go test -tags dolt_integration -run RealDoltTwoClones ./examples/bd/dolt/`.

// bdIssuesSchema is the shape of bd's issues table that matters here: the
// primary key, text and JSON columns the equality predicate must handle, a
// case-insensitive collation on title (the predicate compares bytes, not
// collation-equal strings), and the three columns automatic sweeps touch.
const bdIssuesSchema = `CREATE TABLE issues (
  id VARCHAR(255) PRIMARY KEY,
  title VARCHAR(500) NOT NULL COLLATE utf8mb4_0900_ai_ci,
  description TEXT NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'open',
  assignee VARCHAR(255),
  metadata JSON DEFAULT (JSON_OBJECT()),
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  closed_at DATETIME,
  defer_until DATETIME,
  row_lock VARCHAR(64)
);
CREATE TABLE labels (issue_id VARCHAR(255) NOT NULL, label VARCHAR(255) NOT NULL, PRIMARY KEY (issue_id, label));`

type twoCloneScenario struct {
	name     string
	seed     string // rows both cities start from
	citadel  string // citadel's automatic write (pushed to the hub first)
	jadegate string // jadegate's automatic write (the side that pulls)
}

// twoClones builds hub_<name> and the clones <name>-citadel and
// <name>-jadegate under dataDir, applies each city's write and pushes
// citadel's, so jadegate's pull is the conflicted one.
func twoClones(t *testing.T, doltPath, dataDir, hubParent string, sc twoCloneScenario) (citadel, jadegate string) {
	t.Helper()
	hub := filepath.Join(hubParent, "hub_"+sc.name)
	seedDir := filepath.Join(hubParent, "seed_"+sc.name)
	for _, d := range []string{hub, seedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runDoltForCompactTest(t, doltPath, hub, "init", "--name", "Gas City", "--email", "test@example.com")
	runDoltForCompactTest(t, doltPath, seedDir, "init", "--name", "Gas City", "--email", "test@example.com")
	runDoltForCompactTest(t, doltPath, seedDir, "sql", "-q", bdIssuesSchema+" "+sc.seed)
	runDoltForCompactTest(t, doltPath, seedDir, "add", ".")
	runDoltForCompactTest(t, doltPath, seedDir, "commit", "-m", "seed")
	runDoltForCompactTest(t, doltPath, seedDir, "remote", "add", "origin", "file://"+hub)
	runDoltForCompactTest(t, doltPath, seedDir, "push", "--force", "--set-upstream", "origin", "main")

	citadel = sc.name + "-citadel"
	jadegate = sc.name + "-jadegate"
	runDoltForCompactTest(t, doltPath, dataDir, "clone", "file://"+hub, citadel)
	runDoltForCompactTest(t, doltPath, dataDir, "clone", "file://"+hub, jadegate)
	citadelDir := filepath.Join(dataDir, citadel)
	jadegateDir := filepath.Join(dataDir, jadegate)
	runDoltForCompactTest(t, doltPath, citadelDir, "sql", "-q", sc.citadel)
	runDoltForCompactTest(t, doltPath, citadelDir, "commit", "--author", "bd <bd@citadel>", "-Am", "bd: wake 1 expired defer(s)")
	runDoltForCompactTest(t, doltPath, citadelDir, "push", "origin", "main")
	runDoltForCompactTest(t, doltPath, jadegateDir, "sql", "-q", sc.jadegate)
	runDoltForCompactTest(t, doltPath, jadegateDir, "commit", "--author", "bd <bd@jadegate>", "-Am", "bd: wake 1 expired defer(s)")
	return citadel, jadegate
}

// waitForDoltServerDB is waitForDoltServerQueryForCompactTest against a
// named database (the fixture has no "beads" database).
func waitForDoltServerDB(t *testing.T, doltPath string, port int, db string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastOut []byte
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, doltPath,
			"--host", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--user", "root", "--no-tls",
			"--use-db", db, "sql", "-q", "SELECT 1")
		cmd.Env = append(filteredEnv("DOLT_CLI_PASSWORD"), "DOLT_CLI_PASSWORD=")
		lastOut, lastErr = cmd.CombinedOutput()
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("dolt sql-server did not become query-ready on port %d: %v\n%s", port, lastErr, lastOut)
}

func serverSQL(t *testing.T, doltPath string, port int, db, query string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath,
		"--host", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--user", "root", "--no-tls",
		"--use-db", db, "sql", "--result-format", "csv", "-q", query)
	cmd.Env = append(filteredEnv("DOLT_CLI_PASSWORD"), "DOLT_CLI_PASSWORD=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("server sql %q on %s: %v\n%s", query, db, err, out)
	}
	return strings.TrimSpace(string(out))
}

func runPullScript(t *testing.T, root, cityPath, dataDir string, port int, db string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(root, "commands", "pull", "run.sh"), "--db", db)
	cmd.Env = append(filteredEnv(
		"PATH", "GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_DATA_DIR", "GC_DOLT_PORT",
		"GC_DOLT_HOST", "GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_DOLT_MANAGED_LOCAL",
	),
		"PATH="+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_DOLT_MANAGED_LOCAL=1",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(err, &exitErr) {
			t.Fatalf("run pull: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	return string(out), code
}

func TestRealDoltTwoClonesPull(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}
	root := repoRoot(t)
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hubParent := t.TempDir()

	const deferred = `INSERT INTO issues (id, title, description, status, metadata, created_at, updated_at, defer_until, row_lock) VALUES ('hw-1', 'tile', 'founder-gated', 'deferred', JSON_OBJECT('gc.owner', 'citadel'), '2026-09-09 00:00:00', '2026-09-09 00:00:00', '2026-09-10 05:00:00', 'base'); INSERT INTO labels VALUES ('hw-1', 'owner:citadel');`
	const wakeCitadel = `UPDATE issues SET status = 'open', defer_until = NULL, updated_at = '2026-09-10 05:00:17', row_lock = 'citadel-lock' WHERE id = 'hw-1'`
	const wakeJadegate = `UPDATE issues SET status = 'open', defer_until = NULL, updated_at = '2026-09-10 05:00:18', row_lock = 'jadegate-lock' WHERE id = 'hw-1'`

	scenarios := []twoCloneScenario{
		{
			// Class 1: bd's defer wake ran on both stores.
			name:     "deferwake",
			seed:     deferred,
			citadel:  wakeCitadel,
			jadegate: wakeJadegate,
		},
		{
			// Class 2: the convoy autoclose ran on both stores — closed_at
			// differs too, so the merge step must NOT resolve it (the
			// writer-side fence in gc core is what closes this class).
			name:     "convoyautoclose",
			seed:     `INSERT INTO issues (id, title, description, status, metadata, created_at, updated_at, row_lock) VALUES ('hw-c', 'convoy', '', 'open', JSON_OBJECT(), '2026-09-09 00:00:00', '2026-09-09 00:00:00', 'base');`,
			citadel:  `UPDATE issues SET status = 'closed', closed_at = '2026-09-10 15:10:01', updated_at = '2026-09-10 15:10:01', row_lock = 'citadel-lock', metadata = JSON_OBJECT('close_reason', 'convoy autoclose: all children closed') WHERE id = 'hw-c'`,
			jadegate: `UPDATE issues SET status = 'closed', closed_at = '2026-09-10 15:10:03', updated_at = '2026-09-10 15:10:03', row_lock = 'jadegate-lock', metadata = JSON_OBJECT('close_reason', 'convoy autoclose: all children closed') WHERE id = 'hw-c'`,
		},
		{
			// Two titles equal under the column's case-insensitive collation
			// are not the same title; the predicate compares bytes.
			name:     "collation",
			seed:     deferred,
			citadel:  wakeCitadel + `; UPDATE issues SET title = 'Fix API' WHERE id = 'hw-1'`,
			jadegate: wakeJadegate + `; UPDATE issues SET title = 'fix api' WHERE id = 'hw-1'`,
		},
		{
			// A benign row and a real one in the same pull: nothing is
			// written, the benign row included.
			name:     "mixed",
			seed:     deferred + ` INSERT INTO issues (id, title, description, status, metadata, created_at, updated_at, row_lock) VALUES ('hw-2', 'task', '', 'open', JSON_OBJECT(), '2026-09-09 00:00:00', '2026-09-09 00:00:00', 'base');`,
			citadel:  wakeCitadel + `; UPDATE issues SET status = 'closed', closed_at = '2026-09-10 06:00:00', updated_at = '2026-09-10 06:00:00', row_lock = 'citadel-lock' WHERE id = 'hw-2'`,
			jadegate: wakeJadegate + `; UPDATE issues SET status = 'in_progress', assignee = 'jg-worker', updated_at = '2026-09-10 06:00:01', row_lock = 'jadegate-lock' WHERE id = 'hw-2'`,
		},
	}
	clones := map[string][2]string{}
	for _, sc := range scenarios {
		c, j := twoClones(t, doltPath, dataDir, hubParent, sc)
		clones[sc.name] = [2]string{c, j}
	}

	port, pid := startRealDoltServerForCompactTest(t, doltPath, dataDir)
	writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, pid)
	waitForDoltServerDB(t, doltPath, port, clones["deferwake"][0])

	const rowQuery = "SELECT id, status, assignee, updated_at, closed_at, defer_until, row_lock, metadata FROM issues ORDER BY id"
	head := func(db string) string {
		return serverSQL(t, doltPath, port, db, "SELECT commit_hash FROM dolt_log LIMIT 1")
	}
	assertClean := func(db string) {
		t.Helper()
		if got := serverSQL(t, doltPath, port, db, "SELECT COUNT(*) FROM dolt_status"); !strings.HasSuffix(got, "\n0") {
			t.Errorf("%s dolt_status not empty: %q", db, got)
		}
		if got := serverSQL(t, doltPath, port, db, "SELECT COUNT(*) FROM dolt_conflicts"); !strings.HasSuffix(got, "\n0") {
			t.Errorf("%s dolt_conflicts not empty: %q", db, got)
		}
	}

	t.Run("deferwake resolves to the remote and the pull back is a fast-forward", func(t *testing.T) {
		citadel, jadegate := clones["deferwake"][0], clones["deferwake"][1]
		citadelRow := serverSQL(t, doltPath, port, citadel, rowQuery)
		jadegateRowBefore := serverSQL(t, doltPath, port, jadegate, rowQuery)
		if citadelRow == jadegateRowBefore {
			t.Fatalf("fixture: the two wakes must differ:\n%s", citadelRow)
		}

		out, code := runPullScript(t, root, cityPath, dataDir, port, jadegate)
		if code != 0 {
			t.Fatalf("jadegate pull exit = %d\n%s", code, out)
		}
		if !strings.Contains(out, jadegate+": pulled from file://"+filepath.Join(hubParent, "hub_deferwake")+" (resolved 1 row_lock/updated_at-only conflict(s) in issues: hw-1)") {
			t.Fatalf("output missing the resolved line:\n%s", out)
		}
		if got := serverSQL(t, doltPath, port, jadegate, rowQuery); got != citadelRow {
			t.Fatalf("jadegate row after pull = %q, want citadel's %q", got, citadelRow)
		}
		assertClean(jadegate)
		if msg := serverSQL(t, doltPath, port, jadegate, "SELECT message FROM dolt_log LIMIT 1"); !strings.Contains(msg, "gc dolt pull: merge origin/main (row_lock/updated_at-only conflicts in issues resolved to the remote)") {
			t.Fatalf("jadegate HEAD message = %q, want the merge commit", msg)
		}

		// Sync back: jadegate pushes its merge, citadel pulls — no conflict,
		// same rows, same head on both sides.
		serverSQL(t, doltPath, port, jadegate, "CALL DOLT_PUSH('origin', 'main')")
		out, code = runPullScript(t, root, cityPath, dataDir, port, citadel)
		if code != 0 {
			t.Fatalf("citadel pull back exit = %d\n%s", code, out)
		}
		if !strings.Contains(out, citadel+": pulled from file://") || strings.Contains(out, "resolved") {
			t.Fatalf("citadel pull back should be a plain pull:\n%s", out)
		}
		if got := serverSQL(t, doltPath, port, citadel, rowQuery); got != citadelRow {
			t.Fatalf("citadel row after pull back = %q, want %q", got, citadelRow)
		}
		if head(citadel) != head(jadegate) {
			t.Fatalf("heads differ after both pulls: citadel %s jadegate %s", head(citadel), head(jadegate))
		}
		assertClean(citadel)
	})

	t.Run("convoyautoclose is not the benign class and writes nothing", func(t *testing.T) {
		_, jadegate := clones["convoyautoclose"][0], clones["convoyautoclose"][1]
		before := serverSQL(t, doltPath, port, jadegate, rowQuery)
		headBefore := head(jadegate)

		out, code := runPullScript(t, root, cityPath, dataDir, port, jadegate)
		if code != 1 {
			t.Fatalf("jadegate pull exit = %d, want 1\n%s", code, out)
		}
		for _, want := range []string{
			jadegate + ": conflict hw-c: updated_at,closed_at,row_lock",
			jadegate + ": ERROR: pull failed: 1 conflict(s) in issues need manual resolution; nothing was written",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "pulled from") {
			t.Errorf("a refused pull must not report success:\n%s", out)
		}
		if got := serverSQL(t, doltPath, port, jadegate, rowQuery); got != before {
			t.Fatalf("jadegate row changed under a refused pull:\n%s\nwas:\n%s", got, before)
		}
		if head(jadegate) != headBefore {
			t.Fatalf("jadegate HEAD moved under a refused pull: %s -> %s", headBefore, head(jadegate))
		}
		assertClean(jadegate)
	})

	t.Run("collation-equal titles are a real conflict", func(t *testing.T) {
		_, jadegate := clones["collation"][0], clones["collation"][1]
		before := serverSQL(t, doltPath, port, jadegate, rowQuery+", title")
		headBefore := head(jadegate)
		if got := serverSQL(t, doltPath, port, jadegate, "SELECT title = 'Fix API' FROM issues WHERE id = 'hw-1'"); !strings.HasSuffix(got, "\n1") && !strings.HasSuffix(got, "\ntrue") {
			t.Fatalf("fixture: title should be collation-equal to citadel's, got %q", got)
		}

		out, code := runPullScript(t, root, cityPath, dataDir, port, jadegate)
		if code != 1 {
			t.Fatalf("jadegate pull exit = %d, want 1\n%s", code, out)
		}
		if !strings.Contains(out, jadegate+": conflict hw-1: title,updated_at,row_lock") {
			t.Errorf("output missing the title in the differing columns:\n%s", out)
		}
		if got := serverSQL(t, doltPath, port, jadegate, rowQuery+", title"); got != before {
			t.Fatalf("rows changed under a refused pull:\n%s\nwas:\n%s", got, before)
		}
		if head(jadegate) != headBefore {
			t.Fatalf("jadegate HEAD moved under a refused pull")
		}
		assertClean(jadegate)
	})

	t.Run("mixed leaves the benign row untouched too", func(t *testing.T) {
		_, jadegate := clones["mixed"][0], clones["mixed"][1]
		before := serverSQL(t, doltPath, port, jadegate, rowQuery)
		headBefore := head(jadegate)

		out, code := runPullScript(t, root, cityPath, dataDir, port, jadegate)
		if code != 1 {
			t.Fatalf("jadegate pull exit = %d, want 1\n%s", code, out)
		}
		for _, want := range []string{
			jadegate + ": conflict hw-1: updated_at,row_lock",
			jadegate + ": conflict hw-2: status,assignee,updated_at,closed_at,row_lock",
			jadegate + ": ERROR: pull failed: 1 conflict(s) in issues need manual resolution; nothing was written",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		if got := serverSQL(t, doltPath, port, jadegate, rowQuery); got != before {
			t.Fatalf("rows changed under a refused pull:\n%s\nwas:\n%s", got, before)
		}
		if head(jadegate) != headBefore {
			t.Fatalf("jadegate HEAD moved under a refused pull")
		}
		assertClean(jadegate)
	})
}

// The same race in CLI mode (no dolt sql-server): `dolt pull` leaves the
// merge in the working set, the resolving transaction runs against the
// files, and a refused merge is aborted on disk.
func TestRealDoltTwoClonesPullCLI(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}
	root := repoRoot(t)
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hubParent := t.TempDir()
	const deferred = `INSERT INTO issues (id, title, description, status, metadata, created_at, updated_at, defer_until, row_lock) VALUES ('hw-1', 'tile', 'founder-gated', 'deferred', JSON_OBJECT('gc.owner', 'citadel'), '2026-09-09 00:00:00', '2026-09-09 00:00:00', '2026-09-10 05:00:00', 'base');`
	benign := twoCloneScenario{
		name:     "clibenign",
		seed:     deferred,
		citadel:  `UPDATE issues SET status = 'open', defer_until = NULL, updated_at = '2026-09-10 05:00:17', row_lock = 'citadel-lock' WHERE id = 'hw-1'`,
		jadegate: `UPDATE issues SET status = 'open', defer_until = NULL, updated_at = '2026-09-10 05:00:18', row_lock = 'jadegate-lock' WHERE id = 'hw-1'`,
	}
	real := twoCloneScenario{
		name:     "clireal",
		seed:     deferred,
		citadel:  `UPDATE issues SET status = 'closed', closed_at = '2026-09-10 06:00:00', updated_at = '2026-09-10 06:00:00', row_lock = 'citadel-lock' WHERE id = 'hw-1'`,
		jadegate: `UPDATE issues SET status = 'open', defer_until = NULL, updated_at = '2026-09-10 05:00:18', row_lock = 'jadegate-lock' WHERE id = 'hw-1'`,
	}
	existing := twoCloneScenario{
		name:     "cliexisting",
		seed:     deferred,
		citadel:  `UPDATE issues SET status = 'closed', closed_at = '2026-09-10 06:00:00', updated_at = '2026-09-10 06:00:00', row_lock = 'citadel-lock' WHERE id = 'hw-1'`,
		jadegate: `UPDATE issues SET status = 'open', defer_until = NULL, updated_at = '2026-09-10 05:00:18', row_lock = 'jadegate-lock' WHERE id = 'hw-1'`,
	}
	bc, bj := twoClones(t, doltPath, dataDir, hubParent, benign)
	_, rj := twoClones(t, doltPath, dataDir, hubParent, real)
	_, ej := twoClones(t, doltPath, dataDir, hubParent, existing)
	// The script's CLI branch (unchanged here) discovers the remote from the
	// compact .dolt/remotes.json; a dolt 2.x clone records it in
	// repo_state.json instead, so the fixture writes the file the branch reads.
	for _, db := range []string{bj, rj, ej} {
		hub := filepath.Join(hubParent, "hub_"+strings.TrimSuffix(db, "-jadegate"))
		if err := os.WriteFile(filepath.Join(dataDir, db, ".dolt", "remotes.json"), []byte(`{"name":"origin","url":"file://`+hub+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const rowQuery = "SELECT id, status, updated_at, closed_at, defer_until, row_lock FROM issues ORDER BY id"
	cliSQL := func(db, q string) string {
		return strings.TrimSpace(runDoltForCompactTest(t, doltPath, filepath.Join(dataDir, db), "sql", "-r", "csv", "-q", q))
	}
	// Port 1 is never a listening dolt server: the script takes the CLI path.
	const noServer = 1

	t.Run("benign resolves on disk", func(t *testing.T) {
		citadelRow := cliSQL(bc, rowQuery)
		out, code := runPullScript(t, root, cityPath, dataDir, noServer, bj)
		if code != 0 {
			t.Fatalf("exit = %d\n%s", code, out)
		}
		if !strings.Contains(out, bj+": pulled from file://") || !strings.Contains(out, "(resolved 1 row_lock/updated_at-only conflict(s) in issues: hw-1)") {
			t.Fatalf("output missing the resolved line:\n%s", out)
		}
		if got := cliSQL(bj, rowQuery); got != citadelRow {
			t.Fatalf("jadegate row = %q, want citadel's %q", got, citadelRow)
		}
		if got := cliSQL(bj, "SELECT is_merging FROM dolt_merge_status"); !strings.HasSuffix(got, "\nfalse") {
			t.Fatalf("merge still in progress: %q", got)
		}
		if got := cliSQL(bj, "SELECT COUNT(*) FROM dolt_status"); !strings.HasSuffix(got, "\n0") {
			t.Fatalf("working set not clean: %q", got)
		}
	})

	t.Run("real conflict is aborted on disk", func(t *testing.T) {
		before := cliSQL(rj, rowQuery)
		headBefore := cliSQL(rj, "SELECT commit_hash FROM dolt_log LIMIT 1")
		out, code := runPullScript(t, root, cityPath, dataDir, noServer, rj)
		if code != 1 {
			t.Fatalf("exit = %d, want 1\n%s", code, out)
		}
		if !strings.Contains(out, rj+": conflict hw-1: status,updated_at,closed_at,defer_until,row_lock") {
			t.Errorf("output missing the row detail:\n%s", out)
		}
		if !strings.Contains(out, rj+": ERROR: pull failed: 1 conflict(s) in issues need manual resolution; nothing was written") {
			t.Errorf("output missing the manual-resolution error:\n%s", out)
		}
		if got := cliSQL(rj, "SELECT is_merging FROM dolt_merge_status"); !strings.HasSuffix(got, "\nfalse") {
			t.Fatalf("refused merge left in progress: %q", got)
		}
		if got := cliSQL(rj, "SELECT COUNT(*) FROM dolt_conflicts"); !strings.HasSuffix(got, "\n0") {
			t.Fatalf("conflicts left on disk: %q", got)
		}
		if got := cliSQL(rj, rowQuery); got != before {
			t.Fatalf("rows changed under a refused pull:\n%s\nwas:\n%s", got, before)
		}
		if got := cliSQL(rj, "SELECT commit_hash FROM dolt_log LIMIT 1"); got != headBefore {
			t.Fatalf("HEAD moved under a refused pull")
		}
	})

	t.Run("a merge already in progress is left alone", func(t *testing.T) {
		dir := filepath.Join(dataDir, ej)
		// Someone started the pull by hand and is mid-resolution.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pull := exec.CommandContext(ctx, doltPath, "pull", "origin", "main")
		pull.Dir = dir
		pull.Env = append(os.Environ(), "NO_COLOR=1")
		if pullOut, err := pull.CombinedOutput(); err == nil {
			t.Fatalf("fixture: the manual pull should have conflicted:\n%s", pullOut)
		}
		if got := cliSQL(ej, "SELECT is_merging FROM dolt_merge_status"); !strings.HasSuffix(got, "\ntrue") {
			t.Fatalf("fixture: no merge in progress: %q", got)
		}
		conflictsBefore := cliSQL(ej, "SELECT COUNT(*) FROM dolt_conflicts")

		out, code := runPullScript(t, root, cityPath, dataDir, noServer, ej)
		if code != 1 {
			t.Fatalf("exit = %d, want 1\n%s", code, out)
		}
		if !strings.Contains(out, ej+": ERROR: a merge is already in progress; resolve it or abort it (dolt merge --abort) before pulling; nothing was done") {
			t.Fatalf("output missing the refusal:\n%s", out)
		}
		if got := cliSQL(ej, "SELECT is_merging FROM dolt_merge_status"); !strings.HasSuffix(got, "\ntrue") {
			t.Fatalf("the existing merge was aborted: %q", got)
		}
		if got := cliSQL(ej, "SELECT COUNT(*) FROM dolt_conflicts"); got != conflictsBefore {
			t.Fatalf("the existing merge's conflicts changed: %q -> %q", conflictsBefore, got)
		}
	})
}
