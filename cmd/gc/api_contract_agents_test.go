package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// contractAgentsFamily covers agent template reads and the config-editing
// agent CRUD surface (city-scoped and rig-qualified), each mutation checked
// on the read path.
func contractAgentsFamily(t *testing.T, h *contractHarness) {
	c, ctx, city := h.client, h.ctx, contractCityName

	list, err := c.GetV0CityByCityNameAgentsWithResponse(ctx, city, nil)
	expectStatus(t, "list agents", list, err, http.StatusOK)
	if !contractAgentListed(t, h, contractAgent) {
		t.Fatalf("agent list missing %q: %s", contractAgent, contractBody(list))
	}
	got, err := c.GetV0CityByCityNameAgentByBaseWithResponse(ctx, city, contractAgent)
	expectStatus(t, "get agent", got, err, http.StatusOK)
	output, err := c.GetV0CityByCityNameAgentByBaseOutputWithResponse(ctx, city, contractAgent, nil)
	expectStatus(t, "agent output", output, err, http.StatusOK, http.StatusNotFound)
	h.openStreamOrStatus(t, "agent output stream", func(ctx context.Context) (*http.Response, error) {
		return c.StreamAgentOutput(ctx, city, contractAgent)
	}, http.StatusNotFound)

	// A provider missing from the city's provider catalog is a client error:
	// 400, with city.toml left untouched (the agent must not appear).
	badProvider, err := c.CreateAgentWithResponse(ctx, city, &genclient.CreateAgentParams{XGCRequest: contractCSRF},
		genclient.CreateAgentJSONRequestBody{Name: "orphan", Provider: "no-such-provider"})
	expectStatus(t, "create agent with unknown provider", badProvider, err, http.StatusBadRequest)
	orphan, err := c.GetV0CityByCityNameAgentByBaseWithResponse(ctx, city, "orphan")
	expectStatus(t, "get rejected agent", orphan, err, http.StatusNotFound)

	// City-scoped CRUD.
	created, err := c.CreateAgentWithResponse(ctx, city, &genclient.CreateAgentParams{XGCRequest: contractCSRF},
		genclient.CreateAgentJSONRequestBody{Name: "helper", Provider: "claude"})
	expectStatus(t, "create agent", created, err, http.StatusCreated)
	// Read-after-write through the item read. (The list read is served from
	// a response cache keyed by the event index, which config mutations do
	// not advance — see the PR's bug list.)
	helper, err := c.GetV0CityByCityNameAgentByBaseWithResponse(ctx, city, "helper")
	expectStatus(t, "get created agent", helper, err, http.StatusOK)
	dup, err := c.CreateAgentWithResponse(ctx, city, &genclient.CreateAgentParams{XGCRequest: contractCSRF},
		genclient.CreateAgentJSONRequestBody{Name: "helper", Provider: "claude"})
	expectStatus(t, "create duplicate agent", dup, err, http.StatusConflict)
	patched, err := c.PatchV0CityByCityNameAgentByBaseWithResponse(ctx, city, "helper",
		&genclient.PatchV0CityByCityNameAgentByBaseParams{XGCRequest: contractCSRF},
		genclient.PatchV0CityByCityNameAgentByBaseJSONRequestBody{Suspended: ptr(true)})
	expectStatus(t, "patch agent", patched, err, http.StatusOK)
	h.expectAgentSuspended(t, "helper", true)
	resumed, err := c.PostV0CityByCityNameAgentByBaseByActionWithResponse(ctx, city, "helper",
		genclient.PostV0CityByCityNameAgentByBaseByActionParamsActionResume,
		&genclient.PostV0CityByCityNameAgentByBaseByActionParams{XGCRequest: contractCSRF})
	expectStatus(t, "resume agent", resumed, err, http.StatusOK)
	h.expectAgentSuspended(t, "helper", false)
	deleted, err := c.DeleteV0CityByCityNameAgentByBaseWithResponse(ctx, city, "helper",
		&genclient.DeleteV0CityByCityNameAgentByBaseParams{XGCRequest: contractCSRF})
	expectStatus(t, "delete agent", deleted, err, http.StatusOK)
	gone, err := c.GetV0CityByCityNameAgentByBaseWithResponse(ctx, city, "helper")
	expectStatus(t, "get deleted agent", gone, err, http.StatusNotFound)

	// Rig-qualified routes. Schema-2 cities declare rig-scoped agents in pack
	// config, so the API refuses to create one (documented 400); the
	// qualified reads and edits act on the declared alpha/rigbot.
	rigCreated, err := c.CreateAgentWithResponse(ctx, city, &genclient.CreateAgentParams{XGCRequest: contractCSRF},
		genclient.CreateAgentJSONRequestBody{Name: "rigtwo", Provider: "claude", Dir: ptr(contractRig)})
	expectStatus(t, "create rig agent", rigCreated, err, http.StatusBadRequest)
	rigGot, err := c.GetV0CityByCityNameAgentByDirByBaseWithResponse(ctx, city, contractRig, contractRigAgent)
	expectStatus(t, "get rig agent", rigGot, err, http.StatusOK)
	rigOutput, err := c.GetV0CityByCityNameAgentByDirByBaseOutputWithResponse(ctx, city, contractRig, contractRigAgent, nil)
	expectStatus(t, "rig agent output", rigOutput, err, http.StatusOK, http.StatusNotFound)
	h.openStreamOrStatus(t, "rig agent output stream", func(ctx context.Context) (*http.Response, error) {
		return c.StreamAgentOutputQualified(ctx, city, contractRig, contractRigAgent)
	}, http.StatusNotFound)
	// alpha/rigbot is the alpha instance of the generic scope="rig" template
	// rigbot: it has no config of its own, so the qualified edits address the
	// template (as the CLI does) and the read reflects them.
	rigSuspended, err := c.PostV0CityByCityNameAgentByDirByBaseByActionWithResponse(ctx, city, contractRig, contractRigAgent,
		genclient.PostV0CityByCityNameAgentByDirByBaseByActionParamsActionSuspend,
		&genclient.PostV0CityByCityNameAgentByDirByBaseByActionParams{XGCRequest: contractCSRF})
	expectStatus(t, "suspend rig agent", rigSuspended, err, http.StatusOK)
	h.expectQualifiedAgentSuspended(t, true)
	// PATCH reaches the template too, and answers what PATCH on the template
	// answers: the editor refuses field edits of a rig-scoped convention
	// agent (documented 400), leaving it suspended.
	rigPatched, err := c.PatchV0CityByCityNameAgentByDirByBaseWithResponse(ctx, city, contractRig, contractRigAgent,
		&genclient.PatchV0CityByCityNameAgentByDirByBaseParams{XGCRequest: contractCSRF},
		genclient.PatchV0CityByCityNameAgentByDirByBaseJSONRequestBody{Suspended: ptr(false)})
	expectStatus(t, "patch rig agent", rigPatched, err, http.StatusBadRequest)
	templatePatched, err := c.PatchV0CityByCityNameAgentByBaseWithResponse(ctx, city, contractRigAgent,
		&genclient.PatchV0CityByCityNameAgentByBaseParams{XGCRequest: contractCSRF},
		genclient.PatchV0CityByCityNameAgentByBaseJSONRequestBody{Suspended: ptr(false)})
	expectStatus(t, "patch rig template", templatePatched, err, http.StatusBadRequest)
	h.expectQualifiedAgentSuspended(t, true)
	rigResumed, err := c.PostV0CityByCityNameAgentByDirByBaseByActionWithResponse(ctx, city, contractRig, contractRigAgent,
		genclient.PostV0CityByCityNameAgentByDirByBaseByActionParamsActionResume,
		&genclient.PostV0CityByCityNameAgentByDirByBaseByActionParams{XGCRequest: contractCSRF})
	expectStatus(t, "resume rig agent", rigResumed, err, http.StatusOK)
	h.expectQualifiedAgentSuspended(t, false)
	// Deleting the shared template through one rig's instance is refused.
	rigDeleted, err := c.DeleteV0CityByCityNameAgentByDirByBaseWithResponse(ctx, city, contractRig, contractRigAgent,
		&genclient.DeleteV0CityByCityNameAgentByDirByBaseParams{XGCRequest: contractCSRF})
	expectStatus(t, "delete rig agent", rigDeleted, err, http.StatusConflict)
	stillThere, err := c.GetV0CityByCityNameAgentByDirByBaseWithResponse(ctx, city, contractRig, contractRigAgent)
	expectStatus(t, "get rig agent after refused delete", stillThere, err, http.StatusOK)

	missing, err := c.GetV0CityByCityNameAgentByBaseWithResponse(ctx, city, "no-such-agent")
	expectStatus(t, "get missing agent", missing, err, http.StatusNotFound)
}

func contractAgentListed(t *testing.T, h *contractHarness, name string) bool {
	t.Helper()
	list, err := h.client.GetV0CityByCityNameAgentsWithResponse(h.ctx, contractCityName, nil)
	expectStatus(t, "list agents", list, err, http.StatusOK)
	for _, a := range derefSlice(mustJSON(t, "list agents", list.JSON200, list).Items) {
		if a.Name == name || (a.Pool != nil && *a.Pool == name) {
			return true
		}
	}
	return false
}

func (h *contractHarness) expectQualifiedAgentSuspended(t *testing.T, want bool) {
	t.Helper()
	h.expectAgentSuspendedAt(t, contractRig+"/"+contractRigAgent, want, func() (contractResponse, *genclient.AgentResponse, error) {
		r, err := h.client.GetV0CityByCityNameAgentByDirByBaseWithResponse(h.ctx, contractCityName, contractRig, contractRigAgent)
		if err != nil {
			return nil, nil, err
		}
		return r, r.JSON200, nil
	})
}

func (h *contractHarness) expectAgentSuspended(t *testing.T, name string, want bool) {
	t.Helper()
	h.expectAgentSuspendedAt(t, name, want, func() (contractResponse, *genclient.AgentResponse, error) {
		r, err := h.client.GetV0CityByCityNameAgentByBaseWithResponse(h.ctx, contractCityName, name)
		if err != nil {
			return nil, nil, err
		}
		return r, r.JSON200, nil
	})
}

func (h *contractHarness) expectAgentSuspendedAt(t *testing.T, name string, want bool, get func() (contractResponse, *genclient.AgentResponse, error)) {
	t.Helper()
	h.readAfterWrite(t, fmt.Sprintf("agent %s suspended=%v", name, want), func() (bool, string) {
		resp, body, err := get()
		if err != nil {
			t.Fatalf("get agent %s: %v", name, err)
		}
		if resp.StatusCode() != http.StatusOK || body == nil {
			t.Fatalf("get agent %s: status %d: %s", name, resp.StatusCode(), contractBody(resp))
		}
		return body.Suspended == want, contractBody(resp)
	})
}

// openStreamOrStatus is openStream for streams that may legitimately answer
// one of the listed documented error statuses (for example 404 when the
// target has no live session).
func (h *contractHarness) openStreamOrStatus(t *testing.T, what string, open func(ctx context.Context) (*http.Response, error), allowed ...int) {
	t.Helper()
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	resp, err := open(ctx)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	for _, code := range allowed {
		if resp.StatusCode == code {
			return
		}
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d, want 200 or %v", what, resp.StatusCode, allowed)
	}
}
