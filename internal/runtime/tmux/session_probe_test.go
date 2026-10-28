package tmux

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// AC-1: ProbeSession reports dead only for a stale socket; every other
// unproven socket observation is unobservable and never reaches tmux.
func TestProbeSessionUnprovenSocketIsUnobservable(t *testing.T) {
	first := socketModeFileInfo{FileInfo: plainFileInfo(t)}
	second := socketModeFileInfo{FileInfo: plainFileInfo(t)}
	refused := func(context.Context, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED }
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context
		lstat   func(string) (os.FileInfo, error)
		dial    func(context.Context, string) (net.Conn, error)
		wantErr error
	}{
		{name: "socket absent", lstat: func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, dial: refused},
		{name: "lstat denied", lstat: func(string) (os.FileInfo, error) { return nil, os.ErrPermission }, dial: refused, wantErr: os.ErrPermission},
		{name: "not a socket", lstat: func(string) (os.FileInfo, error) { return first.FileInfo, nil }, dial: refused},
		{name: "refused on a replaced inode", lstat: sequenceLstat(first, second), dial: refused},
		{name: "dial failed otherwise", lstat: func(string) (os.FileInfo, error) { return first, nil }, dial: func(context.Context, string) (net.Conn, error) {
			return nil, syscall.EACCES
		}},
		{name: "context already ended", ctx: canceled, lstat: func(string) (os.FileInfo, error) { return first, nil }, dial: refused, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			ran := false
			got, err := probeSessionWith(ctx, "gc-probe", "soak-loop", sessionProbeDeps{
				lstat: tt.lstat,
				dial:  tt.dial,
				run: func(context.Context, []string) (string, error) {
					ran = true
					return "", nil
				},
			})
			if got != PresenceUnobservable {
				t.Fatalf("presence = %v, want unobservable (err=%v)", got, err)
			}
			if err == nil {
				t.Fatal("err = nil, want the unproven observation explained")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want it to wrap %v", err, tt.wantErr)
			}
			if ran {
				t.Fatal("tmux ran although no server was observed")
			}
		})
	}
}

// AC-1: a socket refusing connections on a stable inode is a gone server.
func TestProbeSessionStaleSocketIsDead(t *testing.T) {
	path := probeSocketPath(t, "gc-probe")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err := probeSessionWith(context.Background(), "gc-probe", "soak-loop", sessionProbeDeps{
		lstat: os.Lstat,
		dial:  dialUnixSocket,
		run: func(context.Context, []string) (string, error) {
			t.Fatal("tmux ran against a stale socket")
			return "", nil
		},
	})
	if got != PresenceDead || err != nil {
		t.Fatalf("ProbeSession = %v, %v; want dead", got, err)
	}
}

// AC-1: once a live socket is observed, a tmux client failure that is not a
// clean answer, or a probe deadline, stays unobservable.
func TestProbeSessionLiveSocketClientFailureIsUnobservable(t *testing.T) {
	listenProbeSocket(t, "gc-probe")
	t.Run("client failure", func(t *testing.T) {
		got, err := probeSessionWith(context.Background(), "gc-probe", "soak-loop", sessionProbeDeps{
			lstat: os.Lstat,
			dial:  dialUnixSocket,
			run: func(context.Context, []string) (string, error) {
				return "", errors.New("tmux: signal: killed")
			},
		})
		if got != PresenceUnobservable || err == nil {
			t.Fatalf("ProbeSession = %v, %v; want unobservable with an error", got, err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		got, err := probeSessionWith(ctx, "gc-probe", "soak-loop", sessionProbeDeps{
			lstat: os.Lstat,
			dial:  dialUnixSocket,
			run: func(context.Context, []string) (string, error) {
				cancel()
				return "", errors.New("tmux: signal: killed")
			},
		})
		if got != PresenceUnobservable || !errors.Is(err, context.Canceled) {
			t.Fatalf("ProbeSession = %v, %v; want unobservable wrapping the context error", got, err)
		}
	})
}

// probeSocketPath points TMUX_TMPDIR at a short fresh directory and returns
// the socket path name resolves to there, with its directory created.
func probeSocketPath(t *testing.T, name string) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", fmt.Sprintf("gcps%d-", os.Getpid()))
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMUX_TMPDIR", root)
	path := SocketPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return path
}

// listenProbeSocket binds a listener where name resolves, standing in for a
// live tmux server.
func listenProbeSocket(t *testing.T, name string) {
	t.Helper()
	ln, err := net.Listen("unix", probeSocketPath(t, name))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

func plainFileInfo(t *testing.T) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	return info
}

// sequenceLstat returns infos in order, repeating the last.
func sequenceLstat(infos ...os.FileInfo) func(string) (os.FileInfo, error) {
	calls := 0
	return func(string) (os.FileInfo, error) {
		info := infos[min(calls, len(infos)-1)]
		calls++
		return info, nil
	}
}

// AC-2: a server that answers is read by exact session name, so a session
// sharing only a prefix or matching as a pattern does not keep it alive.
func TestProbeSessionReadsTheServerListingExactly(t *testing.T) {
	listenProbeSocket(t, "gc-probe")
	tests := []struct {
		name    string
		listing string
		want    SessionPresence
	}{
		{name: "listed", listing: "other\nsoak-loop\n", want: PresenceAlive},
		{name: "listed without a trailing newline", listing: "soak-loop", want: PresenceAlive},
		{name: "only a longer name listed", listing: "soak-loop-2\n", want: PresenceDead},
		{name: "only a shorter name listed", listing: "soak\n", want: PresenceDead},
		{name: "no session listed", want: PresenceDead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotArgs []string
			got, err := probeSessionWith(context.Background(), "gc-probe", "soak-loop", sessionProbeDeps{
				lstat: os.Lstat,
				dial:  dialUnixSocket,
				run: func(_ context.Context, args []string) (string, error) {
					gotArgs = args
					return tt.listing, nil
				},
			})
			if got != tt.want || err != nil {
				t.Fatalf("ProbeSession = %v, %v; want %v", got, err, tt.want)
			}
			wantArgs := []string{"-L", "gc-probe", "list-sessions", "-F", "#{session_name}"}
			if fmt.Sprint(gotArgs) != fmt.Sprint(wantArgs) {
				t.Fatalf("tmux args = %q, want %q", gotArgs, wantArgs)
			}
		})
	}
}
