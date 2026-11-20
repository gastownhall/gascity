package main

// Shared socket-level test helpers for the containment-lease fetch tests
// (T2 garbage replies, T4 fetch-counting, T7 hung listeners). Each helper
// speaks the raw controller socket protocol directly — never through
// registerResidencyRoutes/handleControllerConn — so these tests exercise
// exactly what fetchContainmentLease sends and tolerates, not the
// controller's own dispatch.

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// listenControllerSocketForTest opens the real controller socket path for
// cityDir and returns the listener; the caller decides what, if anything,
// to reply with. The listener is closed on test cleanup.
func listenControllerSocketForTest(t *testing.T, cityDir string) net.Listener {
	t.Helper()
	sockPath := controllerSocketPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(socket dir): %v", err)
	}
	_ = os.Remove(sockPath)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%s): %v", sockPath, err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	return lis
}

// acceptOnceAndReply accepts exactly one connection, reads its one-line
// command, and writes reply back verbatim (the caller supplies any
// trailing newline). It is meant to run in its own goroutine; errors from
// a listener closed by the test's cleanup are swallowed, not failed, since
// shutdown races the goroutine by design.
func acceptOnceAndReply(t *testing.T, lis net.Listener, reply string) {
	t.Helper()
	conn, err := lis.Accept()
	if err != nil {
		return
	}
	defer conn.Close() //nolint:errcheck
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	_, _ = conn.Write([]byte(reply))
}

// countingControllerSocket serves cityDir's controller socket and counts
// every connection whose one-line command is storageVerdictCommand,
// closing every connection without a reply either way. T4 (design doc
// §4.4/§4.5): fetchContainmentLease is reached only from
// confirmInfraConvergenceWith when opts.lease is set, which only
// cli_storage_routes.go's two funnel call sites ever do — so this counter
// must stay at zero for every other production entry point, whether or not
// it would otherwise want a verdict.
//
// The count is specifically of storage-verdict commands, not of
// connections: several of T4's production entry points legitimately ping
// this same socket for an unrelated reason (controllerAlive, checking
// whether a controller is running at all), and a bare connection count
// would conflate that with the lease fetch this test is about.
func countingControllerSocket(t *testing.T, cityDir string) *atomic.Int32 {
	t.Helper()
	lis := listenControllerSocketForTest(t, cityDir)
	var count atomic.Int32
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() //nolint:errcheck
				scanner := bufio.NewScanner(conn)
				if scanner.Scan() && scanner.Text() == storageVerdictCommand {
					count.Add(1)
				}
			}()
		}
	}()
	return &count
}
