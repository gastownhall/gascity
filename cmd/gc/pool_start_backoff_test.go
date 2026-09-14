package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestStartFailureBackoffTable is the spec of the one backoff function (C2):
// 10s after the first consecutive failure, doubling, capped at 5m.
func TestStartFailureBackoffTable(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{-1, 0},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 80 * time.Second},
		{5, 160 * time.Second},
		{6, 5 * time.Minute},
		{7, 5 * time.Minute},
		{40, 5 * time.Minute},
	} {
		if got := startFailureBackoff(tc.failures); got != tc.want {
			t.Errorf("startFailureBackoff(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

// poolStartBackoffHarness drives the real reconciler (reconcileSessionBeads
// over a MemStore, the fake provider's StartErrors, a fake clock) one tick at a
// time, minting a fresh pending-create session bead for the work bead on every
// tick exactly as the pool does after a failed-create rollback.
type poolStartBackoffHarness struct {
	t        *testing.T
	store    *beads.MemStore
	sp       *runtime.Fake
	clk      *clock.Fake
	cfg      *config.City
	desired  map[string]TemplateParams
	mailer   *mail.Fake
	policy   *workStartFailurePolicy
	work     beads.Bead
	sessions int
}

func newPoolStartBackoffHarness(t *testing.T, maxStartFailures *int) *poolStartBackoffHarness {
	t.Helper()
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "route me",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "helper"},
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	cfg := &config.City{
		Agents:  []config.Agent{{Name: "helper", MaxStartFailures: maxStartFailures}},
		Session: config.SessionConfig{ParkAlertTo: "mayor"},
	}
	mailer := mail.NewFake()
	h := &poolStartBackoffHarness{
		t:     t,
		store: store,
		sp:    runtime.NewFake(),
		clk:   &clock.Fake{Time: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)},
		cfg:   cfg,
		desired: map[string]TemplateParams{
			"sky": {Command: "test-cmd", SessionName: "sky", TemplateName: "helper"},
		},
		mailer: mailer,
		work:   work,
	}
	h.policy = &workStartFailurePolicy{
		cfg:        cfg,
		workStore:  store,
		mail:       mailer,
		alertTo:    cfg.Session.ParkAlertTo,
		missLogged: &sync.Map{},
	}
	return h
}

// pendingSessionBead mints the session bead a pool tick creates for the work
// bead: pending create, creating, with the work bead as its trigger.
func (h *poolStartBackoffHarness) pendingSessionBead() beads.Bead {
	h.t.Helper()
	h.sessions++
	b, err := h.store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":                    "sky",
			"session_name_explicit":           "true",
			"pending_create_claim":            "true",
			"template":                        "helper",
			"state":                           "creating",
			"generation":                      "1",
			"continuation_epoch":              "1",
			"instance_token":                  fmt.Sprintf("token-%d", h.sessions),
			beadmeta.TriggerBeadIDMetadataKey: h.work.ID,
		},
	})
	if err != nil {
		h.t.Fatalf("Create(session): %v", err)
	}
	return b
}

// tick runs one reconcile over a fresh pending session bead. startErr nil
// means the provider start succeeds. Returns the reconciler's stderr.
func (h *poolStartBackoffHarness) tick(startErr error, policy *workStartFailurePolicy) string {
	h.t.Helper()
	if startErr != nil {
		h.sp.StartErrors["sky"] = startErr
	} else {
		delete(h.sp.StartErrors, "sky")
	}
	session := h.pendingSessionBead()
	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(h.cfg, "", h.store)
	opts := []startExecutionOption{
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	}
	if policy != nil {
		opts = append(opts, withWorkStartFailurePolicy(policy))
	}
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, h.desired, cfgNames,
		h.cfg, h.sp, h.store, nil, nil, nil, newDrainTracker(), map[string]int{"helper": 1}, false, nil, "",
		nil, h.clk, events.Discard, 0, 0, &stdout, &stderr,
		opts...,
	)
	return stderr.String()
}

func (h *poolStartBackoffHarness) workBead() beads.Bead {
	h.t.Helper()
	b, err := h.store.Get(h.work.ID)
	if err != nil {
		h.t.Fatalf("Get(work): %v", err)
	}
	return b
}

func (h *poolStartBackoffHarness) meta(key string) string {
	return strings.TrimSpace(h.workBead().Metadata[key])
}

func (h *poolStartBackoffHarness) parkMails() []mail.Message {
	h.t.Helper()
	msgs, err := h.mailer.Inbox("mayor")
	if err != nil {
		h.t.Fatalf("Inbox: %v", err)
	}
	return msgs
}

// preStartFailureText is the folded stderr tail of a real pre_start failure; it
// ends with a newline exactly as the setup runner's error does.
const preStartFailureText = "pre_start[0]: setup command failed\nfatal: two branches match gp-fh5f\n"

var errPreStart = fmt.Errorf("%s", preStartFailureText) //nolint:staticcheck // ST1005: the runner's error carries the tail verbatim, trailing newline included

// TestPoolStartBackoff_ConsecutiveFailuresBackOffOnTheWorkBead is test (a):
// the counter and the backoff deadline live on the WORK bead and the gaps
// between consecutive starts follow the table, stamped from the reconciler's
// clock. (The session bead is rolled back and gone after every tick, as the
// existing rollback test pins.)
func TestPoolStartBackoff_ConsecutiveFailuresBackOffOnTheWorkBead(t *testing.T) {
	h := newPoolStartBackoffHarness(t, nil)
	for i, want := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second} {
		stderr := h.tick(errPreStart, h.policy)
		if got := h.meta(beadmeta.StartFailuresMetadataKey); got != fmt.Sprint(i+1) {
			t.Fatalf("tick %d: gc.start_failures = %q, want %d\nstderr:\n%s", i+1, got, i+1, stderr)
		}
		until, ok := parseRFC3339Metadata(h.meta(beadmeta.StartBackoffUntilMetadataKey))
		if !ok {
			t.Fatalf("tick %d: gc.start_backoff_until unparseable: %q", i+1, h.meta(beadmeta.StartBackoffUntilMetadataKey))
		}
		if gap := until.Sub(h.clk.Now()); gap != want {
			t.Fatalf("tick %d: backoff gap = %v, want %v", i+1, gap, want)
		}
		if got := h.meta(beadmeta.StartFailureMetadataKey); got != "fatal: two branches match gp-fh5f" {
			t.Fatalf("tick %d: gc.start_failure = %q, want the last stderr line", i+1, got)
		}
		if got := h.meta(beadmeta.StartFailedAtMetadataKey); got != h.clk.Now().UTC().Format(time.RFC3339) {
			t.Fatalf("tick %d: gc.start_failed_at = %q, want the clock's now", i+1, got)
		}
		if reason, _, deferred := workStartDeferral(h.workBead(), h.clk.Now()); !deferred || reason != TraceReasonStartBackoff {
			t.Fatalf("tick %d: the bead must be deferred until the deadline (deferred=%v reason=%q)", i+1, deferred, reason)
		}
		if h.meta(beadmeta.ParkedAtMetadataKey) != "" {
			t.Fatalf("tick %d: parked too early", i+1)
		}
		// The next start comes no sooner than the deadline.
		h.clk.Time = until
	}
	if got := len(h.parkMails()); got != 0 {
		t.Fatalf("park mails = %d, want 0 before the threshold", got)
	}
}

// TestPoolStartBackoff_ParksAfterMaxFailuresWithOneMailAndNoStart is test (b):
// after max_start_failures consecutive failures the bead is parked, exactly one
// mail reaches the configured target, a further tick charges nothing more, and
// both demand tiers produce ZERO rows for it, so no session is started.
func TestPoolStartBackoff_ParksAfterMaxFailuresWithOneMailAndNoStart(t *testing.T) {
	h := newPoolStartBackoffHarness(t, intPtr(3))
	var stderr string
	for i := 0; i < 3; i++ {
		stderr = h.tick(errPreStart, h.policy)
		h.clk.Time = h.clk.Time.Add(10 * time.Minute)
	}
	if h.meta(beadmeta.ParkedAtMetadataKey) == "" {
		t.Fatalf("not parked after 3 failures\nstderr:\n%s", stderr)
	}
	if got := h.meta(beadmeta.ParkFailuresMetadataKey); got != "3" {
		t.Fatalf("gc.park_failures = %q, want 3", got)
	}
	if got := h.meta(beadmeta.ParkReasonMetadataKey); got != "fatal: two branches match gp-fh5f" {
		t.Fatalf("gc.park_reason = %q", got)
	}
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "" {
		t.Fatalf("gc.start_failures = %q, want cleared at park", got)
	}
	if got := h.meta(beadmeta.ParkMailFailedMetadataKey); got != "" {
		t.Fatalf("gc.park_mail_failed = %q, want empty on a delivered mail", got)
	}
	if !strings.Contains(stderr, "PARKED work bead "+h.work.ID) {
		t.Fatalf("stderr lacks the park line:\n%s", stderr)
	}
	work := h.workBead()
	if work.Status != "open" || work.Assignee != "" || work.Metadata[beadmeta.RoutedToMetadataKey] != "helper" {
		t.Fatalf("park changed status/assignee/route: %+v", work)
	}
	mails := h.parkMails()
	if len(mails) != 1 {
		t.Fatalf("park mails = %d, want exactly 1", len(mails))
	}
	if want := fmt.Sprintf("PARKED %s after 3 start failures", h.work.ID); mails[0].Subject != want {
		t.Fatalf("subject = %q, want %q", mails[0].Subject, want)
	}
	t.Logf("PARK MAIL SAMPLE\nFrom: %s\nTo: %s\nSubject: %s\n\n%s", mails[0].From, mails[0].To, mails[0].Subject, mails[0].Body)
	for _, want := range []string{h.work.ID, "helper", "fatal: two branches match gp-fh5f", "gc sling --reassign helper " + h.work.ID, "--unset-metadata gc.parked_at"} {
		if !strings.Contains(mails[0].Body, want) {
			t.Fatalf("mail body lacks %q:\n%s", want, mails[0].Body)
		}
	}

	// A parked bead is charged no further and mailed no further.
	h.tick(errPreStart, h.policy)
	if got := h.meta(beadmeta.ParkFailuresMetadataKey); got != "3" {
		t.Fatalf("after park: gc.park_failures = %q, want 3 (no further charge)", got)
	}
	if got := len(h.parkMails()); got != 1 {
		t.Fatalf("after park: park mails = %d, want still 1", got)
	}

	// ZERO starts: neither demand tier lists the parked bead.
	work = h.workBead()
	pass := newWorkStartDeferralPass(h.clk.Now().Add(24*time.Hour), nil)
	if _, rows := poolDemandAssignedWork(h.cfg, "", nil, nil, []beads.Bead{work}, []string{""}, pass); len(rows) != 0 {
		t.Fatalf("assigned tier listed a parked bead: %+v", rows)
	}
	counts, demand, _, errs := defaultScaleCheckCountsAndDemand(h.cfg, []defaultScaleCheckTarget{{template: "helper", storeKey: "city", store: h.store}}, pass)
	if len(errs) != 0 {
		t.Fatalf("scale_check errs: %v", errs)
	}
	if counts["helper"] != 0 || len(demand["helper"].WorkBeadIDs) != 0 {
		t.Fatalf("scale_check tier counted a parked bead: counts=%v demand=%+v", counts, demand["helper"])
	}
	if !pass.recheckAt.IsZero() {
		t.Fatalf("a park sets no recheck deadline, got %v", pass.recheckAt)
	}
}

// TestPoolStartBackoff_ResetOnCreationComplete is test (c): one confirmed
// start (the CommitStartedPatch batch) clears the counter to zero.
func TestPoolStartBackoff_ResetOnCreationComplete(t *testing.T) {
	h := newPoolStartBackoffHarness(t, nil)
	for i := 0; i < 3; i++ {
		h.tick(errPreStart, h.policy)
		h.clk.Time = h.clk.Time.Add(10 * time.Minute)
	}
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "3" {
		t.Fatalf("gc.start_failures = %q, want 3", got)
	}
	stderr := h.tick(nil, h.policy)
	if !strings.Contains(stderr, "Woke") && !h.sp.IsRunning("sky") {
		t.Fatalf("the success tick did not start the session\nstderr:\n%s", stderr)
	}
	for _, key := range []string{beadmeta.StartFailuresMetadataKey, beadmeta.StartFailedAtMetadataKey, beadmeta.StartFailureMetadataKey, beadmeta.StartBackoffUntilMetadataKey} {
		if got := h.meta(key); got != "" {
			t.Fatalf("%s = %q after a confirmed start, want cleared\nstderr:\n%s", key, got, stderr)
		}
	}
	if _, _, deferred := workStartDeferral(h.workBead(), h.clk.Now()); deferred {
		t.Fatal("the bead is still deferred after a confirmed start")
	}
}

// TestPoolStartBackoff_TransientFailureRetriesWithoutPark is test (d): one
// failure then success = retried, no park, no mail.
func TestPoolStartBackoff_TransientFailureRetriesWithoutPark(t *testing.T) {
	h := newPoolStartBackoffHarness(t, nil)
	h.tick(errPreStart, h.policy)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "1" {
		t.Fatalf("gc.start_failures = %q, want 1", got)
	}
	h.clk.Time = h.clk.Time.Add(10 * time.Second)
	h.tick(nil, h.policy)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "" {
		t.Fatalf("gc.start_failures = %q after the retry succeeded, want cleared", got)
	}
	if h.meta(beadmeta.ParkedAtMetadataKey) != "" {
		t.Fatal("a transient failure parked the bead")
	}
	if got := len(h.parkMails()); got != 0 {
		t.Fatalf("park mails = %d, want 0", got)
	}
}

// TestPoolStartBackoff_CounterSurvivesReconcilerRestart is test (e): the count
// is store state on the work bead, so a fresh reconciler (new policy value, new
// trackers, nothing in memory) continues it.
func TestPoolStartBackoff_CounterSurvivesReconcilerRestart(t *testing.T) {
	h := newPoolStartBackoffHarness(t, intPtr(2))
	h.tick(errPreStart, h.policy)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "1" {
		t.Fatalf("gc.start_failures = %q, want 1", got)
	}
	h.clk.Time = h.clk.Time.Add(time.Minute)
	restarted := &workStartFailurePolicy{cfg: h.cfg, workStore: h.store, mail: h.mailer, alertTo: "mayor", missLogged: &sync.Map{}}
	h.tick(errPreStart, restarted)
	if h.meta(beadmeta.ParkedAtMetadataKey) == "" || h.meta(beadmeta.ParkFailuresMetadataKey) != "2" {
		t.Fatalf("the restarted reconciler did not continue the count: %+v", h.workBead().Metadata)
	}
	if got := len(h.parkMails()); got != 1 {
		t.Fatalf("park mails = %d, want 1", got)
	}
}

// TestPoolStartBackoff_DeadlineJudgedByTheClock is test (g) (C3): with a
// frozen clock the deadline never expires; the same clock advanced past it
// makes the bead eligible again, on the pure predicate and on both tiers.
func TestPoolStartBackoff_DeadlineJudgedByTheClock(t *testing.T) {
	h := newPoolStartBackoffHarness(t, nil)
	h.tick(errPreStart, h.policy)
	work := h.workBead()
	until, _ := parseRFC3339Metadata(work.Metadata[beadmeta.StartBackoffUntilMetadataKey])
	frozen := h.clk.Now()
	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"frozen", frozen, true},
		{"frozen again", frozen, true},
		{"one second short", until.Add(-time.Second), true},
		{"at the deadline", until, false},
		{"past the deadline", until.Add(time.Hour), false},
	} {
		reason, gotUntil, deferred := workStartDeferral(work, tc.now)
		if deferred != tc.want {
			t.Fatalf("%s: deferred = %v, want %v", tc.name, deferred, tc.want)
		}
		if deferred && (reason != TraceReasonStartBackoff || !gotUntil.Equal(until)) {
			t.Fatalf("%s: reason=%q until=%v, want start_backoff until %v", tc.name, reason, gotUntil, until)
		}
		pass := newWorkStartDeferralPass(tc.now, nil)
		_, rows := poolDemandAssignedWork(h.cfg, "", nil, nil, []beads.Bead{work}, []string{""}, pass)
		counts, _, _, _ := defaultScaleCheckCountsAndDemand(h.cfg, []defaultScaleCheckTarget{{template: "helper", storeKey: "city", store: h.store}}, pass)
		if tc.want && (len(rows) != 0 || counts["helper"] != 0) {
			t.Fatalf("%s: tiers served a backed-off bead: rows=%d count=%d", tc.name, len(rows), counts["helper"])
		}
		if !tc.want && (len(rows) != 1 || counts["helper"] != 1) {
			t.Fatalf("%s: tiers withheld an eligible bead: rows=%d count=%d", tc.name, len(rows), counts["helper"])
		}
		if tc.want && !pass.recheckAt.Equal(until) {
			t.Fatalf("%s: recheckAt = %v, want the deadline %v", tc.name, pass.recheckAt, until)
		}
	}
}

// TestPoolStartBackoff_DemandSnapshotExpiresAtTheRecheckDeadline pins the C4
// snapshot-expiry choice: an event-backed runtime reuses a cached demand
// snapshot for minutes, so the deadline the gate held a row to must expire it.
func TestPoolStartBackoff_DemandSnapshotExpiresAtTheRecheckDeadline(t *testing.T) {
	cr := bindingFingerprintRuntime(t, beads.NewMemStore(), beads.NewMemStore())
	if cr.demandSnapshotPatrolMaxAge() <= time.Minute {
		t.Fatal("fixture is not event-backed; the reuse window under test does not exist")
	}
	cr.demandSnapshot = &runtimeDemandSnapshot{createdAt: time.Now(), sessionFingerprint: "fp", recheckAt: time.Now().Add(time.Hour)}
	if cr.shouldRefreshDemandSnapshot("patrol", false, "fp") {
		t.Fatal("a fresh snapshot with a future recheck deadline was refreshed")
	}
	cr.demandSnapshot.recheckAt = time.Now().Add(-time.Millisecond)
	if !cr.shouldRefreshDemandSnapshot("patrol", false, "fp") {
		t.Fatal("a snapshot whose recheck deadline passed was reused")
	}
	if got := earliestRecheck(time.Time{}, cr.demandSnapshot.recheckAt); !got.Equal(cr.demandSnapshot.recheckAt) {
		t.Fatalf("earliestRecheck(zero, t) = %v", got)
	}
	a, b := time.Unix(100, 0), time.Unix(50, 0)
	if got := earliestRecheck(a, b); !got.Equal(b) {
		t.Fatalf("earliestRecheck = %v, want the earlier %v", got, b)
	}
}

// TestPoolStartBackoff_ParkMailFailureIsLoudAndTheParkStands is test (i): an
// unconfigured target and a failing provider both stamp gc.park_mail_failed,
// log one loud line, and leave the bead parked. The park never depends on the
// mail.
func TestPoolStartBackoff_ParkMailFailureIsLoudAndTheParkStands(t *testing.T) {
	for _, tc := range []struct {
		name    string
		alertTo string
		mailer  mail.Provider
	}{
		{"unconfigured target", "", mail.NewFake()},
		{"send fails", "mayor", mail.NewFailFake()},
		{"no provider", "mayor", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPoolStartBackoffHarness(t, intPtr(1))
			h.policy.alertTo = tc.alertTo
			h.policy.mail = tc.mailer
			stderr := h.tick(errPreStart, h.policy)
			if h.meta(beadmeta.ParkedAtMetadataKey) == "" {
				t.Fatalf("bead not parked\nstderr:\n%s", stderr)
			}
			if got := h.meta(beadmeta.ParkMailFailedMetadataKey); got != h.clk.Now().UTC().Format(time.RFC3339) {
				t.Fatalf("gc.park_mail_failed = %q, want the park instant", got)
			}
			if n := strings.Count(stderr, "PARK MAIL FAILED"); n != 1 {
				t.Fatalf("PARK MAIL FAILED lines = %d, want 1\nstderr:\n%s", n, stderr)
			}
			if _, _, deferred := workStartDeferral(h.workBead(), h.clk.Now().Add(24*time.Hour)); !deferred {
				t.Fatal("the park did not hold")
			}
		})
	}
}

// TestPoolStartBackoff_StartupDeathIsNotASuccess is test (j): a start that
// fails after the provider call (the "died during startup" class) is a failed
// start; the count grows and nothing clears it without the creation_complete
// stamp. A rate-limit screen is not charged (it holds an existing session
// under its own quarantine).
func TestPoolStartBackoff_StartupDeathIsNotASuccess(t *testing.T) {
	h := newPoolStartBackoffHarness(t, nil)
	h.tick(errPreStart, h.policy)
	h.clk.Time = h.clk.Time.Add(time.Minute)
	h.tick(fmt.Errorf("session %q died during startup", "sky"), h.policy)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "2" {
		t.Fatalf("gc.start_failures = %q after a startup death, want 2", got)
	}
	prepared := preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: h.work.ID}, tp: TemplateParams{TemplateName: "helper"}}}
	prepared.attachWorkStartPolicy(h.policy)
	var stderr bytes.Buffer
	recordWorkStartFailure(startResult{prepared: prepared, err: errPreStart, outcome: TraceOutcomeProviderError, rateLimitScreen: true}, h.clk, &stderr, nil)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "2" {
		t.Fatalf("a rate-limit screen charged the bead: %q", got)
	}
	recordWorkStartFailure(startResult{prepared: prepared, err: errPreStart, outcome: TraceOutcomeDeadlineExceeded}, h.clk, &stderr, nil)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "2" {
		t.Fatalf("a non-provider-error outcome charged the bead: %q", got)
	}
}

// TestPoolStartBackoff_MissIsLoggedOnceAndNotCharged pins C7: a trigger the
// carried store ref cannot reach is logged once per bead and never charged.
func TestPoolStartBackoff_MissIsLoggedOnceAndNotCharged(t *testing.T) {
	policy := &workStartFailurePolicy{cfg: &config.City{}, workStore: beads.NewMemStore(), missLogged: &sync.Map{}}
	prepared := preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: "gp-gone", TriggerBeadStoreRef: "rig:absent"}}}
	prepared.attachWorkStartPolicy(policy)
	var stderr bytes.Buffer
	clk := &clock.Fake{Time: time.Now()}
	for i := 0; i < 3; i++ {
		recordWorkStartFailure(startResult{prepared: prepared, err: errPreStart, outcome: TraceOutcomeProviderError}, clk, &stderr, nil)
	}
	if n := strings.Count(stderr.String(), "not charged"); n != 1 {
		t.Fatalf("miss lines = %d, want 1:\n%s", n, stderr.String())
	}
	if got := lastErrorLine(errPreStart); got != "fatal: two branches match gp-fh5f" {
		t.Fatalf("lastErrorLine = %q", got)
	}
	if got := lastErrorLine(errors.New(strings.Repeat("x", 500))); len([]rune(got)) != workStartFailureLineLimit {
		t.Fatalf("lastErrorLine did not bound the line: %d runes", len([]rune(got)))
	}
}

// TestPoolDemandInputsGoThroughStartDeferral is test (h) (C4): every pool
// demand computation in non-test cmd/gc is fed by poolDemandAssignedWork (the
// assigned tier), the raw assigned-work filter is called nowhere else, and the
// scale_check tier's row list is built in one place. A new consumer must be
// added to the pinned set consciously, with the gate in front of it.
func TestPoolDemandInputsGoThroughStartDeferral(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var sites, rawFilterCallers, workBeadIDBuilders []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			gated := map[string]bool{}
			owned := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
					return true
				}
				if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && calleeName(call) == "poolDemandAssignedWork" {
					// (owned, demand): the first is what a session OWNS (the
					// unfiltered projection), the second the gated demand rows.
					if id, ok := assign.Lhs[0].(*ast.Ident); ok {
						owned[id.Name] = true
					}
					if id, ok := assign.Lhs[1].(*ast.Ident); ok {
						gated[id.Name] = true
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calleeName(call)
				site := file + ":" + fn.Name.Name
				switch {
				case strings.HasPrefix(name, "ComputePoolDesiredStates"):
					id, ok := call.Args[1].(*ast.Ident)
					if !ok || !gated[id.Name] {
						t.Errorf("%s: %s is fed %s, not a poolDemandAssignedWork result", site, name, exprString(fset, call.Args[1]))
					}
					// Production computes through the deferring entry so the
					// in-flight tier sees the deferred ids of the same pass.
					if name != "ComputePoolDesiredStatesDeferring" {
						t.Errorf("%s: %s is not the deferring entry; the in-flight tier would reuse a session for a parked bead", site, name)
					}
					sites = append(sites, site)
				case name == "filterAssignedWorkBeadsForPoolDemand":
					rawFilterCallers = append(rawFilterCallers, site)
				}
				return true
			})
			// The materializer's ownership slice (bp.assignedWorkBeads) takes
			// the OWNED result, never the gated one: a session minted for a
			// held-back bead stays owned by it and is not reused for other work
			// (codex r3 finding 4).
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
					return true
				}
				sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "assignedWorkBeads" {
					return true
				}
				if base, ok := sel.X.(*ast.Ident); !ok || base.Name != "bp" {
					return true
				}
				id, ok := assign.Rhs[0].(*ast.Ident)
				if !ok || !owned[id.Name] || gated[id.Name] {
					t.Errorf("%s:%s: bp.assignedWorkBeads = %s; the ownership slice must be poolDemandAssignedWork's OWNED result", file, fn.Name.Name, exprString(fset, assign.Rhs[0]))
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok || len(assign.Lhs) != 1 {
					return true
				}
				if sel, ok := assign.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "WorkBeadIDs" {
					workBeadIDBuilders = append(workBeadIDBuilders, file+":"+fn.Name.Name)
				}
				return true
			})
		}
	}
	sort.Strings(sites)
	t.Logf("POOL DEMAND CALL SITES (each fed by poolDemandAssignedWork): %s", strings.Join(sites, ", "))
	sort.Strings(rawFilterCallers)
	t.Logf("raw filterAssignedWorkBeadsForPoolDemand callers: %s; scale_check WorkBeadIDs builders: %s", strings.Join(rawFilterCallers, ", "), strings.Join(workBeadIDBuilders, ", "))
	wantSites := []string{
		"build_desired_state.go:buildDesiredStateWithSessionBeadsAt",
		"city_runtime.go:beadReconcileTick",
		"city_runtime.go:controlDispatcherTick",
		"city_runtime.go:loadDemandSnapshot",
		"cmd_start.go:doStartStandalone",
	}
	if strings.Join(sites, "\n") != strings.Join(wantSites, "\n") {
		t.Errorf("pool demand call sites = %v, want %v (a new consumer must take poolDemandAssignedWork's result and join this list)", sites, wantSites)
	}
	if want := []string{"pool_start_backoff.go:poolDemandAssignedWork"}; strings.Join(rawFilterCallers, ",") != strings.Join(want, ",") {
		t.Errorf("filterAssignedWorkBeadsForPoolDemand callers = %v, want %v", rawFilterCallers, want)
	}
	sort.Strings(workBeadIDBuilders)
	if want := []string{"build_desired_state.go:defaultScaleCheckCountsAndDemand", "build_desired_state.go:mergeScaleCheckDemand"}; strings.Join(workBeadIDBuilders, ",") != strings.Join(want, ",") {
		t.Errorf("scale_check WorkBeadIDs builders = %v, want %v (the gate runs inside defaultScaleCheckCountsAndDemand)", workBeadIDBuilders, want)
	}
	// Every other consumer of the gate, the charge and the reset, pinned by
	// text so a later edit that drops one fails here rather than in review
	// (the first cut's review found the next consumer three rounds running):
	// the control-dispatcher open-demand flag and the awake input take the
	// gated rows; the targeted tick and the one-shot start hand the policy to
	// the reconciler; every commit arm that knows a start failed charges; the
	// ordinary commit and the pending-create recovery commit reset; the sling
	// re-dispatch clears.
	for _, pin := range []struct{ file, want string }{
		{"build_desired_state.go", "openControlDispatcherDemand(cfg, startDeferral.filter(unassignedRoutedBeads))"},
		{"session_reconciler.go", ".filterWithFlags(assignedWorkBeads, reconcileOpts.readyAssignedFlags)"},
		{"session_reconciler.go", "recoverRunningPendingCreate(infoByID[id], tp, cfg, store, clk, trace, reconcileOpts.workStartFailures, stderr)"},
		{"city_runtime.go", "withWorkStartFailurePolicy(cr.workStartFailurePolicy(store, rigStores))"},
		{"city_runtime.go", "withWorkStartFailurePolicy(cr.workStartFailurePolicy(cr.cityBeadStore(), cr.rigBeadStores()))"},
		{"cmd_start.go", "withWorkStartFailurePolicy(newWorkStartFailurePolicy(cfg, oneShotStore, rigStores, defaultMailProvider(cityPath), cfg.Session.ParkAlertTo))"},
		{"session_lifecycle_parallel.go", "item.attachWorkStartPolicy(startOpts.workStartFailures)"},
		{"session_lifecycle_parallel.go", "recordWorkStartFailure(result, clk, stderr, trace)"},
		{"session_lifecycle_parallel.go", "recordWorkStartFailure(refreshed, clk, stderr, trace)"},
		{"session_lifecycle_parallel.go", "recordWorkStartSuccess(result, stderr)"},
		{"session_lifecycle_parallel.go", "recordWorkStartSuccessFor(workStartFailures, info.TriggerBeadID, info.TriggerBeadStoreRef, stderr)"},
		{"../../internal/sling/sling_core.go", "for _, key := range ParkReleaseMetadataKeys {"},
		{"../../internal/sling/sling_core.go", "if err := reopenForReassign(child.ID, deps); err != nil {"},
		{"pool_desired_state.go", "poolNewDemandRequests(cfg, sessionInfos, resumeSessionBeadIDs, decisionTime, deferredTriggers)"},
		{"pool_desired_state.go", "if _, deferred := deferredTriggers[strings.TrimSpace(sb.TriggerBeadID)]; deferred {"},
		// Round 4, family A (the operator-verb write path): the conditional
		// writer resolves through the policy wrapper; a partial read charges
		// nothing; every release clears all eight keys; the batch releases a
		// child only after its checks; the reset retries once.
		{"pool_start_backoff.go", "writer, ok := workBeadConditionalWriter(store)"},
		{"pool_start_backoff.go", "case hits == 1 && lastErr == nil:"},
		{"../../internal/sling/sling_core.go", "update.Metadata = make(map[string]string, len(ParkReleaseMetadataKeys))"},
		{"../../internal/sling/sling_core.go", "if check.Idempotent && !shouldReopenForReassign(opts) {"},
		{"pool_start_backoff.go", "case conflict && attempt == 0:"},
		// Round 4, family B (the session-survivor consumers): the ownership
		// slice keeps the owned rows (the AST check above); the deferred set
		// rides on the awake input and every wake collector excludes it; the
		// in-flight tier excludes it (the pin above).
		{"build_desired_state.go", "bp.assignedWorkBeads = poolOwnedWorkBeads"},
		{"session_reconciler.go", "awakeInput.DeferredTriggers = awakeDeferral.deferred"},
		{"compute_awake_set.go", "active := input.excludeDeferredSessions(collectActiveBeads(input.SessionBeads, template, input.Now))"},
		{"compute_awake_set.go", "creating := input.excludeDeferredSessions(collectCreatingBeads(input.SessionBeads, template))"},
		{"compute_awake_set.go", "if active := input.excludeDeferredSessions(collectActiveBeads(input.SessionBeads, template, input.Now)); len(active) > 0 {"},
		{"compute_awake_set.go", "if creating := input.excludeDeferredSessions(collectCreatingBeads(input.SessionBeads, template)); len(creating) > 0 {"},
		{"compute_awake_set.go", "for _, bead := range input.excludeDeferredSessions(cityStopPoolBeads(input.SessionBeads, template)) {"},
	} {
		src, err := os.ReadFile(pin.file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), pin.want) {
			t.Errorf("%s no longer carries the gate/charge/reset site %q", pin.file, pin.want)
		}
	}
	// The three charge calls are the three arms that know a start failed:
	// commitStartFailure, the async refresh-failed arm, the context-canceled arm.
	src, err := os.ReadFile("session_lifecycle_parallel.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), "recordWorkStartFailure("); n != 3 {
		t.Errorf("recordWorkStartFailure call sites in session_lifecycle_parallel.go = %d, want 3", n)
	}
}

// TestPoolStartBackoff_ConcurrentChargesAreSerialized pins the per-bead write
// serialization: N async commits failing for one trigger at once produce N
// counted failures, not a lost update, and at the threshold exactly one park
// and one mail.
func TestPoolStartBackoff_ConcurrentChargesAreSerialized(t *testing.T) {
	h := newPoolStartBackoffHarness(t, intPtr(0))
	h.policy.mu = &sync.Mutex{}
	prepared := preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: h.work.ID}, tp: TemplateParams{TemplateName: "helper"}}}
	prepared.attachWorkStartPolicy(h.policy)
	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var stderr bytes.Buffer
			recordWorkStartFailure(startResult{prepared: prepared, err: errPreStart, outcome: TraceOutcomeProviderError}, h.clk, &stderr, nil)
		}()
	}
	wg.Wait()
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != fmt.Sprint(n) {
		t.Fatalf("gc.start_failures = %q after %d concurrent charges, want %d (lost update)", got, n, n)
	}
	// At the threshold: one park, one mail, from many racing commits.
	parkable := newPoolStartBackoffHarness(t, intPtr(1))
	parkable.policy.mu = &sync.Mutex{}
	prepared = preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: parkable.work.ID}, tp: TemplateParams{TemplateName: "helper"}}}
	prepared.attachWorkStartPolicy(parkable.policy)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var stderr bytes.Buffer
			recordWorkStartFailure(startResult{prepared: prepared, err: errPreStart, outcome: TraceOutcomeProviderError}, parkable.clk, &stderr, nil)
		}()
	}
	wg.Wait()
	if got := len(parkable.parkMails()); got != 1 {
		t.Fatalf("park mails = %d after %d racing commits at the threshold, want exactly 1", got, n)
	}
	if got := parkable.meta(beadmeta.ParkFailuresMetadataKey); got != "1" {
		t.Fatalf("gc.park_failures = %q, want 1", got)
	}
}

// TestPoolStartBackoff_RigBeadWithEmptyStoreRefIsCharged pins the assigned
// tier's request shape: it carries no store ref, so a rig-owned trigger is
// found in the rig store by id and charged there, never mis-charged on the
// city store or dropped as a miss.
func TestPoolStartBackoff_RigBeadWithEmptyStoreRefIsCharged(t *testing.T) {
	city := beads.NewMemStore()
	rig := beads.NewMemStore()
	work, err := rig.Create(beads.Bead{Title: "rig work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "helper"}})
	if err != nil {
		t.Fatal(err)
	}
	policy := newWorkStartFailurePolicy(&config.City{Agents: []config.Agent{{Name: "helper"}}}, city, map[string]beads.Store{"riga": rig}, mail.NewFake(), "mayor")
	clk := &clock.Fake{Time: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	var stderr bytes.Buffer
	for _, ref := range []string{"", "rig:riga"} {
		prepared := preparedStart{candidate: startCandidate{tp: TemplateParams{TemplateName: "helper"}}, workStartFailures: policy, triggerBeadID: work.ID, triggerStoreRef: ref}
		recordWorkStartFailure(startResult{prepared: prepared, err: errPreStart, outcome: TraceOutcomeProviderError}, clk, &stderr, nil)
	}
	got, err := rig.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[beadmeta.StartFailuresMetadataKey] != "2" {
		t.Fatalf("rig bead gc.start_failures = %q, want 2 (empty ref and rig ref both charged in the rig store)\nstderr:\n%s", got.Metadata[beadmeta.StartFailuresMetadataKey], stderr.String())
	}
	if _, err := city.Get(work.ID); err == nil {
		t.Fatal("the city store gained the rig bead")
	}
	if stderr.String() != "" && strings.Contains(stderr.String(), "not charged") {
		t.Fatalf("a reachable rig bead was reported as a miss:\n%s", stderr.String())
	}
	// A recovered success with the empty ref clears it in the rig store too.
	recordWorkStartSuccessFor(policy, work.ID, "", &stderr)
	got, _ = rig.Get(work.ID)
	if got.Metadata[beadmeta.StartFailuresMetadataKey] != "" {
		t.Fatalf("rig bead counter = %q after a confirmed start, want cleared", got.Metadata[beadmeta.StartFailuresMetadataKey])
	}
}

// TestPoolStartBackoff_ChargesTheTriggerTheStartCarried pins attribution: the
// async refresh may replace candidate.info with a session bead rebound to
// another trigger; the charge and the reset follow the trigger captured at
// prepare time, never the refreshed one.
func TestPoolStartBackoff_ChargesTheTriggerTheStartCarried(t *testing.T) {
	h := newPoolStartBackoffHarness(t, nil)
	other, err := h.store.Create(beads.Bead{Title: "rebound", Type: "task", Metadata: map[string]string{beadmeta.StartFailuresMetadataKey: "4"}})
	if err != nil {
		t.Fatal(err)
	}
	item := preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: h.work.ID}, tp: TemplateParams{TemplateName: "helper"}}}
	item.attachWorkStartPolicy(h.policy)
	// The refresh swaps the session's trigger to `other` while the start is in flight.
	item.candidate.info.TriggerBeadID = other.ID
	var stderr bytes.Buffer
	recordWorkStartFailure(startResult{prepared: item, err: errPreStart, outcome: TraceOutcomeProviderError}, h.clk, &stderr, nil)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "1" {
		t.Fatalf("the carried trigger was not charged: %q", got)
	}
	rebound, _ := h.store.Get(other.ID)
	if rebound.Metadata[beadmeta.StartFailuresMetadataKey] != "4" {
		t.Fatalf("the rebound trigger was charged: %q", rebound.Metadata[beadmeta.StartFailuresMetadataKey])
	}
	recordWorkStartSuccess(startResult{prepared: item}, &stderr)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "" {
		t.Fatalf("the carried trigger was not reset: %q", got)
	}
	rebound, _ = h.store.Get(other.ID)
	if rebound.Metadata[beadmeta.StartFailuresMetadataKey] != "4" {
		t.Fatalf("the rebound trigger was reset: %q", rebound.Metadata[beadmeta.StartFailuresMetadataKey])
	}
}

// TestPoolStartBackoff_RecoveryCommitResetsTheCounter pins the second
// creation_complete writer: recoverRunningPendingCreate's CommitStartedPatch
// clears the work bead's counter exactly as the ordinary commit does.
func TestPoolStartBackoff_RecoveryCommitResetsTheCounter(t *testing.T) {
	h := newPoolStartBackoffHarness(t, nil)
	if err := h.store.Update(h.work.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.StartFailuresMetadataKey: "4", beadmeta.StartBackoffUntilMetadataKey: "2026-09-14T12:05:00Z"}}); err != nil {
		t.Fatal(err)
	}
	bead, err := h.store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                    "sky",
			"pending_create_claim":            "true",
			"state":                           "active",
			"state_reason":                    "creation_complete",
			beadmeta.TriggerBeadIDMetadataKey: h.work.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tp := TemplateParams{SessionName: "sky", TemplateName: "helper"}
	var stderr bytes.Buffer
	ok, _ := recoverRunningPendingCreate(sessiontest.SeedBead(t, bead), tp, h.cfg, h.store, h.clk, nil, h.policy, &stderr)
	if !ok {
		t.Fatalf("recovery did not commit\nstderr:\n%s", stderr.String())
	}
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "" {
		t.Fatalf("gc.start_failures = %q after the recovery commit stamped creation_complete_at, want cleared", got)
	}
	if got := h.meta(beadmeta.StartBackoffUntilMetadataKey); got != "" {
		t.Fatalf("gc.start_backoff_until = %q, want cleared", got)
	}
}

// TestPoolStartBackoff_AwakeInputDropsDeferredRowsOnly pins the wake-side
// gate: the awake input loses the parked/backed-off rows and their flags,
// index-aligned, and nothing else.
func TestPoolStartBackoff_AwakeInputDropsDeferredRowsOnly(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	rows := []beads.Bead{
		{ID: "a", Status: "in_progress", Assignee: "s1"},
		{ID: "b", Status: "in_progress", Assignee: "s2", Metadata: map[string]string{beadmeta.ParkedAtMetadataKey: now.Format(time.RFC3339)}},
		{ID: "c", Status: "open", Assignee: "s3", Metadata: map[string]string{beadmeta.StartBackoffUntilMetadataKey: now.Add(time.Minute).Format(time.RFC3339)}},
		{ID: "d", Status: "open", Assignee: "s4", Metadata: map[string]string{beadmeta.StartBackoffUntilMetadataKey: now.Add(-time.Minute).Format(time.RFC3339)}},
	}
	flags := []bool{true, true, true, true}
	refs := []string{"", "rig:a", "rig:b", ""}
	pass := newWorkStartDeferralPass(now, nil)
	keptRows, keptFlags := pass.filterWithFlags(rows, flags)
	keptRows2, keptRefs := pass.filterWithRefs(rows, refs)
	ids := func(bs []beads.Bead) string {
		out := make([]string, 0, len(bs))
		for _, b := range bs {
			out = append(out, b.ID)
		}
		return strings.Join(out, ",")
	}
	if ids(keptRows) != "a,d" || ids(keptRows2) != "a,d" {
		t.Fatalf("kept = %q / %q, want a,d", ids(keptRows), ids(keptRows2))
	}
	if len(keptFlags) != 2 || len(keptRefs) != 2 || keptRefs[0] != "" || keptRefs[1] != "" {
		t.Fatalf("aligned companions not filtered with the rows: flags=%v refs=%v", keptFlags, keptRefs)
	}
	if !pass.recheckAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("recheckAt = %v, want the one live deadline", pass.recheckAt)
	}
}

func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	return fmt.Sprintf("%s@%s", fmt.Sprint(e), fset.Position(e.Pos()))
}

// TestPoolStartBackoff_InFlightSessionForDeferredTriggerIsNotReused pins the
// in-flight tier (codex r2 MAJOR 3): a pool session already minted for a bead
// the gate holds back is not reused as spent new demand, so it is not started
// for that bead; demand for another bead mints a fresh seat instead.
func TestPoolStartBackoff_InFlightSessionForDeferredTriggerIsNotReused(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)}}
	inFlight := pendingPoolSessionBead("sess-parked")
	inFlight.Metadata[beadmeta.TriggerBeadIDMetadataKey] = "W"
	sessions := sessionInfosFromBeads([]beads.Bead{inFlight})
	counts := map[string]int{"claude": 1}

	reused := ComputePoolDesiredStatesDeferring(cfg, nil, sessions, counts, nil, nil, time.Time{}, nil)
	if len(reused) != 1 || len(reused[0].Requests) != 1 || reused[0].Requests[0].SessionBeadID != "sess-parked" {
		t.Fatalf("control: without a deferred set the in-flight session is reused: %#v", reused)
	}
	gated := ComputePoolDesiredStatesDeferring(cfg, nil, sessions, counts, nil, map[string]struct{}{"W": {}}, time.Time{}, nil)
	if len(gated) != 1 || len(gated[0].Requests) != 1 {
		t.Fatalf("gated result = %#v, want one request", gated)
	}
	if req := gated[0].Requests[0]; req.SessionBeadID != "" || req.WorkBeadID == "W" {
		t.Fatalf("the in-flight session for the deferred trigger was reused: %#v", req)
	}
}

// TestPoolStartBackoff_AmbiguousIDWithNoStoreRefIsNotCharged pins the
// fail-closed form of the empty-ref lookup (codex r2 MAJOR 2): the same id in
// two independent stores is logged once and charged nowhere.
func TestPoolStartBackoff_AmbiguousIDWithNoStoreRefIsNotCharged(t *testing.T) {
	city := beads.NewMemStore()
	rig := beads.NewMemStore()
	cityWork, err := city.Create(beads.Bead{Title: "city", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	rigWork, err := rig.Create(beads.Bead{Title: "rig", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if cityWork.ID != rigWork.ID {
		t.Fatalf("fixture: ids differ (%s vs %s); two independent stores must share an id here", cityWork.ID, rigWork.ID)
	}
	policy := newWorkStartFailurePolicy(&config.City{Agents: []config.Agent{{Name: "helper"}}}, city, map[string]beads.Store{"riga": rig}, mail.NewFake(), "mayor")
	item := preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: cityWork.ID}, tp: TemplateParams{TemplateName: "helper"}}}
	item.attachWorkStartPolicy(policy)
	var stderr bytes.Buffer
	clk := &clock.Fake{Time: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	for i := 0; i < 3; i++ {
		recordWorkStartFailure(startResult{prepared: item, err: errPreStart, outcome: TraceOutcomeProviderError}, clk, &stderr, nil)
	}
	for name, store := range map[string]beads.Store{"city": city, "rig": rig} {
		got, _ := store.Get(cityWork.ID)
		if got.Metadata[beadmeta.StartFailuresMetadataKey] != "" || got.Metadata[beadmeta.ParkedAtMetadataKey] != "" {
			t.Fatalf("%s bead was charged on an ambiguous id: %v", name, got.Metadata)
		}
	}
	if n := strings.Count(stderr.String(), "ambiguous"); n != 1 {
		t.Fatalf("ambiguity lines = %d, want exactly 1:\n%s", n, stderr.String())
	}
}

// TestPoolStartBackoff_ResetBetweenReadAndWriteSurvives pins the
// revision-conditioned charge (codex r2 MAJOR 4): a --reassign reset landing
// between a failure's read and its write is not overwritten; the failure is
// recomputed from the fresh row (count 1, no park).
func TestPoolStartBackoff_ResetBetweenReadAndWriteSurvives(t *testing.T) {
	h := newPoolStartBackoffHarness(t, intPtr(5))
	if err := h.store.Update(h.work.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.StartFailuresMetadataKey: "4"}}); err != nil {
		t.Fatal(err)
	}
	item := preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: h.work.ID}, tp: TemplateParams{TemplateName: "helper"}}}
	item.attachWorkStartPolicy(h.policy)
	resets := 0
	h.policy.testBeforeWrite = func() {
		if resets == 0 {
			resets++
			// The operator's --reassign lands here: the same release
			// reopenForReassignInStore writes.
			patch := map[string]string{}
			for _, key := range []string{beadmeta.StartFailuresMetadataKey, beadmeta.StartFailedAtMetadataKey, beadmeta.StartFailureMetadataKey, beadmeta.StartBackoffUntilMetadataKey} {
				patch[key] = ""
			}
			if err := h.store.Update(h.work.ID, beads.UpdateOpts{Metadata: patch}); err != nil {
				t.Fatal(err)
			}
		}
	}
	var stderr bytes.Buffer
	recordWorkStartFailure(startResult{prepared: item, err: errPreStart, outcome: TraceOutcomeProviderError}, h.clk, &stderr, nil)
	if got := h.meta(beadmeta.ParkedAtMetadataKey); got != "" {
		t.Fatalf("a stale charge parked the bead over the operator's reset\nstderr:\n%s", stderr.String())
	}
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "1" {
		t.Fatalf("gc.start_failures = %q after reset-then-failure, want 1", got)
	}
	if got := len(h.parkMails()); got != 0 {
		t.Fatalf("park mails = %d, want 0", got)
	}
	// Two changes under one charge: the charge is dropped, never written blind.
	h.policy.testBeforeWrite = func() {
		if err := h.store.Update(h.work.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.StartFailureMetadataKey: "touched"}}); err != nil {
			t.Fatal(err)
		}
	}
	stderr.Reset()
	recordWorkStartFailure(startResult{prepared: item, err: errPreStart, outcome: TraceOutcomeProviderError}, h.clk, &stderr, nil)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "1" {
		t.Fatalf("gc.start_failures = %q after two conflicts, want the charge dropped at 1", got)
	}
	if !strings.Contains(stderr.String(), "changed twice") {
		t.Fatalf("the dropped charge was silent:\n%s", stderr.String())
	}
}

// blockingMail is a mail provider whose FIRST Send blocks until released;
// later sends pass straight through.
type blockingMail struct {
	mail.Provider
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingMail) Send(from, to, subject, body string) (mail.Message, error) {
	if b.calls.Add(1) == 1 {
		close(b.entered)
		<-b.release
	}
	return b.Provider.Send(from, to, subject, body)
}

// TestPoolStartBackoff_ParkMailDoesNotHoldTheLock pins codex r2 MINOR 5: a
// stalled park mail for one bead does not block another bead's charge.
func TestPoolStartBackoff_ParkMailDoesNotHoldTheLock(t *testing.T) {
	a := newPoolStartBackoffHarness(t, intPtr(1))
	blocker := &blockingMail{Provider: mail.NewFake(), entered: make(chan struct{}), release: make(chan struct{})}
	a.policy.mail = blocker
	itemA := preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: a.work.ID}, tp: TemplateParams{TemplateName: "helper"}}}
	itemA.attachWorkStartPolicy(a.policy)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var stderr bytes.Buffer
		recordWorkStartFailure(startResult{prepared: itemA, err: errPreStart, outcome: TraceOutcomeProviderError}, a.clk, &stderr, nil)
	}()
	<-blocker.entered
	// Bead B shares the runtime's lock with A; its charge must not wait on A's mail.
	other, err := a.store.Create(beads.Bead{Title: "other", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	itemB := preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: other.ID}, tp: TemplateParams{TemplateName: "helper"}}}
	itemB.attachWorkStartPolicy(a.policy)
	charged := make(chan struct{})
	go func() {
		defer close(charged)
		var stderr bytes.Buffer
		recordWorkStartFailure(startResult{prepared: itemB, err: errPreStart, outcome: TraceOutcomeProviderError}, a.clk, &stderr, nil)
	}()
	select {
	case <-charged:
	case <-time.After(5 * time.Second):
		t.Fatal("bead B's charge waited on bead A's park mail: the mail is sent under the lock")
	}
	close(blocker.release)
	<-done
	if a.meta(beadmeta.ParkedAtMetadataKey) == "" {
		t.Fatal("bead A was not parked")
	}
}

// TestPoolStartBackoff_LastErrorLineIsTheLastStderrLine pins codex r2 MINOR 6
// against the setup runner's fold ("...; stderr: <tail>; stdout: <tail>").
func TestPoolStartBackoff_LastErrorLineIsTheLastStderrLine(t *testing.T) {
	err := fmt.Errorf("pre_start[0]: %w; stderr: warning: slow\nfatal: invalid worktree; stdout: cleanup\nfinished", errors.New("exit status 128"))
	if got := lastErrorLine(err); got != "fatal: invalid worktree" {
		t.Fatalf("lastErrorLine = %q, want the last stderr line", got)
	}
	if got := lastErrorLine(errors.New("pre_start[0]: exit status 1; stdout: only\nstdout here")); got != "stdout here" {
		t.Fatalf("lastErrorLine without stderr = %q", got)
	}
}

// TestPoolStartBackoff_DeadlineNeverShorterThanTheBackoff pins codex r2 MINOR
// 7: an RFC3339 deadline stamped from a fractional-second clock is rounded UP.
func TestPoolStartBackoff_DeadlineNeverShorterThanTheBackoff(t *testing.T) {
	h := newPoolStartBackoffHarness(t, nil)
	h.clk.Time = time.Date(2026, 9, 14, 12, 0, 0, int(900*time.Millisecond), time.UTC)
	h.tick(errPreStart, h.policy)
	until, ok := parseRFC3339Metadata(h.meta(beadmeta.StartBackoffUntilMetadataKey))
	if !ok {
		t.Fatal("no deadline")
	}
	if gap := until.Sub(h.clk.Now()); gap < 10*time.Second {
		t.Fatalf("deadline %v is %v after the failure, shorter than the 10s backoff", until, gap)
	}
	if got := ceilSecond(time.Unix(100, 0)); !got.Equal(time.Unix(100, 0)) {
		t.Fatalf("ceilSecond on a whole second moved it: %v", got)
	}
}
