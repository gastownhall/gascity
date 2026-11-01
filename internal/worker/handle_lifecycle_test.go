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

// rotatedKey is the session key a restart handoff mints in these tests.
const rotatedKey = "6f1d2c3b-0a9e-4b8c-9d7e-112233445566"

// rotatedKeyResume is the claude-style resume config every rotated-key
// fixture declares.
var rotatedKeyResume = sessionpkg.ProviderResume{
	ResumeFlag:    "--resume",
	ResumeStyle:   "flag",
	SessionIDFlag: "--session-id",
}

// rotatedKeyFixture is an un-started handle over a session row a restart
// handoff left behind, plus the transcript root its handle searches.
type rotatedKeyFixture struct {
	handle *SessionHandle
	store  *beads.MemStore
	id     string
	root   string
}

// newRotatedKeyFixture creates a bead-only session for provider in
// beadWorkDir, applies the restart handoff for rotatedKey, and builds a handle
// over it with spec whose only transcript search path is a fresh temp root.
// HOME is pinned to an empty dir so the default roots hold nothing.
func newRotatedKeyFixture(t *testing.T, spec SessionSpec, provider, beadWorkDir string) rotatedKeyFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := beads.NewMemStore()
	manager := sessionpkg.NewManagerWithOptions(store, runtime.NewFake(), sessionpkg.WithCityPath(t.TempDir()))
	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly: true,
		Template: "worker",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  beadWorkDir,
		Provider: provider,
		Resume:   rotatedKeyResume,
	})
	if err != nil {
		t.Fatalf("CreateSession(bead-only): %v", err)
	}
	applyRestartHandoff(t, store, info.ID, rotatedKey)

	root := t.TempDir()
	spec.ID = info.ID
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		Session:     spec,
		SearchPaths: []string{root},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	return rotatedKeyFixture{handle: handle, store: store, id: info.ID, root: root}
}

// applyRestartHandoff writes the row a fresh-cycle restart handoff leaves:
// RestartRequestPatch rotates the key to key and clears started_config_hash,
// the row is put to sleep, and the previous start's creation_complete_at
// stays set.
func applyRestartHandoff(t *testing.T, store *beads.MemStore, id, key string) {
	t.Helper()
	patch := sessionpkg.RestartRequestPatch(key, time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	patch["state"] = string(sessionpkg.StateAsleep)
	patch["creation_complete_at"] = "2026-11-01T11:00:00Z"
	if err := store.SetMetadataBatch(id, patch); err != nil {
		t.Fatalf("SetMetadataBatch(restart handoff): %v", err)
	}
}

// writeKeyedTranscript writes key's claude transcript for workDir under root.
func writeKeyedTranscript(t *testing.T, root, workDir, key string) {
	t.Helper()
	dir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	writeWorkerTestJSONL(t, filepath.Join(dir, key+".jsonl"), []map[string]any{{"type": "user", "sessionId": key}})
}

func TestStartCommandRotatedKeyTranscriptProbe(t *testing.T) {
	sessionIDStart := "claude --session-id " + rotatedKey
	resumeStart := "claude --resume " + rotatedKey
	tests := []struct {
		name string
		// spec adjusts the handle spec (claude command and provider,
		// rotatedKeyResume); otherWorkDir is a dir distinct from the row's.
		spec func(spec *SessionSpec, otherWorkDir string)
		// provider is the stored and handle provider; default "claude".
		provider string
		// noBeadWorkDir creates the session row without a work dir.
		noBeadWorkDir bool
		// transcriptDir picks the work dir whose slug holds rotatedKey's
		// transcript under the handle root; nil writes none.
		transcriptDir func(beadWorkDir, otherWorkDir string) string
		meta          map[string]string
		want          string
	}{
		{
			name: "AC-1 no transcript starts with the session-id flag",
			want: sessionIDStart,
		},
		{
			name: "AC-1 probes the launch work dir from the hints",
			spec: func(spec *SessionSpec, otherWorkDir string) {
				spec.Hints.WorkDir = otherWorkDir
			},
			transcriptDir: func(beadWorkDir, _ string) string { return beadWorkDir },
			want:          sessionIDStart,
		},
		{
			name: "AC-1 creating row with a stale creation marker starts with the session-id flag",
			meta: map[string]string{"state": string(sessionpkg.StateCreating)},
			want: sessionIDStart,
		},
		{
			name: "AC-1 switched start keeps the resume command precedence",
			spec: func(spec *SessionSpec, _ string) {
				spec.Command = "claude --dangerously-skip-permissions"
			},
			want: "claude --dangerously-skip-permissions --session-id " + rotatedKey,
		},
		{
			name: "empty work dir in hints and row is not probeable and resumes",
			spec: func(spec *SessionSpec, _ string) {
				spec.WorkDir = ""
			},
			noBeadWorkDir: true,
			want:          resumeStart,
		},
		{
			name: "no session-id flag resumes",
			spec: func(spec *SessionSpec, _ string) {
				spec.Resume.SessionIDFlag = ""
			},
			want: resumeStart,
		},
		{
			name: "empty session key returns the base resume output",
			meta: map[string]string{"session_key": ""},
			want: "claude",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := tt.provider
			if provider == "" {
				provider = "claude"
			}
			beadWorkDir := t.TempDir()
			otherWorkDir := t.TempDir()
			if tt.noBeadWorkDir {
				beadWorkDir = ""
			}
			spec := SessionSpec{
				Template: "worker",
				Command:  "claude",
				Provider: provider,
				WorkDir:  beadWorkDir,
				Resume:   rotatedKeyResume,
			}
			if tt.spec != nil {
				tt.spec(&spec, otherWorkDir)
			}
			fx := newRotatedKeyFixture(t, spec, provider, beadWorkDir)
			if len(tt.meta) > 0 {
				if err := fx.store.SetMetadataBatch(fx.id, tt.meta); err != nil {
					t.Fatalf("SetMetadataBatch(%v): %v", tt.meta, err)
				}
			}
			if tt.transcriptDir != nil {
				writeKeyedTranscript(t, fx.root, tt.transcriptDir(beadWorkDir, otherWorkDir), rotatedKey)
			}
			before := beadMetadata(t, fx.store, fx.id)

			got, err := fx.handle.startCommand(fx.id)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			if got != tt.want {
				t.Fatalf("startCommand() = %q, want %q", got, tt.want)
			}
			if after := beadMetadata(t, fx.store, fx.id); !maps.Equal(before, after) {
				t.Fatalf("startCommand changed the row metadata:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

func TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	handle, store, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude --dangerously-skip-permissions",
		WorkDir:  t.TempDir(),
		Provider: "claude",
		Resume:   rotatedKeyResume,
	})
	handle.adapter.SearchPaths = []string{t.TempDir()}

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
		t.Fatalf("Attach(after handoff): %v", err)
	}
	last := lastCall(sp.Calls, "Start")
	if last == nil || last == firstCall(sp.Calls, "Start") {
		t.Fatalf("runtime calls = %#v, want a second Start", sp.Calls)
	}
	if !strings.Contains(last.Config.Command, "--session-id "+rotatedKey) || strings.Contains(last.Config.Command, "--resume") {
		t.Fatalf("start after handoff = %q, want --session-id %s and no --resume", last.Config.Command, rotatedKey)
	}
}

// beadMetadata returns a copy of the stored metadata of bead id.
func beadMetadata(t *testing.T, store *beads.MemStore, id string) map[string]string {
	t.Helper()
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return maps.Clone(bead.Metadata)
}
