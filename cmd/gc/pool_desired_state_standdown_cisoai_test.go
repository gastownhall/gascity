package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// ci-soai behavior 3: the pool mint stand-down. The 2026-11-02 08:12Z
// episode (two adhoc polecat mints at load1=33.51, zero beads pulled, both
// drained without doing work) proved nothing in the desired-state path
// consulted host capacity before minting. These cases pin: over either rail
// no NEW session materializes (anonymous demand creates and min-fill floor
// fills alike), sessions that already exist keep their seats, the verdict is
// traceable, and a probe failure fails closed.

func standDownCfg() (*config.City, string) {
	workspaceMax := 10
	agent := poolAgent("worker", "", nil, 0)
	agent.Scope = "city"
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Agents:    []config.Agent{agent},
	}
	return cfg, agent.QualifiedName()
}

func withStandDownCapacity(t *testing.T, verdict poolCapacityVerdict) {
	t.Helper()
	old := poolHostCapacity
	poolHostCapacity = func() poolCapacityVerdict { return verdict }
	t.Cleanup(func() { poolHostCapacity = old })
}

func traceStandDownRecords(trace *sessionReconcilerTraceCycle) []SessionReconcilerTraceRecord {
	var found []SessionReconcilerTraceRecord
	for _, rec := range trace.records {
		if rec.RecordType == TraceRecordDecision && rec.SiteCode == TraceSitePoolCapacityStandDown {
			found = append(found, rec)
		}
	}
	return found
}

func TestComputePoolDesiredStates_CapacityStandDownSuppressesAnonymousMints(t *testing.T) {
	cfg, template := standDownCfg()
	withStandDownCapacity(t, poolCapacityVerdict{load1: 33.51, freeRAM: 8 << 30})
	trace := newPoolDesiredStateTestTrace(template)

	result := ComputePoolDesiredStatesTraced(cfg, nil, nil, map[string]int{template: 3}, trace)
	counts := poolDesiredRequestCounts(result)
	if counts[template] != 0 {
		t.Fatalf("request counts = %#v, want %s=0: three demand rows at load1=33.51 must mint nothing", counts, template)
	}
	recs := traceStandDownRecords(trace)
	if len(recs) != 1 {
		t.Fatalf("stand-down records = %d, want 1; records=%#v", len(recs), trace.records)
	}
	if got, ok := recs[0].Fields["anonymous_new_suppressed"].(int); !ok || got != 3 {
		t.Fatalf("anonymous_new_suppressed = %#v, want 3; record=%#v", recs[0].Fields["anonymous_new_suppressed"], recs[0])
	}
	if got, ok := recs[0].Fields["load1"].(float64); !ok || got != 33.51 {
		t.Fatalf("load1 = %#v, want 33.51 in the stand-down record", recs[0].Fields["load1"])
	}
}

func TestComputePoolDesiredStates_CapacityStandDownFreeRAMRail(t *testing.T) {
	cfg, template := standDownCfg()
	withStandDownCapacity(t, poolCapacityVerdict{load1: 2.0, freeRAM: 1 << 30})

	result := ComputePoolDesiredStates(cfg, nil, nil, map[string]int{template: 1})
	if counts := poolDesiredRequestCounts(result); counts[template] != 0 {
		t.Fatalf("request counts = %#v, want %s=0: 1GiB free is below the 2GiB rail", counts, template)
	}
}

func TestComputePoolDesiredStates_CapacityProbeFailureFailsClosed(t *testing.T) {
	cfg, template := standDownCfg()
	withStandDownCapacity(t, poolCapacityVerdict{probeErr: errors.New("proc read refused")})

	result := ComputePoolDesiredStates(cfg, nil, nil, map[string]int{template: 1})
	if counts := poolDesiredRequestCounts(result); counts[template] != 0 {
		t.Fatalf("request counts = %#v, want %s=0: an unreadable host must fail closed, not mint", counts, template)
	}
}

func TestComputePoolDesiredStates_UnderRailStillMints(t *testing.T) {
	cfg, template := standDownCfg()
	withStandDownCapacity(t, poolCapacityVerdict{load1: 5.3, freeRAM: 8 << 30})
	trace := newPoolDesiredStateTestTrace(template)

	result := ComputePoolDesiredStatesTraced(cfg, nil, nil, map[string]int{template: 2}, trace)
	if counts := poolDesiredRequestCounts(result); counts[template] != 2 {
		t.Fatalf("request counts = %#v, want %s=2: normal pours must keep working under the rails", counts, template)
	}
	if recs := traceStandDownRecords(trace); len(recs) != 0 {
		t.Fatalf("no stand-down record expected under the rails, got %#v", recs)
	}
}

// Reuse is not minting: a protected session that already exists keeps its
// seat under stand-down, and only the anonymous remainder is suppressed.
func TestComputePoolDesiredStates_CapacityStandDownKeepsProtectedReuse(t *testing.T) {
	now := time.Date(2026, 11, 2, 8, 30, 0, 0, time.UTC)
	workspaceMax := 10
	// The protectedPoolSessionBeadAt fixture stamps template "claude", so
	// the demand agent here must be the same template for the protected
	// session to bind at all.
	agent := poolAgent("claude", "", nil, 0)
	agent.Scope = "city"
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Agents:    []config.Agent{agent},
	}
	template := agent.QualifiedName()
	withStandDownCapacity(t, poolCapacityVerdict{load1: 12.0, freeRAM: 8 << 30})
	info := protectedPoolSessionBeadAt("session-1", now.Add(-30*time.Second))

	result := ComputePoolDesiredStatesAt(cfg, nil, sessionInfosFromBeads([]beads.Bead{info}), map[string]int{template: 3}, now)
	states := poolDesiredRequestCounts(result)
	if states[template] != 1 {
		t.Fatalf("request counts = %#v, want %s=1: the live protected session keeps its seat under stand-down", states, template)
	}
	request := result[0].Requests[0]
	if request.SessionBeadID != "session-1" {
		t.Fatalf("retained request = %#v, want session-1 reused, not an anonymous mint", request)
	}
}

// Floor fills are mints too: a min_active_sessions floor must not materialize
// sessions over the rails, but the same floor refills the first pass after
// the host drops back under the rails.
func TestComputePoolDesiredStates_CapacityStandDownSuppressesFloorFill(t *testing.T) {
	workspaceMax := 10
	agent := poolAgent("worker", "", nil, 2) // min_active_sessions=2
	agent.Scope = "city"
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Agents:    []config.Agent{agent},
	}
	template := agent.QualifiedName()
	withStandDownCapacity(t, poolCapacityVerdict{load1: 9.1, freeRAM: 8 << 30})
	trace := newPoolDesiredStateTestTrace(template)

	result := ComputePoolDesiredStatesTraced(cfg, nil, nil, nil, trace)
	if counts := poolDesiredRequestCounts(result); counts[template] != 0 {
		t.Fatalf("request counts = %#v, want %s=0: floor fill is a session mint and must stand down", counts, template)
	}
	recs := traceStandDownRecords(trace)
	if len(recs) != 1 {
		t.Fatalf("stand-down records = %d, want 1; records=%#v", len(recs), trace.records)
	}
	if got, ok := recs[0].Fields["floors_suppressed"].(int); !ok || got != 2 {
		t.Fatalf("floors_suppressed = %#v, want 2; record=%#v", recs[0].Fields["floors_suppressed"], recs[0])
	}

	withStandDownCapacity(t, poolCapacityOK())
	result = ComputePoolDesiredStates(cfg, nil, nil, nil)
	if counts := poolDesiredRequestCounts(result); counts[template] != 2 {
		t.Fatalf("request counts = %#v, want %s=2 once the host is under the rails again", counts, template)
	}
}
