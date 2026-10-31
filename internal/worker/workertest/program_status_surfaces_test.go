package workertest

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	workerbuiltin "github.com/gastownhall/gascity/internal/worker/builtin"
)

// programStatusReplaySession is the fake session a capture is replayed into.
const (
	programStatusReplaySession = "program-status-replay"
	programStatusReplayPane    = "%1"
	programStatusReplayNonce   = "r1.%1"
)

// programStatusReplay is what the runtime program-status seam read while a
// capture was replayed: one reading after every sequence the capture path
// recognized.
type programStatusReplay struct {
	readings    []runtime.ProgramStatus
	passthrough int // reports that arrived wrapped in tmux DCS passthrough
	dropped     int // reports that broke a spec rule and were dropped whole
}

// replayProgramStatus feeds capture through gc's capture path (Scanner,
// ParseBody, Store, the pane-option codec) into an armed agent pane of a
// [runtime.FullFake], and reads the session's status through
// [runtime.ProgramStatusFor] after every sequence. The capture clock advances
// one millisecond per sequence.
func replayProgramStatus(t *testing.T, capture []byte) programStatusReplay {
	t.Helper()
	f := runtime.NewFake()
	if err := f.Start(t.Context(), programStatusReplaySession, runtime.Config{}); err != nil {
		t.Fatalf("starting replay session: %v", err)
	}
	f.SetProgramPane(programStatusReplaySession, runtime.ProgramPaneRow{
		Mode:        runtime.ProgramStatusShadow,
		CaptureGate: true,
		PaneID:      programStatusReplayPane,
		Tap:         programStatusReplayNonce,
		PanePipe:    true,
		TapLiveness: runtime.ProgramTapAlive,
	})
	reader, ok := runtime.ProgramStatusFor(runtime.FullFake{Fake: f}, programStatusReplaySession)
	if !ok {
		t.Fatal("FullFake does not serve program status")
	}
	store := programstatus.NewStore(programStatusReplayNonce)
	clock := time.Unix(1_800_000_000, 0)
	var out programStatusReplay
	var sc programstatus.Scanner
	sc.Feed(capture, func(ev programstatus.Event) {
		clock = clock.Add(time.Millisecond)
		switch ev.Kind {
		case programstatus.EventProbe:
			store.Probe()
		case programstatus.EventPromptStart:
			store.PromptStart()
		case programstatus.EventReport:
			if ev.Passthrough {
				out.passthrough++
			}
			r, err := programstatus.ParseBody(ev.Body)
			if err != nil {
				out.dropped++
				return
			}
			store.Apply(r, clock)
		}
		v, err := programstatus.Encode(store.Status())
		if err != nil {
			t.Fatalf("encoding the captured record: %v", err)
		}
		rec, err := programstatus.Decode(v)
		row, _ := f.ProgramPane(programStatusReplaySession)
		row.RecordForm, row.Record = programstatus.RecordForm(err), rec
		f.SetProgramPane(programStatusReplaySession, row)
		st, err := reader.ProgramStatus(programStatusReplaySession)
		if err != nil {
			t.Fatalf("reading program status: %v", err)
		}
		out.readings = append(out.readings, st)
	})
	return out
}

// judgeProgramStatusSurfaces holds a replay to WC-PSTAT-001 for a program
// declared to report as app (empty: any app): the first valid reading names
// app, a later reading shows a turn running, and a later one shows that turn
// ended at an input boundary. Every valid reading names app.
func judgeProgramStatusSurfaces(replay programStatusReplay, app string) (map[string]string, error) {
	first, start, end := -1, -1, -1
	for i, st := range replay.readings {
		if !st.Valid() {
			continue
		}
		if app != "" && st.App != app {
			return nil, fmt.Errorf("reading %d names app %q, want %q", i, st.App, app)
		}
		switch {
		case first < 0:
			first = i
		case start < 0 && st.Running() && st.TurnStarts > 0:
			start = i
		case start >= 0 && end < 0 && st.InputBoundary() && st.TurnEnds > 0:
			end = i
		}
	}
	evidence := map[string]string{
		"readings":    strconv.Itoa(len(replay.readings)),
		"passthrough": strconv.Itoa(replay.passthrough),
		"dropped":     strconv.Itoa(replay.dropped),
	}
	switch {
	case first < 0:
		return evidence, fmt.Errorf("no valid report surfaced in %d readings", len(replay.readings))
	case start < 0:
		return evidence, fmt.Errorf("no turn start surfaced after the first report (%s)", replay.readings[first].State)
	case end < 0:
		return evidence, fmt.Errorf("the turn started at reading %d never ended at an input boundary", start)
	}
	last := replay.readings[end]
	evidence["first_state"] = string(replay.readings[first].State)
	evidence["turn_end_state"] = string(last.State)
	evidence["turn_starts"] = strconv.FormatUint(uint64(last.TurnStarts), 10)
	evidence["turn_ends"] = strconv.FormatUint(uint64(last.TurnEnds), 10)
	return evidence, nil
}

// checkProgramStatusSurfaces is WC-PSTAT-001: the profile's captured program
// output, replayed through the capture path, surfaces through the runtime
// program-status seam as a first report, a turn start and a turn end, all
// naming the app the provider data declares.
func checkProgramStatusSurfaces(t *testing.T, profile Profile) Result {
	t.Helper()
	code := RequirementProgramStatusSurfaces
	if profile.ProgramStatus.Capture == "" {
		return Fail(profile.ID, code, "declared profile has no program capture")
	}
	capture, err := os.ReadFile(profile.ProgramStatus.Capture)
	if err != nil {
		return Fail(profile.ID, code, fmt.Sprintf("reading program capture: %v", err))
	}
	app := workerbuiltin.BuiltinProviders()[ProfileFamily(profile)].ProgramStatusApp
	evidence, err := judgeProgramStatusSurfaces(replayProgramStatus(t, capture), app)
	if evidence == nil {
		evidence = map[string]string{}
	}
	evidence["capture"] = profile.ProgramStatus.Capture
	evidence["program"] = profile.ProgramStatus.Program
	evidence["app"] = app
	if err != nil {
		return Fail(profile.ID, code, err.Error()).WithEvidence(evidence)
	}
	return Pass(profile.ID, code, "first report, turn start and turn end surfaced").WithEvidence(evidence)
}

// Kills a judge that passes a program that never reports, reports no turn,
// leaves its turn open, names another app, or a replay that drops
// passthrough-wrapped reports.
func TestJudgeProgramStatusSurfaces(t *testing.T) {
	report := func(body string) string { return "\x1b]7501;" + body + "\x1b\\" }
	wrapped := func(body string) string {
		return "\x1bPtmux;\x1b\x1b]7501;" + body + "\x1b\x1b\\\x1b\\"
	}
	turn := report("state=idle:app=a") + "screen bytes" + report("state=working:app=a") + report("state=done:app=a") + report("state=clear")
	cases := []struct {
		name    string
		capture string
		app     string
		want    string // error substring; empty passes
	}{
		{name: "turn", capture: turn, app: "a"},
		{name: "passthrough turn", capture: wrapped("state=idle:app=a") + wrapped("state=working:app=a") + wrapped("state=idle:app=a"), app: "a"},
		{name: "blocked then working is one turn", capture: report("state=idle:app=a") + report("state=working:app=a") + report("state=blocked:kind=permission:app=a") + report("state=working:app=a") + report("state=idle:app=a"), app: "a"},
		{name: "silent", capture: "screen bytes only", app: "a", want: "no valid report"},
		{name: "probe only", capture: "\x1b]7501;?\x1b\\", app: "a", want: "no valid report"},
		{name: "idle only", capture: report("state=idle:app=a"), app: "a", want: "no turn start"},
		{name: "turn never ends", capture: report("state=idle:app=a") + report("state=working:app=a"), app: "a", want: "never ended"},
		{name: "turn cleared not ended", capture: report("state=idle:app=a") + report("state=working:app=a") + report("state=clear"), app: "a", want: "never ended"},
		{name: "other app", capture: turn, app: "b", want: `names app "a"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			replay := replayProgramStatus(t, []byte(tc.capture))
			_, err := judgeProgramStatusSurfaces(replay, tc.app)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("judge failed a conforming capture: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("judge err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if n := replayProgramStatus(t, []byte(wrapped("state=idle:app=a"))).passthrough; n != 1 {
		t.Fatalf("passthrough reports counted %d, want 1", n)
	}
}
