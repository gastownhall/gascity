//go:build integration

package subprocess

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestSubprocessSeamConformance runs the FULL legacy Provider conformance suite
// against the subprocess provider reconstructed from its seams via
// runtime.NewProviderFromSeams. This is the early-cut-over validation: it proves
// the de-conflated seams are sufficient to back the entire Provider contract for
// subprocess, exercising the otherwise-unwired seam code through the same
// contract real callers depend on.
func TestSubprocessSeamConformance(t *testing.T) {
	raw := NewProviderWithDir(filepath.Join(shortTempDir(t), "seam-pids"))
	rt, tp := raw.Seams()
	p := runtime.NewProviderFromSeams(rt, tp)
	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		name := fmt.Sprintf("gc-subproc-seam-%d", id)
		return p, runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, name
	})
}
