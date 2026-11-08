package lanesubmit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/session"
)

// hookStore is a MemStore that names its id prefix, as a real sessions store
// does, whose op-row writes and reads can be made to fail or run a hook.
type hookStore struct {
	*beads.MemStore

	mu sync.Mutex
	// onOpCreate runs once after the next op-row create commits.
	onOpCreate func()
	// ambiguous commits the next op-row create, then answers an error.
	ambiguous bool
	// updateFails fails this many op-row conditional updates.
	updateFails int
	// getFails fails reads of failID.
	failID string
	// upsert overwrites an op row whose pinned id it already holds instead of
	// refusing the create: a store without the duplicate-id contract.
	upsert bool
}

func (s *hookStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.Type != opjournal.BeadType {
		return s.MemStore.Create(b)
	}
	s.mu.Lock()
	onCreate, ambiguous, upsert := s.onOpCreate, s.ambiguous, s.upsert
	s.onOpCreate, s.ambiguous = nil, false
	s.mu.Unlock()
	if upsert && b.ID != "" {
		if _, err := s.MemStore.Get(b.ID); err == nil {
			if err := s.Delete(b.ID); err != nil {
				return beads.Bead{}, err
			}
		}
	}
	created, err := s.MemStore.Create(b)
	if err != nil {
		return created, err
	}
	if onCreate != nil {
		onCreate()
	}
	if ambiguous {
		return beads.Bead{}, errors.New("transport: connection reset after commit")
	}
	return created, nil
}

func (s *hookStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	fail := s.failID != "" && s.failID == id
	s.mu.Unlock()
	if fail {
		return beads.Bead{}, errors.New("store hiccup")
	}
	return s.MemStore.Get(id)
}

func (s *hookStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if b, err := s.MemStore.Get(id); err == nil && b.Type == opjournal.BeadType {
		s.mu.Lock()
		fail := s.updateFails > 0
		if fail {
			s.updateFails--
		}
		s.mu.Unlock()
		if fail {
			return errors.New("transport: update failed")
		}
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func (s *hookStore) set(f func(*hookStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// acceptT0 is when the fixture's op store proposes ops: an hour before the
// wall clock Accept's resume reads.
var acceptT0 = time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

// acceptEnv is a session row and the op store over its store, with Accept's
// hooks recording their calls in steps.
type acceptEnv struct {
	store    *hookStore
	st       *opjournal.Store
	sessions *session.Store
	id       string
	steps    []string
	pending  error
}

func newAcceptEnv(t *testing.T) *acceptEnv {
	t.Helper()
	store := &hookStore{MemStore: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}}
	row := map[string]string{
		"session_name": "s-accept", "template": "worker", "work_dir": "/tmp", "provider": "claude",
		"generation": "3", "instance_token": "tok-3", "state": string(session.StateAsleep), "slept_at": "2026-10-08T10:00:00Z",
	}
	b, err := store.Create(beads.Bead{Title: "worker", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: row})
	if err != nil {
		t.Fatal(err)
	}
	st, err := opjournal.NewStore(beads.SessionStore{Store: store}, &clock.Fake{Time: acceptT0}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	return &acceptEnv{store: store, st: st, sessions: session.NewStore(beads.SessionStore{Store: store}), id: b.ID}
}

func (e *acceptEnv) request(key, text string, intent session.SubmitIntent, resume bool) Request {
	return Request{
		Key: key, Text: text, Intent: intent, Resume: resume,
		Resolve: func(context.Context) (string, error) { e.steps = append(e.steps, "resolve"); return e.id, nil },
		Pending: func(context.Context, string) error { e.steps = append(e.steps, "pending"); return e.pending },
	}
}

func (e *acceptEnv) accept(req Request) (Accepted, error) {
	return Accept(context.Background(), e.st, e.sessions, req)
}

func (e *acceptEnv) ops(t *testing.T) []opjournal.Op {
	t.Helper()
	rows, err := e.store.List(beads.ListQuery{Label: opjournal.LabelOp, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	ops := make([]opjournal.Op, 0, len(rows))
	for _, r := range rows {
		op, err := e.st.Get(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	return ops
}

func (e *acceptEnv) opID(t *testing.T, key string) string {
	t.Helper()
	id, err := opjournal.DeriveID("gc", e.id, legacyop.PrincipalOperator, key)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// suspend places the operator's hold, stamped at.
func (e *acceptEnv) suspend(t *testing.T, at time.Time) {
	t.Helper()
	if _, err := e.sessions.OperatorSuspend(e.id, at); err != nil {
		t.Fatalf("suspending: %v", err)
	}
}

func (e *acceptEnv) drain(t *testing.T) {
	t.Helper()
	if err := e.store.SetMetadata(e.id, "state", string(session.StateDraining)); err != nil {
		t.Errorf("draining: %v", err)
	}
}

func (e *acceptEnv) meta(t *testing.T) map[string]string {
	t.Helper()
	b, err := e.store.Get(e.id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata
}

// propose records an op of executor under key as its producer would: a lane
// op under Policy, a legacy one under LegacyPolicy. outcome closes it.
func (e *acceptEnv) propose(t *testing.T, executor opjournal.ExecutorKind, intent session.SubmitIntent, key, text string, outcome *opjournal.Outcome) opjournal.Op {
	t.Helper()
	req := opjournal.Request{
		Conversation: e.id, Kind: opjournal.KindMessage, Executor: executor, ClientKey: key,
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  text, Policy: Policy(intent),
	}
	if executor != opjournal.ExecutorLane {
		req.Policy = LegacyPolicy(intent)
		req.StartBy, req.FinishBy = acceptT0.Add(legacyop.ExecutionBound), acceptT0.Add(legacyop.ExecutionBound)
	}
	op, _, err := e.st.Propose(req)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if outcome != nil {
		if _, err := e.st.Close(op.ID, *outcome, "test"); err != nil {
			t.Fatalf("closing: %v", err)
		}
	}
	stored, err := e.st.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// A refusal Accept can tell before the session resolves resolves nothing and
// writes nothing: an invalid key, and a payload the journal refuses.
func TestAcceptRefusesBeforeResolving(t *testing.T) {
	for _, tc := range []struct {
		name, key, text string
		want            any
	}{
		{name: "an invalid key", key: " bad", text: "hi", want: new(*KeyError)},
		{name: "invalid UTF-8", key: "k-1", text: "\xff", want: new(*JournalError)},
		{name: "over the payload limit", key: "k-1", text: strings.Repeat("x", 16*1024+1), want: new(*JournalError)},
		{name: "minted key, invalid UTF-8", text: "\xff", want: new(*JournalError)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newAcceptEnv(t)
			_, err := e.accept(e.request(tc.key, tc.text, session.SubmitIntentDefault, true))
			if err == nil || !errors.As(err, tc.want) {
				t.Fatalf("Accept = %v, want %T", err, tc.want)
			}
			if len(e.steps) != 0 || len(e.ops(t)) != 0 {
				t.Fatalf("steps %v, ops %d; want nothing resolved or written", e.steps, len(e.ops(t)))
			}
		})
	}
}

// The front door (req.Pending) checks only a request no op under its key
// answers, before anything is written, and its refusal comes back as it
// returned it; a same-key retry is answered from its op whatever the session
// waits on (request_id rule 8).
func TestAcceptChecksOnlyANewRequestAtTheFrontDoor(t *testing.T) {
	e := newAcceptEnv(t)
	acc, err := e.accept(e.request("k-1", "hi", session.SubmitIntentFollowUp, false))
	if err != nil || !acc.Owned || acc.Legacy || acc.Key != "k-1" || acc.Op.Executor != opjournal.ExecutorLane || !slices.Equal(e.steps, []string{"resolve", "pending"}) {
		t.Fatalf("new request = %+v, %v, steps %v", acc, err, e.steps)
	}
	if acc.Op.Producer.Principal != legacyop.PrincipalOperator || acc.Op.Producer.Actor != opjournal.ActorOperator || acc.Op.Payload != "hi" {
		t.Fatalf("op = %+v, want the operator's message", acc.Op)
	}
	pending := errors.New("a prompt is pending")
	e.steps, e.pending = nil, pending
	again, err := e.accept(e.request("k-1", "hi", session.SubmitIntentDefault, false))
	if err != nil || again.Op.ID != acc.Op.ID || again.Owned || !slices.Equal(e.steps, []string{"resolve"}) {
		t.Fatalf("retry = %+v, %v, steps %v; want the op answered past the front door", again, err, e.steps)
	}
	e.steps = nil
	if _, err := e.accept(e.request("k-2", "hi", session.SubmitIntentDefault, false)); err != pending { //nolint:errorlint // the front door's error comes back as is
		t.Fatalf("new request while pending: %v, want the front door's error as is", err)
	}
	if !slices.Equal(e.steps, []string{"resolve", "pending"}) {
		t.Fatalf("steps %v", e.steps)
	}
	if _, err := e.st.Get(e.opID(t, "k-2")); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("op after the front door refused: %v, want none", err)
	}
}

// A minted key names no op yet: the request is new and its op owned.
func TestAcceptMintsAKey(t *testing.T) {
	e := newAcceptEnv(t)
	acc, err := e.accept(e.request("", "hi", session.SubmitIntentDefault, false))
	if err != nil || !acc.Owned || !strings.HasPrefix(acc.Key, "k-") || acc.Op.ClientKey != acc.Key {
		t.Fatalf("Accept = %+v, %v; want an owned op under a minted key", acc, err)
	}
}

// A legacy message op under the key, recorded before message was listed,
// answers only the request it recorded (its digest under LegacyPolicy), with
// nothing proposed, front-door checked or resumed; another message, another
// intent, or a default op under a follow_up (whose lane policies agree) is a
// different request.
func TestAcceptAnswersALegacyOpOnlyForItsRequest(t *testing.T) {
	succeeded := &opjournal.Outcome{Arm: opjournal.ArmSucceeded}
	for _, executor := range []opjournal.ExecutorKind{opjournal.ExecutorLegacyAPI, opjournal.ExecutorLegacyCLI} {
		for _, outcome := range []*opjournal.Outcome{nil, succeeded} {
			for _, intent := range []session.SubmitIntent{session.SubmitIntentDefault, session.SubmitIntentFollowUp, session.SubmitIntentInterruptNow} {
				t.Run(string(executor)+"/closed="+boolText(outcome != nil)+"/"+string(intent), func(t *testing.T) {
					e := newAcceptEnv(t)
					e.suspend(t, acceptT0.Add(-time.Minute))
					prior := e.propose(t, executor, intent, "k-legacy", "hi", outcome)
					acc, err := e.accept(e.request("k-legacy", "hi", intent, true))
					if err != nil || !acc.Legacy || acc.Owned || acc.Op.ID != prior.ID || acc.Op.Revision != prior.Revision {
						t.Fatalf("Accept = %+v, %v; want the legacy op %s", acc, err, prior.ID)
					}
					if !slices.Equal(e.steps, []string{"resolve"}) || len(e.ops(t)) != 1 || e.meta(t)["state"] != string(session.StateSuspended) {
						t.Fatalf("steps %v, ops %d, state %q; want no front door, no proposal and no resume", e.steps, len(e.ops(t)), e.meta(t)["state"])
					}
				})
			}
		}
	}
	for _, tc := range []struct {
		name             string
		recorded, intent session.SubmitIntent
		text             string
	}{
		{name: "another message", recorded: session.SubmitIntentDefault, intent: session.SubmitIntentDefault, text: "an earlier message"},
		{name: "another intent", recorded: session.SubmitIntentFollowUp, intent: session.SubmitIntentDefault, text: "hi"},
		{name: "a default op under a follow_up", recorded: session.SubmitIntentDefault, intent: session.SubmitIntentFollowUp, text: "hi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newAcceptEnv(t)
			prior := e.propose(t, opjournal.ExecutorLegacyCLI, tc.recorded, "k-reused", tc.text, succeeded)
			_, err := e.accept(e.request("k-reused", "hi", tc.intent, true))
			var jerr *JournalError
			var reuse *opjournal.KeyReuseError
			if !errors.As(err, &jerr) || !errors.As(err, &reuse) || reuse.Existing.ID != prior.ID {
				t.Fatalf("Accept = %v, want the key reuse of op %s", err, prior.ID)
			}
			if !slices.Equal(e.steps, []string{"resolve"}) || len(e.ops(t)) != 1 {
				t.Fatalf("steps %v, ops %d", e.steps, len(e.ops(t)))
			}
		})
	}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// A key that names an op of another kind is a different request, and
// nothing is written.
func TestAcceptRefusesAKeyOfAnotherKind(t *testing.T) {
	e := newAcceptEnv(t)
	wake, _, err := e.st.Propose(opjournal.Request{
		Conversation: e.id, Kind: opjournal.KindWake, Executor: opjournal.ExecutorLegacyCLI, ClientKey: "k-wake",
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		StartBy:  acceptT0.Add(legacyop.ExecutionBound), FinishBy: acceptT0.Add(legacyop.ExecutionBound),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.accept(e.request("k-wake", "hi", session.SubmitIntentDefault, true))
	var kerr *KindError
	if !errors.As(err, &kerr) || kerr.Op.ID != wake.ID || len(e.ops(t)) != 1 || !slices.Equal(e.steps, []string{"resolve"}) {
		t.Fatalf("Accept = %v, ops %d, steps %v; want the wake op %s refused as another kind", err, len(e.ops(t)), e.steps, wake.ID)
	}
}

// A read of the op the key names that fails proposes nothing: the API may
// have accepted the message.
func TestAcceptReadErrorWritesNothing(t *testing.T) {
	e := newAcceptEnv(t)
	id := e.opID(t, "k-unread")
	e.store.set(func(s *hookStore) { s.failID = id })
	_, err := e.accept(e.request("k-unread", "hi", session.SubmitIntentDefault, true))
	var rerr *ReadError
	if !errors.As(err, &rerr) || rerr.ID != id || len(e.ops(t)) != 0 || !slices.Equal(e.steps, []string{"resolve"}) {
		t.Fatalf("Accept = %v, ops %d, steps %v; want the read refused", err, len(e.ops(t)), e.steps)
	}
}

// With Resume, the operator's hold goes only once the op is written, and
// only a hold stamped at or before the op's created_at: a suspend placed
// later stands through every retry, and a closed op resumes nothing.
func TestAcceptResumesAfterTheProposal(t *testing.T) {
	t.Run("consumes the hold after the write", func(t *testing.T) {
		e := newAcceptEnv(t)
		e.suspend(t, acceptT0.Add(-time.Minute))
		var stateAtCreate string
		e.store.set(func(s *hookStore) {
			s.onOpCreate = func() { stateAtCreate = e.meta(t)["state"] }
		})
		acc, err := e.accept(e.request("k-r", "hi", session.SubmitIntentDefault, true))
		if err != nil || !acc.Owned {
			t.Fatalf("Accept = %+v, %v", acc, err)
		}
		if stateAtCreate != string(session.StateSuspended) {
			t.Fatalf("state at the op's create = %q, want the hold still in place", stateAtCreate)
		}
		if m := e.meta(t); m["state"] != string(session.StateAsleep) || m["wake_request"] != string(session.WakeCauseExplicit) {
			t.Fatalf("row = %v, want the hold consumed and its wake recorded", m)
		}
	})
	t.Run("a retry consumes a hold the first request left", func(t *testing.T) {
		e := newAcceptEnv(t)
		e.suspend(t, acceptT0.Add(-time.Minute))
		first := e.propose(t, opjournal.ExecutorLane, session.SubmitIntentDefault, "k-r", "hi", nil)
		acc, err := e.accept(e.request("k-r", "hi", session.SubmitIntentDefault, true))
		if err != nil || acc.Owned || acc.Op.ID != first.ID {
			t.Fatalf("Accept = %+v, %v; want the open op, not owned", acc, err)
		}
		if m := e.meta(t); m["state"] != string(session.StateAsleep) {
			t.Fatalf("row = %v, want the hold consumed", m)
		}
	})
	t.Run("a later suspend stands", func(t *testing.T) {
		e := newAcceptEnv(t)
		e.suspend(t, acceptT0.Add(time.Minute))
		if _, err := e.accept(e.request("k-r", "hi", session.SubmitIntentDefault, true)); err != nil {
			t.Fatal(err)
		}
		if m := e.meta(t); m["state"] != string(session.StateSuspended) || m["wake_request"] != "" {
			t.Fatalf("row = %v, want the later suspend standing", m)
		}
	})
	t.Run("a closed op resumes nothing", func(t *testing.T) {
		e := newAcceptEnv(t)
		e.suspend(t, acceptT0.Add(-time.Minute))
		done := e.propose(t, opjournal.ExecutorLane, session.SubmitIntentDefault, "k-r", "hi", &opjournal.Outcome{Arm: opjournal.ArmSucceeded})
		acc, err := e.accept(e.request("k-r", "hi", session.SubmitIntentDefault, true))
		if err != nil || acc.Op.ID != done.ID || acc.Op.Status != opjournal.StatusClosed {
			t.Fatalf("Accept = %+v, %v", acc, err)
		}
		if m := e.meta(t); m["state"] != string(session.StateSuspended) {
			t.Fatalf("row = %v, want the hold left", m)
		}
	})
	t.Run("without resume the hold stands", func(t *testing.T) {
		e := newAcceptEnv(t)
		e.suspend(t, acceptT0.Add(-time.Minute))
		if _, err := e.accept(e.request("k-r", "hi", session.SubmitIntentDefault, false)); err != nil {
			t.Fatal(err)
		}
		if m := e.meta(t); m["state"] != string(session.StateSuspended) {
			t.Fatalf("row = %v, want the hold left", m)
		}
	})
}

// A resume the row alone refuses (a suspended row a drain is stopping) is
// refused before the write, leaving the key free. One refused once the op
// is written cancels the op only when this request owns it: it created the
// op, or found it under a key it minted (a create that committed and
// answered an error); a cancel the lane host's take beat, or whose write
// failed, does not close it. A retry's op is never canceled.
func TestAcceptResumeRefusals(t *testing.T) {
	t.Run("before the write", func(t *testing.T) {
		e := newAcceptEnv(t)
		e.suspend(t, acceptT0.Add(-time.Minute))
		e.drain(t)
		_, err := e.accept(e.request("k-d", "hi", session.SubmitIntentDefault, true))
		var rerr *ResumeError
		if !errors.As(err, &rerr) || rerr.Op != nil || rerr.Session != e.id || !errors.Is(err, session.ErrSessionStopping) || len(e.ops(t)) != 0 {
			t.Fatalf("Accept = %v (%+v), ops %d; want refused before the write", err, rerr, len(e.ops(t)))
		}
	})
	for _, tc := range []struct {
		name string
		key  string
		// ambiguous answers the op's create an error after it commits.
		ambiguous, take, failCancel bool
		owned, canceled             bool
		status                      opjournal.Status
	}{
		{name: "created", key: "k-own", owned: true, canceled: true, status: opjournal.StatusClosed},
		{name: "a minted key's ambiguous create", ambiguous: true, owned: true, canceled: true, status: opjournal.StatusClosed},
		{name: "taken by the lane first", key: "k-own", take: true, owned: true, status: opjournal.StatusExecuting},
		{name: "a cancel whose write fails", key: "k-own", failCancel: true, owned: true, status: opjournal.StatusSubmitted},
	} {
		t.Run("after the write: "+tc.name, func(t *testing.T) {
			e := newAcceptEnv(t)
			e.suspend(t, acceptT0.Add(-time.Minute))
			e.store.set(func(s *hookStore) {
				s.ambiguous = tc.ambiguous
				if tc.failCancel {
					s.updateFails = 1
				}
				s.onOpCreate = func() {
					e.drain(t)
					if !tc.take {
						return
					}
					ops := e.ops(t)
					if _, err := e.st.Transition(ops[0].ID, opjournal.StatusSubmitted, opjournal.StatusExecuting, "lane:n1"); err != nil {
						t.Errorf("taking the op: %v", err)
					}
				}
			})
			_, err := e.accept(e.request(tc.key, "hi", session.SubmitIntentDefault, true))
			var rerr *ResumeError
			if !errors.As(err, &rerr) || rerr.Op == nil || rerr.Owned != tc.owned || rerr.Canceled() != tc.canceled || (tc.failCancel != (rerr.CancelErr != nil)) {
				t.Fatalf("Accept = %v (%+v); want owned %t, canceled %t", err, rerr, tc.owned, tc.canceled)
			}
			if tc.take && rerr.Cancel != opjournal.CancelRequested {
				t.Fatalf("cancel = %s, want requested", rerr.Cancel)
			}
			ops := e.ops(t)
			if len(ops) != 1 || ops[0].Status != tc.status || ops[0].ID != rerr.Op.ID {
				t.Fatalf("ops = %+v, want one %s", ops, tc.status)
			}
			if arm, _ := ops[0].Outcome.Effective(); tc.canceled && arm != opjournal.ArmCanceled {
				t.Fatalf("op = %+v, want it canceled", ops[0])
			}
		})
	}
	t.Run("a retry's op is not owned", func(t *testing.T) {
		e := newAcceptEnv(t)
		e.suspend(t, acceptT0.Add(-time.Minute))
		first := e.propose(t, opjournal.ExecutorLane, session.SubmitIntentDefault, "k-open", "hi", nil)
		e.drain(t)
		_, err := e.accept(e.request("k-open", "hi", session.SubmitIntentDefault, true))
		var rerr *ResumeError
		if !errors.As(err, &rerr) || rerr.Op == nil || rerr.Op.ID != first.ID || rerr.Owned || rerr.Canceled() || rerr.Cancel != "" {
			t.Fatalf("Accept = %v (%+v); want the open op refused, not owned or canceled", err, rerr)
		}
		if op, err := e.st.Get(first.ID); err != nil || op.Status != opjournal.StatusSubmitted {
			t.Fatalf("op = %+v, %v; want it left open", op, err)
		}
	})
}

// A supplied key may name an op already (the API's, whose answer was lost),
// so Accept reads before it creates: on a store that overwrites a duplicate
// pinned id, the closed op under the key is replayed, not reset (R1-5).
func TestAcceptReadsBeforeItCreatesUnderASuppliedKey(t *testing.T) {
	e := newAcceptEnv(t)
	e.store.set(func(s *hookStore) { s.upsert = true })
	done := e.propose(t, opjournal.ExecutorLane, session.SubmitIntentDefault, "k-api", "hi", &opjournal.Outcome{Arm: opjournal.ArmSucceeded})
	acc, err := e.accept(e.request("k-api", "hi", session.SubmitIntentDefault, true))
	if err != nil || acc.Owned || acc.Op.ID != done.ID || acc.Op.Status != opjournal.StatusClosed || acc.Op.Revision != done.Revision {
		t.Fatalf("Accept = %+v, %v; want the closed op %+v replayed", acc, err, done)
	}
	if ops := e.ops(t); len(ops) != 1 || ops[0].Revision != done.Revision {
		t.Fatalf("ops = %+v, want the op untouched", ops)
	}
}

// The policies a submit intent records its message op under: the lane's
// delivers default and follow_up at the boundary and interrupt_now as a
// deliver-now unit; the legacy one keeps follow_up after the turn and
// interrupt_now as an interrupt. Every one wakes if needed and is a prompt.
func TestPolicies(t *testing.T) {
	for _, tc := range []struct {
		intent       session.SubmitIntent
		lane, legacy opjournal.When
	}{
		{session.SubmitIntentDefault, opjournal.WhenBoundary, opjournal.WhenBoundary},
		{session.SubmitIntentFollowUp, opjournal.WhenBoundary, opjournal.WhenAfterTurn},
		{session.SubmitIntentInterruptNow, opjournal.WhenNow, opjournal.WhenInterrupt},
	} {
		for name, got := range map[string]opjournal.Policy{"lane": Policy(tc.intent), "legacy": LegacyPolicy(tc.intent)} {
			want := tc.lane
			if name == "legacy" {
				want = tc.legacy
			}
			if m := got.Message; m.When != want || m.Wake != opjournal.WakeIfNeeded || m.Role != opjournal.RolePrompt || got.Guarantee != "" || m.OnUnsupported != "" {
				t.Fatalf("%s policy of %s = %+v, want when %s", name, tc.intent, got, want)
			}
		}
	}
}
