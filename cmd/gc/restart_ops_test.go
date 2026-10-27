package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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
	if r.code != 0 || r.stdout != "Handoff: restart requested.\n" || !strings.Contains(r.stderr, "replayed op "+restart[0].ID) || !strings.Contains(r.stderr, "the first run's mail is not sent again") {
		t.Fatalf("replay = %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if mails := listOpenMessagesBothTiers(t, c.reload(t)); len(mails) != 1 {
		t.Fatalf("mail beads = %d, want the first run's only", len(mails))
	}
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
// is not recorded; the run says so, and a rerun sends the mail again.
func TestHandoffKeyOnASessionTheControllerDoesNotRestartWarns(t *testing.T) {
	h, c := restartOpsCity(t, restartOpsEnv(), map[string]string{namedSessionModeMetadata: "on_demand"})
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
}

// TestHandoffContentRequireWithLifecycleOffRecordsNoNotice pins the
// documented "N rides R" consequence: with ops.content=require and
// ops.lifecycle off, no restart op is recorded, so no notice op is proposed
// and the handoff runs unrecorded and exits 0.
func TestHandoffContentRequireWithLifecycleOffRecordsNoNotice(t *testing.T) {
	h, c := restartOpsCity(t, map[string]string{"GC_OPS_LIFECYCLE": "off", "GC_OPS_CONTENT": "require", "GC_OPS_CONTROL": "off"}, nil)
	if r := runHandoffForTest(c, ""); r.code != 0 {
		t.Fatalf("handoff = %d; stderr=%s", r.code, r.stderr)
	}
	if rows := cliOpRows(t, h.opStore); len(rows) != 0 {
		t.Fatalf("ops = %d, want none", len(rows))
	}
	if m := c.row(t).Metadata; m["restart_requested"] != "true" {
		t.Fatalf("row %v, want the restart recorded", m)
	}
}
