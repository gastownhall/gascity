package session

import (
	"context"
	"fmt"
	"slices"
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
	mgr := NewManagerWithOptions(store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))

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

	err := mgr.StartRuntimeOnly(context.Background(), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})

	assertCapacityRefusalKeptConversation(t, err, sp, store, id, sessName)
}

// The same rule holds on the manager's own bring-up path (Start/Send).
func TestEnsureRunning_CapacityRefusalSkipsFreshRetryAndKeepsKey(t *testing.T) {
	mgr, sp, store, id, sessName := seedResumableACPSession(t, capacityRefusal)

	err := mgr.Start(context.Background(), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"}, ResumeOperator)

	assertCapacityRefusalKeptConversation(t, err, sp, store, id, sessName)
}

// Any other startup death keeps today's stale-key recovery: clear the key and
// relaunch fresh once.
func TestEnsureRunningRuntimeOnly_PlainStartupDeathStillRetriesFresh(t *testing.T) {
	mgr, sp, store, id, _ := seedResumableACPSession(t, func(sessName string) error {
		return fmt.Errorf("%w: session %q", runtime.ErrSessionDiedDuringStartup, sessName)
	})

	err := mgr.StartRuntimeOnly(context.Background(), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})

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

// A startup death says nothing about the identity of a committed conversation.
func TestCommittedConversationStartupFailurePreservesIdentity(t *testing.T) {
	for _, runtimeOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtimeOnly=%t", runtimeOnly), func(t *testing.T) {
			mgr, sp, store, id, name := seedResumableACPSession(t, func(string) error { return runtime.ErrSessionDiedDuringStartup })
			if err := store.SetMetadata(id, "session_origin", "manual"); err != nil {
				t.Fatal(err)
			}
			var err error
			if runtimeOnly {
				err = mgr.StartRuntimeOnly(context.Background(), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})
			} else {
				err = mgr.Start(context.Background(), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"}, ResumeOperator)
			}
			if err == nil {
				t.Fatal("failed resume reported success")
			}
			b, e := store.Get(id)
			if e != nil {
				t.Fatal(e)
			}
			if b.Metadata["session_key"] != capacityTestSessionKey || b.Metadata["started_config_hash"] != capacityTestConfigHash || b.Metadata["continuation_reset_pending"] != "" {
				t.Fatalf("failed resume reset committed identity: %v", b.Metadata)
			}
			if startCallCount(sp) != 1 {
				t.Fatalf("fresh fallback launches = %d", startCallCount(sp)-1)
			}
			if !slices.Contains(sp.unrouted, name) {
				t.Fatal("failed resume retained route")
			}
		})
	}
}

func TestCommittedConversationImmediateExitPreservesIdentity(t *testing.T) {
	for _, runtimeOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtimeOnly=%t", runtimeOnly), func(t *testing.T) {
			_, sp, store, id, _ := seedResumableACPSession(t, func(string) error { return nil })
			if err := store.SetMetadata(id, "session_origin", "manual"); err != nil {
				t.Fatal(err)
			}
			fake := &failOnceStartProvider{Fake: sp.Fake, armed: true}
			mgr := NewManagerWithOptions(store, fake, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
			var err error
			if runtimeOnly {
				err = mgr.StartRuntimeOnly(context.Background(), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})
			} else {
				err = mgr.Start(context.Background(), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"}, ResumeOperator)
			}
			if err == nil {
				t.Fatal("immediate exit reported success")
			}
			b, e := store.Get(id)
			if e != nil {
				t.Fatal(e)
			}
			if b.Metadata["session_key"] != capacityTestSessionKey || b.Metadata["continuation_reset_pending"] != "" {
				t.Fatal("immediate exit reset committed identity")
			}
			if startCallCount(sp) != 1 {
				t.Fatalf("starts=%d, want one failed resume", startCallCount(sp))
			}
		})
	}
}
