package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// newRotatedKeyHandle builds the session row a restart handoff leaves: a fresh
// key K minted onto the bead (sessionpkg.RestartRequestPatch), the row asleep
// with a past creation_complete_at, so startCommand's first-start branch is off
// and only the rotated-key probe decides between --resume and --session-id.
// searchPaths seeds the handle config; a probe of the row reads the adapter's
// roots, so callers that need roots only the launch reads set
// handle.adapter.SearchPaths instead.
func newRotatedKeyHandle(t *testing.T, spec SessionSpec, searchPaths []string) (*SessionHandle, *beads.MemStore, sessionpkg.Info, string) {
	t.Helper()
	// Keep the default probe roots empty regardless of the host's home.
	t.Setenv("HOME", t.TempDir())

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))

	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly:  true,
		Template:  spec.Template,
		Title:     spec.Title,
		Command:   spec.Command,
		WorkDir:   t.TempDir(),
		Provider:  spec.Provider,
		Transport: "",
		Resume:    spec.Resume,
	})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}

	spec.ID = info.ID
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: searchPaths,
		Session:     spec,
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}

	rotatedKey := "rotated-" + info.SessionKey
	patch := map[string]string(sessionpkg.RestartRequestPatch(rotatedKey, time.Now()))
	patch["state"] = string(sessionpkg.StateAsleep)
	patch["creation_complete_at"] = "2026-01-01T00:00:00Z"
	if err := store.SetMetadataBatch(info.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(handoff): %v", err)
	}
	return handle, store, info, rotatedKey
}

// writeKeyedTranscript writes the keyed claude transcript for key under root,
// the file a live session leaves behind: <root>/<slug(workDir)>/<key>.jsonl.
func writeKeyedTranscript(t *testing.T, root, workDir, key string) {
	t.Helper()
	dir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeWorkerTestJSONL(t, filepath.Join(dir, key+".jsonl"), []map[string]any{
		{"type": "user", "message": "started"},
	})
}

func TestStartCommandRotatedKeyTranscriptProbe(t *testing.T) {
	tests := []struct {
		name string
		// setup mutates the row (and handle) after the handoff; want is the
		// exact command startCommand must build.
		setup func(t *testing.T, h *SessionHandle, store *beads.MemStore, info sessionpkg.Info, key string)
		want  func(key string) string
	}{
		{
			name:  "no transcript starts a new conversation under the stored key",
			setup: nil,
			want:  func(key string) string { return "claude --session-id " + key },
		},
		{
			name: "keyed transcript present under the handle's search path resumes",
			setup: func(t *testing.T, h *SessionHandle, _ *beads.MemStore, info sessionpkg.Info, key string) {
				writeKeyedTranscript(t, h.adapter.SearchPaths[0], info.WorkDir, key)
			},
			want: func(key string) string { return "claude --resume " + key },
		},
		{
			name: "committed started_config_hash resumes even with no transcript",
			setup: func(t *testing.T, _ *SessionHandle, store *beads.MemStore, info sessionpkg.Info, _ string) {
				if err := store.SetMetadata(info.ID, "started_config_hash", "hash-abc"); err != nil {
					t.Fatalf("SetMetadata(started_config_hash): %v", err)
				}
			},
			want: func(key string) string { return "claude --resume " + key },
		},
		{
			name: "creating row with a stale creation_complete_at still switches",
			setup: func(t *testing.T, _ *SessionHandle, store *beads.MemStore, info sessionpkg.Info, _ string) {
				if err := store.SetMetadata(info.ID, "state", string(sessionpkg.StateCreating)); err != nil {
					t.Fatalf("SetMetadata(state): %v", err)
				}
			},
			want: func(key string) string { return "claude --session-id " + key },
		},
		{
			name: "empty work dir resumes",
			setup: func(t *testing.T, _ *SessionHandle, store *beads.MemStore, info sessionpkg.Info, _ string) {
				if err := store.SetMetadata(info.ID, "work_dir", ""); err != nil {
					t.Fatalf("SetMetadata(work_dir): %v", err)
				}
			},
			want: func(key string) string { return "claude --resume " + key },
		},
		{
			name: "resume config without a session-id flag resumes",
			setup: func(_ *testing.T, h *SessionHandle, _ *beads.MemStore, _ sessionpkg.Info, _ string) {
				h.session.Resume.SessionIDFlag = ""
			},
			want: func(key string) string { return "claude --resume " + key },
		},
		{
			name: "empty session key returns the base output",
			setup: func(t *testing.T, _ *SessionHandle, store *beads.MemStore, info sessionpkg.Info, _ string) {
				if err := store.SetMetadata(info.ID, "session_key", ""); err != nil {
					t.Fatalf("SetMetadata(session_key): %v", err)
				}
			},
			want: func(string) string { return "claude" },
		},
		{
			name: "switched start uses the handle command (resume precedence)",
			setup: func(_ *testing.T, h *SessionHandle, _ *beads.MemStore, _ sessionpkg.Info, _ string) {
				h.session.Command = "spec-claude"
			},
			want: func(key string) string { return "spec-claude --session-id " + key },
		},
		{
			name: "transcript under the adapter's roots resumes",
			setup: func(t *testing.T, h *SessionHandle, _ *beads.MemStore, info sessionpkg.Info, key string) {
				root := t.TempDir()
				writeKeyedTranscript(t, root, info.WorkDir, key)
				// The launch reads the adapter's roots; the handle config's
				// list stays empty, as it does wherever the adapter is wired
				// after construction (the reconciler's StartResolved path).
				h.adapter.SearchPaths = []string{root}
			},
			want: func(key string) string { return "claude --resume " + key },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := SessionSpec{
				Template: "worker",
				Title:    "Probe",
				Command:  "claude",
				Provider: "claude",
				Resume: sessionpkg.ProviderResume{
					ResumeFlag:    "--resume",
					ResumeStyle:   "flag",
					SessionIDFlag: "--session-id",
				},
			}
			handle, store, info, key := newRotatedKeyHandle(t, spec, []string{t.TempDir()})
			if tt.setup != nil {
				tt.setup(t, handle, store, info, key)
			}

			before, err := store.Get(info.ID)
			if err != nil {
				t.Fatalf("Get(before): %v", err)
			}
			got, err := handle.startCommand(info.ID)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			if want := tt.want(key); got != want {
				t.Fatalf("startCommand() = %q, want %q", got, want)
			}

			after, err := store.Get(info.ID)
			if err != nil {
				t.Fatalf("Get(after): %v", err)
			}
			if !reflect.DeepEqual(before.Metadata, after.Metadata) {
				t.Fatalf("bead metadata changed across startCommand: before %v after %v", before.Metadata, after.Metadata)
			}
		})
	}
}

// TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID runs the
// operator-visible shape of the rotated-key bug: a session that started and
// stopped cleanly, then a restart handoff minting a key no conversation ever
// ran under. The next Attach must start a new conversation under that key, not
// resume a transcript that does not exist.
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
	// The launch reads an empty transcript root.
	handle.adapter.SearchPaths = []string{t.TempDir()}

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start(first): %v", err)
	}
	if err := handle.Stop(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	info, err := handle.manager.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rotated := "rotated-" + info.SessionKey
	patch := map[string]string(sessionpkg.RestartRequestPatch(rotated, time.Now()))
	patch["state"] = string(sessionpkg.StateAsleep)
	if err := store.SetMetadataBatch(info.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(handoff): %v", err)
	}

	if err := handle.Attach(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Attach(second): %v", err)
	}
	secondStart := lastCall(sp.Calls, "Start")
	if secondStart == nil || secondStart == firstCall(sp.Calls, "Start") {
		t.Fatalf("runtime calls = %#v, want second Start", sp.Calls)
	}
	if got := secondStart.Config.Command; !strings.Contains(got, "--session-id "+rotated) || strings.Contains(got, "--resume") {
		t.Fatalf("second start command = %q, want --session-id %s and no --resume", got, rotated)
	}
}
