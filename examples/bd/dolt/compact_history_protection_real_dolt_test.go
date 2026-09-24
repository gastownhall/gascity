//go:build integration || dolt_integration

package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Real-Dolt regressions for #5958: the scheduled compactor must never flatten
// history this city did not grow, and must never force-push over a shared
// remote without an explicit opt-in.

const historyProtectionThreshold = 20

type historyProtectionCity struct {
	t       *testing.T
	dolt    string
	root    string
	city    string
	dataDir string
	port    int
}

func newHistoryProtectionCity(t *testing.T) *historyProtectionCity {
	t.Helper()
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}
	c := &historyProtectionCity{t: t, dolt: doltPath, root: repoRoot(t), city: t.TempDir()}
	c.dataDir = filepath.Join(c.city, ".beads", "dolt")
	if err := os.MkdirAll(c.dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	return c
}

func (c *historyProtectionCity) dbDir() string { return filepath.Join(c.dataDir, "beads") }

func (c *historyProtectionCity) start() {
	c.t.Helper()
	port, pid := startRealDoltServerForCompactTest(c.t, c.dolt, c.dataDir)
	writeManagedRuntimeStateForScriptWithPID(c.t, c.city, port, pid)
	waitForDoltServerQueryForCompactTest(c.t, c.dolt, port)
	c.port = port
}

// sql runs a query against the managed server and returns the CSV rows
// without the header.
func (c *historyProtectionCity) sql(q string) []string {
	c.t.Helper()
	return doltServerQueryForCompactTest(c.t, c.dolt, c.port, q)
}

func (c *historyProtectionCity) cell(q string) string {
	c.t.Helper()
	rows := c.sql(q)
	if len(rows) == 0 {
		return ""
	}
	return strings.TrimSpace(rows[0])
}

func (c *historyProtectionCity) commits() int {
	c.t.Helper()
	n, err := strconv.Atoi(c.cell("SELECT COUNT(*) FROM dolt_log"))
	if err != nil {
		c.t.Fatalf("commit count: %v", err)
	}
	return n
}

func (c *historyProtectionCity) head() string {
	return c.cell("SELECT HASHOF('HEAD')")
}

// rootCommit is the parentless commit reachable from HEAD (by ancestry, not date).
func (c *historyProtectionCity) rootCommit() string {
	return c.cell("SELECT l.commit_hash FROM dolt_log l JOIN dolt_commit_ancestors a ON a.commit_hash = l.commit_hash WHERE a.parent_hash IS NULL")
}

func (c *historyProtectionCity) tag() string {
	return c.cell("SELECT tag_hash FROM dolt_tags WHERE tag_name = 'gc-compact-base'")
}

func (c *historyProtectionCity) addCommits(from, n int) {
	c.t.Helper()
	for i := from; i < from+n; i++ {
		c.sql(fmt.Sprintf("INSERT INTO beads VALUES (%d, 'c%d'); CALL DOLT_COMMIT('-Am', 'city commit %d');", i, i, i))
	}
}

func (c *historyProtectionCity) compactCommand(extraEnv ...string) (string, error) {
	c.t.Helper()
	return c.compactCommandWithArgs(nil, extraEnv...)
}

func (c *historyProtectionCity) compactCommandWithArgs(args []string, extraEnv ...string) (string, error) {
	c.t.Helper()
	env := append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=" + strconv.Itoa(historyProtectionThreshold)}, extraEnv...)
	out, err := runCompactScriptForRealDoltTest(c.t, c.dolt, c.root, c.city, c.dataDir, c.port, args, env...)
	c.t.Logf("compact %v (err=%v):\n%s", args, err, out)
	return out, err
}

func (c *historyProtectionCity) compact(extraEnv ...string) string {
	c.t.Helper()
	out, err := c.compactCommand(extraEnv...)
	if err != nil {
		c.t.Fatalf("compact failed: %v\n%s", err, out)
	}
	return out
}

// seedTeamHistory creates a Dolt database whose history was grown elsewhere:
// init + schema + n commits (n+2 commits in total).
func seedTeamHistory(t *testing.T, doltPath, dir string, n int) {
	t.Helper()
	seedTeamHistoryWithDates(t, doltPath, dir, n, nil)
}

// seedTeamHistoryWithDates is seedTeamHistory with author dates overridden for
// the given team commit numbers (clock skew on the machines that grew them).
func seedTeamHistoryWithDates(t *testing.T, doltPath, dir string, n int, dates map[int]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	runDoltForCompactTest(t, doltPath, dir, "init", "--name", "Team", "--email", "team@example.com")
	runDoltForCompactTest(t, doltPath, dir, "sql", "-q", "CREATE TABLE beads (id int primary key, name varchar(20));")
	runDoltForCompactTest(t, doltPath, dir, "commit", "-Am", "schema")
	for i := 1; i <= n; i++ {
		runDoltForCompactTest(t, doltPath, dir, "sql", "-q", fmt.Sprintf("INSERT INTO beads VALUES (%d, 'b%d');", i, i))
		args := []string{"commit", "-Am", fmt.Sprintf("team commit %d", i)}
		if date, ok := dates[i]; ok {
			args = append(args, "--date", date)
		}
		runDoltForCompactTest(t, doltPath, dir, args...)
	}
}

func doltLogCount(t *testing.T, doltPath, dir string) int {
	t.Helper()
	out := runDoltForCompactTest(t, doltPath, dir, "log", "--oneline")
	return len(strings.Split(strings.TrimSpace(out), "\n"))
}

// sharedRemote seeds a team history, publishes it to a file:// remote, and
// returns the remote directory.
func sharedRemote(t *testing.T, doltPath string, n int) string {
	t.Helper()
	upstream := filepath.Join(t.TempDir(), "upstream")
	remote := filepath.Join(t.TempDir(), "remote")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatalf("mkdir remote: %v", err)
	}
	seedTeamHistory(t, doltPath, upstream, n)
	runDoltForCompactTest(t, doltPath, upstream, "remote", "add", "origin", "file://"+remote)
	runDoltForCompactTest(t, doltPath, upstream, "push", "--set-upstream", "origin", "main")
	return remote
}

func remoteCommitCount(t *testing.T, doltPath, remote string) int {
	t.Helper()
	parent := t.TempDir()
	runDoltForCompactTest(t, doltPath, parent, "clone", "file://"+remote, "probe")
	return doltLogCount(t, doltPath, filepath.Join(parent, "probe"))
}

// Adopted clone of a shared remote: no flatten, no push, bare GC only, and the
// shared remote keeps every commit — even once the city grows past threshold.
func TestCompactHistoryRealDoltAdoptedCloneOfSharedRemoteIsNeverRewritten(t *testing.T) {
	c := newHistoryProtectionCity(t)
	remote := sharedRemote(t, c.dolt, 40)
	runDoltForCompactTest(t, c.dolt, c.dataDir, "clone", "file://"+remote, "beads")
	c.start()
	if got := c.commits(); got != 42 {
		t.Fatalf("seed commits = %d, want 42", got)
	}
	c.addCommits(1000, historyProtectionThreshold+5)

	out := c.compact()
	if !strings.Contains(out, "db=beads remote=origin remotes=1 — history may be shared") ||
		!strings.Contains(out, "db=beads bare-gc duration=") {
		t.Fatalf("shared-history guard did not skip to bare GC:\n%s", out)
	}
	if strings.Contains(out, "flattening") || strings.Contains(out, "pushed compacted") {
		t.Fatalf("guarded database was flattened or pushed:\n%s", out)
	}
	if got := c.commits(); got != 42+historyProtectionThreshold+5 {
		t.Fatalf("local commits = %d, want %d", got, 42+historyProtectionThreshold+5)
	}
	if got := remoteCommitCount(t, c.dolt, remote); got != 42 {
		t.Fatalf("shared remote commits = %d, want 42 (rewritten)", got)
	}
	if tag := c.tag(); tag != "" {
		t.Fatalf("guarded database was stamped with gc-compact-base=%s", tag)
	}

	// --skip-fetch and .no-sync must not bypass the guard.
	if err := os.WriteFile(filepath.Join(c.dbDir(), ".no-sync"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := c.compactCommandWithArgs([]string{"--skip-fetch"}); err != nil || !strings.Contains(out, "skipping flatten") {
		t.Fatalf("--skip-fetch/.no-sync bypassed the guard: %v\n%s", err, out)
	}
	if got := c.commits(); got != 42+historyProtectionThreshold+5 {
		t.Fatalf("local commits after --skip-fetch/.no-sync run = %d", got)
	}
}

// Explicit opt-in: the flatten and force-push happen, but the watermark still
// protects the adopted history, so the remote keeps it plus one flatten commit.
func TestCompactHistoryRealDoltAllowFederatedSquashesOnlyCityGrowth(t *testing.T) {
	c := newHistoryProtectionCity(t)
	remote := sharedRemote(t, c.dolt, 40)
	runDoltForCompactTest(t, c.dolt, c.dataDir, "clone", "file://"+remote, "beads")
	c.start()
	adoptedHead := c.head()

	out := c.compact("GC_DOLT_COMPACT_ALLOW_FEDERATED=1")
	if !strings.Contains(out, "set gc-compact-base="+adoptedHead) {
		t.Fatalf("first sight did not stamp the adopted HEAD:\n%s", out)
	}
	c.addCommits(1000, historyProtectionThreshold+5)
	out = c.compact("GC_DOLT_COMPACT_ALLOW_FEDERATED=1")
	if !strings.Contains(out, "remote=origin pushed compacted main") {
		t.Fatalf("opt-in run did not push:\n%s", out)
	}
	if got := c.commits(); got != 43 {
		t.Fatalf("local commits = %d, want 43 (42 adopted + 1 flatten)", got)
	}
	if got := remoteCommitCount(t, c.dolt, remote); got != 43 {
		t.Fatalf("remote commits = %d, want 43 (adopted history kept)", got)
	}
}

// Incident shape: adopted database, no remote. Stamped at adoption HEAD; later
// growth past threshold squashes only the new commits, cycle after cycle.
func TestCompactHistoryRealDoltAdoptedNoRemoteSquashesOnlyNewCommits(t *testing.T) {
	c := newHistoryProtectionCity(t)
	seedTeamHistory(t, c.dolt, c.dbDir(), 40)
	c.start()
	adoptedHead := c.head()

	out := c.compact()
	if got := c.commits(); got != 42 {
		t.Fatalf("first run flattened adopted history: commits=%d\n%s", got, out)
	}
	if tag := c.tag(); tag != adoptedHead {
		t.Fatalf("gc-compact-base = %q, want adopted HEAD %s", tag, adoptedHead)
	}

	c.addCommits(1000, historyProtectionThreshold-5)
	out = c.compact()
	if !strings.Contains(out, "commits_since_base=15") || !strings.Contains(out, "below_threshold") {
		t.Fatalf("threshold must count commits since the watermark:\n%s", out)
	}
	c.addCommits(2000, 10)
	out = c.compact()
	if !strings.Contains(out, "flattening") {
		t.Fatalf("growth past threshold was not flattened:\n%s", out)
	}
	if got := c.commits(); got != 43 {
		t.Fatalf("commits = %d, want 43 (42 adopted + 1 flatten)", got)
	}
	if rows := c.cell("SELECT COUNT(*) FROM beads"); rows != "65" {
		t.Fatalf("rows = %s, want 65", rows)
	}
	if n := c.cell("SELECT COUNT(*) FROM dolt_log WHERE message LIKE 'team commit %'"); n != "40" {
		t.Fatalf("adopted commits in history = %s, want 40", n)
	}
	if tag := c.tag(); tag != adoptedHead {
		t.Fatalf("gc-compact-base moved to %s", tag)
	}

	c.addCommits(3000, historyProtectionThreshold+1)
	c.compact()
	if got := c.commits(); got != 43 {
		t.Fatalf("second growth cycle commits = %d, want 43", got)
	}
}

// A database this compactor already flattened (root's child is a flatten
// commit) keeps full flattening after the upgrade.
func TestCompactHistoryRealDoltPreviouslyCompactedDatabaseStillFullyFlattens(t *testing.T) {
	c := newHistoryProtectionCity(t)
	seedTeamHistory(t, c.dolt, c.dbDir(), 0)
	c.start()
	root := c.rootCommit()
	c.addCommits(1, 5)
	c.sql(fmt.Sprintf("CALL DOLT_RESET('--soft', '%s'); CALL DOLT_COMMIT('-Am', 'compaction: flatten history');", root))
	c.addCommits(100, historyProtectionThreshold+10)

	out := c.compact()
	if !strings.Contains(out, "set gc-compact-base="+root+": history already flattened by this compactor") {
		t.Fatalf("previously compacted database was not stamped at root:\n%s", out)
	}
	if got := c.commits(); got != 2 {
		t.Fatalf("commits = %d, want 2 (root + flatten)", got)
	}
	if rows := c.cell("SELECT COUNT(*) FROM beads"); rows != "35" {
		t.Fatalf("rows = %s, want 35", rows)
	}
}

// Operator opt-in: .compact-full-history before first sight stamps root.
func TestCompactHistoryRealDoltFullHistoryMarkerStampsRoot(t *testing.T) {
	c := newHistoryProtectionCity(t)
	seedTeamHistory(t, c.dolt, c.dbDir(), historyProtectionThreshold+10)
	if err := os.WriteFile(filepath.Join(c.dbDir(), ".compact-full-history"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c.start()
	c.compact()
	if got := c.commits(); got != 2 {
		t.Fatalf("commits = %d, want 2 (root + flatten)", got)
	}
}

// Fresh city database seen early: stamped near root, later growth flattened.
func TestCompactHistoryRealDoltFreshDatabaseStampedOnFirstSight(t *testing.T) {
	c := newHistoryProtectionCity(t)
	seedTeamHistory(t, c.dolt, c.dbDir(), 0)
	c.start()

	// A dry run reports but never stamps.
	out := c.compact("GC_DOLT_COMPACT_DRY_RUN=1")
	if !strings.Contains(out, "would set gc-compact-base=") || c.tag() != "" {
		t.Fatalf("dry run stamped the watermark (tag=%q):\n%s", c.tag(), out)
	}
	c.compact()
	if c.tag() == "" {
		t.Fatalf("first sight did not stamp the watermark")
	}
	c.addCommits(1, historyProtectionThreshold+10)
	c.compact()
	if got := c.commits(); got != 3 {
		t.Fatalf("commits = %d, want 3 (root + schema + flatten)", got)
	}
}

// Rolling back to before the watermark refuses the flatten loudly, changes
// nothing, and recovers once the operator deletes the tag.
func TestCompactHistoryRealDoltRollbackBeforeWatermarkRefuses(t *testing.T) {
	c := newHistoryProtectionCity(t)
	seedTeamHistory(t, c.dolt, c.dbDir(), 10)
	c.start()
	beforeAdoption := c.cell("SELECT commit_hash FROM dolt_log WHERE message = 'team commit 5'")
	c.compact()
	if c.tag() == "" {
		t.Fatalf("first sight did not stamp the watermark")
	}

	c.sql(fmt.Sprintf("CALL DOLT_RESET('--hard', '%s')", beforeAdoption))
	c.addCommits(1000, historyProtectionThreshold+5)
	wantCommits := c.commits()
	wantHead := c.head()

	out, err := c.compactCommand()
	if err == nil {
		t.Fatalf("compact must fail when the watermark is not an ancestor of HEAD:\n%s", out)
	}
	if !strings.Contains(out, "REFUSING flatten: gc-compact-base=") ||
		!strings.Contains(out, "CALL DOLT_TAG('-d', 'gc-compact-base')") {
		t.Fatalf("refusal lacks operator guidance:\n%s", out)
	}
	if c.commits() != wantCommits || c.head() != wantHead {
		t.Fatalf("refused run changed history: commits=%d head=%s", c.commits(), c.head())
	}

	c.sql("CALL DOLT_TAG('-d', 'gc-compact-base')")
	out = c.compact()
	if tag := c.tag(); tag != wantHead {
		t.Fatalf("re-stamped gc-compact-base = %q, want HEAD %s\n%s", tag, wantHead, out)
	}
	if c.commits() != wantCommits {
		t.Fatalf("re-stamp run flattened history: commits=%d want %d", c.commits(), wantCommits)
	}
}

// A clock-skewed adopted history (one commit dated in the future, one in the
// past) must still be stamped at the real HEAD: date order is not ancestry.
// Before the fix the future-dated commit was taken as "HEAD", the watermark
// landed on it, and the adopted commits after it were flattened on the first
// run (43 -> 9 with 40 adopted commits and threshold 20).
func TestCompactHistoryRealDoltClockSkewedAdoptedHistoryStampsRealHead(t *testing.T) {
	c := newHistoryProtectionCity(t)
	seedTeamHistoryWithDates(t, c.dolt, c.dbDir(), 40, map[int]string{
		6:  "2035-01-01T00:00:00",
		30: "1999-01-01T00:00:00",
	})
	c.start()
	realHead := c.head()

	out := c.compact()
	if tag := c.tag(); tag != realHead {
		t.Fatalf("gc-compact-base = %q, want real HEAD %s\n%s", tag, realHead, out)
	}
	if got := c.commits(); got != 42 {
		t.Fatalf("first run flattened adopted history: commits=%d\n%s", got, out)
	}

	c.addCommits(1000, historyProtectionThreshold+5)
	c.compact()
	if got := c.commits(); got != 43 {
		t.Fatalf("commits = %d, want 43 (42 adopted + 1 flatten)", got)
	}
	if n := c.cell("SELECT COUNT(*) FROM dolt_log WHERE message LIKE 'team commit %'"); n != "40" {
		t.Fatalf("adopted commits in history = %s, want 40", n)
	}
}

// A past-dated commit after a previous flatten must not be mistaken for root:
// the compactor-owned fingerprint (root's child is a flatten commit) is found
// by ancestry, so the database keeps full flattening.
func TestCompactHistoryRealDoltPastDatedCommitDoesNotHideRoot(t *testing.T) {
	c := newHistoryProtectionCity(t)
	seedTeamHistory(t, c.dolt, c.dbDir(), 0)
	c.start()
	root := c.rootCommit()
	c.addCommits(1, 5)
	c.sql(fmt.Sprintf("CALL DOLT_RESET('--soft', '%s'); CALL DOLT_COMMIT('-Am', 'compaction: flatten history');", root))
	c.sql("INSERT INTO beads VALUES (99, 'skewed'); CALL DOLT_COMMIT('-Am', 'skewed city commit', '--date', '1999-01-01T00:00:00');")
	c.addCommits(100, historyProtectionThreshold+10)

	out := c.compact()
	if !strings.Contains(out, "set gc-compact-base="+root+": history already flattened by this compactor") {
		t.Fatalf("previously compacted database was not stamped at the real root %s:\n%s", root, out)
	}
	if got := c.commits(); got != 2 {
		t.Fatalf("commits = %d, want 2 (root + flatten)", got)
	}
	if rows := c.cell("SELECT COUNT(*) FROM beads"); rows != "36" {
		t.Fatalf("rows = %s, want 36", rows)
	}
}
