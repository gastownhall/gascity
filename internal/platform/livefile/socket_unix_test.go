//go:build unix

package livefile

import (
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// bindUnixSocket creates an AF_UNIX socket file at path by binding a socket to
// it without listening, and returns a function that closes the socket; the
// file stays on disk. The socket is also closed when the test ends.
func bindUnixSocket(t *testing.T, path string) func() {
	t.Helper()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("AF_UNIX unavailable: %v", err)
	}
	var once sync.Once
	closeSocket := func() { once.Do(func() { unix.Close(fd) }) } //nolint:errcheck // test cleanup
	t.Cleanup(closeSocket)
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		t.Fatalf("binding a socket at %s: %v", path, err)
	}
	return closeSocket
}
