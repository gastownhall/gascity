package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// resetCLIFactoryRecorders isolates a test from the process-lifetime memo in
// cliFactoryRecorders. The map is package-scoped, so entries a test adds would
// otherwise outlive it and bleed into every later test in the same binary —
// and each entry holds an open file handle on a t.TempDir() that is already
// gone. Entries present before the test are left alone; they belong to whoever
// put them there.
func resetCLIFactoryRecorders(t *testing.T) {
	t.Helper()
	cliFactoryRecordersMu.Lock()
	preexisting := make(map[string]struct{}, len(cliFactoryRecorders))
	for k := range cliFactoryRecorders {
		preexisting[k] = struct{}{}
	}
	cliFactoryRecordersMu.Unlock()

	t.Cleanup(func() {
		cliFactoryRecordersMu.Lock()
		defer cliFactoryRecordersMu.Unlock()
		for k, r := range cliFactoryRecorders {
			if _, ok := preexisting[k]; ok {
				continue
			}
			if closer, ok := r.(io.Closer); ok {
				closer.Close() //nolint:errcheck // test cleanup
			}
			delete(cliFactoryRecorders, k)
		}
	})
}

func TestCliFactoryEventsRecorderEmptyCityPathDiscards(t *testing.T) {
	resetCLIFactoryRecorders(t)
	t.Setenv("GC_EVENTS", "")

	// Other tests in this binary memoize their own cities, so count the delta
	// this call makes rather than the absolute size of the package-level map.
	before := cliFactoryRecorderCount()
	for _, cityPath := range []string{"", "   "} {
		if got := cliFactoryEventsRecorder(cityPath, nil); got != events.Discard {
			t.Fatalf("cliFactoryEventsRecorder(%q) = %#v, want events.Discard", cityPath, got)
		}
	}
	if after := cliFactoryRecorderCount(); after != before {
		t.Fatalf("cliFactoryRecorders grew from %d to %d, want no entry for an empty city path", before, after)
	}
}

// cliFactoryRecorderCount reads the memo size under its mutex.
func cliFactoryRecorderCount() int {
	cliFactoryRecordersMu.Lock()
	defer cliFactoryRecordersMu.Unlock()
	return len(cliFactoryRecorders)
}

// TestCliFactoryEventsRecorderDoesNotMemoizeFailedOpen locks the retry
// contract: a transient open failure must not pin the city to events.Discard
// for the rest of the process. A cached failure would silently disable the
// worker.operation telemetry this recorder exists to carry.
func TestCliFactoryEventsRecorderDoesNotMemoizeFailedOpen(t *testing.T) {
	resetCLIFactoryRecorders(t)
	t.Setenv("GC_EVENTS", "")

	cityPath := t.TempDir()
	// A regular file where the runtime root belongs makes the events path
	// unopenable (ENOTDIR), without needing unwritable permissions.
	blocker := filepath.Join(cityPath, ".gc")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if got := cliFactoryEventsRecorder(cityPath, nil); got != events.Discard {
		t.Fatalf("cliFactoryEventsRecorder with an unopenable events path = %#v, want events.Discard", got)
	}
	cliFactoryRecordersMu.Lock()
	_, cached := cliFactoryRecorders[cityPath]
	cliFactoryRecordersMu.Unlock()
	if cached {
		t.Fatal("failed open was memoized; want the next call to retry")
	}

	// Clear the blocker: the retry must now succeed rather than return the
	// cached fallback.
	if err := os.Remove(blocker); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := cliFactoryEventsRecorder(cityPath, nil)
	if got == events.Discard {
		t.Fatal("cliFactoryEventsRecorder after the failure cleared = events.Discard, want a live recorder")
	}
	if got == nil {
		t.Fatal("cliFactoryEventsRecorder = nil, want a live recorder")
	}
}

// TestCliFactoryEventsRecorderMemoizesPerCityPath locks the memoization the
// file-handle argument rests on: worker.Factory is rebuilt on every
// reconciliation tick, and each rebuild must reuse the city's one recorder
// rather than open another handle on the same log.
func TestCliFactoryEventsRecorderMemoizesPerCityPath(t *testing.T) {
	resetCLIFactoryRecorders(t)
	t.Setenv("GC_EVENTS", "")

	cityPath := t.TempDir()
	first := cliFactoryEventsRecorder(cityPath, nil)
	if first == events.Discard {
		t.Fatalf("first cliFactoryEventsRecorder(%q) = events.Discard, want a live recorder", cityPath)
	}
	second := cliFactoryEventsRecorder(cityPath, nil)
	if first != second {
		t.Fatalf("second cliFactoryEventsRecorder(%q) = %p, want the memoized %p", cityPath, second, first)
	}

	other := cliFactoryEventsRecorder(t.TempDir(), nil)
	if other == first {
		t.Fatal("a different city path reused the same recorder, want one recorder per city")
	}
}
