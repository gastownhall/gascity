package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoltConfigWriteManagedCmd(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "packs", "dolt", "dolt-config.yaml")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"dolt-config", "write-managed",
		"--file", configPath,
		"--host", "127.0.0.1",
		"--port", "3311",
		"--data-dir", "/tmp/city/.beads/dolt",
		"--log-level", "warning",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", configPath, err)
	}
	text := string(data)
	for _, want := range []string{
		"log_level: warning",
		"port: 3311",
		"host: 127.0.0.1",
		`data_dir: "/tmp/city/.beads/dolt"`,
		"archive_level: 1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
}

func TestDoltConfigNormalizeScopeCmd(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(rig .beads): %v", err)
	}
	cityToml := `[workspace]
name = "gascity"
prefix = "gc"

[beads]
provider = "bd"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(`{"database":"legacy","backend":"legacy","dolt_mode":"embedded","dolt_database":"wrong-db","dolt_server_host":"127.0.0.1","dolt_server_port":"3307"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(metadata.json): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("issue-prefix: stale\ndolt.auto-start: true\ndolt_server_port: 3307\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(config.yaml): %v", err)
	}
	for _, name := range []string{"dolt-server.pid", "dolt-server.lock", "dolt-server.log", "dolt-server.port"} {
		if err := os.WriteFile(filepath.Join(rigPath, ".beads", name), []byte("stale\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"dolt-config", "normalize-scope",
		"--city", cityPath,
		"--dir", rigPath,
		"--prefix", "fe",
		"--dolt-database", "fe",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}

	metaData, err := os.ReadFile(filepath.Join(rigPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("ReadFile(metadata.json): %v", err)
	}
	metaText := string(metaData)
	for _, want := range []string{`"database": "dolt"`, `"backend": "dolt"`, `"dolt_mode": "server"`, `"dolt_database": "fe"`} {
		if !strings.Contains(metaText, want) {
			t.Fatalf("metadata missing %q:\n%s", want, metaText)
		}
	}
	for _, forbidden := range []string{"dolt_server_host", "dolt_server_port", "dolt_host", "dolt_port", "wrong-db"} {
		if strings.Contains(metaText, forbidden) {
			t.Fatalf("metadata still contains %q:\n%s", forbidden, metaText)
		}
	}

	cfgData, err := os.ReadFile(filepath.Join(rigPath, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(config.yaml): %v", err)
	}
	cfgText := string(cfgData)
	for _, want := range []string{"issue_prefix: fe", "gc.endpoint_origin: inherited_city", "gc.endpoint_status: verified"} {
		if !strings.Contains(cfgText, want) {
			t.Fatalf("config missing %q:\n%s", want, cfgText)
		}
	}
	for _, forbidden := range []string{"dolt.host:", "dolt.port:", "dolt_server_port"} {
		if strings.Contains(cfgText, forbidden) {
			t.Fatalf("config still contains %q:\n%s", forbidden, cfgText)
		}
	}

	for _, name := range []string{"dolt-server.pid", "dolt-server.lock", "dolt-server.log", "dolt-server.port"} {
		if _, err := os.Stat(filepath.Join(rigPath, ".beads", name)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists, stat err = %v", name, err)
		}
	}
}

func TestDoltStateWriteProviderCmd(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "packs", "dolt", "dolt-provider-state.json")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"dolt-state", "write-provider",
		"--file", statePath,
		"--pid", "1234",
		"--running", "true",
		"--port", "3311",
		"--data-dir", "/tmp/city/.beads/dolt",
		"--started-at", "2026-04-14T00:00:00Z",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	state, err := readDoltRuntimeStateFile(statePath)
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile(%s): %v", statePath, err)
	}
	if !state.Running || state.PID != 1234 || state.Port != 3311 || state.DataDir != "/tmp/city/.beads/dolt" || state.StartedAt != "2026-04-14T00:00:00Z" {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestDoltStateReadProviderCmd(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "packs", "dolt", "dolt-provider-state.json")
	if err := writeDoltRuntimeStateFile(statePath, doltRuntimeState{
		Running:   true,
		PID:       1234,
		Port:      3311,
		DataDir:   "/tmp/city/.beads/dolt",
		StartedAt: "2026-04-14T00:00:00Z",
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"dolt-state", "read-provider",
		"--file", statePath,
		"--field", "data_dir",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "/tmp/city/.beads/dolt\n" {
		t.Fatalf("stdout = %q", got)
	}
}
