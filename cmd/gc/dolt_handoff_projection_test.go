package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setProjectionEligibleSnapshot(t *testing.T, journal *handoffProjectionJournal) {
	t.Helper()
	r := journal.Request
	identity := handoffProtocolIdentity{
		CityRoot: r.CityRoot, ScopeRoot: r.Root, Database: r.Database, Workspace: r.Workspace,
		Endpoint: handoffProtocolEndpoint{Host: r.Endpoint.Host, Port: r.Endpoint.Port, Socket: r.Endpoint.Socket},
		DataDir:  filepath.Join(r.Root, ".beads", "dolt"), ConfigFile: filepath.Join(r.CityRoot, ".gc", "dolt.yaml"),
		PID: 7, StartIdentity: "birth", StartTimeTicks: 1, PortHolderPID: 7,
	}
	token := handoffIdentityToken(identity)
	body, err := json.Marshal(handoffProtocolResponse{SchemaVersion: handoffProtocolSchemaVersion, Operation: "handoff-inspect", Result: "eligible", Owner: "legacy-gc", Identity: identity, IdentityToken: token})
	if err != nil {
		t.Fatal(err)
	}
	journal.Snapshot.Metadata, journal.Snapshot.Sentinel = body, token
}

func TestCommittedBeadsHandoffOwnsScopeProjection(t *testing.T) {
	city := handoffTestCity(t)
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(beadsDir, "ownership-handoff.json")
	write := func(phase, owner string) {
		t.Helper()
		var journal handoffProjectionJournal
		journal.Request.CityRoot = city
		journal.Request.Root = city
		journal.Request.Database = "beads"
		journal.Request.Workspace = "test"
		journal.Request.Endpoint.Host = "127.0.0.1"
		journal.Request.Endpoint.Port = 3307
		journal.Request.Owner = "legacy-gc"
		journal.Phase, journal.Owner = phase, owner
		if phase == "committed" || phase == "rolled_back" {
			journal.SnapshotCaptured = true
			setProjectionEligibleSnapshot(t, &journal)
		}
		if phase == "committed" || phase == "rolled_back" {
			journal.MutationOccurred = true
		}
		if phase == "committed" {
			journal.CommitHookRan = true
			journal.Snapshot.TargetPID = 42
			journal.Snapshot.TargetBirth = "birth"
			journal.Snapshot.TargetDataDir = filepath.Join(city, ".beads", "dolt")
			journal.Snapshot.TargetLaunchID = "0123456789abcdef0123456789abcdef"
			journal.Snapshot.TargetLaunchConfig = filepath.Join(city, ".beads", "dolt-handoff-"+journal.Snapshot.TargetLaunchID+".yaml")
			journal.Snapshot.TargetLaunchExecutable = "/usr/local/bin/dolt"
		}
		body, err := json.Marshal(journal)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("committed", "bd")
	if got, err := committedBeadsHandoffOwnsScope(city); err != nil || !got {
		t.Fatalf("committed projection = %t, %v; want true, nil", got, err)
	}
	write("target_configured", "legacy-gc")
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("pending projection did not fail closed")
	}
	write("rolled_back", "legacy-gc")
	if got, err := committedBeadsHandoffOwnsScope(city); err != nil || got {
		t.Fatalf("restored rollback projection = %t, %v; want false, nil", got, err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("malformed projection did not fail closed")
	}
}

func TestCommittedBeadsHandoffRejectsIncompleteCheckpoint(t *testing.T) {
	city := handoffTestCity(t)
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(city, ".beads", "ownership-handoff.json")
	if err := os.WriteFile(path, []byte(`{"request":{"city_root":"`+city+`","root":"`+city+`"},"phase":"committed","owner":"bd"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("truncated committed journal admitted")
	}
}

func TestCommittedBeadsHandoffRejectsSymlinkedJournal(t *testing.T) {
	city := handoffTestCity(t)
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(city, "journal")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(beadsDir, "ownership-handoff.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("symlinked journal admitted")
	} else if strings.Contains(err.Error(), "%!w(<nil>)") {
		t.Fatalf("symlinked journal diagnostic wrapped nil: %v", err)
	}
}

func TestRestoredProjectionPreservesAbsentAndModeZeroArtifacts(t *testing.T) {
	city := handoffTestCity(t)
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := []byte("legacy: true\n")
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(beadsDir, "config.yaml"), 0); err != nil {
		t.Fatal(err)
	}
	var journal handoffProjectionJournal
	journal.Request.CityRoot, journal.Request.Root = city, city
	journal.Request.Database, journal.Request.Workspace = "beads", "test"
	journal.Request.Endpoint.Host, journal.Request.Endpoint.Port = "127.0.0.1", 3307
	journal.Request.Owner, journal.Phase, journal.Owner = "legacy-gc", "rolled_back", "legacy-gc"
	journal.SnapshotCaptured, journal.MutationOccurred = true, true
	setProjectionEligibleSnapshot(t, &journal)
	journal.Snapshot.WorkspaceConfig, journal.Snapshot.WorkspaceConfigPresent, journal.Snapshot.WorkspaceConfigMode = config, true, 0
	body, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "ownership-handoff.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	// Mode 000 is checked exactly rather than treated as an unspecified mode.
	// The current process cannot read it, so admission fails closed instead of
	// silently accepting an artifact whose bytes it cannot verify.
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("unreadable mode-zero restored artifact admitted")
	}
	if err := os.Chmod(filepath.Join(beadsDir, "config.yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("restored mode drift admitted")
	}
}

func TestCommittedBeadsHandoffProjectionRejectsCorruptIdentityProof(t *testing.T) {
	city := handoffTestCity(t)
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := func() handoffProjectionJournal {
		var journal handoffProjectionJournal
		journal.Request.CityRoot, journal.Request.Root = city, city
		journal.Request.Database, journal.Request.Workspace = "beads", "test"
		journal.Request.Endpoint.Host, journal.Request.Endpoint.Port = "127.0.0.1", 3307
		journal.Request.Owner, journal.Phase, journal.Owner = "legacy-gc", "committed", "bd"
		journal.SnapshotCaptured, journal.MutationOccurred, journal.CommitHookRan = true, true, true
		setProjectionEligibleSnapshot(t, &journal)
		journal.Snapshot.TargetPID, journal.Snapshot.TargetBirth = 42, "strict-birth"
		journal.Snapshot.TargetDataDir = filepath.Join(city, ".beads", "dolt")
		journal.Snapshot.TargetLaunchID = "0123456789abcdef0123456789abcdef"
		journal.Snapshot.TargetLaunchConfig = filepath.Join(beadsDir, "dolt-handoff-"+journal.Snapshot.TargetLaunchID+".yaml")
		journal.Snapshot.TargetLaunchExecutable = "/usr/local/bin/dolt"
		return journal
	}
	for name, corrupt := range map[string]func(*handoffProjectionJournal){
		"snapshot sentinel": func(j *handoffProjectionJournal) {
			j.Snapshot.Sentinel = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
		"legacy token":              func(j *handoffProjectionJournal) { j.Snapshot.Metadata = []byte(`{"schema_version":1}`) },
		"historical data directory": func(j *handoffProjectionJournal) { j.Snapshot.TargetDataDir = filepath.Join(city, "other") },
		"launch nonce":              func(j *handoffProjectionJournal) { j.Snapshot.TargetLaunchID = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			journal := base()
			corrupt(&journal)
			body, err := json.Marshal(journal)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(beadsDir, "ownership-handoff.json"), body, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
				t.Fatal("corrupt committed proof was admitted")
			}
		})
	}
}
