package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// onDemandPoolState returns a fake city with one singleton agent and one
// on-demand (min 0, unlimited max) pool agent — the shape of a default
// city's <rig>/claude worker, which has no session until work arrives.
func onDemandPoolState(t *testing.T) *fakeState {
	t.Helper()
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1)},
		{Name: "claude", Dir: "myrig", Provider: "test-agent", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1)},
	}
	return state
}

type agentListWire struct {
	Items         []agentResponse `json:"items"`
	Total         int             `json:"total"`
	Partial       bool            `json:"partial"`
	PartialErrors []string        `json:"partial_errors"`
}

func getAgentList(t *testing.T, state *fakeState, query string) agentListWire {
	t.Helper()
	h := newTestCityHandlerWith(t, state, New(state))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/agents"+query), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents%s status = %d, want 200; body: %s", query, rec.Code, rec.Body.String())
	}
	var resp agentListWire
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func agentByNameIn(items []agentResponse, name string) (agentResponse, bool) {
	for _, item := range items {
		if item.Name == name {
			return item, true
		}
	}
	return agentResponse{}, false
}

// An on-demand pool with no live session must still be listed, as its
// configured identity, so API clients can see (and sling to) it without
// running `gc agent list --json`.
func TestAgentListIncludesIdleOnDemandPool(t *testing.T) {
	state := onDemandPoolState(t)
	resp := getAgentList(t, state, "")

	row, ok := agentByNameIn(resp.Items, "myrig/claude")
	if !ok {
		t.Fatalf("myrig/claude missing from /agents; items = %+v", resp.Items)
	}
	if row.Pool != "myrig/claude" {
		t.Errorf("Pool = %q, want myrig/claude (a configured pool row names itself as the pool)", row.Pool)
	}
	if row.Rig != "myrig" {
		t.Errorf("Rig = %q, want myrig", row.Rig)
	}
	if row.Provider != "test-agent" {
		t.Errorf("Provider = %q, want test-agent", row.Provider)
	}
	if row.Running {
		t.Errorf("Running = true, want false")
	}
	if row.Session != nil {
		t.Errorf("Session = %+v, want nil", row.Session)
	}
	if row.State != "stopped" {
		t.Errorf("State = %q, want stopped", row.State)
	}
	if row.Suspended {
		t.Errorf("Suspended = true, want false")
	}
	if row.PoolLimits == nil || row.PoolLimits.Min != 0 || row.PoolLimits.Max != -1 {
		t.Errorf("PoolLimits = %+v, want {Min:0 Max:-1}", row.PoolLimits)
	}
	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2 (worker + myrig/claude)", resp.Total)
	}

	worker, ok := agentByNameIn(resp.Items, "myrig/worker")
	if !ok {
		t.Fatalf("myrig/worker missing; items = %+v", resp.Items)
	}
	if worker.PoolLimits == nil || worker.PoolLimits.Min != 0 || worker.PoolLimits.Max != 1 {
		t.Errorf("worker PoolLimits = %+v, want {Min:0 Max:1}", worker.PoolLimits)
	}

	// The filters see the configured row like any other.
	if got := getAgentList(t, state, "?pool=myrig/claude"); len(got.Items) != 1 || got.Items[0].Name != "myrig/claude" {
		t.Errorf("?pool=myrig/claude items = %+v, want just myrig/claude", got.Items)
	}
	if got := getAgentList(t, state, "?running=true"); len(got.Items) != 0 {
		t.Errorf("?running=true items = %+v, want none", got.Items)
	}
}

// Once the pool has live sessions those are listed instead; the configured
// row is not repeated next to them.
func TestAgentListOnDemandPoolWithSessionsListsInstances(t *testing.T) {
	state := onDemandPoolState(t)
	state.sp.Start(context.Background(), "myrig--claude-1", runtime.Config{}) //nolint:errcheck
	resp := getAgentList(t, state, "")

	if _, ok := agentByNameIn(resp.Items, "myrig/claude"); ok {
		t.Errorf("configured row myrig/claude listed alongside its live instance; items = %+v", resp.Items)
	}
	inst, ok := agentByNameIn(resp.Items, "myrig/claude-1")
	if !ok {
		t.Fatalf("myrig/claude-1 missing; items = %+v", resp.Items)
	}
	if !inst.Running || inst.Session == nil || inst.Session.Name != "myrig--claude-1" {
		t.Errorf("instance = %+v, want running with session myrig--claude-1", inst)
	}
	if inst.Pool != "myrig/claude" {
		t.Errorf("instance Pool = %q, want myrig/claude", inst.Pool)
	}
	if inst.PoolLimits == nil || inst.PoolLimits.Min != 0 || inst.PoolLimits.Max != -1 {
		t.Errorf("instance PoolLimits = %+v, want {Min:0 Max:-1}", inst.PoolLimits)
	}
}

// When the session provider cannot list a pool's sessions the response must
// say so rather than report the pool as stopped.
func TestAgentListOnDemandPoolDiscoveryFailureIsPartial(t *testing.T) {
	state := onDemandPoolState(t)
	state.sp = runtime.NewFailFake()
	resp := getAgentList(t, state, "")

	if _, ok := agentByNameIn(resp.Items, "myrig/claude"); ok {
		t.Errorf("myrig/claude listed although its sessions could not be listed; items = %+v", resp.Items)
	}
	if !resp.Partial {
		t.Errorf("Partial = false, want true")
	}
	found := false
	for _, msg := range resp.PartialErrors {
		if strings.Contains(msg, "myrig/claude") && strings.Contains(msg, "session unavailable") {
			found = true
		}
	}
	if !found {
		t.Errorf("PartialErrors = %q, want an entry naming myrig/claude and the provider error", resp.PartialErrors)
	}
}

// The single-agent read of a configured pool reports its limits too.
func TestAgentGetConfiguredPoolReportsLimits(t *testing.T) {
	state := onDemandPoolState(t)
	h := newTestCityHandlerWith(t, state, New(state))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/agent/myrig/claude"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var row agentResponse
	if err := json.NewDecoder(rec.Body).Decode(&row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row.Pool != "myrig/claude" || row.State != "stopped" {
		t.Errorf("row = %+v, want pool myrig/claude, state stopped", row)
	}
	if row.PoolLimits == nil || row.PoolLimits.Min != 0 || row.PoolLimits.Max != -1 {
		t.Errorf("PoolLimits = %+v, want {Min:0 Max:-1}", row.PoolLimits)
	}
}

// serverAbsentProvider is a session provider whose runtime server is not
// running at all — a tmux provider before its first session starts.
type serverAbsentProvider struct {
	*runtime.Fake
}

func (serverAbsentProvider) ListRunning(string) ([]string, error) {
	return nil, &runtime.PartialListError{Err: errors.New("tmux server unreachable: no tmux server running"), ServerAbsent: true}
}

// With no runtime server there are no sessions, which is exactly when an
// on-demand pool must still be listed. It is not a partial result.
func TestAgentListOnDemandPoolWithNoRuntimeServer(t *testing.T) {
	state := onDemandPoolState(t)
	state.sessionProvider = serverAbsentProvider{Fake: runtime.NewFake()}
	resp := getAgentList(t, state, "")

	if resp.Partial || len(resp.PartialErrors) != 0 {
		t.Errorf("Partial = %v, PartialErrors = %q; want a complete list", resp.Partial, resp.PartialErrors)
	}
	row, ok := agentByNameIn(resp.Items, "myrig/claude")
	if !ok {
		t.Fatalf("myrig/claude missing with no runtime server; items = %+v", resp.Items)
	}
	if row.Running || row.State != "stopped" || row.Pool != "myrig/claude" {
		t.Errorf("row = %+v, want stopped configured pool row", row)
	}
}
