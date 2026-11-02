package runtimecontract

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// screenReference is the repo's reference session script, the observe.v1
// upgrade of contrib/session-scripts/gc-session-screen.
const screenReference = "../../../contrib/session-scripts/gc-session-screen"

// TestScreenReferenceIsConformant runs the full catalog against
// gc-session-screen on a private screen socket directory: it must pass
// every requirement it does not skip, and skip only what it does not
// declare (exec, provision, the city-scoped token, report-activity's
// last_activity). Skipped where screen, jq, flock or pgrep is missing.
func TestScreenReferenceIsConformant(t *testing.T) {
	for _, tool := range []string{"screen", "jq", "flock", "pgrep", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	script, err := filepath.Abs(screenReference)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("reference script not available: %v", err)
	}
	dir := t.TempDir()
	screenDir := filepath.Join(dir, "screens")
	if err := os.Mkdir(screenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCREENDIR", screenDir)
	t.Setenv("GC_EXEC_STATE_DIR", filepath.Join(dir, "state"))

	report, err := Run(context.Background(), script, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.V2Ready || report.Protocol.Locality != "local" {
		t.Errorf("handshake: v2-ready %v (missing %v), locality %q; want v2-ready and local", report.V2Ready, report.V2Missing, report.Protocol.Locality)
	}
	wantSkip := map[Code]bool{ReqConnectionExec: true, ReqProvisionBoxWithoutAgent: true, ReqObserveCityScoped: true, ReqObserveInventoryHasActivity: true}
	for _, res := range report.Results {
		switch {
		case res.Status == StatusFail:
			t.Errorf("%s failed: %s", res.Code, res.Detail)
		case res.Status == StatusSkip && !wantSkip[res.Code]:
			t.Errorf("%s skipped: %s", res.Code, res.Detail)
		}
	}
}
