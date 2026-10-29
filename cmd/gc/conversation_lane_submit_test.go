package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/lanesubmit"
)

// gc session submit under lane authority (slice 7b): with message listed,
// its local fallback accepts the message as POST /submit does
// (lanesubmit.Accept), for every session the API accepts.

// cmdSubmitCity points gc session submit at the fixture: city.toml lists
// message (cityOnDisk), the command opens the fixture's store and fake
// runtime, and its API client talks to srv (nil: no API).
func (c *cliLaneCity) cmdSubmitCity(t *testing.T, srv *httptest.Server) {
	t.Helper()
	c.cmdSubmitCityOver(t, srv, c.store)
}

// cmdSubmitCityOver is cmdSubmitCity whose command opens store as the
// city's store.
func (c *cliLaneCity) cmdSubmitCityOver(t *testing.T, srv *httptest.Server, store beads.Store) {
	t.Helper()
	clearGCEnv(t)
	c.cityOnDisk(t)
	prevFactory, prevBuild := openStoreFactoryForCity, buildSessionProviderByName
	prevIdentity, prevClient, prevGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		openStoreFactoryForCity, buildSessionProviderByName = prevFactory, prevBuild
		apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook = prevIdentity, prevClient, prevGrants
	})
	openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: store}, nil
	}
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return c.sp, nil
	}
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{} }
	apiRouteSupervisorGrantsHook = func() bool { return false }
	apiRouteSupervisorClientHook = func(string) *api.Client {
		if srv == nil {
			return nil
		}
		return api.NewCityScopedClient(srv.URL, filepath.Base(c.dir))
	}
}

// cmdSubmit runs gc session submit of the test message to the fixture's
// session under key, asJSON as --json.
func (c *cliLaneCity) cmdSubmit(intent session.SubmitIntent, key string, asJSON bool) (int, string, string) {
	return c.cmdSubmitText(intent, key, cliLaneTestMessage, asJSON)
}

// cmdSubmitText is cmdSubmit of message.
func (c *cliLaneCity) cmdSubmitText(intent session.SubmitIntent, key, message string, asJSON bool) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cmdSessionSubmit([]string{c.id, message}, intent, key, asJSON, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// proposeLaneSubmit proposes payload under key as the API's acceptance
// proposes a submit of intent: a lane op from the operator with
// lanesubmit.Policy. outcome closes it; nil leaves it open. It returns the
// op as stored.
func (c *cliLaneCity) proposeLaneSubmit(t *testing.T, intent session.SubmitIntent, key, payload string, outcome *opjournal.Outcome) opjournal.Op {
	t.Helper()
	return c.proposeSubmitOp(t, opjournal.ExecutorLane, lanesubmit.Policy(intent), key, payload, outcome)
}

// proposeLegacyMessage records payload under key as executor's legacy path
// records a submit of intent: under the operator, with
// lanesubmit.LegacyPolicy. outcome closes it; nil leaves it open.
func (c *cliLaneCity) proposeLegacyMessage(t *testing.T, executor opjournal.ExecutorKind, intent session.SubmitIntent, key, payload string, outcome *opjournal.Outcome) opjournal.Op {
	t.Helper()
	return c.proposeSubmitOp(t, executor, lanesubmit.LegacyPolicy(intent), key, payload, outcome)
}

func (c *cliLaneCity) proposeSubmitOp(t *testing.T, executor opjournal.ExecutorKind, policy opjournal.Policy, key, payload string, outcome *opjournal.Outcome) opjournal.Op {
	t.Helper()
	st := c.laneStore(t)
	req := opjournal.Request{
		Conversation: c.id, Kind: opjournal.KindMessage, Executor: executor, ClientKey: key,
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  payload, Policy: policy,
	}
	if executor != opjournal.ExecutorLane {
		req.StartBy, req.FinishBy = laneT0.Add(legacyop.ExecutionBound), laneT0.Add(legacyop.ExecutionBound)
	}
	op, _, err := st.Propose(req)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if outcome != nil {
		if _, err := st.Close(op.ID, *outcome, string(executor)); err != nil {
			t.Fatalf("closing: %v", err)
		}
	}
	return c.get(t, st, op.ID)
}

// laneOpByKey reads the op principal's key names on the fixture's session.
func (c *cliLaneCity) laneOpByKey(t *testing.T, principal, key string) opjournal.Op {
	t.Helper()
	st := c.laneStore(t)
	id, err := opjournal.DeriveID(st.Prefix(), c.id, principal, key)
	if err != nil {
		t.Fatal(err)
	}
	return c.get(t, st, id)
}

// assertNothingDelivered checks the submit neither delivered nor
// interrupted.
func (c *cliLaneCity) assertNothingDelivered(t *testing.T) {
	t.Helper()
	if n, calls := len(c.nudges()), c.controlCalls(); n != 0 || len(calls) != 0 {
		t.Fatalf("the submit delivered %d times and interrupted %+v, want neither", n, calls)
	}
}

// suspendAndMaybeDrain places the operator's hold, stamped before laneT0,
// and starts a drain when draining.
func (c *cliLaneCity) suspendAndMaybeDrain(t *testing.T, draining bool) {
	t.Helper()
	if _, err := sessionFrontDoor(c.store).OperatorSuspend(c.id, laneT0.Add(-time.Minute)); err != nil {
		t.Fatalf("suspending the session: %v", err)
	}
	if draining {
		c.drainRow(t)
	}
}

func (c *cliLaneCity) drainRow(t *testing.T) {
	t.Helper()
	if err := c.store.SetMetadataBatch(c.id, map[string]string{"state": string(session.StateDraining)}); err != nil {
		t.Errorf("draining the session: %v", err)
	}
}

// laneUpsertStore overwrites a bead whose pinned id it already holds
// instead of refusing the create: a store without the duplicate-id
// contract, which opjournal.Store.Propose guards by reading an op before it
// creates one unless the request's key was minted in the same call.
type laneUpsertStore struct {
	laneTestStore
}

func (s laneUpsertStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.ID != "" {
		if _, err := s.Get(b.ID); err == nil {
			if err := s.Delete(b.ID); err != nil {
				return beads.Bead{}, err
			}
		}
	}
	return s.laneTestStore.Create(b)
}

// opCreateHookStore runs onCreate once after it creates an op row.
type opCreateHookStore struct {
	laneTestStore
	onCreate func(id string)
}

func (s *opCreateHookStore) Create(b beads.Bead) (beads.Bead, error) {
	got, err := s.laneTestStore.Create(b)
	if err == nil && b.Type == opjournal.BeadType && s.onCreate != nil {
		hook := s.onCreate
		s.onCreate = nil
		hook(got.ID)
	}
	return got, err
}

var submitDelivered = map[session.SubmitIntent]string{
	session.SubmitIntentDefault:      "Submitted to ",
	session.SubmitIntentFollowUp:     "Submitted follow-up to ",
	session.SubmitIntentInterruptNow: "Interrupted and submitted to ",
}

var submitIntents = []session.SubmitIntent{session.SubmitIntentDefault, session.SubmitIntentFollowUp, session.SubmitIntentInterruptNow}

// With message listed and no API, the fallback accepts the message as the
// API would: one lane op under the operator and the key, with the lane's
// policy, for every session. A keystroke session at an idle boundary takes
// a default or follow_up message from the CLI's inline pass, and a replay
// sends nothing; an interrupt_now waits for the controller's lane host,
// interrupting nothing; a protocol-mode session, host-stamped or not, gets
// the same op, which the controller's lane host delivers, nothing from the
// CLI, and one ring.
func TestSessionSubmitLaneFallbackAcceptsAsTheAPIDoes(t *testing.T) {
	for _, intent := range []session.SubmitIntent{session.SubmitIntentDefault, session.SubmitIntentFollowUp} {
		t.Run("keystroke/"+string(intent), func(t *testing.T) {
			c := newCLILaneCity(t)
			c.cmdSubmitCity(t, nil)
			code, stdout, stderr := c.cmdSubmit(intent, "submit-1", false)
			if code != 0 || stdout != submitDelivered[intent]+c.id+"\n" {
				t.Fatalf("submit = %d, %q; stderr %s", code, stdout, stderr)
			}
			op := c.laneOpByKey(t, legacyop.PrincipalOperator, "submit-1")
			want := lanesubmit.Policy(intent).Message
			if m := op.Policy.Message; op.Executor != opjournal.ExecutorLane || m.When != want.When || m.Wake != want.Wake || m.Role != want.Role ||
				op.Payload != cliLaneTestMessage || op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != laneViaLive {
				t.Fatalf("op = %+v, want the API's lane op delivered live", op)
			}
			if n := len(c.nudges()); n != 1 {
				t.Fatalf("deliveries = %d, want the inline pass's one", n)
			}
			code, stdout, stderr = c.cmdSubmit(intent, "submit-1", false)
			if code != 0 || stdout != submitDelivered[intent]+c.id+"\n" || !strings.Contains(stderr, "replayed op "+op.ID) || len(c.nudges()) != 1 {
				t.Fatalf("replay = %d, %q, %q; deliveries %d", code, stdout, stderr, len(c.nudges()))
			}
		})
	}
	t.Run("keystroke/interrupt_now waits for the controller", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.cmdSubmitCity(t, nil)
		c.sp.Calls = nil
		code, stdout, stderr := c.cmdSubmit(session.SubmitIntentInterruptNow, "submit-now", false)
		if code != 0 || stdout != "Queued follow-up for "+c.id+"\n" {
			t.Fatalf("submit = %d, %q; stderr %s", code, stdout, stderr)
		}
		c.assertNothingDelivered(t)
		op := c.laneOpByKey(t, legacyop.PrincipalOperator, "submit-now")
		if op.Status == opjournal.StatusClosed || op.Policy.Message.When != opjournal.WhenNow {
			t.Fatalf("op = %+v, want an open deliver-now message", op)
		}
		if want := []string{c.id + ":" + op.ID}; !slices.Equal(c.rings, want) {
			t.Fatalf("rings = %v, want %v", c.rings, want)
		}
	})
	for _, stamped := range []bool{false, true} {
		t.Run(fmt.Sprintf("protocol mode, host stamp %t", stamped), func(t *testing.T) {
			c := newCLILaneCity(t)
			c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
			if stamped {
				stampHost(t, c.laneFixture, c.laneStore(t), 1, opjournal.KindMessage)
			}
			c.cmdSubmitCity(t, nil)
			c.sp.Calls = nil
			code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "submit-p", true)
			if code != 0 {
				t.Fatalf("submit = %d; stderr %s", code, stderr)
			}
			op := c.only(t)
			if op.Executor != opjournal.ExecutorLane || op.Status == opjournal.StatusClosed || op.ClientKey != "submit-p" {
				t.Fatalf("op = %+v, want one open lane op under the key", op)
			}
			if got := cliOpJSON(t, []byte(stdout)); got["outcome"] != "queued" || got["queued"] != true || got["op_id"] != op.ID || got["key"] != "submit-p" {
				t.Fatalf("JSON = %v, want queued with op %s", got, op.ID)
			}
			contentOpValidate(t, "schemas/session/submit/result.schema.json", stdout)
			c.assertNothingDelivered(t)
			if len(c.rings) != 1 {
				t.Fatalf("rings = %v, want one", c.rings)
			}
		})
	}
}

// A lost answer: the API proposed its lane op under the key it was sent,
// then the connection dropped. The fallback finds that op under the same
// key, for every intent and runtime, on a store that overwrites a
// duplicate pinned id too, and writes no op of its own: open, a keystroke
// session at a boundary takes a default or follow_up message from the
// inline pass through that op, once, and otherwise it is queued; closed
// succeeded, it replays; closed otherwise, it is refused as failed before.
// Kills a fallback that mints another key, declines a protocol-mode session
// to the legacy path, or resets the API's op (R5-1, R1-5).
func TestSessionSubmitLaneFallbackAnswersALostAnswer(t *testing.T) {
	succeeded := &opjournal.Outcome{Arm: opjournal.ArmSucceeded}
	failed := &opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonExpiredBeforeStart}
	for _, intent := range submitIntents {
		for _, mode := range []runtime.TurnInterruptMode{runtime.TurnInterruptKeystroke, runtime.TurnInterruptProtocol} {
			for _, state := range []struct {
				name    string
				outcome *opjournal.Outcome
			}{{"open", nil}, {"closed succeeded", succeeded}, {"closed failed", failed}} {
				for _, upsert := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/upsert=%t", intent, mode, state.name, upsert), func(t *testing.T) {
						store := beads.Store(newLaneTestStore())
						if upsert {
							store = laneUpsertStore{newLaneTestStore()}
						}
						c := newCLILaneCityOver(t, store)
						c.sp.TurnInterruptModeValue = mode
						var sent string
						var apiOp opjournal.Op
						c.cmdSubmitCity(t, lostAnswerServer(t, func(key string) {
							sent = key
							apiOp = c.proposeLaneSubmit(t, intent, key, cliLaneTestMessage, state.outcome)
						}))
						c.sp.Calls = nil
						code, stdout, stderr := c.cmdSubmit(intent, "", true)
						if !strings.HasPrefix(sent, "k-") {
							t.Fatalf("Idempotency-Key = %q, want the minted key", sent)
						}
						op := c.only(t)
						if op.ID != apiOp.ID {
							t.Fatalf("op rows = %+v, want only the API's op %s", op, apiOp.ID)
						}
						inline := state.outcome == nil && mode == runtime.TurnInterruptKeystroke && intent != session.SubmitIntentInterruptNow
						if state.outcome == failed {
							if code != 1 || cliOpFailureCode(t, []byte(stdout)) != cliOpCodeAlreadyFailed ||
								!strings.Contains(stderr, "op "+apiOp.ID+" closed failed{expired_before_start}; rerun without --key to act again (key "+sent+")") {
								t.Fatalf("submit = %d, %q, %q; want op_already_failed", code, stdout, stderr)
							}
							c.assertNothingDelivered(t)
							return
						}
						if code != 0 {
							t.Fatalf("submit = %d; stderr %s", code, stderr)
						}
						contentOpValidate(t, "schemas/session/submit/result.schema.json", stdout)
						got := cliOpJSON(t, []byte(stdout))
						wantOutcome, wantQueued, wantReplayed := "queued", true, any(nil)
						switch {
						case state.outcome != nil:
							wantOutcome, wantQueued, wantReplayed = "submitted", false, true
							if intent == session.SubmitIntentInterruptNow {
								wantOutcome = "interrupted"
							}
						case inline:
							wantOutcome, wantQueued = "submitted", false
						}
						if got["outcome"] != wantOutcome || got["queued"] != wantQueued || got["op_id"] != apiOp.ID || got["key"] != sent || got["replayed"] != wantReplayed {
							t.Fatalf("JSON = %v, want %s from op %s under %s", got, wantOutcome, apiOp.ID, sent)
						}
						if strings.Contains(stdout, sessionSubmitAcceptedOutcome) {
							t.Fatalf("stdout %q reports accepted", stdout)
						}
						if inline {
							if n := len(c.nudges()); n != 1 || op.Outcome.Arm != opjournal.ArmSucceeded {
								t.Fatalf("deliveries %d, op %+v; want the API's op delivered once inline", n, op)
							}
							return
						}
						c.assertNothingDelivered(t)
						if state.outcome != nil && op.Revision != apiOp.Revision {
							t.Fatalf("the API's op was rewritten: %+v, was %+v", op, apiOp)
						}
					})
				}
			}
		}
	}
}

// Under lane authority the key goes to the API whatever ops.content says,
// so a lost answer is found even with the content gate off. Kills the
// legacy rule, which sends no key while the gate is off and then delivers
// the message a second time.
func TestSessionSubmitLaneFallbackSendsItsKeyWhateverTheContentGate(t *testing.T) {
	for _, gate := range []string{"off", "auto", "require", ""} {
		t.Run("gate="+gate, func(t *testing.T) {
			c := newCLILaneCity(t)
			var sent string
			c.cmdSubmitCity(t, lostAnswerServer(t, func(key string) {
				sent = key
				c.proposeLaneSubmit(t, session.SubmitIntentDefault, key, cliLaneTestMessage, &opjournal.Outcome{Arm: opjournal.ArmSucceeded})
			}))
			if gate != "" {
				c.env["GC_OPS_CONTENT"] = gate
			}
			c.sp.Calls = nil
			code, _, stderr := c.cmdSubmit(session.SubmitIntentDefault, "", false)
			if code != 0 || sent == "" || !strings.Contains(stderr, "replayed op ") {
				t.Fatalf("submit = %d, key sent %q, stderr %q; want the API's op replayed", code, sent, stderr)
			}
			c.assertNothingDelivered(t)
			if ops := c.ops(t); len(ops) != 1 {
				t.Fatalf("op rows = %d, want only the API's", len(ops))
			}
		})
	}
	t.Run("--key goes to the API as is", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.env["GC_OPS_CONTENT"] = "off"
		var sent string
		c.cmdSubmitCity(t, lostAnswerServer(t, func(key string) { sent = key }))
		if code, _, stderr := c.cmdSubmit(session.SubmitIntentDefault, "k-mine", false); code != 0 || sent != "k-mine" {
			t.Fatalf("submit = %d, key sent %q; stderr %s", code, sent, stderr)
		}
		if op := c.only(t); op.ClientKey != "k-mine" {
			t.Fatalf("op = %+v, want the fallback's op under --key", op)
		}
	})
}

// Under the content gate's builtin default (no [ops] gate, no
// GC_OPS_CONTENT), which records nothing on a sessions store that is not
// SQLite-backed, a submit whose lost answer left no op under the key claims
// the key with its lane op, so an API that commits its op under the key
// later finds it taken instead of delivering the message a second time.
func TestSessionSubmitLaneFallbackClaimsTheKeyUnderTheBuiltinDefault(t *testing.T) {
	c := newCLILaneCity(t)
	c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	var sent string
	c.cmdSubmitCity(t, lostAnswerServer(t, func(key string) { sent = key }))
	c.sp.Calls = nil
	code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "", false)
	if code != 0 || stdout != "Queued follow-up for "+c.id+"\n" {
		t.Fatalf("submit = %d, %q, stderr %q", code, stdout, stderr)
	}
	c.assertNothingDelivered(t)
	if op := c.only(t); op.ClientKey != sent || op.Executor != opjournal.ExecutorLane || op.Status == opjournal.StatusClosed {
		t.Fatalf("op = %+v, want the CLI's open lane op under the key %q the API was sent", op, sent)
	}
}

// A legacy message op under the key (recorded before message was listed,
// by the CLI's legacy path or by an API whose lane host latched before the
// flip) answers only the request it recorded, and the lane never takes,
// closes or rewrites it: open, it is in progress, exit 1, and a rerun under
// the key reports its outcome (never a rerun without the key, which would
// send it twice); closed, it replays (queued when the legacy path only
// queued it) or is refused as failed. Another request's legacy op is a key
// that names a different request (R4-2).
func TestSessionSubmitLaneFallbackAnswersALegacyOpOnlyForItsRequest(t *testing.T) {
	for _, executor := range []opjournal.ExecutorKind{opjournal.ExecutorLegacyCLI, opjournal.ExecutorLegacyAPI} {
		for _, intent := range submitIntents {
			for _, tc := range []struct {
				name    string
				outcome *opjournal.Outcome
				code    int
				stdout  string
				stderr  string
				failure string
			}{
				{name: "open", code: 1, stderr: " is in progress under its legacy path; rerun later with --key k-legacy to see its outcome (key k-legacy)", failure: cliOpCodeInProgress},
				{name: "closed queued", outcome: &opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: legacyop.ViaLegacyQueued}}, stdout: "Queued follow-up for ", stderr: "replayed op "},
				{name: "closed delivered", outcome: &opjournal.Outcome{Arm: opjournal.ArmSucceeded}, stdout: submitDelivered[intent], stderr: "replayed op "},
				{name: "closed failed", outcome: &opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonExpiredBeforeStart}, code: 1, stderr: " closed failed{expired_before_start}; rerun without --key to act again (key k-legacy)", failure: cliOpCodeAlreadyFailed},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", executor, intent, tc.name), func(t *testing.T) {
					c := newCLILaneCity(t)
					c.cmdSubmitCity(t, nil)
					c.sp.Calls = nil
					legacy := c.proposeLegacyMessage(t, executor, intent, "k-legacy", cliLaneTestMessage, tc.outcome)
					code, stdout, stderr := c.cmdSubmit(intent, "k-legacy", tc.failure != "")
					if code != tc.code || !strings.Contains(stderr, "op "+legacy.ID) || !strings.Contains(stderr, tc.stderr) || strings.Contains(stderr, "without --key") != (tc.outcome != nil && tc.failure != "") {
						t.Fatalf("submit = %d, %q, %q; want %d and %q", code, stdout, stderr, tc.code, tc.stderr)
					}
					if tc.failure != "" {
						if got := cliOpFailureCode(t, []byte(stdout)); got != tc.failure {
							t.Fatalf("failure code %q, want %s", got, tc.failure)
						}
					} else if stdout != tc.stdout+c.id+"\n" {
						t.Fatalf("stdout %q, want %q", stdout, tc.stdout+c.id+"\n")
					}
					if got := c.only(t); got.ID != legacy.ID || got.Status != legacy.Status || got.Revision != legacy.Revision {
						t.Fatalf("the legacy op was taken or rewritten: %+v, was %+v", got, legacy)
					}
					c.assertNothingDelivered(t)
					if len(c.rings) != 0 {
						t.Fatalf("rings = %v for a legacy op", c.rings)
					}
				})
			}
		}
	}
	t.Run("the API's open legacy op after a lost answer", func(t *testing.T) {
		c := newCLILaneCity(t)
		var sent string
		var apiOp opjournal.Op
		c.cmdSubmitCity(t, lostAnswerServer(t, func(key string) {
			sent = key
			apiOp = c.proposeLegacyMessage(t, opjournal.ExecutorLegacyAPI, session.SubmitIntentDefault, key, cliLaneTestMessage, nil)
		}))
		c.sp.Calls = nil
		code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "", true)
		if code != 1 || cliOpFailureCode(t, []byte(stdout)) != cliOpCodeInProgress || !strings.Contains(stderr, "rerun later with --key "+sent) || strings.Contains(stderr, "without --key") {
			t.Fatalf("submit = %d, %q, %q; want op_in_progress naming the key", code, stdout, stderr)
		}
		c.assertNothingDelivered(t)
		if got := c.only(t); got.ID != apiOp.ID || got.Status == opjournal.StatusClosed {
			t.Fatalf("op rows = %+v, want only the API's open op", got)
		}
	})
	for _, tc := range []struct {
		name             string
		executor         opjournal.ExecutorKind
		recorded, intent session.SubmitIntent
		payload          string
	}{
		{name: "a delivered op of another message", executor: opjournal.ExecutorLegacyCLI, recorded: session.SubmitIntentDefault, intent: session.SubmitIntentDefault, payload: "an earlier message"},
		{name: "a delivered op of another intent", executor: opjournal.ExecutorLegacyCLI, recorded: session.SubmitIntentFollowUp, intent: session.SubmitIntentDefault, payload: cliLaneTestMessage},
		{name: "a delivered default op under a follow_up", executor: opjournal.ExecutorLegacyCLI, recorded: session.SubmitIntentDefault, intent: session.SubmitIntentFollowUp, payload: cliLaneTestMessage},
		{name: "the API's open op of another message", executor: opjournal.ExecutorLegacyAPI, recorded: session.SubmitIntentDefault, intent: session.SubmitIntentDefault, payload: "an earlier message"},
	} {
		t.Run("another request: "+tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			c.cmdSubmitCity(t, nil)
			c.sp.Calls = nil
			var outcome *opjournal.Outcome
			if tc.executor == opjournal.ExecutorLegacyCLI {
				outcome = &opjournal.Outcome{Arm: opjournal.ArmSucceeded}
			}
			legacy := c.proposeLegacyMessage(t, tc.executor, tc.recorded, "k-reused", tc.payload, outcome)
			code, stdout, stderr := c.cmdSubmit(tc.intent, "k-reused", true)
			if code != 1 || !strings.Contains(stderr, "the key names a different request (op "+legacy.ID+"); rerun with another --key (key k-reused)") {
				t.Fatalf("submit = %d, stdout %q, stderr %q; want the key reuse refused", code, stdout, stderr)
			}
			if got := cliOpFailureCode(t, []byte(stdout)); got != cliOpCodeKeyMismatch {
				t.Fatalf("failure code %q, want %s", got, cliOpCodeKeyMismatch)
			}
			if got := c.only(t); got.ID != legacy.ID || got.Revision != legacy.Revision {
				t.Fatalf("the legacy op was rewritten: %+v, was %+v", got, legacy)
			}
			c.assertNothingDelivered(t)
		})
	}
}

// Under --json every refusal of the fallback writes the failure envelope
// (schemas/failure.schema.json) with its code, exit 1, and names the key on
// stderr (R4-3).
func TestSessionSubmitLaneFallbackRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		// store, when set, is the city's store; arrange sets the case up.
		store   func() beads.Store
		arrange func(t *testing.T, c *cliLaneCity)
		message string
		key     string
		want    string
		stderr  string
	}{
		{
			name: "the store cannot host lanes", want: cliOpCodeUnavailable, stderr: "conversation lanes need guarded batches",
			arrange: func(t *testing.T, c *cliLaneCity) { c.cmdSubmitCityOver(t, nil, noBatchStore{c.store}) },
		},
		{
			name: "the key names a different request", want: cliOpCodeKeyMismatch, stderr: "the key names a different request (op ",
			arrange: func(t *testing.T, c *cliLaneCity) {
				c.cmdSubmitCity(t, nil)
				c.proposeLaneSubmit(t, session.SubmitIntentDefault, "k-json", "an earlier message", nil)
			},
		},
		{
			name: "the key names an op of another kind", want: cliOpCodeKeyMismatch, stderr: ", a wake op); rerun with another --key (key k-json)",
			arrange: func(t *testing.T, c *cliLaneCity) {
				c.cmdSubmitCity(t, nil)
				if _, _, err := c.laneStore(t).Propose(opjournal.Request{
					Conversation: c.id, Kind: opjournal.KindWake, Executor: opjournal.ExecutorLegacyCLI, ClientKey: "k-json",
					Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
					StartBy:  laneT0.Add(legacyop.ExecutionBound), FinishBy: laneT0.Add(legacyop.ExecutionBound),
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "the op the key names cannot be read", want: cliOpCodeUnavailable, stderr: "store hiccup); the message did not run (key k-json)",
			store: func() beads.Store { return &laneGetFailStore{laneTestStore: newLaneTestStore()} },
			arrange: func(t *testing.T, c *cliLaneCity) {
				c.cmdSubmitCity(t, nil)
				id, err := opjournal.DeriveID("gc", c.id, legacyop.PrincipalOperator, "k-json")
				if err != nil {
					t.Fatal(err)
				}
				s := c.store.(*laneGetFailStore)
				s.failID, s.fails = id, 1
			},
		},
		{
			name: "this run's op closed without success", want: cliOpCodeFailed, stderr: " closed failed{expired_before_start} (key k-json)",
			store: func() beads.Store { return &opCreateHookStore{laneTestStore: newLaneTestStore()} },
			arrange: func(t *testing.T, c *cliLaneCity) {
				c.cmdSubmitCity(t, nil)
				c.store.(*opCreateHookStore).onCreate = func(id string) {
					if _, err := c.laneStore(t).Close(id, opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonExpiredBeforeStart}, "lane:ctl"); err != nil {
						t.Errorf("closing the op: %v", err)
					}
				}
			},
		},
		{
			name: "the op closed without success before", want: cliOpCodeAlreadyFailed, stderr: "; rerun without --key to act again (key k-json)",
			arrange: func(t *testing.T, c *cliLaneCity) {
				c.cmdSubmitCity(t, nil)
				c.proposeLaneSubmit(t, session.SubmitIntentDefault, "k-json", cliLaneTestMessage, &opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonExpiredBeforeStart})
			},
		},
		{
			name: "a legacy op is in progress", want: cliOpCodeInProgress, stderr: " is in progress under its legacy path",
			arrange: func(t *testing.T, c *cliLaneCity) {
				c.cmdSubmitCity(t, nil)
				c.proposeLegacyMessage(t, opjournal.ExecutorLegacyCLI, session.SubmitIntentDefault, "k-json", cliLaneTestMessage, nil)
			},
		},
		{
			name: "the key is invalid", key: " bad-key", want: cliOpCodeInvalidKey, stderr: "invalid --key (",
			arrange: func(t *testing.T, c *cliLaneCity) { c.cmdSubmitCity(t, nil) },
		},
		{
			name: "a payload the journal refuses", message: strings.Repeat("x", opjournal.MaxInlinePayload+1), want: cliOpCodeUnavailable, stderr: "); the message did not run (key k-json)",
			arrange: func(t *testing.T, c *cliLaneCity) { c.cmdSubmitCity(t, nil) },
		},
		{
			name: "a prompt is pending", want: cliOpCodePendingInteraction, stderr: "pending interaction",
			arrange: func(t *testing.T, c *cliLaneCity) {
				c.cmdSubmitCity(t, nil)
				c.sp.SetPendingInteraction(c.name, &runtime.PendingInteraction{RequestID: "approval-1", Kind: "approval", Prompt: "Bash: date"})
			},
		},
		{
			name: "the session refuses the resume", want: cliOpCodeResumeRefused, stderr: "; the message did not run (key k-json)",
			arrange: func(t *testing.T, c *cliLaneCity) {
				c.cmdSubmitCity(t, nil)
				c.suspendAndMaybeDrain(t, true)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c *cliLaneCity
			if tc.store != nil {
				c = newCLILaneCityOver(t, tc.store())
			} else {
				c = newCLILaneCity(t)
			}
			tc.arrange(t, c)
			key, message := "k-json", cliLaneTestMessage
			if tc.key != "" {
				key = tc.key
			}
			if tc.message != "" {
				message = tc.message
			}
			before := len(c.ops(t))
			c.sp.Calls = nil
			code, stdout, stderr := c.cmdSubmitText(session.SubmitIntentDefault, key, message, true)
			if code != 1 || !strings.HasPrefix(stderr, "gc session submit: ") || !strings.Contains(stderr, tc.stderr) {
				t.Fatalf("submit = %d, stderr %q; want 1 and %q", code, stderr, tc.stderr)
			}
			if got := cliOpFailureCode(t, []byte(stdout)); got != tc.want {
				t.Fatalf("failure code %q, want %s; stdout %s", got, tc.want, stdout)
			}
			contentOpValidate(t, "schemas/failure.schema.json", stdout)
			c.assertNothingDelivered(t)
			if tc.want != cliOpCodeFailed {
				if after := len(c.ops(t)); after != before {
					t.Fatalf("op rows %d -> %d, want nothing written", before, after)
				}
				if len(c.rings) != 0 {
					t.Fatalf("rings = %v, want none for a refusal", c.rings)
				}
			}
		})
	}
}

// resume: true under lane authority. found: the API's lost answer left its
// open op behind the operator's hold (the request died before its resume);
// the fallback consumes the hold, recording the session's wake, before it
// reports the op, and a resume the session refuses there (its drain is
// stopping it) is resume_refused, exit 1, the op left open, as an earlier
// answer may have accepted it (P-1). own: a row already draining refuses
// before the write, leaving the key free, so a rerun after the drain is
// accepted; a row that starts draining as the op commits refuses after the
// write, and the op this run wrote is canceled, never passed or rung; a
// resumed op is rung only once its hold is consumed (R6-1).
func TestSessionSubmitLaneFallbackResume(t *testing.T) {
	for _, path := range []struct {
		name   string
		mode   runtime.TurnInterruptMode
		intent session.SubmitIntent
	}{
		{name: "keystroke", mode: runtime.TurnInterruptKeystroke, intent: session.SubmitIntentInterruptNow},
		{name: "protocol", mode: runtime.TurnInterruptProtocol, intent: session.SubmitIntentDefault},
	} {
		for _, draining := range []bool{false, true} {
			t.Run(fmt.Sprintf("found/%s/draining=%t", path.name, draining), func(t *testing.T) {
				c := newCLILaneCity(t)
				c.sp.TurnInterruptModeValue = path.mode
				c.suspendAndMaybeDrain(t, draining)
				var apiOp opjournal.Op
				c.cmdSubmitCity(t, lostAnswerServer(t, func(key string) {
					apiOp = c.proposeLaneSubmit(t, path.intent, key, cliLaneTestMessage, nil)
				}))
				c.sp.Calls = nil
				code, stdout, stderr := c.cmdSubmit(path.intent, "", true)
				c.assertNothingDelivered(t)
				if got := c.only(t); got.ID != apiOp.ID || got.Status == opjournal.StatusClosed {
					t.Fatalf("op rows = %+v, want only the API's open op %s", got, apiOp.ID)
				}
				meta := c.row(t).Metadata
				if draining {
					if code != 1 || cliOpFailureCode(t, []byte(stdout)) != cliOpCodeResumeRefused ||
						!strings.Contains(stderr, "resuming "+c.id+" for op "+apiOp.ID) || !strings.Contains(stderr, "op "+apiOp.ID+" stands, as an earlier request accepted it") ||
						meta["state"] != string(session.StateDraining) {
						t.Fatalf("submit = %d, stdout %q, stderr %q, row state %q; want resume_refused, the op standing, the row draining", code, stdout, stderr, meta["state"])
					}
					if len(c.rings) != 0 {
						t.Fatalf("rings = %v, want none", c.rings)
					}
					return
				}
				if got := cliOpJSON(t, []byte(stdout)); code != 0 || got["outcome"] != "queued" || got["op_id"] != apiOp.ID {
					t.Fatalf("submit = %d, %v, stderr %q; want the API's op queued", code, got, stderr)
				}
				if meta["state"] != string(session.StateAsleep) || meta["wake_request"] != string(session.WakeCauseExplicit) {
					t.Fatalf("session row = %v; want the hold consumed and its wake recorded", meta)
				}
			})
		}
	}
	t.Run("own/refused before the write", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.suspendAndMaybeDrain(t, true)
		c.cmdSubmitCity(t, nil)
		code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "k-d", true)
		if code != 1 || cliOpFailureCode(t, []byte(stdout)) != cliOpCodeResumeRefused || !strings.Contains(stderr, "resuming "+c.id+": ") || !strings.Contains(stderr, "the message did not run (key k-d)") {
			t.Fatalf("submit = %d, %q, %q", code, stdout, stderr)
		}
		if ops := c.ops(t); len(ops) != 0 || len(c.rings) != 0 {
			t.Fatalf("op rows = %+v, rings %v; want none", ops, c.rings)
		}
		// The drain finishes and leaves the row suspended: the key is free.
		if err := c.store.SetMetadataBatch(c.id, map[string]string{"state": string(session.StateSuspended)}); err != nil {
			t.Fatal(err)
		}
		c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		code, stdout, stderr = c.cmdSubmit(session.SubmitIntentDefault, "k-d", true)
		if got := cliOpJSON(t, []byte(stdout)); code != 0 || got["outcome"] != "queued" || got["key"] != "k-d" {
			t.Fatalf("rerun = %d, %q, %q; want accepted under the key", code, stdout, stderr)
		}
	})
	for _, intent := range []session.SubmitIntent{session.SubmitIntentDefault, session.SubmitIntentInterruptNow} {
		for _, key := range []string{"", "k-fresh"} {
			t.Run(fmt.Sprintf("own/refused after the write/%s/key=%q", intent, key), func(t *testing.T) {
				store := &opCreateHookStore{laneTestStore: newLaneTestStore()}
				c := newCLILaneCityOver(t, store)
				c.suspendAndMaybeDrain(t, false)
				store.onCreate = func(string) { c.drainRow(t) }
				c.cmdSubmitCity(t, nil)
				c.sp.Calls = nil
				code, stdout, stderr := c.cmdSubmit(intent, key, false)
				op := c.only(t)
				if code != 1 || stdout != "" || !strings.Contains(stderr, "resuming "+c.id+" for op "+op.ID+": ") ||
					!strings.Contains(stderr, "op "+op.ID+", which this run wrote, was canceled: rerun without --key to send the message again") {
					t.Fatalf("submit = %d, %q, %q", code, stdout, stderr)
				}
				if arm, _ := op.Outcome.Effective(); op.Status != opjournal.StatusClosed || arm != opjournal.ArmCanceled || len(c.rings) != 0 {
					t.Fatalf("op = %+v, rings %v; want it canceled, never rung", op, c.rings)
				}
				c.assertNothingDelivered(t)
			})
		}
		t.Run("own/resumed/"+string(intent), func(t *testing.T) {
			c := newCLILaneCity(t)
			c.suspendAndMaybeDrain(t, false)
			c.cmdSubmitCity(t, nil)
			ring := cliLaneRing
			var stateAtRing []string
			cliLaneRing = func(city, conv, opID string) (string, error) {
				stateAtRing = append(stateAtRing, c.row(t).Metadata["state"])
				return ring(city, conv, opID)
			}
			c.sp.Calls = nil
			code, _, stderr := c.cmdSubmit(intent, "", false)
			op := c.only(t)
			if code != 0 {
				t.Fatalf("submit = %d; stderr %s", code, stderr)
			}
			if want := []string{c.id + ":" + op.ID}; !slices.Equal(c.rings, want) || !slices.Equal(stateAtRing, []string{string(session.StateAsleep)}) {
				t.Fatalf("rings %v, the session's state at them %v; want %v, rung once the hold was consumed", c.rings, stateAtRing, want)
			}
			if meta := c.row(t).Metadata; meta["state"] != string(session.StateAsleep) || meta["wake_request"] != string(session.WakeCauseExplicit) {
				t.Fatalf("session row = %v; want the hold consumed and its wake recorded", meta)
			}
		})
	}
}

// A new message to a session waiting on a pending interaction is refused,
// writing nothing, as the API's front door refuses it; a same-key retry of
// an op that exists is answered from it past the prompt (request_id rule
// 8); and a look that cannot reach the runtime accepts the message, with a
// warning, as the lane looks again before it delivers.
func TestSessionSubmitLaneFallbackRefusesANewMessageWhileAPromptIsPending(t *testing.T) {
	t.Run("a new message is refused", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.cmdSubmitCity(t, nil)
		c.sp.SetPendingInteraction(c.name, &runtime.PendingInteraction{RequestID: "approval-1", Kind: "approval", Prompt: "Bash: date"})
		c.sp.Calls = nil
		code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "k-p", true)
		if code != 1 || cliOpFailureCode(t, []byte(stdout)) != cliOpCodePendingInteraction || len(c.ops(t)) != 0 {
			t.Fatalf("submit = %d, %q, %q, ops %d", code, stdout, stderr, len(c.ops(t)))
		}
		c.assertNothingDelivered(t)
	})
	t.Run("a retry is answered past the prompt", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.cmdSubmitCity(t, nil)
		prior := c.proposeLaneSubmit(t, session.SubmitIntentDefault, "k-p", cliLaneTestMessage, &opjournal.Outcome{Arm: opjournal.ArmSucceeded})
		c.sp.SetPendingInteraction(c.name, &runtime.PendingInteraction{RequestID: "approval-1", Kind: "approval", Prompt: "Bash: date"})
		code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "k-p", false)
		if code != 0 || stdout != "Submitted to "+c.id+"\n" || !strings.Contains(stderr, "replayed op "+prior.ID) {
			t.Fatalf("retry = %d, %q, %q; want the op replayed", code, stdout, stderr)
		}
	})
	t.Run("an unavailable look accepts", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		c.cmdSubmitCity(t, nil)
		c.sp.PendingErrors[c.name] = fmt.Errorf("%w: socket gone", runtime.ErrRuntimeUnavailable)
		code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "k-p", false)
		if code != 0 || stdout != "Queued follow-up for "+c.id+"\n" || !strings.Contains(stderr, "gc session submit: warning: accepting the message without a look for a pending interaction (") {
			t.Fatalf("submit = %d, %q, %q; want accepted with the warning", code, stdout, stderr)
		}
		if op := c.only(t); op.ClientKey != "k-p" {
			t.Fatalf("op = %+v", op)
		}
	})
	t.Run("another look error refuses", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.cmdSubmitCity(t, nil)
		c.sp.PendingErrors[c.name] = errors.New("pending look broke")
		code, _, stderr := c.cmdSubmit(session.SubmitIntentDefault, "k-p", false)
		if code != 1 || !strings.Contains(stderr, "pending look broke") || len(c.ops(t)) != 0 {
			t.Fatalf("submit = %d, %q, ops %d; want refused, nothing written", code, stderr, len(c.ops(t)))
		}
	})
}

// laneOpRereadFailStore fails every read of an op row it holds: the read of
// the op the fallback proposed, after its inline pass.
type laneOpRereadFailStore struct {
	laneTestStore
}

func (s *laneOpRereadFailStore) Get(id string) (beads.Bead, error) {
	b, err := s.laneTestStore.Get(id)
	if err == nil && slices.Contains(b.Labels, opjournal.LabelOp) {
		return beads.Bead{}, errors.New("store hiccup")
	}
	return b, err
}

// A read of the op after the inline pass that fails is warned of, and the
// op is reported as accepted: queued for the controller.
func TestSessionSubmitLaneFallbackWarnsOfAFailedReread(t *testing.T) {
	c := newCLILaneCityOver(t, &laneOpRereadFailStore{laneTestStore: newLaneTestStore()})
	c.cmdSubmitCity(t, nil)
	code, stdout, stderr := c.cmdSubmit(session.SubmitIntentDefault, "k-reread", false)
	if code != 0 || stdout != "Queued follow-up for "+c.id+"\n" ||
		!strings.Contains(stderr, "gc session submit: warning: reading op ") || !strings.Contains(stderr, " after the inline delivery failed (") || !strings.Contains(stderr, "store hiccup); it waits for the controller") {
		t.Fatalf("submit = %d, %q, stderr %q; want queued and the failed read warned of", code, stdout, stderr)
	}
}

// laneSubmitAPIServer answers a submit 202 under the key it was sent and
// emits its result event, queued as given.
func laneSubmitAPIServer(t *testing.T, opID, sessionID string, queued bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/submit"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test server
				"status": "accepted", "request_id": opID, "event_cursor": "0", "idempotency_key": r.Header.Get("Idempotency-Key"),
				"op": map[string]any{"id": opID, "conversation": sessionID, "kind": "message", "status": "submitted"},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/stream"):
			raw, err := json.Marshal(map[string]any{
				"type":    events.RequestResultSessionSubmit,
				"payload": api.SessionSubmitSucceededPayload{RequestID: opID, SessionID: sessionID, Queued: queued, Intent: string(session.SubmitIntentDefault)},
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
	return srv
}

// The API path and the fallback report an op in one vocabulary, whoever
// wrote it: open is queued, delivered is submitted, each schema-valid, and
// accepted never appears.
func TestSessionSubmitUnderLaneSpeaksOneVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		queued bool
		want   string
		run    func(t *testing.T, c *cliLaneCity) (int, string, string)
	}{
		{name: "the API's open op", queued: true, want: "queued", run: func(t *testing.T, c *cliLaneCity) (int, string, string) {
			c.cmdSubmitCity(t, laneSubmitAPIServer(t, "gc-op-api", c.id, true))
			return c.cmdSubmit(session.SubmitIntentDefault, "k-v", true)
		}},
		{name: "the API's delivered op", want: "submitted", run: func(t *testing.T, c *cliLaneCity) (int, string, string) {
			c.cmdSubmitCity(t, laneSubmitAPIServer(t, "gc-op-api", c.id, false))
			return c.cmdSubmit(session.SubmitIntentDefault, "k-v", true)
		}},
		{name: "the fallback's open op", queued: true, want: "queued", run: func(t *testing.T, c *cliLaneCity) (int, string, string) {
			c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
			c.cmdSubmitCity(t, nil)
			return c.cmdSubmit(session.SubmitIntentDefault, "k-v", true)
		}},
		{name: "the fallback's delivered op", want: "submitted", run: func(t *testing.T, c *cliLaneCity) (int, string, string) {
			c.cmdSubmitCity(t, nil)
			return c.cmdSubmit(session.SubmitIntentDefault, "k-v", true)
		}},
		{name: "the API's open op found after a lost answer", queued: true, want: "queued", run: func(t *testing.T, c *cliLaneCity) (int, string, string) {
			c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
			c.cmdSubmitCity(t, lostAnswerServer(t, func(key string) {
				c.proposeLaneSubmit(t, session.SubmitIntentDefault, key, cliLaneTestMessage, nil)
			}))
			return c.cmdSubmit(session.SubmitIntentDefault, "k-v", true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			code, stdout, stderr := tc.run(t, c)
			if code != 0 {
				t.Fatalf("submit = %d; stderr %s", code, stderr)
			}
			contentOpValidate(t, "schemas/session/submit/result.schema.json", stdout)
			got := cliOpJSON(t, []byte(stdout))
			if got["outcome"] != tc.want || got["queued"] != tc.queued || got["key"] != "k-v" || got["op_id"] == nil {
				t.Fatalf("JSON = %v, want %s under k-v", got, tc.want)
			}
			if strings.Contains(stdout+stderr, sessionSubmitAcceptedOutcome) || strings.Contains(stdout, "Accepted by the controller") {
				t.Fatalf("stdout %q, stderr %q: want no accepted outcome", stdout, stderr)
			}
		})
	}
}

// gc session submit, with message listed, refuses a lane op it cannot write
// before the configured named session's row materializes (R5-2), as gc
// session nudge does (TestCmdSessionNudgeLaneRefusesBeforeMaterializing):
// the lane preflight runs ahead of the resolution for a store that cannot
// open as a lane store, and Accept's preview ahead of it for an invalid key
// and a payload the journal refuses.
func TestCmdSessionSubmitLaneRefusesBeforeMaterializing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// laneStore opens the city's store as a store that can host lanes,
		// instead of the file store a test city opens, which names no id
		// prefix.
		laneStore bool
		key       string
		message   string
		code      string
		stderr    string
	}{
		{
			name: "the store cannot host lanes", message: "hello", code: cliOpCodeUnavailable,
			stderr: "op journal cannot record this request (opening op store",
		},
		{
			name: "an invalid key", laneStore: true, key: " bad-key", message: "hello", code: cliOpCodeInvalidKey,
			stderr: "invalid --key (",
		},
		{
			name: "a payload the journal refuses", laneStore: true, message: "\xff", code: cliOpCodeUnavailable,
			stderr: "op journal cannot record this request (opjournal: invalid request: payload is not valid UTF-8); the message did not run",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newCLIOpHarness(t, nil)
			dir := newCLIOpNamedCityWithoutRow(t)
			cityTOML := filepath.Join(dir, "city.toml")
			data, err := os.ReadFile(cityTOML)
			if err != nil {
				t.Fatalf("reading city.toml: %v", err)
			}
			if err := os.WriteFile(cityTOML, append(data, []byte("\n[ops]\nauthoritative = [\"message\"]\n")...), 0o644); err != nil {
				t.Fatalf("writing city.toml: %v", err)
			}
			t.Setenv("GC_NO_API", "1")
			if tc.laneStore {
				store := newLaneTestStore()
				prev := openStoreFactoryForCity
				openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
					return beads.StoreOpenResult{Store: store}, nil
				}
				t.Cleanup(func() { openStoreFactoryForCity = prev })
			}
			var stdout, stderr bytes.Buffer
			code := cmdSessionSubmit([]string{killPokeSessionIdentity, tc.message}, session.SubmitIntentDefault, tc.key, true, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("submit = %d, stderr %q; want the lane refusal %q", code, stderr.String(), tc.stderr)
			}
			if got := cliOpFailureCode(t, stdout.Bytes()); got != tc.code {
				t.Fatalf("failure code %q, want %s", got, tc.code)
			}
			if n := cliOpSessionRows(t, dir); n != 0 {
				t.Fatalf("session rows = %d, want none: the refusal comes before the resolution", n)
			}
		})
	}
}
