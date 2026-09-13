package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const statusScript = "commands/status/run.sh"

// writeFakeBeadsBD installs a fake gc-beads-bd.sh at the path the status command
// resolves (GC_CITY_PATH/.gc/scripts/gc-beads-bd.sh) whose `probe` op exits with
// probeExit.
func writeFakeBeadsBD(t *testing.T, cityPath string, probeExit int) {
	t.Helper()
	scriptsDir := filepath.Join(cityPath, ".gc", "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(scriptsDir, "gc-beads-bd.sh"),
		fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  probe) exit %d ;;\nesac\nexit 0\n", probeExit))
}

func runStatus(t *testing.T, cityPath, host, port string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, statusScript))
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST="+host,
		"GC_DOLT_PORT="+port,
		"PATH="+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestStatusScriptExternalReachablePrintsEndpointText is the su-deol8 guard for
// `gc dolt status` producing meaningful text for a reachable external endpoint
// instead of exiting 0 silently.
func TestStatusScriptExternalReachablePrintsEndpointText(t *testing.T) {
	cityPath := t.TempDir()
	writeFakeBeadsBD(t, cityPath, 0)

	out, err := runStatus(t, cityPath, "superlzy-dolt", "3306")
	if err != nil {
		t.Fatalf("status exited nonzero for reachable endpoint: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("status produced no text for reachable external endpoint (su-deol8 regression)")
	}
	for _, want := range []string{"external endpoint", "superlzy-dolt:3306", "reachable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q; got:\n%s", want, out)
		}
	}
}

// TestStatusScriptExternalUnreachablePrintsEndpointText verifies the external
// endpoint failure case still names the remote endpoint (not "not running").
func TestStatusScriptExternalUnreachablePrintsEndpointText(t *testing.T) {
	cityPath := t.TempDir()
	writeFakeBeadsBD(t, cityPath, 2)

	out, err := runStatus(t, cityPath, "superlzy-dolt", "3306")
	if err == nil {
		t.Fatalf("status exited 0 for unreachable endpoint; want nonzero\n%s", out)
	}
	for _, want := range []string{"external endpoint", "superlzy-dolt:3306", "unreachable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not running") {
		t.Fatalf("status reported a reachable-external failure as a local 'not running' signal:\n%s", out)
	}
}

// TestStatusScriptLocalRunningPrintsManagedText verifies the local managed path
// keeps its own message and does not adopt the external phrasing.
func TestStatusScriptLocalRunningPrintsManagedText(t *testing.T) {
	cityPath := t.TempDir()
	writeFakeBeadsBD(t, cityPath, 0)

	out, err := runStatus(t, cityPath, "127.0.0.1", "3311")
	if err != nil {
		t.Fatalf("status exited nonzero for running managed server: %v\n%s", err, out)
	}
	for _, want := range []string{"running", "managed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "external endpoint") {
		t.Fatalf("local managed status must not use external-endpoint phrasing:\n%s", out)
	}
}
