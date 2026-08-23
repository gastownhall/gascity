package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const logsScript = "commands/logs/run.sh"

func runLogs(t *testing.T, cityPath, host, port string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, logsScript))
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

// TestLogsScriptExternalMissingLogIsLimitationNotError is the su-deol8 guard:
// for a configured external Dolt endpoint the server log lives on the remote
// host, so a missing local dolt.log is an endpoint limitation with a clear
// message — not a hard failure the way a missing managed-server log is.
func TestLogsScriptExternalMissingLogIsLimitationNotError(t *testing.T) {
	cityPath := t.TempDir()

	out, err := runLogs(t, cityPath, "superlzy-dolt", "3306")
	if err != nil {
		t.Fatalf("logs hard-failed for external endpoint with missing local log; want exit 0 limitation: %v\n%s", err, out)
	}
	for _, want := range []string{"external Dolt endpoint", "superlzy-dolt:3306", "not available locally"} {
		if !strings.Contains(out, want) {
			t.Fatalf("logs limitation message missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "log file not found") {
		t.Fatalf("external endpoint should not emit the local managed-server 'log file not found' error:\n%s", out)
	}
}

// TestLogsScriptLocalMissingLogIsError verifies the local managed path still
// hard-fails when its expected log file is absent (unchanged behavior).
func TestLogsScriptLocalMissingLogIsError(t *testing.T) {
	cityPath := t.TempDir()

	out, err := runLogs(t, cityPath, "127.0.0.1", "3311")
	if err == nil {
		t.Fatalf("logs unexpectedly succeeded for local missing log; want error\n%s", out)
	}
	if !strings.Contains(out, "log file not found") {
		t.Fatalf("local missing-log error missing 'log file not found'; got:\n%s", out)
	}
}
