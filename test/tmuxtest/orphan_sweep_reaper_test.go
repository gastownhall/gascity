package tmuxtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
)

// fakeReapHost stands in for the host process table and the tmux client so
// reapServerAtSocket's escalation contract can be exercised without spawning
// a real server -- in particular the branch where kill-server FAILS while the
// server keeps running, which no real-tmux fixture can produce on demand.
type fakeReapHost struct {
	pid            int
	startTime      string
	identityErr    error
	killErr        error
	aliveAfterKill bool
	tmuxIdentity   bool
	signalErr      error

	killedSockets []string
	signaled      []int
}

// reaper returns a tmuxServerReaper wired to h. exitWait is zero so the poll
// loop resolves immediately; the escalation decision after it is what these
// tests pin.
func (h *fakeReapHost) reaper() tmuxServerReaper {
	return tmuxServerReaper{
		serverIdentity: func(string) (int, string, error) {
			if h.identityErr != nil {
				return 0, "", h.identityErr
			}
			return h.pid, h.startTime, nil
		},
		killServer: func(socketPath string) error {
			h.killedSockets = append(h.killedSockets, socketPath)
			return h.killErr
		},
		sameProcess: func(pid int, startTime string) bool {
			return h.aliveAfterKill && pid == h.pid && startTime == h.startTime
		},
		isTmuxProcess: func(int) bool { return h.tmuxIdentity },
		signal: func(pid int, _ syscall.Signal) error {
			h.signaled = append(h.signaled, pid)
			return h.signalErr
		},
		exitWait:     0,
		pollInterval: time.Millisecond,
	}
}

// TestReapServerAtSocketEscalation pins what the sweep does to a tmux server
// it cannot politely shut down. The load-bearing row is
// "kill-server failed, server survives": the caller unlinks the socket
// whatever happens here, so returning early on a kill error would strand the
// server alive, socketless, and unlogged -- the exact orphan the sweep exists
// to eliminate (ga-t33q83).
func TestReapServerAtSocketEscalation(t *testing.T) {
	const socket = "/tmp/gct-fake-12345-abc/sock"
	const serverPID = 4242
	const startTime = "8675309"
	self := os.Getpid()

	tests := []struct {
		name            string
		host            fakeReapHost
		wantKills       int
		wantSignaled    []int
		wantDiagnostics string
	}{
		{
			name:            "server exits after kill-server",
			host:            fakeReapHost{pid: serverPID, startTime: startTime},
			wantKills:       1,
			wantDiagnostics: fmt.Sprintf("tmuxtest: killed orphaned tmux server at socket %s (pid %d)\n", socket, serverPID),
		},
		{
			name: "kill-server failed, server survives",
			host: fakeReapHost{
				pid: serverPID, startTime: startTime,
				killErr:        errors.New("exit status 1"),
				aliveAfterKill: true, tmuxIdentity: true,
			},
			wantKills:    1,
			wantSignaled: []int{serverPID},
			wantDiagnostics: fmt.Sprintf("tmuxtest: kill-server failed for orphaned tmux server at socket %s (pid %d): exit status 1\n", socket, serverPID) +
				fmt.Sprintf("tmuxtest: SIGKILLed orphaned tmux server at socket %s (pid %d) after it outlived kill-server by 0s\n", socket, serverPID),
		},
		{
			name: "kill-server accepted but server survives",
			host: fakeReapHost{
				pid: serverPID, startTime: startTime,
				aliveAfterKill: true, tmuxIdentity: true,
			},
			wantKills:       1,
			wantSignaled:    []int{serverPID},
			wantDiagnostics: fmt.Sprintf("tmuxtest: SIGKILLed orphaned tmux server at socket %s (pid %d) after it outlived kill-server by 0s\n", socket, serverPID),
		},
		{
			name: "survivor is no longer a tmux process",
			host: fakeReapHost{
				pid: serverPID, startTime: startTime,
				aliveAfterKill: true, tmuxIdentity: false,
			},
			wantKills:       1,
			wantDiagnostics: fmt.Sprintf("tmuxtest: orphaned tmux server at socket %s (pid %d) outlived kill-server and no longer looks like tmux; not signaling\n", socket, serverPID),
		},
		{
			name: "socket names the sweeping process itself",
			host: fakeReapHost{
				pid: self, startTime: startTime,
				aliveAfterKill: true, tmuxIdentity: true,
			},
			wantDiagnostics: fmt.Sprintf("tmuxtest: socket %s reported this process (pid %d) as its tmux server; not signaling\n", socket, self),
		},
		{
			name: "nothing answering at the socket",
			host: fakeReapHost{identityErr: errNoTmuxServerAtSocket},
		},
		{
			name: "pid query timed out",
			host: fakeReapHost{
				identityErr: fmt.Errorf("querying tmux server pid at %s: %w", socket, context.DeadlineExceeded),
			},
			wantDiagnostics: fmt.Sprintf("tmuxtest: leaving socket %s unreaped: querying tmux server pid at %s: %v\n", socket, socket, context.DeadlineExceeded),
		},
		{
			name: "SIGKILL rejected by the kernel",
			host: fakeReapHost{
				pid: serverPID, startTime: startTime,
				aliveAfterKill: true, tmuxIdentity: true,
				signalErr: syscall.EPERM,
			},
			wantKills:       1,
			wantSignaled:    []int{serverPID},
			wantDiagnostics: fmt.Sprintf("tmuxtest: SIGKILL failed for orphaned tmux server at socket %s (pid %d): %v\n", socket, serverPID, syscall.EPERM),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := tt.host
			var diagnostics bytes.Buffer

			host.reaper().reapServerAtSocket(socket, &diagnostics)

			if len(host.killedSockets) != tt.wantKills {
				t.Errorf("kill-server calls = %v, want %d", host.killedSockets, tt.wantKills)
			}
			for _, got := range host.killedSockets {
				if got != socket {
					t.Errorf("kill-server targeted %q, want the explicit socket %q", got, socket)
				}
			}
			if !equalPIDs(host.signaled, tt.wantSignaled) {
				t.Errorf("signaled pids = %v, want %v", host.signaled, tt.wantSignaled)
			}
			if got := diagnostics.String(); got != tt.wantDiagnostics {
				t.Errorf("diagnostics =\n%q\nwant\n%q", got, tt.wantDiagnostics)
			}
		})
	}
}

func equalPIDs(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestIsTmuxArgv pins the identity rule the SIGKILL escalation gates on: the
// basename of argv[0], not a substring of the command line, so a process
// merely mentioning tmux is never signaled.
func TestIsTmuxArgv(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want bool
	}{
		{name: "absolute tmux path", argv: []string{"/usr/bin/tmux", "-S", "/tmp/sock"}, want: true},
		{name: "bare tmux", argv: []string{"tmux"}, want: true},
		{name: "local build of tmux", argv: []string{"/opt/homebrew/bin/tmux", "new-session"}, want: true},
		{name: "different program with a tmux prefix", argv: []string{"/usr/bin/tmuxinator", "start"}, want: false},
		{name: "tmux named only in an argument", argv: []string{"/bin/sh", "-c", "tmux kill-server"}, want: false},
		{name: "empty argv", argv: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTmuxArgv(tt.argv); got != tt.want {
				t.Errorf("isTmuxArgv(%q) = %v, want %v", tt.argv, got, tt.want)
			}
		})
	}
}
