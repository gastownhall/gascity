//go:build acceptance_a

// An agent session's environment is fixed when it spawns, and bd ranks an env
// Dolt port above the scope's .beads/dolt-server.port mirror. A session on a
// gc-managed city that carried the port it was spawned with kept dialing that
// port after the city's Dolt came back somewhere else, until the session was
// respawned. This records a real session's env at spawn, moves the managed
// Dolt to a new port by holding the old one across a restart, and runs bd with
// exactly that recorded env: it has to reach the store through the
// republished mirror.
//
// gc runs the Dolt server itself only for a city an older gc created (the
// legacy shape); the bd-owned shapes never carry a port in session env. So
// this needs GC_ACCEPTANCE_LEGACY_GC_BIN and skips without it, as the
// topology matrix's legacy row does.
package acceptance_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
	"github.com/gastownhall/gascity/test/toolhome"
)

func TestManagedDirectSessionBdFollowsMovedDoltPort(t *testing.T) {
	bdPath, doltPath := helpers.RequireTopologyTooling(t)
	var topo helpers.BeadsTopology
	for _, candidate := range helpers.BeadsTopologies() {
		if candidate.Name == "M5-legacy-gc-managed" {
			topo = candidate
		}
	}
	if topo.Name == "" {
		t.Fatal("no M5-legacy-gc-managed topology")
	}
	run := helpers.StartTopology(t, testEnv, topo, bdPath, doltPath)

	envFile := filepath.Join(run.Root, "session.env")
	probe := fmt.Sprintf("env -0 > %[1]s.tmp && mv -f %[1]s.tmp %[1]s && exec sleep 3600", envFile)
	agentDir := filepath.Join(run.City.Dir, "agents", "envprobe")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("create envprobe agent dir: %v", err)
	}
	agentTOML := fmt.Sprintf("scope = \"city\"\nstart_command = %q\n", "sh -c '"+probe+"'")
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte(agentTOML), 0o644); err != nil {
		t.Fatalf("write envprobe agent.toml: %v", err)
	}
	// The retry window line lands in the [daemon] table StartTopology appended
	// last. Without it the restart waits out the default window on the held
	// port before it moves on.
	appendCityTOML(t, run.City.Dir, `dolt_start_address_in_use_retry_window = "0s"

[[named_session]]
template = "envprobe"
mode = "always"
`)

	run.Start(t)
	if !run.City.WaitForCondition(func() bool { _, err := os.Stat(envFile); return err == nil }, 3*time.Minute) {
		t.Fatalf("the envprobe session never recorded its env at %s", envFile)
	}
	recorded, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read recorded session env: %v", err)
	}
	sessionEnv := strings.Split(strings.TrimSuffix(string(recorded), "\x00"), "\x00")
	oldPort := readCityDoltPortMirror(t, run.City.Dir)
	t.Logf("session spawned against Dolt port %s; Dolt env it carries: %s", oldPort, doltEnvSummary(sessionEnv))

	if out, err := run.Stop(); err != nil {
		t.Fatalf("gc stop: %v\n%s", err, out)
	}
	if leaked := helpers.WaitForNoDoltProcesses(t, run.Root, 30*time.Second); len(leaked) != 0 {
		t.Fatalf("processes survived gc stop:\n%s", strings.Join(leaked, "\n"))
	}
	// Holding the old port across the restart is what moves the server: the
	// start cannot bind it and goes to the next free port.
	hold, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", oldPort))
	if err != nil {
		t.Fatalf("hold old Dolt port %s: %v", oldPort, err)
	}
	run.Start(t)
	newPort := readCityDoltPortMirror(t, run.City.Dir)
	// Released so a bd still dialing the old port is refused at once instead
	// of waiting on a listener that never speaks MySQL.
	if err := hold.Close(); err != nil {
		t.Fatalf("release old Dolt port %s: %v", oldPort, err)
	}
	if newPort == oldPort {
		t.Fatalf("the restarted Dolt came back on the held port %s", oldPort)
	}
	t.Logf("Dolt moved from port %s to %s", oldPort, newPort)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	home := filepath.Join(helpers.TempDir(t), "bd-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("create bd home: %v", err)
	}
	cmd := exec.CommandContext(ctx, bdPath, "list", "--json") //nolint:gosec // resolved test binary
	cmd.Dir = run.City.Dir
	// The session's own env, re-homed only so bd cannot read the invoking
	// user's ~/.beads: every BEADS_*/BD_* value the session carried is kept.
	cmd.Env = toolhome.Environ(sessionEnv, home, toolhome.Explicit(sessionEnv)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("bd list under the session's spawn-time env after Dolt moved %s -> %s: %v\n%s", oldPort, newPort, err, out.String())
	}
}

// appendCityTOML appends text to the city's city.toml.
func appendCityTOML(t *testing.T, cityDir, text string) {
	t.Helper()
	path := filepath.Join(cityDir, "city.toml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	if err := os.WriteFile(path, append(existing, text...), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
}

// readCityDoltPortMirror returns the port the city's .beads/dolt-server.port
// mirror publishes.
func readCityDoltPortMirror(t *testing.T, cityDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cityDir, ".beads", "dolt-server.port"))
	if err != nil {
		t.Fatalf("read the city's Dolt port mirror: %v", err)
	}
	port := strings.TrimSpace(string(data))
	if port == "" {
		t.Fatal("the city's Dolt port mirror is empty")
	}
	return port
}

// doltEnvSummary lists the Dolt connection keys in env, for the log.
func doltEnvSummary(env []string) string {
	var keys []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if strings.Contains(name, "DOLT") {
			keys = append(keys, kv)
		}
	}
	return strings.Join(keys, " ")
}
