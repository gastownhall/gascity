package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
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

// willStartOf decodes a lane message result's will_start and
// will_not_start_reason.
func willStartOf(t *testing.T, e events.Event) (*bool, string) {
	t.Helper()
	var p struct {
		WillStart          *bool  `json:"will_start"`
		WillNotStartReason string `json:"will_not_start_reason"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decoding %s: %v", e.Payload, err)
	}
	return p.WillStart, p.WillNotStartReason
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

// resume: true consumes an operator's hold, which the lane host's wake
// request cannot pass, and records in the same write the wake the session
// now needs: it is asleep, unsuspended and woken, and the lane's wake
// request coalesces onto that wake.
func TestSubmitUnderLaneResumeConsumesTheHold(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	info := e.session(t, "Held")
	sessions := session.NewStore(e.fs.SessionsBeadStore())
	wantStatus(t, "suspend", e.post(t, "/session/"+info.ID+"/suspend", "", ""), http.StatusOK)
	if held, err := sessions.Get(info.ID); err != nil || held.State != session.StateSuspended {
		t.Fatalf("session after suspend = %+v, %v; want it suspended", held, err)
	}
	if outcome, err := sessions.RequestWakeUnlessHeld(info.ID, false, time.Now().UTC()); err != nil || outcome != session.WakeHeld {
		t.Fatalf("lane wake request on the held session = %v, %v; want WakeHeld", outcome, err)
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
	if got.State != session.StateAsleep || got.WakeRequest != string(session.WakeCauseExplicit) {
		t.Fatalf("session after resume: true = state %s, wake_request %q; want the hold consumed, asleep, its wake recorded", got.State, got.WakeRequest)
	}
	if outcome, err := sessions.RequestWakeUnlessHeld(info.ID, false, time.Now().UTC()); err != nil || outcome != session.WakeRecorded {
		t.Fatalf("lane wake request after resume: true = %v, %v; want WakeRecorded", outcome, err)
	}
}

// resume: true consumes the operator's hold only once its op is proposed: a
// proposal the journal refuses answers 503 with the session still suspended
// and nothing queued, and a proposal whose commit answered an error (Propose
// finds the request's own row) still resumes.
func TestSubmitUnderLaneResumeFollowsTheProposal(t *testing.T) {
	e, _ := laneMessageEnv(t, nil)
	shortSettle(t)
	info := e.session(t, "Held")
	sessions := session.NewStore(e.fs.SessionsBeadStore())
	wantStatus(t, "suspend", e.post(t, "/session/"+info.ID+"/suspend", "", ""), http.StatusOK)
	e.store.set(func(s *opsTestStore) { s.createErr = errors.New("journal write refused") })
	refused := e.post(t, "/session/"+info.ID+"/submit", "k-held", `{"message":"go","resume":true}`)
	wantStatus(t, "refused proposal", refused, http.StatusServiceUnavailable)
	if got, err := sessions.Get(info.ID); err != nil || got.State != session.StateSuspended || got.WakeRequest != "" {
		t.Fatalf("session after a refused proposal = %+v, %v; want it still suspended, no wake", got, err)
	}
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("op rows = %d, want none", len(rows))
	}
	e.store.set(func(s *opsTestStore) { s.createErr, s.ambiguous = nil, true })
	rec := e.post(t, "/session/"+info.ID+"/submit", "k-held", `{"message":"go","resume":true}`)
	wantStatus(t, "proposal whose commit answered an error", rec, http.StatusAccepted)
	if got, err := sessions.Get(info.ID); err != nil || got.State != session.StateAsleep || got.WakeRequest != string(session.WakeCauseExplicit) {
		t.Fatalf("session after its own op was found = %+v, %v; want the hold consumed and its wake recorded", got, err)
	}
}

// laneRefusingState is a lane host city whose controller would refuse to
// start any session, counting how often it is asked.
type laneRefusingState struct {
	*lanePokeState
	asked atomic.Int32
}

func (s *laneRefusingState) WakeStartRefusal(session.Info) (string, bool) {
	s.asked.Add(1)
	return "it is a dependency-only session; it starts with its dependents", true
}

// resume: true on a running session consumes nothing and runs no wake: it
// cancels none of the session's waits, asks no will-not-start verdict
// (which refused the message 409 after the wake ran), and the message is
// proposed, on /submit and /messages alike.
func TestSubmitUnderLaneResumeLeavesARunningSessionAlone(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	refusing := &laneRefusingState{lanePokeState: st}
	e.resolver.state = refusing
	info := e.session(t, "Running")
	if !e.fs.sp.IsRunning(info.SessionName) {
		t.Fatalf("session %s is not running", info.ID)
	}
	sessions := session.NewStore(e.fs.SessionsBeadStore())
	wait, err := sessions.CreateWait(session.WaitSpec{SessionID: info.ID, Kind: "deps", DepIDs: []string{"gc-dep"}, DepMode: "all", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("CreateWait: %v", err)
	}
	for _, path := range []string{"/submit", "/messages"} {
		rec := e.post(t, "/session/"+info.ID+path, "", `{"message":"go","resume":true}`)
		wantStatus(t, path, rec, http.StatusAccepted)
		if _, err := st.ops.Get(decodeAsyncAccepted(t, rec.Body).RequestID); err != nil {
			t.Fatalf("%s op: %v", path, err)
		}
	}
	if n := refusing.asked.Load(); n != 0 {
		t.Fatalf("the controller's will-not-start verdict was asked %d times, want never", n)
	}
	got, err := sessions.GetWait(wait.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "closed" || got.State != "pending" {
		t.Fatalf("wait after resume: true = %s/%s, want it open and pending", got.Status, got.State)
	}
	if row, err := sessions.Get(info.ID); err != nil || row.WakeRequest != "" {
		t.Fatalf("session after resume: true = wake_request %q, %v; want no wake recorded", row.WakeRequest, err)
	}
}

// resume: true to a closed session is refused 409, as /wake refuses it,
// before anything is written: the session's row alone decides it, so no op
// exists to deliver and the lane is not rung.
func TestSubmitUnderLaneResumeRefusesAClosedSession(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	info := e.session(t, "Closed")
	if err := e.fs.cityBeadStore.Close(info.ID); err != nil {
		t.Fatal(err)
	}
	rec := e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"go","resume":true}`)
	wantStatus(t, "resume to a closed session", rec, http.StatusConflict)
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("op rows = %d, want none", len(rows))
	}
	if poked := st.poked(); len(poked) != 0 {
		t.Fatalf("lane rung for %v, want never", poked)
	}
}

// resume: true to a session the operator suspended while its drain stops it
// is refused 409 session_stopping before anything is written, so the key
// stays free: once the drain is done, the same request is accepted and its
// resume consumes the hold. Kills a refusal decided only after the op is
// written, whose cancel spends the key (the retry then replays it as
// request.failed).
func TestSubmitUnderLaneResumeRefusedByTheRowLeavesTheKeyFree(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	info := e.session(t, "Draining")
	sessions := session.NewStore(e.fs.SessionsBeadStore())
	wantStatus(t, "suspend", e.post(t, "/session/"+info.ID+"/suspend", "", ""), http.StatusOK)
	if err := e.store.SetMetadata(info.ID, "state", string(session.StateDraining)); err != nil {
		t.Fatal(err)
	}
	rec := e.post(t, "/session/"+info.ID+"/submit", "k-stop", `{"message":"go","resume":true}`)
	wantStatus(t, "resume while a drain stops the session", rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "session_stopping") {
		t.Fatalf("body = %s, want session_stopping", rec.Body.String())
	}
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("op rows = %d, want none", len(rows))
	}
	if err := e.store.SetMetadata(info.ID, "state", string(session.StateSuspended)); err != nil {
		t.Fatal(err)
	}
	retry := e.post(t, "/session/"+info.ID+"/submit", "k-stop", `{"message":"go","resume":true}`)
	wantStatus(t, "same request once the drain is done", retry, http.StatusAccepted)
	acc := decodeAsyncAccepted(t, retry.Body)
	if op, err := st.ops.Get(acc.RequestID); err != nil || acc.Replayed || op.Status == opjournal.StatusClosed {
		t.Fatalf("retry = %+v, op %+v, %v; want a fresh acceptance of an open op", acc, op, err)
	}
	if got, err := sessions.Get(info.ID); err != nil || got.State != session.StateAsleep || got.WakeRequest != string(session.WakeCauseExplicit) {
		t.Fatalf("session after the retry = %+v, %v; want the hold consumed and its wake recorded", got, err)
	}
}

// A resume refused once the op is written (here the session closes between
// the row's check and the resume, as the op's create commits and answers an
// error, under a key the request minted, so the op is the request's own
// though Propose did not report it created) cancels that op before the lane
// is rung, and the 409 says the key is spent. Kills an ownership read from
// Propose's created alone, which leaves the op open after the 409.
func TestSubmitUnderLaneResumeRefusedAfterTheProposalCancelsTheOp(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	info := e.session(t, "Closing")
	e.store.set(func(s *opsTestStore) {
		s.ambiguous = true
		s.onOpCreate = func() {
			if err := e.store.Close(info.ID); err != nil {
				t.Errorf("closing the session: %v", err)
			}
		}
	})
	rec := e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"go","resume":true}`)
	wantStatus(t, "resume refused after the proposal", rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "resend with a new Idempotency-Key") {
		t.Fatalf("body = %s, want it to say the key is spent", rec.Body.String())
	}
	rows := e.opRows(t)
	if len(rows) != 1 {
		t.Fatalf("op rows = %d, want the one proposed", len(rows))
	}
	op, err := st.ops.Get(rows[0].ID)
	if arm, _ := op.Outcome.Effective(); err != nil || op.Status != opjournal.StatusClosed || arm != opjournal.ArmCanceled {
		t.Fatalf("op = %+v, %v; want it closed canceled", op, err)
	}
	if poked := st.poked(); len(poked) != 0 {
		t.Fatalf("lane rung for %v, want never", poked)
	}
}

// A resume refused once the op is written, whose op its cancel does not
// close, offers no resend under a new Idempotency-Key: the lane host's scan
// took the op before the cancel (the cancel is only requested), or the
// cancel's write failed, so the op may still deliver the message, which a
// resend would deliver twice. The 409 says so, and that a retry under the
// key reports what became of the op; here the lane host delivers it, and
// the retry replays that. The session is suspended and its drain starts as
// the op's create commits, so the resume is refused 409 session_stopping.
// Kills an error that says the op was canceled whatever its cancel did.
func TestSubmitUnderLaneResumeRefusedOffersANewKeyOnlyForACanceledOp(t *testing.T) {
	for name, tc := range map[string]struct {
		// take moves the op to executing as its create commits, as the
		// lane host's scan takes it; failCancel fails the cancel's write.
		take, failCancel bool
		want             opjournal.Status
	}{
		"taken by the lane host first": {take: true, want: opjournal.StatusExecuting},
		"its cancel failed":            {failCancel: true, want: opjournal.StatusSubmitted},
	} {
		t.Run(name, func(t *testing.T) {
			e, st := laneMessageEnv(t, nil)
			shortSettle(t)
			info := e.session(t, "Not canceled")
			wantStatus(t, "suspend", e.post(t, "/session/"+info.ID+"/suspend", "", ""), http.StatusOK)
			id, err := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "k-open")
			if err != nil {
				t.Fatal(err)
			}
			e.store.set(func(s *opsTestStore) {
				if tc.failCancel {
					s.updateFails = 1
				}
				s.onOpCreate = func() {
					if err := e.store.SetMetadata(info.ID, "state", string(session.StateDraining)); err != nil {
						t.Errorf("draining the session: %v", err)
					}
					if !tc.take {
						return
					}
					if _, err := st.ops.Transition(id, opjournal.StatusSubmitted, opjournal.StatusExecuting, "lane:n1"); err != nil {
						t.Errorf("taking the op: %v", err)
					}
				}
			})
			const body = `{"message":"go","resume":true}`
			rec := e.post(t, "/session/"+info.ID+"/submit", "k-open", body)
			wantStatus(t, "resume refused after the proposal", rec, http.StatusConflict)
			if got := rec.Body.String(); !strings.Contains(got, "session_stopping") || strings.Contains(got, "new Idempotency-Key") ||
				!strings.Contains(got, "may still deliver the message") {
				t.Fatalf("body = %s, want session_stopping saying the op may still deliver, with no resend under a new key", got)
			}
			op, err := st.ops.Get(id)
			if err != nil || op.Status != tc.want || op.Cancel.Recorded() != tc.take {
				t.Fatalf("op = %+v, %v; want it %s, its cancel requested %t", op, err, tc.want, tc.take)
			}
			if poked := st.poked(); len(poked) != 0 {
				t.Fatalf("lane rung for %v, want never", poked)
			}
			if _, err := st.ops.Close(id, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:n1"); err != nil {
				t.Fatal(err)
			}
			e.restart(t)
			e.resolver.state = st
			retry := e.post(t, "/session/"+info.ID+"/submit", "k-open", body)
			wantStatus(t, "same-key retry once the lane host delivered the op", retry, http.StatusAccepted)
			if acc := decodeAsyncAccepted(t, retry.Body); acc.RequestID != id || !acc.Replayed {
				t.Fatalf("retry = %+v, want op %s replayed", acc, id)
			}
		})
	}
}

// crashAfterCommit posts body to session id's /submit under key, the
// request dying (a panic) once its op row commits and before its resume,
// and returns the op row it left.
func crashAfterCommit(t *testing.T, e *opsEnv, id, key, body string) beads.Bead {
	t.Helper()
	e.store.set(func(s *opsTestStore) { s.panicAfterCreate = true })
	func() {
		defer func() { _ = recover() }()
		rec := e.post(t, "/session/"+id+"/submit", key, body)
		if rec.Code < 500 {
			t.Fatalf("the crashed request answered %d: %s", rec.Code, rec.Body.String())
		}
	}()
	rows := e.opRows(t)
	if len(rows) != 1 {
		t.Fatalf("op rows after the crash = %d, want 1", len(rows))
	}
	return rows[0]
}

// A request that dies after its op commits and before its resume (a crash)
// leaves the op open behind the operator's hold. The same-key retry consumes
// the hold, recording the session's wake, rings the lane and is accepted
// with the op, so the message is delivered rather than waiting on the hold
// until its start_by fails it. Kills a retry answered from the op without
// its resume.
func TestSubmitUnderLaneRetryResumesAfterACrashAfterCommit(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	info := e.session(t, "Crash")
	sessions := session.NewStore(e.fs.SessionsBeadStore())
	wantStatus(t, "suspend", e.post(t, "/session/"+info.ID+"/suspend", "", ""), http.StatusOK)
	row := crashAfterCommit(t, e, info.ID, "k-crash", `{"message":"go","resume":true}`)
	if got, err := sessions.Get(info.ID); err != nil || got.State != session.StateSuspended || got.WakeRequest != "" {
		t.Fatalf("session after the crash = %+v, %v; want the hold standing", got, err)
	}
	e.restart(t)
	e.resolver.state = st
	rec := e.post(t, "/session/"+info.ID+"/submit", "k-crash", `{"message":"go","resume":true}`)
	wantStatus(t, "same-key retry", rec, http.StatusAccepted)
	acc := decodeAsyncAccepted(t, rec.Body)
	if acc.RequestID != row.ID || acc.Replayed {
		t.Fatalf("retry = %+v, want op %s accepted, not replayed", acc, row.ID)
	}
	if got, err := sessions.Get(info.ID); err != nil || got.State != session.StateAsleep || got.WakeRequest != string(session.WakeCauseExplicit) {
		t.Fatalf("session after the retry = %+v, %v; want the hold consumed and its wake recorded", got, err)
	}
	if poked := st.poked(); !slices.Contains(poked, info.ID) {
		t.Fatalf("lane rung for %v, want %s", poked, info.ID)
	}
}

// A same-key retry of an open lane op whose resume the session refuses
// answers the refusal and leaves the op open: an earlier answer may have
// accepted it, so a retry's refusal cancels nothing and spends no key. The
// hold predates the op (its request crashed before its resume), so the
// retry's resume is the one that must consume it, and a drain is stopping
// the session. Kills a retry that cancels the op it found.
func TestSubmitUnderLaneRetryResumeRefusalLeavesTheOpOpen(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	info := e.session(t, "Retry held")
	wantStatus(t, "suspend", e.post(t, "/session/"+info.ID+"/suspend", "", ""), http.StatusOK)
	row := crashAfterCommit(t, e, info.ID, "k-held", `{"message":"go","resume":true}`)
	if err := e.store.SetMetadata(info.ID, "state", string(session.StateDraining)); err != nil {
		t.Fatal(err)
	}
	e.restart(t)
	e.resolver.state = st
	rec := e.post(t, "/session/"+info.ID+"/submit", "k-held", `{"message":"go","resume":true}`)
	wantStatus(t, "retry while a drain stops the session", rec, http.StatusConflict)
	if body := rec.Body.String(); !strings.Contains(body, "session_stopping") || strings.Contains(body, "new Idempotency-Key") {
		t.Fatalf("body = %s, want session_stopping with the key not spent", body)
	}
	if op, err := st.ops.Get(row.ID); err != nil || op.Status == opjournal.StatusClosed {
		t.Fatalf("op = %+v, %v; want it still open", op, err)
	}
}

// A same-key retry of an open lane op consumes only a hold stamped by the
// op's proposal: a suspend the operator placed while the message waited
// stands. Here the API proposed the op a minute ago and its answer was
// lost; the operator suspends the session now, and the client retries. The
// retry is accepted with the op still open behind the hold, and the session
// stays suspended with no wake recorded; its result names the suspend and
// offers no resume: true resend. Kills a retry whose resume consumes
// whatever suspend the session carries now, after which the controller
// starts the session the operator suspended, and a result that tells the
// client to resend with resume: true, which leaves that suspend standing.
func TestSubmitUnderLaneRetryLeavesALaterSuspendStanding(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	info := e.session(t, "Later hold")
	sessions := session.NewStore(e.fs.SessionsBeadStore())
	earlier, err := opjournal.NewStore(beads.SessionStore{Store: e.store.MemStore}, &clock.Fake{Time: time.Now().UTC().Add(-time.Minute)}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("opening the op store: %v", err)
	}
	op, created, err := earlier.Propose(opjournal.Request{
		Conversation: info.ID, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane, ClientKey: "k-later",
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  "go", Policy: LaneMessageOpPolicy(session.SubmitIntentDefault),
	})
	if err != nil || !created {
		t.Fatalf("proposing the earlier request's op: created=%t err=%v", created, err)
	}
	wantStatus(t, "suspend", e.post(t, "/session/"+info.ID+"/suspend", "", ""), http.StatusOK)
	rec := e.post(t, "/session/"+info.ID+"/submit", "k-later", `{"message":"go","resume":true}`)
	wantStatus(t, "same-key retry", rec, http.StatusAccepted)
	if acc := decodeAsyncAccepted(t, rec.Body); acc.RequestID != op.ID || acc.Replayed {
		t.Fatalf("retry = %+v, want op %s accepted, not replayed", acc, op.ID)
	}
	if got, err := sessions.Get(info.ID); err != nil || got.State != session.StateSuspended || got.WakeRequest != "" {
		t.Fatalf("session after the retry = %+v, %v; want the later suspend standing, no wake", got, err)
	}
	if cur, err := st.ops.Get(op.ID); err != nil || cur.Status == opjournal.StatusClosed {
		t.Fatalf("op = %+v, %v; want it open behind the hold", cur, err)
	}
	// Its result names the suspend and offers no resend with resume: true,
	// which this retry was and which leaves that suspend standing.
	evs := waitTerminalEvents(t, e.fs.eventProv, op.ID, 1)
	var result struct {
		WillStart          *bool  `json:"will_start"`
		WillNotStartReason string `json:"will_not_start_reason"`
	}
	if err := json.Unmarshal(evs[0].Payload, &result); err != nil {
		t.Fatalf("decoding %s: %v", evs[0].Payload, err)
	}
	if want := laneHeldByReason([]string{"a suspend"}); result.WillStart == nil || *result.WillStart || result.WillNotStartReason != want {
		t.Fatalf("retry's result %s; want will_start false, will_not_start_reason %q", evs[0].Payload, want)
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

// laneVerdictState is a lane host city whose controller answers the
// will-not-start verdict with refusal, or cannot tell (uncertain).
type laneVerdictState struct {
	*lanePokeState
	refusal   string
	uncertain bool
}

func (s *laneVerdictState) WakeStartRefusal(session.Info) (string, bool) {
	return s.refusal, !s.uncertain
}

// willStartText is will_start as a result shows it: absent, true or false.
func willStartText(v *bool) string {
	if v == nil {
		return "absent"
	}
	return strconv.FormatBool(*v)
}

// A lane message still waiting once the settle window passes says, as a
// queued legacy message does, whether the controller will start its
// session, read alone and as the lane host reads the row: the lane host's
// wake request records the wake and asks the controller to start it, and it
// reads a dormant row as not running. A dormant session nothing holds will
// start, or says why the controller will not, offering no resume: true
// (under the lane it starts nothing). A held one will not: it offers the
// same-key resume: true resend when that resend lifts every hold (a
// suspend placed before the message); it names the suspend and the holds
// standing beside it, and offers the resend for the suspend, when the
// resend lifts the suspend alone; and otherwise names the holds the resend
// leaves standing (an agent's wait, a quarantine), also to a request that
// carried resume: true. A suspend placed before the message on a session a
// drain is stopping names every hold and the drain, and offers the resend
// once the drain is done, which lifts the suspend then. A dormant row whose
// runtime has not stopped yet is held by its wait as the lane host reads
// it. A pool seat waiting on demand, and a running session the message
// waits on for its boundary, an agent's heartbeat keeping it up included,
// say neither. Nothing here records a wake or asks the controller to
// reconcile, on /submit and /messages alike. Kills a lane result with no
// will_start, one that records the wake or pokes the controller itself,
// one that offers resume: true to start a session the controller will not
// start or to lift a hold it does not lift, one that names only the holds
// a resend leaves, hiding a suspend the resend lifts (the message would
// wait on it after the other holds end), one that reads the drain's
// refusal of a resume as a suspend the resend never lifts (the client
// would never resend, and the message would wait on an indefinite
// suspend), one that reads a dormant row's lingering runtime (it would
// promise a start the lane host's held wake request never asks for), and
// one that reads every row as not running (a busy session under a
// heartbeat would read held).
func TestLaneMessageResultSaysWhetherItWillStart(t *testing.T) {
	yes, no := true, false
	const refusal = "it is a dependency-only session; it starts with its dependents"
	const later = "2099-01-01T00:00:00Z"
	asleep := map[string]string{"state": "asleep", "suspended_at": "", "held_until": "", "sleep_intent": ""}
	over := func(base map[string]string, extra map[string]string) map[string]string {
		out := maps.Clone(base)
		maps.Copy(out, extra)
		return out
	}
	wait := map[string]string{"wait_hold": "true", "sleep_intent": "wait-hold"}
	heartbeat := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	for name, tc := range map[string]struct {
		// suspend suspends the session through the API, which stops its
		// runtime; meta is then written over the row.
		suspend   bool
		meta      map[string]string
		resume    bool
		refusal   string
		uncertain bool
		want      *bool
		reason    string
	}{
		"unheld asleep":                    {suspend: true, meta: asleep, want: &yes},
		"unheld, will not start":           {suspend: true, meta: asleep, refusal: refusal, want: &no, reason: refusal},
		"unheld pool seat, demand unseen":  {suspend: true, meta: asleep, uncertain: true},
		"suspended":                        {suspend: true, want: &no, reason: laneSuspendedReason},
		"suspended over a wait":            {suspend: true, meta: map[string]string{"wait_hold": "true"}, want: &no, reason: laneSuspendedHeldByReason([]string{"an agent's wait"})},
		"suspended over a quarantine":      {suspend: true, meta: map[string]string{"quarantined_until": later}, want: &no, reason: laneSuspendedHeldByReason([]string{"a quarantine until " + later})},
		"suspended over a wait, resumed":   {suspend: true, meta: map[string]string{"wait_hold": "true"}, resume: true, want: &no, reason: laneHeldByReason([]string{"an agent's wait"})},
		"suspended, a drain stopping it":   {suspend: true, meta: map[string]string{"state": string(session.StateDraining)}, want: &no, reason: laneDrainingReason([]string{"a suspend"})},
		"suspended over a wait, draining":  {suspend: true, meta: map[string]string{"state": string(session.StateDraining), "wait_hold": "true"}, want: &no, reason: laneDrainingReason([]string{"a suspend", "an agent's wait"})},
		"quarantined":                      {suspend: true, meta: over(asleep, map[string]string{"quarantined_until": later}), want: &no, reason: laneHeldByReason([]string{"a quarantine until " + later})},
		"waiting, its runtime not stopped": {meta: over(asleep, wait), want: &no, reason: laneHeldByReason([]string{"an agent's wait"})},
		"running":                          {},
		"running under a heartbeat":        {meta: map[string]string{"held_until": heartbeat}},
	} {
		for _, path := range []string{"/submit", "/messages"} {
			t.Run(name+" "+path, func(t *testing.T) {
				e, st := laneMessageEnv(t, nil)
				shortSettle(t)
				e.resolver.state = &laneVerdictState{lanePokeState: st, refusal: tc.refusal, uncertain: tc.uncertain}
				info := e.session(t, "Lane will start")
				if tc.suspend {
					wantStatus(t, "suspend", e.post(t, "/session/"+info.ID+"/suspend", "", ""), http.StatusOK)
				}
				if tc.meta != nil {
					if err := e.store.SetMetadataBatch(info.ID, tc.meta); err != nil {
						t.Fatal(err)
					}
				}
				if running := e.fs.sp.IsRunning(info.SessionName); running == tc.suspend {
					t.Fatalf("runtime running = %t before the message; the case wants it %t", running, !tc.suspend)
				}
				body := `{"message":"hello"}`
				if tc.resume {
					body = `{"message":"hello","resume":true}`
				}
				enqueued := len(e.fs.enqueuedKeys())
				acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+path, "", body).Body)
				evs := waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
				var got struct {
					Queued             bool   `json:"queued"`
					WillStart          *bool  `json:"will_start"`
					WillNotStartReason string `json:"will_not_start_reason"`
				}
				if err := json.Unmarshal(evs[0].Payload, &got); err != nil {
					t.Fatalf("decoding %s: %v", evs[0].Payload, err)
				}
				if !got.Queued || !reflect.DeepEqual(got.WillStart, tc.want) || got.WillNotStartReason != tc.reason {
					t.Fatalf("result %s; want queued, will_start %s, will_not_start_reason %q", evs[0].Payload, willStartText(tc.want), tc.reason)
				}
				if row, err := session.NewStore(e.fs.SessionsBeadStore()).Get(info.ID); err != nil || row.WakeRequest != "" {
					t.Fatalf("session after the result = wake_request %q, %v; want no wake recorded by the API", row.WakeRequest, err)
				}
				if poked := e.fs.enqueuedKeys()[enqueued:]; slices.Contains(poked, reconcilekey.Session(info.ID)) {
					t.Fatalf("the result asked the controller to reconcile %s: %v", info.ID, poked)
				}
			})
		}
	}
}

func TestSubmitUnderLaneSameKeyRetryReplays(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	info := e.session(t, "Retry")
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k-r", `{"message":"hello"}`).Body)
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	// An open lane op: accepted again, nothing proposed again, and the
	// retry's request_id resolves from its own cursor, its result queued
	// while the lane still holds the op.
	e.restart(t)
	e.resolver.state = st
	open := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k-r", `{"message":"hello"}`).Body)
	if open.RequestID != first.RequestID || open.Replayed || open.IdempotencyKey != "k-r" {
		t.Fatalf("retry of an open op = %+v, want %s accepted, not replayed", open, first.RequestID)
	}
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 2)
	out := decodeRequestOutcome(t, getRequestOutcome(t, e.h, e.resolver.state, open.RequestID, open.EventCursor))
	if out.State != RequestStateSucceeded || out.Event == nil || out.Event.Type != events.RequestResultSessionSubmit {
		t.Fatalf("retry's outcome = %+v, want its result after its cursor", out)
	}
	var result struct {
		Queued bool `json:"queued"`
	}
	if err := json.Unmarshal(out.Event.Payload, &result); err != nil || !result.Queued {
		t.Fatalf("retry's result %s, %v; want queued true", out.Event.Payload, err)
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
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 3)
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

// The result of a lane message that waits on a suspend placed before it
// offers a resend with resume: true under the idempotency_key of its 202,
// the client's or one the server minted, and that resend, served by the
// same API process, is answered from the op under the key: accepted again,
// not replayed, its resume consuming the suspend and recording the
// session's wake, with no second op, and its result saying the session
// will start. Kills an Idempotency-Key LRU that answers the resend from
// the first request's 202: 422 idempotency_mismatch for a body it hashed
// with resume false, or a replay that never runs the resume and leaves the
// message waiting on the suspend.
func TestLaneMessageResumeResendOnTheSameProcessConsumesTheHold(t *testing.T) {
	for _, path := range []string{"/submit", "/messages"} {
		for name, key := range map[string]string{"client key": "k-resend", "minted key": ""} {
			t.Run(name+" "+path, func(t *testing.T) {
				e, st := laneMessageEnv(t, nil)
				shortSettle(t)
				info := e.session(t, "Resend")
				sessions := session.NewStore(e.fs.SessionsBeadStore())
				wantStatus(t, "suspend", e.post(t, "/session/"+info.ID+"/suspend", "", ""), http.StatusOK)
				first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+path, key, `{"message":"go"}`).Body)
				if _, reason := willStartOf(t, waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)[0]); reason != laneSuspendedReason {
					t.Fatalf("first result's will_not_start_reason = %q, want %q", reason, laneSuspendedReason)
				}
				rec := e.post(t, "/session/"+info.ID+path, first.IdempotencyKey, `{"message":"go","resume":true}`)
				wantStatus(t, "resend with resume: true", rec, http.StatusAccepted)
				acc := decodeAsyncAccepted(t, rec.Body)
				if acc.RequestID != first.RequestID || acc.Replayed {
					t.Fatalf("resend = %+v, want op %s accepted again, not replayed", acc, first.RequestID)
				}
				if got, err := sessions.Get(info.ID); err != nil || got.State != session.StateAsleep || got.WakeRequest != string(session.WakeCauseExplicit) {
					t.Fatalf("session after the resend = %+v, %v; want the hold consumed and its wake recorded", got, err)
				}
				if op, err := st.ops.Get(first.RequestID); err != nil || op.Status == opjournal.StatusClosed {
					t.Fatalf("op = %+v, %v; want it open for the lane", op, err)
				}
				if rows := e.opRows(t); len(rows) != 1 {
					t.Fatalf("op rows = %d, want the one op", len(rows))
				}
				if willStart, reason := willStartOf(t, waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 2)[1]); willStart == nil || !*willStart || reason != "" {
					t.Fatalf("resend's result: will_start %s, will_not_start_reason %q; want will_start true", willStartText(willStart), reason)
				}
			})
		}
	}
}

// Under lane authority the Idempotency-Key LRU keeps no answer of /submit
// or /messages, and holds the key only while its request runs: a same-key
// request then is 409 in_flight, as the LRU answers one on every path,
// rather than a second acceptance of an op the running request may still
// cancel. Once the first request answered, a same-key retry is answered
// from the op under the key: accepted again, its result after its own
// cursor. Kills an LRU that keeps a lane answer (the retry would replay
// the first 202 and emit nothing) and one skipped outright under lane
// authority (the concurrent request would be accepted).
func TestSubmitUnderLaneKeyIsBusyOnlyWhileItsRequestRuns(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	info := e.session(t, "Busy key")
	parked, release := make(chan struct{}), make(chan struct{})
	var unpark sync.Once
	t.Cleanup(func() { unpark.Do(func() { close(release) }) })
	e.store.set(func(s *opsTestStore) {
		s.onOpCreate = func() {
			close(parked)
			<-release
		}
	})
	path := "/session/" + info.ID + "/submit"
	answer := e.postAsync(t, path, "k-busy", `{"message":"go"}`)
	select {
	case <-parked:
	case <-time.After(testEventTimeout):
		t.Fatal("the first request never wrote its op")
	}
	var busy *httptest.ResponseRecorder
	select {
	case busy = <-e.postAsync(t, path, "k-busy", `{"message":"go"}`):
	case <-time.After(testEventTimeout):
		t.Fatal("a same-key request while the first runs did not answer")
	}
	unpark.Do(func() { close(release) })
	if busy.Code != http.StatusConflict || !strings.Contains(busy.Body.String(), "in_flight") {
		t.Fatalf("same-key request while the first runs = %d: %s, want 409 in_flight", busy.Code, busy.Body.String())
	}
	rec := <-answer
	wantStatus(t, "first request", rec, http.StatusAccepted)
	first := decodeAsyncAccepted(t, rec.Body)
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	retry := decodeAsyncAccepted(t, e.post(t, path, "k-busy", `{"message":"go"}`).Body)
	if retry.RequestID != first.RequestID || retry.Replayed {
		t.Fatalf("retry = %+v, want op %s accepted again, not replayed", retry, first.RequestID)
	}
	if evs := waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 2); evs[1].Seq <= mustSeq(t, retry.EventCursor) {
		t.Fatalf("results %+v, want the retry's after its cursor %s", evs, retry.EventCursor)
	}
	if op, err := st.ops.Get(first.RequestID); err != nil || op.Status == opjournal.StatusClosed {
		t.Fatalf("op = %+v, %v; want it open for the lane", op, err)
	}
	if rows := e.opRows(t); len(rows) != 1 {
		t.Fatalf("op rows = %d, want the one op", len(rows))
	}
}

// An answer the Idempotency-Key LRU kept before the city's lane host
// executed message still replays a same-key retry once it does, in the
// same API process: the legacy path delivered that request and recorded no
// op (every ops gate is off), so the op journal cannot answer the retry,
// and a lane op proposed for it would deliver the message twice. Kills an
// LRU skipped outright under lane authority.
func TestSubmitUnderLaneReplaysAnAnswerKeptBeforeTheLaneRan(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	kinds := st.kinds
	st.kinds = nil // the lane host has not started yet
	info := e.session(t, "Before the lane")
	path := "/session/" + info.ID + "/submit"
	first := decodeAsyncAccepted(t, e.post(t, path, "k-before", `{"message":"hello"}`).Body)
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	st.kinds = kinds // the lane host starts
	rec := e.post(t, path, "k-before", `{"message":"hello"}`)
	wantStatus(t, "same-key retry once the lane runs", rec, http.StatusAccepted)
	if retry := decodeAsyncAccepted(t, rec.Body); retry.RequestID != first.RequestID {
		t.Fatalf("retry = %+v, want the first answer %s replayed", retry, first.RequestID)
	}
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("op rows = %d, want no lane op for the retry", len(rows))
	}
	if got := deliveredCalls(e.fs.sp, info.SessionName); len(got) != 1 {
		t.Fatalf("delivered %d, want the legacy delivery's one", len(got))
	}
}

// Under lane authority the op under a request's key answers it before the
// pending-interaction check, which only a request no op answers passes
// (request_id rule 8): while the session waits on an approval, a same-key
// retry, on the same API process, endpoint and spelling, on another
// spelling and on the other endpoint, is answered from the op, a delivered
// message's replay or a waiting one's acceptance, with no second op, and a
// new key is still 409 pending_interaction, writing nothing. Kills a front
// door that checks the pending interaction first: the Idempotency-Key LRU
// keeps no lane answer, so the retry would be told to answer the approval
// before sending text its 202 already accepted.
func TestSubmitUnderLaneRetryIsAnsweredPastAPendingInteraction(t *testing.T) {
	for _, tc := range []struct {
		name      string
		closeWith func(opjournal.Op) *opjournal.Outcome
		replayed  bool
	}{
		{"delivered", outcome(opjournal.ArmSucceeded, ""), true},
		{"waiting", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := laneMessageEnv(t, tc.closeWith)
			shortSettle(t)
			info := e.session(t, "Front door")
			body := `{"message":"hi"}`
			first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k-front", body).Body)
			waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
			e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
				RequestID: "approval-1", Kind: "approval", Prompt: "Bash: date", Options: []string{"Yes", "No"},
			})
			for i, retry := range []string{info.ID + "/submit", info.SessionName + "/submit", info.ID + "/messages"} {
				rec := e.post(t, "/session/"+retry, "k-front", body)
				wantStatus(t, "retry "+retry, rec, http.StatusAccepted)
				if acc := decodeAsyncAccepted(t, rec.Body); acc.RequestID != first.RequestID || acc.Replayed != tc.replayed {
					t.Fatalf("retry %s = %+v, want op %s with replayed %t", retry, acc, first.RequestID, tc.replayed)
				}
				waitTerminalEvents(t, e.fs.eventProv, first.RequestID, i+2)
			}
			rec := e.post(t, "/session/"+info.ID+"/submit", "k-other", body)
			var problem struct {
				Detail string `json:"detail"`
			}
			if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &problem) != nil || !strings.HasPrefix(problem.Detail, "pending_interaction: ") {
				t.Fatalf("a new key: status %d, want 409 pending_interaction: %s", rec.Code, rec.Body.String())
			}
			if rows := e.opRows(t); len(rows) != 1 {
				t.Fatalf("op rows = %d, want the first request's op alone", len(rows))
			}
		})
	}
}

// A same-key retry of an open lane op is 409 in_flight while one of the
// op's terminal events is kept, so it records no acceptance whose event
// would follow the kept one; once the log takes the kept event, the retry is
// accepted again.
func TestSubmitUnderLaneRetryIsBusyWhileAnEventIsKept(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	shortSettle(t)
	cityLog := events.NewFake()
	live := &closableEvents{Fake: cityLog}
	e.fs.eventProv = live
	// The log closes once the first request is accepted, before its result.
	var closeOnAwait atomic.Bool
	closeOnAwait.Store(true)
	st.onAwait = func(string) {
		if closeOnAwait.Load() {
			live.closed.Store(true)
		}
	}
	info := e.session(t, "Kept")
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k-kept", `{"message":"hello"}`).Body)
	waitFor(t, testEventTimeout, "the event log refuses the first request's result", func() bool {
		return len(terminalEventsOf(live.refusedEvents(), first.RequestID)) == 1
	})
	closeOnAwait.Store(false)
	// Another spelling of the target goes past the per-path LRU to the op.
	retry := "/session/" + info.SessionName + "/submit"
	if rec := e.post(t, retry, "k-kept", `{"message":"hello"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in_flight") {
		t.Fatalf("retry while the result is kept = %d: %s, want 409 in_flight", rec.Code, rec.Body.String())
	}
	live.closed.Store(false)
	rec := e.post(t, retry, "k-kept", `{"message":"hello"}`)
	acc := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || acc.RequestID != first.RequestID || acc.Replayed {
		t.Fatalf("retry once the log takes the kept result = %d %+v, want %s accepted again", rec.Code, acc, first.RequestID)
	}
	if evs := waitTerminalEvents(t, cityLog, first.RequestID, 2); evs[1].Seq <= mustSeq(t, acc.EventCursor) {
		t.Fatalf("results %+v, want the kept one recorded and the retry's after its cursor %s", evs, acc.EventCursor)
	}
}

// A created op whose acceptance the event log refuses is canceled before the
// lane is rung, so nothing delivers it, and the request answers 503; a
// same-key retry replays that outcome.
func TestSubmitUnderLaneRefusedAcceptanceCancelsTheOp(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	cityLog := events.NewFake()
	live := &closableEvents{Fake: cityLog}
	e.fs.eventProv = live
	info := e.session(t, "Refused acceptance")
	live.closed.Store(true)
	wantStatus(t, "refused acceptance", e.post(t, "/session/"+info.ID+"/submit", "k-acc", `{"message":"hello"}`), http.StatusServiceUnavailable)
	rows := e.opRows(t)
	if len(rows) != 1 {
		t.Fatalf("op rows = %d, want the one proposed", len(rows))
	}
	op, err := st.ops.Get(rows[0].ID)
	if arm, _ := op.Outcome.Effective(); err != nil || op.Status != opjournal.StatusClosed || arm != opjournal.ArmCanceled {
		t.Fatalf("op = %+v, %v; want it closed canceled", op, err)
	}
	if poked := st.poked(); len(poked) != 0 {
		t.Fatalf("lane rung for %v, want never", poked)
	}
	live.closed.Store(false)
	rec := e.post(t, "/session/"+info.SessionName+"/submit", "k-acc", `{"message":"hello"}`)
	acc := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || acc.RequestID != op.ID || !acc.Replayed {
		t.Fatalf("retry = %d %+v, want the canceled op %s replayed", rec.Code, acc, op.ID)
	}
	if evs := waitTerminalEvents(t, cityLog, op.ID, 1); evs[0].Type != events.RequestFailed {
		t.Fatalf("replayed result %s %s, want request.failed", evs[0].Type, evs[0].Payload)
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

// A key that names a message op the legacy path recorded before message was
// listed answers only the request it recorded: another message under it is
// 422 idempotency_mismatch, as before the flip, and the same one replays it.
func TestSubmitUnderLanePreFlipKeyAnswersOnlyItsRequest(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	info := e.session(t, "Flip")
	prior, _, err := st.ops.Propose(opjournal.Request{
		Conversation: info.ID, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLegacyAPI, ClientKey: "k-flip",
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  "hello", Policy: MessageOpPolicy(session.SubmitIntentDefault),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ops.Close(prior.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: legacyop.ViaLegacyDelivered}}, "api"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"message":"other"}`, `{"message":"hello","intent":"follow_up"}`} {
		e.restart(t)
		e.resolver.state = st
		if rec := e.post(t, "/session/"+info.ID+"/submit", "k-flip", body); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s under the pre-flip key: %d %s, want 422", body, rec.Code, rec.Body.String())
		}
	}
	e.restart(t)
	e.resolver.state = st
	same := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k-flip", `{"message":"hello"}`).Body)
	if same.RequestID != prior.ID || !same.Replayed {
		t.Fatalf("the same message under the pre-flip key = %+v, want op %s replayed", same, prior.ID)
	}
	if rows := e.opRows(t); len(rows) != 1 {
		t.Fatalf("op rows = %d, want the pre-flip op alone", len(rows))
	}
}

// A lane submit's result and its replay report the intent the lane runs it
// as: interrupt_now its own, and follow_up default, since the lane delivers
// a follow_up at the session's boundary as it does a default. The two are
// one request under a key: a retry with the other of them replays the op,
// where interrupt_now under a default's key is 422, whether the API
// process that answered the first request serves the retry or a new one
// does. Kills a live result that reports the caller's follow_up while its
// replay says default, and an Idempotency-Key LRU that answers the retry
// on the first request's process: 422 for the other of the two, whose body
// it hashed apart, or the first 202 again, which replays nothing.
func TestSubmitUnderLaneReportsTheIntentItRuns(t *testing.T) {
	for _, process := range []struct {
		name    string
		restart bool
	}{{"same process", false}, {"after a restart", true}} {
		// retryProcess is the API process a retry reaches: the one that
		// answered the first request, or a new one.
		retryProcess := func(t *testing.T, e *opsEnv, st *lanePokeState) {
			t.Helper()
			if process.restart {
				e.restart(t)
				e.resolver.state = st
			}
		}
		for _, tc := range []struct {
			name, first, retry string
			want               session.SubmitIntent
		}{
			{"interrupt_now", `{"message":"stop","intent":"interrupt_now"}`, `{"message":"stop","intent":"interrupt_now"}`, session.SubmitIntentInterruptNow},
			{"follow_up", `{"message":"later","intent":"follow_up"}`, `{"message":"later","intent":"follow_up"}`, session.SubmitIntentDefault},
			{"follow_up retried as default", `{"message":"later","intent":"follow_up"}`, `{"message":"later"}`, session.SubmitIntentDefault},
			{"default retried as follow_up", `{"message":"later"}`, `{"message":"later","intent":"follow_up"}`, session.SubmitIntentDefault},
		} {
			t.Run(process.name+"/"+tc.name, func(t *testing.T) {
				e, st := laneMessageEnv(t, outcome(opjournal.ArmSucceeded, ""))
				info := e.session(t, "Intent")
				first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k-intent", tc.first).Body)
				waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
				retryProcess(t, e, st)
				if replay := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k-intent", tc.retry).Body); !replay.Replayed || replay.RequestID != first.RequestID {
					t.Fatalf("retry = %+v, want op %s replayed", replay, first.RequestID)
				}
				for _, ev := range waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 2) {
					var p struct {
						Intent string `json:"intent"`
					}
					if err := json.Unmarshal(ev.Payload, &p); err != nil || p.Intent != string(tc.want) {
						t.Fatalf("terminal event %s %s, want intent %s", ev.Type, ev.Payload, tc.want)
					}
				}
			})
		}
		t.Run(process.name+"/interrupt_now under a default's key", func(t *testing.T) {
			e, st := laneMessageEnv(t, outcome(opjournal.ArmSucceeded, ""))
			info := e.session(t, "Intent")
			first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k-now", `{"message":"stop"}`).Body)
			waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
			retryProcess(t, e, st)
			if rec := e.post(t, "/session/"+info.ID+"/submit", "k-now", `{"message":"stop","intent":"interrupt_now"}`); rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("interrupt_now under a default's key: %d %s, want 422", rec.Code, rec.Body.String())
			}
		})
	}
}

// Under lane authority a message is at most opjournal.MaxInlinePayload
// bytes: /submit and /messages refuse a longer one 422 payload_too_large,
// and POST /sessions refuses one before it creates anything.
func TestLaneMessageOverThePayloadLimitIsRefused(t *testing.T) {
	e, _ := laneMessageEnv(t, nil)
	info := e.session(t, "Big")
	big := strings.Repeat("x", opjournal.MaxInlinePayload+1)
	for _, path := range []string{"/submit", "/messages"} {
		rec := e.post(t, "/session/"+info.ID+path, "", `{"message":"`+big+`"}`)
		wantStatus(t, path, rec, http.StatusUnprocessableEntity)
		if code := problemCode(t, rec); code != "payload-too-large" {
			t.Fatalf("%s problem code = %q, want payload-too-large", path, code)
		}
	}
	rows, starts := e.sessionRowsAndStarts(t)
	rec := e.post(t, "/sessions", "", `{"kind":"provider","name":"test-agent","message":"`+big+`"}`)
	wantStatus(t, "create", rec, http.StatusUnprocessableEntity)
	if code := problemCode(t, rec); code != "payload-too-large" {
		t.Fatalf("create problem code = %q, want payload-too-large", code)
	}
	if afterRows, afterStarts := e.sessionRowsAndStarts(t); afterRows != rows || afterStarts != starts {
		t.Fatalf("session rows %d -> %d, starts %d -> %d; want nothing created", rows, afterRows, starts, afterStarts)
	}
	if ops := e.opRows(t); len(ops) != 0 {
		t.Fatalf("op rows = %d, want none", len(ops))
	}
}

// The compatibility create (the no-city POST /v0/sessions that
// Server.ServeHTTP serves through the legacy handler) refuses an initial
// message over the lane's payload limit 422 payload_too_large before
// anything is created, as the city route does, rather than creating the
// session and rolling it back once the message's proposal fails.
func TestLegacyCreateUnderLaneRefusesAnOversizeMessage(t *testing.T) {
	e, _ := laneMessageEnv(t, nil)
	rows, starts := e.sessionRowsAndStarts(t)
	big := strings.Repeat("x", opjournal.MaxInlinePayload+1)
	rec := httptest.NewRecorder()
	e.server(t).legacySessionHandler().ServeHTTP(rec, newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"provider","name":"test-agent","message":"`+big+`"}`)))
	wantStatus(t, "compatibility create", rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "payload_too_large") {
		t.Fatalf("body = %s, want payload_too_large", rec.Body.String())
	}
	if afterRows, afterStarts := e.sessionRowsAndStarts(t); afterRows != rows || afterStarts != starts {
		t.Fatalf("session rows %d -> %d, starts %d -> %d; want nothing created", rows, afterRows, starts, afterStarts)
	}
	if ops := e.opRows(t); len(ops) != 0 {
		t.Fatalf("op rows = %d, want none", len(ops))
	}
}

// An extmsg member notice too long for a lane op's payload is cut, at a rune
// boundary and saying so, to fit it, and is proposed, so the member is
// notified; its own system-reminder wrapper is stripped when the lane
// delivers it, not nested. A notice that fits, or one no lane carries, is
// the reminder as formatted.
func TestExtmsgNoticeUnderLaneFitsTheLanePayload(t *testing.T) {
	e, st := laneMessageEnv(t, nil)
	info := e.session(t, "Member")
	s := e.server(t)
	r := extmsgNotifyReminder{
		Provider: "slack", ConversationID: "C123", ActorDisplay: "alice", ActorKind: "human",
		Text: strings.Repeat("é", 20<<10), RecipientSelector: info.ID, RecipientSessionID: info.ID, Handle: "worker",
	}
	notice := s.extmsgNotice(r)
	if len(notice) > opjournal.MaxInlinePayload || !utf8.ValidString(notice) || !strings.Contains(notice, extmsgNoticeCutNote) ||
		!strings.HasPrefix(notice, "<system-reminder>\nNew message in shared conversation slack/C123") || !strings.HasSuffix(notice, "</system-reminder>") {
		t.Fatalf("notice of %d bytes (valid UTF-8 %v) = %q...%q; want the reminder cut to fit with its note", len(notice), utf8.ValidString(notice), notice[:80], notice[len(notice)-200:])
	}
	if err := s.sendBackgroundMessageToSession(context.Background(), e.fs.SessionsBeadStore().Store, info.ID, notice); err != nil {
		t.Fatalf("notice: %v", err)
	}
	open, err := st.ops.ListOpen(info.ID)
	if err != nil || len(open) != 1 || open[0].Payload != notice {
		t.Fatalf("open ops = %d, %v; want the notice proposed", len(open), err)
	}
	text, err := opjournal.RenderDelivery(open, opjournal.TextReminder, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(text, "<system-reminder>") != 1 || !strings.Contains(text, "[extmsg]") || !strings.Contains(text, extmsgNoticeCutNote) {
		t.Fatalf("delivered notice = %q...; want one reminder carrying the cut note", text[:200])
	}
	short := r
	short.Text = "new message in #ops"
	if got := s.extmsgNotice(short); got != formatExtmsgNotifyReminder(short) {
		t.Fatalf("a notice that fits = %q, want the reminder as formatted", got)
	}
	unlisted, _ := newLaneOpsEnv(t, "interrupt")
	if got := unlisted.server(t).extmsgNotice(r); got != formatExtmsgNotifyReminder(r) {
		t.Fatalf("a long notice with message unlisted is %d bytes, want the reminder as formatted", len(got))
	}
}
