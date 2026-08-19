//go:build integration

package doctor

import (
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// startDoctorTestDoltServer launches a throwaway dolt sql-server for
// RigStoreBindingCheck integration tests. Unlike production managed-dolt
// servers, it creates no databases — callers create exactly the databases
// each scenario needs, so "database was never created" is reproducible.
func startDoctorTestDoltServer(t *testing.T) int {
	t.Helper()
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt binary not in PATH")
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	if err := lis.Close(); err != nil {
		t.Fatalf("release test port: %v", err)
	}

	dataDir := t.TempDir()
	cmd := exec.Command(doltBin, "sql-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--data-dir", dataDir)
	cmd.Env = append(os.Environ(), "DOLT_ROOT_PATH="+dataDir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port))
	if err != nil {
		t.Fatalf("open dolt connection: %v", err)
	}
	defer func() { _ = db.Close() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := db.Ping(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server did not become ready on port %d", port)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return port
}

// execOnTestDoltServer runs a statement against the throwaway server on a
// fresh connection, for test setup/mutation (creating databases, tables, rows).
func execOnTestDoltServer(t *testing.T, port int, database, query string, args ...any) {
	t.Helper()
	dsn := fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s", port, database)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open dolt connection: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// writeInheritedRigFixture configures a city with an explicit canonical Dolt
// endpoint and a rig that inherits it — the exact shape of the 2026-08-10
// incident's affected rigs (tincan, ProjectWrenUnity), where the shared
// server was reachable but the rig's own database was missing from it.
func writeInheritedRigFixture(t *testing.T, fs fsys.FS, cityDir, rigDir, rigPrefix, rigDatabase, host string, port int) {
	t.Helper()
	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       host,
		DoltPort:       strconv.Itoa(port),
	})
	writeDoctorCanonicalMetadata(t, fs, cityDir, "hq")
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    rigPrefix,
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       host,
		DoltPort:       strconv.Itoa(port),
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, rigDatabase)
}

// TestRigStoreBindingCheck_DatabaseAbsentFails reproduces the 2026-08-10
// incident's undetected failure mode: a rig whose Dolt endpoint is
// reachable (TCP-dial checks pass) but whose configured database was never
// created on that server. RigDoltServerCheck skips inherited-endpoint rigs
// entirely (reachability of the shared server says nothing about whether
// this rig's database exists on it), so nothing else in `gc doctor` catches
// this.
func TestRigStoreBindingCheck_DatabaseAbsentFails(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}
	port := startDoctorTestDoltServer(t)

	writeInheritedRigFixture(t, fs, cityDir, rigDir, "de", "demo", "127.0.0.1", port)

	c := NewRigStoreBindingCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %v, want Error; msg = %s", r.Status, r.Message)
	}
	for _, want := range []string{"demo", "127.0.0.1:" + strconv.Itoa(port)} {
		if !strings.Contains(r.Message, want) {
			t.Fatalf("message = %q, want it to contain %q", r.Message, want)
		}
	}
}

// TestRigStoreBindingCheck_DatabaseRestoredPasses proves the check has teeth:
// the same check instance that failed against a missing database must pass
// once the database is created and populated, per ga-e5lyfu's done-when
// requirement to mutate and re-assert rather than only assert the failure.
func TestRigStoreBindingCheck_DatabaseRestoredPasses(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}
	port := startDoctorTestDoltServer(t)

	writeInheritedRigFixture(t, fs, cityDir, rigDir, "de", "demo", "127.0.0.1", port)

	c := NewRigStoreBindingCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false)
	if r := c.Run(&CheckContext{}); r.Status != StatusError {
		t.Fatalf("status before restore = %v, want Error; msg = %s", r.Status, r.Message)
	}

	execOnTestDoltServer(t, port, "", "CREATE DATABASE demo")
	execOnTestDoltServer(t, port, "demo", "CREATE TABLE issues (id VARCHAR(64) PRIMARY KEY, created_at DATETIME NOT NULL)")
	execOnTestDoltServer(t, port, "demo", "INSERT INTO issues (id, created_at) VALUES ('demo-1', NOW())")

	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status after restore = %v, want OK; msg = %s", r.Status, r.Message)
	}
}

// writeRigExportFixture writes a JSONL export baseline for rigName under
// cityDir's .beads-jsonl-export directory, matching the filename pattern
// latestAcceptedRigExport expects (<rigName>-<suffix>.jsonl).
func writeRigExportFixture(t *testing.T, cityDir, rigName string, createdAts []string) string {
	t.Helper()
	dir := filepath.Join(cityDir, ".beads-jsonl-export", rigName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir export dir: %v", err)
	}
	path := filepath.Join(dir, rigName+"-2026-08-10.jsonl")
	var buf strings.Builder
	for i, ts := range createdAts {
		fmt.Fprintf(&buf, "{\"id\":\"%s-%d\",\"created_at\":%q}\n", rigName, i, ts)
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		t.Fatalf("write export fixture: %v", err)
	}
	return path
}

// TestRigStoreBindingCheck_EmptyStoreUnderExportBaselineFails reproduces the
// done-when scenario distinct from a missing database: the database exists
// and is reachable, but its issues table is empty while the rig's last
// accepted JSONL export has real content — the store collapsed after
// creation rather than never being created.
func TestRigStoreBindingCheck_EmptyStoreUnderExportBaselineFails(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}
	port := startDoctorTestDoltServer(t)

	writeInheritedRigFixture(t, fs, cityDir, rigDir, "de", "demo", "127.0.0.1", port)
	execOnTestDoltServer(t, port, "", "CREATE DATABASE demo")
	execOnTestDoltServer(t, port, "demo", "CREATE TABLE issues (id VARCHAR(64) PRIMARY KEY, created_at DATETIME NOT NULL)")

	writeRigExportFixture(t, cityDir, "demo", []string{
		"2026-08-09T12:00:00Z", "2026-08-09T13:00:00Z", "2026-08-09T14:00:00Z",
	})

	c := NewRigStoreBindingCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %v, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "empty") {
		t.Fatalf("message = %q, want it to mention the empty issues table", r.Message)
	}
}

// TestRigStoreBindingCheck_FrozenStoreWarns proves the done-when frozen-store
// signature: the live store holds data consistent with the export (no
// collapse), but its newest row is no newer than the export baseline's
// newest row — a sign writes may have silently stopped landing here.
func TestRigStoreBindingCheck_FrozenStoreWarns(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}
	port := startDoctorTestDoltServer(t)

	writeInheritedRigFixture(t, fs, cityDir, rigDir, "de", "demo", "127.0.0.1", port)
	execOnTestDoltServer(t, port, "", "CREATE DATABASE demo")
	execOnTestDoltServer(t, port, "demo", "CREATE TABLE issues (id VARCHAR(64) PRIMARY KEY, created_at DATETIME NOT NULL)")
	for i := 0; i < 3; i++ {
		execOnTestDoltServer(t, port, "demo", "INSERT INTO issues (id, created_at) VALUES (?, ?)", fmt.Sprintf("demo-%d", i), "2020-01-01 00:00:00")
	}

	writeRigExportFixture(t, cityDir, "demo", []string{
		"2024-06-01T00:00:00Z", "2024-06-01T01:00:00Z", "2024-06-01T02:00:00Z",
	})

	c := NewRigStoreBindingCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "frozen") {
		t.Fatalf("message = %q, want it to mention frozen", r.Message)
	}
}

// TestRigStoreBindingCheck_HealthyStorePasses proves the OK path: a live
// store with enough issues and a freshest row newer than the export
// baseline reports StatusOK, not just the failure/warning paths.
func TestRigStoreBindingCheck_HealthyStorePasses(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}
	port := startDoctorTestDoltServer(t)

	writeInheritedRigFixture(t, fs, cityDir, rigDir, "de", "demo", "127.0.0.1", port)
	execOnTestDoltServer(t, port, "", "CREATE DATABASE demo")
	execOnTestDoltServer(t, port, "demo", "CREATE TABLE issues (id VARCHAR(64) PRIMARY KEY, created_at DATETIME NOT NULL)")
	for i := 0; i < 5; i++ {
		execOnTestDoltServer(t, port, "demo", "INSERT INTO issues (id, created_at) VALUES (?, ?)", fmt.Sprintf("demo-%d", i), "2026-08-14 00:00:00")
	}

	writeRigExportFixture(t, cityDir, "demo", []string{
		"2026-08-09T12:00:00Z", "2026-08-09T13:00:00Z", "2026-08-09T14:00:00Z",
	})

	c := NewRigStoreBindingCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s", r.Status, r.Message)
	}
}
