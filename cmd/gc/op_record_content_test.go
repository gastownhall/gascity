package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gascity "github.com/gastownhall/gascity"
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// testNudgeMessage is the text deliverSessionNudgeWithWorker nudges with.
const testNudgeMessage = "check deploy status"

// deliverSessionNudgeWithWorker delivers testNudgeMessage in plain-text
// output under a minted key (deliverSessionNudgeKeyed).
func deliverSessionNudgeWithWorker(target nudgeTarget, store beads.Store, sp runtime.Provider, mode nudgeDeliveryMode, stdout, stderr io.Writer) int {
	return deliverSessionNudgeKeyed(target, store, sp, testNudgeMessage, mode, "", false, stdout, stderr)
}

// sendMailNotifyWithWorker notifies target of mail messageID from the human
// sender, without the op id (sendMailNotifyWithOp).
func sendMailNotifyWithWorker(target nudgeTarget, store beads.Store, sp runtime.Provider, messageID string) error {
	_, err := sendMailNotifyWithOp(target, store, sp, "human", messageID)
	return err
}

// contentOpCity is a file-backed city with one running session on a fake
// runtime, for the nudge and mail notify ops. Its op rows live in h's
// opStore, which pins ids.
type contentOpCity struct {
	dir    string
	store  beads.Store
	fake   *runtime.Fake
	target nudgeTarget
	h      *cliOpHarness
}

// newContentOpCity builds the city with the ops.content gate at mode (""
// leaves it off), its session on the codex provider, which takes no live
// wait-idle delivery: a wait-idle delivery queues.
func newContentOpCity(t *testing.T, mode string) *contentOpCity {
	t.Helper()
	return newContentOpCityOn(t, mode, "codex")
}

// newContentOpCityOn is newContentOpCity with its session on provider. On
// claude the fake is set to report an idle boundary, so a wait-idle delivery
// (a mail notice included) is delivered live.
func newContentOpCityOn(t *testing.T, mode, provider string) *contentOpCity {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	env := map[string]string{}
	if mode != "" {
		env["GC_OPS_CONTENT"] = mode
	}
	h := newCLIOpHarness(t, env)
	h.opStore = &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: provider, WorkDir: dir, Provider: provider, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if provider == "claude" {
		fake.WaitForIdleErrors[info.SessionName] = nil
	}
	prevManaged, prevPoke := nudgeCityUsesManagedReconciler, nudgePokeController
	nudgeCityUsesManagedReconciler = func(string) bool { return false }
	nudgePokeController = func(string, reconcilekey.Key) error { return nil }
	prevWarn := nudgeWarningWriter
	nudgeWarningWriter = io.Discard
	t.Cleanup(func() {
		nudgeCityUsesManagedReconciler, nudgePokeController = prevManaged, prevPoke
		nudgeWarningWriter = prevWarn
	})
	return &contentOpCity{
		dir: dir, store: store, fake: fake, h: h,
		target: nudgeTarget{
			cityPath:    dir,
			cfg:         &config.City{Agents: []config.Agent{{Name: "worker", Provider: provider}}},
			sessionID:   info.ID,
			sessionName: info.SessionName,
			identity:    "worker",
			agent:       config.Agent{Name: "worker", Provider: provider},
			resolved:    &config.ResolvedProvider{Name: provider},
		},
	}
}

// nudge runs gc session nudge's delivery under key.
func (c *contentOpCity) nudge(mode nudgeDeliveryMode, key string, asJSON bool) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeKeyed(c.target, c.store, c.fake, testNudgeMessage, mode, key, asJSON, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// delivered lists the runtime deliveries the fake saw: the text a session
// received, with its structured content and delivery key.
func (c *contentOpCity) delivered() []runtime.Call {
	var out []runtime.Call
	for _, call := range c.fake.Calls {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			out = append(out, runtime.Call{Method: call.Method, Message: call.Message, Content: call.Content, DeliveryKey: call.DeliveryKey})
		}
	}
	return out
}

// queuedTexts lists the texts of the nudges queued for the target.
func (c *contentOpCity) queuedTexts(t *testing.T) []string {
	t.Helper()
	pending, _, _, err := listQueuedNudgesForTarget(c.dir, c.target, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("listing queued nudges: %v", err)
	}
	var texts []string
	for _, item := range pending {
		texts = append(texts, item.Source+": "+item.Message)
	}
	return texts
}

// queued counts the nudges queued for the target.
func (c *contentOpCity) queued(t *testing.T) int {
	t.Helper()
	return len(c.queuedTexts(t))
}

// contentOpValidate fails unless out, one JSON line, validates against the
// built-in schema at path.
func contentOpValidate(t *testing.T, path, out string) {
	t.Helper()
	raw, err := gascity.BuiltinSchemas.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var doc any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("decoding JSON %q: %v", out, err)
	}
	if err := compileJSONSchema(t, "gc://"+path, raw).Validate(doc); err != nil {
		t.Fatalf("%s does not match %s: %v", out, path, err)
	}
}

func TestSessionNudgeOpPolicy(t *testing.T) {
	for mode, want := range map[nudgeDeliveryMode]opjournal.MessagePolicy{
		nudgeDeliveryImmediate: {When: opjournal.WhenSteer, Wake: opjournal.WakeIfNeeded, Role: opjournal.RoleNotice, OnUnsupported: opjournal.FallbackBoundary},
		nudgeDeliveryWaitIdle:  {When: opjournal.WhenBoundary, Wake: opjournal.WakeIfNeeded, Role: opjournal.RoleNotice},
		nudgeDeliveryQueue:     {When: opjournal.WhenBoundary, Wake: opjournal.WakeIfNeeded, Role: opjournal.RoleNotice},
	} {
		if got := sessionNudgeOpPolicy(mode).Message; got != want {
			t.Errorf("sessionNudgeOpPolicy(%s) = %+v, want %+v", mode, got, want)
		}
	}
}

// A recorded nudge is one message{role: notice} op, proposed before the
// delivery and closed from it: delivered, or queued.
func TestSessionNudgeOpRecordsTheDelivery(t *testing.T) {
	for _, tc := range []struct {
		mode    nudgeDeliveryMode
		when    opjournal.When
		via     string
		outcome string
	}{
		{nudgeDeliveryImmediate, opjournal.WhenSteer, legacyop.ViaLegacyDelivered, "delivered"},
		// Codex takes no live wait-idle delivery, so a wait-idle nudge queues.
		{nudgeDeliveryWaitIdle, opjournal.WhenBoundary, legacyop.ViaLegacyQueued, "queued"},
		{nudgeDeliveryQueue, opjournal.WhenBoundary, legacyop.ViaLegacyQueued, "queued"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			c := newContentOpCity(t, "auto")
			proposedBeforeDelivery := false
			c.h.onOpen = func() { proposedBeforeDelivery = len(c.delivered()) == 0 && c.queued(t) == 0 }
			code, stdout, stderr := c.nudge(tc.mode, "", true)
			if code != 0 {
				t.Fatalf("nudge = %d, want 0; stderr=%s", code, stderr)
			}
			if !proposedBeforeDelivery {
				t.Fatal("the op store opened after the delivery: the op must be proposed before it")
			}
			op := cliOpOnly(t, c.h.opStore)
			cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", tc.via)
			if op.Kind != opjournal.KindMessage || op.Conversation != c.target.sessionID || op.Payload != testNudgeMessage {
				t.Fatalf("op = %+v, want a message op for %s carrying the nudge text", op, c.target.sessionID)
			}
			if op.Policy.Message.When != tc.when || op.Policy.Message.Role != opjournal.RoleNotice {
				t.Fatalf("op policy = %+v, want when %s, role notice", op.Policy.Message, tc.when)
			}
			if op.Producer.Principal != legacyop.PrincipalOperator {
				t.Fatalf("principal = %q, want operator outside an agent session", op.Producer.Principal)
			}
			contentOpValidate(t, "schemas/session/nudge/result.schema.json", stdout)
			got := cliOpJSON(t, []byte(stdout))
			if got["op_id"] != op.ID || got["key"] != op.ClientKey || got["outcome"] != tc.outcome {
				t.Fatalf("JSON = %v, want op_id %s, key %s, outcome %s", got, op.ID, op.ClientKey, tc.outcome)
			}
			if _, ok := got["replayed"]; ok {
				t.Fatalf("JSON = %v, want no replayed on a first run", got)
			}
		})
	}
}

// Inside an agent session the nudge is the agent's: its principal is the
// caller's conversation.
func TestSessionNudgeOpAgentPrincipal(t *testing.T) {
	c := newContentOpCity(t, "auto")
	c.h.env["GC_SESSION_ID"] = "gc-caller"
	if code, _, stderr := c.nudge(nudgeDeliveryImmediate, "", false); code != 0 {
		t.Fatalf("nudge = %d; stderr=%s", code, stderr)
	}
	op := cliOpOnly(t, c.h.opStore)
	if op.Producer.Principal != "conversation:gc-caller" || op.Producer.Actor != opjournal.ActorAgent {
		t.Fatalf("producer = %+v, want conversation:gc-caller, agent", op.Producer)
	}
}

// A same-key rerun replays the first run's outcome and delivers nothing.
func TestSessionNudgeOpKeyReplaysWithoutDeliveringAgain(t *testing.T) {
	c := newContentOpCity(t, "auto")
	code, first, stderr := c.nudge(nudgeDeliveryImmediate, "k-nudge-1", false)
	if code != 0 {
		t.Fatalf("first nudge = %d; stderr=%s", code, stderr)
	}
	code, again, stderr := c.nudge(nudgeDeliveryImmediate, "k-nudge-1", false)
	if code != 0 {
		t.Fatalf("rerun = %d; stderr=%s", code, stderr)
	}
	if n := len(c.delivered()); n != 1 {
		t.Fatalf("deliveries = %d, want 1: the rerun replays", n)
	}
	op := cliOpOnly(t, c.h.opStore)
	if again != first || !strings.Contains(stderr, "replayed op "+op.ID) {
		t.Fatalf("rerun stdout = %q (first %q), stderr = %q; want the first success line and the replay note", again, first, stderr)
	}
	code, out, _ := c.nudge(nudgeDeliveryImmediate, "k-nudge-1", true)
	contentOpValidate(t, "schemas/session/nudge/result.schema.json", out)
	if got := cliOpJSON(t, []byte(out)); code != 0 || got["replayed"] != true || got["op_id"] != op.ID {
		t.Fatalf("JSON rerun = %d %v, want replayed op %s", code, got, op.ID)
	}
}

// A queued nudge's replay reports the queue, as the first run did.
func TestSessionNudgeOpReplaysAQueuedNudgeAsQueued(t *testing.T) {
	c := newContentOpCity(t, "auto")
	if code, _, stderr := c.nudge(nudgeDeliveryQueue, "k-q", false); code != 0 {
		t.Fatalf("nudge = %d; stderr=%s", code, stderr)
	}
	code, out, stderr := c.nudge(nudgeDeliveryQueue, "k-q", true)
	if code != 0 {
		t.Fatalf("rerun = %d; stderr=%s", code, stderr)
	}
	if got := cliOpJSON(t, []byte(out)); got["outcome"] != "queued" || got["queued"] != true || got["replayed"] != true {
		t.Fatalf("JSON = %v, want a replayed queued outcome", got)
	}
	if n := c.queued(t); n != 1 {
		t.Fatalf("queued nudges = %d, want 1: the rerun replays", n)
	}
}

// An immediate nudge refused behind a permission prompt closes
// failed{refused:pending_interaction}, and types nothing.
func TestSessionNudgeOpRefusedBehindAPendingPrompt(t *testing.T) {
	c := newContentOpCity(t, "auto")
	c.fake.SetPendingInteraction(c.target.sessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "Run it?"})
	code, _, stderr := c.nudge(nudgeDeliveryImmediate, "", false)
	if code != 1 {
		t.Fatalf("nudge = %d, want 1; stderr=%s", code, stderr)
	}
	op := cliOpOnly(t, c.h.opStore)
	cliOpWantOutcome(t, op, opjournal.ArmFailed, opjournal.RefusedReasonPrefix+cliOpRefusedPendingInteraction, "")
	if !strings.Contains(stderr, "waiting on a permission prompt") || !strings.Contains(stderr, op.ID) {
		t.Fatalf("stderr = %q, want the refusal and the op", stderr)
	}
	if n := len(c.delivered()); n != 0 {
		t.Fatalf("deliveries = %d, want none behind the prompt", n)
	}
}

// Off records nothing; under require a store that cannot record refuses the
// nudge before any effect.
func TestSessionNudgeOpOffAndRequire(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		c := newContentOpCity(t, "off")
		code, stdout, stderr := c.nudge(nudgeDeliveryImmediate, "", true)
		if code != 0 || stderr != "" {
			t.Fatalf("nudge = %d, stderr %q; want 0 and nothing on stderr", code, stderr)
		}
		if got := cliOpJSON(t, []byte(stdout)); got["op_id"] != nil || got["key"] != nil {
			t.Fatalf("JSON = %v, want no op fields with the gate off", got)
		}
		if n := len(cliOpRows(t, c.h.opStore)); n != 0 || c.h.opens != 0 {
			t.Fatalf("op rows = %d, store opens = %d; want neither with the gate off", n, c.h.opens)
		}
	})
	t.Run("require", func(t *testing.T) {
		c := newContentOpCity(t, "require")
		c.h.opStore = cliOpIncapableStore{c.h.opStore}
		code, stdout, stderr := c.nudge(nudgeDeliveryImmediate, "", true)
		if code != 1 {
			t.Fatalf("nudge = %d, want 1; stderr=%s", code, stderr)
		}
		if got := cliOpFailureCode(t, []byte(stdout)); got != cliOpCodeUnavailable {
			t.Fatalf("failure code = %q, want %s", got, cliOpCodeUnavailable)
		}
		if n := len(c.delivered()); n != 0 || c.queued(t) != 0 {
			t.Fatalf("deliveries = %d, queued = %d; want no effect", n, c.queued(t))
		}
	})
}

// I11: the text a session receives is the off text, byte for byte, in every
// ops.content mode, and no op id or key reaches the runtime.
func TestSessionNudgeAndMailNotifyDeliveredTextIsUnchangedByTheGate(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			deliveredTextIsUnchangedByTheGate(t, provider)
		})
	}
}

// deliveredTextIsUnchangedByTheGate is I11 on a session on provider: on
// codex the wait-idle nudge and the mail notice queue, on claude they are
// delivered live.
func deliveredTextIsUnchangedByTheGate(t *testing.T, provider string) {
	type run struct {
		calls  []runtime.Call
		queued []string
		ids    []string
	}
	runAll := func(mode string) run {
		c := newContentOpCityOn(t, mode, provider)
		if code, _, stderr := c.nudge(nudgeDeliveryImmediate, "k-text", false); code != 0 {
			t.Fatalf("%s nudge = %d; stderr=%s", mode, code, stderr)
		}
		if code, _, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-text-2", false); code != 0 {
			t.Fatalf("%s wait-idle nudge = %d; stderr=%s", mode, code, stderr)
		}
		if _, err := sendMailNotifyWithOp(c.target, c.store, c.fake, "mayor", "gc-mail-7"); err != nil {
			t.Fatalf("%s mail notify: %v", mode, err)
		}
		r := run{calls: c.delivered(), queued: c.queuedTexts(t)}
		for _, op := range cliOpRows(t, c.h.opStore) {
			r.ids = append(r.ids, op.ID, op.ClientKey)
		}
		return r
	}
	off := runAll("off")
	if len(off.calls)+len(off.queued) != 3 {
		t.Fatalf("off deliveries = %+v, queued = %v; want 3 between them", off.calls, off.queued)
	}
	if wantLive := map[string]int{"codex": 1, "claude": 3}[provider]; len(off.calls) != wantLive {
		t.Fatalf("off live deliveries = %+v, want %d on %s", off.calls, wantLive, provider)
	}
	for _, mode := range []string{"auto", "require"} {
		got := runAll(mode)
		if len(got.ids) != 6 {
			t.Fatalf("%s op rows = %d, want 3 recorded", mode, len(got.ids)/2)
		}
		offJSON, _ := json.Marshal([]any{off.calls, off.queued})
		gotJSON, _ := json.Marshal([]any{got.calls, got.queued})
		if !bytes.Equal(offJSON, gotJSON) {
			t.Fatalf("%s deliveries = %s, want the off deliveries %s", mode, gotJSON, offJSON)
		}
		for _, id := range got.ids {
			if strings.Contains(string(gotJSON), id) {
				t.Fatalf("%s: delivered text carries %q", mode, id)
			}
		}
	}
}

// A mail notify is cause-keyed: key mail:<message id>, causation the message
// id; a notify the op already closed is not delivered again.
func TestMailNotifyOpIsCauseKeyed(t *testing.T) {
	// On claude the notice is delivered live, so a replay that delivered
	// again would show as a second delivery (the file queue would fold a
	// second queued notice for the same message into the first).
	c := newContentOpCityOn(t, "auto", "claude")
	id, err := sendMailNotifyWithOp(c.target, c.store, c.fake, "mayor", "gc-mail-1")
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	op := cliOpOnly(t, c.h.opStore)
	cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", legacyop.ViaLegacyDelivered)
	if n := len(c.delivered()); n != 1 {
		t.Fatalf("live deliveries = %d, want the notice delivered once", n)
	}
	if id != op.ID || op.ClientKey != "mail:gc-mail-1" || op.Causation != "gc-mail-1" || op.Payload != "You have mail from mayor" {
		t.Fatalf("op = %+v (returned id %q), want key mail:gc-mail-1, causation gc-mail-1, the notice text", op, id)
	}
	if op.Policy.Message.When != opjournal.WhenBoundary || op.Policy.Message.Role != opjournal.RoleNotice {
		t.Fatalf("policy = %+v, want a boundary notice", op.Policy.Message)
	}
	again, err := sendMailNotifyWithOp(c.target, c.store, c.fake, "mayor", "gc-mail-1")
	if err != nil || again != op.ID {
		t.Fatalf("second notify = %q, %v; want the replayed op %s", again, err, op.ID)
	}
	if n, q := len(c.delivered()), c.queued(t); n != 1 || q != 0 {
		t.Fatalf("deliveries = %d, queued notices = %d; want 1 and 0: the same message notifies once", n, q)
	}
}

// A queued notice's op closes succeeded{via: legacy.queued} at enqueue, and
// its replay queues nothing more.
func TestMailNotifyOpQueuedNoticeReplays(t *testing.T) {
	c := newContentOpCity(t, "auto")
	if _, err := sendMailNotifyWithOp(c.target, c.store, c.fake, "mayor", "gc-mail-q"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	cliOpWantOutcome(t, cliOpOnly(t, c.h.opStore), opjournal.ArmSucceeded, "", legacyop.ViaLegacyQueued)
	if _, err := sendMailNotifyWithOp(c.target, c.store, c.fake, "mayor", "gc-mail-q"); err != nil {
		t.Fatalf("second notify: %v", err)
	}
	if n, q := len(c.delivered()), c.queued(t); n != 0 || q != 1 {
		t.Fatalf("deliveries = %d, queued = %d; want 0 and 1", n, q)
	}
}

// Under require a notice the journal cannot record is an error, and nothing
// is delivered or queued.
func TestMailNotifyOpRequireRefusalIsAnError(t *testing.T) {
	c := newContentOpCityOn(t, "require", "claude")
	c.h.opStore = cliOpIncapableStore{c.h.opStore}
	id, err := sendMailNotifyWithOp(c.target, c.store, c.fake, "mayor", "gc-mail-r")
	if err == nil || !strings.Contains(err.Error(), "refused the notice for gc-mail-r") || id != "" {
		t.Fatalf("notify = %q, %v; want the refusal as an error", id, err)
	}
	if n, q := len(c.delivered()), c.queued(t); n != 0 || q != 0 {
		t.Fatalf("deliveries = %d, queued = %d; want no effect", n, q)
	}
}

// A broadcast records one notify op per recipient, and --json names them; a
// dedup-suppressed send proposes nothing.
func TestMailSendNotifyOpsPerRecipient(t *testing.T) {
	c := newContentOpCity(t, "auto")
	mp := beadmail.New(beads.NewMemStore())
	nf := func(_, messageID string) (string, error) {
		return sendMailNotifyWithOp(c.target, c.store, c.fake, "human", messageID)
	}
	recipients := map[string]bool{"human": true, "a": true, "b": true}
	var stdout, stderr bytes.Buffer
	if code := doMailSendAllJSON(mp, events.Discard, recipients, "human", []string{"hello"}, nf, true, &stdout, &stderr); code != 0 {
		t.Fatalf("send --all = %d; stderr=%s", code, stderr.String())
	}
	contentOpValidate(t, "schemas/mail/send/result.schema.json", stdout.String())
	var got mailActionResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout.String(), err)
	}
	ops := cliOpRows(t, c.h.opStore)
	if len(ops) != 2 || len(got.NotifyOpIDs) != 2 {
		t.Fatalf("ops = %d, notify_op_ids = %v; want one per recipient", len(ops), got.NotifyOpIDs)
	}
	for i, m := range got.Messages {
		if ops[i].ClientKey != "mail:"+m.ID && ops[1-i].ClientKey != "mail:"+m.ID {
			t.Fatalf("no notify op keyed by message %s among %+v", m.ID, ops)
		}
	}

	before := len(cliOpRows(t, c.h.opStore))
	args := []string{"a", "subject", "body"}
	for i := 0; i < 2; i++ {
		stdout.Reset()
		if code := doMailSendJSON(mp, events.Discard, recipients, "human", args, nf, "dedup-1", false, &stdout, &stderr); code != 0 {
			t.Fatalf("dedup send %d = %d; stderr=%s", i, code, stderr.String())
		}
	}
	if n := len(cliOpRows(t, c.h.opStore)) - before; n != 1 {
		t.Fatalf("notify ops for two dedup sends = %d, want 1: the suppressed send proposes nothing", n)
	}
}

// submitOpCity is the OJ-3 named-session city under ops.content, with the
// CLI's API client pointed at srv (nil: no API).
func submitOpCity(t *testing.T, mode string, srv *httptest.Server) (*cliOpHarness, *cliOpCity) {
	t.Helper()
	env := map[string]string{}
	if mode != "" {
		env["GC_OPS_CONTENT"] = mode
	}
	h := newCLIOpHarness(t, env)
	c := newCLIOpCity(t, h, nil)
	oldIdentity, oldClient, oldGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{} }
	apiRouteSupervisorClientHook = func(string) *api.Client {
		if srv == nil {
			return nil
		}
		return api.NewCityScopedClient(srv.URL, filepath.Base(c.dir))
	}
	apiRouteSupervisorGrantsHook = func() bool { return false }
	t.Cleanup(func() {
		apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook = oldIdentity, oldClient, oldGrants
	})
	return h, c
}

// testSubmitText is the message the submit tests send.
const testSubmitText = "status update please"

// submitDeliveries counts the runtime deliveries of testSubmitText.
func submitDeliveries(sp *runtime.Fake) int {
	n := 0
	for _, call := range sp.Calls {
		if (call.Method == "Nudge" || call.Method == "NudgeNow") && strings.Contains(call.Message, testSubmitText) {
			n++
		}
	}
	return n
}

// proposeAsAPI records the submit as the API's admitMessageOp records it, on
// the op store h opens, and closes it with outcome when one is given.
func proposeAsAPI(t *testing.T, h *cliOpHarness, conv, key, text string, intent session.SubmitIntent, outcome *opjournal.Outcome) opjournal.Op {
	t.Helper()
	adm := admitAsAPI(t, h, conv, key, text, intent)
	if outcome == nil {
		return adm.Op
	}
	return adm.Finish(legacyop.Result{Outcome: *outcome}).Op
}

// admitAsAPI admits the submit as the API's admitMessageOp admits it, on the
// op store h opens, and returns the admission of its open op.
func admitAsAPI(t *testing.T, h *cliOpHarness, conv, key, text string, intent session.SubmitIntent) *legacyop.Admission {
	t.Helper()
	st, err := opjournal.NewStore(beads.SessionStore{Store: h.opStore}, &clock.Fake{Time: cliOpT0}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("opening op store: %v", err)
	}
	rec := &legacyop.Recorder{Mode: rollout.Auto, Executor: legacyop.ExecutorAPI, Open: func() (*opjournal.Store, error) { return st, nil }, Clock: h.clk, Inflight: legacyop.NewInflight()}
	adm, err := rec.Admit(legacyop.Request{
		Conversation: conv, Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator,
		Key: key, Payload: text, Kind: opjournal.KindMessage, Policy: api.MessageOpPolicy(intent),
	})
	if err != nil || !adm.Recorded {
		t.Fatalf("API admit: recorded=%v err=%v", adm != nil && adm.Recorded, err)
	}
	return adm
}

// closingOnReadStore is an op store that runs beforeRead once, just before it
// serves the nth read of op id: a writer that lands between two reads.
type closingOnReadStore struct {
	*beads.MemStore
	id         string
	nth        int
	reads      int
	beforeRead func()
}

// Get serves a read, first running beforeRead at the nth read of id.
func (s *closingOnReadStore) Get(id string) (beads.Bead, error) {
	if id == s.id {
		s.reads++
		if s.reads == s.nth && s.beforeRead != nil {
			before := s.beforeRead
			s.beforeRead = nil
			before()
		}
	}
	return s.MemStore.Get(id)
}

// lostAnswerServer accepts a submit, records it as the API would through
// accept, and then drops the connection before answering, as a transport
// failure after the commit does.
func lostAnswerServer(t *testing.T, accept func(key string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/submit") {
			http.NotFound(w, r)
			return
		}
		accept(r.Header.Get("Idempotency-Key"))
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("response writer cannot hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close() //nolint:errcheck // dropping the answer is the point
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The fallback dedupe matrix (DES-O0API §15 OJ-5): the local path proposes
// the op id the API records, so a message the API accepted is never
// delivered again by the fallback.
func TestSessionSubmitFallbackDedupe(t *testing.T) {
	const text = testSubmitText
	t.Run("api never reached: local proposes and delivers", func(t *testing.T) {
		h, c := submitOpCity(t, "auto", nil)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentDefault, "", true, &stdout, &stderr); code != 0 {
			t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
		}
		op := cliOpOnly(t, h.opStore)
		cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", legacyop.ViaLegacyDelivered)
		if op.Producer.Principal != legacyop.PrincipalOperator || op.Payload != text {
			t.Fatalf("op = %+v, want the operator's message", op)
		}
		if n := submitDeliveries(c.sp); n != 1 {
			t.Fatalf("deliveries = %d, want 1", n)
		}
		contentOpValidate(t, "schemas/session/submit/result.schema.json", stdout.String())
		if got := cliOpJSON(t, stdout.Bytes()); got["op_id"] != op.ID || got["key"] != op.ClientKey {
			t.Fatalf("JSON = %v, want op %s and its key", got, op.ID)
		}
	})
	t.Run("api accepted, answer lost, op open: reports the API's op", func(t *testing.T) {
		var h *cliOpHarness
		var c *cliOpCity
		var sent string
		var apiOp opjournal.Op
		srv := lostAnswerServer(t, func(key string) {
			sent = key
			apiOp = proposeAsAPI(t, h, c.bead.ID, key, text, session.SubmitIntentDefault, nil)
		})
		h, c = submitOpCity(t, "auto", srv)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentDefault, "", false, &stdout, &stderr); code != 0 {
			t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
		}
		if !strings.HasPrefix(sent, "k-") {
			t.Fatalf("Idempotency-Key = %q, want the minted key", sent)
		}
		if n := submitDeliveries(c.sp); n != 0 {
			t.Fatalf("deliveries = %d, want none: the API accepted the message", n)
		}
		if !strings.Contains(stdout.String(), "Accepted by the controller") || !strings.Contains(stdout.String(), apiOp.ID) || !strings.Contains(stdout.String(), "--key "+sent) {
			t.Fatalf("stdout = %q, want the API's op %s and the key %s", stdout.String(), apiOp.ID, sent)
		}
		if ops := cliOpRows(t, h.opStore); len(ops) != 1 || ops[0].Executor != opjournal.ExecutorLegacyAPI || ops[0].Status == opjournal.StatusClosed {
			t.Fatalf("ops = %+v, want only the API's open op", ops)
		}
		// A same-key rerun under --json, whose API answer is lost too, finds
		// the API's op still open: accepted, with the op and its key.
		stdout.Reset()
		if code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentDefault, sent, true, &stdout, &stderr); code != 0 {
			t.Fatalf("JSON rerun = %d; stderr=%s", code, stderr.String())
		}
		contentOpValidate(t, "schemas/session/submit/result.schema.json", stdout.String())
		if got := cliOpJSON(t, stdout.Bytes()); got["outcome"] != sessionSubmitAcceptedOutcome || got["op_id"] != apiOp.ID || got["key"] != sent {
			t.Fatalf("JSON = %v, want accepted with op %s and key %s", got, apiOp.ID, sent)
		}
		if n := submitDeliveries(c.sp); n != 0 {
			t.Fatalf("deliveries = %d, want none", n)
		}
	})
	t.Run("api closed its op between the fallback's two reads: replays the outcome", func(t *testing.T) {
		var h *cliOpHarness
		var c *cliOpCity
		var store *closingOnReadStore
		closed := false
		srv := lostAnswerServer(t, func(key string) {
			adm := admitAsAPI(t, h, c.bead.ID, key, text, session.SubmitIntentDefault)
			// Admit reads the op open; the API closes it before the
			// fallback reads it again to report it accepted.
			store.id, store.nth = adm.Op.ID, 2
			store.beforeRead = func() {
				adm.Finish(legacyop.Result{Outcome: opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: legacyop.ViaLegacyDelivered}}})
				closed = true
			}
		})
		h, c = submitOpCity(t, "auto", srv)
		store = &closingOnReadStore{MemStore: h.opStore.(*beads.MemStore)}
		h.opStore = store
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentDefault, "", false, &stdout, &stderr); code != 0 {
			t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
		}
		if !closed {
			t.Fatalf("the API's op was read %d times after its admission, so it never closed between the fallback's reads", store.reads)
		}
		if n := submitDeliveries(c.sp); n != 0 {
			t.Fatalf("deliveries = %d, want none: the API delivered it", n)
		}
		if !strings.Contains(stdout.String(), "Submitted to "+c.bead.ID) || !strings.Contains(stderr.String(), "replayed op") {
			t.Fatalf("stdout = %q, stderr = %q; want the replayed outcome", stdout.String(), stderr.String())
		}
	})
	t.Run("api accepted and closed, answer lost: replays the outcome", func(t *testing.T) {
		var h *cliOpHarness
		var c *cliOpCity
		queued := opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: legacyop.ViaLegacyQueued}}
		srv := lostAnswerServer(t, func(key string) {
			proposeAsAPI(t, h, c.bead.ID, key, text, session.SubmitIntentFollowUp, &queued)
		})
		h, c = submitOpCity(t, "auto", srv)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentFollowUp, "k-submit-1", false, &stdout, &stderr); code != 0 {
			t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
		}
		if n := submitDeliveries(c.sp); n != 0 {
			t.Fatalf("deliveries = %d, want none: the API delivered it", n)
		}
		if !strings.Contains(stdout.String(), "Queued follow-up for "+c.bead.ID) || !strings.Contains(stderr.String(), "replayed op") {
			t.Fatalf("stdout = %q, stderr = %q; want the replayed queued outcome", stdout.String(), stderr.String())
		}
	})
	t.Run("local op in progress: no second delivery", func(t *testing.T) {
		h, c := submitOpCity(t, "auto", nil)
		st, err := opjournal.NewStore(beads.SessionStore{Store: h.opStore}, h.clk, opjournal.WithIDPrefix("gc"))
		if err != nil {
			t.Fatalf("opening op store: %v", err)
		}
		if _, created, err := st.Propose(opjournal.Request{
			Conversation: c.bead.ID, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLegacyCLI, ClientKey: "k-busy",
			Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
			Payload:  text, Policy: api.MessageOpPolicy(session.SubmitIntentDefault),
			StartBy: cliOpT0.Add(legacyop.ExecutionBound), FinishBy: cliOpT0.Add(legacyop.ExecutionBound),
		}); err != nil || !created {
			t.Fatalf("proposing the open op: created=%t err=%v", created, err)
		}
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentDefault, "k-busy", false, &stdout, &stderr); code != 1 {
			t.Fatalf("submit = %d, want 1; stderr=%s", code, stderr.String())
		}
		if n := submitDeliveries(c.sp); n != 0 || !strings.Contains(stderr.String(), "in progress") {
			t.Fatalf("deliveries = %d, stderr = %q; want none and op_in_progress", n, stderr.String())
		}
	})
}

// proposeAsAPILane proposes the submit as the API's acceptMessageLane does
// when its lane host executes message, on the op store h opens, and closes
// it with outcome, as the lane host would, when one is given.
func proposeAsAPILane(t *testing.T, h *cliOpHarness, conv, key, text string, intent session.SubmitIntent, outcome *opjournal.Outcome) opjournal.Op {
	t.Helper()
	st, err := opjournal.NewStore(beads.SessionStore{Store: h.opStore}, &clock.Fake{Time: cliOpT0}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("opening op store: %v", err)
	}
	op, created, err := st.Propose(opjournal.Request{
		Conversation: conv, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane, ClientKey: key,
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  text, Policy: api.LaneMessageOpPolicy(intent),
	})
	if err != nil || !created {
		t.Fatalf("proposing the API's lane op: created=%t err=%v", created, err)
	}
	if outcome == nil {
		return op
	}
	closed, err := st.Close(op.ID, *outcome, "lane:ctl")
	if err != nil {
		t.Fatalf("closing the API's lane op: %v", err)
	}
	return closed
}

// When the API's lane host executes message, the API proposes the submit as
// a lane op under its LaneMessageOpPolicy, which records follow_up and
// interrupt_now differently from the CLI's own op. A fallback after a lost
// answer still finds it under the key, for every intent: open, it reports the
// API accepted the message; closed, it replays the outcome; either way it
// delivers nothing. Another message under the key is still refused.
func TestSessionSubmitFallbackAnswersFromTheAPIsLaneOp(t *testing.T) {
	const text = testSubmitText
	for _, intent := range []session.SubmitIntent{session.SubmitIntentDefault, session.SubmitIntentFollowUp, session.SubmitIntentInterruptNow} {
		t.Run("open "+string(intent)+": reports the API's lane op", func(t *testing.T) {
			var h *cliOpHarness
			var c *cliOpCity
			var sent string
			var laneOp opjournal.Op
			srv := lostAnswerServer(t, func(key string) {
				sent = key
				laneOp = proposeAsAPILane(t, h, c.bead.ID, key, text, intent, nil)
			})
			h, c = submitOpCity(t, "auto", srv)
			var stdout, stderr bytes.Buffer
			if code := cmdSessionSubmit([]string{c.bead.ID, text}, intent, "", false, &stdout, &stderr); code != 0 {
				t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
			}
			if n := submitDeliveries(c.sp); n != 0 {
				t.Fatalf("deliveries = %d, want none: the API's lane host delivers it", n)
			}
			if !strings.Contains(stdout.String(), "Accepted by the controller") || !strings.Contains(stdout.String(), laneOp.ID) || !strings.Contains(stdout.String(), "--key "+sent) {
				t.Fatalf("stdout = %q, want the lane op %s and the key %s", stdout.String(), laneOp.ID, sent)
			}
			if ops := cliOpRows(t, h.opStore); len(ops) != 1 || ops[0].Executor != opjournal.ExecutorLane || ops[0].Status == opjournal.StatusClosed {
				t.Fatalf("ops = %+v, want only the API's open lane op", ops)
			}
		})
		t.Run("closed "+string(intent)+": replays the outcome", func(t *testing.T) {
			var h *cliOpHarness
			var c *cliOpCity
			srv := lostAnswerServer(t, func(key string) {
				proposeAsAPILane(t, h, c.bead.ID, key, text, intent, &opjournal.Outcome{Arm: opjournal.ArmSucceeded})
			})
			h, c = submitOpCity(t, "auto", srv)
			var stdout, stderr bytes.Buffer
			if code := cmdSessionSubmit([]string{c.bead.ID, text}, intent, "k-lane-"+string(intent), true, &stdout, &stderr); code != 0 {
				t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
			}
			if n := submitDeliveries(c.sp); n != 0 {
				t.Fatalf("deliveries = %d, want none: the lane host delivered it", n)
			}
			if got := cliOpJSON(t, stdout.Bytes()); got["replayed"] != true || got["queued"] != false || !strings.Contains(stderr.String(), "replayed op") {
				t.Fatalf("JSON = %v, stderr = %q; want the replayed outcome", got, stderr.String())
			}
			if ops := cliOpRows(t, h.opStore); len(ops) != 1 || ops[0].Executor != opjournal.ExecutorLane {
				t.Fatalf("ops = %+v, want only the API's lane op", ops)
			}
		})
	}
	t.Run("another message under the lane op's key is refused", func(t *testing.T) {
		h, c := submitOpCity(t, "auto", nil)
		proposeAsAPILane(t, h, c.bead.ID, "k-other", "another message", session.SubmitIntentFollowUp, nil)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentFollowUp, "k-other", false, &stdout, &stderr); code != 1 {
			t.Fatalf("submit = %d, want 1; stderr=%s", code, stderr.String())
		}
		if n := submitDeliveries(c.sp); n != 0 || !strings.Contains(stderr.String(), "different request") {
			t.Fatalf("deliveries = %d, stderr = %q; want none and the key refused", n, stderr.String())
		}
	})
}

// gc session submit sends resume: true, and the API consumes the session's
// operator hold only once it has proposed the lane op, so a request that
// died between the two leaves the op open behind the hold. A fallback that
// finds that op open consumes the hold itself, recording the session's wake,
// before it reports the API's acceptance; a resume the session refuses (its
// drain is stopping it) is reported, exit 1, the op left open. A suspend
// stamped after the op's proposal is one the operator placed while the
// message waits: it stands, and the fallback still reports the acceptance.
// Kills a fallback that reports the message accepted over a hold nothing
// consumed, and one that consumes a suspend placed after the op.
func TestSessionSubmitFallbackResumesTheHoldOfAnOpenLaneOp(t *testing.T) {
	const text = testSubmitText
	for _, tc := range []struct {
		name     string
		draining bool
		// suspended is when the operator suspended the session; the API
		// proposed the op at cliOpT0.
		suspended time.Time
	}{
		{name: "consumes the hold", suspended: cliOpT0.Add(-time.Minute)},
		{name: "a refused resume exits 1", draining: true, suspended: cliOpT0.Add(-time.Minute)},
		{name: "a suspend after the op stands", suspended: cliOpT0.Add(time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h *cliOpHarness
			var c *cliOpCity
			var laneOp opjournal.Op
			srv := lostAnswerServer(t, func(key string) {
				laneOp = proposeAsAPILane(t, h, c.bead.ID, key, text, session.SubmitIntentDefault, nil)
			})
			h, c = submitOpCity(t, "auto", srv)
			if _, err := sessionFrontDoor(c.reload(t)).OperatorSuspend(c.bead.ID, tc.suspended); err != nil {
				t.Fatalf("suspending the session: %v", err)
			}
			if tc.draining {
				if err := c.reload(t).SetMetadataBatch(c.bead.ID, map[string]string{"state": string(session.StateDraining)}); err != nil {
					t.Fatalf("draining the session: %v", err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentDefault, "", false, &stdout, &stderr)
			if n := submitDeliveries(c.sp); n != 0 {
				t.Fatalf("deliveries = %d, want none: the API's lane host delivers it", n)
			}
			if ops := cliOpRows(t, h.opStore); len(ops) != 1 || ops[0].Executor != opjournal.ExecutorLane || ops[0].Status == opjournal.StatusClosed {
				t.Fatalf("ops = %+v, want only the API's open lane op", ops)
			}
			meta := c.row(t).Metadata
			if tc.draining {
				if code != 1 || !strings.Contains(stderr.String(), "resuming "+c.bead.ID+" for op "+laneOp.ID) || meta["state"] != string(session.StateDraining) {
					t.Fatalf("submit = %d, stderr = %q, row state %q; want exit 1 naming the refused resume, the row left draining", code, stderr.String(), meta["state"])
				}
				return
			}
			if code != 0 || !strings.Contains(stdout.String(), "Accepted by the controller") || !strings.Contains(stdout.String(), laneOp.ID) {
				t.Fatalf("submit = %d, stdout = %q, stderr = %q; want the lane op %s accepted", code, stdout.String(), stderr.String(), laneOp.ID)
			}
			if tc.suspended.After(laneOp.CreatedAt) {
				if meta["state"] != string(session.StateSuspended) || meta["sleep_intent"] != string(session.SleepReasonUserHold) || meta["wake_request"] != "" {
					t.Fatalf("session row = %v; want the later suspend standing, no wake", meta)
				}
				return
			}
			if meta["state"] != string(session.StateAsleep) || meta["wake_request"] != string(session.WakeCauseExplicit) {
				t.Fatalf("session row = %v; want the hold consumed and its wake recorded", meta)
			}
		})
	}
}

// listMessageAuthoritative lists message in the city's [ops] authoritative,
// as a city whose lane host executes message does.
func listMessageAuthoritative(t *testing.T, dir string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "city.toml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening city.toml: %v", err)
	}
	defer f.Close() //nolint:errcheck // the write's error is checked
	if _, err := f.WriteString("\n[ops]\nauthoritative = [\"message\"]\n"); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

// The content gate's builtin default (no [ops] gate, no GC_OPS_CONTENT)
// records nothing on a sessions store that is not SQLite-backed, as this
// city's is not, but it does not govern the API's lane op. With message
// listed, a fallback after a lost answer opens the op store all the same: it
// answers from the lane op under the key it sent, open or closed, and
// delivers nothing; a key that names no op it claims with its own op before
// it delivers, so an API that commits the lane op later finds the key taken.
// With message unlisted the default stands: nothing opens or records.
func TestSessionSubmitFallbackUnderTheBuiltinDefaultAnswersFromTheAPIsLaneOp(t *testing.T) {
	const text = testSubmitText
	t.Run("open lane op: reports it", func(t *testing.T) {
		var h *cliOpHarness
		var c *cliOpCity
		var laneOp opjournal.Op
		srv := lostAnswerServer(t, func(key string) {
			laneOp = proposeAsAPILane(t, h, c.bead.ID, key, text, session.SubmitIntentFollowUp, nil)
		})
		h, c = submitOpCity(t, "", srv)
		listMessageAuthoritative(t, c.dir)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentFollowUp, "", false, &stdout, &stderr); code != 0 {
			t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
		}
		if n := submitDeliveries(c.sp); n != 0 {
			t.Fatalf("deliveries = %d, want none: the API's lane host delivers it", n)
		}
		if !strings.Contains(stdout.String(), "Accepted by the controller") || !strings.Contains(stdout.String(), laneOp.ID) {
			t.Fatalf("stdout = %q, want the lane op %s accepted", stdout.String(), laneOp.ID)
		}
		if ops := cliOpRows(t, h.opStore); len(ops) != 1 || ops[0].Executor != opjournal.ExecutorLane {
			t.Fatalf("ops = %+v, want only the API's lane op", ops)
		}
	})
	t.Run("closed lane op: replays it", func(t *testing.T) {
		var h *cliOpHarness
		var c *cliOpCity
		srv := lostAnswerServer(t, func(key string) {
			proposeAsAPILane(t, h, c.bead.ID, key, text, session.SubmitIntentDefault, &opjournal.Outcome{Arm: opjournal.ArmSucceeded})
		})
		h, c = submitOpCity(t, "", srv)
		listMessageAuthoritative(t, c.dir)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentDefault, "", true, &stdout, &stderr); code != 0 {
			t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
		}
		if n := submitDeliveries(c.sp); n != 0 {
			t.Fatalf("deliveries = %d, want none: the lane host delivered it", n)
		}
		if got := cliOpJSON(t, stdout.Bytes()); got["replayed"] != true || !strings.Contains(stderr.String(), "replayed op") {
			t.Fatalf("JSON = %v, stderr = %q; want the replayed outcome", got, stderr.String())
		}
	})
	t.Run("no op under the key: claims it and delivers once", func(t *testing.T) {
		var sent string
		srv := lostAnswerServer(t, func(key string) { sent = key })
		h, c := submitOpCity(t, "", srv)
		listMessageAuthoritative(t, c.dir)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentDefault, "", false, &stdout, &stderr); code != 0 {
			t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
		}
		if n := submitDeliveries(c.sp); n != 1 {
			t.Fatalf("deliveries = %d, want the local delivery", n)
		}
		op := cliOpOnly(t, h.opStore)
		if op.ClientKey != sent || op.Executor != opjournal.ExecutorLegacyCLI || op.Status != opjournal.StatusClosed {
			t.Fatalf("op = %+v, want the CLI's own closed op under the key %q the API was sent", op, sent)
		}
	})
	t.Run("message unlisted: the default stands", func(t *testing.T) {
		srv := lostAnswerServer(t, func(string) {})
		h, c := submitOpCity(t, "", srv)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, text}, session.SubmitIntentDefault, "", false, &stdout, &stderr); code != 0 {
			t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
		}
		if n := submitDeliveries(c.sp); n != 1 || h.opens != 0 || strings.Contains(stderr.String(), "op journal") {
			t.Fatalf("deliveries = %d, op store opens = %d, stderr = %q; want one delivery, no open and no op journal warning", n, h.opens, stderr.String())
		}
	})
}

// Through the API the CLI sends its key as the Idempotency-Key and reports
// the op the API recorded; with the gate off and no --key it sends none.
func TestSessionSubmitThroughTheAPISendsTheKey(t *testing.T) {
	serve := func(t *testing.T, gotKey *string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*gotKey = r.Header.Get("Idempotency-Key")
			replayed, via := true, legacyop.ViaLegacyDelivered
			body := map[string]any{
				"status": "accepted", "request_id": "gc-op-1", "event_cursor": "0", "replayed": replayed,
				"op": map[string]any{
					"id": "gc-op-1", "conversation": "gc-1", "kind": "message", "status": "closed",
					"outcome": map[string]any{"arm": "succeeded", "via": via},
				},
			}
			if *gotKey == "" {
				http.Error(w, "test server refuses this request", http.StatusTeapot)
				return
			}
			body["idempotency_key"] = *gotKey
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(body) //nolint:errcheck // test server
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	t.Run("key", func(t *testing.T) {
		var got string
		srv := serve(t, &got)
		_, c := submitOpCity(t, "auto", srv)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, "hi"}, session.SubmitIntentDefault, "k-api-1", true, &stdout, &stderr); code != 0 {
			t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
		}
		if got != "k-api-1" {
			t.Fatalf("Idempotency-Key = %q, want k-api-1", got)
		}
		contentOpValidate(t, "schemas/session/submit/result.schema.json", stdout.String())
		out := cliOpJSON(t, stdout.Bytes())
		if out["op_id"] != "gc-op-1" || out["key"] != "k-api-1" || out["replayed"] != true || !strings.Contains(stderr.String(), "replayed op gc-op-1") {
			t.Fatalf("JSON = %v, stderr = %q; want the API's replayed op", out, stderr.String())
		}
	})
	t.Run("off sends no key", func(t *testing.T) {
		var got string
		srv := serve(t, &got)
		h, c := submitOpCity(t, "off", srv)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, "hi"}, session.SubmitIntentDefault, "", false, &stdout, &stderr); code != 1 {
			t.Fatalf("submit = %d, want the test server's refusal; stderr=%s", code, stderr.String())
		}
		if got != "" || h.opens != 0 || strings.Contains(stderr.String(), "key") {
			t.Fatalf("Idempotency-Key = %q, opens = %d, stderr = %q; want no key, no op store, no key on stderr", got, h.opens, stderr.String())
		}
	})
}

// D12, §8.2: under require, a store that cannot record, and a request the
// journal refuses whatever its conversation (an invalid key, or a payload too
// large, not valid UTF-8 or empty), refuse before the target resolves, so a
// configured named session with no row gets none.
func TestMessageVerbsRequireRefuseBeforeMaterializing(t *testing.T) {
	oversized := strings.Repeat("x", opjournal.MaxInlinePayload+1)
	invalidUTF8 := "bad \xff text"
	invalidKey := strings.Repeat("k", 513)
	submit := func(text, key string) func(*testing.T) (int, string, string) {
		return func(t *testing.T) (int, string, string) {
			t.Setenv("GC_NO_API", "1")
			var stdout, stderr bytes.Buffer
			code := cmdSessionSubmit([]string{killPokeSessionIdentity, text}, session.SubmitIntentDefault, key, true, &stdout, &stderr)
			return code, stdout.String(), stderr.String()
		}
	}
	nudge := func(text, key string) func(*testing.T) (int, string, string) {
		return func(*testing.T) (int, string, string) {
			var stdout, stderr bytes.Buffer
			code := cmdSessionNudge([]string{killPokeSessionIdentity, text}, nudgeDeliveryImmediate, key, true, &stdout, &stderr)
			return code, stdout.String(), stderr.String()
		}
	}
	for _, tc := range []struct {
		name string
		// incapable: the op store cannot record; else only the request is
		// refused.
		incapable bool
		run       func(t *testing.T) (code int, stdout, stderr string)
		wantCode  string
		wantText  string
	}{
		{"submit store", true, submit("hello", ""), cliOpCodeUnavailable, "op journal cannot record this request"},
		{"submit oversized", false, submit(oversized, ""), cliOpCodeUnavailable, "payload too large"},
		{"submit invalid UTF-8", false, submit(invalidUTF8, ""), cliOpCodeUnavailable, "not valid UTF-8"},
		{"submit empty", false, submit("", ""), cliOpCodeUnavailable, "require a payload"},
		{"submit invalid key", false, submit("hello", invalidKey), cliOpCodeInvalidKey, "invalid --key"},
		{"nudge store", true, nudge("hello", ""), cliOpCodeUnavailable, "op journal cannot record this request"},
		{"nudge oversized", false, nudge(oversized, ""), cliOpCodeUnavailable, "payload too large"},
		{"nudge invalid UTF-8", false, nudge(invalidUTF8, ""), cliOpCodeUnavailable, "not valid UTF-8"},
		{"nudge empty", false, nudge("", ""), cliOpCodeUnavailable, "require a payload"},
		{"nudge invalid key", false, nudge("hello", invalidKey), cliOpCodeInvalidKey, "invalid --key"},
		{"mail notify store", true, func(t *testing.T) (int, string, string) {
			var warn bytes.Buffer
			prev := nudgeWarningWriter
			nudgeWarningWriter = &warn
			t.Cleanup(func() { nudgeWarningWriter = prev })
			_, err := newMailNudgeFunc("human")(killPokeSessionIdentity, "gc-mail-1")
			if err == nil || !strings.Contains(err.Error(), "refused the notice for gc-mail-1") {
				t.Fatalf("notify err = %v, want the refusal", err)
			}
			return 1, "", warn.String()
		}, "", "op journal cannot record this request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCLIOpHarness(t, map[string]string{"GC_OPS_CONTENT": "require"})
			dir := newCLIOpNamedCityWithoutRow(t)
			var opStore beads.Store = &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
			if tc.incapable {
				opStore = cliOpIncapableStore{opStore}
			}
			h.opStore = opStore
			code, stdout, stderr := tc.run(t)
			if code != 1 || !strings.Contains(stderr, tc.wantText) {
				t.Fatalf("code = %d, stderr = %q; want 1 and %q", code, stderr, tc.wantText)
			}
			if stdout != "" {
				if got := cliOpFailureCode(t, []byte(stdout)); got != tc.wantCode {
					t.Fatalf("failure code = %q, want %s", got, tc.wantCode)
				}
			}
			if n := cliOpSessionRows(t, dir); n != 0 {
				t.Fatalf("session rows = %d, want none: require refused, so nothing may be created", n)
			}
		})
	}
}

// The local submit records the unfenced_interrupt degradation of an
// interrupt_now the runtime accepted unfenced, as the API path records it on
// the same op id (TestSessionContentOpsUnfencedInterruptReachesTheOp).
func TestSessionSubmitLocalRecordsUnfencedInterrupt(t *testing.T) {
	h, c := submitOpCity(t, "auto", nil)
	c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	c.sp.TurnScripts = map[string][]string{c.name: {"t1", "t1", ""}}
	prev := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return runtime.FullFake{Fake: c.sp}, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = prev })
	var stdout, stderr bytes.Buffer
	if code := cmdSessionSubmit([]string{c.bead.ID, "now"}, session.SubmitIntentInterruptNow, "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
	}
	sent := false
	for _, call := range c.sp.Calls {
		sent = sent || (call.Method == "InterruptTurn" && call.Value == "sent")
	}
	if !sent {
		t.Fatalf("no unfenced InterruptTurn was sent; calls=%+v", c.sp.Calls)
	}
	op := cliOpOnly(t, h.opStore)
	if op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Degradation != session.DegradationUnfencedInterrupt {
		t.Fatalf("op outcome = %+v, want succeeded with degradation %s", op.Outcome, session.DegradationUnfencedInterrupt)
	}
}

// A local submit the session queues closes its op succeeded{via:
// legacy.queued}, and reports the queue.
func TestSessionSubmitLocalQueuedClosesQueued(t *testing.T) {
	h, c := submitOpCity(t, "auto", nil)
	// A session still being created, with no runtime yet, queues the
	// message for its first start.
	if err := c.store.SetMetadata(c.bead.ID, "state", string(session.StateCreating)); err != nil {
		t.Fatalf("marking the session creating: %v", err)
	}
	if err := c.sp.Stop(c.name); err != nil {
		t.Fatalf("stopping the fake runtime: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := cmdSessionSubmit([]string{c.bead.ID, testSubmitText}, session.SubmitIntentDefault, "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
	}
	if got := cliOpJSON(t, stdout.Bytes()); got["queued"] != true {
		t.Fatalf("JSON = %v, want a queued submit", got)
	}
	cliOpWantOutcome(t, cliOpOnly(t, h.opStore), opjournal.ArmSucceeded, "", legacyop.ViaLegacyQueued)
}

// D5: submit is an operator verb on both paths, so inside an agent session
// (GC_SESSION_ID set) the fallback still finds the op the API recorded under
// the operator, and does not deliver again.
func TestSessionSubmitFallbackInsideAnAgentSessionDedupes(t *testing.T) {
	var h *cliOpHarness
	var c *cliOpCity
	srv := lostAnswerServer(t, func(key string) {
		proposeAsAPI(t, h, c.bead.ID, key, testSubmitText, session.SubmitIntentDefault, nil)
	})
	h, c = submitOpCity(t, "auto", srv)
	h.env["GC_SESSION_ID"] = "gc-agent-1"
	var stdout, stderr bytes.Buffer
	if code := cmdSessionSubmit([]string{c.bead.ID, testSubmitText}, session.SubmitIntentDefault, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
	}
	if n := submitDeliveries(c.sp); n != 0 {
		t.Fatalf("deliveries = %d, want none: the API accepted the message under the operator", n)
	}
	if !strings.Contains(stdout.String(), "Accepted by the controller") {
		t.Fatalf("stdout = %q, want the API's op reported as accepted", stdout.String())
	}
}

// In auto, a run under --key whose request the journal rejects (a payload it
// cannot hold) runs unrecorded and says so: a same-key rerun acts again.
func TestSessionNudgeKeyNotRecordedWarns(t *testing.T) {
	for _, tc := range []struct {
		name, message, reason string
	}{
		{"oversized", strings.Repeat("x", opjournal.MaxInlinePayload+1), "too large"},
		{"invalid UTF-8", "bad \xff text", "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newContentOpCity(t, "auto")
			for i := 0; i < 2; i++ {
				var stdout, stderr bytes.Buffer
				if code := deliverSessionNudgeKeyed(c.target, c.store, c.fake, tc.message, nudgeDeliveryImmediate, "k-big", false, &stdout, &stderr); code != 0 {
					t.Fatalf("nudge %d = %d; stderr=%s", i, code, stderr.String())
				}
				if got := stderr.String(); !strings.Contains(got, "warning: --key not recorded") || !strings.Contains(got, tc.reason) || !strings.Contains(got, "a rerun acts again") {
					t.Fatalf("nudge %d stderr = %q, want the unrecorded --key warning naming %q", i, got, tc.reason)
				}
			}
			if n := len(cliOpRows(t, c.h.opStore)); n != 0 {
				t.Fatalf("op rows = %d, want none", n)
			}
			var stdout, stderr bytes.Buffer
			if code := deliverSessionNudgeKeyed(c.target, c.store, c.fake, tc.message, nudgeDeliveryImmediate, "", false, &stdout, &stderr); code != 0 || strings.Contains(stderr.String(), "--key") {
				t.Fatalf("nudge without --key = %d, stderr %q; want no --key warning", code, stderr.String())
			}
		})
	}
}

// An immediate nudge to an ACP session this process does not own closes
// failed{refused:acp_connection_not_owned}.
func TestSessionNudgeOpACPNotOwnedRefusal(t *testing.T) {
	c := newContentOpCity(t, "auto")
	c.target.transport = "acp"
	c.fake.NudgeErrors = map[string]error{c.target.sessionName: runtime.ErrSessionNotFound}
	code, _, stderr := c.nudge(nudgeDeliveryImmediate, "", false)
	if code != 1 || !strings.Contains(stderr, "does not own the ACP connection") {
		t.Fatalf("nudge = %d, stderr = %q; want the ACP refusal", code, stderr)
	}
	cliOpWantOutcome(t, cliOpOnly(t, c.h.opStore), opjournal.ArmFailed, opjournal.RefusedReasonPrefix+cliOpRefusedACPNotOwned, "")
}

// cliMessageOpResult maps every CLI message path's legacy result.
func TestCLIMessageOpResult(t *testing.T) {
	unfenced := &session.TurnInterruptReport{Degradation: session.DegradationUnfencedInterrupt}
	for _, tc := range []struct {
		name        string
		queued      bool
		err         error
		interrupt   *session.TurnInterruptReport
		arm         opjournal.Arm
		reason, via string
		degradation string
	}{
		{name: "delivered", arm: opjournal.ArmSucceeded, via: legacyop.ViaLegacyDelivered},
		{name: "queued", queued: true, arm: opjournal.ArmSucceeded, via: legacyop.ViaLegacyQueued},
		{name: "unfenced interrupt", interrupt: unfenced, arm: opjournal.ArmSucceeded, via: legacyop.ViaLegacyDelivered, degradation: session.DegradationUnfencedInterrupt},
		{name: "failed interrupt records no degradation", err: errors.New("x"), interrupt: unfenced, arm: opjournal.ArmFailed, reason: opjournal.ReasonOther},
		{name: "refusal", err: &cliMessageRefusal{why: cliOpRefusedPendingInteraction, err: errors.New("prompt")}, arm: opjournal.ArmFailed, reason: opjournal.RefusedReasonPrefix + cliOpRefusedPendingInteraction},
		{name: "unsupported", err: session.ErrTurnInterruptUnsupported, arm: opjournal.ArmFailed, reason: opjournal.ReasonUnsupported},
		{name: "closed", err: session.ErrSessionClosed, arm: opjournal.ArmSkipped, reason: opjournal.ReasonConversationClosed},
		{name: "session not found", err: session.ErrSessionNotFound, arm: opjournal.ArmFailed, reason: opjournal.ReasonSessionGone},
		{name: "runtime session not found", err: fmt.Errorf("nudging: %w", runtime.ErrSessionNotFound), arm: opjournal.ArmFailed, reason: opjournal.ReasonSessionGone},
		{name: "provider unavailable", err: runtime.ErrRuntimeUnavailable, arm: opjournal.ArmFailed, reason: opjournal.ReasonProviderUnavailable},
		{name: "timeout", err: context.DeadlineExceeded, arm: opjournal.ArmFailed, reason: opjournal.ReasonDeadlineInFlight},
		{name: "nudge queue busy", err: fmt.Errorf("gc session nudge: %w", nudgequeue.ErrQueueBusy), arm: opjournal.ArmFailed, reason: opjournal.RefusedReasonPrefix + "nudge_queue_busy"},
		{name: "other", err: errors.New("boom"), arm: opjournal.ArmFailed, reason: opjournal.ReasonOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cliMessageOpResult(tc.queued, tc.err, tc.interrupt).Outcome
			if got.Arm != tc.arm || got.Reason != tc.reason || got.Result.Via != tc.via || got.Degradation != tc.degradation {
				t.Fatalf("outcome = %+v, want %s{%s} via %q degradation %q", got, tc.arm, tc.reason, tc.via, tc.degradation)
			}
		})
	}
}

// gc mail reply --json names the notice's op.
func TestMailReplyJSONNamesTheNotifyOp(t *testing.T) {
	c := newContentOpCity(t, "auto")
	mp := beadmail.New(beads.NewMemStore())
	if _, err := mp.Send("alice", "bob", "Hello", "first"); err != nil {
		t.Fatalf("send: %v", err)
	}
	nf := func(_, messageID string) (string, error) {
		return sendMailNotifyWithOp(c.target, c.store, c.fake, "bob", messageID)
	}
	var stdout, stderr bytes.Buffer
	if code := doMailReplyJSON(mp, events.Discard, "gc-1", "bob", "RE: Hello", "reply body", nf, true, &stdout, &stderr); code != 0 {
		t.Fatalf("reply = %d; stderr=%s", code, stderr.String())
	}
	contentOpValidate(t, "schemas/mail/reply/result.schema.json", stdout.String())
	var got mailActionResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout.String(), err)
	}
	op := cliOpOnly(t, c.h.opStore)
	if len(got.NotifyOpIDs) != 1 || got.NotifyOpIDs[0] != op.ID || op.ClientKey != "mail:"+got.ID {
		t.Fatalf("notify_op_ids = %v, op = %+v; want the reply's notice op", got.NotifyOpIDs, op)
	}
}

// A submit the API records and runs live (not a replay) reports the API's op
// and the key under --json, with no replayed field. A supervisor-hosted city
// reaches that API even when its city.toml names an [api] port nothing serves:
// supervisor mode ignores the port, and a local delivery could not reach the
// supervisor's process-owned runtime (#5262).
func TestSessionSubmitThroughTheAPIReportsTheLiveOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/submit"):
			key := r.Header.Get("Idempotency-Key")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test server
				"status": "accepted", "request_id": "gc-op-live", "event_cursor": "0", "idempotency_key": key,
				"op": map[string]any{"id": "gc-op-live", "conversation": "gc-1", "kind": "message", "status": "submitted"},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/stream"):
			raw, err := json.Marshal(map[string]any{
				"type":    events.RequestResultSessionSubmit,
				"payload": api.SessionSubmitSucceededPayload{RequestID: "gc-op-live", SessionID: "gc-1", Intent: string(session.SubmitIntentDefault)},
			})
			if err != nil {
				t.Errorf("encoding the event: %v", err)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", raw) //nolint:errcheck // test server
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		name string
		host func(t *testing.T, c *cliOpCity)
	}{
		{name: "controller down"},
		{name: "supervisor-hosted city with a dead [api] port", host: func(t *testing.T, c *cliOpCity) {
			apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
				return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
			}
			// Port 1 is privileged and unassigned, so loopback refuses it.
			f, err := os.OpenFile(filepath.Join(c.dir, "city.toml"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("opening city.toml: %v", err)
			}
			if _, err := f.WriteString("\n[api]\nport = 1\n"); err != nil {
				t.Fatalf("appending [api] to city.toml: %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("closing city.toml: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := submitOpCity(t, "auto", srv)
			if tc.host != nil {
				tc.host(t, c)
			}
			var stdout, stderr bytes.Buffer
			if code := cmdSessionSubmit([]string{c.bead.ID, testSubmitText}, session.SubmitIntentDefault, "k-live", true, &stdout, &stderr); code != 0 {
				t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
			}
			contentOpValidate(t, "schemas/session/submit/result.schema.json", stdout.String())
			got := cliOpJSON(t, stdout.Bytes())
			if got["op_id"] != "gc-op-live" || got["key"] != "k-live" || got["outcome"] != "submitted" {
				t.Fatalf("JSON = %v, want the API's live op gc-op-live under k-live", got)
			}
			if _, ok := got["replayed"]; ok || strings.Contains(stderr.String(), "replayed") {
				t.Fatalf("JSON = %v, stderr = %q; want no replay", got, stderr.String())
			}
			if n := submitDeliveries(c.sp) + len(cliOpRows(t, h.opStore)); n != 0 {
				t.Fatalf("local deliveries and op rows = %d, want none: the API ran it", n)
			}
		})
	}
}

// Under require, a nudge whose nudge store did not open cannot be recorded,
// so it is refused before its target resolves.
func TestSessionNudgeRequireNoNudgeStoreRefusesBeforeResolving(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_CONTENT": "require"})
	dir := newCLIOpNamedCityWithoutRow(t)
	h.opStore = &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	prev := openNudgeBeadStore
	openNudgeBeadStore = func(string) beads.NudgesStore { return beads.NudgesStore{} }
	t.Cleanup(func() { openNudgeBeadStore = prev })
	var stdout, stderr bytes.Buffer
	code := cmdSessionNudge([]string{killPokeSessionIdentity, "hello"}, nudgeDeliveryImmediate, "", true, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "op journal cannot record this request") {
		t.Fatalf("nudge = %d, stderr = %q; want 1 and the require refusal", code, stderr.String())
	}
	if got := cliOpFailureCode(t, stdout.Bytes()); got != cliOpCodeUnavailable {
		t.Fatalf("failure code = %q, want %s", got, cliOpCodeUnavailable)
	}
	if n := cliOpSessionRows(t, dir); n != 0 {
		t.Fatalf("session rows = %d, want none", n)
	}
}

// admitMailNotifyAsCLI records, as another gc mail notify run would, the
// notice sendMailNotifyWithOp records for messageID from mayor to c's
// session, and leaves it open.
func admitMailNotifyAsCLI(t *testing.T, c *contentOpCity, messageID string) *legacyop.Admission {
	t.Helper()
	st, err := opjournal.NewStore(beads.SessionStore{Store: c.h.opStore}, c.h.clk, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("opening op store: %v", err)
	}
	principal, actor := cliPrincipal(cliOpLookupEnv)
	rec := &legacyop.Recorder{Mode: rollout.Auto, Executor: legacyop.ExecutorCLI, Open: func() (*opjournal.Store, error) { return st, nil }, Clock: c.h.clk}
	adm, err := rec.Admit(legacyop.Request{
		Conversation: c.target.sessionID, Principal: principal, Actor: actor,
		Key: mailNotifyOpKey(messageID), Payload: mailNotifyText("mayor"), Causation: messageID,
		Kind: opjournal.KindMessage, Policy: mailNotifyOpPolicy,
	})
	if err != nil || !adm.Recorded {
		t.Fatalf("admit: recorded=%v err=%v", adm != nil && adm.Recorded, err)
	}
	return adm
}

// A notice has no --key, so its refusals name no --key remedy: an op in
// progress says rerun later, and a closed failed op says rerun.
func TestMailNotifyOpRefusalsNameNoKeyRemedy(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		failed     bool
	}{
		{name: "in progress", want: "rerun later"},
		{name: "already failed", want: "rerun to act again", failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newContentOpCityOn(t, "auto", "claude")
			adm := admitMailNotifyAsCLI(t, c, "gc-mail-k")
			if tc.failed {
				adm.Finish(legacyop.Result{Outcome: opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.RefusedReasonPrefix + "test"}})
			}
			var warn bytes.Buffer
			nudgeWarningWriter = &warn
			id, err := sendMailNotifyWithOp(c.target, c.store, c.fake, "mayor", "gc-mail-k")
			if err == nil || id != "" {
				t.Fatalf("notify = %q, %v; want the refusal", id, err)
			}
			if got := warn.String(); !strings.Contains(got, tc.want) || strings.Contains(got, "--key") {
				t.Fatalf("warning = %q, want %q and no --key remedy", got, tc.want)
			}
			if n := len(c.delivered()); n != 0 {
				t.Fatalf("deliveries = %d, want none", n)
			}
		})
	}
}

// A submit whose city config does not load says its [ops] gates were not
// read.
func TestSessionSubmitConfigLoadFailureWarns(t *testing.T) {
	_, c := submitOpCity(t, "", nil)
	appendCityTOML(t, c.dir, "\n[ops]\ncontent = \"bogus\"\n")
	var stdout, stderr bytes.Buffer
	cmdSessionSubmit([]string{c.bead.ID, testSubmitText}, session.SubmitIntentDefault, "", false, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "[ops] gates were not read") {
		t.Fatalf("stderr = %q, want a warning that the [ops] gates were not read", stderr.String())
	}
}

// A local submit composes the city config once: the gate read and the local
// path share the invocation's config memo.
func TestSessionSubmitLocalComposesConfigOnce(t *testing.T) {
	_, c := submitOpCity(t, "auto", nil)
	before := composedCityConfigLoads.Load()
	var stdout, stderr bytes.Buffer
	if code := cmdSessionSubmit([]string{c.bead.ID, testSubmitText}, session.SubmitIntentDefault, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("submit = %d; stderr=%s", code, stderr.String())
	}
	if got := composedCityConfigLoads.Load() - before; got != 1 {
		t.Fatalf("composed config loads = %d, want 1", got)
	}
}

// In auto, an API that ran a --key submit without recording an op (its
// answer names none) leaves the key unrecorded, so the CLI warns that a
// rerun acts again, as the local path does; without --key it says nothing.
func TestSessionSubmitThroughTheAPIUnrecordedKeyWarns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/submit"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{"status": "accepted", "request_id": "req-1", "event_cursor": "0"}) //nolint:errcheck // test server
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/stream"):
			raw, err := json.Marshal(map[string]any{
				"type":    events.RequestResultSessionSubmit,
				"payload": api.SessionSubmitSucceededPayload{RequestID: "req-1", SessionID: "gc-1", Intent: string(session.SubmitIntentDefault)},
			})
			if err != nil {
				t.Errorf("encoding the event: %v", err)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", raw) //nolint:errcheck // test server
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		key  string
		warn bool
	}{{key: "k-unrec", warn: true}, {key: ""}} {
		_, c := submitOpCity(t, "auto", srv)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSubmit([]string{c.bead.ID, testSubmitText}, session.SubmitIntentDefault, tc.key, false, &stdout, &stderr); code != 0 {
			t.Fatalf("submit key %q = %d; stderr=%s", tc.key, code, stderr.String())
		}
		if got := strings.Contains(stderr.String(), "warning: --key not recorded"); got != tc.warn {
			t.Fatalf("submit key %q stderr = %q, want the unrecorded --key warning = %v", tc.key, stderr.String(), tc.warn)
		}
	}
}

// A broadcast notify under the builtin default over a store that cannot
// record ops prints no op-journal warning, for any recipient.
func TestMailBroadcastNotifyDefaultAutoIsQuietOnAnIncapableStore(t *testing.T) {
	c := newContentOpCity(t, "")
	c.h.opStore = cliOpIncapableStore{c.h.opStore}
	var warn bytes.Buffer
	nudgeWarningWriter = &warn
	mp := beadmail.New(beads.NewMemStore())
	nf := func(_, messageID string) (string, error) {
		return sendMailNotifyWithOp(c.target, c.store, c.fake, "human", messageID)
	}
	recipients := map[string]bool{"human": true, "a": true, "b": true, "c": true}
	var stdout, stderr bytes.Buffer
	if code := doMailSendAllJSON(mp, events.Discard, recipients, "human", []string{"hello"}, nf, false, &stdout, &stderr); code != 0 {
		t.Fatalf("send --all = %d; stderr=%s", code, stderr.String())
	}
	if strings.Contains(warn.String()+stderr.String(), "op journal") {
		t.Fatalf("warnings = %q, stderr = %q; want no op-journal warning under the builtin default", warn.String(), stderr.String())
	}
}
