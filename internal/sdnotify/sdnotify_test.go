package sdnotify

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// listenNotify creates a unixgram listener in a short temp dir (unix
// socket paths are limited to ~108 bytes, so t.TempDir() is too risky)
// and returns the conn plus the socket path.
func listenNotify(t *testing.T) (*net.UnixConn, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sdn-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) }) //nolint:errcheck
	path := filepath.Join(dir, "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listening on %s: %v", path, err)
	}
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck
	return conn, path
}

// readDatagram reads one datagram from conn with a deadline.
func readDatagram(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("reading notify datagram: %v", err)
	}
	return string(buf[:n])
}

func TestNotifyNoopWithoutNotifySocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	sent, err := Notify(Ready)
	if err != nil {
		t.Fatalf("Notify() error = %v, want nil", err)
	}
	if sent {
		t.Fatal("Notify() sent = true, want false when NOTIFY_SOCKET is unset")
	}
}

func TestNotifySendsStateDatagrams(t *testing.T) {
	conn, path := listenNotify(t)
	t.Setenv("NOTIFY_SOCKET", path)
	for _, state := range []string{Ready, Watchdog, Stopping} {
		sent, err := Notify(state)
		if err != nil {
			t.Fatalf("Notify(%q) error = %v", state, err)
		}
		if !sent {
			t.Fatalf("Notify(%q) sent = false, want true", state)
		}
		if got := readDatagram(t, conn); got != state {
			t.Fatalf("datagram = %q, want %q", got, state)
		}
	}
}

func TestNotifyErrorWhenSocketUnreachable(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	sent, err := Notify(Ready)
	if err == nil {
		t.Fatal("Notify() error = nil, want dial failure")
	}
	if sent {
		t.Fatal("Notify() sent = true, want false on dial failure")
	}
}

func TestNotifyAbstractSocket(t *testing.T) {
	name := fmt.Sprintf("@gc-sdnotify-test-%d", os.Getpid())
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: "\x00" + name[1:], Net: "unixgram"})
	if err != nil {
		t.Skipf("abstract unix sockets unavailable: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	t.Setenv("NOTIFY_SOCKET", name)
	sent, err := Notify(Watchdog)
	if err != nil {
		t.Fatalf("Notify() error = %v, want nil", err)
	}
	if !sent {
		t.Fatal("Notify() sent = false, want true")
	}
	if got := readDatagram(t, conn); got != Watchdog {
		t.Fatalf("datagram = %q, want %q", got, Watchdog)
	}
}
