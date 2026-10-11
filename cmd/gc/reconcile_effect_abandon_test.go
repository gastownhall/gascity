package main

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
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The start's abandon table and A10's rollback (CONTRACT v5 S3, C3; plan
// C5a2): the abandon's classes as pure steps, pinned against legacy's
// writers, then end to end on the launch kit (gc-1, generation 3, token
// tok, its runtime s-gc-1).

// abandonView is the abandon section's view of row after a failure Call
// that read f.
func abandonView(k *txKit, row beads.Bead, rt txRuntime, f startFailureRead) txView {
	return txView{It: k.it, World: k.p.World, Row: sessionInfoFromBead(row), Meta: row.Metadata, RT: &rt, Now: gatherNow, prev: f}
}

// legacyWakeFailure is the metadata legacy's recordWakeFailure leaves on row.
func legacyWakeFailure(t *testing.T, row beads.Bead) map[string]string {
	t.Helper()
	row.Metadata = maps.Clone(row.Metadata)
	m := beads.NewMemStoreFrom(0, []beads.Bead{row}, nil)
	recordWakeFailure(sessionInfoFromBead(row), sessionFrontDoor(m), &clock.Fake{Time: gatherNow}, "worker")
	b, err := m.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata
}

// hasAny reports whether patch writes any of keys.
func hasAny(patch session.MetadataPatch, keys ...string) bool {
	for _, k := range keys {
		if _, ok := patch[k]; ok {
			return true
		}
	}
	return false
}

// Kills a class that writes the wrong patch, an abandon that does not fail,
// a wake failure that drifts from legacy's recordWakeFailure, a restore over
// an operator's newer write, a relaunch that loses its marker (#7570), an
// accrual on a row an operator stopped mid-start, and a pending create that
// is kept, or rolled back, against START-033's rule.
func TestAbandonStepClasses(t *testing.T) {
	k := startKit(t, intentStart, "creating")
	boom := errors.New("boom")
	creating := poolRow("gc-1", "worker", 1, "creating", "generation", "4", "instance_token", "tok-4", "last_woke_at", gatherNow.Format(time.RFC3339),
		"session_key", "k-1", "started_config_hash", "h-1", "wake_attempts", "2", "sleep_reason", "", "wake_request", "")
	undo := preWakeUndo{
		token: "tok-4", written: session.MetadataPatch{"state": "creating", "last_woke_at": gatherNow.Format(time.RFC3339), "wake_request": ""},
		restore: session.MetadataPatch{"state": "asleep", "last_woke_at": "", "wake_request": "explicit"},
	}
	other := startFailureRead{startFailure: startFailure{cause: causeStartError, err: boom, undo: undo, display: "worker"}}

	wake := legacyWakeFailure(t, creating)
	step := abandonStep(abandonView(k, creating, txRuntime{Class: rtAbsent}, other))
	if step.Fail != causeStartError || step.Write["state"] != "asleep" || step.Write["wake_request"] != "explicit" || step.Write["last_woke_at"] != "" {
		t.Fatalf("any other failure: %+v, want the PreWake restored and the start's failure", step)
	}
	for key, v := range wake {
		if v != creating.Metadata[key] && step.Write[key] != v {
			t.Errorf("any other failure: %s = %q, legacy's recordWakeFailure writes %q", key, step.Write[key], v)
		}
	}

	newer := poolRow("gc-1", "worker", 1, "creating", "generation", "4", "instance_token", "tok-4", "wake_request", "operator")
	if step := abandonStep(abandonView(k, newer, txRuntime{Class: rtAbsent}, other)); step.Write["wake_request"] != "" {
		t.Fatalf("the restore wrote wake_request %q over an operator's newer value", step.Write["wake_request"])
	}
	relaunch := other
	relaunch.relaunched = true
	if step := abandonStep(abandonView(k, creating, txRuntime{Class: rtAbsent}, relaunch)); step.Write["state"] != "" || step.Write["last_woke_at"] != "" || step.Write["wake_attempts"] != "3" {
		t.Fatalf("a relaunch: %+v, want its marker kept and the wake failure accrued", step.Write)
	}
	killed := poolRow("gc-1", "worker", 1, "asleep", "generation", "4", "instance_token", "tok-4", "sleep_reason", "killed")
	if step := abandonStep(abandonView(k, killed, txRuntime{Class: rtAbsent}, other)); hasAny(step.Write, "wake_attempts", "session_key") {
		t.Fatalf("a row killed mid-start: %+v, want no accrual", step.Write)
	}
	kept := other
	kept.keeps = true
	if step := abandonStep(abandonView(k, creating, txRuntime{Class: rtAbsent}, kept)); hasAny(step.Write, "session_key", "started_config_hash") || step.Write["wake_attempts"] != "3" {
		t.Fatalf("a transcript on disk: %+v, want the conversation kept and the attempt accrued", step.Write)
	}

	pending := func(f startFailureRead) startFailureRead { f.pending = true; return f }
	terminal := other
	terminal.terminal = "model_not_found"
	limited := other
	limited.rateLimit = true
	prepare := other
	prepare.cause = causePrepare
	named := pending(other)
	named.configured = true
	namedDied := named
	namedDied.died, namedDied.cause = true, causeDiedDuringStartup
	timedOut := pending(other)
	timedOut.err = fmt.Errorf("startup: %w", context.DeadlineExceeded)
	unattributed := pending(other)
	unattributed.cause = causeAttribution
	for _, c := range []struct {
		name     string
		f        startFailureRead
		rt       txRuntime
		want     session.MetadataPatch // the write, keys compared
		refuse   string
		teardown bool
		stop     bool
		event    string
	}{
		{"terminal", terminal, txRuntime{Class: rtAbsent}, session.MetadataPatch(providerTerminalErrorPatch("model_not_found", gatherNow)), "", false, false, ""},
		{"rate-limit", limited, txRuntime{Class: rtAbsent}, session.RateLimitQuarantinePatch(gatherNow.Add(defaultRateLimitQuarantineDuration)), "", false, false, ""},
		{"prepare", prepare, txRuntime{Class: rtAbsent}, session.MetadataPatch{"last_woke_at": ""}, "", false, false, ""},
		{"configured named pending, not died", named, txRuntime{Class: rtAbsent}, session.MetadataPatch{"last_woke_at": ""}, "", false, false, ""},
		{"configured named pending, died", namedDied, txRuntime{Class: rtAbsent}, nil, "", true, false, ""},
		{"pending, timed out", timedOut, txRuntime{Class: rtAbsent}, nil, "", true, false, events.SessionColdStartTimeout},
		{"pending, own zombie", pending(other), txRuntime{Class: rtZombie, Same: true, Identity: runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "4", Token: "tok-4"}}, nil, "", true, true, ""},
		{"pending, its bead-scoped name stopped whoever runs it", pending(other), txRuntime{Class: rtAlive, Same: true, Identity: runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "9", Token: "tok-9"}}, nil, "", true, true, ""},
		{"pending, unknown", pending(other), txRuntime{Class: rtUnknown}, nil, causeLivenessUnknown, false, false, ""},
		{"pending, terminal", pending(terminal), txRuntime{Class: rtAbsent}, nil, "", true, false, ""},
		{"unattributed", unattributed, txRuntime{Class: rtAbsent}, nil, causeAttribution, false, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			step := abandonStep(abandonView(k, creating, c.rt, c.f))
			td, isTeardown := step.Pass.(teardown)
			switch {
			case step.Refuse != c.refuse:
				t.Fatalf("step %+v, want refusal %q", step, c.refuse)
			case isTeardown != c.teardown || (td.stop != nil) != c.stop:
				t.Fatalf("step %+v, want teardown %v with a stop %v", step, c.teardown, c.stop)
			case c.want != nil && (step.Fail != c.f.cause || !maps.Equal(step.Write, c.want)):
				t.Fatalf("write %v fail %q, want %v failing %q", step.Write, step.Fail, c.want, c.f.cause)
			case c.want == nil && len(step.Write) > 0:
				t.Fatalf("write %v, want none", step.Write)
			case (len(step.Facts.Events) == 1 && step.Facts.Events[0].Type == c.event) != (c.event != ""):
				t.Fatalf("events %+v, want %q", step.Facts.Events, c.event)
			}
			if c.f.terminal != "" && isTeardown && td.more[sessionProviderTerminalErrorMetadataKey] != c.f.terminal {
				t.Fatalf("teardown %+v, want the terminal mark folded under the close (ga-z8yi2j)", td)
			}
		})
	}
}

// Kills a rollback that stops a runtime that is not the row's own, keeps a
// bead-scoped name's runtime, closes over a runtime it cannot attribute, or
// counts a corpse as running (C3, O2, C8.2(a), C8.8).
func TestRollbackRuntime(t *testing.T) {
	row := sessionInfoFromBead(poolRow("gc-1", "worker", 1, "creating", "generation", "4", "instance_token", "tok-4", "pending_create_claim", "true"))
	named := sessionInfoFromBead(chatRow("gc-1", "4", "session_name", "chat", "instance_token", "tok-4", "pending_create_claim", "true"))
	id := func(sid, epoch, token string) runtimeIdentity {
		return runtimeIdentity{Known: true, SessionID: sid, Epoch: epoch, Token: token}
	}
	for _, c := range []struct {
		name  string
		row   session.Info
		rt    txRuntime
		stop  bool
		cause string
	}{
		{"absent", row, txRuntime{Class: rtAbsent}, false, ""},
		{"a corpse counts stopped", named, txRuntime{Class: rtCorpse, Same: true, Identity: id("gc-1", "4", "tok-4")}, false, ""},
		{"own", named, txRuntime{Class: rtAlive, Same: true, Identity: id("gc-1", "4", "tok-4")}, true, ""},
		{"own stale epoch is own", named, txRuntime{Class: rtZombie, Same: true, Identity: id("gc-1", "4", "tok-old")}, true, ""},
		{"an older incarnation is foreign", named, txRuntime{Class: rtAlive, Same: true, Identity: id("gc-1", "3", "tok-old")}, false, ""},
		{"foreign", named, txRuntime{Class: rtAlive, Same: true, Identity: id("gc-9", "1", "theirs")}, false, ""},
		{"a bead-scoped name is stopped whoever runs it", row, txRuntime{Class: rtAlive, Same: true, Identity: id("gc-9", "1", "theirs")}, true, ""},
		{"newer self", named, txRuntime{Class: rtAlive, Same: true, Identity: id("gc-1", "9", "tok-9")}, false, causeNewerSelf},
		{"unattributable", named, txRuntime{Class: rtAlive, Same: true, Identity: runtimeIdentity{}}, false, causeAttribution},
		{"unknown", named, txRuntime{Class: rtUnknown}, false, causeLivenessUnknown},
		{"unsupported", named, txRuntime{Class: rtUnsupported}, false, causeLivenessUnsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			if stop, cause := rollbackRuntime(c.row, &c.rt); stop != c.stop || cause != c.cause {
				t.Fatalf("rollbackRuntime = (%v, %q), want (%v, %q)", stop, cause, c.stop, c.cause)
			}
		})
	}
}

// Kills a rollback close that loses legacy's clears, closes a row that no
// longer may roll back, or closes over its own runtime still up.
func TestRollbackClose(t *testing.T) {
	k := startKit(t, intentRollback, "creating")
	explicit := poolRow("gc-1", "worker", 1, "creating", "generation", "4", "instance_token", "tok-4", "pending_create_claim", "true", "session_name_explicit", "true", "last_woke_at", gatherNow.Format(time.RFC3339))
	v := txView{World: k.p.World, Row: sessionInfoFromBead(explicit), RT: &txRuntime{Class: rtAbsent}, Now: gatherNow, prev: teardown{fail: causeStartError}}
	step := rollbackClose(v)
	want := rollbackPatch(v.Row, gatherNow, nil)
	if step.Fail != causeStartError || !maps.Equal(step.Terminal, want) || want["session_name"] != "" || want["last_woke_at"] != "" || want["state"] != string(session.StateFailedCreate) || want["pending_create_claim"] != "" {
		t.Fatalf("close %+v, want legacy's rollback clears (%v) failing start-error", step, want)
	}
	if _, kept := want["session_name"]; !kept {
		t.Fatal("an explicit session name is not cleared")
	}
	active := explicit
	active.Metadata = maps.Clone(explicit.Metadata)
	active.Metadata["state"] = "active"
	if step := rollbackClose(txView{World: k.p.World, Row: sessionInfoFromBead(active), RT: &txRuntime{Class: rtAbsent}, Now: gatherNow}); step.Refuse != causeRedecided {
		t.Fatalf("an active row: %+v, want refused", step)
	}
	own := txRuntime{Class: rtAlive, Same: true, Identity: runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "4", Token: "tok-4"}}
	if step := rollbackClose(txView{World: k.p.World, Row: v.Row, RT: &own, Now: gatherNow}); step.Refuse != causeRuntimePresent {
		t.Fatalf("its own runtime still up: %+v, want refused runtime-present", step)
	}
}

// failingLaunch is the launch kit for a row in state with meta, whose
// Start returns err.
func failingLaunch(t *testing.T, err error, state string, meta ...string) *txKit {
	t.Helper()
	k, _ := launchKit(t, state, meta...)
	k.sp.nextStart = err
	return k
}

// Kills an abandon that is not written, a start that settles as anything
// but its failure, and a PreWake left on the row: a failed launch of an
// asleep seat restores what the PreWake wrote, its incarnation bumped, and
// accrues one wake failure.
func TestFailedLaunchRestoresAndAccrues(t *testing.T) {
	k := failingLaunch(t, errors.New("boom"), "asleep", "sleep_reason", "idle", "wake_attempts", "1")
	s := k.runKind()
	if s.Outcome != settledFailed || s.Cause != causeStartError {
		t.Fatalf("settlement %+v, want failed start-error", s)
	}
	if k.meta("state") != "asleep" || k.meta("sleep_reason") != "idle" || k.meta("last_woke_at") != "" || k.meta("wake_attempts") != "2" || k.meta("generation") != "4" || k.meta("instance_token") == "tok" {
		t.Fatalf("row state %q reason %q woke %q attempts %q generation %q token %q; want restored, accrued, the incarnation bumped",
			k.meta("state"), k.meta("sleep_reason"), k.meta("last_woke_at"), k.meta("wake_attempts"), k.meta("generation"), k.meta("instance_token"))
	}

	relaunch := failingLaunch(t, errors.New("boom"), "creating")
	if s := relaunch.runKind(); s.Cause != causeStartError || relaunch.meta("state") != "creating" || relaunch.meta("last_woke_at") == "" {
		t.Fatalf("a relaunch: settlement %+v, state %q, woke %q; want its marker kept (#7570)", s, relaunch.meta("state"), relaunch.meta("last_woke_at"))
	}

	killed := failingLaunch(t, errors.New("boom"), "asleep")
	killed.on(seamAfterCall, func() {})
	killed.on(seamAfterCall, func() { killed.outside("state", "asleep", "sleep_reason", "killed") })
	if s := killed.runKind(); s.Outcome != settledRefused || killed.meta("wake_attempts") != "" || killed.meta("sleep_reason") != "killed" {
		t.Fatalf("killed mid-start: settlement %+v, attempts %q; want nothing written", s, killed.meta("wake_attempts"))
	}

	deferred := failingLaunch(t, runtime.ErrRuntimeUnavailable, "asleep")
	if s := deferred.runKind(); s.Cause != causeStartDeferred || deferred.meta("state") != "creating" {
		t.Fatalf("deferred: settlement %+v, state %q; want nothing written", s, deferred.meta("state"))
	}
}

// Kills a paced capacity refusal that does not restore the PreWake, or that
// accrues (S3: no accrual; the breaker trips in memory).
func TestCapacityRefusalRestoresThePreWake(t *testing.T) {
	now := gatherNow
	guard := newEndpointCapacityGuard(func() time.Time { return now })
	k := failingLaunch(t, fmt.Errorf("quota: %w", runtime.ErrProviderCapacity), "asleep", "sleep_reason", "idle")
	row := k.p.World.Census.Rows[k.it.Key]
	tp := TemplateParams{TemplateName: "worker", ResolvedProvider: &config.ResolvedProvider{Name: "test-agent"}}
	k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
	k.p.held.start.capacity, k.it.Endpoint = guard, endpointKey("provider:test-agent")
	if s := k.runKind(); s.Cause != causeCapacity || k.meta("state") != "asleep" || k.meta("sleep_reason") != "idle" || k.meta("last_woke_at") != "" || k.meta("wake_attempts") != "" {
		t.Fatalf("settlement %+v, state %q reason %q attempts %q; want capacity, restored, no accrual", s, k.meta("state"), k.meta("sleep_reason"), k.meta("wake_attempts"))
	}
}

// Kills a pending create whose failure keeps its row (START-033), skips
// #46 (START-031), keeps its own runtime up, or closes over it: the failed
// start tears down its own zombie, rolls the row back (failed-create), and
// accrues the episode; a configured named one that did not die keeps its
// row with last_woke_at cleared.
func TestPendingCreateFailureRollsBack(t *testing.T) {
	pending := []string{"pending_create_claim", "true", "pending_create_started_at", gatherNow.Add(-time.Minute).Format(time.RFC3339)}
	k := failingLaunch(t, nil, "creating", pending...)
	k.on(seamAfterCall, func() {})
	k.on(seamAfterCall, func() { k.mutate(func(r *simRuntime) { r.zombie = true }) })
	s := k.runKind()
	b, _ := k.backing.Get("gc-1")
	if s.Outcome != settledFailed || s.Cause != causeDiedDuringStartup || !s.Closed || b.Status != "closed" || b.Metadata["state"] != string(session.StateFailedCreate) || b.Metadata["pending_create_claim"] != "" {
		t.Fatalf("settlement %+v, row %s %v; want failed died-during-startup, rolled back", s, b.Status, b.Metadata)
	}
	if n := len(k.sp.kills); n != 1 || k.sp.kills[0].Hit == nil {
		t.Fatalf("kills %+v, want its own zombie stopped once", k.sp.kills)
	}
	ep, err := sessionFrontDoor(k.backing).LoadStartupHealthEpisode(startupHealthEpisodeKey(sessionInfoFromBead(b), "s-gc-1"))
	if err != nil || ep.ConsecutiveCount != 1 {
		t.Fatalf("episode %+v (%v), want one accrual", ep, err)
	}

	named, _ := launchKit(t, "creating", append(pending, "configured_named_session", "true", "configured_named_identity", "worker", "configured_named_mode", "on_demand")...)
	named.p.World.Env.Cfg.NamedSessions = []config.NamedSession{{Template: "worker", Mode: "on_demand"}}
	named.sp.nextStart = errors.New("boom")
	if s := named.runKind(); s.Closed || s.Cause != causeStartError || named.meta("last_woke_at") != "" || named.meta("pending_create_claim") != "true" {
		t.Fatalf("a configured named pending create: settlement %+v, woke %q claim %q; want kept, last_woke_at cleared", s, named.meta("last_woke_at"), named.meta("pending_create_claim"))
	}
}

// Kills the rollback effect closing over its own runtime, stopping another
// row's, missing the screen that defers it (SESS-051/092), or skipping the
// retired-session cleanup (START-033).
func TestRollbackEffect(t *testing.T) {
	pending := []string{"pending_create_claim", "true", "pending_create_started_at", gatherNow.Add(-11 * time.Minute).Format(time.RFC3339)}
	rollback := func(t *testing.T, meta ...string) *txKit {
		k := startKit(t, intentRollback, "creating", append(pending, meta...)...)
		k.it.Deadline = gatherNow.Add(time.Minute)
		return k
	}
	closed := func(k *txKit) bool { b, _ := k.backing.Get("gc-1"); return b.Status == "closed" }

	k := rollback(t)
	if s := k.runKind(); s.Outcome != settledLanded || !s.Closed || !closed(k) || k.meta("state") != string(session.StateFailedCreate) {
		t.Fatalf("absent: settlement %+v, want rolled back", s)
	}

	k = rollback(t)
	k.runtimeAs("gc-1", "tok", func(r *simRuntime) { r.zombie = true })
	if s := k.runKind(); !s.Closed || len(k.sp.kills) != 1 {
		t.Fatalf("own zombie: settlement %+v kills %+v, want stopped then rolled back", s, k.sp.kills)
	}

	k = rollback(t, "session_name", "chat", "configured_named_session", "true", "configured_named_identity", "chat", "pool_managed", "")
	k.sp.put("chat", "gc-9", "1", "theirs")
	k.p.World.Census = readCensus(t, gatherNow, censusLegs(rowLeg, k.cache))
	if s := k.runKind(); !s.Closed || len(k.sp.kills) != 0 {
		t.Fatalf("foreign: settlement %+v kills %+v, want rolled back without a stop", s, k.sp.kills)
	}

	k = rollback(t, "last_woke_at", gatherNow.Add(-5*time.Second).Format(time.RFC3339))
	k.runtimeAs("gc-1", "tok", func(r *simRuntime) { r.zombie = true })
	k.sp.SetPeekOutput("s-gc-1", "Usage limit reached")
	if s := k.runKind(); s.Closed || k.meta("sleep_reason") != string(session.SleepReasonRateLimit) || k.meta("pending_create_claim") != "" {
		t.Fatalf("a rate-limit screen: settlement %+v reason %q; want quarantined, not rolled back", s, k.meta("sleep_reason"))
	}
}

// Kills a rollback that leaves the closed row's waits open: the around
// cancels them once the close landed, and only then.
func TestRollbackCancelsRetiredWaits(t *testing.T) {
	k := startKit(t, intentRollback, "creating", "pending_create_claim", "true", "pending_create_started_at", gatherNow.Add(-11*time.Minute).Format(time.RFC3339))
	wait, err := sessionFrontDoor(k.backing).CreateWait(session.WaitSpec{SessionID: "gc-1", Kind: "deps", DepIDs: []string{"gw-1"}, DepMode: "all", Now: gatherNow})
	if err != nil {
		t.Fatal(err)
	}
	k.p.held.retire = retiredCleanup{store: k.backing}
	if s := k.runKind(); !s.Closed {
		t.Fatalf("settlement %+v, want closed", s)
	}
	if b, _ := k.backing.Get(wait.ID); !strings.EqualFold(b.Status, "closed") {
		t.Fatalf("wait %s %s, want canceled with its session", b.ID, b.Status)
	}
}

// Kills an A10 rollback proposed for a row that is not a pending create, a
// wanted pending create inside its deadline, or a live one; and one missed
// for the three shapes A10 rolls back.
func TestArmRollback(t *testing.T) {
	for _, c := range []struct {
		name     string
		pending  bool
		desired  desire
		reason   string
		liveness rowLiveness
		want     bool
	}{
		{"past its deadline", true, desireNone, reasonRollbackCandidate, livenessGone, true},
		{"undesired and gone", true, desireDrain, "undesired", livenessGone, true},
		{"undesired and dead", true, desireDrain, "undesired", livenessDead, true},
		{"another row's runtime", true, desireNone, reasonNameOccupied, livenessOccupied, true},
		{"wanted within its deadline", true, desireWake, "", livenessGone, false},
		{"undesired and alive", true, desireDrain, "undesired", livenessAlive, false},
		{"not a pending create", false, desireDrain, "undesired", livenessGone, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			meta := []string{"instance_token", "tok"}
			if c.pending {
				meta = append(meta, "pending_create_claim", "true")
			}
			row := poolRow("gc-1", "worker", 1, "creating", meta...)
			k := rowKeyOf(row.ID)
			r := &rowFacts{row: newCensusRow(k, sessionInfoFromBead(row)), found: true, entry: &selectionEntry{Key: k, Desired: c.desired, Reason: c.reason, Liveness: c.liveness}}
			it, ok := armRollback(r)
			if ok != c.want || ok && (it.Kind != intentRollback || r.rests&restRuntime == 0) {
				t.Fatalf("armRollback = (%+v, %v), want %v resting on the runtime", it, ok, c.want)
			}
		})
	}
}

// Kills a failure routed to the wrong class: a prepare error, an
// unattributed pending create's start error, a death with no error (legacy's
// "died during startup"), and a start error.
func TestCommitStepRoutesFailures(t *testing.T) {
	k := startKit(t, intentStart, "creating", "pending_create_claim", "true")
	row := sessionInfoFromBead(poolRow("gc-1", "worker", 1, "creating", "generation", "3", "instance_token", "tok", "pending_create_claim", "true"))
	failed := func(out startOutcome, rt txRuntime) *startFailure {
		end, _ := commitStep(txView{World: k.p.World, Row: row, RT: &rt, Now: gatherNow, prev: out}).Pass.(startEnd)
		return end.failed
	}
	unread := runtimeIdentity{Known: true, Token: "tok-x", SessionID: "gc-1"}
	for _, c := range []struct {
		name  string
		out   startOutcome
		rt    txRuntime
		cause string
		peek  bool
	}{
		{"prepare", startOutcome{prepErr: errors.New("no dir")}, txRuntime{Class: rtAbsent}, causePrepare, false},
		{"unattributed", startOutcome{err: errors.New("boom")}, txRuntime{Class: rtAlive, Same: true, Identity: unread}, causeAttribution, false},
		{"died with no error", startOutcome{}, txRuntime{Class: rtCorpse}, causeDiedDuringStartup, true},
		{"start error", startOutcome{err: errors.New("boom")}, txRuntime{Class: rtAbsent}, causeStartError, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := failed(c.out, c.rt)
			if f == nil || f.cause != c.cause || f.err == nil || f.peek != c.peek || !f.pending {
				t.Fatalf("failure %+v, want %s (peek %v) on a pending create with an error", f, c.cause, c.peek)
			}
		})
	}
}

// Kills a failure Call that skips #46 for the classes S3 accrues it on, or
// accrues it for a terminal error, and one that misses a rate-limit screen.
func TestReadStartFailureAccrues(t *testing.T) {
	boom := errors.New("boom")
	for _, c := range []struct {
		name    string
		f       startFailure
		screen  string
		accrues bool
		limited bool
	}{
		{"pending", startFailure{cause: causeStartError, err: boom, pending: true}, "", true, false},
		{"prepare", startFailure{cause: causePrepare, err: boom}, "", true, false},
		{"unattributed", startFailure{cause: causeAttribution, err: boom, pending: true}, "", true, false},
		{"unattributed name collision", startFailure{cause: causeAttribution, err: runtime.ErrSessionExists, pending: true}, "", false, false},
		{"terminal", startFailure{cause: causeStartError, err: boom, pending: true, terminal: "model_not_found"}, "", false, false},
		{"rate-limited", startFailure{cause: causeStartError, err: boom, pending: true, peek: true}, "Usage limit reached", false, true},
		{"any other", startFailure{cause: causeStartError, err: boom}, "", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := beads.NewMemStore()
			sp := newSimProvider()
			sp.SetPeekOutput("s-gc-1", c.screen)
			sp.put("s-gc-1", "gc-1", "4", "tok-4")
			c.f.name, c.f.episode = "s-gc-1", "s-gc-1"
			r := readStartFailure(context.Background(), txCaps{episode: startupHealthRecord{store: m}, start: startCaps{sp: sp, clock: newFakePlannerClock(gatherNow)}}, c.f)
			ep, _ := sessionFrontDoor(m).LoadStartupHealthEpisode("s-gc-1")
			if (ep.ConsecutiveCount == 1) != c.accrues || r.rateLimit != c.limited {
				t.Fatalf("episode %+v, rate limit %v; want accrued %v, rate limit %v", ep, r.rateLimit, c.accrues, c.limited)
			}
		})
	}
}
