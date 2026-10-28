package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// /respond under lane authority (design slice 5): with respond listed, the
// API proposes a respond op keyed by the interaction occurrence it answers,
// which the lane host runs at most once.

const laneRespondRequest = "req-1"

// pendingApproval is the approval laneRespondRequest that a lane respond
// session shows pending.
func pendingApproval() *runtime.PendingInteraction {
	return &runtime.PendingInteraction{RequestID: laneRespondRequest, Kind: "approval", Options: []string{"approve", "deny"}}
}

// laneRespondEnv is a city whose lane host executes respond, its passes
// scripted by closeWith (laneStopEnv), with a session showing a pending
// approval laneRespondRequest.
func laneRespondEnv(t *testing.T, closeWith func(opjournal.Op) *opjournal.Outcome) (*opsEnv, *lanePokeState, session.Info) {
	t.Helper()
	e, st := newLaneOpsEnv(t, "message", "respond")
	info := e.session(t, "Lane respond")
	e.fs.sp.SetPendingInteraction(info.SessionName, pendingApproval())
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

// firstRaisingResolved is the end of laneRespondRequest's first occurrence
// in conv's generation gen, as the lane records it once its answer lands.
func firstRaisingResolved(conv string, gen int64) opjournal.Fact {
	return opjournal.Fact{Kind: opjournal.FactInteractionResolved, Conversation: conv, Generation: gen, Interaction: laneRespondRequest + "#1", Basis: opjournal.FactBasisObserved, At: time.Now()}
}

// resolveFirstRaising records laneRespondRequest's first occurrence in
// conv's generation gen resolved (firstRaisingResolved).
func resolveFirstRaising(t *testing.T, st *opjournal.Store, conv string, gen int64) {
	t.Helper()
	if _, err := st.RecordFact(firstRaisingResolved(conv, gen)); err != nil {
		t.Fatalf("recording the resolution: %v", err)
	}
}

func respondBody(action string) string { return `{"action":"` + action + `"}` }

// respondOpRow reports whether a bead is the row of a respond op executor
// executes: a lane op, or the row a keyed lane respond pins its key with.
func respondOpRow(executor opjournal.ExecutorKind) func(beads.Bead) bool {
	return func(b beads.Bead) bool {
		op, err := opjournal.DecodeOp(b)
		return err == nil && op.Kind == opjournal.KindRespond && op.Executor == executor
	}
}

// laneRespondOps are the lane respond ops e's journal holds, open or closed.
func laneRespondOps(t *testing.T, e *opsEnv) []opjournal.Op {
	t.Helper()
	var out []opjournal.Op
	for _, b := range e.opRows(t) {
		op, err := opjournal.DecodeOp(b)
		if err != nil {
			t.Fatalf("decoding op %s: %v", b.ID, err)
		}
		if op.Kind == opjournal.KindRespond && op.Executor == opjournal.ExecutorLane {
			out = append(out, op)
		}
	}
	return out
}

// keyRow reads the op under the operator's key on conv: the row a keyed lane
// respond pins its key with.
func keyRow(t *testing.T, st *opjournal.Store, conv, key string) opjournal.Op {
	t.Helper()
	id, err := opjournal.DeriveID(st.Prefix(), conv, "operator", key)
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.Get(id)
	if err != nil {
		t.Fatalf("reading the row under key %q: %v", key, err)
	}
	return row
}

// wantNoRowUnder fails unless no op is stored under the operator's key on
// conv.
func wantNoRowUnder(t *testing.T, st *opjournal.Store, conv, key string) {
	t.Helper()
	id, err := opjournal.DeriveID(st.Prefix(), conv, legacyop.PrincipalOperator, key)
	if err != nil {
		t.Fatal(err)
	}
	if row, err := st.Get(id); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("row under key %q = %+v, %v; want none", key, row, err)
	}
}

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
		row.Executor != opjournal.ExecutorLegacy || row.Scope.Interaction != laneRespondRequest || row.Policy.Guarantee != opjournal.GuaranteeAtLeastOnce ||
		row.Causation != op.Scope.Interaction {
		t.Fatalf("row under the key = %+v, %v; want the legacy shape, never adopted, pinned to the occurrence, coalesced into the lane op", row, err)
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
	resolveFirstRaising(t, st.ops, info.ID, generationOf(t, e.store, info.ID))
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

// A keyed /respond writes the row that pins its key to the occurrence before
// it proposes the lane op: a row the journal cannot write is 503 with no op
// proposed, so nothing reaches the prompt, and a retry under the key answers
// the same occurrence.
func TestRespondUnderLaneKeyRowFailureProposesNothing(t *testing.T) {
	e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
	e.store.set(func(s *opsTestStore) { s.failCreate = respondOpRow(opjournal.ExecutorLegacy) })
	first := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), "op_journal_unavailable") {
		t.Fatalf("first: %d %s, want 503 op_journal_unavailable", first.Code, first.Body.String())
	}
	if ops := laneRespondOps(t, e); len(ops) != 0 {
		t.Fatalf("a request whose key's row failed proposed %+v", ops)
	}
	if got := st.poked(); len(got) != 0 {
		t.Fatalf("pokes = %v, want none", got)
	}
	e.store.set(func(s *opsTestStore) { s.failCreate = nil })
	retry := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	wantStatus(t, "retry", retry, http.StatusAccepted)
	if ops := laneRespondOps(t, e); len(ops) != 1 || ops[0].Scope.Interaction != laneRespondRequest+"#1" {
		t.Fatalf("lane respond ops = %+v, want the first occurrence's alone", ops)
	}
}

// A /respond keyed by its occurrence's own key, as a producer that saw the
// raising keys its answer (opjournal.RespondKey), names the lane op itself:
// it pins nothing, which would take the op's id and refuse every answer to
// the prompt key_reuse. The op answers, a retry under the key replays it
// after a restart, and a request under another key joins it.
func TestRespondUnderLaneUnderTheOccurrencesOwnKey(t *testing.T) {
	e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
	// A lane look records the raising the producer sees before it answers.
	raising, _, err := st.ops.RecordRaising(info.ID, generationOf(t, e.store, info.ID), laneRespondRequest, opjournal.FactBasisObserved, time.Now())
	if err != nil {
		t.Fatalf("recording the raising: %v", err)
	}
	key, err := opjournal.RespondKey(raising.Raised)
	if err != nil {
		t.Fatal(err)
	}
	first := e.post(t, "/session/"+info.ID+"/respond", key, respondBody("approve"))
	wantStatus(t, "first", first, http.StatusAccepted)
	a := decodeCommandAnswer(t, first)
	if ops := laneRespondOps(t, e); len(ops) != 1 || a.Op == nil || a.Op.ID != ops[0].ID || ops[0].ClientKey != key || a.IdempotencyKey != key {
		t.Fatalf("first = %+v, lane respond ops %+v; want the occurrence's op, under its own key", a, ops)
	}
	e.restart(t)
	e.resolver.state = st
	retry := e.post(t, "/session/"+info.ID+"/respond", key, respondBody("approve"))
	wantStatus(t, "retry", retry, http.StatusAccepted)
	if b := decodeCommandAnswer(t, retry); b.Op == nil || b.Op.ID != a.Op.ID || !b.Replayed {
		t.Fatalf("retry = %+v, want op %s replayed", b, a.Op.ID)
	}
	other := e.post(t, "/session/"+info.ID+"/respond", "r-2", respondBody("approve"))
	wantStatus(t, "another key", other, http.StatusAccepted)
	if c := decodeCommandAnswer(t, other); c.Op == nil || c.Op.ID != a.Op.ID || !c.Replayed || c.IdempotencyKey != "r-2" {
		t.Fatalf("another key = %+v, want op %s replayed under r-2", c, a.Op.ID)
	}
	if ops := laneRespondOps(t, e); len(ops) != 1 {
		t.Fatalf("lane respond ops = %+v, want one", ops)
	}
	if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 0 {
		t.Fatalf("the API answered the runtime itself %d times", n)
	}
}

// raisingKey records requestID raised in conv's generation gen, as a lane
// look records the prompt a producer sees, and returns the key of an answer
// to that raising (opjournal.RespondKey).
func raisingKey(t *testing.T, st *opjournal.Store, conv string, gen int64, requestID string) string {
	t.Helper()
	raising, _, err := st.RecordRaising(conv, gen, requestID, opjournal.FactBasisObserved, time.Now())
	if err != nil {
		t.Fatalf("recording the raising of %s: %v", requestID, err)
	}
	key, err := opjournal.RespondKey(raising.Raised)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// occurrenceKey is the key of an answer to occurrence occ of conv's
// generation gen (opjournal.RespondKey), raised or not.
func occurrenceKey(t *testing.T, st *opjournal.Store, conv string, gen int64, occ string) string {
	t.Helper()
	raised := opjournal.Fact{Kind: opjournal.FactInteractionRaised, Conversation: conv, Generation: gen, Interaction: occ}
	id, err := opjournal.FactID(st.Prefix(), raised)
	if err != nil {
		t.Fatal(err)
	}
	raised.ID = id
	key, err := opjournal.RespondKey(raised)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// A /respond whose key is an occurrence key (opjournal.IsRespondKey)
// answers only the occurrence its key names. A key that names no op and is
// not the key of the occurrence pending now (an occurrence that ended before
// an identical prompt was raised again, one of another request, one not
// raised yet, or none) is 409 no_pending, no op proposed and none pinned,
// however often it is sent. A pin would send its answer to the prompt
// pending now, and would take the id of the operator's lane op of the
// occurrence the key names, refusing every answer to that occurrence
// key_reuse. An unkeyed /respond answers the prompt pending now, and once the
// occurrence the key names is pending, an unkeyed answer to it is the op
// under the key.
func TestRespondUnderLaneAnotherOccurrencesKeyAnswersNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// key is the request's occurrence key, which may record a raising.
		key  func(t *testing.T, st *opjournal.Store, conv string, gen int64) string
		body string
		// now is the occurrence pending when the request is sent.
		now string
		// next: the key names the raising after now, which is pending once
		// now ends.
		next bool
	}{
		{
			name: "an occurrence that ended, an identical prompt pending",
			key: func(t *testing.T, st *opjournal.Store, conv string, gen int64) string {
				key := raisingKey(t, st, conv, gen, laneRespondRequest)
				resolveFirstRaising(t, st, conv, gen)
				return key
			},
			body: `{"action":"approve","request_id":"` + laneRespondRequest + `"}`,
			now:  laneRespondRequest + "#2",
		},
		{
			name: "an occurrence of another request",
			key: func(t *testing.T, st *opjournal.Store, conv string, gen int64) string {
				return raisingKey(t, st, conv, gen, "req-other")
			},
			body: respondBody("approve"),
			now:  laneRespondRequest + "#1",
		},
		{
			name: "the next raising of the request pending",
			key: func(t *testing.T, st *opjournal.Store, conv string, gen int64) string {
				return occurrenceKey(t, st, conv, gen, laneRespondRequest+"#2")
			},
			body: respondBody("approve"),
			now:  laneRespondRequest + "#1",
			next: true,
		},
		{
			name: "no raising",
			key:  func(*testing.T, *opjournal.Store, string, int64) string { return "respond:r-1" },
			body: respondBody("approve"),
			now:  laneRespondRequest + "#1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
			gen := generationOf(t, e.store, info.ID)
			key := tc.key(t, st.ops, info.ID, gen)
			for _, what := range []string{"first", "same-key retry"} {
				rows := len(e.opRows(t))
				rec := e.post(t, "/session/"+info.ID+"/respond", key, tc.body)
				if rec.Code != http.StatusConflict || problemCode(t, rec) != "session-conflict" || !strings.Contains(rec.Body.String(), "no_pending") {
					t.Fatalf("%s: %d %s, want 409 no_pending", what, rec.Code, rec.Body.String())
				}
				if n := len(e.opRows(t)) - rows; n != 0 {
					t.Fatalf("%s wrote %d op rows, want none", what, n)
				}
			}
			wantNoRowUnder(t, st.ops, info.ID, key)
			if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 0 {
				t.Fatalf("the API answered the runtime itself %d times", n)
			}
			unkeyed := e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve"))
			wantStatus(t, "an unkeyed answer", unkeyed, http.StatusAccepted)
			a := decodeCommandAnswer(t, unkeyed)
			if ops := laneRespondOps(t, e); len(ops) != 1 || a.Op == nil || a.Op.ID != ops[0].ID || ops[0].Scope.Interaction != tc.now {
				t.Fatalf("an unkeyed answer = %+v, lane respond ops %+v; want one, answering %s", a, ops, tc.now)
			}
			if !tc.next {
				return
			}
			resolveFirstRaising(t, st.ops, info.ID, gen)
			later := e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve"))
			wantStatus(t, "an unkeyed answer to the next raising", later, http.StatusAccepted)
			b := decodeCommandAnswer(t, later)
			if b.Op == nil || b.IdempotencyKey != key {
				t.Fatalf("an unkeyed answer to the next raising = %+v, want the op under key %q", b, key)
			}
			keyed := e.post(t, "/session/"+info.ID+"/respond", key, respondBody("approve"))
			wantStatus(t, "an answer under the key", keyed, http.StatusAccepted)
			if c := decodeCommandAnswer(t, keyed); c.Op == nil || c.Op.ID != b.Op.ID || !c.Replayed {
				t.Fatalf("an answer under the key = %+v, want op %s replayed", c, b.Op.ID)
			}
		})
	}
}

// A retry under the key of a request that ended before its lane op was
// written answers the occurrence that request pinned its key to, never a
// later, identical prompt with the same request id. Here that occurrence
// ended in between, so the lane closes the retry's op skipped{stale}, as it
// closes any answer to an occurrence that is over, and the retry is 409
// no_pending.
func TestRespondUnderLaneRetryAnswersThePinnedOccurrence(t *testing.T) {
	e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSkipped, opjournal.ReasonStale))
	e.store.set(func(s *opsTestStore) { s.failCreate = respondOpRow(opjournal.ExecutorLane) })
	first := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first: %d %s, want 503", first.Code, first.Body.String())
	}
	e.store.set(func(s *opsTestStore) { s.failCreate = nil })
	// The prompt the first request answered ends; an identical one, with the
	// same request id, is pending now.
	resolveFirstRaising(t, st.ops, info.ID, generationOf(t, e.store, info.ID))
	e.restart(t)
	e.resolver.state = st
	retry := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	if retry.Code != http.StatusConflict || !strings.Contains(retry.Body.String(), "no_pending") {
		t.Fatalf("retry: %d %s, want 409 no_pending", retry.Code, retry.Body.String())
	}
	ops := laneRespondOps(t, e)
	if len(ops) != 1 || ops[0].Scope.Interaction != laneRespondRequest+"#1" {
		t.Fatalf("lane respond ops = %+v, want one, answering the first occurrence", ops)
	}
	if row := keyRow(t, st.ops, info.ID, "r-1"); row.Status != opjournal.StatusClosed || row.Outcome.Result.CoalescedInto != ops[0].ID {
		t.Fatalf("row under the key = %+v, want it closed coalesced into op %s", row, ops[0].ID)
	}
}

// Once a keyed /respond's lane op is proposed and the lane poked, the answer
// may reach the prompt: a write that fails then neither answers 503 for it
// nor lets a retry under the key answer a later, identical prompt. The
// answer comes from the lane op under the request's key, and a retry after a
// restart follows the key's row to that op, whether the row is still open
// (its close failed, and the retry closes it) or the op sweeper closed it
// failed{deadline_in_flight} at its finish_by.
func TestRespondUnderLaneWriteFailingAfterThePokeKeepsOneAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  func(*opsTestStore)
		sweep bool
	}{
		{"a create fails", func(s *opsTestStore) { s.createErr = errors.New("transport: create failed") }, false},
		{"an update fails", func(s *opsTestStore) { s.updateFails = 1 }, false},
		{"an update fails, and the sweeper closes the row", func(s *opsTestStore) { s.updateFails = 1 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
			gen := generationOf(t, e.store, info.ID)
			lane, failed := st.onPoke, false
			st.onPoke = func(conv string) {
				lane(conv)
				if failed {
					return
				}
				failed = true
				// The lane resolves the occurrence in the batch that closes its
				// op, so a fresh request would answer the next one.
				if _, err := st.ops.RecordFact(firstRaisingResolved(conv, gen)); err != nil {
					t.Errorf("recording the resolution: %v", err)
				}
				e.store.set(tc.fail)
			}
			first := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
			wantStatus(t, "first", first, http.StatusAccepted)
			a := decodeCommandAnswer(t, first)
			if a.Op == nil || a.IdempotencyKey != "r-1" || a.Op.Status != string(opjournal.StatusClosed) {
				t.Fatalf("first = %+v, want the lane op's success under the request's key", a)
			}
			e.store.set(func(s *opsTestStore) { s.createErr, s.updateFails = nil, 0 })
			if tc.sweep {
				if _, err := legacyop.Sweep(st.ops, time.Now().Add(2*time.Hour), 100, "test:sweep"); err != nil {
					t.Fatalf("sweeping: %v", err)
				}
				if row := keyRow(t, st.ops, info.ID, "r-1"); row.Status != opjournal.StatusClosed || row.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
					t.Fatalf("swept row = %+v, want it closed failed{deadline_in_flight}", row)
				}
			}
			e.restart(t)
			e.resolver.state = st
			retry := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
			wantStatus(t, "retry", retry, http.StatusAccepted)
			if b := decodeCommandAnswer(t, retry); b.Op == nil || b.Op.ID != a.Op.ID || !b.Replayed {
				t.Fatalf("retry = %+v, want op %s replayed", b, a.Op.ID)
			}
			if ops := laneRespondOps(t, e); len(ops) != 1 {
				t.Fatalf("lane respond ops = %+v, want the first request's alone", ops)
			}
			if row := keyRow(t, st.ops, info.ID, "r-1"); !tc.sweep && (row.Status != opjournal.StatusClosed || row.Outcome.Result.CoalescedInto != a.Op.ID) {
				t.Fatalf("row under the key = %+v, want it closed coalesced into op %s", row, a.Op.ID)
			}
		})
	}
}

// Two raisings of one request id are two occurrences: an answer to each is
// its own op.
func TestRespondUnderLaneIdenticalPromptsGetTwoOps(t *testing.T) {
	e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
	first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve")))
	resolveFirstRaising(t, st.ops, info.ID, generationOf(t, e.store, info.ID))
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
// reuse: the first answer wins. One under a key of its own closes the row
// that pins the key failed{refused:key_reuse}, since every retry under the
// key meets the first answer again, and a retry is 409 key_reuse too.
func TestRespondUnderLaneDifferentAnswerIsKeyReuse(t *testing.T) {
	e, st, info := laneRespondEnv(t, nil)
	shortRespondBound(t)
	wantStatus(t, "first", e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve")), http.StatusAccepted)
	for _, key := range []string{"", "r-2", "r-2"} {
		second := e.post(t, "/session/"+info.ID+"/respond", key, respondBody("deny"))
		if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "key_reuse") {
			t.Fatalf("different answer under %q: %d %s, want 409 key_reuse", key, second.Code, second.Body.String())
		}
	}
	if row := keyRow(t, st.ops, info.ID, "r-2"); row.Status != opjournal.StatusClosed || row.Outcome.Arm != opjournal.ArmFailed ||
		row.Outcome.Reason != opjournal.RefusedReasonPrefix+refusedKeyReuse {
		t.Fatalf("row under the key = %+v, want it closed failed{refused:key_reuse}", row)
	}
	if ops := laneRespondOps(t, e); len(ops) != 1 {
		t.Fatalf("lane respond ops = %+v, want the first answer's alone", ops)
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
// 202, so the retry answers from the op's outcome. A retry while the op is
// still open is provisional too, and not replayed: an open op carries no
// stored outcome.
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
			open := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
			wantStatus(t, "retry while open", open, http.StatusAccepted)
			if a := decodeCommandAnswer(t, open); a.Op == nil || a.Op.ID != first.Op.ID || a.Op.Status == string(opjournal.StatusClosed) || a.Replayed {
				t.Fatalf("retry while open = %+v, want op %s still open, not replayed", a, first.Op.ID)
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
	resolveFirstRaising(t, st.ops, info.ID, generationOf(t, e.store, info.ID))
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

// rollBackRespond restarts e's API with respond unlisted again, as a
// rollback leaves it, and ops.control set to control ("" leaves it unset,
// at its builtin default): /respond takes OJ-4's path.
func rollBackRespond(t *testing.T, e *opsEnv, st *lanePokeState, control string) {
	t.Helper()
	st.kinds = []string{"message"}
	e.fs.cfg.Ops.Authoritative = st.kinds
	e.fs.cfg.Ops.ControlMode = control
	e.restart(t)
	e.resolver.state = st
}

// listRespond restarts e's API with respond listed again and ops.control set
// to control, as the controller restart that lists it does: /respond takes
// the lane's path, and nothing is in flight.
func listRespond(t *testing.T, e *opsEnv, st *lanePokeState, control string) {
	t.Helper()
	st.kinds = []string{"message", "respond"}
	e.fs.cfg.Ops.Authoritative = st.kinds
	e.fs.cfg.Ops.ControlMode = control
	e.restart(t)
	e.resolver.state = st
}

// rollbackControlModes are the ops.control settings a rollback may leave:
// auto set explicitly, which records over e's store; off, which records
// nothing; and unset, whose builtin auto records only over a SQLite-backed
// store. e's MemStore is not SQLite-backed, as a native Dolt sessions store
// is not, so neither of the last two opens it.
var rollbackControlModes = []struct{ name, mode string }{
	{"auto", "auto"},
	{"off", "off"},
	{"builtin default", ""},
}

// answerLeavingItsRowOpen answers laneRespondRequest under key r-1 through
// e's lane, whose op succeeds, while the close of the row that pins the key
// fails, so the row stays open. It returns the lane op's id.
func answerLeavingItsRowOpen(t *testing.T, e *opsEnv, st *lanePokeState, info session.Info) string {
	t.Helper()
	lane, failed := st.onPoke, false
	st.onPoke = func(conv string) {
		lane(conv)
		if !failed {
			failed = true
			e.store.set(func(s *opsTestStore) { s.updateFails = 1 })
		}
	}
	a := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve")))
	if a.Op == nil || a.Op.Status != string(opjournal.StatusClosed) {
		t.Fatalf("first = %+v, want the lane op's success", a)
	}
	if row := keyRow(t, st.ops, info.ID, "r-1"); row.Status == opjournal.StatusClosed {
		t.Fatalf("row under the key = %+v, want it open", row)
	}
	return a.Op.ID
}

// With respond unlisted again (a rollback), a request under a key the lane
// path wrote a respond under answers from that lane op, under the request's
// own key, whatever state the key's row is in and whatever ops.control says,
// and never runs the respond again: the lane's rows are read from its own
// store, which no gate governs, and OJ-4 never adopts the row that pins a
// key (ExecutorLegacy), whose respond could answer a later, identical
// prompt. While that store does not open, the request is 503, nothing done.
func TestRespondUnderLaneRollbackNeverRespondsAgain(t *testing.T) {
	for _, control := range rollbackControlModes {
		t.Run("ops.control "+control.name, func(t *testing.T) {
			testRespondUnderLaneRollback(t, control.mode)
		})
	}
}

// testRespondUnderLaneRollback is TestRespondUnderLaneRollbackNeverRespondsAgain
// for a rollback that leaves ops.control set to control.
func testRespondUnderLaneRollback(t *testing.T, control string) {
	spec, err := opjournal.Lookup(opjournal.KindRespond)
	if err != nil {
		t.Fatal(err)
	}
	// retryAfterRollback rolls e back, then posts body under key when OJ-4's
	// admission, while it records, adopts an open API-executed respond row:
	// the finish_by an adoption stamps (now plus legacyop.ExecutionBound) is
	// past the row's start_by, and the row's own finish_by is still ahead.
	// The retry may not answer the runtime.
	retryAfterRollback := func(t *testing.T, e *opsEnv, st *lanePokeState, info session.Info, key, body string) *httptest.ResponseRecorder {
		t.Helper()
		rollBackRespond(t, e, st, control)
		clk := &skewedClock{}
		clk.advance(spec.StartWithin - legacyop.ExecutionBound + time.Minute)
		e.server(t).ops.control.Clock = clk
		before := e.fs.sp.CountCalls("Respond", info.SessionName)
		rec := e.post(t, "/session/"+info.ID+"/respond", key, body)
		if n := e.fs.sp.CountCalls("Respond", info.SessionName) - before; n != 0 {
			t.Fatalf("%s under %q answered the runtime %d times", body, key, n)
		}
		return rec
	}
	wantReplayed := func(t *testing.T, rec *httptest.ResponseRecorder, key, op string) {
		t.Helper()
		wantStatus(t, "retry after the rollback", rec, http.StatusAccepted)
		if a := decodeCommandAnswer(t, rec); a.Op == nil || a.Op.ID != op || !a.Replayed || a.IdempotencyKey != key {
			t.Fatalf("retry = %+v, want lane op %s replayed under %q", a, op, key)
		}
	}
	wantConflict := func(t *testing.T, rec *httptest.ResponseRecorder, code string) {
		t.Helper()
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), code) {
			t.Fatalf("retry after the rollback: %d %s, want 409 %s", rec.Code, rec.Body.String(), code)
		}
	}
	t.Run("a row closed coalesced into the op", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve")))
		if first.Op == nil {
			t.Fatalf("first = %+v", first)
		}
		wantReplayed(t, retryAfterRollback(t, e, st, info, "r-1", respondBody("approve")), "r-1", first.Op.ID)
		other := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("deny"))
		if other.Code != http.StatusUnprocessableEntity || !strings.Contains(other.Body.String(), "idempotency_mismatch") {
			t.Fatalf("another answer under the key: %d %s, want 422 idempotency_mismatch", other.Code, other.Body.String())
		}
		if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 0 {
			t.Fatalf("another answer under the key answered the runtime %d times", n)
		}
	})
	t.Run("a row whose close failed", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		op := answerLeavingItsRowOpen(t, e, st, info)
		wantReplayed(t, retryAfterRollback(t, e, st, info, "r-1", respondBody("approve")), "r-1", op)
	})
	t.Run("a row the op sweeper closed", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		op := answerLeavingItsRowOpen(t, e, st, info)
		if _, err := legacyop.Sweep(st.ops, time.Now().Add(2*time.Hour), 100, "test:sweep"); err != nil {
			t.Fatalf("sweeping: %v", err)
		}
		if row := keyRow(t, st.ops, info.ID, "r-1"); row.Status != opjournal.StatusClosed || row.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
			t.Fatalf("swept row = %+v, want it closed failed{deadline_in_flight}", row)
		}
		wantReplayed(t, retryAfterRollback(t, e, st, info, "r-1", respondBody("approve")), "r-1", op)
	})
	t.Run("an open row whose occurrence's op records another answer", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		wantStatus(t, "first", e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve")), http.StatusAccepted)
		// The close of the row that pins r-2 fails, so it stays open.
		e.store.set(func(s *opsTestStore) { s.updateFails = 1 })
		second := e.post(t, "/session/"+info.ID+"/respond", "r-2", respondBody("deny"))
		if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "key_reuse") {
			t.Fatalf("different answer: %d %s, want 409 key_reuse", second.Code, second.Body.String())
		}
		if row := keyRow(t, st.ops, info.ID, "r-2"); row.Status == opjournal.StatusClosed {
			t.Fatalf("row under the key = %+v, want it open", row)
		}
		wantConflict(t, retryAfterRollback(t, e, st, info, "r-2", respondBody("deny")), "key_reuse")
	})
	t.Run("a row whose request ended before it proposed the op", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		e.store.set(func(s *opsTestStore) { s.failCreate = respondOpRow(opjournal.ExecutorLane) })
		wantStatus(t, "first", e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve")), http.StatusServiceUnavailable)
		e.store.set(func(s *opsTestStore) { s.failCreate = nil })
		// Nothing was sent, but nothing runs the respond for the row: it is
		// in flight until its finish_by, and replays the op sweeper's close
		// after it, which offers a resend.
		wantConflict(t, retryAfterRollback(t, e, st, info, "r-1", respondBody("approve")), "in_flight")
		if _, err := legacyop.Sweep(st.ops, time.Now().Add(2*time.Hour), 100, "test:sweep"); err != nil {
			t.Fatalf("sweeping: %v", err)
		}
		swept := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
		wantConflict(t, swept, opAlreadyFailedCode)
		if !strings.Contains(swept.Body.String(), "resend") {
			t.Fatalf("retry after the sweep: %s, want a resend offered", swept.Body.String())
		}
		if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 0 {
			t.Fatalf("a retry after the sweep answered the runtime %d times", n)
		}
	})
	t.Run("the lane op's own key", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve")))
		if first.Op == nil || !strings.HasPrefix(first.IdempotencyKey, "respond:") {
			t.Fatalf("key-less answer = %+v, want the op's occurrence key", first)
		}
		wantReplayed(t, retryAfterRollback(t, e, st, info, first.IdempotencyKey, respondBody("approve")), first.IdempotencyKey, first.Op.ID)
	})
	t.Run("an OJ-4 op the lane closed key_reuse", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, nil)
		shortRespondBound(t)
		openOJ4Respond(t, e, st, info, "auto")
		wantStatus(t, "another answer", e.post(t, "/session/"+info.ID+"/respond", "", respondBody("deny")), http.StatusAccepted)
		wantConflict(t, e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve")), "key_reuse")
		if op := keyRow(t, st.ops, info.ID, "r-1"); op.Status != opjournal.StatusClosed || op.Outcome.Reason != opjournal.RefusedReasonPrefix+refusedKeyReuse {
			t.Fatalf("OJ-4's op = %+v, want it closed failed{refused:key_reuse}", op)
		}
		wantConflict(t, retryAfterRollback(t, e, st, info, "r-1", respondBody("approve")), "key_reuse")
	})
	t.Run("an op store that does not open", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve")))
		if first.Op == nil {
			t.Fatalf("first = %+v", first)
		}
		rollBackRespond(t, e, st, control)
		ops := e.server(t).ops
		open := ops.openStore
		// Storage the open cannot reach, which NewStore reports as it
		// reports a store that cannot fence writes: the key's row is there
		// all the same.
		ops.openStore = func(beads.SessionStore, clock.Clock) (*opjournal.Store, error) {
			return nil, fmt.Errorf("opening op store: %w: acquiring storage: dial tcp 127.0.0.1:3307: connect: connection refused", beads.ErrConditionalWriteUnsupported)
		}
		rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "op_journal_unavailable") {
			t.Fatalf("retry while the op store does not open: %d %s, want 503 op_journal_unavailable", rec.Code, rec.Body.String())
		}
		if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 0 {
			t.Fatalf("a retry while the op store does not open answered the runtime %d times", n)
		}
		ops.openStore = open
		wantReplayed(t, e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve")), "r-1", first.Op.ID)
	})
}

// openOJ4Respond leaves OJ-4's respond of laneRespondRequest under key r-1
// open, as an API that died mid-respond leaves it, then lists respond again
// with ops.control set to control, as the restart that lists it does: with
// respond unlisted and ops.control auto, the runtime fails the answer
// retryably, which leaves the client-keyed op open, and the restart leaves
// nothing in flight. It returns the open op.
func openOJ4Respond(t *testing.T, e *opsEnv, st *lanePokeState, info session.Info, control string) opjournal.Op {
	t.Helper()
	rollBackRespond(t, e, st, "auto")
	e.fs.sp.RespondErrors[info.SessionName] = fmt.Errorf("the answer was not acknowledged: %w", runtime.ErrRuntimeUnavailable)
	first := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	delete(e.fs.sp.RespondErrors, info.SessionName)
	op := keyRow(t, st.ops, info.ID, "r-1")
	if first.Code != http.StatusServiceUnavailable || op.Status == opjournal.StatusClosed || op.Executor != opjournal.ExecutorLegacyAPI {
		t.Fatalf("OJ-4's respond: %d %s, op %+v; want 503 with its op left open", first.Code, first.Body.String(), op)
	}
	listRespond(t, e, st, control)
	return op
}

// An OJ-4 respond still open once respond is listed (its API died, and the
// restart that followed listed respond) is finished by a same-key retry,
// whatever ops.control says: OJ-4's admission adopts it, and the lane, which
// executes respond now, answers the request it names through the lane op of
// its occurrence, into which the adopted op closes coalesced. The API sends
// the runtime nothing itself, and a retry after that follows the op.
func TestRespondUnderLaneFinishesAnOpenOJ4Respond(t *testing.T) {
	for _, control := range rollbackControlModes {
		t.Run("ops.control "+control.name, func(t *testing.T) {
			e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
			open := openOJ4Respond(t, e, st, info, control.mode)
			retry := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
			wantStatus(t, "retry", retry, http.StatusAccepted)
			a := decodeCommandAnswer(t, retry)
			ops := laneRespondOps(t, e)
			if len(ops) != 1 || ops[0].Scope.Interaction != laneRespondRequest+"#1" || ops[0].Status != opjournal.StatusClosed ||
				a.Op == nil || a.Op.ID != ops[0].ID || a.IdempotencyKey != "r-1" || a.Replayed {
				t.Fatalf("retry = %+v, lane respond ops %+v; want the occurrence's lane op, answered under r-1", a, ops)
			}
			if op := keyRow(t, st.ops, info.ID, "r-1"); op.Status != opjournal.StatusClosed || op.Attempt != open.Attempt+1 ||
				op.Causation != ops[0].Scope.Interaction ||
				op.Outcome.Reason != opjournal.ReasonCoalescedInto || op.Outcome.Result.CoalescedInto != ops[0].ID {
				t.Fatalf("OJ-4's op = %+v, want it adopted, naming its occurrence, and closed coalesced into lane op %s", op, ops[0].ID)
			}
			if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 1 {
				t.Fatalf("Respond calls = %d, want OJ-4's failed one alone", n)
			}
			e.restart(t)
			e.resolver.state = st
			again := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
			wantStatus(t, "retry after that", again, http.StatusAccepted)
			if b := decodeCommandAnswer(t, again); b.Op == nil || b.Op.ID != ops[0].ID || !b.Replayed || b.IdempotencyKey != "r-1" {
				t.Fatalf("retry after that = %+v, want lane op %s replayed under r-1", b, ops[0].ID)
			}
			if n := len(laneRespondOps(t, e)); n != 1 {
				t.Fatalf("lane respond ops = %d, want the first alone", n)
			}
		})
	}
}

// OJ-4's admission decides what a same-key retry under the lane does with
// OJ-4's open respond, as it decides while respond is unlisted: an op in
// flight in this process is in_flight, untouched, and one past its finish_by
// closes failed{deadline_in_flight} and replays. A refusal at the lane's
// front door closes the adopted op as OJ-4 closes the refusal, no lane op
// proposed: no prompt pending is no_pending, and so is a session that moved
// to another generation since, whose prompt is a later one. A lane op that
// records another answer to the occurrence won: the adopted op closes
// failed{refused:key_reuse}, every retry's answer.
func TestRespondUnderLaneOpenOJ4RespondRefusals(t *testing.T) {
	retry := func(t *testing.T, e *opsEnv, info session.Info, problem, code string) {
		t.Helper()
		rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
		if rec.Code != http.StatusConflict || problemCode(t, rec) != problem || !strings.Contains(rec.Body.String(), code) {
			t.Fatalf("retry: %d %s, want 409 %s (%s)", rec.Code, rec.Body.String(), problem, code)
		}
	}
	wantClosed := func(t *testing.T, st *lanePokeState, info session.Info, arm opjournal.Arm, reason string) {
		t.Helper()
		if op := keyRow(t, st.ops, info.ID, "r-1"); op.Status != opjournal.StatusClosed || op.Outcome.Arm != arm || op.Outcome.Reason != reason {
			t.Fatalf("OJ-4's op = %+v, want it closed %s{%s}", op, arm, reason)
		}
	}
	wantNoLaneOp := func(t *testing.T, e *opsEnv) {
		t.Helper()
		if ops := laneRespondOps(t, e); len(ops) != 0 {
			t.Fatalf("lane respond ops = %+v, want none", ops)
		}
	}
	t.Run("in flight in this process", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		open := openOJ4Respond(t, e, st, info, "auto")
		if _, ok := e.server(t).ops.city.inflight.Reserve(open.ID, open.PayloadDigest); !ok {
			t.Fatalf("op %s is in flight already", open.ID)
		}
		retry(t, e, info, "idempotency-in-flight", "in_flight")
		if op := keyRow(t, st.ops, info.ID, "r-1"); op.Status == opjournal.StatusClosed || op.Attempt != open.Attempt {
			t.Fatalf("OJ-4's op = %+v, want it open and not adopted", op)
		}
		wantNoLaneOp(t, e)
	})
	t.Run("past its finish_by", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		openOJ4Respond(t, e, st, info, "auto")
		clk := &skewedClock{}
		clk.advance(legacyop.ExecutionBound + time.Minute)
		e.server(t).ops.laneAdopt.Clock = clk
		retry(t, e, info, "op-already-failed", opAlreadyFailedCode)
		wantClosed(t, st, info, opjournal.ArmFailed, opjournal.ReasonDeadlineInFlight)
		wantNoLaneOp(t, e)
	})
	t.Run("no prompt pending", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		openOJ4Respond(t, e, st, info, "auto")
		e.fs.sp.SetPendingInteraction(info.SessionName, nil)
		retry(t, e, info, "session-conflict", "no_pending")
		wantClosed(t, st, info, opjournal.ArmSkipped, opjournal.ReasonStale)
		wantNoLaneOp(t, e)
	})
	t.Run("the session moved to another generation", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		open := openOJ4Respond(t, e, st, info, "auto")
		if err := e.store.SetMetadata(info.ID, "generation", strconv.FormatInt(open.Scope.Generation+1, 10)); err != nil {
			t.Fatal(err)
		}
		retry(t, e, info, "session-conflict", "no_pending")
		wantClosed(t, st, info, opjournal.ArmSkipped, opjournal.ReasonStale)
		wantNoLaneOp(t, e)
	})
	t.Run("another answer won the occurrence", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, nil)
		shortRespondBound(t)
		openOJ4Respond(t, e, st, info, "auto")
		wantStatus(t, "another answer", e.post(t, "/session/"+info.ID+"/respond", "", respondBody("deny")), http.StatusAccepted)
		retry(t, e, info, "key-reuse", "key_reuse")
		wantClosed(t, st, info, opjournal.ArmFailed, opjournal.RefusedReasonPrefix+refusedKeyReuse)
		retry(t, e, info, "key-reuse", "key_reuse")
		if ops := laneRespondOps(t, e); len(ops) != 1 {
			t.Fatalf("lane respond ops = %+v, want the other answer's alone", ops)
		}
	})
}

// crashingLaneState is a lane State whose Servers' recorders carry the
// crash-point hook fn (OpsCrashPointsProvider).
type crashingLaneState struct {
	*lanePokeState
	fn legacyop.CrashPointFunc
}

func (c *crashingLaneState) OpsCrashPoints() legacyop.CrashPointFunc { return c.fn }

// adoptOJ4RespondAndDie leaves OJ-4's respond under key r-1 open
// (openOJ4Respond, respond listed after it with ops.control auto), then
// adopts it with a same-key retry whose API dies in the lane's pass, once
// the lane has answered: the lane op closed succeeded, and the adopted op
// still open, as everything the API did before it proposed the lane op left
// it. It returns the adopted op as it stands and the lane op.
func adoptOJ4RespondAndDie(t *testing.T, e *opsEnv, st *lanePokeState, info session.Info) (adopted, laneOp opjournal.Op) {
	t.Helper()
	open := openOJ4Respond(t, e, st, info, "auto")
	lane, died := st.onPoke, make(chan struct{})
	var once sync.Once
	st.onPoke = func(conv string) {
		lane(conv)
		once.Do(func() { close(died) })
		goruntime.Goexit()
	}
	answered := e.postAsync(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	select {
	case <-died:
	case rec := <-answered:
		t.Fatalf("the adopting retry answered %d %s, want its API dead once the lane answered", rec.Code, rec.Body.String())
	case <-time.After(testEventTimeout):
		t.Fatal("the adopting retry never poked the lane")
	}
	st.onPoke = lane
	ops := laneRespondOps(t, e)
	adopted = keyRow(t, st.ops, info.ID, "r-1")
	if len(ops) != 1 || ops[0].Status != opjournal.StatusClosed || ops[0].Outcome.Arm != opjournal.ArmSucceeded ||
		adopted.Status == opjournal.StatusClosed || adopted.Attempt != open.Attempt+1 {
		t.Fatalf("lane respond ops %+v, OJ-4's op %+v; want the lane op succeeded and OJ-4's op adopted, still open", ops, adopted)
	}
	return adopted, ops[0]
}

// An OJ-4 respond the lane adopted names the occurrence whose lane op
// answers it before that op is proposed (adoptLegacyRespond), as the row that
// pins a key does, so a retry under its key follows it to that lane op even
// when its close never landed: the API that adopted it died once the lane
// answered, leaving it open, and the op sweeper may have closed it
// failed{deadline_in_flight} since. With respond unlisted again (a rollback),
// the retry answers from the lane op whatever ops.control says and never
// answers the runtime again, which could answer a later, identical prompt;
// with respond still listed, it replays the lane op's success rather than
// offering a resend.
func TestRespondUnderLaneAdoptedOJ4RespondWhoseCloseNeverLanded(t *testing.T) {
	type next struct {
		name string
		then func(t *testing.T, e *opsEnv, st *lanePokeState)
	}
	nexts := []next{{"respond still listed", func(t *testing.T, e *opsEnv, st *lanePokeState) {
		e.restart(t)
		e.resolver.state = st
	}}}
	for _, control := range rollbackControlModes {
		nexts = append(nexts, next{"rolled back, ops.control " + control.name, func(t *testing.T, e *opsEnv, st *lanePokeState) {
			rollBackRespond(t, e, st, control.mode)
		}})
	}
	for _, swept := range []bool{false, true} {
		for _, n := range nexts {
			name := "open, " + n.name
			if swept {
				name = "swept, " + n.name
			}
			t.Run(name, func(t *testing.T) {
				e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
				_, laneOp := adoptOJ4RespondAndDie(t, e, st, info)
				if swept {
					if _, err := legacyop.Sweep(st.ops, time.Now().Add(2*time.Hour), 100, "test:sweep"); err != nil {
						t.Fatalf("sweeping: %v", err)
					}
					if row := keyRow(t, st.ops, info.ID, "r-1"); row.Status != opjournal.StatusClosed || row.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
						t.Fatalf("swept op = %+v, want it closed failed{deadline_in_flight}", row)
					}
				}
				n.then(t, e, st)
				before := e.fs.sp.CountCalls("Respond", info.SessionName)
				rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
				if calls := e.fs.sp.CountCalls("Respond", info.SessionName) - before; calls != 0 {
					t.Fatalf("the retry answered the runtime %d times", calls)
				}
				wantStatus(t, "retry", rec, http.StatusAccepted)
				if a := decodeCommandAnswer(t, rec); a.Op == nil || a.Op.ID != laneOp.ID || !a.Replayed || a.IdempotencyKey != "r-1" {
					t.Fatalf("retry = %+v, want lane op %s replayed under r-1", a, laneOp.ID)
				}
				if ops := laneRespondOps(t, e); len(ops) != 1 {
					t.Fatalf("lane respond ops = %+v, want the first alone", ops)
				}
			})
		}
	}
}

// The adopted OJ-4 respond names its occurrence before the lane op is
// proposed: a write of that name that fails leaves the op open, adopted and
// naming nothing, proposes no lane op and pokes no lane, so nothing reaches
// the prompt, and the request is 503. A retry adopts the op again and
// answers through the lane.
func TestRespondUnderLaneAdoptedOJ4RespondUnnamedProposesNothing(t *testing.T) {
	e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
	open := openOJ4Respond(t, e, st, info, "auto")
	// A lane look recorded the raising, so the adoption records none, and its
	// next op-row update names the occurrence.
	raisingKey(t, st.ops, info.ID, generationOf(t, e.store, info.ID), laneRespondRequest)
	e.resolver.state = &crashingLaneState{lanePokeState: st, fn: func(cp legacyop.CrashPoint, op opjournal.Op) error {
		if cp == legacyop.CrashPointAdmitted && op.ID == open.ID && op.Attempt == open.Attempt+1 {
			e.store.set(func(s *opsTestStore) { s.updateFails = 1 })
		}
		return nil
	}}
	first := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), "op_journal_unavailable: naming interaction "+laneRespondRequest+"#1") {
		t.Fatalf("first: %d %s, want 503 op_journal_unavailable naming the occurrence", first.Code, first.Body.String())
	}
	if ops := laneRespondOps(t, e); len(ops) != 0 {
		t.Fatalf("lane respond ops = %+v, want none", ops)
	}
	if got := st.poked(); len(got) != 0 {
		t.Fatalf("pokes = %v, want none", got)
	}
	if op := keyRow(t, st.ops, info.ID, "r-1"); op.Status == opjournal.StatusClosed || op.Attempt != open.Attempt+1 || op.Causation != "" {
		t.Fatalf("OJ-4's op = %+v, want it adopted, open, naming nothing", op)
	}
	retry := e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve"))
	wantStatus(t, "retry", retry, http.StatusAccepted)
	a := decodeCommandAnswer(t, retry)
	ops := laneRespondOps(t, e)
	if len(ops) != 1 || a.Op == nil || a.Op.ID != ops[0].ID || ops[0].Status != opjournal.StatusClosed {
		t.Fatalf("retry = %+v, lane respond ops %+v; want the occurrence's lane op", a, ops)
	}
	if op := keyRow(t, st.ops, info.ID, "r-1"); op.Status != opjournal.StatusClosed || op.Attempt != open.Attempt+2 ||
		op.Causation != ops[0].Scope.Interaction || op.Outcome.Result.CoalescedInto != ops[0].ID {
		t.Fatalf("OJ-4's op = %+v, want it adopted again, naming %s, closed coalesced into lane op %s", op, ops[0].Scope.Interaction, ops[0].ID)
	}
	if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 1 {
		t.Fatalf("Respond calls = %d, want OJ-4's failed one alone", n)
	}
}

// While OJ-4 answers /respond (respond unlisted: a rollback, or no lane host
// runs), a request under a raising's own key (opjournal.RespondKey, as a
// producer that saw the raising keys its answer) records no op under the
// key, however it ends: the key derives the id of the operator's lane op
// answering that raising, so an op there would refuse every answer to the
// raising key_reuse once respond is listed again (legacyop.ErrReservedKey).
// auto answers it unrecorded, and require refuses it, 422
// invalid_idempotency_key, nothing sent. Once respond is listed again, an
// unkeyed answer to the raising is the raising's lane op, under that key.
func TestRespondWhileOJ4AnswersUnderARaisingsKeyRecordsNothingUnderIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fails fails OJ-4's answer retryably; otherwise it lands, and an
		// identical prompt is raised again before any lane look.
		fails  bool
		status int
	}{
		{"auto, an answer that fails", true, http.StatusServiceUnavailable},
		{"auto, an answer that lands", false, http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
			key := raisingKey(t, st.ops, info.ID, generationOf(t, e.store, info.ID), laneRespondRequest)
			rollBackRespond(t, e, st, "auto")
			if tc.fails {
				e.fs.sp.RespondErrors[info.SessionName] = fmt.Errorf("the answer was not acknowledged: %w", runtime.ErrRuntimeUnavailable)
			}
			wantStatus(t, "OJ-4's answer", e.post(t, "/session/"+info.ID+"/respond", key, respondBody("approve")), tc.status)
			delete(e.fs.sp.RespondErrors, info.SessionName)
			if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 1 {
				t.Fatalf("Respond calls = %d, want OJ-4's one", n)
			}
			wantNoRowUnder(t, st.ops, info.ID, key)
			// The prompt is still pending, or was raised again identically.
			e.fs.sp.SetPendingInteraction(info.SessionName, pendingApproval())
			listRespond(t, e, st, "auto")
			rec := e.post(t, "/session/"+info.ID+"/respond", "", respondBody("approve"))
			wantStatus(t, "an unkeyed answer once respond is listed", rec, http.StatusAccepted)
			a := decodeCommandAnswer(t, rec)
			ops := laneRespondOps(t, e)
			if len(ops) != 1 || a.Op == nil || a.Op.ID != ops[0].ID || ops[0].ClientKey != key || ops[0].Scope.Interaction != laneRespondRequest+"#1" {
				t.Fatalf("an unkeyed answer = %+v, lane respond ops %+v; want the raising's lane op, under %q", a, ops, key)
			}
		})
	}
	t.Run("require", func(t *testing.T) {
		e, st, info := laneRespondEnv(t, outcome(opjournal.ArmSucceeded, ""))
		key := raisingKey(t, st.ops, info.ID, generationOf(t, e.store, info.ID), laneRespondRequest)
		rollBackRespond(t, e, st, "require")
		rows := len(e.opRows(t))
		rec := e.post(t, "/session/"+info.ID+"/respond", key, respondBody("approve"))
		if rec.Code != http.StatusUnprocessableEntity || problemCode(t, rec) != "invalid-idempotency-key" {
			t.Fatalf("OJ-4's answer: %d %s, want 422 invalid_idempotency_key", rec.Code, rec.Body.String())
		}
		if n := len(e.opRows(t)) - rows; n != 0 {
			t.Fatalf("a refused answer wrote %d op rows, want none", n)
		}
		if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 0 {
			t.Fatalf("a refused answer answered the runtime %d times", n)
		}
	})
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

// A keyed /respond over a sessions store that names no id namespace reaches
// OJ-4, whatever ops.control says: no op id derives over that store, so the
// lane path, which opens the same op store, never wrote a row the key could
// name.
func TestRespondOverAStoreWithNoNamespaceKeepsOJ4(t *testing.T) {
	for _, control := range rollbackControlModes {
		t.Run("ops.control "+control.name, func(t *testing.T) {
			e, st := newLaneOpsEnv(t, "message")
			e.fs.cfg.Ops.ControlMode = control.mode
			e.restart(t)
			e.resolver.state = st
			// The MemStore under e's store names no id prefix.
			e.server(t).ops.openStore = func(_ beads.SessionStore, clk clock.Clock) (*opjournal.Store, error) {
				return opjournal.NewStore(beads.SessionStore{Store: e.store.MemStore}, clk)
			}
			info := e.session(t, "Legacy respond")
			e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: laneRespondRequest, Kind: "approval", Options: []string{"approve", "deny"}})
			wantStatus(t, "respond", e.post(t, "/session/"+info.ID+"/respond", "r-1", respondBody("approve")), http.StatusAccepted)
			if n := e.fs.sp.CountCalls("Respond", info.SessionName); n != 1 {
				t.Fatalf("Respond calls = %d, want OJ-4's one", n)
			}
		})
	}
}
