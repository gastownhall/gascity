package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/gastownhall/gascity/internal/fsyncprobe"
)

// stubFsyncProbe swaps the probe seam for the duration of a test and records
// the arguments the hidden command passed in.
func stubFsyncProbe(t *testing.T, res fsyncprobe.Result) (gotDir *string, gotTimeout *time.Duration) {
	t.Helper()
	var dir string
	var timeout time.Duration
	prev := fsyncProbeFunc
	fsyncProbeFunc = func(d string, to time.Duration) fsyncprobe.Result {
		dir, timeout = d, to
		return res
	}
	t.Cleanup(func() { fsyncProbeFunc = prev })
	return &dir, &timeout
}

func runFsyncProbeCmd(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(append([]string{"dolt-state", "fsync-probe"}, args...), &out, &errb)
	return out.String(), errb.String(), code
}

func TestDoltStateFsyncProbeFinishedPrintsStatusAndMeasuredMS(t *testing.T) {
	dir := t.TempDir()
	gotDir, gotTimeout := stubFsyncProbe(t, fsyncprobe.Result{Status: fsyncprobe.StatusFinished, FsyncMS: 42})

	stdout, stderr, code := runFsyncProbeCmd(t, "--dir", dir, "--timeout-ms", "1500")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	got := parseDoltStateOutput(t, stdout)
	if got["status"] != "finished" || got["fsync_ms"] != "42" {
		t.Fatalf("output = %q, want status=finished fsync_ms=42", stdout)
	}
	if _, ok := got["error"]; ok {
		t.Fatalf("output has an error key on a finished probe: %q", stdout)
	}
	if *gotDir != dir || *gotTimeout != 1500*time.Millisecond {
		t.Fatalf("probe called with dir=%q timeout=%v, want %q / 1.5s", *gotDir, *gotTimeout, dir)
	}
}

func TestDoltStateFsyncProbeUnfinishedExitsZeroWithBound(t *testing.T) {
	stubFsyncProbe(t, fsyncprobe.Result{Status: fsyncprobe.StatusUnfinished, FsyncMS: 1000})

	stdout, stderr, code := runFsyncProbeCmd(t, "--dir", t.TempDir(), "--timeout-ms", "1000")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (unfinished is a measurement); stderr: %s", code, stderr)
	}
	got := parseDoltStateOutput(t, stdout)
	if got["status"] != "unfinished" || got["fsync_ms"] != "1000" {
		t.Fatalf("output = %q, want status=unfinished fsync_ms=1000", stdout)
	}
}

func TestDoltStateFsyncProbeErrorExitsNonZeroWithMessage(t *testing.T) {
	stubFsyncProbe(t, fsyncprobe.Result{Status: fsyncprobe.StatusError, Error: "stat /nope: no such file or directory"})

	stdout, _, code := runFsyncProbeCmd(t, "--dir", "/nope", "--timeout-ms", "1000")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero for status=error; stdout: %s", stdout)
	}
	got := parseDoltStateOutput(t, stdout)
	if got["status"] != "error" || got["error"] == "" {
		t.Fatalf("output = %q, want status=error with an error line", stdout)
	}
}

func TestDoltStateFsyncProbeRequiresDirAndTimeout(t *testing.T) {
	stubFsyncProbe(t, fsyncprobe.Result{Status: fsyncprobe.StatusFinished})
	for _, args := range [][]string{{"--timeout-ms", "1000"}, {"--dir", "/tmp"}} {
		if _, _, code := runFsyncProbeCmd(t, args...); code == 0 {
			t.Fatalf("exit = 0 for %v, want a usage failure", args)
		}
	}
}

// TestDoltStateFsyncProbeUsesTheRealProbe pins the production wiring of the
// seam: every other test here swaps fsyncProbeFunc, so only this one notices
// the default being replaced by something that is not fsyncprobe.Probe.
func TestDoltStateFsyncProbeUsesTheRealProbe(t *testing.T) {
	got := reflect.ValueOf(fsyncProbeFunc).Pointer()
	want := reflect.ValueOf(fsyncprobe.Probe).Pointer()
	if got != want {
		t.Fatalf("fsyncProbeFunc is not fsyncprobe.Probe (%#x != %#x)", got, want)
	}
}

// TestDoltStateFsyncProbeRealChain runs the hidden commands end to end: the
// probe re-execs this test binary as the detached worker (testscript names it
// gc) and reads the result back over fd 3.
func TestDoltStateFsyncProbeRealChain(t *testing.T) {
	testscript.Run(t, newTestscriptParams(t, filepath.Join("testdata", "dolt-state-fsync-probe.txtar")))
}

func TestDoltStateFsyncProbeRejectsNonPositiveTimeout(t *testing.T) {
	called := false
	prev := fsyncProbeFunc
	fsyncProbeFunc = func(string, time.Duration) fsyncprobe.Result {
		called = true
		return fsyncprobe.Result{Status: fsyncprobe.StatusFinished}
	}
	t.Cleanup(func() { fsyncProbeFunc = prev })
	for _, ms := range []string{"0", "-1", "-5000"} {
		stdout, _, code := runFsyncProbeCmd(t, "--dir", t.TempDir(), "--timeout-ms", ms)
		if code == 0 {
			t.Fatalf("--timeout-ms %s: exit = 0, want non-zero", ms)
		}
		if got := parseDoltStateOutput(t, stdout); got["status"] != "error" || got["error"] == "" {
			t.Fatalf("--timeout-ms %s: output = %q, want status=error with an error line", ms, stdout)
		}
	}
	if called {
		t.Fatal("the probe ran with a non-positive bound")
	}
}

func TestDoltStateFsyncProbeIsHidden(t *testing.T) {
	cmd := newDoltStateCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, name := range []string{"fsync-probe", "fsync-probe-worker"} {
		sub, _, err := cmd.Find([]string{name})
		if err != nil || sub == nil || sub.Name() != name {
			t.Fatalf("dolt-state has no %q subcommand (err %v)", name, err)
		}
		if !sub.Hidden {
			t.Fatalf("dolt-state %s is not hidden", name)
		}
	}
}

// fsyncGuardFixture is a materialized city plus a fake gc that answers the
// helpers op_recover reaches before and after the stall guard.
type fsyncGuardFixture struct {
	cityPath string
	script   string
	fakeGC   string
	callLog  string
	dataDir  string
	logFile  string
	port     int
	baseEnv  []string
}

func newFsyncGuardFixture(t *testing.T, listening bool) *fsyncGuardFixture {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, cityPath)

	ln := listenOnRandomPort(t)
	port := ln.Addr().(*net.TCPAddr).Port
	if listening {
		t.Cleanup(func() { _ = ln.Close() })
	} else {
		_ = ln.Close()
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A dolt stub that always fails keeps the no-GC_BIN shell fallback from
	// starting a real server.
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	callLog := filepath.Join(t.TempDir(), "gc-calls")
	fakeGC := filepath.Join(binDir, "gc")
	body := fmt.Sprintf(`#!/bin/sh
call_log=%q
sub="$1 $2"
shift 2
case "$sub" in
  "dolt-state allocate-port")
    printf 'allocate-port\n' >> "$call_log"
    printf '%%s\n' "$FAKE_PORT"
    ;;
  "dolt-state fsync-probe")
    printf 'fsync-probe %%s\n' "$*" >> "$call_log"
    case "${FAKE_PROBE_MODE:-}" in
      unfinished) printf 'status\tunfinished\nfsync_ms\t%%s\n' "${FAKE_PROBE_BOUND:-1000}" ;;
      finished)   printf 'status\tfinished\nfsync_ms\t%%s\n' "${FAKE_PROBE_MS:-5}" ;;
      error)      printf 'status\terror\nerror\tstat failed\n'; exit 1 ;;
      exit1)      exit 1 ;;
      empty)      exit 0 ;;
      garbage)    printf 'this is not tab separated\n' ;;
      *)          exit 1 ;;
    esac
    ;;
  "dolt-state recover-managed")
    printf 'recover-managed\n' >> "$call_log"
    printf 'diagnosed_read_only\tfalse\nhad_pid\ttrue\nforced\tfalse\nready\ttrue\npid\t12345\nport\t%%s\nhealthy\ttrue\n' "$FAKE_PORT"
    ;;
  *)
    printf 'other %%s %%s\n' "$sub" "$*" >> "$call_log"
    exit 1
    ;;
esac
`, callLog)
	if err := os.WriteFile(fakeGC, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	// op_recover's liveness check ends in a `ps` match on `--data-dir.*<basename>`
	// (find_dolt_pid), so the basename must not be one a real Dolt on this host
	// could carry: a plain "dolt-data" would make the "no process" tests depend
	// on what else is running.
	dataDir := filepath.Join(t.TempDir(), "fsyncguard-"+strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())+"-dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "dolt.log")
	f := &fsyncGuardFixture{
		cityPath: cityPath,
		script:   gcBeadsBdScriptPath(cityPath),
		fakeGC:   fakeGC,
		callLog:  callLog,
		dataDir:  dataDir,
		logFile:  logFile,
		port:     port,
	}
	f.baseEnv = []string{
		"GC_CITY_PATH=" + cityPath,
		"GC_DOLT_DATA_DIR=" + dataDir,
		"GC_DOLT_LOG_FILE=" + logFile,
		"GC_DOLT_PORT=" + fmt.Sprint(port),
		"FAKE_PORT=" + fmt.Sprint(port),
		"PATH=" + strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	}
	return f
}

// recover runs `gc-beads-bd recover` with GC_BIN set unless noGCBin, plus extra env.
func (f *fsyncGuardFixture) recover(t *testing.T, noGCBin bool, extra ...string) (out string, err error) {
	t.Helper()
	env := append([]string{}, f.baseEnv...)
	if !noGCBin {
		env = append(env, "GC_BIN="+f.fakeGC)
	}
	env = append(env, extra...)
	stdout, stderr, err := runGCBeadsBdCommand(t, sanitizedBaseEnv(env...), f.script, "recover")
	return stdout + stderr, err
}

func (f *fsyncGuardFixture) calls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.callLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func countLinesContaining(s, sub string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

const failOpenMarker = "fsync-stall guard skipped"

func assertSkippedForStall(t *testing.T, f *fsyncGuardFixture, out string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("recover succeeded under a storage stall; output:\n%s", out)
	}
	if !strings.Contains(out, "storage stall") {
		t.Fatalf("recover output missing the %q die text:\n%s", "storage stall", out)
	}
	if !strings.Contains(out, "docs/troubleshooting/dolt-storage-stalls.md") || !strings.Contains(out, "gc dolt restart --force") {
		t.Fatalf("recover output missing the docs pointer / --force hint:\n%s", out)
	}
	if calls := f.calls(t); strings.Contains(calls, "recover-managed") {
		t.Fatalf("recover-managed was invoked despite the stall guard; calls:\n%s", calls)
	}
}

func assertRecoverProceeded(t *testing.T, f *fsyncGuardFixture, out string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("recover failed: %v\n%s", err, out)
	}
	if !strings.Contains(f.calls(t), "recover-managed") {
		t.Fatalf("recover-managed was not invoked; calls:\n%s\noutput:\n%s", f.calls(t), out)
	}
	if strings.Contains(out, "storage stall") {
		t.Fatalf("recover mentioned a storage stall when it should have proceeded:\n%s", out)
	}
}

func TestGcBeadsBdRecoverSkipsWhenFsyncProbeUnfinished(t *testing.T) {
	f := newFsyncGuardFixture(t, true)
	out, err := f.recover(t, false, "FAKE_PROBE_MODE=unfinished")
	assertSkippedForStall(t, f, out, err)
	if !strings.Contains(f.calls(t), "fsync-probe --dir "+f.dataDir+" --timeout-ms 1000") {
		t.Fatalf("probe not called with the data dir and the default 1000ms bound; calls:\n%s", f.calls(t))
	}
	if strings.Contains(out, failOpenMarker) {
		t.Fatalf("a stall decision must not print the fail-open line:\n%s", out)
	}
}

func TestGcBeadsBdRecoverSkipsWhenFsyncFinishedAtOrAboveWarn(t *testing.T) {
	for _, ms := range []string{"1000", "2500"} {
		t.Run(ms+"ms", func(t *testing.T) {
			f := newFsyncGuardFixture(t, true)
			out, err := f.recover(t, false, "FAKE_PROBE_MODE=finished", "FAKE_PROBE_MS="+ms)
			assertSkippedForStall(t, f, out, err)
		})
	}
}

func TestGcBeadsBdRecoverProceedsWhenFsyncFast(t *testing.T) {
	f := newFsyncGuardFixture(t, true)
	out, err := f.recover(t, false, "FAKE_PROBE_MODE=finished", "FAKE_PROBE_MS=999")
	assertRecoverProceeded(t, f, out, err)
	if strings.Contains(out, failOpenMarker) {
		t.Fatalf("a fast probe must not print the fail-open line:\n%s", out)
	}
}

func TestGcBeadsBdRecoverFsyncGuardFailsOpenWithOneStderrLine(t *testing.T) {
	cases := map[string][]string{
		"probe status=error":    {"FAKE_PROBE_MODE=error"},
		"helper exits non-zero": {"FAKE_PROBE_MODE=exit1"},
		"unparsable output":     {"FAKE_PROBE_MODE=garbage"},
		"helper prints nothing": {"FAKE_PROBE_MODE=empty"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFsyncGuardFixture(t, true)
			out, err := f.recover(t, false, extra...)
			assertRecoverProceeded(t, f, out, err)
			if n := countLinesContaining(out, failOpenMarker); n != 1 {
				t.Fatalf("fail-open stderr lines = %d, want exactly 1; output:\n%s", n, out)
			}
		})
	}
}

func TestGcBeadsBdRecoverFsyncGuardFailsOpenWhenGCBinUnset(t *testing.T) {
	f := newFsyncGuardFixture(t, true)
	out, _ := f.recover(t, true, "FAKE_PROBE_MODE=unfinished")
	// Without GC_BIN the Go helper is unavailable, so the script cannot
	// probe: it must say so once and must not claim a storage stall.
	if n := countLinesContaining(out, failOpenMarker); n != 1 {
		t.Fatalf("fail-open stderr lines = %d, want exactly 1; output:\n%s", n, out)
	}
	if strings.Contains(out, "storage stall") {
		t.Fatalf("GC_BIN unset must fail open, not report a stall:\n%s", out)
	}
	if strings.Contains(f.calls(t), "fsync-probe") {
		t.Fatalf("the probe ran without GC_BIN; calls:\n%s", f.calls(t))
	}
}

func TestGcBeadsBdRecoverNeedsNoProbeWhenDoltNotListening(t *testing.T) {
	f := newFsyncGuardFixture(t, false)
	out, err := f.recover(t, false, "FAKE_PROBE_MODE=unfinished")
	assertRecoverProceeded(t, f, out, err)
	if strings.Contains(f.calls(t), "fsync-probe") {
		t.Fatalf("probed although nothing is listening; calls:\n%s", f.calls(t))
	}
	if strings.Contains(out, failOpenMarker) {
		t.Fatalf("a not-listening Dolt is not a guard decision and must stay silent:\n%s", out)
	}
}

// livePIDFileEnv plants a PID file naming a process that is alive (this test
// binary) and returns the env that points op_recover at it: the shape
// find_dolt_pid reads first, so the script sees a managed Dolt that is running
// whether or not anything listens.
//
// The fake gc's recover-managed always succeeds, which returns from op_recover
// before op_stop_impl. A case that reached the shell stop path would `kill`
// this very process, so never combine this with a run that has no GC_BIN.
func livePIDFileEnv(t *testing.T) string {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "dolt.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return "GC_DOLT_PID_FILE=" + pidFile
}

// A Dolt that is running but not yet accepting connections (a stall-slowed
// relaunch) is as exposed to a restart as a listening one: the guard must test
// "managed Dolt running", not just "port answers".
func TestGcBeadsBdRecoverSkipsARunningDoltThatIsNotListeningWhenFsyncStalled(t *testing.T) {
	for name, extra := range map[string][]string{
		"unfinished probe":               {"FAKE_PROBE_MODE=unfinished"},
		"finished at the warn threshold": {"FAKE_PROBE_MODE=finished", "FAKE_PROBE_MS=1000"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFsyncGuardFixture(t, false)
			out, err := f.recover(t, false, append(extra, livePIDFileEnv(t))...)
			assertSkippedForStall(t, f, out, err)
			if !strings.Contains(f.calls(t), "fsync-probe --dir "+f.dataDir+" --timeout-ms 1000") {
				t.Fatalf("probe not called with the data dir and the default 1000ms bound; calls:\n%s", f.calls(t))
			}
			if strings.Contains(out, failOpenMarker) {
				t.Fatalf("a stall decision must not print the fail-open line:\n%s", out)
			}
		})
	}
}

func TestGcBeadsBdRecoverProceedsForARunningDoltThatIsNotListeningWhenFsyncFast(t *testing.T) {
	f := newFsyncGuardFixture(t, false)
	out, err := f.recover(t, false, "FAKE_PROBE_MODE=finished", "FAKE_PROBE_MS=999", livePIDFileEnv(t))
	assertRecoverProceeded(t, f, out, err)
	if n := countLinesContaining(f.calls(t), "fsync-probe"); n != 1 {
		t.Fatalf("a running Dolt must be probed exactly once, got %d; calls:\n%s", n, f.calls(t))
	}
	if strings.Contains(out, failOpenMarker) {
		t.Fatalf("a fast probe must not print the fail-open line:\n%s", out)
	}
}

func TestGcBeadsBdRecoverFailsOpenWithOneLineForARunningDoltThatIsNotListening(t *testing.T) {
	for _, mode := range []string{"error", "exit1", "garbage", "empty"} {
		t.Run(mode, func(t *testing.T) {
			f := newFsyncGuardFixture(t, false)
			out, err := f.recover(t, false, "FAKE_PROBE_MODE="+mode, livePIDFileEnv(t))
			assertRecoverProceeded(t, f, out, err)
			if n := countLinesContaining(out, failOpenMarker); n != 1 {
				t.Fatalf("fail-open stderr lines = %d, want exactly 1; output:\n%s", n, out)
			}
		})
	}
}

// A PID file that names no live process is stale: nothing is running, so there
// is nothing to protect and recovery must go ahead without probing.
func TestGcBeadsBdRecoverIgnoresAStalePIDFileWhenNothingListens(t *testing.T) {
	f := newFsyncGuardFixture(t, false)
	pidFile := filepath.Join(t.TempDir(), "dolt.pid")
	if err := os.WriteFile(pidFile, []byte("not-a-pid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := f.recover(t, false, "FAKE_PROBE_MODE=unfinished", "GC_DOLT_PID_FILE="+pidFile)
	assertRecoverProceeded(t, f, out, err)
	if strings.Contains(f.calls(t), "fsync-probe") {
		t.Fatalf("probed although the PID file names no live process; calls:\n%s", f.calls(t))
	}
	if strings.Contains(out, failOpenMarker) {
		t.Fatalf("a stale PID file is not a guard decision and must stay silent:\n%s", out)
	}
}

// TestFindDoltPIDTreatsANonPIDFileAsStale pins find_dolt_pid's PID-file check
// to the file holding a PID, not to kill's verdict alone. In POSIX mode bash
// 5.3 (/bin/sh on Fedora 44) `kill -0 not-a-pid` prints an error and exits 0,
// so a PID file holding anything but a PID read as a live Dolt and sent recover
// into the fsync-stall guard (ga-uhg2hta); `kill -0 0` and `kill -0 -1` succeed
// in every shell. The harness kill accepts every operand the way that bash
// does, so this fails under any /bin/sh when the check trusts kill -0 alone.
func TestFindDoltPIDTreatsANonPIDFileAsStale(t *testing.T) {
	materializeBuiltinPacksForTest(t, t.TempDir())
	target, err := bundledGcBeadsBdScriptTarget()
	if err != nil {
		t.Fatalf("resolving bundled gc-beads-bd.sh: %v", err)
	}
	src, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading %s: %v", target, err)
	}
	harness := strings.Join([]string{
		"kill() { return 0; }",
		"find_port_holder() { :; }",
		"ps() { :; }",
		extractShellFunction(t, string(src), "find_dolt_pid"),
		"find_dolt_pid",
	}, "\n") + "\n"
	findDoltPID := func(t *testing.T, content string) (pid, pidFile string) {
		t.Helper()
		dir := t.TempDir()
		pidFile = filepath.Join(dir, "dolt.pid")
		if err := os.WriteFile(pidFile, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		env := sanitizedBaseEnv("DATA_DIR="+filepath.Join(dir, "dolt-data"), "PID_FILE="+pidFile, "CONFIG_FILE=")
		stdout, stderr, err := runGCBeadsBdCommand(t, env, "sh", "-c", harness)
		if err != nil {
			t.Fatalf("find_dolt_pid: %v\n%s", err, stderr)
		}
		return strings.TrimSpace(stdout), pidFile
	}

	for name, content := range map[string]string{
		"word":            "not-a-pid\n",
		"zero":            "0\n",
		"negative":        "-1\n",
		"number and word": "123 abc\n",
		"empty":           "",
		"leading zero":    "007\n",
	} {
		t.Run(name, func(t *testing.T) {
			pid, pidFile := findDoltPID(t, content)
			if pid != "" {
				t.Fatalf("find_dolt_pid reported %q for a PID file holding %q", pid, content)
			}
			if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
				t.Fatalf("the stale PID file holding %q was not removed (stat: %v)", content, err)
			}
		})
	}
	t.Run("a PID is still reported", func(t *testing.T) {
		want := strconv.Itoa(os.Getpid())
		if pid, _ := findDoltPID(t, want+"\n"); pid != want {
			t.Fatalf("find_dolt_pid = %q, want %q", pid, want)
		}
	})
}

func TestGcBeadsBdRecoverFsyncWarnThresholdFromEnv(t *testing.T) {
	t.Run("override lowers the threshold and the probe bound", func(t *testing.T) {
		f := newFsyncGuardFixture(t, true)
		out, err := f.recover(t, false, "GC_DOLT_FSYNC_WARN_MS=10", "FAKE_PROBE_MODE=finished", "FAKE_PROBE_MS=50")
		assertSkippedForStall(t, f, out, err)
		if !strings.Contains(f.calls(t), "--timeout-ms 10") {
			t.Fatalf("probe bound not taken from GC_DOLT_FSYNC_WARN_MS; calls:\n%s", f.calls(t))
		}
	})
	t.Run("override raises the threshold", func(t *testing.T) {
		f := newFsyncGuardFixture(t, true)
		out, err := f.recover(t, false, "GC_DOLT_FSYNC_WARN_MS=3000", "FAKE_PROBE_MODE=finished", "FAKE_PROBE_MS=2500")
		assertRecoverProceeded(t, f, out, err)
	})
	t.Run("leading zeros are decimal", func(t *testing.T) {
		// 0500 parses as octal and 0900 not at all if passed on as written.
		for env, want := range map[string]string{"0500": "500", "0900": "900", "00020": "20"} {
			f := newFsyncGuardFixture(t, true)
			_, _ = f.recover(t, false, "GC_DOLT_FSYNC_WARN_MS="+env, "FAKE_PROBE_MODE=finished", "FAKE_PROBE_MS=1")
			if !strings.Contains(f.calls(t), "--timeout-ms "+want+"\n") {
				t.Fatalf("GC_DOLT_FSYNC_WARN_MS=%s: probe bound is not %s; calls:\n%s", env, want, f.calls(t))
			}
		}
	})
	for _, bad := range []string{"", "abc", "0", "00", "-5", "1.5"} {
		t.Run("invalid value "+bad+" falls back to 1000", func(t *testing.T) {
			f := newFsyncGuardFixture(t, true)
			out, err := f.recover(t, false, "GC_DOLT_FSYNC_WARN_MS="+bad, "FAKE_PROBE_MODE=finished", "FAKE_PROBE_MS=1500")
			assertSkippedForStall(t, f, out, err)
			if !strings.Contains(f.calls(t), "--timeout-ms 1000") {
				t.Fatalf("fallback bound is not 1000; calls:\n%s", f.calls(t))
			}
		})
	}
}

// TestFsyncStallErrorThresholdFromEnv covers GC_DOLT_FSYNC_ERROR_MS, the
// doctor-side threshold the helper owns next to the guard's. The recover guard
// does not read it, so the helper is sourced and asked directly.
func TestFsyncStallErrorThresholdFromEnv(t *testing.T) {
	materializeBuiltinPacksForTest(t, t.TempDir())
	target, err := bundledGcBeadsBdScriptTarget()
	if err != nil {
		t.Fatalf("resolving bundled gc-beads-bd.sh: %v", err)
	}
	helper := filepath.Join(filepath.Dir(target), "dolt-fsync-stall.sh")

	cases := map[string]string{
		"":     "5000",
		"abc":  "5000",
		"0":    "5000",
		"00":   "5000",
		"-5":   "5000",
		"1.5":  "5000",
		"7000": "7000",
		"0900": "900",
	}
	for env, want := range cases {
		t.Run("value "+env, func(t *testing.T) {
			out, stderr, err := runGCBeadsBdCommand(t, sanitizedBaseEnv("GC_DOLT_FSYNC_ERROR_MS="+env),
				"sh", "-c", `. "$1" && fsync_stall_error_ms`, "sh", helper)
			if err != nil {
				t.Fatalf("fsync_stall_error_ms: %v\n%s", err, stderr)
			}
			if got := strings.TrimSpace(out); got != want {
				t.Fatalf("GC_DOLT_FSYNC_ERROR_MS=%q: got %q, want %q", env, got, want)
			}
		})
	}
}

func TestGcBeadsBdRecoverENOSPCWinsOverFsyncStall(t *testing.T) {
	f := newFsyncGuardFixture(t, true)
	if err := os.WriteFile(f.logFile, []byte("fatal: no space left on device\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := f.recover(t, false, "FAKE_PROBE_MODE=unfinished")
	if err == nil {
		t.Fatalf("recover succeeded despite ENOSPC; output:\n%s", out)
	}
	if !strings.Contains(out, "ENOSPC detected") || strings.Contains(out, "storage stall") {
		t.Fatalf("ENOSPC must be reported first and alone; output:\n%s", out)
	}
	if strings.Contains(f.calls(t), "fsync-probe") {
		t.Fatalf("the stall probe ran although the ENOSPC guard had already fired; calls:\n%s", f.calls(t))
	}
}

// TestMaterializedBdPackCarriesFsyncStallHelper pins that the helper ships
// beside the cache-resolved gc-beads-bd.sh (the per-city shim execs that
// copy, so op_recover's sibling lookup resolves there).
func TestMaterializedBdPackCarriesFsyncStallHelper(t *testing.T) {
	cityPath := t.TempDir()
	materializeBuiltinPacksForTest(t, cityPath)
	target, err := bundledGcBeadsBdScriptTarget()
	if err != nil {
		t.Fatalf("resolving bundled gc-beads-bd.sh: %v", err)
	}
	helper := filepath.Join(filepath.Dir(target), "dolt-fsync-stall.sh")
	data, err := os.ReadFile(helper)
	if err != nil {
		t.Fatalf("materialized bd pack is missing the fsync-stall helper: %v", err)
	}
	if !strings.Contains(string(data), "recovery_should_skip_due_to_fsync_stall") {
		t.Fatalf("%s does not define recovery_should_skip_due_to_fsync_stall", helper)
	}
}
