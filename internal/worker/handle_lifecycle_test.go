package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// rotatedKeyFixture is the session row a restart handoff leaves behind: a
// BeadOnly claude session with resume by key whose key was rotated to
// freshKey, asleep, its creation already confirmed (a past
// creation_complete_at) and nothing committed since (RestartRequestPatch
// clears started_config_hash). Every field but freshKey has the value that
// keeps the fixture on the AC-1 shape; a test overrides what its row needs.
type rotatedKeyFixture struct {
	provider     string // bead provider; "" is claude
	beadCommand  string // command stored on the bead; "" is claude
	specCommand  string // handle-spec command override; "" is none
	providerKind string // stamped provider_kind metadata; "" is none
	freshKey     string
	state        string // "" is asleep
	probeRoot    string // when non-empty, the only root the launch probe searches
	startedHash  string // committed started_config_hash; "" leaves it empty
}

// applyRestartHandoff rotates the row's session key to freshKey the way a
// controller handoff does — RestartRequestPatch, which clears
// started_config_hash — then leaves the row in state (default asleep) with
// its creation already confirmed and nothing committed since.
func applyRestartHandoff(t *testing.T, store *beads.MemStore, id, freshKey, state string) {
	t.Helper()
	patch := map[string]string(sessionpkg.RestartRequestPatch(freshKey, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
	if state == "" {
		state = string(sessionpkg.StateAsleep)
	}
	patch["state"] = state
	patch["creation_complete_at"] = "2026-01-01T00:00:00Z"
	if err := store.SetMetadataBatch(id, patch); err != nil {
		t.Fatalf("SetMetadataBatch(handoff): %v", err)
	}
}

// writeKeyedTranscript puts freshKey's keyed claude transcript for workDir
// under root, the file a probe that searches root finds.
func writeKeyedTranscript(t *testing.T, root, workDir, freshKey string) {
	t.Helper()
	dir := filepath.Join(root, sessionlog.ProjectSlug(workDir), freshKey+".jsonl")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(dir, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// newRotatedKeyHandle builds the fixture row and a handle over it. An empty
// HOME keeps the default transcript roots empty, so a probe without
// probeRoot reads nothing.
func newRotatedKeyHandle(t *testing.T, fx rotatedKeyFixture) (*SessionHandle, *beads.MemStore, sessionpkg.Info) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", "")
	if fx.provider == "" {
		fx.provider = "claude"
	}
	if fx.beadCommand == "" {
		fx.beadCommand = "claude"
	}
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))
	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly: true,
		Template: "worker",
		Title:    "Probe",
		Command:  fx.beadCommand,
		WorkDir:  t.TempDir(),
		Provider: fx.provider,
		Resume: sessionpkg.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
	})
	if err != nil {
		t.Fatalf("CreateSession(BeadOnly): %v", err)
	}
	applyRestartHandoff(t, store, info.ID, fx.freshKey, fx.state)
	if fx.startedHash != "" {
		if err := store.SetMetadata(info.ID, "started_config_hash", fx.startedHash); err != nil {
			t.Fatalf("SetMetadata(started_config_hash): %v", err)
		}
	}
	if fx.providerKind != "" {
		if err := store.SetMetadata(info.ID, "provider_kind", fx.providerKind); err != nil {
			t.Fatalf("SetMetadata(provider_kind): %v", err)
		}
	}

	spec := SessionSpec{
		ID:       info.ID,
		Template: "worker",
		Command:  fx.specCommand,
		Provider: fx.provider,
		Resume: sessionpkg.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
	}
	handle, err := NewSessionHandle(SessionHandleConfig{Manager: manager, Session: spec})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if fx.probeRoot != "" {
		handle.adapter.SearchPaths = []string{fx.probeRoot}
	}
	return handle, store, info
}

// beadMetadata snapshots the row's metadata, for asserting a start leaves it
// untouched.
func beadMetadata(t *testing.T, store *beads.MemStore, id string) map[string]string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	snapshot := make(map[string]string, len(b.Metadata))
	for k, v := range b.Metadata {
		snapshot[k] = v
	}
	return snapshot
}

// TestStartCommandRotatedKeyTranscriptProbe pins what startCommand builds
// for the row a restart handoff leaves: a rotated key with no committed
// conversation starts under the session-id flag when the keyed transcript
// probe finds no transcript for it, and keeps the resume path otherwise.
func TestStartCommandRotatedKeyTranscriptProbe(t *testing.T) {
	const freshKey = "k-rotated-2"
	tests := []struct {
		name string
		fx   rotatedKeyFixture
		// transcript writes freshKey's keyed transcript under the probe root
		// before the command is built, the file a live conversation leaves.
		transcript bool
		// wantCommand and wantFlag build the expected command around
		// freshKey; wantFlag "" expects the bare command (no conversation
		// flag at all).
		wantCommand string
		wantFlag    string
	}{
		{
			name:        "no transcript for the rotated key",
			fx:          rotatedKeyFixture{freshKey: freshKey},
			wantCommand: "claude",
			wantFlag:    "--session-id",
		},
		{
			// A transcript the probe finds is a conversation the resume
			// reattaches to: the exact base resume command, BuildResumeCommand
			// flag style.
			name:        "transcript present for the rotated key",
			fx:          rotatedKeyFixture{freshKey: freshKey},
			transcript:  true,
			wantCommand: "claude",
			wantFlag:    "--resume",
		},
		{
			// A committed conversation (a started_config_hash beside the key)
			// keeps the resume even without a findable transcript: only the
			// reconciler's stale-resume clear may reset a committed key, and a
			// transcript outside every probe root is not evidence the
			// conversation is gone.
			name:        "committed conversation without a transcript",
			fx:          rotatedKeyFixture{freshKey: freshKey, startedHash: "hash-abc"},
			wantCommand: "claude",
			wantFlag:    "--resume",
		},
		{
			name:        "creating row with a stale creation_complete_at",
			fx:          rotatedKeyFixture{freshKey: freshKey, state: string(sessionpkg.StateCreating)},
			wantCommand: "claude",
			wantFlag:    "--session-id",
		},
		{
			// The probe dispatches on the history provider (kind over name),
			// so a claude seat behind an alias name still reads probeable. The
			// spec command defaults to the spec provider (glm53), and the
			// switched start keeps the resume branch's precedence.
			name: "provider kind claude under an alias name",
			fx: rotatedKeyFixture{
				provider:     "glm53",
				providerKind: "claude",
				freshKey:     freshKey,
			},
			wantCommand: "glm53",
			wantFlag:    "--session-id",
		},
		{
			name: "spec command overrides the stored one on the switched start",
			fx: rotatedKeyFixture{
				specCommand: "claude-worker",
				freshKey:    freshKey,
			},
			wantCommand: "claude-worker",
			wantFlag:    "--session-id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.fx.probeRoot == "" {
				tt.fx.probeRoot = root
			}
			handle, store, info := newRotatedKeyHandle(t, tt.fx)
			workDir := info.WorkDir
			if tt.transcript {
				writeKeyedTranscript(t, tt.fx.probeRoot, workDir, freshKey)
			}
			before := beadMetadata(t, store, info.ID)

			got, err := handle.startCommand(info.ID)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			want := tt.wantCommand
			if tt.wantFlag != "" {
				want += " " + tt.wantFlag + " " + freshKey
			}
			if got != want {
				t.Fatalf("startCommand() = %q, want %q", got, want)
			}
			if after := beadMetadata(t, store, info.ID); !mapsEqual(after, before) {
				t.Fatalf("row metadata changed under startCommand: before %v after %v", before, after)
			}
		})
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID runs the
// guard through the Attach funnel: a live session is stopped, a controller
// handoff rotates its key to one that never had a conversation, and the
// operator's attach must start under the session-id flag rather than resume
// the unlaunched key.
func TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID(t *testing.T) {
	handle, store, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude --dangerously-skip-permissions",
		WorkDir:  t.TempDir(),
		Provider: "claude",
		Resume: sessionpkg.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
	})
	// An empty transcript root: the rotated key never has a conversation.
	handle.adapter.SearchPaths = []string{t.TempDir()}

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start(first): %v", err)
	}
	if err := handle.Stop(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	const freshKey = "k-handoff-2"
	applyRestartHandoff(t, store, handle.sessionID, freshKey, "")

	if err := handle.Attach(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Attach(after handoff): %v", err)
	}
	secondStart := lastCall(sp.Calls, "Start")
	if secondStart == nil || secondStart == firstCall(sp.Calls, "Start") {
		t.Fatalf("runtime calls = %#v, want a second Start", sp.Calls)
	}
	if got := secondStart.Config.Command; !strings.Contains(got, "--session-id "+freshKey) {
		t.Fatalf("second start command = %q, want --session-id %s", got, freshKey)
	}
	if strings.Contains(secondStart.Config.Command, "--resume") {
		t.Fatalf("second start command = %q, want no --resume for a key without a transcript", secondStart.Config.Command)
	}
}

func TestSessionHandleRestartWithoutKeyedTranscriptStartsFresh(t *testing.T) {
	handle, _, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude --dangerously-skip-permissions",
		WorkDir:  t.TempDir(),
		Provider: "claude",
		Resume: sessionpkg.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
	})
	// An empty transcript root: the stored session key never had a conversation.
	handle.adapter.SearchPaths = []string{t.TempDir()}

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start(first): %v", err)
	}
	if err := handle.Stop(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := handle.Attach(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Attach(second): %v", err)
	}
	secondStart := lastCall(sp.Calls, "Start")
	if secondStart == nil || secondStart == firstCall(sp.Calls, "Start") {
		t.Fatalf("runtime calls = %#v, want second Start", sp.Calls)
	}
	if strings.Contains(secondStart.Config.Command, "--resume") {
		t.Fatalf("second start command = %q, want no --resume for a key without a transcript", secondStart.Config.Command)
	}
	if !strings.Contains(secondStart.Config.Command, "--session-id") {
		t.Fatalf("second start command = %q, want --session-id for a key without a transcript", secondStart.Config.Command)
	}
}
