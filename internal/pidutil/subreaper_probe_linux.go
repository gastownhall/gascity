//go:build linux

package pidutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// probeTimeout bounds the probe shell, which exits as soon as it has started
// its background child.
const probeTimeout = 10 * time.Second

// probeScript starts a background child that blocks reading fd 3 and prints
// its pid; the shell then exits, orphaning the child. The child detaches its
// stdio with exec, which keeps no copy of the old descriptors (a redirection
// on the read builtin would save the shell's stdout aside for the builtin's
// duration), so it holds none of the pipes the shell's output is read
// through.
const probeScript = `{ exec 0<&3 1>/dev/null 2>&1 3<&-; read -r x; } & echo $!`

// probeOrphanAdopter orphans a probe process and returns the pid the kernel
// reparented it to. The probe blocks on a pipe whose only write end this
// process holds, so it is alive while its parent is read, and it exits when
// that end closes: on return, or with this process. Its environment is
// empty, so it carries no session marker a concurrent orphan sweep could
// mistake for an agent's.
func probeOrphanAdopter() (int, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return 0, fmt.Errorf("creating the probe pipe: %w", err)
	}
	defer w.Close() //nolint:errcheck // closing the write end releases the probe

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", probeScript)
	cmd.Env = []string{}
	cmd.ExtraFiles = []*os.File{r}
	out, runErr := cmd.Output()
	if err := r.Close(); err != nil && runErr == nil {
		runErr = err
	}
	if runErr != nil {
		return 0, fmt.Errorf("running the probe shell: %w", runErr)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("probe shell reported pid %q", out)
	}
	// The kernel reparents the probe as its shell exits, before the shell can
	// be reaped, so the parent read here is final.
	adopter, err := ParentPID(pid)
	if err != nil {
		return 0, fmt.Errorf("reading the parent of probe %d: %w", pid, err)
	}
	if adopter == os.Getpid() {
		// This process is a subreaper and adopted the probe itself; nothing
		// else will reap it.
		if err := w.Close(); err != nil {
			return 0, fmt.Errorf("releasing probe %d: %w", pid, err)
		}
		if _, err := syscall.Wait4(pid, nil, 0, nil); err != nil && !errors.Is(err, syscall.ECHILD) {
			return 0, fmt.Errorf("reaping probe %d: %w", pid, err)
		}
	}
	return adopter, nil
}
