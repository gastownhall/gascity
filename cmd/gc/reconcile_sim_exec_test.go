package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The simulator on an exec v2 backend (PAR-EXEC §13.3): the same runtime and
// faults, read through exactly the interfaces an observe.v1 exec pack's
// provider implements (runtime.ProfileExecV2) and answered as such a script
// answers them. Every invariant must hold there too.

// GC_V2_SIM_LEAF=exec runs every sim (scenarios and corpora alike) on the
// exec v2 backend: gc_sim_exec_test does, so each scenario written for tmux
// is also a journey under exec.
func init() {
	if os.Getenv("GC_V2_SIM_LEAF") == "exec" {
		simLeafOverride = newSimExecLeaf
	}
}

// skipOnSimExecLeaf skips a scenario whose subject is a capability an exec
// v2 pack does not have (why), when the sims run on the exec backend.
func skipOnSimExecLeaf(t *testing.T, why string) {
	t.Helper()
	if simLeafOverride != nil {
		t.Skipf("exec v2 has no %s", why)
	}
}

// simExecBackend is the sim's runtime as an observe.v1 script reports it: its
// liveness (always fresh: exec's observe runs the script each time), kills,
// listing and attachment are the sim's; its identity is the box's (exec's
// observe carries it), and its inventory carries each box's object, agent,
// attachment, identity and restart marker, the incarnation
// "<object>:<created>" as exec maps it. While down, every op fails, as a
// script fails every op in a backend outage; calls counts the Starts and
// the destructive calls gc made.
type simExecBackend struct {
	runtime.FullFake
	sp    *simProvider
	down  atomic.Bool
	calls atomic.Int64
}

// simExecLeaf builds the exec v2 leaf over sp and keeps its backend in *be.
func simExecLeaf(be **simExecBackend) func(*simProvider) runtime.Provider {
	return func(sp *simProvider) runtime.Provider {
		*be = &simExecBackend{FullFake: runtime.FullFake{Fake: sp.Fake}, sp: sp}
		return runtime.NewFakeProfile(runtime.ProfileExecV2, *be)
	}
}

func newSimExecLeaf(sp *simProvider) runtime.Provider {
	var be *simExecBackend
	return simExecLeaf(&be)(sp)
}

var errSimBackendDown = fmt.Errorf("sim: exec backend down: %w", runtime.ErrRuntimeUnavailable)

func (b *simExecBackend) ListRunning(prefix string) ([]string, error) {
	if b.down.Load() {
		return nil, errSimBackendDown
	}
	return b.sp.ListRunning(prefix)
}

func (b *simExecBackend) ListRunningComplete() bool { return b.sp.ListRunningComplete() }

func (b *simExecBackend) Start(ctx context.Context, name string, cfg runtime.Config) error {
	b.calls.Add(1)
	if b.down.Load() {
		return errSimBackendDown
	}
	return b.sp.Start(ctx, name, cfg)
}

func (b *simExecBackend) Stop(name string) error {
	b.calls.Add(1)
	if b.down.Load() {
		return errSimBackendDown
	}
	return b.sp.Stop(name)
}

func (b *simExecBackend) ObserveLivenessWithError(name string, pn []string) (runtime.Liveness, error) {
	if b.down.Load() {
		return runtime.Liveness{}, errSimBackendDown
	}
	return b.sp.ObserveLivenessSince(name, pn, time.Time{})
}

func (b *simExecBackend) ObserveLivenessSince(name string, pn []string, _ time.Time) (runtime.Liveness, error) {
	return b.ObserveLivenessWithError(name, pn)
}

func (b *simExecBackend) IsAttached(name string) bool { return b.sp.IsAttached(name) }

func (b *simExecBackend) IsAttachedWithError(name string) (bool, error) {
	if b.down.Load() {
		return false, errSimBackendDown
	}
	return b.sp.IsAttachedWithError(name)
}

func (b *simExecBackend) KillCorpseObject(name, objectID, created string) (runtime.SessionObjectKillResult, error) {
	b.calls.Add(1)
	if b.down.Load() {
		return runtime.SessionObjectNotKilled, errSimBackendDown
	}
	return b.sp.KillCorpseObject(name, objectID, created)
}

func (b *simExecBackend) KillZombieObject(name, objectID, created, panePID string) (runtime.SessionObjectKillResult, error) {
	b.calls.Add(1)
	if b.down.Load() {
		return runtime.SessionObjectNotKilled, errSimBackendDown
	}
	return b.sp.KillZombieObject(name, objectID, created, panePID)
}

func (b *simExecBackend) SetMeta(name, key, value string) error {
	if b.down.Load() {
		return errSimBackendDown
	}
	return b.sp.SetMeta(name, key, value)
}

func (b *simExecBackend) RemoveMeta(name, key string) error {
	if b.down.Load() {
		return errSimBackendDown
	}
	return b.sp.RemoveMeta(name, key)
}

func (b *simExecBackend) GetAllEnvironment(name string) (map[string]string, error) {
	return b.sp.GetAllEnvironment(name)
}

// GetMeta answers the identity keys from the box, as the script's
// metadata seeded at start carries them, and every other key from its
// metadata.
func (b *simExecBackend) GetMeta(name, key string) (string, error) {
	if b.down.Load() {
		return "", errSimBackendDown
	}
	switch key {
	case "GC_SESSION_ID", "GC_RUNTIME_EPOCH", "GC_INSTANCE_TOKEN", "GT_PROCESS_NAMES":
		env, err := b.sp.GetAllEnvironment(name)
		if err != nil {
			return "", nil // no box: nothing set
		}
		return env[key], nil
	}
	return b.sp.GetMeta(name, key)
}

// RuntimeInventory is the script's `inventory`: it fails when the listing
// does or the backend is down, and otherwise lists every box. A box whose
// process probe errors has its agent unknown.
func (b *simExecBackend) RuntimeInventory(context.Context) (map[string]runtime.InventoryEntry, error) {
	if b.down.Load() {
		return nil, errSimBackendDown
	}
	p := b.sp
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listing == 2 {
		return nil, fmt.Errorf("sim: inventory: %w", runtime.ErrRuntimeUnavailable)
	}
	out := make(map[string]runtime.InventoryEntry, len(p.rts))
	for name, rt := range p.rts {
		e := runtime.InventoryEntry{
			Incarnation: fmt.Sprintf("$%d:%d", rt.object, rt.created), DeadKnown: true, AllPanesDead: rt.corpse,
			AttachedKnown: true, Attached: rt.attached, RestartRequested: rt.restart,
			IdentityKnown: true, Identity: map[string]string{"GC_SESSION_ID": rt.id, "GC_RUNTIME_EPOCH": rt.epoch, "GC_INSTANCE_TOKEN": rt.token, "GT_PROCESS_NAMES": "agent"},
		}
		if !rt.corpse && !rt.probeErr {
			e.ProcessKnown, e.ProcessAlive = true, !rt.zombie
		}
		if at := p.Activity[name]; !at.IsZero() {
			e.ActivityKnown, e.LastActivity = true, at
		}
		out[name] = e
	}
	return out, nil
}

// TestSimCorpusExecV2 is the corpus on an exec v2 backend: no invariant may
// break on the provider surface a v2-ready exec pack presents.
func TestSimCorpusExecV2(t *testing.T) {
	for _, seed := range simSeeds(t) {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			s := newSim(t, seed, simOpts{leaf: newSimExecLeaf})
			if _, tmuxShaped := s.leaf.(runtime.ServerDeathConfirmer); tmuxShaped || !runtime.FreshReadable(s.leaf) {
				t.Fatalf("leaf %T is not an exec v2 pack's provider surface", s.leaf)
			}
			s.run()
			s.report(t)
		})
	}
}

// Journey "backend outage" (A12): while the exec backend fails every op
// (the inventory exits 1 pass after pass), v2 admits no start or
// destructive effect and makes no such call, though the boxes died
// meanwhile; once it answers again the dead seat starts.
func TestSimExecOutageMakesNoEffect(t *testing.T) {
	var be *simExecBackend
	s := scriptedWith(t, simOpts{leaf: simExecLeaf(&be)}, []string{"gc-1", "gc-2"},
		poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1"), gc1Work(),
		poolRow("gc-2", "worker", 2, "active", "instance_token", "tok-2", "session_key", "k-2"))
	s.settle()
	before := be.calls.Load()
	be.down.Store(true)
	s.sp.mu.Lock()
	s.sp.drop("s-gc-1")
	s.sp.rts["s-gc-2"].zombie = true
	s.sp.touch("s-gc-2")
	s.sp.mu.Unlock()
	from := len(s.round)
	for range 5 {
		s.advance(simPatrol)
		s.settle()
	}
	if n := be.calls.Load() - before; n != 0 {
		t.Fatalf("%d Starts or destructive calls during the outage, want none", n)
	}
	for _, r := range s.round[from:] {
		switch r.it.Kind {
		case intentStart, intentZombie, intentStop, intentClose, intentRollback, intentDriftRestart:
			t.Fatalf("v2 admitted %s on %s during the outage, want no start or destructive effect", r.it.Kind, r.it.Key.ID)
		}
	}
	be.down.Store(false)
	for range 3 {
		s.advance(simPatrol)
		s.settle()
	}
	if m := s.gc1(); m["generation"] == "1" || be.calls.Load() == before {
		t.Fatalf("gc-1 %v after the backend recovered, %d calls; want the dead seat started", m, be.calls.Load()-before)
	}
	s.noViolations(t)
}

// TestSimExecLeafIsSet pins gc_sim_exec_test's premise: under
// GC_V2_SIM_LEAF=exec every sim runs on the exec v2 backend.
func TestSimExecLeafIsSet(t *testing.T) {
	if os.Getenv("GC_V2_SIM_LEAF") != "exec" {
		t.Skip("GC_V2_SIM_LEAF is not exec")
	}
	s := newSim(t, 1, simOpts{})
	if _, tmuxShaped := s.leaf.(runtime.ServerDeathConfirmer); tmuxShaped || s.leaf == runtime.Provider(s.sp) {
		t.Fatalf("leaf %T under GC_V2_SIM_LEAF=exec, want the exec v2 backend", s.leaf)
	}
}
