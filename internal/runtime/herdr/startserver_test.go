package herdr

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// startServer must unlink a socket and launch a server only for a server it
// has confirmed dead. A dial that merely fails (a full accept backlog, an
// unreadable listener table) says nothing about a live server, and unlinking
// its socket then launching a second server makes pane ids collide across the
// two (workspace ids restart at w1 per server).

// startServerFixture is a client wired to a fake herdr CLI under an isolated
// XDG_CONFIG_HOME, with the session's socket directory created.
type startServerFixture struct {
	c    *client
	fake *fakeHerdr
	sock string
}

func newStartServerFixture(t *testing.T, session string) *startServerFixture {
	t.Helper()
	shortHome(t)
	c := newClient(session, "")
	f := &startServerFixture{c: c, fake: newFakeHerdr(t, filepath.Join(t.TempDir(), "unused-probe")), sock: c.socketPath()}
	c.bin = f.fake.bin
	if err := os.MkdirAll(filepath.Dir(f.sock), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// launched reports whether startServer ran `herdr server`.
func (f *startServerFixture) launched(t *testing.T) bool {
	t.Helper()
	return logIndex(f.fake.logLines(t), "server") >= 0
}

// requireSocketFile fails the test unless a socket inode still sits at the
// session's socket path.
func (f *startServerFixture) requireSocketFile(t *testing.T) {
	t.Helper()
	if fi, err := os.Stat(f.sock); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket %s was unlinked (stat err=%v); a live server's socket must stay", f.sock, err)
	}
}

// listenBusy binds a unix listener at sock whose accept queue is full, so a
// dial of it fails (EAGAIN on Linux, a refusal on macOS) while the server is
// perfectly alive. The returned func makes it a server that accepts again.
func listenBusy(t *testing.T, sock string) (accepting func()) {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: sock}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	var queued []net.Conn
	for i := 0; ; i++ {
		conn, err := net.DialTimeout("unix", sock, 100*time.Millisecond)
		if err != nil {
			break
		}
		queued = append(queued, conn)
		if i > 64 {
			t.Fatal("test setup: the listener's accept queue never filled")
		}
	}
	done := make(chan struct{})
	var loop sync.WaitGroup
	t.Cleanup(func() {
		close(done)
		loop.Wait()
		for _, conn := range queued {
			_ = conn.Close()
		}
		_ = syscall.Close(fd)
	})
	return func() {
		loop.Add(1)
		go func() {
			defer loop.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if nfd, _, err := syscall.Accept(fd); err == nil {
					_ = syscall.Close(nfd)
					continue
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
}

// A live server whose dial keeps failing is not confirmed dead: startServer
// keeps its socket, launches nothing, and says why.
func TestStartServerKeepsALiveServersSocketWhenTheDialKeepsFailing(t *testing.T) {
	f := newStartServerFixture(t, "busy")
	listenBusy(t, f.sock)
	if _, err := net.DialTimeout("unix", f.sock, 100*time.Millisecond); err == nil {
		t.Fatal("test setup: the dial of the busy listener succeeded; want it to fail")
	}

	err := f.c.startServer()

	if err == nil {
		t.Fatal("startServer() = nil with no accepting server and none confirmed dead; want an error")
	} else if !strings.Contains(err.Error(), f.sock) {
		t.Errorf("startServer() error %q does not name the socket %q", err, f.sock)
	}
	f.requireSocketFile(t)
	if f.launched(t) {
		t.Error("startServer() launched a second server beside a live one")
	}
}

// A dial that fails once, then succeeds, is a busy server and not a dead one:
// startServer retries, finds it accepting, and leaves it alone.
func TestStartServerRetriesAFailedDialOfALiveServer(t *testing.T) {
	f := newStartServerFixture(t, "retry")
	accepting := listenBusy(t, f.sock)
	time.AfterFunc(150*time.Millisecond, accepting)

	if err := f.c.startServer(); err != nil {
		t.Fatalf("startServer() = %v, want nil once the live server accepts again", err)
	}
	f.requireSocketFile(t)
	if f.launched(t) {
		t.Error("startServer() launched a second server beside a live one")
	}
}

// With the kernel's listener table unreadable, a refused dial cannot tell a
// dead server from a live one whose socket file survives: nothing is unlinked
// and nothing is launched.
func TestStartServerLeavesTheSocketAloneWhenTheListenerTableIsUnreadable(t *testing.T) {
	f := newStartServerFixture(t, "table")
	addr, err := net.ResolveUnixAddr("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close() // the refusing socket file an uncleanly exited server leaves
	f.c.unixListening = func(string) (bool, error) { return false, os.ErrPermission }

	if err := f.c.startServer(); err == nil {
		t.Error("startServer() = nil with an unreadable listener table; want an error")
	}
	f.requireSocketFile(t)
	if f.launched(t) {
		t.Error("startServer() launched a server it could not show was needed")
	}
}

// A socket whose server is dead (nothing bound) is still cleared so the new
// server can bind: the stranded provider swap of #4342 stays fixed. The test's
// "server" binds the path only once the fake herdr has been asked to launch,
// which fails with EADDRINUSE if the stale inode is still there.
func TestStartServerClearsADeadServersSocketAndLaunches(t *testing.T) {
	f := newStartServerFixture(t, "dead")
	addr, err := net.ResolveUnixAddr("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	_ = stale.Close()
	if fi, err := os.Stat(f.sock); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("test setup: expected a stale socket at %s (err=%v)", f.sock, err)
	}

	bound := make(chan error, 1)
	go func() {
		for {
			if b, err := os.ReadFile(f.fake.log); err == nil && strings.Contains(string(b), "server") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		ln, err := net.ListenUnix("unix", addr)
		if err == nil {
			t.Cleanup(func() { _ = ln.Close() })
		}
		bound <- err
	}()

	if err := f.c.startServer(); err != nil {
		t.Fatalf("startServer() = %v, want nil once the launched server binds", err)
	}
	if err := <-bound; err != nil {
		t.Fatalf("the launched server could not bind: %v (the stale socket inode was not cleared)", err)
	}
}
