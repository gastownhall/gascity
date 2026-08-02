//go:build darwin

package pidutil

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

// platformCmdline reads a PID's argv on macOS via the kern.procargs2 sysctl —
// the darwin equivalent of /proc/<pid>/cmdline. The raw buffer layout is:
// int32 argc, exec_path NUL-terminated, NUL padding, then argc NUL-terminated
// argv strings. Reads are limited to same-UID processes; a refused read
// surfaces as an error and callers decide how to degrade.
func platformCmdline(pid int) (argv []string, err error, handled bool) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("kern.procargs2 for pid %d: %w", pid, err), true
	}
	if len(raw) < 4 {
		return nil, fmt.Errorf("kern.procargs2 for pid %d: short read (%d bytes)", pid, len(raw)), true
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	if argc <= 0 {
		return nil, fmt.Errorf("kern.procargs2 for pid %d: argc %d", pid, argc), true
	}
	rest := raw[4:]
	// Skip exec_path and its NUL padding to reach argv[0].
	execEnd := bytes.IndexByte(rest, 0)
	if execEnd < 0 {
		return nil, fmt.Errorf("kern.procargs2 for pid %d: unterminated exec path", pid), true
	}
	rest = rest[execEnd:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	out := make([]string, 0, argc)
	for len(out) < argc && len(rest) > 0 {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			out = append(out, string(rest))
			rest = nil
			break
		}
		out = append(out, string(rest[:end]))
		rest = rest[end+1:]
	}
	if len(out) < argc {
		return nil, fmt.Errorf("kern.procargs2 for pid %d: truncated argv (%d of %d)", pid, len(out), argc), true
	}
	return NormalizeArgv(out), nil, true
}
