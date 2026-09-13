package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const recoverScript = "commands/recover/run.sh"

// writeFakeDoltForRecover writes a stub `dolt` binary that always succeeds
// with empty output, so the recover script's read-only write probe reports
// "writable" and the script exits 0 without invoking gc-beads-bd.
func writeFakeDoltForRecover(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	body := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return binDir
}

func runRecoverWithHost(t *testing.T, host string) ([]byte, error) {
	t.Helper()
	root := repoRoot(t)
	binDir := writeFakeDoltForRecover(t)
	cityPath := t.TempDir()

	cmd := exec.Command("sh", filepath.Join(root, recoverScript))
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
		"GC_CITY_RUNTIME_DIR", "GC_PACK_STATE_DIR", "GC_DOLT_LOG_FILE",
		"GC_BEADS_BD_SCRIPT",
	),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_PORT=3311",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	if host != "" {
		cmd.Env = append(cmd.Env, "GC_DOLT_HOST="+host)
	}
	return cmd.CombinedOutput()
}

// TestRecoverTreatsLocalHostsAsManaged pins the recover script's host
// classification to the P0.5 contract: empty, 127.0.0.1 (the managed bind
// default), 0.0.0.0 (the explicit wildcard opt-out), localhost, and ::1 all
// mean a GC-managed local server, so recovery must proceed past the
// remote-host guard. Matches contract.DoltHostIsLocal.
func TestRecoverTreatsLocalHostsAsManaged(t *testing.T) {
	for _, host := range []string{"", "127.0.0.1", "0.0.0.0", "localhost", "::1"} {
		name := host
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			out, err := runRecoverWithHost(t, host)
			if err != nil {
				t.Fatalf("gc dolt recover refused GC_DOLT_HOST=%q as if remote: %v\n%s", host, err, out)
			}
			if strings.Contains(string(out), "not supported for remote dolt servers") {
				t.Fatalf("recover misclassified local host %q as remote:\n%s", host, out)
			}
		})
	}
}

func TestRecoverRejectsRemoteHostWithDiagnostic(t *testing.T) {
	out, err := runRecoverWithHost(t, "db.example.com")
	if err == nil {
		t.Fatalf("gc dolt recover unexpectedly proceeded for a remote host:\n%s", out)
	}
	if !strings.Contains(string(out), "not supported for remote dolt servers") {
		t.Fatalf("recover did not explain remote-host refusal:\n%s", out)
	}
}
