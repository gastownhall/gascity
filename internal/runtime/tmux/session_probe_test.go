package tmux

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// liveUnixSocket binds a listener in a short directory under /tmp and returns
// its path; connections are accepted and closed until cleanup.
func liveUnixSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gcps")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return path
}

func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

func runNotCalled(t *testing.T) func(context.Context, []string) (string, error) {
	return func(context.Context, []string) (string, error) {
		t.Error("ran tmux without a live socket")
		return "", errors.New("unexpected tmux run")
	}
}

// AC-1: nothing observed at the canonical socket path is unobservable, never
// dead: an absent socket, an unreadable path, a non-socket file, a socket
// replaced or failing mid-dial, a context that ended, and a tmux client that
// cannot query a live socket.
func TestProbeSessionUnobservableIsNeverDead(t *testing.T) {
	info := socketFixtureInfo(t)
	other := socketFixtureInfo(t)
	regular, err := os.Lstat(t.TempDir())
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	calls := 0
	replaced := func(string) (os.FileInfo, error) {
		calls++
		if calls == 1 {
			return info, nil
		}
		return other, nil
	}
	refused := func(context.Context, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED }
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	live := liveUnixSocket(t)

	cases := []struct {
		name  string
		ctx   context.Context
		path  string
		lstat func(string) (os.FileInfo, error)
		dial  func(context.Context, string) (net.Conn, error)
		run   func(context.Context, []string) (string, error)
	}{
		{name: "socket file absent", path: "/nonexistent/s", lstat: missingSocket, dial: dialNotCalled(t), run: runNotCalled(t)},
		{name: "unreadable path", path: "/x/s", lstat: func(string) (os.FileInfo, error) { return nil, os.ErrPermission }, dial: dialNotCalled(t), run: runNotCalled(t)},
		{name: "not a socket", path: "/x/s", lstat: func(string) (os.FileInfo, error) { return regular, nil }, dial: dialNotCalled(t), run: runNotCalled(t)},
		{name: "refused on a replaced inode", path: "/x/s", lstat: replaced, dial: refused, run: runNotCalled(t)},
		{name: "dial timeout", path: "/x/s", lstat: func(string) (os.FileInfo, error) { return info, nil }, dial: func(context.Context, string) (net.Conn, error) { return nil, context.DeadlineExceeded }, run: runNotCalled(t)},
		{name: "context already ended", ctx: canceled, path: "/x/s", lstat: func(string) (os.FileInfo, error) { return info, nil }, dial: refused, run: runNotCalled(t)},
		{name: "tmux client fails on a live socket", path: live, lstat: os.Lstat, dial: dialUnix, run: func(context.Context, []string) (string, error) {
			return "", errors.New("error connecting to socket (No such file or directory)")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			got, err := probeSessionWith(ctx, tc.path, "s1", tc.lstat, tc.dial, tc.run)
			if got != PresenceUnobservable {
				t.Fatalf("presence = %v, want PresenceUnobservable (err=%v)", got, err)
			}
			if err == nil {
				t.Fatal("err = nil, want the reason nothing was observed")
			}
		})
	}
}

// A socket that refuses connections on a stable inode has no server: the
// session is dead without running tmux.
func TestProbeSessionRefusedStableSocketIsDead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale-socket")
	if err := syscall.Mknod(path, syscall.S_IFSOCK|0o600, 0); err != nil {
		t.Skipf("mknod socket inode: %v", err)
	}
	got, err := probeSessionWith(context.Background(), path, "s1", os.Lstat,
		func(context.Context, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED }, runNotCalled(t))
	if got != PresenceDead || err != nil {
		t.Fatalf("presence = %v, err = %v; want PresenceDead, nil", got, err)
	}
}

// A context that ends while tmux runs is unobservable and wraps ctx.Err().
func TestProbeSessionDeadlineWrapsContextError(t *testing.T) {
	live := liveUnixSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	got, err := probeSessionWith(ctx, live, "s1", os.Lstat, dialUnix, func(context.Context, []string) (string, error) {
		cancel()
		return "", errors.New("signal: killed")
	})
	if got != PresenceUnobservable || !errors.Is(err, context.Canceled) {
		t.Fatalf("presence = %v, err = %v; want PresenceUnobservable wrapping context.Canceled", got, err)
	}
}
