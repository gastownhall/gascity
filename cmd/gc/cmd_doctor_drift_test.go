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

func TestDoltDriftCheckUsesProviderStateWhenPublishedStateIsMissing(t *testing.T) {
	cityDir, rigDir, managedPort, cfg := managedCityDriftFixture(t, "provider")
	state, err := readDoltRuntimeStateFile(managedDoltStatePath(cityDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityDir), state); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(managedDoltStatePath(cityDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := newDoltDriftCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want StatusError; message=%q details=%v", r.Status, r.Message, r.Details)
	}
	joined := r.Message + " " + strings.Join(r.Details, " ")
	if !strings.Contains(joined, managedPort) {
		t.Fatalf("drift output = %q, want managed provider-state port %s", joined, managedPort)
	}
	if _, err := os.Stat(managedDoltStatePath(cityDir)); !os.IsNotExist(err) {
		t.Fatalf("drift check should not publish runtime state, stat err = %v", err)
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

	check := newDoltDriftCheck(cityDir, cfg)
	r := check.Run(&doctor.CheckContext{CityPath: cityDir})
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
	// Pure port-file drift (no live rig-local server) is re-pinnable, so the
	// doctor offers `--fix`.
	if !check.CanFix() {
		t.Errorf("CanFix() = false after pure port-file drift, want true (re-pin is safe)")
	}
}

// TestDoltDriftCheckCanFixGating pins the fix-eligibility decision: re-pinning
// is offered only for port-file drift (Case B) and is suppressed when a live
// rig-local Dolt (Case A) would make re-pinning point at the wrong server.
func TestDoltDriftCheckCanFixGating(t *testing.T) {
	c := &doltDriftCheck{}
	if c.CanFix() {
		t.Error("CanFix() = true with no drift recorded, want false")
	}
	c.repinnablePortDrift = true
	if !c.CanFix() {
		t.Error("CanFix() = false with re-pinnable port drift and no live rig-local, want true")
	}
	c.liveRigLocalBlocking = true
	if c.CanFix() {
		t.Error("CanFix() = true with a live rig-local Dolt present, want false (re-pin unsafe)")
	}
}

// TestDoltDriftCheckSkipsEmbeddedRigPortFile pins the doctor half of the
// ga-p9iuv contract: an embedded rig serves its own store, so its
// .beads/dolt-server.port is bd's (or the operator's), not a mirror of the
// managed city port. A port file there that disagrees with the managed port is
// no drift — reporting it would fail `gc doctor` with a re-pin that `--fix`
// never performs, because the port-file sync skips embedded rigs.
func TestDoltDriftCheckSkipsEmbeddedRigPortFile(t *testing.T) {
	cityPath, rigPath, cfg := legacyManagedCityWithEmbeddedRig(t, cleanEmbeddedRigConfig)
	managedPort := publishManagedCityDoltPort(t, cityPath)
	const rigPort = "3310\n"
	if strings.TrimSpace(rigPort) == managedPort {
		t.Fatalf("test fixture picked the rig port %s", managedPort)
	}
	portFile := filepath.Join(rigPath, ".beads", "dolt-server.port")
	if err := os.WriteFile(portFile, []byte(rigPort), 0o644); err != nil {
		t.Fatal(err)
	}

	check := newDoltDriftCheck(cityPath, cfg)
	r := check.Run(&doctor.CheckContext{CityPath: cityPath})
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() status = %v, want StatusOK for an embedded rig; message=%q details=%v", r.Status, r.Message, r.Details)
	}
	if check.CanFix() {
		t.Error("CanFix() = true for an embedded rig's port file, want false (the re-pin sync skips embedded rigs)")
	}
	got, err := os.ReadFile(portFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != rigPort {
		t.Errorf("embedded rig port file = %q, want it left byte-identical at %q", got, rigPort)
	}
}

// TestDoltDriftCheckFixConvergesBesideEmbeddedRig drives `gc doctor --fix`
// through the doctor framework on a city holding both kinds of rig. The server
// rig's stale port file is still reported and re-pinned, while the embedded
// rig, listed first, neither hides it nor fails the verifying re-run, and keeps
// its own port file.
func TestDoltDriftCheckFixConvergesBesideEmbeddedRig(t *testing.T) {
	cityDir, serverRigDir, managedPort, cfg := managedCityDriftFixture(t, "drifted")
	const stalePort = "29999\n"
	const embeddedPort = "3310\n"
	for _, port := range []string{stalePort, embeddedPort} {
		if strings.TrimSpace(port) == managedPort {
			t.Fatalf("test fixture picked the fixed port %s", managedPort)
		}
	}
	serverPortFile := filepath.Join(serverRigDir, ".beads", "dolt-server.port")
	if err := os.WriteFile(serverPortFile, []byte(stalePort), 0o644); err != nil {
		t.Fatal(err)
	}
	embeddedRigDir := embeddedScopeWithBeads(t, "fr")
	if err := os.WriteFile(filepath.Join(embeddedRigDir, ".beads", "config.yaml"), []byte(cleanEmbeddedRigConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	embeddedPortFile := filepath.Join(embeddedRigDir, ".beads", "dolt-server.port")
	if err := os.WriteFile(embeddedPortFile, []byte(embeddedPort), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Rigs = append([]config.Rig{{Name: "frontend", Path: embeddedRigDir, Prefix: "fr"}}, cfg.Rigs...)

	check := newDoltDriftCheck(cityDir, cfg)
	first := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if first.Status != doctor.StatusError || len(first.Details) != 1 || !strings.Contains(first.Details[0], `rig "drifted"`) {
		t.Fatalf("Run() = status %v details %v, want exactly the server rig's port-file drift", first.Status, first.Details)
	}
	if !check.CanFix() {
		t.Fatal("CanFix() = false for the server rig's port-file drift, want true")
	}

	d := &doctor.Doctor{}
	d.Register(check)
	report := d.RunCollect(&doctor.CheckContext{CityPath: cityDir}, true)
	if report.Fixed != 1 || report.Failed != 0 {
		res := report.Results[0]
		t.Fatalf("doctor --fix: fixed=%d failed=%d, want the drift fixed; status=%v details=%v fixErr=%q", report.Fixed, report.Failed, res.Status, res.Details, res.FixError)
	}
	got, err := os.ReadFile(serverPortFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != managedPort {
		t.Errorf("server rig port file = %q, want it re-pinned to the managed port %s", got, managedPort)
	}
	got, err = os.ReadFile(embeddedPortFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != embeddedPort {
		t.Errorf("embedded rig port file = %q, want it left byte-identical at %q", got, embeddedPort)
	}
}

// TestDoltDriftCheckReportsUnclassifiableRigStorage pins the fail-closed arm of
// the embedded skip: a rig whose metadata.json cannot be read is reported, not
// guessed into either mode — skipping it as embedded would hide real drift, and
// treating it as a server rig would offer a re-pin the sync refuses.
func TestDoltDriftCheckReportsUnclassifiableRigStorage(t *testing.T) {
	cityDir, rigDir, _, cfg := managedCityDriftFixture(t, "unreadable")
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte("29999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory where metadata.json belongs still marks a bd scope (the
	// contract stats it) but fails every read.
	metadataPath := filepath.Join(rigDir, ".beads", "metadata.json")
	if err := os.Remove(metadataPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(metadataPath, 0o755); err != nil {
		t.Fatal(err)
	}

	check := newDoltDriftCheck(cityDir, cfg)
	r := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want StatusError; message=%q details=%v", r.Status, r.Message, r.Details)
	}
	if joined := strings.Join(r.Details, " "); !strings.Contains(joined, `rig "unreadable" classify storage mode`) {
		t.Errorf("details = %q, want the rig's storage-mode classification failure", joined)
	}
	if check.CanFix() {
		t.Error("CanFix() = true for a rig whose storage mode is unknown, want false")
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
