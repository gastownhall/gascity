//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bdDoltTopologyBudget bounds how long a topology test may spend in bdDolt.
// Managed-port recovery polls for 30s, so a passing run that stays under this
// budget proves that recovery never ran.
const bdDoltTopologyBudget = 10 * time.Second

// writeDoltModeCity creates a city directory whose .beads/metadata.json
// records doltMode, the topology marker bdDolt branches on.
func writeDoltModeCity(t *testing.T, doltMode string) string {
	t.Helper()
	cityDir := t.TempDir()
	beadsDir := filepath.Join(cityDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","dolt_mode":%q}`, doltMode)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	return cityDir
}

// storeCityEnvWithStaleManagedHints registers a per-city command env that
// already carries managed Dolt endpoint hints pointing at a dead port, the
// shape a city inherits from an earlier managed-mode command.
func storeCityEnvWithStaleManagedHints(t *testing.T, cityDir string) {
	t.Helper()
	env := append(integrationEnvDolt(),
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT=1",
		"BEADS_DOLT_SERVER_HOST=127.0.0.1",
		"BEADS_DOLT_SERVER_PORT=1",
	)
	cityCommandEnv.Store(cityDir, env)
	t.Cleanup(func() { cityCommandEnv.Delete(cityDir) })
}

// installFakeBDDoltTools swaps gcBinary and bdBinary for scripts that append
// every invocation to a log and then run the supplied shell bodies. The bd log
// records the endpoint hints each invocation saw.
func installFakeBDDoltTools(t *testing.T, bdBody, gcBody string) (gcLog, bdLog string) {
	t.Helper()
	binDir := t.TempDir()
	gcLog = filepath.Join(binDir, "gc.log")
	bdLog = filepath.Join(binDir, "bd.log")

	gcScript := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\n%s", gcLog, gcBody)
	bdScript := fmt.Sprintf("#!/bin/sh\n"+
		"echo \"port=${GC_DOLT_PORT:-} beads_port=${BEADS_DOLT_SERVER_PORT:-} host=${GC_DOLT_HOST:-} beads_host=${BEADS_DOLT_SERVER_HOST:-} args=$*\" >> %q\n%s",
		bdLog, bdBody)
	fakeGC := filepath.Join(binDir, "gc")
	fakeBD := filepath.Join(binDir, "bd")
	if err := os.WriteFile(fakeGC, []byte(gcScript), 0o755); err != nil {
		t.Fatalf("write fake gc: %v", err)
	}
	if err := os.WriteFile(fakeBD, []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	prevGC, prevBD := gcBinary, bdBinary
	gcBinary, bdBinary = fakeGC, fakeBD
	t.Cleanup(func() { gcBinary, bdBinary = prevGC, prevBD })
	return gcLog, bdLog
}

// readLogLines returns the non-empty lines of path, or nil when it does not
// exist (the fake never ran).
func readLogLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestBDDoltProxiedStoreSkipsManagedPortRecovery(t *testing.T) {
	cityDir := writeDoltModeCity(t, "proxied-server")
	storeCityEnvWithStaleManagedHints(t, cityDir)
	gcLog, bdLog := installFakeBDDoltTools(t, "echo ok\n", "")

	start := time.Now()
	out, err := bdDolt(cityDir, "show", "x")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("bdDolt() error = %v, out = %q", err, out)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Errorf("bdDolt() out = %q, want ok", out)
	}
	if elapsed >= bdDoltTopologyBudget {
		t.Errorf("bdDolt() took %s, want well under %s (managed-port recovery likely ran)", elapsed, bdDoltTopologyBudget)
	}
	if got := readLogLines(t, gcLog); len(got) != 0 {
		t.Errorf("gc was invoked %q, want no gc start for a provider-owned proxied store", got)
	}
	want := "port= beads_port= host= beads_host= args=show x"
	if got := readLogLines(t, bdLog); len(got) != 1 || got[0] != want {
		t.Errorf("bd saw %q, want exactly one call with every managed endpoint hint stripped: %q", got, want)
	}
}

func TestBDDoltProxiedStoreTransportRetryStaysPortless(t *testing.T) {
	cityDir := writeDoltModeCity(t, "proxied-server")
	storeCityEnvWithStaleManagedHints(t, cityDir)
	count := filepath.Join(t.TempDir(), "count")
	bdBody := fmt.Sprintf(`n=$(cat %[1]q 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > %[1]q
if [ "$n" -eq 1 ]; then
  echo 'dial tcp 127.0.0.1:1: connect: connection refused'
  exit 1
fi
echo ok
`, count)
	gcLog, bdLog := installFakeBDDoltTools(t, bdBody, "")

	start := time.Now()
	out, err := bdDolt(cityDir, "show", "x")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("bdDolt() error = %v, out = %q", err, out)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Errorf("bdDolt() out = %q, want ok after the transport retry", out)
	}
	if elapsed >= bdDoltTopologyBudget {
		t.Errorf("bdDolt() took %s, want well under %s", elapsed, bdDoltTopologyBudget)
	}
	if got := readLogLines(t, gcLog); len(got) != 0 {
		t.Errorf("gc was invoked %q, want no gc start while retrying a proxied store", got)
	}
	calls := readLogLines(t, bdLog)
	if len(calls) < 2 {
		t.Fatalf("bd calls = %q, want the failed call plus a retry", calls)
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "port= beads_port= host= beads_host= ") {
			t.Errorf("bd call %q carried a managed endpoint hint on the proxied path", call)
		}
	}
}

func TestBDDoltProxiedStoreSurfacesBDFailure(t *testing.T) {
	cityDir := writeDoltModeCity(t, "proxied-server")
	gcLog, _ := installFakeBDDoltTools(t, "echo 'boom: no such issue' >&2\nexit 1\n", "")

	out, err := bdDolt(cityDir, "show", "x")
	if err == nil {
		t.Fatalf("bdDolt() error = nil, want bd's own failure; out = %q", out)
	}
	if !strings.Contains(out, "boom: no such issue") {
		t.Errorf("bdDolt() out = %q, want bd's stderr preserved", out)
	}
	if got := readLogLines(t, gcLog); len(got) != 0 {
		t.Errorf("gc was invoked %q, want no gc start", got)
	}
}

func TestBDDoltManagedStoreStillRecoversManagedPort(t *testing.T) {
	cityDir := writeDoltModeCity(t, "server")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	gcBody := fmt.Sprintf(`if [ "$1" = start ]; then
  mkdir -p "$2/.beads"
  echo %s > "$2/.beads/dolt-server.port"
fi
`, port)
	gcLog, bdLog := installFakeBDDoltTools(t, "echo ok\n", gcBody)

	out, err := bdDolt(cityDir, "show", "x")
	if err != nil {
		t.Fatalf("bdDolt() error = %v, out = %q", err, out)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Errorf("bdDolt() out = %q, want ok", out)
	}
	if got := readLogLines(t, gcLog); len(got) != 1 || got[0] != "start "+cityDir {
		t.Errorf("gc calls = %q, want exactly one %q", got, "start "+cityDir)
	}
	want := fmt.Sprintf("port=%[1]s beads_port=%[1]s host=127.0.0.1 beads_host=127.0.0.1 args=show x", port)
	if got := readLogLines(t, bdLog); len(got) != 1 || got[0] != want {
		t.Errorf("bd saw %q, want one call carrying the recovered managed endpoint: %q", got, want)
	}
}

func TestBDDoltUntilRefusesToRunPastDeadline(t *testing.T) {
	cityDir := writeDoltModeCity(t, "proxied-server")
	_, bdLog := installFakeBDDoltTools(t, "echo ok\n", "")

	_, err := bdDoltUntil(cityDir, time.Now().Add(-time.Second), "show", "x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bdDoltUntil() error = %v, want context.DeadlineExceeded", err)
	}
	if got := readLogLines(t, bdLog); len(got) != 0 {
		t.Errorf("bd was invoked %q, want no run once the deadline passed", got)
	}
}

func TestBDDoltUntilCapsBDCommandAtDeadline(t *testing.T) {
	cityDir := writeDoltModeCity(t, "proxied-server")
	installFakeBDDoltTools(t, "exec sleep 60\n", "")

	start := time.Now()
	_, err := bdDoltUntil(cityDir, time.Now().Add(500*time.Millisecond), "show", "x")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("bdDoltUntil() error = nil, want the hung bd cut off at the deadline")
	}
	if elapsed >= integrationBDCommandTimeout {
		t.Errorf("bdDoltUntil() took %s, want it cut off near the 500ms deadline, not the %s command timeout", elapsed, integrationBDCommandTimeout)
	}
}

func TestEnsureManagedDoltPortForTestUntilCapsRecoveryAtDeadline(t *testing.T) {
	cityDir := writeDoltModeCity(t, "server")
	gcLog, _ := installFakeBDDoltTools(t, "echo ok\n", "")

	start := time.Now()
	port, ok := ensureManagedDoltPortForTestUntil(cityDir, time.Now().Add(500*time.Millisecond))
	elapsed := time.Since(start)

	if ok {
		t.Fatalf("ensureManagedDoltPortForTestUntil() = %q, true; want no port from a gc that never starts one", port)
	}
	if elapsed >= integrationManagedDoltPortWait {
		t.Errorf("ensureManagedDoltPortForTestUntil() took %s, want the port poll cut off near the 500ms deadline, not %s", elapsed, integrationManagedDoltPortWait)
	}
	if got := readLogLines(t, gcLog); len(got) != 1 || got[0] != "start "+cityDir {
		t.Errorf("gc calls = %q, want the single recovery start before the poll", got)
	}
}

func TestEnsureManagedDoltPortForTestUntilSkipsStartPastDeadline(t *testing.T) {
	cityDir := writeDoltModeCity(t, "server")
	gcLog, _ := installFakeBDDoltTools(t, "echo ok\n", "")

	if port, ok := ensureManagedDoltPortForTestUntil(cityDir, time.Now().Add(-time.Second)); ok {
		t.Fatalf("ensureManagedDoltPortForTestUntil() = %q, true; want false past the deadline", port)
	}
	if got := readLogLines(t, gcLog); len(got) != 0 {
		t.Errorf("gc was invoked %q, want no gc start once the deadline passed", got)
	}
}

func TestWaitForBeadConditionBoundsRefreshByWorkflowDeadline(t *testing.T) {
	cityDir := writeDoltModeCity(t, "proxied-server")
	installFakeBDDoltTools(t, "exec sleep 60\n", "")

	start := time.Now()
	_, err := waitForBeadCondition(t, cityDir, "x-1", 700*time.Millisecond, func(graphBead) bool { return true })
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForBeadCondition() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed >= integrationBDCommandTimeout {
		t.Errorf("waitForBeadCondition() took %s, want it to return near its 700ms timeout instead of waiting out a %s bd call", elapsed, integrationBDCommandTimeout)
	}
}
