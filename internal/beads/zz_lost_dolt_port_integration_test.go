//go:build integration

package beads

import (
	"database/sql"
	"fmt"
	"net"
	"testing"
)

// This file is named to sort last among beads_test's sources on purpose: rules_go
// shards round-robin over source order, so a test added anywhere earlier moves
// the heavy_tests.txt tests that follow it into shared shards (scripts
// TestBeadsShardsKeepHeavyTestsApart). Adding at the end moves none of them.

// holdPortWithClientSocket takes port with an ESTABLISHED client socket whose
// local end is port, the way a loopback connect() draws a source port. Nothing
// listens on it, so dolt's own pre-bind check passes and its bind then fails
// with "bind: address already in use". The socket is closed with the test.
func holdPortWithClientSocket(t *testing.T, port int) {
	t.Helper()
	sink, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	client, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}}).Dial("tcp", sink.Addr().String())
	if err != nil {
		t.Fatalf("hold port %d with a client socket: %v", port, err)
	}
	t.Cleanup(func() { _ = client.Close() })
}

// A probe-and-close port can be taken before dolt binds it, by a socket that is
// not a listener (a connect()'s source port). startTestDoltServer must give the
// lost port up and run on another one instead of waiting out a server that
// never listens.
func TestStartTestDoltServerSurvivesALostProbedPort(t *testing.T) {
	lost, err := pickUnreservedTestDoltPort()
	if err != nil {
		t.Fatal(err)
	}
	holdPortWithClientSocket(t, lost)

	picks := 0
	orig := testDoltPortPick
	testDoltPortPick = func() (int, error) {
		picks++
		if picks == 1 {
			return lost, nil
		}
		return pickUnreservedTestDoltPort()
	}
	t.Cleanup(func() { testDoltPortPick = orig })

	port := startTestDoltServer(t)
	if port == lost {
		t.Fatalf("startTestDoltServer returned the lost port %d", lost)
	}
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port))
	if err != nil {
		t.Fatalf("open dolt connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("USE beads"); err != nil {
		t.Fatalf("USE beads on port %d: %v", port, err)
	}
}
