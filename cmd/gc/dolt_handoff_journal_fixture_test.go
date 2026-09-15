package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The one place these tests build an ownership-handoff journal.
//
// It is a fixture of bd's format, which lives in another repository, so it is
// worth saying what keeps the two honest: beads' own
// TestCommittedJournalSatisfiesTheCallersProjection writes a real journal
// through bd's writer and decodes it into a transcription of the reader below.
// A field that moves on bd's side fails there. What this file pins is the other
// direction — that gc's reader admits the shape bd actually writes, and refuses
// everything else.

const (
	handoffFixtureLaunchID = "0123456789abcdef0123456789abcdef"
	handoffFixtureBirth    = "linux-v1:11111111-2222-3333-4444-555555555555:9182736"
)

// handoffFixtureCity is a physical city root. The projection resolves symlinks
// before it compares anything, so a fixture that hands it an unresolved
// spelling is testing the resolver rather than the reader.
func handoffFixtureCity(t *testing.T) string {
	t.Helper()
	city, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve handoff test city: %v", err)
	}
	return city
}

// baseHandoffJournal is the request half every phase shares.
func baseHandoffJournal(city string) handoffProjectionJournal {
	var journal handoffProjectionJournal
	journal.SchemaVersion = handoffJournalSchemaVersion
	journal.Request.Root = city
	journal.Request.Database = "beads"
	journal.Request.Workspace = "workspace-uuid"
	journal.Request.Endpoint.Host = "127.0.0.1"
	journal.Request.Endpoint.Port = 3307
	journal.Request.Owner = "legacy-gc"
	return journal
}

// committedHandoffJournal is what bd leaves behind after a transfer that
// committed: owner bd, both reservations done, and a replacement server
// identified strictly enough for bd to stop it again by identity.
func committedHandoffJournal(city string) handoffProjectionJournal {
	journal := baseHandoffJournal(city)
	journal.Phase, journal.Owner = "committed", "bd"
	journal.Reservations.TargetLaunch = "done"
	journal.Reservations.CommitWriteSet = "done"
	journal.Target.PID = 4242
	journal.Target.Birth = handoffFixtureBirth
	journal.Target.DataDir = filepath.Join(city, ".beads", "dolt")
	journal.Target.LaunchID = handoffFixtureLaunchID
	journal.Target.LaunchConfig = filepath.Join(city, ".beads", "dolt-handoff-"+handoffFixtureLaunchID+".yaml")
	journal.Target.Executable = "/usr/local/bin/dolt"
	journal.Target.Host = "127.0.0.1"
	journal.Target.Port = 3399
	return journal
}

// pendingHandoffJournal is a transfer in flight at phase.
func pendingHandoffJournal(city, phase string) handoffProjectionJournal {
	journal := baseHandoffJournal(city)
	journal.Phase, journal.Owner = phase, "legacy-gc"
	return journal
}

// writeHandoffJournal puts a journal where gc's projection reads it and returns
// that path.
func writeHandoffJournal(t *testing.T, city string, journal handoffProjectionJournal) string {
	t.Helper()
	beadsDir := filepath.Join(city, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(beadsDir, "ownership-handoff.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeCommittedHandoffJournal writes a journal the projection admits, and
// proves it does before any caller depends on it: a fixture the reader fails
// closed on would make every refusal below it a false pass.
func writeCommittedHandoffJournal(t *testing.T, city string) string {
	t.Helper()
	path := writeHandoffJournal(t, city, committedHandoffJournal(city))
	if owned, err := committedBeadsHandoffOwnsScope(city); err != nil || !owned {
		t.Fatalf("fixture journal is not read as a committed handoff: (%t, %v)", owned, err)
	}
	return path
}
