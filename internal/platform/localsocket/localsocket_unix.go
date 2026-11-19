//go:build !windows

package localsocket

import (
	"fmt"
	"os"
)

// pathLimit stays below the smallest sockaddr_un on the platforms Gas City
// supports — 104 bytes on macOS, 108 on Linux, each including the terminating
// NUL — with room for the hashed fallback names built on top of it.
const pathLimit = 100

// fallbackRoot is the shared temporary directory. It is deliberately the
// literal "/tmp" rather than os.TempDir(): TMPDIR on macOS resolves to a
// per-user path under /var/folders that is itself long enough to defeat the
// pathname budget the fallback exists to satisfy.
func fallbackRoot() string { return "/tmp" }

func prepareDir(dir string, opts Options) error {
	mode := opts.DirMode
	if mode == 0 {
		mode = 0o700
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("creating local socket dir %s: %w", dir, err)
	}
	return nil
}

func protectSocket(path string, opts Options) error {
	if opts.SocketMode == 0 {
		return nil
	}
	// There is a narrow window between bind and chmod in which the socket
	// exists at the umask default. Closing it would need umask manipulation,
	// which is process-global; the callers that ask for SocketMode carry no
	// payload worth the trade and document that.
	if err := os.Chmod(path, opts.SocketMode); err != nil {
		return fmt.Errorf("restricting local socket %s: %w", path, err)
	}
	return nil
}
