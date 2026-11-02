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

// rotatedKeyRow builds the session row a restart handoff leaves — a fresh
// session key minted onto a bead that keeps a past creation_complete_at, its
// started_config_hash cleared by the handoff patch, asleep — plus the handle
// reading it, whose transcript search root is root, an empty temp directory.
// HOME is emptied so the default transcript roots stay out of the probe.
func rotatedKeyRow(t *testing.T, spec SessionSpec, provider, beadCommand string) (*SessionHandle, *beads.MemStore, sessionpkg.Info, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", "")
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))

	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly:  true,
		Template:  "worker",
		Title:     "Probe",
		Command:   beadCommand,
		WorkDir:   t.TempDir(),
		Provider:  provider,
		Transport: "",
		Resume: sessionpkg.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
	})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	root := t.TempDir()
	spec.ID = info.ID
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{root},
		Session:     spec,
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	return handle, store, info, root
}

// applyRestartHandoff mints key onto the row the way a request-restart
// handoff does — the patch clears started_config_hash and latches the
// continuation reset — and leaves the row asleep with a past
// creation_complete_at, the shape the first-start predicate misses.
func applyRestartHandoff(t *testing.T, store *beads.MemStore, id, key string) {
	t.Helper()
	kvs := map[string]string(sessionpkg.RestartRequestPatch(key, time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)))
	kvs["state"] = string(sessionpkg.StateAsleep)
	kvs["creation_complete_at"] = "2026-10-01T00:00:00Z"
	if err := store.SetMetadataBatch(id, kvs); err != nil {
		t.Fatalf("SetMetadataBatch(restart handoff): %v", err)
	}
}

// rotatedSessionKey is the session key the restart handoff mints in these tests.
const rotatedSessionKey = "ga-1evykr-k2"

// writeRotatedKeyTranscript writes the claude-family keyed transcript for
// rotatedSessionKey: <root>/<project slug>/<key>.jsonl.
func writeRotatedKeyTranscript(t *testing.T, root, workDir string) {
	t.Helper()
	dir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeWorkerTestJSONL(t, filepath.Join(dir, rotatedSessionKey+".jsonl"), []map[string]any{
		{"type": "user", "message": `{"role":"user","content":"hi"}`},
	})
}

// TestStartCommandRotatedKeyTranscriptProbe pins the command a worker-built
// start builds for a row a restart handoff left with a rotated key: with no
// keyed transcript anywhere the probe reads, the start names a new
// conversation with the stored key instead of resuming a key that never had
// one; a present transcript, a committed config hash, or a provider the probe
// cannot read keeps the resume the row asks for.
func TestStartCommandRotatedKeyTranscriptProbe(t *testing.T) {
	tests := []struct {
		name            string
		provider        string
		transcript      bool   // write key's transcript under the handle's search root
		state           string // bead state; empty keeps the handoff's asleep
		workDirEmpty    bool   // blank the row's work_dir
		noSessionIDFlag bool   // handle spec carries no session_id_flag
		emptyKey        bool   // apply the handoff patch without a key
		specCommand     string // handle-spec command overriding the bead's
		checkMetadata   bool   // assert the bead's metadata survives the call
		want            string // exact start command
	}{
		{
			name:          "absent transcript starts a new session under the stored key",
			provider:      "claude",
			checkMetadata: true,
			want:          "mycmd --session-id " + rotatedSessionKey,
		},
		{
			name:     "creating row with a stale creation marker starts a new session",
			provider: "claude",
			state:    string(sessionpkg.StateCreating),
			want:     "mycmd --session-id " + rotatedSessionKey,
		},
		{
			name:         "empty work dir keeps the resume",
			provider:     "claude",
			workDirEmpty: true,
			want:         "mycmd --resume " + rotatedSessionKey,
		},
		{
			name:            "no session-id flag keeps the resume",
			provider:        "claude",
			noSessionIDFlag: true,
			want:            "mycmd --resume " + rotatedSessionKey,
		},
		{
			name:     "empty key keeps the base output",
			provider: "claude",
			emptyKey: true,
			want:     "mycmd",
		},
		{
			name:        "handle-spec command wins in the switched start",
			provider:    "claude",
			specCommand: "spec-cmd",
			want:        "spec-cmd --session-id " + rotatedSessionKey,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := SessionSpec{
				Template: "worker",
				Command:  "mycmd",
				Provider: tt.provider,
				Resume: sessionpkg.ProviderResume{
					ResumeFlag:  "--resume",
					ResumeStyle: "flag",
				},
			}
			if !tt.noSessionIDFlag {
				spec.Resume.SessionIDFlag = "--session-id"
			}
			if tt.specCommand != "" {
				spec.Command = tt.specCommand
			}
			handle, store, info, root := rotatedKeyRow(t, spec, tt.provider, "mycmd")
			handoffKey := rotatedSessionKey
			if tt.emptyKey {
				handoffKey = ""
			}
			applyRestartHandoff(t, store, info.ID, handoffKey)
			if tt.state != "" {
				if err := store.SetMetadata(info.ID, "state", tt.state); err != nil {
					t.Fatalf("SetMetadata(state): %v", err)
				}
			}
			if tt.workDirEmpty {
				if err := store.SetMetadata(info.ID, "work_dir", ""); err != nil {
					t.Fatalf("SetMetadata(work_dir): %v", err)
				}
			}
			if tt.emptyKey {
				if err := store.SetMetadata(info.ID, "session_key", ""); err != nil {
					t.Fatalf("SetMetadata(session_key): %v", err)
				}
			}
			if tt.transcript {
				writeRotatedKeyTranscript(t, root, info.WorkDir)
			}

			var before map[string]string
			if tt.checkMetadata {
				bead, err := store.Get(info.ID)
				if err != nil {
					t.Fatalf("Get(before): %v", err)
				}
				before = bead.Metadata
			}
			got, err := handle.startCommand(info.ID)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			if got != tt.want {
				t.Fatalf("startCommand() = %q, want %q", got, tt.want)
			}
			if tt.checkMetadata {
				bead, err := store.Get(info.ID)
				if err != nil {
					t.Fatalf("Get(after): %v", err)
				}
				after := bead.Metadata
				for k, v := range before {
					if got, ok := after[k]; !ok || got != v {
						t.Fatalf("bead metadata key %q: before %q, after %q", k, v, got)
					}
				}
				for k, v := range after {
					if got, ok := before[k]; !ok || got != v {
						t.Fatalf("bead metadata key %q: after %q, before %q", k, v, got)
					}
				}
			}
		})
	}
}

// TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID runs the
// guard through the operator funnel a blank pane actually comes through:
// Start launches the first conversation under a minted key, a restart handoff
// rotates the key while the row sleeps, and the next Attach must name a new
// conversation with the rotated key instead of resuming a key that never had
// a transcript.
func TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", "")
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
	// An empty transcript root: neither the first key nor the rotated one has
	// a conversation on disk.
	handle.adapter.SearchPaths = []string{t.TempDir()}

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start(first): %v", err)
	}
	first := firstCall(sp.Calls, "Start")
	if first == nil {
		t.Fatalf("runtime calls = %#v, want initial Start", sp.Calls)
	}
	if !strings.Contains(first.Config.Command, "--session-id") || strings.Contains(first.Config.Command, "--resume") {
		t.Fatalf("first start command = %q, want --session-id and no --resume", first.Config.Command)
	}
	if err := handle.Stop(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	const rotated = "ga-1evykr-attach-k2"
	applyRestartHandoff(t, store, handle.currentSessionID(), rotated)

	if err := handle.Attach(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Attach(second): %v", err)
	}
	second := lastCall(sp.Calls, "Start")
	if second == nil || second == first {
		t.Fatalf("runtime calls = %#v, want a second Start", sp.Calls)
	}
	if !strings.Contains(second.Config.Command, "--session-id "+rotated) {
		t.Fatalf("second start command = %q, want --session-id %s", second.Config.Command, rotated)
	}
	if strings.Contains(second.Config.Command, "--resume") {
		t.Fatalf("second start command = %q, want no --resume for a key without a transcript", second.Config.Command)
	}
}
