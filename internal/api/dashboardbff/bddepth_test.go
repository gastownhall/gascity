//go:build !windows

package dashboardbff

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The recursion fuse (ga-kihcmff, incident ga-2t6u9dy) at the dashboard BFF's
// subprocess runner. The runner builds a deliberately minimal environment
// (cleanEnv inherits nothing), so the depth counter must be carried across
// that scrub explicitly, and a bd launch at the bound must be refused.

// bdDepthStubDir installs a bd on the parent PATH that records the
// GC_BD_DEPTH it was launched with and returns the log path.
func bdDepthStubDir(t *testing.T) (beadsPath, log string) {
	t.Helper()
	root := t.TempDir()
	beadsPath = filepath.Join(root, ".beads")
	if err := os.Mkdir(beadsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log = filepath.Join(root, "bd-depth.log")
	script := "#!/bin/sh\nprintf 'depth=%s\\n' \"$GC_BD_DEPTH\" >> \"" + log + "\"\nprintf '%s' '{\"status\":\"ok\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// exec.Command resolves bd on the parent PATH; env and printf are
	// resolved there too (env) or are builtins (printf).
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ADMIN_PATH", "")
	return beadsPath, log
}

func readBdDepthLog(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestExecBdPingStampsDepthThroughTheCleanEnv(t *testing.T) {
	beadsPath, log := bdDepthStubDir(t)
	t.Setenv("GC_BD_DEPTH", "3")

	res, err := newExecRunner().execBdPing(context.Background(), beadsPath)
	if err != nil {
		t.Fatalf("execBdPing at depth 3: %v", err)
	}
	if res.exitCode != 0 {
		t.Fatalf("execBdPing exit = %d stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
	}
	if got := readBdDepthLog(t, log); len(got) != 1 || got[0] != "depth=4" {
		t.Fatalf("stub saw %v, want one run with depth=4 (the clean env must carry the counter)", got)
	}
}

func TestExecBdPingRefusesAtTheBound(t *testing.T) {
	beadsPath, log := bdDepthStubDir(t)
	t.Setenv("GC_BD_DEPTH", "16")

	_, err := newExecRunner().execBdPing(context.Background(), beadsPath)
	if err == nil {
		t.Fatal("execBdPing launched bd at depth 16, want a refusal")
	}
	for _, want := range []string{"GC_BD_DEPTH", "16"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if runs := readBdDepthLog(t, log); len(runs) != 0 {
		t.Fatalf("stub ran at the bound: %v", runs)
	}
}

// Only bd launches are fused: another subprocess keeps the minimal clean env
// (no counter) and is never refused.
func TestExecRunnerLeavesNonBdChildrenAlone(t *testing.T) {
	t.Setenv("GC_BD_DEPTH", "16")
	res, err := newExecRunner().run(context.Background(), "env", nil, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("non-bd run at depth 16 was refused: %v", err)
	}
	if strings.Contains(res.stdout, "GC_BD_DEPTH") {
		t.Errorf("non-bd child env %q carries GC_BD_DEPTH, want the clean env only", res.stdout)
	}
}
