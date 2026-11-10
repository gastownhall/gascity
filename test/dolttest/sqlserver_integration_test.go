//go:build integration

package dolttest

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// requireDolt returns the dolt binary, skipping the test when there is none.
func requireDolt(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not on PATH")
	}
	return path
}

// serverArgs returns the argv for a loopback server on an empty data dir.
func serverArgs(dataDir string) func(port int) []string {
	return func(port int) []string {
		return []string{"sql-server", "-H", "127.0.0.1", "-P", strconv.Itoa(port), "--data-dir", dataDir, "--loglevel", "warning"}
	}
}

// isolatedDoltEnv keeps dolt's global state out of the real home directory.
func isolatedDoltEnv(t *testing.T) []string {
	t.Helper()
	return append(os.Environ(), "DOLT_ROOT_PATH="+t.TempDir())
}

// holdPort listens on a loopback port and closes every connection it accepts,
// as another test's server would on a port this test also probed.
func holdPort(t *testing.T) (listener net.Listener, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("holding a port: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return listener, listener.Addr().(*net.TCPAddr).Port
}

// heldThenFresh picks heldPort first and a freshly probed port after that,
// recording every pick.
func heldThenFresh(heldPort int, picks *[]int) func() (int, error) {
	return func() (int, error) {
		port := heldPort
		if len(*picks) > 0 {
			var err error
			if port, err = probePort("127.0.0.1"); err != nil {
				return 0, err
			}
		}
		*picks = append(*picks, port)
		return port, nil
	}
}

// requireRetriedOnto fails the test unless the first pick was the held port and
// srv is the server on the last pick, which is a different port.
func requireRetriedOnto(t *testing.T, srv *SQLServer, heldPort int, picks []int) {
	t.Helper()
	if len(picks) < 2 || picks[0] != heldPort || srv.Port != picks[len(picks)-1] {
		t.Fatalf("picked %v and returned port %d, want the held port %d first and the port returned last", picks, srv.Port, heldPort)
	}
	if srv.Port == heldPort {
		t.Fatalf("returned the port %d that another process held", heldPort)
	}
}

// requireOwnsPort fails the test unless the server's own process holds its
// port. Hosts without socket tables cannot tell, so they pass.
func requireOwnsPort(t *testing.T, srv *SQLServer) {
	t.Helper()
	holder, readable := pidutil.ListenerPID(srv.Port, srv.PID)
	if readable && holder != srv.PID {
		t.Fatalf("port %d is held by pid %d, not by the server this test started (pid %d)", srv.Port, holder, srv.PID)
	}
}

// requireAccepting fails the test unless addr accepts a connection.
func requireAccepting(t *testing.T, addr, what string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("%s at %s does not accept connections: %v", what, addr, err)
	}
	_ = conn.Close()
}

func TestStartSQLServerRetriesWhenProbedPortIsTaken(t *testing.T) {
	doltPath := requireDolt(t)
	held, heldPort := holdPort(t)
	var picks []int

	srv := StartSQLServer(t, SQLServerSpec{
		Dolt:     doltPath,
		Args:     serverArgs(t.TempDir()),
		Env:      isolatedDoltEnv(t),
		PickPort: heldThenFresh(heldPort, &picks),
	})

	requireRetriedOnto(t, srv, heldPort, picks)
	requireOwnsPort(t, srv)
	requireAccepting(t, held.Addr().String(), "the other process's listener")
}

func TestStartSQLServerFailsOnNonPortExit(t *testing.T) {
	doltPath := requireDolt(t)
	attempts := 0

	failure := failureOf(t, func(tb testing.TB) {
		StartSQLServer(tb, SQLServerSpec{
			Dolt: doltPath,
			Args: func(port int) []string {
				attempts++
				return []string{"sql-server", "--no-such-flag", "-P", strconv.Itoa(port)}
			},
			Env: isolatedDoltEnv(t),
		})
	})

	if !strings.Contains(failure, "exited before it listened") {
		t.Fatalf("failure %q does not say dolt exited before it listened", failure)
	}
	if attempts != 1 {
		t.Fatalf("started dolt %d times; an exit that is not a lost port is not retried", attempts)
	}
}

func TestStartSQLServerWithoutSocketTablesStillRejectsALostPort(t *testing.T) {
	doltPath := requireDolt(t)
	held, heldPort := holdPort(t)
	var picks []int
	pick := heldThenFresh(heldPort, &picks)
	useListenerPID(t, func(int, ...int) (int, bool) { return 0, false })
	// Without socket tables a successful dial is all that shows a listener, and
	// here it reaches the other process's listener. Only dolt exiting while the
	// settle window is open shows its port lost, so the first attempt's window
	// is long (it ends the moment dolt exits) and the retry's, which has
	// nothing to wait out, is short.
	setDuration(t, &sqlServerSettle, time.Minute)

	srv := StartSQLServer(t, SQLServerSpec{
		Dolt: doltPath,
		Args: serverArgs(t.TempDir()),
		Env:  isolatedDoltEnv(t),
		PickPort: func() (int, error) {
			port, err := pick()
			if len(picks) > 1 {
				sqlServerSettle = 50 * time.Millisecond
			}
			return port, err
		},
	})

	requireRetriedOnto(t, srv, heldPort, picks)
	select {
	case <-srv.Exited:
		t.Fatal("the server exited after StartSQLServer returned it")
	default:
	}
	requireAccepting(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.Port)), "the started server")
	requireAccepting(t, held.Addr().String(), "the other process's listener")
}
