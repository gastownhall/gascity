package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
	var journal handoffProjectionJournal
	journal.Request.CityRoot, journal.Request.Root = city, city
	journal.Request.Database, journal.Request.Workspace = "beads", "test"
	journal.Request.Endpoint.Host, journal.Request.Endpoint.Port = "127.0.0.1", 3307
	journal.Request.Owner = "legacy-gc"
	journal.Phase, journal.Owner = "legacy_config_restored", "legacy-gc"
	journal.SnapshotCaptured, journal.MutationOccurred = true, true
	setProjectionEligibleSnapshot(t, &journal)
	journal.Snapshot.WorkspaceMetadata = metadata
	journal.Snapshot.WorkspaceConfig = config
	journal.Snapshot.WorkspacePort = port
	journal.Snapshot.WorkspaceMetadataPresent = true
	journal.Snapshot.WorkspaceConfigPresent = true
	journal.Snapshot.WorkspacePortPresent = true
	journal.Snapshot.WorkspaceMetadataMode = 0o600
	journal.Snapshot.WorkspaceConfigMode = 0o600
	journal.Snapshot.WorkspacePortMode = 0o600
	body, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "ownership-handoff.json"), body, 0o600); err != nil {
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

// writeCommittedHandoffJournal writes the durable artifact bd leaves behind
// once `bd migrate ownership-handoff` commits: the scope's Dolt endpoint now
// belongs to bd, and every legacy managed-Dolt lifecycle path must refuse it.
func writeCommittedHandoffJournal(t *testing.T, city string) {
	t.Helper()
	var journal handoffProjectionJournal
	journal.Request.CityRoot, journal.Request.Root = city, city
	journal.Request.Database, journal.Request.Workspace = "beads", "test"
	journal.Request.Endpoint.Host, journal.Request.Endpoint.Port = "127.0.0.1", 3307
	journal.Request.Owner = "legacy-gc"
	journal.Phase, journal.Owner = "committed", "bd"
	journal.SnapshotCaptured, journal.MutationOccurred, journal.CommitHookRan = true, true, true
	setProjectionEligibleSnapshot(t, &journal)
	journal.Snapshot.TargetPID, journal.Snapshot.TargetBirth = 42, "birth"
	journal.Snapshot.TargetDataDir = filepath.Join(city, ".beads", "dolt")
	journal.Snapshot.TargetLaunchID = "0123456789abcdef0123456789abcdef"
	journal.Snapshot.TargetLaunchConfig = filepath.Join(city, ".beads", "dolt-handoff-"+journal.Snapshot.TargetLaunchID+".yaml")
	journal.Snapshot.TargetLaunchExecutable = "/usr/local/bin/dolt"
	body, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "ownership-handoff.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// scopeTreeSnapshot lists every path under root with its mode and size. A
// refusal that "mutates nothing" must leave this identical: no lock file, no
// runtime state, no repaired publication.
func scopeTreeSnapshot(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		entries = append(entries, fmt.Sprintf("%s mode=%v size=%d", rel, info.Mode(), info.Size()))
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(entries)
	return entries
}

// TestCommittedHandoffRefusesManagedDoltLifecycleWithoutMutation is the
// responder-side contract bd depends on: once its handoff journal is
// committed, gc must not start or recover a second managed Dolt for the same
// scope, and the refusal must not touch the scope on its way out. The other
// guard tests call the predicates directly; this one goes through the real
// `gc dolt-state start-managed` front door and the recovery entry point, and
// proves the whole scope tree is byte-identical afterwards.
func TestCommittedHandoffRefusesManagedDoltLifecycleWithoutMutation(t *testing.T) {
	city := handoffGuardTestCity(t)
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCommittedHandoffJournal(t, city)
	before := scopeTreeSnapshot(t, city)

	t.Run("start-managed", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"dolt-state", "start-managed", "--city", city, "--host", "127.0.0.1", "--port", "3307", "--user", "root"}, &stdout, &stderr)
		if code == 0 {
			t.Fatalf("start-managed exit = 0, want refusal; stdout = %q", stdout.String())
		}
		// A committed handoff is provider ownership, so the fence answers with
		// the canonical refusal rather than a handoff-specific one.
		if !strings.Contains(stderr.String(), "provider scope ownership") {
			t.Fatalf("start-managed stderr = %q, want a typed provider-ownership refusal", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("start-managed stdout = %q, want no lifecycle report", stdout.String())
		}
	})

	t.Run("recover-managed", func(t *testing.T) {
		probed := false
		_, err := recoverManagedDoltProcessWithOps(city, "127.0.0.1", "3307", "root", "warning", time.Second, managedDoltRecoveryOps{
			queryProbe: func(string, string, string) error { probed = true; return nil },
			start: func(string, string, string, string, string, time.Duration) (managedDoltStartReport, error) {
				t.Error("recovery started managed Dolt on a committed handoff scope")
				return managedDoltStartReport{}, nil
			},
		})
		if err == nil || !strings.Contains(err.Error(), "provider scope ownership") {
			t.Fatalf("recover error = %v, want a typed provider-ownership refusal", err)
		}
		if probed {
			t.Fatal("recovery probed the endpoint on a committed handoff scope")
		}
	})

	if after := scopeTreeSnapshot(t, city); !slices.Equal(before, after) {
		t.Fatalf("refusal mutated the scope:\nbefore = %v\nafter  = %v", before, after)
	}
}
