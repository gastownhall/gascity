//go:build integration

package subprocess

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	"github.com/gastownhall/gascity/internal/testutil"
)

// TestSubprocessSeamConformance runs the full Provider conformance suite
// against the production seam-backed subprocess constructor.
func TestSubprocessSeamConformance(t *testing.T) {
	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBackedWithDir(testutil.ShortTempDir(t, "gc-subproc-seam-")), runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, fmt.Sprintf("gc-subproc-seam-%d", atomic.AddInt64(&counter, 1))
	})
}
