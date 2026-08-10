package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// recordingStartupDeathProvider records every command it is asked to launch and
// fails the first Start after arming with ErrSessionDiedDuringStartup, which is
// how a provider reports a resume key it no longer recognizes. The recorded
// commands are what let a test assert on the shape of the *relaunch*, not just
// on whether the call returned an error.
type recordingStartupDeathProvider struct {
	*runtime.Fake
	armed    bool
	commands []string
}

func (p *recordingStartupDeathProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.commands = append(p.commands, cfg.Command)
	if p.armed {
		p.armed = false
		return fmt.Errorf("%w: session %q", runtime.ErrSessionDiedDuringStartup, name)
	}
	return p.Fake.Start(ctx, name, cfg)
}

// newSuspendedResumableSession creates a suspended session whose provider is
// resume-capable, and returns the manager, the recording provider, the session
// info, and the session key minted at create time.
func newSuspendedResumableSession(t *testing.T) (*Manager, *recordingStartupDeathProvider, Info, string) {
	t.Helper()

	store := beads.NewMemStore()
	base := runtime.NewFake()
	sp := &recordingStartupDeathProvider{Fake: base}
	mgr := NewManagerWithOptions(store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "worker",
		Command:  "claude --dangerously",
		WorkDir:  "/tmp",
		Provider: "claude",
		Resume: ProviderResume{
			ResumeFlag:    "--resume",
			SessionIDFlag: "--session-id",
		},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	sessionKey := b.Metadata["session_key"]
	if sessionKey == "" {
		t.Fatal("expected a session_key in bead metadata after CreateSession with ResumeFlag")
	}

	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// Clear the key the way the lifecycle does when a resume key goes stale.
	// This is the divergence that makes the bug reachable: the resume command
	// was built while the key was present, and the key is gone by the time the
	// start actually runs.
	if err := store.SetMetadata(info.ID, "session_key", ""); err != nil {
		t.Fatalf("clearing session_key: %v", err)
	}

	return mgr, sp, info, sessionKey
}

// TestEnsureRunning_EmptySessionKeyStripsResumeAndStartsFresh is the regression
// test for the start loop: a resume command whose key the bead no longer holds
// died during startup, the fresh-start recovery was gated on a non-empty
// session_key, and so the identical doomed command was retried forever. The
// recovery must now be reachable and must relaunch without the resume flag.
func TestEnsureRunning_EmptySessionKeyStripsResumeAndStartsFresh(t *testing.T) {
	mgr, sp, info, sessionKey := newSuspendedResumableSession(t)

	resumeCmd := "claude --dangerously --resume " + sessionKey
	sp.commands = nil
	sp.armed = true

	if err := mgr.Send(context.Background(), info.ID, "hello", resumeCmd, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send should recover with a fresh start when session_key is empty, got: %v", err)
	}

	if !sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after the fresh relaunch")
	}

	if len(sp.commands) != 2 {
		t.Fatalf("expected the doomed start plus one fresh relaunch, got %d command(s): %q", len(sp.commands), sp.commands)
	}
	if sp.commands[0] != resumeCmd {
		t.Errorf("first launch should be the resume command as given, got %q", sp.commands[0])
	}
	relaunch := sp.commands[1]
	if strings.Contains(relaunch, "--resume") {
		t.Errorf("relaunch must not carry the resume flag, got %q", relaunch)
	}
	if strings.Contains(relaunch, sessionKey) {
		t.Errorf("relaunch must not carry the stale session key, got %q", relaunch)
	}
	if relaunch != "claude --dangerously" {
		t.Errorf("relaunch = %q, want the bare start command %q", relaunch, "claude --dangerously")
	}
}

// TestEnsureRunning_EmptySessionKeyWithoutResumeShapeDoesNotRelaunch guards the
// other half of the fix. When there is no key and the command carries no resume
// shape to strip, there is nothing to recover: relaunching it verbatim would
// repeat the same failure at the same cost, which is the waste the loop was made
// of. The original start error must propagate instead, and exactly one launch
// may be attempted.
func TestEnsureRunning_EmptySessionKeyWithoutResumeShapeDoesNotRelaunch(t *testing.T) {
	mgr, sp, info, _ := newSuspendedResumableSession(t)

	freshCmd := "claude --dangerously"
	sp.commands = nil
	sp.armed = true

	err := mgr.Send(context.Background(), info.ID, "hello", freshCmd, runtime.Config{WorkDir: "/tmp"})
	if err == nil {
		t.Fatal("Send should fail when the start dies and there is no resume shape to strip")
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Errorf("error should wrap ErrSessionDiedDuringStartup, got: %v", err)
	}

	if len(sp.commands) != 1 {
		t.Fatalf("expected exactly one launch attempt with nothing to strip, got %d: %q", len(sp.commands), sp.commands)
	}
}

// TestStartRuntimeOnly_EmptySessionKeyStripsResumeAndStartsFresh covers the
// second start call site. ensureRunning and ensureRunningRuntimeOnly carry
// separate copies of the recovery branch, and the reconciler reaches the
// runtime-only one: startPreparedStartCandidate calls StartResolved, which
// lands on StartRuntimeOnly. That is the path the supervisor retries, so it is
// the path the measured start loop ran on, and a test that only drives Send
// would leave it unpinned.
func TestStartRuntimeOnly_EmptySessionKeyStripsResumeAndStartsFresh(t *testing.T) {
	mgr, sp, info, sessionKey := newSuspendedResumableSession(t)

	resumeCmd := "claude --dangerously --resume " + sessionKey
	sp.commands = nil
	sp.armed = true

	if err := mgr.StartRuntimeOnly(context.Background(), info.ID, resumeCmd, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("StartRuntimeOnly should recover with a fresh start when session_key is empty, got: %v", err)
	}

	if !sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after the fresh relaunch")
	}

	if len(sp.commands) != 2 {
		t.Fatalf("expected the doomed start plus one fresh relaunch, got %d command(s): %q", len(sp.commands), sp.commands)
	}
	relaunch := sp.commands[1]
	if strings.Contains(relaunch, "--resume") {
		t.Errorf("relaunch must not carry the resume flag, got %q", relaunch)
	}
	if strings.Contains(relaunch, sessionKey) {
		t.Errorf("relaunch must not carry the stale session key, got %q", relaunch)
	}
	if relaunch != "claude --dangerously" {
		t.Errorf("relaunch = %q, want the bare start command %q", relaunch, "claude --dangerously")
	}
}

// TestStartRuntimeOnly_EmptySessionKeyWithoutResumeShapeDoesNotRelaunch is the
// runtime-only twin of the decline case. The "nothing to strip" branch is
// duplicated per call site rather than shared, so this pins the copy that the
// Send-driven test cannot reach.
func TestStartRuntimeOnly_EmptySessionKeyWithoutResumeShapeDoesNotRelaunch(t *testing.T) {
	mgr, sp, info, _ := newSuspendedResumableSession(t)

	freshCmd := "claude --dangerously"
	sp.commands = nil
	sp.armed = true

	err := mgr.StartRuntimeOnly(context.Background(), info.ID, freshCmd, runtime.Config{WorkDir: "/tmp"})
	if err == nil {
		t.Fatal("StartRuntimeOnly should fail when the start dies and there is no resume shape to strip")
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Errorf("error should wrap ErrSessionDiedDuringStartup, got: %v", err)
	}

	if len(sp.commands) != 1 {
		t.Fatalf("expected exactly one launch attempt with nothing to strip, got %d: %q", len(sp.commands), sp.commands)
	}
}
