package dashboardbff

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// statusServer returns an httptest server that serves a fixed supervisor status
// body at /v0/city/{name}/status, so refresh()'s fetchStatus succeeds.
func statusServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// TestRefreshReadersDoNotBlockOnProbe is the regression test for the HIGH
// finding: refresh() must not hold the per-city write lock across the blocking
// rig probe. beforeProbe blocks the probe pass mid-flight while a reader calls
// supervisorStatus(); if the write lock were held across probeRig, the reader's
// RLock would block until the probe is released and the deadline would elapse.
func TestRefreshReadersDoNotBlockOnProbe(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":100},"rig_details":[{"name":"r1","path":"/dashboardbff-nonexistent-rig"}]}`)
	defer srv.Close()

	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}

	probing := make(chan struct{}) // closed once the probe pass is in-flight
	release := make(chan struct{}) // test closes this to let the probe finish
	cs.beforeProbe = func() {
		close(probing)
		<-release
	}

	done := make(chan struct{})
	go func() {
		cs.refresh(context.Background())
		close(done)
	}()

	select {
	case <-probing:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never reached the rig probe")
	}

	// The probe is mid-flight. A reader must still return promptly — proving no
	// write lock is held across probeRig.
	got := make(chan supervisorStatusReport, 1)
	go func() { got <- cs.supervisorStatus() }()
	select {
	case <-got:
		// reader returned while the probe is blocked: contract upheld.
	case <-time.After(time.Second):
		t.Fatal("supervisorStatus() blocked while a probe was in flight: write lock held across probeRig")
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not finish after probe released")
	}
}

// TestRefreshPublishesUnderLock confirms the happy path still publishes status,
// the dolt ring, and the rig report after one refresh.
func TestRefreshPublishesUnderLock(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":4096},"rig_details":[{"name":"r1","path":"/dashboardbff-nonexistent-rig"}]}`)
	defer srv.Close()

	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}
	cs.refresh(context.Background())

	if rep := cs.supervisorStatus(); !rep.Available {
		t.Errorf("supervisorStatus available = false, want true after a good fetch")
	}
	trend := cs.doltTrend()
	if !trend.Available || len(trend.Samples) != 1 || trend.Samples[0].Bytes != 4096 {
		t.Errorf("doltTrend = %+v, want one 4096-byte sample available", trend)
	}
	rig := cs.rigStoreHealth()
	if !rig.Available || len(rig.Rigs) != 1 {
		t.Errorf("rigStoreHealth = %+v, want one rig available", rig)
	}
	// The probed rig dir does not exist, so it rolls up down/unreachable.
	if rig.Rigs[0].Reachable {
		t.Errorf("rig reachable = true, want false for a missing .beads dir")
	}
}

// TestRefreshDegradesNotBlankOnFetchError verifies a failed status fetch retains
// the last-good snapshot (status flips to unavailable, dolt/rig data survives).
func TestRefreshDegradesNotBlankOnFetchError(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":2048},"rig_details":[{"name":"r1","path":"/dashboardbff-nonexistent-rig"}]}`)
	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}

	cs.refresh(context.Background()) // seed last-good
	srv.Close()                      // next fetch fails
	cs.refresh(context.Background())

	if rep := cs.supervisorStatus(); rep.Available {
		t.Errorf("supervisorStatus available = true, want false after fetch failure")
	} else if rep.Reason != "status_read_failed" {
		t.Errorf("reason = %q, want status_read_failed", rep.Reason)
	}
	// Last-good dolt + rig data must survive the failed fetch (degrade, not blank).
	if trend := cs.doltTrend(); len(trend.Samples) != 1 {
		t.Errorf("doltTrend samples = %d, want 1 retained after fetch failure", len(trend.Samples))
	}
	if rig := cs.rigStoreHealth(); len(rig.Rigs) != 1 {
		t.Errorf("rigStoreHealth rigs = %d, want 1 retained after fetch failure", len(rig.Rigs))
	}
}

// TestRefreshCadenceGates confirms the dolt ring only appends on its 10-min
// cadence: two back-to-back refreshes append once (the second is inside the
// window), while the rig probe (5-min cadence) likewise runs once.
func TestRefreshCadenceGates(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":100},"rig_details":[]}`)
	defer srv.Close()

	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}

	cs.refresh(context.Background())
	first := cs.doltTrend()
	cs.refresh(context.Background()) // within doltAppendInterval: no new sample
	second := cs.doltTrend()

	if len(first.Samples) != 1 || len(second.Samples) != 1 {
		t.Errorf("dolt ring grew inside the append window: first=%d second=%d", len(first.Samples), len(second.Samples))
	}
}

// TestEnsureDoesNotStoreCityPath documents that ensure no longer tracks the
// city path (the dead cs.path reassignment was removed); the sampler keys off
// cs.name and rig paths come from the status body.
func TestEnsureDoesNotStoreCityPath(t *testing.T) {
	m := newSamplerManager(Deps{}, newExecRunner())
	cs := m.ensure("alpha")
	if cs.name != "alpha" {
		t.Errorf("ensure name = %q, want alpha", cs.name)
	}
	// Calling ensure again returns the same sampler instance.
	if again := m.ensure("alpha"); again != cs {
		t.Error("ensure should return the cached sampler for a known city")
	}
}
