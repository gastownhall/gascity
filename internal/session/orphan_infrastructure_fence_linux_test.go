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
	"github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
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

// execScanOrphanProvider is a fake runtime whose process-table scanner is
// the exec provider's, over a host-sharing golden pack that runs no box,
// recording the PIDs the start would terminate.
type execScanOrphanProvider struct {
	*runtime.Fake
	scanner    runtime.ProcessTableScanner
	terminated []int
}

func (p *execScanOrphanProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	return p.scanner.FindRuntimesBySessionID(id)
}

func (p *execScanOrphanProvider) TerminateRuntime(r runtime.LiveRuntime) error {
	p.terminated = append(p.terminated, r.PID)
	return nil
}

// hostFSGoldenPack writes the conformance golden reference's host-sharing
// variant with an `exec` that runs nothing, and returns its script. The
// golden declares no proc.exec, so gc reads its agents as running outside
// tmux.
func hostFSGoldenPack(t *testing.T) string {
	t.Helper()
	return hostFSGoldenPackWith(t, nil)
}

// hostFSGoldenPackWith is hostFSGoldenPack with edit applied to its op
// bodies first, when set.
func hostFSGoldenPackWith(t *testing.T, edit func(bodies map[string]string)) string {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := reference.HostFSBodies()
	bodies["exec"] = `cat >/dev/null`
	if edit != nil {
		edit(bodies)
	}
	script := filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(stateDir, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// A host-sharing exec pack's session whose agent founded a tmux server in
// its box (`tmux new -d`): the server carries the session's GC_SESSION_ID,
// so the shared scan reads its pane as the session's root. The restart's
// orphan kill terminates the box program's leftovers (under the pack's
// holder, and adopted by init) and never the tmux server, its client, or
// any process in its panes: the exec scanner fences every tmux subtree.
func TestSameSessionRestartOnHostFSExecPackNeverReachesInBoxTmux(t *testing.T) {
	store := beads.NewMemStore()
	city := t.TempDir()
	scanner, ok := runtime.AsProcessTableScanner(sessionexec.NewSeamBackedForCity(hostFSGoldenPack(t), "c", city))
	if !ok {
		t.Fatal("the host-sharing golden pack has no process-table scanner")
	}
	sp := &execScanOrphanProvider{Fake: runtime.NewFake(), scanner: scanner}
	mgr := newTestManager(t, store, sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sp.terminated = nil

	env := []string{"PATH=/usr/bin", "GC_SESSION_ID=" + info.ID, "GC_CITY_PATH=" + city}
	root := t.TempDir()
	const (
		holderPID  = 4100
		programPID = 4101
		clientPID  = 4102
		serverPID  = 4200
		panePID    = 4201
		vimPID     = 4202
		leftover   = 4300
	)
	writeFakeProcess(t, root, holderPID, 1, []string{"holder"}, []string{"PATH=/usr/bin"})
	writeFakeProcess(t, root, programPID, holderPID, []string{"claude"}, env)
	writeFakeProcess(t, root, clientPID, programPID, []string{"tmux: client"}, env)
	writeFakeProcess(t, root, serverPID, 1, []string{"tmux: server"}, env)
	writeFakeProcess(t, root, panePID, serverPID, []string{"bash"}, env)
	writeFakeProcess(t, root, vimPID, panePID, []string{"vim"}, env)
	writeFakeProcess(t, root, leftover, 1, []string{"sleep"}, env)
	proctable.SetScanRootForTesting(t, root)

	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !slices.Equal(sp.terminated, []int{programPID, leftover}) {
		t.Fatalf("pre-start orphan kill terminated %v, want the box's leftovers [%d %d] and never the in-box tmux server %d, client %d, pane %d or vim %d",
			sp.terminated, programPID, leftover, serverPID, clientPID, panePID, vimPID)
	}
}

// The same restart over a host-sharing exec pack whose agent runs in its
// box's tmux session main (it declares proc.exec): the box's tmux server
// copied the box's environment, so the agent of the stopped incarnation is a
// pane of a server carrying the session's GC_SESSION_ID that outlived the
// box, as tmux daemonizes outside the box's process group. The restart's
// orphan kill ends that agent and the box's leftover, so two agents never
// work one session, and leaves the tmux server and the agent's own child to
// the kill of the agent's process group.
func TestSameSessionRestartOnHostFSExecPackEndsTheStaleAgentInTheBoxsTmux(t *testing.T) {
	store := beads.NewMemStore()
	city := t.TempDir()
	pack := hostFSGoldenPackWith(t, func(bodies map[string]string) {
		bodies["protocol"] = strings.Replace(bodies["protocol"], `"fs.host"`, `"fs.host","proc.exec"`, 1)
	})
	scanner, ok := runtime.AsProcessTableScanner(sessionexec.NewSeamBackedForCity(pack, "c", city))
	if !ok {
		t.Fatal("the host-sharing golden pack has no process-table scanner")
	}
	sp := &execScanOrphanProvider{Fake: runtime.NewFake(), scanner: scanner}
	mgr := newTestManager(t, store, sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sp.terminated = nil

	env := []string{"PATH=/usr/bin", "GC_SESSION_ID=" + info.ID, "GC_CITY_PATH=" + city}
	root := t.TempDir()
	const (
		serverPID = 4200
		agentPID  = 4201
		childPID  = 4202
		leftover  = 4300
	)
	writeFakeProcess(t, root, serverPID, 1, []string{"tmux: server"}, env)
	writeFakeProcess(t, root, agentPID, serverPID, []string{"claude"}, env)
	writeFakeProcess(t, root, childPID, agentPID, []string{"node"}, env)
	writeFakeProcess(t, root, leftover, 1, []string{"sleep"}, env)
	proctable.SetScanRootForTesting(t, root)

	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !slices.Equal(sp.terminated, []int{agentPID, leftover}) {
		t.Fatalf("pre-start orphan kill terminated %v, want the stale agent %d in the box's tmux and the leftover %d", sp.terminated, agentPID, leftover)
	}
}

// The host-sharing exec pack of TestSameSessionRestartOnHostFSExecPackNeverReachesInBoxTmux
// as the default backend of the auto composite gc builds when the city also
// has ACP agents (auto(exec, acp), cmd/gc/providers.go). The composite merges
// every leaf's scan, and the ACP leaf's own scan reports an in-box pane as an
// untracked root of the session: merged, the restart's orphan kill would end
// it through the exec leaf, whether or not the pack still lists the box. The
// composite reports only the PIDs its exec leaf's fenced scan reports, so
// the scan reports the box's program and its leftover alone, tracked while
// the box is listed, and the restart never reaches the in-box tmux server,
// its client, or any process in its panes.
func TestSameSessionRestartOnAutoOverHostFSExecPackNeverReachesInBoxTmux(t *testing.T) {
	const (
		holderPID  = 4100
		programPID = 4101
		clientPID  = 4102
		serverPID  = 4200
		panePID    = 4201
		vimPID     = 4202
		leftover   = 4300
	)
	for _, tc := range []struct {
		name           string
		boxListed      bool
		wantTracked    bool
		wantTerminated []int
	}{
		{name: "box listed", boxListed: true, wantTracked: true, wantTerminated: nil},
		{name: "box gone", boxListed: false, wantTracked: false, wantTerminated: []int{programPID, leftover}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			city := t.TempDir()
			execSP := sessionexec.NewSeamBackedForCity(hostFSGoldenPack(t), "c", city)
			composite := auto.New(execSP, acp.NewSeamBackedWithDir(t.TempDir(), acp.Config{}))
			scanner, ok := runtime.AsProcessTableScanner(composite)
			if !ok {
				t.Fatal("auto over the host-sharing golden pack has no process-table scanner")
			}
			sp := &execScanOrphanProvider{Fake: runtime.NewFake(), scanner: scanner}
			mgr := newTestManager(t, store, sp, WithCityPath(city))
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
				t.Fatalf("Suspend: %v", err)
			}
			sp.terminated = nil
			if tc.boxListed {
				cfg := runtime.Config{
					Command:              "sleep 600",
					WorkDir:              t.TempDir(),
					Env:                  map[string]string{"GC_SESSION_ID": info.ID, "GC_SESSION_NAME": "seat"},
					ProcessNames:         []string{"sh", "sleep"},
					AcceptStartupDialogs: new(bool),
				}
				if err := execSP.Start(context.Background(), "seat", cfg); err != nil {
					t.Fatalf("starting the box: %v", err)
				}
				t.Cleanup(func() { _ = execSP.Stop("seat") })
			}

			env := []string{"PATH=/usr/bin", "GC_SESSION_ID=" + info.ID, "GC_CITY_PATH=" + city}
			root := t.TempDir()
			writeFakeProcess(t, root, holderPID, 1, []string{"holder"}, []string{"PATH=/usr/bin"})
			writeFakeProcess(t, root, programPID, holderPID, []string{"claude"}, env)
			writeFakeProcess(t, root, clientPID, programPID, []string{"tmux: client"}, env)
			writeFakeProcess(t, root, serverPID, 1, []string{"tmux: server"}, env)
			writeFakeProcess(t, root, panePID, serverPID, []string{"bash"}, env)
			writeFakeProcess(t, root, vimPID, panePID, []string{"vim"}, env)
			writeFakeProcess(t, root, leftover, 1, []string{"sleep"}, env)
			proctable.SetScanRootForTesting(t, root)

			found, err := scanner.FindRuntimesBySessionID(info.ID)
			if err != nil {
				t.Fatalf("FindRuntimesBySessionID: %v", err)
			}
			var pids []int
			for _, r := range found {
				pids = append(pids, r.PID)
				if r.IsTracked != tc.wantTracked || r.LiveUnlisted {
					t.Errorf("root %+v: want IsTracked=%t and not LiveUnlisted", r, tc.wantTracked)
				}
			}
			if !slices.Equal(pids, []int{programPID, leftover}) {
				t.Fatalf("the composite's scan = %+v, want the box's roots [%d %d] alone: nothing in the in-box tmux server's subtree", found, programPID, leftover)
			}

			if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if !slices.Equal(sp.terminated, tc.wantTerminated) {
				t.Fatalf("pre-start orphan kill terminated %v, want %v, never the in-box tmux server %d, client %d, pane %d or vim %d",
					sp.terminated, tc.wantTerminated, serverPID, clientPID, panePID, vimPID)
			}
		})
	}
}

// The exception to AC-7 (TestSameSessionRestartTerminatesTmuxParentedRootItsProviderDoesNotClaim)
// in a city that runs ACP agents over a host-sharing exec pack: gc wraps the
// exec base as auto(exec, acp), and the composite reports only the PIDs the
// exec leaf's fenced scan reports. The fence cannot tell a tmux server an
// ACP session's agent founded, which carries that session's GC_SESSION_ID,
// from one inside an exec box, so an ACP-routed session's own tmux-parented
// leftover is not reported: its restart leaves the pane running and the
// start proceeds rather than refusing.
func TestSameSessionRestartOnAutoOverHostFSExecPackLeavesAnACPSessionsOwnTmuxPane(t *testing.T) {
	store := beads.NewMemStore()
	city := t.TempDir()
	composite := auto.New(sessionexec.NewSeamBackedForCity(hostFSGoldenPack(t), "c", city), acp.NewSeamBackedWithDir(t.TempDir(), acp.Config{}))
	scanner, ok := runtime.AsProcessTableScanner(composite)
	if !ok {
		t.Fatal("auto over the host-sharing golden pack has no process-table scanner")
	}
	sp := &execScanOrphanProvider{Fake: runtime.NewFake(), scanner: scanner}
	mgr := newTestManager(t, store, sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	composite.RouteACP(info.SessionName)
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sp.terminated = nil
	startsBefore := sp.CountCalls("Start", info.SessionName)

	env := []string{"PATH=/usr/bin", "GC_SESSION_ID=" + info.ID, "GC_CITY_PATH=" + city}
	root := t.TempDir()
	const (
		serverPID   = 4100
		paneRootPID = 4101
	)
	writeFakeProcess(t, root, serverPID, 1, []string{"tmux: server"}, env)
	writeFakeProcess(t, root, paneRootPID, serverPID, []string{"bash"}, append(env, "TMUX=/tmp/tmux-"+strconv.Itoa(os.Getuid())+"/agent-own,"+strconv.Itoa(serverPID)+",0"))
	proctable.SetScanRootForTesting(t, root)

	found, err := scanner.FindRuntimesBySessionID(info.ID)
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("the composite's scan = %+v, want nothing: the exec leaf's fence drops the ACP session's tmux pane", found)
	}
	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(sp.terminated) != 0 {
		t.Fatalf("pre-start orphan kill terminated %v, want nothing: the ACP session's own tmux pane %d is fenced, unlike standalone ACP (AC-7)", sp.terminated, paneRootPID)
	}
	if got := sp.CountCalls("Start", info.SessionName) - startsBefore; got != 1 {
		t.Fatalf("Start calls after the restart = %d, want 1: the start proceeds", got)
	}
}
