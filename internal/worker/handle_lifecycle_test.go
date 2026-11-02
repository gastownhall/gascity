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

// The command and rotated key every rotated-key case launches and stores.
const (
	rotatedKeyCommand = "claude --dangerously-skip-permissions"
	rotatedKey        = "k-rotated-2"
)

// newRotatedKeyHandle builds a handle over a session bead left by a restart
// handoff: RestartRequestPatch rotated the session key to key, the row is
// asleep with a past creation_complete_at and an empty started_config_hash.
// The handle probes probeRoot for the rotated key's transcript, so one exists
// only when the case writes it there. The handle's spec command is
// specCommand, or the stored command when empty, and its spec env is specEnv.
func newRotatedKeyHandle(t *testing.T, probeRoot, specCommand, provider string, specEnv map[string]string, resume sessionpkg.ProviderResume) (*SessionHandle, *beads.MemStore, sessionpkg.Info) {
	t.Helper()

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))

	created, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly: true,
		Template: "worker",
		Title:    "Probe",
		Command:  rotatedKeyCommand,
		WorkDir:  t.TempDir(),
		Provider: provider,
		Resume:   resume,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	patch := sessionpkg.RestartRequestPatch(rotatedKey, time.Now())
	patch["state"] = string(sessionpkg.StateAsleep)
	patch["creation_complete_at"] = "2026-01-01T00:00:00Z"
	if err := store.SetMetadataBatch(created.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(restart handoff): %v", err)
	}

	if specCommand == "" {
		specCommand = rotatedKeyCommand
	}
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{probeRoot},
		Session: SessionSpec{
			ID:       created.ID,
			Template: "worker",
			Title:    "Probe",
			Command:  specCommand,
			WorkDir:  created.WorkDir,
			Provider: provider,
			Env:      specEnv,
			Resume:   resume,
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}

	info, err := manager.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return handle, store, info
}

// TestStartCommandRotatedKeyTranscriptProbe pins the launch command a
// worker-built start builds for a session row left by a restart handoff: the
// rotated key never launched a conversation, so the start opens a fresh one
// under the stored key with the provider's session-id flag unless the keyed
// transcript a resume would reattach to is probeable and present. The bead is
// never written: the key stays stored with its metadata unchanged.
func TestStartCommandRotatedKeyTranscriptProbe(t *testing.T) {
	command, key := rotatedKeyCommand, rotatedKey

	for _, tt := range []struct {
		name          string
		provider      string // empty is claude
		specCommand   string // empty is the stored command
		dropSessionID bool   // true drops the spec's session-id flag
		transcript    bool
		meta          map[string]string
		want          string
	}{
		{
			name:       "rotated key without a transcript starts with the session-id flag",
			transcript: false,
			want:       command + " --session-id " + key,
		},
		{
			name:       "rotated key with a transcript resumes",
			transcript: true,
			want:       command + " --resume " + key,
		},
		{
			name: "a committed hash without a transcript keeps the resume",
			meta: map[string]string{
				"started_config_hash": "hash-abc",
			},
			want: command + " --resume " + key,
		},
		{
			name:     "a provider the probe cannot read keeps the resume",
			provider: "legacy-provider",
			want:     command + " --resume " + key,
		},
		{
			name: "a creating row with a stale creation marker starts with the session-id flag",
			meta: map[string]string{
				"state": string(sessionpkg.StateCreating),
			},
			want: command + " --session-id " + key,
		},
		{
			name: "an empty work dir keeps the resume",
			meta: map[string]string{
				"work_dir": "",
			},
			want: command + " --resume " + key,
		},
		{
			name:          "no session-id flag keeps the resume",
			dropSessionID: true,
			want:          command + " --resume " + key,
		},
		{
			name: "an empty session key returns the base resume output",
			meta: map[string]string{
				"session_key": "",
			},
			want: command,
		},
		{
			name:        "the handle command wins over the stored one on the switched start",
			specCommand: "claude-handle-cmd",
			want:        "claude-handle-cmd --session-id " + key,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", "")

			provider := tt.provider
			if provider == "" {
				provider = "claude"
			}
			resume := sessionpkg.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"}
			if tt.dropSessionID {
				resume.SessionIDFlag = ""
			}

			root := t.TempDir()
			handle, store, info := newRotatedKeyHandle(t, root, tt.specCommand, provider, nil, resume)
			if len(tt.meta) > 0 {
				if err := store.SetMetadataBatch(info.ID, tt.meta); err != nil {
					t.Fatalf("SetMetadataBatch(%v): %v", tt.meta, err)
				}
			}
			if tt.transcript {
				dir := filepath.Join(root, sessionlog.ProjectSlug(info.WorkDir))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				writeWorkerTestJSONL(t, filepath.Join(dir, info.SessionKey+".jsonl"), []map[string]any{{"type": "user"}})
			}

			before, err := store.Get(info.ID)
			if err != nil {
				t.Fatalf("Get before: %v", err)
			}
			got, err := handle.startCommand(info.ID)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			if got != tt.want {
				t.Fatalf("startCommand() = %q, want %q", got, tt.want)
			}

			// The guard is read-only: the rotated key stays stored with its
			// metadata unchanged.
			after, err := store.Get(info.ID)
			if err != nil {
				t.Fatalf("Get after: %v", err)
			}
			if !reflect.DeepEqual(before.Metadata, after.Metadata) {
				t.Fatalf("metadata changed:\nbefore: %v\nafter:  %v", before.Metadata, after.Metadata)
			}
		})
	}
}

// TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID runs the
// restart handoff through the Attach funnel: a first start under the
// pre-rotation key, a stop, the handoff patch rotating the key with the row
// asleep, then an operator attach. The attach must launch a fresh conversation
// under the stored rotated key with the session-id flag — not a resume of a
// conversation that never ran.
func TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", "")

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))
	root := t.TempDir()
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{root},
		Session: SessionSpec{
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
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start(first): %v", err)
	}
	firstStart := firstCall(sp.Calls, "Start")
	if firstStart == nil {
		t.Fatalf("runtime calls = %#v, want initial Start", sp.Calls)
	}
	if !strings.Contains(firstStart.Config.Command, "--session-id") {
		t.Fatalf("first start command = %q, want --session-id", firstStart.Config.Command)
	}

	if err := handle.Stop(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	const rotated = "k-rotated-2"
	id := handle.currentSessionID()
	patch := sessionpkg.RestartRequestPatch(rotated, time.Now())
	patch["state"] = string(sessionpkg.StateAsleep)
	if err := store.SetMetadataBatch(id, patch); err != nil {
		t.Fatalf("SetMetadataBatch(restart handoff): %v", err)
	}

	if err := handle.Attach(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Attach(second): %v", err)
	}
	secondStart := lastCall(sp.Calls, "Start")
	if secondStart == nil || secondStart == firstStart {
		t.Fatalf("runtime calls = %#v, want second Start", sp.Calls)
	}
	if strings.Contains(secondStart.Config.Command, "--resume") {
		t.Fatalf("second start command = %q, want no --resume for a rotated key without a transcript", secondStart.Config.Command)
	}
	if !strings.Contains(secondStart.Config.Command, "--session-id "+rotated) {
		t.Fatalf("second start command = %q, want --session-id %s", secondStart.Config.Command, rotated)
	}
}

// assertRotatedStart runs startCommand for the rotated-key row and pins both
// the built command and the row being untouched.
func assertRotatedStart(t *testing.T, handle *SessionHandle, store *beads.MemStore, info sessionpkg.Info, want string) {
	t.Helper()
	before, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}
	got, err := handle.startCommand(info.ID)
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}
	if got != want {
		t.Fatalf("startCommand() = %q, want %q", got, want)
	}
	after, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if !reflect.DeepEqual(before.Metadata, after.Metadata) {
		t.Fatalf("metadata changed:\nbefore: %v\nafter:  %v", before.Metadata, after.Metadata)
	}
}

// TestStartCommandRotatedKeyClaudeConfigDir pins the rotated-key probe's roots
// when the launch env names a CLAUDE_CONFIG_DIR: an absolute dir's projects
// root joins the handle's search paths, so a transcript that exists only under
// it reads present and keeps the resume, while a relative or ~ value adds no
// root — the provider resolves those against its own working dir and home,
// which the probe cannot reproduce — so a transcript only under them reads
// absent and the start switches to the session-id flag.
func TestStartCommandRotatedKeyClaudeConfigDir(t *testing.T) {
	command, key := rotatedKeyCommand, rotatedKey
	resume := sessionpkg.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"}
	writeKeyed := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
		writeWorkerTestJSONL(t, filepath.Join(dir, key+".jsonl"), []map[string]any{{"type": "user"}})
	}

	t.Run("an absolute dir with the transcript keeps the resume", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("USERPROFILE", "")
		root := t.TempDir()
		configDir := filepath.Join(t.TempDir(), "claude-cfg")
		handle, store, info := newRotatedKeyHandle(t, root, "", "claude",
			map[string]string{"CLAUDE_CONFIG_DIR": "  " + configDir + "  "}, resume)
		writeKeyed(filepath.Join(configDir, "projects", sessionlog.ProjectSlug(info.WorkDir)))
		assertRotatedStart(t, handle, store, info, command+" --resume "+key)
	})

	t.Run("an absolute dir without the transcript starts with the session-id flag", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("USERPROFILE", "")
		root := t.TempDir()
		configDir := filepath.Join(t.TempDir(), "claude-cfg")
		handle, store, info := newRotatedKeyHandle(t, root, "", "claude",
			map[string]string{"CLAUDE_CONFIG_DIR": configDir}, resume)
		assertRotatedStart(t, handle, store, info, command+" --session-id "+key)
	})

	t.Run("a relative dir adds no root, so its transcript reads absent", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv("HOME", t.TempDir())
		t.Setenv("USERPROFILE", "")
		root := t.TempDir()
		handle, store, info := newRotatedKeyHandle(t, root, "", "claude",
			map[string]string{"CLAUDE_CONFIG_DIR": "relcfg"}, resume)
		writeKeyed(filepath.Join("relcfg", "projects", sessionlog.ProjectSlug(info.WorkDir)))
		assertRotatedStart(t, handle, store, info, command+" --session-id "+key)
	})

	t.Run("a home-relative dir adds no root, so its transcript reads absent", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", "")
		root := t.TempDir()
		handle, store, info := newRotatedKeyHandle(t, root, "", "claude",
			map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude-alt"}, resume)
		writeKeyed(filepath.Join(home, ".claude-alt", "projects", sessionlog.ProjectSlug(info.WorkDir)))
		assertRotatedStart(t, handle, store, info, command+" --session-id "+key)
	})
}
