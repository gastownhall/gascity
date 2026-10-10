package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// startHookProvider runs hook right after the runtime starts, before the
// resume confirms it: the window a concurrent writer can land in.
type startHookProvider struct {
	*runtime.Fake
	hook func()
}

func (p *startHookProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	if p.hook != nil {
		p.hook()
	}
	return nil
}

// userHeldRow is a session `gc session suspend` held, its runtime gone.
func userHeldRow(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "worker", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{
		"session_name":      "s-held",
		"template":          "worker",
		"provider":          "claude",
		"state":             string(StateSuspended),
		"generation":        "3",
		"instance_token":    "tok-3",
		"held_until":        time.Now().Add(testIndefiniteHold).UTC().Format(time.RFC3339),
		"sleep_intent":      string(SleepReasonUserHold),
		"suspended_at":      "2026-10-09T11:00:00Z",
		"wake_request":      string(WakeCauseExplicit),
		"wake_requested_at": "2026-10-09T11:00:00Z",
	}})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}
	return b
}

// testIndefiniteHold mirrors the CLI's 100-year suspend hold.
const testIndefiniteHold = 100 * 365 * 24 * time.Hour

func heldKeys(t *testing.T, store beads.Store, id string) map[string]string {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return got.Metadata
}

// TestOperatorResumeConsumesUserHold: Attach, and Start, Submit and Send
// under ActorOperator, are an operator's own resume; once the runtime is up
// they drop the user hold with the wake it satisfies, so the controller does
// not drain the session again.
func TestOperatorResumeConsumesUserHold(t *testing.T) {
	for name, resume := range map[string]func(m *Manager, id string) error{
		"attach": func(m *Manager, id string) error {
			return m.Attach(context.Background(), testActor(m, ActorOperator), id, "claude", runtime.Config{})
		},
		"start": func(m *Manager, id string) error {
			return m.Start(context.Background(), testActor(m, ActorOperator), id, "claude", runtime.Config{})
		},
		"submit": func(m *Manager, id string) error {
			_, err := m.Submit(context.Background(), testActor(m, ActorOperator), id, "hello", "claude", runtime.Config{}, SubmitIntentDefault)
			return err
		},
		"send": func(m *Manager, id string) error {
			_, err := m.Send(context.Background(), testActor(m, ActorOperator), id, "hello", "claude", runtime.Config{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			b := userHeldRow(t, store)
			sp := &startHookProvider{Fake: runtime.NewFake()}
			sp.hook = func() {
				// Never consumed before Start.
				if meta := heldKeys(t, store, b.ID); meta["sleep_intent"] != string(SleepReasonUserHold) {
					t.Errorf("hold consumed before the runtime started: %v", meta)
				}
			}
			if err := resume(newTestManager(t, store, sp), b.ID); err != nil {
				t.Fatalf("resume: %v", err)
			}
			meta := heldKeys(t, store, b.ID)
			for k, want := range map[string]string{
				"state": string(StateActive), "state_reason": "creation_complete", "held_until": "", "sleep_intent": "",
				"suspended_at": "", "wake_request": "", "wake_requested_at": "", "generation": "3", "instance_token": "tok-3",
			} {
				if meta[k] != want {
					t.Errorf("%s = %q, want %q", k, meta[k], want)
				}
			}
		})
	}
}

// TestBackgroundSendKeepsUserHold: a send that is not an operator's resume
// (ActorAgent) queues and never consumes the hold.
func TestBackgroundSendKeepsUserHold(t *testing.T) {
	store := beads.NewMemStore()
	b := userHeldRow(t, store)
	sp := runtime.NewFake()
	out, err := newTestManager(t, store, sp).Send(context.Background(), b.ID, "hello", "claude", runtime.Config{}, ResumeIfUnheld)
	if err != nil || !out.Queued || sp.CountCalls("Start", "s-held") != 0 {
		t.Fatalf("Send = %+v, %v with %d starts; want queued, nothing started", out, err, sp.CountCalls("Start", "s-held"))
	}
	if meta := heldKeys(t, store, b.ID); meta["sleep_intent"] != string(SleepReasonUserHold) || meta["held_until"] == "" {
		t.Fatalf("background send consumed the hold: sleep_intent=%q held_until=%q", meta["sleep_intent"], meta["held_until"])
	}
}

// liveHeldSession is a running session an operator's suspend held before its
// drain stopped it.
func liveHeldSession(t *testing.T, state State) (*Manager, *runtime.Fake, beads.Store, Info) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sp.WaitForIdleErrors = map[string]error{}
	mgr := newTestManager(t, store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadataBatch(info.ID, map[string]string{
		"state": string(state), "sleep_intent": string(SleepReasonUserHold),
		"held_until": time.Now().Add(testIndefiniteHold).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	return mgr, sp, store, info
}

// TestInterruptRestartConsumesOnlyForAnOperator: an interrupt that restarts
// the runtime carries its caller's policy; only ActorOperator consumes the
// user hold, IfUnheld and ViaController keep it.
func TestInterruptRestartConsumesOnlyForAnOperator(t *testing.T) {
	for policy, consumed := range map[ActorKind]bool{ActorOperator: true, ActorAgent: false, ActorBackground: false} {
		mgr, sp, store, info := liveHeldSession(t, StateActive)
		sp.WaitForIdleErrors[info.SessionName] = fmt.Errorf("not idle yet") // forces the restart fallback
		_, _ = mgr.Submit(context.Background(), testActor(mgr, policy), info.ID, "replace the turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
		if sp.CountCalls("Stop", info.SessionName) == 0 || sp.CountCalls("Start", info.SessionName) < 2 {
			t.Fatalf("policy %d: the interrupt did not restart the runtime (test premise)", policy)
		}
		if held := heldKeys(t, store, info.ID)["sleep_intent"] == string(SleepReasonUserHold); held == consumed {
			t.Errorf("policy %d: hold kept = %v, want consumed = %v", policy, held, consumed)
		}
	}
}

// TestLiveResumeConsumesUserHold: an operator's attach of a live row the
// operator holds consumes the hold when the row is active or awake, never
// when it is draining; a background send to the live row consumes nothing.
func TestLiveResumeConsumesUserHold(t *testing.T) {
	for name, tc := range map[string]struct {
		state    State
		operator bool
		consumed bool
	}{
		"active, attach":     {state: StateActive, operator: true, consumed: true},
		"awake, attach":      {state: StateAwake, operator: true, consumed: true},
		"draining, attach":   {state: StateDraining, operator: true},
		"active, background": {state: StateActive},
	} {
		mgr, sp, store, info := liveHeldSession(t, tc.state)
		var err error
		if tc.operator {
			err = mgr.Attach(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{})
		} else {
			_, err = mgr.Send(context.Background(), testActor(mgr, ActorAgent), info.ID, "hello", BuildResumeCommand(info), runtime.Config{})
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		meta := heldKeys(t, store, info.ID)
		if held := meta["sleep_intent"] == string(SleepReasonUserHold); held == tc.consumed || !sp.IsRunning(info.SessionName) {
			t.Errorf("%s: hold kept = %v running = %v, want consumed = %v and the runtime left up", name, held, sp.IsRunning(info.SessionName), tc.consumed)
		}
		if tc.consumed && meta["state"] != string(StateActive) {
			t.Errorf("%s: state = %q, want active", name, meta["state"])
		}
	}
}

// TestUserHoldConsumeRefusalNeverStopsTheRuntime: a write to the premise
// while the runtime starts (here a newer suspend) refuses the consume with an
// ErrStateSync error and leaves the runtime running and the hold in place;
// a row another resumer already consumed at the same generation and token is
// converged.
func TestUserHoldConsumeRefusalNeverStopsTheRuntime(t *testing.T) {
	for name, tc := range map[string]struct {
		race    MetadataPatch
		wantErr bool
	}{
		"newer suspend":     {race: MetadataPatch{"suspended_at": "2026-10-09T11:30:00Z"}, wantErr: true},
		"kill":              {race: MetadataPatch{"generation": "4"}, wantErr: true},
		"already consumed":  {race: consumeUserHoldPatch()},
		"active but waited": {race: MetadataPatch{"state": string(StateActive), "wait_hold": "w"}, wantErr: true},
		"state healed only": {race: MetadataPatch{"state": string(StateAwake)}},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			b := userHeldRow(t, store)
			sp := &startHookProvider{Fake: runtime.NewFake()}
			sp.hook = func() {
				if err := store.SetMetadataBatch(b.ID, tc.race); err != nil {
					t.Error(err)
				}
			}
			err := newTestManager(t, store, sp).Attach(context.Background(), b.ID, "claude", runtime.Config{})
			if gotErr := err != nil; gotErr != tc.wantErr || (err != nil && !errors.Is(err, ErrStateSync)) {
				t.Fatalf("Attach = %v, want error=%v (ErrStateSync)", err, tc.wantErr)
			}
			if !sp.IsRunning("s-held") || sp.CountCalls("Stop", "s-held") != 0 {
				t.Fatalf("running=%v stops=%d, want the launched runtime left running", sp.IsRunning("s-held"), sp.CountCalls("Stop", "s-held"))
			}
			if held := heldKeys(t, store, b.ID)["sleep_intent"] == string(SleepReasonUserHold); held != tc.wantErr {
				t.Fatalf("hold kept = %v, want %v", held, tc.wantErr)
			}
		})
	}
}

// TestLiveResumeConsumeDefersOnABusyLease: the live-branch consume takes the
// runtime lease with a try. While the controller holds it (its stop is
// deciding), the attach returns the retryable ErrSessionStarting and the
// hold stays.
func TestLiveResumeConsumeDefersOnABusyLease(t *testing.T) {
	defer SetOperatorLeaseWaitForTest(0)()
	_, sp, store, info := liveHeldSession(t, StateActive)
	city := t.TempDir()
	mgr := newTestManager(t, store, sp, WithCityPath(city))
	held, err := TryRuntimeLease(NewStore(beads.SessionStore{Store: store}), RuntimeLeaseRequest{City: city, Name: info.SessionName, ID: info.ID, TTL: RuntimeLeaseTTL(0)})
	if err != nil {
		t.Fatalf("TryRuntimeLease: %v", err)
	}
	err = mgr.Attach(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{})
	if !errors.Is(err, ErrSessionStarting) {
		t.Fatalf("Attach under a busy lease = %v, want ErrSessionStarting", err)
	}
	if heldKeys(t, store, info.ID)["sleep_intent"] != string(SleepReasonUserHold) {
		t.Fatal("the hold was consumed while the lease was busy")
	}
	held.Release()
	if err := mgr.Attach(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{}); err != nil {
		t.Fatalf("Attach after the lease is released: %v", err)
	}
	if heldKeys(t, store, info.ID)["sleep_intent"] != "" {
		t.Fatal("the retried attach did not consume the hold")
	}
}
