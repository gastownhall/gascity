//go:build unix

package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compactDoltCall is one fake-dolt invocation from the compact fixture log.
type compactDoltCall struct {
	db    string
	query string
}

// readCompactDoltCalls parses the fake dolt log into its invocations. A
// multi-line query (the flatten) continues until the next "db=" entry.
func readCompactDoltCalls(t *testing.T, logPath string) []compactDoltCall {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	var calls []compactDoltCall
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "db="); ok {
			if db, query, found := strings.Cut(rest, " query="); found {
				calls = append(calls, compactDoltCall{db: db, query: query})
				continue
			}
		}
		if len(calls) > 0 {
			calls[len(calls)-1].query += "\n" + line
		}
	}
	return calls
}

// isCompactVerifyRead reports whether query is one of the four flatten
// verification probes: row count, table value hash, user-table membership and
// database value hash.
func isCompactVerifyRead(query string) bool {
	return strings.HasPrefix(query, "SELECT COUNT(*) FROM `") ||
		strings.HasPrefix(query, "SELECT DOLT_HASHOF_TABLE(") ||
		strings.Contains(query, "information_schema.tables") ||
		strings.HasPrefix(query, "SELECT DOLT_HASHOF_DB(")
}

func isCompactFlattenCall(query string) bool {
	return strings.Contains(query, "CALL DOLT_RESET('--soft'")
}

// A writer that commits right after the flatten's DOLT_COMMIT (a same-count
// UPDATE visible only in the live database) neither quarantines nor rewrites
// the flatten identity: verification reads the flatten commit, the run records
// that commit, and full GC is deferred to the next quiet run (#8502).
func TestCompactScriptLaterWriterAfterFlattenDefersWithoutQuarantine(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "writer_after_flatten", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("later writer after the flatten must exit 0: %v\n%s", err, out)
	}
	for _, unwanted := range []string{"value hash changed", "INTEGRITY check failed", "quarantine and investigate"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("later writer's live change was verified as flatten damage (%q):\n%s", unwanted, out)
		}
	}
	for _, want := range []string{
		"flatten_HEAD=" + compactFakeFlattenCommit,
		"final_verify_HEAD=writercommit",
		"deferring full GC until the next quiet run",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	quarantine := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, statErr := os.Stat(quarantine); !os.IsNotExist(statErr) {
		t.Fatalf("later writer must not quarantine; stat=%v", statErr)
	}
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if reason := compactMarkerValue(t, pendingGC, "reason"); reason != "writer race during flatten deferred full GC" {
		t.Fatalf("pending-GC reason = %q, want the writer-race defer", reason)
	}
	for _, call := range readCompactDoltCalls(t, fixture.doltLog) {
		if strings.Contains(call.query, "DOLT_GC") {
			t.Fatalf("full GC ran while a later writer was active: %+v", call)
		}
	}
}

// The flatten commit comes only from DOLT_COMMIT's own result row. Output that
// does not carry exactly one valid commit hash quarantines the database before
// any verification read or GC, and the live HEAD is never substituted.
func TestCompactScriptQuarantinesWhenFlattenCommitHashUnavailable(t *testing.T) {
	const status = "+--------+\n| status |\n+--------+\n| 0      |\n+--------+\n\n"
	const border = "+----------------------------------+\n"
	hashSet := func(rows ...string) string {
		set := border + "| hash                             |\n" + border
		for _, row := range rows {
			set += "| " + row + " |\n"
		}
		return set + border
	}
	valid := compactFakeFlattenCommit
	cases := map[string]string{
		"empty output":         "",
		"no hash result set":   status,
		"empty hash value":     status + hashSet(""),
		"31 characters":        status + hashSet(valid[:31]),
		"33 characters":        status + hashSet(valid+"0"),
		"uppercase":            status + hashSet(strings.ToUpper(valid)),
		"letter beyond v":      status + hashSet(valid[:31]+"w"),
		"embedded whitespace":  status + hashSet(valid[:16]+" "+valid[17:]),
		"ref text":             status + hashSet("refs/heads/"+valid[:21]),
		"sql text":             status + hashSet(valid+";"),
		"two rows":             status + hashSet(valid, valid),
		"duplicate hash sets":  status + hashSet(valid) + "\n" + hashSet(valid),
		"non-hash column":      status + "+--------+\n| commit |\n+--------+\n| " + valid + " |\n+--------+\n",
		"two columns":          status + "+------+------+\n| hash | hash |\n+------+------+\n| " + valid + " | " + valid + " |\n+------+------+\n",
		"unframed hash line":   status + valid + "\n",
		"truncated hash table": status + border + "| hash                             |\n" + border + "| " + valid + " |\n",
	}
	for name, stdout := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			stdoutFile := filepath.Join(t.TempDir(), "flatten-stdout")
			if err := os.WriteFile(stdoutFile, []byte(stdout), 0o644); err != nil {
				t.Fatalf("write flatten stdout: %v", err)
			}
			out, err := fixture.run(t, "success",
				"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
				"GC_FAKE_DOLT_FLATTEN_STDOUT_FILE="+stdoutFile,
			)
			if err == nil {
				t.Fatalf("compact succeeded without a valid flatten commit hash:\n%s", out)
			}
			if !strings.Contains(out, "DOLT_COMMIT result carried no single valid commit hash") {
				t.Fatalf("output missing the unavailable-hash failure:\n%s", out)
			}
			marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
			if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten commit hash unavailable" {
				t.Fatalf("quarantine reason = %q, want post-flatten commit hash unavailable", reason)
			}
			if got := compactMarkerValue(t, marker, "flatten_head"); got != "" {
				t.Fatalf("quarantine marker recorded flatten_head=%q; the live HEAD must not stand in for the flatten commit", got)
			}
			flattens := 0
			for _, call := range readCompactDoltCalls(t, fixture.doltLog) {
				if isCompactFlattenCall(call.query) {
					flattens++
					continue
				}
				if strings.Contains(call.query, "DOLT_RESET") {
					t.Fatalf("the quarantine path reset HEAD again, which could roll back a later writer: %+v", call)
				}
				if strings.Contains(call.query, "DOLT_GC") {
					t.Fatalf("GC ran without a flatten commit hash: %+v", call)
				}
				if flattens > 0 && (isCompactVerifyRead(call.query) || strings.Contains(call.db, "/")) {
					t.Fatalf("verification read ran without a flatten commit hash: %+v", call)
				}
			}
			if flattens != 1 {
				t.Fatalf("fixture flattened %d times, want exactly one flatten call:\n%s", flattens, out)
			}
			// The flatten call succeeded, so its commit stays HEAD: no restore
			// may roll back over a writer that committed after it.
			if state, readErr := os.ReadFile(fixture.stateFile); readErr != nil || strings.TrimSpace(string(state)) != compactFakeFlattenCommit {
				t.Fatalf("flatten commit must remain HEAD after the quarantine, state=%q err=%v", state, readErr)
			}
		})
	}
}

// The stdout/stderr captures around the flatten call are temporary files and
// must not outlive the run, whether it verified, could not parse the flatten
// commit hash or failed the flatten call.
func TestCompactScriptCleansFlattenOutputCaptureFiles(t *testing.T) {
	unparseable := filepath.Join(t.TempDir(), "flatten-stdout")
	if err := os.WriteFile(unparseable, []byte("+--------+\n| status |\n+--------+\n| 0      |\n+--------+\n"), 0o644); err != nil {
		t.Fatalf("write flatten stdout: %v", err)
	}
	cases := []struct {
		name, mode string
		env        []string
		wantErr    bool
	}{
		{name: "verified", mode: "success"},
		{name: "commit hash unavailable", mode: "success", env: []string{"GC_FAKE_DOLT_FLATTEN_STDOUT_FILE=" + unparseable}, wantErr: true},
		{name: "flatten call failed", mode: "flatten_failure", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			captureDir := t.TempDir()
			env := append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "TMPDIR=" + captureDir}, tc.env...)
			out, err := fixture.run(t, tc.mode, env...)
			if (err != nil) != tc.wantErr {
				t.Fatalf("compact err=%v, want error=%v:\n%s", err, tc.wantErr, out)
			}
			entries, readErr := os.ReadDir(captureDir)
			if readErr != nil {
				t.Fatalf("read capture dir: %v", readErr)
			}
			if len(entries) != 0 {
				var names []string
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				t.Fatalf("compact left temporary capture files behind: %v\n%s", names, out)
			}
		})
	}
}

// assertCompactVerifiedAtRevision fails unless the database value hash was
// read from revisionDB.
func assertCompactVerifiedAtRevision(t *testing.T, fixture compactScriptFixture, revisionDB string) {
	t.Helper()
	for _, call := range readCompactDoltCalls(t, fixture.doltLog) {
		if call.db == revisionDB && strings.HasPrefix(call.query, "SELECT DOLT_HASHOF_DB(") {
			return
		}
	}
	t.Fatalf("no verification read used revision database %s", revisionDB)
}

// Any valid Dolt commit hash (digits and a-v, 32 characters) is accepted as the
// flatten commit, with or without extra cell padding.
func TestCompactScriptAcceptsValidFlattenCommitHash(t *testing.T) {
	t.Run("full alphabet", func(t *testing.T) {
		const commit = "0123456789abcdefghijklmnopqrstuv"
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "success",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_FAKE_DOLT_FLATTEN_COMMIT="+commit,
		)
		if err != nil {
			t.Fatalf("compact failed for a valid flatten commit hash: %v\n%s", err, out)
		}
		assertCompactVerifiedAtRevision(t, fixture, "beads/"+commit)
	})
	t.Run("padded cell", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		stdoutFile := filepath.Join(t.TempDir(), "flatten-stdout")
		stdout := "+--------+\n| status |\n+--------+\n| 0      |\n+--------+\n\n" +
			"+--------------------------------------+\n" +
			"|   hash                               |\n" +
			"+--------------------------------------+\n" +
			"|   " + compactFakeFlattenCommit + "   |\n" +
			"+--------------------------------------+\n"
		if err := os.WriteFile(stdoutFile, []byte(stdout), 0o644); err != nil {
			t.Fatalf("write flatten stdout: %v", err)
		}
		out, err := fixture.run(t, "success",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_FAKE_DOLT_FLATTEN_STDOUT_FILE="+stdoutFile,
		)
		if err != nil {
			t.Fatalf("compact failed for a padded flatten commit hash: %v\n%s", err, out)
		}
		assertCompactVerifiedAtRevision(t, fixture, "beads/"+compactFakeFlattenCommit)
	})
}

// The four verification probes read the revision database pinned to the
// flatten commit; preflight, HEAD, marker and GC operations keep the base
// database — for every database in the run.
func TestCompactScriptPinsVerificationReadsToFlattenCommit(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, "zed", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir second dolt db: %v", err)
	}
	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	pinned := map[string]map[string]bool{}
	flattened := map[string]bool{}
	gcRan := map[string]bool{}
	for _, call := range readCompactDoltCalls(t, fixture.doltLog) {
		base, rev, isRevision := strings.Cut(call.db, "/")
		if isRevision && rev != compactFakeFlattenCommit {
			t.Fatalf("read pinned to %q, want the flatten commit %s: %+v", rev, compactFakeFlattenCommit, call)
		}
		switch {
		case isCompactFlattenCall(call.query):
			if isRevision {
				t.Fatalf("flatten ran against a revision database: %+v", call)
			}
			flattened[base] = true
		case isCompactVerifyRead(call.query) && flattened[base] && !gcRan[base]:
			if !isRevision {
				t.Fatalf("verification read on %s used the live database, not the flatten commit: %+v", base, call)
			}
			if pinned[base] == nil {
				pinned[base] = map[string]bool{}
			}
			switch {
			case strings.HasPrefix(call.query, "SELECT COUNT(*) FROM `"):
				pinned[base]["row count"] = true
			case strings.HasPrefix(call.query, "SELECT DOLT_HASHOF_TABLE("):
				pinned[base]["table value hash"] = true
			case strings.Contains(call.query, "information_schema.tables"):
				if !strings.Contains(call.query, "table_schema = DATABASE()") {
					t.Fatalf("membership probe must scope to the selected revision database: %+v", call)
				}
				pinned[base]["membership"] = true
			default:
				pinned[base]["database value hash"] = true
			}
		default:
			if isRevision {
				t.Fatalf("only verification reads may use the revision database: %+v", call)
			}
			if strings.Contains(call.query, "DOLT_GC") {
				gcRan[base] = true
			}
		}
	}
	for _, db := range []string{"beads", "zed"} {
		if !flattened[db] || !gcRan[db] {
			t.Fatalf("db %s: flattened=%v gc=%v, want both:\n%s", db, flattened[db], gcRan[db], out)
		}
		for _, probe := range []string{"row count", "table value hash", "membership", "database value hash"} {
			if !pinned[db][probe] {
				t.Fatalf("db %s: %s probe never read the flatten commit; pinned=%v", db, probe, pinned[db])
			}
		}
	}
}
