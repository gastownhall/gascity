package runtimetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// ProcessTableScannerFixtures supplies providers for the
// RUNTIME-CONTRACT-024 cases. A nil field skips its cases.
type ProcessTableScannerFixtures struct {
	// Scanning returns a provider whose sessions run on this host, as this
	// user, with the session's Config.Env in their environment, and whose
	// process-table scanner must therefore find their leftovers.
	Scanning func(t *testing.T) runtime.Provider
	// NotScanning returns a provider of the same runtime whose sessions do
	// not share this host, so it must not claim the scanner.
	NotScanning func(t *testing.T) runtime.Provider
	// StartConfig is the config a case starts its session with, before the
	// case sets Command and the session's identity env; nil starts in a
	// fresh work dir.
	StartConfig func(t *testing.T) runtime.Config
}

// startConfig is the config f's cases start a session with: command, with
// sessionID and city as its GC_SESSION_ID and GC_CITY_PATH.
func (f ProcessTableScannerFixtures) startConfig(t *testing.T, command, sessionID, city string) runtime.Config {
	cfg := runtime.Config{WorkDir: t.TempDir()}
	if f.StartConfig != nil {
		cfg = f.StartConfig(t)
	}
	cfg.Command = command
	env := make(map[string]string, len(cfg.Env)+2)
	for k, v := range cfg.Env {
		env[k] = v
	}
	env["GC_SESSION_ID"] = sessionID
	env["GC_CITY_PATH"] = city
	cfg.Env = env
	return cfg
}

// processTableSession is the session the RUNTIME-CONTRACT-024 cases start.
const processTableSession = "process-table-contract"

// RunProcessTableScannerContract runs the RUNTIME-CONTRACT-024 cases, the
// [runtime.ProcessTableScanner] contract for a provider whose sessions run on
// this host: a leftover process of a session (a child that left the agent's
// process group and outlives it) is a root of that session, tracked while
// the provider lists the session and untracked once it is stopped, and is
// found by the orphan sweep's scan once re-parented; TerminateRuntime ends it
// only while its start identity is the scanned one; a root that is city
// infrastructure is still reported, for the callers' fence to judge; and a
// provider whose sessions do not share the host reads as having no scanner,
// finds nothing and terminates nothing.
//
// The scans read a snapshot of the leftover's /proc entry
// ([SnapshotProcRoot]), never the host's whole process table, so no case
// can reach a process it did not start.
func RunProcessTableScannerContract(t *testing.T, f ProcessTableScannerFixtures) {
	t.Helper()

	t.Run("ProcessTable_FindsTheSessionsLeftover", func(t *testing.T) {
		sp, leftover := startLeftoverSession(t, f)
		if sp == nil {
			return
		}
		if err := checkLeftoverFound(t, sp, leftover); err != nil {
			t.Error(err)
		}
	})

	t.Run("ProcessTable_TerminateIsBoundToStartIdentity", func(t *testing.T) {
		sp, leftover := startLeftoverSession(t, f)
		if sp == nil {
			return
		}
		if err := checkTerminateBoundToIdentity(t, sp, leftover); err != nil {
			t.Error(err)
		}
	})

	t.Run("ProcessTable_InfrastructureRootIsReported", func(t *testing.T) {
		sp, leftover := startLeftoverSession(t, f)
		if sp == nil {
			return
		}
		if err := checkInfrastructureRootReported(t, sp, leftover); err != nil {
			t.Error(err)
		}
	})

	t.Run("ProcessTable_NoScannerOffHost", func(t *testing.T) {
		if f.NotScanning == nil {
			t.Skip("no off-host provider fixture")
		}
		requireProcfs(t)
		if err := checkNoScannerOffHost(t, f.NotScanning(t)); err != nil {
			t.Error(err)
		}
	})
}

// requireProcfs skips t where the scan cannot be pointed at a snapshot.
func requireProcfs(t *testing.T) {
	t.Helper()
	if goruntime.GOOS != "linux" {
		t.Skip("the process-table contract reads /proc snapshots, which only linux has")
	}
}

// contractLeftover is a session's leftover process, as a case started it.
type contractLeftover struct {
	Leftover
	name, sessionID, city string
}

// startLeftoverSession starts processTableSession on the Scanning fixture
// with a command that leaves a process behind, and returns the provider and
// the leftover, or a nil provider when the case is skipped.
func startLeftoverSession(t *testing.T, f ProcessTableScannerFixtures) (runtime.Provider, contractLeftover) {
	t.Helper()
	if f.Scanning == nil {
		t.Skip("no scanning-provider fixture")
	}
	requireProcfs(t)
	sp := f.Scanning(t)
	l := contractLeftover{name: processTableSession, sessionID: "process-table-" + randomToken(t), city: t.TempDir()}
	command, wait := LeftoverCommand(t)
	if err := sp.Start(context.Background(), l.name, f.startConfig(t, command, l.sessionID, l.city)); err != nil {
		t.Fatalf("runtime contract 024: starting %s: %v", l.name, err)
	}
	t.Cleanup(func() { _ = sp.Stop(l.name) })
	l.Leftover = wait()
	return sp, l
}

// scannerOf returns sp's scanner, or an error naming the contract sp breaks
// by not having one.
func scannerOf(sp runtime.Provider) (runtime.ProcessTableScanner, error) {
	s, ok := runtime.AsProcessTableScanner(sp)
	if !ok {
		return nil, fmt.Errorf("runtime contract 024: %T has no process-table scanner (runtime.AsProcessTableScanner), but its sessions run on this host", sp)
	}
	return s, nil
}

// findLeftover scans for sessionID on a snapshot of l alone and returns the
// roots the scan reports.
func findLeftover(t *testing.T, s runtime.ProcessTableScanner, l contractLeftover, sessionID string) ([]runtime.LiveRuntime, error) {
	t.Helper()
	proctable.SetScanRootForTesting(t, SnapshotProcRoot(t, l.PID))
	found, err := s.FindRuntimesBySessionID(sessionID)
	if err != nil {
		return found, fmt.Errorf("runtime contract 024: FindRuntimesBySessionID(%q) = %w", sessionID, err)
	}
	return found, nil
}

// checkLeftoverFound reports how sp breaks RUNTIME-CONTRACT-024 in finding
// its session's leftover, while the session runs and after its stop, or nil
// when it keeps the contract.
func checkLeftoverFound(t *testing.T, sp runtime.Provider, l contractLeftover) error {
	t.Helper()
	s, err := scannerOf(sp)
	if err != nil {
		return err
	}
	if err := checkLeftoverWhileRunning(t, s, l); err != nil {
		return err
	}
	return checkLeftoverAfterStop(t, sp, s, l)
}

// checkLeftoverWhileRunning reports how s breaks RUNTIME-CONTRACT-024 for
// the leftover of a running session: the scan for the session reports it
// alone, with its session, city and start identity, tracked under the
// session's name, and a scan for another session reports nothing.
func checkLeftoverWhileRunning(t *testing.T, s runtime.ProcessTableScanner, l contractLeftover) error {
	t.Helper()
	found, err := findLeftover(t, s, l, l.sessionID)
	if err != nil {
		return err
	}
	if len(found) != 1 || found[0].PID != l.PID || found[0].SessionID != l.sessionID || found[0].City != l.city || found[0].StartIdentity != l.Identity {
		return fmt.Errorf("runtime contract 024: scan for %s while it runs = %+v, want only the leftover pid %d with its session, city and start identity %q", l.sessionID, found, l.PID, l.Identity)
	}
	if !found[0].IsTracked || found[0].ProviderName != l.name {
		return fmt.Errorf("runtime contract 024: the leftover of running session %s reads %+v, want tracked as %q: an untracked root of a live session is reaped", l.name, found[0], l.name)
	}
	if other, err := findLeftover(t, s, l, l.sessionID+"-other"); err != nil || len(other) != 0 {
		return fmt.Errorf("runtime contract 024: scan for another session = %+v, %w; want nothing", other, err)
	}
	return nil
}

// checkLeftoverAfterStop stops l's session on sp and reports how s breaks
// RUNTIME-CONTRACT-024 for the leftover that outlives it: the scan for the
// session reports it untracked, and the orphan sweep's scan reports it.
func checkLeftoverAfterStop(t *testing.T, sp runtime.Provider, s runtime.ProcessTableScanner, l contractLeftover) error {
	t.Helper()
	if err := sp.Stop(l.name); err != nil {
		return fmt.Errorf("runtime contract 024: stopping %s: %w", l.name, err)
	}
	if !pidutil.AliveWithStartTime(l.PID, l.Identity) {
		return fmt.Errorf("runtime contract 024: the leftover pid %d did not outlive its session's stop; the case proves nothing", l.PID)
	}
	found, err := findLeftover(t, s, l, l.sessionID)
	if err != nil {
		return err
	}
	if len(found) != 1 || found[0].PID != l.PID || found[0].IsTracked || found[0].ProviderName != "" {
		return fmt.Errorf("runtime contract 024: scan for %s after its stop = %+v, want the leftover pid %d untracked", l.sessionID, found, l.PID)
	}
	orphans, err := findLeftover(t, s, l, "")
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(orphans, func(r runtime.LiveRuntime) bool { return r.PID == l.PID && !r.IsTracked }) {
		return fmt.Errorf("runtime contract 024: the orphan sweep's scan = %+v, want the re-parented leftover pid %d untracked", orphans, l.PID)
	}
	return nil
}

// checkTerminateBoundToIdentity reports how sp breaks RUNTIME-CONTRACT-024
// in terminating a scanned leftover, or nil when it keeps the contract.
func checkTerminateBoundToIdentity(t *testing.T, sp runtime.Provider, l contractLeftover) error {
	t.Helper()
	s, err := scannerOf(sp)
	if err != nil {
		return err
	}
	if err := sp.Stop(l.name); err != nil {
		return fmt.Errorf("runtime contract 024: stopping %s: %w", l.name, err)
	}
	found, err := findLeftover(t, s, l, l.sessionID)
	if err != nil {
		return err
	}
	if len(found) != 1 || found[0].PID != l.PID {
		return fmt.Errorf("runtime contract 024: scan for %s after its stop = %+v, want the leftover pid %d", l.sessionID, found, l.PID)
	}
	recycled := found[0]
	recycled.StartIdentity += "0"
	if err := s.TerminateRuntime(recycled); err != nil {
		return fmt.Errorf("runtime contract 024: TerminateRuntime under a start identity that is not the process's = %w, want nil: the scanned process is gone", err)
	}
	if !pidutil.AliveWithStartTime(l.PID, l.Identity) {
		return fmt.Errorf("runtime contract 024: TerminateRuntime under another start identity ended pid %d: a recycled PID would be signaled", l.PID)
	}
	if err := s.TerminateRuntime(found[0]); err != nil {
		return fmt.Errorf("runtime contract 024: TerminateRuntime(%+v) = %w, want nil", found[0], err)
	}
	if pidutil.AliveWithStartTime(l.PID, l.Identity) {
		return fmt.Errorf("runtime contract 024: pid %d still runs after TerminateRuntime returned nil", l.PID)
	}
	if err := s.TerminateRuntime(found[0]); err != nil {
		return fmt.Errorf("runtime contract 024: TerminateRuntime of a process already gone = %w, want nil", err)
	}
	return nil
}

// checkInfrastructureRootReported reports how sp breaks RUNTIME-CONTRACT-024
// for a root that is city infrastructure, or nil when it keeps the
// contract: the scanner reports it, and only the callers' fence
// (proctable.IsCityInfrastructureRoot) leaves it alone.
func checkInfrastructureRootReported(t *testing.T, sp runtime.Provider, l contractLeftover) error {
	t.Helper()
	s, err := scannerOf(sp)
	if err != nil {
		return err
	}
	root := SnapshotProcRoot(t, l.PID)
	argv := "gc\x00" + proctable.BDProxyChildVerb + "\x00"
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(l.PID), "cmdline"), []byte(argv), 0o644); err != nil {
		t.Fatal(err)
	}
	proctable.SetScanRootForTesting(t, root)
	found, err := s.FindRuntimesBySessionID(l.sessionID)
	if err != nil {
		return fmt.Errorf("runtime contract 024: FindRuntimesBySessionID(%q) = %w", l.sessionID, err)
	}
	if len(found) != 1 || found[0].PID != l.PID {
		return fmt.Errorf("runtime contract 024: scan with the leftover running as city infrastructure = %+v, want it reported (pid %d): the callers fence it", found, l.PID)
	}
	if !proctable.IsCityInfrastructureRoot(l.PID) {
		return fmt.Errorf("runtime contract 024: the snapshot's infrastructure argv is not fenced; the case proves nothing")
	}
	return nil
}

// checkNoScannerOffHost reports how sp, whose sessions do not share this
// host, breaks RUNTIME-CONTRACT-024, or nil when it keeps the contract: it
// reads as having no scanner, and its scanner methods, where it has them,
// find nothing and terminate nothing.
func checkNoScannerOffHost(t *testing.T, sp runtime.Provider) error {
	t.Helper()
	if _, ok := runtime.AsProcessTableScanner(sp); ok {
		return fmt.Errorf("runtime contract 024: %T claims a process-table scanner, but its sessions do not share this host", sp)
	}
	s, ok := sp.(runtime.ProcessTableScanner)
	if !ok {
		return nil
	}
	sessionID := "process-table-" + randomToken(t)
	command, wait := LeftoverCommand(t)
	runLeftoverDirectly(t, command, sessionID)
	l := wait()
	proctable.SetScanRootForTesting(t, SnapshotProcRoot(t, l.PID))
	for _, id := range []string{sessionID, ""} {
		if found, err := s.FindRuntimesBySessionID(id); len(found) != 0 || err != nil {
			return fmt.Errorf("runtime contract 024: FindRuntimesBySessionID(%q) without the scanner = %+v, %w; want nothing", id, found, err)
		}
	}
	live := runtime.LiveRuntime{SessionID: sessionID, PID: l.PID, PPID: 1, StartIdentity: l.Identity}
	if err := s.TerminateRuntime(live); err == nil {
		return fmt.Errorf("runtime contract 024: TerminateRuntime without the scanner = nil, want a refusal")
	}
	if !pidutil.AliveWithStartTime(l.PID, l.Identity) {
		return fmt.Errorf("runtime contract 024: TerminateRuntime without the scanner ended pid %d", l.PID)
	}
	return nil
}

// runLeftoverDirectly runs command on this host with sessionID as its
// GC_SESSION_ID, outside any provider, and reaps it at the end of the test.
func runLeftoverDirectly(t *testing.T, command, sessionID string) {
	t.Helper()
	attr := &os.ProcAttr{
		Env:   append(execenv.Environ(), "GC_SESSION_ID="+sessionID),
		Files: []*os.File{nil, nil, nil},
	}
	proc, err := os.StartProcess("/bin/sh", []string{"sh", "-c", command}, attr)
	if err != nil {
		t.Fatalf("starting %q: %v", command, err)
	}
	t.Cleanup(func() {
		_ = proc.Kill()
		_, _ = proc.Wait()
	})
}

// Leftover is a process a session's agent left behind: it runs in a session
// of its own, re-parented away from the agent, carrying the agent's
// environment (GC_SESSION_ID, GC_CITY_PATH).
type Leftover struct {
	PID      int
	Identity string // its start identity (proctable.ProcessIdentity)
}

// leftoverWait bounds how long LeftoverCommand's wait waits for the leftover
// to report itself.
const leftoverWait = 15 * time.Second

// LeftoverCommand returns a session command whose agent leaves a process
// behind, and a function that waits for that process and returns it. The
// leftover leaves the agent's process group (setsid) through a parent that
// exits at once, so it is re-parented from the start, and stays a shell
// waiting on a sleep, so its pid keeps one start identity. The agent itself
// waits until it is stopped. The command holds no character a start
// request's JSON escapes (it runs a script file), so any runtime can carry
// it.
//
// The leftover's process group is killed when the test ends, but only while
// the pid is still the process this command started (its environment
// carries the command's own token), so the cleanup never signals a process
// the test did not start.
func LeftoverCommand(t *testing.T) (command string, wait func() Leftover) {
	t.Helper()
	dir := t.TempDir()
	token := randomToken(t)
	pidFile := filepath.Join(dir, "leftover.pid")
	script := filepath.Join(dir, "agent.sh")
	body := fmt.Sprintf("#!/bin/sh\n( GC_LEFTOVER_TOKEN=%s setsid sh -c 'echo $$ > %q.tmp && mv %q.tmp %q; sleep 600; :' </dev/null >/dev/null 2>&1 & )\nexec sleep 600\n",
		token, pidFile, pidFile, pidFile)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killLeftover(pidFile, token) })
	return "sh " + script, func() Leftover {
		t.Helper()
		deadline := time.Now().Add(leftoverWait)
		for {
			data, err := os.ReadFile(pidFile)
			if err == nil {
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil || pid <= 1 {
					t.Fatalf("leftover reported pid %q: %v", data, err)
				}
				identity, err := proctable.ProcessIdentity(pid)
				if err != nil {
					t.Fatalf("reading the leftover's start identity: %v", err)
				}
				return Leftover{PID: pid, Identity: identity}
			}
			if time.Now().After(deadline) {
				t.Fatalf("the session's leftover never reported its pid (%s)", pidFile)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// killLeftover kills the process group of the leftover pidFile names while
// its environment carries token.
func killLeftover(pidFile, token string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return
	}
	env, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil || !slices.ContainsFunc(bytes.Split(env, []byte{0}), func(kv []byte) bool { return string(kv) == "GC_LEFTOVER_TOKEN="+token }) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// randomToken is a fresh hex token.
func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// SnapshotProcRoot copies the scanner-visible /proc files (environ, stat,
// comm, status, cmdline) of pids into a fresh fake procfs root for
// proctable.SetScanRootForTesting, with each process's parent rewritten to
// init: re-parented, as the orphan sweep requires, whatever reaper the test
// runner's host has. The scanner refuses the live /proc under go test, and
// a snapshot restricted to the test's own processes keeps any scan away
// from processes it did not start. A pid already gone is left out. The
// host's boot time (the btime line of /proc/stat) is kept, so each root's
// StartedAt is its real start time.
func SnapshotProcRoot(t *testing.T, pids ...int) string {
	t.Helper()
	root := t.TempDir()
	if hostStat, err := os.ReadFile("/proc/stat"); err == nil {
		for _, line := range strings.Split(string(hostStat), "\n") {
			if strings.HasPrefix(line, "btime ") {
				if err := os.WriteFile(filepath.Join(root, "stat"), []byte(line+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, pid := range pids {
		files, err := readProcFiles(pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		stat, err := reparentStat(string(files["stat"]))
		if err != nil {
			t.Fatal(err)
		}
		files["stat"] = []byte(stat)
		dst := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(dst, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// readProcFiles reads pid's scanner-visible /proc files.
func readProcFiles(pid int) (map[string][]byte, error) {
	files := make(map[string][]byte, 5)
	for _, name := range []string{"environ", "stat", "comm", "status", "cmdline"} {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), name))
		if err != nil {
			return nil, err
		}
		files[name] = data
	}
	return files, nil
}

// reparentStat rewrites the parent pid (the field after the state, past the
// command's closing parenthesis) of a /proc/<pid>/stat line to 1.
func reparentStat(stat string) (string, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return "", fmt.Errorf("malformed stat %q", stat)
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 2 {
		return "", fmt.Errorf("malformed stat %q", stat)
	}
	fields[1] = "1"
	return stat[:end+1] + " " + strings.Join(fields, " ") + "\n", nil
}
