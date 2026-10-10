package dolttest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
)

const (
	// sqlServerMaxAttempts bounds how many probed ports StartSQLServer loses
	// to other listeners before it gives up.
	sqlServerMaxAttempts = 5
	// sqlServerPollInterval is how often an attempt re-checks its process.
	sqlServerPollInterval = 25 * time.Millisecond
	// sqlServerAttemptTimeout bounds one attempt's wait for the listener.
	sqlServerAttemptTimeout = 60 * time.Second
	// sqlServerLogTailLines is how much of the server log a failure carries.
	sqlServerLogTailLines = 40
	// sqlServerPortTaken is what dolt prints, in any case, when it cannot
	// bind the port it was handed ("Port N already in use.").
	sqlServerPortTaken = "already in use"
)

var (
	// sqlServerSettle is how long a process must stay alive after a
	// successful dial before a host without socket tables takes it to own
	// the port. Tests lengthen it so the check does not depend on how fast
	// dolt gives up a port it lost.
	sqlServerSettle = 500 * time.Millisecond
	// sqlServerStopGrace is how long Stop waits after SIGTERM before it
	// sends SIGKILL.
	sqlServerStopGrace = 10 * time.Second
	// listenerPID is pidutil.ListenerPID; tests replace it to stand in for a
	// host without socket tables.
	listenerPID = pidutil.ListenerPID
)

// SQLServerSpec describes a test-owned `dolt sql-server` launch.
type SQLServerSpec struct {
	// Dolt is the dolt binary.
	Dolt string
	// Args returns the argv after the binary for the chosen port, including
	// the "sql-server" subcommand.
	Args func(port int) []string
	// Host is the address the port probe binds and the dial fallback
	// reaches; "" means 127.0.0.1 and "0.0.0.0" suits a server that binds
	// every interface.
	Host string
	// Dir is the optional working directory.
	Dir string
	// Env is the optional environment; nil inherits the test process's.
	Env []string
	// LogPath receives stdout and stderr; "" means a file in t.TempDir().
	// Each attempt truncates it, so it holds the final server's output.
	LogPath string
	// PickPort chooses the port for each attempt; nil probes a free port on
	// Host. It is a test seam for forcing a lost race.
	PickPort func() (int, error)
	// SysProcAttr is the optional process attributes, for a server that has
	// to lead its own process group.
	SysProcAttr *syscall.SysProcAttr
}

// SQLServer is a running test-owned `dolt sql-server` whose own process holds
// Port.
type SQLServer struct {
	// Port is the TCP port the server's own process listens on.
	Port int
	// PID is the server's process ID.
	PID int
	// Process is the server's process. StartSQLServer owns its Wait, so
	// callers signal it and wait on Exited instead.
	Process *os.Process
	// Exited is closed once the process has exited and been reaped.
	Exited <-chan struct{}
	// LogPath is the file holding the server's stdout and stderr.
	LogPath string
}

// Stop ends the server: SIGTERM, then SIGKILL if it is still running after ten
// seconds. It is idempotent and returns once the process has exited.
func (s *SQLServer) Stop() {
	select {
	case <-s.Exited:
		return
	default:
	}
	_ = s.Process.Signal(syscall.SIGTERM)
	grace := time.NewTimer(sqlServerStopGrace)
	defer grace.Stop()
	select {
	case <-s.Exited:
	case <-grace.C:
		_ = s.Process.Kill()
		<-s.Exited
	}
}

// StartSQLServer starts a test-owned `dolt sql-server` on a free port and
// returns it only once the process it started holds that port, for as long as
// the server lives.
//
// A port picked by probe-and-close can be taken by another listener before
// dolt binds it. Dolt then exits with "Port N already in use.", and a readiness
// probe that accepts any answering server would run the test against the other
// process's server. StartSQLServer checks that its own process listens on the
// port and retries on a fresh port when dolt lost it; any other early exit, or
// no listener within a minute, fails the test with the tail of the server log.
// The server is stopped when the test ends.
func StartSQLServer(t testing.TB, spec SQLServerSpec) *SQLServer {
	t.Helper()
	if spec.Dolt == "" || spec.Args == nil {
		t.Fatalf("dolttest.StartSQLServer: SQLServerSpec needs Dolt and Args")
	}
	host := spec.Host
	if host == "" {
		host = "127.0.0.1"
	}
	pick := spec.PickPort
	if pick == nil {
		pick = func() (int, error) { return probePort(host) }
	}
	logPath := spec.LogPath
	if logPath == "" {
		logPath = filepath.Join(t.TempDir(), "dolt-sql-server.log")
	}
	for range sqlServerMaxAttempts {
		port, err := pick()
		if err != nil {
			t.Fatalf("picking a port for dolt sql-server: %v", err)
		}
		server, lostPort := startSQLServerOnPort(t, spec, host, logPath, port)
		if lostPort {
			t.Logf("dolt sql-server lost probed port %d to another process; retrying on a fresh port", port)
			continue
		}
		t.Cleanup(server.Stop)
		return server
	}
	t.Fatalf("dolt sql-server lost its probed port to another process on %d attempts in a row", sqlServerMaxAttempts)
	return nil
}

// probePort returns a port that is free on host right now, by binding it and
// letting go. The port can be taken again before the caller uses it.
func probePort(host string) (int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// startSQLServerOnPort starts dolt on port and waits for it to hold the port.
// It reports lostPort when dolt exited because another process held the port;
// every other failure ends the test.
func startSQLServerOnPort(t testing.TB, spec SQLServerSpec, host, logPath string, port int) (server *SQLServer, lostPort bool) {
	t.Helper()
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating dolt sql-server log: %v", err)
	}
	cmd := exec.Command(spec.Dolt, spec.Args(port)...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.SysProcAttr = spec.SysProcAttr
	cmd.Stdout, cmd.Stderr = logFile, logFile
	startErr := cmd.Start()
	// The child has its own descriptor; this one is not needed either way.
	_ = logFile.Close()
	if startErr != nil {
		t.Fatalf("starting %s: %v", cmd.String(), startErr)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline := time.Now().Add(sqlServerAttemptTimeout)
	for {
		select {
		case <-exited:
			output := readServerLog(logPath)
			if strings.Contains(strings.ToLower(output), sqlServerPortTaken) {
				return nil, true
			}
			t.Fatalf("dolt sql-server exited before it listened on %s: %v\n%s", addr, waitErr, tailLines(output, sqlServerLogTailLines))
		default:
		}
		if ownsPort(host, port, cmd.Process.Pid, exited) {
			return &SQLServer{Port: port, PID: cmd.Process.Pid, Process: cmd.Process, Exited: exited, LogPath: logPath}, false
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-exited
			t.Fatalf("dolt sql-server did not listen on %s within %s\n%s", addr, sqlServerAttemptTimeout, tailLines(readServerLog(logPath), sqlServerLogTailLines))
		}
		select {
		case <-exited:
		case <-time.After(sqlServerPollInterval):
		}
	}
}

// ownsPort reports whether process pid holds a listening socket on port.
//
// Where the kernel's socket tables are readable that is exact. Elsewhere
// (darwin) a dial that succeeds says only that something listens, so the
// process must also still be running once sqlServerSettle has passed: a dolt
// that lost the port exits well inside that window.
func ownsPort(host string, port, pid int, exited <-chan struct{}) bool {
	holder, readable := listenerPID(port, pid)
	if readable {
		return holder == pid
	}
	if host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	settle := time.NewTimer(sqlServerSettle)
	defer settle.Stop()
	select {
	case <-exited:
		return false
	case <-settle.C:
		return true
	}
}

// readServerLog returns the server log's contents, or a note on why it could
// not be read.
func readServerLog(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(dolt sql-server log unreadable: %v)", err)
	}
	return string(data)
}

// tailLines returns the last n lines of text.
func tailLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
