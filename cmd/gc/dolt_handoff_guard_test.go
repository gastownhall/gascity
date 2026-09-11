package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func handoffGuardTestCity(t *testing.T) string {
	t.Helper()
	city, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve handoff test city: %v", err)
	}
	return city
}

func TestHandoffJournalBlocksManagedDoltStart(t *testing.T) {
	city := handoffGuardTestCity(t)
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(city, ".beads", "ownership-handoff.json")
	for _, phase := range []string{"prepared", "target_configured", "old_owner_stopped", "verified", "committed"} {
		t.Run(phase, func(t *testing.T) {
			owner := "legacy-gc"
			if phase == "committed" {
				owner = "bd"
			}
			data := []byte(`{"request":{"city_root":"` + city + `","root":"` + city + `"},"phase":"` + phase + `","owner":"` + owner + `"}`)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := handoffJournalBlocksManagedDoltStart(city); err == nil {
				t.Fatal("handoff journal did not block managed start")
			}
		})
	}
}

func TestRecoverManagedDoltRefusesPendingHandoffBeforeLifecycleWork(t *testing.T) {
	city := handoffGuardTestCity(t)
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	journal := `{"request":{"city_root":"` + city + `","root":"` + city + `"},"phase":"old_owner_stopped","owner":"legacy-gc"}`
	if err := os.WriteFile(filepath.Join(city, ".beads", "ownership-handoff.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := recoverManagedDoltProcessWithOps(city, "127.0.0.1", "3307", "root", "warning", time.Second, managedDoltRecoveryOps{
		queryProbe: func(string, string, string) error { called = true; return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "ownership handoff journal") {
		t.Fatalf("recover error = %v, want handoff refusal", err)
	}
	if called {
		t.Fatal("recovery probed or started despite pending handoff")
	}
}

func TestHandoffJournalAllowsManagedDoltStartOnlyWhenAbsent(t *testing.T) {
	city := handoffGuardTestCity(t)
	if err := handoffJournalBlocksManagedDoltStart(city); err != nil {
		t.Fatalf("absent journal blocked managed start: %v", err)
	}
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "ownership-handoff.json"), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handoffJournalBlocksManagedDoltStart(city); err == nil {
		t.Fatal("malformed journal did not fail closed")
	}
}

func TestCommittedBeadsHandoffLeavesFreshMissingScopeLegacy(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "rigs", "fresh")
	owned, err := committedBeadsHandoffOwnsScope(fresh)
	if err != nil || owned {
		t.Fatalf("missing fresh scope ownership = (%t, %v), want legacy", owned, err)
	}

	city := handoffGuardTestCity(t)
	if err := os.Symlink(filepath.Join(city, "outside-beads"), filepath.Join(city, ".beads")); err != nil {
		t.Fatal(err)
	}
	owned, err = committedBeadsHandoffOwnsScope(city)
	if err != nil || owned {
		t.Fatalf("legacy .beads symlink without journal ownership = (%t, %v), want legacy", owned, err)
	}
}

func TestHandoffJournalAllowsOnlyExactRestoredLegacyControls(t *testing.T) {
	city := handoffGuardTestCity(t)
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"backend":"dolt","dolt_database":"beads"}`)
	config := []byte("gc.endpoint_origin: managed_city\ndolt.auto-start: false\n")
	port := []byte("3307")
	for _, file := range []struct {
		name string
		body []byte
	}{{"metadata.json", metadata}, {"config.yaml", config}, {"dolt-server.port", port}} {
		if err := os.WriteFile(filepath.Join(beadsDir, file.name), file.body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacy := base64.StdEncoding.EncodeToString([]byte(`{"schema_version":1,"operation":"handoff-inspect","result":"eligible","owner":"legacy-gc","identity_token":"token"}`))
	journal := fmt.Sprintf(`{"request":{"city_root":%q,"root":%q,"database":"beads","workspace":"test","endpoint":{"host":"127.0.0.1","port":3307},"owner":"legacy-gc"},"phase":"legacy_config_restored","owner":"legacy-gc","snapshot_captured":true,"mutation_occurred":true,"snapshot":{"metadata":%q,"workspace_metadata":"eyJiYWNrZW5kIjoiZG9sdCIsImRvbHRfZGF0YWJhc2UiOiJiZWFkcyJ9","workspace_config":"Z2MuZW5kcG9pbnRfb3JpZ2luOiBtYW5hZ2VkX2NpdHkKZG9sdC5hdXRvLXN0YXJ0OiBmYWxzZQo=","workspace_port":"MzMwNw==","workspace_metadata_present":true,"workspace_config_present":true,"workspace_port_present":true,"workspace_metadata_mode":384,"workspace_config_mode":384,"workspace_port_mode":384}}`, city, city, legacy)
	if err := os.WriteFile(filepath.Join(beadsDir, "ownership-handoff.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handoffJournalBlocksManagedDoltStart(city); err != nil {
		t.Fatalf("exact restored journal blocked start: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("dolt.auto-start: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handoffJournalBlocksManagedDoltStart(city); err == nil {
		t.Fatal("drifted restored config admitted managed start")
	}
}

func TestCommittedBeadsHandoffOwnsScopeOnlyAfterCommit(t *testing.T) {
	city := handoffGuardTestCity(t)
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(phase, owner string, complete bool) {
		t.Helper()
		var journal handoffProjectionJournal
		journal.Request.CityRoot, journal.Request.Root = city, city
		journal.Request.Database, journal.Request.Workspace = "beads", "test"
		journal.Request.Endpoint.Host, journal.Request.Endpoint.Port = "127.0.0.1", 3307
		journal.Request.Owner, journal.Phase, journal.Owner = "legacy-gc", phase, owner
		if complete {
			journal.SnapshotCaptured, journal.MutationOccurred, journal.CommitHookRan = true, true, true
			setProjectionEligibleSnapshot(t, &journal)
			journal.Snapshot.TargetPID, journal.Snapshot.TargetBirth = 42, "birth"
			journal.Snapshot.TargetDataDir = filepath.Join(city, ".beads", "dolt")
			journal.Snapshot.TargetLaunchID = "0123456789abcdef0123456789abcdef"
			journal.Snapshot.TargetLaunchConfig = filepath.Join(city, ".beads", "dolt-handoff-"+journal.Snapshot.TargetLaunchID+".yaml")
			journal.Snapshot.TargetLaunchExecutable = "/usr/local/bin/dolt"
		}
		body, err := json.Marshal(journal)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "ownership-handoff.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("prepared", "legacy-gc", false)
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("pending journal was admitted")
	}
	write("committed", "bd", true)
	if got, err := committedBeadsHandoffOwnsScope(city); err != nil || !got {
		t.Fatalf("committed projection = %t, %v", got, err)
	}
}
