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

func TestSubprocessConformance(t *testing.T) {
	p := NewProviderWithDir(filepath.Join(shortTempDir(t), "pids"))
	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		name := fmt.Sprintf("gc-subproc-conform-%d", id)
		// Safety cleanup: stop any lingering process.
		t.Cleanup(func() { _ = p.Stop(name) })
		return p, runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, name
	})
}
