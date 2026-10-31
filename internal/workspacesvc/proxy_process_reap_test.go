package workspacesvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/processgroup"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

var errInjectedReap = errors.New("injected: owned tree not confirmed reaped")

// reapTestService prepares a proxy_process service running command, with the
// service tree release replaced by release for the rest of the test. It returns
// the runtime, the service, and the state root its unconfirmed reaps are held
// under. Anything still pending when the test ends is settled with the real
// release, so a failing test cannot strand a tree.
func reapTestService(t *testing.T, command []string, release func(*exec.Cmd) error) (*testRuntime, config.Service, string) {
	t.Helper()
	cityPath := t.TempDir()
	rt := &testRuntime{
		cityPath: cityPath,
		cityName: "test-city",
		cfg:      &config.City{},
		sp:       runtime.NewFake(),
		store:    beads.NewMemStore(),
	}
	svc := config.Service{
		Name: "bridge",
		Kind: "proxy_process",
		Process: config.ServiceProcessConfig{
			Command:    command,
			HealthPath: "/healthz",
		},
	}
	if _, err := ensureStateRoot(cityPath, svc); err != nil {
		t.Fatalf("ensureStateRoot: %v", err)
	}
	root := filepath.Join(cityPath, svc.StateRootOrDefault())

	t.Cleanup(func() {
		if err := settleServiceReaps(serviceRootKey(root)); err != nil {
			t.Errorf("settling pending reaps after the test: %v", err)
		}
	})
	prev := releaseProxyProcessTree
	releaseProxyProcessTree = release
	t.Cleanup(func() { releaseProxyProcessTree = prev })
	return rt, svc, root
}

// startReadyReapTestInstance starts a service that serves from this test binary
// and waits for it to report ready. The instance is closed, and its last
// generation's reaper joined, when the test ends.
func startReadyReapTestInstance(t *testing.T, release func(*exec.Cmd) error) (*proxyProcessInstance, string) {
	t.Helper()
	t.Setenv("GC_SERVICE_HELPER", "1")
	setHelperPassthrough(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	rt, svc, root := reapTestService(t, []string{exe, "-test.run=^TestProxyProcessHelper$", "--"}, release)
	inst, err := newProxyProcessInstance(rt, svc, Status{ServiceName: svc.Name, Kind: svc.Kind})
	if err != nil {
		t.Fatalf("newProxyProcessInstance: %v", err)
	}
	p := inst.(*proxyProcessInstance)
	t.Cleanup(func() {
		_ = p.Close()
		p.mu.Lock()
		doneCh := p.doneCh
		p.mu.Unlock()
		awaitClosed(t, doneCh, "the last generation's reaper")
	})
	if status := p.Status(); status.LocalState != "ready" {
		t.Fatalf("LocalState = %q, want ready (reason=%q)", status.LocalState, status.Reason)
	}
	return p, root
}

func currentGeneration(p *proxyProcessInstance) (*exec.Cmd, chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd, p.doneCh
}

func pendingReaps(root string) []*exec.Cmd {
	serviceReaps.Lock()
	defer serviceReaps.Unlock()
	return slices.Clone(serviceReaps.unconfirmed[serviceRootKey(root)])
}

func killLeader(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing service leader %d: %v", cmd.Process.Pid, err)
	}
}

func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// failingRelease reports every release unconfirmed while failing is set, and
// performs the real release otherwise.
func failingRelease(failing *atomic.Bool) func(*exec.Cmd) error {
	return func(cmd *exec.Cmd) error {
		if failing.Load() {
			return errInjectedReap
		}
		return processgroup.ReleaseCommand(cmd)
	}
}

func TestProxyProcessGenerationStaysCurrentWhileItsTreeIsReaped(t *testing.T) {
	entered := make(chan struct{}, 1)
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	inst, _ := startReadyReapTestInstance(t, func(cmd *exec.Cmd) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-unblock
		return processgroup.ReleaseCommand(cmd)
	})
	t.Cleanup(func() { unblockOnce.Do(func() { close(unblock) }) })

	first, firstDone := currentGeneration(inst)
	killLeader(t, first)
	awaitClosed(t, entered, "the reaper to begin releasing the tree")

	if cmd, _ := currentGeneration(inst); cmd != first {
		t.Errorf("generation retired while its tree release was still running (current=%v)", cmd)
	}
	select {
	case <-firstDone:
		t.Error("generation reported finished while its tree release was still running")
	default:
	}
	inst.Tick(context.Background(), time.Now().UTC().Add(time.Minute))
	if cmd, _ := currentGeneration(inst); cmd != first {
		t.Errorf("Tick replaced the generation while its tree release was still running (current=%v)", cmd)
	}

	unblockOnce.Do(func() { close(unblock) })
	awaitClosed(t, firstDone, "the reaper to retire the generation")
	if cmd, _ := currentGeneration(inst); cmd != nil {
		t.Fatalf("generation still current after its reaper finished (current=%v)", cmd)
	}
}

func TestProxyProcessWithholdsRestartUntilTheReapIsConfirmed(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	inst, root := startReadyReapTestInstance(t, failingRelease(&failing))

	first, firstDone := currentGeneration(inst)
	killLeader(t, first)
	awaitClosed(t, firstDone, "the reaper to retire the generation")
	exitReason := inst.Status().Reason

	later := time.Now().UTC().Add(time.Minute)
	inst.Tick(context.Background(), later)
	if cmd, _ := currentGeneration(inst); cmd != nil {
		t.Fatal("Tick started a replacement beside a tree whose reap is unconfirmed")
	}
	if !strings.Contains(exitReason, errInjectedReap.Error()) {
		t.Errorf("status reason at retirement = %q, want it to record the unconfirmed reap", exitReason)
	}
	if reason := inst.Status().Reason; !strings.Contains(reason, "not confirmed reaped") || !strings.Contains(reason, errInjectedReap.Error()) {
		t.Errorf("status reason = %q, want it to say the restart waits on the unconfirmed reap", reason)
	}
	if !slices.Contains(pendingReaps(root), first) {
		t.Error("a generation whose reap failed is not held for a retry")
	}

	failing.Store(false)
	inst.Tick(context.Background(), later.Add(time.Minute))
	second, _ := currentGeneration(inst)
	if second == nil || second == first {
		t.Fatalf("Tick did not restart once the reap was confirmed (current=%v)", second)
	}
	if pending := pendingReaps(root); len(pending) != 0 {
		t.Fatalf("%d reap(s) still pending after a confirmed release", len(pending))
	}
	if status := inst.Status(); status.LocalState != "ready" {
		t.Fatalf("LocalState = %q after the restart, want ready (reason=%q)", status.LocalState, status.Reason)
	}
}

func TestProxyProcessCloseReportsAnUnconfirmedReap(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	inst, root := startReadyReapTestInstance(t, failingRelease(&failing))

	first, firstDone := currentGeneration(inst)
	killLeader(t, first)
	awaitClosed(t, firstDone, "the reaper to retire the generation")

	if err := inst.Close(); !errors.Is(err, errInjectedReap) {
		t.Fatalf("Close = %v, want the unconfirmed reap", err)
	}
	if !slices.Contains(pendingReaps(root), first) {
		t.Fatal("Close dropped a generation whose reap is still unconfirmed")
	}

	failing.Store(false)
	if err := inst.Close(); err != nil {
		t.Fatalf("Close once the reap is confirmed = %v, want nil", err)
	}
	if pending := pendingReaps(root); len(pending) != 0 {
		t.Fatalf("%d reap(s) still pending after Close confirmed them", len(pending))
	}
}

func TestNewProxyProcessInstanceReportsAndWaitsOnAnUnconfirmedReap(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	var failing atomic.Bool
	failing.Store(true)
	// A run that matches no tests exits at once, before any listener exists.
	rt, svc, root := reapTestService(t, []string{exe, "-test.run=^$"}, failingRelease(&failing))
	construct := func() error {
		inst, err := newProxyProcessInstance(rt, svc, Status{ServiceName: svc.Name, Kind: svc.Kind})
		if err == nil {
			_ = inst.Close()
			t.Fatal("newProxyProcessInstance succeeded for a service that exits at once")
		}
		return err
	}

	err = construct()
	if !errors.Is(err, errProxyProcessExitedEarly) || !errors.Is(err, errInjectedReap) {
		t.Errorf("first construction = %v, want the early exit and the unconfirmed reap", err)
	}
	if len(pendingReaps(root)) != 1 {
		t.Errorf("pending reaps = %d, want the exited generation held", len(pendingReaps(root)))
	}

	err = construct()
	if errors.Is(err, errProxyProcessExitedEarly) || !errors.Is(err, errInjectedReap) {
		t.Fatalf("second construction = %v, want it refused on the unconfirmed reap before spawning", err)
	}

	failing.Store(false)
	err = construct()
	if !errors.Is(err, errProxyProcessExitedEarly) || errors.Is(err, errInjectedReap) {
		t.Fatalf("construction once the reap is confirmed = %v, want only the early exit", err)
	}
	if pending := pendingReaps(root); len(pending) != 0 {
		t.Fatalf("%d reap(s) still pending after a confirmed release", len(pending))
	}
}

// TestProxyProcessReplacementWaitsForAReleaseStillRunning is the replacement
// that overtook an old generation's reap: a release still running was recorded
// nowhere a new instance could see, so the new instance started beside a tree
// that release might yet fail to reap.
func TestProxyProcessReplacementWaitsForAReleaseStillRunning(t *testing.T) {
	entered := make(chan struct{}, 1)
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	inst, _ := startReadyReapTestInstance(t, func(cmd *exec.Cmd) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-unblock
		return processgroup.ReleaseCommand(cmd)
	})
	t.Cleanup(func() { unblockOnce.Do(func() { close(unblock) }) })

	first, firstDone := currentGeneration(inst)
	if !processgroup.HoldsTree(first) {
		t.Skip("releasing a service tree does no work on this platform, so no release is ever in flight")
	}
	killLeader(t, first)
	awaitClosed(t, entered, "the reaper to begin releasing the tree")

	construct := func() (Instance, error) {
		return newProxyProcessInstance(inst.rt, inst.svc, Status{ServiceName: inst.svc.Name, Kind: inst.svc.Kind})
	}
	replacement, err := construct()
	if err == nil {
		_ = replacement.Close()
		t.Fatal("a replacement instance started while the previous generation's release was still running")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("replacement refused with %v, want it to name the release still running", err)
	}

	unblockOnce.Do(func() { close(unblock) })
	awaitClosed(t, firstDone, "the reaper to retire the generation")
	replacement, err = construct()
	if err != nil {
		t.Fatalf("replacement once the release confirmed = %v, want it to start", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatalf("closing the replacement: %v", err)
	}
}

// TestProxyProcessCloseReportsTheReapOfTheGenerationItStopped is the Close
// that returned before the generation it stopped had finished its release, so
// a reap that then failed was reported to nobody.
func TestProxyProcessCloseReportsTheReapOfTheGenerationItStopped(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	inst, root := startReadyReapTestInstance(t, func(cmd *exec.Cmd) error {
		if failing.Load() {
			// Late, as a release finishing after the stop would be.
			time.Sleep(500 * time.Millisecond)
			return errInjectedReap
		}
		return processgroup.ReleaseCommand(cmd)
	})
	first, _ := currentGeneration(inst)
	if !processgroup.HoldsTree(first) {
		t.Skip("releasing a service tree does no work on this platform")
	}

	if err := inst.Close(); !errors.Is(err, errInjectedReap) {
		t.Fatalf("Close = %v, want the failed reap of the generation it stopped", err)
	}
	if !slices.Contains(pendingReaps(root), first) {
		t.Fatal("the generation whose reap failed is not held for a retry")
	}

	failing.Store(false)
	if err := inst.Close(); err != nil {
		t.Fatalf("Close once the reap is confirmed = %v, want nil", err)
	}
}

// TestProxyProcessReapRegistryMatchesAnEquivalentSpelling is the barrier an
// equivalent spelling walked around: on a case-insensitive file system a state
// root spelled in another case is the same directory, and was a different key.
func TestProxyProcessReapRegistryMatchesAnEquivalentSpelling(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	var failing atomic.Bool
	failing.Store(true)
	rt, svc, root := reapTestService(t, []string{exe, "-test.run=^$"}, failingRelease(&failing))
	variant := svc
	variant.StateRoot = strings.ToUpper(svc.StateRootOrDefault())
	variantRoot := proxyProcessStateRoot(rt, variant)
	if variantRoot == root {
		t.Fatalf("the variant spelling %q is not a different spelling", variantRoot)
	}
	rootInfo, rootErr := os.Stat(root)
	variantInfo, variantErr := os.Stat(variantRoot)
	if rootErr != nil || variantErr != nil || !os.SameFile(rootInfo, variantInfo) {
		t.Skipf("%q and %q are not one directory on this file system", root, variantRoot)
	}
	finishServiceRelease(serviceRootKey(root), &exec.Cmd{}, false, errInjectedReap)

	inst, err := newProxyProcessInstance(rt, variant, Status{ServiceName: variant.Name, Kind: variant.Kind})
	if err == nil {
		_ = inst.Close()
		t.Fatal("an instance started under a state root with an unconfirmed reap, spelled another way")
	}
	if !errors.Is(err, errInjectedReap) || errors.Is(err, errProxyProcessExitedEarly) {
		t.Fatalf("construction = %v, want it refused on the unconfirmed reap before spawning", err)
	}
	failing.Store(false)
}

// TestManagerCloseReportsAFailedConstructionsUnconfirmedReap is the Close that
// reported every service stopped while a failed construction had left a
// generation whose reap was never confirmed: with no instance, nothing settled
// its state root.
func TestManagerCloseReportsAFailedConstructionsUnconfirmedReap(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	var failing atomic.Bool
	failing.Store(true)
	// A run that matches no tests exits at once, before any listener exists.
	rt, svc, root := reapTestService(t, []string{exe, "-test.run=^$"}, failingRelease(&failing))
	rt.cfg = &config.City{Services: []config.Service{svc}}
	mgr := NewManager(rt)
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if status, _ := mgr.Get(svc.Name); status.LocalState != "config_error" {
		t.Fatalf("LocalState after Reload = %q, want the failed construction (reason=%q)", status.LocalState, status.Reason)
	}
	if len(pendingReaps(root)) != 1 {
		t.Fatalf("pending reaps = %d, want the failed construction's generation held", len(pendingReaps(root)))
	}

	if err := mgr.Close(); !errors.Is(err, errInjectedReap) {
		t.Fatalf("Manager.Close = %v, want the failed construction's unconfirmed reap", err)
	}
	if status, _ := mgr.Get(svc.Name); status.LocalState != "close_error" {
		t.Errorf("LocalState after Close = %q, want close_error (reason=%q)", status.LocalState, status.Reason)
	}

	failing.Store(false)
	if err := mgr.Close(); err != nil {
		t.Fatalf("Manager.Close once the reap is confirmed = %v, want nil", err)
	}
	if status, _ := mgr.Get(svc.Name); status.LocalState != "stopped" {
		t.Errorf("LocalState after the second Close = %q, want stopped", status.LocalState)
	}
}

// refreshTestManager starts a manager running one published proxy_process
// service that serves from this test binary, with every release reported
// unconfirmed while failing is set. It returns the manager, the service, its
// state root, and a function that moves the service's published URL, which
// makes the next Tick or Reload refresh it.
func refreshTestManager(t *testing.T, failing *atomic.Bool) (*Manager, config.Service, string, func(string)) {
	t.Helper()
	t.Setenv("GC_SERVICE_HELPER", "1")
	setHelperPassthrough(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	rt, svc, root := reapTestService(t, []string{exe, "-test.run=^TestProxyProcessHelper$", "--"}, failingRelease(failing))
	svc.Publication = config.ServicePublicationConfig{Visibility: "public"}
	rt.cfg = &config.City{Workspace: config.Workspace{Name: "demo-app"}, Services: []config.Service{svc}}
	rt.pubCfg = supervisor.PublicationConfig{Provider: "hosted", TenantSlug: "acme", PublicBaseDomain: "apps.example.com"}
	publish := func(url string) {
		writePublicationStoreForTest(t, rt.cityPath, fmt.Sprintf(`[{"service_name": %q, "visibility": "public", "url": %q}]`, svc.Name, url))
	}
	publish("https://bridge--acme--11111111.apps.example.com")
	mgr := NewManager(rt)
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	t.Cleanup(func() {
		failing.Store(false)
		if err := mgr.Close(); err != nil {
			t.Errorf("Manager.Close: %v", err)
		}
	})
	if status, _ := mgr.Get(svc.Name); status.LocalState != "ready" {
		t.Fatalf("LocalState = %q, want ready (reason=%q)", status.LocalState, status.Reason)
	}
	return mgr, svc, root, publish
}

func managerInstance(mgr *Manager, name string) Instance {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	if e, ok := mgr.entries[name]; ok {
		return e.inst
	}
	return nil
}

// skipUnlessReleaseDoesWork skips where releasing a service tree does no work,
// so no reap is ever pending.
func skipUnlessReleaseDoesWork(t *testing.T, mgr *Manager, name string) {
	t.Helper()
	inst, ok := managerInstance(mgr, name).(*proxyProcessInstance)
	if !ok {
		t.Fatalf("service %s has no running proxy process", name)
	}
	if cmd, _ := currentGeneration(inst); !processgroup.HoldsTree(cmd) {
		t.Skip("releasing a service tree does no work on this platform")
	}
}

// TestATickRefreshStopsItsReplacementWhileTheOldReapIsUnconfirmed is D1 (c)
// on the publication refresh: the new generation starts first, and when the
// old one's reap cannot be confirmed the new one is stopped rather than left
// running beside whatever survived. The service stays down until the reap is
// confirmed, then comes back.
func TestATickRefreshStopsItsReplacementWhileTheOldReapIsUnconfirmed(t *testing.T) {
	var failing atomic.Bool
	mgr, svc, root, publish := refreshTestManager(t, &failing)
	skipUnlessReleaseDoesWork(t, mgr, svc.Name)

	failing.Store(true)
	publish("https://bridge--acme--22222222.apps.example.com")
	now := time.Now().UTC()
	mgr.Tick(context.Background(), now)
	if managerInstance(mgr, svc.Name) != nil {
		t.Fatalf("the replacement is running while the generation it replaced is not confirmed reaped (%d pending)", len(pendingReaps(root)))
	}
	if status, _ := mgr.Get(svc.Name); status.State != "degraded" || !strings.Contains(status.Reason, "not confirmed reaped") {
		t.Fatalf("status = %s/%s %q, want degraded, naming the unconfirmed reap", status.State, status.LocalState, status.Reason)
	}

	mgr.Tick(context.Background(), now.Add(2*proxyProcessRestartBackoff))
	if managerInstance(mgr, svc.Name) != nil {
		t.Fatal("a retry started the service while the reap was still unconfirmed")
	}

	failing.Store(false)
	mgr.Tick(context.Background(), now.Add(4*proxyProcessRestartBackoff))
	if managerInstance(mgr, svc.Name) == nil {
		status, _ := mgr.Get(svc.Name)
		t.Fatalf("the service did not come back once the reap was confirmed: %s/%s %q", status.State, status.LocalState, status.Reason)
	}
}

// TestAReloadRefreshStopsItsReplacementWhileTheOldReapIsUnconfirmed is the
// same on a Reload that recreates the service for a new publication context.
func TestAReloadRefreshStopsItsReplacementWhileTheOldReapIsUnconfirmed(t *testing.T) {
	var failing atomic.Bool
	mgr, svc, root, publish := refreshTestManager(t, &failing)
	skipUnlessReleaseDoesWork(t, mgr, svc.Name)

	failing.Store(true)
	publish("https://bridge--acme--33333333.apps.example.com")
	if err := mgr.Reload(); !errors.Is(err, errInjectedReap) {
		t.Fatalf("Reload = %v, want the replaced generation's unconfirmed reap", err)
	}
	if managerInstance(mgr, svc.Name) != nil {
		t.Fatalf("the replacement is running while the generation it replaced is not confirmed reaped (%d pending)", len(pendingReaps(root)))
	}

	failing.Store(false)
	mgr.Tick(context.Background(), time.Now().UTC().Add(2*proxyProcessRestartBackoff))
	if managerInstance(mgr, svc.Name) == nil {
		status, _ := mgr.Get(svc.Name)
		t.Fatalf("the service did not come back once the reap was confirmed: %s/%s %q", status.State, status.LocalState, status.Reason)
	}
}

// TestAFailedStopHoldsTheStateRootUntilAReleaseConfirmsIt covers a stop that
// fails with the leader still running. Its reaper has not registered the tree,
// so Close holds it pending itself, and nothing starts under the state root
// until a release confirms it.
func TestAFailedStopHoldsTheStateRootUntilAReleaseConfirmsIt(t *testing.T) {
	var failing atomic.Bool
	inst, root := startReadyReapTestInstance(t, failingRelease(&failing))
	cmd, _ := currentGeneration(inst)
	if !processgroup.HoldsTree(cmd) {
		t.Skip("releasing a service tree does no work on this platform")
	}
	prev := stopProxyProcessTree
	stopProxyProcessTree = func(*exec.Cmd) error { return errors.New("injected: stop not confirmed") }
	t.Cleanup(func() { stopProxyProcessTree = prev })

	failing.Store(true)
	if err := inst.Close(); err == nil {
		t.Fatal("Close reported success over a tree it could not stop")
	}
	if !slices.Contains(pendingReaps(root), cmd) {
		t.Fatal("the tree the stop could not confirm is not held pending")
	}
	if replacement, err := newProxyProcessInstance(inst.rt, inst.svc, Status{ServiceName: inst.svc.Name, Kind: inst.svc.Kind}); err == nil {
		_ = replacement.Close()
		t.Fatal("a replacement started beside a tree its stop could not confirm reaped")
	}

	failing.Store(false)
	if err := settleServiceReaps(serviceRootKey(root)); err != nil {
		t.Fatalf("settling once releases confirm: %v", err)
	}
}
