package runtime_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestFakeStopContract runs the RUNTIME-CONTRACT-003 cases against the fake: a
// working fake holds no such session, and a broken fake cannot observe its
// sessions at all.
func TestFakeStopContract(t *testing.T) {
	runtimetest.RunStopContractTests(t, runtimetest.StopFixtures{
		Responsive:  func(_ *testing.T) runtime.Provider { return runtime.NewFake() },
		Unreachable: func(_ *testing.T) runtime.Provider { return runtime.NewFailFake() },
	})
}
