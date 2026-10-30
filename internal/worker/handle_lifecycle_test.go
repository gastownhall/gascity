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

// rotatedKeyFixture is the session row a restart handoff leaves behind, plus
// the handle whose startCommand reads it: the row ran a conversation once (a
// past creation_complete_at), then RestartRequestPatch rotated its session
// key to freshKey — a key no conversation has run under — and put it to
// sleep. The handle probes root as its transcript search path.
type rotatedKeyFixture struct {
	handle  *SessionHandle
	store   *beads.MemStore
	id      string
	root    string
	workDir string
}

// newRotatedKeyHandle builds rotatedKeyFixture from the handle spec the row
// was created with. spec.WorkDir must name the row's work dir.
func newRotatedKeyHandle(t *testing.T, spec SessionSpec, freshKey string) rotatedKeyFixture {
	t.Helper()
	root := t.TempDir()
	store := beads.NewMemStore()
	manager := sessionpkg.NewManagerWithOptions(store, runtime.NewFake(), sessionpkg.WithCityPath(t.TempDir()))

	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly: true,
		Template: "worker",
		Title:    "Probe",
		Command:  spec.Command,
		WorkDir:  spec.WorkDir,
		Provider: spec.Provider,
		Resume:   spec.Resume,
	})
	if err != nil {
		t.Fatalf("CreateSession(BeadOnly): %v", err)
	}
	spec.ID = info.ID
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{root},
		Session:     spec,
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}

	patch := map[string]string(sessionpkg.RestartRequestPatch(freshKey, time.Now()))
	patch["state"] = string(sessionpkg.StateAsleep)
	patch["creation_complete_at"] = "2026-01-01T00:00:00Z"
	if err := store.SetMetadataBatch(info.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(handoff): %v", err)
	}
	return rotatedKeyFixture{handle: handle, store: store, id: info.ID, root: root, workDir: spec.WorkDir}
}

// writeKeyedTranscript writes the keyed claude transcript for key under root:
// <root>/<ProjectSlug(workDir)>/<key>.jsonl, the path the keyed probe
// resolves. A live session writes this file itself.
func writeKeyedTranscript(t *testing.T, root, workDir, key string) {
	t.Helper()
	dir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	writeWorkerTestJSONL(t, filepath.Join(dir, key+".jsonl"), []map[string]any{{"type": "user"}})
}

// beadMetadata is session id's persisted metadata map.
func beadMetadata(t *testing.T, store *beads.MemStore, id string) map[string]string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return b.Metadata
}

// TestStartCommandRotatedKeyTranscriptProbe pins how startCommand launches a
// row a restart handoff left behind: a rotated session key no conversation
// has run under. The keyed-transcript probe decides — the rule the
// reconciler's own start already applies — and the start stays read-only: the
// bead keeps its rotated key and metadata whatever it launches.
func TestStartCommandRotatedKeyTranscriptProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", "")
	const key = "k-rotated-1"
	claudeResume := sessionpkg.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"}
	for _, c := range []struct {
		name  string
		spec  SessionSpec
		patch func(t *testing.T, f rotatedKeyFixture)
		want  string
	}{
		{
			name: "AC-1 rotated key without a transcript starts a new conversation",
			spec: SessionSpec{Command: "claude", Provider: "claude", WorkDir: t.TempDir(), Resume: claudeResume},
			want: "claude --session-id " + key,
		},
		{
			name: "AC-2 rotated key with its transcript present resumes exactly the base resume command",
			spec: SessionSpec{Command: "claude", Provider: "claude", WorkDir: t.TempDir(), Resume: claudeResume},
			patch: func(t *testing.T, f rotatedKeyFixture) {
				writeKeyedTranscript(t, f.root, f.workDir, key)
			},
			want: sessionpkg.BuildResumeCommand(sessionpkg.Info{
				Command: "claude", Provider: "claude", ResumeFlag: "--resume", ResumeStyle: "flag", SessionKey: key,
			}),
		},
		{
			name: "AC-3 a committed started_config_hash resumes without a transcript",
			spec: SessionSpec{Command: "claude", Provider: "claude", WorkDir: t.TempDir(), Resume: claudeResume},
			patch: func(t *testing.T, f rotatedKeyFixture) {
				if err := f.store.SetMetadata(f.id, "started_config_hash", "hash-abc"); err != nil {
					t.Fatalf("SetMetadata(started_config_hash): %v", err)
				}
			},
			want: "claude --resume " + key,
		},
		{
			name: "AC-4 a provider the probe cannot read resumes",
			spec: SessionSpec{
				Command: "legacy-provider", Provider: "legacy-provider", WorkDir: t.TempDir(),
				Resume: sessionpkg.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"},
			},
			want: "legacy-provider --resume " + key,
		},
		{
			name: "a creating row with a stale creation-complete marker also starts a new conversation",
			spec: SessionSpec{Command: "claude", Provider: "claude", WorkDir: t.TempDir(), Resume: claudeResume},
			patch: func(t *testing.T, f rotatedKeyFixture) {
				if err := f.store.SetMetadata(f.id, "state", string(sessionpkg.StateCreating)); err != nil {
					t.Fatalf("SetMetadata(state): %v", err)
				}
			},
			want: "claude --session-id " + key,
		},
		{
			name: "no session-id flag resumes",
			spec: SessionSpec{
				Command: "claude", Provider: "claude", WorkDir: t.TempDir(),
				Resume: sessionpkg.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag"},
			},
			want: "claude --resume " + key,
		},
		{
			name: "an empty session key returns the base resume output",
			spec: SessionSpec{Command: "claude", Provider: "claude", WorkDir: t.TempDir(), Resume: claudeResume},
			patch: func(t *testing.T, f rotatedKeyFixture) {
				if err := f.store.SetMetadata(f.id, "session_key", ""); err != nil {
					t.Fatalf("SetMetadata(session_key): %v", err)
				}
			},
			want: "claude",
		},
		{
			name: "an empty work dir is not probeable and resumes",
			spec: SessionSpec{Command: "claude", Provider: "claude", WorkDir: t.TempDir(), Resume: claudeResume},
			patch: func(t *testing.T, f rotatedKeyFixture) {
				if err := f.store.SetMetadata(f.id, "work_dir", ""); err != nil {
					t.Fatalf("SetMetadata(work_dir): %v", err)
				}
			},
			want: "claude --resume " + key,
		},
		{
			name: "the handle's command overrides the stored one on the switched start",
			spec: SessionSpec{Command: "claude-alt", Provider: "claude", WorkDir: t.TempDir(), Resume: claudeResume},
			want: "claude-alt --session-id " + key,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRotatedKeyHandle(t, c.spec, key)
			if c.patch != nil {
				c.patch(t, f)
			}
			before := beadMetadata(t, f.store, f.id)
			got, err := f.handle.startCommand(f.id)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			if got != c.want {
				t.Fatalf("startCommand() = %q, want %q", got, c.want)
			}
			if after := beadMetadata(t, f.store, f.id); !reflect.DeepEqual(before, after) {
				t.Fatalf("metadata changed across startCommand: before %v after %v", before, after)
			}
		})
	}
}

// TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID runs the
// restart handoff end to end through the Attach funnel: a first start under
// the allocated key, a stop, the handoff's rotated key, then an operator
// attach that must name a new conversation under the rotated key rather than
// resume a conversation the provider never had.
func TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", "")
	workDir := t.TempDir()
	handle, store, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude --dangerously-skip-permissions",
		WorkDir:  workDir,
		Provider: "claude",
		Resume: sessionpkg.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
	})
	handle.adapter.SearchPaths = []string{t.TempDir()}

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start(first): %v", err)
	}
	if err := handle.Stop(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	const rotated = "k-attach-rotated-2"
	patch := map[string]string(sessionpkg.RestartRequestPatch(rotated, time.Now()))
	patch["state"] = string(sessionpkg.StateAsleep)
	if err := store.SetMetadataBatch(handle.currentSessionID(), patch); err != nil {
		t.Fatalf("SetMetadataBatch(handoff): %v", err)
	}

	// Resuming a suspended session is an operator's own act (D8): Attach.
	if err := handle.Attach(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Attach(second): %v", err)
	}
	secondStart := lastCall(sp.Calls, "Start")
	if secondStart == nil || secondStart == firstCall(sp.Calls, "Start") {
		t.Fatalf("runtime calls = %#v, want a second Start", sp.Calls)
	}
	if strings.Contains(secondStart.Config.Command, "--resume") {
		t.Fatalf("second start command = %q, want no --resume for a rotated key without a transcript", secondStart.Config.Command)
	}
	if !strings.Contains(secondStart.Config.Command, "--session-id "+rotated) {
		t.Fatalf("second start command = %q, want --session-id %s", secondStart.Config.Command, rotated)
	}
}
