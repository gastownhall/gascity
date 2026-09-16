package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCommittedHandoffJournal puts a genuinely committed journal at the path
// gc's ownership projection reads — the full checkpoint, the strict-launch
// target identity and the authenticated legacy inspect proof, because the
// projection fails closed on anything less and a fail-closed error is a
// different refusal than the one these tests are about.
func writeCommittedHandoffJournal(t *testing.T, city string) string {
	t.Helper()
	beadsDir := filepath.Join(city, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var journal handoffProjectionJournal
	journal.Request.CityRoot, journal.Request.Root = city, city
	journal.Request.Database, journal.Request.Workspace = "hq", "workspace"
	journal.Request.Endpoint.Host, journal.Request.Endpoint.Port = "127.0.0.1", 3307
	journal.Request.Owner = "legacy-gc"
	journal.Phase, journal.Owner = "committed", "bd"
	journal.SnapshotCaptured, journal.MutationOccurred, journal.CommitHookRan = true, true, true
	setProjectionEligibleSnapshot(t, &journal)
	journal.Snapshot.TargetPID = 42
	journal.Snapshot.TargetBirth = "birth"
	journal.Snapshot.TargetDataDir = filepath.Join(city, ".beads", "dolt")
	journal.Snapshot.TargetLaunchID = "0123456789abcdef0123456789abcdef"
	journal.Snapshot.TargetLaunchConfig = filepath.Join(city, ".beads", "dolt-handoff-"+journal.Snapshot.TargetLaunchID+".yaml")
	journal.Snapshot.TargetLaunchExecutable = "/usr/local/bin/dolt"

	body, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(beadsDir, "ownership-handoff.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if owned, err := committedBeadsHandoffOwnsScope(city); err != nil || !owned {
		t.Fatalf("fixture journal is not read as a committed handoff: (%t, %v)", owned, err)
	}
	return path
}

// A refusal an operator cannot read is not a refusal. `gc dolt-state
// start-managed` on a handed-off city must say, on stderr, that a committed
// ownership handoff is why — naming the journal, so the next question ("says
// who?") has an answer on the same line.
//
// The guard itself is unit-covered in dolt_handoff_guard_test.go. What this
// pins is the path from the guard to the operator: the first real run against a
// bd that performs the handoff reported a refusal with an empty message, and an
// empty message is indistinguishable from any other way the command can fail.
func TestStartManagedRefusalNamesTheCommittedHandoff(t *testing.T) {
	city := handoffGuardTestCity(t)
	journal := writeCommittedHandoffJournal(t, city)

	var stdout, stderr bytes.Buffer
	cmd := newDoltStateCmd(&stdout, &stderr)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"start-managed", "--city", city, "--port", "3307"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("start-managed did not refuse a handed-off city\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	got := stderr.String()
	if strings.TrimSpace(got) == "" {
		t.Fatal("start-managed refused a handed-off city with an empty message")
	}
	for _, want := range []string{"ownership handoff", journal} {
		if !strings.Contains(got, want) {
			t.Errorf("start-managed refusal does not mention %q:\n%s", want, got)
		}
	}
}

// A missing required flag must not be silent either. The acceptance run that
// surfaced all of this asked `gc dolt-state start-managed --city <root>` with no
// --port and got a non-zero exit and nothing at all on either stream, which
// reads exactly like the ownership refusal the caller was looking for and is
// not.
func TestStartManagedSaysWhichRequiredFlagIsMissing(t *testing.T) {
	city := handoffGuardTestCity(t)

	var stdout, stderr bytes.Buffer
	cmd := newDoltStateCmd(&stdout, &stderr)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"start-managed", "--city", city})
	if err := cmd.Execute(); err == nil {
		t.Fatal("start-managed accepted a call with no --port")
	}
	if got := stdout.String() + stderr.String(); !strings.Contains(got, "port") {
		t.Errorf("start-managed did not say which flag was missing:\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
}

// The same obligation for the operator-facing verbs beside it: a refusal
// without a reason sends the operator to the wrong problem.
func TestManagedDoltVerbsRefuseAHandedOffCityOutLoud(t *testing.T) {
	for _, tc := range []struct {
		verb string
		args []string
	}{
		{verb: "stop-managed", args: []string{"--port", "3307"}},
		{verb: "probe-managed", args: []string{"--port", "3307"}},
		{verb: "inspect-managed", args: []string{"--port", "3307"}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			city := handoffGuardTestCity(t)
			writeCommittedHandoffJournal(t, city)

			var stdout, stderr bytes.Buffer
			cmd := newDoltStateCmd(&stdout, &stderr)
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(append([]string{tc.verb, "--city", city}, tc.args...))
			if err := cmd.Execute(); err == nil {
				t.Fatalf("%s did not refuse a handed-off city\nstderr: %s", tc.verb, stderr.String())
			}
			if !strings.Contains(stderr.String(), "ownership handoff") {
				t.Errorf("%s refusal does not name the ownership handoff:\n%s", tc.verb, stderr.String())
			}
		})
	}
}
