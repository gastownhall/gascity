package main

import (
	"io"
	"os"
	"testing"
)

// TestDefaultSupervisorBeadsActor verifies that the supervisor's process
// env defaults BEADS_ACTOR=controller when the operator has not set it.
//
// Without this default, bd hooks (which spawn `gc event emit`
// subprocesses inheriting the supervisor's ambient process env) fall
// through eventActor()'s GC_ALIAS → GC_AGENT → GC_SESSION_ID → BEADS_ACTOR
// chain and land on the literal "human" fallback, mis-attributing every
// dispatcher-issued tracking-bead create/update/close to "human".
//
// applyControllerBdEnv (cmd/gc/bd_env.go) sets BEADS_ACTOR=controller in
// the env *map* used when spawning bd commands, but bd hooks are
// spawned from the supervisor's own process — they inherit the process
// env, not that map — so the helper here ensures the hook path resolves
// to "controller" as well.
//
// Order-exec subprocesses still override BEADS_ACTOR to "order:<name>"
// via orderExecEnv (cmd/gc/order_store.go), which beats the inherited
// process env, so per-order attribution is preserved.
func TestDefaultSupervisorBeadsActor(t *testing.T) {
	t.Run("unset env defaults to controller", func(t *testing.T) {
		t.Setenv("BEADS_ACTOR", "")
		_ = os.Unsetenv("BEADS_ACTOR")
		defaultSupervisorBeadsActor()
		if got := os.Getenv("BEADS_ACTOR"); got != "controller" {
			t.Fatalf("BEADS_ACTOR = %q, want %q", got, "controller")
		}
	})

	t.Run("whitespace-only treated as unset", func(t *testing.T) {
		t.Setenv("BEADS_ACTOR", "   ")
		defaultSupervisorBeadsActor()
		if got := os.Getenv("BEADS_ACTOR"); got != "controller" {
			t.Fatalf("BEADS_ACTOR = %q, want %q", got, "controller")
		}
	})

	t.Run("operator-set value is respected", func(t *testing.T) {
		t.Setenv("BEADS_ACTOR", "custom-operator")
		defaultSupervisorBeadsActor()
		if got := os.Getenv("BEADS_ACTOR"); got != "custom-operator" {
			t.Fatalf("BEADS_ACTOR = %q, want %q", got, "custom-operator")
		}
	})
}

// TestEventActorAfterSupervisorDefault verifies the end-to-end effect:
// once defaultSupervisorBeadsActor has run with an empty ambient env,
// eventActor() — the function bd-hook-forwarded `gc event emit`
// subprocesses use to determine the event Actor — returns "controller"
// instead of falling through to "human".
func TestEventActorAfterSupervisorDefault(t *testing.T) {
	// Clear every env var eventActor() consults so the discriminator
	// isolates the BEADS_ACTOR path that the supervisor default touches.
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "GC_SESSION_ID", "BEADS_ACTOR"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	if got := eventActor(); got != "human" {
		t.Fatalf("eventActor() = %q before supervisor default, want %q (parent-RED baseline)", got, "human")
	}

	defaultSupervisorBeadsActor()

	if got := eventActor(); got != "controller" {
		t.Fatalf("eventActor() = %q after supervisor default, want %q", got, "controller")
	}
}

// TestDoSupervisorRunInvokesDefaultBeadsActor verifies that
// doSupervisorRun runs defaultSupervisorBeadsActor before delegating to
// the run-loop, covering the call site that unit tests cannot exercise
// directly (runSupervisor blocks until shutdown). The runSupervisorFunc
// indirection lets the test substitute a no-op loop, observe the
// pre-loop env state, and confirm the helper executed.
func TestDoSupervisorRunInvokesDefaultBeadsActor(t *testing.T) {
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "GC_SESSION_ID", "BEADS_ACTOR"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}

	origRun := runSupervisorFunc
	called := false
	runSupervisorFunc = func(io.Writer, io.Writer) int {
		called = true
		return 0
	}
	t.Cleanup(func() { runSupervisorFunc = origRun })

	if rc := doSupervisorRun(io.Discard, io.Discard); rc != 0 {
		t.Fatalf("doSupervisorRun = %d, want 0", rc)
	}
	if !called {
		t.Fatal("runSupervisorFunc was not invoked")
	}
	if got := os.Getenv("BEADS_ACTOR"); got != "controller" {
		t.Fatalf("BEADS_ACTOR after doSupervisorRun = %q, want %q", got, "controller")
	}
}
