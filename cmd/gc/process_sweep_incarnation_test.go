package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/session"
)

// The incarnation-keyed process sweep (CONTRACT v5.8b C4, owner ruling B2;
// BEHAVIORS MAINT-033; §12.2 row 29). Journey J14 B2: a setsid child of a
// stopped row, reparented to init, outlives the row while the row stays open.

const (
	sweepRowID   = "gc-1"
	sweepRowName = "worker"
)

// sweepStopAt is the fixtures' slept_at; sweepBoot is the fake host's boot.
var (
	sweepStopAt = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	sweepBoot   = sweepStopAt.Add(-time.Hour)
)

// sweepRow is open session row sweepRowID, named sweepRowName, at state and
// generation, stopped at slept (slept_at; "" for none). extra sets more
// metadata, e.g. suspended_at.
func sweepRow(state, generation, slept string, extra ...string) beads.Bead {
	md := map[string]string{"session_name": sweepRowName, "state": state, "generation": generation}
	if slept != "" {
		md["slept_at"] = slept
	}
	for i := 0; i+1 < len(extra); i += 2 {
		md[extra[i]] = extra[i+1]
	}
	return beads.Bead{ID: sweepRowID, Status: "open", Type: sessionBeadType, Metadata: md}
}

var sweepStopped = sweepStopAt.Format(time.RFC3339)

// sweepRead is one read of the row. A read that is not open still carries
// the row's decoded fields, so a verdict that skipped the openness test would
// see matching generations.
func sweepRead(open bool, row beads.Bead) processRowRead {
	return processRowRead{open: open, info: sessionInfoFromBead(row)}
}

// sweepRoot is a reparented root of the row at epoch, started at started.
func sweepRoot(epoch int, started time.Time) runtime.LiveRuntime {
	return runtime.LiveRuntime{SessionID: sweepRowID, PID: 3002, PPID: 1, Epoch: epoch, StartIdentity: "1", StartedAt: started}
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

// incarnationCase is one row of TestProcessRootIncarnationOver.
type incarnationCase struct {
	name       string
	live       runtime.LiveRuntime
	snap, row  processRowRead
	tracking   bool
	noLivePane func(string) bool
	want       string
}

// Kills, one row each: dropping rule 1 or 2; reaping on one read's word
// (openness, generation, name or stop timestamp); reaping with unknown
// tracking, a missing epoch, a newer epoch or an unknown start time; reaping a
// root started at or after the stop, or within the start-time slack; reading
// suspended_at before slept_at; dropping either read's live-runtime claim
// (active, creating, start-pending, draining) or the inventory's live-pane test.
func TestProcessRootIncarnationOver(t *testing.T) {
	noPane := func(string) bool { return true }
	livePane := func(string) bool { return false }
	asleep := sweepRow("asleep", "4", sweepStopped)
	before := sweepStopAt.Add(-time.Minute)
	cases := []incarnationCase{
		{"rule 1: closed or absent in both reads", sweepRoot(0, time.Time{}), processRowRead{}, processRowRead{}, false, livePane, processOrphanRowGone},
		{"snapshot open, store closed: disagree", sweepRoot(4, before), sweepRead(true, asleep), sweepRead(false, asleep), true, noPane, ""},
		{"store open, snapshot closed: disagree", sweepRoot(4, before), sweepRead(false, asleep), sweepRead(true, asleep), true, noPane, ""},
		{"rule 2: stopped row, root before the stop", sweepRoot(4, before), sweepRead(true, asleep), sweepRead(true, asleep), true, noPane, processOrphanRowStopped},
		{"rule 2: older epoch, root before the stop", sweepRoot(3, before), sweepRead(true, asleep), sweepRead(true, asleep), true, noPane, processOrphanRowStopped},
		{"rule 2: suspended row", sweepRoot(4, before), sweepRead(true, sweepRow("suspended", "4", "", "suspended_at", sweepStopped)), sweepRead(true, sweepRow("suspended", "4", "", "suspended_at", sweepStopped)), true, noPane, processOrphanRowStopped},
		{"tracking unknown", sweepRoot(4, before), sweepRead(true, asleep), sweepRead(true, asleep), false, noPane, ""},
		{"missing epoch", sweepRoot(0, before), sweepRead(true, asleep), sweepRead(true, asleep), true, noPane, ""},
		{"newer epoch", sweepRoot(5, before), sweepRead(true, asleep), sweepRead(true, asleep), true, noPane, ""},
		{"unknown start time", sweepRoot(4, time.Time{}), sweepRead(true, asleep), sweepRead(true, asleep), true, noPane, ""},
		{"root started after the stop (resume, pre_start)", sweepRoot(4, sweepStopAt.Add(time.Second)), sweepRead(true, asleep), sweepRead(true, asleep), true, noPane, ""},
		{"root started within the slack", sweepRoot(4, sweepStopAt.Add(-processStartSlack)), sweepRead(true, asleep), sweepRead(true, asleep), true, noPane, ""},
		{"root started just past the slack", sweepRoot(4, sweepStopAt.Add(-processStartSlack-time.Millisecond)), sweepRead(true, asleep), sweepRead(true, asleep), true, noPane, processOrphanRowStopped},
		{"no stop timestamp", sweepRoot(4, before), sweepRead(true, sweepRow("asleep", "4", "")), sweepRead(true, sweepRow("asleep", "4", "")), true, noPane, ""},
		{"unparseable stop timestamp", sweepRoot(4, before), sweepRead(true, sweepRow("asleep", "4", "yesterday")), sweepRead(true, sweepRow("asleep", "4", "yesterday")), true, noPane, ""},
		{"stop timestamps disagree", sweepRoot(4, before), sweepRead(true, sweepRow("asleep", "4", sweepStopAt.Add(-time.Hour).Format(time.RFC3339))), sweepRead(true, asleep), true, noPane, ""},
		{
			"slept_at is read before suspended_at", sweepRoot(4, before),
			sweepRead(true, sweepRow("asleep", "4", sweepStopAt.Add(-2*time.Minute).Format(time.RFC3339), "suspended_at", sweepStopped)),
			sweepRead(true, sweepRow("asleep", "4", sweepStopAt.Add(-2*time.Minute).Format(time.RFC3339), "suspended_at", sweepStopped)), true, noPane, "",
		},
		{"generations disagree", sweepRoot(4, before), sweepRead(true, sweepRow("asleep", "5", sweepStopped)), sweepRead(true, asleep), true, noPane, ""},
		{"generation unparseable", sweepRoot(4, before), sweepRead(true, sweepRow("asleep", "x", sweepStopped)), sweepRead(true, sweepRow("asleep", "x", sweepStopped)), true, noPane, ""},
		{"live pane listed", sweepRoot(4, before), sweepRead(true, asleep), sweepRead(true, asleep), true, livePane, ""},
	}
	for _, state := range []string{"active", "awake", "creating", string(session.StateStartPending), "draining"} {
		claiming := sweepRow(state, "4", sweepStopped)
		cases = append(cases,
			incarnationCase{"store claims a runtime: " + state, sweepRoot(4, before), sweepRead(true, asleep), sweepRead(true, claiming), true, noPane, ""},
			incarnationCase{"snapshot claims a runtime: " + state, sweepRoot(4, before), sweepRead(true, claiming), sweepRead(true, asleep), true, noPane, ""},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := processRootIncarnationOver(tc.live, tc.snap, tc.row, tc.tracking, tc.noLivePane); got != tc.want {
				t.Fatalf("processRootIncarnationOver = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("rule 2 reads the inventory for the agreed name only", func(t *testing.T) {
		renamed := sweepRead(true, asleep)
		renamed.info.SessionName = "other"
		var asked []string
		probe := func(name string) bool { asked = append(asked, name); return true }
		if got := processRootIncarnationOver(sweepRoot(4, before), renamed, sweepRead(true, asleep), true, probe); got != "" {
			t.Fatalf("names disagree: verdict %q, want none", got)
		}
		if got := processRootIncarnationOver(sweepRoot(4, before), sweepRead(true, asleep), sweepRead(true, asleep), true, probe); got != processOrphanRowStopped {
			t.Fatalf("names agree: verdict %q, want %q", got, processOrphanRowStopped)
		}
		if !slices.Equal(asked, []string{sweepRowName}) {
			t.Fatalf("inventory asked for %v, want only %q", asked, sweepRowName)
		}
	})
}

// Kills: reaping the controller itself, a root with no start identity to bind
// the kill to, a root whose parent is live or unreported (PPID 0), a root
// under tmux, or missing the user-subreaper topology.
func TestProcessRootReapable(t *testing.T) {
	const self, subreaper = 77, 900
	root := func(pid, ppid int, identity string, tmuxParent bool) runtime.LiveRuntime {
		return runtime.LiveRuntime{PID: pid, PPID: ppid, StartIdentity: identity, ParentIsProviderInfrastructure: tmuxParent}
	}
	cases := []struct {
		name      string
		live      runtime.LiveRuntime
		subreaper int
		want      bool
	}{
		{"reparented to init", root(10, 1, "s", false), 0, true},
		{"reparented to the user subreaper", root(10, subreaper, "s", false), subreaper, true},
		{"parent unreported", root(10, 0, "s", false), 0, false},
		{"parent unreported, subreaper detected", root(10, 0, "s", false), subreaper, false},
		{"live non-subreaper parent", root(10, 4242, "s", false), subreaper, false},
		{"live parent, no subreaper detected", root(10, subreaper, "s", false), 0, false},
		{"tmux parent", root(10, 4242, "s", true), subreaper, false},
		{"tmux parent misdetected as the subreaper", root(10, subreaper, "s", true), subreaper, false},
		{"tmux mark on an init parent", root(10, 1, "s", true), 0, false},
		{"no start identity", root(10, 1, "", false), 0, false},
		{"the controller itself", root(self, 1, "s", false), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := processRootReapable(tc.live, self, tc.subreaper); got != tc.want {
				t.Fatalf("processRootReapable(%+v, self=%d, subreaper=%d) = %v, want %v", tc.live, self, tc.subreaper, got, tc.want)
			}
		})
	}
}

// Kills: a complete-pass test that accepts a partial, failed, unattested,
// merged-error or stale pass, a nil view, a live or unknown pane, a corpse
// fact from a probe rather than the pass (finding 5), or a name a probe listed
// after the pass; and one that refuses a gone name or the pass's own corpse.
func TestRuntimeInventoryViewNoLivePane(t *testing.T) {
	stale := sweepView(nil, completeBackend("tmux"))
	stale.now = censusNow.Add(observeMaxAge + time.Millisecond)
	merged := &runtimeInventoryView{
		snap:   newObserveCache().publishMerged(censusNow, errors.New("list failed"), nil, completeBackend("tmux")),
		now:    censusNow,
		maxAge: observeMaxAge,
	}
	noted := func(attrs map[string]InventoryAttrs, kind FactKind, v ObsFact, names ...string) *runtimeInventoryView {
		c := newObserveCache()
		c.publish(censusNow, attrs, completeBackend("tmux", names...))
		c.Note(sweepRowName, kind, v, censusNow.Add(time.Second), SourceProbe, "")
		return &runtimeInventoryView{snap: c.Snapshot(), now: censusNow.Add(time.Second), maxAge: observeMaxAge}
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
		{"corpse fact from a later probe", noted(livePaneAttrs(), FactRunning, ObsNo, sweepRowName), false},
		{"listed by a probe after the pass", noted(nil, FactListed, ObsYes), false},
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
// PPID, the tmux-parent mark, the epoch and the start time come from the
// scan, as in production.
type procSweep struct {
	t        *testing.T
	cityPath string
	root     string
	tracked  map[string]bool
	trackErr error
}

func newProcSweep(t *testing.T) *procSweep {
	t.Helper()
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	s := &procSweep{t: t, cityPath: t.TempDir(), root: t.TempDir()}
	if err := os.WriteFile(filepath.Join(s.root, "stat"), []byte("btime "+strconv.FormatInt(sweepBoot.Unix(), 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proctable.SetScanRootForTesting(t, s.root)
	return s
}

// tmuxServer writes a tmux server process, never a root itself.
func (s *procSweep) tmuxServer(pid int) {
	writeFakeProcEntry(s.t, s.root, pid, 1, "tmux: server", []string{"PATH=/usr/bin"})
}

// process writes a process of session sweepRowID at epoch under ppid,
// started at started. An epoch of "" writes no GC_RUNTIME_EPOCH.
func (s *procSweep) process(pid, ppid int, comm, epoch string, started time.Time) {
	env := []string{"PATH=/usr/bin", "GC_CITY_PATH=" + s.cityPath, "GC_SESSION_ID=" + sweepRowID}
	if epoch != "" {
		env = append(env, "GC_RUNTIME_EPOCH="+epoch)
	}
	writeFakeProcEntry(s.t, s.root, pid, ppid, comm, env)
	writeFakeProcStartTime(s.t, s.root, pid, ppid, comm, int(started.Sub(sweepBoot)/(10*time.Millisecond)))
}

// sweep runs the sweep with snapshot rows snap, the store's rows live and
// inventory view inv, and returns the terminated pids and the log.
func (s *procSweep) sweep(snap, live []beads.Bead, inv *runtimeInventoryView) ([]int, string) {
	s.t.Helper()
	sp := &procfsSweepScanner{Fake: runtime.NewFake(), tracked: s.tracked, trackErr: s.trackErr}
	var stderr bytes.Buffer
	got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(snap), inv, beads.NewMemStoreFrom(0, live, nil), s.cityPath, &stderr)
	pids := make([]int, 0, len(sp.terminated))
	for _, r := range sp.terminated {
		pids = append(pids, r.PID)
	}
	if got != len(pids) {
		s.t.Fatalf("sweep reported %d reaped, terminated %v; stderr=%q", got, pids, stderr.String())
	}
	return pids, stderr.String()
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

func pidsOf(pids []int, _ string) []int { return pids }

// The row stopped at sweepStopAt: asleep, generation 4. The leaked child
// started a minute earlier, in the incarnation that stopped.
var (
	sweepAsleep  = []beads.Bead{sweepRow("asleep", "4", sweepStopped)}
	sweepAwake   = []beads.Bead{sweepRow("active", "4", "")}
	sweepLeakAt  = sweepStopAt.Add(-time.Minute)
	sweepClosed  = []beads.Bead{{ID: sweepRowID, Status: "closed"}}
	sweepNoPanes = func() *runtimeInventoryView { return sweepView(nil, completeBackend("tmux")) }
)

// J14 B2: a stopped row stays open (asleep at its generation) and the setsid
// child its runtime leaked, reparented to init, lives on. Legacy reaped it
// only once the row closed. It is reaped once both reads agree the row is
// stopped and a complete pass lists no live pane; while either read still has
// the row awake, the reads disagree and it lives. A leaked root with no
// GC_RUNTIME_EPOCH waits for the row to close.
func TestProcessSweep_StoppedOpenRowLeakedChildReapedAfterTwoReads(t *testing.T) {
	s := newProcSweep(t)
	s.process(3002, 1, "sleep", "4", sweepLeakAt)
	s.process(3003, 1, "sleep", "", sweepLeakAt)

	wantPIDs(t, "snapshot still awake", pidsOf(s.sweep(sweepAwake, sweepAsleep, sweepNoPanes())))
	wantPIDs(t, "store still awake", pidsOf(s.sweep(sweepAsleep, sweepAwake, sweepNoPanes())))
	wantPIDs(t, "both reads stopped", pidsOf(s.sweep(sweepAsleep, sweepAsleep, sweepNoPanes())), 3002)
	wantPIDs(t, "row closed", pidsOf(s.sweep(nil, sweepClosed, nil)), 3002, 3003)
}

// An older epoch alone is not proof: warm reuse, SessionExists convergence
// and S4's rekey keep a live runtime below the row's generation. A reparented
// root at an older epoch of an awake row is never reaped, even when a tmux
// listing failed; once the row is stopped and the root predates the stop, it is.
func TestProcessSweep_OlderEpochOfAwakeRowNeverReaped(t *testing.T) {
	s := newProcSweep(t)
	s.tmuxServer(3000)
	s.process(3001, 3000, "bash", "4", sweepLeakAt)
	s.process(3002, 1, "node", "4", sweepLeakAt)
	awake := []beads.Bead{sweepRow("active", "5", "")}

	wantPIDs(t, "awake at generation 5", pidsOf(s.sweep(awake, awake, sweepView(livePaneAttrs(), completeBackend("tmux", sweepRowName)))))
	wantPIDs(t, "awake, no pass", pidsOf(s.sweep(awake, awake, nil)))
	s.trackErr = errors.New("tmux list running: timed out")
	wantPIDs(t, "awake, tracking failed", pidsOf(s.sweep(awake, awake, sweepNoPanes())))
	s.trackErr = nil
	stopped := []beads.Bead{sweepRow("asleep", "5", sweepStopped)}
	wantPIDs(t, "stopped at generation 5", pidsOf(s.sweep(stopped, stopped, sweepNoPanes())), 3002)
}

// A D8 resume or a pre_start daemon starts processes at the unchanged
// generation before the row leaves asleep. A root started after the row's
// stop is never reaped by rule 2.
func TestProcessSweep_RootNewerThanStopNeverReaped(t *testing.T) {
	s := newProcSweep(t)
	s.process(3002, 1, "daemon", "4", sweepStopAt.Add(30*time.Second))
	s.process(3003, 1, "daemon", "4", sweepStopAt.Add(-time.Second))

	wantPIDs(t, "started after or just before the stop", pidsOf(s.sweep(sweepAsleep, sweepAsleep, sweepNoPanes())))
	suspended := []beads.Bead{sweepRow("suspended", "4", "", "suspended_at", sweepStopAt.Add(time.Minute).Format(time.RFC3339))}
	wantPIDs(t, "suspended after both started", pidsOf(s.sweep(suspended, suspended, sweepNoPanes())), 3002, 3003)
}

// A stopped row whose name the last complete pass lists with a live pane is
// not over: a leaked child of that pane is spared until a pass lists no live
// pane (a corpse or no listing).
func TestProcessSweep_LivePaneChildNeverReaped(t *testing.T) {
	s := newProcSweep(t)
	s.tmuxServer(3000)
	s.process(3001, 3000, "bash", "4", sweepLeakAt)
	s.process(3002, 1, "node", "4", sweepLeakAt)

	wantPIDs(t, "awake row, live pane", pidsOf(s.sweep(sweepAwake, sweepAwake, sweepView(livePaneAttrs(), completeBackend("tmux", sweepRowName)))))
	wantPIDs(t, "stopped row, live pane", pidsOf(s.sweep(sweepAsleep, sweepAsleep, sweepView(livePaneAttrs(), completeBackend("tmux", sweepRowName)))))
	wantPIDs(t, "stopped row, pane unknown", pidsOf(s.sweep(sweepAsleep, sweepAsleep, sweepView(nil, completeBackend("tmux", sweepRowName)))))
	wantPIDs(t, "stopped row, corpse", pidsOf(s.sweep(sweepAsleep, sweepAsleep, sweepView(corpseAttrs(), completeBackend("tmux", sweepRowName)))), 3002)
}

// A detached handoff runs in its own tmux session (gc.detached), so its root
// is parented to a tmux server; it is never reaped, under either rule, while a
// reparented root of the same session is.
func TestProcessSweep_DetachedHandoffNeverReaped(t *testing.T) {
	s := newProcSweep(t)
	s.tmuxServer(3100)
	s.process(3101, 3100, "bash", "4", sweepLeakAt)
	s.process(3002, 1, "node", "4", sweepLeakAt)

	wantPIDs(t, "row closed", pidsOf(s.sweep(nil, sweepClosed, nil)), 3002)
	wantPIDs(t, "row absent", pidsOf(s.sweep(nil, nil, nil)), 3002)
	wantPIDs(t, "row stopped", pidsOf(s.sweep(sweepAsleep, sweepAsleep, sweepNoPanes())), 3002)
}

// Rule 2 rests on a complete pass. Without one (no view, a partial, failed or
// unattested listing, a failed merged listing, or a stale pass) the stopped
// row's leaked child is left for a later sweep.
func TestProcessSweep_IncompletePassReapsNothing(t *testing.T) {
	s := newProcSweep(t)
	s.process(3002, 1, "sleep", "4", sweepLeakAt)
	stale := sweepNoPanes()
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
		wantPIDs(t, tc.name, pidsOf(s.sweep(sweepAsleep, sweepAsleep, tc.inv)))
	}
}

// A root still parented to a tmux server (an agent pane the provider does
// not track, e.g. after a failed listing) is never reaped, whichever rule
// would call its incarnation over.
func TestProcessSweep_TmuxParentedRootNeverReaped(t *testing.T) {
	s := newProcSweep(t)
	s.tmuxServer(3000)
	s.process(3001, 3000, "bash", "4", sweepLeakAt)
	wantPIDs(t, "rule 1: row closed", pidsOf(s.sweep(nil, sweepClosed, sweepNoPanes())))
	wantPIDs(t, "rule 2: row stopped", pidsOf(s.sweep(sweepAsleep, sweepAsleep, sweepNoPanes())))
}

// Tracking fails closed. A root the provider tracks is never reaped. When the
// provider's listing or a per-session GC_SESSION_ID read failed, a live
// runtime's roots can read untracked, so the pass admits only rule 1;
// unreadable process entries do not count as such a failure.
func TestProcessSweep_TrackingFailsClosed(t *testing.T) {
	s := newProcSweep(t)
	s.process(3002, 1, "sleep", "4", sweepLeakAt)

	s.tracked = map[string]bool{sweepRowID: true}
	wantPIDs(t, "tracked, row closed", pidsOf(s.sweep(nil, sweepClosed, nil)))
	wantPIDs(t, "tracked, row stopped", pidsOf(s.sweep(sweepAsleep, sweepAsleep, sweepNoPanes())))
	s.tracked = nil

	s.trackErr = errors.New("tmux tracking: reading GC_SESSION_ID from session \"worker\": server busy")
	wantPIDs(t, "tracking failed, row stopped", pidsOf(s.sweep(sweepAsleep, sweepAsleep, sweepNoPanes())))
	wantPIDs(t, "tracking failed, row closed", pidsOf(s.sweep(nil, sweepClosed, nil)), 3002)

	s.trackErr = &proctable.EntryError{PID: 9, Err: errors.New("permission denied")}
	wantPIDs(t, "unreadable entry only, row stopped", pidsOf(s.sweep(sweepAsleep, sweepAsleep, sweepNoPanes())), 3002)
}

// gc's detached nudge poller inherits its spawner's session env and outlives
// it by design; it is fenced as city infrastructure, under either rule.
func TestProcessSweep_NudgePollerFenced(t *testing.T) {
	s := newProcSweep(t)
	s.process(3002, 1, "gc", "4", sweepLeakAt)
	writeFakeProcCmdline(t, s.root, 3002, "/usr/local/bin/gc", "nudge", "poll", "--city", s.cityPath, "--session", sweepRowName, "mayor")
	t.Cleanup(func() { fencedInfrastructureRoots.put(normalizePathForCompare(s.cityPath), nil) })

	pids, log := s.sweep(nil, sweepClosed, nil)
	wantPIDs(t, "row closed", pids)
	if !strings.Contains(log, "leaving process-table root pid=3002") {
		t.Fatalf("stderr %q does not report the fenced poller", log)
	}
	wantPIDs(t, "row stopped", pidsOf(s.sweep(sweepAsleep, sweepAsleep, sweepNoPanes())))
}

// The process guard reaches the sweep: of roots of a closed row, only one
// reparented to init with a start identity, not the controller, is reaped
// (the fake scanner reports the fields directly).
func TestProcessSweep_ProcessGuardReachesSweep(t *testing.T) {
	sp := newProcessTableSweepProvider(
		runtime.LiveRuntime{SessionID: sweepRowID, PID: 601, PPID: 1, StartIdentity: "s"},
		runtime.LiveRuntime{SessionID: sweepRowID, PID: 602, PPID: 1, StartIdentity: "s", ParentIsProviderInfrastructure: true},
		runtime.LiveRuntime{SessionID: sweepRowID, PID: 603, PPID: 4242, StartIdentity: "s"},
		runtime.LiveRuntime{SessionID: sweepRowID, PID: 604, PPID: 0, StartIdentity: "s"},
		runtime.LiveRuntime{SessionID: sweepRowID, PID: 605, PPID: 1},
		runtime.LiveRuntime{SessionID: sweepRowID, PID: os.Getpid(), PPID: 1, StartIdentity: "s"},
	)
	var stderr bytes.Buffer
	store := beads.NewMemStoreFrom(0, sweepClosed, nil)
	if got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, store, "", &stderr); got != 1 {
		t.Fatalf("swept %d, want 1; stderr=%q", got, stderr.String())
	}
	if len(sp.terminated) != 1 || sp.terminated[0].PID != 601 {
		t.Fatalf("terminated %+v, want only pid 601", sp.terminated)
	}
}
