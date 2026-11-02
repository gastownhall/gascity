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
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
// and runtime name. Its city publishes the switch on, so each row's mode is
// the one switch a reading is taken under.
func programStatusSessionServer(t *testing.T) (*fakeState, http.Handler, string, string) {
	t.Helper()
	fs := newSessionFakeState(t)
	fs.cfg.Session.ProgramStatus = string(runtime.ProgramStatusOn)
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

// Kills a projection that copies a record's time past year 9999 onto the
// wire, where encoding/json cannot write it: one such row failed the whole
// session list, agent read and stream. The reading is shown as unknown.
func TestUnencodableProgramStatusTimeReadsUnknown(t *testing.T) {
	fs, h, id, name := programStatusSessionServer(t)
	if err := fs.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	past9999 := time.UnixMilli(253402300800000)
	forgedSince := programRowAt(runtime.ProgramStatusOn, runtime.ProgramWorking, "", past9999)
	forgedQuiet := programRowAt(runtime.ProgramStatusOn, runtime.ProgramIdle, "", time.Now())
	forgedQuiet.Record.QuietSince = past9999
	for label, row := range map[string]runtime.ProgramPaneRow{"since": forgedSince, "quiet since": forgedQuiet} {
		fs.sp.SetProgramPane(name, row)
		fs.sp.SetProgramPane("myrig--worker", row)
		var resp sessionResponse
		body := getProgramStatusJSON(t, h, cityURL(fs, "/session/")+id, &resp)
		var list struct {
			Items []sessionResponse `json:"items"`
		}
		listBody := getProgramStatusJSON(t, h, cityURL(fs, "/sessions"), &list)
		var agent agentResponse
		agentBody := getProgramStatusJSON(t, h, cityURL(fs, "/agent/myrig/worker"), &agent)
		for what, b := range map[string]string{"session": body, "session list": listBody, "agent": agentBody} {
			if strings.Contains(b, `"program_status"`) || strings.Contains(b, `"activity_source"`) {
				t.Errorf("%s past year 9999: the %s shows the record: %s", label, what, b)
			}
		}
		if programStatusView(runtime.JudgeProgramStatus(row)) != nil {
			t.Errorf("%s past year 9999: the stream would frame it", label)
		}
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

	// The list projects each running agent's reading as the single read does:
	// under on the record decides state and activity and names its source;
	// under shadow it is shown and decides nothing. Each mode reads a server of
	// its own, because the list's response cache answers a second read within
	// its time bucket with the first read's body.
	for mode, want := range map[runtime.ProgramStatusMode]string{runtime.ProgramStatusShadow: "idle", runtime.ProgramStatusOn: "blocked"} {
		fs, h, _, _ := programStatusSessionServer(t)
		if err := fs.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		fs.sp.SetProgramPane("myrig--worker", programRowAt(mode, runtime.ProgramBlocked, runtime.ProgramBlockAuth, time.Now()))
		wantActivity, wantSource := "", ""
		if mode == runtime.ProgramStatusOn {
			wantActivity, wantSource = activityInTurn, activitySourceProgram
		}
		var list struct {
			Items []agentResponse `json:"items"`
		}
		listBody := getProgramStatusJSON(t, h, cityURL(fs, "/agents"), &list)
		if len(list.Items) != 1 {
			t.Fatalf("%s: agent list has %d items, want the one agent: %s", mode, len(list.Items), listBody)
		}
		item := list.Items[0]
		if item.State != want || item.ProgramStatus == nil || item.ProgramStatus.State != "blocked" || item.ProgramStatus.Kind != "auth" ||
			item.Activity != wantActivity || item.ActivitySource != wantSource {
			t.Fatalf("%s: agent list item state %q program_status %+v activity %q source %q; want %q with the blocked:auth status, activity %q from %q: %s",
				mode, item.State, item.ProgramStatus, item.Activity, item.ActivitySource, want, wantActivity, wantSource, listBody)
		}
	}
}

// publishProgramStatusSwitch swaps fs's config for a copy whose [session]
// program_status is mode, as a reload publishes it: the provider and its
// leaves, which read under the switch they were last set to, are kept.
func publishProgramStatusSwitch(fs *fakeState, mode runtime.ProgramStatusMode) {
	next := *fs.cfg
	next.Session.ProgramStatus = string(mode)
	fs.cfg = &next
}

// Kills a kill switch that waits for the leaves: the published switch caps
// every reading, so a reload from on to shadow stops a reading from deciding
// and one to off hides it, on the session and agent reads, while the leaf
// still reads under on.
func TestPublishedSwitchCapsProgramStatusWithoutRestart(t *testing.T) {
	fs, h, id, name := programStatusSessionServer(t)
	since := time.Now().Add(-time.Minute)
	fs.sp.SetProgramPane(name, programRowAt(runtime.ProgramStatusOn, runtime.ProgramWorking, "", since))
	if err := fs.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	fs.sp.SetProgramPane("myrig--worker", programRowAt(runtime.ProgramStatusOn, runtime.ProgramBlocked, runtime.ProgramBlockAuth, since))
	read := func() (sessionResponse, string, agentResponse, string, string) {
		t.Helper()
		var resp sessionResponse
		sessionBody := getProgramStatusJSON(t, h, cityURL(fs, "/session/")+id, &resp)
		var agent agentResponse
		agentBody := getProgramStatusJSON(t, h, cityURL(fs, "/agent/myrig/worker"), &agent)
		var list struct {
			Items []agentResponse `json:"items"`
		}
		listBody := getProgramStatusJSON(t, h, cityURL(fs, "/agents"), &list)
		return resp, sessionBody, agent, agentBody, listBody
	}

	resp, _, agent, _, _ := read()
	if resp.ProgramStatus == nil || resp.ActivitySource != activitySourceProgram || agent.State != "blocked" {
		t.Fatalf("published on: session %+v (source %q), agent state %q; want the record deciding both", resp.ProgramStatus, resp.ActivitySource, agent.State)
	}

	publishProgramStatusSwitch(fs, runtime.ProgramStatusShadow)
	resp, sessionBody, agent, _, _ := read()
	if resp.ProgramStatus == nil || resp.ProgramStatus.State != "working" || resp.Activity != "" || resp.ActivitySource != "" {
		t.Fatalf("published shadow: program_status %+v activity %q source %q; want it shown, the transcript's activity (none) kept: %s", resp.ProgramStatus, resp.Activity, resp.ActivitySource, sessionBody)
	}
	if agent.ProgramStatus == nil || agent.State != "idle" {
		t.Fatalf("published shadow: agent state %q program_status %+v; want today's idle with the status shown", agent.State, agent.ProgramStatus)
	}

	publishProgramStatusSwitch(fs, runtime.ProgramStatusOff)
	resp, sessionBody, agent, agentBody, listBody := read()
	for label, body := range map[string]string{"session": sessionBody, "agent": agentBody, "agent list": listBody} {
		if strings.Contains(body, `"program_status"`) || strings.Contains(body, `"activity_source"`) {
			t.Errorf("published off: %s body shows the record: %s", label, body)
		}
	}
	if resp.Activity != "" || agent.State != "idle" {
		t.Fatalf("published off: session activity %q, agent state %q; want today's (none, idle)", resp.Activity, agent.State)
	}
}

// Kills a program-status read that probes liveness first. A session or agent
// row answered IsRunning for its running field, and the stream needs no
// probe: a stopped session reads Unread or exited, which nothing shows. A
// backend without the capability is never asked, and with the switch off
// nothing is read.
func TestProgramStatusReadAddsNoLivenessProbe(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Session.ProgramStatus = string(runtime.ProgramStatusOn)
	srv := New(fs)
	var infos []session.Info
	for _, title := range []string{"One", "Two", "Three"} {
		infos = append(infos, createTestSession(t, fs.cityBeadStore, fs.sp, title))
	}
	isRunning := func() int {
		n := 0
		for _, c := range fs.sp.SnapshotCalls() {
			if c.Method == "IsRunning" {
				n++
			}
		}
		return n
	}

	fs.sp.Calls = nil
	for _, info := range infos {
		var resp sessionResponse
		srv.enrichSessionResponse(&resp, info, fs.cfg, fs.sp, false, false, false, 0)
		if !resp.Running {
			t.Fatalf("%s reads stopped", info.SessionName)
		}
	}
	if got := isRunning(); got != len(infos) {
		t.Fatalf("%d running session responses made %d IsRunning calls, want one each: %+v", len(infos), got, fs.sp.SnapshotCalls())
	}

	fs.sp.Calls = nil
	for _, info := range infos {
		if _, changed := srv.newProgramStatusFrames(info).next(); changed {
			t.Fatalf("%s: a backend without the capability sent a frame", info.SessionName)
		}
	}
	if calls := fs.sp.SnapshotCalls(); len(calls) != 0 {
		t.Fatalf("stream reloads over a backend without the capability called it: %+v", calls)
	}

	agents := []string{"worker", "helper", "scout"}
	next := *fs.cfg
	next.Agents = append([]config.Agent(nil), fs.cfg.Agents...)
	for _, name := range agents[1:] {
		next.Agents = append(next.Agents, config.Agent{Name: name, Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1)})
	}
	fs.cfg = &next
	for _, name := range agents {
		if err := fs.sp.Start(context.Background(), "myrig--"+name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	h := newTestCityHandlerWith(t, fs, srv)
	fs.sp.Calls = nil
	var list struct {
		Items []agentResponse `json:"items"`
	}
	getProgramStatusJSON(t, h, cityURL(fs, "/agents"), &list)
	if got := isRunning(); got != len(agents) {
		t.Fatalf("an agent list of %d running agents made %d IsRunning calls, want one each: %+v", len(agents), got, fs.sp.SnapshotCalls())
	}

	fs.sessionProvider = runtime.FullFake{Fake: fs.sp}
	publishProgramStatusSwitch(fs, runtime.ProgramStatusOff)
	fs.sp.Calls = nil
	var resp sessionResponse
	srv.enrichSessionResponse(&resp, infos[0], fs.cfg, fs.sp, false, false, false, 0)
	if _, changed := srv.newProgramStatusFrames(infos[0]).next(); changed {
		t.Fatal("the stream sent a frame with the switch off")
	}
	if got := fs.sp.CountCalls("ProgramStatus", infos[0].SessionName); got != 0 {
		t.Fatalf("with the switch off the reads asked the runtime %d times", got)
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

// programStreamSession creates an active session on fs whose stream reads a
// transcript, a Claude transcript under searchBase, when transcript is set,
// and otherwise a session with none, whose stream peeks the pane.
func programStreamSession(t *testing.T, fs *fakeState, searchBase string, transcript bool) session.Info {
	t.Helper()
	if !transcript {
		return createTestSession(t, fs.cityBeadStore, fs.sp, "Peek")
	}
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude",
		Resume:    session.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	return info
}

// Kills a session stream loop that drops the program_status frame. A stream
// reads the session's transcript when it has one and peeks the pane when it
// has none, in each of the raw, conversation and structured formats: six
// loops. Each sends the current view when it opens, repeats no unchanged view
// across reloads, sends a change at the next reload, and says when the status
// became unknown.
func TestEverySessionStreamLoopSendsProgramStatusFrames(t *testing.T) {
	since := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	for _, format := range []string{"raw", "conversation", "structured"} {
		for _, transcript := range []bool{true, false} {
			source := "peek"
			if transcript {
				source = "transcript"
			}
			t.Run(format+"/"+source, func(t *testing.T) {
				fs := newSessionFakeState(t)
				fs.cfg.Session.ProgramStatus = string(runtime.ProgramStatusOn)
				fs.sessionProvider = runtime.FullFake{Fake: fs.sp}
				srv := New(fs)
				searchBase := t.TempDir()
				srv.sessionLogSearchPaths = []string{searchBase}
				h := newTestCityHandlerWith(t, fs, srv)
				info := programStreamSession(t, fs, searchBase, transcript)
				fs.sp.SetProgramPane(info.SessionName, programRowAt(runtime.ProgramStatusShadow, runtime.ProgramWorking, "", since))

				url := cityURL(fs, "/session/") + info.ID + "/stream"
				if format != "conversation" {
					url += "?format=" + format
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				rec := newSyncResponseRecorder()
				done := make(chan struct{})
				go func() {
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil).WithContext(ctx))
					close(done)
				}()
				defer func() {
					cancel()
					<-done
				}()

				frames := func() []SessionProgramStatusEvent { return streamedProgramStatusFrames(t, rec.BodyString()) }
				// Each reload reads the status once, so the read count says
				// a reload finished, and with it any frame it sent.
				reads := func() int { return fs.sp.CountCalls("ProgramStatus", info.SessionName) }
				await := func(what string, cond func() bool, wake bool) {
					t.Helper()
					met := false
					waitForRecorderBody(rec, 10*time.Second, func(string) bool {
						if met = cond(); !met && wake {
							fs.eventProv.(*events.Fake).Record(events.Event{Type: events.WorkerOperation, Subject: info.ID})
						}
						return met
					})
					if !met {
						t.Fatalf("%s never came; body:\n%s", what, rec.BodyString())
					}
				}

				await("the opening frame", func() bool { return len(frames()) >= 1 }, false)
				if ps := frames()[0].ProgramStatus; ps == nil || ps.State != "working" || !ps.Since.Equal(since) {
					t.Fatalf("opening frame = %+v, want working since %s", ps, since)
				}
				if read := strings.Contains(rec.BodyString(), "2025-01-01T00:00:01Z"); read != transcript {
					t.Fatalf("stream read the transcript: %t, want %t; body:\n%s", read, transcript, rec.BodyString())
				}
				opened := reads()
				await("two reloads", func() bool { return reads() >= opened+2 }, true)
				if n := len(frames()); n != 1 {
					t.Fatalf("reloads over an unchanged view sent %d frames, want 1; body:\n%s", n, rec.BodyString())
				}

				fs.sp.SetProgramPane(info.SessionName, programRowAt(runtime.ProgramStatusShadow, runtime.ProgramBlocked, runtime.ProgramBlockQuestion, since.Add(time.Minute)))
				await("the changed frame", func() bool { return len(frames()) >= 2 }, true)
				if ps := frames()[1].ProgramStatus; ps == nil || ps.State != "blocked" || ps.Kind != "question" {
					t.Fatalf("second frame = %+v, want blocked:question", ps)
				}

				fs.sp.SetProgramPane(info.SessionName, programRowAt(runtime.ProgramStatusOff, runtime.ProgramBlocked, runtime.ProgramBlockQuestion, since))
				await("the unknown frame", func() bool { return len(frames()) >= 3 }, true)
				if ps := frames()[2].ProgramStatus; ps != nil {
					t.Fatalf("third frame = %+v, want no program_status (unknown)", ps)
				}
			})
		}
	}
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
