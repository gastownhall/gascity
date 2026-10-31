package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/runtime"
)

// programRowAt is an armed agent-pane row whose capture read st (with kind,
// for blocked) under mode at since, its reader attached: a valid reading
// unless mode is off.
func programRowAt(mode runtime.ProgramStatusMode, st runtime.ProgramState, kind runtime.ProgramBlockKind, since time.Time) runtime.ProgramPaneRow {
	return runtime.ProgramPaneRow{
		Mode: mode, CaptureGate: true, PaneID: "%7",
		PanePipe: true, Tap: "api.%7", TapLiveness: runtime.ProgramTapAlive,
		RecordForm: runtime.ProgramRecordDecoded,
		Record: runtime.ProgramStatus{
			Nonce: "api.%7", State: st, Kind: kind, App: "probe", Since: since,
			Seq: 5, Progress: -1, TurnStarts: 2, TurnEnds: 1, Probed: true, LastError: true,
			Reported: true, WorkingChildren: 1, Children: 2,
		},
	}
}

// programStatusSessionServer is a session API over a FullFake (which takes
// program-status readings) with one active session, and that session's id
// and runtime name.
func programStatusSessionServer(t *testing.T) (*fakeState, http.Handler, string, string) {
	t.Helper()
	fs := newSessionFakeState(t)
	fs.sessionProvider = runtime.FullFake{Fake: fs.sp}
	h := newTestCityHandlerWith(t, fs, New(fs))
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Program")
	return fs, h, info.ID, info.SessionName
}

// getProgramStatusJSON GETs url and decodes the body into out, returning the raw body.
func getProgramStatusJSON(t *testing.T, h http.Handler, url string, out any) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if err := json.Unmarshal([]byte(body), out); err != nil {
		t.Fatalf("decode %s: %v; body: %s", url, err, body)
	}
	return body
}

// Kills a session response that drops the worker state's reading, leaks its
// counters or annotations, moves activity under shadow, or does not take the
// record's activity under on, on the single and the list read.
func TestSessionResponseProgramStatusFromWorkerState(t *testing.T) {
	fs, h, id, name := programStatusSessionServer(t)
	since := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

	fs.sp.SetProgramPane(name, programRowAt(runtime.ProgramStatusShadow, runtime.ProgramBlocked, runtime.ProgramBlockPermission, since))
	var resp sessionResponse
	body := getProgramStatusJSON(t, h, cityURL(fs, "/session/")+id, &resp)
	want := SessionProgramStatus{State: "blocked", Kind: "permission", App: "probe", Since: since, WorkingChildren: 1}
	if resp.ProgramStatus == nil || !sameProgramStatusView(resp.ProgramStatus, &want) {
		t.Fatalf("shadow program_status = %+v, want %+v; body: %s", resp.ProgramStatus, want, body)
	}
	if resp.Activity != "" || resp.ActivitySource != "" {
		t.Fatalf("shadow activity = %q (source %q), want the transcript's (none) unchanged", resp.Activity, resp.ActivitySource)
	}
	for _, internal := range []string{"nonce", "seq", "turn_starts", "probed", "last_error", "validity", "reader_verified"} {
		if strings.Contains(body, `"`+internal+`"`) {
			t.Fatalf("response leaks internal field %q: %s", internal, body)
		}
	}

	fs.sp.SetProgramPane(name, programRowAt(runtime.ProgramStatusOn, runtime.ProgramWorking, "", since))
	resp = sessionResponse{}
	getProgramStatusJSON(t, h, cityURL(fs, "/session/")+id, &resp)
	if resp.ProgramStatus == nil || resp.ProgramStatus.State != "working" || resp.Activity != activityInTurn || resp.ActivitySource != activitySourceProgram {
		t.Fatalf("on working: program_status %+v activity %q source %q; want working, in-turn from program", resp.ProgramStatus, resp.Activity, resp.ActivitySource)
	}

	fs.sp.SetProgramPane(name, programRowAt(runtime.ProgramStatusOn, runtime.ProgramDone, "", since))
	var list struct {
		Items []sessionResponse `json:"items"`
	}
	getProgramStatusJSON(t, h, cityURL(fs, "/sessions"), &list)
	if len(list.Items) != 1 || list.Items[0].ProgramStatus == nil || list.Items[0].ProgramStatus.State != "done" || list.Items[0].Activity != activityIdle || list.Items[0].ActivitySource != activitySourceProgram {
		t.Fatalf("list on done: %+v; want done, idle from program", list.Items)
	}
}

// Kills a response that shows an invalid reading, or reads an unknown status
// as idle, for the session and the agent reads.
func TestAbsentProgramStatusIsUnknownNotIdle(t *testing.T) {
	fs, h, id, name := programStatusSessionServer(t)
	since := time.Now().Add(-time.Minute)
	silent := programRowAt(runtime.ProgramStatusOn, runtime.ProgramWorking, "", since)
	silent.RecordForm, silent.Record = runtime.ProgramRecordAbsent, runtime.ProgramStatus{}
	readerLost := programRowAt(runtime.ProgramStatusOn, runtime.ProgramIdle, "", since)
	readerLost.PanePipe = false
	for label, row := range map[string]runtime.ProgramPaneRow{
		"off":         programRowAt(runtime.ProgramStatusOff, runtime.ProgramIdle, "", since),
		"silent":      silent,
		"reader lost": readerLost,
	} {
		fs.sp.SetProgramPane(name, row)
		var resp sessionResponse
		body := getProgramStatusJSON(t, h, cityURL(fs, "/session/")+id, &resp)
		if strings.Contains(body, `"program_status"`) || strings.Contains(body, `"activity_source"`) || resp.Activity == activityIdle {
			t.Fatalf("%s: session body shows a status or idle: %s", label, body)
		}
	}

	if err := fs.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	fs.sp.SetProgramPane("myrig--worker", silent)
	var agent agentResponse
	body := getProgramStatusJSON(t, h, cityURL(fs, "/agent/myrig/worker"), &agent)
	if strings.Contains(body, `"program_status"`) || agent.Activity == activityIdle {
		t.Fatalf("agent with a silent capture shows a status or idle: %s", body)
	}
}

// Kills a computeAgentState that widens the enum outside on, misses blocked or
// error under on, or lets a record outrank suspension or a stopped runtime.
func TestComputeAgentStateBlockedAndErrorOnlyWhenOn(t *testing.T) {
	read := func(mode runtime.ProgramStatusMode, st runtime.ProgramState) runtime.ProgramStatus {
		return runtime.JudgeProgramStatus(programRowAt(mode, st, "", time.Now()))
	}
	recent := time.Now()
	cases := []struct {
		name       string
		suspended  bool
		running    bool
		activeBead string
		lastAct    *time.Time
		program    runtime.ProgramStatus
		want       string
	}{
		{"on blocked", false, true, "b-1", &recent, read(runtime.ProgramStatusOn, runtime.ProgramBlocked), "blocked"},
		{"on error", false, true, "", nil, read(runtime.ProgramStatusOn, runtime.ProgramError), "error"},
		{"on working without a bead", false, true, "", nil, read(runtime.ProgramStatusOn, runtime.ProgramWorking), "working"},
		{"on idle with a recently active bead", false, true, "b-1", &recent, read(runtime.ProgramStatusOn, runtime.ProgramIdle), "waiting"},
		{"on done without a bead", false, true, "", nil, read(runtime.ProgramStatusOn, runtime.ProgramDone), "idle"},
		{"shadow blocked", false, true, "", nil, read(runtime.ProgramStatusShadow, runtime.ProgramBlocked), "idle"},
		{"shadow error", false, true, "b-1", &recent, read(runtime.ProgramStatusShadow, runtime.ProgramError), "working"},
		{"on blocked but suspended", true, true, "", nil, read(runtime.ProgramStatusOn, runtime.ProgramBlocked), "suspended"},
		{"on error but stopped", false, false, "", nil, read(runtime.ProgramStatusOn, runtime.ProgramError), "stopped"},
		{"unread", false, true, "", nil, runtime.ProgramStatus{}, "idle"},
	}
	for _, tc := range cases {
		if got := computeAgentState(tc.suspended, false, tc.running, tc.activeBead, tc.lastAct, tc.program); got != tc.want {
			t.Errorf("%s: computeAgentState = %q, want %q", tc.name, got, tc.want)
		}
	}

	fs, h, _, _ := programStatusSessionServer(t)
	if err := fs.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[runtime.ProgramStatusMode]string{runtime.ProgramStatusShadow: "idle", runtime.ProgramStatusOn: "blocked"} {
		fs.sp.SetProgramPane("myrig--worker", programRowAt(mode, runtime.ProgramBlocked, runtime.ProgramBlockAuth, time.Now()))
		var agent agentResponse
		getProgramStatusJSON(t, h, cityURL(fs, "/agent/myrig/worker"), &agent)
		if agent.State != want || agent.ProgramStatus == nil || agent.ProgramStatus.Kind != "auth" {
			t.Fatalf("%s: agent state %q program_status %+v; want %q with the blocked:auth status", mode, agent.State, agent.ProgramStatus, want)
		}
	}
}

// streamedProgramStatusFrames returns the complete program_status frames of
// an SSE body.
func streamedProgramStatusFrames(t *testing.T, body string) []SessionProgramStatusEvent {
	t.Helper()
	end := strings.LastIndex(body, "\n\n")
	if end < 0 {
		return nil
	}
	var out []SessionProgramStatusEvent
	for _, frame := range parseSSETestFrames(body[:end+2]) {
		if frame.Event != "program_status" {
			continue
		}
		var ev SessionProgramStatusEvent
		if err := json.Unmarshal([]byte(frame.Data), &ev); err != nil {
			t.Fatalf("decode program_status frame %q: %v", frame.Data, err)
		}
		out = append(out, ev)
	}
	return out
}

// Kills a stream that sends no program_status frame, repeats an unchanged
// view on every poll, or never says when the status became unknown.
func TestSessionStreamEmitsProgramStatusOnChange(t *testing.T) {
	fs, _, id, name := programStatusSessionServer(t)
	srv := New(fs)
	peekPoll := make(chan time.Time)
	srv.structuredPeekPoll = peekPoll
	h := newTestCityHandlerWith(t, fs, srv)
	since := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	fs.sp.SetProgramPane(name, programRowAt(runtime.ProgramStatusShadow, runtime.ProgramWorking, "", since))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+id+"/stream?format=structured", nil).WithContext(ctx))
		close(done)
	}()
	tick := func() {
		select {
		case peekPoll <- time.Now():
		case <-time.After(10 * time.Millisecond):
		}
	}
	waitFrames := func(n int) []SessionProgramStatusEvent {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			if frames := streamedProgramStatusFrames(t, rec.BodyString()); len(frames) >= n {
				return frames
			}
			if time.Now().After(deadline) {
				t.Fatalf("stream never sent %d program_status frames; body:\n%s", n, rec.BodyString())
			}
			tick()
		}
	}

	frames := waitFrames(1)
	if ps := frames[0].ProgramStatus; ps == nil || ps.State != "working" || !ps.Since.Equal(since) {
		t.Fatalf("first frame = %+v, want working since %s", ps, since)
	}
	for range 5 {
		tick()
	}
	if frames := streamedProgramStatusFrames(t, rec.BodyString()); len(frames) != 1 {
		t.Fatalf("unchanged view sent %d frames, want 1; body:\n%s", len(frames), rec.BodyString())
	}

	fs.sp.SetProgramPane(name, programRowAt(runtime.ProgramStatusShadow, runtime.ProgramBlocked, runtime.ProgramBlockQuestion, since.Add(time.Minute)))
	frames = waitFrames(2)
	if ps := frames[1].ProgramStatus; ps == nil || ps.State != "blocked" || ps.Kind != "question" {
		t.Fatalf("second frame = %+v, want blocked:question", ps)
	}

	fs.sp.SetProgramPane(name, programRowAt(runtime.ProgramStatusOff, runtime.ProgramBlocked, runtime.ProgramBlockQuestion, since))
	frames = waitFrames(3)
	if frames[2].ProgramStatus != nil {
		t.Fatalf("third frame = %+v, want no program_status (unknown)", frames[2].ProgramStatus)
	}
	cancel()
	<-done
}

// Kills a CLI view that drops or mangles the generated client's program
// status, or invents one the API did not report.
func TestProgramStatusFromGenRoundTrips(t *testing.T) {
	if programStatusFromGen(nil) != nil {
		t.Fatal("absent program status decoded as present")
	}
	since := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	progress := 40
	want := &SessionProgramStatus{State: "blocked", Kind: "question", App: "probe", Progress: &progress, Since: since, QuietSince: &since, WorkingChildren: 2}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var g genclient.SessionProgramStatus
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if got := programStatusFromGen(&g); !sameProgramStatusView(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}
