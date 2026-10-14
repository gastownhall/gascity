package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	capacityTestSessionKey = "key-before-refusal"
	capacityTestConfigHash = "hash-before-refusal"
	capacityTestResumeCmd  = "claude --dangerously --resume " + capacityTestSessionKey
	capacityTestPrimedAt   = "2026-09-29T00:00:00Z"
)

// routeRecordingFake is a runtime.Fake that also records ACP route release,
// so a test can prove a failed resume gave back the route it reserved.
type routeRecordingFake struct {
	*runtime.Fake
	unrouted []string
}

func (p *routeRecordingFake) RouteACP(string)     {}
func (p *routeRecordingFake) Unroute(name string) { p.unrouted = append(p.unrouted, name) }

// seedResumableACPSession stores a suspended, resume-capable session bead that
// holds a conversation key, and scripts every Start of it to fail with
// startErr(sessionName).
func seedResumableACPSession(t *testing.T, startErr func(sessName string) error) (*Manager, *routeRecordingFake, beads.Store, string, string) {
	t.Helper()
	store := beads.NewMemStore()
	sp := &routeRecordingFake{Fake: runtime.NewFake()}
	mgr := newTestManager(t, store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))

	b, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":               string(StateSuspended),
			"provider":            "claude",
			"transport":           "acp",
			"work_dir":            "/tmp",
			"command":             "claude --dangerously",
			"resume_flag":         "--resume",
			"session_id_flag":     "--session-id",
			"session_key":         capacityTestSessionKey,
			"started_config_hash": capacityTestConfigHash,
			PrimedAtMetadataKey:   capacityTestPrimedAt,
		},
	})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}
	sessName := sessionNameFor(b.ID)
	if err := store.SetMetadata(b.ID, "session_name", sessName); err != nil {
		t.Fatalf("SetMetadata(session_name): %v", err)
	}
	sp.StartErrors = map[string]error{sessName: startErr(sessName)}
	return mgr, sp, store, b.ID, sessName
}

func capacityRefusal(sessName string) error {
	return &runtime.CapacityError{
		ExitCode: runtime.ExitCodeTempFail,
		Source:   runtime.CapacitySourceExitStatus,
		Err:      fmt.Errorf("%w: session %q; last pane output:\ntunnel down", runtime.ErrSessionDiedDuringStartup, sessName),
	}
}

func startCallCount(sp *routeRecordingFake) int {
	n := 0
	for _, c := range sp.Calls {
		if c.Method == "Start" {
			n++
		}
	}
	return n
}

// assertCapacityRefusalKeptConversation fails unless a capacity-refused resume
// launched once, left the resume metadata exactly as it was, released its ACP
// route, and reported the typed refusal.
func assertCapacityRefusalKeptConversation(t *testing.T, err error, sp *routeRecordingFake, store beads.Store, id, sessName string) {
	t.Helper()
	if !runtime.IsProviderCapacity(err) {
		t.Fatalf("error = %v, want the typed capacity refusal", err)
	}
	if got := startCallCount(sp); got != 1 {
		t.Fatalf("Start called %d times, want 1: a refused endpoint must not get a fresh-start relaunch", got)
	}
	b, getErr := store.Get(id)
	if getErr != nil {
		t.Fatalf("Get bead: %v", getErr)
	}
	if got := b.Metadata["session_key"]; got != capacityTestSessionKey {
		t.Errorf("session_key = %q, want %q kept (capacity is not a stale key)", got, capacityTestSessionKey)
	}
	if got := b.Metadata["started_config_hash"]; got != capacityTestConfigHash {
		t.Errorf("started_config_hash = %q, want %q kept", got, capacityTestConfigHash)
	}
	if got := b.Metadata["continuation_reset_pending"]; got != "" {
		t.Errorf("continuation_reset_pending = %q, want unset (no conversation reset)", got)
	}
	if got := b.Metadata[PrimedAtMetadataKey]; got != capacityTestPrimedAt {
		t.Errorf("%s = %q, want %q kept (priming markers share the resume identity)", PrimedAtMetadataKey, got, capacityTestPrimedAt)
	}
	if !slices.Contains(sp.unrouted, sessName) {
		t.Errorf("unrouted = %v, want the ACP route for %q released", sp.unrouted, sessName)
	}
}

// The reconciler's start bridge (StartRuntimeOnly) must not treat a capacity
// refusal as a stale resume key: that relaunched into the same full endpoint
// and threw away the conversation handle.
func TestEnsureRunningRuntimeOnly_CapacityRefusalSkipsFreshRetryAndKeepsKey(t *testing.T) {
	mgr, sp, store, id, sessName := seedResumableACPSession(t, capacityRefusal)

	err := mgr.StartRuntimeOnly(context.Background(), runtimeOnlyActor(t, mgr, id), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})

	assertCapacityRefusalKeptConversation(t, err, sp, store, id, sessName)
}

// The same rule holds on the manager's own bring-up path (Start/Send).
func TestEnsureRunning_CapacityRefusalSkipsFreshRetryAndKeepsKey(t *testing.T) {
	mgr, sp, store, id, sessName := seedResumableACPSession(t, capacityRefusal)

	err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})

	assertCapacityRefusalKeptConversation(t, err, sp, store, id, sessName)
}

// Any other startup death keeps today's stale-key recovery: clear the key and
// relaunch fresh once.
func TestEnsureRunningRuntimeOnly_PlainStartupDeathStillRetriesFresh(t *testing.T) {
	mgr, sp, store, id, _ := seedResumableACPSession(t, func(sessName string) error {
		return fmt.Errorf("%w: session %q", runtime.ErrSessionDiedDuringStartup, sessName)
	})

	err := mgr.StartRuntimeOnly(context.Background(), runtimeOnlyActor(t, mgr, id), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})

	if err == nil {
		t.Fatal("StartRuntimeOnly succeeded, want the scripted startup death")
	}
	if got := startCallCount(sp); got != 2 {
		t.Fatalf("Start called %d times, want the failed resume plus one fresh relaunch", got)
	}
	b, getErr := store.Get(id)
	if getErr != nil {
		t.Fatalf("Get bead: %v", getErr)
	}
	if got := b.Metadata["session_key"]; got != "" {
		t.Errorf("session_key = %q, want cleared by the stale-key recovery", got)
	}
	if got := b.Metadata["continuation_reset_pending"]; got != "true" {
		t.Errorf("continuation_reset_pending = %q, want true", got)
	}
}

// committedConversationOwners are the rows PreserveConversationOnRuntimeLoss
// retains once their conversation is committed: a manual conversation and a
// configured always-named session.
var committedConversationOwners = []struct {
	name string
	meta map[string]string
}{
	{"manual", map[string]string{"session_origin": "manual"}},
	{"always-named", map[string]string{NamedSessionMetadataKey: "true", NamedSessionModeMetadata: "always"}},
}

// capacityTestRefusalLine is the provider refusing the resume of
// capacityTestSessionKey, as the dead pane shows it.
const capacityTestRefusalLine = "No conversation found with session ID: " + capacityTestSessionKey

// resumeRefusal is a startup death whose last pane output is the provider
// refusing the resume.
func resumeRefusal(sessName string) error {
	return fmt.Errorf("%w: session %q; last pane output:\n%s", runtime.ErrSessionDiedDuringStartup, sessName, capacityTestRefusalLine)
}

func setSessionMetadata(t *testing.T, store beads.Store, id string, meta map[string]string) {
	t.Helper()
	for k, v := range meta {
		if err := store.SetMetadata(id, k, v); err != nil {
			t.Fatalf("SetMetadata(%s): %v", k, err)
		}
	}
}

// assertConversationRetained fails unless err reports a retained committed
// conversation: ErrConversationRetained over the startup death, the provider's
// last pane output, and the reset that starts it fresh. It is not
// ErrResumeRequired, which reports a session with no resume command.
func assertConversationRetained(t *testing.T, err error, id string) {
	t.Helper()
	if !errors.Is(err, ErrConversationRetained) || !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("error = %v, want ErrConversationRetained over the startup death", err)
	}
	if errors.Is(err, ErrResumeRequired) {
		t.Fatalf("error = %v, want it not to read as ErrResumeRequired", err)
	}
	for _, want := range []string{capacityTestRefusalLine, "gc session reset " + id} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to carry %q", err, want)
		}
	}
}

// A startup death says nothing about the identity of a committed conversation:
// the failed resume keeps it, releases its route and reports the provider's
// own error with the reset that would start it fresh.
func TestCommittedConversationStartupFailurePreservesIdentity(t *testing.T) {
	for _, owner := range committedConversationOwners {
		for _, runtimeOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/runtimeOnly=%t", owner.name, runtimeOnly), func(t *testing.T) {
				mgr, sp, store, id, name := seedResumableACPSession(t, resumeRefusal)
				setSessionMetadata(t, store, id, owner.meta)
				var err error
				if runtimeOnly {
					err = mgr.StartRuntimeOnly(context.Background(), runtimeOnlyActor(t, mgr, id), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})
				} else {
					err = mgr.Start(context.Background(), testActor(mgr, ActorOperator), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})
				}
				assertConversationRetained(t, err, id)
				b, e := store.Get(id)
				if e != nil {
					t.Fatal(e)
				}
				if b.Metadata["session_key"] != capacityTestSessionKey || b.Metadata["started_config_hash"] != capacityTestConfigHash || b.Metadata["continuation_reset_pending"] != "" {
					t.Fatalf("failed resume reset committed identity: %v", b.Metadata)
				}
				if startCallCount(sp) != 1 {
					t.Fatalf("starts = %d, want exactly one failed resume", startCallCount(sp))
				}
				if !slices.Contains(sp.unrouted, name) {
					t.Fatal("failed resume retained route")
				}
			})
		}
	}
}

// A resume that starts and then exits is the same failed resume: the
// conversation is kept, and the error carries the dead session's last pane
// output, read before the route is released, with the launch's credentials
// redacted.
func TestCommittedConversationImmediateExitPreservesIdentity(t *testing.T) {
	const secret = "s3cr3t-worker-service-token"
	launch := runtime.Config{WorkDir: "/tmp", Env: map[string]string{"WORKER_SERVICE_TOKEN": secret}}
	for _, owner := range committedConversationOwners {
		for _, runtimeOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/runtimeOnly=%t", owner.name, runtimeOnly), func(t *testing.T) {
				_, sp, store, id, name := seedResumableACPSession(t, func(string) error { return nil })
				setSessionMetadata(t, store, id, owner.meta)
				// The launch echoed its own credential before it died.
				sp.SetPeekOutput(name, "export WORKER_SERVICE_TOKEN="+secret+"\n> claude --resume\n"+capacityTestRefusalLine)
				fake := &failOnceStartProvider{Fake: sp.Fake, armed: true}
				mgr := newTestManager(t, store, fake, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
				var err error
				if runtimeOnly {
					err = mgr.StartRuntimeOnly(context.Background(), runtimeOnlyActor(t, mgr, id), id, capacityTestResumeCmd, launch)
				} else {
					err = mgr.Start(context.Background(), testActor(mgr, ActorOperator), id, capacityTestResumeCmd, launch)
				}
				assertConversationRetained(t, err, id)
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error = %q, want the launch's credential redacted from its pane output", err)
				}
				b, e := store.Get(id)
				if e != nil {
					t.Fatal(e)
				}
				if b.Metadata["session_key"] != capacityTestSessionKey || b.Metadata["started_config_hash"] != capacityTestConfigHash || b.Metadata["continuation_reset_pending"] != "" {
					t.Fatalf("immediate exit reset committed identity: %v", b.Metadata)
				}
				if startCallCount(sp) != 1 {
					t.Fatalf("starts = %d, want exactly one failed resume", startCallCount(sp))
				}
			})
		}
	}
}

// peekFailingProvider is a failOnceStartProvider whose Peek fails.
type peekFailingProvider struct {
	*failOnceStartProvider
	peekErr error
}

func (p *peekFailingProvider) Peek(string, int) (string, error) { return "", p.peekErr }

// A dead session whose pane cannot be read still reports the startup death,
// and says why its last output is missing.
func TestCommittedConversationImmediateExitReportsUnreadablePane(t *testing.T) {
	_, sp, store, id, _ := seedResumableACPSession(t, func(string) error { return nil })
	setSessionMetadata(t, store, id, map[string]string{"session_origin": "manual"})
	fake := &peekFailingProvider{failOnceStartProvider: &failOnceStartProvider{Fake: sp.Fake, armed: true}, peekErr: errors.New("capture-pane: no pane")}
	mgr := newTestManager(t, store, fake, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))

	err := mgr.StartRuntimeOnly(context.Background(), runtimeOnlyActor(t, mgr, id), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})

	if !errors.Is(err, ErrConversationRetained) || !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("error = %v, want ErrConversationRetained over the startup death", err)
	}
	if !strings.Contains(err.Error(), "capture-pane: no pane") {
		t.Fatalf("error = %q, want the failed pane read reported", err)
	}
}

// An always-named session's first start has a preallocated key but no
// committed conversation, so a startup death still takes the stale-key
// recovery: clear the key and relaunch fresh once.
func TestAlwaysNamedFirstStartStillRetriesFresh(t *testing.T) {
	for _, runtimeOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtimeOnly=%t", runtimeOnly), func(t *testing.T) {
			mgr, sp, store, id, _ := seedResumableACPSession(t, resumeRefusal)
			setSessionMetadata(t, store, id, map[string]string{NamedSessionMetadataKey: "true", NamedSessionModeMetadata: "always", "started_config_hash": ""})
			var err error
			if runtimeOnly {
				err = mgr.StartRuntimeOnly(context.Background(), runtimeOnlyActor(t, mgr, id), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})
			} else {
				err = mgr.Start(context.Background(), testActor(mgr, ActorOperator), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})
			}
			if err == nil || errors.Is(err, ErrConversationRetained) {
				t.Fatalf("error = %v, want the fresh relaunch's own startup death", err)
			}
			if got := startCallCount(sp); got != 2 {
				t.Fatalf("starts = %d, want the failed start plus one fresh relaunch", got)
			}
			b, getErr := store.Get(id)
			if getErr != nil {
				t.Fatalf("Get bead: %v", getErr)
			}
			if got := b.Metadata["session_key"]; got != "" {
				t.Errorf("session_key = %q, want cleared by the stale-key recovery", got)
			}
			if got := b.Metadata["continuation_reset_pending"]; got != "true" {
				t.Errorf("continuation_reset_pending = %q, want true", got)
			}
		})
	}
}
