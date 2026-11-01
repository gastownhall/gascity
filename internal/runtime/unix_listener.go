package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
)

// UnixPathListening reports whether the kernel lists a unix socket bound to
// path (or to path under its resolved ancestors). Both tables keep a bound
// path after the file is unlinked: /proc/net/unix on Linux, where it counts
// only listeners, and lsof's bound addresses on macOS. Any other platform has
// no table and errors. A provider that found its server's socket missing or
// refusing asks it before confirming that server dead: a listed listener is a
// live server whose socket file was unlinked, and an error must count as one,
// so such a server is never confirmed dead.
func UnixPathListening(path string) (bool, error) {
	paths := unixSocketPathCandidates(path)
	var listed func(path string) bool
	switch goruntime.GOOS {
	case "linux":
		table, err := os.ReadFile("/proc/net/unix")
		if err != nil {
			return false, err
		}
		listed = func(path string) bool { return procNetUnixListening(string(table), path) }
	case "darwin":
		for _, candidate := range paths {
			if !lsofPrintsVerbatim(candidate) {
				return false, fmt.Errorf("socket path %q has bytes lsof escapes", candidate)
			}
		}
		out, err := lsofUnixSockets()
		if err != nil {
			return false, err
		}
		listed = func(path string) bool { return lsofUnixBound(out, path) }
	default:
		return false, fmt.Errorf("no unix socket table on %s", goruntime.GOOS)
	}
	for _, candidate := range paths {
		if listed(candidate) {
			return true, nil
		}
	}
	return false, nil
}

// unixSocketPathCandidates returns path and the same path rejoined under its
// longest existing ancestor with symlinks resolved. A server may bind its
// socket under the resolved directory (tmux binds under /private/tmp for /tmp
// on macOS), and a removed socket directory still has a resolvable parent.
func unixSocketPathCandidates(path string) []string {
	paths := []string{path}
	dir, tail := filepath.Dir(path), filepath.Base(path)
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return append(paths, filepath.Join(resolved, tail))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return paths
		}
		dir, tail = parent, filepath.Join(filepath.Base(dir), tail)
	}
}

// procNetUnixListening reports whether a /proc/net/unix table ("Num RefCount
// Protocol Flags Type St Inode Path") holds a socket bound to path with
// __SO_ACCEPTCON (0x10000) in Flags, the mark of a listener.
func procNetUnixListening(table, path string) bool {
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || strings.Join(fields[7:], " ") != path {
			continue
		}
		if flags, err := strconv.ParseUint(fields[3], 16, 64); err == nil && flags&0x10000 != 0 {
			return true
		}
	}
	return false
}

// lsofUnixSocketsTimeout bounds lsof's scan of every process it can read.
const lsofUnixSocketsTimeout = 10 * time.Second

// lsofUnixSockets returns lsof's field output ("n<name>" lines) for every
// unix socket it can read: the caller's own processes, which include any
// server on the caller's sockets.
func lsofUnixSockets() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lsofUnixSocketsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-w", "-U", "-Fn")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return lsofAnswer(out, stderr.String(), err)
}

// lsofAnswer reads an lsof run. With no search argument lsof exits 0 even
// when it selects nothing, so any failure, a timeout's kill included, is an
// error.
func lsofAnswer(out []byte, stderr string, err error) (string, error) {
	if err != nil {
		return "", fmt.Errorf("lsof -U: %w: %s", err, strings.TrimSpace(stderr))
	}
	return string(out), nil
}

// lsofPrintsVerbatim reports whether lsof prints path unchanged: it escapes
// a backslash, control bytes and bytes from 0x80, so such a path would never
// match its own listing.
func lsofPrintsVerbatim(path string) bool {
	for i := 0; i < len(path); i++ {
		if c := path[i]; c == '\\' || c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// lsofUnixBound reports whether lsof field output names a unix socket bound
// to path. On macOS the name is the address the kernel holds for the socket,
// not a file lookup: a server's listener and its accepted connections keep
// it after the file is unlinked, while a client's socket names its peer
// ("->0x...") instead.
func lsofUnixBound(out, path string) bool {
	for _, line := range strings.Split(out, "\n") {
		if line == "n"+path {
			return true
		}
	}
	return false
}
