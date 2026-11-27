package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// serverPackScript is an exec runtime pack that owns a real server process:
// the first start of the city brings it up (a shell whose argv carries the
// test's unique token), and server-stop ends every box and the server. Its
// boxes are files in its state dir, and it logs every op it is sent as one
// "<op> <name>" line. Flag files in the state dir switch it:
//
//   - declare: the handshake declares server-lifecycle (otherwise none);
//   - fail-server-stop: server-stop exits 1 naming a survivor.
const serverPackScript = `#!/bin/sh
D=%DIR%
op=$1
name=$2
echo "$op $name" >> "$D/ops.log"
case "$op" in
protocol)
  if [ -f "$D/declare" ]; then echo '{"version":0,"capabilities":["server-lifecycle"]}'; else echo '{"version":0,"capabilities":[]}'; fi ;;
start)
  cat >/dev/null
  if [ -f "$D/box.$name" ]; then echo "session $name already exists" >&2; exit 1; fi
  if [ ! -f "$D/server.pid" ]; then
    sh -c 'trap "exit 0" TERM; while :; do sleep 1 & wait; done' "%TOKEN%" </dev/null >/dev/null 2>&1 &
    echo $! > "$D/server.pid"
  fi
  touch "$D/box.$name" ;;
stop) rm -f "$D/box.$name" ;;
is-running) if [ -f "$D/box.$name" ]; then echo true; else echo false; fi ;;
list-running)
  for f in "$D"/box.*; do
    [ -e "$f" ] || continue
    n=${f#"$D"/box.}
    case "$n" in "$name"*) echo "$n" ;; esac
  done ;;
server-stop)
  if [ -f "$D/fail-server-stop" ]; then echo "session x may survive: backend wedged" >&2; exit 1; fi
  rm -f "$D"/box.*
  if [ -f "$D/server.pid" ]; then
    pid=$(cat "$D/server.pid")
    kill "$pid" 2>/dev/null
    while [ -n "$(tr -d '\000' < "/proc/$pid/cmdline" 2>/dev/null)" ]; do sleep 0.1; done
    rm -f "$D/server.pid"
  fi ;;
*) exit 2 ;;
esac
`

// serverPack is one serverPackScript install: its state dir, the token its
// server process carries, and the city's exec provider over it.
type serverPack struct {
	dir   string
	token string
	sp    runtime.Provider
}

// newServerPack installs serverPackScript, declaring server-lifecycle when
// declare is set, and registers a cleanup that kills a server the test left.
func newServerPack(t *testing.T, declare bool, flags ...string) serverPack {
	t.Helper()
	dir := t.TempDir()
	token := fmt.Sprintf("gc-pack-server-%d", time.Now().UnixNano())
	script := filepath.Join(dir, "pack.sh")
	body := strings.NewReplacer("%DIR%", dir, "%TOKEN%", token).Replace(serverPackScript)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if declare {
		flags = append(flags, "declare")
	}
	for _, f := range flags {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := serverPack{dir: dir, token: token, sp: sessionexec.NewSeamBackedForCity(script, "server-pack-city", dir)}
	t.Cleanup(func() { p.killServer(t) })
	return p
}

// killServer kills the pack's server when the test left it running, as the
// failure-path and undeclared-pack tests do. It reads the pid the pack
// recorded when it brought the server up, not the process table, so it also
// runs where there is no /proc (macOS). server-stop removes that record only
// once its server is gone, so a recorded pid is the live server's.
func (p serverPack) killServer(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(p.dir, "server.pid"))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Errorf("reading the pack's server pid: %v", err)
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Errorf("reading the pack's server pid %q: %v", data, err)
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Errorf("finding the pack's server %d: %v", pid, err)
		return
	}
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("killing the pack's server %d: %v", pid, err)
	}
}

// startBoxes starts names on the pack, which brings its server up.
func (p serverPack) startBoxes(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := p.sp.Start(t.Context(), name, runtime.Config{WorkDir: t.TempDir(), Command: "sleep 300", AcceptStartupDialogs: new(bool)}); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
	}
}

// ops returns the ops the pack was sent, as "<op> <name>" lines ("<op>" for
// an op with no name).
func (p serverPack) ops(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(p.dir, "ops.log"))
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ops = append(ops, line)
		}
	}
	return ops
}

// processesCarrying returns the pids of live processes whose command line
// carries token. A zombie's command line is empty, so it is not counted. It
// reads /proc, so only a test that calls requireProcTable first may use it.
func processesCarrying(t *testing.T, token string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("reading the process table: %v", err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if strings.Contains(string(cmdline), token) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// requireProcTable skips a test that reads the process table where there is
// no /proc.
func requireProcTable(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skipf("no /proc process table: %v", err)
	}
}

// stopServerPackCity runs gc stop's body over a city whose runtime is p,
// returning its exit code and stderr.
func stopServerPackCity(t *testing.T, p serverPack) (int, string) {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "server-pack-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return p.sp, nil }

	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
	return code, stderr.String()
}

// assertServerStopAfterDrain fails unless ops holds exactly one server-stop,
// after every session stop and after the last listing.
func assertServerStopAfterDrain(t *testing.T, ops []string, stopped ...string) {
	t.Helper()
	idx := slices.Index(ops, "server-stop")
	if idx < 0 || slices.Index(ops[idx+1:], "server-stop") >= 0 {
		t.Fatalf("ops = %q, want exactly one server-stop", ops)
	}
	for _, name := range stopped {
		at := slices.Index(ops, "stop "+name)
		if at < 0 || at > idx {
			t.Errorf("ops = %q: stop %s at %d, want it before server-stop at %d", ops, name, at, idx)
		}
	}
	for i, op := range ops[idx+1:] {
		if strings.HasPrefix(op, "list-running") {
			t.Errorf("ops = %q: a listing at %d follows server-stop at %d; teardown must come after the sweep", ops, idx+1+i, idx)
		}
	}
}

// TestGCStopTearsDownServerAfterSessionsDrain pins the order gc stop owes a
// pack declaring server-lifecycle: every session is stopped, and the
// listing swept, before one server-stop.
func TestGCStopTearsDownServerAfterSessionsDrain(t *testing.T) {
	p := newServerPack(t, true)
	p.startBoxes(t, "drain-a", "drain-b")

	code, stderr := stopServerPackCity(t, p)
	if code != 0 {
		t.Fatalf("gc stop = %d, want 0; stderr=%q", code, stderr)
	}
	assertServerStopAfterDrain(t, p.ops(t), "drain-a", "drain-b")
}

// TestCityShutdownTearsDownServerAfterSweep pins the same order for the
// controller's shutdown, the supervisor-managed stop.
func TestCityShutdownTearsDownServerAfterSweep(t *testing.T) {
	p := newServerPack(t, true)
	p.startBoxes(t, "sweep-a", "sweep-b")
	cr := serverLifecycleCityRuntime(t, p.sp)
	markOwnedForTest(cr)

	cr.shutdown()

	assertServerStopAfterDrain(t, p.ops(t), "sweep-a", "sweep-b")
	for _, name := range []string{"sweep-a", "sweep-b"} {
		if p.sp.IsRunning(name) {
			t.Errorf("%s still running after the shutdown", name)
		}
	}
}

// TestGCStopLeavesNoPackServerProcess is the leak test: a pack's server
// process does not outlive gc stop.
func TestGCStopLeavesNoPackServerProcess(t *testing.T) {
	requireProcTable(t)
	p := newServerPack(t, true)
	p.startBoxes(t, "leak-a", "leak-b")
	if len(processesCarrying(t, p.token)) == 0 {
		t.Fatal("the pack's server process is not running after start; the test proves nothing")
	}

	if code, stderr := stopServerPackCity(t, p); code != 0 {
		t.Fatalf("gc stop = %d, want 0; stderr=%q", code, stderr)
	}
	if pids := processesCarrying(t, p.token); len(pids) > 0 {
		t.Fatalf("the pack's server process(es) %v outlived gc stop", pids)
	}
}

// TestStopReportsTeardownServerFailure pins that a server-stop which cannot
// end everything reaches the operator: gc stop prints the pack's account of
// what may survive, as a warning that leaves its exit code alone.
func TestStopReportsTeardownServerFailure(t *testing.T) {
	p := newServerPack(t, true, "fail-server-stop")
	p.startBoxes(t, "fail-a")

	code, stderr := stopServerPackCity(t, p)
	if !strings.Contains(stderr, "gc stop: teardown server:") || !strings.Contains(stderr, "session x may survive: backend wedged") {
		t.Fatalf("stderr = %q, want the teardown failure with the pack's stderr", stderr)
	}
	if code != 0 {
		t.Fatalf("gc stop = %d, want 0: a teardown failure is a warning; stderr=%q", code, stderr)
	}
}

// TestGCStopSendsNoServerStopToUndeclaredPack pins that a pack which does
// not declare server-lifecycle keeps its behavior: gc stop sends it no
// server-stop and reports no teardown.
func TestGCStopSendsNoServerStopToUndeclaredPack(t *testing.T) {
	p := newServerPack(t, false)
	p.startBoxes(t, "plain-a")

	code, stderr := stopServerPackCity(t, p)
	if code != 0 {
		t.Fatalf("gc stop = %d, want 0; stderr=%q", code, stderr)
	}
	if ops := p.ops(t); slices.Contains(ops, "server-stop") {
		t.Fatalf("ops = %q, want no server-stop for an undeclared pack", ops)
	}
	if strings.Contains(stderr, "teardown server") {
		t.Fatalf("stderr = %q, want no teardown report", stderr)
	}
}
