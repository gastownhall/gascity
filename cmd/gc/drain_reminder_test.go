package main

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// blindAckSourceProvider fails exactly the acknowledgement-source read. It is
// how the fail-closed pins distinguish "no ack" from "cannot tell".
type blindAckSourceProvider struct {
	*runtime.Fake
}

func (p *blindAckSourceProvider) GetMeta(name, key string) (string, error) {
	if key == reconcilerDrainAckSourceKey {
		return "", fmt.Errorf("session unavailable")
	}
	return p.Fake.GetMeta(name, key)
}

// nudgeFailingProvider is an input-dead pane: the session is alive and idle, but
// nothing typed at it lands.
type nudgeFailingProvider struct {
	*runtime.Fake
}

func (p *nudgeFailingProvider) Nudge(name string, content []runtime.ContentBlock) error {
	_ = p.Fake.Nudge(name, content)
	return fmt.Errorf("input fenced")
}

// drainReminderEnv is one durably wedged row: drain-acked, moved to
// stop-pending, its stop queued — and the runtime still alive, idle, and
// carrying no agent provenance. That is the class that sits for days. Each case
// below removes exactly one entitlement.
type drainReminderEnv struct {
	t       *testing.T
	sp      *runtime.Fake
	store   beads.Store
	clk     *clock.Fake
	bead    beads.Bead
	out     *bytes.Buffer
	name    string
	now     time.Time
	drainAt string
}

func newDrainReminderEnv(t *testing.T) *drainReminderEnv {
	t.Helper()
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	name := "gc-city-worker-1"
	drainAt := now.Add(-3 * time.Hour).UTC().Format(time.RFC3339)

	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("start fake session: %v", err)
	}
	// The runtime carries reconciler-authored provenance and no agent ack — the
	// state the stop-pending re-examination refuses to act on.
	mustSetMeta(t, sp, name, reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue)
	mustSetMeta(t, sp, name, "GC_DRAIN_ACK", "1")
	sp.SetActivity(name, now.Add(-30*time.Minute))

	bead, err := store.Create(beads.Bead{
		Title: "session",
		Type:  sessionBeadType,
		Metadata: map[string]string{
			"session_name":   name,
			"template":       "worker",
			"generation":     "3",
			"state":          string(sessionpkg.StateDraining),
			"state_reason":   sessionpkg.DrainAckStopPendingReason,
			"drain_at":       drainAt,
			"instance_token": "tok-a",
			"pool_managed":   "true",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	return &drainReminderEnv{
		t: t, sp: sp, store: store, clk: &clock.Fake{Time: now},
		bead: bead, out: &bytes.Buffer{}, name: name, now: now, drainAt: drainAt,
	}
}

func mustSetMeta(t *testing.T, sp *runtime.Fake, name, key, value string) {
	t.Helper()
	if err := sp.SetMeta(name, key, value); err != nil {
		t.Fatalf("SetMeta %s: %v", key, err)
	}
}

func (e *drainReminderEnv) info() sessionpkg.Info {
	e.t.Helper()
	got, err := e.store.Get(e.bead.ID)
	if err != nil {
		e.t.Fatalf("read session bead: %v", err)
	}
	return seedSessionInfo(got)
}

func (e *drainReminderEnv) remind() drainReminderOutcome {
	e.t.Helper()
	return maybeRemindDrainingSession(e.sp, e.store, e.info(), e.clk, e.out)
}

func (e *drainReminderEnv) remindWith(sp runtime.Provider) drainReminderOutcome {
	e.t.Helper()
	return maybeRemindDrainingSession(sp, e.store, e.info(), e.clk, e.out)
}

func (e *drainReminderEnv) setMeta(kvs map[string]string) {
	e.t.Helper()
	if err := e.store.SetMetadataBatch(e.bead.ID, kvs); err != nil {
		e.t.Fatalf("set session metadata: %v", err)
	}
}

func (e *drainReminderEnv) meta(key string) string {
	e.t.Helper()
	got, err := e.store.Get(e.bead.ID)
	if err != nil {
		e.t.Fatalf("read session bead: %v", err)
	}
	return strings.TrimSpace(got.Metadata[key])
}

func (e *drainReminderEnv) nudges() []runtime.Call {
	var out []runtime.Call
	for _, c := range e.sp.SnapshotCalls() {
		if c.Method == "Nudge" && c.Name == e.name {
			out = append(out, c)
		}
	}
	return out
}

// providerWrites returns the mutating provider calls, which is what the
// agent-ack pin asserts the absence of.
func (e *drainReminderEnv) providerWrites() []runtime.Call {
	var out []runtime.Call
	for _, c := range e.sp.SnapshotCalls() {
		switch c.Method {
		case "SetMeta", "RemoveMeta", "Nudge", "NudgeNow", "SendKeys", "Stop", "Interrupt":
			out = append(out, c)
		}
	}
	return out
}

// beadSnapshot captures every reminder-owned key so the no-write pins can prove
// nothing moved.
func (e *drainReminderEnv) beadSnapshot() map[string]string {
	e.t.Helper()
	got, err := e.store.Get(e.bead.ID)
	if err != nil {
		e.t.Fatalf("read session bead: %v", err)
	}
	snapshot := make(map[string]string, 5)
	for _, key := range []string{drainReminderCountKey, drainReminderFailedKey, drainReminderAtKey, drainReminderDrainKey, drainReminderHoldKey} {
		snapshot[key] = got.Metadata[key]
	}
	return snapshot
}

func (e *drainReminderEnv) assertBeadUnchanged(before map[string]string) {
	e.t.Helper()
	after := e.beadSnapshot()
	for key, want := range before {
		if after[key] != want {
			e.t.Errorf("%s changed %q -> %q; this path must write nothing", key, want, after[key])
		}
	}
}

// The wedge, answered. A row whose stop did not take, still alive, still idle,
// still unacknowledged, draws its first reminder on FIRST SIGHT — there is no
// observe-first grace, because the caller only reaches here for a row that is
// already late.
func TestDrainReminderDeliversOnFirstSightOfAWedgedStopPendingRow(t *testing.T) {
	e := newDrainReminderEnv(t)

	if got := e.remind(); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered", got)
	}
	if n := len(e.nudges()); n != 1 {
		t.Fatalf("nudge count = %d, want 1", n)
	}
	if got := e.meta(drainReminderCountKey); got != "1" {
		t.Errorf("%s = %q, want 1", drainReminderCountKey, got)
	}
	if got, want := e.meta(drainReminderDrainKey), "tok-a/"+e.drainAt; got != want {
		t.Errorf("%s = %q, want %q", drainReminderDrainKey, got, want)
	}
	if got := e.meta(drainReminderAtKey); got != e.now.UTC().Format(time.RFC3339) {
		t.Errorf("%s = %q, want %q", drainReminderAtKey, got, e.now.UTC().Format(time.RFC3339))
	}
	if !strings.Contains(e.out.String(), "drain reminder 1/3 delivered to "+e.name) {
		t.Errorf("journal line missing from %q", e.out.String())
	}
}

// The stale-pane-environment survival contract: the no-argument ack binds the
// requester from the pane's own environment, which an adopted pane may no
// longer have. The reminder must name the id in the command it asks for.
func TestDrainReminderContentNamesExplicitAckCommand(t *testing.T) {
	e := newDrainReminderEnv(t)
	if got := e.remind(); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered", got)
	}
	msg := e.nudges()[0].Message
	if want := "gc runtime drain-ack " + e.bead.ID; !strings.Contains(msg, want) {
		t.Errorf("nudge %q does not contain %q", msg, want)
	}
	if !strings.Contains(msg, "A drain of session "+e.bead.ID) {
		t.Errorf("nudge %q does not name the session id", msg)
	}
	if !strings.Contains(msg, "(name "+e.name+")") {
		t.Errorf("nudge %q does not name the session name", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("nudge must be a single line, got %q", msg)
	}
}

// Only the wedge class, and only pool seats. The named-row gate is the one the
// design demanded: a named row's ack lane refuses agent provenance
// (named_row / policy_unsupported), so talking its agent into minting some
// would route it into a refusal class it does not sit in today.
func TestDrainReminderGovernsOnlyWedgedPoolRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch map[string]string
	}{
		{"not draining", map[string]string{"state": "active", "state_reason": ""}},
		{"draining for another reason", map[string]string{"state_reason": "pool-excess"}},
		{"named row", map[string]string{"pool_managed": ""}},
		{"no drain identity", map[string]string{"drain_at": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDrainReminderEnv(t)
			e.setMeta(tc.patch)
			before := e.beadSnapshot()

			if got := e.remind(); got != drainReminderSkipped {
				t.Fatalf("outcome = %v, want skipped", got)
			}
			if n := len(e.nudges()); n != 0 {
				t.Errorf("nudge count = %d, want 0", n)
			}
			e.assertBeadUnchanged(before)
		})
	}
}

func TestDrainReminderHoldsWhileSessionIsBusy(t *testing.T) {
	e := newDrainReminderEnv(t)
	e.sp.SetActivity(e.name, e.now.Add(-10*time.Second))

	if got := e.remind(); got != drainReminderHeld {
		t.Fatalf("outcome = %v, want held", got)
	}
	if n := len(e.nudges()); n != 0 {
		t.Errorf("nudge count = %d, want 0", n)
	}
	if got := e.meta(drainReminderHoldKey); got != drainReminderHoldBusy {
		t.Errorf("%s = %q, want %q", drainReminderHoldKey, got, drainReminderHoldBusy)
	}
	if got := e.meta(drainReminderCountKey); got != "" {
		t.Errorf("%s = %q, want unset", drainReminderCountKey, got)
	}
}

// A human at the pane is already on this row. Do not type into their session.
func TestDrainReminderHoldsWhileAttached(t *testing.T) {
	e := newDrainReminderEnv(t)
	e.sp.SetAttached(e.name, true)

	if got := e.remind(); got != drainReminderHeld {
		t.Fatalf("outcome = %v, want held", got)
	}
	if n := len(e.nudges()); n != 0 {
		t.Errorf("nudge count = %d, want 0", n)
	}
	if got := e.meta(drainReminderHoldKey); got != drainReminderHoldAttached {
		t.Errorf("%s = %q, want %q", drainReminderHoldKey, got, drainReminderHoldAttached)
	}
}

// An unreadable activity signal is not evidence of idleness, and it must say so
// under its own reason so an operator can tell the two holds apart.
func TestDrainReminderHoldsWhenActivityUnreadable(t *testing.T) {
	e := newDrainReminderEnv(t)
	e.sp.SetActivity(e.name, time.Time{})

	if got := e.remind(); got != drainReminderHeld {
		t.Fatalf("outcome = %v, want held", got)
	}
	if got := e.meta(drainReminderHoldKey); got != drainReminderHoldUnreadable {
		t.Errorf("%s = %q, want %q", drainReminderHoldKey, got, drainReminderHoldUnreadable)
	}
}

// Fail CLOSED on an unreadable acknowledgement. "Cannot tell" is not "no ack":
// this guard stands in front of every write in the pass, so a transient
// provider failure must hold, and must not leave a breadcrumb over a row whose
// state it could not read.
func TestDrainReminderHoldsAndWritesNothingWhenAckSourceUnreadable(t *testing.T) {
	e := newDrainReminderEnv(t)
	before := e.beadSnapshot()

	if got := e.remindWith(&blindAckSourceProvider{Fake: e.sp}); got != drainReminderHeld {
		t.Fatalf("outcome = %v, want held", got)
	}
	if n := len(e.nudges()); n != 0 {
		t.Errorf("nudge count = %d, want 0", n)
	}
	e.assertBeadUnchanged(before)
}

// The mutation pin. A landed agent acknowledgement is the outcome this pass
// exists to produce; if the guard is removed the reminder writes over it.
func TestDrainReminderWritesNothingOnceAgentAcked(t *testing.T) {
	e := newDrainReminderEnv(t)
	mustSetMeta(t, e.sp, e.name, reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
	e.setMeta(map[string]string{
		drainReminderCountKey: "1",
		drainReminderAtKey:    e.now.Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		drainReminderDrainKey: "tok-a/" + e.drainAt,
	})
	before := e.beadSnapshot()
	e.sp.Calls = nil

	if got := e.remind(); got != drainReminderSkipped {
		t.Fatalf("outcome = %v, want skipped", got)
	}
	if writes := e.providerWrites(); len(writes) != 0 {
		t.Errorf("provider writes = %+v, want none", writes)
	}
	if got, _ := e.sp.GetMeta(e.name, reconcilerDrainAckSourceKey); got != drainAckSourceAgentValue {
		t.Errorf("ack source = %q, want %q (clobbered)", got, drainAckSourceAgentValue)
	}
	e.assertBeadUnchanged(before)
}

// The hot-path pin. "Not due" is the overwhelmingly common answer on a
// reconcile tick, and it must cost zero provider round-trips: a tmux
// show-environment is a subprocess, and a wedged tmux server must not be able
// to stall the reconciler through this pass.
func TestDrainReminderPacingGateCostsNoProviderReads(t *testing.T) {
	e := newDrainReminderEnv(t)
	if got := e.remind(); got != drainReminderDelivered {
		t.Fatalf("first outcome = %v, want delivered", got)
	}

	e.clk.Time = e.now.Add(drainReminderInterval - time.Second)
	e.sp.Calls = nil
	if got := e.remind(); got != drainReminderSkipped {
		t.Fatalf("second outcome = %v, want skipped", got)
	}
	if calls := e.sp.SnapshotCalls(); len(calls) != 0 {
		t.Errorf("provider calls while not due = %+v, want none", calls)
	}
}

func TestDrainReminderWaitsOutTheCadenceBetweenAttempts(t *testing.T) {
	e := newDrainReminderEnv(t)
	if got := e.remind(); got != drainReminderDelivered {
		t.Fatalf("first outcome = %v, want delivered", got)
	}
	e.clk.Time = e.now.Add(drainReminderInterval - time.Second)
	if got := e.remind(); got != drainReminderSkipped {
		t.Fatalf("second outcome = %v, want skipped", got)
	}
	e.clk.Time = e.now.Add(drainReminderInterval)
	if got := e.remind(); got != drainReminderDelivered {
		t.Fatalf("third outcome = %v, want delivered", got)
	}
	if n := len(e.nudges()); n != 2 {
		t.Fatalf("nudge count = %d, want 2", n)
	}
	if got := e.meta(drainReminderCountKey); got != "2" {
		t.Errorf("%s = %q, want 2", drainReminderCountKey, got)
	}
}

// The budget belongs to ONE drain, not to an incarnation. A canceled drain
// leaves its markers behind on a session that goes back to work; the next drain
// of that same incarnation must start over rather than inherit a spent budget
// it never earned.
func TestDrainReminderBudgetIsScopedToOneDrain(t *testing.T) {
	e := newDrainReminderEnv(t)
	e.setMeta(map[string]string{
		drainReminderCountKey: strconv.Itoa(drainReminderMaxAttempts),
		drainReminderAtKey:    e.now.Add(-time.Hour).UTC().Format(time.RFC3339),
		drainReminderDrainKey: "tok-a/" + e.now.Add(-9*time.Hour).UTC().Format(time.RFC3339),
	})

	if got := e.remind(); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered (the spent budget belongs to an earlier drain)", got)
	}
	if got := e.meta(drainReminderCountKey); got != "1" {
		t.Errorf("%s = %q, want 1", drainReminderCountKey, got)
	}
	if got, want := e.meta(drainReminderDrainKey), "tok-a/"+e.drainAt; got != want {
		t.Errorf("%s = %q, want %q", drainReminderDrainKey, got, want)
	}
}

// The cap is durable: the marker survives a controller restart, so the budget
// resumes rather than replays, and exhaustion is announced exactly once.
func TestDrainReminderAttemptCapIsDurableAndAnnouncedOnce(t *testing.T) {
	e := newDrainReminderEnv(t)
	for i := 0; i < drainReminderMaxAttempts; i++ {
		e.clk.Time = e.now.Add(time.Duration(i) * drainReminderInterval)
		if got := e.remind(); got != drainReminderDelivered {
			t.Fatalf("attempt %d outcome = %v, want delivered", i+1, got)
		}
	}
	e.clk.Time = e.now.Add(4 * drainReminderInterval)
	e.out.Reset()

	if got := e.remind(); got != drainReminderExhausted {
		t.Fatalf("outcome = %v, want exhausted", got)
	}
	if n := len(e.nudges()); n != drainReminderMaxAttempts {
		t.Errorf("nudge count = %d, want %d (the cap must not replay)", n, drainReminderMaxAttempts)
	}
	if !strings.Contains(e.out.String(), "drain reminders exhausted for "+e.name) {
		t.Errorf("exhaustion journal line missing from %q", e.out.String())
	}
	if got := e.meta(drainReminderHoldKey); got != drainReminderHoldExhausted {
		t.Errorf("%s = %q, want %q", drainReminderHoldKey, got, drainReminderHoldExhausted)
	}

	e.out.Reset()
	if got := e.remind(); got != drainReminderExhausted {
		t.Fatalf("repeat outcome = %v, want exhausted", got)
	}
	if e.out.Len() != 0 {
		t.Errorf("exhaustion re-announced: %q", e.out.String())
	}
}

func TestDrainReminderSkipsWhenRuntimeIsGone(t *testing.T) {
	e := newDrainReminderEnv(t)
	if err := e.sp.Stop(e.name); err != nil {
		t.Fatalf("stop fake session: %v", err)
	}

	if got := e.remind(); got != drainReminderSkipped {
		t.Fatalf("outcome = %v, want skipped", got)
	}
	if n := len(e.nudges()); n != 0 {
		t.Errorf("nudge count = %d, want 0", n)
	}
}

// drainRemindersSpent is the durable question the enterprise escalation asks of
// the markers this file writes; pin it here, where the markers are produced.
func TestDrainRemindersSpentReadsTheDurableBudget(t *testing.T) {
	e := newDrainReminderEnv(t)
	for i := 0; i < drainReminderMaxAttempts; i++ {
		e.clk.Time = e.now.Add(time.Duration(i) * drainReminderInterval)
		if got := e.remind(); got != drainReminderDelivered {
			t.Fatalf("attempt %d outcome = %v, want delivered", i+1, got)
		}
	}
	bead, err := e.store.Get(e.bead.ID)
	if err != nil {
		t.Fatalf("read session bead: %v", err)
	}
	lastAt := e.now.Add(2 * drainReminderInterval)
	if drainRemindersSpent(bead, lastAt.Add(drainReminderInterval-time.Second)) {
		t.Error("budget reported spent before the last reminder had a full interval to be answered")
	}
	if !drainRemindersSpent(bead, lastAt.Add(drainReminderInterval)) {
		t.Error("budget not reported spent after three delivered reminders and a full interval")
	}
	// A marker from a different drain does not satisfy it, however old.
	bead.Metadata[drainReminderDrainKey] = "tok-a/" + e.now.Add(-9*time.Hour).UTC().Format(time.RFC3339)
	if drainRemindersSpent(bead, lastAt.Add(24*time.Hour)) {
		t.Error("an earlier drain's spent budget satisfied this drain's precondition")
	}
}

// An attempt that never arrived is not a reminder anybody ignored. The spend is
// never unwound — a permanently input-dead pane must not re-send forever — but
// it is recorded separately, so the escalation that follows says what actually
// happened instead of claiming three conversations that never occurred.
func TestDrainReminderRecordsUndeliverableAttemptsSeparately(t *testing.T) {
	e := newDrainReminderEnv(t)
	deaf := &nudgeFailingProvider{Fake: e.sp}

	for i := 0; i < drainReminderMaxAttempts; i++ {
		e.clk.Time = e.now.Add(time.Duration(i) * drainReminderInterval)
		if got := e.remindWith(deaf); got != drainReminderSkipped {
			t.Fatalf("attempt %d outcome = %v, want skipped (nothing was delivered)", i+1, got)
		}
	}

	if got := e.meta(drainReminderCountKey); got != strconv.Itoa(drainReminderMaxAttempts) {
		t.Errorf("%s = %q, want %d: the spend must not be unwound", drainReminderCountKey, got, drainReminderMaxAttempts)
	}
	if got := e.meta(drainReminderFailedKey); got != strconv.Itoa(drainReminderMaxAttempts) {
		t.Errorf("%s = %q, want %d", drainReminderFailedKey, got, drainReminderMaxAttempts)
	}
	if !strings.Contains(e.out.String(), "was undeliverable") {
		t.Errorf("undeliverable journal line missing from %q", e.out.String())
	}

	bead, err := e.store.Get(e.bead.ID)
	if err != nil {
		t.Fatalf("read session bead: %v", err)
	}
	// A budget nobody could receive earns no answer window: waiting one out for
	// messages that never arrived is waiting for nothing.
	if !drainRemindersSpent(bead, e.clk.Time) {
		t.Error("an all-undeliverable budget is not reported spent")
	}
	want := fmt.Sprintf("%d undeliverable reminder attempts (input-dead pane)", drainReminderMaxAttempts)
	if got := drainReminderSpendPhraseFor(bead); got != want {
		t.Errorf("spend phrase = %q, want %q", got, want)
	}
}

// The control for the pin above: a delivered budget reads as unanswered, and it
// still earns its full answer window.
func TestDrainReminderDeliveredBudgetReadsAsUnanswered(t *testing.T) {
	e := newDrainReminderEnv(t)
	for i := 0; i < drainReminderMaxAttempts; i++ {
		e.clk.Time = e.now.Add(time.Duration(i) * drainReminderInterval)
		if got := e.remind(); got != drainReminderDelivered {
			t.Fatalf("attempt %d outcome = %v, want delivered", i+1, got)
		}
	}
	bead, err := e.store.Get(e.bead.ID)
	if err != nil {
		t.Fatalf("read session bead: %v", err)
	}
	if got := e.meta(drainReminderFailedKey); got != "0" {
		t.Errorf("%s = %q, want 0", drainReminderFailedKey, got)
	}
	if drainRemindersSpent(bead, e.clk.Time) {
		t.Error("a delivered budget skipped its answer window")
	}
	want := fmt.Sprintf("%d unanswered reminders", drainReminderMaxAttempts)
	if got := drainReminderSpendPhraseFor(bead); got != want {
		t.Errorf("spend phrase = %q, want %q", got, want)
	}
}

// The wiring pin. The stop-pending re-examination is the anchor, and the arm it
// reminds from is the wedge itself: the stop was queued, the tick came back
// round, and the runtime is still alive. A row whose runtime is gone takes the
// finalize arm instead and is never nudged.
func TestFinalizeDrainAckStopPendingRemindsTheLiveWedgeOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		alive      bool
		wantNudges int
	}{
		{"runtime still alive", true, 1},
		{"runtime gone", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDrainReminderEnv(t)
			if !tc.alive {
				if err := e.sp.Stop(e.name); err != nil {
					t.Fatalf("stop fake session: %v", err)
				}
			}
			e.setMeta(map[string]string{"work_dir": t.TempDir(), "provider": "claude"})
			cfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}

			finalizeDrainAckStopPendingSessions(
				t.TempDir(), cfg, e.sp, beads.SessionStore{Store: e.store}, nil,
				[]sessionpkg.Info{e.info()}, nil, newDrainTracker(), &asyncStartTracker{},
				e.clk, nil, e.out,
			)

			if n := len(e.nudges()); n != tc.wantNudges {
				t.Errorf("nudge count = %d, want %d", n, tc.wantNudges)
			}
		})
	}
}
