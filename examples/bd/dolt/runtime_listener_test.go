// Package dolt_test pins runtime.sh's listener-pid lookup to the kernel
// TCP tables. `lsof -nP -t -iTCP:<port> -sTCP:LISTEN` stats through every
// /proc/<pid>/fd link, so one stalled FUSE mount anywhere on the host parks
// it in uninterruptible sleep and every dolt order that resolves the port
// times out with an unkillable lsof behind it (gascity#7553). The shell
// must answer from $DOLT_PROC_ROOT/net/tcp{,6} plus the fd tables — the
// same algorithm as internal/pidutil.ListenerPID (#6702) — and keep lsof
// only for hosts without those tables.
package dolt_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// fakeProcRow is one /proc/net/tcp{,6} row written by writeFakeProcRoot,
// in the kernel field layout the lookup parses: local address in field 2
// (port as the hex after the last colon), state in field 4, inode in
// field 10.
type fakeProcRow struct {
	port  int
	state string
	inode string
	ipv6  bool
}

// fakeProcFD is one <pid>/fd/<fd> symlink written by writeFakeProcRoot.
// target "socket:[<inode>]" models a real holder; anything else is noise
// that must never count as one.
type fakeProcFD struct {
	pid    string
	fd     string
	target string
}

const fakeProcTCPHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// writeFakeProcRoot writes a self-contained process-filesystem tree under a
// fresh temp dir: both TCP tables (always present, header-only when rows is
// nil — readable tables with no LISTEN rows) and a <pid>/fd directory per
// referenced pid, each seeded with a /dev/null link as non-socket noise.
// Returns the root path.
func writeFakeProcRoot(t *testing.T, rows []fakeProcRow, fds []fakeProcFD) string {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatalf("mkdir net: %v", err)
	}

	var tcp4, tcp6 strings.Builder
	tcp4.WriteString(fakeProcTCPHeader)
	tcp6.WriteString(fakeProcTCPHeader)
	for i, row := range rows {
		local := fmt.Sprintf("0100007F:%04X", row.port)
		remote := "00000000:0000"
		if row.ipv6 {
			local = fmt.Sprintf("00000000000000000000000001000000:%04X", row.port)
			remote = "00000000000000000000000000000000:0000"
		}
		line := fmt.Sprintf("  %d: %s %s %s 00000000:00000000 00:00000000 00000000  1000        0 %s 1 0000000000000000 1 0 0 10 0\n",
			i, local, remote, row.state, row.inode)
		if row.ipv6 {
			tcp6.WriteString(line)
		} else {
			tcp4.WriteString(line)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(tcp4.String()), 0o644); err != nil {
		t.Fatalf("write net/tcp: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "net", "tcp6"), []byte(tcp6.String()), 0o644); err != nil {
		t.Fatalf("write net/tcp6: %v", err)
	}

	pids := map[string]struct{}{}
	for _, fd := range fds {
		pids[fd.pid] = struct{}{}
	}
	for pid := range pids {
		dir := filepath.Join(root, pid, "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s/fd: %v", pid, err)
		}
		if err := os.Symlink("/dev/null", filepath.Join(dir, "0")); err != nil {
			t.Fatalf("symlink noise fd for %s: %v", pid, err)
		}
	}
	for _, fd := range fds {
		if err := os.Symlink(fd.target, filepath.Join(root, fd.pid, "fd", fd.fd)); err != nil {
			t.Fatalf("symlink %s/fd/%s: %v", fd.pid, fd.fd, err)
		}
	}
	return root
}

// writeListenerProcRoot builds a fake proc root mapping each port to the
// pid holding its LISTEN socket — the port→pid maps the zombie-scan
// fixtures modeled with a fake lsof before the TCP-table switch (#7553).
func writeListenerProcRoot(t *testing.T, portPids map[int]string) string {
	t.Helper()
	var rows []fakeProcRow
	var fds []fakeProcFD
	inode := 800000
	for port, pid := range portPids {
		rows = append(rows, fakeProcRow{port: port, state: "0A", inode: strconv.Itoa(inode)})
		fds = append(fds, fakeProcFD{pid: pid, fd: "3", target: "socket:[" + strconv.Itoa(inode) + "]"})
		inode++
	}
	return writeFakeProcRoot(t, rows, fds)
}

// writeLsofTripwire puts a fake lsof in bin that records its invocation in
// a marker file and exits 0 with no output, standing in for a lsof that
// answers nothing. Procfs-path tests assert the marker stays absent: once
// the TCP tables are readable, lsof must never run (#7553). Returns the
// marker path.
func writeLsofTripwire(t *testing.T, bin string) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "lsof-ran")
	writeExecutable(t, filepath.Join(bin, "lsof"), `#!/bin/sh
touch `+shellQuote(marker)+`
exit 0
`)
	return marker
}

// assertLsofNeverRan fails when a writeLsofTripwire marker exists.
func assertLsofNeverRan(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("lsof ran; with readable TCP tables the listener lookup must answer from procfs alone")
	}
}

// overrideEnvEntry returns env with key's entry replaced by key=value,
// appending the entry when absent.
func overrideEnvEntry(env []string, key, value string) []string {
	entry := key + "=" + value
	for i, e := range env {
		if strings.HasPrefix(e, key+"=") {
			env[i] = entry
			return env
		}
	}
	return append(env, entry)
}

// listenLoopback binds 127.0.0.1:0 in this process and closes the listener
// on cleanup.
func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// runManagedRuntimeListenerPID sources runtime.sh (GC_DOLT_PORT seeded so
// source-time port resolution stays out of the way) and runs
// managed_runtime_listener_pid PORT CANDIDATE..., returning combined
// output. extraEnv is appended after the base env.
func runManagedRuntimeListenerPID(t *testing.T, port string, candidates []string, extraEnv ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	script := `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; managed_runtime_listener_pid "$@"`
	args := append([]string{"-c", script, "listener", port}, candidates...)
	cmd := exec.Command("sh", args...)
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "PATH"),
		"GC_CITY_PATH="+t.TempDir(),
		"GC_PACK_DIR="+root,
		"GC_DOLT_PORT=4406",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestManagedRuntimeListenerPIDReadsProcTables drives
// managed_runtime_listener_pid over a fake proc root. Every row asserts
// lsof (a tripwire on PATH) never runs: the answer comes from the TCP
// tables and fd links alone, never from an lsof that stats through fd
// links (#7553).
func TestManagedRuntimeListenerPIDReadsProcTables(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs fixture layout is linux-only; other hosts keep the lsof path")
	}

	tests := []struct {
		name       string
		rows       []fakeProcRow
		fds        []fakeProcFD
		port       string
		candidates []string
		want       string
	}{
		{
			name: "two holders print the lowest pid",
			rows: []fakeProcRow{{port: 24972, state: "0A", inode: "700001"}},
			fds: []fakeProcFD{
				{pid: "300", fd: "3", target: "socket:[700001]"},
				{pid: "500", fd: "4", target: "socket:[700001]"},
			},
			port: "24972",
			want: "300",
		},
		{
			name: "listen row in tcp6 still finds its holder",
			rows: []fakeProcRow{{port: 24973, state: "0A", inode: "700002", ipv6: true}},
			fds:  []fakeProcFD{{pid: "310", fd: "3", target: "socket:[700002]"}},
			port: "24973",
			want: "310",
		},
		{
			name: "established socket and inode-zero listen row are not matches",
			rows: []fakeProcRow{
				{port: 24974, state: "01", inode: "700003"},
				{port: 24974, state: "0A", inode: "0"},
			},
			fds:  []fakeProcFD{{pid: "320", fd: "3", target: "socket:[700003]"}},
			port: "24974",
			want: "",
		},
		{
			name: "listen rows for other ports are not matches",
			rows: []fakeProcRow{{port: 24975, state: "0A", inode: "700004"}},
			fds:  []fakeProcFD{{pid: "330", fd: "3", target: "socket:[700004]"}},
			port: "24976",
			want: "",
		},
		{
			name: "candidate holding the socket wins over a lower holder",
			rows: []fakeProcRow{{port: 24977, state: "0A", inode: "700005"}},
			fds: []fakeProcFD{
				{pid: "300", fd: "3", target: "socket:[700005]"},
				{pid: "500", fd: "3", target: "socket:[700005]"},
			},
			port:       "24977",
			candidates: []string{"500"},
			want:       "500",
		},
		{
			name: "candidate holding nothing falls back to the lowest holder",
			rows: []fakeProcRow{{port: 24978, state: "0A", inode: "700006"}},
			fds: []fakeProcFD{
				{pid: "300", fd: "3", target: "socket:[700006]"},
				{pid: "700", fd: "3", target: "/dev/null"},
			},
			port:       "24978",
			candidates: []string{"700"},
			want:       "300",
		},
		{
			name: "spoofed fd link targets never count as holders",
			rows: []fakeProcRow{{port: 24979, state: "0A", inode: "700007"}},
			fds: []fakeProcFD{
				{pid: "300", fd: "3", target: "socket:[700007]"},
				{pid: "400", fd: "9", target: "/tmp/x 9 -> socket:[700007]"},
				{pid: "400", fd: "8", target: "/tmp/a\nlrwx------ 1 0 0 64 Oct 26 12:00 9 -> socket:[700007]"},
			},
			port: "24979",
			want: "300",
		},
		{
			name: "spoofed candidate is rejected and the walk answers",
			rows: []fakeProcRow{{port: 24980, state: "0A", inode: "700008"}},
			fds: []fakeProcFD{
				{pid: "300", fd: "3", target: "socket:[700008]"},
				{pid: "400", fd: "9", target: "/tmp/x 9 -> socket:[700008]"},
				{pid: "400", fd: "8", target: "/tmp/a\nlrwx------ 1 0 0 64 Oct 26 12:00 9 -> socket:[700008]"},
			},
			port:       "24980",
			candidates: []string{"400"},
			want:       "300",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			procRoot := writeFakeProcRoot(t, tt.rows, tt.fds)
			fakeBin := t.TempDir()
			marker := writeLsofTripwire(t, fakeBin)

			got, err := runManagedRuntimeListenerPID(t, tt.port, tt.candidates,
				"GC_DOLT_PROC_ROOT="+procRoot,
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			if err != nil {
				t.Fatalf("managed_runtime_listener_pid: %v\noutput:\n%s", err, got)
			}
			if trimmed := strings.TrimSpace(got); trimmed != tt.want {
				t.Fatalf("managed_runtime_listener_pid %s = %q, want %q", tt.port, trimmed, tt.want)
			}
			assertLsofNeverRan(t, marker)
		})
	}
}

// TestManagedRuntimePortReadsRealProcWithoutLsof resolves the port through
// runtime.sh sourcing against the real /proc: managed_runtime_port must
// accept the state file's port when the state pid holds the listener, and
// reject it (resolve_dolt_port_or_die exit 78) when a different live
// process holds it or the listener is gone — with lsof never invoked.
func TestManagedRuntimePortReadsRealProcWithoutLsof(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads the real /proc TCP tables; linux-only")
	}
	root := repoRoot(t)

	runPortResolve := func(t *testing.T, cityPath string, extraFakes func(fakeBin string)) ([]byte, error) {
		t.Helper()
		fakeBin := t.TempDir()
		marker := writeLsofTripwire(t, fakeBin)
		if extraFakes != nil {
			extraFakes(fakeBin)
		}
		cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; printf '%s\n' "$GC_DOLT_PORT"`)
		cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST", "PATH"),
			"GC_CITY_PATH="+cityPath,
			"GC_PACK_DIR="+root,
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		)
		out, err := cmd.CombinedOutput()
		assertLsofNeverRan(t, marker)
		return out, err
	}

	t.Run("state pid holding the listener is accepted", func(t *testing.T) {
		listener := listenLoopback(t)
		port := listener.Addr().(*net.TCPAddr).Port
		cityPath := t.TempDir()
		writeManagedRuntimeStateForScript(t, cityPath, port)

		out, err := runPortResolve(t, cityPath, nil)
		if err != nil {
			t.Fatalf("runtime.sh failed: %v\n%s", err, out)
		}
		if got := strings.TrimSpace(string(out)); got != strconv.Itoa(port) {
			t.Fatalf("GC_DOLT_PORT = %q, want the listener port %d", got, port)
		}
	})

	t.Run("live pid that is not the holder rejects with exit 78", func(t *testing.T) {
		listener := listenLoopback(t)
		port := listener.Addr().(*net.TCPAddr).Port
		cityPath := t.TempDir()
		writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, os.Getppid())

		out, err := runPortResolve(t, cityPath, nil)
		assertRuntimePortExit78(t, err, out, filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json"), cityPath)
	})

	t.Run("closed listener with failing nc rejects with exit 78", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if err := listener.Close(); err != nil {
			t.Fatalf("close listener: %v", err)
		}
		cityPath := t.TempDir()
		writeManagedRuntimeStateForScript(t, cityPath, port)

		out, err := runPortResolve(t, cityPath, func(fakeBin string) {
			writeExecutable(t, filepath.Join(fakeBin, "nc"), "#!/bin/sh\nexit 1\n")
		})
		assertRuntimePortExit78(t, err, out, filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json"), cityPath)
	})
}

// TestManagedRuntimeListenerPIDFallsBackToLsofWithoutProcTables pins the
// fallback half of the contract: when neither TCP table under
// $DOLT_PROC_ROOT is readable — what every darwin host looks like, which
// an empty override models — the lookup still answers through the lsof
// pipeline, skipping lsof's non-numeric noise and keeping only pids that
// are alive. It passes both before and after the TCP-table switch; it is
// the regression guard that keeps that switch from swallowing the
// fallback (#7553).
func TestManagedRuntimeListenerPIDFallsBackToLsofWithoutProcTables(t *testing.T) {
	fakeBin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "lsof-ran")
	testPid := strconv.Itoa(os.Getpid())
	writeExecutable(t, filepath.Join(fakeBin, "lsof"), `#!/bin/sh
touch `+shellQuote(marker)+`
echo "non-numeric noise"
echo `+testPid+`
`)

	// An empty root has neither net/tcp nor net/tcp6: nothing readable,
	// so the lookup must fall back to lsof and return its live pid.
	got, err := runManagedRuntimeListenerPID(t, "4406", nil,
		"GC_DOLT_PROC_ROOT="+t.TempDir(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if err != nil {
		t.Fatalf("managed_runtime_listener_pid: %v\noutput:\n%s", err, got)
	}
	if trimmed := strings.TrimSpace(got); trimmed != testPid {
		t.Fatalf("managed_runtime_listener_pid fallback = %q, want the lsof pid %q", trimmed, testPid)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("lsof did not run; without readable TCP tables the lookup must keep the lsof fallback")
	}
}
