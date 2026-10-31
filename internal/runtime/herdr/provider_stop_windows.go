//go:build windows

package herdr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

const stopOrphanCleanupAttempts = 3

// Stop closes the agent's pane and clears its metadata sidecar. Idempotent.
// Windows keeps the provider contract shared with Unix: Stop reports success for
// a best-effort teardown and clears the whole sidecar at the end.
func (p *Provider) Stop(name string) error {
	ctx := context.Background()
	boundPane, _ := p.GetMeta(name, metaBoundPane)
	pid, err := p.paneID(ctx, name)
	if pid == "" {
		pid = boundPane
	}
	if pid != "" {
		err = p.closeSessionPane(ctx, name, pid)
	}
	remaining, orphanErr := p.retrySessionOrphanCleanup(name)
	if orphanErr != nil {
		err = errors.Join(err, orphanErr)
	}
	if len(remaining) > 0 {
		err = errors.Join(err, fmt.Errorf("fenced orphan runtime(s) still alive after cleanup attempts: %v", remaining))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr: stop cleanup for %q incomplete: %v\n", name, err) //nolint:errcheck
	}
	_ = p.clearMeta(name)
	return nil
}

func (p *Provider) retrySessionOrphanCleanup(name string) ([]int, error) {
	var remaining []int
	var err error
	for attempt := 0; attempt < stopOrphanCleanupAttempts; attempt++ {
		remaining, err = p.terminateSessionOrphans(name)
		if len(remaining) == 0 {
			return nil, err
		}
		if attempt+1 < stopOrphanCleanupAttempts {
			time.Sleep(time.Second)
		}
	}
	return remaining, err
}
