//go:build integration

package integration

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestPinnedBdStoreCommandRunnerUsesExactEnvironmentAndKeepsStdoutJSON(t *testing.T) {
	contaminated := map[string]string{
		"BEADS_DIR":              "/ambient/beads",
		"BEADS_DOLT_SERVER_HOST": "ambient-dolt.invalid",
		"BEADS_DOLT_SERVER_PORT": "3307",
		"GC_DOLT_HOST":           "ambient-gc-dolt.invalid",
		"GC_DOLT_PORT":           "3308",
		"BEADS_ACTOR":            "ambient-actor",
	}
	for name, value := range contaminated {
		t.Setenv(name, value)
	}

	useFixtureBD(t, `#!/bin/sh
printf '{"sentinel":"%s","home":"%s","backup_enabled":"%s","beads_dir":"%s","beads_dolt_server_host":"%s","beads_dolt_server_port":"%s","gc_dolt_host":"%s","gc_dolt_port":"%s","beads_actor":"%s"}\n' \
  "${RUNNER_SENTINEL-}" "${HOME-}" "${BD_BACKUP_ENABLED-}" "${BEADS_DIR-}" \
  "${BEADS_DOLT_SERVER_HOST-}" "${BEADS_DOLT_SERVER_PORT-}" \
  "${GC_DOLT_HOST-}" "${GC_DOLT_PORT-}" "${BEADS_ACTOR-}"
printf '%s\n' 'diagnostic after JSON: [warn] {"stream":"stderr"}' >&2
`)

	isolatedHome := t.TempDir()
	runner := isolatedBdStoreCommandRunner([]string{
		"HOME=" + t.TempDir(),
		"GC_HOME=" + isolatedHome,
		"RUNNER_SENTINEL=isolated",
	})
	out, err := runner(t.TempDir(), "bd")
	if err != nil {
		t.Fatalf("pinned runner: %v", err)
	}

	var got struct {
		Sentinel            string `json:"sentinel"`
		Home                string `json:"home"`
		BackupEnabled       string `json:"backup_enabled"`
		BeadsDir            string `json:"beads_dir"`
		BeadsDoltServerHost string `json:"beads_dolt_server_host"`
		BeadsDoltServerPort string `json:"beads_dolt_server_port"`
		GCDoltHost          string `json:"gc_dolt_host"`
		GCDoltPort          string `json:"gc_dolt_port"`
		BeadsActor          string `json:"beads_actor"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("runner stdout is not standalone JSON: %v\nstdout: %q", err, out)
	}
	if got.Sentinel != "isolated" {
		t.Errorf("RUNNER_SENTINEL = %q, want isolated environment value", got.Sentinel)
	}
	if got.Home != isolatedHome {
		t.Errorf("HOME = %q, want GC_HOME %q", got.Home, isolatedHome)
	}
	if got.BackupEnabled != "false" {
		t.Errorf("BD_BACKUP_ENABLED = %q, want %q: bd auto-backup opt-out", got.BackupEnabled, "false")
	}
	for name, value := range map[string]string{
		"BEADS_DIR":              got.BeadsDir,
		"BEADS_DOLT_SERVER_HOST": got.BeadsDoltServerHost,
		"BEADS_DOLT_SERVER_PORT": got.BeadsDoltServerPort,
		"GC_DOLT_HOST":           got.GCDoltHost,
		"GC_DOLT_PORT":           got.GCDoltPort,
		"BEADS_ACTOR":            got.BeadsActor,
	} {
		if value != "" {
			t.Errorf("child inherited ambient %s=%q", name, value)
		}
	}
}

func TestPinnedBdStoreCommandRunnerReportsSilentFallback(t *testing.T) {
	useFixtureBD(t, `#!/bin/sh
printf '%s\n' 'Auto-importing 3 issues into empty database' >&2
`)

	runner := isolatedBdStoreCommandRunner([]string{"HOME=" + t.TempDir()})
	if _, err := runner(t.TempDir(), "bd"); !errors.Is(err, beads.ErrBDSilentFallback) {
		t.Fatalf("runner error = %v, want beads.ErrBDSilentFallback", err)
	}
}

// useFixtureBD points bdBinary at an executable bd stand-in that runs script,
// for the duration of the test.
func useFixtureBD(t *testing.T, script string) {
	t.Helper()
	fixture := filepath.Join(t.TempDir(), "bd-fixture")
	if err := os.WriteFile(fixture, []byte(script), 0o755); err != nil {
		t.Fatalf("writing bd fixture: %v", err)
	}
	oldBDBinary := bdBinary
	bdBinary = fixture
	t.Cleanup(func() { bdBinary = oldBDBinary })
}
