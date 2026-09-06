package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// metaCountingProvider counts GetMeta calls: on the tmux provider each one is
// a `tmux show-environment` fork, so this is the per-request fork budget.
type metaCountingProvider struct {
	*runtime.Fake
	getMeta atomic.Int64
}

func (p *metaCountingProvider) GetMeta(name, key string) (string, error) {
	p.getMeta.Add(1)
	return p.Fake.GetMeta(name, key)
}

// TestRigResponseObservesEachSlotOnce verifies a rig response observes every
// configured session slot exactly once: the running count, last activity and
// the all-agents-suspended check are derived from one observation per slot,
// not one per field (sys-4za3nm — /rigs forked two full passes per request).
func TestRigResponseObservesEachSlotOnce(t *testing.T) {
	base := newFakeState(t)
	base.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "myrig", MaxActiveSessions: intPtr(1)},
		{Name: "coder", Dir: "myrig", MaxActiveSessions: intPtr(1)},
	}
	sp := &metaCountingProvider{Fake: runtime.NewFake()}
	for _, name := range []string{"myrig--worker", "myrig--coder"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("Start %s: %v", name, err)
		}
		if err := sp.SetMeta(name, "suspended", "true"); err != nil {
			t.Fatalf("SetMeta %s: %v", name, err)
		}
	}
	state := &sessionProviderOverrideState{fakeState: base, provider: sp}
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rig/myrig"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rig rigResponse
	if err := json.NewDecoder(rec.Body).Decode(&rig); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rig.AgentCount != 2 || rig.RunningCount != 2 {
		t.Errorf("AgentCount/RunningCount = %d/%d, want 2/2", rig.AgentCount, rig.RunningCount)
	}
	if !rig.Suspended {
		t.Errorf("Suspended = false, want true (every agent session is runtime-suspended)")
	}
	// observeProviderSession reads two keys (suspended, GC_SESSION_ID) per
	// slot; two slots → four GetMeta calls for the whole response.
	if got := sp.getMeta.Load(); got != 4 {
		t.Errorf("GetMeta calls = %d, want 4 (one observation per slot)", got)
	}
}
