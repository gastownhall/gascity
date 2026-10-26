package dolt_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// procTCPRow is one row of a fake /proc/net/tcp or /proc/net/tcp6 table.
type procTCPRow struct {
	table string // "tcp" or "tcp6"
	port  int
	state string // kernel TCP state in hex; "0A" is LISTEN
	inode string
}

// writeFakeProcRoot builds a directory shaped like the parts of /proc that
// runtime.sh reads to find a listener: net/tcp and net/tcp6 (kernel header
// plus rows) and <pid>/fd/<n> symlinks whose targets come from fds. Both
// tables are always written, so a nil rows argument gives a readable root
// with no listeners.
func writeFakeProcRoot(t *testing.T, rows []procTCPRow, fds map[string][]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	tables := map[string]*strings.Builder{"tcp": {}, "tcp6": {}}
	for _, b := range tables {
		b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	}
	for i, row := range rows {
		b, ok := tables[row.table]
		if !ok {
			t.Fatalf("procTCPRow table %q, want tcp or tcp6", row.table)
		}
		local := "0100007F"
		if row.table == "tcp6" {
			local = "00000000000000000000000001000000"
		}
		fmt.Fprintf(b, "%4d: %s:%04X %s:0000 %s 00000000:00000000 00:00000000 00000000  1000        0 %s 1 0000000000000000 100 0 0 10 0\n",
			i, local, row.port, strings.Repeat("0", len(local)), row.state, row.inode)
	}
	for name, b := range tables {
		if err := os.WriteFile(filepath.Join(root, "net", name), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for pid, targets := range fds {
		fdDir := filepath.Join(root, pid, "fd")
		if err := os.MkdirAll(fdDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, target := range targets {
			if err := os.Symlink(target, filepath.Join(fdDir, strconv.Itoa(n))); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// writeFakeProcListeners builds a fake proc root in which each port is a
// LISTEN socket held by the mapped pid, alongside non-socket fds.
func writeFakeProcListeners(t *testing.T, portPIDs map[string]string) string {
	t.Helper()
	ports := make([]string, 0, len(portPIDs))
	for port := range portPIDs {
		ports = append(ports, port)
	}
	sort.Strings(ports)
	var rows []procTCPRow
	fds := map[string][]string{}
	for i, port := range ports {
		portNum, err := strconv.Atoi(port)
		if err != nil {
			t.Fatalf("fake listener port %q: %v", port, err)
		}
		inode := strconv.Itoa(910000 + i)
		rows = append(rows, procTCPRow{table: "tcp", port: portNum, state: "0A", inode: inode})
		pid := portPIDs[port]
		fds[pid] = append(fds[pid], "/dev/null", "socket:["+inode+"]")
	}
	return writeFakeProcRoot(t, rows, fds)
}

// writeLsofTripwire puts an lsof on a fresh PATH directory that records each
// invocation in the returned marker file and prints nothing.
func writeLsofTripwire(t *testing.T) (binDir, marker string) {
	t.Helper()
	binDir = t.TempDir()
	marker = filepath.Join(t.TempDir(), "lsof-ran")
	writeExecutable(t, filepath.Join(binDir, "lsof"),
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+shellQuote(marker)+"\nexit 0\n")
	return binDir, marker
}

func assertLsofNotRun(t *testing.T, marker string) {
	t.Helper()
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("lsof ran while the proc TCP tables were readable:\n%s", data)
	} else if !os.IsNotExist(err) {
		t.Fatalf("read lsof marker: %v", err)
	}
}

// TestManagedRuntimePortReadsRealProcWithoutLsof guards gascity#7553: lsof
// stat()s every open fd target, so a stalled FUSE mount anywhere on the host
// parks it in uninterruptible sleep and the dolt orders that resolve the
// managed port hang past their timeout. Port resolution reads /proc instead.
func TestManagedRuntimePortReadsRealProcWithoutLsof(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs listener lookup is Linux-only")
	}
	tests := []struct {
		name       string
		statePID   int
		closeFirst bool
		wantExit78 bool
	}{
		{name: "state pid holds the listener", statePID: os.Getpid()},
		{name: "another live process holds the listener", statePID: os.Getppid(), wantExit78: true},
		{name: "no listener and port unreachable", statePID: os.Getpid(), closeFirst: true, wantExit78: true},
	}

	root := repoRoot(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			fakeBin, marker := writeLsofTripwire(t)
			writeExecutable(t, filepath.Join(fakeBin, "nc"), "#!/bin/sh\nexit 1\n")

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("Listen: %v", err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			port := listener.Addr().(*net.TCPAddr).Port
			writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, tt.statePID)
			if tt.closeFirst {
				_ = listener.Close()
			}

			cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; printf '%s\n' "$GC_DOLT_PORT"`)
			cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST", "GC_DOLT_PROC_ROOT", "PATH"),
				"GC_CITY_PATH="+cityPath,
				"GC_PACK_DIR="+root,
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			out, err := cmd.CombinedOutput()
			assertLsofNotRun(t, marker)
			if tt.wantExit78 {
				assertRuntimePortExit78(t, err, out, filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json"), cityPath)
				return
			}
			if err != nil {
				t.Fatalf("runtime.sh failed: %v\n%s", err, out)
			}
			if got, want := strings.TrimSpace(string(out)), strconv.Itoa(port); got != want {
				t.Fatalf("GC_DOLT_PORT = %q, want %q", got, want)
			}
		})
	}
}

// lsofFallbackProcRootEnv points runtime.sh at a proc root with no TCP
// tables, pinning managed_runtime_listener_pid to its lsof fallback so a
// fake lsof on PATH decides the listener.
func lsofFallbackProcRootEnv(t *testing.T) string {
	t.Helper()
	return "GC_DOLT_PROC_ROOT=" + filepath.Join(t.TempDir(), "missing")
}
