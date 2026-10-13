package tmuxtest

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// TestSweepOrphanPIDPrefixedDirsKillsLiveTmuxServerBeforeRemoval covers the
// gap behind ga-t33q83: a tmux server whose socket lives inside a
// PID-prefixed socket parent dir must not survive that dir being swept.
// Uncertain or failed reaping must retain the socket and its parent so a live
// server remains discoverable. After confirmed reaping, both may be removed.
func TestSweepOrphanPIDPrefixedDirsKillsLiveTmuxServerBeforeRemoval(t *testing.T) {
	RequireTmux(t)

	// Unix domain socket paths are capped at ~108 bytes (sizeof sun_path).
	// t.TempDir() embeds the full test name and is too long for that limit,
	// so this uses a short private root directly under /tmp. Production test
	// binaries use GC_TEST_TMUX_SOCKET_PARENT_ROOT when configured and retain
	// /tmp only as a creation-only legacy fallback.
	root, err := os.MkdirTemp("/tmp", "tmuxtest-orphan-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	var socket string
	var serverPID int
	var serverStart string
	t.Cleanup(func() {
		// A failing sweep may already have unlinked the socket. Reuse the
		// bounded reaper with the exact captured server identity when known;
		// before capture, query only sockets under this private fixture root.
		reaper := liveTmuxServerReaper
		var diagnostics bytes.Buffer
		var reaped bool
		if serverPID > 0 && serverStart != "" {
			reaper.serverIdentity = func(string) (int, string, error) {
				return serverPID, serverStart, nil
			}
			reaped = reaper.reapServerAtSocket(socket, &diagnostics)
		} else {
			reaped = reaper.killServersUnder(root, &diagnostics)
		}
		if !reaped {
			t.Errorf("retaining unresolved tmux fixture %s: %s", root, &diagnostics)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("removing reaped tmux fixture %s: %v", root, err)
		}
	})
	dir := pidPrefixedTestDir(t, root, "pfx-", nonLivePID(t))
	socket = filepath.Join(dir, "sock")
	ctx, cancel := context.WithTimeout(context.Background(), tmuxGuardCommandTimeout)
	out, err := exec.CommandContext(ctx, "tmux", "-S", socket, "new-session", "-d", "-s", "orphantest", "sleep", "60").CombinedOutput()
	cancel()
	if err != nil {
		t.Fatalf("starting real tmux server at %s: %v: %s", socket, err, out)
	}

	ctx, cancel = context.WithTimeout(context.Background(), tmuxGuardCommandTimeout)
	pidOut, err := exec.CommandContext(ctx, "tmux", "-S", socket, "display-message", "-p", "-t", "orphantest", "#{pid}").Output()
	cancel()
	if err != nil {
		t.Fatalf("querying tmux server pid: %v", err)
	}
	serverPID, err = strconv.Atoi(strings.TrimSpace(string(pidOut)))
	if err != nil || serverPID <= 0 {
		t.Fatalf("parsing tmux server pid %q: %v", pidOut, err)
	}
	// Capture identity while this PID still names the owned server.
	serverStart, _ = pidutil.StartTime(serverPID)
	t.Logf("owned tmux fixture: socket=%s pid=%d start=%s", socket, serverPID, serverStart)

	if !pidutil.Alive(serverPID) {
		t.Fatalf("tmux server pid %d not alive right after starting it", serverPID)
	}

	// Backdate only now that the server has finished creating its socket
	// file inside dir -- an earlier backdate would be overwritten by that
	// write touching dir's mtime again, leaving it under the sweep's
	// min-age guard (the same ordering TestSweepOrphanPIDPrefixedDirsRemovesFreeSentinel
	// uses relative to HoldAliveSentinel).
	backdatePastSweepAge(t, dir)

	for _, tt := range []struct {
		name         string
		identityErr  error
		signalErr    error
		tmuxIdentity bool
	}{
		{name: "pid query times out", identityErr: context.DeadlineExceeded, tmuxIdentity: true},
		{name: "pid query exits nonzero", identityErr: errNoTmuxServerAtSocket, tmuxIdentity: true},
		{name: "SIGKILL is rejected", signalErr: syscall.EPERM, tmuxIdentity: true},
		{name: "survivor no longer looks like tmux"},
		{name: "server survives SIGKILL", tmuxIdentity: true},
	} {
		if !t.Run(tt.name, func(t *testing.T) {
			host := fakeReapHost{
				pid: serverPID, startTime: serverStart,
				identityErr: tt.identityErr, signalErr: tt.signalErr,
				aliveAfterKill: true, aliveAfterSignal: true, tmuxIdentity: tt.tmuxIdentity,
			}
			live := liveTmuxServerReaper
			liveTmuxServerReaper = host.reaper()
			t.Cleanup(func() { liveTmuxServerReaper = live })
			var diagnostics bytes.Buffer
			SweepOrphanPIDPrefixedDirs(root, "pfx-", &diagnostics)
			for _, path := range []string{dir, socket} {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("unresolved server lost %s: %v; diagnostics: %s", path, err, &diagnostics)
				}
			}
			if !pidutil.AliveWithStartTime(serverPID, serverStart) {
				t.Error("real tmux fixture exited during an unresolved-reap canary")
			}
		}) {
			return
		}
	}

	SweepOrphanPIDPrefixedDirs(root, "pfx-", io.Discard)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("socket parent dir survived sweep, fixture invalid: %s", dir)
	}
	if pidutil.AliveWithStartTime(serverPID, serverStart) {
		t.Errorf("tmux server pid %d at %s is still alive after its socket parent dir was swept -- orphaned, matches ga-t33q83", serverPID, socket)
	}
}
