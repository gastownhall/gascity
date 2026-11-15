package runtime_test

import (
	"errors"
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

// TestFullFakeServerTeardownContract runs the RUNTIME-CONTRACT-023 cases
// against the full fake, the backend the server-owning profiles forward to:
// its teardown ends every session, as a real server's does, and reports one
// whose stop fails.
func TestFullFakeServerTeardownContract(t *testing.T) {
	runtimetest.RunServerTeardownContract(t, runtimetest.ServerTeardownFixtures{
		Serving: func(_ *testing.T) runtime.Provider { return runtime.FullFake{Fake: runtime.NewFake()} },
		Unstoppable: func(t *testing.T) (runtime.Provider, string) {
			f := runtime.NewFake()
			if err := f.Start(t.Context(), "survivor", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			f.StopErrors["survivor"] = errors.New("backend unreachable")
			return runtime.FullFake{Fake: f}, "survivor"
		},
	})
}
