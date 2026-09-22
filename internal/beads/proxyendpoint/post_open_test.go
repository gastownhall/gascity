package proxyendpoint

import (
	"context"
	"errors"
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
		report, err := ReadPostOpen(context.Background(), pool.DB)
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
		if _, err := ReadPostOpen(context.Background(), pool.DB); err == nil {
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
			report, err := ReadPostOpen(context.Background(), pool.DB)
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
					strings.Count(statement, "information_schema.tables") != len(ignoredSentinelTables)+1 ||
					strings.Count(statement, "information_schema.columns") != 1 {
					t.Fatalf("statement %q does not carry HEAD, the cursor table, every sentinel table and the column", statement)
				}
			}
		})
	}

	t.Run("no pool is an error", func(t *testing.T) {
		if _, err := ReadPostOpen(context.Background(), nil); !errors.Is(err, ErrPostOpenNoPool) {
			t.Fatalf("ReadPostOpen(nil) err = %v, want ErrPostOpenNoPool", err)
		}
	})
}
