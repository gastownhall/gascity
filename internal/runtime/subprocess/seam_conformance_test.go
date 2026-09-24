//go:build integration

package subprocess

import (
	"fmt"
	"os"
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

// TestSubprocessDefaultDirSeamConformance runs the same full Provider
// conformance suite against the constructor cmd/gc's "subprocess" registration
// calls when a city path is absent: NewSeamBacked, which keeps socket and meta
// files in the shared default temporary directory rather than an injected one.
// That directory is process-shared by design, so session names carry the PID —
// the suite only asserts membership of its own names, and PID-scoped names keep
// concurrent runs on one machine from colliding there.
func TestSubprocessDefaultDirSeamConformance(t *testing.T) {
	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBacked(), runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, fmt.Sprintf("gc-subproc-default-%d-%d", os.Getpid(), atomic.AddInt64(&counter, 1))
	})
}
