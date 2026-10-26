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

// TestManagedRuntimeListenerPIDReadsProcTables pins how the procfs lookup
// picks the listener: the first candidate holding a LISTEN socket for the
// port, otherwise the lowest holding pid, and nothing when no LISTEN row
// matches. lsof never runs while the tables are readable.
func TestManagedRuntimeListenerPIDReadsProcTables(t *testing.T) {
	const port = 4406
	listen := func(table, inode string) procTCPRow {
		return procTCPRow{table: table, port: port, state: "0A", inode: inode}
	}
	spoofed := []string{
		"/tmp/x 9 -> socket:[7001]",
		"/tmp/a\nlrwx------ 1 0 0 64 Oct 26 12:00 9 -> socket:[7001]",
	}
	tests := []struct {
		name string
		rows []procTCPRow
		fds  map[string][]string
		args []string
		want string
	}{
		{
			name: "candidate holding the socket wins over a lower holder",
			rows: []procTCPRow{listen("tcp", "7001")},
			fds:  map[string][]string{"200": {"socket:[7001]"}, "300": {"/dev/null", "socket:[7001]"}},
			args: []string{"300"},
			want: "300",
		},
		{
			name: "candidate holding nothing falls back to the lowest holder",
			rows: []procTCPRow{listen("tcp", "7001")},
			fds:  map[string][]string{"150": {"/dev/null"}, "200": {"socket:[7001]"}, "300": {"socket:[7001]"}},
			args: []string{"150"},
			want: "200",
		},
		{
			name: "lowest of several holders by number",
			rows: []procTCPRow{listen("tcp", "7001"), listen("tcp", "7002")},
			fds:  map[string][]string{"1000": {"socket:[7001]"}, "999": {"/dev/null", "socket:[7002]"}},
			want: "999",
		},
		{
			name: "listener only in tcp6",
			rows: []procTCPRow{listen("tcp6", "7001")},
			fds:  map[string][]string{"4242": {"/dev/null", "socket:[7001]"}},
			want: "4242",
		},
		{
			name: "non-listen row and zero inode match nothing",
			rows: []procTCPRow{
				{table: "tcp", port: port, state: "01", inode: "7001"},
				{table: "tcp6", port: port, state: "0A", inode: "0"},
			},
			fds: map[string][]string{"200": {"socket:[7001]", "socket:[0]"}},
		},
		{
			name: "listeners only on other ports",
			rows: []procTCPRow{{table: "tcp", port: port + 1, state: "0A", inode: "7001"}},
			fds:  map[string][]string{"200": {"socket:[7001]"}},
		},
		{
			name: "spoofed fd target names are not holders",
			rows: []procTCPRow{listen("tcp", "7001")},
			fds:  map[string][]string{"100": spoofed, "500": {"socket:[7001]"}},
			want: "500",
		},
		{
			name: "spoofed candidate is not a holder",
			rows: []procTCPRow{listen("tcp", "7001")},
			fds:  map[string][]string{"100": spoofed, "500": {"socket:[7001]"}},
			args: []string{"100"},
			want: "500",
		},
	}

	root := repoRoot(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			procRoot := writeFakeProcRoot(t, tt.rows, tt.fds)
			fakeBin, marker := writeLsofTripwire(t)
			cmd := exec.Command("sh", append([]string{
				"-c",
				`. "$GC_PACK_DIR/assets/scripts/runtime.sh"; managed_runtime_listener_pid "$@"`,
				"sh", strconv.Itoa(port),
			}, tt.args...)...)
			cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST", "GC_DOLT_PROC_ROOT", "PATH"),
				"GC_CITY_PATH="+t.TempDir(),
				"GC_PACK_DIR="+root,
				"GC_DOLT_PORT="+strconv.Itoa(port),
				"GC_DOLT_PROC_ROOT="+procRoot,
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("managed_runtime_listener_pid: %v\n%s", err, out)
			}
			assertLsofNotRun(t, marker)
			if got := strings.TrimSpace(string(out)); got != tt.want {
				t.Fatalf("managed_runtime_listener_pid %d %v = %q, want %q", port, tt.args, got, tt.want)
			}
		})
	}
}

// TestManagedRuntimeListenerPIDFallsBackToLsofWithoutProcTables pins the
// path for hosts without procfs TCP tables (darwin): lsof answers, and its
// first pid that pid_is_running confirms is the listener.
func TestManagedRuntimeListenerPIDFallsBackToLsofWithoutProcTables(t *testing.T) {
	fakeBin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "lsof-ran")
	writeExecutable(t, filepath.Join(fakeBin, "lsof"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
echo not-a-pid
echo %d
`, shellQuote(marker), os.Getpid()))

	root := repoRoot(t)
	cmd := exec.Command("sh", "-c",
		`. "$GC_PACK_DIR/assets/scripts/runtime.sh"; managed_runtime_listener_pid 4406`)
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST", "GC_DOLT_PROC_ROOT", "PATH"),
		"GC_CITY_PATH="+t.TempDir(),
		"GC_PACK_DIR="+root,
		"GC_DOLT_PORT=4406",
		"GC_DOLT_PROC_ROOT="+t.TempDir(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("managed_runtime_listener_pid: %v\n%s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("managed_runtime_listener_pid = %q, want lsof's live pid %q", got, want)
	}
	args, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("lsof did not run without proc TCP tables: %v", err)
	}
	if got, want := strings.TrimSpace(string(args)), "-nP -t -iTCP:4406 -sTCP:LISTEN"; got != want {
		t.Fatalf("lsof args = %q, want %q", got, want)
	}
}
