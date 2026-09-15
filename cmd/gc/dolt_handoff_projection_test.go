package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The version gate is the first thing the reader does, and these are the two
// halves of why it has to be.
//
// The phase names overlap between journal versions and do not survive the
// translation: old_owner_stopped means "bd's replacement is already configured"
// under v1 and "the legacy server is gone, nothing has replaced it yet" under
// v2. A reader that recognizes the name and skips the version will, on exactly
// one phase, believe a server exists that does not — or license a second one
// over a live target. So a journal that is not this version is refused by
// version, with no phase interpreted at all.
func TestProjectionRefusesAJournalItCannotVersion(t *testing.T) {
	for name, body := range map[string]string{
		// A v1 journal: no schema_version at all, and a phase name this reader
		// also has.
		"v1 journal": `{"request":{"city_root":"CITY","root":"CITY","database":"beads","workspace":"w",` +
			`"endpoint":{"host":"127.0.0.1","port":3307},"owner":"legacy-gc"},"phase":"old_owner_stopped","owner":"legacy-gc"}`,
		"future journal": `{"schema_version":3,"request":{"root":"CITY","database":"beads","workspace":"w",` +
			`"endpoint":{"host":"127.0.0.1","port":3307},"owner":"legacy-gc"},"phase":"committed","owner":"bd"}`,
		// A v1 journal whose phase this reader would have admitted as owned.
		"v1 committed journal": `{"request":{"city_root":"CITY","root":"CITY","database":"beads","workspace":"w",` +
			`"endpoint":{"host":"127.0.0.1","port":3307},"owner":"legacy-gc"},"phase":"committed","owner":"bd"}`,
	} {
		t.Run(name, func(t *testing.T) {
			city := handoffFixtureCity(t)
			if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(city, ".beads", "ownership-handoff.json")
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "CITY", city)), 0o600); err != nil {
				t.Fatal(err)
			}
			owned, err := committedBeadsHandoffOwnsScope(city)
			if err == nil {
				t.Fatalf("a journal of another version was interpreted (owned=%t)", owned)
			}
			if owned {
				t.Fatal("a journal of another version reported the scope as bd's")
			}
			if !strings.Contains(err.Error(), "unsupported handoff journal version") {
				t.Fatalf("refusal does not name the version as the reason: %v", err)
			}
		})
	}
}

func TestCommittedBeadsHandoffOwnsScopeProjection(t *testing.T) {
	city := handoffFixtureCity(t)

	writeCommittedHandoffJournal(t, city)
	if got, err := committedBeadsHandoffOwnsScope(city); err != nil || !got {
		t.Fatalf("committed projection = %t, %v; want true, nil", got, err)
	}

	writeHandoffJournal(t, city, pendingHandoffJournal(city, "target_configured"))
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("pending projection did not fail closed")
	}

	if err := os.WriteFile(filepath.Join(city, ".beads", "ownership-handoff.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("malformed projection did not fail closed")
	}
}

// old_owner_stopped is the phase the version gate exists for, so it gets its
// own case rather than riding along in a table.
//
// Under v2 it means the legacy server has been proven gone and bd has not yet
// launched a replacement: the scope has no running Dolt at all, and no owner
// has been settled. gc must read that as neither "bd's" nor "mine" — it is a
// transfer in flight, and the only safe answer is to fail closed, which is what
// fences gc's managed start out of the window where starting a server would
// race bd's own configure.
func TestOldOwnerStoppedIsATransferInFlightNotAnOwnership(t *testing.T) {
	city := handoffFixtureCity(t)
	writeHandoffJournal(t, city, pendingHandoffJournal(city, "old_owner_stopped"))

	owned, err := committedBeadsHandoffOwnsScope(city)
	if owned {
		t.Fatal("old_owner_stopped was read as bd owning the scope; under v2 there is no target yet")
	}
	if err == nil {
		t.Fatal("old_owner_stopped was admitted as the legacy condition; gc would start a second server mid-transfer")
	}
	if !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("refusal does not say the transfer is in flight: %v", err)
	}
	if err := handoffJournalBlocksManagedDoltStart(city); err == nil {
		t.Fatal("gc's managed start was admitted over a handoff that had already stopped the legacy server")
	}
}

// A rollback that ran to completion archives its journal. gc sees the scope the
// way the legacy condition looks — no journal — and must not go looking for the
// archive, which is bd's own history and not a record of current ownership.
func TestArchivedRolledBackJournalsAreIgnored(t *testing.T) {
	city := handoffFixtureCity(t)
	beadsDir := filepath.Join(city, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(committedHandoffJournal(city))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"ownership-handoff.json.rolled-back-1757000000000000000",
		"ownership-handoff.json.rolled-back-1757000000000000001",
	} {
		if err := os.WriteFile(filepath.Join(beadsDir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	owned, err := committedBeadsHandoffOwnsScope(city)
	if err != nil {
		t.Fatalf("an archived rollback was read as a live journal: %v", err)
	}
	if owned {
		t.Fatal("an archived rollback reported the scope as bd's")
	}
	if err := handoffJournalBlocksManagedDoltStart(city); err != nil {
		t.Fatalf("gc refused to manage a city whose handoff was rolled back and archived: %v", err)
	}
}

func TestCommittedBeadsHandoffRejectsIncompleteCheckpoint(t *testing.T) {
	city := handoffFixtureCity(t)
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(city, ".beads", "ownership-handoff.json")
	body := `{"schema_version":2,"request":{"root":"` + city + `"},"phase":"committed","owner":"bd"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("truncated committed journal admitted")
	}
}

func TestCommittedBeadsHandoffRejectsSymlinkedJournal(t *testing.T) {
	city := handoffFixtureCity(t)
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
	city := handoffFixtureCity(t)
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"backend":"dolt","dolt_database":"beads"}`)
	config := []byte("legacy: true\n")
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(beadsDir, "config.yaml"), 0); err != nil {
		t.Fatal(err)
	}
	journal := pendingHandoffJournal(city, "rolled_back")
	journal.Snapshot.Metadata = handoffProjectionArtifact{Present: true, Data: metadata, Mode: 0o600}
	// The captured mode is 0o600 while the file on disk is 000. Mode 000 is
	// checked exactly rather than treated as unspecified: the current process
	// cannot read it, so admission fails closed instead of silently accepting
	// an artifact whose bytes it cannot verify.
	journal.Snapshot.Config = handoffProjectionArtifact{Present: true, Data: config, Mode: 0o600}
	// dolt-server.port was absent when bd captured it and must still be absent.
	writeHandoffJournal(t, city, journal)

	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("unreadable mode-zero restored artifact admitted")
	}
	if err := os.Chmod(filepath.Join(beadsDir, "config.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("restored mode drift admitted")
	}
	if err := os.Chmod(filepath.Join(beadsDir, "config.yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if owned, err := committedBeadsHandoffOwnsScope(city); err != nil || owned {
		t.Fatalf("byte- and mode-exact restoration = (%t, %v); want (false, nil)", owned, err)
	}
	// The absent artifact stayed absent. A restoration that put a
	// dolt-server.port back that the legacy city never had is not a
	// restoration.
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte("3307\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rolledBackHandoffAdmissionPath(city)); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("a restoration that produced a file the journal recorded absent was admitted")
	}
}

// The committed journal is the license for gc to stop running a Dolt server for
// this city. What gc can hold it to is that bd's replacement is bound to THIS
// workspace and identified strictly enough to be stopped again — so every way
// that binding can be broken is a refusal.
func TestCommittedProjectionRejectsAnUnboundReplacement(t *testing.T) {
	city := handoffFixtureCity(t)
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, corrupt := range map[string]func(*handoffProjectionJournal){
		"another scope's data directory": func(j *handoffProjectionJournal) {
			j.Target.DataDir = filepath.Join(city, "other")
		},
		"launch nonce": func(j *handoffProjectionJournal) { j.Target.LaunchID = "bad" },
		"launch config from another nonce": func(j *handoffProjectionJournal) {
			j.Target.LaunchConfig = filepath.Join(city, ".beads", "dolt-handoff-ffffffffffffffffffffffffffffffff.yaml")
		},
		"launch config outside the workspace": func(j *handoffProjectionJournal) {
			j.Target.LaunchConfig = filepath.Join("/tmp", "dolt-handoff-"+handoffFixtureLaunchID+".yaml")
		},
		"no process birth":          func(j *handoffProjectionJournal) { j.Target.Birth = "" },
		"a pid restated as a birth": func(j *handoffProjectionJournal) { j.Target.Birth = "4242" },
		"no pid":                    func(j *handoffProjectionJournal) { j.Target.PID = 0 },
		"relative executable":       func(j *handoffProjectionJournal) { j.Target.Executable = "dolt" },
		"a replacement on no port":  func(j *handoffProjectionJournal) { j.Target.Port = 0 },
		"a replacement off loopback": func(j *handoffProjectionJournal) {
			j.Target.Host = "10.0.0.4"
		},
		"an unfinished write set":                  func(j *handoffProjectionJournal) { j.Reservations.CommitWriteSet = "in_progress" },
		"an unfinished launch":                     func(j *handoffProjectionJournal) { j.Reservations.TargetLaunch = "" },
		"the legacy side still claiming ownership": func(j *handoffProjectionJournal) { j.Owner = "legacy-gc" },
		"another scope's root":                     func(j *handoffProjectionJournal) { j.Request.Root = filepath.Join(city, "elsewhere") },
	} {
		t.Run(name, func(t *testing.T) {
			journal := committedHandoffJournal(city)
			corrupt(&journal)
			writeHandoffJournal(t, city, journal)
			owned, err := committedBeadsHandoffOwnsScope(city)
			if err == nil {
				t.Fatal("an unbound replacement was admitted as a committed handoff")
			}
			if owned {
				t.Fatal("an unbound replacement reported the scope as bd's")
			}
		})
	}
}
