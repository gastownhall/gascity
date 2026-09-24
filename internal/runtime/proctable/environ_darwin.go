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
