package dolt_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The pull's conflict branch (gp-c04p): a conflicted bd `issues` row that
// differs from the remote only in row_lock and updated_at is resolved to the
// remote inside one transaction; anything else fails closed. These tests
// drive the script against a fake dolt that scripts each answer; the real
// two-clone behavior is pinned in pull_conflicts_real_dolt_test.go.

const (
	fakePullConflictError = "error on line 1 for query CALL DOLT_PULL('origin', 'main'): Error 1105 (HY000): Merge conflict detected, @autocommit transaction rolled back. @autocommit must be disabled so that merge conflicts can be resolved using the dolt_conflicts and dolt_schema_conflicts tables before manually committing the transaction."
	fakePullColumnsCSV    = "COLUMN_NAME\nid\ntitle\ndescription\nstatus\nmetadata\nupdated_at\nrow_lock\nclosed_at\n"
	fakePullBenignSession = "k,t,n\nconflict,issues,1\nk,id,differing\nrow,hw-1,\"updated_at,row_lock\"\nk,t,n\nremaining,issues,0\nstatus\n0\nhash\nabc\n"
	fakePullRealSession   = "k,t,n\nconflict,issues,2\nk,id,differing\nrow,hw-1,\"updated_at,row_lock\"\nrow,hw-2,\"status,updated_at,row_lock,closed_at\"\nk,t,n\nremaining,issues,1\nstatus\n0\nerror on line 1 for query CALL DOLT_COMMIT(...): Error 1105 (HY000): error: the table(s) issues are in conflict\n"
)

// writePullConflictFakeDolt scripts a dolt whose first (autocommit) pull
// fails with the conflict error and whose resolving session answers per
// mode: "benign" (resolved, exit 0), "real" (a conflict left, exit 1),
// "nothing" (the retried pull found no conflict), "notbd" (the issues
// table has no row_lock column), "plainfail" (a non-conflict pull error).
// CLI mode is scripted too: `dolt pull` fails with dolt's conflict text and
// the conflict count query reports one conflicted table.
func writePullConflictFakeDolt(t *testing.T, dir, mode string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	columns := fakePullColumnsCSV
	if mode == "notbd" {
		columns = "COLUMN_NAME\nid\ntitle\nstatus\nupdated_at\n"
	}
	session := "printf '%s' " + shellQuote(fakePullBenignSession) + "; exit 0"
	switch mode {
	case "real":
		session = "printf '%s' " + shellQuote(fakePullRealSession) + "; exit 1"
	case "nothing":
		session = "printf '%s\\n' 'k,t,n' 'error on line 1 for query CALL DOLT_COMMIT(...): Error 1105 (HY000): nothing to commit'; exit 1"
	}
	pullFailure := "printf '%s\\n' " + shellQuote(fakePullConflictError) + " >&2; exit 1"
	if mode == "plainfail" {
		pullFailure = "printf '%s\\n' 'error on line 1 for query CALL DOLT_PULL: dial tcp 127.0.0.1:1: connect: connection refused' >&2; exit 1"
	}
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes"*)
    printf 'name,url\norigin,file:///hub\n'
    ;;
  *"SET @@autocommit = 0;"*)
    ` + session + `
    ;;
  *"CALL DOLT_PULL("*)
    ` + pullFailure + `
    ;;
  *"information_schema.columns"*)
    printf '%s' ` + shellQuote(columns) + `
    ;;
  *"SELECT COUNT(*) FROM dolt_conflicts"*)
    printf 'COUNT(*)\n1\n'
    ;;
  "pull origin main")
    printf '%s\n' 'Auto-merging issues' 'CONFLICT (content): Merge conflict in issues' 'Automatic merge failed; 1 table(s) are unmerged.'
    exit 1
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return logPath
}

// runPullConflictScript runs the pull for database "app" against the fake
// dolt, in SQL mode (a reachable server port) or CLI mode (no server; the
// database directory carries a remotes.json).
func runPullConflictScript(t *testing.T, mode string, sqlMode bool) (string, string, int) {
	t.Helper()
	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port := 1
	if sqlMode {
		p, cleanup := startReachableTCPListener(t)
		defer cleanup()
		port = p
	}
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	dbDir := filepath.Join(dataDir, "app", ".dolt")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, "remotes.json"), []byte(`{"name":"origin","url":"file:///hub"}`), 0o644); err != nil {
		t.Fatalf("write remotes.json: %v", err)
	}
	binDir := t.TempDir()
	doltLog := writePullConflictFakeDolt(t, binDir, mode)
	writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		ok := errors.As(err, &exitErr)
		if !ok {
			t.Fatalf("run pull: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	log, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	return string(out), string(log), code
}

// resolvingSession returns the one dolt invocation that carried the
// resolving transaction, or "" when none was issued.
func resolvingSession(log string) string {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "SET @@autocommit = 0;") {
			return line
		}
	}
	return ""
}

func TestPullResolvesRowLockOnlyConflictInsideOneTransaction(t *testing.T) {
	for _, sqlMode := range []bool{true, false} {
		t.Run(fmt.Sprintf("sql=%v", sqlMode), func(t *testing.T) {
			out, log, code := runPullConflictScript(t, "benign", sqlMode)
			if code != 0 {
				t.Fatalf("exit = %d, want 0\n%s\nlog:\n%s", code, out, log)
			}
			if !strings.Contains(out, "app: pulled from file:///hub (resolved 1 row_lock/updated_at-only conflict(s) in issues: hw-1)") {
				t.Fatalf("output missing the resolved pull line:\n%s", out)
			}
			session := resolvingSession(log)
			if session == "" {
				t.Fatalf("no resolving session issued\nlog:\n%s", log)
			}
			if strings.Contains(session, "CALL DOLT_PULL('origin', 'main')") != sqlMode {
				t.Fatalf("SQL mode retries the pull inside the transaction, CLI mode holds the merge already; sqlMode=%v session:\n%s", sqlMode, session)
			}
			// The predicate is what makes the class narrow: every column but
			// row_lock and updated_at must be equal, both sides modified, and
			// the merged schema must still be the one the predicate was built
			// from.
			deleteIdx := strings.Index(session, "DELETE FROM dolt_conflicts_issues WHERE ")
			if deleteIdx < 0 {
				t.Fatalf("session has no conflict-marker delete:\n%s", session)
			}
			predicate, _, _ := strings.Cut(session[deleteIdx:], ";")
			for _, want := range []string{
				"our_diff_type = 'modified' AND their_diff_type = 'modified'",
				"`our_id` <=> `their_id`",
				"`our_title` <=> `their_title`",
				"`our_description` <=> `their_description`",
				"`our_status` <=> `their_status`",
				"`our_metadata` <=> `their_metadata`",
				"`our_closed_at` <=> `their_closed_at`",
				"table_name = 'issues') = 'id,title,description,status,metadata,updated_at,row_lock,closed_at'",
			} {
				if !strings.Contains(predicate, want) {
					t.Errorf("predicate missing %q:\n%s", want, predicate)
				}
			}
			for _, reject := range []string{"`our_row_lock`", "`our_updated_at`"} {
				if strings.Contains(predicate, reject) {
					t.Errorf("predicate must not compare %s:\n%s", reject, predicate)
				}
			}
			for _, want := range []string{
				"UPDATE issues SET row_lock = (SELECT c.their_row_lock FROM dolt_conflicts_issues c WHERE c.our_id = issues.id AND ",
				"updated_at = (SELECT c.their_updated_at FROM dolt_conflicts_issues c WHERE c.our_id = issues.id AND ",
				"SELECT 'remaining' AS k, `table` AS t, num_conflicts AS n FROM dolt_conflicts;",
				"CALL DOLT_ADD('-A'); CALL DOLT_COMMIT('-m', 'gc dolt pull: merge origin/main (row_lock/updated_at-only conflicts in issues resolved to the remote)', '--author', 'gc dolt pull <gc-dolt-pull@gascity.local>'); COMMIT;",
				"IF(`our_row_lock` <=> `their_row_lock`, NULL, 'row_lock')",
			} {
				if !strings.Contains(session, want) {
					t.Errorf("session missing %q:\n%s", want, session)
				}
			}
			if strings.Contains(log, "merge --abort") {
				t.Fatalf("a resolved pull must not abort the merge\nlog:\n%s", log)
			}
		})
	}
}

func TestPullFailsClosedWhenAnyOtherColumnConflicts(t *testing.T) {
	for _, sqlMode := range []bool{true, false} {
		t.Run(fmt.Sprintf("sql=%v", sqlMode), func(t *testing.T) {
			out, log, code := runPullConflictScript(t, "real", sqlMode)
			if code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, out)
			}
			if strings.Contains(out, "pulled from") {
				t.Fatalf("a failed pull must not report success:\n%s", out)
			}
			for _, want := range []string{
				"app: conflict hw-1: updated_at,row_lock",
				"app: conflict hw-2: status,updated_at,row_lock,closed_at",
				"app: ERROR: pull failed: 1 conflict(s) in issues need manual resolution; nothing was written",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			if resolvingSession(log) == "" {
				t.Fatalf("no resolving session issued\nlog:\n%s", log)
			}
			// CLI mode holds the merge on disk; a refused pull puts the
			// working set back. SQL mode never wrote anything to abort.
			if strings.Contains(log, "merge --abort") != !sqlMode {
				t.Fatalf("merge --abort logged = %v, want %v (CLI mode only)\nlog:\n%s", strings.Contains(log, "merge --abort"), !sqlMode, log)
			}
		})
	}
}

func TestPullReportsRetriedPullWithNoConflictAsPlainPull(t *testing.T) {
	out, _, code := runPullConflictScript(t, "nothing", true)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "app: pulled from file:///hub\n") || strings.Contains(out, "resolved") {
		t.Fatalf("output should be the plain pull line:\n%s", out)
	}
}

func TestPullNeverAutoResolvesANonBdIssuesTable(t *testing.T) {
	out, log, code := runPullConflictScript(t, "notbd", true)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if resolvingSession(log) != "" {
		t.Fatalf("no resolving session may run without row_lock/updated_at columns\nlog:\n%s", log)
	}
	if !strings.Contains(out, "app: ERROR: pull failed: merge conflict, and the issues table is not a bd store (no row_lock/updated_at) — resolve manually") {
		t.Fatalf("output missing the not-a-bd-store error:\n%s", out)
	}
}

func TestPullNonConflictFailureIsUnchanged(t *testing.T) {
	out, log, code := runPullConflictScript(t, "plainfail", true)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "app: ERROR: pull failed\n") {
		t.Fatalf("output missing the plain failure line:\n%s", out)
	}
	if resolvingSession(log) != "" || strings.Contains(log, "information_schema") {
		t.Fatalf("a non-conflict failure must not start resolution\nlog:\n%s", log)
	}
}
