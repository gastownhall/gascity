//go:build gc_coarse_clock

package beads

import "time"

// Built with -tags gc_coarse_clock, every MemStore (and so every FileStore)
// reads a millisecond wall clock, far coarser than macOS's microsecond one,
// so Linux reproduces deterministically the same-tick creates a Mac runner
// hits only now and then. `make test-coarse-clock` runs the suites under it.
func init() {
	memStoreWallClock = func() time.Time { return time.Now().Truncate(time.Millisecond) }
}
