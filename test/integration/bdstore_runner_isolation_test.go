//go:build integration

package integration

import (
	"encoding/json"
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

	binDir := t.TempDir()
	bdFixture := filepath.Join(binDir, "bd-fixture")
	fixture := `#!/bin/sh
printf '{"sentinel":"%s","home":"%s","beads_dir":"%s","beads_dolt_server_host":"%s","beads_dolt_server_port":"%s","gc_dolt_host":"%s","gc_dolt_port":"%s","beads_actor":"%s"}\n' \
  "${RUNNER_SENTINEL-}" "${HOME-}" "${BEADS_DIR-}" \
  "${BEADS_DOLT_SERVER_HOST-}" "${BEADS_DOLT_SERVER_PORT-}" \
  "${GC_DOLT_HOST-}" "${GC_DOLT_PORT-}" "${BEADS_ACTOR-}"
printf '%s\n' 'diagnostic after JSON: [warn] {"stream":"stderr"}' >&2
`
	if err := os.WriteFile(bdFixture, []byte(fixture), 0o755); err != nil {
		t.Fatalf("writing bd fixture: %v", err)
	}

	oldBDBinary := bdBinary
	bdBinary = bdFixture
	t.Cleanup(func() { bdBinary = oldBDBinary })

	isolatedHome := t.TempDir()
	runner := pinnedBdStoreCommandRunnerForEnv(t, []string{
		"HOME=" + isolatedHome,
		"RUNNER_SENTINEL=isolated",
	})
	out, err := runner(t.TempDir(), "bd")
	if err != nil {
		t.Fatalf("pinned runner: %v", err)
	}

	var got struct {
		Sentinel            string `json:"sentinel"`
		Home                string `json:"home"`
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
		t.Errorf("HOME = %q, want %q", got.Home, isolatedHome)
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

func pinnedBdStoreCommandRunnerForEnv(t *testing.T, env []string) beads.CommandRunner {
	t.Helper()
	switch factory := any(pinnedBdStoreCommandRunner).(type) {
	case func([]string) beads.CommandRunner:
		return factory(env)
	case func() beads.CommandRunner:
		return factory()
	default:
		t.Fatal("pinnedBdStoreCommandRunner has an unsupported factory signature")
		return nil
	}
}
