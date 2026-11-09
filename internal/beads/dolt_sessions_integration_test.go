//go:build integration

package beads

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// reservedDoltTestPorts are ports a test must never bind or reach.
var reservedDoltTestPorts = map[int]bool{3307: true, 45865: true, 8123: true}

// TestForeignDoltSessionsNamesEveryOtherSession runs on a throwaway server
// (startTestDoltServer: a free non-reserved port, stopped by its process): the
// asking session is left out, and another client's session is listed with its
// user and database until it goes.
func TestForeignDoltSessionsNamesEveryOtherSession(t *testing.T) {
	ctx := context.Background()
	port := startTestDoltServer(t)
	self, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = self.Close() })
	self.SetMaxOpenConns(1)
	// The harness's own admin client still holds a session; it is foreign
	// here, and is the baseline.
	baseline, err := ForeignDoltSessions(ctx, self)
	if err != nil {
		t.Fatalf("ForeignDoltSessions: %v", err)
	}
	var ownID int64
	if err := self.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&ownID); err != nil {
		t.Fatal(err)
	}
	for _, c := range baseline {
		if c.ID == ownID {
			t.Fatalf("ForeignDoltSessions = %+v lists the asking session %d", baseline, ownID)
		}
	}

	other, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/beads", port))
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var otherID int64
	if err := pinned.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	foreign, err := ForeignDoltSessions(ctx, self)
	if err != nil {
		t.Fatal(err)
	}
	var found []DoltConnection
	for _, c := range foreign {
		if c.ID == otherID {
			found = append(found, c)
		}
	}
	if len(foreign) != len(baseline)+1 || len(found) != 1 || found[0].User != "root" || found[0].DB != "beads" {
		t.Fatalf("ForeignDoltSessions = %+v, want the baseline %+v plus session %d (root on beads)", foreign, baseline, otherID)
	}

	_ = pinned.Close()
	_ = other.Close()
	// The server reaps a closed session asynchronously; each pass is a real
	// round trip, so a bounded number of them waits it out without a timer.
	for range 500 {
		gone, err := ForeignDoltSessions(ctx, self)
		if err != nil {
			t.Fatal(err)
		}
		if len(gone) == len(baseline) {
			return
		}
	}
	t.Fatalf("session %d is still listed after its client left", otherID)
}
