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
	fakePullBenignSession = "k,n,t\nconflict,1,issues\nk,n\nschema,0\nk,detail\nrow,\"hw-1: updated_at,row_lock\"\nk,n,t\nremaining,0,issues\nstatus\n0\nhash\nabc\n"
	fakePullRealSession   = "k,n,t\nconflict,2,issues\nk,n\nschema,0\nk,detail\nrow,\"hw-1: updated_at,row_lock\"\nrow,\"hw-2: status,updated_at,row_lock,closed_at\"\nk,n,t\nremaining,1,issues\nstatus\n0\nerror on line 1 for query CALL DOLT_COMMIT(...): Error 1105 (HY000): error: the table(s) issues are in conflict\n"
	fakePullSchemaSession = "k,n,t\nk,n\nschema,1\nk,detail\nk,n,t\nstatus\n0\nerror on line 1 for query CALL DOLT_COMMIT(...): Error 1105 (HY000): error: merge has unresolved conflicts or constraint violations\n"
)

// writePullConflictFakeDolt scripts a dolt whose first (autocommit) pull
// fails with the conflict error and whose resolving session answers per
// mode: "benign" (resolved, exit 0), "real" (a conflict left, exit 1),
// "nothing" (the retried pull found no conflict), "nothingadversary" (a
// conflicted table named "nothing to commit" and one named "a,b"), "notbd"
// (the issues table has no row_lock column), "plainfail" (a non-conflict
// pull error), "schemaonly" (a schema conflict and no row conflict),
// "countfail" (the conflict-count query fails), "notmerging" (the CLI pull
// failed without leaving a merge). CLI mode is scripted too: `dolt pull`
// fails with dolt's conflict text, dolt_merge_status says a merge is in
// progress, and the conflict count query reports one conflicted table.
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
		session = "printf '%s\\n' 'k,n,t' 'k,n' 'schema,0' 'k,detail' 'error on line 1 for query CALL DOLT_COMMIT(...): Error 1105 (HY000): nothing to commit'; exit 1"
	case "nothingadversary":
		// Conflicted tables literally named "nothing to commit" and "a,b"
		// beside the unresolved issues conflict: the report carries the
		// phrase and a comma, the commit was refused.
		session = "printf '%s\\n' 'k,n,t' 'conflict,1,issues' 'conflict,1,\"a,b\"' 'conflict,1,nothing to commit' 'k,n' 'schema,0' 'k,detail' 'row,\"hw-2: status,updated_at,row_lock,closed_at\"' 'k,n,t' 'remaining,1,issues' 'remaining,1,\"a,b\"' 'remaining,1,nothing to commit' 'error on line 1 for query CALL DOLT_COMMIT(...): Error 1105 (HY000): error: the table(s) issues, a,b, nothing to commit are in conflict'; exit 1"
	case "schemaonly":
		session = "printf '%s' " + shellQuote(fakePullSchemaSession) + "; exit 1"
	}
	mergeStatus := "true"
	if mode == "notmerging" {
		mergeStatus = "false"
	}
	schemaCount := "0"
	if mode == "schemaonly" {
		schemaCount = "1"
	}
	conflictCount := "printf 'COUNT(*)\\n1\\n'"
	if mode == "countfail" {
		conflictCount = "printf '%s\\n' 'error on line 1 for query SELECT COUNT(*) FROM dolt_conflicts: database is locked' >&2; exit 1"
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
  *"SELECT is_merging FROM dolt_merge_status"*)
    printf 'is_merging\n` + mergeStatus + `\n'
    ;;
  *"SELECT COUNT(*) FROM dolt_schema_conflicts"*)
    printf 'COUNT(*)\n` + schemaCount + `\n'
    ;;
  *"SELECT COUNT(*) FROM dolt_conflicts"*)
    ` + conflictCount + `
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
				"BINARY `our_id` <=> BINARY `their_id`",
				"BINARY `our_title` <=> BINARY `their_title`",
				"BINARY `our_description` <=> BINARY `their_description`",
				"BINARY `our_status` <=> BINARY `their_status`",
				"BINARY `our_metadata` <=> BINARY `their_metadata`",
				"BINARY `our_closed_at` <=> BINARY `their_closed_at`",
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
				"SELECT 'conflict' AS k, num_conflicts AS n, `table` AS t FROM dolt_conflicts; SELECT 'schema' AS k, COUNT(*) AS n FROM dolt_schema_conflicts; SELECT 'row' AS k, CONCAT(our_id, ': ', CONCAT_WS(',', IF(BINARY `our_id` <=> BINARY `their_id`, NULL, 'id'),",
				"SELECT 'remaining' AS k, num_conflicts AS n, `table` AS t FROM dolt_conflicts;",
				"CALL DOLT_ADD('-A'); CALL DOLT_COMMIT('-m', 'gc dolt pull: merge origin/main (row_lock/updated_at-only conflicts in issues resolved to the remote)', '--author', 'gc dolt pull <gc-dolt-pull@gascity.local>'); COMMIT;",
				"IF(BINARY `our_row_lock` <=> BINARY `their_row_lock`, NULL, 'row_lock')",
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
	for _, sqlMode := range []bool{true, false} {
		t.Run(fmt.Sprintf("sql=%v", sqlMode), func(t *testing.T) {
			out, log, code := runPullConflictScript(t, "notbd", sqlMode)
			if code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, out)
			}
			if resolvingSession(log) != "" {
				t.Fatalf("no resolving session may run without row_lock/updated_at columns\nlog:\n%s", log)
			}
			if !strings.Contains(out, "app: ERROR: pull failed: merge conflict, and the issues table is not a bd store (no row_lock/updated_at) — resolve manually") {
				t.Fatalf("output missing the not-a-bd-store error:\n%s", out)
			}
			// CLI mode holds the merge on disk; every refused path puts it back.
			if strings.Contains(log, "merge --abort") != !sqlMode {
				t.Fatalf("merge --abort logged = %v, want %v\nlog:\n%s", strings.Contains(log, "merge --abort"), !sqlMode, log)
			}
		})
	}
}

func TestPullNothingToCommitIsMatchedByTheErrorLineNotByResultText(t *testing.T) {
	for _, sqlMode := range []bool{true, false} {
		t.Run(fmt.Sprintf("sql=%v", sqlMode), func(t *testing.T) {
			out, log, code := runPullConflictScript(t, "nothingadversary", sqlMode)
			if code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, out)
			}
			if strings.Contains(out, "pulled from") {
				t.Fatalf("a refused merge reported as success:\n%s", out)
			}
			if !strings.Contains(out, "app: ERROR: pull failed: 3 conflict(s) in a,b issues nothing to commit need manual resolution; nothing was written") {
				t.Fatalf("output missing the manual-resolution error with every table named as it is:\n%s", out)
			}
			if !strings.Contains(out, "app: conflict hw-2: status,updated_at,row_lock,closed_at") {
				t.Fatalf("output missing the row detail:\n%s", out)
			}
			if strings.Contains(log, "merge --abort") != !sqlMode {
				t.Fatalf("merge --abort logged = %v, want %v\nlog:\n%s", strings.Contains(log, "merge --abort"), !sqlMode, log)
			}
		})
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

func TestPullCLISchemaOnlyConflictAbortsTheMerge(t *testing.T) {
	for _, sqlMode := range []bool{true, false} {
		t.Run(fmt.Sprintf("sql=%v", sqlMode), func(t *testing.T) {
			out, log, code := runPullConflictScript(t, "schemaonly", sqlMode)
			if code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, out)
			}
			if strings.Contains(out, "pulled from") {
				t.Fatalf("a schema conflict reported as success:\n%s", out)
			}
			want := "app: ERROR: pull failed: 1 schema conflict(s) need manual resolution; the merge was aborted, nothing was written"
			if sqlMode {
				want = "app: ERROR: pull failed: 1 schema conflict(s) and 0 row conflict(s) in no table need manual resolution; nothing was written"
			}
			if !strings.Contains(out, want) {
				t.Fatalf("output missing %q:\n%s", want, out)
			}
			if strings.Contains(log, "merge --abort") != !sqlMode {
				t.Fatalf("merge --abort logged = %v, want %v\nlog:\n%s", strings.Contains(log, "merge --abort"), !sqlMode, log)
			}
			if !sqlMode && resolvingSession(log) != "" {
				t.Fatalf("CLI mode must not open a resolving session for a schema-only conflict\nlog:\n%s", log)
			}
		})
	}
}

func TestPullCLIAbortsWhenTheConflictsCannotBeRead(t *testing.T) {
	out, log, code := runPullConflictScript(t, "countfail", false)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "app: ERROR: pull failed, and the conflicts could not be read (error on line 1 for query SELECT COUNT(*) FROM dolt_conflicts: database is locked); the merge was aborted") {
		t.Fatalf("output missing the read-failure error:\n%s", out)
	}
	if !strings.Contains(log, "merge --abort") || resolvingSession(log) != "" {
		t.Fatalf("a failed read must abort and never resolve\nlog:\n%s", log)
	}
}

func TestPullCLIWithoutAMergeInProgressIsAPlainFailure(t *testing.T) {
	out, log, code := runPullConflictScript(t, "notmerging", false)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "app: ERROR: pull failed\n") {
		t.Fatalf("output missing the plain failure line:\n%s", out)
	}
	if strings.Contains(log, "merge --abort") || resolvingSession(log) != "" {
		t.Fatalf("no merge in progress: nothing to abort or resolve\nlog:\n%s", log)
	}
}
