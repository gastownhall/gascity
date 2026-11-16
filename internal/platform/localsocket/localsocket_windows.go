//go:build windows

package localsocket

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"

	"github.com/gastownhall/gascity/internal/runtime"
	"golang.org/x/sys/windows"
)

// pathLimit matches the Unix ceiling. Windows SOCKADDR_UN carries 108 bytes
// including the terminating NUL, measured by TestProbeExactPathLimit as a hard
// cut at 107; holding the same 100-byte policy budget everywhere means a
// pathname that binds on one developer's machine binds on the rest.
const pathLimit = 100

// fallbackRoot is the user's temporary directory. "/tmp" cannot be used here:
// filepath.Join would make it drive-relative, so the fallback would land in
// \tmp on whichever volume the process is running from — a directory Gas City
// never creates, never protects, and may not even be able to write.
func fallbackRoot() string { return os.TempDir() }

// prepareDir creates the endpoint's parent directory.
//
// When the caller asked for an owner-only socket, the directory is created and
// verified as private state instead. Mode bits are not an access boundary on
// Windows, so the equivalent of the Unix chmod has to be applied one level up:
// EnsurePrivateDir writes an owner-only DACL carrying OI|CI, which every entry
// created inside afterwards inherits, including the socket.
//
// EnsurePrivateDir also fails closed on a foreign owner or a reparse point, so
// an attacker who wins the race to pre-create a predictable endpoint directory
// cannot have the socket placed inside a directory they control.
func prepareDir(dir string, opts Options) error {
	if opts.SocketMode != 0 {
		if err := runtime.EnsurePrivateDir(dir); err != nil {
			return fmt.Errorf("preparing private local socket dir %s: %w", dir, err)
		}
		return nil
	}
	mode := opts.DirMode
	if mode == 0 {
		mode = 0o700
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("creating local socket dir %s: %w", dir, err)
	}
	return nil
}

// protectSocket is satisfied before the bind rather than after it.
//
// os.Chmod on Windows toggles the read-only attribute and nothing else, so
// calling it on a socket would either fail or report success while granting
// exactly the access it was asked to revoke. The owner-only DACL prepareDir
// applied to the parent directory is the real boundary and the socket inherits
// it at creation, which is strictly earlier than a post-bind chmod could act.
func protectSocket(_ string, _ Options) error { return nil }

const sioAFUnixGetPeerPID = 0x58000100

type syscallConn interface {
	SyscallConn() (syscall.RawConn, error)
}

// PeerPID returns the process ID at the other end of a connected Windows
// AF_UNIX socket.
func PeerPID(conn net.Conn) (int, error) {
	rawer, ok := conn.(syscallConn)
	if !ok {
		return 0, fmt.Errorf("local socket peer PID: %T does not expose a syscall connection", conn)
	}
	raw, err := rawer.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("local socket peer PID: %w", err)
	}

	var pid uint32
	var bytesReturned uint32
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		ioctlErr = windows.WSAIoctl(
			windows.Handle(fd),
			sioAFUnixGetPeerPID,
			nil,
			0,
			(*byte)(unsafe.Pointer(&pid)),
			uint32(unsafe.Sizeof(pid)),
			&bytesReturned,
			nil,
			0)
	}); err != nil {
		return 0, fmt.Errorf("local socket peer PID: %w", err)
	}
	if ioctlErr != nil {
		return 0, fmt.Errorf("local socket peer PID: %w", ioctlErr)
	}
	if pid == 0 {
		return 0, fmt.Errorf("local socket peer PID: ioctl returned PID 0 (%d bytes)", bytesReturned)
	}
	return int(pid), nil
}
