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

const rotatedKey = "rotated-key-k2"

// rotatedKeyOptions shapes a session row left by a restart handoff.
type rotatedKeyOptions struct {
	// provider is both the stored and the handle provider.
	provider string
	// storedCommand is the command on the bead; handleCommand the handle's.
	storedCommand string
	handleCommand string
	// noSessionIDFlag drops session_id_flag from the handle resume config.
	noSessionIDFlag bool
	// noWorkDir leaves the work dir empty on both the bead and the hints.
	noWorkDir bool
	// env is the handle session env.
	env map[string]string
	// meta is written onto the row after the handoff patch.
	meta map[string]string
}

type rotatedKeyFixture struct {
	handle  *SessionHandle
	store   *beads.MemStore
	sp      *runtime.Fake
	id      string
	workDir string
	// root is the handle's only search path.
	root string
}

// newRotatedKeyHandle builds a bead-only session, then applies a restart
// handoff to it: RestartRequestPatch(rotatedKey), state asleep and a past
// creation_complete_at, as the fresh-cycle path leaves it. HOME is a fresh
// temp dir, so the default transcript roots hold nothing.
func newRotatedKeyHandle(t *testing.T, opts rotatedKeyOptions) rotatedKeyFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", "")
	if opts.provider == "" {
		opts.provider = "claude"
	}
	if opts.storedCommand == "" {
		opts.storedCommand = "claude"
	}
	if opts.handleCommand == "" {
		opts.handleCommand = opts.storedCommand
	}
	workDir := ""
	if !opts.noWorkDir {
		workDir = t.TempDir()
	}
	resume := sessionpkg.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"}
	handleResume := resume
	if opts.noSessionIDFlag {
		handleResume.SessionIDFlag = ""
	}

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))
	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly: true,
		Template: "worker",
		Title:    "Probe",
		Command:  opts.storedCommand,
		WorkDir:  workDir,
		Provider: opts.provider,
		Resume:   resume,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	patch := sessionpkg.RestartRequestPatch(rotatedKey, time.Now())
	patch["state"] = string(sessionpkg.StateAsleep)
	patch["creation_complete_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	maps.Copy(patch, opts.meta)
	if err := store.SetMetadataBatch(info.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	root := t.TempDir()
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager: manager,
		Session: SessionSpec{
			ID:       info.ID,
			Template: "worker",
			Command:  opts.handleCommand,
			Provider: opts.provider,
			Env:      opts.env,
			Resume:   handleResume,
		},
		SearchPaths: []string{root},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	return rotatedKeyFixture{handle: handle, store: store, sp: sp, id: info.ID, workDir: workDir, root: root}
}

// writeKeyedClaudeTranscript writes the claude transcript of key for workDir
// under root.
func writeKeyedClaudeTranscript(t *testing.T, root, workDir, key string) {
	t.Helper()
	dir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorkerTestJSONL(t, filepath.Join(dir, key+".jsonl"), []map[string]any{{"type": "user", "sessionId": key}})
}

func (f rotatedKeyFixture) metadata(t *testing.T) map[string]string {
	t.Helper()
	b, err := f.store.Get(f.id)
	if err != nil {
		t.Fatalf("Get(%s): %v", f.id, err)
	}
	return maps.Clone(b.Metadata)
}

func TestStartCommandRotatedKeyTranscriptProbe(t *testing.T) {
	for _, c := range []struct {
		name string
		opts rotatedKeyOptions
		// transcript writes K's keyed transcript under the handle's root.
		transcript bool
		want       string
	}{
		{
			name: "AC-1 no transcript starts with the session id",
			want: "claude --session-id " + rotatedKey,
		},
		{
			name:       "AC-2 present transcript resumes",
			transcript: true,
			want:       "claude --resume " + rotatedKey,
		},
		{
			name: "AC-3 committed conversation resumes",
			opts: rotatedKeyOptions{meta: map[string]string{"started_config_hash": "hash-abc"}},
			want: "claude --resume " + rotatedKey,
		},
		{
			name: "creating with a stale creation_complete_at starts with the session id",
			opts: rotatedKeyOptions{meta: map[string]string{"state": string(sessionpkg.StateCreating)}},
			want: "claude --session-id " + rotatedKey,
		},
		{
			name: "empty work dir is not probeable and resumes",
			opts: rotatedKeyOptions{noWorkDir: true},
			want: "claude --resume " + rotatedKey,
		},
		{
			name: "no session id flag resumes",
			opts: rotatedKeyOptions{noSessionIDFlag: true},
			want: "claude --resume " + rotatedKey,
		},
		{
			name: "empty key returns the base command",
			opts: rotatedKeyOptions{meta: map[string]string{"session_key": ""}},
			want: "claude",
		},
		{
			name: "session id start uses the handle command over the stored one",
			opts: rotatedKeyOptions{storedCommand: "claude --old", handleCommand: "claude --new"},
			want: "claude --new --session-id " + rotatedKey,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRotatedKeyHandle(t, c.opts)
			if c.transcript {
				writeKeyedClaudeTranscript(t, f.root, f.workDir, rotatedKey)
			}
			before := f.metadata(t)

			got, err := f.handle.startCommand(f.id)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			if got != c.want {
				t.Fatalf("startCommand = %q, want %q", got, c.want)
			}
			if after := f.metadata(t); !maps.Equal(before, after) {
				t.Fatalf("startCommand changed bead metadata:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID drives the
// rotated key through the Attach funnel: a first start, a stop, a restart
// handoff to a fresh key and an operator attach.
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
	handle.adapter.SearchPaths = []string{t.TempDir()}

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start(first): %v", err)
	}
	if err := handle.Stop(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	patch := sessionpkg.RestartRequestPatch(rotatedKey, time.Now())
	patch["state"] = string(sessionpkg.StateAsleep)
	if err := store.SetMetadataBatch(handle.currentSessionID(), patch); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if err := handle.Attach(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	start := lastCall(sp.Calls, "Start")
	if start == nil || start == firstCall(sp.Calls, "Start") {
		t.Fatalf("runtime calls = %#v, want a second Start", sp.Calls)
	}
	if !strings.Contains(start.Config.Command, "--session-id "+rotatedKey) {
		t.Fatalf("attach start command = %q, want --session-id %s", start.Config.Command, rotatedKey)
	}
	if strings.Contains(start.Config.Command, "--resume") {
		t.Fatalf("attach start command = %q, want no --resume", start.Config.Command)
	}
}
