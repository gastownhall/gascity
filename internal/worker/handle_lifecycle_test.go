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
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// rotatedKey is the session key a restart handoff mints onto the row.
const rotatedKey = "rotated-key-k"

// backfilledKey is the session key the session sync mints onto a row a churn
// reset left keyless.
const backfilledKey = "backfilled-key-k2"

// rotatedKeyOptions shapes the handle and row newRotatedKeyHandle builds.
type rotatedKeyOptions struct {
	provider      string // bead and handle provider; "claude" when empty
	providerKind  string // provider_kind stamped on the bead
	handleCommand string // handle command; "claude" when empty
	hintsWorkDir  bool   // the handle's launch hints name their own work dir
	noFlag        bool   // the handle resume config has no session-id flag
}

// rotatedKeyFixture is a session row left by a restart handoff and the handle
// that starts it.
type rotatedKeyFixture struct {
	handle  *SessionHandle
	store   *beads.MemStore
	sp      *runtime.Fake
	id      string
	workDir string // the dir the launch runs in, and the probe's slug key
	root    string // the handle's only transcript search path
}

// newRotatedKeyHandle builds a bead-only session and applies the restart
// handoff to it: RestartRequestPatch with rotatedKey, state asleep, and a past
// creation_complete_at, which leaves started_config_hash empty. HOME is a
// fresh temp dir, so the default transcript root holds nothing.
func newRotatedKeyHandle(t *testing.T, opts rotatedKeyOptions) rotatedKeyFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	provider := opts.provider
	if provider == "" {
		provider = "claude"
	}
	handleCommand := opts.handleCommand
	if handleCommand == "" {
		handleCommand = "claude"
	}
	resume := sessionpkg.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"}

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(store), sp, sessionpkg.WithStaleKeyDetectionWaiter(
		func(context.Context, string) error { return nil },
	), sessionpkg.WithCityPath(t.TempDir()))
	beadWorkDir := t.TempDir()
	extra := map[string]string{}
	if opts.providerKind != "" {
		extra["provider_kind"] = opts.providerKind
	}
	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly:  true,
		Template:  "worker",
		Title:     "Probe",
		Command:   "claude",
		WorkDir:   beadWorkDir,
		Provider:  provider,
		Resume:    resume,
		ExtraMeta: extra,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	patch := sessionpkg.RestartRequestPatch(rotatedKey, time.Now())
	patch["state"] = string(sessionpkg.StateAsleep)
	patch["creation_complete_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := store.SetMetadataBatch(info.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(restart handoff): %v", err)
	}

	handleResume := resume
	if opts.noFlag {
		handleResume.SessionIDFlag = ""
	}
	spec := SessionSpec{
		ID:       info.ID,
		Template: "worker",
		Command:  handleCommand,
		Provider: provider,
		Resume:   handleResume,
	}
	workDir := beadWorkDir
	if opts.hintsWorkDir {
		workDir = t.TempDir()
		spec.Hints.WorkDir = workDir
	}
	root := t.TempDir()
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{root},
		Session:     spec,
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	return rotatedKeyFixture{handle: handle, store: store, sp: sp, id: info.ID, workDir: workDir, root: root}
}

// writeKeyedTranscript writes rotatedKey's claude transcript for workDir under
// root.
func writeKeyedTranscript(t *testing.T, root, workDir string) {
	t.Helper()
	dir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	writeWorkerTestJSONL(t, filepath.Join(dir, rotatedKey+".jsonl"), []map[string]any{
		{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "hello"}},
	})
}

// commitStart records a committed start of f's row under its stored key, as
// the reconciler's start commit writes it: a started_config_hash, and the
// conversation reset the restart handoff left pending consumed.
func commitStart(t *testing.T, f rotatedKeyFixture) {
	t.Helper()
	patch := sessionpkg.CommitStartedPatch(sessionpkg.CommitStartedPatchInput{
		CoreHash: "hash-abc",
		Now:      time.Now().Add(-time.Hour),
	})
	if err := f.store.SetMetadataBatch(f.id, patch); err != nil {
		t.Fatalf("SetMetadataBatch(commit start): %v", err)
	}
}

func beadMetadata(t *testing.T, store *beads.MemStore, id string) map[string]string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return cloneStringMap(b.Metadata)
}

func TestStartCommandRotatedKeyTranscriptProbe(t *testing.T) {
	sessionID := "claude --session-id " + rotatedKey
	tests := []struct {
		name  string
		opts  rotatedKeyOptions
		setup func(t *testing.T, f rotatedKeyFixture)
		want  string
	}{
		{
			name: "AC-1 transcript absent starts with the session-id flag",
			want: sessionID,
		},
		{
			name: "creating with a stale creation_complete_at starts with the session-id flag",
			setup: func(t *testing.T, f rotatedKeyFixture) {
				if err := f.store.SetMetadata(f.id, "state", string(sessionpkg.StateCreating)); err != nil {
					t.Fatalf("SetMetadata(state): %v", err)
				}
			},
			want: sessionID,
		},
		{
			name: "claude provider_kind behind a custom name is probed as claude",
			opts: rotatedKeyOptions{provider: "glm53", providerKind: "claude"},
			want: sessionID,
		},
		{
			name: "transcript under the bead work dir does not count for a launch in the hints work dir",
			opts: rotatedKeyOptions{hintsWorkDir: true},
			setup: func(t *testing.T, f rotatedKeyFixture) {
				info, err := f.handle.manager.Get(f.id)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				writeKeyedTranscript(t, f.root, info.WorkDir)
			},
			want: sessionID,
		},
		{
			name: "AC-2 transcript present resumes",
			setup: func(t *testing.T, f rotatedKeyFixture) {
				writeKeyedTranscript(t, f.root, f.workDir)
			},
			want: "claude --resume " + rotatedKey,
		},
		{
			name: "claude transcript of a kimi-named claude seat resumes",
			opts: rotatedKeyOptions{provider: "kimi-k3-manifold", providerKind: "claude"},
			setup: func(t *testing.T, f rotatedKeyFixture) {
				writeKeyedTranscript(t, f.root, f.workDir)
			},
			want: "claude --resume " + rotatedKey,
		},
		{
			name:  "AC-3 committed conversation without a transcript resumes",
			setup: commitStart,
			want:  "claude --resume " + rotatedKey,
		},
		{
			name: "churn reset beside a committed hash with a backfilled key starts with the session-id flag",
			setup: func(t *testing.T, f rotatedKeyFixture) {
				commitStart(t, f)
				// A churn reset keeps started_config_hash and clears the key,
				// which the session sync then mints anew.
				if err := f.store.SetMetadataBatch(f.id, sessionpkg.ConversationResetPatch(false)); err != nil {
					t.Fatalf("SetMetadataBatch(churn reset): %v", err)
				}
				if err := f.store.SetMetadata(f.id, "session_key", backfilledKey); err != nil {
					t.Fatalf("SetMetadata(session_key): %v", err)
				}
			},
			want: "claude --session-id " + backfilledKey,
		},
		{
			name: "AC-4 provider the probe cannot read resumes",
			opts: rotatedKeyOptions{provider: "legacy-provider"},
			want: "claude --resume " + rotatedKey,
		},
		{
			name: "switched start keeps the resume command precedence",
			opts: rotatedKeyOptions{handleCommand: "claude --model opus"},
			want: "claude --model opus --session-id " + rotatedKey,
		},
		{
			name: "no session-id flag resumes",
			opts: rotatedKeyOptions{noFlag: true},
			want: "claude --resume " + rotatedKey,
		},
		{
			name: "empty work dir is not probeable and resumes",
			setup: func(t *testing.T, f rotatedKeyFixture) {
				if err := f.store.SetMetadata(f.id, "work_dir", ""); err != nil {
					t.Fatalf("SetMetadata(work_dir): %v", err)
				}
			},
			want: "claude --resume " + rotatedKey,
		},
		{
			name: "empty session key returns the base command",
			setup: func(t *testing.T, f rotatedKeyFixture) {
				if err := f.store.SetMetadata(f.id, "session_key", ""); err != nil {
					t.Fatalf("SetMetadata(session_key): %v", err)
				}
			},
			want: "claude",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRotatedKeyHandle(t, tt.opts)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			before := beadMetadata(t, f.store, f.id)

			got, err := f.handle.startCommand(f.id)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			if got != tt.want {
				t.Fatalf("startCommand() = %q, want %q", got, tt.want)
			}
			if after := beadMetadata(t, f.store, f.id); !reflect.DeepEqual(before, after) {
				t.Fatalf("startCommand changed bead metadata:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID drives the
// rotated key through a worker-built start: a first start under K1, a stop, a
// restart handoff that mints K2 and leaves the row asleep, then an operator
// attach.
func TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(store), sp, sessionpkg.WithStaleKeyDetectionWaiter(
		func(context.Context, string) error { return nil },
	), sessionpkg.WithCityPath(t.TempDir()))
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{t.TempDir()},
		Session: SessionSpec{
			Profile:  ProfileClaudeTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "claude",
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
	first, err := manager.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if start := lastCall(sp.Calls, "Start"); start == nil || !strings.Contains(start.Config.Command, "--session-id "+first.SessionKey) {
		t.Fatalf("first start = %#v, want --session-id %s", start, first.SessionKey)
	}
	if err := handle.Stop(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	patch := sessionpkg.RestartRequestPatch(rotatedKey, time.Now())
	patch["state"] = string(sessionpkg.StateAsleep)
	if err := store.SetMetadataBatch(handle.sessionID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(restart handoff): %v", err)
	}

	sp.Calls = nil
	if err := handle.Attach(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	start := lastCall(sp.Calls, "Start")
	if start == nil {
		t.Fatalf("runtime calls = %#v, want Start", sp.Calls)
	}
	if strings.Contains(start.Config.Command, "--resume") || !strings.Contains(start.Config.Command, "--session-id "+rotatedKey) {
		t.Fatalf("attach start command = %q, want --session-id %s and no --resume", start.Config.Command, rotatedKey)
	}
}

func TestStartCommandRotatedKeyClaudeConfigDir(t *testing.T) {
	sessionID := "claude --session-id " + rotatedKey
	resume := "claude --resume " + rotatedKey
	absolute := func(t *testing.T) string { return filepath.Join(t.TempDir(), "claude") }
	tests := []struct {
		name string
		// configDir returns the CLAUDE_CONFIG_DIR value and the dir the
		// provider would resolve it to.
		configDir  func(t *testing.T) (value, dir string)
		inHints    bool
		transcript bool
		want       string
	}{
		{
			name:       "AC-5 absolute dir holding the transcript resumes",
			configDir:  func(t *testing.T) (string, string) { d := absolute(t); return "  " + d + "  ", d },
			transcript: true,
			want:       resume,
		},
		{
			name:      "AC-5 absolute dir without the transcript starts with the session-id flag",
			configDir: func(t *testing.T) (string, string) { d := absolute(t); return d, d },
			want:      sessionID,
		},
		{
			name:       "absolute dir in the launch hints env holding the transcript resumes",
			configDir:  func(t *testing.T) (string, string) { d := absolute(t); return d, d },
			inHints:    true,
			transcript: true,
			want:       resume,
		},
		{
			name: "relative dir adds no root",
			configDir: func(t *testing.T) (string, string) {
				cwd := t.TempDir()
				t.Chdir(cwd)
				return "rel/claude", filepath.Join(cwd, "rel", "claude")
			},
			transcript: true,
			want:       sessionID,
		},
		{
			name: "home-relative dir adds no root",
			configDir: func(*testing.T) (string, string) {
				return "~/.claude-alt", filepath.Join(os.Getenv("HOME"), ".claude-alt")
			},
			transcript: true,
			want:       sessionID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRotatedKeyHandle(t, rotatedKeyOptions{})
			value, dir := tt.configDir(t)
			env := map[string]string{"CLAUDE_CONFIG_DIR": value}
			if tt.inHints {
				f.handle.session.Hints.Env = env
			} else {
				f.handle.session.Env = env
			}
			if tt.transcript {
				writeKeyedTranscript(t, filepath.Join(dir, "projects"), f.workDir)
			}
			before := beadMetadata(t, f.store, f.id)

			got, err := f.handle.startCommand(f.id)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			if got != tt.want {
				t.Fatalf("startCommand() = %q, want %q", got, tt.want)
			}
			if after := beadMetadata(t, f.store, f.id); !reflect.DeepEqual(before, after) {
				t.Fatalf("startCommand changed bead metadata:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}
