package main

import (
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The dead row of S1's table (CONTRACT v5 S5; plan C5a3) on the launch kit:
// bringUp of gc-1 (generation 3, token tok) over a dead runtime s-gc-1.

// recycleLeaf is the kit's leaf whose Peek fails with peekErr, and whose
// attach probe reports attached.
type recycleLeaf struct {
	*startHookLeaf
	peekErr   error
	attached  bool
	attachErr error
}

func (l *recycleLeaf) Peek(name string, lines int) (string, error) {
	if l.peekErr != nil {
		return "", l.peekErr
	}
	return l.startHookLeaf.Peek(name, lines)
}

func (l *recycleLeaf) IsAttachedWithError(string) (bool, error) { return l.attached, l.attachErr }

func (l *recycleLeaf) Capabilities() runtime.ProviderCapabilities {
	c := l.startHookLeaf.Capabilities()
	c.CanReportAttachment = true
	return c
}

// deadKit is the launch kit with a dead runtime under s-gc-1 carrying id and
// token (epoch 3), its pane showing pane, and a recorder.
func deadKit(t *testing.T, zombie bool, id, token, pane string, meta ...string) (*txKit, *recycleLeaf, *events.Fake) {
	t.Helper()
	k, leaf := launchKit(t, "asleep", meta...)
	k.runtimeAs(id, token, func(r *simRuntime) {
		r.corpse, r.zombie = !zombie, zombie
	})
	k.sp.SetPeekOutput("s-gc-1", pane)
	rec := events.NewFake()
	k.p.held.start.rec = rec
	l := &recycleLeaf{startHookLeaf: leaf}
	k.p.Runtime = l
	return k, l, rec
}

func (k *txKit) killsBy(method string) int {
	n := 0
	for _, kill := range k.sp.kills {
		if kill.Method == method {
			n++
		}
	}
	return n
}

func crashes(rec *events.Fake) int {
	evts, _ := rec.List(events.Filter{Type: events.SessionCrashed})
	return len(evts)
}

// Kills a dead runtime launched over without its pane classified, a recycle
// that stops a runtime that is not the row's own, and a crash attributed to
// another row: the row's own corpse or zombie is removed by exact object and
// the row launches; its crash output records one session.crashed; another
// row's corpse is removed unclassified; another row's zombie refuses
// occupied and is never stopped.
func TestStartRecyclesTheRowsOwnDeadRuntime(t *testing.T) {
	for _, c := range []struct {
		name          string
		zombie        bool
		id, token     string
		pane          string
		method        string
		kills, starts int
		crashed       int
		cause         string
	}{
		{"own corpse, no output", false, "gc-1", "tok", "", "KillCorpseObject", 1, 1, 0, ""},
		{"own corpse, crashed: no crash event (legacy's are a zombie's)", false, "gc-1", "tok", "panic: boom", "KillCorpseObject", 1, 1, 0, ""},
		{"own zombie, crashed", true, "gc-1", "tok", "panic: boom", "KillZombieObject", 1, 1, 1, ""},
		{"another row's corpse", false, "gc-9", "theirs", "panic: boom", "KillCorpseObject", 1, 1, 0, ""},
		{"another row's zombie", true, "gc-9", "theirs", "panic: boom", "KillZombieObject", 0, 0, 0, causeOccupied},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, _, rec := deadKit(t, c.zombie, c.id, c.token, c.pane)
			s := k.runKind()
			if k.killsBy(c.method) != c.kills || len(k.starts()) != c.starts || crashes(rec) != c.crashed {
				t.Fatalf("settlement %+v: kills %d starts %d crashes %d, want %d %d %d", s, k.killsBy(c.method), len(k.starts()), crashes(rec), c.kills, c.starts, c.crashed)
			}
			if c.cause != "" && s.Cause != c.cause {
				t.Fatalf("settlement %+v, want refused %s", s, c.cause)
			}
			if c.starts == 1 && s.Outcome != settledLanded {
				t.Fatalf("settlement %+v, want the launch landed", s)
			}
		})
	}
}

// Kills a restart loop over a provider's terminal error or rate-limit
// screen (SESS-088, SESS-530; §12.2 #19): the row's own dead pane parks the
// row, or quarantines a crash candidate 30 minutes, with no kill, no launch
// and no crash; a row already parked writes nothing again.
func TestStartDeadPaneScreens(t *testing.T) {
	recent := gatherNow.Add(-5 * time.Second).Format(time.RFC3339)
	t.Run("terminal", func(t *testing.T) {
		k, _, rec := deadKit(t, true, "gc-1", "tok", "Error: model_not_found")
		if s := k.runKind(); s.Outcome != settledLanded || k.meta("sleep_reason") != string(session.SleepReasonProviderTerminalError) || len(k.sp.kills) != 0 || len(k.starts()) != 0 || crashes(rec) != 0 {
			t.Fatalf("settlement %+v reason %q kills %d starts %d, want parked", s, k.meta("sleep_reason"), len(k.sp.kills), len(k.starts()))
		}
	})
	t.Run("already parked", func(t *testing.T) {
		k, _, _ := deadKit(t, true, "gc-1", "tok", "Error: model_not_found", "provider_terminal_error", "model_not_found", "sleep_reason", string(session.SleepReasonProviderTerminalError))
		if s := k.runKind(); s.Outcome != settledNoop || len(k.sp.kills) != 0 {
			t.Fatalf("settlement %+v kills %d, want a no-op", s, len(k.sp.kills))
		}
	})
	t.Run("rate limit", func(t *testing.T) {
		k, _, rec := deadKit(t, false, "gc-1", "tok", "Usage limit reached", "last_woke_at", recent)
		if s := k.runKind(); s.Outcome != settledLanded || k.meta("sleep_reason") != string(session.SleepReasonRateLimit) || k.meta("quarantined_until") == "" || len(k.sp.kills) != 0 || len(k.starts()) != 0 || crashes(rec) != 0 {
			t.Fatalf("settlement %+v reason %q, want quarantined", s, k.meta("sleep_reason"))
		}
	})
	t.Run("another row's screen is not this row's", func(t *testing.T) {
		k, _, _ := deadKit(t, false, "gc-9", "theirs", "Error: model_not_found")
		if k.runKind(); k.meta("sleep_reason") != "" || k.killsBy("KillCorpseObject") != 1 {
			t.Fatalf("reason %q kills %d, want the corpse removed and nothing parked", k.meta("sleep_reason"), k.killsBy("KillCorpseObject"))
		}
	})
}

// Kills a recycle that tears down an attached pane, a classification over a
// failed peek, and a crash recorded per refused attempt: an attached dead
// pane refuses with nothing removed and no crash; a failed peek refuses
// peek-error with nothing removed.
func TestStartRecycleRefusals(t *testing.T) {
	for name, set := range map[string]func(*recycleLeaf){
		"attached":            func(l *recycleLeaf) { l.attached = true },
		"an attach probe err": func(l *recycleLeaf) { l.attachErr = errors.New("probe failed") },
	} {
		k, leaf, rec := deadKit(t, true, "gc-1", "tok", "panic: boom")
		set(leaf)
		if s := k.runKind(); s.Cause != causeAttached || len(k.sp.kills) != 0 || crashes(rec) != 0 || len(k.starts()) != 0 {
			t.Fatalf("%s: settlement %+v kills %d crashes %d, want refused attached", name, s, len(k.sp.kills), crashes(rec))
		}
	}
	k, leaf, _ := deadKit(t, true, "gc-1", "tok", "panic: boom")
	leaf.peekErr = errors.New("peek failed")
	if s := k.runKind(); s.Cause != causePeekError || len(k.sp.kills) != 0 {
		t.Fatalf("peek error: settlement %+v kills %d, want refused peek-error", s, len(k.sp.kills))
	}
}

// Kills a PreWake that stamps a token on a dead incarnation: the recycle
// runs before it, so the launch's PreWake bumps the generation once, over a
// name already cleared.
func TestStartRecycleBeforePreWake(t *testing.T) {
	k, _, _ := deadKit(t, false, "gc-1", "tok", "")
	if s := k.runKind(); s.Outcome != settledLanded || k.meta("generation") != "4" {
		t.Fatalf("settlement %+v generation %q, want one PreWake after the recycle", s, k.meta("generation"))
	}
	if st := k.starts(); len(st) != 1 || !st[0].Created {
		t.Fatalf("starts %+v, want one fresh runtime", st)
	}
}

// Kills a rollback that leaves the corpse it counts stopped (C5a2's
// TODO): A10's rollback removes a corpse under the pending create's name by
// exact object, its own or another row's, then closes the row.
func TestRollbackRemovesTheCorpse(t *testing.T) {
	for _, id := range []string{"gc-1", "gc-9"} {
		k := startKit(t, intentRollback, "creating", "pending_create_claim", "true", "pending_create_started_at", gatherNow.Add(-11*time.Minute).Format(time.RFC3339))
		k.it.Deadline = gatherNow.Add(time.Minute)
		k.runtimeAs(id, "tok", func(r *simRuntime) { r.corpse = true })
		s := k.runKind()
		if b, _ := k.backing.Get("gc-1"); !s.Closed || b.Status != "closed" || k.killsBy("KillCorpseObject") != 1 {
			t.Fatalf("%s's corpse: settlement %+v row %s kills %+v, want removed and rolled back", id, s, b.Status, k.sp.kills)
		}
	}
}

// Kills a start that died on a provider's terminal error relaunched at its
// backoff forever (§12.2 #19): the failure Call reads the dead pane's
// screen, and the abandon parks the row with no wake failure accrued.
func TestDiedDuringStartupOnATerminalErrorParks(t *testing.T) {
	k, _ := launchKit(t, "asleep")
	k.sp.SetPeekOutput("s-gc-1", "Error: model_not_found")
	k.on(seamAfterCall, func() {})
	k.on(seamAfterCall, func() { k.mutate(func(r *simRuntime) { r.zombie = true }) })
	s := k.runKind()
	if s.Cause != causeDiedDuringStartup || k.meta("sleep_reason") != string(session.SleepReasonProviderTerminalError) || k.meta("wake_attempts") != "" {
		t.Fatalf("settlement %+v reason %q attempts %q, want died-during-startup parked with no accrual", s, k.meta("sleep_reason"), k.meta("wake_attempts"))
	}
}

// Kills a crash recorded for a kill that did not land, a recycle over a
// read whose object changed, and a quarantine of a pane that is no crash
// candidate: a kill that finds another object records no crash; a dead
// object replaced across the read refuses; a rate-limit screen on a row
// with no wake recorded is recycled as a crash, not quarantined.
func TestStartRecycleGuards(t *testing.T) {
	t.Run("a kill that missed records no crash", func(t *testing.T) {
		k, _, rec := deadKit(t, true, "gc-1", "tok", "panic: boom")
		k.on(seamBeforeCall, func() {}) // the ticket's Call comes later
		k.on(seamBeforeCall, func() {})
		k.on(seamAfterReads, func() {})
		k.on(seamAfterRowRead, func() { k.runtimeAs("gc-1", "tok", func(r *simRuntime) { r.zombie = true }) }) // another object, after the read
		k.runKind()
		if crashes(rec) != 0 {
			t.Fatalf("crashes %d, want none: the kill found another object", crashes(rec))
		}
	})
	t.Run("an object replaced across the read", func(t *testing.T) {
		k, leaf, _ := deadKit(t, true, "gc-1", "tok", "")
		n := 0
		leaf.during = func() {
			if n++; n == 2 { // between the bracketing reads
				k.runtimeAs("gc-1", "tok", func(r *simRuntime) { r.zombie = true })
			}
		}
		if s := k.runKind(); s.Cause != causeLivenessUnknown || len(k.sp.kills) != 0 {
			t.Fatalf("settlement %+v kills %d, want refused liveness-unknown with nothing killed", s, len(k.sp.kills))
		}
	})
	t.Run("a rate-limit screen of no crash candidate", func(t *testing.T) {
		k, _, rec := deadKit(t, true, "gc-1", "tok", "Usage limit reached")
		if s := k.runKind(); k.meta("sleep_reason") == string(session.SleepReasonRateLimit) || k.killsBy("KillZombieObject") != 1 || crashes(rec) != 0 || s.Outcome != settledLanded {
			t.Fatalf("settlement %+v reason %q kills %d crashes %d, want recycled with no crash recorded", s, k.meta("sleep_reason"), k.killsBy("KillZombieObject"), crashes(rec))
		}
	})
	t.Run("a terminal error beside a rate-limit screen parks", func(t *testing.T) { // R05: terminal wins, as legacy
		k, _, _ := deadKit(t, true, "gc-1", "tok", "Usage limit reached\nError: model_not_found", "last_woke_at", gatherNow.Add(-5*time.Second).Format(time.RFC3339))
		if k.runKind(); k.meta("sleep_reason") != string(session.SleepReasonProviderTerminalError) {
			t.Fatalf("reason %q, want parked", k.meta("sleep_reason"))
		}
	})
	t.Run("a kill the object refuses blocks with operator-pane", func(t *testing.T) {
		k, _, _ := deadKit(t, false, "gc-1", "tok", "")
		k.on(seamBeforeCall, func() { k.mutate(func(r *simRuntime) { r.corpse = false }) }) // the pane lives again: the kill refuses Live
		if s := k.runKind(); s.Cause != causeOperatorPane || len(k.sp.kills) != 0 || len(k.starts()) != 0 {
			t.Fatalf("settlement %+v kills %d starts %d, want refused operator-pane", s, len(k.sp.kills), len(k.starts()))
		}
	})
}

// Kills a crash loop that never quarantines (the C5a3 review's must-fix): a
// committed seat's own dead runtime is removed and its death accounted as
// A6 accounts a gone one. A rapid crash accrues a wake failure, quarantining
// at the threshold, and resets the conversation; the effect ends with that
// record and the row restarts the next pass. A productive death heals and
// restarts in the same effect.
func TestStartRecycleAccountsTheDeath(t *testing.T) {
	recent := gatherNow.Add(-10 * time.Second).Format(time.RFC3339)
	for _, zombie := range []bool{false, true} {
		k, _, _ := deadKit(t, zombie, "gc-1", "tok", "", "state", "awake", "last_woke_at", recent, "wake_attempts", "4", "started_config_hash", "h-1")
		s := k.runKind()
		if len(k.sp.kills) != 1 || len(k.starts()) != 0 || k.meta("wake_attempts") != "5" || k.meta("quarantined_until") == "" || k.meta("sleep_reason") != string(session.SleepReasonQuarantine) || k.meta("started_config_hash") != "" {
			t.Fatalf("zombie %v: settlement %+v kills %d starts %d attempts %q quarantine %q reason %q hash %q, want the death accrued, quarantined and reset, no restart",
				zombie, s, len(k.sp.kills), len(k.starts()), k.meta("wake_attempts"), k.meta("quarantined_until"), k.meta("sleep_reason"), k.meta("started_config_hash"))
		}
	}
	k, _, _ := deadKit(t, false, "gc-1", "tok", "", "state", "awake", "last_woke_at", recent, "wake_attempts", "1")
	if s := k.runKind(); s.Outcome != settledLanded || len(k.starts()) != 0 || k.meta("wake_attempts") != "2" || k.meta("quarantined_until") != "" {
		t.Fatalf("an accrual under the threshold: settlement %+v starts %d attempts %q, want recorded and no restart this pass", s, len(k.starts()), k.meta("wake_attempts"))
	}
	k, _, _ = deadKit(t, false, "gc-1", "tok", "", "state", "awake", "last_woke_at", gatherNow.Add(-time.Hour).Format(time.RFC3339), "churn_count", "2")
	if s := k.runKind(); s.Outcome != settledLanded || len(k.starts()) != 1 || k.meta("churn_count") != "0" || k.meta("state") != "awake" {
		t.Fatalf("a productive death: settlement %+v starts %d churn %q state %q, want healed and restarted", s, len(k.starts()), k.meta("churn_count"), k.meta("state"))
	}
}

// Kills a parked row re-proposed every pass (its recycle peeks it again and
// settles a no-op, resetting its backoff): A18 holds a row an operator must
// clear of a terminal provider error.
func TestStartHoldsAParkedRow(t *testing.T) {
	k, _ := launchKit(t, "asleep", "sleep_reason", string(session.SleepReasonProviderTerminalError), "provider_terminal_error", "model_not_found",
		"health_state", "unhealthy", "health_reason", "model_not_found", "drainable", "true")
	row := k.p.World.Census.Rows[k.it.Key]
	r := &rowFacts{w: k.p.World, k: k.it.Key, row: row, found: true, entry: &selectionEntry{Key: k.it.Key, Liveness: livenessGone, Desired: desireWake, Template: "worker"}}
	if it, ok := r.startIntent(decideStart); !ok || it.Kind != "" || it.Reason != decideStartParked {
		t.Fatalf("startIntent = (%+v, %v), want held %s", it, ok, decideStartParked)
	}
}

// Kills a park with no operator exit, and one keyed on the terminal mark,
// which nothing clears (the C5a3 delta review's must-fix): an explicit wake
// starts a parked row, and a row carrying a stale mark but no park starts.
func TestStartParkExits(t *testing.T) {
	for name, meta := range map[string][]string{
		"an explicit wake": {"sleep_reason", string(session.SleepReasonProviderTerminalError), "provider_terminal_error", "model_not_found", "wake_request", string(session.WakeCauseExplicit)},
		"a stale mark":     {"provider_terminal_error", "quota_exceeded", "health_state", "unhealthy", "health_reason", "quota_exceeded", "drainable", "true"},
	} {
		k, _ := launchKit(t, "asleep", meta...)
		row := k.p.World.Census.Rows[k.it.Key]
		r := &rowFacts{w: k.p.World, k: k.it.Key, row: row, found: true, entry: &selectionEntry{Key: k.it.Key, Liveness: livenessGone, Desired: desireWake, Template: "worker"}}
		if it, ok := r.startIntent(decideStart); ok && it.Kind == "" {
			t.Fatalf("%s: startIntent = (%+v, %v), want a start", name, it, ok)
		}
	}
}

// Kills a woken parked row whose pane still shows the error it was parked
// on, re-parked every pass, and a stale mark that suppresses a new park: the
// woken row's dead pane is recycled and the row launches; a row with only a
// stale mark whose agent dies on a terminal error is parked.
func TestStartRecycleAfterThePark(t *testing.T) {
	park := string(session.SleepReasonProviderTerminalError)
	k, _, _ := deadKit(t, true, "gc-1", "tok", "Error: model_not_found", "sleep_reason", park, "provider_terminal_error", "model_not_found", "wake_request", string(session.WakeCauseExplicit))
	if s := k.runKind(); s.Outcome != settledLanded || k.killsBy("KillZombieObject") != 1 || len(k.starts()) != 1 || k.meta("sleep_reason") != "" || k.meta("wake_request") != "" {
		t.Fatalf("woken: settlement %+v kills %d starts %d reason %q wake %q, want the pane recycled, the row launched and unparked",
			s, k.killsBy("KillZombieObject"), len(k.starts()), k.meta("sleep_reason"), k.meta("wake_request"))
	}
	k, _, _ = deadKit(t, true, "gc-1", "tok", "Error: model_not_found", "provider_terminal_error", "quota_exceeded")
	if s := k.runKind(); k.meta("sleep_reason") != park || len(k.sp.kills) != 0 || len(k.starts()) != 0 {
		t.Fatalf("stale mark: settlement %+v reason %q kills %d starts %d, want parked", s, k.meta("sleep_reason"), len(k.sp.kills), len(k.starts()))
	}
}

// Kills another row's death accounted to this one: a committed seat whose
// name holds another row's corpse has it removed, accrues nothing, and
// launches.
func TestStartRecycleAccountsOnlyItsOwnDeath(t *testing.T) {
	k, _, _ := deadKit(t, false, "gc-9", "theirs", "", "state", "awake", "last_woke_at", gatherNow.Add(-10*time.Second).Format(time.RFC3339), "wake_attempts", "4")
	if s := k.runKind(); k.meta("wake_attempts") != "4" || k.meta("quarantined_until") != "" || len(k.starts()) != 1 {
		t.Fatalf("settlement %+v attempts %q quarantine %q starts %d, want nothing accrued and a launch", s, k.meta("wake_attempts"), k.meta("quarantined_until"), len(k.starts()))
	}
}

// Kills a death accounted for a row that never committed (D11 of the C5a3
// delta review): an asleep row woken over a leftover corpse of its own
// recycles it and launches with its conversation and wake record intact.
func TestStartRecycleAccountsOnlyACommittedDeath(t *testing.T) {
	k, _, _ := deadKit(t, false, "gc-1", "tok", "", "last_woke_at", gatherNow.Add(-10*time.Second).Format(time.RFC3339), "wake_attempts", "1", "churn_count", "1")
	if s := k.runKind(); s.Outcome != settledLanded || len(k.starts()) != 1 || k.meta("churn_count") != "1" {
		t.Fatalf("settlement %+v starts %d churn %q, want a launch with nothing accounted", s, len(k.starts()), k.meta("churn_count"))
	}
}

// Kills a quarantine of a rate-limit screen on a row being drained (D09 of
// the C5a3 delta review): legacy's exit decision quarantines no row under a
// drain, which A18 never starts but A12 classifies (C5c2).
func TestDeadScreenUnderADrain(t *testing.T) {
	k, _, _ := deadKit(t, true, "gc-1", "tok", "", "last_woke_at", gatherNow.Add(-5*time.Second).Format(time.RFC3339))
	v := txView{It: k.it, Row: k.p.World.Census.Rows[k.it.Key].Info, World: k.p.World, Now: gatherNow}
	if _, ok := deadScreen(v, "Usage limit reached"); !ok {
		t.Fatal("control: want the crash candidate quarantined")
	}
	k, _, _ = deadKit(t, true, "gc-1", "tok", "", "state", "awake", "last_woke_at", gatherNow.Add(-5*time.Second).Format(time.RFC3339),
		session.DrainIntentReasonKey, "idle", session.DrainIntentAtKey, gatherNow.Format(time.RFC3339), session.DrainIntentIncarnationKey, "3")
	v = txView{It: k.it, Row: k.p.World.Census.Rows[k.it.Key].Info, World: k.p.World, Now: gatherNow}
	if patch, ok := deadScreen(v, "Usage limit reached"); ok {
		t.Fatalf("deadScreen = %v, want no quarantine under a drain", patch)
	}
	// E05: a drain requested since the pass, on the fresh row alone.
	k, _, _ = deadKit(t, true, "gc-1", "tok", "", "state", "awake", "last_woke_at", gatherNow.Add(-5*time.Second).Format(time.RFC3339))
	b, err := k.backing.Get(k.it.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	meta := maps.Clone(b.Metadata)
	meta[session.DrainIntentReasonKey], meta[session.DrainIntentAtKey], meta[session.DrainIntentIncarnationKey] = "idle", gatherNow.Format(time.RFC3339), "3"
	v = txView{It: k.it, Row: k.p.World.Census.Rows[k.it.Key].Info, Meta: meta, World: k.p.World, Now: gatherNow}
	if patch, ok := deadScreen(v, "Usage limit reached"); ok {
		t.Fatalf("deadScreen = %v, want no quarantine under a drain the fresh row alone carries", patch)
	}
}

// Kills a death accounted without the fresh row's stop keys (newCensusRow
// carries none; the delta review's A20): a Wake row with a requested drain
// whose agent dies counts no churn and no wake failure, as legacy's drain
// reads its own death.
func TestStartRecycleDrainedDeathCountsNothing(t *testing.T) {
	drain := []string{session.DrainIntentReasonKey, "idle", session.DrainIntentAtKey, gatherNow.Format(time.RFC3339), session.DrainIntentIncarnationKey, "3"}
	for name, woke := range map[string]time.Duration{"productive": -time.Hour, "rapid": -10 * time.Second} {
		k, _, _ := deadKit(t, false, "gc-1", "tok", "", append([]string{"state", "awake", "last_woke_at", gatherNow.Add(woke).Format(time.RFC3339), "churn_count", "1", "wake_attempts", "1"}, drain...)...)
		k.runKind()
		if k.meta("churn_count") != "1" || k.meta("wake_attempts") != "1" {
			t.Fatalf("%s: churn %q attempts %q, want nothing counted for a drained death", name, k.meta("churn_count"), k.meta("wake_attempts"))
		}
	}
}
