package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

func init() { resumeBackoff = func(int) {} }

// objectProvider is a fake runtime with tmux's identity surface: each Start
// creates a new session object ($N, created at N) carrying the
// GC_INSTANCE_TOKEN it was launched with, a fresh liveness read names it,
// and exact-object kills honor it. afterStart runs once a Start lands
// (between the resume's fence and its consume).
type objectProvider struct {
	*runtime.Fake
	next       int
	objects    map[string]string
	afterStart func()
	verdict    runtime.SessionObjectKillResult // forced kill verdict, when set
	kills      int
}

func newObjectProvider() *objectProvider {
	return &objectProvider{Fake: runtime.NewFake(), objects: map[string]string{}}
}

func (p *objectProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	p.next++
	p.objects[name] = fmt.Sprintf("$%d", p.next)
	if err := p.SetMeta(name, "GC_INSTANCE_TOKEN", cfg.Env["GC_INSTANCE_TOKEN"]); err != nil {
		return err
	}
	if p.afterStart != nil {
		p.afterStart()
	}
	return nil
}

func (p *objectProvider) ObserveLivenessSince(name string, _ []string, _ time.Time) (runtime.Liveness, error) {
	if !p.IsRunning(name) {
		return runtime.Liveness{}, nil
	}
	id := p.objects[name]
	return runtime.Liveness{Running: true, Alive: true, ObjectID: id, ObjectCreated: strings.TrimPrefix(id, "$"), PanePID: "100"}, nil
}

func (p *objectProvider) kill(name, objectID, created string) (runtime.SessionObjectKillResult, error) {
	p.kills++
	if p.verdict != 0 {
		return p.verdict, nil
	}
	if !p.IsRunning(name) || p.objects[name] != objectID || strings.TrimPrefix(objectID, "$") != created {
		return runtime.SessionObjectGone, nil
	}
	return runtime.SessionObjectKilled, p.Stop(name)
}

func (p *objectProvider) KillCorpseObject(name, objectID, created string) (runtime.SessionObjectKillResult, error) {
	return p.kill(name, objectID, created)
}

func (p *objectProvider) KillZombieObject(name, objectID, created, _ string) (runtime.SessionObjectKillResult, error) {
	return p.kill(name, objectID, created)
}

// heldRow is J27 A's row: suspended under an operator's hold with a stale
// drained reason, a pending wake, both stop-request halves and a baseline,
// at generation 3. extra overrides it.
func heldRow(extra map[string]string) map[string]string {
	meta := map[string]string{
		"session_name": resumeName, "template": "worker", "work_dir": "/tmp", "provider": "claude",
		"generation": "3", "instance_token": "tok-3", "started_config_hash": "base-hash",
		"awake_started_at": "2026-10-01T00:00:00Z", "last_woke_at": "2026-10-01T00:00:00Z",
		"state": string(StateSuspended), "suspended_at": "2026-10-08T10:00:00Z", "sleep_reason": "drained",
		"held_until": "2099-01-01T00:00:00Z", "sleep_intent": "user-hold",
		"wake_request": string(WakeCauseExplicit), "wake_requested_at": "2026-10-08T11:00:00Z",
		DrainIntentReasonKey: "idle", DrainIntentAtKey: "2026-10-08T11:00:00Z", DrainIntentIncarnationKey: "3",
		DrainAckIncarnationKey: "3", DrainAckAtKey: "2026-10-08T11:01:00Z",
	}
	maps.Copy(meta, extra)
	return meta
}

type fenceEnv struct {
	store beads.Store
	sp    *objectProvider
	mgr   *Manager
	id    string
}

// newFenceEnv seeds meta on a revision-fenced store; a live runtime, when
// asked for, carries the row's token.
func newFenceEnv(t *testing.T, meta map[string]string, live bool) *fenceEnv {
	t.Helper()
	store := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	b, err := store.Create(beads.Bead{Title: "worker", Type: BeadType, Labels: []string{LabelSession}, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	sp := newObjectProvider()
	if live {
		if err := sp.Start(context.Background(), resumeName, runtime.Config{Env: map[string]string{"GC_INSTANCE_TOKEN": meta["instance_token"]}}); err != nil {
			t.Fatal(err)
		}
	}
	mgr := NewManagerWithOptions(store, sp, WithClock(&clock.Fake{Time: resumeNow}), WithCityPath(t.TempDir()))
	return &fenceEnv{store: store, sp: sp, mgr: mgr, id: b.ID}
}

func (e *fenceEnv) row(t *testing.T) map[string]string {
	t.Helper()
	b, err := e.store.Get(e.id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata
}

func (e *fenceEnv) attach() error {
	return e.mgr.Attach(context.Background(), e.id, "claude --resume k", runtime.Config{})
}

func wantRow(t *testing.T, got, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// consumed is what D8 rule 4's CAS leaves; launched adds the new interval.
func consumed(launched bool) map[string]string {
	want := map[string]string{
		"state": string(StateActive), "state_reason": "creation_complete",
		"sleep_reason": "", "slept_at": "", "suspended_at": "", "sleep_intent": "", "held_until": "",
		"wait_hold": "", "quarantined_until": "", "wake_request": "", "wake_requested_at": "",
		DrainIntentReasonKey: "", DrainIntentAtKey: "", DrainIntentIncarnationKey: "",
		DrainAckIncarnationKey: "", DrainAckAtKey: "", ResumePendingAtKey: "",
		"generation": "3", "instance_token": "tok-3", "started_config_hash": "base-hash",
		"awake_started_at": "2026-10-01T00:00:00Z", "last_woke_at": "2026-10-01T00:00:00Z",
	}
	if launched {
		want["awake_started_at"] = resumeNow.Format(time.RFC3339Nano)
		want["last_woke_at"] = resumeNow.Format(time.RFC3339)
	}
	return want
}

// holdShapes are the operator holds a resume consumes, one at a time
// (review finding 4).
var holdShapes = map[string]map[string]string{
	"bare suspended":  {"held_until": "", "sleep_intent": "", "sleep_reason": "", "wake_request": "", "wake_requested_at": ""},
	"user-hold only":  {"state": string(StateAsleep), "suspended_at": "", "held_until": ""},
	"heartbeat hold":  {"state": string(StateAsleep), "suspended_at": "", "sleep_intent": "", "sleep_reason": ""},
	"managed suspend": {},
	"quarantine":      {"state": string(StateAsleep), "suspended_at": "", "held_until": "", "sleep_intent": "", "quarantined_until": "2099-01-01T00:00:00Z", "sleep_reason": "quarantine"},
	"wait hold":       {"state": string(StateAsleep), "suspended_at": "", "held_until": "", "sleep_intent": "wait-hold", "wait_hold": "true", "sleep_reason": "wait-hold"},
}

// TestOperatorResumeConsumesTheHold is J27 A (CONTRACT v5.9 D8, §12.2 row
// 30), per hold shape and per operator verb: while the runtime starts the
// row keeps its hold and carries the fence; then one CAS leaves it active,
// unheld, with no stale reason, wake, stop request or fence, a new awake
// interval, and the same generation, token and baseline. Kills a resume that
// clears the hold before the start, a consume that skips any key, and a
// restamped generation.
func TestOperatorResumeConsumesTheHold(t *testing.T) {
	verbs := map[string]func(e *fenceEnv) error{
		"attach": (*fenceEnv).attach,
		"send": func(e *fenceEnv) error {
			out, err := e.mgr.Send(context.Background(), e.id, "hello", "claude --resume k", runtime.Config{}, ResumeOperator)
			if err == nil && out.Queued {
				err = errors.New("queued")
			}
			return err
		},
		"submit": func(e *fenceEnv) error {
			out, err := e.mgr.Submit(context.Background(), e.id, "hello", "claude --resume k", runtime.Config{}, SubmitIntentDefault, ResumeOperator)
			if err == nil && out.Queued {
				err = errors.New("queued")
			}
			return err
		},
	}
	for shape, extra := range holdShapes {
		for verb, run := range verbs {
			t.Run(shape+"/"+verb, func(t *testing.T) {
				e := newFenceEnv(t, heldRow(extra), false)
				before := e.row(t)
				var atStart map[string]string
				e.sp.afterStart = func() { atStart = e.row(t) }
				if err := run(e); err != nil {
					t.Fatalf("resume: %v", err)
				}
				for _, key := range []string{"held_until", "sleep_intent", "wait_hold", "quarantined_until", "suspended_at"} {
					if atStart[key] != before[key] {
						t.Errorf("at start %s = %q, want the hold %q kept until the consume", key, atStart[key], before[key])
					}
				}
				if atStart[ResumePendingAtKey] != resumeNow.Format(time.RFC3339Nano) {
					t.Errorf("at start the fence = %q, want this call's stamp", atStart[ResumePendingAtKey])
				}
				wantRow(t, e.row(t), consumed(true))
			})
		}
	}
}

// TestResumeOfLiveDormantRowIsOneCAS: attaching to a dormant row whose own
// runtime is alive writes one CAS, no fence, and keeps the running
// interval's epoch (#3513). A live runtime whose token is foreign, empty or
// unreadable is refused untouched, and a foreign one names `gc session
// kill`. Kills a fence or restamp on the live path, adopting a foreign
// runtime, and an unknown identity read as foreign or own.
func TestResumeOfLiveDormantRowIsOneCAS(t *testing.T) {
	e := newFenceEnv(t, heldRow(map[string]string{"state": string(StateAsleep), "suspended_at": "", "held_until": "", "sleep_intent": "user-hold"}), true)
	rec := &rowWriteRecorder{Store: e.store}
	e.mgr = NewManagerWithOptions(rec, e.sp, WithClock(&clock.Fake{Time: resumeNow}))
	if err := e.attach(); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if len(rec.writes) != 1 || rec.writes[0] != "UpdateIfMatch" {
		t.Fatalf("writes = %v, want one CAS", rec.writes)
	}
	wantRow(t, e.row(t), consumed(false))

	for name, tc := range map[string]struct {
		token   string
		readErr error
		want    error
		remedy  bool
	}{
		"foreign":    {token: "tok-other", want: ErrResumeSuperseded, remedy: true},
		"empty":      {token: "", want: ErrResumeRuntimeUnknown},
		"unreadable": {readErr: errors.New("socket gone"), want: ErrResumeRuntimeUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			e := newFenceEnv(t, heldRow(map[string]string{"state": string(StateAsleep), "suspended_at": ""}), true)
			_ = e.sp.SetMeta(resumeName, "GC_INSTANCE_TOKEN", tc.token)
			if tc.readErr != nil {
				e.sp.GetMetaErrors = map[string]map[string]error{resumeName: {"GC_INSTANCE_TOKEN": tc.readErr}}
			}
			before := e.row(t)
			err := e.attach()
			if !errors.Is(err, tc.want) || (tc.remedy != strings.Contains(fmt.Sprint(err), "gc session kill")) {
				t.Fatalf("attach = %v, want %v (remedy named: %v)", err, tc.want, tc.remedy)
			}
			if !e.sp.IsRunning(resumeName) || !maps.Equal(e.row(t), before) {
				t.Fatal("a runtime the resume does not own was stopped, or its row written")
			}
		})
	}
}

// TestFailedResumeStartKeepsTheHold (D8 rule 5): a failed start writes no
// consume; the row keeps its hold, reason and wake.
func TestFailedResumeStartKeepsTheHold(t *testing.T) {
	e := newFenceEnv(t, heldRow(nil), false)
	e.sp.StartErrors = map[string]error{resumeName: errors.New("no capacity")}
	if err := e.attach(); err == nil {
		t.Fatal("attach succeeded over a failed start")
	}
	want := heldRow(nil)
	wantRow(t, e.row(t), map[string]string{
		"state": want["state"], "held_until": want["held_until"], "sleep_intent": want["sleep_intent"],
		"sleep_reason": want["sleep_reason"], "suspended_at": want["suspended_at"], "wake_request": want["wake_request"],
	})
}

func patchWrite(patch MetadataPatch) func(beads.Store, string) error {
	return func(s beads.Store, id string) error { return s.SetMetadataBatch(id, map[string]string(patch)) }
}

// TestResumeRefusedBetweenFenceAndConsume (D8 rules 4 and 5): an operator
// write landing while the resume's runtime starts refuses the consume; the
// call kills the exact object it launched and returns ErrResumeSuperseded,
// not ErrStateSync, and the row keeps the newer write. Kills a consume
// missing a premise fact, a blind consume, a refusal that leaves the
// launched runtime, and wrapping the refusal as ErrStateSync.
func TestResumeRefusedBetweenFenceAndConsume(t *testing.T) {
	for name, tc := range map[string]struct {
		write func(s beads.Store, id string) error
		check map[string]string
	}{
		"kill fence":       {write: patchWrite(KillPendingPatch(resumeNow)), check: map[string]string{"state_reason": KillPendingReason}},
		"completed kill":   {write: patchWrite(SleepPatch(resumeNow, string(SleepReasonKilled))), check: map[string]string{"sleep_reason": string(SleepReasonKilled)}},
		"managed suspend":  {write: patchWrite(MetadataPatch{"held_until": "2099-02-01T00:00:00Z", "sleep_intent": "user-hold", "state": "suspended"}), check: map[string]string{"held_until": "2099-02-01T00:00:00Z"}},
		"operator suspend": {write: patchWrite(MetadataPatch{"suspended_at": "2026-10-08T12:00:01Z", "state": "suspended"}), check: map[string]string{"suspended_at": "2026-10-08T12:00:01Z"}},
		"wait hold":        {write: patchWrite(MetadataPatch{"wait_hold": "true"}), check: map[string]string{"wait_hold": "true"}},
		"quarantine":       {write: patchWrite(MetadataPatch{"quarantined_until": "2099-01-01T00:00:00Z"}), check: map[string]string{"quarantined_until": "2099-01-01T00:00:00Z"}},
		"new wake":         {write: patchWrite(MetadataPatch{"wake_requested_at": "2026-10-08T12:00:01Z"}), check: map[string]string{"wake_requested_at": "2026-10-08T12:00:01Z"}},
		"new generation":   {write: patchWrite(MetadataPatch{"generation": "4"}), check: map[string]string{"generation": "4"}},
		"new token":        {write: patchWrite(MetadataPatch{"instance_token": "tok-4"}), check: map[string]string{"instance_token": "tok-4"}},
		"another fence":    {write: patchWrite(MetadataPatch{ResumePendingAtKey: "2026-10-08T12:00:01Z"}), check: map[string]string{ResumePendingAtKey: "2026-10-08T12:00:01Z"}},
		"close":            {write: func(s beads.Store, id string) error { return s.Close(id) }},
	} {
		t.Run(name, func(t *testing.T) {
			e := newFenceEnv(t, heldRow(nil), false)
			e.sp.afterStart = func() {
				if err := tc.write(e.store, e.id); err != nil {
					t.Fatalf("concurrent write: %v", err)
				}
			}
			err := e.attach()
			if !errors.Is(err, ErrResumeSuperseded) || errors.Is(err, ErrStateSync) {
				t.Fatalf("attach = %v, want ErrResumeSuperseded and not ErrStateSync", err)
			}
			if e.sp.IsRunning(resumeName) || e.sp.kills != 1 {
				t.Fatalf("running = %v, exact-object kills = %d; want the launched object killed", e.sp.IsRunning(resumeName), e.sp.kills)
			}
			got := e.row(t)
			wantRow(t, got, tc.check)
			if got["state"] == string(StateActive) || got["last_woke_at"] == resumeNow.Format(time.RFC3339) {
				t.Fatalf("refused resume wrote: %v", got)
			}
		})
	}
}

// TestTwoResumers (review finding 1): a live foreign fence refuses a second
// resume with the retryable ErrResumeInProgress, starting and writing
// nothing; an expired one is taken over. A resume that loses its consume to
// another resume of the same incarnation converges without killing the
// session's runtime. Kills a fence premise that ignores a live foreign
// fence, an expiry that never ends, and a converged refusal that kills.
func TestTwoResumers(t *testing.T) {
	foreign := resumeNow.Add(-time.Second).Format(time.RFC3339Nano)
	live := newFenceEnv(t, heldRow(map[string]string{ResumePendingAtKey: foreign}), false)
	if err := live.attach(); !errors.Is(err, ErrResumeInProgress) || live.sp.CountCalls("Start", resumeName) != 0 || live.row(t)[ResumePendingAtKey] != foreign {
		t.Fatalf("attach over a live foreign fence = %v (starts %d); want ErrResumeInProgress, nothing started or written", err, live.sp.CountCalls("Start", resumeName))
	}
	expired := newFenceEnv(t, heldRow(map[string]string{ResumePendingAtKey: resumeNow.Add(-ResumeFenceTTL).Format(time.RFC3339Nano)}), false)
	if err := expired.attach(); err != nil {
		t.Fatalf("attach over an expired fence: %v", err)
	}
	wantRow(t, expired.row(t), consumed(true))

	conv := newFenceEnv(t, heldRow(nil), false)
	conv.sp.afterStart = func() {
		// The other resume consumed the row at the same incarnation.
		other := consumed(false)
		delete(other, "awake_started_at")
		delete(other, "last_woke_at")
		_ = patchWrite(MetadataPatch(other))(conv.store, conv.id)
	}
	if err := conv.attach(); err != nil {
		t.Fatalf("attach that lost to a converged resume: %v", err)
	}
	if !conv.sp.IsRunning(resumeName) || conv.sp.kills != 0 {
		t.Fatal("a converged resume killed the session's runtime")
	}

	// Unheld but not active is not converged: the launch is killed.
	notActive := newFenceEnv(t, heldRow(nil), false)
	notActive.sp.afterStart = func() {
		_ = patchWrite(MetadataPatch{"state": string(StateAsleep), "suspended_at": "", "held_until": "", "sleep_intent": ""})(notActive.store, notActive.id)
	}
	if err := notActive.attach(); !errors.Is(err, ErrResumeSuperseded) || notActive.sp.IsRunning(resumeName) {
		t.Fatalf("attach = %v, running = %v; want refused and the launch killed", err, notActive.sp.IsRunning(resumeName))
	}
}

// TestResumeBackfillsTokenIntoItsPremise: a resume of a row with no
// instance token mints one before the start; the consume's premise expects
// that token, not the empty one it read. Kills a premise left at the read.
func TestResumeBackfillsTokenIntoItsPremise(t *testing.T) {
	e := newFenceEnv(t, heldRow(map[string]string{"instance_token": ""}), false)
	if err := e.attach(); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got := e.row(t); got["state"] != string(StateActive) || got["instance_token"] == "" {
		t.Fatalf("row = %v, want consumed under the minted token", got)
	}
}

// TestStaleFenceDoesNotBlockLiveResume: a crashed caller's expired fence
// beside its live runtime does not block the next attach, which consumes the
// row and clears it; an unexpired one is a live resume and refuses.
func TestStaleFenceDoesNotBlockLiveResume(t *testing.T) {
	stale := resumeNow.Add(-2 * ResumeFenceTTL).Format(time.RFC3339Nano)
	e := newFenceEnv(t, heldRow(map[string]string{ResumePendingAtKey: stale, "state": string(StateAsleep), "suspended_at": ""}), true)
	if err := e.attach(); err != nil {
		t.Fatalf("attach: %v", err)
	}
	wantRow(t, e.row(t), consumed(false))
}

// casLoser makes every UpdateIfMatch lose to an unrelated write.
type casLoser struct {
	*rowWriteRecorder
	backing beads.Store
	n       int
}

func (c *casLoser) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if _, ok := opts.Metadata["state_reason"]; ok {
		c.n++
		_ = c.backing.SetMetadataBatch(id, map[string]string{"nudge_at": fmt.Sprint(c.n)})
	}
	return c.rowWriteRecorder.UpdateIfMatch(id, rev, opts)
}

// TestResumeCASExhaustionIsRetryable (review finding 5): a consume whose CAS
// loses on every attempt backs off, then returns the retryable
// ErrResumeInProgress without killing the launched runtime. Kills a single
// attempt, and an exhaustion treated as a refusal.
func TestResumeCASExhaustionIsRetryable(t *testing.T) {
	e := newFenceEnv(t, heldRow(nil), false)
	loser := &casLoser{rowWriteRecorder: &rowWriteRecorder{Store: e.store}, backing: e.store}
	e.mgr = NewManagerWithOptions(loser, e.sp, WithClock(&clock.Fake{Time: resumeNow}))
	if err := e.attach(); !errors.Is(err, ErrResumeInProgress) {
		t.Fatalf("attach = %v, want ErrResumeInProgress", err)
	}
	if loser.n < 2 || !e.sp.IsRunning(resumeName) || e.sp.kills != 0 {
		t.Fatalf("consume attempts = %d, running = %v, kills = %d; want retries and no kill", loser.n, e.sp.IsRunning(resumeName), e.sp.kills)
	}
}

// consumeHook runs before once, ahead of the consume's first CAS.
type consumeHook struct {
	*rowWriteRecorder
	before func()
}

func (h *consumeHook) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if _, ok := opts.Metadata["state_reason"]; ok && h.before != nil {
		h.before()
		h.before = nil
	}
	return h.rowWriteRecorder.UpdateIfMatch(id, rev, opts)
}

// TestRefusedResumeKillsOnlyItsObject (review finding 6): the refusal kills
// only the object its Start launched, and honors the kill's verdict: a
// runtime replaced since is left running, and a refused kill is an error.
func TestRefusedResumeKillsOnlyItsObject(t *testing.T) {
	e := newFenceEnv(t, heldRow(nil), false)
	hook := &consumeHook{rowWriteRecorder: &rowWriteRecorder{Store: e.store}}
	hook.before = func() {
		// After the capture, the session is replaced and re-keyed.
		_ = e.sp.Stop(resumeName)
		_ = e.sp.Start(context.Background(), resumeName, runtime.Config{Env: map[string]string{"GC_INSTANCE_TOKEN": "tok-3"}})
		_ = patchWrite(MetadataPatch{"generation": "4"})(e.store, e.id)
	}
	e.mgr = NewManagerWithOptions(hook, e.sp, WithClock(&clock.Fake{Time: resumeNow}))
	if err := e.attach(); !errors.Is(err, ErrResumeSuperseded) || !e.sp.IsRunning(resumeName) {
		t.Fatalf("attach = %v, running = %v; want refused and the replacement left running", err, e.sp.IsRunning(resumeName))
	}

	refused := newFenceEnv(t, heldRow(nil), false)
	refused.sp.verdict = runtime.SessionObjectChanged
	refused.sp.afterStart = func() { _ = patchWrite(MetadataPatch{"generation": "4"})(refused.store, refused.id) }
	if err := refused.attach(); !errors.Is(err, ErrResumeSuperseded) || !strings.Contains(fmt.Sprint(err), "kill verdict") {
		t.Fatalf("attach = %v, want the refused kill reported", err)
	}
}

// TestPlainConfirmationIsFenced: a row that was not dormant is confirmed by
// a CAS that refuses a kill fence (killing the object it launched), never
// re-activates a row suspended since the read, and with nothing to confirm
// reads and writes nothing. Kills the old blind confirm.
func TestPlainConfirmationIsFenced(t *testing.T) {
	creating := heldRow(map[string]string{"state": string(StateCreating), "suspended_at": "", "held_until": "", "sleep_intent": "", "pending_create_claim": "true"})
	t.Run("kill fence refuses", func(t *testing.T) {
		e := newFenceEnv(t, creating, false)
		e.sp.afterStart = func() { _ = patchWrite(KillPendingPatch(resumeNow))(e.store, e.id) }
		if err := e.attach(); !errors.Is(err, ErrResumeSuperseded) || e.sp.IsRunning(resumeName) {
			t.Fatalf("attach = %v, running = %v; want refused and the launch killed", err, e.sp.IsRunning(resumeName))
		}
	})
	t.Run("new generation refuses", func(t *testing.T) {
		e := newFenceEnv(t, creating, false)
		e.sp.afterStart = func() { _ = patchWrite(MetadataPatch{"generation": "4"})(e.store, e.id) }
		if err := e.attach(); !errors.Is(err, ErrResumeSuperseded) {
			t.Fatalf("attach = %v, want ErrResumeSuperseded", err)
		}
		wantRow(t, e.row(t), map[string]string{"state": string(StateCreating), "generation": "4"})
	})
	t.Run("fresh suspend stays", func(t *testing.T) {
		e := newFenceEnv(t, creating, false)
		e.sp.afterStart = func() { _ = patchWrite(MetadataPatch{"state": string(StateSuspended)})(e.store, e.id) }
		if err := e.attach(); err != nil {
			t.Fatalf("attach: %v", err)
		}
		wantRow(t, e.row(t), map[string]string{"state": string(StateSuspended), "pending_create_claim": ""})
	})
	t.Run("nothing to confirm reads and writes nothing", func(t *testing.T) {
		e := newFenceEnv(t, heldRow(map[string]string{"state": string(StateActive), "suspended_at": "", "held_until": "", "sleep_intent": ""}), true)
		rec := &rowWriteRecorder{Store: e.store}
		counter := &getCounter{Store: rec}
		e.mgr = NewManagerWithOptions(counter, e.sp, WithClock(&clock.Fake{Time: resumeNow}))
		if err := e.attach(); err != nil || len(rec.writes) != 0 || counter.gets != 1 {
			t.Fatalf("attach = %v, writes = %v, reads = %d; want none beyond the attach's own read", err, rec.writes, counter.gets)
		}
	})
}

// TestSuspendDuringResumeRestamps (review finding 10): an operator suspend of
// a row that still reads suspended while a resume's fence is live writes a
// fresh suspended_at, so the resume's consume refuses; without a fence, or
// from the shutdown sweep, it stays the no-op it was.
func TestSuspendDuringResumeRestamps(t *testing.T) {
	fence := resumeNow.Add(-time.Second).Format(time.RFC3339Nano)
	for name, tc := range map[string]struct {
		meta    map[string]string
		suspend func(*Manager, string) error
		changed bool
	}{
		"operator, live fence": {meta: map[string]string{ResumePendingAtKey: fence}, suspend: (*Manager).Suspend, changed: true},
		"operator, no fence":   {suspend: (*Manager).Suspend},
		"shutdown, live fence": {meta: map[string]string{ResumePendingAtKey: fence}, suspend: (*Manager).SuspendForShutdown},
	} {
		t.Run(name, func(t *testing.T) {
			e := newFenceEnv(t, heldRow(tc.meta), false)
			if err := tc.suspend(e.mgr, e.id); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			if changed := e.row(t)["suspended_at"] != heldRow(nil)["suspended_at"]; changed != tc.changed {
				t.Fatalf("suspended_at restamped = %v, want %v", changed, tc.changed)
			}
		})
	}
}

// getCounter counts the row reads that reach the store.
type getCounter struct {
	beads.Store
	gets int
}

func (g *getCounter) Get(id string) (beads.Bead, error) {
	g.gets++
	return g.Store.Get(id)
}
