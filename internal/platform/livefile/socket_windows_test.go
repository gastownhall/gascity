//go:build windows

package livefile

import (
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

// bindUnixSocket creates an AF_UNIX socket file at path by binding a socket to
// it without listening, and returns a function that closes the socket; the
// file stays on disk. The socket is also closed when the test ends.
func bindUnixSocket(t *testing.T, path string) func() {
	t.Helper()
	var wsa windows.WSAData
	if err := windows.WSAStartup(uint32(0x202), &wsa); err != nil {
		t.Fatalf("WSAStartup: %v", err)
	}
	t.Cleanup(func() { windows.WSACleanup() }) //nolint:errcheck // test cleanup
	s, err := windows.Socket(windows.AF_UNIX, windows.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("AF_UNIX unavailable: %v", err)
	}
	var once sync.Once
	closeSocket := func() { once.Do(func() { windows.Closesocket(s) }) } //nolint:errcheck // test cleanup
	t.Cleanup(closeSocket)
	if err := windows.Bind(s, &windows.SockaddrUnix{Name: path}); err != nil {
		t.Fatalf("binding a socket at %s: %v", path, err)
	}
	return closeSocket
}
