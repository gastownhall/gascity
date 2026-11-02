package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// restartOpsEnv records lifecycle and content ops.
func restartOpsEnv() map[string]string {
	return map[string]string{"GC_OPS_LIFECYCLE": "auto", "GC_OPS_CONTENT": "auto", "GC_OPS_CONTROL": "off"}
}

// restartOpsCity is a city whose session the controller restarts, run from
// inside that session.
func restartOpsCity(t *testing.T, env map[string]string, extra map[string]string) (*cliOpHarness, *cliOpCity) {
	t.Helper()
	h := newCLIOpHarness(t, env)
	meta := map[string]string{"instance_token": cliOpRestartToken, namedSessionModeMetadata: "always", "continuation_epoch": "3"}
	for k, v := range extra {
		meta[k] = v
	}
	c := newCLIOpCity(t, h, meta)
	inSessionRestart(t, c)
	return h, c
}

// unitRestart is a rowFirstRestart over c's row, recording ops through the
// harness, with a poke that runs onPoke.
func unitRestart(t *testing.T, c *cliOpCity, verb string, stderr io.Writer, onPoke func()) rowFirstRestart {
	t.Helper()
	r := testRowFirstRestart(verb, c.name, newFakeDrainOps(), stderr)
	r.display = killPokeSessionIdentity
	r.token = cliOpRestartToken
	r.record = fencedRecorder(t, c.store, runtime.NewFake(), c.bead.ID)
	r.poke = func(string, reconcilekey.Key) error {
		if onPoke != nil {
			onPoke()
		}
		return nil
	}
	r.ops = newRestartOps(nil, nil, c.store, "", c.name, killPokeSessionIdentity, false, io.Discard, stderr)
	return r
}

// opsByKind splits the harness's op rows into the restart op and the notice.
func opsByKind(t *testing.T, h *cliOpHarness) (restart, notice []opjournal.Op) {
	t.Helper()
	for _, op := range cliOpRows(t, h.opStore) {
		switch op.Kind {
		case opjournal.KindRestart:
			restart = append(restart, op)
		case opjournal.KindMessage:
			notice = append(notice, op)
		default:
			t.Fatalf("unexpected op %+v", op)
		}
	}
	return restart, notice
}

// TestRequestRestartRecordsARestartOp pins R's shape (§3.2) and that it is
// closed before the controller is poked.
func TestRequestRestartRecordsARestartOp(t *testing.T) {
	env := restartOpsEnv()
	env["GC_SESSION_ID"] = "gc-agent-7"
	h, c := restartOpsCity(t, env, nil)
	var stderr bytes.Buffer
	poked := false
	r := unitRestart(t, c, "gc runtime request-restart", &stderr, func() {
		poked = true
		op := cliOpOnly(t, h.opStore)
		if op.Status != opjournal.StatusClosed {
			t.Errorf("restart op %s is %s at the poke, want closed", op.ID, op.Status)
		}
	})
	if code := doRuntimeRequestRestart(r, events.Discard, io.Discard); code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !poked {
		t.Fatal("controller never poked")
	}
	op := cliOpOnly(t, h.opStore)
	if op.Kind != opjournal.KindRestart || op.Policy.Restart.Mode != opjournal.RestartFresh || op.Conversation != c.bead.ID ||
		op.Scope.Kind != opjournal.ScopeConversation || op.Executor != opjournal.ExecutorLegacyCLI || op.After != "" || op.Payload != "" || op.Causation != "" {
		t.Fatalf("op = %+v, want restart{fresh} on %s, conversation scope, legacy:cli, no after", op, c.bead.ID)
	}
	if op.Producer.Principal != "conversation:gc-agent-7" || op.Producer.Actor != opjournal.ActorAgent {
		t.Fatalf("producer = %+v, want the agent's conversation", op.Producer)
	}
	if arm, _ := op.Outcome.Effective(); arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != legacyop.ViaLegacy {
		t.Fatalf("outcome = %+v, want succeeded{via: legacy}", op.Outcome)
	}
	if op.FinishBy.Sub(op.CreatedAt) != legacyop.ExecutionBound {
		t.Fatalf("finish_by - created = %s, want %s", op.FinishBy.Sub(op.CreatedAt), legacyop.ExecutionBound)
	}
}

// TestRestartOpOutcomes pins §3.6's table.
func TestRestartOpOutcomes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		arm    opjournal.Arm
		reason string
		via    string
	}{
		{nil, opjournal.ArmSucceeded, "", legacyop.ViaLegacy},
		{session.ErrStaleRestartRequest, opjournal.ArmSkipped, opjournal.ReasonStale, ""},
		{session.ErrSessionClosed, opjournal.ArmFailed, opjournal.RefusedReasonPrefix + cliOpRefusedSessionClosed, ""},
		{session.ErrRestartRequestContended, opjournal.ArmFailed, opjournal.RefusedReasonPrefix + cliOpRefusedRowContended, ""},
		{session.ErrRestartRequestUnfenced, opjournal.ArmFailed, opjournal.RefusedReasonPrefix + cliOpRefusedNoInstanceToken, ""},
		{errors.New("store down"), opjournal.ArmFailed, opjournal.ReasonOther, ""},
	} {
		name := "nil"
		if tc.err != nil {
			name = tc.err.Error()
		}
		t.Run(name, func(t *testing.T) {
			res := restartOpOutcome(tc.err)
			if res.Outcome.Arm != tc.arm || res.Outcome.Reason != tc.reason || res.Outcome.Result.Via != tc.via || res.LeaveOpen {
				t.Fatalf("outcome = %+v (leave open %t), want %s{%s} via %q, closed", res.Outcome, res.LeaveOpen, tc.arm, tc.reason, tc.via)
			}
		})
	}
}

// TestRequestRestartStaleIncarnationClosesSkippedStale pins that a refused
// fence closes R skipped{stale} and exits 1.
func TestRequestRestartStaleIncarnationClosesSkippedStale(t *testing.T) {
	h, c := restartOpsCity(t, restartOpsEnv(), map[string]string{"instance_token": "tok-successor"})
	var stderr bytes.Buffer
	r := unitRestart(t, c, "gc runtime request-restart", &stderr, nil)
	if code := doRuntimeRequestRestart(r, events.Discard, io.Discard); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	op := cliOpOnly(t, h.opStore)
	if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmSkipped || op.Outcome.Reason != opjournal.ReasonStale {
		t.Fatalf("op = %+v, want closed skipped{stale}", op)
	}
}

// TestHandoffRecordsRestartThenNotice pins R before N, N's shape, and that N
// closes legacy.queued after the mail and before the row records the restart.
func TestHandoffRecordsRestartThenNotice(t *testing.T) {
	h, c := restartOpsCity(t, restartOpsEnv(), nil)
	var stdout, stderr bytes.Buffer
	r := unitRestart(t, c, "gc handoff", &stderr, nil)
	write := r.record
	r.record = func(token string) error {
		_, notice := opsByKind(t, h)
		if len(notice) != 1 || notice[0].Status != opjournal.StatusClosed {
			t.Errorf("notice ops at the row write = %+v, want one closed", notice)
		}
		if mails := listOpenMessagesBothTiers(t, c.store); len(mails) != 1 {
			t.Errorf("mail beads at the row write = %d, want 1", len(mails))
		}
		return write(token)
	}
	outcome := doHandoffWithOutcome(c.store, c.store, events.NewFake(), newFakeDrainOps(), r, killPokeSessionIdentity, c.name, []string{"context cycle"}, &stdout, &stderr)
	if outcome.code != 0 || !outcome.restartRequested {
		t.Fatalf("outcome = %+v; stderr=%s", outcome, stderr.String())
	}
	restart, notice := opsByKind(t, h)
	if len(restart) != 1 || len(notice) != 1 {
		t.Fatalf("ops = %d restart, %d notice; want one of each", len(restart), len(notice))
	}
	R, N := restart[0], notice[0]
	if N.ClientKey != R.ID+handoffNoticeKeySuffix || N.Causation != R.ID {
		t.Fatalf("notice key %q causation %q, want %s:notice and %s", N.ClientKey, N.Causation, R.ID, R.ID)
	}
	if N.Policy.Message.Role != opjournal.RoleNotice || N.Policy.Message.When != opjournal.WhenBoundary || N.Policy.Message.Wake != opjournal.WakeIfNeeded {
		t.Fatalf("notice policy = %+v, want boundary/if_needed/notice", N.Policy.Message)
	}
	if N.Scope != (opjournal.Scope{Kind: opjournal.ScopeContext, Epoch: 4}) || N.After != "" || N.Conversation != c.bead.ID {
		t.Fatalf("notice scope %+v after %q conversation %q, want context(4), no after, %s", N.Scope, N.After, N.Conversation, c.bead.ID)
	}
	if N.Payload != mailNotifyText(killPokeSessionIdentity) || N.Executor != opjournal.ExecutorLegacyCLI {
		t.Fatalf("notice payload %q executor %s", N.Payload, N.Executor)
	}
	if N.Outcome.Arm != opjournal.ArmSucceeded || N.Outcome.Result.Via != legacyop.ViaLegacyQueued {
		t.Fatalf("notice outcome = %+v, want succeeded{via: legacy.queued}", N.Outcome)
	}
	if R.Status != opjournal.StatusClosed || R.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("restart op = %+v, want closed succeeded", R)
	}
	if R.CreatedAt.After(N.CreatedAt) {
		t.Fatalf("restart op created %s after the notice %s", R.CreatedAt, N.CreatedAt)
	}
}

// TestHandoffNoticeNeverDeclaresAfterOnAShadowRestart (DES-O1 §13): while
// the restart op is a shadow record, the notice never waits on it.
func TestHandoffNoticeNeverDeclaresAfterOnAShadowRestart(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		t.Run(mode, func(t *testing.T) {
			h, c := restartOpsCity(t, map[string]string{"GC_OPS_LIFECYCLE": mode, "GC_OPS_CONTENT": mode}, nil)
			for i, key := range []string{"", "k-after"} {
				if r := runHandoffForTest(c, key); r.code != 0 {
					t.Fatalf("run %d = %d; stderr=%s", i, r.code, r.stderr)
				}
			}
			restart, notice := opsByKind(t, h)
			if len(notice) == 0 {
				t.Fatal("no notice recorded")
			}
			for _, op := range append(restart, notice...) {
				if op.After != "" || op.Executor != opjournal.ExecutorLegacyCLI {
					t.Fatalf("op %s after %q executor %s, want no after under a legacy:cli restart", op.ID, op.After, op.Executor)
				}
			}
		})
	}
}

// runHandoffForTest runs the self handoff verb under key.
func runHandoffForTest(c *cliOpCity, key string) cliOpVerbRun {
	var stdout, stderr bytes.Buffer
	code := runHandoffVerb(c, key, false, &stdout, &stderr)
	return cliOpVerbRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// TestHandoffNoticeTextCarriesNoIdentifier (D-NOMARK): the notice text names
// no op id, key or digest.
func TestHandoffNoticeTextCarriesNoIdentifier(t *testing.T) {
	h, c := restartOpsCity(t, restartOpsEnv(), nil)
	if r := runHandoffForTest(c, "k-nomark"); r.code != 0 {
		t.Fatalf("handoff = %d; stderr=%s", r.code, r.stderr)
	}
	restart, notice := opsByKind(t, h)
	if len(restart) != 1 || len(notice) != 1 {
		t.Fatalf("ops = %d/%d, want one of each", len(restart), len(notice))
	}
	for _, mark := range []string{restart[0].ID, notice[0].ID, "k-nomark", notice[0].ClientKey, notice[0].PayloadDigest} {
		if mark != "" && strings.Contains(notice[0].Payload, mark) {
			t.Fatalf("notice text %q carries %q", notice[0].Payload, mark)
		}
	}
}

// handoffMailFailingStore refuses every Create, as an unreachable messaging store
// would.
type handoffMailFailingStore struct{ beads.Store }

func (s handoffMailFailingStore) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, errors.New("messaging store unreachable")
}

// TestHandoffMailFailureClosesBothOps: a mail that is not created exits 1,
// records no restart, and closes both ops failed.
func TestHandoffMailFailureClosesBothOps(t *testing.T) {
	h, c := restartOpsCity(t, restartOpsEnv(), nil)
	var stdout, stderr bytes.Buffer
	r := unitRestart(t, c, "gc handoff", &stderr, nil)
	outcome := doHandoffWithOutcome(handoffMailFailingStore{c.store}, c.store, events.NewFake(), newFakeDrainOps(), r, killPokeSessionIdentity, c.name, []string{"context cycle"}, &stdout, &stderr)
	if outcome.code != 1 || outcome.restartRequested {
		t.Fatalf("outcome = %+v, want 1 and no restart", outcome)
	}
	if m := c.row(t).Metadata; m["restart_requested"] != "" {
		t.Fatalf("row %v records a restart after a failed mail", m)
	}
	restart, notice := opsByKind(t, h)
	if len(restart) != 1 || len(notice) != 1 {
		t.Fatalf("ops = %d/%d, want one of each", len(restart), len(notice))
	}
	if R := restart[0]; R.Status != opjournal.StatusClosed || R.Outcome.Arm != opjournal.ArmFailed || R.Outcome.Reason != opjournal.RefusedReasonPrefix+cliOpRefusedHandoffMailFailed {
		t.Fatalf("restart op = %+v, want failed{refused:handoff_mail_failed}", R)
	}
	if N := notice[0]; N.Status != opjournal.StatusClosed || N.Outcome.Arm != opjournal.ArmFailed {
		t.Fatalf("notice op = %+v, want closed failed", N)
	}
}

// TestHandoffNoticeRidesTheRestartOp: with lifecycle off nothing is recorded;
// with content off only the restart op is.
func TestHandoffNoticeRidesTheRestartOp(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		env                   map[string]string
		wantRestart, wantNote int
	}{
		{"lifecycle off", map[string]string{"GC_OPS_LIFECYCLE": "off", "GC_OPS_CONTENT": "auto"}, 0, 0},
		{"content off", map[string]string{"GC_OPS_LIFECYCLE": "auto", "GC_OPS_CONTENT": "off"}, 1, 0},
		{"both on", restartOpsEnv(), 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := restartOpsCity(t, tc.env, nil)
			if r := runHandoffForTest(c, ""); r.code != 0 {
				t.Fatalf("handoff = %d; stderr=%s", r.code, r.stderr)
			}
			restart, notice := opsByKind(t, h)
			if len(restart) != tc.wantRestart || len(notice) != tc.wantNote {
				t.Fatalf("ops = %d restart, %d notice; want %d and %d", len(restart), len(notice), tc.wantRestart, tc.wantNote)
			}
		})
	}
}

// TestHandoffAutoOpensNoOpStore (O6): the PreCompact hook path never opens
// the op store.
func TestHandoffAutoOpensNoOpStore(t *testing.T) {
	h, c := restartOpsCity(t, restartOpsEnv(), nil)
	var stdout, stderr bytes.Buffer
	if code := cmdHandoff(handoffRun{args: []string{"context cycle"}, auto: true, stdout: &stdout, jsonOut: &stdout, stderr: &stderr}); code != 0 {
		t.Fatalf("handoff --auto = %d; stderr=%s", code, stderr.String())
	}
	if h.opens != 0 {
		t.Fatalf("op store opened %d times on the hook path", h.opens)
	}
	if mails := listOpenMessagesBothTiers(t, c.reload(t)); len(mails) != 1 {
		t.Fatalf("auto mail beads = %d, want 1", len(mails))
	}
}

// TestHandoffTargetAndUnrestartableRecordNothing (O6): --target and a self
// handoff of a session the controller does not restart record no ops.
func TestHandoffTargetAndUnrestartableRecordNothing(t *testing.T) {
	t.Run("unrestartable", func(t *testing.T) {
		h, c := restartOpsCity(t, restartOpsEnv(), map[string]string{namedSessionModeMetadata: "on_demand"})
		r := runHandoffForTest(c, "")
		if r.code != 0 || !strings.Contains(r.stdout, "restart skipped") {
			t.Fatalf("handoff = %d stdout %q; stderr=%s", r.code, r.stdout, r.stderr)
		}
		if ops := cliOpRows(t, h.opStore); len(ops) != 0 {
			t.Fatalf("ops = %+v, want none", ops)
		}
	})
	t.Run("target", func(t *testing.T) {
		h, _ := restartOpsCity(t, restartOpsEnv(), nil)
		var stdout, stderr bytes.Buffer
		cmdHandoff(handoffRun{args: []string{"context cycle"}, target: killPokeSessionIdentity, stdout: &stdout, jsonOut: &stdout, stderr: &stderr})
		if h.opens != 0 {
			t.Fatalf("op store opened %d times by --target; stderr=%s", h.opens, stderr.String())
		}
	})
}

// TestHandoffKeyRefusesAutoAndTarget: --key with --auto or --target is
// refused before anything is written.
func TestHandoffKeyRefusesAutoAndTarget(t *testing.T) {
	for _, run := range []handoffRun{
		{args: []string{"context cycle"}, auto: true, key: "k"},
		{args: []string{"context cycle"}, target: killPokeSessionIdentity, key: "k"},
	} {
		h, c := restartOpsCity(t, restartOpsEnv(), nil)
		var stdout, stderr bytes.Buffer
		run.stdout, run.jsonOut, run.stderr = &stdout, &stdout, &stderr
		if code := cmdHandoff(run); code != 1 {
			t.Fatalf("handoff %+v = %d, want 1", run, code)
		}
		if !strings.Contains(stderr.String(), errHandoffKeyWithoutOps.Error()) {
			t.Fatalf("stderr = %q, want the --key refusal", stderr.String())
		}
		if mails := listOpenMessagesBothTiers(t, c.reload(t)); len(mails) != 0 || h.opens != 0 {
			t.Fatalf("mail %d, op store opens %d; want nothing written", len(mails), h.opens)
		}
	}
}

// TestHandoffReplaySendsNoMail: a same-key rerun replays from the op and
// sends no second mail.
func TestHandoffReplaySendsNoMail(t *testing.T) {
	h, c := restartOpsCity(t, restartOpsEnv(), nil)
	if r := runHandoffForTest(c, "k-replay"); r.code != 0 {
		t.Fatalf("first = %d; stderr=%s", r.code, r.stderr)
	}
	r := runHandoffForTest(c, "k-replay")
	restart, _ := opsByKind(t, h)
	if r.code != 0 || r.stdout != "Handoff: restart requested.\n" || !strings.Contains(r.stderr, "replayed op "+restart[0].ID) || !strings.Contains(r.stderr, "gc handoff: a replay sends no mail") {
		t.Fatalf("replay = %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if mails := listOpenMessagesBothTiers(t, c.reload(t)); len(mails) != 1 {
		t.Fatalf("mail beads = %d, want the first run's only", len(mails))
	}
}

// TestRestartVerbsDoNotReplayEachOthersKey: gc session reset, gc runtime
// request-restart and gc handoff record the same request (restart{fresh} on
// the conversation, no payload), so the in-session verbs store --key in their
// own namespace: a run under a key another verb already used records its own
// op and acts, rather than replaying the other's. A handoff after a
// request-restart under one key sends its mail.
func TestRestartVerbsDoNotReplayEachOthersKey(t *testing.T) {
	requestRestart := func(_ *cliOpCity, key string) cliOpVerbRun {
		var stdout, stderr bytes.Buffer
		code := cmdRuntimeRequestRestart(key, &stdout, &stderr)
		return cliOpVerbRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}
	reset := func(c *cliOpCity, key string) cliOpVerbRun {
		var stdout, stderr bytes.Buffer
		code := cmdSessionReset([]string{c.bead.ID}, key, &stdout, &stderr, false)
		return cliOpVerbRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}
	for _, tc := range []struct {
		name        string
		first, then func(*cliOpCity, string) cliOpVerbRun
		wantMails   int
	}{
		{"request-restart then handoff", requestRestart, runHandoffForTest, 1},
		{"handoff then request-restart", runHandoffForTest, requestRestart, 1},
		{"session reset then handoff", reset, runHandoffForTest, 1},
		{"session reset then request-restart", reset, requestRestart, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := restartOpsCity(t, restartOpsEnv(), nil)
			startCLIOpController(t, c.dir)
			if r := tc.first(c, "k-shared"); r.code != 0 {
				t.Fatalf("first run = %d; stderr=%s", r.code, r.stderr)
			}
			r := tc.then(c, "k-shared")
			if r.code != 0 || strings.Contains(r.stderr, "replayed op") {
				t.Fatalf("second run = %d, stdout %q, stderr %q; want it to act under its own op, not replay", r.code, r.stdout, r.stderr)
			}
			restart, _ := opsByKind(t, h)
			if len(restart) != 2 || restart[0].ID == restart[1].ID {
				t.Fatalf("restart ops = %+v, want one per verb", restart)
			}
			for _, op := range restart {
				if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmSucceeded {
					t.Fatalf("restart op %s = %s %+v, want closed succeeded", op.ID, op.Status, op.Outcome)
				}
			}
			if mails := listOpenMessagesBothTiers(t, c.reload(t)); len(mails) != tc.wantMails {
				t.Fatalf("mail beads = %d, want %d", len(mails), tc.wantMails)
			}
		})
	}
}

// TestSessionResetKeyCannotNameARestartRequestsOp: gc session reset keeps
// --key as given, so a reset key that spells an in-session restart request's
// stored key (handoff:<key>, request-restart:<key>) would name that request's
// op, and whichever ran second would replay the other's success and do
// nothing. The namespaces are reserved: in auto such a reset warns that its
// --key is invalid and resets unrecorded, in either order, while the restart
// request records and acts under its own op; in require the reset is refused
// invalid_key with no effect.
func TestSessionResetKeyCannotNameARestartRequestsOp(t *testing.T) {
	requestRestart := func(_ *cliOpCity, key string) cliOpVerbRun {
		var stdout, stderr bytes.Buffer
		code := cmdRuntimeRequestRestart(key, &stdout, &stderr)
		return cliOpVerbRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}
	for _, verb := range []struct {
		name, namespace string
		run             func(*cliOpCity, string) cliOpVerbRun
		mails           int
	}{
		{"handoff", legacyop.KeyNamespaceHandoff, runHandoffForTest, 1},
		{"request-restart", legacyop.KeyNamespaceRequestRestart, requestRestart, 0},
	} {
		resetKey := verb.namespace + "abc"
		for _, resetFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/reset first %t", verb.name, resetFirst), func(t *testing.T) {
				h, c := restartOpsCity(t, restartOpsEnv(), nil)
				ctl := startCLIOpController(t, c.dir)
				reset := func() {
					t.Helper()
					var stdout, stderr bytes.Buffer
					code := cmdSessionReset([]string{c.bead.ID}, resetKey, &stdout, &stderr, false)
					if code != 0 || !strings.Contains(stderr.String(), "warning: invalid --key") || strings.Contains(stderr.String(), "replayed op") {
						t.Fatalf("reset --key %s = %d, stderr %q; want 0, an invalid-key warning and no replay", resetKey, code, stderr.String())
					}
				}
				if resetFirst {
					reset()
				}
				if r := verb.run(c, "abc"); r.code != 0 || strings.Contains(r.stderr, "replayed op") {
					t.Fatalf("%s --key abc = %d, stdout %q, stderr %q; want it to act under its own op", verb.name, r.code, r.stdout, r.stderr)
				}
				if !resetFirst {
					reset()
				}
				if got := ctl.breakerResets.Load(); got != 1 {
					t.Fatalf("circuit-breaker resets = %d, want the reset's one", got)
				}
				restart, _ := opsByKind(t, h)
				if len(restart) != 1 || restart[0].ClientKey != resetKey || restart[0].Status != opjournal.StatusClosed || restart[0].Outcome.Arm != opjournal.ArmSucceeded {
					t.Fatalf("restart ops = %+v, want only the %s's, closed succeeded", restart, verb.name)
				}
				if mails := listOpenMessagesBothTiers(t, c.reload(t)); len(mails) != verb.mails {
					t.Fatalf("mail beads = %d, want %d", len(mails), verb.mails)
				}
			})
		}
		t.Run(verb.name+"/require refuses the reset", func(t *testing.T) {
			h, c := restartOpsCity(t, map[string]string{"GC_OPS_LIFECYCLE": "require", "GC_OPS_CONTENT": "require", "GC_OPS_CONTROL": "off"}, nil)
			ctl := startCLIOpController(t, c.dir)
			before := c.snap(t)
			var stdout, stderr bytes.Buffer
			if code := cmdSessionReset([]string{c.bead.ID}, resetKey, &stdout, &stderr, true); code != 1 || cliOpFailureCode(t, stdout.Bytes()) != cliOpCodeInvalidKey {
				t.Fatalf("reset --key %s = %d, stdout %q, stderr %q; want 1, invalid_key", resetKey, code, stdout.String(), stderr.String())
			}
			if d := c.snap(t).diff(before); d != "" || ctl.breakerResets.Load() != 0 {
				t.Fatalf("the refused reset acted: row %s, circuit-breaker resets %d", d, ctl.breakerResets.Load())
			}
			if ops := cliOpRows(t, h.opStore); len(ops) != 0 {
				t.Fatalf("op rows = %+v, want none", ops)
			}
		})
	}
}

// TestRestartVerbsKeepAnInvalidKeyInvalid: the key namespace never turns a
// --key the journal refuses (one with leading space) into a valid stored
// key, and a --key valid elsewhere that the namespace makes too long is
// invalid for the verb, named by its own length and the verb's limit, while
// the longest key the namespace leaves room for records. Under require an
// invalid key is refused invalid_key with no effect; under auto it warns and
// runs unrecorded.
func TestRestartVerbsKeepAnInvalidKeyInvalid(t *testing.T) {
	for _, tc := range cliOpVerbCases() {
		if tc.keyNamespace == "" {
			continue
		}
		limit := opjournal.MaxClientKeyLen - len(tc.keyNamespace)
		for _, k := range []struct{ name, key, says string }{
			{"padded", " padded", "leading or trailing space"},
			{"too long in its namespace", strings.Repeat("k", limit+1), fmt.Sprintf("client key is %d bytes, max %d in the %q key namespace", limit+1, limit, tc.keyNamespace)},
		} {
			t.Run(tc.name+"/"+k.name+"/require refuses", func(t *testing.T) {
				f := newCLIOpVerbFixture(t, tc, map[string]string{"GC_OPS_LIFECYCLE": "require", "GC_OPS_CONTENT": "require"})
				before := f.c.snap(t)
				r := f.run(k.key, true)
				if r.code != 1 || !strings.Contains(r.stderr, k.says) {
					t.Fatalf("%s = %d, stderr %q; want 1 for an invalid key in require, naming %q", tc.name, r.code, r.stderr, k.says)
				}
				if got := tc.failureCode(t, r); got != cliOpCodeInvalidKey {
					t.Fatalf("failure code = %q, want %s", got, cliOpCodeInvalidKey)
				}
				if d := f.c.snap(t).diff(before); d != "" {
					t.Fatalf("require acted on an invalid key: %s", d)
				}
				if ops := cliOpRows(t, f.h.opStore); len(ops) != 0 {
					t.Fatalf("op rows = %+v, want none", ops)
				}
			})
			t.Run(tc.name+"/"+k.name+"/auto warns and runs unrecorded", func(t *testing.T) {
				f := newCLIOpVerbFixture(t, tc, map[string]string{"GC_OPS_LIFECYCLE": "auto", "GC_OPS_CONTENT": "auto"})
				r := f.run(k.key, false)
				if r.code != 0 || !strings.Contains(r.stderr, "warning: invalid --key (") || !strings.Contains(r.stderr, k.says) {
					t.Fatalf("%s = %d, stderr %q; want 0 and an invalid-key warning naming %q", tc.name, r.code, r.stderr, k.says)
				}
				if ops := cliOpRows(t, f.h.opStore); len(ops) != 0 {
					t.Fatalf("op rows = %+v, want none for an invalid key", ops)
				}
			})
		}
		t.Run(tc.name+"/the longest key its namespace allows records", func(t *testing.T) {
			f := newCLIOpVerbFixture(t, tc, map[string]string{"GC_OPS_LIFECYCLE": "auto", "GC_OPS_CONTENT": "auto"})
			key := strings.Repeat("k", limit)
			if r := f.run(key, false); r.code != 0 || strings.Contains(r.stderr, "--key") {
				t.Fatalf("%s = %d, stderr %q; want 0 and no --key warning", tc.name, r.code, r.stderr)
			}
			restart, _ := opsByKind(t, f.h)
			if len(restart) != 1 || restart[0].ClientKey != tc.keyNamespace+key {
				t.Fatalf("restart ops = %+v, want one under the %d-byte key in %q", restart, limit, tc.keyNamespace)
			}
		})
	}
}

// TestHandoffSameKeyRerunWithAnotherSubjectReplays: the key covers neither
// the subject nor the message, so a same-key rerun with another subject
// replays the first run and sends no mail, and its --json result leaves the
// subject out rather than naming one no mail carried.
func TestHandoffSameKeyRerunWithAnotherSubjectReplays(t *testing.T) {
	_, c := restartOpsCity(t, restartOpsEnv(), nil)
	run := func(subject string) (map[string]any, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := cmdHandoff(handoffRun{args: []string{subject, "notes for the successor"}, key: "k-subject", asJSON: true, stdout: io.Discard, jsonOut: &stdout, stderr: &stderr}); code != 0 {
			t.Fatalf("handoff %q = %d; stderr=%s", subject, code, stderr.String())
		}
		validateJSONAgainstResultSchema(t, []string{"handoff"}, stdout.Bytes())
		return cliOpJSON(t, stdout.Bytes()), stderr.String()
	}
	if first, _ := run("subject A"); first["subject"] != "subject A" || first["replayed"] == true {
		t.Fatalf("first JSON %v, want subject A, not replayed", first)
	}
	replay, stderr := run("subject B")
	if _, ok := replay["subject"]; ok || replay["replayed"] != true || replay["key"] != "k-subject" {
		t.Fatalf("replay JSON %v, want replayed under k-subject with no subject", replay)
	}
	if !strings.Contains(stderr, "gc handoff: a replay sends no mail") {
		t.Fatalf("replay stderr %q, want the no-mail note", stderr)
	}
	if mails := listOpenMessagesBothTiers(t, c.reload(t)); len(mails) != 1 || mails[0].Title != "subject A" {
		t.Fatalf("mail beads = %+v, want the first run's subject A only", mails)
	}
}

// sweepRestartOpAfterRowWrite makes r's row write leave its restart op to the
// op sweeper: once the row lands, the sweeper closes the op
// failed{deadline_in_flight}, as when the restart the row requests stops the
// run before the run closes its op.
func sweepRestartOpAfterRowWrite(t *testing.T, h *cliOpHarness, r *rowFirstRestart) {
	t.Helper()
	write := r.record
	r.record = func(token string) error {
		if err := write(token); err != nil {
			return err
		}
		restart, _ := opsByKind(t, h)
		if len(restart) != 1 {
			t.Fatalf("restart ops at the row write = %+v, want one", restart)
		}
		past := restart[0].FinishBy.Add(time.Second)
		st, err := opjournal.NewStore(beads.SessionStore{Store: h.opStore}, &clock.Fake{Time: past}, opjournal.WithIDPrefix("gc"))
		if err != nil {
			t.Fatalf("opening op store: %v", err)
		}
		if _, closed, err := st.CloseExpired(restart[0], past, "sweeper"); err != nil || !closed {
			t.Fatalf("sweeping the restart op: closed %t, err %v", closed, err)
		}
		return nil
	}
}

// restartFresh moves c's row past the fresh restart its request asked for,
// as the controller's next start leaves it: the request consumed, the
// continuation epoch at epoch, and a new incarnation, whose token the next
// run runs under.
func restartFresh(t *testing.T, c *cliOpCity, epoch int) {
	t.Helper()
	const token = "tok-cli-op-successor"
	if err := c.reload(t).SetMetadataBatch(c.bead.ID, map[string]string{
		"restart_requested": "", "continuation_reset_pending": "", "continuation_epoch": strconv.Itoa(epoch),
		"generation": "5", "instance_token": token,
	}); err != nil {
		t.Fatalf("restarting the session row: %v", err)
	}
	t.Setenv("GC_INSTANCE_TOKEN", token)
}

// TestHandoffSweptRestartOpReplaysWhenTheRowShowsTheRestart: the restart a
// handoff requests stops the session, and so can stop the run between the
// row write and the restart op's close, leaving the op to the sweeper, which
// closes it failed{deadline_in_flight}. A same-key rerun replays it as the
// first run's success, sending no second mail, when the notice op succeeded
// and the row shows the restart still pending or past the context the notice
// names. It reports the op's failure when the row shows no restart, when the
// first run recorded no notice, or when the notice does not read.
func TestHandoffSweptRestartOpReplaysWhenTheRowShowsTheRestart(t *testing.T) {
	ran := func(t *testing.T, _ *cliOpHarness, c *cliOpCity) { restartFresh(t, c, 4) }
	for _, tc := range []struct {
		name    string
		content string // the gate the first run records its notice under
		after   func(t *testing.T, h *cliOpHarness, c *cliOpCity)
		// shows is how the replay line says the row shows the restart; ""
		// wants the op's failure.
		shows    string
		warnings string
	}{
		{name: "restart ran", content: "auto", after: ran, shows: "ran"},
		{name: "restart pending", content: "auto", shows: "still pending"},
		{name: "restart never ran", content: "auto", after: func(t *testing.T, _ *cliOpHarness, c *cliOpCity) {
			if err := c.reload(t).SetMetadataBatch(c.bead.ID, map[string]string{"restart_requested": "", "continuation_reset_pending": ""}); err != nil {
				t.Fatalf("withdrawing the restart request: %v", err)
			}
		}},
		{name: "no notice recorded", content: "off", after: ran},
		{name: "notice does not read", content: "auto", after: func(t *testing.T, h *cliOpHarness, c *cliOpCity) {
			ran(t, h, c)
			_, notice := opsByKind(t, h)
			h.opStore = noticeGetFailingStore{Store: h.opStore, id: notice[0].ID}
		}, warnings: "gc handoff: warning: reading the first run's notice op: reading op "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := restartOpsEnv()
			env["GC_OPS_CONTENT"] = tc.content
			h, c := restartOpsCity(t, env, nil)
			rows := h.opStore
			var stderr bytes.Buffer
			r := unitRestart(t, c, "gc handoff", &stderr, nil)
			r.ops = newRestartOps(nil, nil, c.store, "k-swept", c.name, killPokeSessionIdentity, false, io.Discard, &stderr)
			sweepRestartOpAfterRowWrite(t, h, &r)
			outcome := doHandoffWithOutcome(c.store, c.store, events.NewFake(), newFakeDrainOps(), r, killPokeSessionIdentity, c.name, []string{"context cycle"}, io.Discard, &stderr)
			if outcome.code != 0 || !outcome.restartRequested {
				t.Fatalf("first run = %+v; stderr=%s", outcome, stderr.String())
			}
			restart, notice := opsByKind(t, h)
			R := restart[0]
			cliOpWantOutcome(t, R, opjournal.ArmFailed, opjournal.ReasonDeadlineInFlight, "")
			if tc.after != nil {
				tc.after(t, h, c)
			}

			rerun := runHandoffForTest(c, "k-swept")
			if tc.shows == "" {
				want := "op " + R.ID + " closed failed{deadline_in_flight}; rerun without --key to act again (key k-swept)"
				if rerun.code != 1 || !strings.Contains(rerun.stderr, want) {
					t.Fatalf("rerun = %d, stderr %q; want 1 and %q", rerun.code, rerun.stderr, want)
				}
			} else {
				want := "gc handoff: replayed op " + R.ID + ": it closed failed{deadline_in_flight}, its run having stopped before closing it, and the session row shows the restart it requested " + tc.shows
				if rerun.code != 0 || rerun.stdout != "Handoff: restart requested.\n" || !strings.Contains(rerun.stderr, want) {
					t.Fatalf("rerun = %d, stdout %q, stderr %q; want 0 and %q", rerun.code, rerun.stdout, rerun.stderr, want)
				}
			}
			if tc.warnings != "" && !strings.Contains(rerun.stderr, tc.warnings) {
				t.Fatalf("rerun stderr %q, want %q", rerun.stderr, tc.warnings)
			}
			if mails := listOpenMessagesBothTiers(t, c.reload(t)); len(mails) != 1 {
				t.Fatalf("mail beads = %d, want the first run's only", len(mails))
			}
			if ops := cliOpRows(t, rows); len(ops) != len(restart)+len(notice) {
				t.Fatalf("ops = %+v, want the first run's only", ops)
			}
			for _, op := range cliOpRows(t, rows) {
				if op.ID == R.ID {
					cliOpWantOutcome(t, op, opjournal.ArmFailed, opjournal.ReasonDeadlineInFlight, "")
				}
			}
		})
	}
}

// TestRequestRestartSweptRestartOpReplaysOnlyWhilePending: request-restart's
// restart op records no context, so a same-key rerun of an op the sweeper
// closed failed{deadline_in_flight} replays as the first run's success only
// while the row still records the restart as pending; once the restart has
// run, the rerun cannot tell it from one that never ran and reports the op's
// failure.
func TestRequestRestartSweptRestartOpReplaysOnlyWhilePending(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ran     bool
		replays bool
	}{
		{"restart pending", false, true},
		{"restart ran", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := restartOpsCity(t, restartOpsEnv(), nil)
			var stderr bytes.Buffer
			r := unitRestart(t, c, "gc runtime request-restart", &stderr, nil)
			r.ops = newRestartOps(nil, nil, c.store, "k-swept", c.name, killPokeSessionIdentity, false, io.Discard, &stderr)
			sweepRestartOpAfterRowWrite(t, h, &r)
			if code := doRuntimeRequestRestart(r, events.Discard, io.Discard); code != 0 {
				t.Fatalf("first run = %d; stderr=%s", code, stderr.String())
			}
			R := cliOpOnly(t, h.opStore)
			cliOpWantOutcome(t, R, opjournal.ArmFailed, opjournal.ReasonDeadlineInFlight, "")
			if tc.ran {
				restartFresh(t, c, 4)
			}

			var stdout, rerunErr bytes.Buffer
			code := cmdRuntimeRequestRestart("k-swept", &stdout, &rerunErr)
			if tc.replays {
				want := "gc runtime request-restart: replayed op " + R.ID + ": it closed failed{deadline_in_flight}, its run having stopped before closing it, and the session row shows the restart it requested still pending"
				if code != 0 || stdout.String() != "Restart requested.\n" || !strings.Contains(rerunErr.String(), want) {
					t.Fatalf("rerun = %d, stdout %q, stderr %q; want 0 and %q", code, stdout.String(), rerunErr.String(), want)
				}
			} else if want := "op " + R.ID + " closed failed{deadline_in_flight}; rerun without --key to act again (key k-swept)"; code != 1 || !strings.Contains(rerunErr.String(), want) {
				t.Fatalf("rerun = %d, stderr %q; want 1 and %q", code, rerunErr.String(), want)
			}
			if op := cliOpOnly(t, h.opStore); op.ID != R.ID {
				t.Fatalf("op = %+v, want the first run's %s only", op, R.ID)
			}
		})
	}
}

// noticeGetFailingStore fails reading one op row by id, as a store that
// stops answering would.
type noticeGetFailingStore struct {
	beads.Store
	id string
}

// ConditionalWritesResolveTarget keeps the wrapped store's conditional
// writes, so the op store still opens over it.
func (s noticeGetFailingStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

func (s noticeGetFailingStore) Get(id string) (beads.Bead, error) {
	if id == s.id {
		return beads.Bead{}, errors.New("store down")
	}
	return s.Store.Get(id)
}

// TestHandoffJSONReplayNamesTheFirstRunsNotice: a same-key --json rerun
// reports the first run's restart op and the notice op recorded beside it,
// replayed; a first run that recorded no notice replays with none, and a
// notice that does not read is left out with a warning.
func TestHandoffJSONReplayNamesTheFirstRunsNotice(t *testing.T) {
	run := func(t *testing.T, c *cliOpCity, key string) (map[string]any, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := runHandoffVerb(c, key, true, &stdout, &stderr); code != 0 {
			t.Fatalf("handoff --key %s = %d; stderr=%s", key, code, stderr.String())
		}
		validateJSONAgainstResultSchema(t, []string{"handoff"}, stdout.Bytes())
		return cliOpJSON(t, stdout.Bytes()), stderr.String()
	}
	t.Run("notice recorded", func(t *testing.T) {
		h, c := restartOpsCity(t, restartOpsEnv(), nil)
		first, _ := run(t, c, "k-json-replay")
		restart, notice := opsByKind(t, h)
		if len(restart) != 1 || len(notice) != 1 || first["notice_op_id"] != notice[0].ID {
			t.Fatalf("first JSON %v over ops %+v / %+v, want one of each, naming the notice", first, restart, notice)
		}
		replay, _ := run(t, c, "k-json-replay")
		if replay["replayed"] != true || replay["op_id"] != restart[0].ID || replay["notice_op_id"] != notice[0].ID || replay["key"] != "k-json-replay" {
			t.Fatalf("replay JSON %v, want restart op %s and notice op %s, replayed", replay, restart[0].ID, notice[0].ID)
		}
	})
	t.Run("no notice recorded", func(t *testing.T) {
		h, c := restartOpsCity(t, map[string]string{"GC_OPS_LIFECYCLE": "auto", "GC_OPS_CONTENT": "off", "GC_OPS_CONTROL": "off"}, nil)
		run(t, c, "k-json-replay")
		h.env["GC_OPS_CONTENT"] = "auto"
		replay, stderr := run(t, c, "k-json-replay")
		if _, ok := replay["notice_op_id"]; ok || replay["replayed"] != true || strings.Contains(stderr, "notice op") {
			t.Fatalf("replay JSON %v, stderr %q; want replayed with no notice_op_id and no notice warning", replay, stderr)
		}
	})
	t.Run("notice does not read", func(t *testing.T) {
		h, c := restartOpsCity(t, restartOpsEnv(), nil)
		run(t, c, "k-json-replay")
		_, notice := opsByKind(t, h)
		if len(notice) != 1 {
			t.Fatalf("notice ops = %+v, want one", notice)
		}
		h.opStore = noticeGetFailingStore{Store: h.opStore, id: notice[0].ID}
		replay, stderr := run(t, c, "k-json-replay")
		const want = "gc handoff: warning: reading the first run's notice op: reading op "
		if _, ok := replay["notice_op_id"]; ok || replay["replayed"] != true || !strings.Contains(stderr, want) || !strings.Contains(stderr, "store down; the JSON result leaves out notice_op_id") {
			t.Fatalf("replay JSON %v, stderr %q; want replayed with no notice_op_id and %q", replay, stderr, want)
		}
	})
}

// TestRestartOpKeysDoNotCollideUnderReexecution (§3.3; the row's acceptance
// test): two agent processes under one GC_SESSION_ID, GC_INSTANCE_TOKEN and
// GC_RUNTIME_EPOCH, as a relaunch in place or a stale-key retry runs them,
// get distinct minted keys: neither replays nor refuses the other.
func TestRestartOpKeysDoNotCollideUnderReexecution(t *testing.T) {
	env := restartOpsEnv()
	env["GC_SESSION_ID"] = "gc-agent-7"
	for _, verb := range []string{"request-restart", "handoff"} {
		t.Run(verb, func(t *testing.T) {
			h, c := restartOpsCity(t, env, nil)
			t.Setenv("GC_RUNTIME_EPOCH", "4")
			for i := 0; i < 2; i++ {
				var stdout, stderr bytes.Buffer
				var code int
				if verb == "request-restart" {
					code = cmdRuntimeRequestRestart("", &stdout, &stderr)
				} else {
					code = runHandoffVerb(c, "", false, &stdout, &stderr)
				}
				if code != 0 || strings.Contains(stderr.String(), "replayed op") {
					t.Fatalf("run %d = %d; stderr=%s", i, code, stderr.String())
				}
			}
			restart, notice := opsByKind(t, h)
			if len(restart) != 2 || restart[0].ID == restart[1].ID || restart[0].ClientKey == restart[1].ClientKey {
				t.Fatalf("restart ops = %+v, want two distinct", restart)
			}
			for _, op := range restart {
				if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmSucceeded {
					t.Fatalf("restart op %s = %s %+v, want closed succeeded", op.ID, op.Status, op.Outcome)
				}
			}
			if verb == "handoff" && (len(notice) != 2 || notice[0].ID == notice[1].ID) {
				t.Fatalf("notice ops = %+v, want two distinct", notice)
			}
		})
	}
}

// TestHandoffJSONMailFailureWritesOneRecord: a --json handoff whose mail
// fails with both ops recorded closes both failed, but writes exactly one
// JSON record (the bounded-command rule), naming the restart op.
func TestHandoffJSONMailFailureWritesOneRecord(t *testing.T) {
	h, c := restartOpsCity(t, restartOpsEnv(), nil)
	var stdout, stderr, jsonOut bytes.Buffer
	r := unitRestart(t, c, "gc handoff", &stderr, nil)
	r.ops = newRestartOps(nil, nil, c.store, "", c.name, killPokeSessionIdentity, true, &jsonOut, &stderr)
	outcome := doHandoffWithOutcome(handoffMailFailingStore{c.store}, c.store, events.NewFake(), newFakeDrainOps(), r, killPokeSessionIdentity, c.name, []string{"context cycle"}, &stdout, &stderr)
	if outcome.code != 1 {
		t.Fatalf("outcome = %+v, want 1", outcome)
	}
	restart, notice := opsByKind(t, h)
	if len(restart) != 1 || len(notice) != 1 || restart[0].Outcome.Arm != opjournal.ArmFailed || notice[0].Outcome.Arm != opjournal.ArmFailed {
		t.Fatalf("ops = %+v / %+v, want both closed failed", restart, notice)
	}
	lines := strings.Split(strings.TrimSpace(jsonOut.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("JSON records = %d, want exactly one:\n%s", len(lines), jsonOut.String())
	}
	var record struct {
		OpID string `json:"op_id"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("decoding %q: %v", lines[0], err)
	}
	if record.OpID != restart[0].ID {
		t.Fatalf("record op_id = %q, want the restart op %q", record.OpID, restart[0].ID)
	}
}

// TestHandoffKeyOnASessionTheControllerDoesNotRestartWarns: a self handoff
// of a session the controller does not restart records no ops, so its --key
// is not recorded; the run says so, and a rerun sends the mail again. With
// every ops gate off the key is ignored, as the flag says, and nothing warns.
func TestHandoffKeyOnASessionTheControllerDoesNotRestartWarns(t *testing.T) {
	unrestartable := map[string]string{namedSessionModeMetadata: "on_demand"}
	t.Run("gate records", func(t *testing.T) {
		h, c := restartOpsCity(t, restartOpsEnv(), unrestartable)
		const want = "gc handoff: warning: --key not recorded (the controller does not restart this session, so the handoff records no ops); a rerun sends the mail again"
		for i := range 2 {
			r := runHandoffForTest(c, "k-unrestartable")
			if r.code != 0 || !strings.Contains(r.stderr, want) {
				t.Fatalf("run %d: exit %d, stderr %q; want 0 and %q", i+1, r.code, r.stderr, want)
			}
		}
		if mails := listOpenMessagesBothTiers(t, c.reload(t)); len(mails) != 2 {
			t.Fatalf("mails = %d, want 2 (nothing dedups an unrecorded key)", len(mails))
		}
		if rows := cliOpRows(t, h.opStore); len(rows) != 0 {
			t.Fatalf("ops = %d, want none", len(rows))
		}
		if r := runHandoffForTest(c, ""); strings.Contains(r.stderr, "--key") {
			t.Fatalf("stderr = %q, want no --key warning without --key", r.stderr)
		}
	})
	t.Run("gates off", func(t *testing.T) {
		_, c := restartOpsCity(t, map[string]string{"GC_OPS_LIFECYCLE": "off", "GC_OPS_CONTENT": "off", "GC_OPS_CONTROL": "off"}, unrestartable)
		if r := runHandoffForTest(c, "k-unrestartable"); r.code != 0 || strings.Contains(r.stderr, "--key") {
			t.Fatalf("exit %d, stderr %q; want 0 and no --key warning with the gates off", r.code, r.stderr)
		}
	})
}

// TestHandoffContentRequireRidesAnUnrecordedRestart pins the documented "N
// rides R" consequence: with ops.content=require and no restart op recorded
// (ops.lifecycle off, or auto over a store that cannot record), the notice op
// is neither checked nor proposed, so the handoff sends its mail, records the
// restart and exits 0.
func TestHandoffContentRequireRidesAnUnrecordedRestart(t *testing.T) {
	for _, tc := range []struct {
		name, lifecycle string
		incapable       bool
	}{
		{"lifecycle off", "off", false},
		{"lifecycle off, store cannot record", "off", true},
		{"lifecycle auto, store cannot record", "auto", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := restartOpsCity(t, map[string]string{"GC_OPS_LIFECYCLE": tc.lifecycle, "GC_OPS_CONTENT": "require", "GC_OPS_CONTROL": "off"}, nil)
			rows := h.opStore
			if tc.incapable {
				h.opStore = cliOpIncapableStore{rows}
			}
			if r := runHandoffForTest(c, ""); r.code != 0 {
				t.Fatalf("handoff = %d; stderr=%s", r.code, r.stderr)
			}
			if ops := cliOpRows(t, rows); len(ops) != 0 {
				t.Fatalf("ops = %d, want none", len(ops))
			}
			if mails := listOpenMessagesBothTiers(t, c.reload(t)); len(mails) != 1 {
				t.Fatalf("mail beads = %d, want 1", len(mails))
			}
			if m := c.row(t).Metadata; m["restart_requested"] != "true" {
				t.Fatalf("row %v, want the restart recorded", m)
			}
		})
	}
}

// noticeCreateFailingStore fails the create of every message op row, as a
// journal write failing between the restart op and its notice would.
type noticeCreateFailingStore struct{ beads.Store }

// ConditionalWritesResolveTarget keeps the wrapped store's conditional
// writes, so the op store still opens over it.
func (s noticeCreateFailingStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

func (s noticeCreateFailingStore) Create(b beads.Bead) (beads.Bead, error) {
	if op, err := opjournal.DecodeOp(b); err == nil && op.Kind == opjournal.KindMessage {
		return beads.Bead{}, errors.New("database is locked")
	}
	return s.Store.Create(b)
}

// TestHandoffNoticeRefusalReportsTheRestartOp: under ops.content=require, a
// notice op the journal refuses once the restart op is recorded closes the
// restart op failed{refused:notice_refused} with nothing run. The run names
// the restart op and the caller's key, on stderr and in its one --json
// record, and a same-key rerun is refused as that op's failure.
func TestHandoffNoticeRefusalReportsTheRestartOp(t *testing.T) {
	h, c := restartOpsCity(t, map[string]string{"GC_OPS_LIFECYCLE": "auto", "GC_OPS_CONTENT": "require", "GC_OPS_CONTROL": "off"}, nil)
	rows := h.opStore
	h.opStore = noticeCreateFailingStore{rows}
	var stdout, stderr bytes.Buffer
	if code := runHandoffVerb(c, "k-notice", true, &stdout, &stderr); code != 1 {
		t.Fatalf("handoff = %d, want 1; stderr=%s", code, stderr.String())
	}
	R := cliOpOnly(t, rows)
	cliOpWantOutcome(t, R, opjournal.ArmFailed, opjournal.RefusedReasonPrefix+cliOpRefusedNoticeRefused, "")
	if R.Kind != opjournal.KindRestart || R.ClientKey != legacyop.KeyNamespaceHandoff+"k-notice" {
		t.Fatalf("op = %+v, want the restart op under handoff:k-notice", R)
	}
	want := "gc handoff: op " + R.ID + " (key k-notice): failed{refused:notice_refused}: the journal refused the handoff's notice op, so nothing ran; rerun without --key to act again"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("JSON records = %d, want exactly one:\n%s", len(lines), stdout.String())
	}
	if code := cliOpFailureCode(t, []byte(lines[0])); code != cliOpCodeFailed {
		t.Fatalf("failure code = %q, want %s", code, cliOpCodeFailed)
	}
	if m := cliOpJSON(t, []byte(lines[0])); m["op_id"] != R.ID || m["key"] != "k-notice" {
		t.Fatalf("failure envelope %v, want op_id %s and key k-notice", m, R.ID)
	}
	wantNothingRan := func(run string) {
		t.Helper()
		if mails := listOpenMessagesBothTiers(t, c.reload(t)); len(mails) != 0 {
			t.Fatalf("%s: mail beads = %d, want none", run, len(mails))
		}
		if m := c.row(t).Metadata; m["restart_requested"] != "" {
			t.Fatalf("%s: row %v records a restart", run, m)
		}
	}
	wantNothingRan("first run")

	h.opStore = rows
	r := runHandoffForTest(c, "k-notice")
	if r.code != 1 || !strings.Contains(r.stderr, "op "+R.ID+" closed failed{refused:notice_refused}; rerun without --key to act again (key k-notice)") {
		t.Fatalf("rerun = %d, stderr %q; want 1 and %s's failed replay", r.code, r.stderr, R.ID)
	}
	wantNothingRan("rerun")
	if ops := cliOpRows(t, rows); len(ops) != 1 {
		t.Fatalf("ops = %+v, want the first run's restart op only", ops)
	}
}
