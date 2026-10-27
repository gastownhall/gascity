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

func TestRespondUnderLaneProposesForTheOccurrence(t *testing.T) {
	e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
	rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	wantStatus(t, "respond", rec, http.StatusAccepted)
	a := decodeCommandAnswer(t, rec)
	if a.Op == nil {
		t.Fatalf("answer = %+v", a)
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
	old := laneRespondAnswerBound
	laneRespondAnswerBound = 20 * time.Millisecond
	t.Cleanup(func() { laneRespondAnswerBound = old })
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
	old := laneRespondAnswerBound
	laneRespondAnswerBound = 20 * time.Millisecond
	t.Cleanup(func() { laneRespondAnswerBound = old })
	e, _, info := laneRespondEnv(t, nil)
	rec := e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve"))
	wantStatus(t, "open", rec, http.StatusAccepted)
	if a := decodeCommandAnswer(t, rec); a.Op == nil || a.Op.Status == string(opjournal.StatusClosed) {
		t.Fatalf("open answer = %+v, want the open op", a)
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
