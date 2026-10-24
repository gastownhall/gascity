//go:build integration

package conversationlane_test

// The bd-dolt arm in the gating run: the evidence load is its only other
// user, a manual hour-long variant, so a bd or dolt pin bump that breaks
// `bd init --server`, or a provisioning that leaks into an enclosing git
// repository, fails here first.

import (
	"database/sql"
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
// was, and that the arm's server runs with automatic GC off (R3 measures
// its data dir per round); then a city on the arm, in require, records a
// well-formed API submit and a CLI nudge, and both ops close succeeded.
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

	if got, err := doltAutoGCEnabled(c.doltServer.port); err != nil || got != 0 {
		t.Errorf("the arm's dolt sql-server @@dolt_auto_gc_enabled = %d (%v), want 0", got, err)
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
}

// doltAutoGCEnabled is @@dolt_auto_gc_enabled on the dolt sql-server
// listening on port.
func doltAutoGCEnabled(port int) (int, error) {
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port))
	if err != nil {
		return 0, fmt.Errorf("opening the dolt sql-server on port %d: %w", port, err)
	}
	defer func() { _ = db.Close() }()
	var enabled int
	if err := db.QueryRow("SELECT @@dolt_auto_gc_enabled").Scan(&enabled); err != nil {
		return 0, fmt.Errorf("reading @@dolt_auto_gc_enabled: %w", err)
	}
	return enabled, nil
}
