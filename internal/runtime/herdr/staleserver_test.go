package herdr

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// shortHome returns a short temp dir set as $HOME. The default t.TempDir()
// (/var/folders/… on macOS) blows past the 104-byte unix-socket sun_path limit
// once socketPath() appends .config/herdr/sessions/<name>/herdr.sock, so we root
// under /tmp instead.
func shortHome(t *testing.T) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "hdr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
}

// A stale socket inode — left by a herdr server that exited uncleanly — must not
// be mistaken for a live server. This is the regression for the provider-swap
// failure where startServer() no-op'd on a dead socket (an os.Stat file-presence
// check), so the very next op (`herdr agent list`) hit ECONNREFUSED, the tmux→herdr
// swap aborted, and pool polecats hung in start-pending / agent_not_found.
func TestServerAliveRejectsStaleSocket(t *testing.T) {
	shortHome(t)

	c := newClient("staletest", "")
	sock := c.socketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a real socket inode, then drop the listener while KEEPING the file —
	// exactly the state an uncleanly-exited server leaves behind.
	addr, err := net.ResolveUnixAddr("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()

	// Precondition: the inode is present and is a socket — what the OLD
	// file-presence check keyed on. It would wrongly report "running".
	fi, err := os.Stat(sock)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("test setup: expected a stale socket at %s (err=%v)", sock, err)
	}

	// The fix: liveness is decided by dialing, so a stale socket reads as dead.
	if c.serverAlive() {
		t.Fatal("serverAlive() = true for a stale (unlistened) socket; want false")
	}

	// And the stale inode must be removable so a fresh server can bind.
	c.removeStaleSocket()
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("removeStaleSocket() left the inode behind: err=%v", err)
	}
}

// A live server must read as alive (guards against the fix over-correcting into
// "always restart").
func TestServerAliveDetectsLiveServer(t *testing.T) {
	shortHome(t)

	c := newClient("livetest", "")
	sock := c.socketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	addr, err := net.ResolveUnixAddr("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if !c.serverAlive() {
		t.Fatal("serverAlive() = false while a listener is accepting; want true")
	}
}
