package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The pool loop under test, as the incident ran it (papercut pc_b969af2a45eb):
// a routed work bead with no live session is demand every tick; the pool
// creates a fresh session bead for it (pending_create_claim, gc.trigger_bead_id
// = the work bead); the start fails in pre_start; the reconciler rolls the
// session bead back and closes it failed-create; the next tick repeats. This
// harness drives exactly those two halves per tick — the demand gate
// (workStartDeferral, the predicate excludeStartDeferredWork and the scale_check
// counter apply) and the real reconcileSessionBeads commit path with a
// runtime.Fake whose Start fails — against one MemStore, so the record the
// policy keeps on the work bead is the only state between ticks.

const backoffHarnessTemplate = "helper"

type startBackoffHarness struct {
	t        *testing.T
	env      *reconcilerTestEnv
	policy   *workStartFailurePolicy
	work     beads.Bead
	mails    []parkedWorkNotice
	mailErr  error // returned by notify while set
	sessions int
	// startsAt records the clock time of every start the pool planned.
	startsAt []time.Time
}

func newStartBackoffHarness(t *testing.T, limit int) *startBackoffHarness {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: backoffHarnessTemplate, MaxActiveSessions: intPtr(1), MaxStartFailures: intPtr(limit)}}}
	work, err := env.store.Create(beads.Bead{
		Title:    "routed work whose pre_start keeps failing",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: backoffHarnessTemplate},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	// The incident shape: in_progress under the bare pool identity after a
	// handoff killed its session (Create always opens a bead; claim it after).
	inProgress, template := "in_progress", backoffHarnessTemplate
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &template}); err != nil {
		t.Fatalf("claim work bead: %v", err)
	}
	h := &startBackoffHarness{t: t, env: env, work: work}
	h.installPolicy()
	return h
}

// installPolicy builds a fresh policy over the same store — what a reconciler
// restart does (process memory gone, the store intact).
func (h *startBackoffHarness) installPolicy() {
	cfg := h.env.cfg
	h.policy = &workStartFailurePolicy{
		workStore: h.env.store,
		templateOf: func(b beads.Bead) string {
			return poolTemplateForWorkBead(cfg, b)
		},
		limitFor: func(template string) int {
			return findAgentByTemplate(cfg, template).EffectiveMaxStartFailures()
		},
		notify: func(n parkedWorkNotice) error {
			if h.mailErr != nil {
				return h.mailErr
			}
			h.mails = append(h.mails, n)
			return nil
		},
		stderr: &h.env.stderr,
	}
	h.env.startOptions = []startExecutionOption{
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		withWorkStartFailurePolicy(h.policy),
	}
}

func (h *startBackoffHarness) reload() beads.Bead {
	h.t.Helper()
	got, err := h.env.store.Get(h.work.ID)
	if err != nil {
		h.t.Fatalf("Get(work): %v", err)
	}
	return got
}

// tick is one reconciler pass: the demand gate decides whether the pool plans
// a start for the work bead; when it does, the pool's fresh session bead is
// created and the real reconciler starts it (startErr nil = the start
// succeeds). Returns whether a start was planned.
func (h *startBackoffHarness) tick(startErr error) bool {
	h.t.Helper()
	now := h.env.clk.Now()
	work := h.reload()
	// Both demand tiers answer with the same predicate: the assigned-work
	// filter and the scale_check counter must agree with workStartDeferral.
	if kept := excludeStartDeferredWork([]beads.Bead{work}, now, nil); len(kept) == 0 {
		if deferred, _, _ := workStartDeferral(work.Metadata, now); !deferred {
			h.t.Fatalf("excludeStartDeferredWork dropped a bead workStartDeferral keeps")
		}
		return false
	}
	h.sessions++
	name := fmt.Sprintf("sky-%d", h.sessions)
	session, err := h.env.store.Create(beads.Bead{
		Title:  backoffHarnessTemplate,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:" + backoffHarnessTemplate},
		Metadata: map[string]string{
			"session_name":                          name,
			"session_name_explicit":                 "true",
			"pending_create_claim":                  "true",
			"template":                              backoffHarnessTemplate,
			"state":                                 "creating",
			"generation":                            "1",
			"continuation_epoch":                    "1",
			"instance_token":                        "test-token",
			beadmeta.TriggerBeadIDMetadataKey:       h.work.ID,
			beadmeta.TriggerBeadStoreRefMetadataKey: "city",
		},
	})
	if err != nil {
		h.t.Fatalf("create session bead: %v", err)
	}
	h.env.desiredState = map[string]TemplateParams{name: {Command: "test-cmd", SessionName: name, TemplateName: backoffHarnessTemplate}}
	if startErr != nil {
		h.env.sp.StartErrors = map[string]error{name: startErr}
	} else {
		h.env.sp.StartErrors = nil
	}
	h.startsAt = append(h.startsAt, now)
	h.env.reconcileWithPoolDesired([]beads.Bead{session}, map[string]int{backoffHarnessTemplate: 1})
	got, err := h.env.store.Get(session.ID)
	if err != nil {
		h.t.Fatalf("Get(session): %v", err)
	}
	if startErr != nil && got.Status != "closed" {
		h.t.Fatalf("failed start must close the session bead failed-create, status=%q stderr=%s", got.Status, h.env.stderr.String())
	}
	if startErr == nil && got.Status == "closed" {
		h.t.Fatalf("successful start must not close the session bead: %s", h.env.stderr.String())
	}
	// The reconciler never touches the work bead's claim: it stays
	// in_progress under the pool identity so no other pool claims it.
	if w := h.reload(); w.Status != "in_progress" || w.Assignee != backoffHarnessTemplate {
		h.t.Fatalf("tick %d changed the work bead claim: status=%q assignee=%q\nstderr:\n%s", h.sessions, w.Status, w.Assignee, h.env.stderr.String())
	}
	return true
}

// advance runs ticks every second for d, failing every start the pool plans.
func (h *startBackoffHarness) advance(d time.Duration, startErr error) (starts int) {
	for elapsed := time.Duration(0); elapsed < d; elapsed += time.Second {
		if h.tick(startErr) {
			starts++
		}
		h.env.clk.Time = h.env.clk.Time.Add(time.Second)
	}
	return starts
}

var errPreStartFailure = errors.New("resuming session: running pre_start: pre_start[0]: exit status 1; stderr: worker-worktree: ERROR several branches name bead gp-fh5f; pass --bead with a unique id, or --base and no bead: gp-fh5f gp-fh5f-upstream")

// TestPoolStartBackoffGapsThenParkWithOneMail pins (a) the backoff gaps between
// consecutive starts for one bead and (b) exactly one park mail after N
// failures and zero starts after it.
func TestPoolStartBackoffGapsThenParkWithOneMail(t *testing.T) {
	h := newStartBackoffHarness(t, 5)
	starts := h.advance(20*time.Minute, errPreStartFailure)
	if starts != 5 {
		t.Fatalf("starts in 20 minutes = %d, want exactly the 5 that reach the limit (incident: ~160 in 23 minutes); stderr:\n%s", starts, h.env.stderr.String())
	}
	wantGaps := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second}
	for i, want := range wantGaps {
		if got := h.startsAt[i+1].Sub(h.startsAt[i]); got != want {
			t.Fatalf("gap between start %d and %d = %v, want %v (starts at %v)", i+1, i+2, got, want, h.startsAt)
		}
	}
	if len(h.mails) != 1 {
		t.Fatalf("park mails = %d, want exactly 1", len(h.mails))
	}
	mail := h.mails[0]
	if mail.BeadID != h.work.ID || mail.Agent != backoffHarnessTemplate || mail.Failures != 5 {
		t.Fatalf("park mail names the wrong thing: %+v", mail)
	}
	if !strings.Contains(mail.Reason, "several branches name bead gp-fh5f") {
		t.Fatalf("park mail must carry the last stderr line, got %q", mail.Reason)
	}
	body := mail.Body()
	for _, want := range []string{h.work.ID, "gc sling --reassign", "--unset-metadata gc.park_reason", "gc bd show"} {
		if !strings.Contains(body, want) {
			t.Fatalf("park mail body must name %q:\n%s", want, body)
		}
	}
	work := h.reload()
	state := readWorkStartFailureState(work.Metadata)
	if !state.Parked() || state.ParkFailures != 5 || state.ParkMailedAt.IsZero() {
		t.Fatalf("park not recorded on the work bead: %+v", state)
	}
	if work.Status != "in_progress" || work.Assignee != backoffHarnessTemplate || work.Metadata[beadmeta.RoutedToMetadataKey] != backoffHarnessTemplate {
		t.Fatalf("a parked bead keeps its status, assignee and route: status=%q assignee=%q routed=%q\nstderr:\n%s", work.Status, work.Assignee, work.Metadata[beadmeta.RoutedToMetadataKey], h.env.stderr.String())
	}
	// A day later: still zero starts, still one mail.
	h.env.clk.Time = h.env.clk.Time.Add(24 * time.Hour)
	if more := h.advance(time.Minute, errPreStartFailure); more != 0 || len(h.mails) != 1 {
		t.Fatalf("parked bead started %d more times / mailed %d times", more, len(h.mails))
	}
	if !strings.Contains(h.env.stderr.String(), "PARKED work bead "+h.work.ID) {
		t.Fatalf("the park must be said on stderr:\n%s", h.env.stderr.String())
	}
}

// TestPoolStartBackoffTransientFailureRetriesWithoutPark pins (d): one failure
// then success is retried after one backoff, with no park and no mail, and
// (c): the successful start clears the record.
func TestPoolStartBackoffTransientFailureRetriesWithoutPark(t *testing.T) {
	h := newStartBackoffHarness(t, 5)
	transient := errors.New("resuming session: running pre_start: pre_start[0]: context deadline exceeded: exit status 130; stderr: worker-worktree: waiting for another run on this repository (pid 63320)")
	if !h.tick(transient) {
		t.Fatal("first tick must start")
	}
	state := readWorkStartFailureState(h.reload().Metadata)
	if state.Failures != 1 || state.BackoffUntil.Sub(h.env.clk.Now()) != 10*time.Second {
		t.Fatalf("one failure = count 1 + 10s backoff, got %+v", state)
	}
	h.env.clk.Time = h.env.clk.Time.Add(5 * time.Second)
	if h.tick(nil) {
		t.Fatal("inside the backoff window no start is planned")
	}
	h.env.clk.Time = h.env.clk.Time.Add(5 * time.Second)
	if !h.tick(nil) {
		t.Fatal("once the backoff elapsed the start is retried")
	}
	work := h.reload()
	for _, key := range beadmeta.WorkStartFailureMetadataKeys {
		if work.Metadata[key] != "" {
			t.Fatalf("a confirmed start clears the record, %s=%q", key, work.Metadata[key])
		}
	}
	if len(h.mails) != 0 {
		t.Fatalf("transient failure mailed %d times", len(h.mails))
	}
	if !strings.Contains(h.env.stderr.String(), "start-failure record cleared") {
		t.Fatalf("the reset must be said on stderr:\n%s", h.env.stderr.String())
	}
}

// TestPoolStartBackoffResetsAfterSuccessThenCountsAgain pins (c) further: a
// success in the middle of a run resets the count to zero, so the park needs
// N consecutive failures again.
func TestPoolStartBackoffResetsAfterSuccessThenCountsAgain(t *testing.T) {
	h := newStartBackoffHarness(t, 3)
	h.advance(25*time.Second, errPreStartFailure) // failures at 0s and 10s; the 20s backoff runs to 30s
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("count after two failures = %d", got)
	}
	h.env.clk.Time = h.env.clk.Time.Add(time.Minute)
	if !h.tick(nil) {
		t.Fatal("start after the backoff")
	}
	if got := readWorkStartFailureState(h.reload().Metadata); got.Failures != 0 || got.Parked() {
		t.Fatalf("success must reset: %+v", got)
	}
	h.env.clk.Time = h.env.clk.Time.Add(time.Minute)
	starts := h.advance(5*time.Minute, errPreStartFailure)
	if starts != 3 || len(h.mails) != 1 {
		t.Fatalf("after the reset the park needs 3 fresh failures: starts=%d mails=%d", starts, len(h.mails))
	}
}

// TestPoolStartBackoffCountSurvivesReconcilerRestart pins (e): the count is
// metadata on the work bead, so a new reconciler (new policy, fresh process
// memory) over the same store continues it.
func TestPoolStartBackoffCountSurvivesReconcilerRestart(t *testing.T) {
	h := newStartBackoffHarness(t, 3)
	h.advance(15*time.Second, errPreStartFailure) // failures at 0s and 10s
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("count before the restart = %d, want 2", got)
	}
	h.installPolicy() // the restart
	h.env.clk.Time = h.env.clk.Time.Add(time.Minute)
	if !h.tick(errPreStartFailure) {
		t.Fatal("start after the restart")
	}
	state := readWorkStartFailureState(h.reload().Metadata)
	if !state.Parked() || state.ParkFailures != 3 {
		t.Fatalf("the third failure after a restart must park at limit 3: %+v", state)
	}
	if len(h.mails) != 1 {
		t.Fatalf("mails = %d", len(h.mails))
	}
}

// TestPoolStartBackoffParkMailRetriedUntilItLands: an unlanded park mail
// leaves gc.park_mailed_at empty and is re-sent on a later tick; once it
// lands the stamp closes the retry. Exactly one delivered mail.
func TestPoolStartBackoffParkMailRetriedUntilItLands(t *testing.T) {
	h := newStartBackoffHarness(t, 1)
	h.mailErr = errors.New("mail store down")
	if !h.tick(errPreStartFailure) {
		t.Fatal("start")
	}
	state := readWorkStartFailureState(h.reload().Metadata)
	if !state.Parked() || !state.ParkMailedAt.IsZero() {
		t.Fatalf("park must be recorded unmailed: %+v", state)
	}
	if !strings.Contains(h.env.stderr.String(), "did not land") {
		t.Fatalf("the unlanded mail must be said:\n%s", h.env.stderr.String())
	}
	work := h.reload()
	h.policy.retryUnmailedParks([]beads.Bead{work}, []string{"city"})
	h.policy.awaitParkMailRetries()
	if len(h.mails) != 0 {
		t.Fatal("a still-failing mail route delivers nothing")
	}
	h.mailErr = nil
	h.policy.retryUnmailedParks([]beads.Bead{work}, []string{"city"})
	h.policy.awaitParkMailRetries()
	if len(h.mails) != 1 {
		t.Fatalf("retry delivered %d mails, want 1", len(h.mails))
	}
	work = h.reload()
	if readWorkStartFailureState(work.Metadata).ParkMailedAt.IsZero() {
		t.Fatal("a landed mail is stamped")
	}
	h.policy.retryUnmailedParks([]beads.Bead{work}, []string{"city"})
	h.policy.awaitParkMailRetries()
	if len(h.mails) != 1 {
		t.Fatalf("a stamped park is not re-sent, mails=%d", len(h.mails))
	}
}

// TestPoolStartBackoffLimitZeroBacksOffWithoutPark: max_start_failures = 0
// keeps the backoff and never parks or mails.
func TestPoolStartBackoffLimitZeroBacksOffWithoutPark(t *testing.T) {
	h := newStartBackoffHarness(t, 0)
	starts := h.advance(20*time.Minute, errPreStartFailure)
	// 10+20+40+80+160+300+300+... : starts at 0, 10, 30, 70, 150, 310, 610, 910 s.
	if starts != 8 || len(h.mails) != 0 {
		t.Fatalf("limit 0: starts=%d mails=%d, want 8 starts and no mail", starts, len(h.mails))
	}
	if state := readWorkStartFailureState(h.reload().Metadata); state.Parked() || state.Failures != 8 {
		t.Fatalf("limit 0 never parks: %+v", state)
	}
}

// TestPoolStartBackoffTerminalProviderErrorAlsoCounts: a terminal provider
// error on a fresh create marks the session terminal AND charges the work
// bead — the marked session is skipped for reuse, so without the record the
// next tick would create another.
func TestPoolStartBackoffTerminalProviderErrorAlsoCounts(t *testing.T) {
	h := newStartBackoffHarness(t, 2)
	terminal := errors.New("provider: model_not_found: claude-nope")
	starts := h.advance(time.Minute, terminal)
	if starts != 2 || len(h.mails) != 1 {
		t.Fatalf("terminal errors: starts=%d mails=%d, want 2 starts then a park", starts, len(h.mails))
	}
}

// TestDefaultScaleCheckCountsAndDemandSkipsDeferredWork pins the scale_check
// (open, unassigned) tier: a parked or backed-off routed bead is not demand,
// and the same bead is demand again once its backoff elapsed.
func TestDefaultScaleCheckCountsAndDemandSkipsDeferredWork(t *testing.T) {
	const template = "hello-world/polecat"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "polecat", Dir: "hello-world", MaxActiveSessions: intPtr(3)}},
	}
	now := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)
	store := beads.NewMemStore()
	mk := func(title string, meta map[string]string) beads.Bead {
		meta[beadmeta.RoutedToMetadataKey] = template
		b, err := store.Create(beads.Bead{Title: title, Type: "task", Status: "open", Metadata: meta})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		return b
	}
	plain := mk("plain", map[string]string{})
	parked := mk("parked", map[string]string{beadmeta.ParkedAtMetadataKey: now.Add(-time.Hour).Format(time.RFC3339), beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "5"})
	backedOff := mk("backed off", map[string]string{beadmeta.StartFailuresMetadataKey: "2", beadmeta.StartBackoffUntilMetadataKey: now.Add(20 * time.Second).Format(time.RFC3339)})
	count := func(at time.Time) []string {
		counts, demand, _, errs := defaultScaleCheckCountsAndDemand(cfg, at, []defaultScaleCheckTarget{{template: template, storeKey: "city", store: store}})
		if len(errs) != 0 {
			t.Fatalf("errs = %v", errs)
		}
		if counts[template] != len(demand[template].WorkBeadIDs) {
			t.Fatalf("count %d disagrees with ids %v", counts[template], demand[template].WorkBeadIDs)
		}
		return demand[template].WorkBeadIDs
	}
	if got := count(now); len(got) != 1 || got[0] != plain.ID {
		t.Fatalf("at now demand = %v, want only %s (parked %s and backed-off %s excluded)", got, plain.ID, parked.ID, backedOff.ID)
	}
	got := count(now.Add(20 * time.Second))
	if len(got) != 2 {
		t.Fatalf("once the backoff elapsed demand = %v, want %s and %s", got, plain.ID, backedOff.ID)
	}
	for _, id := range got {
		if id == parked.ID {
			t.Fatalf("a parked bead is never demand: %v", got)
		}
	}
}

// TestExcludeStartDeferredWorkKeepsOnlyUndeferred: the assigned-work tier
// skips the same rows the scale_check tier skips.
func TestExcludeStartDeferredWorkKeepsOnlyUndeferred(t *testing.T) {
	now := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)
	work := []beads.Bead{
		{ID: "w-plain", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "helper"}},
		{ID: "w-parked", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "helper", beadmeta.ParkedAtMetadataKey: now.Format(time.RFC3339)}},
		{ID: "w-backoff", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "helper", beadmeta.StartBackoffUntilMetadataKey: now.Add(time.Second).Format(time.RFC3339)}},
		{ID: "w-elapsed", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "helper", beadmeta.StartBackoffUntilMetadataKey: now.Format(time.RFC3339)}},
	}
	kept := excludeStartDeferredWork(work, now, nil)
	if len(kept) != 2 || kept[0].ID != "w-plain" || kept[1].ID != "w-elapsed" {
		t.Fatalf("kept = %v", kept)
	}
	if got := excludeStartDeferredWork(nil, now, nil); got != nil {
		t.Fatalf("nil in, nil out, got %v", got)
	}
}
