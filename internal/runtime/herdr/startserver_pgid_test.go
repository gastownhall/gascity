//go:build !windows

package herdr

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pgidHelperSockEnv and pgidHelperOutEnv carry the unix socket the helper
// process binds and the file it records "<pid> <pgid>" in. Setting the socket
// variable is what turns TestHerdrServerPgidHelperProcess from a no-op into the
// stand-in server.
const (
	pgidHelperSockEnv = "GC_TEST_HERDR_PGID_HELPER_SOCK"
	pgidHelperOutEnv  = "GC_TEST_HERDR_PGID_HELPER_OUT"
)

// TestHerdrServerPgidHelperProcess is not a test: startServer launches the
// fake herdr CLI below, which re-executes this test binary into this function
// so the "server" binds a real unix socket (serverAlive dials for real) and
// reports the process group it was started in. Under a normal run the
// environment is unset and it returns immediately.
func TestHerdrServerPgidHelperProcess(_ *testing.T) {
	sock := os.Getenv(pgidHelperSockEnv)
	if sock == "" {
		return
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper listen: %v\n", err)
		os.Exit(2)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	pgid, err := syscall.Getpgid(0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper getpgid: %v\n", err)
		os.Exit(2)
	}
	out := os.Getenv(pgidHelperOutEnv)
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d %d", os.Getpid(), pgid)), 0o644); err != nil {
		os.Exit(2)
	}
	if err := os.Rename(tmp, out); err != nil {
		os.Exit(2)
	}
	// Reap ourselves if the test that launched us never gets to its cleanup.
	time.Sleep(time.Minute)
	os.Exit(0)
}

// startServer spawns the city's herdr server detached from gc. Release() only
// stops Go's bookkeeping; process-group membership is what decides which
// signals reach the server, and a server left in its launcher's group dies
// with it when that group is signaled (ga-mo0i1j: a deployer's suite run was
// SIGTERMed mid-test and took the shared herdr server with it). The server must
// lead a group of its own.
func TestStartServerLaunchesServerInItsOwnProcessGroup(t *testing.T) {
	shortHome(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "server.pgid")

	c := newClient("pgidtest", "")
	t.Setenv(pgidHelperSockEnv, c.socketPath())
	t.Setenv(pgidHelperOutEnv, out)
	if err := os.MkdirAll(filepath.Dir(c.socketPath()), 0o755); err != nil {
		t.Fatal(err)
	}

	// The fake herdr CLI: startServer runs `<bin> --session pgidtest server`.
	// XML_OUTPUT_FILE is unset so the re-executed test binary cannot overwrite
	// the real test's result file on its way out.
	c.bin = filepath.Join(dir, "herdr")
	script := "#!/bin/sh\nunset XML_OUTPUT_FILE\nexec " + sq(os.Args[0]) + " -test.run='^TestHerdrServerPgidHelperProcess$'\n"
	if err := os.WriteFile(c.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	serverPID := 0
	t.Cleanup(func() {
		// Kill by the PID the helper reported, never by name: sibling runs on
		// this host launch the same test binary.
		if serverPID > 1 {
			_ = syscall.Kill(serverPID, syscall.SIGKILL)
		}
	})

	if err := c.startServer(); err != nil {
		t.Fatalf("startServer: %v", err)
	}

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the stand-in server never reported its process group: %v", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		t.Fatalf("helper report %q, want \"<pid> <pgid>\"", b)
	}
	serverPID, _ = strconv.Atoi(fields[0])
	serverPGID, _ := strconv.Atoi(fields[1])

	if mine := syscall.Getpgrp(); serverPGID == mine {
		t.Fatalf("herdr server pgid = %d, same as the launching process: a signal to gc's process group kills the shared server", serverPGID)
	}
	if serverPGID != serverPID {
		t.Errorf("herdr server pgid = %d, want it to lead its own group (pid %d)", serverPGID, serverPID)
	}
}
