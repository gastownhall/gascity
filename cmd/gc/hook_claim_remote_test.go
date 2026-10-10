package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The G8 hook-claim lanes for a remote work store (hook_claim_remote.go).

// fakeRemoteNativeStore stands in for a NativeDoltStore opened over a remote
// backend. claim decides each Claim; the rest record what was asked.
type fakeRemoteNativeStore struct {
	claim    func(id, assignee string) (beads.Bead, bool, error)
	beads    map[string]beads.Bead
	calls    []string
	released []string
}

func (f *fakeRemoteNativeStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	f.calls = append(f.calls, "claim "+id)
	if f.claim != nil {
		return f.claim(id, assignee)
	}
	b := beads.Bead{ID: id, Status: "in_progress", Assignee: assignee}
	f.beads[id] = b
	return b, true, nil
}

func (f *fakeRemoteNativeStore) Get(id string) (beads.Bead, error) {
	f.calls = append(f.calls, "get "+id)
	b, ok := f.beads[id]
	if !ok {
		return beads.Bead{}, fmt.Errorf("get %q: %w", id, beads.ErrNotFound)
	}
	return b, nil
}

func (f *fakeRemoteNativeStore) List(beads.ListQuery) ([]beads.Bead, error) {
	f.calls = append(f.calls, "list")
	return nil, nil
}

func (f *fakeRemoteNativeStore) Update(id string, _ beads.UpdateOpts) error {
	f.calls = append(f.calls, "update "+id)
	return nil
}

func (f *fakeRemoteNativeStore) ReleaseIfCurrent(id, _ string) (bool, error) {
	f.released = append(f.released, id)
	return true, nil
}

func (f *fakeRemoteNativeStore) TransferIfCurrent(id, _, _ string) (bool, error) {
	f.calls = append(f.calls, "transfer "+id)
	return true, nil
}

const remoteLegScope = "/city/rigs/remote"

// remoteLegEnv is a leg env whose BEADS_DIR names the remote scope.
func remoteLegEnv() []string {
	return []string{"BEADS_DIR=" + filepath.Join(remoteLegScope, ".beads"), "PATH=/usr/bin"}
}

// testRemoteRouter is a router whose only remote scope is remoteLegScope,
// served by lane (or failing to open with openErr). Credential projection
// appends a marker so a test can see which env reached the bd CLI.
func testRemoteRouter(t *testing.T, lane hookClaimRemoteLane, openErr error, stderr *bytes.Buffer) (*hookClaimRemoteRouter, *int) {
	t.Helper()
	opens := 0
	r := &hookClaimRemoteRouter{cityPath: "/city", stderr: stderr, lanes: map[string]hookClaimRemoteLane{}}
	r.isRemote = func(scope string) bool { return scope == remoteLegScope }
	r.openLane = func(scope string) (hookClaimRemoteLane, func(), error) {
		opens++
		if scope != remoteLegScope {
			t.Errorf("opened %q, want only the remote scope", scope)
		}
		return lane, nil, openErr
	}
	r.credentialEnv = func(scope string, env []string) ([]string, error) {
		return append(slices.Clone(env), "BEADS_HTTP_TOKEN=scope-cred:"+scope), nil
	}
	return r, &opens
}

// recordingBaseClaim is the bd CLI lane: it records the env it ran with.
func recordingBaseClaim(envs *[][]string, result error) hookClaimFunc {
	return func(_ context.Context, _ string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		*envs = append(*envs, slices.Clone(env))
		if result != nil {
			return beads.Bead{}, false, result
		}
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
	}
}

func TestRemoteRoutedHookClaimLeavesALocalLegOnTheBdCLI(t *testing.T) {
	native := &fakeRemoteNativeStore{beads: map[string]beads.Bead{}}
	router, opens := testRemoteRouter(t, hookClaimRemoteLane{native: native}, nil, &bytes.Buffer{})
	var envs [][]string
	ops := remoteRoutedHookClaimOps(hookClaimOps{Claim: recordingBaseClaim(&envs, nil)}, router)

	local := []string{"BEADS_DIR=/city/.beads"}
	if _, ok, err := ops.Claim(context.Background(), "/city", local, "gc-1", "worker"); err != nil || !ok {
		t.Fatalf("local claim = ok %v err %v", ok, err)
	}
	if len(envs) != 1 || !slices.Equal(envs[0], local) {
		t.Fatalf("local leg ran the bd CLI with %v, want its env unchanged %v", envs, local)
	}
	if *opens != 0 || len(native.calls) != 0 {
		t.Fatalf("a local leg opened the remote store (%d opens, calls %v)", *opens, native.calls)
	}
}

func TestRemoteRoutedHookClaimUsesTheNativeClaimerUnderAuto(t *testing.T) {
	native := &fakeRemoteNativeStore{beads: map[string]beads.Bead{}}
	var stderr bytes.Buffer
	router, opens := testRemoteRouter(t, hookClaimRemoteLane{native: native}, nil, &stderr)
	var envs [][]string
	ops := remoteRoutedHookClaimOps(hookClaimOps{Claim: recordingBaseClaim(&envs, nil)}, router)

	for i := 0; i < 2; i++ {
		claimed, ok, err := ops.Claim(context.Background(), remoteLegScope, remoteLegEnv(), "mc-1", "worker")
		if err != nil || !ok || claimed.Assignee != "worker" {
			t.Fatalf("native claim = %+v ok %v err %v", claimed, ok, err)
		}
	}
	if len(envs) != 0 {
		t.Fatalf("a native-lane claim shelled out to bd: %v", envs)
	}
	if *opens != 1 {
		t.Fatalf("remote store opened %d times, want once per invocation", *opens)
	}
	if !slices.Contains(native.calls, "claim mc-1") {
		t.Fatalf("native calls = %v, want the Claimer", native.calls)
	}
	if _, err := ops.RestampAdopted(context.Background(), remoteLegScope, remoteLegEnv(), "mc-1", "a", "b"); err != nil || !slices.Contains(native.calls, "transfer mc-1") {
		t.Fatalf("restamp = %v, calls %v; want the native TransferIfCurrent", err, native.calls)
	}
	if _, err := ops.Release(context.Background(), remoteLegScope, remoteLegEnv(), "mc-1", "worker"); err != nil || !slices.Equal(native.released, []string{"mc-1"}) {
		t.Fatalf("release = %v, released %v", err, native.released)
	}
	for i := 0; i < 2; i++ {
		if reclaimed, _, err := ops.ReclaimStale(context.Background(), remoteLegScope, remoteLegEnv(), "mc-1"); err != nil || reclaimed {
			t.Fatalf("reclaim on the native lane = %v %v, want a skip", reclaimed, err)
		}
	}
	if got := strings.Count(stderr.String(), "skipping stale-lease reclaim"); got != 1 {
		t.Fatalf("reclaim skip warned %d times, want once: %q", got, stderr.String())
	}
}

func TestRemoteRoutedHookClaimUsesTheBdCLIWithTheScopeCredentialUnderOff(t *testing.T) {
	router, _ := testRemoteRouter(t, hookClaimRemoteLane{}, nil, &bytes.Buffer{})
	var envs [][]string
	ops := remoteRoutedHookClaimOps(hookClaimOps{Claim: recordingBaseClaim(&envs, nil)}, router)

	if _, ok, err := ops.Claim(context.Background(), remoteLegScope, remoteLegEnv(), "mc-1", "worker"); err != nil || !ok {
		t.Fatalf("CLI-lane claim = ok %v err %v", ok, err)
	}
	if len(envs) != 1 || !slices.Contains(envs[0], "BEADS_HTTP_TOKEN=scope-cred:"+remoteLegScope) {
		t.Fatalf("bd CLI ran with %v, want the scope's credential projected", envs)
	}
}

func TestRemoteRoutedHookClaimNeverFallsBackWhenTheNativeStoreIsRequired(t *testing.T) {
	refusal := &beads.HTTPNativeOpenRequiredError{ScopeRoot: remoteLegScope, Backend: "http", Gate: "wire_compat", Reason: "server does not advertise required capabilities: issues.claim"}
	router, _ := testRemoteRouter(t, hookClaimRemoteLane{}, refusal, &bytes.Buffer{})
	var envs [][]string
	ops := remoteRoutedHookClaimOps(hookClaimOps{Claim: recordingBaseClaim(&envs, nil)}, router)

	_, ok, err := ops.Claim(context.Background(), remoteLegScope, remoteLegEnv(), "mc-1", "worker")
	if ok || !beads.IsHTTPNativeOpenRequired(err) {
		t.Fatalf("claim = ok %v err %v, want the terminal native-required refusal", ok, err)
	}
	if len(envs) != 0 {
		t.Fatalf("a refused native open fell back to the bd CLI: %v", envs)
	}
	if hookClaimBeadIsElsewhere(err) || hookClaimBeadIsAWisp(err) {
		t.Fatalf("the open refusal must fail the hook closed, not be skipped: %v", err)
	}
}

// TestRemoteWorkStoreWispClaimFallsBackToTheGraphStore is MC's shape: wisps
// live in the sqlite graph binding, the remote work store does not hold them,
// its not-found opens the class route, and the claim lands in the binding.
func TestRemoteWorkStoreWispClaimFallsBackToTheGraphStore(t *testing.T) {
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, "gcg-300", nil)
	native := &fakeRemoteNativeStore{beads: map[string]beads.Bead{}, claim: func(id, _ string) (beads.Bead, bool, error) {
		return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", id, beads.ErrNotFound)
	}}
	router, _ := testRemoteRouter(t, hookClaimRemoteLane{native: native}, nil, &bytes.Buffer{})
	ops := classRoutedHookClaimOps(remoteRoutedHookClaimOps(hookClaimOps{}, router), newClaimRouteFor(t, class))

	claimed, ok, err := ops.Claim(context.Background(), remoteLegScope, remoteLegEnv(), "gcg-300", "worker-1")
	if err != nil || !ok || claimed.Assignee != "worker-1" {
		t.Fatalf("routed wisp claim = %+v ok %v err %v, want it claimed in the graph store", claimed, ok, err)
	}
	if !slices.Contains(native.calls, "claim gcg-300") {
		t.Fatalf("the remote work store was not asked first: %v", native.calls)
	}
	held, err := class.Get("gcg-300")
	if err != nil || held.Assignee != "worker-1" {
		t.Fatalf("graph store holds %+v (%v), want worker-1", held, err)
	}
}

// TestRemoteWorkStoreWispClaimIsANamedRefusal: a wisp the remote work store
// DOES hold is refused by name on both lanes, never read as "elsewhere", so
// the class route does not go looking for it and the hook skips it loudly.
func TestRemoteWorkStoreWispClaimIsANamedRefusal(t *testing.T) {
	assertNamed := func(t *testing.T, err error) {
		t.Helper()
		var named *beads.WispClaimRefusedError
		if !errors.As(err, &named) || named.ID != "mc-wisp-1" {
			t.Fatalf("err = %v, want *WispClaimRefusedError naming mc-wisp-1", err)
		}
		if !hookClaimBeadIsAWisp(err) || hookClaimBeadIsElsewhere(err) {
			t.Fatalf("err = %v: wisp=%v elsewhere=%v, want a wisp skip and never the not-found escalation", err, hookClaimBeadIsAWisp(err), hookClaimBeadIsElsewhere(err))
		}
		if !strings.Contains(err.Error(), "refusing to claim wisp") {
			t.Fatalf("err text %q does not name the refusal", err)
		}
	}
	t.Run("native lane", func(t *testing.T) {
		class := newClaimRouteClassStore(t)
		mintClaimRouteBead(t, class, "mc-wisp-1", nil)
		native := &fakeRemoteNativeStore{beads: map[string]beads.Bead{}, claim: func(id, _ string) (beads.Bead, bool, error) {
			return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", id, beads.ErrWispNotClaimable)
		}}
		router, _ := testRemoteRouter(t, hookClaimRemoteLane{native: native}, nil, &bytes.Buffer{})
		ops := classRoutedHookClaimOps(remoteRoutedHookClaimOps(hookClaimOps{}, router), newClaimRouteFor(t, class))
		_, ok, err := ops.Claim(context.Background(), remoteLegScope, remoteLegEnv(), "mc-wisp-1", "worker-1")
		if ok {
			t.Fatal("a wisp on the remote work store was claimed")
		}
		assertNamed(t, err)
		if held, _ := class.Get("mc-wisp-1"); held.Assignee != "" {
			t.Fatalf("the refusal escalated into the graph store: %+v", held)
		}
	})
	t.Run("bd CLI lane", func(t *testing.T) {
		router, _ := testRemoteRouter(t, hookClaimRemoteLane{}, nil, &bytes.Buffer{})
		cliRefusal := &beads.WispClaimRefusedError{ID: "mc-wisp-1", ScopeRoot: remoteLegScope, Detail: "claiming a wisp is not supported by this bd serve"}
		var envs [][]string
		ops := remoteRoutedHookClaimOps(hookClaimOps{Claim: recordingBaseClaim(&envs, cliRefusal)}, router)
		_, ok, err := ops.Claim(context.Background(), remoteLegScope, remoteLegEnv(), "mc-wisp-1", "worker-1")
		if ok {
			t.Fatal("a wisp on the remote work store was claimed")
		}
		assertNamed(t, err)
	})
}

// TestRemoteRoutedHookClaimOpsRoutesEveryWithEnvField: every claim-cycle op
// that takes a leg env is routed, so no remote leg op silently stays on a bd
// child without the scope's credential.
func TestRemoteRoutedHookClaimOpsRoutesEveryWithEnvField(t *testing.T) {
	router, _ := testRemoteRouter(t, hookClaimRemoteLane{}, nil, &bytes.Buffer{})
	ops := remoteRoutedHookClaimOps(hookClaimOps{}, router)
	for name, set := range map[string]bool{
		"Claim":                    ops.Claim != nil,
		"ListContinuation":         ops.ListContinuation != nil,
		"AssignContinuation":       ops.AssignContinuation != nil,
		"StampWorkMeta":            ops.StampWorkMeta != nil,
		"ReadWorkMeta":             ops.ReadWorkMeta != nil,
		"Release":                  ops.Release != nil,
		"RestampAdopted":           ops.RestampAdopted != nil,
		"ReclaimStale":             ops.ReclaimStale != nil,
		"ConfirmBlocked":           ops.ConfirmBlocked != nil,
		"EmitExecutionStepStarted": ops.EmitExecutionStepStarted != nil,
	} {
		if !set {
			t.Errorf("remoteRoutedHookClaimOps left %s unset", name)
		}
	}
}

func TestHookClaimRemoteScopeRootReadsTheLegsBeadsDir(t *testing.T) {
	if got := hookClaimRemoteScopeRoot("/work/dir", []string{"BEADS_DIR=/city/rigs/a/.beads/"}); got != "/city/rigs/a" {
		t.Fatalf("scope = %q, want the BEADS_DIR parent", got)
	}
	if got := hookClaimRemoteScopeRoot("/work/dir", nil); got != "/work/dir" {
		t.Fatalf("scope without BEADS_DIR = %q, want dir", got)
	}
}

// TestHookClaimRemoteRouterDefaultsReadTheScopeOnDisk: the production seams
// classify an Attach-written scope as remote and a plain one as local, and
// leave a leg env alone when the scope configures no credential.
func TestHookClaimRemoteRouterDefaultsReadTheScopeOnDisk(t *testing.T) {
	cityPath := t.TempDir()
	attached := filepath.Join(cityPath, "rigs", "attached")
	attachRemoteScope(t, attached)
	router := newHookClaimRemoteRouter(cityPath, &bytes.Buffer{})
	if !router.isRemote(attached) {
		t.Fatalf("an Attach-written scope is not routed as remote")
	}
	if router.isRemote(t.TempDir()) {
		t.Fatalf("a scope without remote metadata is routed as remote")
	}
	env := []string{"BEADS_DIR=" + filepath.Join(attached, ".beads")}
	got, err := router.credentialEnv(attached, env)
	if err != nil || !slices.Equal(got, env) {
		t.Fatalf("credential env with none configured = %v (%v), want the leg env unchanged", got, err)
	}
}
