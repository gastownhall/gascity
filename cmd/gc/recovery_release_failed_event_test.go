package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// recoveryEventRecorder is a goroutine-safe events.Recorder: the on_boot hooks
// publish from concurrent workers.
type recoveryEventRecorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recoveryEventRecorder) Record(e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recoveryEventRecorder) recoveryFailures(t *testing.T) []api.BeadRecoveryReleaseFailedPayload {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []api.BeadRecoveryReleaseFailedPayload
	for _, e := range r.events {
		if e.Type != events.BeadRecoveryReleaseFailed {
			t.Errorf("unexpected event type %q recorded", e.Type)
			continue
		}
		var p api.BeadRecoveryReleaseFailedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal payload %s: %v", e.Payload, err)
		}
		if e.Subject != p.BeadID {
			t.Errorf("event subject = %q, want the bead id %q", e.Subject, p.BeadID)
		}
		if e.Actor != "gc" {
			t.Errorf("event actor = %q, want gc", e.Actor)
		}
		out = append(out, p)
	}
	return out
}

func wantRecoveryFailure(t *testing.T, got []api.BeadRecoveryReleaseFailedPayload, want api.BeadRecoveryReleaseFailedPayload) {
	t.Helper()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("recovery failure events = %+v, want exactly %+v", got, want)
	}
}

const (
	onDeathFailureLine = config.RecoveryHookMarker + " on_death release failed for ga-7: dolt: connection refused\n"
	onBootFailureLine  = config.RecoveryHookMarker + " on_boot reopen failed for ga-8: update refused\n"
)

// The on_death call site publishes one typed event per failed release, with
// the bead id, the agent's qualified name (not the session name), the hook and
// bd's error text, and keeps the stderr line it always printed.
func TestRunPoolDeathHookPublishesRecoveryReleaseFailed(t *testing.T) {
	rec := &recoveryEventRecorder{}
	var stderr bytes.Buffer
	run := func(string, string, map[string]string) (string, error) { return onDeathFailureLine, nil }
	info := poolDeathInfo{Command: "release", Dir: "/city", Agent: "hello-world/dog-1"}

	if err := runPoolDeathHook(run, &stderr, rec, "hello-world--dog-1", info); err != nil {
		t.Fatalf("runPoolDeathHook: %v", err)
	}

	wantRecoveryFailure(t, rec.recoveryFailures(t), api.BeadRecoveryReleaseFailedPayload{
		BeadID: "ga-7",
		Agent:  "hello-world/dog-1",
		Hook:   "on_death",
		Error:  "dolt: connection refused",
	})
	if got, want := stderr.String(), "on_death hello-world--dog-1: "+onDeathFailureLine; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// Several failures in one hook run publish one event each.
func TestRunPoolDeathHookPublishesOneEventPerFailedBead(t *testing.T) {
	rec := &recoveryEventRecorder{}
	out := config.RecoveryHookMarker + " on_death release failed for ga-1: a\n" +
		config.RecoveryHookMarker + " on_death release failed for ga-2: b\n"
	run := func(string, string, map[string]string) (string, error) { return out, nil }

	_ = runPoolDeathHook(run, io.Discard, rec, "dog-1", poolDeathInfo{Agent: "dog-1"})

	got := rec.recoveryFailures(t)
	if len(got) != 2 || got[0].BeadID != "ga-1" || got[1].BeadID != "ga-2" {
		t.Fatalf("events = %+v, want one per failed bead in order", got)
	}
}

// A clean run, a user override's arbitrary output, and an unparseable marker
// line publish nothing; a nil recorder (no event sink wired) is tolerated.
func TestRunPoolDeathHookPublishesNothingWithoutAParsedFailure(t *testing.T) {
	for _, out := range []string{
		"",
		"custom override output\n",
		config.RecoveryHookMarker + " could not release\n",
	} {
		rec := &recoveryEventRecorder{}
		run := func(string, string, map[string]string) (string, error) { return out, nil }
		_ = runPoolDeathHook(run, io.Discard, rec, "dog-1", poolDeathInfo{Agent: "dog-1"})
		if got := rec.recoveryFailures(t); len(got) != 0 {
			t.Errorf("output %q published %+v, want none", out, got)
		}
	}
	run := func(string, string, map[string]string) (string, error) { return onDeathFailureLine, nil }
	_ = runPoolDeathHook(run, io.Discard, nil, "dog-1", poolDeathInfo{Agent: "dog-1"})
}

// The event is published through the real lane: a pool session that stops
// makes the on_death worker run the hook, and the failure it reports reaches
// the city runtime's recorder.
func TestInventoryOnDeath_PublishesRecoveryReleaseFailedThroughTheLane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1")
		handlers := map[string]poolDeathInfo{
			"worker-1": {Command: "release worker-1", Dir: "/city", Agent: "rig/worker-1"},
		}
		cr, hooks, _ := onDeathLaneRuntime(t, sp, handlers)
		rec := &recoveryEventRecorder{}
		cr.rec = rec
		hooks.out = onDeathFailureLine
		hooks.err = errors.New("exit status 1")
		startOnDeathWorkerInBubble(t, cr)
		passAndDrain(cr)
		sp.setListing(nil)
		passAndDrain(cr)

		wantRecoveryFailure(t, rec.recoveryFailures(t), api.BeadRecoveryReleaseFailedPayload{
			BeadID: "ga-7",
			Agent:  "rig/worker-1",
			Hook:   "on_death",
			Error:  "dolt: connection refused",
		})
	})
}

// computePoolDeathHandlers records each instance's agent identity, which is
// what the event names (the map key is the session name).
func TestComputePoolDeathHandlersRecordsTheAgentIdentity(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "my-city"},
		Agents: []config.Agent{
			{Name: "dog", Dir: "rig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
		},
	}
	handlers := computePoolDeathHandlers(cfg, "my-city", t.TempDir(), runtime.NewFake(), io.Discard)
	if len(handlers) == 0 {
		t.Fatal("computePoolDeathHandlers returned no handlers for a pool agent")
	}
	for sn, info := range handlers {
		if info.Agent == "" {
			t.Errorf("handler %q has no Agent identity", sn)
		}
	}
}

// The on_boot call site publishes the same event with hook=on_boot.
func TestRunPoolOnBootHooksPublishesRecoveryReleaseFailed(t *testing.T) {
	rec := &recoveryEventRecorder{}
	var stderr bytes.Buffer
	hooks := []poolOnBootHook{{agent: "hello-world/dog", command: "boot", dir: "/rig"}}
	runner := func(string, string, map[string]string) (string, error) { return onBootFailureLine, nil }

	runPoolOnBootHooks(hooks, runner, &stderr, rec)

	wantRecoveryFailure(t, rec.recoveryFailures(t), api.BeadRecoveryReleaseFailedPayload{
		BeadID: "ga-8",
		Agent:  "hello-world/dog",
		Hook:   "on_boot",
		Error:  "update refused",
	})
	if got, want := stderr.String(), "on_boot hello-world/dog: "+onBootFailureLine; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// Concurrent on_boot hooks each publish under their own agent identity.
func TestRunPoolOnBootHooksPublishesPerAgentUnderConcurrency(t *testing.T) {
	rec := &recoveryEventRecorder{}
	hooks := onBootHooks("dog", "cat", "fox")
	runner := func(_, dir string, _ map[string]string) (string, error) {
		return config.RecoveryHookMarker + " on_boot reopen failed for bead-" + dir + ": e\n", nil
	}

	runPoolOnBootHooks(hooks, runner, io.Discard, rec)

	got := rec.recoveryFailures(t)
	if len(got) != 3 {
		t.Fatalf("events = %+v, want one per agent", got)
	}
	for _, p := range got {
		if p.BeadID != "bead-/rig/"+p.Agent || p.Hook != "on_boot" {
			t.Errorf("payload %+v: agent identity does not match the hook that reported it", p)
		}
	}
}

// Through the real runPoolOnBoot: planning, the default-hook path and the
// publish all line up, and an agent whose hook reports nothing publishes nothing.
func TestRunPoolOnBootPublishesRecoveryReleaseFailed(t *testing.T) {
	rec := &recoveryEventRecorder{}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", Dir: "rig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			{Name: "cat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		},
	}
	var mu sync.Mutex
	runner := func(cmd, _ string, _ map[string]string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if bytes.Contains([]byte(cmd), []byte("rig/dog")) {
			return onBootFailureLine, nil
		}
		return "", nil
	}

	runPoolOnBoot(cfg, t.TempDir(), runner, io.Discard, rec)

	wantRecoveryFailure(t, rec.recoveryFailures(t), api.BeadRecoveryReleaseFailedPayload{
		BeadID: "ga-8",
		Agent:  "rig/dog",
		Hook:   "on_boot",
		Error:  "update refused",
	})
}
