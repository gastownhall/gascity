package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// These tests pin gc's reading of the input-outcome wire
// (runtime.ProtocolCapabilityInputOutcome) to the sentinels gc already acts
// on: every word projects one sentinel, and the two consumers that switch on
// them (the conversation lane, laneDeliveryResult, and the nudge queue,
// queuedNudgeReleaseAfter) answer each word as the table says; the callers
// that nudge a session directly count a delivered-but-unobserved nudge from
// any runtime as delivered.

// inputOutcomeSentinelNames names each word's sentinel as the switches spell
// it, so the AST pass can tie a name in a switch to a word.
var inputOutcomeSentinelNames = map[runtime.InputOutcome]string{
	runtime.InputOutcomeDeliveredUnobserved: "ErrNudgeDeliveredUnobserved",
	runtime.InputOutcomeUnconfirmed:         "ErrNudgeDeliveryUnconfirmed",
	runtime.InputOutcomeNotSent:             "ErrNudgeNotSent",
	runtime.InputOutcomeRejected:            "ErrNudgeRejected",
	runtime.InputOutcomeBusy:                "ErrSessionBusy",
	runtime.InputOutcomePendingInteraction:  "ErrNudgeRefusedPendingInteraction",
	runtime.InputOutcomeUnavailable:         "ErrRuntimeUnavailable",
	runtime.InputOutcomeGone:                "ErrSessionNotFound",
}

// sentinelByName is the runtime value of each name above.
var sentinelByName = map[string]error{
	"ErrNudgeDeliveredUnobserved":       runtime.ErrNudgeDeliveredUnobserved,
	"ErrNudgeDeliveryUnconfirmed":       runtime.ErrNudgeDeliveryUnconfirmed,
	"ErrNudgeNotSent":                   runtime.ErrNudgeNotSent,
	"ErrNudgeRejected":                  runtime.ErrNudgeRejected,
	"ErrSessionBusy":                    runtime.ErrSessionBusy,
	"ErrNudgeRefusedPendingInteraction": runtime.ErrNudgeRefusedPendingInteraction,
	"ErrRuntimeUnavailable":             runtime.ErrRuntimeUnavailable,
	"ErrSessionNotFound":                runtime.ErrSessionNotFound,
}

// inputOutcomeExemptions are the runtime names the two switches act on that
// no input-outcome word projects, each with the reason it cannot reach this
// wire. A switch naming anything else fails the AST pass.
var inputOutcomeExemptions = map[string]string{
	"FenceMismatchError":    "needs a fenced op; the ops carrier refuses every fenced entry point (ErrInputFenceUnsupported) before any op runs, and an unfenced partial delivery is unconfirmed",
	"ErrPendingInteraction": "the session layer's translation of ErrNudgeRefusedPendingInteraction, which pending_interaction projects",
}

// Lane classes and queue answers the projection table names.
const (
	laneDelivered           = "delivered"
	laneDeliveredUnobserved = "delivered, unobserved"
	laneUnknown             = "unknown"
	laneTransientMiss       = "transient miss"
	laneRejected            = "rejected"

	queueAcked             = "acked"
	queueCharged           = "charged"
	queueReleased          = "released, no attempt"
	queueReleasedAfterWait = "released after the retry delay, no attempt"
)

func laneClass(t *testing.T, r opjournal.DeliveryResult) string {
	t.Helper()
	switch r := r.(type) {
	case opjournal.Delivered:
		if r.Record.Ack == opjournal.AckDeliveredUnobserved {
			return laneDeliveredUnobserved
		}
		return laneDelivered
	case opjournal.Unknown:
		return laneUnknown
	case opjournal.NotDelivered:
		if r.Transient {
			return laneTransientMiss
		}
		return "not delivered"
	case opjournal.Rejected:
		return laneRejected
	}
	t.Fatalf("unexpected lane result %T", r)
	return ""
}

func queueAnswer(err error, now time.Time) string {
	if runtime.NudgeDelivered(err) {
		return queueAcked
	}
	retryAt, released := queuedNudgeReleaseAfter(err, now)
	switch {
	case !released:
		return queueCharged
	case retryAt.IsZero():
		return queueReleased
	default:
		return queueReleasedAfterWait
	}
}

// TestInputOutcomeProjectionMatchesLaneAndQueue pins every input-outcome
// word to its runtime sentinel and to the answers the conversation lane and
// the nudge queue give it, building each error exactly as the exec provider
// returns it. An AST pass over both switches fails if either names a
// runtime sentinel that no word projects and no exemption explains, so
// neither the wire nor the switches can drift.
func TestInputOutcomeProjectionMatchesLaneAndQueue(t *testing.T) {
	table := []struct {
		word  runtime.InputOutcome
		lane  string
		queue string
	}{
		{runtime.InputOutcomeDelivered, laneDelivered, queueAcked},
		{runtime.InputOutcomeDeliveredUnobserved, laneDeliveredUnobserved, queueAcked},
		{runtime.InputOutcomeUnconfirmed, laneUnknown, queueCharged},
		{runtime.InputOutcomeNotSent, laneTransientMiss, queueCharged},
		{runtime.InputOutcomeRejected, laneRejected, queueCharged},
		{runtime.InputOutcomeBusy, laneTransientMiss, queueReleasedAfterWait},
		{runtime.InputOutcomePendingInteraction, laneTransientMiss, queueReleased},
		{runtime.InputOutcomeUnavailable, laneTransientMiss, queueReleasedAfterWait},
		{runtime.InputOutcomeGone, laneUnknown, queueReleased},
	}
	if len(table) != len(runtime.InputOutcomes()) {
		t.Fatalf("table has %d words, the vocabulary %d", len(table), len(runtime.InputOutcomes()))
	}
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	for _, row := range table {
		t.Run(string(row.word), func(t *testing.T) {
			code, line := 0, ""
			switch {
			case row.word == runtime.InputOutcomeDeliveredUnobserved:
				line = `{"outcome":"delivered_unobserved","detail":"turn start not seen"}`
			case !row.word.Success():
				code, line = runtime.ExitCodeInputOutcome, `{"outcome":"`+string(row.word)+`","detail":"the pack's reason"}`
			}
			err := sessionexec.InputOutcomeError("nudge", "worker", code, line)
			if want := row.word.Sentinel(); want == nil && err != nil || want != nil && !errors.Is(err, want) {
				t.Fatalf("exec's error for %q = %v, want its sentinel %v", row.word, err, want)
			}
			if name, ok := inputOutcomeSentinelNames[row.word]; ok && !errors.Is(err, sentinelByName[name]) {
				t.Fatalf("%q's sentinel is not %s", row.word, name)
			}
			res := worker.NudgeResult{Delivered: err == nil}
			if got := laneClass(t, laneDeliveryResult(res, err, now, false, false)); got != row.lane {
				t.Errorf("lane reads %q as %q, want %q", row.word, got, row.lane)
			}
			if got := queueAnswer(err, now); got != row.queue {
				t.Errorf("queue answers %q with %q, want %q", row.word, got, row.queue)
			}
		})
	}

	projected := map[string]bool{}
	for _, name := range inputOutcomeSentinelNames {
		projected[name] = true
	}
	for _, sw := range []struct{ file, fn string }{
		{"conversation_lane_exec.go", "laneDeliveryResult"},
		{"cmd_nudge.go", "queuedNudgeReleaseAfter"},
	} {
		names := runtimeSentinelsNamed(parseFuncDecl(t, sw.file, sw.fn))
		if len(names) == 0 {
			t.Fatalf("%s names no runtime sentinel: did the switch move?", sw.fn)
		}
		for _, name := range names {
			if !projected[name] && inputOutcomeExemptions[name] == "" {
				t.Errorf("%s switches on runtime.%s, which no input-outcome word projects: add a word or an exemption with its reason", sw.fn, name)
			}
		}
	}
}

// runtimeSentinelsNamed lists the runtime.Err* and runtime.FenceMismatchError
// names fn's body mentions, sorted and distinct.
func runtimeSentinelsNamed(fn *ast.FuncDecl) []string {
	seen := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == "runtime" &&
			(strings.HasPrefix(sel.Sel.Name, "Err") || sel.Sel.Name == "FenceMismatchError") {
			seen[sel.Sel.Name] = true
		}
		return true
	})
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// errGenericUnobserved is a delivered-but-unobserved nudge as a runtime other
// than tmux reports it: the generic sentinel, not tmux's.
var errGenericUnobserved = fmt.Errorf("pack nudge: delivered_unobserved: %w", runtime.ErrNudgeDeliveredUnobserved)

// TestQueuedNudgeAcksGenericDeliveredUnobserved pins the nudge queue's three
// delivery sites to runtime.NudgeDelivered: a delivered-but-unobserved nudge
// from any runtime is acked once, never redelivered or re-queued.
func TestQueuedNudgeAcksGenericDeliveredUnobserved(t *testing.T) {
	t.Run("queue poller", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		dir := t.TempDir()
		if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", time.Now().Add(-time.Minute))); err != nil {
			t.Fatalf("enqueueQueuedNudge: %v", err)
		}
		store := openNudgeBeadStore(dir)
		fake := runtime.NewFake()
		mgr := newSessionManagerWithConfig(dir, store, fake, nil)
		info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		idleSince := time.Now().Add(-10 * time.Second)
		fake.Activity = map[string]time.Time{info.SessionName: idleSince}
		fake.NudgeErrors = map[string]error{info.SessionName: errGenericUnobserved}
		target := nudgeTarget{cityPath: dir, agent: config.Agent{Name: "worker"}, sessionID: info.ID, resolved: &config.ResolvedProvider{Name: "codex"}, sessionName: info.SessionName}
		obs := worker.LiveObservation{Running: true, Alive: true, LastActivity: &idleSince}

		delivered, err := tryDeliverQueuedNudgesByPoller(target, store, store, fake, 3*time.Second, obs)
		if err != nil || !delivered {
			t.Fatalf("tryDeliverQueuedNudgesByPoller = (%t, %v), want delivered", delivered, err)
		}
		if n := fake.CountCalls("Nudge", info.SessionName); n != 1 {
			t.Fatalf("nudge calls = %d, want exactly 1", n)
		}
		pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
		if err != nil {
			t.Fatalf("listQueuedNudges: %v", err)
		}
		if len(pending)+len(inFlight)+len(dead) != 0 {
			t.Fatalf("pending=%d inFlight=%d dead=%d, want the item acked", len(pending), len(inFlight), len(dead))
		}
	})

	t.Run("session nudge", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		dir := t.TempDir()
		store := openNudgeBeadStore(dir)
		fake := runtime.NewFake()
		mgr := newSessionManagerWithConfig(dir, store, fake, nil)
		info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		fake.NudgeErrors = map[string]error{info.SessionName: errGenericUnobserved}
		target := nudgeTarget{cityPath: dir, sessionID: info.ID, sessionName: info.SessionName}
		var stdout, stderr bytes.Buffer
		if code := deliverSessionNudgeWithWorker(target, store, fake, nudgeDeliveryImmediate, &stdout, &stderr); code != 0 {
			t.Fatalf("deliverSessionNudgeWithWorker = %d, want 0; stderr: %s", code, stderr.String())
		}
		assertSessionLastNudgeDeliveredAtStamped(t, store, info.ID)
	})

	t.Run("mail notify", func(t *testing.T) {
		clearGCEnv(t)
		disableManagedDoltRecoveryForTest(t)
		clearInheritedCityRoutingEnv(t)
		t.Setenv("GC_BEADS", "file")
		dir := t.TempDir()
		store := openNudgeBeadStore(dir)
		fake := runtime.NewFake()
		mgr := newSessionManagerWithConfig(dir, store, fake, nil)
		info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "mayor", Title: "Mayor", Command: "claude", WorkDir: dir, Provider: "claude", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		fake.WaitForIdleErrors[info.SessionName] = nil
		fake.NudgeErrors = map[string]error{info.SessionName: errGenericUnobserved}
		target := nudgeTarget{cityPath: dir, agent: config.Agent{Name: "mayor"}, sessionID: info.ID, resolved: &config.ResolvedProvider{Name: "claude"}, sessionName: info.SessionName}
		if err := sendMailNotifyWithWorker(target, store, fake, ""); err != nil {
			t.Fatalf("sendMailNotifyWithWorker: %v", err)
		}
		pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
		if err != nil {
			t.Fatalf("listQueuedNudgesForTarget: %v", err)
		}
		if len(pending)+len(inFlight)+len(dead) != 0 {
			t.Fatalf("pending=%d inFlight=%d dead=%d, want no duplicate notification queued", len(pending), len(inFlight), len(dead))
		}
		assertSessionLastNudgeDeliveredAtStamped(t, store, info.ID)
	})

	t.Run("sling nudge", func(t *testing.T) {
		clearGCEnv(t)
		disableManagedDoltRecoveryForTest(t)
		clearInheritedCityRoutingEnv(t)
		t.Setenv("GC_BEADS", "file")
		dir := t.TempDir()
		store := openNudgeBeadStore(dir)
		fake := runtime.NewFake()
		mgr := newSessionManagerWithConfig(dir, store, fake, nil)
		info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		fake.WaitForIdleErrors[info.SessionName] = nil
		fake.NudgeErrors = map[string]error{info.SessionName: errGenericUnobserved}
		target := nudgeTarget{cityPath: dir, agent: config.Agent{Name: "worker"}, sessionID: info.ID, resolved: &config.ResolvedProvider{Name: "claude"}, sessionName: info.SessionName}
		var stdout, stderr bytes.Buffer
		deliverSlingNudge(target, fake, store, store, dir, "", &stdout, &stderr)
		if n := fake.CountCalls("NudgeNow", info.SessionName) + fake.CountCalls("Nudge", info.SessionName); n != 1 {
			t.Fatalf("direct nudge calls = %d, want exactly 1", n)
		}
		pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
		if err != nil {
			t.Fatalf("listQueuedNudgesForTarget: %v", err)
		}
		if len(pending)+len(inFlight)+len(dead) != 0 {
			t.Fatalf("pending=%d inFlight=%d dead=%d, want no duplicate sling nudge queued (stdout %q)", len(pending), len(inFlight), len(dead), stdout.String())
		}
		if !strings.Contains(stdout.String(), "Nudged worker") {
			t.Fatalf("stdout %q, want the delivered-but-unobserved sling nudge reported as delivered", stdout.String())
		}
		assertSessionLastNudgeDeliveredAtStamped(t, store, info.ID)
	})
}

// unobservedProvider delivers every nudge through the fake and answers it
// delivered-but-unobserved, as a pack runtime answering delivered_unobserved
// does.
type unobservedProvider struct{ *runtime.Fake }

func (p unobservedProvider) Nudge(name string, content []runtime.ContentBlock) error {
	if err := p.Fake.Nudge(name, content); err != nil {
		return err
	}
	return errGenericUnobserved
}

// TestWarmBindNudgeAcksDeliveredUnobserved pins the warm-bind claim nudge to
// runtime.NudgeDelivered: a delivered-but-unobserved claim nudge is
// delivered, so the next tick does not nudge the slot again.
func TestWarmBindNudgeAcksDeliveredUnobserved(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "slot-1", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors["slot-1"] = nil
	if !nudgeWarmBindSlot(context.Background(), unobservedProvider{fake}, "slot-1", "work-a", "claim work-a") {
		t.Fatal("nudgeWarmBindSlot = false, want a delivered-but-unobserved nudge counted as delivered")
	}
}

// TestNudgeBackstopCountsDeliveredUnobserved pins the idle-claim backstop to
// runtime.NudgeDelivered: a delivered-but-unobserved nudge is logged as
// delivered, its attempt counted, not reported as a failure.
func TestNudgeBackstopCountsDeliveredUnobserved(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(idleClaimNudgeGrace + time.Second)
	s := staleSnapshotClaimSession(base)
	s.Metadata["instance_token"] = "tok-1"
	s.Metadata[idleClaimNudgeTokenKey] = "tok-1"
	store := beads.NewMemStoreFrom(0, []beads.Bead{s, {ID: "work-a", Status: "open"}}, nil)
	fake := quietClaimFake(t, now)
	drains := &drainRecorder{}
	var out bytes.Buffer
	fake.SetActivity("session-a", now.Add(-time.Hour))
	nudgeStalledPoolClaims(unobservedProvider{fake}, idleClaimTestCfg(), store, []beads.Bead{mustGetTestBead(t, store, s.ID)}, []beads.Bead{{ID: "work-a", Status: "open"}}, nil, now, liveClaimDrain(store, drains.request), &out)
	if fake.CountCalls("Nudge", "session-a") != 1 {
		t.Fatalf("nudge calls = %d, want 1 (log %q)", fake.CountCalls("Nudge", "session-a"), out.String())
	}
	if strings.Contains(out.String(), "failed") || !strings.Contains(out.String(), "nudged session-a") {
		t.Fatalf("log %q, want the delivered-but-unobserved nudge logged as delivered", out.String())
	}
	if got := mustGetTestBead(t, store, s.ID).Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("marker count %q, want attempt 1 counted", got)
	}
}

// TestDrainReminderCountsDeliveredUnobserved pins the drain reminder to
// runtime.NudgeDelivered: a delivered-but-unobserved reminder is delivered,
// not an undeliverable attempt.
func TestDrainReminderCountsDeliveredUnobserved(t *testing.T) {
	e := newDrainReminderEnv(t)
	if got := e.remindWith(unobservedProvider{e.sp}); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered", got)
	}
	if got := e.meta(drainReminderFailedKey); got != "0" {
		t.Errorf("%s = %q, want 0: a delivered-but-unobserved reminder is not undeliverable", drainReminderFailedKey, got)
	}
	if got := e.meta(drainReminderCountKey); got != "1" {
		t.Errorf("%s = %q, want 1", drainReminderCountKey, got)
	}
}
