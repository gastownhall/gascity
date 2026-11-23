//go:build !windows

package main

import (
	"bytes"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// TestStatusObservationSeesProbeInsideSnapshotBudget pins the status
// observation budget at one runtime snapshot fetch. The pass used to give up
// at 750ms, before a snapshot fetch (up to 3s) could publish, and reported
// live agents unknown. A probe that takes longer than that old budget and
// less than one fetch must still be observed. The bubble's fake clock models
// the probe delay without elapsed wall time.
func TestStatusObservationSeesProbeInsideSnapshotBudget(t *testing.T) {
	if statusObservationTimeout < statusRuntimeProbeBudget {
		t.Fatalf("statusObservationTimeout = %s, want at least one snapshot fetch (%s)", statusObservationTimeout, statusRuntimeProbeBudget)
	}
	synctest.Test(t, func(t *testing.T) {
		oldObserve := observeSessionTargetForStatus
		observeSessionTargetForStatus = func(string, beads.Store, runtime.Provider, *config.City, string) (worker.LiveObservation, error) {
			probe := time.NewTimer(900 * time.Millisecond)
			defer probe.Stop()
			<-probe.C
			return worker.LiveObservation{Running: true}, nil
		}
		t.Cleanup(func() { observeSessionTargetForStatus = oldObserve })

		var stderr bytes.Buffer
		obs := observeSessionTargetWithWarning(
			"gc status",
			"/city",
			nil,
			runtime.NewFake(),
			&config.City{},
			statusObservationTarget{runtimeSessionName: "slow-but-alive"},
			&stderr,
		)
		if !obs.Running {
			t.Fatalf("observation running = false, stderr=%q; probe finished inside one snapshot fetch", stderr.String())
		}
		if strings.Contains(stderr.String(), "timed out") {
			t.Fatalf("stderr = %q, want no timeout for a probe inside the snapshot budget", stderr.String())
		}
	})
}
