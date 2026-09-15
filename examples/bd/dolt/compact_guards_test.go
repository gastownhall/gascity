package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A disabled city must reach no Dolt operation, including reclaim and previews.
func TestCompactScriptMaintenanceDisabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  []string
	}{
		{name: "flatten"},
		{name: "gc-only", args: []string{"--gc-only"}},
		{name: "dry-run", args: []string{"--dry-run"}},
		{name: "bare-gc", env: []string{"GC_DOLT_COMPACT_BARE_GC=1"}},
		{name: "allow-federated", env: []string{"GC_DOLT_COMPACT_ALLOW_FEDERATED=1"}},
		{name: "no-runtime", env: []string{"GC_DOLT_PORT=", "GC_PACK_DIR=/does-not-exist"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			writeCompactConfig(t, fixture, `{"config":{"Maintenance":{"Dolt":{"Enabled":false}}}}`)
			out, err := fixture.runWithArgs(t, "remote_success", tc.args, tc.env...)
			if err != nil || out != "compact: maintenance.dolt enabled=false, skipping\n" {
				t.Fatalf("disabled compact = %v, %q", err, out)
			}
			if log := readCompactDoltLog(t, fixture); log != "" {
				t.Fatalf("disabled maintenance reached Dolt:\n%s", log)
			}
			if log := readCompactGCLog(t, fixture); !strings.Contains(log, "gc --city "+fixture.cityPath+" config show --json") {
				t.Fatalf("must resolve the target city's config:\n%s", log)
			}
		})
	}
}

func TestCompactScriptMaintenanceConfigFailure(t *testing.T) {
	for _, config := range []string{"", "not json", `{}`, `{"config":{"Maintenance":{"Dolt":{"Enabled":"false"}}}}`, "command-failed"} {
		t.Run(config, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			writeCompactConfig(t, fixture, config)
			if config == "command-failed" {
				if err := os.WriteFile(filepath.Join(fixture.cityPath, "compact-config-fail"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err == nil || !strings.Contains(out, "maintenance.dolt") {
				t.Fatalf("unreadable flag must fail closed: %v\n%s", err, out)
			}
			if log := readCompactDoltLog(t, fixture); log != "" {
				t.Fatalf("unreadable flag reached Dolt:\n%s", log)
			}
		})
	}
}

func TestCompactScriptForceOverridesMaintenanceDisabled(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	writeCompactConfig(t, fixture, `{"config":{"Maintenance":{"Dolt":{"Enabled":false}}}}`)
	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_DOLT_COMPACT_FORCE=1")
	if err != nil || !strings.Contains(out, "flattening...") {
		t.Fatalf("force should permit a manual run: %v\n%s", err, out)
	}
	if log := readCompactDoltLog(t, fixture); !strings.Contains(log, "DOLT_RESET") || !strings.Contains(log, "DOLT_GC") {
		t.Fatalf("force did not compact:\n%s", log)
	}
}

// Any remote protects history, independently of sync policy or remote selection.
func TestCompactScriptFederatedGuard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   string
		remote string
		noSync bool
		marker string
		args   []string
		env    []string
	}{
		{name: "origin", mode: "remote_success", remote: "origin"},
		{name: "non-origin", mode: "explicit_backup_remote", remote: "backup"},
		{name: "multiple", mode: "multiple_remotes_no_origin", remote: "backup"},
		{name: "no-sync", mode: "remote_success", remote: "origin", noSync: true},
		{name: "skip-fetch", mode: "remote_success", remote: "origin", args: []string{"--skip-fetch"}},
		{name: "dry-run", mode: "remote_success", remote: "origin", args: []string{"--dry-run"}},
		{name: "force", mode: "remote_success", remote: "origin", env: []string{"GC_DOLT_COMPACT_FORCE=1"}},
		{name: "pending-push", mode: "remote_success", remote: "origin", marker: "compact-pending-push"},
		{name: "pending-gc", mode: "remote_success", remote: "origin", marker: "compact-pending-gc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			if tc.noSync {
				writeNoSyncMarker(t, fixture.dataDir)
			}
			markerPath := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", tc.marker, "beads")
			const markerData = "remote=origin\nreason=deferred from previous flatten\n"
			if tc.marker != "" {
				if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(markerPath, []byte(markerData), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			env := append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"}, tc.env...)
			out, err := fixture.runWithArgs(t, tc.mode, tc.args, env...)
			if err != nil || !strings.Contains(out, "db=beads remote="+tc.remote) || !strings.Contains(out, "skipping flatten") {
				t.Fatalf("federated database must skip flatten: %v\n%s", err, out)
			}
			if log := readCompactDoltLog(t, fixture); strings.Contains(log, "CALL ") {
				t.Fatalf("federated guard allowed a mutation:\n%s", log)
			}
			if tc.marker != "" {
				data, err := os.ReadFile(markerPath)
				if err != nil || string(data) != markerData {
					t.Fatalf("guard changed deferred marker: %v\n%s", err, data)
				}
			}
		})
	}
}

func TestCompactScriptFederatedProbeFailure(t *testing.T) {
	for _, mode := range []string{"remote_count_failure", "remote_count_invalid", "remote_name_failure", "remote_name_empty"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, mode, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err == nil || !strings.Contains(out, "probe") {
				t.Fatalf("remote probe must fail closed: %v\n%s", err, out)
			}
			if log := readCompactDoltLog(t, fixture); strings.Contains(log, "CALL ") {
				t.Fatalf("failed remote probe allowed a mutation:\n%s", log)
			}
		})
	}
}

func TestCompactScriptFederatedReclaim(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  []string
	}{
		{name: "bare-gc", env: []string{"GC_DOLT_COMPACT_BARE_GC=1"}},
		{name: "gc-only", args: []string{"--gc-only"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.runWithArgs(t, "remote_success", tc.args, tc.env...)
			if err != nil {
				t.Fatalf("federated reclaim failed: %v\n%s", err, out)
			}
			log := readCompactDoltLog(t, fixture)
			if !strings.Contains(log, "DOLT_GC") || strings.Contains(log, "DOLT_RESET") || strings.Contains(log, "DOLT_PUSH") {
				t.Fatalf("reclaim must GC without rewriting history or pushing:\n%s", log)
			}
		})
	}
}

func writeCompactConfig(t *testing.T, fixture compactScriptFixture, config string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.cityPath, "compact-config.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCompactDoltLog(t *testing.T, fixture compactScriptFixture) string {
	t.Helper()
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}
