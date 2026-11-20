package proxyendpoint

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint/proxyendpointtest"
)

// TestReadPostOpenObservesTheStateAfterTheOpen is council pr2 E-S3.
//
// The pool models beads' be-itm5: a connection that ran the library's open-time
// checks is pinned to the pre-open session root, answers its FIRST later
// statement from it, and is advanced only by a succeeding statement. The
// control proves the model bites — one statement on the pool is the pre-open
// answer — so the main row is not passing against a pool with nothing stale in
// it.
func TestReadPostOpenObservesTheStateAfterTheOpen(t *testing.T) {
	before := proxyendpointtest.State{Head: "before0000"}
	after := proxyendpointtest.State{Head: "after11111"}

	t.Run("control: one statement on the pool reads the pre-open root", func(t *testing.T) {
		pool := proxyendpointtest.NewPostOpenDB(before, after)
		t.Cleanup(func() { _ = pool.DB.Close() })
		var head string
		if err := pool.DB.QueryRowContext(context.Background(), postOpenHeadQuery).Scan(&head); err != nil {
			t.Fatalf("single statement: %v", err)
		}
		if head != "before0000" {
			t.Fatalf("the modeled pool answered %q on its first statement, want the pre-open hash", head)
		}
	})

	t.Run("the observation is the second statement on one pinned connection", func(t *testing.T) {
		pool := proxyendpointtest.NewPostOpenDB(before, after)
		t.Cleanup(func() { _ = pool.DB.Close() })
		report, err := ReadPostOpen(context.Background(), pool.DB, "")
		if err != nil {
			t.Fatalf("ReadPostOpen: %v", err)
		}
		if report.Head != "after11111" {
			t.Fatalf("head = %q, want the hash after the open: an open that committed would pass the check", report.Head)
		}
		if pool.Conns() != 1 || len(pool.Statements()) != 2 {
			t.Fatalf("the read used %d connection(s) and %d statement(s), want 1 and 2", pool.Conns(), len(pool.Statements()))
		}
	})

	t.Run("a failing advancing statement is a failure, not a stale answer", func(t *testing.T) {
		pool := proxyendpointtest.NewPostOpenDB(before, after).Failing(errors.New("invalid connection"))
		t.Cleanup(func() { _ = pool.DB.Close() })
		if _, err := ReadPostOpen(context.Background(), pool.DB, ""); err == nil {
			t.Fatal("a failed statement produced an observation")
		}
		if got := len(pool.Statements()); got != 1 {
			t.Fatalf("the read issued %d statement(s) after the first one failed, want 1: "+
				"a failing statement does not advance the root, so the second answer would be as stale", got)
		}
	})

	// Council pr2 E-S4: the same statement reads the ignored plane, in the
	// library's order, AFTER the open.
	for _, tc := range []struct {
		name  string
		after proxyendpointtest.State
		want  PostOpenReport
	}{
		{
			name:  "a healthy plane",
			after: proxyendpointtest.State{Head: "h"},
			want:  PostOpenReport{Head: "h", IgnoredCursorTable: true},
		},
		{
			name: "a missing sentinel table floors at 0 and short-circuits the column",
			after: proxyendpointtest.State{
				Head: "h", AbsentTables: map[string]bool{"wisps": true},
				AbsentColumns: map[string]bool{"leases.granted_node": true},
			},
			want: PostOpenReport{
				Head: "h", IgnoredCursorTable: true,
				Reality: CursorReality{Limited: true, Floor: IgnoredSentinelTableFloor, Missing: "wisps"},
			},
		},
		{
			name:  "a missing sentinel column floors at 11",
			after: proxyendpointtest.State{Head: "h", AbsentColumns: map[string]bool{"leases.granted_node": true}},
			want: PostOpenReport{
				Head: "h", IgnoredCursorTable: true,
				Reality: CursorReality{Limited: true, Floor: IgnoredSentinelColumnFloor, Missing: "leases.granted_node"},
			},
		},
		{
			name:  "a missing floored sentinel table floors at its replay floor",
			after: proxyendpointtest.State{Head: "h", AbsentTables: map[string]bool{"bd_events_journal": true}},
			want: PostOpenReport{
				Head: "h", IgnoredCursorTable: true,
				Reality: CursorReality{Limited: true, Floor: 21, Missing: "bd_events_journal"},
			},
		},
		{
			name: "the lowest floor of the absent floored tables and the column wins",
			after: proxyendpointtest.State{
				Head: "h", AbsentTables: map[string]bool{"events": true, "bd_events_seq": true},
				AbsentColumns: map[string]bool{"leases.granted_node": true},
			},
			want: PostOpenReport{
				Head: "h", IgnoredCursorTable: true,
				Reality: CursorReality{Limited: true, Floor: IgnoredSentinelColumnFloor, Missing: "leases.granted_node"},
			},
		},
		{
			name:  "a vanished cursor table",
			after: proxyendpointtest.State{Head: "h", AbsentTables: map[string]bool{"ignored_schema_migrations": true}},
			want:  PostOpenReport{Head: "h"},
		},
	} {
		t.Run("the ignored plane: "+tc.name, func(t *testing.T) {
			// The pre-open state is healthy in every row, so a reader that
			// took the first statement's answer would report no change.
			pool := proxyendpointtest.NewPostOpenDB(proxyendpointtest.State{Head: "h"}, tc.after)
			t.Cleanup(func() { _ = pool.DB.Close() })
			report, err := ReadPostOpen(context.Background(), pool.DB, "")
			if err != nil {
				t.Fatalf("ReadPostOpen: %v", err)
			}
			if report != tc.want {
				t.Fatalf("report = %+v, want %+v", report, tc.want)
			}
			// HEAD and the plane ride ONE statement (issued twice), not one
			// statement per question.
			for _, statement := range pool.Statements() {
				if !strings.Contains(statement, "DOLT_HASHOF('HEAD')") ||
					strings.Count(statement, "information_schema.tables") != len(ignoredSentinelTables)+len(ignoredSentinelFlooredTables)+1 ||
					strings.Count(statement, "information_schema.columns") != 1 {
					t.Fatalf("statement %q does not carry HEAD, the cursor table, every sentinel and floored sentinel table and the column", statement)
				}
			}
		})
	}

	t.Run("no pool is an error", func(t *testing.T) {
		if _, err := ReadPostOpen(context.Background(), nil, ""); !errors.Is(err, ErrPostOpenNoPool) {
			t.Fatalf("ReadPostOpen(nil) err = %v, want ErrPostOpenNoPool", err)
		}
	})
}

// TestReadPostOpenReadsTheWindowWhenHeadMoved is the evidence half of the
// moved-HEAD attribution.
//
// head_moved fires whenever HEAD moved, because gc cannot tell its own open's
// commit from another bd client's — and on a busy rig the second is the common
// case, so a verdict that cannot tell them apart demotes every open on the
// store. The window between the two hashes is what CAN tell them apart: every
// commit a linked-library open mints carries the "schema:" message prefix, and
// every table it touches on the committed plane is in the library's open-write
// set. A window that shows neither is a move somebody else made, and the
// observation — not the verdict — is the only place both hashes are in hand to
// ask for it.
//
// The window is read ONLY for a moved HEAD, on the same pinned connection the
// observation already holds, because the questions cost two statements and an
// unmoved HEAD has nothing to attribute.
func TestReadPostOpenReadsTheWindowWhenHeadMoved(t *testing.T) {
	// Real 32-character Dolt hashes: the window's table functions take no bind
	// parameters, so the observation inlines the two hashes into the statements
	// and must refuse anything a real DOLT_HASHOF would not answer with.
	const since = "0000000000000000000000000000aaaa"
	const after = "0000000000000000000000000000bbbb"
	scripted := &proxyendpointtest.Window{
		Commits: []string{"bd: update ga-1", "bd: close ga-2"},
		Tables:  []proxyendpointtest.WindowTable{{Name: "issues"}, {Name: "statuses", SchemaChange: true}},
	}

	t.Run("a moved head carries the window between the two hashes", func(t *testing.T) {
		pool := proxyendpointtest.NewPostOpenDB(
			proxyendpointtest.State{Head: since},
			proxyendpointtest.State{Head: after, Window: scripted})
		t.Cleanup(func() { _ = pool.DB.Close() })
		report, err := ReadPostOpen(context.Background(), pool.DB, since)
		if err != nil {
			t.Fatalf("ReadPostOpen: %v", err)
		}
		if report.Window == nil {
			t.Fatal("a moved HEAD was observed without its window: attribution has nothing to work with")
		}
		if got := report.Window.Unreadable; got != "" {
			t.Fatalf("the window read failed: %s", got)
		}
		if !reflect.DeepEqual(report.Window.Commits, []string{"bd: update ga-1", "bd: close ga-2"}) {
			t.Fatalf("commits = %v", report.Window.Commits)
		}
		if !reflect.DeepEqual(report.Window.Tables, []WindowTable{{Name: "issues"}, {Name: "statuses", SchemaChange: true}}) {
			t.Fatalf("tables = %+v", report.Window.Tables)
		}
		if report.Window.Truncated {
			t.Error("a two-commit window was read as truncated")
		}
		statements := pool.Statements()
		if len(statements) != 4 {
			t.Fatalf("the read issued %d statement(s), want 4: the advancing statement, the observation, then the window's dolt_log and dolt_diff_summary", len(statements))
		}
		logQuery, diffQuery := statements[2], statements[3]
		if !strings.Contains(logQuery, "dolt_log(") || !strings.Contains(logQuery, since) || !strings.Contains(logQuery, after) {
			t.Fatalf("the commit statement is not dolt_log over %s..%s: %s", since, after, logQuery)
		}
		if !strings.Contains(diffQuery, "dolt_diff_summary(") || !strings.Contains(diffQuery, since) || !strings.Contains(diffQuery, after) {
			t.Fatalf("the table statement is not dolt_diff_summary over %s..%s: %s", since, after, diffQuery)
		}
		if pool.Conns() != 1 {
			t.Fatalf("the window used %d connection(s), want 1: it must ride the pinned connection the observation already holds, not open another", pool.Conns())
		}
	})

	t.Run("an unmoved head spends no window statements", func(t *testing.T) {
		pool := proxyendpointtest.NewPostOpenDB(
			proxyendpointtest.State{Head: after},
			proxyendpointtest.State{Head: after, Window: scripted})
		t.Cleanup(func() { _ = pool.DB.Close() })
		report, err := ReadPostOpen(context.Background(), pool.DB, after)
		if err != nil {
			t.Fatalf("ReadPostOpen: %v", err)
		}
		if report.Window != nil {
			t.Fatalf("an unmoved HEAD carried a window: %+v", report.Window)
		}
		if got := len(pool.Statements()); got != 2 {
			t.Fatalf("an unmoved HEAD issued %d statement(s), want 2: the window is evidence about a move, and there was none", got)
		}
	})

	t.Run("no pinned since spends no window statements", func(t *testing.T) {
		pool := proxyendpointtest.NewPostOpenDB(
			proxyendpointtest.State{Head: since},
			proxyendpointtest.State{Head: after, Window: scripted})
		t.Cleanup(func() { _ = pool.DB.Close() })
		report, err := ReadPostOpen(context.Background(), pool.DB, "")
		if err != nil {
			t.Fatalf("ReadPostOpen: %v", err)
		}
		if report.Window != nil {
			t.Fatalf("a memo-shaped read (no pinned hash) carried a window: %+v", report.Window)
		}
		if got := len(pool.Statements()); got != 2 {
			t.Fatalf("a read with no since issued %d statement(s), want 2", got)
		}
	})
}
