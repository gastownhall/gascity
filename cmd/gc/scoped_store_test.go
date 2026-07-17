package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

func noopBdRunner() beads.CommandRunner {
	return func(string, string, ...string) ([]byte, error) {
		return nil, nil
	}
}

func TestBdStoreBackingFindsDirectBdStore(t *testing.T) {
	store := beads.NewBdStore("/city", noopBdRunner())
	got, ok := bdStoreBacking(store)
	if !ok || got != store {
		t.Fatalf("bdStoreBacking() = (%p, %v), want (%p, true)", got, ok, store)
	}
}

func TestBdStoreBackingUnwrapsCachingStore(t *testing.T) {
	inner := beads.NewBdStore("/city", noopBdRunner())
	cached := beads.NewCachingStoreForTest(inner, nil)
	got, ok := bdStoreBacking(cached)
	if !ok || got != inner {
		t.Fatalf("bdStoreBacking() = (%p, %v), want (%p, true)", got, ok, inner)
	}
}

// TestBdStoreBackingUnwrapsCachingAndPolicyLayers proves unwrapping is
// order-independent. Production re-applies policy outside the cache, while
// this inverse stack can still arise in tests and adapters; both must expose
// the *beads.BdStore whose subprocess ga-cdmx6x is about.
func TestBdStoreBackingUnwrapsCachingAndPolicyLayers(t *testing.T) {
	inner := beads.NewBdStore("/city", noopBdRunner())
	policyWrapped := wrapStoreWithBeadPolicies(inner, &config.City{})
	cached := beads.NewCachingStoreForTest(policyWrapped, nil)
	got, ok := bdStoreBacking(cached)
	if !ok || got != inner {
		t.Fatalf("bdStoreBacking() = (%p, %v), want (%p, true)", got, ok, inner)
	}
}

func TestBdStoreBackingReturnsFalseForNonBdStore(t *testing.T) {
	if got, ok := bdStoreBacking(beads.NewMemStore()); ok {
		t.Fatalf("bdStoreBacking() = (%v, true), want ok=false for a MemStore", got)
	}
}

func TestBdStoreBackingReturnsFalseForNil(t *testing.T) {
	if _, ok := bdStoreBacking(nil); ok {
		t.Fatal("bdStoreBacking(nil) ok = true, want false")
	}
}

// TestScopedStoreLikeReturnsNilForNonBdBackedStore proves the mitigation
// leaves non-bd-CLI backends (native, file, exec, mem) untouched — they
// have no subprocess to leak, so callers should keep reading through the
// existing store rather than pay for (or risk breaking) a reconstruction.
func TestScopedStoreLikeReturnsNilForNonBdBackedStore(t *testing.T) {
	scoped, err := scopedStoreLike(context.Background(), "/city", &config.City{}, beads.NewMemStore())
	if err != nil {
		t.Fatalf("scopedStoreLike: %v", err)
	}
	if scoped != nil {
		t.Fatalf("scopedStoreLike() = %v, want nil for a non-bd-backed store", scoped)
	}
}

// TestScopedStoreLikeSelectsCityScopeWhenDirMatchesCityPath proves the
// city/rig branch selection: when the backing BdStore's dir equals
// cityPath, the clone must be built via the city-level env resolution
// (scopedBdStoreForCity), not the rig-level one.
func TestScopedStoreLikeSelectsCityScopeWhenDirMatchesCityPath(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	existing := beads.NewBdStore(cityDir, noopBdRunner())

	scoped, err := scopedStoreLike(context.Background(), cityDir, &config.City{}, existing)
	if err != nil {
		t.Fatalf("scopedStoreLike: %v", err)
	}
	bs, ok := scoped.(*beads.BdStore)
	if !ok {
		t.Fatalf("scopedStoreLike() = %T, want *beads.BdStore", scoped)
	}
	if got := bs.Dir(); got != cityDir {
		t.Fatalf("scoped store Dir() = %q, want city path %q", got, cityDir)
	}
}

// TestScopedStoreLikeSelectsRigScopeWhenDirIsARig proves the rig branch:
// when the backing BdStore's dir is a rig root (not the city root), the
// clone is built via the rig-level env resolution (scopedBdStoreForRig),
// pointed at the rig's own dir.
func TestScopedStoreLikeSelectsRigScopeWhenDirIsARig(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	rigDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: "rigs/repo"}}}
	existing := beads.NewBdStore(rigDir, noopBdRunner())

	scoped, err := scopedStoreLike(context.Background(), cityDir, cfg, existing)
	if err != nil {
		t.Fatalf("scopedStoreLike: %v", err)
	}
	bs, ok := scoped.(*beads.BdStore)
	if !ok {
		t.Fatalf("scopedStoreLike() = %T, want *beads.BdStore", scoped)
	}
	if got := bs.Dir(); got != rigDir {
		t.Fatalf("scoped store Dir() = %q, want rig dir %q", got, rigDir)
	}
}

// TestScopedStoreLikePreservesBeadPolicyWrapper pins behavioral equivalence,
// not just the backing directory. Production stores are policy-wrapped outside
// the cache; dropping that wrapper from a scoped clone changes zero-value List
// and Ready reads from TierBoth to TierIssues.
func TestScopedStoreLikePreservesBeadPolicyWrapper(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	cfg := &config.City{}
	backing := beads.NewBdStore(cityDir, noopBdRunner())
	cached := beads.NewCachingStoreForTest(backing, nil)
	existing := wrapStoreWithBeadPolicies(cached, cfg)

	scoped, err := scopedStoreLike(context.Background(), cityDir, cfg, existing)
	if err != nil {
		t.Fatalf("scopedStoreLike: %v", err)
	}
	inner, policy, ok := unwrapBeadPolicyStore(scoped)
	if !ok {
		t.Fatalf("scopedStoreLike() = %T, want a policy-wrapped clone", scoped)
	}
	if policy.cfg != cfg {
		t.Fatalf("scoped policy config = %p, want original %p", policy.cfg, cfg)
	}
	if _, ok := inner.(*beads.BdStore); !ok {
		t.Fatalf("scoped policy backing = %T, want *beads.BdStore", inner)
	}
}

func TestScopedStoreLikeHonorsCanceledResolutionContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	existing := beads.NewBdStore(t.TempDir(), noopBdRunner())

	_, err := scopedStoreLike(ctx, t.TempDir(), &config.City{}, existing)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scopedStoreLike error = %v, want context.Canceled", err)
	}
}

// TestScopedStoreLikeAvoidsManagedDoltRecovery is a regression test: an
// earlier version of scopedBdStoreForCity/scopedBdStoreForRig called
// bdRuntimeEnvWithError/bdRuntimeEnvForRigWithError (allowRecovery=true),
// which — the first time a city's bd-CLI store is constructed and no
// managed dolt server is yet running — spawns and waits on a real `dolt
// sql-server` before returning env, taking 10+ seconds. That defeats "fast
// bounded mitigation" and, worse, means every concurrent short-budget
// status read would each attempt that recovery simultaneously during
// exactly the kind of incident this bead exists to bound.
// scopedBdStoreForCity/scopedBdStoreForRig must stay on the NoRecovery env
// resolution so this path is fast-fail, not fast-fix, when no managed
// server is reachable.
func TestScopedStoreLikeAvoidsManagedDoltRecovery(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)

	// bdStoreForCity mirrors how the real shared store gets constructed at
	// controller/CLI startup; its first call materializes
	// .gc/scripts/gc-beads-bd.sh, which is what made a later env
	// resolution believe a managed dolt server ought to exist and attempt
	// to start/recover one.
	realStore := bdStoreForCity(cityDir, cityDir)

	start := time.Now()
	scoped, err := scopedStoreLike(context.Background(), cityDir, &config.City{}, realStore)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("scopedStoreLike took %s, want fast (no managed-dolt recovery attempt)", elapsed)
	}
	if err != nil {
		t.Fatalf("scopedStoreLike: %v", err)
	}
	if _, ok := scoped.(*beads.BdStore); !ok {
		t.Fatalf("scopedStoreLike() = %T, want *beads.BdStore", scoped)
	}
}

// TestScopedBdStoreForCityKillsChildOnCtxCancel is the ga-cdmx6x regression
// test: proves a bd child spawned through scopedBdStoreForCity is killed
// when ctx is canceled, instead of surviving to bdCommandTimeout the way
// the long-lived context.Background()-bound shared store's child would.
// Mirrors TestKillCommandTreeKillsProcessGroup's pidfile pattern
// (internal/beads/bdstore_exec_internal_test.go), applied through the real
// scopedBdStoreForCity construction path instead of a bare exec.Command.
func TestScopedBdStoreForCityKillsChildOnCtxCancel(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)

	binDir := t.TempDir()
	pidFile := filepath.Join(binDir, "bd-child.pid")
	writeExecutable(t, filepath.Join(binDir, "bd"), "#!/bin/sh\n"+
		"sleep 30 &\n"+
		"echo \"$!\" > "+pidFile+"\n"+
		"wait\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	store, err := scopedBdStoreForCity(ctx, cityDir)
	if err != nil {
		t.Fatalf("scopedBdStoreForCity: %v", err)
	}

	start := time.Now()
	if _, listErr := store.List(beads.ListQuery{AllowScan: true}); listErr == nil {
		t.Fatal("List unexpectedly succeeded against a sleeping bd stub")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("List blocked %s; the 200ms ctx deadline was not honored", elapsed)
	}

	childPid := waitForNonEmptyFileContent(t, pidFile, 5*time.Second)
	for range 50 {
		if err := exec.Command("kill", "-0", childPid).Run(); err != nil {
			return // child is gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = exec.Command("kill", "-KILL", childPid).Run()
	t.Fatalf("bd child process %s survived scopedBdStoreForCity's ctx cancellation", childPid)
}

func waitForNonEmptyFileContent(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.TrimSpace(string(data))
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to be written", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
