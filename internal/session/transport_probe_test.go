package session

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestTransportDerivationDoesNotProbeIsRunning pins that an unresolved
// transport falls through without a per-session liveness probe (#6483/#6485).
func TestTransportDerivationDoesNotProbeIsRunning(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s-running", runtime.Config{}); err != nil {
		t.Fatalf("start fake session: %v", err)
	}
	m := newTestManager(t, beads.NewMemStore(), fake, WithCityPath(""))

	for _, sessName := range []string{"s-running", "s-stopped"} {
		b := beads.Bead{
			ID:       "b-" + sessName,
			Type:     BeadType,
			Status:   "open",
			Labels:   []string{LabelSession},
			Metadata: map[string]string{"session_name": sessName, "state": "active", "provider": "claude"},
		}

		fake.Calls = nil
		if transport, fallback := m.transportForBead(b, sessName); transport != "" || fallback {
			t.Errorf("%s: transportForBead = (%q, %v), want (\"\", false)", sessName, transport, fallback)
		}
		assertNoIsRunningCall(t, fake, "transportForBead", sessName)

		fake.Calls = nil
		if transport, fallback := m.transportForInfo(infoFromPersistedBead(b)); transport != "" || fallback {
			t.Errorf("%s: transportForInfo = (%q, %v), want (\"\", false)", sessName, transport, fallback)
		}
		assertNoIsRunningCall(t, fake, "transportForInfo", sessName)
	}
}

func assertNoIsRunningCall(t *testing.T, fake *runtime.Fake, fn, sessName string) {
	t.Helper()
	for _, call := range fake.SnapshotCalls() {
		if call.Method == "IsRunning" {
			t.Errorf("%s(%s) probed IsRunning(%q); transport derivation must not probe liveness", fn, sessName, call.Name)
		}
	}
}
