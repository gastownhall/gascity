package beadstest

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestClassHandleCapabilityConformanceMemStore(t *testing.T) {
	RunClassHandleCapabilityConformance(t, func(*testing.T) beads.Store { return beads.NewMemStore() })
}

func TestClassHandleCapabilityConformanceCachingStore(t *testing.T) {
	RunClassHandleCapabilityConformance(t, func(*testing.T) beads.Store {
		return beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	})
}

func TestClassHandleCapabilityConformanceFileStore(t *testing.T) {
	RunClassHandleCapabilityConformance(t, func(t *testing.T) beads.Store {
		s, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestSingleStoreMintsEveryClassOverOneStore(t *testing.T) {
	mem := beads.NewMemStore()
	c := SingleStore(mem)
	all := c.All()
	if len(all) != 6 {
		t.Fatalf("SingleStore has %d classes, want 6", len(all))
	}
	for class, h := range all {
		if !beads.SameStore(h, mem) {
			t.Errorf("%s: not over the given store", class)
		}
	}
	if c.Graph.Raw() != beads.Store(mem) || c.Nudges.Raw() != beads.Store(mem) {
		t.Fatal("a handle wraps a different store")
	}
}

// recordedErrors is an errorReporter that keeps what the conformance reports.
type recordedErrors []string

func (*recordedErrors) Helper() {}

func (r *recordedErrors) Errorf(format string, args ...any) {
	*r = append(*r, fmt.Sprintf(format, args...))
}

// TestConformanceCatchesAHiddenCapability: the conformance fails a handle
// that hides a capability its store has, and passes the handles it mints.
func TestConformanceCatchesAHiddenCapability(t *testing.T) {
	mem := beads.NewMemStore()
	if _, ok := beads.As[beads.ConditionalWriter](mem); !ok {
		t.Fatal("fixture: MemStore is no ConditionalWriter")
	}
	var got recordedErrors
	checkHandleCapabilities(&got, mem, SingleStore(mem).All())
	if len(got) != 0 {
		t.Fatalf("the minted handles over a MemStore: %q", got)
	}
	// A wrapper that is not a class layer stops As: the capability is hidden.
	checkHandleCapabilities(&got, mem, map[string]beads.Store{"opaque": struct{ beads.Store }{mem}})
	const want = "opaque handle, ConditionalWriter: found = false, the store's = true"
	if !slices.Contains(got, want) {
		t.Fatalf("the conformance over a handle that hides ConditionalWriter reported %q, want %q among them", got, want)
	}
}

func TestClassHandleCapabilityConformanceBdStore(t *testing.T) {
	RunClassHandleCapabilityConformance(t, func(*testing.T) beads.Store { return NewFakeBd().NewBdStore() })
}
