package main

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	testTriggerBeadIDKey       = "gc.trigger_bead_id"
	testTriggerBeadStoreRefKey = "gc.trigger_bead_store_ref"
)

func idleClaimTestCfg() *config.City {
	return &config.City{Agents: []config.Agent{{
		Name:  "agent-a",
		Nudge: "Run gc hook --claim --json now; if it returns work, execute the claimed formula immediately.",
	}}}
}

func idleClaimPoolSession() beads.Bead {
	return beads.Bead{
		ID:     "session-bead-a",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"session_name":       "session-a",
			"pool_managed":       "true",
			"template":           "agent-a",
			testTriggerBeadIDKey: "work-a",
		},
	}
}

//nolint:unparam // sessionName is always "session-a" today; kept as a param so new cases can vary it.
func runningIdleClaimFake(t *testing.T, sessionName string) *runtime.Fake {
	t.Helper()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("fake start: %v", err)
	}
	return sp
}

func mustGetTestBead(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return b
}

func TestNudgeStalledPoolClaims_NudgesAfterGrace(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), nil, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("first tick Nudge calls = %d, want 0 inside grace", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeTriggerKey]; got != "work-a" {
		t.Fatalf("idle claim marker trigger = %q, want work-a", got)
	}

	clk.Advance(idleClaimNudgeGrace + time.Second)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), nil, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 1 {
		t.Fatalf("Nudge calls = %d, want 1 after grace", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("idle claim attempt count = %q, want 1", got)
	}
	if got := session.Metadata[idleClaimNudgeAtKey]; got != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("idle claim last nudge at = %q, want %q", got, clk.Now().UTC().Format(time.RFC3339))
	}
}

func TestNudgeStalledPoolClaims_UsesClaimFallbackWhenNudgeIsBlank(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nudge string
	}{
		{name: "empty", nudge: ""},
		{name: "whitespace", nudge: " \t "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := runningIdleClaimFake(t, "session-a")
			cfg := idleClaimTestCfg()
			cfg.Agents[0].Nudge = tc.nudge
			session := idleClaimPoolSession()
			work := []beads.Bead{{ID: "work-a", Status: "open"}}
			store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var out bytes.Buffer

			nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, base, nil, &out)
			nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, base.Add(idleClaimNudgeGrace+time.Second), nil, &out)

			for _, call := range sp.SnapshotCalls() {
				if call.Method == "Nudge" && call.Name == "session-a" {
					if got, want := call.Message, defaultPoolClaimNudge; got != want {
						t.Fatalf("fallback nudge = %q, want %q", got, want)
					}
					return
				}
			}
			t.Fatal("fallback nudge was not delivered")
		})
	}
}

func TestPoolClaimBackstopContent_PreservesExplicitNudge(t *testing.T) {
	cfg := idleClaimTestCfg()
	cfg.Agents[0].Nudge = "  Use the configured wake text exactly.  "

	if got, want := (poolClaimBackstop{cfg: cfg}).content(idleClaimPoolSession()), "Use the configured wake text exactly."; got != want {
		t.Fatalf("claim nudge = %q, want normalized configured value %q", got, want)
	}
}

func TestPoolClaimBackstopContent_FailsClosedForUnknownTemplate(t *testing.T) {
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	session.Metadata["template"] = "unknown-agent"

	if got := (poolClaimBackstop{cfg: cfg}).content(session); got != "" {
		t.Fatalf("claim nudge for unknown template = %q, want empty", got)
	}
}

// Two stores can hold beads with the same ID, so the backstop must resolve the
// slot's trigger through the store ref it was bound to. Here the rig-scoped
// copy is still open (nudge-worthy) while the city-scoped copy of the same ID
// is closed; keying on ID alone would read the wrong bead and stay silent.
func TestNudgeStalledPoolClaims_MatchesTriggerStoreRefForDuplicateIDs(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata[testTriggerBeadStoreRefKey] = "rig:fixture"
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = "0"
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	work := []beads.Bead{
		{ID: "work-a", Status: "open"},
		{ID: "work-a", Status: "closed"},
	}
	storeRefs := []string{"rig:fixture", "city:test-city"}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: base.Add(idleClaimNudgeGrace + time.Second)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, storeRefs, clk.Now(), nil, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 1 {
		t.Fatalf("Nudge calls = %d, want 1 for the open rig-scoped trigger", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("idle claim attempt count = %q, want 1", got)
	}
}

func TestNudgeStalledPoolClaims_NeverTouchesWorkingSlot(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = "1"
	session.Metadata[idleClaimNudgeAtKey] = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "in_progress", Assignee: "session-a"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), nil, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("working slot Nudge calls = %d, want 0", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeTriggerKey]; got != "" {
		t.Fatalf("idle claim marker trigger = %q, want cleared", got)
	}
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "" {
		t.Fatalf("idle claim marker count = %q, want cleared", got)
	}
	if got := session.Metadata[idleClaimNudgeAtKey]; got != "" {
		t.Fatalf("idle claim marker at = %q, want cleared", got)
	}
}

func TestNudgeStalledPoolClaims_GivesUpAtCap(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = strconv.Itoa(idleClaimNudgeMaxAttempts)
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: base.Add(time.Hour)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), nil, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("Nudge calls past cap = %d, want 0", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != strconv.Itoa(idleClaimNudgeMaxAttempts) {
		t.Fatalf("idle claim attempt count = %q, want cap preserved", got)
	}
}

// The attempt is reserved on the session bead BEFORE delivery, so a nudge the
// provider fails to deliver still consumes one of the bounded attempts. That is
// what stops a slot whose provider is wedged from being re-nudged on every tick
// forever; the cost is that transient delivery failures burn the cap. The
// failing-provider fixture is continuationFailingNudgeProvider
// (continuation_nudge_test.go), shared across both backstop lanes.
func TestNudgeStalledPoolClaims_DeliveryFailureConsumesAttempt(t *testing.T) {
	sp := &continuationFailingNudgeProvider{Provider: runningIdleClaimFake(t, "session-a")}
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = "0"
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: base.Add(idleClaimNudgeGrace + time.Second)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), nil, &out)
	if sp.nudgeCalls != 1 {
		t.Fatalf("delivery calls = %d, want 1 failed attempt", sp.nudgeCalls)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("persisted attempt count = %q, want 1 despite delivery failure", got)
	}
	if got := session.Metadata[idleClaimNudgeAtKey]; got != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("persisted attempt time = %q, want %q", got, clk.Now().UTC().Format(time.RFC3339))
	}

	// The reservation paces the next retry exactly as a delivered nudge would:
	// nothing more is attempted until the backoff elapses.
	clk.Advance(idleClaimNudgeBackoff - time.Second)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), nil, &out)
	if sp.nudgeCalls != 1 {
		t.Fatalf("inside-backoff delivery calls = %d, want unchanged 1", sp.nudgeCalls)
	}

	for want := 2; want <= idleClaimNudgeMaxAttempts; want++ {
		session = mustGetTestBead(t, store, session.ID)
		clk.Advance(idleClaimNudgeBackoff + time.Second)
		nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), nil, &out)
		if sp.nudgeCalls != want {
			t.Fatalf("attempt %d delivery calls = %d, want %d", want, sp.nudgeCalls, want)
		}
	}

	// Every attempt failed, so exhausted() is reached without the trigger ever
	// being claimed: the lane stops attempting and leaves the cap in place.
	session = mustGetTestBead(t, store, session.ID)
	clk.Advance(time.Hour)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), nil, &out)
	if sp.nudgeCalls != idleClaimNudgeMaxAttempts {
		t.Fatalf("past-cap delivery calls = %d, want %d", sp.nudgeCalls, idleClaimNudgeMaxAttempts)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != strconv.Itoa(idleClaimNudgeMaxAttempts) {
		t.Fatalf("persisted attempt count = %q, want cap %d preserved", got, idleClaimNudgeMaxAttempts)
	}
}

func TestNudgeStalledPoolClaims_SkipsNonPool(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	delete(session.Metadata, "pool_managed")
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), nil, &out)
	clk.Advance(time.Hour)
	session = mustGetTestBead(t, store, session.ID)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), nil, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("non-pool Nudge calls = %d, want 0", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeTriggerKey]; got != "" {
		t.Fatalf("non-pool marker trigger = %q, want empty", got)
	}
}

// drainRecorder records the drain requests the claim backstop makes.
type drainRecorder struct{ ids []string }

func (d *drainRecorder) request(b beads.Bead) error {
	d.ids = append(d.ids, b.ID)
	return nil
}

// Kills a wedged slot holding its pool seat forever (mc-zndi7.80): once the
// bounded nudges are spent and the slot's own trigger is still open and
// unclaimed, the backstop requests the slot's drain once for its
// incarnation, delivering no more nudges; the latch holds on the next ticks,
// re-arms for a new incarnation or after the retry window, and a trigger the
// slot claimed in the meantime drains nothing.
func TestNudgeStalledPoolClaims_DrainsAWedgedSlotAtCap(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	atCap := func(token string) beads.Bead {
		s := idleClaimPoolSession()
		s.Metadata[idleClaimNudgeTriggerKey] = "work-a"
		s.Metadata[idleClaimNudgeCountKey] = strconv.Itoa(idleClaimNudgeMaxAttempts)
		s.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
		s.Metadata["instance_token"] = token
		return s
	}
	open := []beads.Bead{{ID: "work-a", Status: "open"}}

	sp := runningIdleClaimFake(t, "session-a")
	session := atCap("tok-1")
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	drains := &drainRecorder{}
	var out bytes.Buffer
	now := base.Add(idleClaimNudgeBackoff + time.Second)
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{session}, open, nil, now, drains.request, &out)
	session = mustGetTestBead(t, store, session.ID)
	if len(drains.ids) != 1 || drains.ids[0] != session.ID || sp.CountCalls("Nudge", "session-a") != 0 {
		t.Fatalf("drains %v nudges %d, want one drain and no nudge", drains.ids, sp.CountCalls("Nudge", "session-a"))
	}
	if session.Metadata[idleClaimNudgeStalledTokenKey] != "tok-1" || session.Metadata[idleClaimNudgeStalledKey] != now.UTC().Format(time.RFC3339) ||
		session.Metadata[idleClaimNudgeCountKey] != strconv.Itoa(idleClaimNudgeMaxAttempts) || !strings.Contains(out.String(), "draining (execution-stalled)") {
		t.Fatalf("metadata %v log %q, want the latch for tok-1, the cap kept, and the drain logged", session.Metadata, out.String())
	}

	// The latch holds on the next ticks of the same incarnation.
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{session}, open, nil, now.Add(5*time.Minute), drains.request, &out)
	if len(drains.ids) != 1 {
		t.Fatalf("drains %v, want the latch to hold", drains.ids)
	}
	// It re-arms past the retry window.
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{mustGetTestBead(t, store, session.ID)}, open, nil, now.Add(executionStalledLatchRetryAfter+time.Second), drains.request, &out)
	if len(drains.ids) != 2 {
		t.Fatalf("drains %v, want a retry past the window", drains.ids)
	}

	// A new incarnation is drained again, the old latch notwithstanding.
	next := mustGetTestBead(t, store, session.ID)
	next.Metadata["instance_token"] = "tok-2"
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{next}, open, nil, now.Add(executionStalledLatchRetryAfter+2*time.Second), drains.request, &out)
	if len(drains.ids) != 3 {
		t.Fatalf("drains %v, want a new incarnation drained", drains.ids)
	}

	// A trigger the slot claimed drains nothing.
	claimed := atCap("tok-3")
	cstore := beads.NewMemStoreFrom(0, []beads.Bead{claimed}, nil)
	cdrains := &drainRecorder{}
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), cstore, []beads.Bead{claimed}, []beads.Bead{{ID: "work-a", Status: "in_progress", Assignee: "session-a"}}, nil, now, cdrains.request, &out)
	if len(cdrains.ids) != 0 {
		t.Fatalf("drains %v, want none for a claimed trigger", cdrains.ids)
	}
}

// Kills a slot whose template resolves to no agent parking silently forever
// (mc-zndi7.80): with no nudge to send it is drained once the grace passes.
func TestNudgeStalledPoolClaims_UnresolvedTemplateDrainsAfterGrace(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sp := runningIdleClaimFake(t, "session-a")
	session := idleClaimPoolSession()
	session.Metadata["template"] = "gone"
	session.Metadata["instance_token"] = "tok-1"
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	open := []beads.Bead{{ID: "work-a", Status: "open"}}
	drains := &drainRecorder{}
	var out bytes.Buffer
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{session}, open, nil, base, drains.request, &out)
	if len(drains.ids) != 0 {
		t.Fatalf("drains %v at first sight, want the grace first", drains.ids)
	}
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{mustGetTestBead(t, store, session.ID)}, open, nil, base.Add(idleClaimNudgeGrace+time.Second), drains.request, &out)
	if len(drains.ids) != 1 || sp.CountCalls("Nudge", "session-a") != 0 {
		t.Fatalf("drains %v nudges %d, want a drain after the grace and no nudge", drains.ids, sp.CountCalls("Nudge", "session-a"))
	}
}

// Kills the production-dead backstop (mc-zndi7.80: build 79bd3633a1 nudged
// only agents that configure a nudge): a rig-scoped pool shaped like
// beads/claude-max-gw, no nudge configured, is nudged with the default
// claim nudge after the grace, its trigger a routed step in the city store.
func TestNudgeStalledPoolClaims_RigPoolWithoutNudgeGetsTheDefault(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "c"},
		Rigs:      []config.Rig{{Name: "beads", Path: t.TempDir()}},
		Agents:    []config.Agent{{Name: "claude-max-gw", Dir: "beads", MaxActiveSessions: intPtr(100)}},
	}
	sp := runningIdleClaimFake(t, "beads--claude-max-gw-2-pool")
	session := beads.Bead{ID: "gcg-session-2", Status: "open", Type: "session", Metadata: map[string]string{
		"session_name": "beads--claude-max-gw-2-pool", "pool_managed": "true", "pool_slot": "2", "template": "beads/claude-max-gw",
		"agent_name": "beads/claude-max-gw-2", testTriggerBeadIDKey: "gcg-step", beadmeta.TriggerBeadStoreRefMetadataKey: "city:c",
	}}
	step := beads.Bead{ID: "gcg-step", Status: "open", Metadata: map[string]string{
		"gc.routed_to": "beads/claude-max-gw", "gc.session_affinity": "require", "gc.continuation_group": "main",
	}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	var out bytes.Buffer
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, []beads.Bead{step}, []string{"city:c"}, base, nil, &out)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{mustGetTestBead(t, store, session.ID)}, []beads.Bead{step}, []string{"city:c"}, base.Add(idleClaimNudgeGrace+time.Second), nil, &out)
	if got := sp.CountCalls("Nudge", "beads--claude-max-gw-2-pool"); got != 1 {
		t.Fatalf("nudges = %d (log %q), want the default claim nudge delivered", got, out.String())
	}
	if !strings.Contains(out.String(), "nudged beads--claude-max-gw-2-pool for gcg-step") {
		t.Fatalf("log %q, want the nudge logged", out.String())
	}
}

// Kills a wedged pool slot that is never recycled (mc-zndi7.80, the
// 2026-10-05 adopt-PR stall): a live slot bound to routed work whose agent
// never answers (a provider refusing every turn) is nudged three times,
// paced, then drained through the controller's real drain request with the
// non-cancelable execution-stalled reason; the next reconcile tick, whose
// desired state still wants the pool, does not cancel that drain, and the
// slot gets no further nudges.
func TestWedgedPoolSlotIsRecycledWhileRoutedWorkWaits(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1), StartCommand: "true"}}}
	env.addDesired("worker", "worker", false)
	work, err := env.store.Create(beads.Bead{Title: "step", Type: "task", Metadata: map[string]string{
		"gc.routed_to": "worker", "gc.session_affinity": "require", "gc.continuation_group": "main",
	}})
	if err != nil {
		t.Fatal(err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{"pool_managed": "true", testTriggerBeadIDKey: work.ID, "instance_token": "tok-1"})
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	cr := &CityRuntime{
		cityPath: env.city, cityName: "test-city", cfg: env.cfg, sp: env.sp, standaloneCityStore: env.store,
		sessionDrains: env.dt, rec: events.Discard, stdout: io.Discard, stderr: io.Discard,
	}

	var out bytes.Buffer
	now := time.Now().UTC()
	for tick := 0; tick < 12; tick++ { // four-minute ticks: grace, three paced nudges, the drain
		raw, err := loadSessionBeads(env.store)
		if err != nil {
			t.Fatal(err)
		}
		nudgeStalledPoolClaims(env.sp, env.cfg, beads.SessionStore{Store: env.store}, raw, []beads.Bead{work}, []string{"city"}, now.Add(time.Duration(tick)*4*time.Minute), cr.requestExecutionStalledDrain, &out)
	}
	if got := env.sp.CountCalls("Nudge", "worker"); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("nudges = %d, want %d before the slot is given up on (log %q)", got, idleClaimNudgeMaxAttempts, out.String())
	}
	ds := env.dt.get(session.ID)
	if ds == nil || ds.reason != executionStalledDrainReason {
		t.Fatalf("drain %+v, want the slot draining for %s (log %q)", ds, executionStalledDrainReason, out.String())
	}

	env.reconcile([]beads.Bead{mustGetTestBead(t, env.store, session.ID)})
	if ds := env.dt.get(session.ID); ds == nil || ds.reason != executionStalledDrainReason {
		t.Fatalf("drain after a reconcile tick %+v, want it kept: the routed trigger's wake reason must not cancel it", ds)
	}
}

// Kills the legacy tick wiring the claim backstop's terminal drain to
// nothing (mc-zndi7.80): the controller's bead reconcile tick runs the
// backstop with its real drain request, so a live pool slot at the cap whose
// routed trigger is still open is draining (execution-stalled) after one tick.
func TestBeadReconcileTickDrainsAWedgedPoolSlot(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "step", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": "worker-1", "template": "worker", "agent_name": "worker-1", "pool_managed": "true", "pool_slot": "1", "state": "awake",
		"instance_token": "tok-1", "generation": "1", testTriggerBeadIDKey: work.ID,
		idleClaimNudgeTriggerKey: work.ID, idleClaimNudgeCountKey: strconv.Itoa(idleClaimNudgeMaxAttempts),
		idleClaimNudgeAtKey: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1), StartCommand: "true"}}}
	cr := &CityRuntime{
		cityPath: t.TempDir(), cityName: "test-city", cfg: cfg, sp: sp, standaloneCityStore: store,
		sessionDrains: newDrainTracker(), rec: events.Discard, stdout: io.Discard, stderr: io.Discard,
	}
	result := DesiredStateResult{
		State:                              map[string]TemplateParams{"worker-1": {TemplateName: "worker", SessionName: "worker-1", Command: "true"}},
		PoolDesiredCounts:                  map[string]int{"worker": 1}, // the routed step's demand keeps the slot awake, as in production
		ReadyUnassignedRoutedWorkBeads:     []beads.Bead{work},
		ReadyUnassignedRoutedWorkStoreRefs: []string{"city"},
	}
	cr.beadReconcileTick(context.Background(), newSeatWork(cr.workLegs()), result, newSessionBeadSnapshot([]beads.Bead{session}), nil, false)
	if ds := cr.sessionDrains.get(session.ID); ds == nil || ds.reason != executionStalledDrainReason {
		t.Fatalf("drain %+v, want the wedged slot draining for %s", ds, executionStalledDrainReason)
	}
}
