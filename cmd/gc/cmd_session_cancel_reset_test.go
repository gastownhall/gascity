package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestDecideCancelReset covers the guard logic for `gc session cancel-reset`:
// clearing a pending reset is only safe for a LIVE session (a not-running
// session may be mid-restart, past the point of no return), and there must be a
// pending reset to cancel.
func TestDecideCancelReset(t *testing.T) {
	tests := []struct {
		name    string
		running bool
		meta    map[string]string
		want    cancelResetOutcome
	}{
		{
			name:    "live + continuation_reset_pending -> clear",
			running: true,
			meta:    map[string]string{"continuation_reset_pending": "true"},
			want:    cancelResetClear,
		},
		{
			name:    "live + restart_requested -> clear",
			running: true,
			meta:    map[string]string{"restart_requested": "true"},
			want:    cancelResetClear,
		},
		{
			name:    "live + no pending reset -> nothing to cancel",
			running: true,
			meta:    map[string]string{},
			want:    cancelResetNothingPending,
		},
		{
			name:    "not running (even with a pending reset) -> refuse",
			running: false,
			meta:    map[string]string{"continuation_reset_pending": "true"},
			want:    cancelResetRefuseNotRunning,
		},
		{
			name:    "not running, no flags -> refuse",
			running: false,
			meta:    map[string]string{},
			want:    cancelResetRefuseNotRunning,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideCancelReset(tc.running, tc.meta); got != tc.want {
				t.Errorf("decideCancelReset(%v, %v) = %v, want %v", tc.running, tc.meta, got, tc.want)
			}
		})
	}
}

// TestCmdSessionCancelReset_ClearsPendingResetOnLiveSession drives the command
// end-to-end: a running session carrying a pending reset has its reset-intent
// flags cleared in place, while its session_key (the live conversation) is
// preserved and the runtime stays up.
func TestCmdSessionCancelReset_ClearsPendingResetOnLiveSession(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-cancel-reset-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	fakeProvider := runtime.NewFake()
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fakeProvider, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const identity = "session-a"
	const sessionName = "s-gc-cancel-reset-live"
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                      identity,
			"template":                   "worker",
			"session_name":               sessionName,
			"state":                      "awake",
			"session_key":                "original-key",
			"started_config_hash":        "hash-before",
			"restart_requested":          "true",
			"continuation_reset_pending": "true",
			session.ResetCommittedAtKey:  "2026-07-10T12:00:00Z",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}
	if err := fakeProvider.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("fakeProvider.Start: %v", err)
	}
	if err := fakeProvider.SetMeta(sessionName, "GC_SESSION_ID", bead.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionCancelReset([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionCancelReset = %d, want 0; stderr=%s", code, stderr.String())
	}

	if !fakeProvider.IsRunning(sessionName) {
		t.Fatalf("session %q was stopped; cancel-reset must not recycle a live session", sessionName)
	}
	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(session bead): %v", err)
	}
	if got := updated.Metadata["restart_requested"]; got != "" {
		t.Fatalf("restart_requested = %q, want cleared", got)
	}
	if got := updated.Metadata["continuation_reset_pending"]; got != "" {
		t.Fatalf("continuation_reset_pending = %q, want cleared", got)
	}
	if got := updated.Metadata[session.ResetCommittedAtKey]; got != "" {
		t.Fatalf("reset_committed_at = %q, want cleared", got)
	}
	if got := updated.Metadata["session_key"]; got != "original-key" {
		t.Fatalf("session_key = %q, want preserved (live conversation must survive)", got)
	}
	if got := updated.Metadata["started_config_hash"]; got != "hash-before" {
		t.Fatalf("started_config_hash = %q, want preserved", got)
	}
}

// TestCmdSessionCancelReset_RefusesWhenNotRunning is the safety guard: clearing a
// pending reset on a session whose runtime is not alive could clobber a restart
// that is genuinely mid-flight, so the command refuses and leaves the flag intact.
func TestCmdSessionCancelReset_RefusesWhenNotRunning(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-cancel-reset-notrunning-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	fakeProvider := runtime.NewFake()
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fakeProvider, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const identity = "session-a"
	// The runtime is never started, so sp.IsRunning is false.
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                      identity,
			"template":                   "worker",
			"session_name":               "s-gc-cancel-reset-notrunning",
			"state":                      string(session.StateAsleep),
			"continuation_reset_pending": "true",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionCancelReset([]string{identity}, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdSessionCancelReset = %d, want 1 (refuse); stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "not running") {
		t.Fatalf("stderr = %q, want not-running refusal", stderr.String())
	}
	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(session bead): %v", err)
	}
	if got := updated.Metadata["continuation_reset_pending"]; got != "true" {
		t.Fatalf("continuation_reset_pending = %q, want unchanged (true) after refusal", got)
	}
}
