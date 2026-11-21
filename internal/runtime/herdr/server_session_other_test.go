//go:build !windows

package herdr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// herdr's remote attach treats a server as surviving SSH loss only when it
// leads its own Unix session (getsid(0) == getpid(), herdr
// src/platform/mod.rs current_process_is_detached_server_daemon). A server
// left in gc's session reports detached_server_daemon=false, and every
// `herdr --remote <host> --session <city>` asks to restart it, which kills
// the city's panes. startServer must therefore launch the server as the
// leader of a new session.
func TestStartServerLaunchesServerAsSessionLeader(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "server.pid")
	bin := filepath.Join(dir, "herdr")
	// The fake server records its PID, then execs sleep so that PID stays
	// alive for Getsid; the test kills it at cleanup.
	script := "#!/bin/sh\n" +
		"[ \"$3\" = server ] || exit 0\n" +
		"printf '%s\\n' \"$$\" > '" + pidFile + ".tmp'\n" +
		"mv -f '" + pidFile + ".tmp' '" + pidFile + "'\n" +
		"exec sleep 60\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake herdr: %v", err)
	}
	readPID := func() (int, error) {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(strings.TrimSpace(string(b)))
	}

	// Kill the fake server on every exit path, including a startServer
	// failure after launch.
	t.Cleanup(func() {
		if pid, err := readPID(); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	// The connect reads as live once the fake server has recorded its PID,
	// so startServer's readiness wait ends on the launch it made.
	c := &client{session: "sidtest", sockPath: filepath.Join(dir, "herdr.sock"), bin: bin}
	c.dialUnix = func(context.Context, string) (net.Conn, error) {
		if _, err := readPID(); err != nil {
			return nil, errors.New("nothing is listening")
		}
		server, conn := net.Pipe()
		_ = server.Close()
		return conn, nil
	}

	if err := c.startServer(); err != nil {
		t.Fatalf("startServer() = %v; want nil", err)
	}
	pid, err := readPID()
	if err != nil {
		t.Fatalf("reading fake herdr server pid: %v", err)
	}

	sid, err := unix.Getsid(pid)
	if err != nil {
		t.Fatalf("Getsid(%d): %v", pid, err)
	}
	if sid != pid {
		t.Errorf("herdr server pid %d is in session %d; want it to lead its own session (sid == pid)", pid, sid)
	}
	if own, err := unix.Getsid(0); err == nil && own == sid {
		t.Errorf("herdr server shares the test process's session %d; want a new session", sid)
	}
}
