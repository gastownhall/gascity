package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// makeSessionBead creates a minimal session bead for heartbeat tests.
func makeSessionBead(id, sessionName string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"session_name": sessionName,
		},
	}
}

func TestDoRuntimeHeartbeatSetsHeldUntil(t *testing.T) {
	const sessionName = "testpack__worker"
	const beadID = "bead-123"
	store := beads.NewMemStoreFrom(0, []beads.Bead{makeSessionBead(beadID, sessionName)}, nil)

	var stdout, stderr bytes.Buffer
	before := time.Now().Truncate(time.Second)
	code := doRuntimeHeartbeat(store, 45*time.Minute, sessionName, sessionName, false, &stdout, &stderr)
	after := time.Now().Add(time.Second).Truncate(time.Second)

	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s", code, stderr.String())
	}

	b, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("getting bead: %v", err)
	}
	heldUntilStr := b.Metadata["held_until"]
	if heldUntilStr == "" {
		t.Fatal("expected held_until to be set, got empty string")
	}
	heldUntil, err := time.Parse(time.RFC3339, heldUntilStr)
	if err != nil {
		t.Fatalf("parsing held_until %q: %v", heldUntilStr, err)
	}
	expectedMin := before.Add(45 * time.Minute)
	expectedMax := after.Add(45 * time.Minute)
	if heldUntil.Before(expectedMin) || heldUntil.After(expectedMax) {
		t.Errorf("held_until %v outside expected range [%v, %v]", heldUntil, expectedMin, expectedMax)
	}
	if !strings.Contains(stdout.String(), "Heartbeat set") {
		t.Errorf("expected stdout to contain 'Heartbeat set', got %q", stdout.String())
	}
}

func TestDoRuntimeHeartbeatCustomDuration(t *testing.T) {
	const sessionName = "testpack__worker"
	const beadID = "bead-456"
	store := beads.NewMemStoreFrom(0, []beads.Bead{makeSessionBead(beadID, sessionName)}, nil)

	var stdout, stderr bytes.Buffer
	before := time.Now().Truncate(time.Second)
	code := doRuntimeHeartbeat(store, 30*time.Minute, sessionName, sessionName, false, &stdout, &stderr)
	after := time.Now().Add(time.Second).Truncate(time.Second)

	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s", code, stderr.String())
	}

	b, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("getting bead: %v", err)
	}
	heldUntil, err := time.Parse(time.RFC3339, b.Metadata["held_until"])
	if err != nil {
		t.Fatalf("parsing held_until: %v", err)
	}
	expectedMin := before.Add(30 * time.Minute)
	expectedMax := after.Add(30 * time.Minute)
	if heldUntil.Before(expectedMin) || heldUntil.After(expectedMax) {
		t.Errorf("held_until %v outside expected range [%v, %v]", heldUntil, expectedMin, expectedMax)
	}
}

func TestDoRuntimeHeartbeatJSONOutput(t *testing.T) {
	const sessionName = "testpack__worker"
	const beadID = "bead-789"
	store := beads.NewMemStoreFrom(0, []beads.Bead{makeSessionBead(beadID, sessionName)}, nil)

	var stdout, stderr bytes.Buffer
	code := doRuntimeHeartbeat(store, 45*time.Minute, sessionName, sessionName, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s", code, stderr.String())
	}

	var out runtimeHeartbeatJSON
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("parsing JSON output: %v; raw: %s", err, stdout.String())
	}
	if !out.OK {
		t.Error("expected ok=true")
	}
	if out.Command != "runtime heartbeat" {
		t.Errorf("unexpected command %q", out.Command)
	}
	if out.HeldUntil == "" {
		t.Error("expected held_until in JSON output")
	}
	if out.Session != sessionName {
		t.Errorf("unexpected session %q", out.Session)
	}
}

func TestValidateHeartbeatDuration(t *testing.T) {
	tests := []struct {
		name    string
		d       time.Duration
		wantErr string
	}{
		{"below floor", minimumHeartbeatDuration - time.Second, "at least"},
		{"at floor", minimumHeartbeatDuration, ""},
		{"default", defaultHeartbeatDuration, ""},
		{"at ceiling", maximumHeartbeatDuration, ""},
		{"above ceiling", maximumHeartbeatDuration + time.Second, "at most"},
		{"absurdly large", 8760 * time.Hour, "at most"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHeartbeatDuration(tc.d)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateHeartbeatDuration(%s) = %v, want nil", tc.d, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateHeartbeatDuration(%s) = %v, want error containing %q", tc.d, err, tc.wantErr)
			}
		})
	}
}

// TestRuntimeHeartbeatCmdRejectsOversizedDuration exercises the command's flag
// validation: an over-ceiling --duration must fail with the friendly bound
// message before any session resolution is attempted.
func TestRuntimeHeartbeatCmdRejectsOversizedDuration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeHeartbeatCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--duration", "1000h"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for an over-ceiling --duration, got nil")
	}
	if !strings.Contains(stderr.String(), "must be at most") {
		t.Errorf("expected stderr to mention the ceiling, got %q", stderr.String())
	}
}

func TestDoRuntimeHeartbeatSessionNotFound(t *testing.T) {
	store := beads.NewMemStoreFrom(0, nil, nil)

	var stdout, stderr bytes.Buffer
	code := doRuntimeHeartbeat(store, 45*time.Minute, "ghost", "ghost", false, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit for missing session")
	}
	if !strings.Contains(stderr.String(), "resolving session") {
		t.Errorf("expected error about resolving session, got: %s", stderr.String())
	}
}

// TestDoRuntimeHeartbeatNeverShortensAHold: a shorter heartbeat leaves a
// later one in place (the disposition's Heartbeat cell, SR1-4c), and a
// longer one extends it.
func TestDoRuntimeHeartbeatNeverShortensAHold(t *testing.T) {
	const sessionName = "testpack__beater"
	const beadID = "bead-789"
	store := beads.NewMemStoreFrom(0, []beads.Bead{makeSessionBead(beadID, sessionName)}, nil)
	heldUntil := func() time.Time {
		t.Helper()
		b, err := store.Get(beadID)
		if err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse(time.RFC3339, b.Metadata["held_until"])
		if err != nil {
			t.Fatalf("held_until %q: %v", b.Metadata["held_until"], err)
		}
		return at
	}
	var stdout, stderr bytes.Buffer
	if code := doRuntimeHeartbeat(store, 2*time.Hour, sessionName, sessionName, false, &stdout, &stderr); code != 0 {
		t.Fatalf("2h heartbeat: exit %d; %s", code, stderr.String())
	}
	long := heldUntil()
	if code := doRuntimeHeartbeat(store, 10*time.Minute, sessionName, sessionName, false, &stdout, &stderr); code != 0 {
		t.Fatalf("10m heartbeat: exit %d; %s", code, stderr.String())
	}
	if got := heldUntil(); !got.Equal(long) {
		t.Fatalf("a shorter heartbeat moved held_until %v -> %v", long, got)
	}
	if code := doRuntimeHeartbeat(store, 3*time.Hour, sessionName, sessionName, false, &stdout, &stderr); code != 0 {
		t.Fatalf("3h heartbeat: exit %d; %s", code, stderr.String())
	}
	if got := heldUntil(); !got.After(long) {
		t.Fatalf("a longer heartbeat left held_until at %v, want past %v", got, long)
	}
}

// casLoser loses every conditional write to a newer revision.
type casLoser struct{ *beads.MemStore }

func (c casLoser) UpdateIfMatch(id string, rev int64, _ beads.UpdateOpts) error {
	return &beads.PreconditionFailedError{ID: id, Expected: rev, Current: rev + 1}
}

// TestDoRuntimeHeartbeatReportsALostWrite: a heartbeat whose fenced write
// loses every attempt says so and exits 1, never a silent success.
func TestDoRuntimeHeartbeatReportsALostWrite(t *testing.T) {
	m, id := stampedMem(t, gate.Require)
	if err := m.SetMetadata(id, "session_name", "testpack__lost"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doRuntimeHeartbeat(casLoser{m}, time.Hour, "testpack__lost", "testpack__lost", false, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "lost to concurrent writes") {
		t.Fatalf("heartbeat under a CAS that always loses = %d, stderr %q; want 1, lost", code, stderr.String())
	}
	if b, _ := m.Get(id); b.Metadata["held_until"] != "" {
		t.Fatalf("the lost heartbeat wrote held_until %q", b.Metadata["held_until"])
	}
}
