package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beadmeta"
)

const (
	contractFormula     = "contract-flow"
	contractFormulaTOML = "formula = \"contract-flow\"\nversion = 1\nphase = \"vapor\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Do contract work\"\n"
	// contractOrder is a webhook-trigger exec order written at harness
	// startup (orders/contract-hook.toml).
	contractOrder = "contract-hook"
)

// cityScope is the scope_kind/scope_ref pair the formula reads require.
func cityScope() (*string, *string) { return ptr("city"), ptr(contractCityName) }

// contractFormulasFamily covers formula authoring (upsert, validate, source,
// delete) and the formula reads (list, feed, detail, preview, runs), then
// slings the formula so the runs and workflow surfaces have a real run.
func contractFormulasFamily(t *testing.T, h *contractHarness) {
	c, ctx, city := h.client, h.ctx, contractCityName
	kind, ref := cityScope()

	put, err := c.PutV0CityByCityNameFormulasByNameWithBodyWithResponse(ctx, city, contractFormula,
		&genclient.PutV0CityByCityNameFormulasByNameParams{XGCRequest: contractCSRF},
		"application/octet-stream", strings.NewReader(contractFormulaTOML))
	expectStatus(t, "upsert formula", put, err, http.StatusOK)
	validate, err := c.PostV0CityByCityNameFormulasByNameValidateWithBodyWithResponse(ctx, city, contractFormula,
		&genclient.PostV0CityByCityNameFormulasByNameValidateParams{XGCRequest: contractCSRF},
		"application/octet-stream", strings.NewReader(contractFormulaTOML))
	expectStatus(t, "validate formula", validate, err, http.StatusOK)
	source, err := c.GetV0CityByCityNameFormulasByNameSourceWithResponse(ctx, city, contractFormula)
	expectStatus(t, "formula source", source, err, http.StatusOK)
	if !strings.Contains(string(source.Body), "Do contract work") {
		t.Fatalf("formula source does not round-trip the upsert: %s", source.Body)
	}

	list, err := c.GetV0CityByCityNameFormulasWithResponse(ctx, city, &genclient.GetV0CityByCityNameFormulasParams{ScopeKind: kind, ScopeRef: ref})
	expectStatus(t, "list formulas", list, err, http.StatusOK)
	if !strings.Contains(string(list.Body), `"`+contractFormula+`"`) {
		t.Fatalf("formula list missing %q: %s", contractFormula, list.Body)
	}
	feed, err := c.GetV0CityByCityNameFormulasFeedWithResponse(ctx, city, &genclient.GetV0CityByCityNameFormulasFeedParams{ScopeKind: kind, ScopeRef: ref})
	expectStatus(t, "formula feed", feed, err, http.StatusOK)
	detail, err := c.GetV0CityByCityNameFormulasByNameWithResponse(ctx, city, contractFormula,
		&genclient.GetV0CityByCityNameFormulasByNameParams{ScopeKind: kind, ScopeRef: ref, Target: contractAgent})
	expectStatus(t, "formula detail", detail, err, http.StatusOK)
	alias, err := c.GetV0CityByCityNameFormulaByNameWithResponse(ctx, city, contractFormula,
		&genclient.GetV0CityByCityNameFormulaByNameParams{ScopeKind: kind, ScopeRef: ref, Target: contractAgent})
	expectStatus(t, "formula detail (singular alias)", alias, err, http.StatusOK)
	preview, err := c.PostV0CityByCityNameFormulasByNamePreviewWithResponse(ctx, city, contractFormula,
		&genclient.PostV0CityByCityNameFormulasByNamePreviewParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameFormulasByNamePreviewJSONRequestBody{ScopeKind: kind, ScopeRef: ref, Target: contractAgent})
	expectStatus(t, "formula preview", preview, err, http.StatusOK)

	// Sling the (root-only, vapor) formula onto the pool template.
	h.slingFormula(t)

	// A graph.v2 workflow root created through the bead API is a run: it
	// must be visible on the run, formula-runs, and workflow reads.
	runID := h.createWorkflowRun(t)
	runs, err := c.GetV0CityByCityNameRunsWithResponse(ctx, city, nil)
	expectStatus(t, "list runs", runs, err, http.StatusOK)
	if !strings.Contains(string(runs.Body), runID) {
		t.Fatalf("runs list missing %s: %s", runID, runs.Body)
	}
	census, err := c.GetV0CityByCityNameRunsCensusWithResponse(ctx, city)
	// The census is served from the supervisor's shared run-census source;
	// a single-city controller mux has none and answers the documented 503.
	expectStatus(t, "runs census", census, err, http.StatusOK, http.StatusServiceUnavailable)
	run, err := c.GetV0CityByCityNameRunsByRunIdWithResponse(ctx, city, runID)
	expectStatus(t, "get run", run, err, http.StatusOK)
	if r := mustJSON(t, "get run", run.JSON200, run); r.RunId != runID {
		t.Fatalf("run id = %q, want %q", r.RunId, runID)
	}
	steps, err := c.GetV0CityByCityNameRunsByRunIdStepsWithResponse(ctx, city, runID)
	expectStatus(t, "run steps", steps, err, http.StatusOK)
	formulaRuns, err := c.GetV0CityByCityNameFormulasByNameRunsWithResponse(ctx, city, contractFormula,
		&genclient.GetV0CityByCityNameFormulasByNameRunsParams{ScopeKind: kind, ScopeRef: ref})
	expectStatus(t, "formula runs", formulaRuns, err, http.StatusOK)
	workflow, err := c.GetV0CityByCityNameWorkflowByWorkflowIdWithResponse(ctx, city, runID,
		&genclient.GetV0CityByCityNameWorkflowByWorkflowIdParams{ScopeKind: kind, ScopeRef: ref})
	expectStatus(t, "get workflow", workflow, err, http.StatusOK)
	deleted, err := c.DeleteV0CityByCityNameWorkflowByWorkflowIdWithResponse(ctx, city, runID,
		&genclient.DeleteV0CityByCityNameWorkflowByWorkflowIdParams{ScopeKind: kind, ScopeRef: ref, XGCRequest: contractCSRF})
	expectStatus(t, "delete workflow", deleted, err, http.StatusOK)
	h.expectBeadStatus(t, runID, "closed")

	// A city-local formula can be deleted; reads then 404.
	scratch, err := c.PutV0CityByCityNameFormulasByNameWithBodyWithResponse(ctx, city, "contract-scratch",
		&genclient.PutV0CityByCityNameFormulasByNameParams{XGCRequest: contractCSRF},
		"application/octet-stream", strings.NewReader("formula = \"contract-scratch\"\n"))
	expectStatus(t, "upsert scratch formula", scratch, err, http.StatusOK)
	removed, err := c.DeleteV0CityByCityNameFormulasByNameWithResponse(ctx, city, "contract-scratch",
		&genclient.DeleteV0CityByCityNameFormulasByNameParams{XGCRequest: contractCSRF})
	expectStatus(t, "delete formula", removed, err, http.StatusOK)
	gone, err := c.GetV0CityByCityNameFormulasByNameSourceWithResponse(ctx, city, "contract-scratch")
	expectStatus(t, "deleted formula source", gone, err, http.StatusNotFound)
}

// slingFormula slings contractFormula onto the harness pool template.
func (h *contractHarness) slingFormula(t *testing.T) {
	t.Helper()
	slung, err := h.client.PostV0CityByCityNameSlingWithResponse(h.ctx, contractCityName,
		&genclient.PostV0CityByCityNameSlingParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameSlingJSONRequestBody{Target: contractAgent, Formula: ptr(contractFormula), Title: ptr("contract formula run")})
	expectStatus(t, "sling formula", slung, err, http.StatusOK)
	resp := mustJSON(t, "sling formula", slung.JSON200, slung)
	if resp.Formula == nil || *resp.Formula != contractFormula || resp.Target != contractAgent {
		t.Fatalf("formula sling response = %s", contractBody(slung))
	}
}

// createWorkflowRun creates a graph.v2 workflow root (the shape molecule
// instantiation stamps) through the bead API and returns its id, which is
// the run id.
func (h *contractHarness) createWorkflowRun(t *testing.T) string {
	t.Helper()
	created, err := h.client.CreateBeadWithResponse(h.ctx, contractCityName, &genclient.CreateBeadParams{XGCRequest: contractCSRF},
		genclient.CreateBeadJSONRequestBody{
			Title: "contract workflow run",
			Type:  ptr("task"),
			Metadata: &map[string]string{
				beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
				beadmeta.FormulaMetadataKey:         contractFormula,
				beadmeta.FormulaContractMetadataKey: "graph.v2",
				beadmeta.WorkflowIDMetadataKey:      "contract-workflow",
			},
		})
	expectStatus(t, "create workflow root", created, err, http.StatusCreated)
	return mustJSON(t, "create workflow root", created.JSON201, created).Id
}

// contractConvoysFamily covers the convoy lifecycle with membership checked
// on the read path.
func contractConvoysFamily(t *testing.T, h *contractHarness) {
	c, ctx, city := h.client, h.ctx, contractCityName
	var items []string
	for _, title := range []string{"convoy item one", "convoy item two"} {
		b, err := c.CreateBeadWithResponse(ctx, city, &genclient.CreateBeadParams{XGCRequest: contractCSRF},
			genclient.CreateBeadJSONRequestBody{Title: title})
		expectStatus(t, "create convoy item", b, err, http.StatusCreated)
		items = append(items, mustJSON(t, "create convoy item", b.JSON201, b).Id)
	}
	created, err := c.CreateConvoyWithResponse(ctx, city, &genclient.CreateConvoyParams{XGCRequest: contractCSRF},
		genclient.CreateConvoyJSONRequestBody{Title: "contract convoy", Items: &[]string{items[0]}})
	expectStatus(t, "create convoy", created, err, http.StatusCreated)
	convoyID := mustJSON(t, "create convoy", created.JSON201, created).Id

	list, err := c.GetV0CityByCityNameConvoysWithResponse(ctx, city, nil)
	expectStatus(t, "list convoys", list, err, http.StatusOK)
	if !strings.Contains(string(list.Body), convoyID) {
		t.Fatalf("convoy list missing %s: %s", convoyID, list.Body)
	}
	added, err := c.PostV0CityByCityNameConvoyByIdAddWithResponse(ctx, city, convoyID,
		&genclient.PostV0CityByCityNameConvoyByIdAddParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameConvoyByIdAddJSONRequestBody{Items: &[]string{items[1]}})
	expectStatus(t, "add convoy item", added, err, http.StatusOK)
	h.expectConvoyChildren(t, convoyID, items...)
	removed, err := c.PostV0CityByCityNameConvoyByIdRemoveWithResponse(ctx, city, convoyID,
		&genclient.PostV0CityByCityNameConvoyByIdRemoveParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameConvoyByIdRemoveJSONRequestBody{Items: &[]string{items[1]}})
	expectStatus(t, "remove convoy item", removed, err, http.StatusOK)
	h.expectConvoyChildren(t, convoyID, items[0])
	check, err := c.GetV0CityByCityNameConvoyByIdCheckWithResponse(ctx, city, convoyID)
	expectStatus(t, "check convoy", check, err, http.StatusOK)
	closed, err := c.PostV0CityByCityNameConvoyByIdCloseWithResponse(ctx, city, convoyID,
		&genclient.PostV0CityByCityNameConvoyByIdCloseParams{XGCRequest: contractCSRF})
	expectStatus(t, "close convoy", closed, err, http.StatusOK)
	h.expectBeadStatus(t, convoyID, "closed")

	// DELETE is a soft delete (like DELETE /bead/{id}): the convoy closes and
	// stays readable.
	second, err := c.CreateConvoyWithResponse(ctx, city, &genclient.CreateConvoyParams{XGCRequest: contractCSRF},
		genclient.CreateConvoyJSONRequestBody{Title: "contract convoy to delete"})
	expectStatus(t, "create second convoy", second, err, http.StatusCreated)
	secondID := mustJSON(t, "create second convoy", second.JSON201, second).Id
	deleted, err := c.DeleteV0CityByCityNameConvoyByIdWithResponse(ctx, city, secondID,
		&genclient.DeleteV0CityByCityNameConvoyByIdParams{XGCRequest: contractCSRF})
	expectStatus(t, "delete convoy", deleted, err, http.StatusOK)
	h.expectBeadStatus(t, secondID, "closed")
	missing, err := c.GetV0CityByCityNameConvoyByIdWithResponse(ctx, city, "hq-nosuchconvoy")
	expectStatus(t, "get missing convoy", missing, err, http.StatusNotFound)
}

func (h *contractHarness) expectConvoyChildren(t *testing.T, convoyID string, want ...string) {
	t.Helper()
	got, err := h.client.GetV0CityByCityNameConvoyByIdWithResponse(h.ctx, contractCityName, convoyID)
	expectStatus(t, "get convoy", got, err, http.StatusOK)
	body := mustJSON(t, "get convoy", got.JSON200, got)
	children := derefSlice(body.Children)
	if len(children) != len(want) {
		t.Fatalf("convoy %s children = %s, want %v", convoyID, contractBody(got), want)
	}
	for _, id := range want {
		if !contractBeadListed(body.Children, id) {
			t.Fatalf("convoy %s missing child %s: %s", convoyID, id, contractBody(got))
		}
	}
}

// contractOrdersFamily covers order reads and enable/disable. The 202 run
// path is a P1-5 case in api_contract_async_test.go.
func contractOrdersFamily(t *testing.T, h *contractHarness) {
	c, ctx, city := h.client, h.ctx, contractCityName
	list, err := c.GetV0CityByCityNameOrdersWithResponse(ctx, city)
	expectStatus(t, "list orders", list, err, http.StatusOK)
	if !strings.Contains(string(list.Body), `"`+contractOrder+`"`) {
		t.Fatalf("order list missing %q: %s", contractOrder, list.Body)
	}
	check, err := c.GetV0CityByCityNameOrdersCheckWithResponse(ctx, city, nil)
	expectStatus(t, "check orders", check, err, http.StatusOK)
	kind, ref := cityScope()
	feed, err := c.GetV0CityByCityNameOrdersFeedWithResponse(ctx, city, &genclient.GetV0CityByCityNameOrdersFeedParams{ScopeKind: kind, ScopeRef: ref})
	expectStatus(t, "orders feed", feed, err, http.StatusOK)
	got, err := c.GetV0CityByCityNameOrderByNameWithResponse(ctx, city, contractOrder)
	expectStatus(t, "get order", got, err, http.StatusOK)
	disabled, err := c.PostV0CityByCityNameOrderByNameDisableWithResponse(ctx, city, contractOrder,
		&genclient.PostV0CityByCityNameOrderByNameDisableParams{XGCRequest: contractCSRF})
	expectStatus(t, "disable order", disabled, err, http.StatusOK)
	enabled, err := c.PostV0CityByCityNameOrderByNameEnableWithResponse(ctx, city, contractOrder,
		&genclient.PostV0CityByCityNameOrderByNameEnableParams{XGCRequest: contractCSRF})
	expectStatus(t, "enable order", enabled, err, http.StatusOK)
	missing, err := c.GetV0CityByCityNameOrderByNameWithResponse(ctx, city, "no-such-order")
	expectStatus(t, "get missing order", missing, err, http.StatusNotFound)
}
