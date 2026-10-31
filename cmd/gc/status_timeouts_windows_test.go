//go:build windows

package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// spawningStatusProvider answers IsRunning after starting real processes, as a
// herdr-backed provider does for every status call on Windows.
type spawningStatusProvider struct {
	runtime.Provider
	spawns int
}

func (p *spawningStatusProvider) IsRunning(string) bool {
	for i := 0; i < p.spawns; i++ {
		if err := exec.Command("cmd.exe", "/c", "exit 0").Run(); err != nil {
			return false
		}
	}
	return true
}

// A status provider call that starts processes, as a herdr liveness
// observation does, gets a real answer on Windows rather than the timeout
// fallback that renders the agent unknown.
func TestStatusProviderCallAllowsWindowsProcessStarts(t *testing.T) {
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() { statusProviderTimeoutWarning = origWarn })
	statusProviderTimeoutWarning = func() {}

	wrapped := newBoundedStatusProvider(&spawningStatusProvider{Provider: runtime.NewFake(), spawns: 3})
	for i := 0; i < 3; i++ {
		if !wrapped.IsRunning("worker") {
			t.Fatalf("IsRunning round %d = false, want the provider's answer within %s", i, statusProviderCallTimeout)
		}
	}
	if statusProviderPartial(wrapped) {
		t.Fatalf("status marked partial: a provider call that starts three processes exceeded %s", statusProviderCallTimeout)
	}
}

// One session's observation may make two bounded provider calls in sequence;
// it is not cut off while they run.
func TestStatusObservationAllowsTwoBoundedProviderCalls(t *testing.T) {
	if statusObservationTimeout <= 2*statusProviderCallTimeout {
		t.Fatalf("statusObservationTimeout %s does not exceed two provider-call bounds (%s each)", statusObservationTimeout, statusProviderCallTimeout)
	}
	took := 2*statusProviderCallTimeout + 100*time.Millisecond
	oldObserve := observeSessionTargetForStatus
	observeSessionTargetForStatus = func(string, beads.Store, runtime.Provider, *config.City, string) (worker.LiveObservation, error) {
		time.Sleep(took)
		return worker.LiveObservation{Running: true}, nil
	}
	t.Cleanup(func() { observeSessionTargetForStatus = oldObserve })

	var stderr bytes.Buffer
	obs := observeSessionTargetWithWarning("gc status", "/city", nil, runtime.NewFake(), &config.City{},
		statusObservationTarget{runtimeSessionName: "worker"}, &stderr)
	if !obs.Running || strings.Contains(stderr.String(), "timed out") {
		t.Fatalf("observation taking %s = %+v, stderr %q; want it to complete within %s", took, obs, stderr.String(), statusObservationTimeout)
	}
}
