package main

import (
	"bytes"
	"errors"
	"fmt"
	goruntime "runtime"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/session"
)

// The incarnation-keyed process sweep (CONTRACT v5.7 C4, owner ruling B2;
// BEHAVIORS MAINT-033; §12.2 row 29). Journey J14 B2: a setsid child of a
// stopped row, reparented to init, outlives the row while the row stays open.

const (
	sweepRowID   = "gc-1"
	sweepRowName = "worker"
)

// sweepRow is open session row sweepRowID, named sweepRowName, at state and
// generation.
func sweepRow(state, generation string) beads.Bead {
	return beads.Bead{ID: sweepRowID, Status: "open", Type: sessionBeadType, Metadata: map[string]string{
		"session_name": sweepRowName,
		"state":        state,
		"generation":   generation,
	}}
}

// sweepRead is one read of row sweepRowID. A read that is not open still carries
// the row's decoded fields when state is set (a closed row), so a verdict
// that skipped the openness test would see matching generations.
func sweepRead(open bool, state, generation string) processRowRead {
	if state == "" {
		return processRowRead{open: open}
	}
	return processRowRead{open: open, info: sessionInfoFromBead(sweepRow(state, generation))}
}

// sweepView is a tick's inventory view over one published pass.
func sweepView(attrs map[string]InventoryAttrs, backends ...BackendPass) *runtimeInventoryView {
	return &runtimeInventoryView{snap: newObserveCache().publish(censusNow, attrs, backends...), now: censusNow, maxAge: observeMaxAge}
}

// livePaneAttrs and corpseAttrs are the lane's facts for sweepRowName.
func livePaneAttrs() map[string]InventoryAttrs {
	return map[string]InventoryAttrs{sweepRowName: {DeadKnown: true, AllPanesDead: false}}
}

func corpseAttrs() map[string]InventoryAttrs {
	return map[string]InventoryAttrs{sweepRowName: {DeadKnown: true, AllPanesDead: true}}
}

// Kills, one row each: dropping rule 1, 2 or 3; reaping on one read's word
// (open vs not open, or two generations); letting a missing epoch reach
// rules 2 or 3; reaping a newer or current incarnation; dropping either
// read's live-runtime claim or the inventory's live-pane test; and reading
// the inventory for a different name than both reads agree on.
func TestProcessRootIncarnationOver(t *testing.T) {
	noPane := func(string) bool { return true }
	livePane := func(string) bool { return false }
	cases := []struct {
		name       string
		epoch      int
		snap, live processRowRead
		noLivePane func(string) bool
		want       string
	}{
		{"rule 1: closed or absent in both reads", 4, sweepRead(false, "", ""), sweepRead(false, "", ""), livePane, processOrphanRowGone},
		{"rule 1 needs no epoch", 0, sweepRead(false, "", ""), sweepRead(false, "", ""), livePane, processOrphanRowGone},
		{"snapshot open, store not: disagree", 3, sweepRead(true, "asleep", "4"), sweepRead(false, "", ""), noPane, ""},
		{"store open, snapshot not: disagree", 3, sweepRead(false, "", ""), sweepRead(true, "asleep", "4"), noPane, ""},
		{"snapshot open, store closed at one generation: disagree", 3, sweepRead(true, "asleep", "4"), sweepRead(false, "asleep", "4"), noPane, ""},
		{"store open, snapshot closed at one generation: disagree", 3, sweepRead(false, "asleep", "4"), sweepRead(true, "asleep", "4"), noPane, ""},
		{"rule 2: epoch below generation", 3, sweepRead(true, "active", "4"), sweepRead(true, "active", "4"), livePane, processOrphanOlderRun},
		{"rule 2: generations disagree", 3, sweepRead(true, "active", "3"), sweepRead(true, "active", "4"), livePane, ""},
		{"rule 2: snapshot generation unparseable", 3, sweepRead(true, "active", "x"), sweepRead(true, "active", "4"), livePane, ""},
		{"rule 2: store generation unparseable", 3, sweepRead(true, "active", "x"), sweepRead(true, "active", "x"), livePane, ""},
		{"missing epoch admits only rule 1", 0, sweepRead(true, "asleep", "4"), sweepRead(true, "asleep", "4"), noPane, ""},
		{"newer epoch is never over", 5, sweepRead(true, "asleep", "4"), sweepRead(true, "asleep", "4"), noPane, ""},
		{"rule 3: stopped row, no live pane", 4, sweepRead(true, "asleep", "4"), sweepRead(true, "asleep", "4"), noPane, processOrphanRowAsleep},
		{"rule 3: suspended row, no live pane", 4, sweepRead(true, "suspended", "4"), sweepRead(true, "suspended", "4"), noPane, processOrphanRowAsleep},
		{"rule 3: live pane listed", 4, sweepRead(true, "asleep", "4"), sweepRead(true, "asleep", "4"), livePane, ""},
		{"rule 3: store claims a live runtime", 4, sweepRead(true, "asleep", "4"), sweepRead(true, "active", "4"), noPane, ""},
		{"rule 3: snapshot claims a live runtime", 4, sweepRead(true, "active", "4"), sweepRead(true, "asleep", "4"), noPane, ""},
		{"rule 3: both claim a live runtime", 4, sweepRead(true, "active", "4"), sweepRead(true, "active", "4"), noPane, ""},
		{"rule 3: draining row claims a live runtime", 4, sweepRead(true, "draining", "4"), sweepRead(true, "draining", "4"), noPane, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := processRootIncarnationOver(tc.epoch, tc.snap, tc.live, tc.noLivePane); got != tc.want {
				t.Fatalf("processRootIncarnationOver(epoch=%d) = %q, want %q", tc.epoch, got, tc.want)
			}
		})
	}

	t.Run("rule 3 reads the inventory for the agreed name only", func(t *testing.T) {
		renamed := sweepRead(true, "asleep", "4")
		renamed.info.SessionName = "other"
		var asked []string
		probe := func(name string) bool { asked = append(asked, name); return true }
		if got := processRootIncarnationOver(4, renamed, sweepRead(true, "asleep", "4"), probe); got != "" {
			t.Fatalf("names disagree: verdict %q, want none", got)
		}
		if got := processRootIncarnationOver(4, sweepRead(true, "asleep", "4"), sweepRead(true, "asleep", "4"), probe); got != processOrphanRowAsleep {
			t.Fatalf("names agree: verdict %q, want %q", got, processOrphanRowAsleep)
		}
		if !slices.Equal(asked, []string{sweepRowName}) {
			t.Fatalf("inventory asked for %v, want only %q", asked, sweepRowName)
		}
	})
}

// Kills: reaping a root whose parent is a live process (a tmux server, any
// other owner), ignoring the scan's positive tmux-parent mark, or missing
// the user-subreaper topology.
func TestProcessRootReparented(t *testing.T) {
	const subreaper = 900
	cases := []struct {
		name      string
		live      runtime.LiveRuntime
		subreaper int
		want      bool
	}{
		{"reparented to init", runtime.LiveRuntime{PPID: 1}, 0, true},
		{"parent unreported", runtime.LiveRuntime{PPID: 0}, 0, true},
		{"reparented to the user subreaper", runtime.LiveRuntime{PPID: subreaper}, subreaper, true},
		{"live non-subreaper parent", runtime.LiveRuntime{PPID: 4242}, subreaper, false},
		{"live parent, no subreaper detected", runtime.LiveRuntime{PPID: subreaper}, 0, false},
		{"tmux parent", runtime.LiveRuntime{PPID: 4242, ParentIsProviderInfrastructure: true}, subreaper, false},
		{"tmux parent misdetected as the subreaper", runtime.LiveRuntime{PPID: subreaper, ParentIsProviderInfrastructure: true}, subreaper, false},
		{"tmux mark on an init parent", runtime.LiveRuntime{PPID: 1, ParentIsProviderInfrastructure: true}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := processRootReparented(tc.live, tc.subreaper); got != tc.want {
				t.Fatalf("processRootReparented(%+v, %d) = %v, want %v", tc.live, tc.subreaper, got, tc.want)
			}
		})
	}
}

// Kills: a complete-pass test that accepts a partial, failed, unattested,
// merged-error or stale pass, a nil view, or a live pane; and one that
// refuses a gone name or a listed corpse.
func TestRuntimeInventoryViewNoLivePane(t *testing.T) {
	stale := sweepView(nil, completeBackend("tmux"))
	stale.now = censusNow.Add(observeMaxAge + time.Millisecond)
	merged := &runtimeInventoryView{
		snap:   newObserveCache().publishMerged(censusNow, errors.New("list failed"), nil, completeBackend("tmux")),
		now:    censusNow,
		maxAge: observeMaxAge,
	}
	cases := []struct {
		name string
		view *runtimeInventoryView
		want bool
	}{
		{"nil view", nil, false},
		{"complete, name gone", sweepView(nil, completeBackend("tmux")), true},
		{"complete, listed corpse", sweepView(corpseAttrs(), completeBackend("tmux", sweepRowName)), true},
		{"complete, listed live pane", sweepView(livePaneAttrs(), completeBackend("tmux", sweepRowName)), false},
		{"complete, listed pane unknown", sweepView(nil, completeBackend("tmux", sweepRowName)), false},
		{"partial pass", sweepView(nil, partialSingle()), false},
		{"partial pass listing a corpse", sweepView(corpseAttrs(), partialSingle(sweepRowName)), false},
		{"unattested pass listing a corpse", sweepView(corpseAttrs(), unattestedBackend("exec", sweepRowName)), false},
		{"failed backend", sweepView(nil, completeBackend("tmux"), BackendPass{Label: "acp", Outcome: OutcomeFailed, Err: errors.New("down")}), false},
		{"unattested backend", sweepView(nil, unattestedBackend("exec")), false},
		{"merged listing failed", merged, false},
		{"stale pass", stale, false},
		{"no backends", sweepView(nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.view.noLivePane(sweepRowName); got != tc.want {
				t.Fatalf("noLivePane(%q) = %v, want %v", sweepRowName, got, tc.want)
			}
		})
	}
	if sweepView(nil, completeBackend("tmux")).noLivePane("") {
		t.Fatal("noLivePane(\"\") = true, want false")
	}
}

// procSweep is one sweep over a procfs-shaped tree with the real scanner:
// PPID and the tmux-parent mark come from the scan, as in production.
type procSweep struct {
	t        *testing.T
	cityPath string
	root     string
}

func newProcSweep(t *testing.T) *procSweep {
	t.Helper()
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	s := &procSweep{t: t, cityPath: t.TempDir(), root: t.TempDir()}
	t.Cleanup(proctable.SetScanRootForTesting(s.root))
	return s
}

// tmuxServer writes a tmux server process, never a root itself.
func (s *procSweep) tmuxServer(pid int) {
	writeFakeProcEntry(s.t, s.root, pid, 1, "tmux: server", []string{"PATH=/usr/bin"})
}

// process writes a process of session sweepRowID at epoch under ppid. An
// epoch of "" writes no GC_RUNTIME_EPOCH.
func (s *procSweep) process(pid, ppid int, comm, epoch string) {
	env := []string{"PATH=/usr/bin", "GC_CITY_PATH=" + s.cityPath, "GC_SESSION_ID=" + sweepRowID}
	if epoch != "" {
		env = append(env, "GC_RUNTIME_EPOCH="+epoch)
	}
	writeFakeProcEntry(s.t, s.root, pid, ppid, comm, env)
}

// sweep runs the sweep with snapshot rows snap, the store's rows live and
// inventory view inv, and returns the terminated pids.
func (s *procSweep) sweep(snap, live []beads.Bead, inv *runtimeInventoryView) []int {
	s.t.Helper()
	sp := &procfsSweepScanner{Fake: runtime.NewFake()}
	var stderr bytes.Buffer
	got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(snap), inv, beads.NewMemStoreFrom(0, live, nil), s.cityPath, &stderr)
	pids := make([]int, 0, len(sp.terminated))
	for _, r := range sp.terminated {
		pids = append(pids, r.PID)
	}
	if got != len(pids) {
		s.t.Fatalf("sweep reported %d reaped, terminated %v; stderr=%q", got, pids, stderr.String())
	}
	return pids
}

func wantPIDs(t *testing.T, step string, got []int, want ...int) {
	t.Helper()
	if want == nil {
		want = []int{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s: terminated pids %v, want %v", step, got, want)
	}
}

// J14 B2: a stopped row stays open (asleep at its generation) and the setsid
// child its runtime leaked, reparented to init, lives on. Legacy reaped it
// only once the row closed. It is reaped once both reads agree the row
// claims no live runtime and a complete pass lists no live pane; while the
// snapshot still has the row awake, the reads disagree and it lives.
func TestProcessSweep_StoppedOpenRowLeakedChildReapedAfterTwoReads(t *testing.T) {
	s := newProcSweep(t)
	s.process(3002, 1, "sleep", "4")
	stopped := []beads.Bead{sweepRow("asleep", "4")}
	awake := []beads.Bead{sweepRow("active", "4")}
	inv := sweepView(nil, completeBackend("tmux"))

	wantPIDs(t, "snapshot still awake", s.sweep(awake, stopped, inv))
	wantPIDs(t, "store still awake", s.sweep(stopped, awake, inv))
	wantPIDs(t, "both reads stopped", s.sweep(stopped, stopped, inv), 3002)
}

// A root an older incarnation of an open row leaked (epoch below the row's
// generation) is reaped while the row's current incarnation runs; that
// incarnation's own reparented root and its pane are not.
func TestProcessSweep_EpochOlderOrphanReaped(t *testing.T) {
	s := newProcSweep(t)
	s.tmuxServer(3000)
	s.process(3001, 3000, "bash", "5")
	s.process(3002, 1, "node", "4")
	s.process(3003, 1, "node", "5")
	s.process(3004, 1, "node", "")
	rows := []beads.Bead{sweepRow("active", "5")}

	wantPIDs(t, "row awake at generation 5", s.sweep(rows, rows, sweepView(livePaneAttrs(), completeBackend("tmux", sweepRowName))), 3002)
	stale := []beads.Bead{sweepRow("active", "4")}
	wantPIDs(t, "snapshot at generation 4", s.sweep(stale, rows, nil))
}

// A stopped row whose name the last complete pass lists with a live pane is
// not over: a leaked child of that pane is spared until a pass lists no live
// pane (a corpse or no listing).
func TestProcessSweep_LivePaneChildNeverReaped(t *testing.T) {
	s := newProcSweep(t)
	s.tmuxServer(3000)
	s.process(3001, 3000, "bash", "4")
	s.process(3002, 1, "node", "4")
	stopped := []beads.Bead{sweepRow("asleep", "4")}
	awake := []beads.Bead{sweepRow("active", "4")}

	wantPIDs(t, "awake row, live pane", s.sweep(awake, awake, sweepView(livePaneAttrs(), completeBackend("tmux", sweepRowName))))
	wantPIDs(t, "stopped row, live pane", s.sweep(stopped, stopped, sweepView(livePaneAttrs(), completeBackend("tmux", sweepRowName))))
	wantPIDs(t, "stopped row, pane unknown", s.sweep(stopped, stopped, sweepView(nil, completeBackend("tmux", sweepRowName))))
	wantPIDs(t, "stopped row, corpse", s.sweep(stopped, stopped, sweepView(corpseAttrs(), completeBackend("tmux", sweepRowName))), 3002)
}

// A detached handoff runs in its own tmux session (gc.detached), so its root
// is parented to a tmux server; it is never reaped, under any rule, while a
// reparented root of the same session is.
func TestProcessSweep_DetachedHandoffNeverReaped(t *testing.T) {
	s := newProcSweep(t)
	s.tmuxServer(3100)
	s.process(3101, 3100, "bash", "4")
	s.process(3002, 1, "node", "4")

	wantPIDs(t, "row closed", s.sweep(nil, []beads.Bead{{ID: sweepRowID, Status: "closed"}}, nil), 3002)
	wantPIDs(t, "row absent", s.sweep(nil, nil, nil), 3002)
	stopped := []beads.Bead{sweepRow("asleep", "4")}
	wantPIDs(t, "row stopped", s.sweep(stopped, stopped, sweepView(nil, completeBackend("tmux"))), 3002)
	newer := []beads.Bead{sweepRow("active", "5")}
	wantPIDs(t, "row at a newer generation", s.sweep(newer, newer, nil), 3002)
}

// Rule 3 rests on a complete pass. Without one (no view, a partial, failed or
// unattested listing, a failed merged listing, or a stale pass) the stopped
// row's leaked child is left for a later sweep.
func TestProcessSweep_IncompletePassReapsNothing(t *testing.T) {
	s := newProcSweep(t)
	s.process(3002, 1, "sleep", "4")
	stopped := []beads.Bead{sweepRow("asleep", "4")}
	stale := sweepView(nil, completeBackend("tmux"))
	stale.now = censusNow.Add(observeMaxAge + time.Second)
	for _, tc := range []struct {
		name string
		inv  *runtimeInventoryView
	}{
		{"no view", nil},
		{"partial", sweepView(nil, partialSingle())},
		{"failed backend", sweepView(nil, completeBackend("tmux"), BackendPass{Label: "acp", Outcome: OutcomeFailed, Err: errors.New("down")})},
		{"unattested", sweepView(nil, unattestedBackend("exec"))},
		{"merged failed", &runtimeInventoryView{snap: newObserveCache().publishMerged(censusNow, errors.New("list"), nil, completeBackend("tmux")), now: censusNow, maxAge: observeMaxAge}},
		{"stale", stale},
	} {
		wantPIDs(t, tc.name, s.sweep(stopped, stopped, tc.inv))
	}
}

// A root still parented to a tmux server (an agent pane the provider does
// not track, e.g. after a failed listing) is never reaped, whichever rule
// would call its incarnation over.
func TestProcessSweep_TmuxParentedRootNeverReaped(t *testing.T) {
	s := newProcSweep(t)
	s.tmuxServer(3000)
	s.process(3001, 3000, "bash", "4")
	stopped := []beads.Bead{sweepRow("asleep", "4")}
	newer := []beads.Bead{sweepRow("active", "5")}
	for _, tc := range []struct {
		name       string
		snap, live []beads.Bead
	}{
		{"rule 1: row closed", nil, []beads.Bead{{ID: sweepRowID, Status: "closed"}}},
		{"rule 2: older epoch", newer, newer},
		{"rule 3: row stopped", stopped, stopped},
	} {
		wantPIDs(t, tc.name, s.sweep(tc.snap, tc.live, sweepView(nil, completeBackend("tmux"))))
	}
}

// The subreaper and the tmux mark reach the sweep: a root reparented to init
// is reaped, a root whose scanned parent is tmux is not, even when its PPID
// alone would pass (the fake scanner reports both fields directly).
func TestProcessSweep_TmuxMarkOverridesReparentedPPID(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{{ID: sweepRowID, Status: "closed"}}, nil)
	sp := newProcessTableSweepProvider(
		runtime.LiveRuntime{SessionID: sweepRowID, PID: 601, PPID: 1},
		runtime.LiveRuntime{SessionID: sweepRowID, PID: 602, PPID: 1, ParentIsProviderInfrastructure: true},
		runtime.LiveRuntime{SessionID: sweepRowID, PID: 603, PPID: 4242},
	)
	var stderr bytes.Buffer
	if got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, store, "", &stderr); got != 1 {
		t.Fatalf("swept %d, want 1; stderr=%q", got, stderr.String())
	}
	if got := fmt.Sprint(sp.terminated[0].PID); got != "601" {
		t.Fatalf("terminated pid %s, want 601", got)
	}
}

// A sanity check on the fixture: sweepRow's states classify as the sweep's
// rule 3 expects.
func TestProcessSweepFixtureStates(t *testing.T) {
	for state, want := range map[string]bool{"active": true, "draining": true, "asleep": false, "suspended": false} {
		info := sessionInfoFromBead(sweepRow(state, "4"))
		if got := sessionBeadClaimsLiveRuntime(info); got != want {
			t.Fatalf("state %q claims live = %v, want %v", state, got, want)
		}
		if info.SessionName != sweepRowName || info.Generation != "4" {
			t.Fatalf("state %q decoded as %+v", state, session.Info{SessionName: info.SessionName, Generation: info.Generation})
		}
	}
}
