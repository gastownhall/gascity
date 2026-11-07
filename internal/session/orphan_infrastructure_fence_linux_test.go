//go:build linux

package session

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
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
}

func (p *procfsOrphanProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.started = append(p.started, cfg.Env["GC_SESSION_ID"])
	return p.Fake.Start(ctx, name, cfg)
}

func (p *procfsOrphanProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	return proctable.ScanBySessionID(id)
}

func (p *procfsOrphanProvider) TerminateRuntime(r runtime.LiveRuntime) error {
	p.terminated = append(p.terminated, r.PID)
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

// TestSameSessionRestartLeavesDetachedUserServiceAndReapsInSessionChild is
// the systemd-run --user case: a transient user service that inherited the
// session id survives the session's restart, and an in-session child that
// stayed in the session's scope is still reaped. A leftover in the session's
// own unit (the reaper's service, planted at this process's pid) is reaped
// and named. The service's child is not promoted to a root in its place.
func TestSameSessionRestartLeavesDetachedUserServiceAndReapsInSessionChild(t *testing.T) {
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
		servicePID = 5100
		childPID   = 5101
		scopePID   = 5200
		ownPID     = 5300
		systemPID  = 5400
	)
	writeFakeProcess(t, root, servicePID, 1, []string{"sleep", "900"}, env)
	writeFakeCgroup(t, root, servicePID, "/user.slice/user-1000.slice/user@1000.service/app.slice/detached-worker.service")
	writeFakeProcess(t, root, childPID, servicePID, []string{"sleep", "1"}, env)
	writeFakeCgroup(t, root, childPID, "/user.slice/user-1000.slice/user@1000.service/app.slice/detached-worker.service")
	writeFakeProcess(t, root, scopePID, 1, []string{"sleep", "900"}, env)
	writeFakeCgroup(t, root, scopePID, "/user.slice/user-1000.slice/user@1000.service/app.slice/tmux-spawn-abc.scope")
	writeFakeProcess(t, root, ownPID, 1, []string{"claude", "--resume"}, env)
	writeFakeCgroup(t, root, ownPID, "/user.slice/user-1000.slice/user@1000.service/app.slice/agent.service")
	writeFakeProcess(t, root, systemPID, 1, []string{"rogue"}, env)
	writeFakeCgroup(t, root, systemPID, "/system.slice/rogue.service")
	// The reaper's own unit. No session id, so the scan does not reap it.
	self := os.Getpid()
	writeFakeProcess(t, root, self, 1, []string{"gc", "test"}, []string{"PATH=/usr/bin"})
	writeFakeCgroup(t, root, self, "/user.slice/user-1000.slice/user@1000.service/app.slice/agent.service")
	proctable.SetScanRootForTesting(t, root)

	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := map[int]bool{}
	for _, pid := range sp.terminated {
		got[pid] = true
	}
	if got[servicePID] || got[childPID] {
		t.Fatalf("restart terminated %v, want the detached service %d and its child %d left alone", sp.terminated, servicePID, childPID)
	}
	for _, pid := range []int{scopePID, ownPID, systemPID} {
		if !got[pid] {
			t.Fatalf("restart terminated %v, want in-session scope %d, own-unit leftover %d, and system service %d", sp.terminated, scopePID, ownPID, systemPID)
		}
	}
	if len(got) != 3 {
		t.Fatalf("restart terminated %v, want exactly the scope, own-unit, and system leftovers", sp.terminated)
	}
	text := logs.String()
	if !strings.Contains(text, "user service detached-worker.service") {
		t.Errorf("log %q does not name the skipped unit", text)
	}
	if !strings.Contains(text, "unit=agent.service") {
		t.Errorf("log %q does not name the reaped session unit", text)
	}
}

func writeFakeCgroup(t *testing.T, root string, pid int, path string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "cgroup"), []byte("0::"+path+"\n"), 0o644); err != nil {
		t.Fatalf("write cgroup: %v", err)
	}
}
