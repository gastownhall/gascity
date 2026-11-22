package proxyendpoint

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// TestProbeSessionReadsTheIgnoredLanesCursorReality is council A-F2's pin.
//
// Before it, one probe session read exactly four statements — an existence
// probe and a MAX(version) for each cursor table — and reported the raw ignored
// number as if it were the number the linked library acts on. It is not.
// beads' migrationSource.currentVersion clamps a cursor the live schema
// contradicts down to a reality floor, and beads documents both contradicted
// shapes as ones that occur in the field. A database at raw ignored=26 with
// `leases.granted_node` absent is a database the library believes is at 11, so
// MigrateUp replays ignored 0012-0025 against it — and gc's proxied open is
// writable, because the library exports no read-only open to an embedder at
// v1.3.0.
//
// The session therefore reads the sentinels too, and the assertion is on the
// STATEMENTS as much as on the answer: a reader that concluded the right thing
// from evidence it never gathered would pass an outcome-only test.
func TestProbeSessionReadsTheIgnoredLanesCursorReality(t *testing.T) {
	const rawIgnored = 26

	cases := []struct {
		name           string
		absentTables   map[string]bool
		absentColumns  map[string]bool
		likeDecoys     map[string][]string
		recasedColumns map[string]string
		likeFoldsCase  bool
		ignored        int
		wantLimited    bool
		wantFloor      int
		wantMissing    string
		wantEffective  int
	}{
		{
			name:          "a corroborated cursor is believed as read",
			ignored:       rawIgnored,
			wantEffective: rawIgnored,
		},
		{
			name:          "a missing sentinel column floors the lane at the replay floor",
			absentColumns: map[string]bool{"leases.granted_node": true},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     11,
			wantMissing:   "leases.granted_node",
			wantEffective: 11,
		},
		{
			name:          "a missing sentinel table floors the lane at zero",
			absentTables:  map[string]bool{"wisp_dependencies": true},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     0,
			wantMissing:   "wisp_dependencies",
			wantEffective: 0,
		},
		{
			// The library probes the tables first and short-circuits on them,
			// because they floor at zero and no column probe could lower that.
			name:          "a sentinel table outranks a sentinel column",
			absentTables:  map[string]bool{"wisps": true},
			absentColumns: map[string]bool{"leases.granted_node": true},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     0,
			wantMissing:   "wisps",
			wantEffective: 0,
		},
		{
			// beads v1.3.1 sentinelFlooredTables: events is created by
			// ignored/0019, so its absence leaves 18 believable.
			name:          "a missing floored sentinel table floors the lane at its own replay floor",
			absentTables:  map[string]bool{"events": true},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     18,
			wantMissing:   "events",
			wantEffective: 18,
		},
		{
			name:          "the lowest floor of the absent floored tables wins",
			absentTables:  map[string]bool{"bd_events_seq": true, "events": true},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     18,
			wantMissing:   "events",
			wantEffective: 18,
		},
		{
			// Equal floors keep the first absent table the library probed.
			name:          "an equal floor keeps the first absent floored table",
			absentTables:  map[string]bool{"bd_events_journal": true, "bd_events_seq": true},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     21,
			wantMissing:   "bd_events_journal",
			wantEffective: 21,
		},
		{
			// Floored tables do not short-circuit: the column is still read,
			// and its lower floor wins.
			name:          "a missing sentinel column lowers a floored table's floor",
			absentTables:  map[string]bool{"bd_events_journal": true},
			absentColumns: map[string]bool{"leases.granted_node": true},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     11,
			wantMissing:   "leases.granted_node",
			wantEffective: 11,
		},
		{
			// The column's own table absent reads as the column absent, without
			// a SHOW COLUMNS: that statement fails on an absent table, and a
			// failing statement pins the session to its pre-statement catalog.
			name:          "a missing sentinel column table floors the lane at the column's replay floor",
			absentTables:  map[string]bool{"leases": true},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     11,
			wantMissing:   "leases.granted_node",
			wantEffective: 11,
		},
		{
			// SHOW COLUMNS ... LIKE 'granted_node' also lists a column the '_'
			// wildcard matches; only the exact name corroborates the cursor.
			name:          "a column the LIKE wildcard matches is not the sentinel column",
			absentColumns: map[string]bool{"leases.granted_node": true},
			likeDecoys:    map[string][]string{"leases.granted_node": {"grantedXnode"}},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     11,
			wantMissing:   "leases.granted_node",
			wantEffective: 11,
		},
		{
			// Dolt's LIKE does not list a column re-cased by hand, so it reads
			// as absent: the case-sensitive match the library's own SHOW COLUMNS
			// reads make (beads#7592).
			name:           "a re-cased sentinel column is not the sentinel column",
			recasedColumns: map[string]string{"leases.granted_node": "GRANTED_NODE"},
			ignored:        rawIgnored,
			wantLimited:    true,
			wantFloor:      11,
			wantMissing:    "leases.granted_node",
			wantEffective:  11,
		},
		{
			// The Field comparison decides, not the server's LIKE collation: a
			// LIKE that folds case lists the re-cased column, which still
			// reads as absent.
			name:           "a re-cased sentinel column is absent even where LIKE folds case",
			recasedColumns: map[string]string{"leases.granted_node": "GRANTED_NODE"},
			likeFoldsCase:  true,
			ignored:        rawIgnored,
			wantLimited:    true,
			wantFloor:      11,
			wantMissing:    "leases.granted_node",
			wantEffective:  11,
		},
		{
			// A floor at or above the raw cursor clamps nothing.
			name:          "a floored table's absence does not raise a lower cursor",
			absentTables:  map[string]bool{"bd_events_seq": true},
			ignored:       15,
			wantLimited:   true,
			wantFloor:     21,
			wantMissing:   "bd_events_seq",
			wantEffective: 15,
		},
		{
			// A cursor of zero has nothing to contradict, so the library
			// returns before it consults the floor — and so does the probe:
			// the plane statement still names every sentinel table (they ride
			// the one statement whatever the cursor is), but its answer is
			// not applied and SHOW COLUMNS is not issued for a lane that has
			// never run.
			name:          "an empty ignored lane is not floored by its sentinels",
			absentTables:  map[string]bool{"wisps": true, "wisp_dependencies": true},
			absentColumns: map[string]bool{"leases.granted_node": true},
			ignored:       0,
			wantEffective: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeProbeConnector{
				cursors:        map[string]int64{mainCursorQuery: 66, ignoredCursorQuery: int64(tc.ignored)},
				absentTables:   tc.absentTables,
				absentColumns:  tc.absentColumns,
				likeDecoys:     tc.likeDecoys,
				recasedColumns: tc.recasedColumns,
				likeFoldsCase:  tc.likeFoldsCase,
			}
			report, err := readCursorsOver(context.Background(), fake)
			if err != nil {
				t.Fatalf("readCursorsOver: %v", err)
			}
			if report.Cursors.Ignored != tc.ignored {
				t.Fatalf("the probe reported ignored=%d, want the RAW on-disk %d: the payload an operator reads must be the disk truth",
					report.Cursors.Ignored, tc.ignored)
			}
			if report.Reality.Limited != tc.wantLimited {
				t.Fatalf("reality.Limited = %v, want %v (reality %+v)", report.Reality.Limited, tc.wantLimited, report.Reality)
			}
			if tc.wantLimited {
				if report.Reality.Floor != tc.wantFloor {
					t.Errorf("reality.Floor = %d, want %d", report.Reality.Floor, tc.wantFloor)
				}
				if report.Reality.Missing != tc.wantMissing {
					t.Errorf("reality.Missing = %q, want %q", report.Reality.Missing, tc.wantMissing)
				}
				if !strings.Contains(report.Reality.String(), tc.wantMissing) {
					t.Errorf("reality message %q does not name the missing sentinel", report.Reality.String())
				}
			}
			// Council pr2 D-F11: the session is what marks a reality
			// evaluated, on every row — including the empty lane, where the
			// library too has nothing to corroborate.
			if !report.Reality.Checked() {
				t.Fatalf("a completed session left its reality unchecked (%+v); the gate would refuse every probe", report.Reality)
			}
			if got := report.Reality.EffectiveIgnored(report.Cursors.Ignored); got != tc.wantEffective {
				t.Fatalf("EffectiveIgnored(%d) = %d, want %d: this is the number migrationSource.atLatest computes, and therefore the number that decides whether MigrateUp replays the ignored series",
					report.Cursors.Ignored, got, tc.wantEffective)
			}

			// The evidence, not just the conclusion: every table — both
			// cursor tables, every sentinel and floored sentinel table and the
			// sentinel column's table — is asked about in the ONE plane
			// statement, first; the column itself by SHOW COLUMNS, and only
			// when there is a cursor to contradict and a table to ask.
			statements := fake.statements()
			if len(statements) == 0 || !isSchemaPlaneQuery(statements[0]) {
				t.Fatalf("the session's first statement is not the plane statement: %v", statements)
			}
			for _, table := range schemaPlaneTables() {
				if !strings.Contains(statements[0], " | "+table) {
					t.Fatalf("the plane statement does not ask about %s: %v", table, statements[0])
				}
			}
			probedColumn := false
			for _, statement := range statements {
				if strings.HasPrefix(statement, sentinelColumnQuery) {
					probedColumn = true
				}
				// information_schema.columns answers in one statement, but Dolt
				// does not push its predicate down: it materializes every column
				// of every table on the server, 360-410 ms on a shared server.
				if strings.Contains(strings.ToLower(statement), "information_schema.columns") {
					t.Fatalf("the session asked information_schema.columns: %q", statement)
				}
				// One catalog scan for every table question, not one each.
				if n := strings.Count(statement, "information_schema.tables"); n > 1 || (n == 1 && !isSchemaPlaneQuery(statement)) {
					t.Fatalf("the session scanned information_schema.tables outside the plane statement: %q", statement)
				}
			}
			wantColumnProbe := tc.ignored > 0 && !tc.absentTables["wisps"] && !tc.absentTables["wisp_dependencies"] &&
				!tc.absentTables[IgnoredSentinelColumnTable]
			if probedColumn != wantColumnProbe {
				t.Fatalf("the session probed the sentinel column = %v, want %v; statements: %v",
					probedColumn, wantColumnProbe, fake.statements())
			}
			want := 3
			if wantColumnProbe {
				want = 4
			}
			if n := len(statements); n != want {
				t.Fatalf("the session issued %d statements, want %d (the plane, the two MAX reads, SHOW COLUMNS when owed): %v", n, want, statements)
			}
		})
	}
}

// TestProbeSessionReadsHeadOnItsFirstStatement is council pr2 D-F3's probe
// half: the pre-open HEAD observation rides as a second column on the
// statement every session already issues first, so it costs no statement, no
// round trip and no session of its own.
//
// It pins the COST as well as the value, because the cheap shape is the
// finding: a HEAD read issued as its own statement would pass a value-only test
// while adding a round trip to every probe, and one issued on its own session
// would add an accepted connection on bd's proxy per open.
func TestProbeSessionReadsHeadOnItsFirstStatement(t *testing.T) {
	fake := &fakeProbeConnector{
		cursors: map[string]int64{mainCursorQuery: 66, ignoredCursorQuery: 26},
		head:    "9abjndl5v792ofto5hlg6u0lvbao65tq",
	}
	report, err := readCursorsOver(context.Background(), fake)
	if err != nil {
		t.Fatalf("readCursorsOver: %v", err)
	}
	if report.Head != "9abjndl5v792ofto5hlg6u0lvbao65tq" {
		t.Fatalf("report.Head = %q, want the session's HEAD", report.Head)
	}
	statements := fake.statements()
	if len(statements) == 0 || !isSchemaPlaneQuery(statements[0]) {
		t.Fatalf("the session's first statement is not the plane statement carrying HEAD: %v", statements)
	}
	headReads := 0
	for _, statement := range statements {
		if strings.Contains(statement, "DOLT_HASHOF") {
			headReads++
		}
	}
	if headReads != 1 {
		t.Fatalf("the session asked for HEAD in %d statement(s), want exactly 1: %v", headReads, statements)
	}
	// A healthy database at 66/26: the plane statement (both cursor tables,
	// every sentinel and floored sentinel table, the sentinel column's table,
	// and HEAD), the two MAX reads, and SHOW COLUMNS for the sentinel column.
	// HEAD adds none of them.
	if want := 4; len(statements) != want {
		t.Fatalf("the session issued %d statements, want the %d it issues without a HEAD read of its own: %v", len(statements), want, statements)
	}
	if opened := fake.opened.Load(); opened != 1 {
		t.Fatalf("the session opened %d connection(s), want 1", opened)
	}

	t.Run("a truncated GROUP_CONCAT fails the session and admits nothing", func(t *testing.T) {
		// group_concat_max_len below the list's length cuts the last name
		// short. Read as "absent" that would floor the ignored lane from a
		// server setting; it must be an error, and the error is a server
		// answer, not a verdict about the proxy.
		fake := &fakeProbeConnector{
			cursors:         map[string]int64{mainCursorQuery: 66, ignoredCursorQuery: 26},
			truncatePresent: 20,
		}
		dials := 0
		got := Probe(context.Background(), ProbeIO{
			Session: func(ctx context.Context) (CursorReport, error) { return readCursorsOver(ctx, fake) },
			Dial:    func(context.Context) error { dials++; return nil },
		})
		if got.Outcome != ProbeUnknown {
			t.Fatalf("outcome = %v, want ProbeUnknown for a truncated plane answer (err %v)", got.Outcome, got.Err)
		}
		if got.Err == nil || !strings.Contains(got.Err.Error(), "not asked for") {
			t.Fatalf("err = %v, want the truncated-list error", got.Err)
		}
		if dials != 0 {
			t.Fatalf("a server error spent %d confirming dial(s)", dials)
		}
	})

	t.Run("a HEAD the server cannot answer fails the session and admits nothing", func(t *testing.T) {
		fake := &fakeProbeConnector{
			cursors: map[string]int64{mainCursorQuery: 66, ignoredCursorQuery: 26},
			headErr: errors.New("Error 1105 (HY000): function: 'dolt_hashof' not found"),
		}
		dials := 0
		got := Probe(context.Background(), ProbeIO{
			Session: func(ctx context.Context) (CursorReport, error) { return readCursorsOver(ctx, fake) },
			Dial:    func(context.Context) error { dials++; return nil },
		})
		if got.Outcome != ProbeUnknown {
			t.Fatalf("outcome = %v, want ProbeUnknown: an unobservable HEAD is a server answer, not a verdict about the proxy", got.Outcome)
		}
		if dials != 0 {
			t.Fatalf("a server error spent %d confirming dial(s); it must not reach the no-greeting ladder", dials)
		}
	})
}

// TestOnlyTheSessionAndTheNamedHelperMarkARealityChecked is council pr2 D-F11.
//
// A reality's zero value says "nothing was missing", which is the raw-cursor
// comparison A-F2 removed, so an unchecked one is refused by the gate. That is
// only a protection if nothing marks a reality checked by accident: the mark is
// unexported, the session sets it only when every read it owed completed, and
// the one exported way to get it is a helper whose name says it is for tests.
func TestOnlyTheSessionAndTheNamedHelperMarkARealityChecked(t *testing.T) {
	if (CursorReality{}).Checked() {
		t.Fatal("a zero-value reality reports itself checked")
	}
	handBuilt := ProbeResult{Outcome: ProbeServed, Cursors: Cursors{Main: 66, Ignored: 26}}
	if handBuilt.Reality.Checked() {
		t.Fatal("a hand-built served result carries a checked reality; a stub could be admitted without saying so")
	}
	if got := ServedProbeForTest(Cursors{Main: 66, Ignored: 26}, CursorReality{}); !got.Reality.Checked() || got.Outcome != ProbeServed {
		t.Fatalf("ServedProbeForTest = %+v, want a served result with a checked reality", got)
	}

	// A session that failed part-way marks nothing, even though it may have
	// filled the cursors it did read.
	failing := &fakeProbeConnector{
		cursors:  map[string]int64{mainCursorQuery: 66, ignoredCursorQuery: 26},
		queryErr: errors.New("invalid connection"),
	}
	report, err := readCursorsOver(context.Background(), failing)
	if err == nil {
		t.Fatal("a failing session reported success")
	}
	if report.Reality.Checked() {
		t.Fatal("a failed session marked its reality checked")
	}
}

// TestServedProbeForTestIsNeverCalledInProduction holds the other half of
// D-F11's line: the helper that marks a reality checked must not become a
// production shortcut past the session. It scans every non-test Go file in the
// module.
func TestServedProbeForTestIsNeverCalledInProduction(t *testing.T) {
	// Bazel compiles with runfiles-relative paths, so runtime.Caller
	// arithmetic lands on "." rather than the module root; the runfiles tree
	// (//:go.mod + //:repo_go_srcs in this test's data) is the module there.
	root := bazeltest.OverrideRoot()
	if root == "" {
		_, self, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("runtime.Caller failed")
		}
		root = filepath.Clean(filepath.Join(filepath.Dir(self), "..", "..", ".."))
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", ".claude":
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
					return filepath.SkipDir // a nested worktree is another checkout
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			filepath.Base(path) == "probe_testing.go" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if strings.Contains(string(body), "ServedProbeForTest(") {
			t.Errorf("%s calls ServedProbeForTest outside a test: only the probe session may mark a reality checked", path)
		}
		if strings.Contains(string(body), "CursorReportForTest(") {
			t.Errorf("%s calls CursorReportForTest outside a test: only the probe session may mark a reality checked", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if scanned < 100 {
		t.Fatalf("scanned %d non-test Go files from %s; the walk is not seeing the module", scanned, root)
	}
}
