//go:build linux

package session

import (
	"context"
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

// writeFakeProcess writes the environ, stat, comm and cmdline files the Linux
// scanner and the kill-path argv fence read for one process.
func writeFakeProcess(t *testing.T, root string, pid, ppid int, argv, env []string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	comm := filepath.Base(argv[0])
	files := map[string]string{
		"environ": strings.Join(env, "\x00") + "\x00",
		"stat":    strconv.Itoa(pid) + " (" + comm + ") S " + strconv.Itoa(ppid) + strings.Repeat(" 0", 48),
		"comm":    comm + "\n",
		"cmdline": strings.Join(argv, "\x00") + "\x00",
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
	mgr := NewManagerWithOptions(store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sp.terminated = nil
	sp.started = nil

	env := []string{"PATH=/usr/bin", "GC_SESSION_ID=" + info.ID, "GC_AGENT=helper"}
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
	t.Cleanup(proctable.SetScanRootForTesting(root))

	if err := mgr.Start(context.Background(), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(sp.terminated) != 1 || sp.terminated[0] != agentPID {
		t.Fatalf("pre-start orphan kill terminated %v, want only the leftover agent pid %d (never the watchdog %d, its dolt %d, or the bd proxy %d)", sp.terminated, agentPID, watchdogPID, doltPID, proxyPID)
	}
	if len(sp.started) != 1 || sp.started[0] != info.ID {
		t.Fatalf("started %v, want the resumed session %s", sp.started, info.ID)
	}
}
