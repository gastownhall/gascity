// Package localsocket owns the transport for Gas City's own local control
// plane: the supervisor control socket, the per-city controller socket, and
// the supervisor nudge-dispatcher wake socket.
//
// # Transport
//
// All three endpoints speak AF_UNIX on every supported platform, Windows
// included. Windows has carried AF_UNIX since Windows 10 1803 and Go binds and
// dials it through the same net.Listen("unix", …) / net.DialTimeout("unix", …)
// calls, so these endpoints need no second transport. probe_test.go holds that
// to evidence rather than assertion: it binds, dials, echoes, rebinds over a
// stale socket and measures the pathname ceiling on whatever machine runs it,
// so a platform where the claim stops being true fails loudly instead of
// degrading quietly.
//
// herdr is a separate question and deliberately out of scope. herdr chose
// named pipes for its own IPC on Windows, so internal/runtime/herdr needs a
// named-pipe client whatever this package concludes about Gas City's sockets.
//
// # Why a package rather than net.Listen at each call site
//
// Two things vary by platform and neither is visible where an endpoint is
// opened: the longest pathname the kernel accepts, and where a short fallback
// pathname may live when the canonical one is too long. Callers used to hold
// both locally — a private const per package and a literal "/tmp" join — and
// "/tmp" is not a temporary directory on Windows. filepath.Join turns it into a
// drive-relative \tmp on whatever volume the process is running from, outside
// every runtime directory Gas City owns and creates. Keeping the pair here puts
// the transport decision in one platform-split package instead of a GOOS branch
// at each endpoint.
package localsocket

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// PathLimit is the longest endpoint pathname callers may use.
//
// It is a policy ceiling rather than the kernel's: sockaddr_un carries 108
// bytes on Linux, 104 on macOS and 108 on Windows, each including the
// terminating NUL, and a pathname that fits none of them fails at bind with an
// errno that names neither the limit nor the path. Staying under the smallest
// of them on every platform keeps a pathname that works on one developer's
// machine working on the others.
const PathLimit = pathLimit

// PathTooLongError reports a pathname that cannot be bound or dialed on this
// platform. The kernel's own answer is EINVAL ("bind: invalid argument"), which
// says nothing about which limit was hit, so callers that resolve their own
// endpoint paths get told the number they exceeded and by how much.
type PathTooLongError struct {
	Op    string
	Path  string
	Limit int
}

func (e *PathTooLongError) Error() string {
	return fmt.Sprintf("%s local socket %q: pathname is %d bytes, over the %d-byte limit on this platform",
		e.Op, e.Path, len(e.Path), e.Limit)
}

// Options describes how an endpoint's parent directory and the bound socket
// should be protected.
//
// The two fields are separate because the endpoints differ in which one
// carries the privacy guarantee: the supervisor and controller sockets rely on
// a directory Gas City created, while the nudge wake socket historically sat in
// a group-readable directory and was narrowed individually after bind.
type Options struct {
	// DirMode is the POSIX mode applied when the parent directory has to be
	// created. It is meaningless on Windows, where access is a DACL.
	DirMode os.FileMode

	// SocketMode, when non-zero, requests that the bound socket be reachable
	// only by its owner. On Unix that is a chmod of the socket. On Windows,
	// where mode bits are not an access-control boundary, the parent directory
	// is instead created and verified as owner-only private state and the
	// socket inherits it — see prepareDir in localsocket_windows.go.
	SocketMode os.FileMode
}

// Listen binds a local control-plane listener at path.
//
// A socket file left behind by a crashed owner is removed first. That is safe
// for the same reason it was safe before this package existed: every endpoint
// that matters is guarded by a separate exclusive file lock, so a stale socket
// is a leftover name and never evidence that an owner is alive.
func Listen(path string, opts Options) (net.Listener, error) {
	if err := checkPath("listening on", path); err != nil {
		return nil, err
	}
	if err := prepareDir(filepath.Dir(path), opts); err != nil {
		return nil, err
	}
	// A stale socket from a previous crash blocks bind with "address already
	// in use" on every platform this runs on.
	os.Remove(path) //nolint:errcheck // stale socket cleanup; bind reports a real conflict
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := protectSocket(path, opts); err != nil {
		lis.Close() //nolint:errcheck // returning the protection failure, not the close failure
		return nil, err
	}
	return lis, nil
}

// Dial connects to a local control-plane endpoint.
func Dial(path string, timeout time.Duration) (net.Conn, error) {
	if err := checkPath("dialing", path); err != nil {
		return nil, err
	}
	return net.DialTimeout("unix", path, timeout)
}

// FallbackDir returns this platform's short private directory for hashed
// fallback endpoint paths in namespace.
//
// Endpoints prefer a canonical path next to the state they belong to and fall
// back to a hashed short path only when the canonical one would exceed
// PathLimit. namespace separates one endpoint family's fallbacks from another's
// (for example "gascity-controller" and "gascity-supervisor").
func FallbackDir(namespace string) string {
	return filepath.Join(fallbackRoot(), namespace)
}

func checkPath(op, path string) error {
	if len(path) > PathLimit {
		return &PathTooLongError{Op: op, Path: path, Limit: PathLimit}
	}
	return nil
}
