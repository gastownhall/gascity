package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// rigListLatencyBudget is the time GET /rigs must answer in on the fleet
// rigListLatencyFleet builds: the handler's own wall time plus the modeled
// cost of the provider metadata reads it made. `gc rig list --json` renders
// this response, and orders call it on every event they handle
// (ga-ylpbsy.93).
const rigListLatencyBudget = time.Second

// rigListMetaCost is what one provider metadata read costs on tmux: one
// `tmux show-environment` fork, ~16-24 ms measured on maintainer-city's
// socket (2026-11-04). The fake answers in microseconds, so the test charges
// each read this cost instead of waiting it out.
const rigListMetaCost = 15 * time.Millisecond

// rigListLatencyFleet configures rigs rigs of agentsPerRig single-seat agents
// each and starts the first runningPerRig of every rig's sessions. It returns
// the running session names.
func rigListLatencyFleet(t *testing.T, state *fakeState, sp *runtime.Fake, rigs, agentsPerRig, runningPerRig int) map[string]bool {
	t.Helper()
	state.cfg.Rigs = nil
	state.cfg.Agents = nil
	state.cfg.NamedSessions = nil
	running := make(map[string]bool)
	for r := 0; r < rigs; r++ {
		rig := fmt.Sprintf("rig%d", r)
		state.cfg.Rigs = append(state.cfg.Rigs, config.Rig{Name: rig, Path: "/tmp/" + rig})
		for a := 0; a < agentsPerRig; a++ {
			name := fmt.Sprintf("agent%d", a)
			state.cfg.Agents = append(state.cfg.Agents, config.Agent{Name: name, Dir: rig, Provider: "test-agent", MaxActiveSessions: intPtr(1)})
			if a < runningPerRig {
				session := rig + "--" + name
				if err := sp.Start(context.Background(), session, runtime.Config{}); err != nil {
					t.Fatalf("Start %s: %v", session, err)
				}
				running[session] = true
			}
		}
	}
	return running
}

// TestRigListLatencyBudget guards the cost of GET /rigs on a large fleet:
// 3 rigs of 110 configured agents with 2 running each, close to
// maintainer-city's 332 agent rows with 7 running, where the handler took
// 43 s because it read two metadata keys, twice, for every configured row
// whether or not its session existed (ga-ylpbsy.93). The response, with
// every metadata read charged rigListMetaCost, must cost less than
// rigListLatencyBudget and still report the running counts.
//
// A provider that attests its listing complete answers every stopped row
// from the request's one full listing, with no probe of the row's session.
// One that does not still probes every row's liveness, but metadata lives on
// a running session only, so it reads the metadata of running sessions only.
func TestRigListLatencyBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unattested bool
	}{
		{name: "attested listing"},
		{name: "unattested listing", unattested: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const rigs, agentsPerRig, runningPerRig = 3, 110, 2
			base := newFakeState(t)
			fake := runtime.NewFake()
			fake.ListingUnattested = tc.unattested
			running := rigListLatencyFleet(t, base, fake, rigs, agentsPerRig, runningPerRig)
			state := &sessionProviderOverrideState{fakeState: base, provider: fake}
			h := newTestCityHandler(t, state)
			fake.Calls = nil

			start := time.Now()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rigs"), nil))
			elapsed := time.Since(start)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Items []rigResponse `json:"items"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Items) != rigs {
				t.Fatalf("rigs = %d, want %d", len(resp.Items), rigs)
			}
			for _, rig := range resp.Items {
				if rig.AgentCount != agentsPerRig || rig.RunningCount != runningPerRig || rig.Suspended {
					t.Errorf("rig %s: agents=%d running=%d suspended=%v, want %d/%d/false", rig.Name, rig.AgentCount, rig.RunningCount, rig.Suspended, agentsPerRig, runningPerRig)
				}
			}
			var metaReads int
			for _, call := range fake.SnapshotCalls() {
				if call.Method == "GetMeta" {
					metaReads++
					if !running[call.Name] {
						t.Errorf("GET /rigs read metadata %q of stopped session %q", call.Key, call.Name)
					}
					continue
				}
				if tc.unattested && call.Method == "ListRunning" {
					t.Errorf("GET /rigs listed an unattested provider with no unlimited pool to expand; its listing proves nothing")
				}
				if !tc.unattested && call.Name != "" && !running[call.Name] {
					t.Errorf("GET /rigs called %s on stopped session %q under an attested listing", call.Method, call.Name)
				}
			}
			// One "suspended" read per running row for its observation, and
			// one per rig for the suspension check, which stops at the rig's
			// first row that is not suspended.
			if most := len(running) + rigs; metaReads > most {
				t.Errorf("GET /rigs made %d metadata reads, want at most %d (one per running session and one per rig)", metaReads, most)
			}
			if cost := elapsed + time.Duration(metaReads)*rigListMetaCost; cost >= rigListLatencyBudget {
				t.Errorf("GET /rigs cost %s for %d agent rows (%s handler + %d metadata reads at %s), budget %s", cost.Round(time.Millisecond), rigs*agentsPerRig, elapsed.Round(time.Millisecond), metaReads, rigListMetaCost, rigListLatencyBudget)
			}
		})
	}
}
