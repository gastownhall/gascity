package subprocess

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestSubprocessProcessTableScannerContract runs RUNTIME-CONTRACT-024
// against the production seam-backed subprocess provider, whose sessions
// always run on this host, so it has no off-host fixture.
func TestSubprocessProcessTableScannerContract(t *testing.T) {
	runtimetest.RunProcessTableScannerContract(t, runtimetest.ProcessTableScannerFixtures{
		Scanning: func(t *testing.T) runtime.Provider { return NewSeamBackedWithDir(t.TempDir()) },
	})
}
