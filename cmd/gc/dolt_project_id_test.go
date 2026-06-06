package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestEnsureManagedDoltProjectIDGeneratesLocalIdentityWhenMetadataAndDatabaseMissing(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	doltPath := os.Getenv("GC_DOLT_REAL_BINARY")
	var err error
	if doltPath == "" {
		doltPath, err = exec.LookPath("dolt")
		if err != nil {
			t.Skip("dolt not installed")
		}
	}
	bdPath := waitTestRealBDPath(t)

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeBuiltinPacks(cityDir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(homeDir, ".gitconfig")
	if err := os.WriteFile(gitConfig, []byte("[user]\n\tname = Test User\n\temail = test@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)))

	if err := ensureBeadsProvider(cityDir); err != nil {
		t.Fatalf("ensureBeadsProvider: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdownBeadsProvider(cityDir)
	})
	if err := initAndHookDir(cityDir, cityDir, "gc"); err != nil {
		t.Fatalf("initAndHookDir(city): %v", err)
	}

	portData, err := os.ReadFile(filepath.Join(cityDir, ".beads", "dolt-server.port"))
	if err != nil {
		t.Fatalf("ReadFile(dolt-server.port): %v", err)
	}
	port := strings.TrimSpace(string(portData))
	if port == "" {
		t.Fatal("dolt-server.port empty")
	}

	metadataPath := filepath.Join(cityDir, ".beads", "metadata.json")
	metadataData, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("ReadFile(metadata.json): %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metadataData, &meta); err != nil {
		t.Fatalf("Unmarshal(metadata.json): %v", err)
	}
	delete(meta, "project_id")
	patched, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(metadata.json): %v", err)
	}
	patched = append(patched, '\n')
	if err := os.WriteFile(metadataPath, patched, 0o644); err != nil {
		t.Fatalf("WriteFile(metadata.json): %v", err)
	}
	if err := os.Remove(contract.ProjectIdentityPath(cityDir)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("Remove(identity.toml): %v", err)
	}

	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%s)/hq", port))
	if err != nil {
		t.Fatalf("sql.Open(hq): %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "DELETE FROM metadata WHERE `key` = '_project_id'"); err != nil {
		t.Fatalf("delete database _project_id: %v", err)
	}

	report, err := ensureManagedDoltProjectID(metadataPath, port)
	if err != nil {
		t.Fatalf("ensureManagedDoltProjectID: %v", err)
	}
	if report.Source != "generated" {
		t.Fatalf("report.Source = %q, want generated", report.Source)
	}
	if !report.MetadataUpdated {
		t.Fatal("report.MetadataUpdated = false, want true")
	}
	if !report.DatabaseUpdated {
		t.Fatal("report.DatabaseUpdated = false, want true")
	}
	if !report.IdentityFileUpdated {
		t.Fatal("report.IdentityFileUpdated = false, want true")
	}
	if report.Layer != "generated" {
		t.Fatalf("report.Layer = %q, want generated", report.Layer)
	}
	if !strings.HasPrefix(report.ProjectID, "gc-local-") {
		t.Fatalf("report.ProjectID = %q, want gc-local-*", report.ProjectID)
	}

	metadataData, err = os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("ReadFile(metadata.json): %v", err)
	}
	meta = map[string]any{}
	if err := json.Unmarshal(metadataData, &meta); err != nil {
		t.Fatalf("Unmarshal(metadata.json): %v", err)
	}
	if got := strings.TrimSpace(fmt.Sprint(meta["project_id"])); got != report.ProjectID {
		t.Fatalf("metadata project_id = %q, want %q", got, report.ProjectID)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	var databaseProjectID string
	if err := db.QueryRowContext(ctx2, "SELECT value FROM metadata WHERE `key` = '_project_id'").Scan(&databaseProjectID); err != nil {
		t.Fatalf("read database _project_id: %v", err)
	}
	if got := strings.TrimSpace(databaseProjectID); got != report.ProjectID {
		t.Fatalf("database _project_id = %q, want %q", got, report.ProjectID)
	}

	identityProjectID, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("ReadProjectIdentity: %v", err)
	}
	if !ok {
		t.Fatal("identity project id missing")
	}
	if identityProjectID != report.ProjectID {
		t.Fatalf("identity project_id = %q, want %q", identityProjectID, report.ProjectID)
	}
}

func TestEnsureProjectIDCmdRequiresCityFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newEnsureProjectIDCmd(&stdout, &stderr)
	metadataPath := filepath.Join(t.TempDir(), ".beads", "metadata.json")
	cmd.SetArgs([]string{
		"--metadata", metadataPath,
		"--port", "3306",
		"--database", "hq",
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("ensure-project-id without --city succeeded")
	}
	if !strings.Contains(err.Error(), `required flag(s) "city" not set`) {
		t.Fatalf("ensure-project-id error = %v, want required --city", err)
	}
}

func TestEnsureProjectIDEmitsStampedEvent(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	cases := []struct {
		name       string
		l1         string
		l2         string
		l3         string
		wantEvents []wantProjectIdentityStampedEvent
	}{
		{
			name: "generated_emits_three_events_one_per_layer",
			wantEvents: []wantProjectIdentityStampedEvent{
				{source: "generated", layer: "L1"},
				{source: "generated", layer: "L2"},
				{source: "generated", layer: "L3"},
			},
		},
		{
			name: "migrated_from_metadata_emits_one_l1_event",
			l2:   "metadata-id",
			l3:   "metadata-id",
			wantEvents: []wantProjectIdentityStampedEvent{
				{source: "migrated_from_metadata", layer: "L1", newID: "metadata-id"},
			},
		},
		{
			name: "cache_repair_l2_emits_one_l2_event",
			l1:   "canonical-id",
			l2:   "wrong-l2",
			l3:   "canonical-id",
			wantEvents: []wantProjectIdentityStampedEvent{
				{source: "cache_repair", layer: "L2", oldID: "wrong-l2", newID: "canonical-id"},
			},
		},
		{
			name: "cache_repair_l3_emits_one_l3_event",
			l1:   "canonical-id",
			l2:   "canonical-id",
			wantEvents: []wantProjectIdentityStampedEvent{
				{source: "cache_repair", layer: "L3", newID: "canonical-id"},
			},
		},
		{
			name: "migrated_from_metadata_with_l3_seed_emits_l1_and_l3",
			l2:   "metadata-id",
			wantEvents: []wantProjectIdentityStampedEvent{
				{source: "migrated_from_metadata", layer: "L1", newID: "metadata-id"},
				{source: "migrated_from_metadata", layer: "L3", newID: "metadata-id"},
			},
		},
		{
			name: "migrated_from_database_with_l2_seed_emits_l1_and_l2",
			l3:   "database-id",
			wantEvents: []wantProjectIdentityStampedEvent{
				{source: "migrated_from_database", layer: "L1", newID: "database-id"},
				{source: "migrated_from_database", layer: "L2", newID: "database-id"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			scopeRoot := filepath.Join(cityDir, "rigs", "demo")
			metadataPath := writeProjectIDMetadataFile(t, scopeRoot, tc.l2)
			if tc.l1 != "" {
				if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, tc.l1); err != nil {
					t.Fatalf("WriteProjectIdentity: %v", err)
				}
			}
			var setup []string
			if tc.l3 != "" {
				setup = seedDatabaseProjectIDQueries(tc.l3)
			}
			port, cleanup := startProjectIDTestServer(t, setup...)
			defer cleanup()

			rec := &projectIdentityRecordingRecorder{}
			report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
			if err != nil {
				t.Fatalf("ensureManagedDoltProjectIDWithRecorder: %v", err)
			}
			assertProjectIdentityStampedEvents(t, rec.records, tc.wantEvents)
			if tc.name == "generated_emits_three_events_one_per_layer" {
				if report.ProjectID == "" {
					t.Fatal("generated report.ProjectID empty")
				}
				payloads := decodeProjectIdentityStampedPayloads(t, rec.records)
				for _, payload := range payloads {
					if payload.NewID != report.ProjectID {
						t.Fatalf("generated event NewID = %q, want report.ProjectID %q", payload.NewID, report.ProjectID)
					}
				}
			}
		})
	}
}

func TestEnsureProjectIDEmitsNothingOnNoOp(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	cityDir := t.TempDir()
	scopeRoot := filepath.Join(cityDir, "rigs", "demo")
	metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "canonical-id")
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, "canonical-id"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries("canonical-id")...)
	defer cleanup()

	rec := &projectIdentityRecordingRecorder{}
	if _, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec); err != nil {
		t.Fatalf("ensureManagedDoltProjectIDWithRecorder: %v", err)
	}
	if len(rec.records) != 0 {
		t.Fatalf("recorded %d event(s), want none: %+v", len(rec.records), rec.records)
	}
}

func TestEnsureProjectIDEmitsNothingOnRefusal(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	cityDir := t.TempDir()
	scopeRoot := filepath.Join(cityDir, "rigs", "demo")
	metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "identity-id")
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, "identity-id"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries("database-id")...)
	defer cleanup()

	rec := &projectIdentityRecordingRecorder{}
	_, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
	if err == nil {
		t.Fatal("ensureManagedDoltProjectIDWithRecorder unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "PROJECT IDENTITY MISMATCH") {
		t.Fatalf("error = %v, want PROJECT IDENTITY MISMATCH", err)
	}
	if len(rec.records) != 0 {
		t.Fatalf("recorded %d event(s), want none: %+v", len(rec.records), rec.records)
	}
}

func TestEnsureManagedDoltProjectIDLegacyMigration(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	scopeRoot := t.TempDir()
	metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "legacy-id")
	port, cleanup := startProjectIDTestServer(t)
	defer cleanup()

	report, err := ensureManagedDoltProjectID(metadataPath, port)
	if err != nil {
		t.Fatalf("ensureManagedDoltProjectID: %v", err)
	}
	if report.ProjectID != "legacy-id" || report.Source != "l1-migrate-l3-seed" || report.Layer != "l2" {
		t.Fatalf("report = %+v, want l2 migration", report)
	}
	if !report.IdentityFileUpdated || report.MetadataUpdated || !report.DatabaseUpdated {
		t.Fatalf("report updates = %+v, want identity+database only", report)
	}
	assertProjectIdentityFile(t, scopeRoot, "legacy-id")
	assertMetadataProjectID(t, metadataPath, "legacy-id")
	assertDatabaseProjectID(t, port, "legacy-id")
}

func TestEnsureManagedDoltProjectIDDBReseed(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	scopeRoot := t.TempDir()
	metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "canonical-id")
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, "canonical-id"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	port, cleanup := startProjectIDTestServer(t)
	defer cleanup()

	report, err := ensureManagedDoltProjectID(metadataPath, port)
	if err != nil {
		t.Fatalf("ensureManagedDoltProjectID: %v", err)
	}
	if report.ProjectID != "canonical-id" || report.Source != "l3-seed" || report.Layer != "l1" {
		t.Fatalf("report = %+v, want l3 seed from l1", report)
	}
	if report.IdentityFileUpdated || report.MetadataUpdated || !report.DatabaseUpdated {
		t.Fatalf("report updates = %+v, want database only", report)
	}
	assertProjectIdentityFile(t, scopeRoot, "canonical-id")
	assertMetadataProjectID(t, metadataPath, "canonical-id")
	assertDatabaseProjectID(t, port, "canonical-id")
}

func TestEnsureManagedDoltProjectIDHotPathNoOp(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	scopeRoot := t.TempDir()
	metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "canonical-id")
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, "canonical-id"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries("canonical-id")...)
	defer cleanup()
	beforeIdentity := mustReadFile(t, contract.ProjectIdentityPath(scopeRoot))
	beforeMetadata := mustReadFile(t, metadataPath)

	report, err := ensureManagedDoltProjectID(metadataPath, port)
	if err != nil {
		t.Fatalf("ensureManagedDoltProjectID: %v", err)
	}
	if report.ProjectID != "canonical-id" || report.Source != "match" || report.Layer != "l1" {
		t.Fatalf("report = %+v, want hot-path match", report)
	}
	if report.IdentityFileUpdated || report.MetadataUpdated || report.DatabaseUpdated {
		t.Fatalf("report updates = %+v, want no updates", report)
	}
	if got := mustReadFile(t, contract.ProjectIdentityPath(scopeRoot)); string(got) != string(beforeIdentity) {
		t.Fatalf("identity changed on hot path:\n%s", got)
	}
	if got := mustReadFile(t, metadataPath); string(got) != string(beforeMetadata) {
		t.Fatalf("metadata changed on hot path:\n%s", got)
	}
	assertDatabaseProjectID(t, port, "canonical-id")
}

func TestEnsureManagedDoltProjectIDRefusesL1L3Mismatch(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	scopeRoot := t.TempDir()
	metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "metadata-id")
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, "identity-id"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	beforeMetadata := mustReadFile(t, metadataPath)
	port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries("database-id")...)
	defer cleanup()

	_, err := ensureManagedDoltProjectID(metadataPath, port)
	if err == nil {
		t.Fatal("ensureManagedDoltProjectID unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "PROJECT IDENTITY MISMATCH") || !strings.Contains(err.Error(), "identity-id") || !strings.Contains(err.Error(), "database-id") {
		t.Fatalf("ensureManagedDoltProjectID error = %v, want L1/L3 mismatch with both ids", err)
	}
	if got := mustReadFile(t, metadataPath); string(got) != string(beforeMetadata) {
		t.Fatalf("metadata changed on refusal:\n%s", got)
	}
	assertProjectIdentityFile(t, scopeRoot, "identity-id")
	assertDatabaseProjectID(t, port, "database-id")
}

func TestEnsureManagedDoltProjectIDRepairsL2Drift(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	scopeRoot := t.TempDir()
	metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "wrong-l2")
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, "canonical-id"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries("canonical-id")...)
	defer cleanup()

	report, err := ensureManagedDoltProjectID(metadataPath, port)
	if err != nil {
		t.Fatalf("ensureManagedDoltProjectID: %v", err)
	}
	if report.ProjectID != "canonical-id" || report.Source != "l2-repair" || report.Layer != "l1" {
		t.Fatalf("report = %+v, want l2 repair", report)
	}
	if report.IdentityFileUpdated || !report.MetadataUpdated || report.DatabaseUpdated {
		t.Fatalf("report updates = %+v, want metadata only", report)
	}
	assertProjectIdentityFile(t, scopeRoot, "canonical-id")
	assertMetadataProjectID(t, metadataPath, "canonical-id")
	assertDatabaseProjectID(t, port, "canonical-id")
}

func TestEnsureManagedDoltProjectIDAdoptsFromL3(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	scopeRoot := t.TempDir()
	metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "")
	port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries("database-id")...)
	defer cleanup()

	report, err := ensureManagedDoltProjectID(metadataPath, port)
	if err != nil {
		t.Fatalf("ensureManagedDoltProjectID: %v", err)
	}
	if report.ProjectID != "database-id" || report.Source != "l1-adopt-l2-seed" || report.Layer != "l3" {
		t.Fatalf("report = %+v, want l3 adoption", report)
	}
	if !report.IdentityFileUpdated || !report.MetadataUpdated || report.DatabaseUpdated {
		t.Fatalf("report updates = %+v, want identity+metadata only", report)
	}
	assertProjectIdentityFile(t, scopeRoot, "database-id")
	assertMetadataProjectID(t, metadataPath, "database-id")
	assertDatabaseProjectID(t, port, "database-id")
}

func writeProjectIDMetadataFile(t *testing.T, scopeRoot string, projectID string) string {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"backend":       "dolt",
		"database":      "dolt",
		"dolt_database": "hq",
		"dolt_mode":     "server",
	}
	if projectID != "" {
		meta["project_id"] = projectID
	}
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if err := os.WriteFile(metadataPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return metadataPath
}

type projectIdentityRecordingRecorder struct {
	records []events.Event
}

func (r *projectIdentityRecordingRecorder) Record(e events.Event) {
	r.records = append(r.records, e)
}

type wantProjectIdentityStampedEvent struct {
	source string
	layer  string
	oldID  string
	newID  string
}

func assertProjectIdentityStampedEvents(t *testing.T, records []events.Event, want []wantProjectIdentityStampedEvent) {
	t.Helper()
	if len(records) != len(want) {
		t.Fatalf("recorded %d event(s), want %d: %+v", len(records), len(want), records)
	}
	payloads := decodeProjectIdentityStampedPayloads(t, records)
	for i, payload := range payloads {
		if records[i].Type != events.ProjectIdentityStamped {
			t.Fatalf("records[%d].Type = %q, want %q", i, records[i].Type, events.ProjectIdentityStamped)
		}
		if payload.ScopeRoot != "rigs/demo" {
			t.Fatalf("payload[%d].ScopeRoot = %q, want rigs/demo", i, payload.ScopeRoot)
		}
		if payload.Source != want[i].source {
			t.Fatalf("payload[%d].Source = %q, want %q", i, payload.Source, want[i].source)
		}
		if payload.Layer != want[i].layer {
			t.Fatalf("payload[%d].Layer = %q, want %q", i, payload.Layer, want[i].layer)
		}
		if payload.OldID != want[i].oldID {
			t.Fatalf("payload[%d].OldID = %q, want %q", i, payload.OldID, want[i].oldID)
		}
		if want[i].newID != "" && payload.NewID != want[i].newID {
			t.Fatalf("payload[%d].NewID = %q, want %q", i, payload.NewID, want[i].newID)
		}
		if payload.NewID == "" {
			t.Fatalf("payload[%d].NewID empty", i)
		}
	}
}

func decodeProjectIdentityStampedPayloads(t *testing.T, records []events.Event) []gcapi.ProjectIdentityStampedPayload {
	t.Helper()
	payloads := make([]gcapi.ProjectIdentityStampedPayload, 0, len(records))
	for i, record := range records {
		var payload gcapi.ProjectIdentityStampedPayload
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			t.Fatalf("unmarshal records[%d].Payload: %v", i, err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

func startProjectIDTestServer(t *testing.T, setupQueries ...string) (string, func()) {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), "hq")
	_, port, _, cleanup := startPasswordedDoltServer(t, repoDir, setupQueries...)
	return fmt.Sprintf("%d", port), cleanup
}

func seedDatabaseProjectIDQueries(projectID string) []string {
	return []string{
		"CREATE TABLE IF NOT EXISTS metadata (`key` VARCHAR(255) PRIMARY KEY, value LONGTEXT)",
		fmt.Sprintf("INSERT INTO metadata (`key`, value) VALUES ('_project_id', '%s') ON DUPLICATE KEY UPDATE value = VALUES(value)", projectID),
	}
}

func assertProjectIdentityFile(t *testing.T, scopeRoot string, want string) {
	t.Helper()
	got, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, scopeRoot)
	if err != nil {
		t.Fatalf("ReadProjectIdentity: %v", err)
	}
	if !ok {
		t.Fatal("identity project id missing")
	}
	if got != want {
		t.Fatalf("identity project_id = %q, want %q", got, want)
	}
}

func assertMetadataProjectID(t *testing.T, metadataPath string, want string) {
	t.Helper()
	got, err := readManagedMetadataProjectID(metadataPath)
	if err != nil {
		t.Fatalf("readManagedMetadataProjectID: %v", err)
	}
	if got != want {
		t.Fatalf("metadata project_id = %q, want %q", got, want)
	}
}

func assertDatabaseProjectID(t *testing.T, port string, want string) {
	t.Helper()
	db, err := managedDoltOpenDatabase("127.0.0.1", port, "root", "hq")
	if err != nil {
		t.Fatalf("managedDoltOpenDatabase: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, ok, err := readDatabaseProjectID(ctx, db)
	if err != nil {
		t.Fatalf("readDatabaseProjectID: %v", err)
	}
	if !ok {
		t.Fatal("database project id missing")
	}
	if got != want {
		t.Fatalf("database _project_id = %q, want %q", got, want)
	}
}

func startPasswordedDoltServer(t *testing.T, repoDir string, setupQueries ...string) (string, int, int, func()) {
	t.Helper()
	configureTestDoltIdentityEnv(t)

	doltPath := os.Getenv("GC_DOLT_REAL_BINARY")
	var err error
	if doltPath == "" {
		doltPath, err = exec.LookPath("dolt")
		if err != nil {
			t.Skip("dolt not installed")
		}
	}
	if repoDir == "" {
		repoDir = t.TempDir()
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", repoDir, err)
	}

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(doltPath, args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("dolt %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init")
	for _, query := range setupQueries {
		run("sql", "-q", query)
	}
	run("sql", "-q", "CREATE USER 'root'@'%' IDENTIFIED BY 'secret'; GRANT ALL ON *.* TO 'root'@'%';")

	port := reserveRandomTCPPort(t)
	cmd := exec.Command(doltPath, "sql-server", "--host", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--allow-cleartext-passwords", "--loglevel=warning")
	cmd.Dir = repoDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start passworded dolt sql-server: %v", err)
	}

	t.Setenv("GC_DOLT_PASSWORD", "secret")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := managedDoltQueryProbeDirect("127.0.0.1", fmt.Sprintf("%d", port), "root"); err == nil {
			cleanup := func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				_, _ = cmd.Process.Wait()
			}
			return repoDir, port, cmd.Process.Pid, cleanup
		}
		time.Sleep(250 * time.Millisecond)
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	t.Fatalf("passworded dolt sql-server on %d did not become query-ready", port)
	return "", 0, 0, func() {}
}

func TestManagedDoltHealthCheckWithPasswordUsesDirectHelpersAgainstRealServer(t *testing.T) {
	binDir := t.TempDir()
	realDolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	t.Setenv("GC_DOLT_REAL_BINARY", realDolt)
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, port, _, cleanup := startPasswordedDoltServer(t, "")
	defer cleanup()

	report, err := managedDoltHealthCheck("0.0.0.0", fmt.Sprintf("%d", port), "root", true)
	if err != nil {
		t.Fatalf("managedDoltHealthCheck() error = %v", err)
	}
	if !report.QueryReady || report.ReadOnly != "false" {
		t.Fatalf("managedDoltHealthCheck() = %+v, want query-ready writable server", report)
	}
	if report.ConnectionCount == "" {
		t.Fatalf("managedDoltHealthCheck() = %+v, want connection count", report)
	}
}

func TestManagedDoltWaitReadyWithPasswordUsesDirectQueryProbe(t *testing.T) {
	binDir := t.TempDir()
	realDolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	t.Setenv("GC_DOLT_REAL_BINARY", realDolt)
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	repoDir, port, pid, cleanup := startPasswordedDoltServer(t, "")
	defer cleanup()

	report, err := waitForManagedDoltReady(repoDir, "0.0.0.0", fmt.Sprintf("%d", port), "root", pid, 5*time.Second, false)
	if err != nil {
		t.Fatalf("waitForManagedDoltReady() error = %v", err)
	}
	if !report.Ready || !report.PIDAlive {
		t.Fatalf("waitForManagedDoltReady() = %+v, want ready pid_alive", report)
	}
}

func TestRecoverManagedDoltProcessWithPasswordReusesHealthyRealServer(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	_, port, pid, cleanup := startPasswordedDoltServer(t, layout.DataDir, "CREATE DATABASE IF NOT EXISTS `hq`")
	defer cleanup()
	t.Cleanup(func() {
		if state, err := readDoltRuntimeStateFile(layout.StateFile); err == nil && state.PID > 0 {
			_ = terminateManagedDoltPID("", state.PID)
		}
	})

	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dir): %v", err)
	}
	if err := os.WriteFile(layout.PIDFile, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", fmt.Sprintf("%d", port), "root", "warning", 10*time.Second)
	if err != nil {
		t.Fatalf("recoverManagedDoltProcess() error = %v", err)
	}
	if !report.Ready || !report.Healthy {
		t.Fatalf("recoverManagedDoltProcess() = %+v, want ready healthy", report)
	}
	if !report.HadPID {
		t.Fatalf("recoverManagedDoltProcess() HadPID = false, want true")
	}
	if report.PID != pid {
		t.Fatalf("recoverManagedDoltProcess() pid = %d, want reused pid %d", report.PID, pid)
	}
	if report.Port != port {
		t.Fatalf("recoverManagedDoltProcess() port = %d, want %d", report.Port, port)
	}
	if report.Restarted {
		t.Fatalf("recoverManagedDoltProcess() Restarted = true, want false")
	}
}

func TestEnsureManagedDoltProjectIDGeneratesLocalIdentityWithPasswordedServer(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	cityDir := t.TempDir()
	metadataPath := filepath.Join(cityDir, ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	repoDir := filepath.Join(cityDir, ".beads", "dolt")
	_, port, _, cleanup := startPasswordedDoltServer(t, repoDir, "CREATE DATABASE IF NOT EXISTS `hq`; USE `hq`; CREATE TABLE IF NOT EXISTS metadata (`key` VARCHAR(255) PRIMARY KEY, value LONGTEXT);")
	defer cleanup()

	db, err := managedDoltOpenDatabase("127.0.0.1", fmt.Sprintf("%d", port), "root", "hq")
	if err != nil {
		t.Fatalf("managedDoltOpenDatabase: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("DELETE FROM metadata WHERE `key` = '_project_id'"); err != nil {
		t.Fatalf("delete database _project_id: %v", err)
	}

	report, err := ensureManagedDoltProjectID(metadataPath, fmt.Sprintf("%d", port))
	if err != nil {
		t.Fatalf("ensureManagedDoltProjectID: %v", err)
	}
	if report.Source != "generated" {
		t.Fatalf("report.Source = %q, want generated", report.Source)
	}
	if !report.MetadataUpdated || !report.DatabaseUpdated || !report.IdentityFileUpdated {
		t.Fatalf("report = %+v, want identity, metadata, and database updated", report)
	}
	if report.Layer != "generated" {
		t.Fatalf("report.Layer = %q, want generated", report.Layer)
	}
	if !strings.HasPrefix(report.ProjectID, "gc-local-") {
		t.Fatalf("report.ProjectID = %q, want gc-local-*", report.ProjectID)
	}
	assertProjectIdentityFile(t, cityDir, report.ProjectID)

	var databaseProjectID string
	if err := db.QueryRow("SELECT value FROM metadata WHERE `key` = '_project_id'").Scan(&databaseProjectID); err != nil {
		t.Fatalf("read database _project_id: %v", err)
	}
	if strings.TrimSpace(databaseProjectID) != report.ProjectID {
		t.Fatalf("database _project_id = %q, want %q", databaseProjectID, report.ProjectID)
	}
}
