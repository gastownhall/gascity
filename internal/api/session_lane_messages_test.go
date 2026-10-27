package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/session"
)

// /submit, /messages and extmsg notices under lane authority (design slice
// 6): with message listed, the API proposes lane message ops the lane host
// delivers, answers 202 once the op is committed, and emits the request's
// terminal event from the op's outcome after the settle window.

// laneMessageEnv is a city whose lane host executes message, its passes
// scripted by closeWith over the poked conversation's open lane message ops.
func laneMessageEnv(t *testing.T, closeWith func(opjournal.Op) *opjournal.Outcome) (*opsEnv, *lanePokeState) {
	t.Helper()
	e, st := newLaneOpsEnv(t, "message")
	st.onPoke = func(conv string) {
		if closeWith == nil {
			return
		}
		open, err := st.ops.ListOpen(conv)
		if err != nil {
			t.Errorf("listing open ops: %v", err)
			return
		}
		for _, op := range open {
			if op.Executor != opjournal.ExecutorLane || op.Kind != opjournal.KindMessage {
				continue
			}
			if o := closeWith(op); o != nil {
				if _, err := st.ops.Close(op.ID, *o, "lane:ctl"); err != nil {
					t.Errorf("closing op %s: %v", op.ID, err)
				}
			}
		}
	}
	return e, st
}

// shortSettle shortens the settle window for a test.
func shortSettle(t *testing.T) {
	t.Helper()
	old := laneSubmitSettle
	laneSubmitSettle = 50 * time.Millisecond
	t.Cleanup(func() { laneSubmitSettle = old })
}

// resultQueued decodes a terminal result event's queued flag.
func resultQueued(t *testing.T, e events.Event) bool {
	t.Helper()
	var p struct {
		Queued bool `json:"queued"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decoding %s: %v", e.Payload, err)
	}
	return p.Queued
}

func TestSubmitUnderLaneProposesAMessageOp(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		when             opjournal.When
		event            string
	}{
		{"submit default", "/submit", `{"message":"hello"}`, opjournal.WhenBoundary, events.RequestResultSessionSubmit},
		{"submit follow_up", "/submit", `{"message":"later","intent":"follow_up"}`, opjournal.WhenBoundary, events.RequestResultSessionSubmit},
		{"submit interrupt_now", "/submit", `{"message":"stop","intent":"interrupt_now"}`, opjournal.WhenNow, events.RequestResultSessionSubmit},
		{"messages", "/messages", `{"message":"hello"}`, opjournal.WhenBoundary, events.RequestResultSessionMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, st := laneMessageEnv(t, outcome(opjournal.ArmSucceeded, ""))
			info := e.session(t, "Lane submit")
			rec := e.post(t, "/session/"+info.ID+tc.path, "k-1", tc.body)
			wantStatus(t, tc.name, rec, http.StatusAccepted)
			acc := decodeAsyncAccepted(t, rec.Body)
			op, err := st.ops.Get(acc.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if op.Executor != opjournal.ExecutorLane || op.Kind != opjournal.KindMessage || op.Producer.Principal != "operator" ||
				op.ClientKey != "k-1" || acc.IdempotencyKey != "k-1" || op.Policy.Message.When != tc.when ||
				op.Policy.Message.Role != opjournal.RolePrompt || op.Policy.Message.Wake != opjournal.WakeIfNeeded {
				t.Fatalf("op = %+v", op)
			}
			evs := waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
			if evs[0].Type != tc.event || resultQueued(t, evs[0]) {
				t.Fatalf("terminal event %s %s, want %s queued false", evs[0].Type, evs[0].Payload, tc.event)
			}
			if got := deliveredCalls(e.fs.sp, info.SessionName); len(got) != 0 {
				t.Fatalf("the API delivered %+v itself", got)
			}
			if rows := e.opRows(t); len(rows) != 1 {
				t.Fatalf("op rows = %d, want the lane op alone", len(rows))
			}
		})
	}
}

func TestSubmitUnderLaneFailureIsRequestFailed(t *testing.T) {
	e, _ := laneMessageEnv(t, outcome(opjournal.ArmFailed, opjournal.ReasonUnsupported))
	info := e.session(t, "Lane failed")
	acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"hello"}`).Body)
	evs := waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
	if evs[0].Type != events.RequestFailed || !strings.Contains(string(evs[0].Payload), turnInterruptUnsupportedCode) {
		t.Fatalf("terminal event %s %s, want request.failed", evs[0].Type, evs[0].Payload)
	}
}

func TestSubmitUnderLaneMaterializesANamedSessionFirst(t *testing.T) {
	e, st := laneMessageEnv(t, outcome(opjournal.ArmSucceeded, ""))
	// A refusal known before the target resolves materializes nothing.
	before, _ := e.sessionRowsAndStarts(t)
	refused := e.post(t, "/session/"+url.PathEscape("myrig/worker")+"/submit", strings.Repeat("k", 513), `{"message":"x"}`)
	if refused.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid key: %d %s", refused.Code, refused.Body.String())
	}
	if after, _ := e.sessionRowsAndStarts(t); after != before {
		t.Fatalf("a refusal materialized %d rows", after-before)
	}
	rowsBefore, _ := e.sessionRowsAndStarts(t)
	rec := e.post(t, "/session/"+url.PathEscape("myrig/worker")+"/submit", "", `{"message":"wake up"}`)
	wantStatus(t, "named submit", rec, http.StatusAccepted)
	acc := decodeAsyncAccepted(t, rec.Body)
	rows, _ := e.sessionRowsAndStarts(t)
	if rows != rowsBefore+1 {
		t.Fatalf("session rows %d -> %d, want the named session materialized", rowsBefore, rows)
	}
	op, err := st.ops.Get(acc.RequestID)
	if err != nil || op.Conversation == "" || op.Conversation == previewConversation {
		t.Fatalf("op = %+v, %v; want it on the materialized row", op, err)
	}
}

func TestSubmitUnderLaneResumeWakesAHeldSession(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	info := e.session(t, "Held")
	sessions := session.NewStore(e.fs.SessionsBeadStore())
	wantStatus(t, "suspend", e.post(t, "/session/"+info.ID+"/suspend", "", ""), http.StatusOK)
	if held, err := sessions.Get(info.ID); err != nil || held.State != session.StateSuspended {
		t.Fatalf("session after suspend = %+v, %v; want it suspended", held, err)
	}
	rec := e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"go","resume":true}`)
	wantStatus(t, "resume", rec, http.StatusAccepted)
	acc := decodeAsyncAccepted(t, rec.Body)
	if _, err := st.ops.Get(acc.RequestID); err != nil {
		t.Fatalf("op: %v", err)
	}
	got, err := sessions.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == session.StateSuspended {
		t.Fatalf("session state %s after resume: true, want the hold consumed", got.State)
	}
}

func TestSubmitUnderLaneResultAfterTheSettleWindow(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	info := e.session(t, "Busy")
	acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"hello"}`).Body)
	evs := waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
	if evs[0].Type != events.RequestResultSessionSubmit || !resultQueued(t, evs[0]) {
		t.Fatalf("terminal event %s %s, want the result queued true", evs[0].Type, evs[0].Payload)
	}
	if op, err := st.ops.Get(acc.RequestID); err != nil || op.Status == opjournal.StatusClosed {
		t.Fatalf("op = %+v, %v; want it still open for the lane", op, err)
	}
}

func TestSubmitUnderLaneSameKeyRetryReplays(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	info := e.session(t, "Retry")
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k-r", `{"message":"hello"}`).Body)
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	// An open lane op: 202 replayed, nothing proposed again.
	e.restart(t)
	e.resolver.state = st
	open := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k-r", `{"message":"hello"}`).Body)
	if open.RequestID != first.RequestID || !open.Replayed {
		t.Fatalf("retry of an open op = %+v, want %s replayed", open, first.RequestID)
	}
	// A closed op: OJ-2b's replay, its terminal event re-emitted.
	if _, err := st.ops.Close(first.RequestID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:ctl"); err != nil {
		t.Fatal(err)
	}
	e.restart(t)
	e.resolver.state = st
	closed := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k-r", `{"message":"hello"}`).Body)
	if closed.RequestID != first.RequestID || !closed.Replayed {
		t.Fatalf("retry of a closed op = %+v", closed)
	}
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 2)
	// Another request under the key is idempotency_mismatch.
	e.restart(t)
	e.resolver.state = st
	if rec := e.post(t, "/session/"+info.ID+"/submit", "k-r", `{"message":"other"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("other request under the key: %d %s", rec.Code, rec.Body.String())
	}
	if rows := e.opRows(t); len(rows) != 1 {
		t.Fatalf("op rows = %d, want one", len(rows))
	}
}

// An extmsg member notice is a lane notice from gc:extmsg; its delivered
// text names the source and carries no identifier (D-NOMARK).
func TestExtmsgNotifyUnderLaneProposesANotice(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	info := e.session(t, "Member")
	s := e.server(t)
	if err := s.sendBackgroundMessageToSession(context.Background(), e.fs.SessionsBeadStore().Store, info.ID, "new message in #ops"); err != nil {
		t.Fatalf("notice: %v", err)
	}
	open, err := st.ops.ListOpen(info.ID)
	if err != nil || len(open) != 1 {
		t.Fatalf("open ops = %+v, %v", open, err)
	}
	op := open[0]
	if op.Executor != opjournal.ExecutorLane || op.Producer.Principal != extmsgNoticePrincipal || op.Producer.Actor != opjournal.ActorBackground ||
		op.Policy.Message.Role != opjournal.RoleNotice || op.Policy.Message.When != opjournal.WhenBoundary || op.Policy.Message.Wake != opjournal.WakeIfNeeded {
		t.Fatalf("op = %+v", op)
	}
	text, err := opjournal.RenderDelivery([]opjournal.Op{op}, opjournal.TextReminder, false)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "<system-reminder>\nYou have a deferred reminder that was queued until a safe boundary:\n\n- [extmsg] new message in #ops\n\nHandle them after this turn.\n</system-reminder>\n"
	if text != golden {
		t.Fatalf("delivered text = %q, want %q", text, golden)
	}
	for _, id := range []string{op.ID, op.ClientKey, op.Conversation} {
		if strings.Contains(text, id) {
			t.Fatalf("delivered text %q carries identifier %q", text, id)
		}
	}
	if got := deliveredCalls(e.fs.sp, info.SessionName); len(got) != 0 {
		t.Fatalf("the API delivered %+v itself", got)
	}
}

// POST /sessions for a provider session with an initial message, under lane
// authority, proposes the message as an operator lane message op at the
// boundary instead of delivering it through the session layer itself.
func TestCreateWithMessageUnderLaneProposesAMessageOp(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	rec := e.post(t, "/sessions", "", `{"kind":"provider","name":"test-agent","message":"hello from create"}`)
	wantStatus(t, "create", rec, http.StatusAccepted)
	acc := decodeAsyncAccepted(t, rec.Body)
	success, failure := waitForSessionCreateResult(t, e.fs.eventProv, acc.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if got := deliveredCalls(e.fs.sp, success.Session.SessionName); len(got) != 0 {
		t.Fatalf("the API delivered the initial message itself: %+v", got)
	}
	open, err := st.ops.ListOpen(success.Session.ID)
	if err != nil || len(open) != 1 {
		t.Fatalf("open ops = %+v, %v; want the initial message's lane op", open, err)
	}
	op := open[0]
	if op.Executor != opjournal.ExecutorLane || op.Kind != opjournal.KindMessage || op.Payload != "hello from create" ||
		op.Producer.Actor != opjournal.ActorOperator || op.Policy.Message.When != opjournal.WhenBoundary || op.Policy.Message.Role != opjournal.RolePrompt {
		t.Fatalf("op = %+v", op)
	}
}

func TestSubmitWithoutLaneMessageKeepsOJ2b(t *testing.T) {
	e, st := newLaneOpsEnv(t, "interrupt")
	e.fs.cfg.Ops.ContentMode = "auto"
	e.restart(t)
	e.resolver.state = st
	info := e.session(t, "Legacy submit")
	acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"hello"}`).Body)
	op := e.waitClosed(t, acc.RequestID)
	if op.Executor == opjournal.ExecutorLane {
		t.Fatalf("op = %+v, want OJ-2b's legacy op", op)
	}
	if got := deliveredCalls(e.fs.sp, info.SessionName); len(got) != 1 {
		t.Fatalf("delivered %d, want the legacy delivery's one", len(got))
	}
}
