package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

func TestHandleOrderList_Empty(t *testing.T) {
	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/orders"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Orders []orderResponse `json:"orders"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Orders) != 0 {
		t.Errorf("len(orders) = %d, want 0", len(resp.Orders))
	}
}

func TestHandleOrderList(t *testing.T) {
	fs := newFakeState(t)
	enabled := true
	fs.autos = []orders.Order{
		{
			Name:        "dolt-health",
			Description: "Check dolt status",
			Exec:        "dolt status",
			Trigger:     "cooldown",
			Interval:    "5m",
			Enabled:     &enabled,
		},
		{
			Name:    "deploy",
			Formula: "deploy-steps",
			Trigger: "manual",
			Pool:    "workers",
			Rig:     "myrig",
		},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/orders"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Orders []orderResponse `json:"orders"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Orders) != 2 {
		t.Fatalf("len(orders) = %d, want 2", len(resp.Orders))
	}

	a0 := resp.Orders[0]
	if a0.Name != "dolt-health" {
		t.Errorf("name = %q, want %q", a0.Name, "dolt-health")
	}
	if a0.Type != "exec" {
		t.Errorf("type = %q, want %q", a0.Type, "exec")
	}
	if a0.Trigger != "cooldown" {
		t.Errorf("trigger = %q, want %q", a0.Trigger, "cooldown")
	}
	if a0.Interval != "5m" {
		t.Errorf("interval = %q, want %q", a0.Interval, "5m")
	}
	if !a0.Enabled {
		t.Error("expected enabled=true")
	}

	a1 := resp.Orders[1]
	if a1.Name != "deploy" {
		t.Errorf("name = %q, want %q", a1.Name, "deploy")
	}
	if a1.Type != "formula" {
		t.Errorf("type = %q, want %q", a1.Type, "formula")
	}
	if a1.Rig != "myrig" {
		t.Errorf("rig = %q, want %q", a1.Rig, "myrig")
	}
	if a1.Pool != "workers" {
		t.Errorf("pool = %q, want %q", a1.Pool, "workers")
	}
}

func TestHandleOrderGet(t *testing.T) {
	fs := newFakeState(t)
	fs.autos = []orders.Order{
		{
			Name:        "dolt-health",
			Description: "Check dolt status",
			Exec:        "dolt status",
			Trigger:     "cooldown",
			Interval:    "5m",
		},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/order/dolt-health"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp orderResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Name != "dolt-health" {
		t.Errorf("name = %q, want %q", resp.Name, "dolt-health")
	}
	if resp.Type != "exec" {
		t.Errorf("type = %q, want %q", resp.Type, "exec")
	}
}

func TestHandleOrderGet_ExposesTriggerAndLegacyGateAlias(t *testing.T) {
	fs := newFakeState(t)
	fs.autos = []orders.Order{
		{
			Name:     "dolt-health",
			Exec:     "dolt status",
			Trigger:  "cooldown",
			Interval: "5m",
		},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/order/dolt-health"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["trigger"] != "cooldown" {
		t.Fatalf("trigger = %#v, want %q", resp["trigger"], "cooldown")
	}
	if resp["gate"] != "cooldown" {
		t.Fatalf("gate = %#v, want %q", resp["gate"], "cooldown")
	}
}

func TestHandleOrderGet_ScopedName(t *testing.T) {
	fs := newFakeState(t)
	fs.autos = []orders.Order{
		{
			Name:    "health",
			Exec:    "echo ok",
			Trigger: "cooldown",
			Rig:     "myrig",
		},
	}
	h := newTestCityHandler(t, fs)

	// Match by scoped name: health:rig:myrig
	req := httptest.NewRequest("GET", cityURL(fs, "/order/health:rig:myrig"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp orderResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Name != "health" {
		t.Errorf("name = %q, want %q", resp.Name, "health")
	}
	if resp.Rig != "myrig" {
		t.Errorf("rig = %q, want %q", resp.Rig, "myrig")
	}
}

func TestHandleOrderGet_NotFound(t *testing.T) {
	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/order/nonexistent"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleOrderDisable(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.autos = []orders.Order{
		{Name: "health", Exec: "echo ok", Trigger: "cooldown"},
	}
	h := newTestCityHandler(t, fs)

	req := newPostRequest(cityURL(fs, "/order/health/disable"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify override was written.
	if len(fs.cfg.Orders.Overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(fs.cfg.Orders.Overrides))
	}
	ov := fs.cfg.Orders.Overrides[0]
	if ov.Name != "health" {
		t.Errorf("override name = %q, want %q", ov.Name, "health")
	}
	if ov.Enabled == nil || *ov.Enabled {
		t.Error("expected enabled=false")
	}
}

func TestHandleOrderEnable(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.autos = []orders.Order{
		{Name: "health", Exec: "echo ok", Trigger: "cooldown"},
	}
	h := newTestCityHandler(t, fs)

	req := newPostRequest(cityURL(fs, "/order/health/enable"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if len(fs.cfg.Orders.Overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(fs.cfg.Orders.Overrides))
	}
	ov := fs.cfg.Orders.Overrides[0]
	if ov.Enabled == nil || !*ov.Enabled {
		t.Error("expected enabled=true")
	}
}

func TestHandleOrdersFeedReturnsWorkflowAndScheduledOrderRuns(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2", Trigger: "cron", Pool: "reviewers", Rig: "myrig"},
	}

	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}
	root, err := rigStore.Create(beads.Bead{
		Title: "Adopt PR",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-123",
			"gc.run_target":       "myrig/claude",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
			"gc.source_bead_id":   "bd-42",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	inProgress := "in_progress"
	assignee := "myrig/claude"
	if err := rigStore.Update(root.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("set workflow in_progress: %v", err)
	}

	_, err = fs.cityBeadStore.Create(beads.Bead{
		Title:  "order:nightly-review:rig:myrig",
		Status: "closed",
		Labels: []string{"order-tracking", "order-run:nightly-review:rig:myrig", "wisp"},
	})
	if err != nil {
		t.Fatalf("create tracking bead: %v", err)
	}
	time.Sleep(time.Millisecond)
	_, err = fs.cityBeadStore.Create(beads.Bead{
		Title:  "nightly-review wisp",
		Type:   "wisp",
		Status: "in_progress",
		Labels: []string{"order-run:nightly-review:rig:myrig", "wisp"},
	})
	if err != nil {
		t.Fatalf("create wisp bead: %v", err)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/feed?scope_kind=rig&scope_ref=myrig"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Items         []monitorFeedItemResponse `json:"items"`
		Partial       bool                      `json:"partial"`
		PartialErrors []string                  `json:"partial_errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(resp.Items))
	}

	if resp.Items[0].WorkflowID != "wf-123" || resp.Items[0].Type != "formula" {
		t.Fatalf("items[0] = %+v, want workflow feed item first", resp.Items[0])
	}
	if resp.Items[0].Target != "myrig/claude" {
		t.Fatalf("workflow target = %q, want myrig/claude", resp.Items[0].Target)
	}
	if !resp.Items[0].RunDetailAvailable || resp.Items[0].DetailAvailable {
		t.Fatalf("workflow detail flags = %+v, want run_detail_available only", resp.Items[0])
	}

	if resp.Items[1].BeadID == "" || resp.Items[1].Type != "formula" {
		t.Fatalf("items[1] = %+v, want scheduled formula order tracking item", resp.Items[1])
	}
	if resp.Items[1].Target != "myrig/reviewers" {
		t.Fatalf("scheduled order target = %q, want myrig/reviewers", resp.Items[1].Target)
	}
	if resp.Items[1].UpdatedAt == resp.Items[1].StartedAt {
		t.Fatalf("scheduled order timestamps = started %q updated %q, want updated_at to reflect newer run activity", resp.Items[1].StartedAt, resp.Items[1].UpdatedAt)
	}
}

func TestHandleOrderCheckTreatsWispFailedAsFailed(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2", Trigger: "cooldown", Interval: "1h", Rig: "myrig"},
	}

	_, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "order:nightly-review:rig:myrig",
		Status: "closed",
		Labels: []string{"order-tracking", "order-run:nightly-review:rig:myrig", "wisp", "wisp-failed"},
	})
	if err != nil {
		t.Fatalf("create tracking bead: %v", err)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/check"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	var resp struct {
		Checks []struct {
			ScopedName     string  `json:"scoped_name"`
			LastRunOutcome *string `json:"last_run_outcome"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(resp.Checks))
	}
	if resp.Checks[0].LastRunOutcome == nil || *resp.Checks[0].LastRunOutcome != "failed" {
		t.Fatalf("last_run_outcome = %v, want failed", resp.Checks[0].LastRunOutcome)
	}
}

func TestLastRunOutcomeFromLabelsPrioritizesTerminalLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{name: "wisp failed dominates success", labels: []string{"wisp", "wisp-failed"}, want: "failed"},
		{name: "failed alone", labels: []string{"wisp-failed"}, want: "failed"},
		{name: "exec failed dominates success", labels: []string{"exec", "exec-failed"}, want: "failed"},
		{name: "canceled dominates success", labels: []string{"wisp", "wisp-canceled"}, want: "canceled"},
		{name: "success fallback", labels: []string{"exec"}, want: "success"},
		{name: "unknown", labels: []string{"order-tracking"}, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastRunOutcomeFromLabels(tc.labels); got != tc.want {
				t.Fatalf("lastRunOutcomeFromLabels(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

func TestHandleOrdersFeedIgnoresUnrelatedStoreListFailures(t *testing.T) {
	fs := newFakeState(t)
	fs.stores["alpha"] = failListStore{Store: beads.NewMemStore()}
	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}

	root, err := rigStore.Create(beads.Bead{
		Title: "Adopt PR",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-healthy",
			"gc.run_target":       "myrig/claude",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(root.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set workflow in_progress: %v", err)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/feed?scope_kind=rig&scope_ref=myrig"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Items         []monitorFeedItemResponse `json:"items"`
		Partial       bool                      `json:"partial"`
		PartialErrors []string                  `json:"partial_errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].WorkflowID != "wf-healthy" {
		t.Fatalf("items[0] = %+v, want healthy workflow result", resp.Items[0])
	}
	if resp.Partial {
		t.Fatalf("partial = true, want false; errors = %v", resp.PartialErrors)
	}
}

func TestHandleOrdersFeedCityScopeIncludesRigWorkflowRuns(t *testing.T) {
	fs := newFakeState(t)
	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}

	_, err := rigStore.Create(beads.Bead{
		Title: "Cross-rig run",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-city-view",
			"gc.run_target":       "myrig/codex",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/feed?scope_kind=city&scope_ref=test-city"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Items []monitorFeedItemResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].WorkflowID != "wf-city-view" {
		t.Fatalf("items[0] = %+v, want rig workflow visible in city feed", resp.Items[0])
	}
	if resp.Items[0].ScopeKind != "rig" || resp.Items[0].ScopeRef != "myrig" {
		t.Fatalf("scope = %s/%s, want rig/myrig", resp.Items[0].ScopeKind, resp.Items[0].ScopeRef)
	}
}

func TestHandleOrdersFeedCityScopeReportsPartialRigFailures(t *testing.T) {
	fs := newFakeState(t)
	fs.stores["alpha"] = failListStore{Store: beads.NewMemStore()}
	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}

	_, err := rigStore.Create(beads.Bead{
		Title: "Cross-rig run",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-city-view",
			"gc.run_target":       "myrig/codex",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/feed?scope_kind=city&scope_ref=test-city"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Items         []monitorFeedItemResponse `json:"items"`
		Partial       bool                      `json:"partial"`
		PartialErrors []string                  `json:"partial_errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(resp.Items))
	}
	if !resp.Partial {
		t.Fatalf("partial = false, want true")
	}
	if len(resp.PartialErrors) != 1 || resp.PartialErrors[0] != "rig:alpha store unavailable" {
		t.Fatalf("partial_errors = %v, want rig:alpha store unavailable", resp.PartialErrors)
	}
}

func TestHandleOrderGet_Ambiguous(t *testing.T) {
	fs := newFakeState(t)
	fs.autos = []orders.Order{
		{Name: "health", Exec: "echo ok", Trigger: "cooldown", Rig: "rig-a"},
		{Name: "health", Exec: "echo ok", Trigger: "cooldown", Rig: "rig-b"},
	}
	h := newTestCityHandler(t, fs)

	// Bare name should return 409 when ambiguous.
	req := httptest.NewRequest("GET", cityURL(fs, "/order/health"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusConflict, w.Body.String())
	}

	// Scoped name should resolve unambiguously.
	req = httptest.NewRequest("GET", cityURL(fs, "/order/health:rig:rig-a"), nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp orderResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Rig != "rig-a" {
		t.Errorf("rig = %q, want %q", resp.Rig, "rig-a")
	}
}

func TestHandleOrderDisable_Ambiguous(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.autos = []orders.Order{
		{Name: "health", Exec: "echo ok", Trigger: "cooldown", Rig: "rig-a"},
		{Name: "health", Exec: "echo ok", Trigger: "cooldown", Rig: "rig-b"},
	}
	h := newTestCityHandler(t, fs)

	req := newPostRequest(cityURL(fs, "/order/health/disable"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleOrderDisable_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	req := newPostRequest(cityURL(fs, "/order/nonexistent/disable"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}
