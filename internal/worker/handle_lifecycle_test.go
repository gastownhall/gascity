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

// rotatedKeyFixture is a session row a restart handoff left behind: the
// handoff patch wrote a fresh session key, cleared started_config_hash and
// parked the row asleep, while creation_complete_at still records the
// previous incarnation's start.
type rotatedKeyFixture struct {
	handle  *SessionHandle
	store   *beads.MemStore
	id      string
	key     string
	root    string // the handle's only transcript search path
	workDir string // the work dir the bead records
}

const rotatedKey = "11111111-2222-4333-8444-555555555555"

func claudeResume() sessionpkg.ProviderResume {
	return sessionpkg.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"}
}

// newRotatedKeyFixture creates the bead with provider, then applies the
// restart handoff for rotatedKey. spec is the handle's session spec; its ID
// is filled in.
func newRotatedKeyFixture(t *testing.T, provider string, spec SessionSpec) rotatedKeyFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := beads.NewMemStore()
	manager := sessionpkg.NewManagerWithOptions(store, runtime.NewFake(), sessionpkg.WithCityPath(t.TempDir()))
	workDir := t.TempDir()
	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly: true,
		Template: "worker",
		Title:    "Probe",
		Command:  provider,
		WorkDir:  workDir,
		Provider: provider,
		Resume:   claudeResume(),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	patch := sessionpkg.RestartRequestPatch(rotatedKey, time.Now())
	patch["state"] = string(sessionpkg.StateAsleep)
	patch["creation_complete_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := store.SetMetadataBatch(info.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(handoff): %v", err)
	}
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
	return rotatedKeyFixture{handle: handle, store: store, id: info.ID, key: rotatedKey, root: root, workDir: workDir}
}

// writeKeyedTranscript writes key's claude transcript for workDir under root.
func writeKeyedTranscript(t *testing.T, root, workDir, key string) {
	t.Helper()
	dir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeWorkerTestJSONL(t, filepath.Join(dir, key+".jsonl"), []map[string]any{{"type": "user", "sessionId": key}})
}

func (f rotatedKeyFixture) metadata(t *testing.T) map[string]string {
	t.Helper()
	b, err := f.store.Get(f.id)
	if err != nil {
		t.Fatalf("Get(%q): %v", f.id, err)
	}
	return maps.Clone(b.Metadata)
}

func setMeta(t *testing.T, f rotatedKeyFixture, key, value string) {
	t.Helper()
	if err := f.store.SetMetadata(f.id, key, value); err != nil {
		t.Fatalf("SetMetadata(%s): %v", key, err)
	}
}

func claudeSpec() SessionSpec {
	return SessionSpec{Template: "worker", Command: "claude", Provider: "claude", Resume: claudeResume()}
}

func TestStartCommandRotatedKeyTranscriptProbe(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		spec     func() SessionSpec
		setup    func(t *testing.T, f rotatedKeyFixture)
		want     string
	}{
		{
			name:     "AC-1 transcript absent starts with session id",
			provider: "claude",
			spec:     claudeSpec,
			want:     "claude --session-id " + rotatedKey,
		},
		{
			name:     "AC-1 another key's transcript does not count",
			provider: "claude",
			spec:     claudeSpec,
			setup: func(t *testing.T, f rotatedKeyFixture) {
				writeKeyedTranscript(t, f.root, f.workDir, "99999999-2222-4333-8444-555555555555")
			},
			want: "claude --session-id " + rotatedKey,
		},
		{
			name:     "creating row with a stale creation_complete_at starts with session id",
			provider: "claude",
			spec:     claudeSpec,
			setup: func(t *testing.T, f rotatedKeyFixture) {
				setMeta(t, f, "state", string(sessionpkg.StateCreating))
			},
			want: "claude --session-id " + rotatedKey,
		},
		{
			name:     "switched start takes the handle command like the resume it replaces",
			provider: "claude",
			spec: func() SessionSpec {
				s := claudeSpec()
				s.Command = "claude --model opus"
				return s
			},
			want: "claude --model opus --session-id " + rotatedKey,
		},
		{
			name:     "empty work dir is not probeable and resumes",
			provider: "claude",
			spec:     claudeSpec,
			setup: func(t *testing.T, f rotatedKeyFixture) {
				setMeta(t, f, "work_dir", "")
			},
			want: "claude --resume " + rotatedKey,
		},
		{
			name:     "no session id flag resumes",
			provider: "claude",
			spec: func() SessionSpec {
				s := claudeSpec()
				s.Resume.SessionIDFlag = ""
				return s
			},
			want: "claude --resume " + rotatedKey,
		},
		{
			name:     "AC-2 transcript present resumes",
			provider: "claude",
			spec:     claudeSpec,
			setup: func(t *testing.T, f rotatedKeyFixture) {
				writeKeyedTranscript(t, f.root, f.workDir, f.key)
			},
			want: "claude --resume " + rotatedKey,
		},
		{
			name:     "AC-2 transcript under the launch work dir resumes",
			provider: "claude",
			spec:     claudeSpec,
			setup: func(t *testing.T, f rotatedKeyFixture) {
				launchDir := t.TempDir()
				f.handle.session.Hints.WorkDir = launchDir
				writeKeyedTranscript(t, f.root, launchDir, f.key)
			},
			want: "claude --resume " + rotatedKey,
		},
		{
			name:     "AC-3 committed conversation resumes without a transcript",
			provider: "claude",
			spec:     claudeSpec,
			setup: func(t *testing.T, f rotatedKeyFixture) {
				setMeta(t, f, "started_config_hash", "hash-abc")
			},
			want: "claude --resume " + rotatedKey,
		},
		{
			name:     "AC-4 provider the probe cannot read resumes",
			provider: "legacy-provider",
			spec: func() SessionSpec {
				return SessionSpec{Template: "worker", Command: "legacy-provider", Provider: "legacy-provider", Resume: claudeResume()}
			},
			want: "legacy-provider --resume " + rotatedKey,
		},
		{
			name:     "empty key keeps the base command",
			provider: "claude",
			spec:     claudeSpec,
			setup: func(t *testing.T, f rotatedKeyFixture) {
				setMeta(t, f, "session_key", "")
			},
			want: "claude",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRotatedKeyFixture(t, tt.provider, tt.spec())
			if tt.setup != nil {
				tt.setup(t, f)
			}
			before := f.metadata(t)
			got, err := f.handle.startCommand(f.id)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			if got != tt.want {
				t.Fatalf("startCommand() = %q, want %q", got, tt.want)
			}
			if after := f.metadata(t); !maps.Equal(before, after) {
				t.Fatalf("startCommand changed bead metadata:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID drives the
// handoff through the Attach funnel: the first start names K1, the handoff
// rotates the key to K2 and parks the row, and the operator's Attach must
// start a new conversation under K2 rather than resume it.
func TestSessionHandleAttachAfterRestartHandoffStartsWithSessionID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	handle, store, sp, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude --dangerously-skip-permissions",
		WorkDir:  t.TempDir(),
		Provider: "claude",
		Resume:   claudeResume(),
	})
	handle.searchPaths = []string{t.TempDir()}

	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start(first): %v", err)
	}
	if err := handle.Stop(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	patch := sessionpkg.RestartRequestPatch(rotatedKey, time.Now())
	patch["state"] = string(sessionpkg.StateAsleep)
	if err := store.SetMetadataBatch(handle.sessionID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(handoff): %v", err)
	}
	if err := handle.Attach(context.Background(), handleActor(t, handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	last := lastCall(sp.Calls, "Start")
	if last == nil || last == firstCall(sp.Calls, "Start") {
		t.Fatalf("runtime calls = %#v, want a second Start", sp.Calls)
	}
	if got := last.Config.Command; !strings.Contains(got, "--session-id "+rotatedKey) || strings.Contains(got, "--resume") {
		t.Fatalf("Attach start command = %q, want --session-id %s and no --resume", got, rotatedKey)
	}
}

func TestStartCommandRotatedKeyClaudeConfigDir(t *testing.T) {
	tests := []struct {
		name string
		// env returns CLAUDE_CONFIG_DIR's value and the root, if any, the
		// transcript is written under.
		env     func(t *testing.T) (value, transcriptRoot string)
		inHints bool // set it in the launch hints' env, not the session env
		want    string
	}{
		{
			name: "AC-5 transcript only under the absolute config dir resumes",
			env: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				return dir, filepath.Join(dir, "projects")
			},
			want: "claude --resume " + rotatedKey,
		},
		{
			name: "AC-5 absolute config dir without the transcript starts with session id",
			env:  func(t *testing.T) (string, string) { return t.TempDir(), "" },
			want: "claude --session-id " + rotatedKey,
		},
		{
			name: "AC-5 absolute config dir in the launch hints resumes",
			env: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				return dir, filepath.Join(dir, "projects")
			},
			inHints: true,
			want:    "claude --resume " + rotatedKey,
		},
		{
			name: "AC-5 whitespace around an absolute config dir is trimmed",
			env: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				return "  " + dir + "  ", filepath.Join(dir, "projects")
			},
			want: "claude --resume " + rotatedKey,
		},
		{
			name: "relative config dir adds no root",
			env: func(t *testing.T) (string, string) {
				cwd := t.TempDir()
				t.Chdir(cwd)
				return "rel/claude", filepath.Join(cwd, "rel", "claude", "projects")
			},
			want: "claude --session-id " + rotatedKey,
		},
		{
			name: "tilde config dir adds no root",
			env: func(t *testing.T) (string, string) {
				home, err := os.UserHomeDir()
				if err != nil {
					t.Fatalf("UserHomeDir: %v", err)
				}
				return "~/.claude-alt", filepath.Join(home, ".claude-alt", "projects")
			},
			want: "claude --session-id " + rotatedKey,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRotatedKeyFixture(t, "claude", claudeSpec())
			value, transcriptRoot := tt.env(t)
			env := map[string]string{"CLAUDE_CONFIG_DIR": value}
			if tt.inHints {
				f.handle.session.Hints.Env = env
			} else {
				f.handle.session.Env = env
			}
			if transcriptRoot != "" {
				writeKeyedTranscript(t, transcriptRoot, f.workDir, f.key)
			}
			got, err := f.handle.startCommand(f.id)
			if err != nil {
				t.Fatalf("startCommand: %v", err)
			}
			if got != tt.want {
				t.Fatalf("startCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}
