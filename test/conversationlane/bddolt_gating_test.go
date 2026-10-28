//go:build integration

package conversationlane_test

// The bd-dolt arm in the gating run: the evidence load is its only other
// user, a manual hour-long variant, so a bd or dolt pin bump that breaks
// `bd init --server`, or a provisioning that leaks into an enclosing git
// repository, fails here first.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
	_ "github.com/go-sql-driver/mysql"
)

// TestBdDoltArmProvisionsAndRecords provisions the bd-dolt arm with the
// city's temp root inside a git repository that holds a staged change, and
// checks that the repository gained no commit and kept its index as it
// was, and that the arm's server runs with automatic GC and the stats
// workers off (R3 measures its data dir per round); then a city on the arm,
// in require, records a well-formed API submit and a CLI nudge, both ops
// close succeeded, and both rows sit in the no-history plane: bd's wisps
// table, which the workspace's dolt_ignore keeps out of every Dolt commit,
// before and after the garbage collection R3 runs at each round's end.
func TestBdDoltArmProvisionsAndRecords(t *testing.T) {
	outer := t.TempDir()
	gitIn := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = outer
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=gc-test", "GIT_AUTHOR_EMAIL=gc-test@test.local", "GIT_COMMITTER_NAME=gc-test", "GIT_COMMITTER_EMAIL=gc-test@test.local")
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := gitIn("init", "--quiet"); err != nil {
		t.Fatalf("git init %s: %v: %s", outer, err, out)
	}
	if err := os.WriteFile(filepath.Join(outer, "user-staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := gitIn("add", "user-staged.txt"); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	t.Setenv("TMPDIR", outer)

	c := newX12City(t, armBdDolt, "require", 0, false)
	if head, err := gitIn("rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		log, _ := gitIn("log", "--stat", "-1")
		t.Errorf("provisioning the arm committed %s into the enclosing repository:\n%s", head, log)
	}
	if staged, err := gitIn("diff", "--cached", "--name-only"); err != nil || staged != "user-staged.txt" {
		t.Errorf("the enclosing repository's index = %q (%v), want only the staged user-staged.txt", staged, err)
	}

	off := doltSwitches{autoGC: 0, stats: 0, statsGC: 0, statsMemoryOnly: 1, statsPaused: 1}
	if got, err := readDoltSwitches(c.doltServer.port); err != nil || got != off {
		t.Errorf("the arm's dolt sql-server switches = %+v (%v), want %+v: automatic GC and the stats workers off", got, err, off)
	}

	c.start("")
	w := c.worker()
	submit := c.send(verbByName("submit"), w, "x12-bddolt-submit", "x12 bd dolt submit alpha", nil)
	if !submit.ok() || submit.opID == "" {
		t.Fatalf("require answered the submit %s with op %q, want a recorded 202", submit, submit.opID)
	}
	if out := c.pollOutcome(submit.requestID, submit.cursor); !strings.HasPrefix(out.class, classSucceeded) {
		t.Errorf("the submit's outcome %+v, want a success class", out)
	}
	nudge := c.send(verbByName("cli-nudge-wait"), w, "x12-bddolt-nudge", "x12 bd dolt nudge alpha", nil)
	if !nudge.ok() {
		t.Fatalf("require answered the CLI nudge %s, want exit 0", nudge)
	}
	ids := []string{submit.opID, operatorOpID(t, w.id, "x12-bddolt-nudge")}
	for _, op := range c.waitClosed(x12OutcomeTimeout, ids...) {
		if arm, reason := op.Outcome.Effective(); arm != opjournal.ArmSucceeded {
			t.Errorf("op %s closed %s{%s}, want succeeded", op.ID, arm, reason)
		}
	}
	planes, err := readRowPlanes(c.doltServer.port, ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if got := planes[id]; got != rowPlaneNoHistory {
			t.Errorf("op %s is in %q, want %q: an op row is no-history, so recording it mints no Dolt commit", id, got, rowPlaneNoHistory)
		}
	}

	// R3's collection at each round's end on this arm keeps the op rows.
	if size := c.collectGarbage(); size <= 0 {
		t.Errorf("the binding after a garbage collection is %d bytes, want its data", size)
	}
	if planes, err = readRowPlanes(c.doltServer.port, ids); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if got := planes[id]; got != rowPlaneNoHistory {
			t.Errorf("after a garbage collection op %s is in %q, want %q", id, got, rowPlaneNoHistory)
		}
	}
}

// rowPlaneNoHistory is readRowPlanes' answer for a row in bd's wisps table
// with its no_history flag set, the table being dolt_ignored.
const rowPlaneNoHistory = "wisps (no_history, dolt_ignored)"

// readRowPlanes reads which table of the bd-dolt arm's workspace each of ids
// is in: rowPlaneNoHistory, "wisps" for a wisps row without the flag or with
// the table not ignored, "issues" for the versioned table, absent for none.
func readRowPlanes(port int, ids []string) (map[string]string, error) {
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s", port, laneIDPrefix))
	if err != nil {
		return nil, fmt.Errorf("opening the dolt sql-server on port %d: %w", port, err)
	}
	defer func() { _ = db.Close() }()
	var ignored int
	if err := db.QueryRow("SELECT COUNT(*) FROM dolt_ignore WHERE pattern = 'wisps' AND ignored").Scan(&ignored); err != nil {
		return nil, fmt.Errorf("reading the workspace's dolt_ignore: %w", err)
	}
	planes := map[string]string{}
	for _, id := range ids {
		var noHistory int
		switch err := db.QueryRow("SELECT no_history FROM wisps WHERE id = ?", id).Scan(&noHistory); {
		case err == nil && noHistory == 1 && ignored > 0:
			planes[id] = rowPlaneNoHistory
			continue
		case err == nil:
			planes[id] = "wisps"
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("reading %s from wisps: %w", id, err)
		}
		var one int
		switch err := db.QueryRow("SELECT 1 FROM issues WHERE id = ?", id).Scan(&one); {
		case err == nil:
			planes[id] = "issues"
		case !errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("reading %s from issues: %w", id, err)
		}
	}
	return planes, nil
}

// doltSwitches are a dolt sql-server's automatic-GC and stats switches as
// its system variables read (1 on, 0 off): @@dolt_auto_gc_enabled,
// @@dolt_stats_enabled, @@dolt_stats_gc_enabled, @@dolt_stats_memory_only
// and @@dolt_stats_paused.
type doltSwitches struct {
	autoGC, stats, statsGC, statsMemoryOnly, statsPaused int
}

// readDoltSwitches reads the switches of the dolt sql-server listening on
// port.
func readDoltSwitches(port int) (doltSwitches, error) {
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port))
	if err != nil {
		return doltSwitches{}, fmt.Errorf("opening the dolt sql-server on port %d: %w", port, err)
	}
	defer func() { _ = db.Close() }()
	var s doltSwitches
	row := db.QueryRow("SELECT @@dolt_auto_gc_enabled, @@dolt_stats_enabled, @@dolt_stats_gc_enabled, @@dolt_stats_memory_only, @@dolt_stats_paused")
	if err := row.Scan(&s.autoGC, &s.stats, &s.statsGC, &s.statsMemoryOnly, &s.statsPaused); err != nil {
		return doltSwitches{}, fmt.Errorf("reading the automatic-GC and stats switches: %w", err)
	}
	return s, nil
}
