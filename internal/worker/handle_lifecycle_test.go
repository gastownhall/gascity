package worker

import (
	"context"
	"maps"
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

// rotatedKey is the session key a restart handoff mints onto the row.
const rotatedKey = "rotated-key-1"

// rotatedKeyResume is a claude-style resume config with a session-id flag.
var rotatedKeyResume = sessionpkg.ProviderResume{
	ResumeFlag:    "--resume",
	ResumeStyle:   "flag",
	SessionIDFlag: "--session-id",
}

// rotatedKeyFixture is a bead-only session row as a restart handoff leaves it,
// plus the handle that builds its next start command.
type rotatedKeyFixture struct {
	store *beads.MemStore
	id    string
	// beadWorkDir is the work dir stored on the bead.
	beadWorkDir string
	// root is the handle's only transcript search path; HOME is empty.
	root string
	spec SessionSpec
}

// newRotatedKeyFixture creates a bead-only session for provider and applies
// the restart handoff for rotatedKey: RestartRequestPatch, state asleep, and
// the creation_complete_at an earlier start left behind.
func newRotatedKeyFixture(t *testing.T, provider string) *rotatedKeyFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", "")
	store := beads.NewMemStore()
	manager := sessionpkg.NewManagerWithOptions(store, runtime.NewFake(), sessionpkg.WithCityPath(t.TempDir()))
	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly: true,
		Template: "worker",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: provider,
		Resume:   rotatedKeyResume,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	applyRestartHandoff(t, store, info.ID, rotatedKey)
	return &rotatedKeyFixture{
		store:       store,
		id:          info.ID,
		beadWorkDir: info.WorkDir,
		root:        t.TempDir(),
		spec: SessionSpec{
			ID:       info.ID,
			Template: "worker",
			Command:  "claude",
			Provider: provider,
			Resume:   rotatedKeyResume,
		},
	}
}

// applyRestartHandoff writes what a restart handoff leaves on row id: the
// rotated key with its RestartRequestPatch, state asleep, and a
// creation_complete_at from the start before it.
func applyRestartHandoff(t *testing.T, store *beads.MemStore, id, key string) {
	t.Helper()
	now := time.Now().UTC()
	patch := sessionpkg.RestartRequestPatch(key, now)
	patch["state"] = string(sessionpkg.StateAsleep)
	patch["creation_complete_at"] = now.Add(-time.Hour).Format(time.RFC3339)
	if err := store.SetMetadataBatch(id, patch); err != nil {
		t.Fatalf("SetMetadataBatch(restart handoff): %v", err)
	}
}

// handle builds the session handle over the fixture's row and spec.
func (f *rotatedKeyFixture) handle(t *testing.T) *SessionHandle {
	t.Helper()
	manager := sessionpkg.NewManagerWithOptions(f.store, runtime.NewFake(), sessionpkg.WithCityPath(t.TempDir()))
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{f.root},
		Session:     f.spec,
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	return handle
}

// setMetadata writes key=value on the fixture's row.
func (f *rotatedKeyFixture) setMetadata(t *testing.T, key, value string) {
	t.Helper()
	if err := f.store.SetMetadata(f.id, key, value); err != nil {
		t.Fatalf("SetMetadata(%s): %v", key, err)
	}
}

// writeClaudeTranscript writes the keyed claude transcript of key for workDir
// under root.
func writeClaudeTranscript(t *testing.T, root, workDir, key string) {
	t.Helper()
	dir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeWorkerTestJSONL(t, filepath.Join(dir, key+".jsonl"), []map[string]any{{"type": "user"}})
}

func TestStartCommandRotatedKeyTranscriptProbe(t *testing.T) {
	sessionIDStart := "claude --session-id " + rotatedKey
	resumeStart := "claude --resume " + rotatedKey
	tests := []struct {
		name     string
		provider string
		setup    func(t *testing.T, f *rotatedKeyFixture)
		want     string
	}{
		{
			name:     "transcript absent starts with the session id",
			provider: "claude",
			want:     sessionIDStart,
		},
		{
			name:     "transcript present resumes",
			provider: "claude",
			setup: func(t *testing.T, f *rotatedKeyFixture) {
				writeClaudeTranscript(t, f.root, f.beadWorkDir, rotatedKey)
			},
			want: resumeStart,
		},
		{
			name:     "committed conversation resumes",
			provider: "claude",
			setup: func(t *testing.T, f *rotatedKeyFixture) {
				f.setMetadata(t, "started_config_hash", "hash-abc")
			},
			want: resumeStart,
		},
		{
			name:     "provider the probe cannot read resumes",
			provider: "legacy-provider",
			want:     resumeStart,
		},
		{
			name:     "probe reads the launch work dir",
			provider: "claude",
			setup: func(t *testing.T, f *rotatedKeyFixture) {
				f.spec.Hints.WorkDir = t.TempDir()
				writeClaudeTranscript(t, f.root, f.beadWorkDir, rotatedKey)
			},
			want: sessionIDStart,
		},
		{
			name:     "creating with a stale creation_complete_at starts with the session id",
			provider: "claude",
			setup: func(t *testing.T, f *rotatedKeyFixture) {
				f.setMetadata(t, "state", string(sessionpkg.StateCreating))
			},
			want: sessionIDStart,
		},
		{
			name:     "empty launch work dir resumes",
			provider: "claude",
			setup: func(t *testing.T, f *rotatedKeyFixture) {
				f.setMetadata(t, "work_dir", "")
			},
			want: resumeStart,
		},
		{
			name:     "no session id flag resumes",
			provider: "claude",
			setup: func(_ *testing.T, f *rotatedKeyFixture) {
				f.spec.Resume.SessionIDFlag = ""
			},
			want: resumeStart,
		},
		{
			name:     "empty session key keeps the base command",
			provider: "claude",
			setup: func(t *testing.T, f *rotatedKeyFixture) {
				f.setMetadata(t, "session_key", "")
			},
			want: "claude",
		},
		{
			name:     "switched start uses the handle command",
			provider: "claude",
			setup: func(_ *testing.T, f *rotatedKeyFixture) {
				f.spec.Command = "claude --model opus"
			},
			want: "claude --model opus --session-id " + rotatedKey,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRotatedKeyFixture(t, tt.provider)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			before, err := f.store.Get(f.id)
			if err != nil {
				t.Fatalf("Get(before): %v", err)
			}

			got, err := f.handle(t).startCommand(f.id)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			if got != tt.want {
				t.Fatalf("startCommand() = %q, want %q", got, tt.want)
			}

			after, err := f.store.Get(f.id)
			if err != nil {
				t.Fatalf("Get(after): %v", err)
			}
			if !maps.Equal(before.Metadata, after.Metadata) {
				t.Fatalf("startCommand changed bead metadata:\nbefore %v\nafter  %v", before.Metadata, after.Metadata)
			}
		})
	}
}

func TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", "")
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{t.TempDir()},
		Session: SessionSpec{
			Profile:  ProfileClaudeTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "claude --dangerously-skip-permissions",
			WorkDir:  t.TempDir(),
			Provider: "claude",
			Resume:   rotatedKeyResume,
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start(first): %v", err)
	}
	if first := firstCall(sp.Calls, "Start"); first == nil || !strings.Contains(first.Config.Command, "--session-id") {
		t.Fatalf("runtime calls = %#v, want a first Start with --session-id", sp.Calls)
	}
	if err := handle.Stop(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	applyRestartHandoff(t, store, handle.sessionID, rotatedKey)

	if err := handle.Attach(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	last := lastCall(sp.Calls, "Start")
	if last == nil || last == firstCall(sp.Calls, "Start") {
		t.Fatalf("runtime calls = %#v, want a second Start", sp.Calls)
	}
	if !strings.Contains(last.Config.Command, "--session-id "+rotatedKey) || strings.Contains(last.Config.Command, "--resume") {
		t.Fatalf("attach start command = %q, want --session-id %s and no --resume", last.Config.Command, rotatedKey)
	}
}
