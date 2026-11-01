//go:build darwin

package proctable

import (
	"errors"

	"github.com/gastownhall/gascity/internal/pidutil"
	"golang.org/x/sys/unix"
)

// ProcessEnvValue returns the value of key in pid's environment, or "" when
// the process is gone, its environment is not this user's to read, or key is
// absent. It refuses the live process table under go test, as
// [ScanBySessionID] does.
//
// Darwin has no /proc: the environment comes from the kern.procargs2 sysctl
// that ps(1) reads (pidutil.Environ), which keeps each entry whole, so a value
// containing whitespace is intact: a TMUX_TMPDIR, or a gc state directory
// under a path with spaces, reads as it is set. The kernel answers EINVAL for
// a pid that does not exist and for one another user owns, and withholds the
// environment of a restricted process (an Apple platform binary): each reads
// as "", as each did in ps's output. Duplicate keys resolve last-wins,
// matching the linux reader.
//
// [ScanBySessionID] still reads each process's environment off its flattened
// "ps eww" line, where a value containing whitespace truncates at its first
// space; a caller that needs such a value whole reads it here.
func ProcessEnvValue(pid int, key string) (string, error) {
	if err := liveScanGuard(); err != nil {
		return "", err
	}
	env, err := pidutil.Environ(pid)
	if errors.Is(err, unix.EINVAL) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return parseInlineEnv(env)[key], nil
}
