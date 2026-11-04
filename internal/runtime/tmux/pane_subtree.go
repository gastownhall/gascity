package tmux

import (
	"fmt"
	"time"
)

// paneSubtreeTimeout bounds one readPaneSubtree. Its few small reads finish
// in well under a millisecond; a read that blocks (a process wedged with its
// memory map locked stalls a cmdline read) must not stall the probe, which
// then takes the full snapshot.
const paneSubtreeTimeout = 500 * time.Millisecond

// readPaneProcesses returns the processes a one-shot pane probe judges the
// pane process pid by (paneRuntimeState.processAlive reads only pid and its
// descendants): the pane's own subtree read from /proc (readPaneSubtree),
// else, where that read is unavailable or fails, the full process-table
// snapshot (readProcessSnapshot).
//
// The subtree comes first because the full snapshot is the read that
// degrades: procps scans every process on the host, and on a saturated host
// it overruns fetchTimeout or is killed, while a pane rooted in a wrapper
// (the gc-osc7501 relay) can only be judged by its descendants. A start
// waiting on such a pane would otherwise wait out every failed snapshot.
func (t *Tmux) readPaneProcesses(pid string) (processSnapshot, error) {
	if processes, err := readPaneSubtreeBounded(pid, paneSubtreeTimeout); err == nil {
		return processes, nil
	}
	return t.readProcessSnapshot()
}

// readPaneSubtreeBounded is readPaneSubtree giving up after timeout. A read
// that overruns it finishes in the background and is discarded.
func readPaneSubtreeBounded(pid string, timeout time.Duration) (processSnapshot, error) {
	type result struct {
		processes processSnapshot
		err       error
	}
	done := make(chan result, 1)
	go func() {
		processes, err := readPaneSubtree(pid)
		done <- result{processes, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.processes, r.err
	case <-timer.C:
		return processSnapshot{}, fmt.Errorf("reading the process subtree of %s: no answer within %v", pid, timeout)
	}
}
