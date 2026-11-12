//go:build linux

package session

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// procfsOrphanProvider is orphanScanProvider backed by the real process-table
// scanner over an injected procfs root, recording the PIDs it would terminate.
type procfsOrphanProvider struct {
	*runtime.Fake
	terminated []int
	started    []string
	// events orders terminations ("terminate:<pid>") and starts ("start").
	events []string
}

func (p *procfsOrphanProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.started = append(p.started, cfg.Env["GC_SESSION_ID"])
	p.events = append(p.events, "start")
	return p.Fake.Start(ctx, name, cfg)
}

func (p *procfsOrphanProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	return proctable.ScanBySessionID(id)
}

func (p *procfsOrphanProvider) TerminateRuntime(r runtime.LiveRuntime) error {
	p.terminated = append(p.terminated, r.PID)
	p.events = append(p.events, "terminate:"+strconv.Itoa(r.PID))
	return nil
}

// writeFakeProcess writes the environ, stat, comm, cmdline and status files
// the Linux scanner and the kill-path argv fence read for one process. The
// status file names the test's effective UID: the scanner reports only
// processes it owns.
func writeFakeProcess(t *testing.T, root string, pid, ppid int, argv, env []string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	comm := filepath.Base(argv[0])
	uid := strconv.Itoa(os.Geteuid())
	files := map[string]string{
		"environ": strings.Join(env, "\x00") + "\x00",
		"stat":    strconv.Itoa(pid) + " (" + comm + ") S " + strconv.Itoa(ppid) + strings.Repeat(" 0", 48),
		"comm":    comm + "\n",
		"cmdline": strings.Join(argv, "\x00") + "\x00",
		"status":  "Name:\t" + comm + "\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// TestSameSessionRestartDoesNotReachManagedDoltWatchdog pins the second
// trigger of #6316: a managed Dolt scope watchdog (or bd's db-proxy-child)
// started from an agent's shell inherits that session's GC_SESSION_ID and,
// once its spawner exits, is reparented to init — so the scanner reports it as
// the session's root. When the same session restarts, killExistingOrphans must
// still reap the genuine leftover agent runtime but leave the infrastructure
// alone, or it SIGTERMs the watchdog's process group and the city's Dolt
// server goes with it. The fixture includes pre-fix, session-stamped
// processes: the argv fence is what protects them, not the env scrub.
func TestSameSessionRestartDoesNotReachManagedDoltWatchdog(t *testing.T) {
	store := beads.NewMemStore()
	sp := &procfsOrphanProvider{Fake: runtime.NewFake()}
	city := t.TempDir()
	mgr := newTestManager(t, store, sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sp.terminated = nil
	sp.started = nil

	env := []string{"PATH=/usr/bin", "GC_SESSION_ID=" + info.ID, "GC_AGENT=helper", "GC_CITY_PATH=" + city}
	root := t.TempDir()
	const (
		watchdogPID = 4100
		doltPID     = 4101
		proxyPID    = 4200
		agentPID    = 4300
	)
	writeFakeProcess(t, root, watchdogPID, 1, []string{"/usr/local/bin/gc", proctable.ManagedDoltScopeWatchdogVerb, "/city/.beads/dolt-config.yaml", "/city/.gc/dolt.log", "/city"}, env)
	writeFakeProcess(t, root, doltPID, watchdogPID, []string{"dolt", "sql-server", "--config", "/city/.beads/dolt-config.yaml"}, env)
	writeFakeProcess(t, root, proxyPID, 1, []string{"/usr/local/bin/bd", proctable.BDProxyChildVerb, "--root", "/city/.beads/proxy"}, env)
	writeFakeProcess(t, root, agentPID, 1, []string{"claude", "--resume"}, env)
	proctable.SetScanRootForTesting(t, root)

	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(sp.terminated) != 1 || sp.terminated[0] != agentPID {
		t.Fatalf("pre-start orphan kill terminated %v, want only the leftover agent pid %d (never the watchdog %d, its dolt %d, or the bd proxy %d)", sp.terminated, agentPID, watchdogPID, doltPID, proxyPID)
	}
	if len(sp.started) != 1 || sp.started[0] != info.ID {
		t.Fatalf("started %v, want the resumed session %s", sp.started, info.ID)
	}
}

// AC-7: a root whose parent is a tmux server is an orphan when its provider
// does not claim it as a live runtime of its own (LiveUnlisted). An agent
// that started a tmux server on a socket of its own leaves such a root
// carrying the session's identity under any provider; this provider, like
// subprocess and ACP, claims none of them. The restart terminates the root
// and starts, and does not defer for as long as that tmux server lives.
func TestSameSessionRestartTerminatesTmuxParentedRootItsProviderDoesNotClaim(t *testing.T) {
	store := beads.NewMemStore()
	sp := &procfsOrphanProvider{Fake: runtime.NewFake()}
	city := t.TempDir()
	mgr := newTestManager(t, store, sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sp.terminated = nil
	sp.started = nil

	env := []string{"PATH=/usr/bin", "GC_SESSION_ID=" + info.ID, "GC_CITY_PATH=" + city}
	root := t.TempDir()
	const (
		serverPID   = 4100
		paneRootPID = 4101
	)
	writeFakeProcess(t, root, serverPID, 1, []string{"tmux: server"}, env)
	writeFakeProcess(t, root, paneRootPID, serverPID, []string{"bash"}, append(env, "TMUX=/tmp/tmux-"+strconv.Itoa(os.Getuid())+"/agent-own,"+strconv.Itoa(serverPID)+",0"))
	proctable.SetScanRootForTesting(t, root)

	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(sp.terminated) != 1 || sp.terminated[0] != paneRootPID {
		t.Fatalf("pre-start orphan kill terminated %v, want the tmux-parented root %d (never its server %d)", sp.terminated, paneRootPID, serverPID)
	}
	if len(sp.started) != 1 || sp.started[0] != info.ID {
		t.Fatalf("started %v, want the resumed session %s", sp.started, info.ID)
	}
}

// writeFakeCgroup writes pid's cgroup file, the record the detached
// user-service fence reads.
func writeFakeCgroup(t *testing.T, root string, pid int, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "cgroup"), []byte(content+"\n"), 0o644); err != nil {
		t.Fatalf("write cgroup: %v", err)
	}
}

// fakeUserManager is the unified cgroup path of the test user's systemd user
// manager.
func fakeUserManager() string {
	uid := strconv.Itoa(os.Geteuid())
	return "0::/user.slice/user-" + uid + ".slice/user@" + uid + ".service"
}

// suspendedProcfsSession creates and suspends a session over a
// procfsOrphanProvider and clears the provider's records.
func suspendedProcfsSession(t *testing.T) (*Manager, *procfsOrphanProvider, Info, string) {
	t.Helper()
	store := beads.NewMemStore()
	sp := &procfsOrphanProvider{Fake: runtime.NewFake()}
	city := t.TempDir()
	mgr := newTestManager(t, store, sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sp.terminated, sp.started, sp.events = nil, nil, nil
	return mgr, sp, info, city
}

// TestSameSessionRestartLeavesDetachedUserServiceAndReapsInSessionChild: a
// session that started a systemd user service leaves a root carrying its
// identity in a user-manager service unit of its own. Neither that root nor
// the session's leftover agent runtime is claimed live by the provider. The
// restart terminates the leftover, confirms it dead before the replacement
// starts, and leaves the service running without blocking the start.
func TestSameSessionRestartLeavesDetachedUserServiceAndReapsInSessionChild(t *testing.T) {
	mgr, sp, info, city := suspendedProcfsSession(t)
	env := []string{"PATH=/usr/bin", "GC_SESSION_ID=" + info.ID, "GC_CITY_PATH=" + city}
	root := t.TempDir()
	const (
		servicePID = 4400
		agentPID   = 4300
	)
	writeFakeProcess(t, root, servicePID, 1, []string{"node", "server.js"}, env)
	writeFakeCgroup(t, root, servicePID, fakeUserManager()+"/app.slice/devserver.service")
	writeFakeProcess(t, root, agentPID, 1, []string{"claude", "--resume"}, env)
	writeFakeCgroup(t, root, agentPID, fakeUserManager()+"/app.slice/tmux-spawn-1234.scope")
	proctable.SetScanRootForTesting(t, root)

	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := []string{"terminate:" + strconv.Itoa(agentPID), "start"}
	if strings.Join(sp.events, ",") != strings.Join(want, ",") {
		t.Fatalf("restart events = %v, want %v (the leftover agent %d terminated before the start, the user service %d never)", sp.events, want, agentPID, servicePID)
	}
	if len(sp.started) != 1 || sp.started[0] != info.ID {
		t.Fatalf("started %v, want the resumed session %s", sp.started, info.ID)
	}
}

// TestSameSessionRestartReapsOrphansInOwnAndSupervisorUnits: the restart's
// user-service fence spares only a foreign unit. The session's leftover roots
// in the restarting process's own unit, or in Gas City's default or suffixed
// supervisor unit, are terminated before the replacement starts; a foreign
// service beside them is left running.
func TestSameSessionRestartReapsOrphansInOwnAndSupervisorUnits(t *testing.T) {
	mgr, sp, info, city := suspendedProcfsSession(t)
	env := []string{"PATH=/usr/bin", "GC_SESSION_ID=" + info.ID, "GC_CITY_PATH=" + city}
	root := t.TempDir()
	self := os.Getpid()
	writeFakeProcess(t, root, self, 1, []string{"gc", "supervisor", "run"}, []string{"PATH=/usr/bin"})
	writeFakeCgroup(t, root, self, fakeUserManager()+"/app.slice/gc-controller.service")
	units := map[int]string{
		4500: "gc-controller.service",
		4501: proctable.SupervisorSystemdUnitStem + ".service",
		4502: proctable.SupervisorSystemdUnitStem + "-3f9a2c.service",
		4400: "devserver.service",
	}
	for pid, unit := range units {
		writeFakeProcess(t, root, pid, 1, []string{"claude", "--resume"}, env)
		writeFakeCgroup(t, root, pid, fakeUserManager()+"/app.slice/"+unit)
	}
	proctable.SetScanRootForTesting(t, root)

	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	terminated := slices.Sorted(slices.Values(sp.terminated))
	if !slices.Equal(terminated, []int{4500, 4501, 4502}) {
		t.Fatalf("pre-start orphan kill terminated %v, want 4500, 4501 and 4502 (own and supervisor units), never the foreign service 4400", terminated)
	}
	if n := len(sp.events); n == 0 || sp.events[n-1] != "start" || slices.Index(sp.events, "start") != n-1 {
		t.Fatalf("restart events = %v, want every termination before the single start", sp.events)
	}
}
