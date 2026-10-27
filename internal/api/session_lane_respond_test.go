package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// /respond under lane authority (design slice 5): with respond listed, the
// API proposes a respond op keyed by the interaction occurrence it answers,
// which the lane host runs at most once.

const laneRespondRequest = "req-1"

// laneRespondEnv is a city whose lane host executes respond, its passes
// scripted by closeWith (laneStopEnv), with a session showing a pending
// approval laneRespondRequest.
func laneRespondEnv(t *testing.T, closeWith func(opjournal.Op) *opjournal.Outcome) (*opsEnv, *lanePokeState, session.Info) {
	t.Helper()
	e, st := newLaneOpsEnv(t, "message", "respond")
	info := e.session(t, "Lane respond")
	e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: laneRespondRequest, Kind: "approval", Options: []string{"approve", "deny"}})
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
			if op.Executor != opjournal.ExecutorLane || op.Kind != opjournal.KindRespond {
				continue
			}
			if o := closeWith(op); o != nil {
				if _, err := st.ops.Close(op.ID, *o, "lane:ctl"); err != nil {
					t.Errorf("closing op %s: %v", op.ID, err)
				}
			}
		}
	}
	return e, st, info
}

// resolveRaising records occurrence occ of conv's generation gen resolved,
// as the lane does once its answer lands.
func resolveRaising(t *testing.T, st *opjournal.Store, conv string, gen int64, occ string) {
	t.Helper()
	if _, err := st.RecordFact(opjournal.Fact{Kind: opjournal.FactInteractionResolved, Conversation: conv, Generation: gen, Interaction: occ, Basis: opjournal.FactBasisObserved, At: time.Now()}); err != nil {
		t.Fatalf("recording the resolution: %v", err)
	}
}

func respondBody(action string) string { return `{"action":"` + action + `"}` }

// shortRespondBound shortens laneRespondAnswerBound for one test, so a
// respond the scripted lane leaves open answers at once.
func shortRespondBound(t *testing.T) {
	t.Helper()
	old := laneRespondAnswerBound
	laneRespondAnswerBound = 20 * time.Millisecond
	t.Cleanup(func() { laneRespondAnswerBound = old })
}

func TestRespondUnderLaneProposesForTheOccurrence(t *testing.T) {
	e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
	rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	wantStatus(t, "respond", rec, http.StatusAccepted)
	a := decodeCommandAnswer(t, rec)
	if a.Op == nil || a.IdempotencyKey != "r-1" {
		t.Fatalf("answer = %+v, want the op under the request's own key", a)
	}
	op, err := st.ops.Get(a.Op.ID)
	if err != nil {
		t.Fatal(err)
	}
	gen := generationOf(t, e.store, info.ID)
	raising, found, err := st.ops.LatestRaising(info.ID, gen, laneRespondRequest)
	if err != nil || !found {
		t.Fatalf("raising = %+v, %v, %v", raising, found, err)
	}
	wantKey, _ := opjournal.RespondKey(raising.Raised)
	payload, err := opjournal.DecodeRespondPayload(op.Payload)
	if op.Executor != opjournal.ExecutorLane || op.Kind != opjournal.KindRespond || op.ClientKey != wantKey ||
		op.Scope.Interaction != laneRespondRequest+"#1" || op.Scope.Generation != gen ||
		op.Policy.Guarantee != opjournal.GuaranteeAtMostOnce || err != nil || payload.Action != "approve" {
		t.Fatalf("op = %+v (payload %+v, %v)", op, payload, err)
	}
	rowID, _ := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", "r-1")
	row, err := st.ops.Get(rowID)
	if err != nil || row.Status != opjournal.StatusClosed || row.Outcome.Result.CoalescedInto != op.ID ||
		row.Executor == opjournal.ExecutorLane || row.Scope.Interaction != laneRespondRequest || row.Policy.Guarantee != opjournal.GuaranteeAtLeastOnce {
		t.Fatalf("row under the key = %+v, %v; want the legacy shape coalesced into the lane op", row, err)
	}
	if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 0 {
		t.Fatalf("the API answered the runtime itself %d times", n)
	}
}

func TestRespondUnderLaneFrontDoorWritesNothing(t *testing.T) {
	e, _, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
	mismatch := e.post(t, "/session/"+info.ID+"/respond", "", `{"action":"approve","request_id":"other"}`)
	if mismatch.Code != http.StatusConflict || !strings.Contains(mismatch.Body.String(), "invalid_interaction") {
		t.Fatalf("mismatch: %d %s", mismatch.Code, mismatch.Body.String())
	}
	e.fs.sp.SetPendingInteraction(info.SessionName, nil)
	none := e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve"))
	if none.Code != http.StatusConflict || !strings.Contains(none.Body.String(), "no_pending") {
		t.Fatalf("no pending: %d %s", none.Code, none.Body.String())
	}
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("front-door refusals wrote %d rows", len(rows))
	}
}

// A retry under the first request's key follows its row back to the first
// op: it never answers an identical prompt raised after the first was
// resolved, which carries the same request id.
func TestRespondUnderLaneRetryFollowsItsRow(t *testing.T) {
	e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
	first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve")))
	resolveRaising(t, st.ops, info.ID, generationOf(t, e.store, info.ID), laneRespondRequest+"#1")
	e.restart(t)
	e.resolver.state = st
	retry := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	wantStatus(t, "retry", retry, http.StatusAccepted)
	if a := decodeCommandAnswer(t, retry); a.Op == nil || first.Op == nil || a.Op.ID != first.Op.ID || !a.Replayed {
		t.Fatalf("retry = %+v, want the first op %+v replayed", a, first.Op)
	}
	if open, _ := st.ops.ListOpen(info.ID); len(open) != 0 {
		t.Fatalf("the retry proposed %+v", open)
	}
}

// Two raisings of one request id are two occurrences: an answer to each is
// its own op.
func TestRespondUnderLaneIdenticalPromptsGetTwoOps(t *testing.T) {
	e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
	first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve")))
	resolveRaising(t, st.ops, info.ID, generationOf(t, e.store, info.ID), laneRespondRequest+"#1")
	second := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve")))
	if first.Op == nil || second.Op == nil || first.Op.ID == second.Op.ID {
		t.Fatalf("first %+v second %+v, want two ops", first.Op, second.Op)
	}
	op, err := st.ops.Get(second.Op.ID)
	if err != nil || op.Scope.Interaction != laneRespondRequest+"#2" {
		t.Fatalf("second op = %+v, %v; want the second occurrence", op, err)
	}
}

// A different answer to the same occurrence from the same principal is key
// reuse: the first answer wins.
func TestRespondUnderLaneDifferentAnswerIsKeyReuse(t *testing.T) {
	e, _, info := laneRespondEnv(t, nil)
	shortRespondBound(t)
	wantStatus(t, "first", e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve")), http.StatusAccepted)
	second := e.post(t, "/session/"+info.ID+"/respond", "", respondBody("deny"))
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "key_reuse") {
		t.Fatalf("different answer: %d %s, want 409 key_reuse", second.Code, second.Body.String())
	}
}

func TestRespondUnderLaneAnswersFromTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		arm    opjournal.Arm
		reason string
		status int
		code   string
	}{
		{"succeeded", opjournal.ArmSucceeded, "", http.StatusAccepted, ""},
		{"stale", opjournal.ArmSkipped, opjournal.ReasonStale, http.StatusConflict, "no_pending"},
		{"unsupported", opjournal.ArmFailed, opjournal.ReasonUnsupported, http.StatusNotImplemented, ""},
		{"refused action", opjournal.ArmFailed, opjournal.ReasonRefusedAction, http.StatusConflict, "invalid_interaction"},
		{"ambiguous", opjournal.ArmFailed, opjournal.ReasonAmbiguousDelivery, http.StatusConflict, opAlreadyFailedCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, info := laneRespondEnv(t, outcome(tc.arm, tc.reason))
			rec := e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve"))
			wantStatus(t, tc.name, rec, tc.status)
			if tc.code != "" && !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("body %s, want %s", rec.Body.String(), tc.code)
			}
		})
	}
	// Still open at the bound: 202 with the open op.
	shortRespondBound(t)
	e, _, info := laneRespondEnv(t, nil)
	rec := e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve"))
	wantStatus(t, "open", rec, http.StatusAccepted)
	if a := decodeCommandAnswer(t, rec); a.Op == nil || a.Op.Status == string(opjournal.StatusClosed) {
		t.Fatalf("open answer = %+v, want the open op", a)
	}
}

// A retry in the same process, under the key of a respond the bound cut
// short, reads the op again: the Idempotency-Key LRU kept no provisional
// 202, so the retry answers from the op's outcome.
func TestRespondUnderLaneSameProcessRetryReadsTheOpAgain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		closed opjournal.Outcome
		status int
		code   string
	}{
		{"refused", opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonRefusedAction}, http.StatusConflict, "invalid_interaction"},
		{"succeeded", opjournal.Outcome{Arm: opjournal.ArmSucceeded}, http.StatusAccepted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortRespondBound(t)
			e, st, info := laneRespondEnv(t, nil)
			first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve")))
			if first.Op == nil || first.Op.Status == string(opjournal.StatusClosed) {
				t.Fatalf("first = %+v, want the op still open", first)
			}
			if _, err := st.ops.Close(first.Op.ID, tc.closed, "lane:ctl"); err != nil {
				t.Fatal(err)
			}
			retry := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
			wantStatus(t, "retry", retry, tc.status)
			if tc.code != "" {
				if !strings.Contains(retry.Body.String(), tc.code) {
					t.Fatalf("retry body %s, want %s", retry.Body.String(), tc.code)
				}
				return
			}
			if a := decodeCommandAnswer(t, retry); a.Op == nil || a.Op.ID != first.Op.ID || a.Op.Status != string(opjournal.StatusClosed) || !a.Replayed {
				t.Fatalf("retry = %+v, want op %s closed and replayed", a, first.Op.ID)
			}
		})
	}
}

// A key reused for another request is 422 idempotency_mismatch, as OJ-4
// answers it, with nothing written: whether the key names the row a keyed
// request left, across a restart or after a refusal the Idempotency-Key LRU
// does not keep, or the lane op itself.
func TestRespondUnderLaneReusedKeyForAnotherRequestIsMismatch(t *testing.T) {
	wantMismatch := func(t *testing.T, e *opsEnv, info session.Info, key, body string) {
		t.Helper()
		rows := len(e.opRows(t))
		rec := e.post(t, "/session/"+info.ID+"/respond", key, body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "idempotency_mismatch") {
			t.Fatalf("%s under %q: %d %s, want 422 idempotency_mismatch", body, key, rec.Code, rec.Body.String())
		}
		if n := len(e.opRows(t)); n != rows {
			t.Fatalf("%s under %q wrote %d rows, want none", body, key, n-rows)
		}
	}
	t.Run("the key's row, after a restart", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		wantStatus(t, "first", e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve")), http.StatusAccepted)
		e.restart(t)
		e.resolver.state = st
		wantMismatch(t, e, info, "r-1", respondBody("deny"))
		wantMismatch(t, e, info, "r-1", `{"action":"approve","request_id":"other"}`)
		wantMismatch(t, e, info, "r-1", `{"action":"approve","text":"why"}`)
		same := e.post(t, "/session/"+info.ID+"/respond", "r-1", `{"action":"approve","request_id":"`+laneRespondRequest+`"}`)
		wantStatus(t, "same request", same, http.StatusAccepted)
		if a := decodeCommandAnswer(t, same); !a.Replayed {
			t.Fatalf("same request = %+v, want the first answer replayed", a)
		}
	})
	t.Run("the key's row, after a refusal in the same process", func(t *testing.T) {
		e, _, info := laneRespondEnv(t, outcome(opjournal.ArmFailed, opjournal.ReasonRefusedAction))
		first := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
		if first.Code != http.StatusConflict || !strings.Contains(first.Body.String(), "invalid_interaction") {
			t.Fatalf("first: %d %s, want 409 invalid_interaction", first.Code, first.Body.String())
		}
		wantMismatch(t, e, info, "r-1", respondBody("deny"))
	})
	t.Run("the lane op's own key", func(t *testing.T) {
		e, _, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve")))
		if first.Op == nil || !strings.HasPrefix(first.IdempotencyKey, "respond:") {
			t.Fatalf("key-less answer = %+v, want the op's occurrence key", first)
		}
		wantMismatch(t, e, info, first.IdempotencyKey, respondBody("deny"))
		same := e.post(t, "/session/"+info.ID+"/respond", first.IdempotencyKey, respondBody("approve"))
		wantStatus(t, "same answer", same, http.StatusAccepted)
		if a := decodeCommandAnswer(t, same); a.Op == nil || a.Op.ID != first.Op.ID || !a.Replayed {
			t.Fatalf("same answer = %+v, want op %s replayed", a, first.Op.ID)
		}
	})
}

// An answer that may have reached its prompt is never sent again, and its
// refusal offers no resend: a same-key retry replays it. Once the lane has
// ended its occurrence (ambiguous), the prompt still showing may be a later,
// identical one, so a request under a new key answers it as the next
// occurrence, with its own op, never the occurrence the first answer may
// have reached.
func TestRespondUnderLaneMaybeDeliveredOffersNoResend(t *testing.T) {
	e, st, info := laneRespondEnv(t, func(op opjournal.Op) *opjournal.Outcome {
		if op.Scope.Interaction == laneRespondRequest+"#1" {
			return &opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonAmbiguousDelivery}
		}
		return &opjournal.Outcome{Arm: opjournal.ArmSucceeded}
	})
	for _, what := range []string{"first", "same-key retry"} {
		rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), opAlreadyFailedCode) || strings.Contains(rec.Body.String(), "resend") {
			t.Fatalf("%s: %d %s, want 409 %s offering no resend", what, rec.Code, rec.Body.String(), opAlreadyFailedCode)
		}
	}
	resolveRaising(t, st.ops, info.ID, generationOf(t, e.store, info.ID), laneRespondRequest+"#1")
	next := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "r-2", respondBody("approve")))
	if next.Op == nil {
		t.Fatalf("new key = %+v, want an op", next)
	}
	if op, err := st.ops.Get(next.Op.ID); err != nil || op.Scope.Interaction != laneRespondRequest+"#2" {
		t.Fatalf("new key's op = %+v, %v; want the next occurrence", op, err)
	}
}

// laneRespondRefusal offers a resend under a new key only for a close that
// sent nothing: one that may have reached its prompt offers none, whatever
// its arm.
func TestLaneRespondRefusalOffersAResendOnlyWhenNothingWasSent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		closed opjournal.Outcome
		maybe  int
		resend bool
	}{
		{"ambiguous", opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonAmbiguousDelivery}, 1, false},
		{"deadline in flight", opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonDeadlineInFlight}, 1, false},
		{"canceled after a take that may have answered", opjournal.Outcome{Arm: opjournal.ArmCanceled}, 1, false},
		{"canceled before any take", opjournal.Outcome{Arm: opjournal.ArmCanceled}, 0, true},
		{"expired before start", opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonExpiredBeforeStart}, 0, true},
		{"poison", opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonPoison}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := laneRespondRefusal(opjournal.Op{
				ID: "gc-op-1", Kind: opjournal.KindRespond, Executor: opjournal.ExecutorLane,
				Status: opjournal.StatusClosed, Attempt: tc.maybe, MaybeDelivered: tc.maybe, Outcome: tc.closed,
			})
			if err == nil || statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), opAlreadyFailedCode) {
				t.Fatalf("refusal = %v (%d), want 409 %s", err, statusOf(err), opAlreadyFailedCode)
			}
			if got := strings.Contains(err.Error(), "resend"); got != tc.resend {
				t.Fatalf("refusal %q offers a resend: %t, want %t", err, got, tc.resend)
			}
		})
	}
	if err := laneRespondRefusal(opjournal.Op{Status: opjournal.StatusClosed, Outcome: opjournal.Outcome{Arm: opjournal.ArmSucceeded}}); err != nil {
		t.Fatalf("succeeded: refusal = %v, want none", err)
	}
}

// With respond unlisted again (a rollback, control=auto), a retry under a
// lane respond's key replays it through the row of the legacy op's frozen
// shape, which OJ-4 follows to the lane op: nothing answers the runtime.
func TestRespondUnderLaneRollbackRetryReplaysThroughTheRow(t *testing.T) {
	e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
	first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve")))
	if first.Op == nil {
		t.Fatalf("first = %+v", first)
	}
	st.kinds = []string{"message"}
	e.fs.cfg.Ops.Authoritative = st.kinds
	e.fs.cfg.Ops.ControlMode = "auto"
	e.restart(t)
	e.resolver.state = st
	retry := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	wantStatus(t, "retry after the rollback", retry, http.StatusAccepted)
	if a := decodeCommandAnswer(t, retry); a.Op == nil || a.Op.ID != first.Op.ID || !a.Replayed {
		t.Fatalf("retry = %+v, want the lane op %s replayed", a, first.Op.ID)
	}
	if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 0 {
		t.Fatalf("the retry answered the runtime %d times", n)
	}
}

func TestRespondWithoutLaneRespondKeepsOJ4(t *testing.T) {
	e, st := newLaneOpsEnv(t, "message")
	e.fs.cfg.Ops.ControlMode = "auto"
	e.restart(t)
	e.resolver.state = st
	info := e.session(t, "Legacy respond")
	e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: laneRespondRequest, Kind: "approval", Options: []string{"approve", "deny"}})
	rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	wantStatus(t, "legacy respond", rec, http.StatusAccepted)
	a := decodeCommandAnswer(t, rec)
	if a.Op == nil {
		t.Fatalf("answer = %+v, want OJ-4's recorded op", a)
	}
	op, err := st.ops.Get(a.Op.ID)
	if err != nil || op.Executor == opjournal.ExecutorLane || op.Scope.Interaction != laneRespondRequest {
		t.Fatalf("op = %+v, %v; want OJ-4's legacy op", op, err)
	}
	if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 1 {
		t.Fatalf("Respond calls = %d, want the legacy respond's one", n)
	}
}
