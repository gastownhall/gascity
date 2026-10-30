//go:build unix

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// holdGcBeadsBdInitLockForTest takes database db's init lock in lockDir the
// way a concurrent initializer's op_init does, shared (syscall.LOCK_SH) or
// exclusive (syscall.LOCK_EX), and holds it until the test ends.
func holdGcBeadsBdInitLockForTest(t *testing.T, lockDir, db string, how int) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(lockDir, db+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold init lock: %v", err)
	}
}

// TestGcBeadsBdInitNeverHealsWhileAnotherInitializerHoldsTheLock pins the
// exclusion half of the heal. A concurrent initializer between one migration
// step's DDL and its commit shows exactly the signature the heal matches (a
// dirty working set, no issues), so the reset only ever runs under the
// database's init lock held exclusively. While another initializer holds that
// lock, migrating (shared) or forcing a reinit (exclusive), init must give up
// with the lock error instead of resetting the database or running bd against
// it, on both paths the heal runs on: a present schema (the ready path) and a
// missing one (before a forced reinit). Init gets no wait budget, under a
// flock that refuses a zero wait the way the macOS port does.
func TestGcBeadsBdInitNeverHealsWhileAnotherInitializerHoldsTheLock(t *testing.T) {
	hostFlock := requireFlockForTest(t)
	for _, tc := range []struct {
		name    string
		markers []string
		how     int
	}{
		{"schema present, initializer migrating", []string{"init-done"}, syscall.LOCK_SH},
		{"schema present, initializer forcing a reinit", []string{"init-done"}, syscall.LOCK_EX},
		{"schema missing, initializer migrating", nil, syscall.LOCK_SH},
		{"schema missing, initializer forcing a reinit", nil, syscall.LOCK_EX},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGcBeadsBdHealTest(t, true, 0, tc.markers...)
			installMacOSFlockForTest(t, h.binDir, hostFlock)
			lockDir := t.TempDir()
			holdGcBeadsBdInitLockForTest(t, lockDir, "hq", tc.how)

			out, err := h.runInit(t, "GC_DOLT_INIT_LOCK_DIR="+lockDir, "GC_DOLT_INIT_LOCK_TIMEOUT_MS=0")
			if !failedOnHeldInitLock(out, err) {
				t.Fatalf("expected init to fail on the held init lock, got err=%v:\n%s", err, out)
			}
			sql, bdArgs := h.logs(t)
			if strings.Contains(sql, "DOLT_RESET") {
				t.Fatalf("reset a database while another initializer held its lock:\n%s", sql)
			}
			if strings.Contains(bdArgs, "migrate") || strings.Contains(bdArgs, "init ") {
				t.Fatalf("ran bd against a database while another initializer held its lock:\n%s", bdArgs)
			}
		})
	}
}

// TestGcBeadsBdInitMigrationStepsHoldTheInitLockShared pins the lock mode of
// init's two migration steps, `bd migrate schema` on the ready path and the
// plain `bd init`: each runs alongside another initializer's migration (both
// hold the lock shared, so migrations never queue behind one another, as they
// did not before the lock existed), and neither runs while another initializer
// holds the lock exclusively to reset the database or force a reinit. Init
// gets no wait budget, under a flock that refuses a zero wait the way the
// macOS port does.
func TestGcBeadsBdInitMigrationStepsHoldTheInitLockShared(t *testing.T) {
	hostFlock := requireFlockForTest(t)
	for _, tc := range []struct {
		name         string
		withMetadata bool
		markers      []string
		step         string
		how          int
	}{
		{"migrate schema alongside a migrating initializer", true, []string{"init-done", "reset-done"}, "migrate schema", syscall.LOCK_SH},
		{"migrate schema during an exclusive hold", true, []string{"init-done", "reset-done"}, "migrate schema", syscall.LOCK_EX},
		{"plain init alongside a migrating initializer", false, []string{"reset-done"}, "init ", syscall.LOCK_SH},
		{"plain init during an exclusive hold", false, []string{"reset-done"}, "init ", syscall.LOCK_EX},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGcBeadsBdHealTest(t, tc.withMetadata, 0, tc.markers...)
			installMacOSFlockForTest(t, h.binDir, hostFlock)
			lockDir := t.TempDir()
			holdGcBeadsBdInitLockForTest(t, lockDir, "hq", tc.how)

			out, err := h.runInit(t, "GC_DOLT_INIT_LOCK_DIR="+lockDir, "GC_DOLT_INIT_LOCK_TIMEOUT_MS=0")
			_, bdArgs := h.logs(t)
			if tc.how == syscall.LOCK_SH {
				if err != nil {
					t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
				}
				if !strings.Contains(bdArgs, tc.step) {
					t.Fatalf("expected bd %s, got:\n%s", tc.step, bdArgs)
				}
				return
			}
			if !failedOnHeldInitLock(out, err) {
				t.Fatalf("expected init to fail on the held init lock, got err=%v:\n%s", err, out)
			}
			if strings.Contains(bdArgs, tc.step) {
				t.Fatalf("bd %s ran while another initializer held the lock exclusively:\n%s", tc.step, bdArgs)
			}
		})
	}
}

func TestGcBeadsBdStartRestartsServerHoldingDeletedDataInodes(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	countFile := filepath.Join(t.TempDir(), "dolt-start-count")
	deletedMarkerFile := filepath.Join(t.TempDir(), "deleted-inode-held")
	fakeDolt := filepath.Join(binDir, "dolt")
	port := freeLoopbackPort(t)
	fakeScript := fmt.Sprintf(`#!/bin/sh
set -eu
count_file=%q
data_dir=%q
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    count=0
    if [ -f "$count_file" ]; then
      count=$(cat "$count_file")
    fi
    count=$((count + 1))
    printf '%%s\n' "$count" > "$count_file"
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    exec python3 - "$port" "$data_dir" %q <<'INNERPY'
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
data_dir = sys.argv[2]
marker_path = sys.argv[3]
os.makedirs(data_dir, exist_ok=True)
os.chdir(data_dir)
open_file = None
if not os.path.exists(marker_path):
    with open(marker_path, "w") as marker:
        marker.write("held")
    stale = os.path.join(data_dir, "stale-open.txt")
    open_file = open(stale, "w+")
    open_file.write("stale")
    open_file.flush()
    os.unlink(stale)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
	`, countFile, filepath.Join(cityPath, ".beads", "dolt"), deletedMarkerFile)
	if err := os.WriteFile(fakeDolt, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_DOLT_PORT="+port,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)

	runStart := func() {
		t.Helper()
		cmd := exec.Command(script, "start")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
		}
	}

	runStart()
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
	})

	firstPIDData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.pid"))
	if err != nil {
		t.Fatalf("read first pid file: %v", err)
	}
	firstPID := strings.TrimSpace(string(firstPIDData))
	if firstPID == "" {
		t.Fatal("first pid file is empty")
	}
	initialStartCount := readDoltStartCountForTest(t, countFile)

	runStart()

	secondPIDData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.pid"))
	if err != nil {
		t.Fatalf("read second pid file: %v", err)
	}
	secondPID := strings.TrimSpace(string(secondPIDData))
	if secondPID == "" {
		t.Fatal("second pid file is empty")
	}

	if got := readDoltStartCountForTest(t, countFile); got <= initialStartCount {
		t.Fatalf("dolt sql-server launch count = %d, want greater than initial %d", got, initialStartCount)
	}

	// The relaunch only counts as a restart if the stale server is gone:
	// otherwise the new one would be fighting it for the port. Stopping an
	// owned server that holds deleted inodes is the one thing start does to
	// a port holder, so pin it separately from the launch count.
	stalePID, err := strconv.Atoi(firstPID)
	if err != nil {
		t.Fatalf("parse first pid %q: %v", firstPID, err)
	}
	if !waitForProcessExit(stalePID, 5*time.Second) {
		t.Fatalf("stale dolt server (pid %d) is still running after start restarted it", stalePID)
	}
	if secondPID == firstPID {
		t.Fatalf("pid file still names the stale server (pid %s) after the restart", firstPID)
	}
}
