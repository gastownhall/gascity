//go:build darwin

package proctable

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ProcessEnvValue returns the value of key in pid's environment, or "" when
// the process is gone, its environment is unreadable, or key is absent. It
// reads the environment the way [ScanBySessionID] does and refuses the live
// process table under go test for the same reason.
//
// Darwin has no /proc: the environment comes from "ps eww -o command=", which
// prints the command, its arguments and its environment as one
// whitespace-separated line. Nothing in that output delimits the three, so a
// value containing whitespace truncates at the first space and a
// key=value-shaped argument parses as an environment entry. Duplicate keys
// resolve last-wins, matching the linux reader, so a real environment entry
// still outranks a same-named argument token.
//
// The truncation belongs to that output, not to this function:
// [ScanBySessionID] fills each record's environment from the same flattened
// line, so reading a value off the scan record instead would save the second
// ps fork but truncate identically. Recovering such a value needs a source
// that keeps the NUL delimiters — the kern.procargs2 sysctl ps itself reads.
// Until then a gc state directory under a path containing spaces degrades
// darwin attribution: a lookup of an absolute path (acp's
// GC_ACP_CONTROL_SOCKET marker) returns a prefix that matches nothing, which
// is the same outcome as an absent key. For the acp scanner that is its
// documented markerless case — the root is tracked only by the reading gc
// process's own live connections — so an owner keeps its running sessions,
// but another gc process reads that owner's live agent as reapable, which is
// the case the marker was added to cover.
func ProcessEnvValue(pid int, key string) (string, error) {
	if err := liveScanGuard(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), processSnapshotTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "eww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// ps exits non-zero when the pid does not exist.
			return "", nil
		}
		return "", fmt.Errorf("running ps for pid %d: %w", pid, err)
	}
	return parseInlineEnv(strings.Fields(string(out)))[key], nil
}
