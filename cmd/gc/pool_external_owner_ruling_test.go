package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// I1: a live gc session owns its assigned bead regardless of the identity
// used for the assignment or whether that session is presently awake.
func TestPoolExternalOccupancySkipsWorkOwnedByAnotherSession(t *testing.T) {
	identities := []struct {
		name     string
		assignee string
		named    bool
	}{
		{"bead ID", "session-a", false},
		{"session name", "a-session", false},
		{"configured named identity", "rig/a-named", true},
		{"alias", "a-alias", false},
		{"alias history", "a-old-alias", false},
	}
	for _, identity := range identities {
		for _, state := range []string{"active", "asleep", "drained"} {
			for _, sameTemplate := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/same-template=%v", identity.name, state, sameTemplate)
				t.Run(name, func(t *testing.T) {
					ownerTemplate := "rig/a"
					maxSessions := 1
					if sameTemplate {
						ownerTemplate = "rig/b"
						maxSessions = 2 // one resume plus the queued request
					}
					cfg := &config.City{Agents: []config.Agent{
						poolAgent("a", "rig", intPtr(2), 0),
						poolAgent("b", "rig", &maxSessions, 0),
					}}
					owner := sessionBead("session-a", "open")
					owner.Metadata = map[string]string{
						"template": ownerTemplate, "session_name": "a-session",
						"configured_named_identity": "rig/a-named", "alias": "a-alias",
						"alias_history": "a-old-alias", "state": state,
					}
					if identity.named {
						owner.Metadata["configured_named_session"] = "true"
					}
					workDir := t.TempDir()
					work := workBead("owned", "rig/b", identity.assignee, "in_progress", 2)
					work.Metadata[beadmeta.WorkDirMetadataKey] = workDir
					trace := newPoolDesiredStateTestTrace("rig/b")
					states := ComputePoolDesiredStatesWithLiveness(cfg, []beads.Bead{work},
						sessionInfosFromBeads([]beads.Bead{owner}), map[string]int{"rig/b": 1},
						map[string]scaleCheckDemand{"rig/b": {WorkBeadIDs: []string{"queued"}}},
						trace, map[string]bool{pathutil.NormalizePathForCompare(workDir): true})
					if got := admittedExternalWorkBeads(t, trace); len(got) != 0 {
						t.Errorf("owned work charged as external: %v", got)
					}
					foundNew := false
					for _, state := range states {
						for _, request := range state.Requests {
							if request.Template == "rig/b" && request.Tier == "new" && request.WorkBeadID == "queued" {
								foundNew = true
							}
						}
					}
					if !foundNew {
						t.Errorf("desired states = %+v, want rig/b's queued new request", states)
					}
				})
			}
		}
	}
}

// I7: a failed process scan treats enumerated worktrees as live, but owner
// resolution still precedes the conservative occupancy charge.
func TestPoolIndeterminateScanChargesOnlyUnownedWork(t *testing.T) {
	_, rigRoot := initReapRig(t)
	workDir := filepath.Join(t.TempDir(), "external")
	if err := os.MkdirAll(filepath.Dir(workDir), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "indeterminate-worker", workDir)
	injectLiveness(t, liveWorktreeState{scanned: false})
	cfg := reapTestConfig(rigRoot)
	cfg.Agents = []config.Agent{
		poolAgent("a", reapTestRigName, intPtr(1), 0),
		poolAgent("b", reapTestRigName, intPtr(1), 0),
	}
	ownerTemplate := reapTestRigName + "/a"
	targetTemplate := reapTestRigName + "/b"
	owner := sessionBead("session-a", "open")
	owner.Metadata = map[string]string{"template": ownerTemplate, "session_name": "a-session"}
	liveDirs := liveExternalWorkDirSet(cfg, nil, io.Discard)
	if !liveDirs[pathutil.NormalizePathForCompare(workDir)] {
		t.Fatalf("indeterminate scan omitted enumerable worktree %s", workDir)
	}
	for _, tc := range []struct {
		name      string
		assignee  string
		wantCount int
	}{
		{"owned", "session-a", 1},
		{"unowned", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := workBead("work", targetTemplate, tc.assignee, "in_progress", 2)
			work.Metadata[beadmeta.WorkDirMetadataKey] = workDir
			trace := newPoolDesiredStateTestTrace(targetTemplate)
			states := ComputePoolDesiredStatesWithLiveness(cfg, []beads.Bead{work},
				sessionInfosFromBeads([]beads.Bead{owner}), map[string]int{targetTemplate: 1},
				nil, trace, liveDirs)
			if got := PoolDesiredCounts(states)[targetTemplate]; got != tc.wantCount {
				t.Errorf("pool desired = %d, want %d after indeterminate scan", got, tc.wantCount)
			}
			if tc.name == "unowned" {
				rec := poolTraceDecision(t, trace, TraceSitePoolExternalLiveOccupancy)
				if rec.Fields["basis"] != "indeterminate_scan" {
					t.Errorf("occupancy basis = %v, want indeterminate_scan", rec.Fields["basis"])
				}
			}
		})
	}
}

// I7: a rig discovery error is visible by name in the decision trace. The
// healthy rig is still enumerated by the existing liveness test.
func TestPoolExternalOccupancyTraceNamesRigWithDiscoveryError(t *testing.T) {
	_, rigRoot := initReapRig(t)
	badRoot := t.TempDir()
	goodWorkDir := filepath.Join(t.TempDir(), "healthy-worktree")
	mustGit(t, rigRoot, "worktree", "add", "-b", "healthy-rig", goodWorkDir)
	injectLiveness(t, liveWorktreeState{scanned: true, cwds: []string{
		pathutil.NormalizePathForCompare(badRoot),
		pathutil.NormalizePathForCompare(goodWorkDir),
	}})
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "broken", Path: badRoot},
			{Name: reapTestRigName, Path: rigRoot},
		},
		Agents: []config.Agent{poolAgent("worker", reapTestRigName, intPtr(1), 0)},
	}
	liveDirs := liveExternalWorkDirSet(cfg, nil, io.Discard)
	if !liveDirs[pathutil.NormalizePathForCompare(goodWorkDir)] {
		t.Error("healthy rig's live worktree was dropped after another rig failed enumeration")
	}
	if liveDirs[pathutil.NormalizePathForCompare(badRoot)] {
		t.Error("failed rig was charged without an enumerable worktree")
	}
	trace := newPoolDesiredStateTestTrace(reapTestRigName + "/worker")
	store := beads.NewMemStore()
	_ = buildDesiredStateWithSessionBeads("test-city", t.TempDir(),
		time.Now().UTC(), cfg, runtime.NewFake(), store, nil, newSessionBeadSnapshot(nil), trace, io.Discard)
	for _, rec := range trace.records {
		if rec.RecordType == TraceRecordDecision && rec.Fields["rig"] == "broken" && rec.Fields["error"] != nil {
			return
		}
	}
	t.Errorf("no discovery-error decision naming rig broken among %d trace records", len(trace.records))
}

// I3: an unavailable or closed session is not an owner. A live external
// worktree still fills the sole slot, just as an unassigned worker does.
func TestPoolExternalOccupancyChargesWhenSessionCannotOwnWork(t *testing.T) {
	for _, tc := range []struct {
		name      string
		assignee  string
		ownerEdit func(*beads.Bead)
	}{
		{"closed", "session-a", func(b *beads.Bead) { b.Status = "closed" }},
		{"provider terminal error", "session-a", func(b *beads.Bead) {
			b.Metadata[sessionProviderTerminalErrorMetadataKey] = "model_not_found"
		}},
		{"empty assignee", "", func(*beads.Bead) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{Agents: []config.Agent{poolAgent("b", "rig", intPtr(1), 0)}}
			owner := sessionBead("session-a", "open")
			owner.Metadata = map[string]string{"template": "rig/b", "session_name": "a-session"}
			tc.ownerEdit(&owner)
			workDir := t.TempDir()
			work := workBead("external", "rig/b", tc.assignee, "in_progress", 2)
			work.Metadata[beadmeta.WorkDirMetadataKey] = workDir
			trace := newPoolDesiredStateTestTrace("rig/b")
			states := ComputePoolDesiredStatesWithLiveness(cfg, []beads.Bead{work},
				sessionInfosFromBeads([]beads.Bead{owner}), map[string]int{"rig/b": 1}, nil,
				trace, map[string]bool{pathutil.NormalizePathForCompare(workDir): true})
			if len(states) != 0 {
				t.Errorf("desired states = %+v, want external work to fill cap", states)
			}
			if got := admittedExternalWorkBeads(t, trace); len(got) != 1 || got[0] != "external" {
				t.Errorf("external charges = %v, want [external]", got)
			}
		})
	}
}

// I2: one unowned live worker consumes the same nested cap a gc worker would;
// the very same metadata does not consume capacity after the worktree exits.
func TestPoolExternalOccupancyUsesAgentRigAndWorkspaceCaps(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configure  func(*config.City)
		demandName string
	}{
		{"agent", func(c *config.City) { c.Agents[0].MaxActiveSessions = intPtr(1) }, "rig/b"},
		{"rig", func(c *config.City) {
			c.Rigs = []config.Rig{{Name: "rig", MaxActiveSessions: intPtr(1)}}
		}, "rig/c"},
		{"workspace", func(c *config.City) { c.Workspace.MaxActiveSessions = intPtr(1) }, "rig/c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{Agents: []config.Agent{
				poolAgent("b", "rig", intPtr(2), 0), poolAgent("c", "rig", intPtr(2), 0),
			}}
			tc.configure(cfg)
			workDir := t.TempDir()
			external := workBead("external", "rig/b", "", "in_progress", 2)
			external.Metadata[beadmeta.WorkDirMetadataKey] = workDir
			counts := map[string]int{tc.demandName: 1}
			for _, live := range []bool{true, false} {
				liveDirs := map[string]bool{}
				if live {
					liveDirs[pathutil.NormalizePathForCompare(workDir)] = true
				}
				trace := newPoolDesiredStateTestTrace("rig/b", "rig/c")
				states := ComputePoolDesiredStatesWithLiveness(cfg, []beads.Bead{external}, nil, counts, nil, trace, liveDirs)
				if live {
					if len(states) != 0 {
						t.Errorf("live external work: states=%+v, want cap to reject second request", states)
					}
					if got := admittedExternalWorkBeads(t, trace); len(got) != 1 || got[0] != "external" {
						t.Errorf("external charges=%v, want exactly [external]", got)
					}
				} else if len(states) != 1 || states[0].Template != tc.demandName || len(states[0].Requests) != 1 || states[0].Requests[0].Tier != "new" {
					t.Errorf("inactive external work: states=%+v, want one new request", states)
				}
			}
		})
	}
}

// I4: a false charge on B must not consume the rig headroom needed by C.
func TestPoolOwnedCrossTemplateWorkDoesNotConsumeSiblingRigCap(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "rig", Path: t.TempDir(), MaxActiveSessions: intPtr(1)}},
		Agents: []config.Agent{
			poolAgent("a", "rig", intPtr(1), 0),
			poolAgent("b", "rig", intPtr(1), 0),
			poolAgent("c", "rig", intPtr(1), 0),
		},
	}
	owner := sessionBead("session-a", "open")
	owner.Metadata = map[string]string{"template": "rig/a", "session_name": "a-session"}
	workDir := t.TempDir()
	work := workBead("owned", "rig/b", "session-a", "in_progress", 2)
	work.Metadata[beadmeta.WorkDirMetadataKey] = workDir
	states := ComputePoolDesiredStatesWithLiveness(cfg, []beads.Bead{work},
		sessionInfosFromBeads([]beads.Bead{owner}), map[string]int{"rig/c": 1},
		map[string]scaleCheckDemand{"rig/c": {WorkBeadIDs: []string{"queued-c"}}}, nil,
		map[string]bool{pathutil.NormalizePathForCompare(workDir): true})
	if len(states) != 1 || states[0].Template != "rig/c" || len(states[0].Requests) != 1 ||
		states[0].Requests[0].Tier != "new" || states[0].Requests[0].WorkBeadID != "queued-c" {
		t.Errorf("desired states = %+v, want C's queued request within the rig cap", states)
	}
}

// I9: an owned-but-unclaimed bead is visible exactly once as a skipped
// occupancy decision. An accepted external charge says how liveness was known.
func TestPoolExternalOccupancyTraceExplainsOwnershipAndLiveness(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("a", "rig", intPtr(1), 0), poolAgent("b", "rig", intPtr(1), 0)}}
	owner := sessionBead("session-a", "open")
	owner.Metadata = map[string]string{"template": "rig/a", "session_name": "a-session"}
	workDir := t.TempDir()
	owned := workBead("owned", "rig/b", "session-a", "in_progress", 2)
	owned.Metadata[beadmeta.WorkDirMetadataKey] = workDir
	trace := newPoolDesiredStateTestTrace("rig/b")
	ComputePoolDesiredStatesWithLiveness(cfg, []beads.Bead{owned},
		sessionInfosFromBeads([]beads.Bead{owner}), nil, nil, trace,
		map[string]bool{pathutil.NormalizePathForCompare(workDir): true})
	count := 0
	for _, rec := range trace.records {
		if rec.RecordType == TraceRecordDecision && rec.SiteCode == TraceSitePoolExternalLiveOccupancy &&
			rec.OutcomeCode == TraceOutcomeSkipped && rec.Fields["work_bead"] == "owned" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("owned skip decisions = %d, want exactly one; trace=%+v", count, trace.records)
	}
	unowned := workBead("external", "rig/b", "", "in_progress", 2)
	unowned.Metadata[beadmeta.WorkDirMetadataKey] = workDir
	trace = newPoolDesiredStateTestTrace("rig/b")
	ComputePoolDesiredStatesWithLiveness(cfg, []beads.Bead{unowned}, nil, nil, nil, trace,
		map[string]bool{pathutil.NormalizePathForCompare(workDir): true})
	rec := poolTraceDecision(t, trace, TraceSitePoolExternalLiveOccupancy)
	if rec.Fields["basis"] != "live" {
		t.Errorf("occupancy basis = %v, want live", rec.Fields["basis"])
	}
}
