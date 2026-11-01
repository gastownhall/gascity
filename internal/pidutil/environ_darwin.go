//go:build darwin

package pidutil

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Environ returns pid's environment as it stood when the process started, one
// KEY=VALUE entry per string, read from the kern.procargs2 sysctl that ps(1)
// reads: each entry stays whole, so a value containing whitespace is intact.
// The kernel answers EINVAL for a pid that does not exist and for one another
// user owns, and withholds the environment of a restricted process (an Apple
// platform binary) from every other process, which reads as none.
func Environ(pid int) ([]string, error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("pidutil: reading the environment of PID %d via kern.procargs2: %w", pid, err)
	}
	env, err := parseProcArgs2Environ(data)
	if err != nil {
		return nil, fmt.Errorf("pidutil: parsing the environment of PID %d: %w", pid, err)
	}
	return env, nil
}
