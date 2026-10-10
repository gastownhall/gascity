package main

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// createAgentSession drives POST /sessions (202) for the harness agent
// template and returns the session once the documented terminal outcome
// arrived: request.result.session.create correlated by request_id, after
// which the session is commandable (the controller reconciled it) and the
// fake runtime reports it running.
func (h *contractHarness) createAgentSession(t *testing.T, title string) genclient.SessionResponse {
	t.Helper()
	resp, err := h.client.CreateSessionWithResponse(h.ctx, contractCityName,
		&genclient.CreateSessionParams{XGCRequest: contractCSRF},
		genclient.CreateSessionJSONRequestBody{Kind: ptr("agent"), Name: ptr(contractAgent), Title: ptr(title)})
	expectStatus(t, "create session", resp, err, http.StatusAccepted)
	acc := mustJSON(t, "create session", resp.JSON202, resp)
	if acc.Status != "accepted" {
		t.Fatalf("create session status = %q, want accepted", acc.Status)
	}
	e := h.awaitSuccess(t, contractAccepted{RequestID: acc.RequestId, EventCursor: acc.EventCursor}, events.RequestResultSessionCreate)
	var payload struct {
		Session genclient.SessionResponse `json:"session"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil || payload.Session.Id == "" {
		t.Fatalf("session.create payload = %s (%v)", string(e.Payload), err)
	}
	got := h.getSession(t, payload.Session.Id)
	switch got.State {
	case "active", "awake":
	default:
		t.Fatalf("created session %s state = %q, want a commandable running state", got.Id, got.State)
	}
	if !got.Running || !h.sp.IsRunning(got.SessionName) {
		t.Fatalf("created session %s not running (api running=%v, runtime=%v)", got.Id, got.Running, h.sp.IsRunning(got.SessionName))
	}
	if got.Title != title || got.Template != contractAgent {
		t.Fatalf("created session = %+v, want title %q template %q", got, title, contractAgent)
	}
	return got
}

func (h *contractHarness) getSession(t *testing.T, id string) genclient.SessionResponse {
	t.Helper()
	resp, err := h.client.GetV0CityByCityNameSessionByIdWithResponse(h.ctx, contractCityName, id, nil)
	expectStatus(t, "get session "+id, resp, err, http.StatusOK)
	return *mustJSON(t, "get session "+id, resp.JSON200, resp)
}

// openStream opens an SSE operation, checks it answers 200 text/event-stream,
// and closes it. Frame-level stream semantics are owned by the SSE suites;
// this proves the operation is wired and declared.
func (h *contractHarness) openStream(t *testing.T, what string, open func(ctx context.Context) (*http.Response, error)) {
	t.Helper()
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	resp, err := open(ctx)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || mt != "text/event-stream" {
		t.Fatalf("%s: status %d content-type %q, want 200 text/event-stream", what, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// contractSessionsFamily covers the session surface on a session created
// through the async create path.
func contractSessionsFamily(t *testing.T, h *contractHarness) {
	c, ctx, city := h.client, h.ctx, contractCityName
	sess := h.createAgentSession(t, "contract session")

	list, err := c.GetV0CityByCityNameSessionsWithResponse(ctx, city, &genclient.GetV0CityByCityNameSessionsParams{Template: ptr(contractAgent)})
	expectStatus(t, "list sessions", list, err, http.StatusOK)
	found := false
	for _, s := range derefSlice(mustJSON(t, "list sessions", list.JSON200, list).Items) {
		found = found || s.Id == sess.Id
	}
	if !found {
		t.Fatalf("session list missing %s: %s", sess.Id, contractBody(list))
	}

	patched, err := c.PatchV0CityByCityNameSessionByIdWithResponse(ctx, city, sess.Id,
		&genclient.PatchV0CityByCityNameSessionByIdParams{XGCRequest: contractCSRF},
		genclient.PatchV0CityByCityNameSessionByIdJSONRequestBody{Title: ptr("contract session patched")})
	expectStatus(t, "patch session", patched, err, http.StatusOK)
	if got := h.getSession(t, sess.Id); got.Title != "contract session patched" {
		t.Fatalf("title after patch = %q", got.Title)
	}
	renamed, err := c.PostV0CityByCityNameSessionByIdRenameWithResponse(ctx, city, sess.Id,
		&genclient.PostV0CityByCityNameSessionByIdRenameParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameSessionByIdRenameJSONRequestBody{Title: "contract session renamed"})
	expectStatus(t, "rename session", renamed, err, http.StatusOK)
	if got := h.getSession(t, sess.Id); got.Title != "contract session renamed" {
		t.Fatalf("title after rename = %q", got.Title)
	}

	transcript, err := c.GetV0CityByCityNameSessionByIdTranscriptWithResponse(ctx, city, sess.Id, nil)
	expectStatus(t, "session transcript", transcript, err, http.StatusOK)
	agents, err := c.GetV0CityByCityNameSessionByIdAgentsWithResponse(ctx, city, sess.Id)
	expectStatus(t, "session agents", agents, err, http.StatusOK)
	agent, err := c.GetV0CityByCityNameSessionByIdAgentsByAgentIdWithResponse(ctx, city, sess.Id, "no-such-subagent")
	expectStatus(t, "session agent", agent, err, http.StatusNotFound)
	h.openStream(t, "session stream", func(ctx context.Context) (*http.Response, error) {
		return c.StreamSession(ctx, city, sess.Id, nil)
	})

	// Pending interaction reads: the runtime raises one and both the session
	// and the city pending reads surface it. (Responding is the
	// respond-session row of the async-202 table.)
	pending, err := c.GetV0CityByCityNameSessionByIdPendingWithResponse(ctx, city, sess.Id)
	expectStatus(t, "session pending (none)", pending, err, http.StatusOK)
	if p := mustJSON(t, "session pending", pending.JSON200, pending); !p.Supported || p.Pending != nil {
		t.Fatalf("pending before interaction = %s", contractBody(pending))
	}
	h.raisePending(t, sess)
	cityPending, err := c.GetV0CityByCityNamePendingWithResponse(ctx, city)
	expectStatus(t, "city pending", cityPending, err, http.StatusOK)
	if !strings.Contains(string(cityPending.Body), runtimePendingApproval.RequestID) {
		t.Fatalf("city pending missing %s: %s", runtimePendingApproval.RequestID, cityPending.Body)
	}
	h.sp.SetPendingInteraction(sess.SessionName, nil)

	perm, err := c.PostV0CityByCityNameSessionByIdPermissionModeWithResponse(ctx, city, sess.Id,
		&genclient.PostV0CityByCityNameSessionByIdPermissionModeParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameSessionByIdPermissionModeJSONRequestBody{PermissionMode: "plan"})
	// Permission mode is a launch-time option: on a running session the
	// documented refusal is 409 session-conflict (501 where a provider has
	// no permission modes) — never a 500 or an undeclared status.
	expectStatus(t, "session permission mode", perm, err, http.StatusConflict, http.StatusNotImplemented)

	// Bead assignment resolves a session id to the session's assignee
	// identifier (its session name).
	bead, err := c.CreateBeadWithResponse(ctx, city, &genclient.CreateBeadParams{XGCRequest: contractCSRF},
		genclient.CreateBeadJSONRequestBody{Title: "contract assigned work"})
	expectStatus(t, "create bead to assign", bead, err, http.StatusCreated)
	beadID := mustJSON(t, "create bead to assign", bead.JSON201, bead).Id
	assigned, err := c.PostV0CityByCityNameBeadByIdAssignWithResponse(ctx, city, beadID,
		&genclient.PostV0CityByCityNameBeadByIdAssignParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameBeadByIdAssignJSONRequestBody{Assignee: ptr(sess.Id)})
	expectStatus(t, "assign bead", assigned, err, http.StatusOK)
	reread, err := c.GetV0CityByCityNameBeadByIdWithResponse(ctx, city, beadID)
	expectStatus(t, "reread assigned bead", reread, err, http.StatusOK)
	if b := mustJSON(t, "reread assigned bead", reread.JSON200, reread); b.Assignee == nil || *b.Assignee != sess.SessionName {
		t.Fatalf("assignee = %s, want %s", contractBody(reread), sess.SessionName)
	}

	// Lifecycle verbs, each checked on the read path.
	suspended, err := c.PostV0CityByCityNameSessionByIdSuspendWithResponse(ctx, city, sess.Id,
		&genclient.PostV0CityByCityNameSessionByIdSuspendParams{XGCRequest: contractCSRF})
	expectStatus(t, "suspend session", suspended, err, http.StatusOK)
	if got := h.getSession(t, sess.Id); got.State != "suspended" {
		t.Fatalf("state after suspend = %q", got.State)
	}
	woke, err := c.PostV0CityByCityNameSessionByIdWakeWithResponse(ctx, city, sess.Id,
		&genclient.PostV0CityByCityNameSessionByIdWakeParams{XGCRequest: contractCSRF})
	expectStatus(t, "wake session", woke, err, http.StatusOK)
	reset, err := c.PostV0CityByCityNameSessionByIdResetWithResponse(ctx, city, sess.Id,
		&genclient.PostV0CityByCityNameSessionByIdResetParams{XGCRequest: contractCSRF})
	expectStatus(t, "reset session", reset, err, http.StatusOK)
	// Wake and reset only request a start; the reconciler restarts the
	// runtime asynchronously. Kill acts on a live runtime, so wait for it.
	h.backoff(t, "session running again after wake and reset", func() (bool, string) {
		got := h.getSession(t, sess.Id)
		return h.sp.IsRunning(sess.SessionName), fmt.Sprintf("state=%q running=%v", got.State, got.Running)
	})
	stopped, err := c.PostV0CityByCityNameSessionByIdStopWithResponse(ctx, city, sess.Id,
		&genclient.PostV0CityByCityNameSessionByIdStopParams{XGCRequest: contractCSRF})
	expectStatus(t, "stop session", stopped, err, http.StatusOK)
	killed, err := c.PostV0CityByCityNameSessionByIdKillWithResponse(ctx, city, sess.Id,
		&genclient.PostV0CityByCityNameSessionByIdKillParams{XGCRequest: contractCSRF})
	expectStatus(t, "kill session", killed, err, http.StatusOK)
	closed, err := c.PostV0CityByCityNameSessionByIdCloseWithResponse(ctx, city, sess.Id,
		&genclient.PostV0CityByCityNameSessionByIdCloseParams{XGCRequest: contractCSRF})
	expectStatus(t, "close session", closed, err, http.StatusOK)
	after, err := c.GetV0CityByCityNameSessionByIdWithResponse(ctx, city, sess.Id, nil)
	expectStatus(t, "get closed session", after, err, http.StatusOK)
	// A closed session reads back closed and not running.
	if got := mustJSON(t, "get closed session", after.JSON200, after); got.State != "closed" || got.Running || h.sp.IsRunning(sess.SessionName) {
		t.Fatalf("closed session reads back state=%q running=%v: %s", got.State, got.Running, contractBody(after))
	}

	missing, err := c.GetV0CityByCityNameSessionByIdWithResponse(ctx, city, "no-such-session", nil)
	expectStatus(t, "get missing session", missing, err, http.StatusNotFound)
}

// runtimePendingApproval is the interaction the suite raises on the fake
// runtime.
var runtimePendingApproval = runtime.PendingInteraction{RequestID: "contract-perm-1", Kind: "approval", Prompt: "allow?"}

// respondAndExpectCleared drives respond-session (202). Its documented
// terminal state is synchronous: the runtime received exactly one response
// for the pending request and the pending read is empty again.
func (h *contractHarness) respondAndExpectCleared(t *testing.T, sess genclient.SessionResponse) {
	t.Helper()
	before := h.sp.CountCalls("Respond", sess.SessionName)
	resp, err := h.client.RespondSessionWithResponse(h.ctx, contractCityName, sess.Id,
		&genclient.RespondSessionParams{XGCRequest: contractCSRF},
		genclient.RespondSessionJSONRequestBody{Action: "approve", RequestId: ptr(runtimePendingApproval.RequestID)})
	expectStatus(t, "respond session", resp, err, http.StatusAccepted)
	if body := mustJSON(t, "respond session", resp.JSON202, resp); body.Status != "accepted" || body.Id != sess.Id {
		t.Fatalf("respond body = %s", contractBody(resp))
	}
	if got := h.sp.CountCalls("Respond", sess.SessionName) - before; got != 1 {
		t.Fatalf("runtime Respond calls = %d, want 1", got)
	}
	pending, err := h.client.GetV0CityByCityNameSessionByIdPendingWithResponse(h.ctx, contractCityName, sess.Id)
	expectStatus(t, "pending after respond", pending, err, http.StatusOK)
	if p := mustJSON(t, "pending after respond", pending.JSON200, pending); p.Pending != nil {
		t.Fatalf("pending still set after respond: %s", contractBody(pending))
	}
	again, err := h.client.RespondSessionWithResponse(h.ctx, contractCityName, sess.Id,
		&genclient.RespondSessionParams{XGCRequest: contractCSRF},
		genclient.RespondSessionJSONRequestBody{Action: "approve", RequestId: ptr(runtimePendingApproval.RequestID)})
	expectStatus(t, "respond with nothing pending", again, err, http.StatusConflict)
}

// submitAndExpectDelivered drives submit-session (202) to
// request.result.session.submit and checks the runtime received the text.
func (h *contractHarness) submitAndExpectDelivered(t *testing.T, sess genclient.SessionResponse) {
	t.Helper()
	resp, err := h.client.SubmitSessionWithResponse(h.ctx, contractCityName, sess.Id,
		&genclient.SubmitSessionParams{XGCRequest: contractCSRF},
		genclient.SubmitSessionJSONRequestBody{Message: "contract submit"})
	expectStatus(t, "submit session", resp, err, http.StatusAccepted)
	acc := mustJSON(t, "submit session", resp.JSON202, resp)
	h.awaitSuccess(t, contractAccepted{RequestID: acc.RequestId, EventCursor: acc.EventCursor}, events.RequestResultSessionSubmit)
	h.expectRuntimeReceived(t, sess.SessionName, "contract submit")
}

// messageAndExpectDelivered drives send-session-message (202) to
// request.result.session.message and checks the runtime received the text.
func (h *contractHarness) messageAndExpectDelivered(t *testing.T, sess genclient.SessionResponse) {
	t.Helper()
	resp, err := h.client.SendSessionMessageWithResponse(h.ctx, contractCityName, sess.Id,
		&genclient.SendSessionMessageParams{XGCRequest: contractCSRF},
		genclient.SendSessionMessageJSONRequestBody{Message: "contract message"})
	expectStatus(t, "send session message", resp, err, http.StatusAccepted)
	acc := mustJSON(t, "send session message", resp.JSON202, resp)
	h.awaitSuccess(t, contractAccepted{RequestID: acc.RequestId, EventCursor: acc.EventCursor}, events.RequestResultSessionMessage)
	h.expectRuntimeReceived(t, sess.SessionName, "contract message")
}

// expectRuntimeReceived checks a Nudge/SendKeys call to the session carried
// the text.
func (h *contractHarness) expectRuntimeReceived(t *testing.T, sessionName, text string) {
	t.Helper()
	for _, call := range h.sp.SnapshotCalls() {
		if call.Name == sessionName && strings.Contains(call.Message, text) {
			return
		}
	}
	t.Fatalf("runtime never received %q for %s; calls: %+v", text, sessionName, h.sp.SnapshotCalls())
}

func derefSlice[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
}
