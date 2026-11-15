package tmux

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// inventoryLines renders runtimeInventoryFormat lines as tmux prints them:
// each line ends with the terminator field.
func inventoryLines(lines ...string) string {
	return strings.Join(lines, "\t"+runtimeInventoryEnd+"\n") + "\t" + runtimeInventoryEnd
}

func runtimeInventoryFrom(t *testing.T, fe *fakeExecutor) (map[string]runtime.InventoryEntry, error) {
	t.Helper()
	p := NewProviderWithConfig(Config{})
	p.tm.exec = fe
	return p.RuntimeInventory(context.Background())
}

// Kills: a live pane marked dead; an all-dead session not flagged; attach
// inverted or read as "exactly one client"; a multi-pane session misgrouped
// (one entry per pane, or the last pane winning).
func TestTmuxRuntimeInventory_ParsesPanesCorpsesAndAttach(t *testing.T) {
	fe := &fakeExecutor{out: inventoryLines(
		"live\t$1\t1700000001\t0\t0.0\t0\t101\t1700000100\t0",
		"corpse\t$2\t1700000002\t0\t0.0\t1\t201\t1700000100\t0",
		"corpse\t$2\t1700000002\t0\t0.1\t1\t202\t1700000100\t0",
		"mixed\t$3\t1700000003\t1\t0.0\t0\t301\t1700000100\t0",
		"mixed\t$3\t1700000003\t1\t0.1\t1\t302\t1700000100\t0",
		"watched\t$4\t1700000004\t2\t0.0\t0\t401\t1700000100\t0",
		"odd\t$5\t1700000005\tx\t0.0\t?\t501\t1700000100\t0",
	)}

	got, err := runtimeInventoryFrom(t, fe)
	if err != nil {
		t.Fatalf("RuntimeInventory: %v", err)
	}
	act := time.Unix(1700000100, 0)
	want := map[string]runtime.InventoryEntry{
		"live":    {Incarnation: "$1:1700000001:101", DeadKnown: true, AllPanesDead: false, AttachedKnown: true, Attached: false, ActivityKnown: true, LastActivity: act},
		"corpse":  {Incarnation: "$2:1700000002:201", DeadKnown: true, AllPanesDead: true, AttachedKnown: true, Attached: false, ActivityKnown: true, LastActivity: act},
		"mixed":   {Incarnation: "$3:1700000003:301", DeadKnown: true, AllPanesDead: false, AttachedKnown: true, Attached: true, ActivityKnown: true, LastActivity: act},
		"watched": {Incarnation: "$4:1700000004:401", DeadKnown: true, AllPanesDead: false, AttachedKnown: true, Attached: true, ActivityKnown: true, LastActivity: act},
		"odd":     {Incarnation: "$5:1700000005:501", DeadKnown: false, AllPanesDead: false, AttachedKnown: false, Attached: false, ActivityKnown: true, LastActivity: act},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RuntimeInventory =\n%#v\nwant\n%#v", got, want)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("tmux calls = %d, want exactly one batched read", len(fe.calls))
	}
	wantArgs := []string{"-u", "list-panes", "-a", "-F", runtimeInventoryFormat}
	if !slices.Equal(fe.calls[0], wantArgs) {
		t.Fatalf("tmux args = %q, want %q", fe.calls[0], wantArgs)
	}
}

// Kills: a respawn (Relaunch) that leaves the incarnation unchanged, and an
// incarnation taken from a pane other than the lowest window.pane index.
func TestTmuxRuntimeInventory_IncarnationIncludesFirstPanePID(t *testing.T) {
	before, err := runtimeInventoryFrom(t, &fakeExecutor{out: inventoryLines(
		"s\t$7\t1700000007\t0\t10.0\t0\t900\t1700000100\t0",
		"s\t$7\t1700000007\t0\t2.1\t0\t210\t1700000100\t0",
		"s\t$7\t1700000007\t0\t2.0\t0\t200\t1700000100\t0",
	)})
	if err != nil {
		t.Fatalf("RuntimeInventory before respawn: %v", err)
	}
	if got, want := before["s"].Incarnation, "$7:1700000007:200"; got != want {
		t.Fatalf("incarnation = %q, want %q (lowest window.pane index, numerically)", got, want)
	}

	// respawn-pane keeps the session id and creation time but starts a new
	// process in the same pane.
	after, err := runtimeInventoryFrom(t, &fakeExecutor{out: inventoryLines(
		"s\t$7\t1700000007\t0\t10.0\t0\t900\t1700000100\t0",
		"s\t$7\t1700000007\t0\t2.1\t0\t210\t1700000100\t0",
		"s\t$7\t1700000007\t0\t2.0\t0\t555\t1700000100\t0",
	)})
	if err != nil {
		t.Fatalf("RuntimeInventory after respawn: %v", err)
	}
	if before["s"].Incarnation == after["s"].Incarnation {
		t.Fatalf("incarnation %q unchanged across a respawn of the first pane", after["s"].Incarnation)
	}
}

// Kills: an activity that is not the session's latest window's, or one read
// from a field tmux left blank (C6a2b: the observation lane's idle
// reference, in the same batched read).
func TestTmuxRuntimeInventory_ActivityIsTheLatestWindowsOrUnknown(t *testing.T) {
	got, err := runtimeInventoryFrom(t, &fakeExecutor{out: inventoryLines(
		"s\t$1\t1700000001\t0\t0.0\t0\t101\t1700000200\t0",
		"s\t$1\t1700000001\t0\t1.0\t1\t102\t1700000300\t0",
		"s\t$1\t1700000001\t0\t2.0\t0\t103\t1700000250\t0",
		"quiet\t$2\t1700000002\t0\t0.0\t0\t201\t\t0",
	)})
	if err != nil {
		t.Fatalf("RuntimeInventory: %v", err)
	}
	if e := got["s"]; !e.ActivityKnown || !e.LastActivity.Equal(time.Unix(1700000300, 0)) {
		t.Fatalf("s: activity %v known %v, want the latest window's, a dead pane's included", e.LastActivity, e.ActivityKnown)
	}
	if e := got["quiet"]; e.ActivityKnown {
		t.Fatalf("quiet: activity known from a blank field: %v", e.LastActivity)
	}
}

// Kills: a live, empty server treated as a failure (the ga-jnavd shape).
func TestTmuxRuntimeInventory_NoCurrentTargetIsEmpty(t *testing.T) {
	got, err := runtimeInventoryFrom(t, &fakeExecutor{err: ErrNoCurrentTarget})
	if err != nil {
		t.Fatalf("RuntimeInventory on a live, empty server: %v, want nil", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("RuntimeInventory = %#v, want an empty non-nil map", got)
	}
}

// Kills: an absent server read as an empty fleet.
func TestTmuxRuntimeInventory_NoServerIsError(t *testing.T) {
	got, err := runtimeInventoryFrom(t, &fakeExecutor{err: ErrNoServer})
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("RuntimeInventory error = %v, want ErrNoServer", err)
	}
	if got != nil {
		t.Fatalf("RuntimeInventory = %#v, want nil on error", got)
	}
}

// Kills: one malformed line costing the whole inventory (mc-ym1u4: every
// tmux seat liveness-unknown), and a malformed line costing less than its
// whole session. Each line is parsed on its own; a session with a malformed
// line is omitted, every line of it, so it is never folded from the panes
// that did parse: dropping its only live pane would read it all-dead. An
// omitted name is unread, not gone. The others read exactly as without it.
func TestTmuxRuntimeInventory_MalformedLineCostsOnlyItsSession(t *testing.T) {
	end := "\t" + runtimeInventoryEnd
	good := []string{
		"a\t$1\t1700000001\t0\t0.0\t0\t101\t1700000100\t0" + end,
		"b\t$2\t1700000002\t1\t0.0\t1\t201\t1700000100\t0" + end,
		"b\t$2\t1700000002\t1\t0.1\t0\t202\t1700000100\t0" + end,
	}
	// s's corpse pane parses; its live pane is the malformed line.
	sCorpse := "s\t$9\t1700000009\t0\t0.0\t1\t900\t1700000100\t0" + end
	act := time.Unix(1700000100, 0)
	want := map[string]runtime.InventoryEntry{
		"a": {Incarnation: "$1:1700000001:101", DeadKnown: true, AttachedKnown: true, ActivityKnown: true, LastActivity: act},
		"b": {Incarnation: "$2:1700000002:201", DeadKnown: true, AttachedKnown: true, Attached: true, ActivityKnown: true, LastActivity: act},
	}
	for _, tc := range []struct {
		name, bad string
		// extra is a session the bad line's tail reads as. An unescaped
		// newline (tmux before 3.2) splits a name, and the tail is a
		// well-formed line under the tail's name, which list-sessions
		// splits out and lists the same way.
		extra map[string]runtime.InventoryEntry
	}{
		{name: "missing field", bad: "s\t$9\t1700000009\t0\t0.1\t0\t901\t1700000100" + end},
		{name: "trailing empty field trimmed, no terminator", bad: "s\t$9\t1700000009\t0\t0.1\t0\t901\t1700000100\t"},
		{name: "terminator cut short", bad: "s\t$9\t1700000009\t0\t0.1\t0\t901\t1700000100\t0\tgc-"},
		{name: "embedded tab in the name", bad: "s\tx\t$9\t1700000009\t0\t0.1\t0\t901\t1700000100\t0" + end},
		{
			name: "embedded newline in the name", bad: "s\nx\t$9\t1700000009\t0\t0.1\t0\t901\t1700000100\t0" + end,
			extra: map[string]runtime.InventoryEntry{"x": {Incarnation: "$9:1700000009:901", DeadKnown: true, AttachedKnown: true, ActivityKnown: true, LastActivity: act}},
		},
		{name: "extra trailing field", bad: "s\t$9\t1700000009\t0\t0.1\t0\t901\t1700000100\t0" + end + "\t"},
		{name: "malformed pane index", bad: "s\t$9\t1700000009\t0\tx.1\t0\t901\t1700000100\t0" + end},
		{name: "restart marker not 0 or 1", bad: "s\t$9\t1700000009\t0\t0.1\t0\t901\t1700000100\t1700000050" + end},
	} {
		for _, order := range []string{"bad line first", "bad line last"} {
			t.Run(tc.name+"/"+order, func(t *testing.T) {
				lines := append([]string{tc.bad, sCorpse}, good...)
				if order == "bad line last" {
					lines = append(append([]string{sCorpse}, good...), tc.bad)
				}
				got, err := runtimeInventoryFrom(t, &fakeExecutor{out: strings.Join(lines, "\n")})
				if err != nil {
					t.Fatalf("RuntimeInventory: %v, want the well-formed sessions", err)
				}
				wantHere := maps.Clone(want)
				maps.Copy(wantHere, tc.extra)
				if !reflect.DeepEqual(got, wantHere) {
					t.Fatalf("RuntimeInventory =\n%#v\nwant\n%#v\n(s omitted, not read from the panes that parsed)", got, wantHere)
				}
			})
		}
	}

	// A line with no name attributes to no session and costs none.
	got, err := runtimeInventoryFrom(t, &fakeExecutor{out: strings.Join(append([]string{"\t$9\t1700000009\t0\t0.1\t0\t901\t1700000100\t0" + end}, good...), "\n")})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("RuntimeInventory with a nameless line = %#v, %v; want %#v", got, err, want)
	}
}

// Kills a restart marker that can forge a line (tmux prints user options
// raw): the format reads only whether the marker is set.
func TestTmuxRuntimeInventory_MarkerIsReadAsAComparison(t *testing.T) {
	if strings.Contains(strings.ReplaceAll(runtimeInventoryFormat, "#{!=:#{"+restartRequestedOption+"},}", ""), restartRequestedOption) {
		t.Fatalf("format %q prints the restart marker's value", runtimeInventoryFormat)
	}
}

// Kills: a field-count constant that drifts from the format.
func TestTmuxRuntimeInventory_FieldCountMatchesTheFormat(t *testing.T) {
	if got := strings.Count(runtimeInventoryFormat, "\t") + 1; got != runtimeInventoryFields {
		t.Fatalf("runtimeInventoryFormat has %d fields, runtimeInventoryFields = %d", got, runtimeInventoryFields)
	}
	if !strings.HasSuffix(runtimeInventoryFormat, "\t"+runtimeInventoryEnd) {
		t.Fatalf("runtimeInventoryFormat %q does not end with the terminator", runtimeInventoryFormat)
	}
}

// Kills: an inventory that leaves no session (a tmux that prints the format
// differently, or every session with a malformed line) reported as an empty
// fleet with no error; and the two shapes reported alike.
func TestTmuxRuntimeInventory_NoSessionLeftIsError(t *testing.T) {
	for _, tc := range []struct{ name, out, want string }{
		{"no well-formed line", "s\t$1\t1700000001\t0\t0.0\t0\t101\t1700000100\t", "no well-formed inventory line"},
		{"every session omitted", inventoryLines("s\t$1\t1700000001\t0\t0.0\t1\t100\t1700000100\t0") + "\ns\tbad", `every session omitted for a malformed inventory line: ["s"]`},
	} {
		got, err := runtimeInventoryFrom(t, &fakeExecutor{out: tc.out})
		if err == nil || got != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: RuntimeInventory = %#v, %v; want nil and an error containing %q", tc.name, got, err, tc.want)
		}
	}
}

// Kills: a malformed line that goes unlogged, logs once per pass, or loses
// the sessions it cost; a count of skipped reads that is never reset; and an
// episode with no end. The first faulted read logs; later ones within
// inventoryMalformedLogEvery are counted and reported by the next line; a
// clean read ends the episode, once.
func TestTmuxRuntimeInventory_MalformedLinesLogRateLimited(t *testing.T) {
	var logged []string
	p := NewProviderWithConfig(Config{})
	p.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	clean := inventoryLines("a\t$1\t1700000001\t0\t0.0\t0\t101\t1700000100\t0")
	faulted := &fakeExecutor{out: clean + "\nbad\tline\nzed"}
	read := func(exec executor) {
		t.Helper()
		p.tm.exec = exec
		_, _ = p.RuntimeInventory(context.Background())
	}
	rewind := func() {
		p.inventoryMalformedLog.at = p.inventoryMalformedLog.at.Add(-inventoryMalformedLogEvery)
	}

	for range 3 {
		read(faulted)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], `["bad" "zed"]`) || !strings.Contains(logged[0], `first line "bad\tline"`) || strings.Contains(logged[0], "unlogged") {
		t.Fatalf("logged %q, want one line naming the omitted sessions and the first malformed line", logged)
	}
	rewind()
	read(faulted)
	if len(logged) != 2 || !strings.Contains(logged[1], "2 unlogged faulted reads") {
		t.Fatalf("logged %q, want a second line counting the 2 unlogged reads", logged)
	}
	rewind()
	read(faulted)
	if len(logged) != 3 || strings.Contains(logged[2], "unlogged") {
		t.Fatalf("logged %q, want a third line with the count reset", logged)
	}

	// A read that leaves no session is an error, and is logged.
	rewind()
	read(&fakeExecutor{out: "bad"})
	if len(logged) != 4 || !strings.Contains(logged[3], `first line "bad"`) {
		t.Fatalf("logged %q, want the all-malformed read logged", logged)
	}

	read(&fakeExecutor{out: clean})
	read(&fakeExecutor{out: clean})
	read(&fakeExecutor{err: ErrNoServer})
	if len(logged) != 5 || !strings.Contains(logged[4], "well-formed again") {
		t.Fatalf("logged %q, want one recovery line", logged)
	}
	read(faulted)
	if len(logged) != 6 || !strings.Contains(logged[5], "malformed") {
		t.Fatalf("logged %q, want a new episode logged at once", logged)
	}
}

// Kills: a declaration lost behind the seam-backed provider. The tmux
// cut-over embeds the raw provider, so it must carry the attestation, the
// batched inventory and the batched environment read.
func TestCutoverForwardsListingAttestation(t *testing.T) {
	sp := NewSeamBackedWithConfig(Config{})
	if !runtime.ListRunningAttested(sp) {
		t.Error("seam-backed tmux provider does not attest its listing")
	}
	if _, ok := sp.(runtime.InventoryProvider); !ok {
		t.Error("seam-backed tmux provider does not implement InventoryProvider")
	}
	if _, ok := sp.(runtime.EnvironmentBatchProvider); !ok {
		t.Error("seam-backed tmux provider does not implement EnvironmentBatchProvider")
	}

	// The attestation promises complete-or-err, so the seam-backed listing
	// must keep an absent server a ServerAbsent partial with no names, and
	// keep any other failure a plain error.
	plain := errors.New("tmux list-sessions: server busy")
	for _, tc := range []struct {
		name       string
		execErr    error
		wantAbsent bool
	}{
		{name: "no server", execErr: ErrNoServer, wantAbsent: true},
		{name: "plain error", execErr: plain, wantAbsent: false},
	} {
		sp := NewSeamBackedWithConfig(Config{})
		sp.(*seamBackedProvider).tm.exec = &fakeExecutor{err: tc.execErr}
		names, err := sp.ListRunning("")
		if err == nil || names != nil {
			t.Errorf("%s: ListRunning = (%q, %v), want nil names and an error", tc.name, names, err)
			continue
		}
		if got := runtime.IsRuntimeServerAbsent(err); got != tc.wantAbsent {
			t.Errorf("%s: IsRuntimeServerAbsent(%v) = %v, want %v", tc.name, err, got, tc.wantAbsent)
		}
		if !tc.wantAbsent && (!errors.Is(err, plain) || runtime.IsPartialListError(err)) {
			t.Errorf("%s: ListRunning err = %v, want the plain error unchanged", tc.name, err)
		}
	}
}

type inventoryCtxKey struct{}

// ctxRecordingExecutor records the context each tmux call runs under.
type ctxRecordingExecutor struct {
	ctxs []context.Context
}

func (e *ctxRecordingExecutor) execute(args []string) (string, error) {
	return e.executeCtx(context.Background(), args)
}

func (e *ctxRecordingExecutor) executeCtx(ctx context.Context, _ []string) (string, error) {
	e.ctxs = append(e.ctxs, ctx)
	return "", ctx.Err()
}

// Kills: a RuntimeInventory that detaches from its caller's context, so a
// caller bounding or canceling the batched read cannot stop the tmux call.
func TestTmuxRuntimeInventory_PropagatesContext(t *testing.T) {
	rec := &ctxRecordingExecutor{}
	p := NewProviderWithConfig(Config{})
	p.tm.exec = rec
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), inventoryCtxKey{}, "caller"))
	cancel()

	if _, err := p.RuntimeInventory(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RuntimeInventory under a canceled context: err = %v, want context.Canceled", err)
	}
	if len(rec.ctxs) != 1 {
		t.Fatalf("tmux calls = %d, want 1", len(rec.ctxs))
	}
	if got := rec.ctxs[0].Value(inventoryCtxKey{}); got != "caller" {
		t.Fatalf("tmux call context value = %v, want the caller's context", got)
	}
}

// inventoryFixturePane is one pane of an inventoryFixtureSession.
type inventoryFixturePane struct {
	window, pane int
	dead         string
	pid          string
}

// inventoryFixtureSession is one tmux session in an inventoryFixtureExecutor.
type inventoryFixtureSession struct {
	name, id, created, attached string
	panes                       []inventoryFixturePane
	activity                    map[int]string // #{window_activity} by window index
	env                         map[string]string
	envErr                      error
}

// inventoryFixtureExecutor answers tmux commands from one fleet model, so the
// batched inventory and the legacy per-session reads observe the same state.
// Targets are matched by session name whether spelled name, =name or =name:
// (and pane suffixes), so the fixture is independent of target spelling.
type inventoryFixtureExecutor struct {
	sessions []inventoryFixtureSession
}

func (f *inventoryFixtureExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func (f *inventoryFixtureExecutor) execute(args []string) (string, error) {
	if len(args) > 0 && args[0] == "-u" {
		args = args[1:]
	}
	flag := func(name string) string {
		if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	switch args[0] {
	case "list-sessions":
		lines := make([]string, 0, len(f.sessions))
		for _, s := range f.sessions {
			lines = append(lines, f.render(flag("-F"), s, s.panes[0]))
		}
		return strings.Join(lines, "\n"), nil
	case "list-panes":
		var lines []string
		for _, s := range f.sessions {
			if !slices.Contains(args, "-a") && s.name != fixtureTargetSession(flag("-t")) {
				continue
			}
			for _, p := range s.panes {
				lines = append(lines, f.render(flag("-F"), s, p))
			}
		}
		if lines == nil {
			return "", ErrSessionNotFound
		}
		return strings.Join(lines, "\n"), nil
	case "list-windows":
		s, ok := f.session(flag("-t"))
		if !ok {
			return "", ErrSessionNotFound
		}
		var lines []string
		for i, p := range s.panes {
			if i == 0 || p.window != s.panes[i-1].window {
				lines = append(lines, f.render(flag("-F"), s, p))
			}
		}
		return strings.Join(lines, "\n"), nil
	case "display-message":
		s, ok := f.session(flag("-t"))
		if !ok {
			return "", ErrSessionNotFound
		}
		return f.render(flag("-p"), s, s.panes[0]), nil
	case "show-environment":
		s, ok := f.session(flag("-t"))
		if !ok {
			return "", ErrSessionNotFound
		}
		if s.envErr != nil {
			return "", s.envErr
		}
		if key := args[len(args)-1]; key != flag("-t") {
			v, ok := s.env[key]
			if !ok {
				return "", fmt.Errorf("tmux show-environment: unknown variable: %s", key)
			}
			return key + "=" + v, nil
		}
		lines := make([]string, 0, len(s.env))
		for k, v := range s.env {
			lines = append(lines, k+"="+v)
		}
		slices.Sort(lines)
		return strings.Join(lines, "\n"), nil
	}
	return "", fmt.Errorf("inventory fixture: unscripted tmux command %q", args)
}

func (f *inventoryFixtureExecutor) session(target string) (inventoryFixtureSession, bool) {
	name := fixtureTargetSession(target)
	for _, s := range f.sessions {
		if s.name == name {
			return s, true
		}
	}
	return inventoryFixtureSession{}, false
}

func (f *inventoryFixtureExecutor) render(format string, s inventoryFixtureSession, p inventoryFixturePane) string {
	return strings.NewReplacer(
		"#{session_name}", s.name,
		"#{session_id}", s.id,
		"#{session_created}", s.created,
		"#{session_attached}", s.attached,
		"#{window_index}", strconv.Itoa(p.window),
		"#{pane_index}", strconv.Itoa(p.pane),
		"#{pane_dead}", p.dead,
		"#{pane_pid}", p.pid,
		"#{window_activity}", s.activity[p.window],
		"#{!=:#{"+restartRequestedOption+"},}", "0",
	).Replace(format)
}

func fixtureTargetSession(target string) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(target, "="), ":")
	return name
}

func inventoryFixtureFleet() *inventoryFixtureExecutor {
	return &inventoryFixtureExecutor{sessions: []inventoryFixtureSession{
		{
			name: "gc-live", id: "$1", created: "1700000001", attached: "0",
			panes:    []inventoryFixturePane{{0, 0, "0", "101"}},
			activity: map[int]string{0: "1700000101"},
			env:      map[string]string{"GC_SESSION_ID": "gc-101", "GC_INSTANCE_TOKEN": "tok-101"},
		},
		{
			name: "gc-corpse", id: "$2", created: "1700000002", attached: "0",
			panes:    []inventoryFixturePane{{0, 0, "1", "201"}, {0, 1, "1", "202"}},
			activity: map[int]string{0: "1700000201"},
			env:      map[string]string{"GC_SESSION_ID": "gc-201"},
		},
		{
			name: "gc-mixed", id: "$3", created: "1700000003", attached: "1",
			panes:    []inventoryFixturePane{{0, 0, "1", "301"}, {1, 0, "0", "302"}},
			activity: map[int]string{0: "1700000399", 1: "1700000301"},
			env:      map[string]string{"GC_SESSION_ID": "gc-301", "GC_INSTANCE_TOKEN": "tok-301"},
		},
		{
			name: "gc-ownerless", id: "$4", created: "1700000004", attached: "0",
			panes:    []inventoryFixturePane{{0, 0, "0", "401"}},
			activity: map[int]string{0: "1700000401"},
			env:      map[string]string{"PATH": "/usr/bin"},
		},
		{
			name: "gc-unreadable", id: "$5", created: "1700000005", attached: "0",
			panes:    []inventoryFixturePane{{0, 0, "0", "501"}},
			activity: map[int]string{0: "1700000501"},
			envErr:   errors.New("tmux show-environment: server busy"),
		},
	}}
}

// Differential: on one fleet fixture, the batched inventory answers exactly
// what the legacy per-session reads answer — the entry set is the listing,
// AllPanesDead is IsDeadRuntimeSession, Attached is IsAttached (whose
// "exactly one client" reading agrees with a client count for 0 and 1), and
// LastActivity is GetLastActivity (legacy's idle reference).
// Kills: an inventory that diverges from the per-runtime probes it replaces.
func TestTmuxRuntimeInventory_MatchesPerSessionProbes(t *testing.T) {
	raw := NewProviderWithConfig(Config{})
	raw.tm.exec = inventoryFixtureFleet()
	sp := &seamBackedProvider{Provider: raw, seams: runtime.NewProviderFromSeams(raw.Seams())}

	inv, err := sp.RuntimeInventory(context.Background())
	if err != nil {
		t.Fatalf("RuntimeInventory: %v", err)
	}
	listed, err := sp.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	invNames := make([]string, 0, len(inv))
	for name := range inv {
		invNames = append(invNames, name)
	}
	slices.Sort(invNames)
	slices.Sort(listed)
	if !slices.Equal(invNames, listed) {
		t.Fatalf("inventory names = %q, ListRunning = %q", invNames, listed)
	}
	for _, name := range listed {
		entry := inv[name]
		dead, err := sp.IsDeadRuntimeSession(name)
		if err != nil {
			t.Fatalf("IsDeadRuntimeSession(%s): %v", name, err)
		}
		if !entry.DeadKnown || entry.AllPanesDead != dead {
			t.Errorf("%s: inventory AllPanesDead = %v (known %v), IsDeadRuntimeSession = %v", name, entry.AllPanesDead, entry.DeadKnown, dead)
		}
		if attached := raw.IsAttached(name); !entry.AttachedKnown || entry.Attached != attached {
			t.Errorf("%s: inventory Attached = %v (known %v), IsAttached = %v", name, entry.Attached, entry.AttachedKnown, attached)
		}
		if entry.Incarnation == "" {
			t.Errorf("%s: inventory incarnation is empty", name)
		}
		activity, err := raw.GetLastActivity(name)
		if err != nil {
			t.Fatalf("GetLastActivity(%s): %v", name, err)
		}
		if !entry.ActivityKnown || !entry.LastActivity.Equal(activity) {
			t.Errorf("%s: inventory activity = %v (known %v), GetLastActivity = %v", name, entry.LastActivity, entry.ActivityKnown, activity)
		}
	}
}

// Differential: the attribution read the inventory lane uses, one
// GetAllEnvironment per incarnation, returns the same GC_SESSION_ID and
// GC_INSTANCE_TOKEN the per-key GetMeta reads return, and surfaces a read
// failure as an error instead of an empty environment.
// Kills: a batched attribution read that disagrees with GetMeta, or that
// reports an unreadable runtime as ownerless.
func TestTmuxAttributionEnvironment_MatchesGetMeta(t *testing.T) {
	fleet := inventoryFixtureFleet()
	raw := NewProviderWithConfig(Config{})
	raw.tm.exec = fleet
	sp := &seamBackedProvider{Provider: raw, seams: runtime.NewProviderFromSeams(raw.Seams())}
	batch, ok := runtime.Provider(sp).(runtime.EnvironmentBatchProvider)
	if !ok {
		t.Fatal("seam-backed tmux provider does not implement EnvironmentBatchProvider")
	}

	for _, s := range fleet.sessions {
		env, err := batch.GetAllEnvironment(s.name)
		if s.envErr != nil {
			if !errors.Is(err, s.envErr) {
				t.Errorf("%s: GetAllEnvironment error = %v, want %v", s.name, err, s.envErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: GetAllEnvironment: %v", s.name, err)
		}
		for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN"} {
			meta, err := sp.GetMeta(s.name, key)
			if err != nil {
				t.Fatalf("%s: GetMeta(%s): %v", s.name, key, err)
			}
			if env[key] != meta {
				t.Errorf("%s: GetAllEnvironment[%s] = %q, GetMeta = %q", s.name, key, env[key], meta)
			}
		}
	}
}

// Kills: an inventory activity that counts gc's own poke as the agent's
// activity (C6a2b): the batched read applies the poke discount
// GetLastActivity applies, so a session whose only activity since its prior
// is a poke's echo reads its prior.
func TestTmuxRuntimeInventory_ActivityDiscountsAPoke(t *testing.T) {
	prior, poke := time.Unix(1700000000, 0), time.Now().Add(-time.Hour).Truncate(time.Second)
	p := NewProviderWithConfig(Config{})
	p.tm.exec = &fakeExecutor{out: inventoryLines(fmt.Sprintf("s\t$1\t1700000001\t0\t0.0\t0\t101\t%d\t0", poke.Unix()))}
	p.tm.pokeMu.Lock()
	if p.tm.pokes == nil {
		p.tm.pokes = map[string]pokeInfo{}
	}
	p.tm.pokes["s"] = pokeInfo{at: poke, prior: prior}
	p.tm.pokeMu.Unlock()
	got, err := p.RuntimeInventory(context.Background())
	if err != nil {
		t.Fatalf("RuntimeInventory: %v", err)
	}
	if e := got["s"]; !e.ActivityKnown || !e.LastActivity.Equal(prior) {
		t.Fatalf("activity %v, want the prior %v: a poke's echo is not the agent's activity", e.LastActivity, prior)
	}
}

// Kills a restart marker lost from the batched read, or one read off a
// session that has none (CONTRACT v5.10q, C7a-2b): the session's
// @GC_RESTART_REQUESTED user option, carried on every pane line, sets
// RestartRequested; a blank one leaves it unset.
func TestTmuxRuntimeInventory_ReadsTheRestartMarker(t *testing.T) {
	got, err := runtimeInventoryFrom(t, &fakeExecutor{out: inventoryLines(
		"asked\t$1\t1700000001\t0\t0.0\t0\t101\t1700000100\t1",
		"asked\t$1\t1700000001\t0\t0.1\t0\t102\t1700000100\t1",
		"quiet\t$2\t1700000002\t0\t0.0\t0\t201\t1700000100\t0",
	)})
	if err != nil {
		t.Fatalf("RuntimeInventory: %v", err)
	}
	if !got["asked"].RestartRequested || got["quiet"].RestartRequested {
		t.Fatalf("RestartRequested asked=%t quiet=%t, want only asked", got["asked"].RestartRequested, got["quiet"].RestartRequested)
	}
	if !strings.HasSuffix(runtimeInventoryFormat, "\t#{!=:#{@GC_RESTART_REQUESTED},}\t"+runtimeInventoryEnd) {
		t.Fatalf("format %q does not read the marker's option", runtimeInventoryFormat)
	}
}

// Regression (#7871): the real executor returns strings.TrimSpace(stdout).
// Before the terminator field, tmux ended every line with the marker field,
// so an unmarked LAST session arrived with its trailing tab trimmed away and
// failed the whole inventory (which left every tmux seat liveness-unknown).
// The terminator ends every line now, so trimming cannot change a line's
// field count. The fixtures are tmux's raw stdout, trimmed exactly as
// realExecutor trims it.
// Kills: a parser that misreads an unmarked last session.
func TestTmuxRuntimeInventory_ExecutorTrimmedEmptyMarker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		asked  map[string]bool
	}{
		{
			name:   "empty marker on the last line",
			stdout: "asked\t$1\t1700000001\t0\t0.0\t0\t101\t1700000100\t1\nquiet\t$2\t1700000002\t0\t0.0\t0\t201\t1700000100\t0\n",
			asked:  map[string]bool{"asked": true, "quiet": false},
		},
		{
			name:   "empty marker on a middle line",
			stdout: "quiet\t$2\t1700000002\t0\t0.0\t0\t201\t1700000100\t0\nasked\t$1\t1700000001\t0\t0.0\t0\t101\t1700000100\t1\n",
			asked:  map[string]bool{"asked": true, "quiet": false},
		},
		{
			name:   "no marker anywhere",
			stdout: "a\t$1\t1700000001\t0\t0.0\t0\t101\t1700000100\t0\nb\t$2\t1700000002\t0\t0.0\t0\t201\t1700000100\t0\nb\t$2\t1700000002\t0\t0.1\t0\t202\t1700000100\t0\n",
			asked:  map[string]bool{"a": false, "b": false},
		},
		{
			name:   "one unmarked session",
			stdout: "only\t$1\t1700000001\t0\t0.0\t0\t101\t1700000100\t0\n",
			asked:  map[string]bool{"only": false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout := inventoryLines(strings.Split(strings.TrimSuffix(tc.stdout, "\n"), "\n")...) + "\n"
			got, err := runtimeInventoryFrom(t, &fakeExecutor{out: strings.TrimSpace(stdout)})
			if err != nil {
				t.Fatalf("RuntimeInventory: %v", err)
			}
			if len(got) != len(tc.asked) {
				t.Fatalf("RuntimeInventory = %#v, want sessions %v", got, tc.asked)
			}
			for name, asked := range tc.asked {
				e, ok := got[name]
				if !ok {
					t.Fatalf("session %q missing from %#v", name, got)
				}
				if e.RestartRequested != asked || !e.DeadKnown || e.AllPanesDead || !e.ActivityKnown || e.Incarnation == "" {
					t.Errorf("%s = %#v, want a live, known session with RestartRequested=%t", name, e, asked)
				}
			}
		})
	}
}

// Kills a restart marker the batched read cannot see (C7a-2b): SetMeta and
// RemoveMeta of GC_RESTART_REQUESTED write its user-option mirror beside the
// environment, and no other key gets one.
func TestTmuxRestartMarkerIsMirroredToAnOption(t *testing.T) {
	fe := &fakeExecutor{}
	p := NewProviderWithConfig(Config{})
	p.tm.exec = fe
	if err := p.SetMeta("s", "GC_RESTART_REQUESTED", "1700000050"); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveMeta("s", "GC_RESTART_REQUESTED"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("s", "GC_OTHER", "v"); err != nil {
		t.Fatal(err)
	}
	var options []string
	for _, call := range fe.calls {
		if slices.Contains(call, "set-option") {
			options = append(options, strings.Join(call, " "))
		}
	}
	if len(options) != 2 || !strings.Contains(options[0], "@GC_RESTART_REQUESTED 1700000050") || !strings.Contains(options[1], "-u @GC_RESTART_REQUESTED") {
		t.Fatalf("option writes %q, want the marker set then unset, and nothing else", options)
	}
	// set-option's -t is a target-pane: tmux 3.4 answers "no such session"
	// for a bare "=s", so the session is fenced as "=s:" (paneTarget).
	for _, option := range options {
		if !strings.Contains(option, "-t =s: ") {
			t.Errorf("option write %q, want the session fenced as a pane target (-t =s:)", option)
		}
	}
}
