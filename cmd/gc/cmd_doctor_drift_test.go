package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

func managedCityDriftFixture(t *testing.T, rigName string) (cityDir, rigDir, managedPort string, cfg *config.City) {
	t.Helper()
	cityDir = t.TempDir()
	rigDir = filepath.Join(t.TempDir(), rigName)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	managedPort = strconv.Itoa(port)

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	writeCanonicalScopeConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(cityDir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "gc",
	}); err != nil {
		t.Fatal(err)
	}

	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "r",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "r",
	}); err != nil {
		t.Fatal(err)
	}

	cfg = &config.City{
		Workspace: config.Workspace{Name: "drift-test"},
		Rigs:      []config.Rig{{Name: rigName, Path: rigDir, Prefix: "r"}},
	}
	return cityDir, rigDir, managedPort, cfg
}

func writeSQLServerInfo(t *testing.T, rigDir string, pid, port int) {
	t.Helper()
	dir := filepath.Join(rigDir, ".dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("%d:%d:dead-beef-cafe-feed\n", pid, port)
	if err := os.WriteFile(filepath.Join(dir, "sql-server.info"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDoltDriftCheckCleanManagedCityIsOK(t *testing.T) {
	cityDir, rigDir, managedPort, cfg := managedCityDriftFixture(t, "clean")
	// Port file matches canonical managed port — no drift.
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte(managedPort+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newDoltDriftCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() status = %v, want StatusOK; message=%q details=%v", r.Status, r.Message, r.Details)
	}
}

func TestDoltDriftCheckDetectsLiveRigLocalDolt(t *testing.T) {
	cityDir, rigDir, _, cfg := managedCityDriftFixture(t, "runner")
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })
	rigLocalPort := ln.Addr().(*net.TCPAddr).Port
	// Write sql-server.info with our own PID and a port held by this process.
	writeSQLServerInfo(t, rigDir, os.Getpid(), rigLocalPort)

	r := newDoltDriftCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want StatusError; message=%q details=%v", r.Status, r.Message, r.Details)
	}
	joined := r.Message + " " + strings.Join(r.Details, " ") + " " + r.FixHint
	if !strings.Contains(joined, "runner") {
		t.Errorf("want rig name in message/details, got:\n%s", joined)
	}
	if !strings.Contains(joined, "rig-local Dolt") && !strings.Contains(joined, "sql-server.info") {
		t.Errorf("want rig-local Dolt mention in details, got:\n%s", joined)
	}
	if !strings.Contains(joined, "--self --port") || !strings.Contains(joined, "--force") {
		t.Errorf("want explicit --self --port --force remediation, got:\n%s", joined)
	}
	if strings.Contains(joined, "--inherit") {
		t.Errorf("want no --inherit no-op remediation for inherited rig, got:\n%s", joined)
	}
}

func TestDoltDriftCheckTreatsLivePIDWithoutMatchingPortAsStale(t *testing.T) {
	cityDir, rigDir, _, cfg := managedCityDriftFixture(t, "reused")
	unusedPort := reserveRandomTCPPort(t)
	// The PID is live, but it is not the process listening on the recorded
	// sql-server.info port. Treat it as stale instead of a live rig-local Dolt.
	writeSQLServerInfo(t, rigDir, os.Getpid(), unusedPort)

	r := newDoltDriftCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("Run() status = %v, want StatusWarning; message=%q details=%v", r.Status, r.Message, r.Details)
	}
	joined := r.Message + " " + strings.Join(r.Details, " ") + " " + r.FixHint
	if !strings.Contains(joined, "stale") {
		t.Errorf("want stale classification, got:\n%s", joined)
	}
	if strings.Contains(joined, "endpoint_origin=inherited_city") {
		t.Errorf("want no live rig-local error for reused PID, got:\n%s", joined)
	}
	if strings.Contains(joined, "PID is not alive") {
		t.Errorf("want stale hint to account for live PID with mismatched port, got:\n%s", joined)
	}
}

func TestDoltDriftCheckDetectsStaleRigLocalInfo(t *testing.T) {
	cityDir, rigDir, _, cfg := managedCityDriftFixture(t, "stale")
	// Use a PID that is extremely unlikely to be alive.
	writeSQLServerInfo(t, rigDir, 2147483640, 28233)

	r := newDoltDriftCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("Run() status = %v, want StatusWarning; message=%q details=%v", r.Status, r.Message, r.Details)
	}
	joined := r.Message + " " + strings.Join(r.Details, " ")
	if !strings.Contains(joined, "stale") {
		t.Errorf("want 'stale' in result, got:\n%s", joined)
	}
	if !strings.Contains(joined, "pid 2147483640") {
		t.Errorf("want stale PID detail, got:\n%s", joined)
	}
}

func TestDoltDriftCheckDetectsPortFileDrift(t *testing.T) {
	cityDir, rigDir, managedPort, cfg := managedCityDriftFixture(t, "drifted")
	const stalePort = "29999"
	if stalePort == managedPort {
		t.Fatalf("test fixture picked the stale port %s", managedPort)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte(stalePort+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := newDoltDriftCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want StatusError; message=%q details=%v", r.Status, r.Message, r.Details)
	}
	joined := r.Message + " " + strings.Join(r.Details, " ")
	if !strings.Contains(joined, "drifted") {
		t.Errorf("want rig name in result, got:\n%s", joined)
	}
	if !strings.Contains(joined, stalePort) || !strings.Contains(joined, managedPort) {
		t.Errorf("want both stale %s and managed %s ports in result, got:\n%s", stalePort, managedPort, joined)
	}
}

func TestDoltDriftCheckNoRigsIsOK(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "empty"}}
	r := newDoltDriftCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() status = %v, want StatusOK", r.Status)
	}
}

func TestRigLocalDoltPIDFromSQLServerInfoParsesColonFormat(t *testing.T) {
	dir := t.TempDir()
	const parsedPID = 2147483639
	writeSQLServerInfo(t, dir, parsedPID, 28232)
	pid, port, exists, alive := rigLocalDoltPIDFromSQLServerInfo(dir)
	if !exists {
		t.Fatalf("infoExists = false, want true")
	}
	if pid != parsedPID {
		t.Errorf("pid = %d, want %d", pid, parsedPID)
	}
	if port != 28232 {
		t.Errorf("port = %d, want 28232", port)
	}
	if alive {
		t.Errorf("pidAliveNow = true, want false when recorded PID is not tied to the port")
	}
}

func TestRigLocalDoltPIDFromSQLServerInfoMissingFile(t *testing.T) {
	dir := t.TempDir()
	pid, port, exists, alive := rigLocalDoltPIDFromSQLServerInfo(dir)
	if exists {
		t.Errorf("infoExists = true, want false when file is absent")
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0", pid)
	}
	if port != 0 {
		t.Errorf("port = %d, want 0", port)
	}
	if alive {
		t.Errorf("pidAliveNow = true, want false when file absent")
	}
}
