package tmux

import (
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// writeTrackingProcRoot writes one agent root of session gc-1, reparented to
// init, under a procfs-shaped root the scanner reads.
func writeTrackingProcRoot(t *testing.T) {
	t.Helper()
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "4100")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uid := strconv.Itoa(os.Geteuid())
	for name, body := range map[string]string{
		"environ": "GC_SESSION_ID=gc-1\x00",
		"stat":    "4100 (sleep) S 1" + strings.Repeat(" 0", 48),
		"comm":    "sleep\n",
		"status":  "Name:\tsleep\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proctable.SetScanRootForTesting(t, root)
}

// Kills: a tracking read that drops a failed per-session GC_SESSION_ID read.
// The process-table orphan sweep reaps only untracked roots, so a session
// whose ID could not be read must make the scan's tracking unknown (a whole
// scan failure), not quietly report its roots untracked (CONTRACT C4).
func TestFindRuntimesBySessionIDReportsTrackingFailure(t *testing.T) {
	writeTrackingProcRoot(t)
	for _, tc := range []struct {
		name        string
		outs        []string
		errs        []error
		wantTracked bool
		wantWhole   bool
	}{
		{name: "tracked", outs: []string{"worker", "GC_SESSION_ID=gc-1"}, wantTracked: true},
		{name: "other session", outs: []string{"worker", "GC_SESSION_ID=gc-2"}},
		{name: "per-session read fails", outs: []string{"worker", ""}, errs: []error{nil, errors.New("server busy")}, wantWhole: true},
		{name: "listing fails", errs: []error{errors.New("list timed out")}, wantWhole: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProviderWithConfig(Config{SocketName: "x"})
			p.tm.exec = &fakeExecutor{outs: tc.outs, errs: tc.errs}
			found, err := p.FindRuntimesBySessionID("")
			if len(found) != 1 || found[0].PID != 4100 {
				t.Fatalf("found %+v, want the one root", found)
			}
			if found[0].IsTracked != tc.wantTracked {
				t.Errorf("IsTracked = %v, want %v", found[0].IsTracked, tc.wantTracked)
			}
			if got := proctable.HasWholeScanFailure(err); got != tc.wantWhole {
				t.Errorf("HasWholeScanFailure(%v) = %v, want %v", err, got, tc.wantWhole)
			}
		})
	}
}

// Fixture pids for writePaneProcRoot: a tmux server and the root of one of
// its panes, both carrying session gc-1.
const (
	paneServerPID = 4000
	paneRootPID   = 4100
)

// writePaneProcRoot writes a procfs-shaped root holding a tmux server and a
// pane root of session gc-1 whose parent is that server, with paneEnv added
// to the pane root's environment.
func writePaneProcRoot(t *testing.T, paneEnv ...string) {
	t.Helper()
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	root := t.TempDir()
	uid := strconv.Itoa(os.Geteuid())
	write := func(pid, ppid int, comm string, env []string) {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"environ": strings.Join(env, "\x00") + "\x00",
			"stat":    strconv.Itoa(pid) + " (" + comm + ") S " + strconv.Itoa(ppid) + strings.Repeat(" 0", 48),
			"comm":    comm + "\n",
			"status":  "Name:\t" + comm + "\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(paneServerPID, 1, "tmux: server", []string{"GC_SESSION_ID=gc-1"})
	write(paneRootPID, paneServerPID, "bash", append([]string{"GC_SESSION_ID=gc-1"}, paneEnv...))
	proctable.SetScanRootForTesting(t, root)
}

// AC-7: the tmux provider claims an untracked, tmux-parented root as a live
// runtime of its own that it does not list (LiveUnlisted) unless the root's
// TMUX variable proves its parent is a server bound to another socket. A
// pane of this provider's socket name under another TMUX_TMPDIR, or one the
// listing failed to show, is claimed, and so is a root whose TMUX proves
// nothing; a pane of a server an agent started on its own socket is not, so
// a start terminates it.
func TestFindRuntimesBySessionIDClaimsUnlistedPanesOfItsOwnSocket(t *testing.T) {
	uidDir := "tmux-" + strconv.Itoa(os.Getuid())
	ownElsewhere := "TMUX=/run/user/elsewhere/" + uidDir + "/city," + strconv.Itoa(paneServerPID) + ",0"
	agentOwn := "TMUX=/tmp/" + uidDir + "/agent-own," + strconv.Itoa(paneServerPID) + ",0"
	listedOther := []string{"worker", "GC_SESSION_ID=gc-2"}
	listFails := []error{errors.New("tmux server unreachable")}
	for _, tc := range []struct {
		name       string
		socketName string
		tmuxEnv    []string
		outs       []string
		errs       []error
		want       bool
	}{
		{name: "own socket name in another socket directory", socketName: "city", tmuxEnv: []string{ownElsewhere}, outs: listedOther, want: true},
		{name: "own socket while the listing fails", socketName: "city", tmuxEnv: []string{ownElsewhere}, errs: listFails, want: true},
		{name: "own default socket", tmuxEnv: []string{"TMUX=/tmp/" + uidDir + "/default," + strconv.Itoa(paneServerPID) + ",0"}, outs: listedOther, want: true},
		{name: "own socket path holding a comma", socketName: "city", tmuxEnv: []string{"TMUX=/run/a,b/" + uidDir + "/city," + strconv.Itoa(paneServerPID) + ",0"}, outs: listedOther, want: true},
		{name: "server an agent started on its own socket", socketName: "city", tmuxEnv: []string{agentOwn}, outs: listedOther},
		{name: "agent's own socket while the listing fails", socketName: "city", tmuxEnv: []string{agentOwn}, errs: listFails},
		{name: "default socket of a city on its own socket", socketName: "city", tmuxEnv: []string{"TMUX=/tmp/" + uidDir + "/default," + strconv.Itoa(paneServerPID) + ",0"}, outs: listedOther},
		{name: "TMUX absent", socketName: "city", outs: listedOther, want: true},
		{name: "TMUX names another server pid", socketName: "city", tmuxEnv: []string{"TMUX=/tmp/" + uidDir + "/agent-own,999,0"}, outs: listedOther, want: true},
		{name: "TMUX malformed", socketName: "city", tmuxEnv: []string{"TMUX=agent-own"}, outs: listedOther, want: true},
		{name: "TMUX socket path relative", socketName: "city", tmuxEnv: []string{"TMUX=" + uidDir + "/agent-own," + strconv.Itoa(paneServerPID) + ",0"}, outs: listedOther, want: true},
		{name: "tracked by the listing", socketName: "city", tmuxEnv: []string{ownElsewhere}, outs: []string{"worker", "GC_SESSION_ID=gc-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writePaneProcRoot(t, tc.tmuxEnv...)
			p := NewProviderWithConfig(Config{SocketName: tc.socketName})
			p.tm.exec = &fakeExecutor{outs: tc.outs, errs: tc.errs}
			found, _ := p.FindRuntimesBySessionID("gc-1")
			if len(found) != 1 || found[0].PID != paneRootPID || !found[0].ParentIsProviderInfrastructure {
				t.Fatalf("found %+v, want the one tmux-parented pane root", found)
			}
			if found[0].LiveUnlisted != tc.want {
				t.Errorf("LiveUnlisted = %v, want %v (root %+v)", found[0].LiveUnlisted, tc.want, found[0])
			}
		})
	}
}

// A root reparented away from tmux is never claimed: no pane of this
// provider's is a child of init.
func TestFindRuntimesBySessionIDNeverClaimsReparentedRoots(t *testing.T) {
	writeTrackingProcRoot(t)
	for _, tc := range []struct {
		name string
		outs []string
		errs []error
	}{
		{name: "listed other session", outs: []string{"worker", "GC_SESSION_ID=gc-2"}},
		{name: "listing fails", errs: []error{errors.New("list timed out")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProviderWithConfig(Config{SocketName: "x"})
			p.tm.exec = &fakeExecutor{outs: tc.outs, errs: tc.errs}
			found, _ := p.FindRuntimesBySessionID("gc-1")
			if len(found) != 1 || found[0].PID != 4100 || found[0].LiveUnlisted {
				t.Fatalf("found %+v, want the one reparented root, not LiveUnlisted", found)
			}
		})
	}
}

// Kills: a TerminateRuntime that is not bound to the scanned process
// (CONTRACT C4). Its target is this test process under a start identity that
// is not its own, as a recycled PID would present: the bound kill finds the
// scanned process gone and signals nothing. An unbound kill would terminate
// the test binary, failing the run. proctable's
// TestKillByPIDIdentityRefusesRecycledPID kills a real child under its own
// identity.
func TestTerminateRuntimeBindsKillToScannedIdentity(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("process identity is read on linux and darwin")
	}
	self := os.Getpid()
	identity, err := proctable.ProcessIdentity(self)
	if err != nil {
		t.Fatalf("ProcessIdentity(self): %v", err)
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	if err := p.TerminateRuntime(runtime.LiveRuntime{SessionID: "gc-kill-bind", PID: self, StartIdentity: identity + "0"}); err != nil {
		t.Fatalf("TerminateRuntime(recycled identity) = %v, want nil (the scanned process is gone)", err)
	}
}
