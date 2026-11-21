package proxyendpoint

import (
	"context"
	"errors"
	"fmt"
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

// TestReadPostOpenReadsTheWindowOnThePinnedConnection is the evidence half of
// attributing a moved HEAD: when the observed HEAD is not the one admission
// saw, the read asks what the commits in between were and which tables they
// changed, on the same pinned connection, and a window it cannot read is
// carried in the report rather than returned as an error (a returned error is
// "unobserved, proceed" to the caller).
func TestReadPostOpenReadsTheWindowOnThePinnedConnection(t *testing.T) {
	const since = "0123456789abcdefghijklmnopqrstuv"
	const after = "vutsrqponmlkjihgfedcba9876543210"

	t.Run("an unmoved head reads no window", func(t *testing.T) {
		pool := proxyendpointtest.NewPostOpenDB(proxyendpointtest.State{Head: since}, proxyendpointtest.State{Head: since})
		t.Cleanup(func() { _ = pool.DB.Close() })
		report, err := ReadPostOpen(context.Background(), pool.DB, since)
		if err != nil {
			t.Fatalf("ReadPostOpen: %v", err)
		}
		if report.Window != nil {
			t.Fatalf("window = %+v for an unmoved head, want none", report.Window)
		}
		if pool.Conns() != 1 || len(pool.Statements()) != 2 {
			t.Fatalf("the read used %d connection(s) and %d statement(s), want 1 and 2", pool.Conns(), len(pool.Statements()))
		}
	})

	t.Run("no admitted head reads no window", func(t *testing.T) {
		pool := proxyendpointtest.NewPostOpenDB(proxyendpointtest.State{Head: since}, proxyendpointtest.State{Head: after})
		t.Cleanup(func() { _ = pool.DB.Close() })
		report, err := ReadPostOpen(context.Background(), pool.DB, "")
		if err != nil {
			t.Fatalf("ReadPostOpen: %v", err)
		}
		if report.Window != nil || len(pool.Statements()) != 2 {
			t.Fatalf("window = %+v after %d statement(s), want none after 2", report.Window, len(pool.Statements()))
		}
	})

	t.Run("a moved head reads the commits and tables between the two hashes", func(t *testing.T) {
		pool := proxyendpointtest.NewPostOpenDB(proxyendpointtest.State{Head: since}, proxyendpointtest.State{Head: after}).
			WithWindow(proxyendpointtest.Window{
				Commits: []string{"bd: update ga-1", "bd: create ga-2"},
				Tables:  []proxyendpointtest.WindowTable{{Name: "issues"}, {Name: "events", SchemaChange: true}},
			})
		t.Cleanup(func() { _ = pool.DB.Close() })
		report, err := ReadPostOpen(context.Background(), pool.DB, since)
		if err != nil {
			t.Fatalf("ReadPostOpen: %v", err)
		}
		want := &PostOpenWindow{
			Commits: []string{"bd: update ga-1", "bd: create ga-2"},
			Tables:  []WindowTable{{Name: "issues"}, {Name: "events", SchemaChange: true}},
		}
		if report.Head != after || !reflect.DeepEqual(report.Window, want) {
			t.Fatalf("report = head %q window %+v, want head %q window %+v", report.Head, report.Window, after, want)
		}
		statements := pool.Statements()
		if pool.Conns() != 1 || len(statements) != 4 {
			t.Fatalf("the read used %d connection(s) and %d statement(s), want 1 and 4: the window rides the pinned connection",
				pool.Conns(), len(statements))
		}
		if !strings.Contains(statements[2], "dolt_log('"+since+".."+after+"')") {
			t.Errorf("the log statement %q does not name the range %s..%s", statements[2], since, after)
		}
		if !strings.Contains(statements[3], "dolt_diff_summary('"+since+"', '"+after+"')") {
			t.Errorf("the diff statement %q does not name both hashes", statements[3])
		}
	})

	t.Run("a window longer than the limit is truncated, not read whole", func(t *testing.T) {
		commits := make([]string, PostOpenWindowCommitLimit+1)
		for i := range commits {
			commits[i] = "bd: write"
		}
		pool := proxyendpointtest.NewPostOpenDB(proxyendpointtest.State{Head: since}, proxyendpointtest.State{Head: after}).
			WithWindow(proxyendpointtest.Window{Commits: commits})
		t.Cleanup(func() { _ = pool.DB.Close() })
		report, err := ReadPostOpen(context.Background(), pool.DB, since)
		if err != nil {
			t.Fatalf("ReadPostOpen: %v", err)
		}
		if report.Window == nil || !report.Window.Truncated || len(report.Window.Commits) != PostOpenWindowCommitLimit {
			t.Fatalf("window = %+v, want truncated at %d commits", report.Window, PostOpenWindowCommitLimit)
		}
		if !strings.Contains(pool.Statements()[2], fmt.Sprintf("LIMIT %d", PostOpenWindowCommitLimit+1)) {
			t.Errorf("the log statement %q does not bound the read at one past the limit", pool.Statements()[2])
		}
	})

	t.Run("a failing window statement is carried in the report, not returned", func(t *testing.T) {
		pool := proxyendpointtest.NewPostOpenDB(proxyendpointtest.State{Head: since}, proxyendpointtest.State{Head: after}).
			WithWindow(proxyendpointtest.Window{Err: errors.New("table function dolt_log not found")})
		t.Cleanup(func() { _ = pool.DB.Close() })
		report, err := ReadPostOpen(context.Background(), pool.DB, since)
		if err != nil {
			t.Fatalf("ReadPostOpen = %v, want the head observed and the window marked unreadable", err)
		}
		if report.Head != after || report.Window == nil || !strings.Contains(report.Window.Unreadable, "dolt_log not found") {
			t.Fatalf("report = head %q window %+v, want head %q and the failure in Window.Unreadable", report.Head, report.Window, after)
		}
	})

	for _, tc := range []struct{ name, since string }{
		{"a non-hash admitted head", "before0000"},
		{"an admitted head carrying a quote", "0123456789abcdefghijklmnopqrst'v"},
	} {
		t.Run(tc.name+" is never inlined into a statement", func(t *testing.T) {
			pool := proxyendpointtest.NewPostOpenDB(proxyendpointtest.State{Head: tc.since}, proxyendpointtest.State{Head: after})
			t.Cleanup(func() { _ = pool.DB.Close() })
			report, err := ReadPostOpen(context.Background(), pool.DB, tc.since)
			if err != nil {
				t.Fatalf("ReadPostOpen: %v", err)
			}
			if report.Window == nil || !strings.Contains(report.Window.Unreadable, "not a Dolt commit hash") {
				t.Fatalf("window = %+v, want it unreadable for a hash that fails validation", report.Window)
			}
			if got := len(pool.Statements()); got != 2 {
				t.Fatalf("the read issued %d statement(s), want 2: an unvalidated hash must not reach a window statement", got)
			}
		})
	}
}
