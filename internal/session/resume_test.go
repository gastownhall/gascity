package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

var resumeNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

const resumeName = "s-resume"

// policyRow is an asleep row nothing holds; extra overrides it.
func policyRow(extra map[string]string) map[string]string {
	meta := map[string]string{
		"session_name":   resumeName,
		"template":       "worker",
		"work_dir":       "/tmp",
		"provider":       "claude",
		"generation":     "3",
		"instance_token": "tok-3",
		"state":          string(StateAsleep),
		"slept_at":       "2026-10-08T10:00:00Z",
	}
	maps.Copy(meta, extra)
	return meta
}

type policyEnv struct {
	store    beads.Store
	sp       *runtime.Fake
	mgr      *Manager
	id       string
	cityPath string
}

func newPolicyEnv(t *testing.T, meta map[string]string, live bool) *policyEnv {
	t.Helper()
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Title: "worker", Type: BeadType, Labels: []string{LabelSession}, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if live {
		if err := sp.Start(context.Background(), resumeName, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	cityPath := t.TempDir()
	mgr := newTestManager(t, store, sp, WithClock(&clock.Fake{Time: resumeNow}), WithCityPath(cityPath))
	return &policyEnv{store: store, sp: sp, mgr: mgr, id: b.ID, cityPath: cityPath}
}

// queued is the messages waiting in the city's queue for the row.
func (e *policyEnv) queued(t *testing.T) []string {
	t.Helper()
	state, err := nudgequeue.LoadState(e.cityPath)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, item := range state.Pending {
		if item.SessionID == e.id {
			msgs = append(msgs, item.Message)
		}
	}
	return msgs
}

func (e *policyEnv) row(t *testing.T) map[string]string {
	t.Helper()
	b, err := e.store.Get(e.id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata
}

// TestBackgroundSendToHeldRowQueues is D8 rule 1 (CONTRACT v5.9): a send
// that is not the operator's own never consumes an operator's hold. On a
// suspended, user-held, quarantined or wait-held row it queues the message,
// starts nothing and writes nothing. A row nothing holds, an expired hold,
// and legacy's own idle-stop-pending intent on a live row are delivered
// (the last live, without a restart). An operator's send resumes a held
// row. Kills a background send that resumes a held row, a hold read from
// legacy's own sleep intent, and an operator policy that queues.
func TestBackgroundSendToHeldRowQueues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		meta   map[string]string
		live   bool
		kind   ActorKind // zero: ActorAgent, which resumes only an unheld row
		queued bool
	}{
		{name: "suspended", meta: map[string]string{"state": string(StateSuspended), "suspended_at": "2026-10-08T10:00:00Z"}, queued: true},
		{name: "managed suspend, runtime still live", meta: map[string]string{"state": string(StateSuspended), "sleep_intent": "user-hold", "held_until": "2099-01-01T00:00:00Z"}, live: true, queued: true},
		{name: "user hold", meta: map[string]string{"held_until": "2099-01-01T00:00:00Z", "sleep_reason": "user-hold"}, queued: true},
		{name: "quarantine", meta: map[string]string{"quarantined_until": "2099-01-01T00:00:00Z"}, queued: true},
		{name: "wait hold", meta: map[string]string{"wait_hold": "true", "sleep_intent": "wait-hold"}, queued: true},
		{name: "wait hold without its intent", meta: map[string]string{"wait_hold": "true"}, queued: true},
		{name: "expired hold resumes", meta: map[string]string{"held_until": "2026-10-01T00:00:00Z"}},
		{name: "unheld asleep resumes", meta: nil},
		{name: "stale wait-hold intent without the hold resumes", meta: map[string]string{"sleep_intent": "wait-hold"}},
		{name: "idle drain on a live row is delivered live", meta: map[string]string{"state": string(StateActive), "slept_at": "", "sleep_intent": "idle-stop-pending"}, live: true},
		{name: "heartbeat hold on a live row is delivered live", meta: map[string]string{"state": string(StateActive), "slept_at": "", "held_until": "2099-01-01T00:00:00Z"}, live: true},
		{name: "unparseable timer blocks the start", meta: map[string]string{"held_until": "soon"}, queued: true},
		{name: "unparseable timer on a live row is delivered live", meta: map[string]string{"state": string(StateActive), "slept_at": "", "held_until": "soon"}, live: true},
		{name: "creating, quarantined, runtime dead", meta: map[string]string{"state": string(StateCreating), "quarantined_until": "2099-01-01T00:00:00Z"}, queued: true},
		{name: "operator resumes a held row", meta: map[string]string{"state": string(StateSuspended)}, kind: ActorOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPolicyEnv(t, policyRow(tc.meta), tc.live)
			before := e.row(t)
			startsBefore := e.sp.CountCalls("Start", resumeName)
			kind := tc.kind
			if kind == 0 {
				kind = ActorAgent
			}
			out, err := e.mgr.Send(context.Background(), testActor(e.mgr, kind), e.id, "hello", "claude --resume k", runtime.Config{})
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if out.Queued != tc.queued {
				t.Fatalf("queued = %v, want %v", out.Queued, tc.queued)
			}
			starts := e.sp.CountCalls("Start", resumeName) - startsBefore
			if tc.queued {
				if starts != 0 {
					t.Fatalf("Start calls = %d, want none", starts)
				}
				if got := e.row(t); !maps.Equal(got, before) {
					t.Fatalf("held row written:\n got %v\nwant %v", got, before)
				}
				if got := e.queued(t); len(got) != 1 || got[0] != "hello" {
					t.Fatalf("queue = %q, want the message", got)
				}
				return
			}
			if tc.live && starts != 0 {
				t.Fatalf("Start calls = %d on a live row, want delivery to the live runtime", starts)
			}
			if e.sp.CountCalls("Nudge", resumeName) == 0 {
				t.Fatal("message not delivered")
			}
		})
	}
}

// TestWaitIdleNudgeToHeldRowIsUndelivered: a background wait-idle nudge to a
// held row reports ErrResumeHeld, undelivered, without a start or a queue
// entry, so its caller queues it and says the session is held. Kills a held
// refusal reported as a plain miss (no held note) or as delivered.
func TestWaitIdleNudgeToHeldRowIsUndelivered(t *testing.T) {
	e := newPolicyEnv(t, policyRow(map[string]string{"state": string(StateSuspended)}), false)
	delivered, err := e.mgr.TryWaitIdleNudge(context.Background(), testActor(e.mgr, ActorAgent), e.id, "mail", "hello", "claude --resume k", runtime.Config{})
	if !errors.Is(err, ErrResumeHeld) || delivered || e.sp.CountCalls("Start", resumeName) != 0 || len(e.queued(t)) != 0 {
		t.Fatalf("TryWaitIdleNudge = %v, %v (starts %d, queue %q); want ErrResumeHeld, no start, nothing queued", delivered, err, e.sp.CountCalls("Start", resumeName), e.queued(t))
	}
}

// TestInterruptSubmitToHeldLiveRowQueues: a background interrupt_now on a
// held row whose runtime is still live (a managed suspend the controller has
// not acted on) queues before touching the runtime. Kills a policy check
// that runs only after the interrupt's hard restart, and a held branch that
// reports queued without enqueueing (MA-2).
func TestInterruptSubmitToHeldLiveRowQueues(t *testing.T) {
	e := newPolicyEnv(t, policyRow(map[string]string{"state": string(StateSuspended), "sleep_intent": "user-hold", "provider": "pi"}), true)
	out, err := e.mgr.Submit(context.Background(), testActor(e.mgr, ActorAgent), e.id, "now", "pi --resume k", runtime.Config{}, SubmitIntentInterruptNow)
	if err != nil || !out.Queued {
		t.Fatalf("Submit = %+v, %v; want queued", out, err)
	}
	if got := e.queued(t); len(got) != 1 || got[0] != "now" {
		t.Fatalf("queue = %q, want the message", got)
	}
	if !e.sp.IsRunning(resumeName) || e.sp.CountCalls("Stop", resumeName) != 0 {
		t.Fatal("the held row's live runtime was stopped")
	}
}

// TestRequestWakeUnlessHeld: a background wake records an explicit wake by
// CAS only on an asleep or drained row nothing holds; a held row and a row
// that is starting or running are left untouched and say so, and a CAS lost
// on every attempt is a retryable error. Kills a wake written over a hold,
// one written to a creating row, and exhaustion read as success.
func TestRequestWakeUnlessHeld(t *testing.T) {
	for name, tc := range map[string]struct {
		meta map[string]string
		want WakeRequestOutcome
	}{
		"unheld asleep":     {want: WakeRecorded},
		"drained":           {meta: map[string]string{"state": string(StateDrained)}, want: WakeRecorded},
		"held":              {meta: map[string]string{"state": string(StateSuspended)}, want: WakeHeld},
		"unparseable timer": {meta: map[string]string{"quarantined_until": "x"}, want: WakeHeld},
		"creating":          {meta: map[string]string{"state": string(StateCreating)}, want: WakeNotDormant},
	} {
		t.Run(name, func(t *testing.T) {
			e := newPolicyEnv(t, policyRow(tc.meta), false)
			got, err := NewStore(beads.SessionStore{Store: e.store}).RequestWakeUnlessHeld(e.id, false, resumeNow)
			if err != nil || got != tc.want {
				t.Fatalf("RequestWakeUnlessHeld = %v, %v; want %v", got, err, tc.want)
			}
			if written := e.row(t)["wake_request"] != ""; written != (tc.want == WakeRecorded) {
				t.Fatalf("wake_request written = %v, want %v", written, tc.want == WakeRecorded)
			}
		})
	}
	t.Run("contended", func(t *testing.T) {
		stamped := stampedMemStore(t, gate.Auto, beads.NewMemStore())
		b, err := stamped.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: policyRow(nil)})
		if err != nil {
			t.Fatal(err)
		}
		loser := &wakeCASLoser{rowWriteRecorder: &rowWriteRecorder{Store: stamped}, backing: stamped}
		if _, err := NewStore(beads.SessionStore{Store: loser}).RequestWakeUnlessHeld(b.ID, false, resumeNow); !errors.Is(err, ErrWakeRequestContended) {
			t.Fatalf("RequestWakeUnlessHeld = %v, want ErrWakeRequestContended", err)
		}
	})
}

// TestExplicitWakeUnserved pins the single statement of the pending-explicit-
// wake rule: only an explicit request counts, and it is served only when both
// timestamps parse and the request is strictly before the sleep. Kills a
// garbled or missing timestamp read as served (a lost wake) and an equal
// pair read as served.
func TestExplicitWakeUnserved(t *testing.T) {
	const (
		early = "2026-10-08T10:00:00Z"
		late  = "2026-10-08T11:00:00Z"
	)
	explicit := string(WakeCauseExplicit)
	for name, tc := range map[string]struct {
		meta map[string]string
		want bool
	}{
		"no request":              {meta: map[string]string{"slept_at": early}, want: false},
		"non-explicit request":    {meta: map[string]string{"wake_request": "pending-create", "wake_requested_at": late, "slept_at": early}, want: false},
		"explicit after sleep":    {meta: map[string]string{"wake_request": explicit, "wake_requested_at": late, "slept_at": early}, want: true},
		"explicit padded":         {meta: map[string]string{"wake_request": " " + explicit + " ", "wake_requested_at": " " + late + " ", "slept_at": early}, want: true},
		"served by a later sleep": {meta: map[string]string{"wake_request": explicit, "wake_requested_at": early, "slept_at": late}, want: false},
		"equal timestamps":        {meta: map[string]string{"wake_request": explicit, "wake_requested_at": early, "slept_at": early}, want: true},
		"missing requested_at":    {meta: map[string]string{"wake_request": explicit, "slept_at": late}, want: true},
		"missing slept_at":        {meta: map[string]string{"wake_request": explicit, "wake_requested_at": early}, want: true},
		"garbled requested_at":    {meta: map[string]string{"wake_request": explicit, "wake_requested_at": "yesterday", "slept_at": late}, want: true},
		"garbled slept_at":        {meta: map[string]string{"wake_request": explicit, "wake_requested_at": early, "slept_at": "x"}, want: true},
		"nil metadata":            {meta: nil, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ExplicitWakeUnserved(infoFromPersistedBead(beads.Bead{Metadata: tc.meta})); got != tc.want {
				t.Fatalf("ExplicitWakeUnserved(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
}

// coalesceRow creates meta as a session row in a revision-stamped store with
// conditional writes, so every write bumps the row's revision.
func coalesceRow(t *testing.T, meta map[string]string) (beads.Store, string) {
	t.Helper()
	stamped := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	b, err := stamped.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	return stamped, b.ID
}

func rowRevision(t *testing.T, store beads.Store, id string) (int64, map[string]string) {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Revision, b.Metadata
}

// TestRequestWakeCoalescesOntoUnservedWake: a wake request on a dormant row
// that already carries an unserved explicit wake writes nothing and reports
// WakeRecorded; once a later sleep serves that request, the next one writes
// again. Kills a rewrite on every call (fan-in contention) and a coalesce
// onto a served request (a lost wake).
func TestRequestWakeCoalescesOntoUnservedWake(t *testing.T) {
	for name, state := range map[string]State{"asleep": StateAsleep, "drained": StateDrained} {
		t.Run(name, func(t *testing.T) {
			store, id := coalesceRow(t, policyRow(map[string]string{"state": string(state)}))
			ss := NewStore(beads.SessionStore{Store: store})
			rev0, _ := rowRevision(t, store, id)

			if got, err := ss.RequestWakeUnlessHeld(id, false, resumeNow); err != nil || got != WakeRecorded {
				t.Fatalf("first RequestWakeUnlessHeld = %v, %v; want WakeRecorded", got, err)
			}
			rev1, meta := rowRevision(t, store, id)
			if rev1 == rev0 || meta["wake_request"] != string(WakeCauseExplicit) || meta["wake_requested_at"] != resumeNow.Format(time.RFC3339) {
				t.Fatalf("first call: revision %d -> %d, meta %v; want a written explicit wake", rev0, rev1, meta)
			}

			later := resumeNow.Add(time.Minute)
			if got, err := ss.RequestWakeUnlessHeld(id, false, later); err != nil || got != WakeRecorded {
				t.Fatalf("second RequestWakeUnlessHeld = %v, %v; want WakeRecorded", got, err)
			}
			rev2, meta := rowRevision(t, store, id)
			if rev2 != rev1 || meta["wake_requested_at"] != resumeNow.Format(time.RFC3339) {
				t.Fatalf("second call: revision %d -> %d, wake_requested_at %q; want no write", rev1, rev2, meta["wake_requested_at"])
			}

			// The reconciler served the request: the session woke and slept again.
			if err := store.SetMetadataBatch(id, map[string]string{"slept_at": later.Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
			rev3, _ := rowRevision(t, store, id)
			third := later.Add(time.Minute)
			if got, err := ss.RequestWakeUnlessHeld(id, false, third); err != nil || got != WakeRecorded {
				t.Fatalf("third RequestWakeUnlessHeld = %v, %v; want WakeRecorded", got, err)
			}
			rev4, meta := rowRevision(t, store, id)
			if rev4 == rev3 || meta["wake_requested_at"] != third.Format(time.RFC3339) {
				t.Fatalf("after a served request: revision %d -> %d, wake_requested_at %q; want a fresh write", rev3, rev4, meta["wake_requested_at"])
			}
		})
	}
}

// TestRequestWakeNonDormantOutcomesUnchanged: a held row still reports
// WakeHeld, a running row WakeNotDormant and a closed row *WakeConflictError,
// all without a write, even when an unserved explicit wake is already
// present. Kills the coalesce check shadowing the conflict, hold and dormancy
// checks.
func TestRequestWakeNonDormantOutcomesUnchanged(t *testing.T) {
	pending := map[string]string{"wake_request": string(WakeCauseExplicit), "wake_requested_at": "2026-10-08T11:00:00Z"}
	for name, tc := range map[string]struct {
		meta     map[string]string
		running  bool
		close    bool
		want     WakeRequestOutcome
		conflict bool
	}{
		"held":            {meta: map[string]string{"state": string(StateSuspended)}, want: WakeHeld},
		"held pending":    {meta: map[string]string{"state": string(StateSuspended), "wake_request": pending["wake_request"], "wake_requested_at": pending["wake_requested_at"]}, want: WakeHeld},
		"running":         {meta: map[string]string{"state": string(StateActive)}, running: true, want: WakeNotDormant},
		"running pending": {meta: map[string]string{"state": string(StateActive), "wake_request": pending["wake_request"], "wake_requested_at": pending["wake_requested_at"]}, running: true, want: WakeNotDormant},
		"closed":          {close: true, conflict: true},
		"closed pending":  {meta: pending, close: true, conflict: true},
	} {
		t.Run(name, func(t *testing.T) {
			store, id := coalesceRow(t, policyRow(tc.meta))
			if tc.close {
				if err := store.Close(id); err != nil {
					t.Fatal(err)
				}
			}
			rev0, _ := rowRevision(t, store, id)
			got, err := NewStore(beads.SessionStore{Store: store}).RequestWakeUnlessHeld(id, tc.running, resumeNow)
			var conflict *WakeConflictError
			switch {
			case tc.conflict && !errors.As(err, &conflict):
				t.Fatalf("RequestWakeUnlessHeld = %v, %v; want *WakeConflictError", got, err)
			case !tc.conflict && (err != nil || got != tc.want):
				t.Fatalf("RequestWakeUnlessHeld = %v, %v; want %v", got, err, tc.want)
			}
			if rev1, _ := rowRevision(t, store, id); rev1 != rev0 {
				t.Fatalf("revision %d -> %d; want no write", rev0, rev1)
			}
		})
	}
}

// TestWakeRequestVerdictAgreesWithRequestWakeUnlessHeld: WakeRequestVerdict
// answers, on a read and with no write, what RequestWakeUnlessHeld answers
// for the same row and runtime: WakeRecorded for a dormant row nothing holds
// (a pending wake included), WakeHeld for a held one, WakeNotDormant for a
// row that is starting or runs, and the wake's *WakeConflictError for a
// closed one. A heartbeat holds a dormant row but not one whose runtime
// runs. Kills a verdict that misreads a hold, dormancy or the runtime, and
// one that writes.
func TestWakeRequestVerdictAgreesWithRequestWakeUnlessHeld(t *testing.T) {
	heartbeat := map[string]string{"state": string(StateActive), "held_until": "2026-10-08T13:00:00Z", "slept_at": ""}
	for name, tc := range map[string]struct {
		meta     map[string]string
		running  bool
		closed   bool
		want     WakeRequestOutcome
		conflict bool
	}{
		"unheld asleep":       {want: WakeRecorded},
		"drained":             {meta: map[string]string{"state": string(StateDrained)}, want: WakeRecorded},
		"pending wake":        {meta: map[string]string{"wake_request": string(WakeCauseExplicit), "wake_requested_at": "2026-10-08T11:00:00Z"}, want: WakeRecorded},
		"operator suspend":    {meta: operatorSuspendedRow(nil), want: WakeHeld},
		"soft suspend":        {meta: map[string]string{"state": string(StateSuspended)}, want: WakeHeld},
		"wait hold":           {meta: map[string]string{"wait_hold": "true", "sleep_intent": string(SleepReasonWaitHold)}, want: WakeHeld},
		"unparseable timer":   {meta: map[string]string{"quarantined_until": "x"}, want: WakeHeld},
		"creating":            {meta: map[string]string{"state": string(StateCreating)}, want: WakeNotDormant},
		"running":             {meta: map[string]string{"state": string(StateActive), "slept_at": ""}, running: true, want: WakeNotDormant},
		"heartbeat, running":  {meta: heartbeat, running: true, want: WakeNotDormant},
		"heartbeat, stopped":  {meta: heartbeat, want: WakeHeld},
		"running, suspending": {meta: operatorSuspendedRow(nil), running: true, want: WakeHeld},
		"closed":              {closed: true, conflict: true},
	} {
		t.Run(name, func(t *testing.T) {
			store, id := coalesceRow(t, policyRow(tc.meta))
			if tc.closed {
				if err := store.Close(id); err != nil {
					t.Fatal(err)
				}
			}
			ss := NewStore(beads.SessionStore{Store: store})
			rev0, _ := rowRevision(t, store, id)
			info, persisted, err := ss.GetPersistedResponse(id)
			if err != nil {
				t.Fatal(err)
			}
			got, err := WakeRequestVerdict(info, persisted, tc.running, resumeNow)
			if rev1, meta := rowRevision(t, store, id); rev1 != rev0 {
				t.Fatalf("revision %d -> %d: WakeRequestVerdict wrote (row %v)", rev0, rev1, meta)
			}
			requested, reqErr := ss.RequestWakeUnlessHeld(id, tc.running, resumeNow)
			var conflict *WakeConflictError
			switch {
			case tc.conflict && (!errors.As(err, &conflict) || !errors.As(reqErr, &conflict)):
				t.Fatalf("WakeRequestVerdict = %v, %v and RequestWakeUnlessHeld = %v, %v; want *WakeConflictError from both", got, err, requested, reqErr)
			case tc.conflict:
			case err != nil || got != tc.want:
				t.Fatalf("WakeRequestVerdict = %v, %v; want %v", got, err, tc.want)
			case reqErr != nil || requested != got:
				t.Fatalf("RequestWakeUnlessHeld = %v, %v where WakeRequestVerdict answered %v", requested, reqErr, got)
			}
		})
	}
}

// TestRequestWakeFanInWritesOnce: 64 concurrent wake requests on one dormant
// row produce exactly one write and no ErrWakeRequestContended; every caller
// sees WakeRecorded. Kills fan-in CAS contention (decentralized-at-scale E14).
func TestRequestWakeFanInWritesOnce(t *testing.T) {
	store, id := coalesceRow(t, policyRow(nil))
	ss := NewStore(beads.SessionStore{Store: store})
	rev0, _ := rowRevision(t, store, id)
	const callers = 64
	errs := make([]error, callers)
	outcomes := make([]WakeRequestOutcome, callers)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range callers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			outcomes[i], errs[i] = ss.RequestWakeUnlessHeld(id, false, resumeNow)
		}()
	}
	start.Done()
	done.Wait()
	for i := range callers {
		if errs[i] != nil || outcomes[i] != WakeRecorded {
			t.Fatalf("caller %d: RequestWakeUnlessHeld = %v, %v; want WakeRecorded", i, outcomes[i], errs[i])
		}
	}
	rev1, meta := rowRevision(t, store, id)
	if rev1-rev0 != 1 || meta["wake_request"] != string(WakeCauseExplicit) {
		t.Fatalf("revision %d -> %d (wake_request %q); want exactly one write", rev0, rev1, meta["wake_request"])
	}
}

// heldUntil is an operator suspend's indefinite held_until.
const heldUntil = "2099-01-01T00:00:00Z"

// operatorSuspendedRow is an operator's suspend on a dormant row, with extra
// over it.
func operatorSuspendedRow(extra map[string]string) map[string]string {
	meta := map[string]string{"state": string(StateSuspended), "sleep_intent": "user-hold", "held_until": heldUntil, "suspended_at": "2026-10-08T09:00:00Z"}
	maps.Copy(meta, extra)
	return meta
}

// TestResumeHeld: an operator's resume that starts nothing consumes the
// operator's suspend alone, moves a suspended row asleep and, in the same
// write, records the explicit wake that row now needs, onto which the lane
// host's wake request that follows coalesces without a write; a wait's hold
// and a quarantine stand, and a row nothing suspends is not written. Kills a
// resume that clears every blocker (the wake's), one that leaves a
// suspended row suspended (the wake request then reads it not dormant), one
// that consumes the suspend without the wake (a controller pass then reads
// the row unheld and unwoken, which closes a pool row), one that records a
// wake on a row something else still holds, and a write on a row it has
// nothing to consume on.
func TestResumeHeld(t *testing.T) {
	slept := resumeNow.Format(time.RFC3339)
	for name, tc := range map[string]struct {
		meta    map[string]string
		written bool
		// want is what the row reads after, key by key.
		want map[string]string
		// woke is whether the resume recorded the explicit wake itself.
		woke bool
		// wake is the answer of the wake request that follows.
		wake WakeRequestOutcome
	}{
		"operator suspend": {
			meta: operatorSuspendedRow(nil), written: true,
			want: map[string]string{"state": string(StateAsleep), "slept_at": slept, "sleep_intent": "", "held_until": "", "suspended_at": ""},
			woke: true, wake: WakeRecorded,
		},
		"operator suspend with its hold reason": {
			meta: operatorSuspendedRow(map[string]string{"sleep_reason": string(SleepReasonUserHold)}), written: true,
			want: map[string]string{"state": string(StateAsleep), "sleep_reason": ""},
			woke: true, wake: WakeRecorded,
		},
		"soft suspend": {
			meta: map[string]string{"state": string(StateSuspended)}, written: true,
			want: map[string]string{"state": string(StateAsleep), "slept_at": slept},
			woke: true, wake: WakeRecorded,
		},
		"operator suspend over a wait keeps the wait's hold": {
			meta: operatorSuspendedRow(map[string]string{"wait_hold": "true"}), written: true,
			want: map[string]string{"state": string(StateAsleep), "sleep_intent": "", "held_until": "", "wait_hold": "true"},
			wake: WakeHeld,
		},
		"wait hold": {
			meta: map[string]string{"wait_hold": "true", "sleep_intent": string(SleepReasonWaitHold)},
			want: map[string]string{"wait_hold": "true", "sleep_intent": string(SleepReasonWaitHold)},
			wake: WakeHeld,
		},
		"quarantine": {
			meta: map[string]string{"quarantined_until": heldUntil},
			want: map[string]string{"quarantined_until": heldUntil},
			wake: WakeHeld,
		},
		"unheld asleep": {wake: WakeRecorded},
		"running": {
			meta: map[string]string{"state": string(StateActive), "slept_at": ""},
			want: map[string]string{"state": string(StateActive)},
			wake: WakeNotDormant,
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, id := coalesceRow(t, policyRow(tc.meta))
			ss := NewStore(beads.SessionStore{Store: store})
			rev0, _ := rowRevision(t, store, id)
			if err := ss.ResumeHeld(id, resumeNow, resumeNow); err != nil {
				t.Fatalf("ResumeHeld: %v", err)
			}
			rev1, meta := rowRevision(t, store, id)
			if written := rev1 != rev0; written != tc.written {
				t.Fatalf("revision %d -> %d: written %v, want %v (row %v)", rev0, rev1, written, tc.written, meta)
			}
			for k, v := range tc.want {
				if meta[k] != v {
					t.Fatalf("%s = %q, want %q (row %v)", k, meta[k], v, meta)
				}
			}
			wantWake, wantAt := "", ""
			if tc.woke {
				wantWake, wantAt = string(WakeCauseExplicit), slept
			}
			if meta["wake_request"] != wantWake || meta["wake_requested_at"] != wantAt {
				t.Fatalf("wake_request %q at %q, want %q at %q (row %v)", meta["wake_request"], meta["wake_requested_at"], wantWake, wantAt, meta)
			}
			if got, err := ss.RequestWakeUnlessHeld(id, false, resumeNow.Add(time.Second)); err != nil || got != tc.wake {
				t.Fatalf("RequestWakeUnlessHeld after ResumeHeld = %v, %v; want %v", got, err, tc.wake)
			}
			if rev2, after := rowRevision(t, store, id); tc.woke && rev2 != rev1 {
				t.Fatalf("revision %d -> %d: the wake request wrote over the resume's wake (row %v)", rev1, rev2, after)
			}
		})
	}
}

// TestResumeHeldRefusesWithoutWriting: a closed row is the wake's
// *WakeConflictError, a suspended row a drain is stopping wraps
// ErrSessionStopping, and a CAS lost on every attempt wraps
// ErrWakeRequestContended, none with a write. Kills a resume that writes
// over a closed row, one that takes a draining row's runtime from its stop,
// and exhaustion read as success.
func TestResumeHeldRefusesWithoutWriting(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		store, id := coalesceRow(t, policyRow(operatorSuspendedRow(nil)))
		if err := store.Close(id); err != nil {
			t.Fatal(err)
		}
		rev0, _ := rowRevision(t, store, id)
		var conflict *WakeConflictError
		if err := NewStore(beads.SessionStore{Store: store}).ResumeHeld(id, resumeNow, resumeNow); !errors.As(err, &conflict) || conflict.State != "closed" {
			t.Fatalf("ResumeHeld = %v, want *WakeConflictError{closed}", err)
		}
		if rev1, _ := rowRevision(t, store, id); rev1 != rev0 {
			t.Fatalf("revision %d -> %d; want no write", rev0, rev1)
		}
	})
	t.Run("draining", func(t *testing.T) {
		store, id := coalesceRow(t, policyRow(operatorSuspendedRow(map[string]string{"state": string(StateDraining)})))
		rev0, _ := rowRevision(t, store, id)
		if err := NewStore(beads.SessionStore{Store: store}).ResumeHeld(id, resumeNow, resumeNow); !errors.Is(err, ErrSessionStopping) {
			t.Fatalf("ResumeHeld = %v, want ErrSessionStopping", err)
		}
		if rev1, _ := rowRevision(t, store, id); rev1 != rev0 {
			t.Fatalf("revision %d -> %d; want no write", rev0, rev1)
		}
	})
	t.Run("contended", func(t *testing.T) {
		stamped := stampedMemStore(t, gate.Auto, beads.NewMemStore())
		b, err := stamped.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: policyRow(operatorSuspendedRow(nil))})
		if err != nil {
			t.Fatal(err)
		}
		loser := &wakeCASLoser{rowWriteRecorder: &rowWriteRecorder{Store: stamped}, backing: stamped}
		if err := NewStore(beads.SessionStore{Store: loser}).ResumeHeld(b.ID, resumeNow, resumeNow); !errors.Is(err, ErrWakeRequestContended) {
			t.Fatalf("ResumeHeld = %v, want ErrWakeRequestContended", err)
		}
	})
}

// TestResumeHeldRefusalAgreesWithResumeHeld: ResumeHeldRefusal answers, on a
// read and with no write, the refusal ResumeHeld answers for the same row:
// none for a row the resume consumes a hold on or has nothing to consume on,
// the wake's *WakeConflictError for a closed row, and ErrSessionStopping for
// a suspended row a drain is stopping; a row it cannot read is the read's
// error. Kills a check that misses either refusal, one that refuses a row
// the resume consumes, and one that writes.
func TestResumeHeldRefusalAgreesWithResumeHeld(t *testing.T) {
	for name, tc := range map[string]struct {
		meta   map[string]string
		closed bool
		want   string // resumeRefusalClass of both answers
	}{
		"operator suspend":           {meta: operatorSuspendedRow(nil)},
		"soft suspend":               {meta: map[string]string{"state": string(StateSuspended)}},
		"wait hold":                  {meta: map[string]string{"wait_hold": "true", "sleep_intent": string(SleepReasonWaitHold)}},
		"unheld asleep":              {},
		"running":                    {meta: map[string]string{"state": string(StateActive), "slept_at": ""}},
		"closed":                     {meta: operatorSuspendedRow(nil), closed: true, want: "conflict"},
		"operator suspend, draining": {meta: operatorSuspendedRow(map[string]string{"state": string(StateDraining)}), want: "stopping"},
	} {
		t.Run(name, func(t *testing.T) {
			store, id := coalesceRow(t, policyRow(tc.meta))
			if tc.closed {
				if err := store.Close(id); err != nil {
					t.Fatal(err)
				}
			}
			ss := NewStore(beads.SessionStore{Store: store})
			rev0, _ := rowRevision(t, store, id)
			checked := ss.ResumeHeldRefusal(id, resumeNow)
			if rev1, meta := rowRevision(t, store, id); rev1 != rev0 {
				t.Fatalf("revision %d -> %d: ResumeHeldRefusal wrote (row %v)", rev0, rev1, meta)
			}
			if got := resumeRefusalClass(t, checked); got != tc.want {
				t.Fatalf("ResumeHeldRefusal = %v (%q), want %q", checked, got, tc.want)
			}
			if got := resumeRefusalClass(t, ss.ResumeHeld(id, resumeNow, resumeNow)); got != tc.want {
				t.Fatalf("ResumeHeld refused %q where ResumeHeldRefusal answered %q", got, tc.want)
			}
		})
	}
	store, _ := coalesceRow(t, policyRow(nil))
	if err := NewStore(beads.SessionStore{Store: store}).ResumeHeldRefusal("gc-missing", resumeNow); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("ResumeHeldRefusal of a missing row = %v, want beads.ErrNotFound", err)
	}
}

// resumeRefusalClass classifies a resume's answer: "" no refusal,
// "conflict" a *WakeConflictError, "stopping" ErrSessionStopping. Any other
// error fails t.
func resumeRefusalClass(t *testing.T, err error) string {
	t.Helper()
	var conflict *WakeConflictError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &conflict):
		return "conflict"
	case errors.Is(err, ErrSessionStopping):
		return "stopping"
	}
	t.Fatalf("resume answered an unclassified error: %v", err)
	return ""
}

// TestResumeHeldPreviewAgreesWithResumeHeld: ResumeHeldPreview, on one read
// and with no write, is the row ResumeHeld then writes: the suspend
// consumed and the wake recorded, a wait's hold or a quarantine left
// standing, a later suspend left alone, a row with nothing to consume
// unchanged, and ResumeHeld's refusal for a closed row and for one a drain
// is stopping. Kills a preview that writes, one that lifts a hold the
// resume leaves (so a client is told a resend with resume: true lifts it),
// and one that keeps a suspend the resume consumes.
func TestResumeHeldPreviewAgreesWithResumeHeld(t *testing.T) {
	later := resumeNow.Add(-time.Minute)
	for name, tc := range map[string]struct {
		meta     map[string]string
		notAfter time.Time
		closed   bool
		refusal  string // resumeRefusalClass of both answers
	}{
		"operator suspend":              {meta: operatorSuspendedRow(nil)},
		"soft suspend":                  {meta: map[string]string{"state": string(StateSuspended)}},
		"operator suspend over a wait":  {meta: operatorSuspendedRow(map[string]string{"wait_hold": "true"})},
		"operator suspend, quarantined": {meta: operatorSuspendedRow(map[string]string{"quarantined_until": heldUntil})},
		"suspend stamped after":         {meta: operatorSuspendedRow(map[string]string{"suspended_at": resumeNow.Format(time.RFC3339)}), notAfter: later},
		"wait hold":                     {meta: map[string]string{"wait_hold": "true", "sleep_intent": string(SleepReasonWaitHold)}},
		"quarantine":                    {meta: map[string]string{"quarantined_until": heldUntil}},
		"unheld asleep":                 {},
		"running":                       {meta: map[string]string{"state": string(StateActive), "slept_at": ""}},
		"closed":                        {meta: operatorSuspendedRow(nil), closed: true, refusal: "conflict"},
		"operator suspend, draining":    {meta: operatorSuspendedRow(map[string]string{"state": string(StateDraining)}), refusal: "stopping"},
	} {
		t.Run(name, func(t *testing.T) {
			store, id := coalesceRow(t, policyRow(tc.meta))
			if tc.closed {
				if err := store.Close(id); err != nil {
					t.Fatal(err)
				}
			}
			notAfter := tc.notAfter
			if notAfter.IsZero() {
				notAfter = resumeNow
			}
			ss := NewStore(beads.SessionStore{Store: store})
			info, persisted, err := ss.GetPersistedResponse(id)
			if err != nil {
				t.Fatal(err)
			}
			rev0, before := rowRevision(t, store, id)
			moved, after, previewErr := ResumeHeldPreview(info, persisted, notAfter, resumeNow)
			if rev1, meta := rowRevision(t, store, id); rev1 != rev0 {
				t.Fatalf("revision %d -> %d: ResumeHeldPreview wrote (row %v)", rev0, rev1, meta)
			}
			if got := resumeRefusalClass(t, previewErr); got != tc.refusal {
				t.Fatalf("ResumeHeldPreview refused %q (%v), want %q", got, previewErr, tc.refusal)
			}
			resumeErr := ss.ResumeHeld(id, notAfter, resumeNow)
			if got := resumeRefusalClass(t, resumeErr); got != tc.refusal {
				t.Fatalf("ResumeHeld refused %q (%v) where ResumeHeldPreview refused %q", got, resumeErr, tc.refusal)
			}
			_, written := rowRevision(t, store, id)
			if tc.refusal != "" {
				if !reflect.DeepEqual(after.Metadata, before) || !reflect.DeepEqual(moved, info) {
					t.Fatalf("refused preview = %v; want the read unchanged, %v", after.Metadata, before)
				}
				return
			}
			if !reflect.DeepEqual(after.Metadata, written) {
				t.Fatalf("ResumeHeldPreview = %v; ResumeHeld wrote %v", after.Metadata, written)
			}
			if moved.MetadataState != written["state"] || moved.WakeRequest != written["wake_request"] || moved.SuspendedAt != written["suspended_at"] {
				t.Fatalf("preview's info = state %q, wake %q, suspended_at %q; ResumeHeld wrote %v", moved.MetadataState, moved.WakeRequest, moved.SuspendedAt, written)
			}
		})
	}
}

// TestResumeHeldLeavesWaitsOpen: the resume cancels none of the row's
// durable waits, unlike an operator's wake. Kills a resume that reuses
// WakeSession, whose wake cancels every open wait.
func TestResumeHeldLeavesWaitsOpen(t *testing.T) {
	store, id := coalesceRow(t, policyRow(operatorSuspendedRow(map[string]string{"wait_hold": "true"})))
	ss := NewStore(beads.SessionStore{Store: store})
	wait, err := ss.CreateWait(WaitSpec{SessionID: id, Kind: "deps", DepIDs: []string{"gc-dep"}, DepMode: "all", Now: resumeNow})
	if err != nil {
		t.Fatalf("CreateWait: %v", err)
	}
	if err := ss.ResumeHeld(id, resumeNow, resumeNow); err != nil {
		t.Fatalf("ResumeHeld: %v", err)
	}
	got, err := ss.GetWait(wait.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "closed" || got.State != waitStatePending {
		t.Fatalf("wait after ResumeHeld = %s/%s, want it open and pending", got.Status, got.State)
	}
}

// TestResumeHeldLeavesASuspendStampedAfterItsMessage: the resume consumes
// only a suspend stamped at or before its message's proposal (notAfter). A
// suspend stamped later stands, with nothing written and nothing refused,
// even on a row a drain is stopping. One stamped at notAfter, in its second
// (suspended_at is whole seconds), or with no readable stamp is consumed.
// Kills a resume that ignores the bound (a same-key retry of a waiting
// message consumes the suspend the operator placed after it), one that
// refuses for a suspend it leaves standing, and a bound that misses a
// suspend in force when the message was proposed.
func TestResumeHeldLeavesASuspendStampedAfterItsMessage(t *testing.T) {
	const stamp = "2026-10-08T11:00:00Z"
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	before := at.Add(-time.Second)
	for name, tc := range map[string]struct {
		meta     map[string]string
		notAfter time.Time
		consumed bool
	}{
		"operator suspend stamped after":   {meta: operatorSuspendedRow(map[string]string{"suspended_at": stamp}), notAfter: before},
		"operator suspend after, draining": {meta: operatorSuspendedRow(map[string]string{"suspended_at": stamp, "state": string(StateDraining)}), notAfter: before},
		"soft suspend stamped after":       {meta: map[string]string{"state": string(StateSuspended), "suspended_at": stamp}, notAfter: before},
		"operator suspend stamped at":      {meta: operatorSuspendedRow(map[string]string{"suspended_at": stamp}), notAfter: at, consumed: true},
		"operator suspend in its second":   {meta: operatorSuspendedRow(map[string]string{"suspended_at": stamp}), notAfter: at.Add(500 * time.Millisecond), consumed: true},
		"operator suspend, no stamp":       {meta: operatorSuspendedRow(map[string]string{"suspended_at": ""}), notAfter: before, consumed: true},
		"operator suspend, unreadable":     {meta: operatorSuspendedRow(map[string]string{"suspended_at": "yesterday"}), notAfter: before, consumed: true},
	} {
		t.Run(name, func(t *testing.T) {
			store, id := coalesceRow(t, policyRow(tc.meta))
			ss := NewStore(beads.SessionStore{Store: store})
			rev0, was := rowRevision(t, store, id)
			if err := ss.ResumeHeld(id, tc.notAfter, resumeNow); err != nil {
				t.Fatalf("ResumeHeld: %v", err)
			}
			rev1, meta := rowRevision(t, store, id)
			if tc.consumed {
				if meta["state"] != string(StateAsleep) || meta["sleep_intent"] != "" || meta["suspended_at"] != "" || meta["wake_request"] != string(WakeCauseExplicit) {
					t.Fatalf("row = %v; want the suspend consumed and its wake recorded", meta)
				}
				return
			}
			if rev1 != rev0 || meta["state"] != was["state"] || meta["suspended_at"] != stamp || meta["wake_request"] != "" {
				t.Fatalf("revision %d -> %d (row %v); want the later suspend standing and nothing written", rev0, rev1, meta)
			}
		})
	}
}

// TestResumeHeldConfirmsAgainstTheLiveRow: a row with no suspend to consume
// is not written, the one verdict that skips the CAS, so it must not rest
// on a cached read that lags another process's write. Kills (a) a resume
// whose cached read missed an operator suspend written out of process,
// answered as done with the suspend standing, and (b) a fresh cache paying
// for a write it does not need.
func TestResumeHeldConfirmsAgainstTheLiveRow(t *testing.T) {
	t.Run("suspended out of process", func(t *testing.T) {
		cache, backing, id := cachedCoalesceRow(t, policyRow(nil))
		if err := backing.SetMetadataBatch(id, operatorSuspendedRow(nil)); err != nil {
			t.Fatal(err)
		}
		if err := NewStore(beads.SessionStore{Store: cache}).ResumeHeld(id, resumeNow, resumeNow); err != nil {
			t.Fatalf("ResumeHeld: %v", err)
		}
		if _, meta := rowRevision(t, backing, id); meta["state"] != string(StateAsleep) || meta["sleep_intent"] != "" || meta["wake_request"] != string(WakeCauseExplicit) {
			t.Fatalf("live row = %v; want the suspend consumed and its wake recorded", meta)
		}
	})

	t.Run("fresh cache, nothing to consume", func(t *testing.T) {
		cache, backing, id := cachedCoalesceRow(t, policyRow(nil))
		rev0, _ := rowRevision(t, backing, id)
		if err := NewStore(beads.SessionStore{Store: cache}).ResumeHeld(id, resumeNow, resumeNow); err != nil {
			t.Fatalf("ResumeHeld: %v", err)
		}
		if rev1, meta := rowRevision(t, backing, id); rev1 != rev0 {
			t.Fatalf("live row revision %d -> %d (row %v); want no write", rev0, rev1, meta)
		}
	})
}

// TestResumeHeldFailsWhenTheLiveRowIsUnreadable: a no-write verdict that
// cannot be confirmed on a live read is an error, not a resume done. Kills
// swallowing the live read's error, which answers the resume as done on an
// unconfirmed cached read.
func TestResumeHeldFailsWhenTheLiveRowIsUnreadable(t *testing.T) {
	store, id := coalesceRow(t, policyRow(nil))
	rev0, _ := rowRevision(t, store, id)
	sentinel := errors.New("backing unreachable")
	if err := NewStore(beads.SessionStore{Store: failingLiveStore{Store: store, err: sentinel}}).ResumeHeld(id, resumeNow, resumeNow); !errors.Is(err, sentinel) {
		t.Fatalf("ResumeHeld = %v, want an error wrapping %v", err, sentinel)
	}
	if rev1, meta := rowRevision(t, store, id); rev1 != rev0 {
		t.Fatalf("row revision %d -> %d (row %v); want no write", rev0, rev1, meta)
	}
}

// wakeCASLoser makes every UpdateIfMatch lose to an unrelated write.
type wakeCASLoser struct {
	*rowWriteRecorder
	backing beads.Store
	n       int
}

func (c *wakeCASLoser) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	c.n++
	_ = c.backing.SetMetadataBatch(id, map[string]string{"test_nudge_at": fmt.Sprint(c.n)})
	return c.rowWriteRecorder.UpdateIfMatch(id, rev, opts)
}

// TestAttachResumesHeldRow: Attach is the operator's own resume, so a held
// row starts and is attached. Kills Attach taking the background policy.
func TestAttachResumesHeldRow(t *testing.T) {
	e := newPolicyEnv(t, policyRow(map[string]string{"state": string(StateSuspended), "sleep_intent": "user-hold", "held_until": "2099-01-01T00:00:00Z"}), false)
	if err := e.mgr.Attach(context.Background(), testActor(e.mgr, ActorOperator), e.id, "claude --resume k", runtime.Config{}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !e.sp.IsRunning(resumeName) || e.sp.CountCalls("Attach", resumeName) != 1 {
		t.Fatal("Attach did not resume the held row")
	}
}

// TestInterruptRestartKeepsHeartbeatHold is the re-review's N1: a background
// interrupt_now on a live row with a heartbeat or unreadable held_until
// passes the hold check (the runtime is running), and its hard restart, a
// codex boundary timeout's or pi's, must not re-read the hold on the runtime
// it just stopped, under IfUnheld or ViaController (N3: the API's own
// interrupt). Kills a restart that refuses, which loses the session and the
// message.
func TestInterruptRestartKeepsHeartbeatHold(t *testing.T) {
	for _, policy := range []ActorKind{ActorAgent, ActorBackground} {
		for _, provider := range []string{"codex", "pi"} {
			for _, until := range []string{"2099-01-01T00:00:00Z", "soon"} {
				t.Run(fmt.Sprintf("%d/%s/%s", policy, provider, until), func(t *testing.T) {
					store := beads.NewMemStore()
					sp := runtime.NewFake()
					mgr := newTestManager(t, store, sp)
					info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: provider + " --session k", WorkDir: t.TempDir(), Provider: provider, ExtraMeta: map[string]string{"session_origin": "manual"}})
					if err != nil {
						t.Fatal(err)
					}
					if err := store.SetMetadata(info.ID, "held_until", until); err != nil {
						t.Fatal(err)
					}
					sp.InterruptBoundaryErrors[info.SessionName] = fmt.Errorf("no turn_aborted marker yet")
					hints := runtime.Config{WorkDir: info.WorkDir, Env: map[string]string{"PI_CODING_AGENT_SESSION_DIR": t.TempDir()}}
					out, err := mgr.Submit(context.Background(), testActor(mgr, policy), info.ID, "replace", BuildResumeCommand(info), hints, SubmitIntentInterruptNow)
					if err != nil || out.Queued {
						t.Fatalf("Submit = %+v, %v; want delivered", out, err)
					}
					if sp.CountCalls("Stop", info.SessionName) == 0 || sp.CountCalls("Start", info.SessionName) == 0 || !sp.IsRunning(info.SessionName) {
						t.Fatalf("calls = %#v, want the runtime stopped and restarted", sp.Calls)
					}
					var delivered bool
					for _, call := range sp.Calls {
						delivered = delivered || call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "replace"
					}
					if !delivered {
						t.Fatal("the message was not delivered after the restart")
					}
				})
			}
		}
	}
}

// cachedCoalesceRow creates meta as a session row behind a primed CachingStore
// over a revision-stamped store, and returns the cache and its backing. A write
// to the backing is another process's write the cache has not seen yet.
func cachedCoalesceRow(t *testing.T, meta map[string]string) (cache, backing beads.Store, id string) {
	t.Helper()
	backing = stampedMemStore(t, gate.Auto, beads.NewMemStore())
	b, err := backing.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cs, backing, b.ID
}

// TestRequestWakeCoalesceConfirmsAgainstTheLiveRow: the coalesce verdict is
// the one decision that skips the CAS, so it must not rest on a cached read
// that lags another process's write. Kills (a) an operator suspend written out
// of process reported as WakeRecorded on the held row, (b) a wake consumed and
// slept again out of process swallowed by the stale unserved request, and
// (c) a fresh cache paying for a write it does not need.
func TestRequestWakeCoalesceConfirmsAgainstTheLiveRow(t *testing.T) {
	pending := policyRow(map[string]string{
		"wake_request":      string(WakeCauseExplicit),
		"wake_requested_at": "2026-10-08T11:00:00Z",
	})

	t.Run("suspended out of process", func(t *testing.T) {
		cache, backing, id := cachedCoalesceRow(t, pending)
		if err := backing.SetMetadataBatch(id, map[string]string{
			"state":             string(StateSuspended),
			"held_until":        "2099-01-01T00:00:00Z",
			"sleep_intent":      "user-hold",
			"wake_request":      "",
			"wake_requested_at": "",
		}); err != nil {
			t.Fatal(err)
		}
		rev0, _ := rowRevision(t, backing, id)
		got, err := NewStore(beads.SessionStore{Store: cache}).RequestWakeUnlessHeld(id, false, resumeNow)
		if err != nil || got != WakeHeld {
			t.Fatalf("RequestWakeUnlessHeld = %v, %v; want WakeHeld", got, err)
		}
		if rev1, meta := rowRevision(t, backing, id); rev1 != rev0 || meta["wake_request"] != "" {
			t.Fatalf("live row revision %d -> %d, wake_request %q; want no write", rev0, rev1, meta["wake_request"])
		}
	})

	t.Run("served out of process", func(t *testing.T) {
		cache, backing, id := cachedCoalesceRow(t, pending)
		// The owner consumed the wake, ran the session and slept it again.
		if err := backing.SetMetadataBatch(id, map[string]string{
			"wake_request":      "",
			"wake_requested_at": "",
			"slept_at":          "2026-10-08T11:30:00Z",
		}); err != nil {
			t.Fatal(err)
		}
		got, err := NewStore(beads.SessionStore{Store: cache}).RequestWakeUnlessHeld(id, false, resumeNow)
		if err != nil || got != WakeRecorded {
			t.Fatalf("RequestWakeUnlessHeld = %v, %v; want WakeRecorded", got, err)
		}
		_, meta := rowRevision(t, backing, id)
		if meta["wake_request"] != string(WakeCauseExplicit) || meta["wake_requested_at"] != resumeNow.Format(time.RFC3339) {
			t.Fatalf("live row wake_request %q wake_requested_at %q; want a fresh explicit wake", meta["wake_request"], meta["wake_requested_at"])
		}
	})

	t.Run("fresh cache coalesces", func(t *testing.T) {
		cache, backing, id := cachedCoalesceRow(t, pending)
		rev0, _ := rowRevision(t, backing, id)
		got, err := NewStore(beads.SessionStore{Store: cache}).RequestWakeUnlessHeld(id, false, resumeNow)
		if err != nil || got != WakeRecorded {
			t.Fatalf("RequestWakeUnlessHeld = %v, %v; want WakeRecorded", got, err)
		}
		if rev1, meta := rowRevision(t, backing, id); rev1 != rev0 || meta["wake_requested_at"] != "2026-10-08T11:00:00Z" {
			t.Fatalf("live row revision %d -> %d, wake_requested_at %q; want no write", rev0, rev1, meta["wake_requested_at"])
		}
	})
}

// failingLiveStore is a store whose Live.Get fails with err; every other read
// and write goes to the wrapped store.
type failingLiveStore struct {
	beads.Store
	err error
}

func (s failingLiveStore) Handles() beads.StoreHandles {
	h := beads.HandlesFor(s.Store)
	h.Live = failingLiveReader{LiveReader: h.Live, err: s.err}
	return h
}

type failingLiveReader struct {
	beads.LiveReader
	err error
}

func (r failingLiveReader) Get(string) (beads.Bead, error) { return beads.Bead{}, r.err }

// TestRequestWakeCoalesceFailsWhenTheLiveRowIsUnreadable: a coalesce verdict
// that cannot be confirmed on a live read is an error, not WakeRecorded.
// Kills swallowing the live-read error, which reports a wake recorded on an
// unconfirmed cached read and writes nothing.
func TestRequestWakeCoalesceFailsWhenTheLiveRowIsUnreadable(t *testing.T) {
	store, id := coalesceRow(t, policyRow(map[string]string{
		"wake_request":      string(WakeCauseExplicit),
		"wake_requested_at": "2026-10-08T11:00:00Z",
	}))
	rev0, _ := rowRevision(t, store, id)
	sentinel := errors.New("backing unreachable")

	got, err := NewStore(beads.SessionStore{Store: failingLiveStore{Store: store, err: sentinel}}).RequestWakeUnlessHeld(id, false, resumeNow)
	if !errors.Is(err, sentinel) || got != 0 {
		t.Fatalf("RequestWakeUnlessHeld = %v, %v; want 0, error wrapping %v", got, err, sentinel)
	}
	if rev1, meta := rowRevision(t, store, id); rev1 != rev0 || meta["wake_requested_at"] != "2026-10-08T11:00:00Z" {
		t.Fatalf("row revision %d -> %d, wake_requested_at %q; want no write", rev0, rev1, meta["wake_requested_at"])
	}
}
