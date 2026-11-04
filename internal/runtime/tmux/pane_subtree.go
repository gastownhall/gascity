package tmux

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// paneSubtreeTimeout bounds how long a probe waits on one readPaneSubtree.
// Its few small reads finish in well under a millisecond; a read that blocks
// (a process wedged with its memory map locked stalls a cmdline read) must
// not stall the probe, which then takes the full snapshot.
const paneSubtreeTimeout = 500 * time.Millisecond

// maxPaneSubtreeWalks bounds the pane-subtree walks in flight in this
// process. A walk answers in well under a millisecond, so only walks held
// by blocked reads come near it.
const maxPaneSubtreeWalks = 8

// errPaneSubtreeBusy is a walk refused because earlier walks still hold the
// slot it needs: its pane's own, or every slot.
var errPaneSubtreeBusy = errors.New("no free walk slot")

// errNoPaneSubtree is a walk on a platform without /proc
// (paneSubtreeSupported is false).
var errNoPaneSubtree = errors.New("reading a process subtree: no /proc on this platform")

// readPaneProcesses returns the processes a one-shot pane probe judges the
// pane process pid by (paneRuntimeState.processAlive reads only pid and its
// descendants): the pane's own subtree read from /proc (paneSubtree), else,
// where that read is unavailable or fails, the full process-table snapshot
// (readProcessSnapshot).
//
// The subtree comes first because the full snapshot is the read that
// degrades: procps scans every process on the host, and on a saturated host
// it overruns fetchTimeout or is killed, while a pane rooted in a wrapper
// (the gc-osc7501 relay) can only be judged by its descendants. A start
// waiting on such a pane would otherwise wait out every failed snapshot.
func (t *Tmux) readPaneProcesses(pid string) (processSnapshot, error) {
	if processes, err := t.paneSubtree(pid); err == nil {
		return processes, nil
	}
	return t.readProcessSnapshot()
}

// paneSubtree reads pid's subtree for a one-shot pane probe through the
// process's bounded walks (paneSubtreeWalks). A failed walk sends the probe
// to the full snapshot, so it is logged, rate-limited (subtreeFailures): on
// a host where every walk fails, every probe pays the full snapshot again.
func (t *Tmux) paneSubtree(pid string) (processSnapshot, error) {
	if !paneSubtreeSupported {
		return processSnapshot{}, errNoPaneSubtree
	}
	walks := t.subtreeWalks
	if walks == nil {
		walks = paneSubtrees
	}
	processes, err := walks.walk(pid)
	if err != nil {
		t.subtreeFailures.note("pane subtree read failed; the probe takes the full process snapshot instead", err)
	}
	return processes, err
}

// paneSubtreeWalks runs pane-subtree walks under a bound: at most one in
// flight per pane pid, and at most limit in all. A walk holds its slot until
// its read returns, not until its caller stops waiting: a read blocked in the
// kernel keeps its goroutine and an OS thread for as long as the block lasts,
// so a walk started per probe would park one more of each with every probe
// of a wedged pane. A walk that finds its pid's slot, or every slot, held
// fails at once with errPaneSubtreeBusy, and its probe takes the full
// snapshot, whose ps is a child process that can be killed.
type paneSubtreeWalks struct {
	limit   int                                       // walks in flight at most
	timeout time.Duration                             // how long a caller waits on its walk
	read    func(pid string) (processSnapshot, error) // one walk (readPaneSubtree)

	mu      sync.Mutex
	running map[string]struct{} // the pids whose walk has not returned
}

// paneSubtrees is this process's pane-subtree walks, shared by every Tmux:
// the threads a blocked read holds are the process's.
var paneSubtrees = &paneSubtreeWalks{limit: maxPaneSubtreeWalks, timeout: paneSubtreeTimeout, read: readPaneSubtree}

// walk is read(pid) giving up after timeout. A walk that overruns it
// finishes in the background, still holding its slot, and its answer is
// discarded.
func (w *paneSubtreeWalks) walk(pid string) (processSnapshot, error) {
	if err := w.claim(pid); err != nil {
		return processSnapshot{}, err
	}
	type result struct {
		processes processSnapshot
		err       error
	}
	done := make(chan result, 1)
	go func() {
		processes, err := w.read(pid)
		w.release(pid)
		done <- result{processes, err}
	}()
	timer := time.NewTimer(w.timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.processes, r.err
	case <-timer.C:
		return processSnapshot{}, fmt.Errorf("reading the process subtree of %s: no answer within %v", pid, w.timeout)
	}
}

// claim takes pid's slot, or fails with errPaneSubtreeBusy while pid's last
// walk, or limit walks, have not returned.
func (w *paneSubtreeWalks) claim(pid string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, held := w.running[pid]; held {
		return fmt.Errorf("reading the process subtree of %s: %w: its last walk has not returned", pid, errPaneSubtreeBusy)
	}
	if len(w.running) >= w.limit {
		return fmt.Errorf("reading the process subtree of %s: %w: %d walks have not returned", pid, errPaneSubtreeBusy, len(w.running))
	}
	if w.running == nil {
		w.running = make(map[string]struct{})
	}
	w.running[pid] = struct{}{}
	return nil
}

// release frees pid's slot once its read has returned.
func (w *paneSubtreeWalks) release(pid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.running, pid)
}
