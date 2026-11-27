//go:build linux

package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// postStartScanProvider is an orphanScanProvider whose scan reports
// postStart once a Start has run: the roots beside the runtime it launched.
// Before that it reports none, so the pre-start orphan gate passes.
type postStartScanProvider struct {
	*orphanScanProvider
	postStart []runtime.LiveRuntime
}

func (p *postStartScanProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.orphanScanProvider.Start(ctx, name, cfg); err != nil {
		return err
	}
	p.results = p.postStart
	return nil
}

// injectCgroups confines the process-table readers to a procfs root holding
// only each pid's cgroup record, the one file the detached user-service fence
// reads.
func injectCgroups(t *testing.T, cgroups map[int]string) {
	t.Helper()
	root := t.TempDir()
	for pid, content := range cgroups {
		if err := os.MkdirAll(filepath.Join(root, strconv.Itoa(pid)), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFakeCgroup(t, root, pid, content)
	}
	proctable.SetScanRootForTesting(t, root)
}

// recordUnderAnotherUID creates a session under firstUID over a provider
// whose scan, once the runtime is up, reports the runtime root (the test
// process, in a tmux scope) beside service, a root in a foreign user service
// unit (its parent, a live process whose start time reads). It returns the
// store, the city, the session and the record the start wrote.
func recordUnderAnotherUID(t *testing.T, firstUID int, service runtime.LiveRuntime) (*beads.MemStore, string, Info, runtimeOwner) {
	t.Helper()
	classifyInfrastructure(t)
	for _, pid := range []int{os.Getpid(), service.PID} {
		if _, err := pidutil.StartTime(pid); err != nil {
			t.Skipf("start time of pid %d is unreadable here: %v", pid, err)
		}
	}
	injectCgroups(t, map[int]string{
		os.Getpid(): fakeUserManager() + "/app.slice/tmux-spawn-1234.scope",
		service.PID: fakeUserManager() + "/app.slice/devserver.service",
	})
	setCurrentEUID(t, firstUID)
	store := beads.NewMemStore()
	city := t.TempDir()
	sp := &postStartScanProvider{orphanScanProvider: &orphanScanProvider{Fake: runtime.NewFake(), city: city}}
	sp.postStart = []runtime.LiveRuntime{{PID: os.Getpid(), PPID: os.Getppid(), IsTracked: true}, service}
	mgr := newTestManager(t, store, sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatalf("CreateSession under uid %d: %v", firstUID, err)
	}
	owner, ok := readRuntimeOwner(t, mgr, info.ID)
	if !ok || owner.UID != firstUID {
		t.Fatalf("runtime_owner = %+v (present %v), want one under uid %d", owner, ok, firstUID)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend under uid %d: %v", firstUID, err)
	}
	return store, city, info, owner
}

// runtimeEnds rewrites the record as the runtime's exit leaves it: its root
// (the test process) gone, every other recorded root as it was.
func runtimeEnds(t *testing.T, store *beads.MemStore, id string, owner runtimeOwner) {
	t.Helper()
	for i := range owner.Roots {
		if owner.Roots[i].PID == os.Getpid() {
			owner.Roots[i].PID = goneRootPID
		}
	}
	if err := store.SetMetadata(id, "runtime_owner", runtimeOwnerJSON(t, owner.UID, owner.Boot, owner.Roots...)); err != nil {
		t.Fatal(err)
	}
}

// startUnderOwnUID starts the session from a Manager under the test's own
// euid, whose scan, like a restart under another uid's, sees none of the
// recorded roots.
func startUnderOwnUID(t *testing.T, store *beads.MemStore, city string, info Info) error {
	t.Helper()
	setCurrentEUID(t, os.Geteuid())
	mgr := newTestManager(t, store, &orphanScanProvider{Fake: runtime.NewFake(), city: city}, WithCityPath(city))
	return mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
}

// A foreign systemd user service carrying the session's identity
// (SESSION-START-028) is left out of the record a start writes, although the
// provider reports it tracked, as every scanner does for each root carrying
// a running session's id. Once the runtime ends, a start under another uid
// proceeds beside the service rather than being refused on it.
func TestRuntimeOwnerRecordLeavesADetachedUserServiceOut(t *testing.T) {
	logged := captureStdLog(t)
	service := runtime.LiveRuntime{PID: os.Getppid(), PPID: 1, Name: "node", IsTracked: true}
	store, city, info, owner := recordUnderAnotherUID(t, os.Geteuid()+1, service)
	if len(owner.Roots) != 1 || owner.Roots[0].PID != os.Getpid() {
		t.Fatalf("runtime_owner roots = %+v, want the runtime root %d alone, the detached service %d left out", owner.Roots, os.Getpid(), service.PID)
	}
	wantLine := fmt.Sprintf("session: recording runtime owner of %s: leaving root pid=%d out: detached user service devserver.service (ppid=1 name=%q)", info.ID, service.PID, "node")
	if !strings.Contains(logged.String(), wantLine) {
		t.Fatalf("log %q, want the service's exemption logged as %q", logged.String(), wantLine)
	}

	runtimeEnds(t, store, info.ID, owner)
	if err := startUnderOwnUID(t, store, city, info); err != nil {
		t.Fatalf("Start under another uid after the runtime ended: %v, want it to proceed beside the detached user service", err)
	}
}

// A root in a foreign user service that the provider claims as a live
// runtime of its own it does not list (LiveUnlisted, its provider-scoped
// evidence) is recorded as killExistingOrphans would refuse on it, so while
// it lives a start under another uid is still refused.
func TestRuntimeOwnerRecordKeepsAClaimedRootInAUserService(t *testing.T) {
	service := runtime.LiveRuntime{PID: os.Getppid(), PPID: 1, Name: "tmux", LiveUnlisted: true}
	store, city, info, owner := recordUnderAnotherUID(t, os.Geteuid()+1, service)
	if len(owner.Roots) != 2 {
		t.Fatalf("runtime_owner roots = %+v, want the runtime root %d and the claimed root %d", owner.Roots, os.Getpid(), service.PID)
	}

	runtimeEnds(t, store, info.ID, owner)
	err := startUnderOwnUID(t, store, city, info)
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !strings.Contains(err.Error(), "pids "+strconv.Itoa(service.PID)) {
		t.Fatalf("Start under another uid while the claimed root lives: %v, want a refusal naming pid %d", err, service.PID)
	}
}
