package main

import (
	"fmt"
	goruntime "runtime"
	"slices"
	"testing"
)

// TestSimDeterministic (mc-gfdi5) kills a seed whose run depends on the
// scheduler: each seed's schedule is the same at GOMAXPROCS 1 and at the
// default. Before, an effect's context canceled on a goroutine of the fake
// clock's, some turn after the Advance that reached its deadline, so an
// effect released past its deadline saw its context live or done by the
// scheduler, and a seed's run could take another path (seed 459 parted at
// step 77; the 32 censuses differed in 7 to 29 ranges by load and
// GOMAXPROCS).
func TestSimDeterministic(t *testing.T) {
	run := func(t *testing.T, seed uint64, procs int) []string {
		defer goruntime.GOMAXPROCS(goruntime.GOMAXPROCS(procs))
		var schedule []string
		t.Run(fmt.Sprintf("procs=%d", procs), func(t *testing.T) {
			s := newSim(t, seed, simOpts{})
			s.run()
			schedule = slices.Clone(s.schedule)
		})
		return schedule
	}
	for _, seed := range []uint64{130, 296, 459, 580, 834} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			one, many := run(t, seed, 1), run(t, seed, goruntime.NumCPU())
			if len(one) == 0 || !slices.Equal(one, many) {
				for i := range min(len(one), len(many)) {
					if one[i] != many[i] {
						t.Fatalf("seed %d: the schedules part at step %d:\n  GOMAXPROCS=1: %s\n  GOMAXPROCS=%d: %s", seed, i, one[i], goruntime.NumCPU(), many[i])
					}
				}
				t.Fatalf("seed %d: schedules of %d and %d steps", seed, len(one), len(many))
			}
		})
	}
}
