package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/ssrf"
)

// P1-5: the async-202 terminal-outcome contract.
//
// Every operation the spec declares with a 202 response has exactly one row
// here. A row drives the operation against the running controller and
// asserts its documented terminal outcome: a correlated request.result.* /
// request.failed event (matched by request_id from the accepted
// event_cursor), the resource's terminal state on the read path, or the
// explicit typed refusal this harness is documented to receive. A new 202
// operation without a row fails TestAPIContractSuite.
type contractAsyncCase struct {
	operationID string
	// terminal documents the outcome the row asserts.
	terminal string
	run      func(t *testing.T, h *contractHarness)
}

var contractAsyncCases = []contractAsyncCase{
	{
		operationID: "create-session",
		terminal:    "request.result.session.create for the request_id; session commandable (active/awake) and running in the runtime",
		run: func(t *testing.T, h *contractHarness) {
			h.createAgentSession(t, "async create")
		},
	},
	{
		operationID: "submit-session",
		terminal:    "request.result.session.submit for the request_id; the runtime received the submitted text",
		run: func(t *testing.T, h *contractHarness) {
			h.submitAndExpectDelivered(t, h.createAgentSession(t, "async submit"))
		},
	},
	{
		operationID: "send-session-message",
		terminal:    "request.result.session.message for the request_id; the runtime received the message text",
		run: func(t *testing.T, h *contractHarness) {
			h.messageAndExpectDelivered(t, h.createAgentSession(t, "async message"))
		},
	},
	{
		operationID: "respond-session",
		terminal:    "synchronous delivery behind the 202: the runtime received exactly one response and the pending interaction is cleared; a repeat answers 409",
		run: func(t *testing.T, h *contractHarness) {
			sess := h.createAgentSession(t, "async respond")
			h.raisePending(t, sess)
			h.respondAndExpectCleared(t, sess)
		},
	},
	{
		operationID: "create-rig",
		terminal:    "git_url create: request.result.rig.create for the request_id (after rig.provision.progress); the rig is listed and resolvable",
		run:         contractAsyncRigCreate,
	},
	{
		operationID: "post-v0-city-by-city-name-order-by-name-run",
		terminal:    "order.completed for the order's scoped name; the tracking bead is in order history with its outcome",
		run:         contractAsyncOrderRun,
	},
	{
		operationID: "post-v0-city-by-city-name-runs-by-run-id-cancel",
		terminal:    "synchronous wind-down behind the 202: run status canceled with its root closed; a repeat answers 409",
		run:         contractAsyncRunCancel,
	},
	{
		operationID: "trigger-maintenance-dolt-gc",
		terminal:    "explicit refusal: a file-backed city has no Dolt store, so the trigger answers the documented 503 (no 202, no orphaned run; the Dolt-backed 202 run body is owned by internal/api TestHumaHandleMaintenanceTriggerDoltGC_WaitSuccess)",
		run: func(t *testing.T, h *contractHarness) {
			resp, err := h.client.TriggerMaintenanceDoltGcWithResponse(h.ctx, contractCityName,
				&genclient.TriggerMaintenanceDoltGcParams{XGCRequest: contractCSRF})
			expectStatus(t, "trigger dolt gc", resp, err, http.StatusServiceUnavailable)
			status, err := h.client.GetV0CityByCityNameMaintenanceStatusWithResponse(h.ctx, contractCityName)
			expectStatus(t, "maintenance status", status, err, http.StatusOK, http.StatusServiceUnavailable)
		},
	},
	{
		operationID: "post-v0-city",
		terminal:    "explicit refusal: a single-city controller mux has no city initializer, so create answers the documented 501 (the supervisor path is owned by TestGCLiveContract_BeadsAndEvents)",
		run: func(t *testing.T, h *contractHarness) {
			resp, err := h.client.PostV0CityWithResponse(h.ctx, &genclient.PostV0CityParams{XGCRequest: contractCSRF},
				genclient.PostV0CityJSONRequestBody{Dir: filepath.Join(t.TempDir(), "new-city")})
			expectStatus(t, "create city", resp, err, http.StatusNotImplemented)
		},
	},
	{
		operationID: "post-v0-city-by-city-name-unregister",
		terminal:    "explicit refusal: a single-city controller mux has no city initializer, so unregister answers 501 and the city stays registered",
		run: func(t *testing.T, h *contractHarness) {
			resp, err := h.client.PostV0CityByCityNameUnregisterWithResponse(h.ctx, contractCityName,
				&genclient.PostV0CityByCityNameUnregisterParams{XGCRequest: contractCSRF})
			expectStatus(t, "unregister city", resp, err, http.StatusNotImplemented)
			city, err := h.client.GetV0CityByCityNameWithResponse(h.ctx, contractCityName)
			expectStatus(t, "city after refused unregister", city, err, http.StatusOK)
		},
	},
}

// contractAsyncFamily checks the table covers exactly the spec's 202
// operations, then runs every row.
func contractAsyncFamily(t *testing.T, h *contractHarness) {
	want := map[string]bool{}
	for _, op := range h.spec.Operations() {
		for _, status := range op.Statuses {
			if status == "202" {
				want[op.ID] = true
			}
		}
	}
	have := map[string]bool{}
	for _, tc := range contractAsyncCases {
		have[tc.operationID] = true
	}
	var missing, extra []string
	for id := range want {
		if !have[id] {
			missing = append(missing, id)
		}
	}
	for id := range have {
		if !want[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("202 operations without a terminal-outcome row in contractAsyncCases: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("contractAsyncCases rows for operations the spec no longer declares 202: %v", extra)
	}

	for _, tc := range contractAsyncCases {
		t.Run(tc.operationID, func(t *testing.T) {
			if tc.terminal == "" {
				t.Fatal("async case must document its terminal outcome")
			}
			tc.run(t, h)
			if h.transport.Exercised()[tc.operationID] == 0 {
				t.Fatalf("case never called %s", tc.operationID)
			}
		})
	}
}

func (h *contractHarness) raisePending(t *testing.T, sess genclient.SessionResponse) {
	t.Helper()
	h.sp.SetPendingInteraction(sess.SessionName, &runtimePendingApproval)
	pending, err := h.client.GetV0CityByCityNameSessionByIdPendingWithResponse(h.ctx, contractCityName, sess.Id)
	expectStatus(t, "session pending", pending, err, http.StatusOK)
	if p := mustJSON(t, "session pending", pending.JSON200, pending); p.Pending == nil || p.Pending.RequestId != runtimePendingApproval.RequestID {
		t.Fatalf("pending after interaction = %s", contractBody(pending))
	}
}

const (
	contractCloneHost = "contract-git.example.test"
	contractCloneURL  = "https://" + contractCloneHost + "/repo.git"
)

// contractAsyncRigCreate drives the git_url rig create. The two network
// boundaries are stubbed exactly as the capstone E2E stubs them: the clone
// materializes a plain working tree and the SSRF resolver maps the host to a
// public TEST-NET address so the real fence runs.
func contractAsyncRigCreate(t *testing.T, h *contractHarness) {
	origClone := rigCloneGit
	origResolver := ssrf.HostResolver
	t.Cleanup(func() {
		rigCloneGit = origClone
		ssrf.HostResolver = origResolver
	})
	rigCloneGit = func(_ context.Context, _, dst string, _ git.CloneOptions) error {
		h.clones.Add(1)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, "README.md"), []byte("contract clone\n"), 0o644)
	}
	ssrf.HostResolver = func(host string) ([]net.IP, error) {
		if host == contractCloneHost {
			return []net.IP{net.ParseIP("198.51.100.9")}, nil
		}
		return nil, fmt.Errorf("contract resolver: unexpected host %q", host)
	}

	requestID := "contract-rig-gamma"
	resp, err := h.client.CreateRigWithResponse(h.ctx, contractCityName, &genclient.CreateRigParams{XGCRequest: contractCSRF},
		genclient.CreateRigJSONRequestBody{Name: "gamma", GitUrl: ptr(contractCloneURL), Prefix: ptr("gm"), DefaultBranch: ptr("main"), RequestId: ptr(requestID)})
	expectStatus(t, "create rig (git_url)", resp, err, http.StatusAccepted)
	body := mustJSON(t, "create rig (git_url)", resp.JSON202, resp)
	if body.Status != genclient.Accepted || body.RequestId == nil || *body.RequestId != requestID || body.EventCursor == nil {
		t.Fatalf("202 rig create body = %s", contractBody(resp))
	}
	acc := contractAccepted{RequestID: *body.RequestId, EventCursor: *body.EventCursor}
	h.awaitSuccess(t, acc, events.RequestResultRigCreate)
	if h.clones.Load() != 1 {
		t.Fatalf("git clone boundary ran %d times, want 1", h.clones.Load())
	}
	rig := h.getRig(t, "gamma")
	if !strings.HasPrefix(rig.Path, h.cityPath) {
		t.Fatalf("cloned rig path %q is not under the city root", rig.Path)
	}

	// The same client request_id replays the terminal outcome (200 exists)
	// instead of re-cloning.
	replay, err := h.client.CreateRigWithResponse(h.ctx, contractCityName, &genclient.CreateRigParams{XGCRequest: contractCSRF},
		genclient.CreateRigJSONRequestBody{Name: "gamma", GitUrl: ptr(contractCloneURL), Prefix: ptr("gm"), DefaultBranch: ptr("main"), RequestId: body.RequestId})
	expectStatus(t, "replay rig create", replay, err, http.StatusOK)
	if h.clones.Load() != 1 {
		t.Fatalf("replayed request re-cloned (clones=%d)", h.clones.Load())
	}
}

// contractAsyncOrderRun fires the webhook-trigger exec order. Its 202 body
// carries the tracking bead id; the terminal outcome is order.completed for
// the scoped name and an order-history entry for the tracking bead.
func contractAsyncOrderRun(t *testing.T, h *contractHarness) {
	after := h.latestSeq()
	resp, err := h.client.PostV0CityByCityNameOrderByNameRunWithResponse(h.ctx, contractCityName, contractOrder,
		&genclient.PostV0CityByCityNameOrderByNameRunParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameOrderByNameRunJSONRequestBody{Vars: &map[string]string{"ticket": "T-1"}})
	expectStatus(t, "run order", resp, err, http.StatusAccepted)
	body := mustJSON(t, "run order", resp.JSON202, resp)
	if body.Status != "dispatched" || body.TrackingId == nil || body.ScopedName == nil {
		t.Fatalf("order run body = %s", contractBody(resp))
	}
	scoped := *body.ScopedName
	done := h.waitEvent(after, "order outcome for "+scoped, func(e events.Event) bool {
		return (e.Type == events.OrderCompleted || e.Type == events.OrderFailed) && e.Subject == scoped
	})
	if done.Type != events.OrderCompleted {
		t.Fatalf("order %s outcome = %s: %s", scoped, done.Type, done.Message)
	}

	history, err := h.client.GetV0CityByCityNameOrdersHistoryWithResponse(h.ctx, contractCityName,
		&genclient.GetV0CityByCityNameOrdersHistoryParams{ScopedName: scoped})
	expectStatus(t, "order history", history, err, http.StatusOK)
	var entry *genclient.OrderHistoryEntry
	for _, e := range derefSlice(mustJSON(t, "order history", history.JSON200, history).Entries) {
		if e.BeadId == *body.TrackingId {
			entry = &e
		}
	}
	if entry == nil {
		t.Fatalf("order history missing tracking bead %s: %s", *body.TrackingId, contractBody(history))
	}
	detail, err := h.client.GetV0CityByCityNameOrderHistoryByBeadIdWithResponse(h.ctx, contractCityName, entry.BeadId,
		&genclient.GetV0CityByCityNameOrderHistoryByBeadIdParams{StoreRef: ptr(entry.StoreRef)})
	expectStatus(t, "order history detail", detail, err, http.StatusOK)
}

// contractAsyncRunCancel cancels a workflow run.
func contractAsyncRunCancel(t *testing.T, h *contractHarness) {
	runID := h.createWorkflowRun(t)
	resp, err := h.client.PostV0CityByCityNameRunsByRunIdCancelWithResponse(h.ctx, contractCityName, runID,
		&genclient.PostV0CityByCityNameRunsByRunIdCancelParams{XGCRequest: contractCSRF})
	expectStatus(t, "cancel run", resp, err, http.StatusAccepted)
	body := mustJSON(t, "cancel run", resp.JSON202, resp)
	if body.RunId != runID || body.Status != genclient.RunStatusCanceled {
		t.Fatalf("cancel body = %s", contractBody(resp))
	}
	run, err := h.client.GetV0CityByCityNameRunsByRunIdWithResponse(h.ctx, contractCityName, runID)
	expectStatus(t, "get canceled run", run, err, http.StatusOK)
	if r := mustJSON(t, "get canceled run", run.JSON200, run); r.Status != genclient.RunStatusCanceled {
		t.Fatalf("run %s status after cancel = %q", runID, r.Status)
	}
	h.expectBeadStatus(t, runID, "closed")
	again, err := h.client.PostV0CityByCityNameRunsByRunIdCancelWithResponse(h.ctx, contractCityName, runID,
		&genclient.PostV0CityByCityNameRunsByRunIdCancelParams{XGCRequest: contractCSRF})
	expectStatus(t, "cancel terminal run", again, err, http.StatusConflict)
	missing, err := h.client.PostV0CityByCityNameRunsByRunIdCancelWithResponse(h.ctx, contractCityName, "hq-nosuchrun",
		&genclient.PostV0CityByCityNameRunsByRunIdCancelParams{XGCRequest: contractCSRF})
	expectStatus(t, "cancel missing run", missing, err, http.StatusNotFound)
}
