package beads

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// defaultBDMaxChildren is the process-wide bound on concurrently running bd
// children.
const defaultBDMaxChildren = 16

// bdExecGate bounds how many bd children one gc process runs at once (#9092).
// Every exec runner shares the process gate, because runners and stores are
// rebuilt per call site and per request: a bound held by any one of them would
// bound nothing.
//
// The gate has two lanes. Reads (see bdCommandIsRead) may hold at most readCap
// of the size slots, so size-readCap slots (at least one) always stay free for
// mutations such as bd update, close and create; mutations may also use idle
// read capacity. Both semaphores are FIFO with unit weights, so callers are
// admitted in arrival order within their lane and no lane starves.
type bdExecGate struct {
	size    int
	readCap int
	total   *semaphore.Weighted
	reads   *semaphore.Weighted
	running atomic.Int64
}

// newBDExecGate returns a gate of size slots whose read ceiling is
// size - max(1, size/4). size must be at least 2.
func newBDExecGate(size int) *bdExecGate {
	readCap := size - max(1, size/4)
	return &bdExecGate{
		size:    size,
		readCap: readCap,
		total:   semaphore.NewWeighted(int64(size)),
		reads:   semaphore.NewWeighted(int64(readCap)),
	}
}

// processBDExecGate is the gate every exported exec runner constructor uses.
var processBDExecGate = sync.OnceValue(func() *bdExecGate {
	return newBDExecGate(defaultBDMaxChildren)
})

// acquire blocks until the call's lane admits it or ctx ends. On success the
// returned release must be called once the child has been reaped. On failure
// it returns ctx's error and holds no slot.
//
// A read takes its lane slot before a total slot and gives back the lane slot
// if the total wait fails; a slot is held only while a child runs. The fixed
// acquire order leaves no wait cycle between the two semaphores.
func (g *bdExecGate) acquire(ctx context.Context, read bool) (release func(), err error) {
	if read {
		if err := g.reads.Acquire(ctx, 1); err != nil {
			return nil, err
		}
	}
	if err := g.total.Acquire(ctx, 1); err != nil {
		if read {
			g.reads.Release(1)
		}
		return nil, err
	}
	g.running.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			g.running.Add(-1)
			g.total.Release(1)
			if read {
				g.reads.Release(1)
			}
		})
	}, nil
}

// bdVerb describes a bd subcommand gc issues: whether it only reads, and the
// per-command budget it runs under.
type bdVerb struct {
	read    bool
	timeout *time.Duration
}

// bdVerbs is the one table both the exec lane (bdCommandIsRead) and the
// per-command timeout (bdCommandTimeoutFor) read. A verb missing here runs in
// the mutation lane under bdCommandTimeout: classifying a read as a mutation
// only gives it priority, which is the safe direction.
var bdVerbs = map[string]bdVerb{
	"count":   {read: true, timeout: &bdReadCommandTimeout},
	"list":    {read: true, timeout: &bdReadCommandTimeout},
	"ready":   {read: true, timeout: &bdReadCommandTimeout},
	"show":    {read: true, timeout: &bdReadCommandTimeout},
	"sql":     {read: true, timeout: &bdReadCommandTimeout},
	"stats":   {read: true, timeout: &bdReadCommandTimeout},
	"version": {read: true, timeout: &bdReadCommandTimeout},
	"query":   {read: true, timeout: &bdQueryCommandTimeout},
	"blocked": {read: true, timeout: &bdCommandTimeout},
}

// bdCommandIsRead reports whether a bd argv runs in the read lane. It looks
// only at args[0]: an empty argv, an unknown verb and an argv that starts with
// a flag (such as the doltlite "--dolt-auto-commit off" write prefix) are all
// mutations.
func bdCommandIsRead(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return bdVerbs[args[0]].read
}
