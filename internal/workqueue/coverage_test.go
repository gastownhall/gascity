package workqueue

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
)

func covered[K comparable](c *Coverage[K]) bool {
	select {
	case <-c.Done():
		return true
	default:
		return false
	}
}

// Kills: coverage completed by any Done (readiness declared before the
// tracked Add was reconciled). A key in flight when its tracked Add lands is
// covered only by its dirty requeue.
func TestCoverageRequiresReconcileAfterTrackedAdd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.Add("a", LaneHot, hot("event"))
		inFlight := mustGet(t, q, "a")

		seqA, _ := q.Add("a", LaneResync, hot("boot"))
		seqB, _ := q.Add("b", LaneResync, hot("boot"))
		cov := NewCoverage(map[string]uint64{"a": seqA, "b": seqB})

		q.Done("a")
		cov.Observe("a", inFlight.LastSeq)
		cov.Observe("zz", seqB) // untracked
		if covered(cov) || cov.Pending() != 2 {
			t.Fatalf("after the in-flight item: covered=%v pending=%d, want false 2", covered(cov), cov.Pending())
		}
		for range 2 {
			it, ok := tryGet(q)
			if !ok {
				t.Fatal("Get: nothing available, want the boot items")
			}
			q.Done(it.Key)
			cov.Observe(it.Key, it.LastSeq)
		}
		if !covered(cov) || cov.Pending() != 0 {
			t.Fatalf("after both boot items: covered=%v pending=%d, want true 0", covered(cov), cov.Pending())
		}
		cov.Observe("a", seqA) // repeat after completion is harmless

		if empty := NewCoverage(map[string]uint64{}); !covered(empty) {
			t.Fatal("coverage over no keys not complete at once")
		}
	})
}

// Kills: a sweep coverage that never completes because a live worker
// reconciled a key before the sweep's Track for it landed (Observe not
// remembered until Seal); completing before Seal.
func TestCoverageSweepWithLiveWorkerCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cov := &Coverage[string]{}
		go func() {
			for {
				it, ok := q.Get(ctx)
				if !ok {
					return
				}
				cov.Observe(it.Key, it.LastSeq)
				q.Done(it.Key)
			}
		}()

		seqA, _ := q.Add("a", LaneResync, hot("resync"))
		synctest.Wait() // a is reconciled before its Track
		cov.Track("a", seqA)
		seqB, _ := q.Add("b", LaneResync, hot("resync"))
		cov.Track("b", seqB)
		synctest.Wait()
		if covered(cov) || cov.Pending() != 0 {
			t.Fatalf("before Seal: covered=%v pending=%d, want false 0", covered(cov), cov.Pending())
		}

		// A key still queued at Seal is covered when it runs.
		q.Hold("reload")
		seqC, _ := q.Add("c", LaneResync, hot("resync"))
		cov.Track("c", seqC)
		cov.Seal()
		cov.Track("late", seqC) // ignored after Seal
		if covered(cov) || cov.Pending() != 1 {
			t.Fatalf("sealed with c queued: covered=%v pending=%d, want false 1", covered(cov), cov.Pending())
		}
		q.Release("reload")
		synctest.Wait()
		if !covered(cov) {
			t.Fatalf("sweep not covered after c ran: pending=%d", cov.Pending())
		}
	})
}

// Kills: a non-stdlib import creeping into the package.
func TestWorkqueueImportsStdlibOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %q; the workqueue package is stdlib-only", name, path)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no production files found")
	}
}
