package runtimecontract

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// screenReferencePath is the repo's reference session script,
// contrib/session-scripts/gc-session-screen: under Bazel in the test's
// runfiles (//contrib:all_files), otherwise beside this source file.
func screenReferencePath(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return filepath.Join(root, "contrib", "session-scripts", "gc-session-screen")
	}
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contrib", "session-scripts", "gc-session-screen"))
}

// TestScreenReferenceIsConformant runs the full catalog against
// gc-session-screen in a private state directory (its screen sockets live
// there too): it must pass every requirement it does not skip, read
// v2-ready, and skip only what it does not declare (exec, provision,
// report-activity's last_activity). Skipped where screen, jq, flock, cksum
// or procps ps is missing, each of which the script requires: macOS has
// only BSD ps, so the Mac lane skips it, and in CI it runs on the Bazel
// lane's RBE workers.
func TestScreenReferenceIsConformant(t *testing.T) {
	for _, tool := range []string{"screen", "jq", "flock", "cksum", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	// The script reads the process table in procps ps's fixed-width columns
	// (pid:10=), which BSD ps refuses.
	if exec.Command("ps", "-o", "pid:10=", "-p", strconv.Itoa(os.Getpid())).Run() != nil {
		t.Skip("procps ps not installed")
	}
	script := screenReferencePath(t)
	if _, err := os.Stat(script); err != nil {
		t.Skipf("reference script not available: %v", err)
	}
	// A short state directory: screen's socket paths must fit 107 bytes.
	// Under /tmp, not os.TempDir: macOS's $TMPDIR (/var/folders/...) alone
	// is about 50 bytes, too long for the catalog's session names.
	state, err := os.MkdirTemp("/tmp", "gcs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	t.Setenv("GC_EXEC_STATE_DIR", state)

	report, err := Run(context.Background(), script, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.V2Ready || report.Protocol.Locality != "local" {
		t.Errorf("handshake: v2-ready %v (missing %v), locality %q; want v2-ready and local", report.V2Ready, report.V2Missing, report.Protocol.Locality)
	}
	wantSkip := map[Code]bool{ReqConnectionExec: true, ReqProvisionBoxWithoutAgent: true, ReqObserveInventoryHasActivity: true}
	for _, res := range report.Results {
		switch {
		case res.Status == StatusFail:
			t.Errorf("%s failed: %s", res.Code, res.Detail)
		case res.Status == StatusSkip && !wantSkip[res.Code]:
			t.Errorf("%s skipped: %s", res.Code, res.Detail)
		}
	}
}
