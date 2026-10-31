package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// The armed nonces the read-path tests use: each names its pane.
const (
	readNonce      = "rAnd0m.%5"
	readSplitNonce = "spl1t.%4"
	readOldNonce   = "0ldInc.%5"
)

// psRecord encodes a published record under nonce in state (a root report
// when state is set), with mutate applied before encoding.
func psRecord(t *testing.T, nonce string, state runtime.ProgramState, mutate ...func(*runtime.ProgramStatus)) string {
	t.Helper()
	s := runtime.ProgramStatus{Nonce: nonce, State: state, Progress: -1, Seq: 1}
	if state != "" {
		s.Reported, s.Since = true, time.UnixMilli(1793408464412)
		if state == runtime.ProgramWorking {
			s.TurnStarts = 1
		}
	}
	for _, m := range mutate {
		m(&s)
	}
	v, err := programstatus.Encode(s)
	if err != nil {
		t.Fatalf("encoding record: %v", err)
	}
	return v
}

// capturePane is one pane's programPaneFormat fields as tmux prints them.
type capturePane struct {
	mark, id, pipe, tap, record string
	dead                        bool
}

func (c capturePane) fields() []string {
	return []string{c.mark, c.id, c.pipe, c.tap, c.record}
}

func (c capturePane) deadField() string {
	if c.dead {
		return "1"
	}
	return "0"
}

// armedPane is session's marked agent pane %5 with its pipe open, armed
// under readNonce and carrying record.
func armedPane(record string) capturePane {
	return capturePane{mark: "agent", id: "%5", pipe: "1", tap: readNonce, record: record}
}

// fetchRows renders programStateFetchFormat rows for session "s"'s panes.
func fetchRows(panes ...capturePane) string {
	var lines []string
	for _, p := range panes {
		row := []string{"s", p.deadField(), "emitter", "100", "0", "1700000000", "$1", "1700000000"}
		row = append(row, p.fields()...)
		lines = append(lines, strings.Join(append(row, runtimeInventoryEnd), "\t"))
	}
	return strings.Join(lines, "\n")
}

// inventoryRows renders programRuntimeInventoryFormat rows for session's
// panes, the first at window.pane index 0.0, the next at 0.1 and so on.
func inventoryRows(session string, panes ...capturePane) []string {
	var lines []string
	for i, p := range panes {
		row := []string{session, "$1", "1700000000", "0", "0." + string(rune('0'+i)), p.deadField(), "100", "1700000100", "0"}
		row = append(row, p.fields()...)
		lines = append(lines, strings.Join(row, "\t"))
	}
	return lines
}

// tapArgs is a running tap's argv as ps prints it.
func tapArgs(nonce string) string {
	rand, pane, _ := strings.Cut(nonce, ".")
	return "/usr/local/bin/gc-osc7501 tap -t " + pane + " -n " + rand + " -x /usr/bin/tmux -S /tmp/tmux-1000/city"
}

// tapTable is a process table holding a tap for each nonce, beside an
// unrelated process.
func tapTable(nonces ...string) processSnapshot {
	processes := []processRuntimeState{{PID: "1", PPID: "0", Command: "init", Args: "/sbin/init"}}
	for i, n := range nonces {
		processes = append(processes, processRuntimeState{PID: string(rune('a' + i)), PPID: "1", Command: programstatus.CompanionName, Args: tapArgs(n)})
	}
	return newProcessSnapshot(processes)
}

// passGate is a capability gate that passes without a companion or server.
func passGate() (captureCapability, error) { return captureCapability{}, nil }

// readProvider is a tmux provider under mode whose list-panes answers out
// and whose process table is table (nil: the table cannot be read), with
// the capture gate passed.
func readProvider(mode runtime.ProgramStatusMode, out string, table *processSnapshot) (*Provider, *fakeExecutor) {
	fe := &fakeExecutor{out: out}
	tm := &Tmux{cfg: Config{ProgramStatus: mode}, exec: fe}
	tm.capture.probe = passGate
	snapshot := func(context.Context) (processSnapshot, error) {
		if table == nil {
			return processSnapshot{}, errors.New("ps timed out")
		}
		return *table, nil
	}
	p := &Provider{tm: tm, cfg: tm.cfg, workDirs: map[string]string{}}
	p.cache = NewStateCache(&tmuxFetcher{tm: tm, snapshot: snapshot}, time.Hour)
	return p, fe
}

// readStatus reads session "s" through ProgramStatus.
func readStatus(t *testing.T, mode runtime.ProgramStatusMode, out string, table *processSnapshot) runtime.ProgramStatus {
	t.Helper()
	p, _ := readProvider(mode, out, table)
	r, err := p.ProgramStatus("s")
	if err != nil {
		t.Fatalf("ProgramStatus: %v", err)
	}
	return r
}

// Kills a read taken from another pane than the session's agent pane: the
// lowest window.pane index (a human split there carries its own armed
// record), or, for an unmarked session of several panes, any pane at all. An
// unmarked single-pane session reads its only pane.
func TestParseRuntimeInventoryReadsAgentPaneRecordNotSplit(t *testing.T) {
	split := capturePane{mark: "orphan", id: "%4", pipe: "1", tap: readSplitNonce, record: psRecord(t, readSplitNonce, runtime.ProgramIdle)}
	agent := armedPane(psRecord(t, readNonce, runtime.ProgramWorking))
	unmarkedA := capturePane{mark: "pane", id: "%7", pipe: "1", tap: "uA.%7", record: psRecord(t, "uA.%7", runtime.ProgramIdle)}
	unmarkedB := capturePane{mark: "pane", id: "%8", pipe: "1", tap: "uB.%8", record: psRecord(t, "uB.%8", runtime.ProgramIdle)}
	single := capturePane{mark: "pane", id: "%9", pipe: "1", tap: "sOlo.%9", record: psRecord(t, "sOlo.%9", runtime.ProgramDone)}
	var lines []string
	lines = append(lines, inventoryRows("s", split, agent)...)
	lines = append(lines, inventoryRows("unmarked", unmarkedA, unmarkedB)...)
	lines = append(lines, inventoryRows("single", single)...)

	table := tapTable(readNonce, readSplitNonce, "uA.%7", "uB.%8", "sOlo.%9")
	p, fe := readProvider(runtime.ProgramStatusOn, inventoryLines(lines...), &table)
	p.cache = NewStateCache(&mockFetcher{state: runtimeStateSnapshot{
		Sessions:    map[string]sessionRuntimeState{},
		ProgramRead: true, ProcessesAvailable: true, ProgramTaps: tapNonces(table),
	}}, time.Hour)
	got, err := p.RuntimeInventory(context.Background())
	if err != nil {
		t.Fatalf("RuntimeInventory: %v", err)
	}
	if want := []string{"-u", "list-panes", "-a", "-F", programRuntimeInventoryFormat}; len(fe.calls) != 1 || !slices.Equal(fe.calls[0], want) {
		t.Fatalf("tmux calls = %q, want one %q", fe.calls, want)
	}
	if s := got["s"].Program; !s.Running() || s.Nonce != readNonce || !s.ReaderVerified || s.Mode != runtime.ProgramStatusOn {
		t.Errorf("marked session reads %+v, want the agent pane's verified working record under %s", s, readNonce)
	}
	if got["s"].Incarnation != "$1:1700000000:100" {
		t.Errorf("capture fields moved the incarnation: %+v", got["s"])
	}
	if s := got["unmarked"].Program; s.Validity != runtime.ProgramStatusNotArmed {
		t.Errorf("unmarked two-pane session reads %+v, want not_armed: no pane can be named", s)
	}
	if s := got["single"].Program; !s.Valid() || s.State != runtime.ProgramDone || s.Nonce != "sOlo.%9" {
		t.Errorf("unmarked single-pane session reads %+v, want its pane's done record", s)
	}

	// The state cache folds the same way.
	cached := readStatus(t, runtime.ProgramStatusOn, fetchRows(split, agent), &table)
	if !cached.Running() || cached.Nonce != readNonce {
		t.Errorf("state cache reads %+v, want the agent pane's working record", cached)
	}
}

// Kills a judgement that trusts an inherited tap nonce: a window-, session-
// or global-scope @gc_ps_tap (and @gc_ps) is inherited onto every pane that
// has none of its own, but names another pane, so the pane is not armed.
func TestInheritedGlobalTapOptionIsNotArmed(t *testing.T) {
	const global = "gL0bal.%9"
	table := tapTable(global)
	inherited := capturePane{mark: "agent", id: "%5", pipe: "0", tap: global, record: psRecord(t, global, runtime.ProgramWorking)}
	r := readStatus(t, runtime.ProgramStatusOn, fetchRows(inherited), &table)
	if r.Validity != runtime.ProgramStatusNotArmed || r.State != "" || r.Valid() {
		t.Fatalf("inherited global nonce reads %+v, want not_armed with no state", r)
	}

	// Our own nonce with an inherited record of another incarnation reads
	// silent: the record is compared to the nonce exactly, in Go.
	table = tapTable(readNonce)
	foreignRecord := armedPane(psRecord(t, global, runtime.ProgramWorking))
	if r := readStatus(t, runtime.ProgramStatusOn, fetchRows(foreignRecord), &table); r.Validity != runtime.ProgramStatusSilent || r.Valid() {
		t.Fatalf("own nonce, inherited record reads %+v, want silent", r)
	}
}

// Kills a reader liveness taken from the pipe alone: a tap that died leaves
// pane_pipe at 1 until the pane next writes, so a process table without the
// tap reads reader_lost. An unreadable process table cannot say, so the
// reading stays valid but unverified.
func TestDeadTapProcessReadsReaderLost(t *testing.T) {
	out := fetchRows(armedPane(psRecord(t, readNonce, runtime.ProgramWorking)))
	other := tapTable("0ther.%5", "rAnd0m.%6")
	if r := readStatus(t, runtime.ProgramStatusOn, out, &other); r.Validity != runtime.ProgramStatusReaderLost || r.Valid() {
		t.Fatalf("no tap holding the nonce: %+v, want reader_lost", r)
	}
	alive := tapTable(readNonce)
	if r := readStatus(t, runtime.ProgramStatusOn, out, &alive); !r.Running() || !r.ReaderVerified {
		t.Fatalf("tap alive: %+v, want a verified working reading", r)
	}
	if r := readStatus(t, runtime.ProgramStatusOn, out, nil); !r.Running() || r.ReaderVerified {
		t.Fatalf("process table unavailable: %+v, want a working reading, unverified", r)
	}
}

// Kills a kill switch that leaks: with the switch off, armed panes and live
// taps read Unread with no state, from the snapshot or the inventory, and
// nothing judges them. A reading carries the mode it was taken under.
func TestSwitchOffIgnoresArmedRecords(t *testing.T) {
	table := tapTable(readNonce)
	armed := fetchRows(armedPane(psRecord(t, readNonce, runtime.ProgramWorking)))
	p, fe := readProvider(runtime.ProgramStatusOff, armed, &table)
	r, err := p.ProgramStatus("s")
	if err != nil {
		t.Fatalf("ProgramStatus: %v", err)
	}
	if r != (runtime.ProgramStatus{Mode: runtime.ProgramStatusOff}) {
		t.Fatalf("switch off reads %+v, want Unread under off", r)
	}
	if len(fe.calls) != 0 {
		t.Fatalf("switch off read tmux: %q", fe.calls)
	}
	if missing, err := p.ProgramStatus("never-started"); err != nil || missing.Validity != runtime.ProgramStatusUnread {
		t.Fatalf("switch off, missing session: %+v, %v; want Unread and no error", missing, err)
	}

	inv, _ := readProvider(runtime.ProgramStatusOff, inventoryLines("s\t$1\t1700000000\t0\t0.0\t0\t100\t1700000100\t0"), &table)
	entries, err := inv.RuntimeInventory(context.Background())
	if err != nil {
		t.Fatalf("RuntimeInventory: %v", err)
	}
	if e := entries["s"]; e.Program != (runtime.ProgramStatus{}) {
		t.Fatalf("switch off inventory entry carries %+v, want the zero reading", e.Program)
	}

	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusShadow, runtime.ProgramStatusOn} {
		if r := readStatus(t, mode, armed, &table); r.Mode != mode || !r.Running() {
			t.Errorf("switch %s reads %+v, want a working reading stamped %s", mode, r, mode)
		}
	}
	shadow := readStatus(t, runtime.ProgramStatusShadow, armed, &table)
	if shadow.Decides() {
		t.Errorf("a shadow reading decides: %+v", shadow)
	}

	// A failed capability gate is Unread too, whatever the panes hold.
	gateless, _ := readProvider(runtime.ProgramStatusOn, armed, &table)
	gateless.tm.capture.probe = func() (captureCapability, error) { return captureCapability{}, errors.New("companion missing") }
	defer swapCaptureWarnings(t)()
	if r, err := gateless.ProgramStatus("s"); err != nil || r.Validity != runtime.ProgramStatusUnread || r.Mode != runtime.ProgramStatusOn {
		t.Fatalf("gate failed: %+v, %v; want Unread under on", r, err)
	}
}

// swapCaptureWarnings discards capture warnings until the returned restore.
func swapCaptureWarnings(t *testing.T) func() {
	t.Helper()
	old := captureWarningOut
	captureWarningOut = &strings.Builder{}
	return func() { captureWarningOut = old }
}

// Kills a read path that misjudges a staleness situation (design §7.2): each
// row is one agent-pane observable and the reading it must give.
func TestProgramStatusGapTable(t *testing.T) {
	alive := tapTable(readNonce)
	gone := tapTable()
	for _, tc := range []struct {
		name     string
		pane     capturePane
		table    *processSnapshot
		want     runtime.ProgramStatusValidity
		state    runtime.ProgramState
		verified bool
		check    func(runtime.ProgramStatus) bool
	}{
		{name: "login screen or legacy build: own nonce, nothing published", pane: armedPane(""), table: &alive, want: runtime.ProgramStatusSilent},
		{
			name: "probed, unanswered: silent and Probed", pane: armedPane(psRecord(t, readNonce, "", func(s *runtime.ProgramStatus) { s.Probed = true })), table: &alive, want: runtime.ProgramStatusSilent,
			check: func(s runtime.ProgramStatus) bool { return s.Probed },
		},
		{
			name: "stuck working: valid, the hold bounded by HoldLive", pane: armedPane(psRecord(t, readNonce, runtime.ProgramWorking)), table: &alive, want: runtime.ProgramStatusValid, state: runtime.ProgramWorking, verified: true,
			check: func(s runtime.ProgramStatus) bool {
				return s.HoldLive(time.Time{}, s.Since.Add(time.Minute)) && !s.HoldLive(time.Time{}, s.Since.Add(runtime.ProgramStatusHoldLimit))
			},
		},
		{name: "tap died, pane silent: pipe still 1, no tap in ps", pane: armedPane(psRecord(t, readNonce, runtime.ProgramWorking)), table: &gone, want: runtime.ProgramStatusReaderLost},
		{name: "tap died, ps unavailable: valid, unverified", pane: armedPane(psRecord(t, readNonce, runtime.ProgramWorking)), want: runtime.ProgramStatusValid, state: runtime.ProgramWorking},
		{name: "pipe closed", pane: capturePane{mark: "agent", id: "%5", pipe: "0", tap: readNonce, record: psRecord(t, readNonce, runtime.ProgramIdle)}, table: &alive, want: runtime.ProgramStatusReaderLost},
		{
			name: "process exited under remain-on-exit, record still working", pane: capturePane{
				mark: "agent", id: "%5", pipe: "0", tap: readNonce, dead: true,
				record: psRecord(t, readNonce, runtime.ProgramWorking, func(s *runtime.ProgramStatus) { s.LastError = true }),
			}, table: &gone, want: runtime.ProgramStatusExited,
			check: func(s runtime.ProgramStatus) bool { return s.LastError },
		},
		{name: "respawn: leftover record of the old incarnation", pane: armedPane(psRecord(t, readOldNonce, runtime.ProgramWorking)), table: &alive, want: runtime.ProgramStatusSilent},
		{name: "inherited tap nonce naming another pane", pane: capturePane{mark: "agent", id: "%5", pipe: "1", tap: "gL0bal.%9", record: psRecord(t, "gL0bal.%9", runtime.ProgramWorking)}, table: &alive, want: runtime.ProgramStatusNotArmed},
		{name: "undeclared: no nonce", pane: capturePane{mark: "agent", id: "%5", pipe: "0"}, table: &alive, want: runtime.ProgramStatusNotArmed},
		{name: "foreign pipe displaced the tap", pane: armedPane(psRecord(t, readNonce, runtime.ProgramWorking)), table: &gone, want: runtime.ProgramStatusReaderLost},
		{name: "unknown codec version under a live tap", pane: armedPane("v9|" + readNonce + "|1|working"), table: &alive, want: runtime.ProgramStatusReaderLost},
		{name: "undecodable record", pane: armedPane("v1|" + readNonce + "|x"), table: &alive, want: runtime.ProgramStatusSilent},
		{name: "root cleared", pane: armedPane(psRecord(t, readNonce, "", func(s *runtime.ProgramStatus) { s.Reported, s.TurnStarts, s.TurnEnds = true, 1, 1 })), table: &alive, want: runtime.ProgramStatusCleared},
		{
			name: "blocked: neither boundary nor running", pane: armedPane(psRecord(t, readNonce, runtime.ProgramBlocked, func(s *runtime.ProgramStatus) { s.Kind = runtime.ProgramBlockQuestion })), table: &alive, want: runtime.ProgramStatusValid, state: runtime.ProgramBlocked, verified: true,
			check: func(s runtime.ProgramStatus) bool {
				return s.Blocked() && !s.Running() && !s.InputBoundary() && s.Kind == runtime.ProgramBlockQuestion
			},
		},
		{
			name: "error is an input boundary", pane: armedPane(psRecord(t, readNonce, runtime.ProgramError)), table: &alive, want: runtime.ProgramStatusValid, state: runtime.ProgramError, verified: true,
			check: func(s runtime.ProgramStatus) bool { return s.InputBoundary() },
		},
		{
			name: "working child keeps an idle root from quiescence", pane: armedPane(psRecord(t, readNonce, runtime.ProgramIdle, func(s *runtime.ProgramStatus) { s.Children, s.WorkingChildren = 2, 1 })), table: &alive, want: runtime.ProgramStatusValid, state: runtime.ProgramIdle, verified: true,
			check: func(s runtime.ProgramStatus) bool { return s.InputBoundary() && !s.Quiescent() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := readStatus(t, runtime.ProgramStatusOn, fetchRows(tc.pane), tc.table)
			if r.Validity != tc.want || r.State != tc.state || r.ReaderVerified != tc.verified || r.Mode != runtime.ProgramStatusOn {
				t.Fatalf("reads %+v, want validity %q state %q verified %t under on", r, tc.want, tc.state, tc.verified)
			}
			if tc.check != nil && !tc.check(r) {
				t.Fatalf("reads %+v, failing the row's check", r)
			}
		})
	}
}

// Kills a read path that breaks the provider contract's errors: a session
// the snapshot does not hold is ErrSessionNotFound, an unvouched snapshot is
// ErrRuntimeUnavailable, and a malformed capture row names no pane.
func TestProgramStatusReadErrors(t *testing.T) {
	table := tapTable(readNonce)
	p, _ := readProvider(runtime.ProgramStatusOn, fetchRows(armedPane("")), &table)
	if _, err := p.ProgramStatus("missing"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("missing session: %v, want ErrSessionNotFound", err)
	}

	broken, fe := readProvider(runtime.ProgramStatusOn, "", &table)
	fe.err = errors.New("tmux wedged")
	if _, err := broken.ProgramStatus("s"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("unvouched snapshot: %v, want ErrRuntimeUnavailable", err)
	}

	bad := armedPane(psRecord(t, readNonce, runtime.ProgramWorking))
	bad.pipe = "maybe"
	if r := readStatus(t, runtime.ProgramStatusOn, fetchRows(bad), &table); r.Validity != runtime.ProgramStatusNotArmed {
		t.Fatalf("malformed capture row reads %+v, want not_armed", r)
	}
}

// Kills a read format change that reaches the switch-off path: with the
// switch off, the state cache's and the inventory's list-panes formats are
// today's byte for byte; with it on, both carry the capture fields, every
// option sanitized and the row terminated.
func TestSwitchOffReadsAreByteIdentical(t *testing.T) {
	const (
		todayFetch     = "#{session_name}\t#{pane_dead}\t#{pane_current_command}\t#{pane_pid}\t#{session_attached}\t#{window_activity}\t#{session_id}\t#{session_created}"
		todayInventory = "#{session_name}\t#{session_id}\t#{session_created}\t#{session_attached}\t#{window_index}.#{pane_index}\t#{pane_dead}\t#{pane_pid}\t#{window_activity}\t#{!=:#{@GC_RESTART_REQUESTED},}\tgc-eol"
		sanitized      = "#{s/[^-+A-Za-z0-9=_.|%]//:"
	)
	table := tapTable()
	for _, mode := range []runtime.ProgramStatusMode{"", runtime.ProgramStatusOff, "bogus", runtime.ProgramStatusShadow, runtime.ProgramStatusOn} {
		p, fe := readProvider(mode, "", &table)
		if _, err := (&tmuxFetcher{tm: p.tm, snapshot: func(context.Context) (processSnapshot, error) { return table, nil }}).FetchState(context.Background()); err != nil {
			t.Fatalf("%q: FetchState: %v", mode, err)
		}
		if _, _, _, err := p.tm.listRuntimeInventory(context.Background()); err != nil {
			t.Fatalf("%q: listRuntimeInventory: %v", mode, err)
		}
		if len(fe.calls) != 2 {
			t.Fatalf("%q: tmux calls %q, want the state read and the inventory read", mode, fe.calls)
		}
		fetch, inventory := fe.calls[0][len(fe.calls[0])-1], fe.calls[1][len(fe.calls[1])-1]
		if mode != runtime.ProgramStatusShadow && mode != runtime.ProgramStatusOn {
			if fetch != todayFetch || inventory != todayInventory {
				t.Errorf("switch %q: formats\n%q\n%q\nwant today's\n%q\n%q", mode, fetch, inventory, todayFetch, todayInventory)
			}
			continue
		}
		for _, format := range []string{fetch, inventory} {
			if !strings.Contains(format, sanitized+programTapOption+"}") || !strings.Contains(format, sanitized+programStatusOption+"}") ||
				!strings.Contains(format, "#{pane_pipe}") || !strings.HasSuffix(format, "\t"+runtimeInventoryEnd) || strings.Contains(format, programTextOption) {
				t.Errorf("switch %q: format %q does not carry the sanitized capture fields, terminated", mode, format)
			}
		}
		if !strings.HasPrefix(fetch, todayFetch+"\t") || !strings.HasPrefix(inventory, strings.TrimSuffix(todayInventory, runtimeInventoryEnd)) {
			t.Errorf("switch %q: capture fields do not follow today's fields: %q, %q", mode, fetch, inventory)
		}
	}
}

// Kills a tap-liveness index that takes a process for a tap by a stray word,
// misses a tap still in the shell tmux ran it under, or reads another pane's
// or another incarnation's tap as this one.
func TestTapNonceReadsOnlyTapArgv(t *testing.T) {
	for _, tc := range []struct {
		args, nonce string
	}{
		{tapArgs(readNonce), readNonce},
		{"/usr/local/bin/gc-osc7501 tap -t %12 -n x_Y-z -x /usr/bin/tmux -S /tmp/s -a codex", "x_Y-z.%12"},
		{"sh -c exec /usr/local/bin/gc-osc7501 tap -t %3 -n abc -x /usr/bin/tmux -S /tmp/s", "abc.%3"},
		// A listing that cut the argv short after the nonce still names it.
		{"/usr/local/bin/gc-osc7501 tap -t %3 -n abc -x /usr/bi", "abc.%3"},
		{"/usr/local/bin/gc-osc7501 relay -- /bin/sh -c codex", ""},
		{"vim -n notes -t %3", ""},
		{"vim tap -n notes -t %3", ""},
		{"/usr/local/bin/gc-osc7501 tap -t 3 -n abc -x /usr/bin/tmux -S /tmp/s", ""},
		{"/usr/local/bin/gc-osc7501 tap -t %3 -n bad!rand -x /usr/bin/tmux -S /tmp/s", ""},
		{"/usr/local/bin/gc-osc7501 tap -t %3", ""},
		// Only the arm's word order names a nonce.
		{"/usr/local/bin/gc-osc7501 tap -x /usr/bin/tmux -S /tmp/s -t %3 -n abc", ""},
	} {
		got, ok := tapNonce(tc.args)
		if got != tc.nonce || ok != (tc.nonce != "") {
			t.Errorf("tapNonce(%q) = %q, %t; want %q", tc.args, got, ok, tc.nonce)
		}
	}
	taps := programTaps{known: true, nonces: tapNonces(tapTable(readNonce))}
	if taps.liveness(readNonce) != runtime.ProgramTapAlive || taps.liveness("rAnd0m.%6") != runtime.ProgramTapGone || taps.liveness(readOldNonce) != runtime.ProgramTapGone {
		t.Fatalf("liveness misreads the index %v", taps.nonces)
	}
	if (programTaps{}).liveness(readNonce) != runtime.ProgramTapUnknown {
		t.Fatal("an unread process table answers tap liveness")
	}
}

// Kills a fold that guesses: two marked agent panes, or a marked session
// whose agent pane is gone, name no pane.
func TestFoldProgramPaneNeverGuesses(t *testing.T) {
	agent := programPane{mark: "agent", id: "%5"}
	orphan := programPane{mark: "orphan", id: "%4"}
	for _, tc := range []struct {
		name  string
		rows  []programPane
		want  string
		named bool
	}{
		{"marked", []programPane{orphan, agent}, "%5", true},
		{"agent pane gone", []programPane{orphan, orphan}, "", false},
		{"two agent panes", []programPane{agent, agent}, "", false},
		{"only pane", []programPane{{mark: "pane", id: "%1"}}, "%1", true},
		{"several unmarked panes", []programPane{{mark: "pane", id: "%1"}, {mark: "pane", id: "%2"}}, "", false},
		{"no rows", nil, "", false},
	} {
		got, named := foldProgramPane(tc.rows)
		if got.id != tc.want || named != tc.named {
			t.Errorf("%s: fold = %q, %t; want %q, %t", tc.name, got.id, named, tc.want, tc.named)
		}
	}
}

// Kills a provider that keeps the switch it was built with: the controller
// keeps the tmux provider across a reload and publishes each accepted
// config's switch to it, and the readings follow in every direction.
func TestProgramStatusFollowsPublishedSwitch(t *testing.T) {
	table := tapTable(readNonce)
	armed := fetchRows(armedPane(psRecord(t, readNonce, runtime.ProgramWorking)))
	read := func(t *testing.T, p *Provider) runtime.ProgramStatus {
		t.Helper()
		r, err := p.ProgramStatus("s")
		if err != nil {
			t.Fatalf("ProgramStatus: %v", err)
		}
		return r
	}

	t.Run("off to shadow and on", func(t *testing.T) {
		for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusShadow, runtime.ProgramStatusOn} {
			p, fe := readProvider(runtime.ProgramStatusOff, armed, &table)
			// The cache holds a snapshot read under off: no capture fields.
			fe.outs = []string{"s\t0\temitter\t100\t0\t1700000000\t$1\t1700000000"}
			if !p.cache.IsRunning("s") {
				t.Fatal("the off snapshot does not hold the session")
			}
			if r := read(t, p); r.Validity != runtime.ProgramStatusUnread {
				t.Fatalf("under off: %+v, want Unread", r)
			}
			p.SetProgramStatusMode(mode)
			if r := read(t, p); r.Mode != mode || !r.Running() {
				t.Fatalf("published %s: %+v, want a working reading stamped %s", mode, r, mode)
			}
			if got := callsContaining(fe.calls, programPaneFormat); len(got) != 1 {
				t.Fatalf("published %s: %d reads carried the capture fields, want 1: %q", mode, len(got), fe.calls)
			}
		}
	})

	t.Run("on to off", func(t *testing.T) {
		p, _ := readProvider(runtime.ProgramStatusOn, armed, &table)
		if r := read(t, p); !r.Decides() {
			t.Fatalf("under on: %+v, want a deciding reading", r)
		}
		p.SetProgramStatusMode(runtime.ProgramStatusOff)
		if r := read(t, p); r != (runtime.ProgramStatus{Mode: runtime.ProgramStatusOff}) {
			t.Fatalf("published off: %+v, want Unread under off", r)
		}
		if p.tm.programReadEnabled() {
			t.Fatal("published off: the read formats still carry capture fields")
		}
	})

	t.Run("on to shadow and back", func(t *testing.T) {
		p, _ := readProvider(runtime.ProgramStatusOn, armed, &table)
		if r := read(t, p); !r.Decides() {
			t.Fatalf("under on: %+v, want a deciding reading", r)
		}
		p.SetProgramStatusMode(runtime.ProgramStatusShadow)
		if r := read(t, p); r.Mode != runtime.ProgramStatusShadow || !r.Running() || r.Decides() {
			t.Fatalf("published shadow: %+v, want an advisory working reading", r)
		}
		p.SetProgramStatusMode(runtime.ProgramStatusOn)
		if r := read(t, p); !r.Decides() {
			t.Fatalf("published on again: %+v, want a deciding reading", r)
		}
	})

	t.Run("an unknown switch reads off", func(t *testing.T) {
		p, _ := readProvider(runtime.ProgramStatusOn, armed, &table)
		p.SetProgramStatusMode("sideways")
		if r := read(t, p); r.Validity != runtime.ProgramStatusUnread || r.Mode != runtime.ProgramStatusOff {
			t.Fatalf("published an unknown switch: %+v, want Unread under off", r)
		}
	})
}

// Kills a seam-backed provider (the one the controller builds) that hides
// the switch publication its raw provider takes.
func TestSeamBackedTakesTheProgramStatusSwitch(t *testing.T) {
	p := NewSeamBackedWithConfig(Config{ProgramStatus: runtime.ProgramStatusOn})
	setter, ok := p.(interface {
		SetProgramStatusMode(runtime.ProgramStatusMode)
	})
	if !ok {
		t.Fatal("the seam-backed tmux provider does not take the published program-status switch")
	}
	setter.SetProgramStatusMode(runtime.ProgramStatusOff)
	if mode := p.(*seamBackedProvider).tm.programStatusMode(); mode != runtime.ProgramStatusOff {
		t.Fatalf("published off reads %q", mode)
	}
}

// Kills an arm that keeps the switch the Tmux was built with: a respawn
// under a switch published off arms nothing and runs today's command line.
func TestRespawnFollowsPublishedSwitch(t *testing.T) {
	tm, rec, _, _ := captureTestTmux(t)
	tm.cfg.ProgramStatus = runtime.ProgramStatusOn
	tm.SetProgramStatusMode(runtime.ProgramStatusOff)
	if err := tm.respawnPaneWithWorkDir("%4", "/work", "codex", runtime.ProgramStatusArm{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"-u", "respawn-pane", "-k", "-t", "%4", "-c", "/work", "codex"}
	if len(rec.calls) != 1 || !slices.Equal(rec.calls[0], want) {
		t.Fatalf("respawn under a switch published off = %q, want [%q]", rec.calls, want)
	}
}
