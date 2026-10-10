package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func ptr[T any](v T) *T { return &v }

// contractBeadsFamily drives the bead lifecycle through the API and checks
// that each mutation is visible on the read paths (not just that it
// returned 2xx): create (city and rig routing), get, list, ready, patch,
// update, parent/child deps and graph, close, reopen, delete.
func contractBeadsFamily(t *testing.T, h *contractHarness) {
	c, ctx := h.client, h.ctx
	city := contractCityName

	created, err := c.CreateBeadWithResponse(ctx, city, &genclient.CreateBeadParams{XGCRequest: contractCSRF},
		genclient.CreateBeadJSONRequestBody{Title: "contract root", Labels: &[]string{"contract"}})
	expectStatus(t, "create city bead", created, err, http.StatusCreated)
	root := mustJSON(t, "create city bead", created.JSON201, created)
	if !strings.HasPrefix(root.Id, "hq-") || root.Status != "open" {
		t.Fatalf("city bead = %+v, want an open hq- bead", root)
	}

	rigCreated, err := c.CreateBeadWithResponse(ctx, city, &genclient.CreateBeadParams{XGCRequest: contractCSRF},
		genclient.CreateBeadJSONRequestBody{Title: "contract rig bead", Rig: ptr(contractRig)})
	expectStatus(t, "create rig bead", rigCreated, err, http.StatusCreated)
	rigBead := mustJSON(t, "create rig bead", rigCreated.JSON201, rigCreated)
	if !strings.HasPrefix(rigBead.Id, "al-") {
		t.Fatalf("rig bead id = %q, want the rig prefix al-", rigBead.Id)
	}

	child, err := c.CreateBeadWithResponse(ctx, city, &genclient.CreateBeadParams{XGCRequest: contractCSRF},
		genclient.CreateBeadJSONRequestBody{Title: "contract child", Parent: ptr(root.Id)})
	expectStatus(t, "create child bead", child, err, http.StatusCreated)
	childBead := mustJSON(t, "create child bead", child.JSON201, child)

	got, err := c.GetV0CityByCityNameBeadByIdWithResponse(ctx, city, root.Id)
	expectStatus(t, "get bead", got, err, http.StatusOK)
	if b := mustJSON(t, "get bead", got.JSON200, got); b.Title != "contract root" {
		t.Fatalf("get bead title = %q", b.Title)
	}

	list, err := c.GetV0CityByCityNameBeadsWithResponse(ctx, city, &genclient.GetV0CityByCityNameBeadsParams{All: ptr(true)})
	expectStatus(t, "list beads", list, err, http.StatusOK)
	items := mustJSON(t, "list beads", list.JSON200, list)
	for _, id := range []string{root.Id, rigBead.Id, childBead.Id} {
		if !contractBeadListed(items.Items, id) {
			t.Fatalf("bead list missing %s: %s", id, contractBody(list))
		}
	}
	rigList, err := c.GetV0CityByCityNameBeadsWithResponse(ctx, city, &genclient.GetV0CityByCityNameBeadsParams{Rig: ptr(contractRig)})
	expectStatus(t, "list rig beads", rigList, err, http.StatusOK)
	rigItems := mustJSON(t, "list rig beads", rigList.JSON200, rigList)
	if !contractBeadListed(rigItems.Items, rigBead.Id) || contractBeadListed(rigItems.Items, root.Id) {
		t.Fatalf("rig-filtered list = %s, want only rig beads", contractBody(rigList))
	}

	ready, err := c.GetV0CityByCityNameBeadsReadyWithResponse(ctx, city, nil)
	expectStatus(t, "ready beads", ready, err, http.StatusOK)
	if r := mustJSON(t, "ready beads", ready.JSON200, ready); !contractBeadListed(r.Items, rigBead.Id) {
		t.Fatalf("ready list missing open unblocked bead %s: %s", rigBead.Id, contractBody(ready))
	}

	patched, err := c.PatchV0CityByCityNameBeadByIdWithResponse(ctx, city, root.Id,
		&genclient.PatchV0CityByCityNameBeadByIdParams{XGCRequest: contractCSRF},
		genclient.PatchV0CityByCityNameBeadByIdJSONRequestBody{Title: ptr("contract root renamed"), Priority: ptr(int64(1))})
	expectStatus(t, "patch bead", patched, err, http.StatusOK)
	updated, err := c.PostV0CityByCityNameBeadByIdUpdateWithResponse(ctx, city, root.Id,
		&genclient.PostV0CityByCityNameBeadByIdUpdateParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameBeadByIdUpdateJSONRequestBody{Labels: &[]string{"updated"}, RemoveLabels: &[]string{"contract"}})
	expectStatus(t, "update bead", updated, err, http.StatusOK)
	// Assignment needs a concrete session id; the sessions family covers it.
	unassignable, err := c.PostV0CityByCityNameBeadByIdAssignWithResponse(ctx, city, root.Id,
		&genclient.PostV0CityByCityNameBeadByIdAssignParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameBeadByIdAssignJSONRequestBody{Assignee: ptr(contractAgent)})
	expectStatus(t, "assign bead to a template name", unassignable, err, http.StatusBadRequest)

	reread, err := c.GetV0CityByCityNameBeadByIdWithResponse(ctx, city, root.Id)
	expectStatus(t, "reread bead", reread, err, http.StatusOK)
	rb := mustJSON(t, "reread bead", reread.JSON200, reread)
	if rb.Title != "contract root renamed" || rb.Priority == nil || *rb.Priority != 1 ||
		rb.Labels == nil || strings.Join(*rb.Labels, ",") != "updated" {
		t.Fatalf("bead after patch/update = %s", contractBody(reread))
	}

	deps, err := c.GetV0CityByCityNameBeadByIdDepsWithResponse(ctx, city, root.Id)
	expectStatus(t, "bead deps", deps, err, http.StatusOK)
	if d := mustJSON(t, "bead deps", deps.JSON200, deps); !contractBeadListed(d.Children, childBead.Id) {
		t.Fatalf("deps of %s missing child %s: %s", root.Id, childBead.Id, contractBody(deps))
	}
	graph, err := c.GetV0CityByCityNameBeadsGraphByRootIdWithResponse(ctx, city, root.Id)
	expectStatus(t, "bead graph", graph, err, http.StatusOK)
	if g := mustJSON(t, "bead graph", graph.JSON200, graph); g.Root.Id != root.Id {
		t.Fatalf("graph root = %q, want %q", g.Root.Id, root.Id)
	}

	closed, err := c.PostV0CityByCityNameBeadByIdCloseWithResponse(ctx, city, childBead.Id,
		&genclient.PostV0CityByCityNameBeadByIdCloseParams{XGCRequest: contractCSRF},
		genclient.BeadCloseBody{})
	expectStatus(t, "close bead", closed, err, http.StatusOK)
	h.expectBeadStatus(t, childBead.Id, "closed")
	reopened, err := c.PostV0CityByCityNameBeadByIdReopenWithResponse(ctx, city, childBead.Id,
		&genclient.PostV0CityByCityNameBeadByIdReopenParams{XGCRequest: contractCSRF})
	expectStatus(t, "reopen bead", reopened, err, http.StatusOK)
	h.expectBeadStatus(t, childBead.Id, "open")

	deleted, err := c.DeleteV0CityByCityNameBeadByIdWithResponse(ctx, city, childBead.Id,
		&genclient.DeleteV0CityByCityNameBeadByIdParams{XGCRequest: contractCSRF})
	expectStatus(t, "delete bead", deleted, err, http.StatusOK)

	missing, err := c.GetV0CityByCityNameBeadByIdWithResponse(ctx, city, "hq-doesnotexist")
	expectStatus(t, "get missing bead", missing, err, http.StatusNotFound)
}

// contractSlingFamily slings a rig bead to the configured agent template and
// checks the bead is routed to it.
func contractSlingFamily(t *testing.T, h *contractHarness) {
	c, ctx := h.client, h.ctx
	created, err := c.CreateBeadWithResponse(ctx, contractCityName, &genclient.CreateBeadParams{XGCRequest: contractCSRF},
		genclient.CreateBeadJSONRequestBody{Title: "contract sling work"})
	expectStatus(t, "create sling bead", created, err, http.StatusCreated)
	bead := mustJSON(t, "create sling bead", created.JSON201, created)

	slung, err := c.PostV0CityByCityNameSlingWithResponse(ctx, contractCityName,
		&genclient.PostV0CityByCityNameSlingParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameSlingJSONRequestBody{Target: contractAgent, Bead: ptr(bead.Id), NoFormula: ptr(true)})
	expectStatus(t, "sling", slung, err, http.StatusOK)
	resp := mustJSON(t, "sling", slung.JSON200, slung)
	if resp.Target != contractAgent {
		t.Fatalf("sling target = %q, want %q", resp.Target, contractAgent)
	}

	got, err := c.GetV0CityByCityNameBeadByIdWithResponse(ctx, contractCityName, bead.Id)
	expectStatus(t, "get slung bead", got, err, http.StatusOK)
	b := mustJSON(t, "get slung bead", got.JSON200, got)
	if b.Metadata == nil || (*b.Metadata)["gc.routed_to"] != contractAgent {
		t.Fatalf("slung bead not routed to %q: %s", contractAgent, contractBody(got))
	}

	unknown, err := c.PostV0CityByCityNameSlingWithResponse(ctx, contractCityName,
		&genclient.PostV0CityByCityNameSlingParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameSlingJSONRequestBody{Target: "no-such-agent", Bead: ptr(bead.Id), NoFormula: ptr(true)})
	expectStatus(t, "sling unknown target", unknown, err, http.StatusNotFound, http.StatusBadRequest)
}

func (h *contractHarness) expectBeadStatus(t *testing.T, id, want string) {
	t.Helper()
	got, err := h.client.GetV0CityByCityNameBeadByIdWithResponse(h.ctx, contractCityName, id)
	expectStatus(t, "get bead "+id, got, err, http.StatusOK)
	if b := mustJSON(t, "get bead "+id, got.JSON200, got); b.Status != want {
		t.Fatalf("bead %s status = %q, want %q", id, b.Status, want)
	}
}

func contractBeadListed(items *[]genclient.Bead, id string) bool {
	if items == nil {
		return false
	}
	for _, b := range *items {
		if b.Id == id {
			return true
		}
	}
	return false
}
