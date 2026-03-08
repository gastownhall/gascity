package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

type startOverrideProvider struct {
	*runtime.Fake
	startErr error
}

func (p *startOverrideProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.startErr != nil {
		return p.startErr
	}
	return p.Fake.Start(ctx, name, cfg)
}

func TestCreate(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "my chat", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.Template != "helper" {
		t.Errorf("Template = %q, want %q", info.Template, "helper")
	}
	if info.Title != "my chat" {
		t.Errorf("Title = %q, want %q", info.Title, "my chat")
	}
	if info.State != StateActive {
		t.Errorf("State = %q, want %q", info.State, StateActive)
	}
	if info.ID == "" {
		t.Error("ID is empty")
	}

	// Verify the tmux session was started.
	if !sp.IsRunning(info.SessionName) {
		t.Error("runtime session not started")
	}

	// Verify bead was created with correct type and labels.
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Type != BeadType {
		t.Errorf("bead Type = %q, want %q", b.Type, BeadType)
	}
	if b.Status != "open" {
		t.Errorf("bead Status = %q, want %q", b.Status, "open")
	}
	hasLabel := false
	for _, l := range b.Labels {
		if l == LabelSession {
			hasLabel = true
		}
	}
	if !hasLabel {
		t.Errorf("bead missing label %q", LabelSession)
	}
}

func TestCreateRoutesACPSessionsThroughAutoProvider(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	mgr := NewManager(store, sessionauto.New(defaultSP, acpSP))

	info, err := mgr.CreateWithTransport(context.Background(), "helper", "acp chat", "claude", "/tmp", "claude", "acp", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if defaultSP.IsRunning(info.SessionName) {
		t.Fatalf("default backend should not own ACP session %q", info.SessionName)
	}
	if !acpSP.IsRunning(info.SessionName) {
		t.Fatalf("ACP backend should own session %q", info.SessionName)
	}
}

func TestSuspendAndResume(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Suspend.
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// Verify runtime session stopped.
	if sp.IsRunning(info.SessionName) {
		t.Error("runtime session should be stopped after suspend")
	}

	// Verify bead state updated.
	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateSuspended {
		t.Errorf("State = %q, want %q", got.State, StateSuspended)
	}

	// Suspend again is idempotent.
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend (idempotent): %v", err)
	}

	// Resume via Attach.
	err = mgr.Attach(context.Background(), info.ID, "claude --resume", runtime.Config{})
	if err != nil {
		t.Fatalf("Attach (resume): %v", err)
	}

	// Verify runtime session restarted.
	if !sp.IsRunning(info.SessionName) {
		t.Error("runtime session should be running after resume")
	}

	// Verify state back to active.
	got, err = mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateActive {
		t.Errorf("State = %q, want %q", got.State, StateActive)
	}
}

func TestClose(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Close active session.
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify runtime stopped.
	if sp.IsRunning(info.SessionName) {
		t.Error("runtime session should be stopped after close")
	}

	// Verify bead closed.
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Status != "closed" {
		t.Errorf("bead Status = %q, want %q", b.Status, "closed")
	}

	// Close again is idempotent.
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close (idempotent): %v", err)
	}
}

func TestCloseSuspended(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// Close suspended session.
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Status != "closed" {
		t.Errorf("bead Status = %q, want %q", b.Status, "closed")
	}
}

func TestList(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Create two sessions with different templates.
	_, err := mgr.Create(context.Background(), "helper", "first", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	info2, err := mgr.Create(context.Background(), "review", "second", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}

	// Suspend the second one.
	if err := mgr.Suspend(info2.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// List all (default excludes closed).
	sessions, err := mgr.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("List returned %d sessions, want 2", len(sessions))
	}

	// Filter by state.
	active, err := mgr.List("active", "")
	if err != nil {
		t.Fatalf("List active: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("List active returned %d, want 1", len(active))
	}

	suspended, err := mgr.List("suspended", "")
	if err != nil {
		t.Fatalf("List suspended: %v", err)
	}
	if len(suspended) != 1 {
		t.Errorf("List suspended returned %d, want 1", len(suspended))
	}

	// Filter by template.
	helpers, err := mgr.List("", "helper")
	if err != nil {
		t.Fatalf("List template: %v", err)
	}
	if len(helpers) != 1 {
		t.Errorf("List template=helper returned %d, want 1", len(helpers))
	}
}

func TestPeek(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Set canned peek output on the session name.
	sp.SetPeekOutput(info.SessionName, "hello world")

	out, err := mgr.Peek(info.ID, 50)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if out != "hello world" {
		t.Errorf("Peek output = %q, want %q", out, "hello world")
	}
}

func TestPeekSuspended(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	_, err = mgr.Peek(info.ID, 50)
	if err == nil {
		t.Error("Peek on suspended session should error")
	}
}

func TestAttachClosedErrors(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = mgr.Attach(context.Background(), info.ID, "claude --resume", runtime.Config{})
	if err == nil {
		t.Error("Attach to closed session should error")
	}
}

func TestSessionNameFor(t *testing.T) {
	tests := []struct {
		beadID string
		want   string
	}{
		{"gc-1", "s-gc-1"},
		{"gc-42", "s-gc-42"},
	}
	for _, tt := range tests {
		got := sessionNameFor(tt.beadID)
		if got != tt.want {
			t.Errorf("sessionNameFor(%q) = %q, want %q", tt.beadID, got, tt.want)
		}
	}
}

func TestListExcludesClosedFromActiveFilter(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Filtering by "active" should NOT return the closed session.
	active, err := mgr.List("active", "")
	if err != nil {
		t.Fatalf("List active: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("List active returned %d, want 0 (closed session leaked)", len(active))
	}
}

func TestAttachActiveReattach(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Attach to an active session — should reattach without restarting.
	err = mgr.Attach(context.Background(), info.ID, "claude --resume", runtime.Config{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Verify state is still active.
	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateActive {
		t.Errorf("State = %q, want %q", got.State, StateActive)
	}
}

func TestSuspendCrashedSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate crash by stopping the runtime behind the manager's back.
	_ = sp.Stop(info.SessionName)

	// Suspend should succeed even though runtime is dead.
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend crashed session: %v", err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateSuspended {
		t.Errorf("State = %q, want %q", got.State, StateSuspended)
	}
}

func TestCreateStoresCommand(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude --dangerously-skip-permissions", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify the command is stored in the bead metadata.
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["command"] != "claude --dangerously-skip-permissions" {
		t.Errorf("stored command = %q, want %q", b.Metadata["command"], "claude --dangerously-skip-permissions")
	}

	// Verify it's accessible via Info.
	if info.Command != "claude --dangerously-skip-permissions" {
		t.Errorf("Info.Command = %q, want %q", info.Command, "claude --dangerously-skip-permissions")
	}
}

func TestCreateWithSessionID(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	resume := ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}

	info, err := mgr.Create(context.Background(), "helper", "", "claude --dangerously-skip-permissions", "/tmp", "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Session key should be generated.
	if info.SessionKey == "" {
		t.Fatal("SessionKey is empty")
	}
	// Should look like a UUID.
	if len(info.SessionKey) != 36 {
		t.Errorf("SessionKey length = %d, want 36 (UUID)", len(info.SessionKey))
	}

	// Resume metadata should be stored.
	if info.ResumeFlag != "--resume" {
		t.Errorf("ResumeFlag = %q, want %q", info.ResumeFlag, "--resume")
	}
	if info.ResumeStyle != "flag" {
		t.Errorf("ResumeStyle = %q, want %q", info.ResumeStyle, "flag")
	}

	// The start command should include --session-id <uuid>.
	started := sp.LastStartConfig(info.SessionName)
	if started == nil {
		t.Fatal("session was not started")
	}
	if !strings.Contains(started.Command, "--session-id "+info.SessionKey) {
		t.Errorf("start command = %q, should contain --session-id %s", started.Command, info.SessionKey)
	}
}

func TestBuildResumeCommand(t *testing.T) {
	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "provider with resume flag",
			info: Info{
				Command:     "claude --dangerously-skip-permissions",
				Provider:    "claude",
				SessionKey:  "abc-123",
				ResumeFlag:  "--resume",
				ResumeStyle: "flag",
			},
			want: "claude --dangerously-skip-permissions --resume abc-123",
		},
		{
			name: "provider with subcommand style",
			info: Info{
				Command:     "codex",
				Provider:    "codex",
				SessionKey:  "abc-123",
				ResumeFlag:  "resume",
				ResumeStyle: "subcommand",
			},
			want: "codex resume abc-123",
		},
		{
			name: "no resume flag falls back to command",
			info: Info{
				Command:    "claude --dangerously-skip-permissions",
				Provider:   "claude",
				SessionKey: "abc-123",
			},
			want: "claude --dangerously-skip-permissions",
		},
		{
			name: "no session key falls back to command",
			info: Info{
				Command:    "claude --dangerously-skip-permissions",
				Provider:   "claude",
				ResumeFlag: "--resume",
			},
			want: "claude --dangerously-skip-permissions",
		},
		{
			name: "no command falls back to provider",
			info: Info{
				Provider:   "claude",
				SessionKey: "abc-123",
				ResumeFlag: "--resume",
			},
			want: "claude --resume abc-123",
		},
		{
			name: "subcommand with flags in command",
			info: Info{
				Command:     "codex --model o3",
				Provider:    "codex",
				SessionKey:  "abc-123",
				ResumeFlag:  "resume",
				ResumeStyle: "subcommand",
			},
			want: "codex resume abc-123 --model o3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildResumeCommand(tt.info)
			if got != tt.want {
				t.Errorf("BuildResumeCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCreateWithResumeFlagNoSessionIDFlag(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Provider supports resume but NOT Generate & Pass (no SessionIDFlag).
	resume := ProviderResume{
		ResumeFlag:  "resume",
		ResumeStyle: "subcommand",
		// SessionIDFlag deliberately empty.
	}

	info, err := mgr.Create(context.Background(), "helper", "", "codex --model o3", "/tmp", "codex", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// No session key should be generated since SessionIDFlag is empty.
	if info.SessionKey != "" {
		t.Errorf("SessionKey = %q, want empty (no SessionIDFlag)", info.SessionKey)
	}

	// The start command should be the original command (no --session-id injection).
	started := sp.LastStartConfig(info.SessionName)
	if started == nil {
		t.Fatal("session was not started")
	}
	if started.Command != "codex --model o3" {
		t.Errorf("start command = %q, want %q", started.Command, "codex --model o3")
	}

	// BuildResumeCommand should fall back to stored command (no key to resume with).
	resumeCmd := BuildResumeCommand(info)
	if resumeCmd != "codex --model o3" {
		t.Errorf("BuildResumeCommand() = %q, want %q (fallback to stored command)", resumeCmd, "codex --model o3")
	}
}

func TestCreateFailsCleanup(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFailFake() // all operations fail
	mgr := NewManager(store, sp)

	_, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err == nil {
		t.Fatal("Create should fail when provider fails")
	}

	// The bead should be closed (cleaned up).
	all, _ := store.List()
	for _, b := range all {
		if b.Type == BeadType && b.Status == "open" {
			t.Errorf("orphan session bead %s left open after failed create", b.ID)
		}
	}
}

func TestRename(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "old title", "echo test", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.Rename(info.ID, "new title"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "new title" {
		t.Errorf("Title = %q, want %q", got.Title, "new title")
	}
}

func TestRenameNonSessionBead(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Create a plain bead (not a session).
	b, err := store.Create(beads.Bead{Title: "not a session", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	err = mgr.Rename(b.ID, "new title")
	if err == nil {
		t.Error("Rename on non-session bead should error")
	}
}

func TestRenameNotFound(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	if err := mgr.Rename("nonexistent", "title"); err == nil {
		t.Error("Rename should fail for nonexistent session")
	}
}

func TestPrune(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Create and suspend two sessions.
	s1, err := mgr.Create(context.Background(), "default", "S1", "echo s1", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := mgr.Create(context.Background(), "default", "S2", "echo s2", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.Suspend(s1.ID); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Suspend(s2.ID); err != nil {
		t.Fatal(err)
	}

	// Prune with cutoff in the future — should prune both.
	pruned, err := mgr.Prune(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Errorf("pruned = %d, want 2", pruned)
	}

	// Both should be closed.
	sessions, err := mgr.List("all", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.ID == s1.ID || s.ID == s2.ID {
			if s.State != "" { // closed beads have empty state
				t.Errorf("session %s state = %q after prune, want empty (closed)", s.ID, s.State)
			}
		}
	}
}

func TestPruneUsesSuspendedAt(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Create two sessions and suspend them.
	old, err := mgr.Create(context.Background(), "default", "Old", "echo old", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}
	recent, err := mgr.Create(context.Background(), "default", "Recent", "echo recent", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.Suspend(old.ID); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Suspend(recent.ID); err != nil {
		t.Fatal(err)
	}

	// Backdate the "old" session's suspended_at to 10 days ago.
	tenDaysAgo := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if err := store.SetMetadata(old.ID, "suspended_at", tenDaysAgo); err != nil {
		t.Fatal(err)
	}

	// Cutoff at 7 days ago should prune only the old one.
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	pruned, err := mgr.Prune(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1", pruned)
	}

	// Old should be closed, recent should still be suspended.
	gotOld, err := mgr.Get(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOld.State != "" {
		t.Errorf("old session state = %q, want empty (closed)", gotOld.State)
	}

	gotRecent, err := mgr.Get(recent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRecent.State != StateSuspended {
		t.Errorf("recent session state = %q, want %q", gotRecent.State, StateSuspended)
	}
}

func TestSuspendSetsSuspendedAt(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}

	before := time.Now().Add(-time.Second)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatal(err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw := b.Metadata["suspended_at"]
	if raw == "" {
		t.Fatal("suspended_at metadata not set")
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("suspended_at not valid RFC3339: %v", err)
	}
	if ts.Before(before) {
		t.Errorf("suspended_at = %v, expected after %v", ts, before)
	}
}

func TestPruneSkipsActive(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	s1, err := mgr.Create(context.Background(), "default", "Active", "echo a", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}

	// Active session should not be pruned.
	pruned, err := mgr.Prune(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 0 {
		t.Errorf("pruned = %d, want 0 (active session should be skipped)", pruned)
	}

	got, err := mgr.Get(s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateActive {
		t.Errorf("active session state = %q, want %q", got.State, StateActive)
	}
}

func TestSendResumesSuspendedSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if err := mgr.Send(context.Background(), info.ID, "hello", "claude --resume "+info.SessionKey, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateActive {
		t.Errorf("State = %q, want %q", got.State, StateActive)
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after Send resumes it")
	}
	found := false
	for _, call := range sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want Nudge hello", sp.Calls)
	}
}

func TestSendResumesSuspendedACPSessionOnACPBackend(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	mgr := NewManager(store, sessionauto.New(defaultSP, acpSP))

	info, err := mgr.CreateWithTransport(context.Background(), "helper", "", "claude", "/tmp", "claude", "acp", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if err := mgr.Send(context.Background(), info.ID, "hello", "claude --resume", runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if defaultSP.IsRunning(info.SessionName) {
		t.Fatalf("default backend should not own resumed ACP session %q", info.SessionName)
	}
	if !acpSP.IsRunning(info.SessionName) {
		t.Fatalf("ACP backend should own resumed session %q", info.SessionName)
	}
	found := false
	for _, call := range acpSP.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ACP calls = %#v, want Nudge hello", acpSP.Calls)
	}
}

func TestSendReRoutesActiveACPSessionBeforeNudge(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	autoSP := sessionauto.New(defaultSP, acpSP)
	mgr := NewManager(store, autoSP)

	info, err := mgr.CreateWithTransport(context.Background(), "helper", "", "claude", "/tmp", "claude", "acp", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	autoSP.Unroute(info.SessionName)

	if err := mgr.Send(context.Background(), info.ID, "hello again", "claude --resume", runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if defaultSP.IsRunning(info.SessionName) {
		t.Fatalf("default backend should not own active ACP session %q", info.SessionName)
	}
	found := false
	for _, call := range acpSP.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello again" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ACP calls = %#v, want rerouted Nudge", acpSP.Calls)
	}
}

func TestSendConvergesWhenSessionAlreadyResumed(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := sp.Start(context.Background(), info.SessionName, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("fake concurrent Start: %v", err)
	}

	if err := mgr.Send(context.Background(), info.ID, "hello", "claude --resume", runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateActive {
		t.Errorf("State = %q, want %q", got.State, StateActive)
	}
	found := false
	for _, call := range sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want Nudge hello after converged resume", sp.Calls)
	}
}

func TestSendRequiresResumeCommandForSuspendedSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	err = mgr.Send(context.Background(), info.ID, "hello", "", runtime.Config{})
	if !errors.Is(err, ErrResumeRequired) {
		t.Fatalf("Send error = %v, want ErrResumeRequired", err)
	}
}

func TestSendClosedSessionReturnsErrSessionClosed(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = mgr.Send(context.Background(), info.ID, "hello", "claude --resume", runtime.Config{WorkDir: "/tmp"})
	if !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Send error = %v, want ErrSessionClosed", err)
	}
}

func TestSendDoesNotSuppressNonDuplicateResumeError(t *testing.T) {
	base := runtime.NewFake()
	sp := &startOverrideProvider{Fake: base}
	store := beads.NewMemStore()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := sp.Fake.Start(context.Background(), info.SessionName, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("fake concurrent Start: %v", err)
	}
	sp.startErr = errors.New("out of memory")

	err = mgr.Send(context.Background(), info.ID, "hello", "claude --resume", runtime.Config{WorkDir: "/tmp"})
	if err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("Send error = %v, want underlying non-duplicate start failure", err)
	}
}

func TestStopTurnInterruptsActiveSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}

	found := false
	for _, call := range sp.Calls {
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected Interrupt call")
	}
}

func TestPendingAndRespond(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	pending, supported, err := mgr.Pending(info.ID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if !supported {
		t.Fatal("Pending should report supported for runtime.Fake")
	}
	if pending == nil || pending.RequestID != "req-1" {
		t.Fatalf("Pending = %#v, want req-1", pending)
	}

	if err := mgr.Respond(info.ID, runtime.InteractionResponse{Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got, _, err := mgr.Pending(info.ID); err != nil {
		t.Fatalf("Pending after Respond: %v", err)
	} else if got != nil {
		t.Fatalf("pending should be cleared after Respond, got %#v", got)
	}
}

func TestTranscriptPathPrefersSessionKey(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	workDir := t.TempDir()
	resume := ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	info, err := mgr.Create(context.Background(), "helper", "", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	searchBase := t.TempDir()
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	keyPath := filepath.Join(slugDir, info.SessionKey+".jsonl")
	if err := os.WriteFile(keyPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(key): %v", err)
	}
	latestPath := filepath.Join(slugDir, "latest.jsonl")
	if err := os.WriteFile(latestPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(latest): %v", err)
	}

	path, err := mgr.TranscriptPath(info.ID, []string{searchBase})
	if err != nil {
		t.Fatalf("TranscriptPath: %v", err)
	}
	if path != keyPath {
		t.Errorf("TranscriptPath = %q, want %q", path, keyPath)
	}
}

func TestTranscriptPathAllowsClosedSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	workDir := t.TempDir()
	resume := ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	info, err := mgr.Create(context.Background(), "helper", "", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	searchBase := t.TempDir()
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	keyPath := filepath.Join(slugDir, info.SessionKey+".jsonl")
	if err := os.WriteFile(keyPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	path, err := mgr.TranscriptPath(info.ID, []string{searchBase})
	if err != nil {
		t.Fatalf("TranscriptPath: %v", err)
	}
	if path != keyPath {
		t.Errorf("TranscriptPath = %q, want %q for closed session", path, keyPath)
	}
}

func TestTranscriptPathSkipsAmbiguousWorkDirFallback(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	workDir := t.TempDir()
	if _, err := mgr.Create(context.Background(), "helper", "one", "claude", workDir, "claude", nil, ProviderResume{}, runtime.Config{}); err != nil {
		t.Fatalf("Create one: %v", err)
	}
	info2, err := mgr.Create(context.Background(), "helper", "two", "claude", workDir, "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create two: %v", err)
	}

	searchBase := t.TempDir()
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fallbackPath := filepath.Join(slugDir, "latest.jsonl")
	if err := os.WriteFile(fallbackPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	path, err := mgr.TranscriptPath(info2.ID, []string{searchBase})
	if err != nil {
		t.Fatalf("TranscriptPath: %v", err)
	}
	if path != "" {
		t.Errorf("TranscriptPath = %q, want empty when workdir fallback is ambiguous", path)
	}
}
