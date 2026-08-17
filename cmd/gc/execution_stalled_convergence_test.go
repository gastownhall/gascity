package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The convergence chain for a claim that never became execution.
//
// This is the liveness half of the execution backstop, and it is load-bearing in
// a way the nudges are not: with the assigned-work wake signal repaired (F3), a
// live session holding an in_progress claim is kept awake BY DESIGN, so the
// no-wake-reason drain that used to recycle wedged seats by accident no longer
// fires for them. If the backstop's exhaustion did not converge, this branch
// would trade an accidental recovery for none at all.
//
// The chain: exhaustion -> TRACKED drain (non-cancelable reason) -> reconciler
// advances it -> runtime stops -> session bead closes -> the dead-assignee
// reopen lane releases the claim -> the row is demand again -> a fresh seat
// claims it.

type stalledConvergenceHarness struct {
	env         *reconcilerTestEnv
	template    string
	sessionID   string
	sessionName string
	work        beads.Bead
	rec         *events.Fake
}

type executionTokenReadErrorProvider struct {
	*runtime.Fake
}

func (p *executionTokenReadErrorProvider) GetMeta(name, key string) (string, error) {
	if key == "GC_INSTANCE_TOKEN" {
		return "", errors.New("runtime token read failed")
	}
	return p.Fake.GetMeta(name, key)
}

type completeClaimOnTokenReadProvider struct {
	*runtime.Fake
	store     beads.Store
	workID    string
	armed     bool
	triggered bool
}

func (p *completeClaimOnTokenReadProvider) GetMeta(name, key string) (string, error) {
	if key == "GC_INSTANCE_TOKEN" && p.armed {
		p.armed = false
		p.triggered = true
		if err := p.store.Close(p.workID); err != nil {
			return "", err
		}
	}
	return p.Fake.GetMeta(name, key)
}

// testWrappedStore declares the conditional-writes resolve target so the
// metadata CAS capability lookup (used by the distributed hook activity gate)
// reaches the wrapped backing store through these fault-injection wrappers,
// exactly like the production class wrappers do.
type testWrappedStore struct{ beads.Store }

func (s testWrappedStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

type failSecondRunningSessionGetStore struct {
	testWrappedStore
	sp          *runtime.Fake
	sessionID   string
	sessionName string
	runningGets int
}

func (s *failSecondRunningSessionGetStore) Get(id string) (beads.Bead, error) {
	if id == s.sessionID && s.sp.IsRunning(s.sessionName) {
		s.runningGets++
		if s.runningGets == 2 {
			return beads.Bead{}, errors.New("ambiguous pre-stop session probe")
		}
	}
	return s.Store.Get(id)
}

type failSecondStoppedSessionGetStore struct {
	testWrappedStore
	sp          *runtime.Fake
	sessionID   string
	sessionName string
	stoppedGets int
	failed      bool
}

func (s *failSecondStoppedSessionGetStore) Get(id string) (beads.Bead, error) {
	if id == s.sessionID && !s.sp.IsRunning(s.sessionName) && !s.failed {
		s.stoppedGets++
		if s.stoppedGets == 2 {
			s.failed = true
			return beads.Bead{}, errors.New("ambiguous post-stop session probe")
		}
	}
	return s.Store.Get(id)
}

type failNextSessionMetadataStore struct {
	testWrappedStore
	sessionID string
	failNext  bool
}

type failNextSessionMetadataTx struct {
	beads.Tx
	store *failNextSessionMetadataStore
}

type sequentialFailCloseStore struct {
	testWrappedStore
	sessionID                 string
	failNextClose             bool
	failNextRollback          bool
	failPostCloseMetadata     bool
	postCloseMetadataFailures int
}

type sequentialFailCloseTx struct {
	store *sequentialFailCloseStore
}

type completeClaimOnGuardSessionReadStore struct {
	testWrappedStore
	sessionID   string
	workID      string
	armed       bool
	sessionGets int
	triggered   bool
}

func (s *completeClaimOnGuardSessionReadStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err != nil || !s.armed || id != s.sessionID {
		return b, err
	}
	s.sessionGets++
	// The execution advance performs two reads for its liveness observation;
	// the third is the guard's authoritative incarnation read. Complete the
	// claim there. The immediately following work proof must see it before Stop.
	if s.sessionGets == 3 {
		s.triggered = true
		s.armed = false
		if closeErr := s.Close(s.workID); closeErr != nil {
			return beads.Bead{}, closeErr
		}
	}
	return b, nil
}

func (s *failNextSessionMetadataStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.sessionID && s.failNext {
		s.failNext = false
		return errors.New("transient drain completion write failure")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *failNextSessionMetadataStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.sessionID && s.failNext {
		s.failNext = false
		return errors.New("transient drain completion write failure")
	}
	return s.Store.Update(id, opts)
}

func (s *failNextSessionMetadataStore) Tx(commitMsg string, fn func(beads.Tx) error) error {
	return s.Store.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&failNextSessionMetadataTx{Tx: tx, store: s})
	})
}

func (tx *failNextSessionMetadataTx) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == tx.store.sessionID && tx.store.failNext {
		tx.store.failNext = false
		return errors.New("transient drain completion write failure")
	}
	return tx.Tx.SetMetadataBatch(id, kvs)
}

func (s *sequentialFailCloseStore) Tx(_ string, fn func(beads.Tx) error) error {
	return fn(&sequentialFailCloseTx{store: s})
}

func (s *sequentialFailCloseStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.sessionID && s.failNextRollback {
		_, hasCloseReason := opts.Metadata["close_reason"]
		_, hasClosedAt := opts.Metadata["closed_at"]
		if hasCloseReason && hasClosedAt && opts.Metadata["state"] != executionStalledDrainReason {
			s.failNextRollback = false
			return errors.New("transient non-atomic rollback update failure")
		}
	}
	return s.Store.Update(id, opts)
}

func (s *sequentialFailCloseStore) Close(id string) error {
	if id == s.sessionID && s.failNextClose {
		s.failNextClose = false
		return errors.New("transient non-atomic session close failure")
	}
	return s.Store.Close(id)
}

func (s *sequentialFailCloseStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.sessionID && s.failPostCloseMetadata {
		current, err := s.Get(id)
		if err == nil && current.Status == "closed" {
			s.failPostCloseMetadata = false
			s.postCloseMetadataFailures++
			return errors.New("transient terminal metadata failure after close")
		}
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (tx *sequentialFailCloseTx) Create(b beads.Bead) (beads.Bead, error) {
	return tx.store.Create(b)
}

func (tx *sequentialFailCloseTx) Update(id string, opts beads.UpdateOpts) error {
	return tx.store.Store.Update(id, opts)
}

func (tx *sequentialFailCloseTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return tx.store.Store.SetMetadataBatch(id, kvs)
}

func (tx *sequentialFailCloseTx) Close(id string) error {
	if id == tx.store.sessionID && tx.store.failNextClose {
		tx.store.failNextClose = false
		return errors.New("transient non-atomic session close failure")
	}
	return tx.store.Store.Close(id)
}

func newStalledConvergenceHarness(t *testing.T) *stalledConvergenceHarness {
	t.Helper()
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	template := "worker"
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              template,
			StartCommand:      "true",
			Nudge:             "gc hook --claim --drain-ack --json",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
	}

	manager := sessionpkg.NewManagerWithOptions(env.store, env.sp, sessionpkg.WithClock(env.clk))
	info, err := manager.CreateSession(t.Context(), sessionpkg.CreateOptions{
		BeadOnly: true, Template: template, Title: "pool worker", Command: "true", Provider: "fake",
	})
	if err != nil {
		t.Fatalf("creating the pool session: %v", err)
	}
	h := &stalledConvergenceHarness{env: env, template: template, sessionID: info.ID, sessionName: info.SessionName, rec: rec}

	if err := env.store.SetMetadataBatch(info.ID, map[string]string{
		"pool_managed": "true",
		"state":        "active",
	}); err != nil {
		t.Fatalf("marking the session pool-managed: %v", err)
	}
	// Let the reconciler start the seat itself, so the runtime, the session bead
	// and the desired state agree the way they do in production. Starting the
	// provider session behind the reconciler's back leaves a pending create it
	// rolls back ("live runtime belongs to another session").
	env.desiredState[h.sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  h.sessionName,
		TemplateName: template,
	}
	sessions, err := loadSessionBeads(env.store)
	if err != nil {
		t.Fatalf("loading session beads: %v", err)
	}
	env.reconcile(sessions)
	if !env.sp.IsRunning(h.sessionName) {
		t.Fatalf("the seat did not start; stdout=%s stderr=%s", env.stdout.String(), env.stderr.String())
	}
	// Production runs the execution backstop after reconciliation. Give the
	// freshly started seat its next reconciliation first so its persisted
	// active state is normalized to the reconciler's equivalent awake alias
	// before a later backstop drain pins the lifecycle snapshot.
	sessions, err = loadSessionBeads(env.store)
	if err != nil {
		t.Fatalf("reloading the started session bead: %v", err)
	}
	env.reconcile(sessions)
	startedInfo, err := sessionFrontDoor(env.store).Get(info.ID)
	if err != nil {
		t.Fatalf("loading the started session incarnation: %v", err)
	}
	if startedInfo.InstanceToken == "" {
		t.Fatal("started session has no persisted instance token")
	}
	if err := env.sp.SetMeta(h.sessionName, "GC_INSTANCE_TOKEN", startedInfo.InstanceToken); err != nil {
		t.Fatalf("stamping the fake runtime instance token: %v", err)
	}

	work, err := env.store.Create(beads.Bead{
		Title:    "claimed but never executed",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: template, beadmeta.RootBeadIDMetadataKey: "root-1"},
	})
	if err != nil {
		t.Fatalf("seeding work: %v", err)
	}
	inProgress := "in_progress"
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &h.sessionName}); err != nil {
		t.Fatalf("claiming work: %v", err)
	}
	h.work, _ = env.store.Get(work.ID)
	return h
}

func (h *stalledConvergenceHarness) sessionBead(t *testing.T) beads.Bead {
	t.Helper()
	bead, err := h.env.store.Get(h.sessionID)
	if err != nil {
		t.Fatalf("re-reading the session bead: %v", err)
	}
	return bead
}

// drainRequester is the production wiring's shape: a tracked drain with the
// non-cancelable execution-stalled reason.
func (h *stalledConvergenceHarness) drainRequester(t *testing.T) executionStalledDrainRequester {
	return h.drainRequesterAtCityPath(t, "")
}

func (h *stalledConvergenceHarness) drainRequesterAtCityPath(t *testing.T, cityPath string) executionStalledDrainRequester {
	t.Helper()
	return func(sessionBead beads.Bead, target backstopTarget) (backstopResolution, error) {
		sessFront := sessionFrontDoor(h.env.store)
		info, err := sessFront.Get(sessionBead.ID)
		if err != nil {
			return backstopResolutionHold, err
		}
		guard := executionStalledDrainActionGuard(cityPath, h.env.store, sessFront, info, target)
		return guard(func(latest sessionpkg.Info) error {
			beginSessionDrainInfoWithActionGuard(latest, h.env.dt, executionStalledDrainReason, h.env.clk, defaultDrainTimeout, guard)
			return nil
		})
	}
}

func (h *stalledConvergenceHarness) controllerDrainRequester(cityPath string) executionStalledDrainRequester {
	return func(sessionBead beads.Bead, target backstopTarget) (backstopResolution, error) {
		cr := &CityRuntime{
			cityPath:            cityPath,
			cfg:                 h.env.cfg,
			sessionDrains:       h.env.dt,
			standaloneCityStore: h.env.store,
		}
		return cr.requestExecutionStalledDrain(sessionBead, target)
	}
}

func (h *stalledConvergenceHarness) tickBackstop(t *testing.T, now time.Time, requestDrain executionStalledDrainRequester) {
	t.Helper()
	h.env.sp.SetActivity(h.sessionName, now.Add(-time.Hour))
	sessions, err := loadSessionBeads(h.env.store)
	if err != nil {
		t.Fatalf("loading session beads: %v", err)
	}
	work, err := h.env.store.List(beads.ListQuery{Status: "in_progress"})
	if err != nil {
		t.Fatalf("listing work: %v", err)
	}
	stores := make([]beads.Store, len(work))
	refs := make([]string, len(work))
	for j := range work {
		stores[j] = h.env.store
	}
	nudgeStalledPoolExecution(h.env.sp, h.env.cfg, h.env.store, sessions, work, stores, refs, false, "",
		now, h.env.rec, requestDrain, &h.env.stdout)
}

func (h *stalledConvergenceHarness) stalledEventCount() int {
	count := 0
	for _, event := range h.rec.Events {
		if event.Type == events.ExecutionStepStalled {
			count++
		}
	}
	return count
}

func (h *stalledConvergenceHarness) nudgeCount() int {
	return strings.Count(h.env.stdout.String(), "execution-claim-nudge: nudged")
}

// runBackstopToExhaustion drives the backstop past its grace and every attempt.
func (h *stalledConvergenceHarness) runBackstopToExhaustion(t *testing.T) {
	h.runBackstopToExhaustionWithRequester(t, h.drainRequester(t))
}

func (h *stalledConvergenceHarness) runBackstopToExhaustionWithRequester(t *testing.T, requestDrain executionStalledDrainRequester) {
	t.Helper()
	now := h.env.clk.Now()
	for i := 0; i <= idleClaimNudgeMaxAttempts+1; i++ {
		h.tickBackstop(t, now, requestDrain)
		now = now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
	}
}

// TestExecutionStalledDrainReconstructsAfterControllerRestartAndConverges is
// the end-to-end chain with the process-local failure boundary made explicit.
func TestExecutionStalledDrainReconstructsAfterControllerRestartAndConverges(t *testing.T) {
	h := newStalledConvergenceHarness(t)

	h.runBackstopToExhaustion(t)

	ds := h.env.dt.get(h.sessionID)
	if ds == nil {
		t.Fatal("exhaustion did not begin a TRACKED drain; nothing else converges a live seat holding a claim")
	}
	if ds.reason != executionStalledDrainReason {
		t.Fatalf("drain reason = %q, want %q", ds.reason, executionStalledDrainReason)
	}
	if got := h.stalledEventCount(); got != 1 {
		t.Fatalf("execution.step_stalled events before restart = %d, want exactly 1", got)
	}
	if got := h.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("nudges before restart = %d, want the attempt cap %d", got, idleClaimNudgeMaxAttempts)
	}

	// A controller restart retains the durable session marker but loses every
	// process-local tracked drain. The next matching-claim tick must reconstruct
	// the non-cancelable drain without replaying its observable escalation.
	h.env.dt = newDrainTracker()
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("fresh controller unexpectedly retained drain state: %+v", ds)
	}
	h.tickBackstop(t, h.env.clk.Now().Add(time.Hour), h.drainRequester(t))
	ds = h.env.dt.get(h.sessionID)
	if ds == nil || ds.reason != executionStalledDrainReason {
		t.Fatalf("drain after controller restart = %+v, want reconstructed %q drain", ds, executionStalledDrainReason)
	}
	if got := h.stalledEventCount(); got != 1 {
		t.Fatalf("execution.step_stalled events after restart = %d, want still 1", got)
	}
	if got := h.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("nudges after restart = %d, want still %d", got, idleClaimNudgeMaxAttempts)
	}

	// The reconciler advances the tracked drain: deferred interrupt, then stop.
	for i := 0; i < 6 && h.env.sp.IsRunning(h.sessionName); i++ {
		h.env.clk.Advance(defaultDrainTimeout + time.Minute)
		sessions, err := loadSessionBeads(h.env.store)
		if err != nil {
			t.Fatalf("loading session beads: %v", err)
		}
		h.env.reconcile(sessions)
	}
	if h.env.sp.IsRunning(h.sessionName) {
		t.Fatalf("the runtime is still running after the drain advanced; stdout=%s stderr=%s",
			h.env.stdout.String(), h.env.stderr.String())
	}

	// The execution-stalled drain owns the whole terminal transition. Stopping
	// the process but leaving its bead open/asleep would let assigned-work demand
	// wake the same wedged incarnation again before the orphan-release lane can
	// recover its claim.
	closedSession, err := h.env.store.Get(h.sessionID)
	if err != nil {
		t.Fatalf("re-reading the terminal session bead: %v", err)
	}
	if closedSession.Status != "closed" {
		t.Fatalf("terminal session status = %q, want closed without a manual close", closedSession.Status)
	}
	if closedSession.Metadata["state"] != executionStalledDrainReason {
		t.Fatalf("terminal session state = %q, want %q", closedSession.Metadata["state"], executionStalledDrainReason)
	}

	// With the owner now durably terminal, let the dead-assignee reopen lane run:
	// the claim must come back as claimable work rather than staying held by a
	// session that no longer exists.
	claimed, err := h.env.store.Get(h.work.ID)
	if err != nil {
		t.Fatalf("re-reading the claim: %v", err)
	}
	if strings.TrimSpace(claimed.Assignee) != "" || strings.EqualFold(strings.TrimSpace(claimed.Status), "in_progress") {
		released := releaseOrphanedPoolAssignments(h.env.store, h.env.cfg, "", nil,
			[]beads.Bead{claimed}, []beads.Store{h.env.store}, []string{""}, nil)
		if len(released) != 1 || released[0].ID != h.work.ID {
			t.Fatalf("released = %+v, want the stalled claim reopened", released)
		}
	}

	reopened, err := h.env.store.Get(h.work.ID)
	if err != nil {
		t.Fatalf("re-reading the reopened row: %v", err)
	}
	if !strings.EqualFold(strings.TrimSpace(reopened.Status), "open") || strings.TrimSpace(reopened.Assignee) != "" {
		t.Fatalf("reopened row = status %q assignee %q, want open and unassigned", reopened.Status, reopened.Assignee)
	}

	// And the loop closes: the row is countable demand again, so a fresh seat is
	// minted for it, and a fresh seat's query serves it.
	templates := map[string]struct{}{h.template: {}}
	if _, servable := demandServableForTemplates(h.env.cfg, reopened, templates); !servable {
		t.Fatal("the reopened row is not demand for its template; the chain does not close")
	}
	if !hookClaimMatchesRoute(reopened, hookClaimRouteTargets(h.template)) {
		t.Fatal("the reopened row is not claimable by a worker for its template")
	}

	// A later reconcile must not resurrect the retired incarnation or replay its
	// one-shot escalation. A fresh seat may be admitted separately by pool
	// desired-state construction; this closed session itself is never wakeable.
	eventsBefore := h.stalledEventCount()
	sessionRows, err := loadSessionBeads(h.env.store)
	if err != nil {
		t.Fatalf("loading sessions after terminal close: %v", err)
	}
	h.env.reconcile(sessionRows)
	afterReconcile, err := h.env.store.Get(h.sessionID)
	if err != nil {
		t.Fatalf("re-reading terminal session after reconcile: %v", err)
	}
	if afterReconcile.Status != "closed" || h.env.sp.IsRunning(h.sessionName) {
		t.Fatalf("retired session resurrected: status=%q running=%v", afterReconcile.Status, h.env.sp.IsRunning(h.sessionName))
	}
	if got := h.stalledEventCount(); got != eventsBefore {
		t.Fatalf("execution.step_stalled replayed after terminal close: %d -> %d", eventsBefore, got)
	}
}

func TestExecutionStalledDrainControllerRestartRejectsPostLatchLifecycleDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*stalledConvergenceHarness, *testing.T)
	}{
		{
			name: "restart requested",
			mutate: func(h *stalledConvergenceHarness, t *testing.T) {
				t.Helper()
				if err := h.env.store.SetMetadata(h.sessionID, "restart_requested", "true"); err != nil {
					t.Fatalf("requesting restart after stalled latch: %v", err)
				}
			},
		},
		{
			name: "explicit wake",
			mutate: func(h *stalledConvergenceHarness, t *testing.T) {
				t.Helper()
				info, err := sessionFrontDoor(h.env.store).GetLive(h.sessionID)
				if err != nil {
					t.Fatalf("loading pre-wake lifecycle: %v", err)
				}
				patch := sessionpkg.ClearWakeBlockersPatch(info.State, info.SleepReason)
				for key, value := range sessionpkg.RequestExplicitWakePatch(string(sessionpkg.WakeCauseExplicit), h.env.clk.Now().UTC()) {
					patch[key] = value
				}
				patch["wake_request_token"] = "post-latch-wake-token"
				if err := h.env.store.SetMetadataBatch(h.sessionID, patch); err != nil {
					t.Fatalf("persisting explicit wake after stalled latch: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newStalledConvergenceHarness(t)
			h.runBackstopToExhaustion(t)
			if h.stalledEventCount() != 1 {
				t.Fatal("precondition: stalled escalation was not latched exactly once")
			}

			tc.mutate(h, t)
			h.env.dt = newDrainTracker() // controller restart loses process-local drains
			h.tickBackstop(t, h.env.clk.Now().Add(time.Hour), h.controllerDrainRequester(""))

			if ds := h.env.dt.get(h.sessionID); ds != nil {
				t.Fatalf("post-latch lifecycle drift reconstructed a drain: %+v", ds)
			}
			if !h.env.sp.IsRunning(h.sessionName) {
				t.Fatal("post-latch lifecycle drift stopped the runtime during reconstruction")
			}
			current, err := h.env.store.Get(h.sessionID)
			if err != nil {
				t.Fatalf("loading session after rejected reconstruction: %v", err)
			}
			if current.Status == "closed" {
				t.Fatal("post-latch lifecycle drift closed the session during reconstruction")
			}
			if got := h.stalledEventCount(); got != 1 {
				t.Fatalf("execution.step_stalled events after rejected reconstruction = %d, want 1", got)
			}
			if got := h.nudgeCount(); got != idleClaimNudgeMaxAttempts {
				t.Fatalf("nudges after rejected reconstruction = %d, want %d", got, idleClaimNudgeMaxAttempts)
			}
		})
	}
}

func TestExecutionStalledDrainLegacyLifecycleAuthorityHoldsWithoutRearming(t *testing.T) {
	for _, authority := range []string{"", "v1:sha256:not-a-digest"} {
		name := "missing"
		if authority != "" {
			name = "malformed"
		}
		t.Run(name, func(t *testing.T) {
			h := newStalledConvergenceHarness(t)
			h.runBackstopToExhaustion(t)
			before := h.sessionBead(t)
			if strings.TrimSpace(before.Metadata[executionClaimNudgeStalledKey]) == "" {
				t.Fatal("precondition: stalled latch is absent")
			}
			if err := h.env.store.SetMetadata(h.sessionID, "execution_claim_nudge_lifecycle_authority", authority); err != nil {
				t.Fatalf("installing legacy lifecycle authority: %v", err)
			}

			h.env.dt = newDrainTracker()
			h.tickBackstop(t, h.env.clk.Now().Add(time.Hour), h.controllerDrainRequester(""))

			if ds := h.env.dt.get(h.sessionID); ds != nil {
				t.Fatalf("legacy lifecycle authority reconstructed a drain: %+v", ds)
			}
			after := h.sessionBead(t)
			if after.Metadata[executionClaimNudgeStalledKey] != before.Metadata[executionClaimNudgeStalledKey] {
				t.Fatalf("legacy stalled latch changed from %q to %q", before.Metadata[executionClaimNudgeStalledKey], after.Metadata[executionClaimNudgeStalledKey])
			}
			if after.Metadata[executionClaimNudgeCountKey] != before.Metadata[executionClaimNudgeCountKey] {
				t.Fatalf("legacy attempt budget rearmed from %q to %q", before.Metadata[executionClaimNudgeCountKey], after.Metadata[executionClaimNudgeCountKey])
			}
			if got := h.stalledEventCount(); got != 1 {
				t.Fatalf("legacy authority replayed stalled event: got %d", got)
			}
			if got := h.nudgeCount(); got != idleClaimNudgeMaxAttempts {
				t.Fatalf("legacy authority replayed nudges: got %d want %d", got, idleClaimNudgeMaxAttempts)
			}
		})
	}
}

func TestExecutionStalledDrainClosesBeforeSplitStoreOrphanRelease(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	rigStore := beads.NewMemStore()
	rigWork, err := rigStore.Create(beads.Bead{
		Title: "split-store claimed but never executed",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:   h.template,
			beadmeta.RootBeadIDMetadataKey: "root-split",
		},
	})
	if err != nil {
		t.Fatalf("creating split-store work: %v", err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(rigWork.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &h.sessionName}); err != nil {
		t.Fatalf("claiming split-store work: %v", err)
	}
	rigWork, err = rigStore.Get(rigWork.ID)
	if err != nil {
		t.Fatalf("reading split-store claim: %v", err)
	}

	requestDrain := h.drainRequester(t)
	now := h.env.clk.Now()
	for i := 0; i <= idleClaimNudgeMaxAttempts+1; i++ {
		h.env.sp.SetActivity(h.sessionName, now.Add(-time.Hour))
		sessionRows, loadErr := loadSessionBeads(h.env.store)
		if loadErr != nil {
			t.Fatalf("loading session beads: %v", loadErr)
		}
		nudgeStalledPoolExecution(
			h.env.sp,
			h.env.cfg,
			h.env.store,
			sessionRows,
			[]beads.Bead{rigWork},
			[]beads.Store{rigStore},
			[]string{"rig:test"},
			false,
			"",
			now,
			h.env.rec,
			requestDrain,
			&h.env.stdout,
		)
		now = now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
	}
	if ds := h.env.dt.get(h.sessionID); ds == nil {
		t.Fatal("split-store exhaustion did not enqueue a drain")
	}

	for i := 0; i < 6 && h.env.sp.IsRunning(h.sessionName); i++ {
		h.env.clk.Advance(defaultDrainTimeout + time.Minute)
		sessionRows, loadErr := loadSessionBeads(h.env.store)
		if loadErr != nil {
			t.Fatalf("loading session beads during drain: %v", loadErr)
		}
		h.env.reconcile(sessionRows)
	}
	closedSession, err := h.env.store.Get(h.sessionID)
	if err != nil {
		t.Fatalf("reading closed session: %v", err)
	}
	if closedSession.Status != "closed" {
		t.Fatalf("split-store session status = %q, want closed", closedSession.Status)
	}
	stillClaimed, err := rigStore.Get(rigWork.ID)
	if err != nil {
		t.Fatalf("reading split-store work before orphan release: %v", err)
	}
	if stillClaimed.Status != "in_progress" || stillClaimed.Assignee != h.sessionName {
		t.Fatalf("split-store work changed before orphan pass: status=%q assignee=%q", stillClaimed.Status, stillClaimed.Assignee)
	}

	released := releaseOrphanedPoolAssignments(
		h.env.store,
		h.env.cfg,
		"",
		nil,
		[]beads.Bead{stillClaimed},
		[]beads.Store{rigStore},
		[]string{"rig:test"},
		map[string]beads.Store{"test": rigStore},
	)
	if len(released) != 1 || released[0].ID != rigWork.ID {
		t.Fatalf("split-store released = %+v, want %s", released, rigWork.ID)
	}
	reopened, err := rigStore.Get(rigWork.ID)
	if err != nil {
		t.Fatalf("reading reopened split-store work: %v", err)
	}
	if reopened.Status != "open" || strings.TrimSpace(reopened.Assignee) != "" {
		t.Fatalf("reopened split-store work = status %q assignee %q, want open/unassigned", reopened.Status, reopened.Assignee)
	}
}

// TestExecutionStalledDrainSurvivesTheKeepAliveGuards is the reason this drain
// has its own reason. The session is awake, running, and holding an in_progress
// claim — the exact shape every cancel lens protects — so a cancelable reason
// would be canceled by the very claim that justified draining it, and the seat
// would stay wedged forever.
func TestExecutionStalledDrainSurvivesTheKeepAliveGuards(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	info := sessiontest.SeedBead(t, h.sessionBead(t))
	beginSessionDrainInfo(info, h.env.sp, h.env.dt, executionStalledDrainReason, h.env.clk, defaultDrainTimeout)

	for _, tt := range []struct {
		name   string
		cancel func() bool
	}{
		{"wake reasons reappear", func() bool { return cancelSessionDrainInfo(info, h.env.sp, h.env.dt) }},
		{"pending interaction", func() bool { return cancelSessionDrainForPendingInfo(info, h.env.sp, h.env.dt) }},
		{"assigned work", func() bool { return cancelSessionDrainForAssignedWorkInfo(info, h.env.sp, h.env.dt) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.cancel() {
				t.Fatalf("%s canceled the execution-stalled drain; the wedged seat would never converge", tt.name)
			}
			if ds := h.env.dt.get(h.sessionID); ds == nil || ds.reason != executionStalledDrainReason {
				t.Fatalf("drain state after %s = %+v, want the execution-stalled drain intact", tt.name, ds)
			}
		})
	}
}

// Enqueue authority is not a lease. If the claim completes after the tracker is
// created but before its first stop, the retained boundary guard must retire the
// otherwise non-cancelable drain and spare the runtime.
func TestExecutionStalledDrainRevalidatesAgainBeforeFirstStop(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	h.runBackstopToExhaustion(t)
	ds := h.env.dt.get(h.sessionID)
	if ds == nil || ds.actionGuard == nil {
		t.Fatalf("guarded execution-stalled drain = %+v, want a retained authority guard", ds)
	}
	if err := h.env.store.Close(h.work.ID); err != nil {
		t.Fatalf("completing the claim after enqueue: %v", err)
	}

	info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session info: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)

	if !h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("runtime was stopped after its claim completed before the first stop boundary")
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("stale non-cancelable tracker survived claim completion: %+v", ds)
	}
}

func TestExecutionStalledDrainDefersWhileCityLifecycleLockIsHeld(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	cityPath := t.TempDir()
	h.runBackstopToExhaustionWithRequester(t, h.drainRequesterAtCityPath(t, cityPath))
	if ds := h.env.dt.get(h.sessionID); ds == nil || ds.actionGuard == nil {
		t.Fatalf("guarded execution-stalled drain = %+v, want retained authority", ds)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- sessionpkg.WithCitySessionLifecycleLock(cityPath, h.sessionID, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		close(release)
		<-done
		t.Fatalf("loading session info: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		cityPath,
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)
	close(release)
	if lockErr := <-done; lockErr != nil {
		t.Fatalf("holding lifecycle lock: %v", lockErr)
	}
	if !h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("runtime stopped while another lifecycle transition held the city lock")
	}
	if ds := h.env.dt.get(h.sessionID); ds == nil {
		t.Fatal("tracker retired while the city lifecycle lock was busy; want hold/retry")
	}
}

func TestExecutionStalledDrainRejectsRestartRequestedBehindStaleSnapshot(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	h.runBackstopToExhaustion(t)
	staleInfo, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading pre-restart session info: %v", err)
	}
	if err := h.env.store.SetMetadata(h.sessionID, "restart_requested", "true"); err != nil {
		t.Fatalf("requesting a restart behind the drain snapshot: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return staleInfo, id == staleInfo.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)
	if !h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("runtime stopped after a newer restart request superseded the drain")
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("tracker survived a newer restart request: %+v", ds)
	}
}

func TestExecutionStalledDrainRejectsForeignRuntimeSessionID(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	h.runBackstopToExhaustion(t)
	if err := h.env.sp.SetMeta(h.sessionName, "GC_SESSION_ID", "foreign-session"); err != nil {
		t.Fatalf("stamping foreign live runtime session id: %v", err)
	}
	info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session info: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)
	if !h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("runtime stopped when GC_SESSION_ID belonged to another session")
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("tracker survived a positive foreign runtime identity: %+v", ds)
	}
}

// A deleted work row is authoritative completion, not an ambiguous store read.
// The retained non-cancelable tracker must therefore retire at the first action
// boundary instead of holding forever on ErrNotFound.
func TestExecutionStalledDrainRetiresWhenClaimDeletedBeforeFirstStop(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	h.runBackstopToExhaustion(t)
	ds := h.env.dt.get(h.sessionID)
	if ds == nil || ds.actionGuard == nil {
		t.Fatalf("guarded execution-stalled drain = %+v, want a retained authority guard", ds)
	}
	if err := h.env.store.Delete(h.work.ID); err != nil {
		t.Fatalf("deleting the claim after enqueue: %v", err)
	}

	info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session info: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)

	if !h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("runtime was stopped after its claim was deleted before the first stop boundary")
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("non-cancelable tracker survived authoritative claim deletion: %+v", ds)
	}
}

func TestExecutionStalledDrainRetiresWhenCurrentAliasRotatesBeforeFirstStop(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	oldAlias := "nux"
	if err := h.env.store.SetMetadataBatch(h.sessionID, map[string]string{"alias": oldAlias}); err != nil {
		t.Fatalf("setting initial session alias: %v", err)
	}
	if err := h.env.store.Update(h.work.ID, beads.UpdateOpts{Assignee: &oldAlias}); err != nil {
		t.Fatalf("assigning work through the initial alias: %v", err)
	}
	h.runBackstopToExhaustion(t)
	ds := h.env.dt.get(h.sessionID)
	if ds == nil || ds.actionGuard == nil {
		t.Fatalf("guarded execution-stalled drain = %+v, want a retained authority guard", ds)
	}
	if err := h.env.store.SetMetadataBatch(h.sessionID, map[string]string{"alias": "rictus"}); err != nil {
		t.Fatalf("rotating the current session alias: %v", err)
	}

	info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session info: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)

	if !h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("runtime was stopped after the work assignee ceased to be a current session identity")
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("tracker survived current-alias rotation: %+v", ds)
	}
}

func TestExecutionStalledDrainDoesNotOverwriteConcurrentSuspend(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	h.runBackstopToExhaustion(t)
	staleInfo, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading pre-suspend session info: %v", err)
	}
	manager := sessionpkg.NewManagerWithOptions(h.env.store, h.env.sp)
	if err := manager.Suspend(h.sessionID); err != nil {
		t.Fatalf("suspending session after drain enqueue: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return staleInfo, id == staleInfo.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)

	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("tracker survived an explicit suspend: %+v", ds)
	}
	latest, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading suspended session: %v", err)
	}
	if latest.State != sessionpkg.StateSuspended {
		t.Fatalf("state after guarded completion = %q, want suspended (not overwritten with asleep)", latest.State)
	}
}

func TestExecutionStalledDrainRetiresWhenSessionClosesBeforeFirstStop(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	h.runBackstopToExhaustion(t)
	staleInfo, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading pre-close session info: %v", err)
	}
	if err := h.env.store.Close(h.sessionID); err != nil {
		t.Fatalf("closing session after drain enqueue: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return staleInfo, id == staleInfo.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)

	if !h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("runtime was stopped after the session bead had already closed")
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("tracker survived a closed session incarnation: %+v", ds)
	}
	closed, err := h.env.store.Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading closed session: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("session status = %q, want closed", closed.Status)
	}
}

func TestExecutionStalledDrainRevalidatesWorkAfterLastSessionRead(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	hookedStore := &completeClaimOnGuardSessionReadStore{
		testWrappedStore: testWrappedStore{Store: h.env.store}, sessionID: h.sessionID, workID: h.work.ID,
	}
	h.env.store = hookedStore
	h.runBackstopToExhaustion(t)
	hookedStore.sessionGets = 0
	hookedStore.armed = true
	info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session info: %v", err)
	}
	// Exclude the explicit test setup read above from the guarded advance's
	// three-read sequence.
	hookedStore.sessionGets = 0
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)

	if !hookedStore.triggered {
		t.Fatal("claim-completion hook did not run on the guard's final session read")
	}
	if !h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("runtime was stopped after the claim completed at the final session-read boundary")
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("tracker survived claim completion at the final session-read boundary: %+v", ds)
	}
}

func TestExecutionStalledDrainHoldsWhenLiveRuntimeTokenIsUnproven(t *testing.T) {
	for _, tt := range []struct {
		name     string
		provider func(*testing.T, *runtime.Fake, string) runtime.Provider
	}{
		{
			name: "missing token",
			provider: func(t *testing.T, sp *runtime.Fake, sessionName string) runtime.Provider {
				t.Helper()
				if err := sp.SetMeta(sessionName, "GC_INSTANCE_TOKEN", ""); err != nil {
					t.Fatalf("clearing fake runtime token: %v", err)
				}
				return sp
			},
		},
		{
			name: "token read error",
			provider: func(_ *testing.T, sp *runtime.Fake, _ string) runtime.Provider {
				return &executionTokenReadErrorProvider{Fake: sp}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newStalledConvergenceHarness(t)
			h.runBackstopToExhaustion(t)
			if ds := h.env.dt.get(h.sessionID); ds == nil || ds.actionGuard == nil {
				t.Fatalf("guarded execution-stalled drain = %+v, want retained authority", ds)
			}
			sp := tt.provider(t, h.env.sp, h.sessionName)
			info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
			if err != nil {
				t.Fatalf("loading session info: %v", err)
			}
			advanceSessionDrainsWithSessionsTraced(
				"",
				h.env.dt,
				sp,
				h.env.store,
				func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
				map[string]wakeEvaluation{},
				h.env.cfg,
				h.env.clk,
				nil,
			)

			if !h.env.sp.IsRunning(h.sessionName) {
				t.Fatal("runtime was stopped without an exact live instance-token proof")
			}
			if ds := h.env.dt.get(h.sessionID); ds == nil {
				t.Fatal("tracker was retired for an ambiguous live-token read; want hold/retry")
			}
		})
	}
}

// Provider token verification may be remote I/O. If the claim completes while
// that read is in flight, the final action guard must re-read work afterward and
// retire the tracker without stopping the runtime.
func TestExecutionStalledDrainRevalidatesClaimAfterRuntimeTokenRead(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	h.runBackstopToExhaustion(t)
	provider := &completeClaimOnTokenReadProvider{
		Fake: h.env.sp, store: h.env.store, workID: h.work.ID, armed: true,
	}
	info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session info: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		provider,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)

	if !provider.triggered {
		t.Fatal("claim-completion hook did not run during runtime-token verification")
	}
	if !h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("runtime was stopped after its claim completed during runtime-token verification")
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("tracker survived claim completion during runtime-token verification: %+v", ds)
	}
}

func TestExecutionStalledDrainHoldsOnAmbiguousPreStopProbe(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	h.runBackstopToExhaustion(t)
	probeStore := &failSecondRunningSessionGetStore{
		testWrappedStore: testWrappedStore{Store: h.env.store}, sp: h.env.sp, sessionID: h.sessionID, sessionName: h.sessionName,
	}
	info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session info: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		probeStore,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)

	if !h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("runtime was stopped after an ambiguous pre-stop liveness probe")
	}
	if ds := h.env.dt.get(h.sessionID); ds == nil {
		t.Fatal("tracker was retired after an ambiguous pre-stop liveness probe")
	}
}

func TestExecutionStalledDrainHoldsOnAmbiguousPostStopProbe(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	probeStore := &failSecondStoppedSessionGetStore{
		testWrappedStore: testWrappedStore{Store: h.env.store}, sp: h.env.sp, sessionID: h.sessionID, sessionName: h.sessionName,
	}
	h.env.store = probeStore
	h.runBackstopToExhaustion(t)
	info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session info: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)

	if h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("strict stop did not stop the runtime before the scripted post-stop probe error")
	}
	if ds := h.env.dt.get(h.sessionID); ds == nil {
		t.Fatal("tracker was retired after an ambiguous post-stop liveness probe")
	}
	afterFailure, err := h.env.store.Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session after failed post-stop probe: %v", err)
	}
	if afterFailure.Metadata["state"] == "asleep" {
		t.Fatal("terminal drain metadata was written after an ambiguous post-stop probe")
	}

	// The scripted read failure is one-shot. A positive not-running probe on the
	// next tick completes the same retained tracker.
	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("tracker survived a successful retry probe: %+v", ds)
	}
}

func TestExecutionStalledDrainRetriesFailedTerminalMetadataWrite(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	failingStore := &failNextSessionMetadataStore{testWrappedStore: testWrappedStore{Store: h.env.store}, sessionID: h.sessionID}
	h.env.store = failingStore
	h.runBackstopToExhaustion(t)
	failingStore.failNext = true
	info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session info: %v", err)
	}
	advance := func() {
		advanceSessionDrainsWithSessionsTraced(
			"",
			h.env.dt,
			h.env.sp,
			h.env.store,
			func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
			map[string]wakeEvaluation{},
			h.env.cfg,
			h.env.clk,
			nil,
		)
	}
	advance()
	if h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("strict stop did not stop the runtime before the terminal write")
	}
	if ds := h.env.dt.get(h.sessionID); ds == nil {
		t.Fatal("tracker was retired after terminal metadata persistence failed")
	}
	afterFailure, err := h.env.store.Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session after terminal write failure: %v", err)
	}
	if afterFailure.Status == "closed" || afterFailure.Metadata["state"] == executionStalledDrainReason {
		t.Fatalf("failed terminal close leaked terminal state: status=%q state=%q", afterFailure.Status, afterFailure.Metadata["state"])
	}

	advance()
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("tracker survived successful terminal metadata retry: %+v", ds)
	}
	afterRetry, err := h.env.store.Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session after terminal retry: %v", err)
	}
	if afterRetry.Status != "closed" || afterRetry.Metadata["state"] != executionStalledDrainReason ||
		afterRetry.Metadata["close_reason"] != sessionpkg.CanonicalCloseReason(executionStalledDrainReason) {
		t.Fatalf("terminal metadata = status %q state %q close_reason %q, want closed/%q/%q",
			afterRetry.Status, afterRetry.Metadata["state"], afterRetry.Metadata["close_reason"],
			executionStalledDrainReason, sessionpkg.CanonicalCloseReason(executionStalledDrainReason))
	}
}

func TestExecutionStalledDrainRestoresLifecycleAfterPartialCloseFailure(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	failingStore := &sequentialFailCloseStore{testWrappedStore: testWrappedStore{Store: h.env.store}, sessionID: h.sessionID}
	h.env.store = failingStore
	h.runBackstopToExhaustion(t)
	failingStore.failNextClose = true
	info, err := sessionFrontDoor(h.env.store).Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session info: %v", err)
	}
	originalState := info.MetadataState
	advance := func() {
		advanceSessionDrainsWithSessionsTraced(
			"",
			h.env.dt,
			h.env.sp,
			h.env.store,
			func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
			map[string]wakeEvaluation{},
			h.env.cfg,
			h.env.clk,
			nil,
		)
	}
	advance()
	if h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("strict stop did not stop the runtime before the partial close")
	}
	if ds := h.env.dt.get(h.sessionID); ds == nil {
		t.Fatal("tracker retired after a partial non-atomic close failure")
	}
	afterFailure, err := h.env.store.Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session after partial close failure: %v", err)
	}
	if afterFailure.Status != "open" || afterFailure.Metadata["state"] != originalState ||
		afterFailure.Metadata["closed_at"] != "" || afterFailure.Metadata["close_reason"] != "" {
		t.Fatalf("partial close rollback = status %q state %q closed_at %q close_reason %q, want open/%q/empty/empty",
			afterFailure.Status, afterFailure.Metadata["state"], afterFailure.Metadata["closed_at"],
			afterFailure.Metadata["close_reason"], originalState)
	}

	advance()
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("tracker survived successful close retry: %+v", ds)
	}
	afterRetry, err := h.env.store.Get(h.sessionID)
	if err != nil {
		t.Fatalf("loading session after close retry: %v", err)
	}
	if afterRetry.Status != "closed" || afterRetry.Metadata["state"] != executionStalledDrainReason {
		t.Fatalf("terminal retry = status %q state %q, want closed/%q", afterRetry.Status, afterRetry.Metadata["state"], executionStalledDrainReason)
	}
}

func TestExecutionStalledDrainCloseFailureAndFailedRollbackRecoversAfterControllerRestart(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	failingStore := &sequentialFailCloseStore{
		testWrappedStore: testWrappedStore{Store: h.env.store},
		sessionID:        h.sessionID,
		failNextClose:    true,
		failNextRollback: true,
	}
	h.env.store = failingStore
	h.runBackstopToExhaustion(t)

	advance := func() {
		advanceSessionDrainsWithSessionsTraced(
			"",
			h.env.dt,
			h.env.sp,
			h.env.store,
			func(id string) (sessionpkg.Info, bool) {
				info, err := sessionFrontDoor(h.env.store).GetLive(id)
				return info, err == nil
			},
			map[string]wakeEvaluation{},
			h.env.cfg,
			h.env.clk,
			nil,
		)
	}
	advance()
	if h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("strict stop did not stop the runtime before the failed close")
	}
	if ds := h.env.dt.get(h.sessionID); ds == nil {
		t.Fatal("tracker retired in the same tick as the failed close and failed rollback")
	}
	afterFailure := h.sessionBead(t)
	if afterFailure.Status != "open" {
		t.Fatalf("failed close status = %q, want open", afterFailure.Status)
	}

	// Work-side cleanup may race ahead once ClosePatch lands. That cannot revoke
	// the irreversible terminal-close-pending authority.
	if err := h.env.store.Close(h.work.ID); err != nil {
		t.Fatalf("completing claim after partial close: %v", err)
	}

	// The controller crashes after both failures. Its tracker disappears, while
	// the durable stalled latch and exact ClosePatch tuple remain. The early
	// finalizer closes directly without revalidating the now-complete claim and
	// before any generic wake/heal lane can recreate the runtime.
	h.env.dt = newDrainTracker()
	startsBefore := h.env.sp.CountCalls("Start", h.sessionName)
	sessionRows, err := loadSessionBeads(h.env.store)
	if err != nil {
		t.Fatalf("loading sessions after controller restart: %v", err)
	}
	h.env.reconcile(sessionRows)
	if h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("controller restart re-woke a stopped session while its stalled latch awaited close recovery")
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("early terminal finalizer unexpectedly reconstructed a tracker: %+v", ds)
	}
	afterRecovery := h.sessionBead(t)
	if afterRecovery.Status != "closed" || afterRecovery.Metadata["state"] != executionStalledDrainReason {
		t.Fatalf("recovered session = status %q state %q, want closed/%q",
			afterRecovery.Status, afterRecovery.Metadata["state"], executionStalledDrainReason)
	}
	if h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("recovered terminal close re-woke the runtime")
	}
	if got := h.env.sp.CountCalls("Start", h.sessionName); got != startsBefore {
		t.Fatalf("controller restart started terminal-close-pending runtime: %d -> %d", startsBefore, got)
	}
}

func TestExecutionStalledDrainPartialCloseRequiresExactAuditTuple(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	failingStore := &sequentialFailCloseStore{
		testWrappedStore: testWrappedStore{Store: h.env.store},
		sessionID:        h.sessionID,
		failNextClose:    true,
		failNextRollback: true,
	}
	h.env.store = failingStore
	h.runBackstopToExhaustion(t)

	info, err := sessionFrontDoor(h.env.store).GetLive(h.sessionID)
	if err != nil {
		t.Fatalf("loading session before close: %v", err)
	}
	advanceSessionDrainsWithSessionsTraced(
		"", h.env.dt, h.env.sp, h.env.store,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{}, h.env.cfg, h.env.clk, nil,
	)

	afterClose := h.sessionBead(t)
	if afterClose.Status != "open" || afterClose.Metadata["state"] != executionStalledDrainReason {
		t.Fatalf("partial close precondition = status %q state %q", afterClose.Status, afterClose.Metadata["state"])
	}
	if err := h.env.store.SetMetadata(h.sessionID, "synced_at", h.env.clk.Now().Add(time.Second).UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("tampering ClosePatch audit tuple: %v", err)
	}
	h.env.dt = newDrainTracker()
	sessionRows, err := loadSessionBeads(h.env.store)
	if err != nil {
		t.Fatalf("loading closed sessions after controller restart: %v", err)
	}
	h.env.reconcile(sessionRows)
	afterRecovery := h.sessionBead(t)
	if afterRecovery.Status != "open" {
		t.Fatalf("mismatched ClosePatch audit tuple authorized close: status=%q", afterRecovery.Status)
	}
	if h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("mismatched ClosePatch audit tuple re-woke runtime")
	}
}

// Control: an ORDINARY drain reason is still cancelable by the same lenses. The
// non-cancelability above is a property of this reason, not a global change to
// how drains behave.
func TestOrdinaryDrainReasonsStayCancelable(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	info := sessiontest.SeedBead(t, h.sessionBead(t))
	beginSessionDrainInfo(info, h.env.sp, h.env.dt, "idle", h.env.clk, defaultDrainTimeout)

	if !cancelSessionDrainInfo(info, h.env.sp, h.env.dt) {
		t.Fatal("an idle drain is no longer cancelable; the non-cancelable reason leaked into the general path")
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		t.Fatalf("idle drain survived cancellation: %+v", ds)
	}
}

// An execution-stalled reason without its exact session+work guard is not
// authorized to enter the generic drain-ack lane. This pins fail-closed
// behavior for a malformed or partially reconstructed in-memory tracker.
func TestExecutionStalledDrainWithoutAuthorityGuardFailsClosed(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	info := sessiontest.SeedBead(t, h.sessionBead(t))
	beginSessionDrainInfo(info, h.env.sp, h.env.dt, executionStalledDrainReason, h.env.clk, defaultDrainTimeout)
	h.env.clk.Advance(defaultDrainTimeout + time.Minute)

	advanceSessionDrainsWithSessionsTraced(
		"",
		h.env.dt,
		h.env.sp,
		h.env.store,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{},
		h.env.cfg,
		h.env.clk,
		nil,
	)

	if !h.env.sp.IsRunning(h.sessionName) {
		t.Fatal("execution-stalled drain stopped the runtime without an authority guard")
	}
	if ds := h.env.dt.get(h.sessionID); ds == nil {
		t.Fatal("execution-stalled tracker without authority was retired instead of held")
	}
	if ack, _ := h.env.sp.GetMeta(h.sessionName, "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("execution-stalled tracker without authority set GC_DRAIN_ACK=%q", ack)
	}
}

// The adversarial case the council named: the agent wakes mid-drain and starts
// executing after the drain was requested. The claim must not be stranded EITHER
// way — the drain proceeds (the seat had its bounded chances), and the work is
// released rather than left held by a session on its way out.
func TestExecutionStalledDrainDoesNotStrandAMidDrainWake(t *testing.T) {
	h := newStalledConvergenceHarness(t)
	h.runBackstopToExhaustion(t)
	if ds := h.env.dt.get(h.sessionID); ds == nil {
		t.Fatal("no tracked drain after exhaustion")
	}

	// The agent wakes up and starts working: fresh activity, and the claim it
	// holds is genuinely in progress.
	h.env.sp.SetActivity(h.sessionName, h.env.clk.Now())
	if err := h.env.store.SetMetadataBatch(h.sessionID, map[string]string{"state": "active"}); err != nil {
		t.Fatalf("marking the session active: %v", err)
	}

	// The drain is not canceled by the wake — it already spent its chances.
	if ds := h.env.dt.get(h.sessionID); ds == nil || ds.reason != executionStalledDrainReason {
		t.Fatalf("the mid-drain wake canceled the drain: %+v", ds)
	}

	// The latched backstop remains level-triggered even when provider activity
	// changes: a restarted controller may report fresh activity while attaching.
	// Re-enqueue is an idempotent no-op for the already-tracked drain, and neither
	// the event nor the nudges may replay.
	beforeNudges := h.nudgeCount()
	beforeEvents := h.stalledEventCount()
	beforeDrain := h.env.dt.get(h.sessionID)
	sessions, err := loadSessionBeads(h.env.store)
	if err != nil {
		t.Fatalf("loading session beads: %v", err)
	}
	work, err := h.env.store.List(beads.ListQuery{Status: "in_progress"})
	if err != nil {
		t.Fatalf("listing work: %v", err)
	}
	stores := make([]beads.Store, len(work))
	refs := make([]string, len(work))
	for j := range work {
		stores[j] = h.env.store
	}
	nudgeStalledPoolExecution(h.env.sp, h.env.cfg, h.env.store, sessions, work, stores, refs, false, "",
		h.env.clk.Now(), h.env.rec, h.drainRequester(t), &h.env.stdout)

	if got := h.nudgeCount(); got != beforeNudges {
		t.Fatalf("nudges delivered to a now-active session: %d -> %d", beforeNudges, got)
	}
	if got := h.stalledEventCount(); got != beforeEvents {
		t.Fatalf("execution.step_stalled replayed for a now-active session: %d -> %d", beforeEvents, got)
	}
	if afterDrain := h.env.dt.get(h.sessionID); afterDrain == nil || afterDrain != beforeDrain {
		t.Fatalf("idempotent re-enqueue replaced the tracked drain: before=%+v after=%+v", beforeDrain, afterDrain)
	}

	// Whichever way the race resolves, the claim ends up actionable: either the
	// agent finishes it, or the session goes away and the reopen lane releases
	// it. What must NOT happen is in_progress work owned by a closed session.
	if err := h.env.store.Close(h.sessionID); err != nil {
		t.Fatalf("closing the session: %v", err)
	}
	claimed, err := h.env.store.Get(h.work.ID)
	if err != nil {
		t.Fatalf("re-reading the claim: %v", err)
	}
	released := releaseOrphanedPoolAssignments(h.env.store, h.env.cfg, "", nil,
		[]beads.Bead{claimed}, []beads.Store{h.env.store}, []string{""}, nil)
	if len(released) != 1 {
		t.Fatalf("released = %+v, want the claim released once its holder is gone", released)
	}
	reopened, err := h.env.store.Get(h.work.ID)
	if err != nil {
		t.Fatalf("re-reading the row: %v", err)
	}
	if strings.TrimSpace(reopened.Assignee) != "" {
		t.Fatalf("the row is still assigned to a closed session (%q): stranded", reopened.Assignee)
	}
}
