package session

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
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
	store beads.Store
	sp    *runtime.Fake
	mgr   *Manager
	id    string
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
	mgr := NewManagerWithOptions(store, sp, WithClock(&clock.Fake{Time: resumeNow}), WithCityPath(t.TempDir()))
	return &policyEnv{store: store, sp: sp, mgr: mgr, id: b.ID}
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
		policy ResumePolicy
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
		{name: "operator resumes a held row", meta: map[string]string{"state": string(StateSuspended)}, policy: ResumeOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPolicyEnv(t, policyRow(tc.meta), tc.live)
			before := e.row(t)
			startsBefore := e.sp.CountCalls("Start", resumeName)
			out, err := e.mgr.Send(context.Background(), e.id, "hello", "claude --resume k", runtime.Config{}, tc.policy)
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
// held row reports not delivered, without error or start, so its caller
// queues it. Kills a held refusal surfacing as a nudge failure.
func TestWaitIdleNudgeToHeldRowIsUndelivered(t *testing.T) {
	e := newPolicyEnv(t, policyRow(map[string]string{"state": string(StateSuspended)}), false)
	delivered, err := e.mgr.TryWaitIdleNudge(context.Background(), e.id, "mail", "hello", "claude --resume k", runtime.Config{}, ResumeIfUnheld)
	if err != nil || delivered || e.sp.CountCalls("Start", resumeName) != 0 {
		t.Fatalf("TryWaitIdleNudge = %v, %v (starts %d); want undelivered, no error, no start", delivered, err, e.sp.CountCalls("Start", resumeName))
	}
}

// TestInterruptSubmitToHeldLiveRowQueues: a background interrupt_now on a
// held row whose runtime is still live (a managed suspend the controller has
// not acted on) queues before touching the runtime. Kills a policy check
// that runs only after the interrupt's hard restart.
func TestInterruptSubmitToHeldLiveRowQueues(t *testing.T) {
	e := newPolicyEnv(t, policyRow(map[string]string{"state": string(StateSuspended), "sleep_intent": "user-hold", "provider": "pi"}), true)
	out, err := e.mgr.Submit(context.Background(), e.id, "now", "pi --resume k", runtime.Config{}, SubmitIntentInterruptNow, ResumeIfUnheld)
	if err != nil || !out.Queued {
		t.Fatalf("Submit = %+v, %v; want queued", out, err)
	}
	if !e.sp.IsRunning(resumeName) || e.sp.CountCalls("Stop", resumeName) != 0 {
		t.Fatal("the held row's live runtime was stopped")
	}
}

// TestRequestWakeUnlessHeld: a background wake records an explicit wake by
// CAS on an unheld row and leaves a held one untouched. Kills a wake written
// over an operator's hold.
func TestRequestWakeUnlessHeld(t *testing.T) {
	for name, tc := range map[string]struct {
		meta  map[string]string
		woken bool
	}{
		"unheld": {woken: true},
		"held":   {meta: map[string]string{"state": string(StateSuspended)}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newPolicyEnv(t, policyRow(tc.meta), false)
			woken, err := NewStore(beads.SessionStore{Store: e.store}).RequestWakeUnlessHeld(e.id, resumeNow)
			if err != nil || woken != tc.woken {
				t.Fatalf("RequestWakeUnlessHeld = %v, %v; want %v", woken, err, tc.woken)
			}
			if got := e.row(t)["wake_request"] != ""; got != tc.woken {
				t.Fatalf("wake_request written = %v, want %v", got, tc.woken)
			}
		})
	}
}

// TestAttachResumesHeldRow: Attach is the operator's own resume, so a held
// row starts and is attached. Kills Attach taking the background policy.
func TestAttachResumesHeldRow(t *testing.T) {
	e := newPolicyEnv(t, policyRow(map[string]string{"state": string(StateSuspended), "sleep_intent": "user-hold", "held_until": "2099-01-01T00:00:00Z"}), false)
	if err := e.mgr.Attach(context.Background(), e.id, "claude --resume k", runtime.Config{}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !e.sp.IsRunning(resumeName) || e.sp.CountCalls("Attach", resumeName) != 1 {
		t.Fatal("Attach did not resume the held row")
	}
}
