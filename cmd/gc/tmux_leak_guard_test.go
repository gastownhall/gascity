//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/test/tmuxtest"
	"github.com/rogpeppe/go-internal/testscript"
)

// Real-tmux cmd/gc tests derive their tmux socket from the CITY NAME (the
// named-session fixtures use "test-city"), and every NewSession* call runs
// ConfigureServer → `set-option -g exit-empty off`, so the tmux SERVER
// deliberately outlives its last session. TestMain's teardown removes the
// per-run socket parent directory, but removing a socket file does not stop
// the server bound to it — each suite run that started a real session leaked
// one immortal `tmux -u -L test-city new-session …` server (dip-73cr05: 72
// accumulated over two weeks and surfaced as phantom provider usage). This
// guard is the source fix: after the suite, any tmux server that belongs to
// this run is killed AND fails the build, so the leak cannot silently return.
//
// Ownership attribution is exact, never name-based. A tmux server belongs to
// this run iff its socket file lives under this run's socket root, or its
// /proc/<pid>/environ carries this run's TMUX_TMPDIR (the per-run root
// TestMain exported before any test ran — every tmux client, and the server a
// client daemonizes, inherits it; the environ path catches servers whose
// socket file was already deleted). Kills therefore target only explicitly
// enumerated socket paths / attributed PIDs under the isolated root — never
// `-L <name>` on the default socket root — so a user's real tmux server (or a
// live city's) can never be hit.

// tmuxGuardKillTimeout bounds each `tmux -S <socket> kill-server` probe so a
// wedged server cannot hang teardown.
const tmuxGuardKillTimeout = 2 * time.Second

type tmuxProcInfo struct {
	PID  int
	Argv []string
}

// readTmuxArgv reads /proc/<pid>/cmdline and returns the NUL-split argv if
// and only if the process is a tmux invocation (argv[0] basename "tmux").
func readTmuxArgv(pid int) ([]string, bool) {
	data, err := readWithTimeout(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	argv := splitCmdline(data)
	if len(argv) == 0 || filepath.Base(argv[0]) != "tmux" {
		return nil, false
	}
	return argv, true
}

// procEnvValue returns the value of key in /proc/<pid>/environ. environ uses
// the same NUL-separated encoding as cmdline.
func procEnvValue(pid int, key string) (string, bool) {
	data, err := readWithTimeout(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil || len(data) == 0 {
		return "", false
	}
	prefix := key + "="
	for _, kv := range splitCmdline(data) {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix), true
		}
	}
	return "", false
}

// discoverTmuxProcessesWithSocketRootEnv walks /proc and returns tmux
// processes whose TMUX_TMPDIR environment value satisfies matchRoot. Hosts
// without /proc return nil — the socket-file pass still covers them.
func discoverTmuxProcessesWithSocketRootEnv(matchRoot func(string) bool) []tmuxProcInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []tmuxProcInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		argv, ok := readTmuxArgv(pid)
		if !ok {
			continue
		}
		root, ok := procEnvValue(pid, "TMUX_TMPDIR")
		if !ok || !matchRoot(root) {
			continue
		}
		out = append(out, tmuxProcInfo{PID: pid, Argv: argv})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// socketFilesUnderTmuxRoot enumerates socket files below root using tmux's
// per-uid layout ($TMUX_TMPDIR/tmux-<uid>/<socket>).
func socketFilesUnderTmuxRoot(root string) []string {
	matches, _ := filepath.Glob(filepath.Join(root, "tmux-*", "*"))
	var out []string
	for _, m := range matches {
		info, err := os.Lstat(m)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			continue
		}
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// killTmuxServerAtSocket issues `tmux -S <socketPath> kill-server` and
// reports whether a live server answered (exit 0). A dead socket file (no
// server) exits non-zero and reports false.
func killTmuxServerAtSocket(socketPath string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxGuardKillTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "tmux", "-S", socketPath, "kill-server").Run() == nil
}

// killTmuxServersUnderSocketRoot kills every live tmux server whose socket
// file sits under root and returns the socket paths that had one. Each
// returned path is a leaked server: nothing under the per-run root may
// legitimately outlive the suite.
func killTmuxServersUnderSocketRoot(root string) []string {
	var leaked []string
	for _, socketPath := range socketFilesUnderTmuxRoot(root) {
		if killTmuxServerAtSocket(socketPath) {
			leaked = append(leaked, socketPath)
		}
	}
	return leaked
}

// reapTmuxLeakProcesses TERM-then-KILLs the given tmux processes.
func reapTmuxLeakProcesses(procs []tmuxProcInfo) {
	pids := make([]int, 0, len(procs))
	for _, p := range procs {
		pids = append(pids, p.PID)
	}
	_ = reapDoltLeakPIDsWithKiller(pids, killProcess)
}

// sweepStaleTmuxTestServers reaps tmux servers left behind by prior or
// sibling test runs that are already gone: a tmux process whose TMUX_TMPDIR
// names a "<gct-…>/tmux" socket root that no longer exists on disk. A live
// run's root exists for its whole lifetime (its alive-sentinel flock is held
// inside it and the dir is only removed at that run's own teardown), so a
// missing root is definitive proof the owning run ended — the server is
// residue of the exact leak class this guard exists for (a crashed or
// pre-guard run whose teardown removed the dir but never killed the server).
// Roots that still exist — including this run's own — are never touched.
func sweepStaleTmuxTestServers(label string, out io.Writer) {
	stale := discoverTmuxProcessesWithSocketRootEnv(func(root string) bool {
		root = strings.TrimSpace(root)
		if root == "" || filepath.Base(root) != "tmux" {
			return false
		}
		parent := filepath.Dir(root)
		if _, ok := pidFromPrefixedDirName(filepath.Base(parent), tmuxtest.SocketParentDirPrefix); !ok {
			return false
		}
		_, err := os.Stat(root)
		return os.IsNotExist(err)
	})
	if len(stale) == 0 {
		return
	}
	fmt.Fprintf(out, "cmd/gc tmux leak guard: %s sweep reaping %d stale test tmux server(s) whose socket root is gone\n", label, len(stale)) //nolint:errcheck
	writeTmuxLeakReport(out, stale)
	reapTmuxLeakProcesses(stale)
}

func writeTmuxLeakReport(w io.Writer, leaked []tmuxProcInfo) {
	for _, proc := range leaked {
		fmt.Fprintf(w, "  pid=%d argv=%q\n", proc.PID, strings.Join(proc.Argv, " ")) //nolint:errcheck
	}
}

// tmuxLeakGuardedTestingM wraps the suite runner with the tmux server leak
// guard: a startup sweep for residue of dead runs, and a fail-the-build
// teardown assertion that zero tmux servers created under this run's socket
// root survive the suite. It composes inside cleanupTestingM (the guard must
// run BEFORE the socket root directory is removed) and outside the dolt leak
// guard.
type tmuxLeakGuardedTestingM struct {
	m          testscript.TestingM
	socketRoot string
}

func newTmuxLeakGuardedTestingM(m testscript.TestingM, socketRoot string) *tmuxLeakGuardedTestingM {
	return &tmuxLeakGuardedTestingM{m: m, socketRoot: socketRoot}
}

func (g *tmuxLeakGuardedTestingM) Run() int {
	return g.runWith(g.m.Run, killTmuxServersUnderSocketRoot, g.discoverOwnedTmuxProcesses, reapTmuxLeakProcesses, os.Stderr)
}

// discoverOwnedTmuxProcesses returns tmux processes attributed to this run
// via environ TMUX_TMPDIR under (or equal to) the run's socket root.
func (g *tmuxLeakGuardedTestingM) discoverOwnedTmuxProcesses() []tmuxProcInfo {
	return discoverTmuxProcessesWithSocketRootEnv(func(root string) bool {
		return pathutil.PathWithin(g.socketRoot, filepath.Clean(strings.TrimSpace(root)))
	})
}

func (g *tmuxLeakGuardedTestingM) runWith(
	runTests func() int,
	killUnderRoot func(string) []string,
	discoverOwned func() []tmuxProcInfo,
	reap func([]tmuxProcInfo),
	out io.Writer,
) int {
	sweepStaleTmuxTestServers("startup", out)

	code := runTests()

	// Phase 1: kill by explicit socket path under the per-run root. Exit 0
	// means a live server answered — a leak.
	leakedSockets := killUnderRoot(g.socketRoot)
	// Phase 2: environ-attributed survivors — servers whose socket file was
	// already deleted (RemoveAll'd early, or tmux cleaned it up) but whose
	// process still runs. A phase-1 server normally exits with its client's
	// return, but a straggler mid-exit may appear in both lists; the double
	// report is cosmetic and reaping an exiting PID is harmless (ESRCH).
	leakedProcs := discoverOwned()
	reap(leakedProcs)

	if len(leakedSockets) == 0 && len(leakedProcs) == 0 {
		return code
	}
	fmt.Fprintf(out, "cmd/gc tmux leak guard: %d tmux server(s) leaked by this run under %s\n", len(leakedSockets)+len(leakedProcs), g.socketRoot) //nolint:errcheck
	for _, socketPath := range leakedSockets {
		fmt.Fprintf(out, "  socket=%s (killed)\n", socketPath) //nolint:errcheck
	}
	writeTmuxLeakReport(out, leakedProcs)
	fmt.Fprintf(out, "cmd/gc tmux leak guard: a test created a real tmux session without tearing its server down; city-name sockets (e.g. -L test-city) have exit-empty off and live forever unless killed (dip-73cr05)\n") //nolint:errcheck
	if code == 0 {
		return 1
	}
	return code
}
