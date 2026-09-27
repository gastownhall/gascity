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
		name          string
		absentTables  map[string]bool
		absentColumns map[string]bool
		ignored       int
		wantLimited   bool
		wantFloor     int
		wantMissing   string
		wantEffective int
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
			// A cursor of zero has nothing to contradict, so the library
			// returns before it consults the floor — and so does the probe,
			// which keeps the sentinel reads off the path of a database that
			// has never run the lane.
			name:          "an empty ignored lane is not probed for sentinels",
			absentTables:  map[string]bool{"wisps": true, "wisp_dependencies": true},
			absentColumns: map[string]bool{"leases.granted_node": true},
			ignored:       0,
			wantEffective: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeProbeConnector{
				cursors:       map[string]int64{mainCursorQuery: 66, ignoredCursorQuery: int64(tc.ignored)},
				absentTables:  tc.absentTables,
				absentColumns: tc.absentColumns,
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

			// The evidence, not just the conclusion.
			probedColumn := false
			for _, statement := range fake.statements() {
				if strings.HasPrefix(statement, columnExistsQuery) {
					probedColumn = true
				}
			}
			wantColumnProbe := tc.ignored > 0 && len(tc.absentTables) == 0
			if probedColumn != wantColumnProbe {
				t.Fatalf("the session probed the sentinel column = %v, want %v; statements: %v",
					probedColumn, wantColumnProbe, fake.statements())
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
	if len(statements) == 0 || !strings.HasPrefix(statements[0], cursorExistsWithHeadQuery) {
		t.Fatalf("the session's first statement is not the main-lane existence check carrying HEAD: %v", statements)
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
	// A healthy database at 66/26 with every sentinel present: two cursor
	// existence checks, two MAX reads, two sentinel tables, one sentinel column.
	// That is what a session issued before the HEAD observation existed.
	if len(statements) != 7 {
		t.Fatalf("the session issued %d statements, want the 7 it issued before HEAD was observed: %v", len(statements), statements)
	}
	if opened := fake.opened.Load(); opened != 1 {
		t.Fatalf("the session opened %d connection(s), want 1", opened)
	}

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
	// (//:go.mod + //:repo_source_tree in this test's data) is the module there.
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
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if scanned < 100 {
		t.Fatalf("scanned %d non-test Go files from %s; the walk is not seeing the module", scanned, root)
	}
}
