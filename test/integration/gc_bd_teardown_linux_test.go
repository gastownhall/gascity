//go:build integration && linux

package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestGcBdFixtureStopsCancelledOrderWriters guards the fixture boundary that
// precedes TempDir removal. A natural canceled exec-order outcome write must
// not outlive the supervisor, including through the file-store bd shim.
func TestGcBdFixtureStopsCancelledOrderWriters(t *testing.T) {
	requireDoltIntegration(t)
	root := t.TempDir()
	orderBarrier := newGCBdTeardownBarrier(t)
	writerBarrier := newGCBdTeardownBarrier(t)

	// Wrap the real bd below filebdshim, exactly where an in-flight client can
	// escape the supervisor's parent-death signal. Blocking on a socket changes
	// only the release time of the natural outcome write; no fake writer, sleep,
	// runtime operation or latency-budget change is involved.
	originalBD := realBDBinary
	defer func() { realBDBinary = originalBD }()
	wrapper := filepath.Join(root, "delayed-bd")
	realBDBinary = wrapper
	if err := writeExecShim(wrapper, originalBD); err != nil {
		t.Fatal(err)
	}
	env := newIsolatedCommandEnv(t, true)
	// buildIntegrationEnv has captured the wrapper for this supervisor only.
	realBDBinary = originalBD

	var cityDir, rigDir string
	var orderConn net.Conn
	orderStarted := false
	// Registered before the fixture cleanups, after root's TempDir cleanup:
	// this assertion runs after city/supervisor/Dolt teardown, BEFORE RemoveAll.
	t.Cleanup(func() {
		if orderConn != nil {
			defer func() {
				if err := orderConn.Close(); err != nil {
					t.Errorf("closing order barrier: %v", err)
				}
			}()
		}
		if !orderStarted {
			return // setup already reported the missing precondition
		}
		writer, identity := writerBarrier.accept(t)
		defer func() {
			if err := writer.Close(); err != nil {
				t.Errorf("closing outcome writer barrier: %v", err)
			}
		}()
		t.Logf("canceled supervisor outcome write at teardown: %s", identity)
		fields := strings.Fields(identity)
		if len(fields) < 4 || fields[0] != "writer" {
			t.Fatalf("invalid outcome-write barrier identity: %q", identity)
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatal(err)
		}
		if proc, ok := readProcessSnapshot()[pid]; ok {
			cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
			if err != nil || !pathWithinIntegrationRoot(cityDir, cwd) {
				t.Fatalf("held writer ownership: pid=%d cwd=%q err=%v", pid, cwd, err)
			}
			t.Errorf("owned outcome writer survived fixture teardown before TempDir removal: pid=%d ppid=%d cwd=%s cmd=%s", pid, proc.ppid, cwd, proc.cmd)
		}

		// If teardown killed the client, its socket is already at EOF. Otherwise
		// release the real write and await completion, exposing any proxy/Dolt
		// resurrection while the fixture directories still exist.
		if _, err := io.WriteString(writer, "release\n"); err != nil {
			t.Logf("outcome writer socket already closed: %v", err)
		}
		if _, err := io.Copy(io.Discard, writer); err != nil {
			t.Errorf("waiting for outcome writer completion: %v", err)
		}
		owned, survivors := gcBdTeardownOwnedProcesses(t, cityDir, rigDir)
		// Rescue cleanup runs only after the assertions capture a broken fixture.
		// It uses the existing terminator and only the observed owned processes.
		defer terminateIntegrationPIDs(owned)
		if len(survivors) > 0 {
			t.Errorf("owned processes remain before TempDir removal: %s", strings.Join(survivors, "; "))
		}
		// An entry created after the fixture's two bd dolt stops proves a live
		// writer, independently of the much narrower ENOTEMPTY removal race.
		if _, err := os.Stat(filepath.Join(cityDir, ".beads", "dolt", "proxy.secret")); err == nil {
			t.Error("owned bd proxy can write after fixture teardown, before TempDir removal")
		} else if !os.IsNotExist(err) {
			t.Errorf("checking retired proxy: %v", err)
		}
	})
	cityDir, rigDir = newGCBdLatencyFixture(t, env, root)

	// A real supervisor-dispatched order remains in flight until city stop
	// cancels it. Its tracking bead and exec-failed outcome use the real store.
	orderCommand := fmt.Sprintf("bash -c %s", singleQuoteShell(fmt.Sprintf(
		"exec 9<>/dev/tcp/%s; printf 'order started\\n' >&9; read -r -u 9 release", orderBarrier.address())))
	ordersDir := filepath.Join(cityDir, "orders")
	if err := os.MkdirAll(ordersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	order := fmt.Sprintf("[order]\ntrigger = \"cooldown\"\ninterval = \"24h\"\nexec = %q\n", orderCommand)
	if err := os.WriteFile(filepath.Join(ordersDir, "teardown-writer.toml"), []byte(order), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runGCDoltWithEnv(env, "", "start", cityDir); err != nil {
		t.Fatalf("reconcile order fixture: %v\n%s", err, out)
	}
	var ready string
	orderConn, ready = orderBarrier.accept(t)
	if ready != "order started" {
		t.Fatalf("order precondition: %q", ready)
	}
	// Arm only this order's outcome write. Other canceled orders keep their
	// normal timing, even if a shared worker delays teardown until a patrol.
	out, err := runGCDoltWithEnv(env, cityDir, "order", "history", "teardown-writer", "--json")
	if err != nil {
		t.Fatalf("reading order tracking bead: %v\n%s", err, out)
	}
	var history struct {
		Entries []struct {
			BeadID string `json:"bead_id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(extractJSONPayload(out)), &history); err != nil || len(history.Entries) != 1 || history.Entries[0].BeadID == "" {
		t.Fatalf("expected one real order tracking bead: %v\n%s", err, out)
	}
	trackingID := history.Entries[0].BeadID
	t.Logf("in-flight supervisor order tracking bead: %s", trackingID)
	script := fmt.Sprintf(`#!/bin/bash
set -eu
if [[ "$PWD" == %s && "$*" == %s ]]; then
  parent=()
  mapfile -d '' -t parent < "/proc/$PPID/cmdline" || parent=(exited)
  exec 9<>/dev/tcp/%s
  printf 'writer %%s %%s %%s parent=%%s\n' "$$" "$PPID" "$*" "${parent[*]}" >&9
  read -r -u 9 release
fi
exec %s "$@"
`, singleQuoteShell(cityDir), singleQuoteShell("update --json "+trackingID+" --add-label exec-failed"), writerBarrier.address(), singleQuoteShell(originalBD))
	if err := os.WriteFile(wrapper+".new", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(wrapper+".new", wrapper); err != nil {
		t.Fatal(err)
	}
	orderStarted = true
}

// gcBdTeardownBarrier synchronizes actual child processes without polling or
// persisted process-state files. The existing lifecycle timeout is a hang
// detector; socket messages determine progress.
type gcBdTeardownBarrier struct{ listener *net.TCPListener }

func newGCBdTeardownBarrier(t *testing.T) *gcBdTeardownBarrier {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("closing teardown listener: %v", err)
		}
	})
	return &gcBdTeardownBarrier{listener: listener}
}

func (b *gcBdTeardownBarrier) address() string {
	return strings.ReplaceAll(b.listener.Addr().String(), ":", "/")
}

func (b *gcBdTeardownBarrier) accept(t *testing.T) (net.Conn, string) {
	t.Helper()
	if err := b.listener.SetDeadline(time.Now().Add(integrationGCDoltCommandTimeout)); err != nil {
		t.Fatal(err)
	}
	conn, err := b.listener.AcceptTCP()
	if err != nil {
		t.Fatalf("child did not reach teardown barrier: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(integrationGCDoltCommandTimeout)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading child barrier: %v", err)
	}
	return conn, strings.TrimSpace(line)
}

func gcBdTeardownOwnedProcesses(t *testing.T, roots ...string) (map[int]bool, []string) {
	t.Helper()
	procs := readProcessSnapshot()
	if procs == nil {
		t.Fatal("cannot read process table before TempDir removal")
	}
	owned := make(map[int]bool)
	var survivors []string
	for pid, proc := range procs {
		cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		for _, root := range roots {
			if pathWithinIntegrationRoot(root, cwd) || strings.Contains(proc.cmd, root+string(os.PathSeparator)) {
				owned[pid] = true
				survivors = append(survivors, fmt.Sprintf("pid=%d ppid=%d cwd=%s cmd=%s", pid, proc.ppid, cwd, proc.cmd))
				break
			}
		}
	}
	slices.Sort(survivors)
	return owned, survivors
}
