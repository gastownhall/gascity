package tmux

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

// bindProbeSocket binds a unix listener at path, standing in for a live tmux
// server socket: the probe dials it to attest a peer pid but never speaks the
// tmux protocol, so no accept loop is needed.
func bindProbeSocket(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind probe socket %s: %v", path, err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(path)
	})
}

// probeSocketFixtureInfo returns a FileInfo whose mode is a unix socket, for
// the lstat-injecting rows that never bind a listener.
func probeSocketFixtureInfo(t *testing.T, name string) os.FileInfo {
	t.Helper()
	fixture := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(fixture, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("write socket fixture: %v", err)
	}
	info, err := os.Lstat(fixture)
	if err != nil {
		t.Fatalf("lstat socket fixture: %v", err)
	}
	return socketModeFileInfo{FileInfo: info}
}

// TestProbeSessionWithClassifiesSocketBeforeAskingTmux pins the probe's
// fail-closed core: only a live socket with an attested peer gets to be asked
// (the exec injection), a stable refused dial is the one attested death, and
// every other observation — absent, unstable, unreadable, or a dead context —
// is unobservable, never dead.
func TestProbeSessionWithClassifiesSocketBeforeAskingTmux(t *testing.T) {
	const session = "soak-loop"
	neverExec := func(context.Context, []string) (string, error) {
		t.Fatal("exec ran for an observation that never attested a live socket")
		return "", errors.New("unreachable")
	}

	t.Run("absent socket is unobservable, never dead", func(t *testing.T) {
		presence, err := probeSessionWith(context.Background(), "gascity", session, func(string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		}, func(context.Context, string) (net.Conn, error) {
			return nil, os.ErrNotExist
		}, neverExec)
		if presence != PresenceUnobservable {
			t.Fatalf("presence = %v, want PresenceUnobservable", presence)
		}
		if err == nil {
			t.Fatal("err = nil, want an unobservable diagnostic")
		}
	})

	t.Run("unreadable socket is unobservable", func(t *testing.T) {
		presence, err := probeSessionWith(context.Background(), "gascity", session, func(string) (os.FileInfo, error) {
			return nil, os.ErrPermission
		}, func(context.Context, string) (net.Conn, error) {
			return nil, os.ErrNotExist
		}, neverExec)
		if presence != PresenceUnobservable || err == nil {
			t.Fatalf("presence = %v err = %v, want unobservable with a diagnostic", presence, err)
		}
	})

	t.Run("stable refused dial is the attested dead", func(t *testing.T) {
		// A socket file nothing listens on dials ECONNREFUSED and keeps its
		// inode across the re-lstat: the attested death. The fixture must be
		// a real socket file and a real lstat — os.SameFile compares only
		// raw *os.fileStat values, which is also why the production path
		// lstats instead of reusing a wrapped FileInfo.
		dir := t.TempDir()
		t.Setenv("TMUX_TMPDIR", dir)
		path := SocketPath("gascity")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create socket dir: %v", err)
		}
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatalf("bind stale socket %s: %v", path, err)
		}
		if ul, ok := ln.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(false)
		}
		_ = ln.Close() // the socket file stays; nothing answers its dial
		t.Cleanup(func() { _ = os.Remove(path) })
		presence, err := probeSessionWith(context.Background(), "gascity", session, os.Lstat, probeSessionDial, neverExec)
		if presence != PresenceDead || err != nil {
			t.Fatalf("presence = %v err = %v, want attested dead", presence, err)
		}
	})

	t.Run("refused dial with the path gone is the attested dead", func(t *testing.T) {
		info := probeSocketFixtureInfo(t, "stale-gone")
		calls := 0
		presence, _ := probeSessionWith(context.Background(), "gascity", session, func(string) (os.FileInfo, error) {
			calls++
			if calls == 1 {
				return info, nil
			}
			return nil, os.ErrNotExist
		}, func(context.Context, string) (net.Conn, error) {
			return nil, syscall.ECONNREFUSED
		}, neverExec)
		if presence != PresenceDead {
			t.Fatalf("presence = %v, want attested dead", presence)
		}
	})

	t.Run("unstable inode is unobservable, never dead", func(t *testing.T) {
		first := probeSocketFixtureInfo(t, "first")
		second := probeSocketFixtureInfo(t, "second")
		calls := 0
		presence, err := probeSessionWith(context.Background(), "gascity", session, func(string) (os.FileInfo, error) {
			calls++
			if calls == 1 {
				return first, nil
			}
			return second, nil
		}, func(context.Context, string) (net.Conn, error) {
			return nil, syscall.ECONNREFUSED
		}, neverExec)
		if presence != PresenceUnobservable || err == nil {
			t.Fatalf("presence = %v err = %v, want unobservable for an unstable inode", presence, err)
		}
	})

	t.Run("socket vanishing mid-dial is unobservable, never dead", func(t *testing.T) {
		info := probeSocketFixtureInfo(t, "vanishing")
		calls := 0
		presence, _ := probeSessionWith(context.Background(), "gascity", session, func(string) (os.FileInfo, error) {
			calls++
			if calls == 1 {
				return info, nil
			}
			return nil, os.ErrNotExist
		}, func(context.Context, string) (net.Conn, error) {
			return nil, os.ErrNotExist
		}, neverExec)
		if presence != PresenceUnobservable {
			t.Fatalf("presence = %v, want unobservable for a socket that raced away", presence)
		}
	})

	t.Run("canceled context is unobservable", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		presence, err := probeSessionWith(ctx, "gascity", session, os.Lstat, probeSessionDial, neverExec)
		if presence != PresenceUnobservable || !errors.Is(err, context.Canceled) {
			t.Fatalf("presence = %v err = %v, want unobservable wrapping context.Canceled", presence, err)
		}
	})
}

// TestProbeSessionWithAsksLiveServerForExactSession pins the live-server half:
// the probe addresses the server by its resolved SocketPath through -S, and
// classifies alive only on an exact session-name line.
func TestProbeSessionWithAsksLiveServerForExactSession(t *testing.T) {
	socketName := "gascity"
	path := SocketPath(socketName)
	bindProbeSocket(t, path)

	t.Run("hosts the session", func(t *testing.T) {
		var gotArgs []string
		presence, err := probeSessionWith(context.Background(), socketName, "soak-loop", os.Lstat, probeSessionDial,
			func(_ context.Context, args []string) (string, error) {
				gotArgs = args
				return "other\nsoak-loop\n", nil
			})
		if presence != PresenceAlive || err != nil {
			t.Fatalf("presence = %v err = %v, want alive", presence, err)
		}
		wantArgs := []string{"-u", "-S", path, "list-sessions", "-F", "#{session_name}"}
		if !reflect.DeepEqual(gotArgs, wantArgs) {
			t.Fatalf("tmux args = %v, want %v", gotArgs, wantArgs)
		}
	})

	t.Run("does not host the session", func(t *testing.T) {
		presence, err := probeSessionWith(context.Background(), socketName, "soak-loop", os.Lstat, probeSessionDial,
			func(context.Context, []string) (string, error) {
				return "soak-loop-2\n", nil
			})
		if presence != PresenceDead || err != nil {
			t.Fatalf("presence = %v err = %v, want dead: the server answered without the session", presence, err)
		}
	})

	t.Run("exec failure is unobservable, never dead", func(t *testing.T) {
		presence, err := probeSessionWith(context.Background(), socketName, "soak-loop", os.Lstat, probeSessionDial,
			func(context.Context, []string) (string, error) {
				return "", errors.New("error connecting to socket (No such file or directory)")
			})
		if presence != PresenceUnobservable || err == nil {
			t.Fatalf("presence = %v err = %v, want unobservable for a failed ask", presence, err)
		}
	})

	t.Run("expired bound is unobservable wrapping the deadline", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		presence, err := probeSessionWith(ctx, socketName, "soak-loop", os.Lstat, probeSessionDial,
			func(context.Context, []string) (string, error) {
				return "", ctx.Err()
			})
		if presence != PresenceUnobservable || !errors.Is(err, context.Canceled) {
			t.Fatalf("presence = %v err = %v, want unobservable wrapping the context error", presence, err)
		}
	})

	t.Run("absolute socket reference asks the cleaned path", func(t *testing.T) {
		absPath := filepath.Join(t.TempDir(), "nested", "probe-socket")
		if err := os.MkdirAll(filepath.Dir(absPath), 0o700); err != nil {
			t.Fatalf("create socket dir: %v", err)
		}
		bindProbeSocket(t, absPath)
		var gotArgs []string
		presence, err := probeSessionWith(context.Background(), absPath+"/", "soak-loop", os.Lstat, probeSessionDial,
			func(_ context.Context, args []string) (string, error) {
				gotArgs = args
				return "soak-loop\n", nil
			})
		if presence != PresenceAlive || err != nil {
			t.Fatalf("presence = %v err = %v, want alive through the absolute reference", presence, err)
		}
		if len(gotArgs) < 3 || gotArgs[1] != "-S" || gotArgs[2] != absPath {
			t.Fatalf("tmux args = %v, want -S %q", gotArgs, absPath)
		}
	})
}

// TestProbeSessionProductionReachesAFakeTmux runs ProbeSession's full
// production wiring against a fake tmux binary on PATH: a live socket at the
// canonical path plus an answering client yields alive; a client that cannot
// answer yields unobservable.
func TestProbeSessionProductionReachesAFakeTmux(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not present")
	}
	socketName := "gascity"
	path := SocketPath(socketName)
	bindProbeSocket(t, path)

	dir := t.TempDir()
	script := filepath.Join(dir, "tmux")
	body := "#!/bin/sh\nprintf 'soak-loop\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	presence, err := ProbeSession(context.Background(), socketName, "soak-loop")
	if presence != PresenceAlive || err != nil {
		t.Fatalf("ProbeSession = %v, %v; want alive, nil", presence, err)
	}
}
