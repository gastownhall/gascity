package beads

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bdSubprocessConcurrency bounds how many `bd` child processes one gc process
// may have in flight at once.
//
// Every store read on a bd-backed scope is a subprocess (bd list/query/ready/
// show), and the supervisor fans those reads out per session, per order and
// per API request. When the backing Dolt server slows down (all cities share
// one sql-server), each call takes tens of seconds while new callers keep
// arriving, so the children accumulate far past what the box can hold — each
// bd costs ~140MB RSS — and the machine slides into swap (workflow-rx4).
// Bounding the fan-out at the single spawn point turns that pile-up into
// queueing inside gc, where ctx deadlines still apply.
//
// Lanes are split read vs write so a read storm cannot starve mutations:
// reads are the storm driver (per-session probes, work/ready, sweeps), while
// writers (bd update/close/create — the claim/commit path) must keep making
// progress or ready work stays ready and feeds the read storm. Total bound
// is readCap+writeCap children.
//
// The bound is process-wide and deliberately NOT keyed by store dir: a
// supervisor serves every registered city from one process, and a city plus
// its rigs all draw from the same dolt server — the scarce resource is the
// host's process/memory budget, so all callers share one bound rather than
// multiplying it per city or per rig.
//
// Queue waits are bounded by bdQueueWait, independent of the command's own
// exec timeout: a caller that would wait longer sheds fast instead of
// spawning a bd that is killed mid-query by its own deadline.
//
// GC_BD_MAX_CONCURRENT overrides the READ limit (writes keep their own small
// cap): a positive integer sets it, "0" disables both bounds (pre-change
// behavior), and an unset or unparsable value keeps the defaults.
const (
	defaultBDReadConcurrency  = 4
	defaultBDWriteConcurrency = 2
	bdMaxConcurrentEnvVar     = "GC_BD_MAX_CONCURRENT"

	// bdQueueWait bounds how long a caller may wait for a slot before the
	// call sheds. Chosen so a queued call fails fast under saturation
	// instead of spending its whole exec budget in line.
	bdQueueWait = 20 * time.Second
	// bdMinExecBudget: if the caller's parent deadline leaves less than this
	// after a successful acquire, the slot is released and the call sheds —
	// spawning under that budget means being SIGKILLed mid-query, which
	// wastes the slot and the dolt-side work.
	bdMinExecBudget = 3 * time.Second
)

var (
	bdReadSlots  = sync.OnceValue(func() chan struct{} { return newBDSlotChan(defaultBDReadConcurrency) })
	bdWriteSlots = sync.OnceValue(func() chan struct{} { return newBDSlotChan(defaultBDWriteConcurrency) })
)

func newBDSlotChan(def int) chan struct{} {
	limit := def
	if raw := strings.TrimSpace(os.Getenv(bdMaxConcurrentEnvVar)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			switch {
			case n == 0:
				return nil // bound disabled
			case n > 0:
				limit = n
			}
		}
	}
	return make(chan struct{}, limit)
}

// bdReadOnlyArgs reports whether the bd invocation is a read-only command.
// Anything unrecognized is treated as a writer so mutations always get the
// write lane (fail-safe direction: never starve a mutation as a "read").
func bdReadOnlyArgs(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "list", "ready", "show", "query", "next", "stats", "search",
			"export", "history", "status", "info", "count", "blocked",
			"stale", "orphans", "graph", "ready-count":
			return true
		default:
			return false
		}
	}
	return false // no verb at all -> treat as writer (rare)
}

// acquireBDSubprocessSlot waits for a lane slot with a bounded queue wait
// and a minimum post-acquire budget check. Returns release func on success.
func acquireBDSubprocessSlot(parent context.Context, args []string) (func(), error) {
	slots := bdWriteSlots()
	if bdReadOnlyArgs(args) {
		slots = bdReadSlots()
	}
	return acquireBDSubprocessSlotOn(parent, slots)
}

// acquireBDSubprocessSlotOn is the lane-generic core, split out so tests
// can drive it with a private channel instead of the process-wide lanes.
func acquireBDSubprocessSlotOn(parent context.Context, slots chan struct{}) (func(), error) {
	if slots == nil {
		return func() {}, nil
	}
	qctx, qcancel := context.WithTimeout(parent, bdQueueWait)
	defer qcancel()
	select {
	case slots <- struct{}{}:
	case <-qctx.Done():
		return nil, qctx.Err()
	}
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) < bdMinExecBudget {
		<-slots
		return nil, context.DeadlineExceeded
	}
	return func() { <-slots }, nil
}
