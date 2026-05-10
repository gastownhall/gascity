package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
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

func TestHandleOrderCheckRunsConditionByDefault(t *testing.T) {
	fs := newFakeState(t)
	marker := t.TempDir() + "/condition-ran"
	fs.autos = []orders.Order{
		{Name: "router", Formula: "review-pr", Trigger: "condition", Check: "printf x >> " + strconv.Quote(marker)},
	}

	h := newTestCityHandler(t, fs)
	for _, path := range []string{"/orders/check", "/orders/check", "/orders/check?fresh=true"} {
		req := httptest.NewRequest(http.MethodGet, cityURL(fs, path), nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status for %s = %d, want 200; body = %s", path, w.Code, w.Body.String())
		}
	}

	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read condition marker: %v", err)
	}
	if string(got) != "xxx" {
		t.Fatalf("condition marker = %q, want one execution per request", got)
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

func TestHandleOrdersFeedIncludesRigStoreTrackingBeads(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2", Trigger: "cron", Interval: "24h", Pool: "reviewers", Rig: "myrig"},
	}

	tracking, err := rigStore.Create(beads.Bead{
		Title:  "order:nightly-review:rig:myrig",
		Status: "closed",
		Labels: []string{"order-tracking", "order-run:nightly-review:rig:myrig", "wisp"},
	})
	if err != nil {
		t.Fatalf("create tracking bead: %v", err)
	}
	time.Sleep(time.Millisecond)
	_, err = rigStore.Create(beads.Bead{
		Title:  "nightly-review wisp",
		Type:   "wisp",
		Status: "closed",
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
		Items []monitorFeedItemResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(resp.Items))
	}
	item := resp.Items[0]
	if item.BeadID != tracking.ID {
		t.Fatalf("bead_id = %q, want %q", item.BeadID, tracking.ID)
	}
	if item.StoreRef != "rig:myrig" {
		t.Fatalf("store_ref = %q, want rig:myrig", item.StoreRef)
	}
	if item.ID != "order:rig:myrig:"+tracking.ID {
		t.Fatalf("id = %q, want store-qualified order id", item.ID)
	}
	if item.ScopeKind != "rig" || item.ScopeRef != "myrig" {
		t.Fatalf("scope = %s/%s, want rig/myrig", item.ScopeKind, item.ScopeRef)
	}
	if item.Target != "myrig/reviewers" {
		t.Fatalf("target = %q, want myrig/reviewers", item.Target)
	}
	if item.UpdatedAt == item.StartedAt {
		t.Fatalf("updated_at = %q, started_at = %q, want updated_at to reflect newer run activity", item.UpdatedAt, item.StartedAt)
	}
}

func TestHandleOrdersFeedRigScopeReturnsRequestedStoreFailure(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	fs.stores["myrig"] = failListStore{Store: fs.stores["myrig"]}

	if _, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "legacy rig tracking",
		Status: "closed",
		Labels: []string{"order-tracking", "order-run:nightly-review:rig:myrig", "wisp"},
	}); err != nil {
		t.Fatalf("create legacy tracking bead: %v", err)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/feed?scope_kind=rig&scope_ref=myrig"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

func TestHandleOrderCheckUsesRigStoreLastRunState(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2", Trigger: "cooldown", Interval: "24h", Rig: "myrig"},
	}

	if _, err := rigStore.Create(beads.Bead{
		Title:  "nightly-review wisp",
		Status: "closed",
		Labels: []string{"order-run:nightly-review:rig:myrig", "wisp"},
	}); err != nil {
		t.Fatalf("create rig run: %v", err)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/check"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Checks []struct {
			Name           string  `json:"name"`
			ScopedName     string  `json:"scoped_name"`
			Due            bool    `json:"due"`
			LastRunOutcome *string `json:"last_run_outcome"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(resp.Checks))
	}
	check := resp.Checks[0]
	if check.ScopedName != "nightly-review:rig:myrig" {
		t.Fatalf("scoped_name = %q, want nightly-review:rig:myrig", check.ScopedName)
	}
	if check.Due {
		t.Fatalf("due = true, want false when rig store has a recent run")
	}
	if check.LastRunOutcome == nil || *check.LastRunOutcome != "success" {
		t.Fatalf("last_run_outcome = %v, want success", check.LastRunOutcome)
	}
}

type cachedOnlyOrderHistoryStore struct {
	beads.Store
	cached                 []beads.Bead
	cacheOK                bool
	includeClosedListCalls int
}

func (s *cachedOnlyOrderHistoryStore) CachedList(query beads.ListQuery) ([]beads.Bead, bool) {
	return beads.ApplyListQuery(s.cached, query), s.cacheOK
}

func (s *cachedOnlyOrderHistoryStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.IncludeClosed {
		s.includeClosedListCalls++
	}
	return s.Store.List(query)
}

type fixedOrderHistoryStore struct {
	beads.Store
	rows []beads.Bead
}

func (s *fixedOrderHistoryStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	return beads.ApplyListQuery(s.rows, query), nil
}

func TestHandleOrderCheckUsesCachedHistoryWhenAvailable(t *testing.T) {
	fs := newFakeState(t)
	run := beads.Bead{
		ID:        "run-1",
		Title:     "nightly-review wisp",
		Status:    "closed",
		CreatedAt: time.Now().UTC(),
		Labels:    []string{"order-run:nightly-review", "wisp"},
	}
	cachedStore := &cachedOnlyOrderHistoryStore{
		Store:   beads.NewMemStore(),
		cached:  []beads.Bead{run},
		cacheOK: true,
	}
	fs.cityBeadStore = cachedStore
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2", Trigger: "cooldown", Interval: "24h"},
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/check"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Checks []struct {
			Due            bool    `json:"due"`
			LastRunOutcome *string `json:"last_run_outcome"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(resp.Checks))
	}
	if resp.Checks[0].Due {
		t.Fatal("due = true, want false from cached recent run")
	}
	if resp.Checks[0].LastRunOutcome == nil || *resp.Checks[0].LastRunOutcome != "success" {
		t.Fatalf("last_run_outcome = %v, want success", resp.Checks[0].LastRunOutcome)
	}
	if cachedStore.includeClosedListCalls != 0 {
		t.Fatalf("IncludeClosed List calls = %d, want 0 when cached history is available", cachedStore.includeClosedListCalls)
	}
}

func TestHandleOrderCheckFallsBackToLiveHistoryWhenCacheUnavailable(t *testing.T) {
	fs := newFakeState(t)
	cachedStore := &cachedOnlyOrderHistoryStore{
		Store: beads.NewMemStore(),
	}
	_, err := cachedStore.Create(beads.Bead{
		Title:     "nightly-review wisp",
		Status:    "closed",
		CreatedAt: time.Now().UTC(),
		Labels:    []string{"order-run:nightly-review", "wisp"},
	})
	if err != nil {
		t.Fatalf("create live history bead: %v", err)
	}
	fs.cityBeadStore = cachedStore
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2", Trigger: "cooldown", Interval: "24h"},
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/check"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Checks []struct {
			Due            bool    `json:"due"`
			LastRunOutcome *string `json:"last_run_outcome"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(resp.Checks))
	}
	if resp.Checks[0].Due {
		t.Fatal("due = true, want false from live recent run")
	}
	if resp.Checks[0].LastRunOutcome == nil || *resp.Checks[0].LastRunOutcome != "success" {
		t.Fatalf("last_run_outcome = %v, want success", resp.Checks[0].LastRunOutcome)
	}
	if cachedStore.includeClosedListCalls == 0 {
		t.Fatal("IncludeClosed List calls = 0, want live fallback when cache is unavailable")
	}
}

func TestHandleOrderCheckSkipsUnavailableRigStore(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	delete(fs.stores, "missing")
	fs.autos = []orders.Order{
		{Name: "city-review", Formula: "mol-adopt-pr-v2", Trigger: "manual"},
		{Name: "rig-review", Formula: "mol-adopt-pr-v2", Trigger: "manual", Rig: "missing"},
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/check"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Checks []orderCheckResponse `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2", len(resp.Checks))
	}
}

func TestHandleOrderHistoryUsesRigStore(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2", Rig: "myrig"},
	}

	run, err := rigStore.Create(beads.Bead{
		Title:  "nightly-review wisp",
		Status: "closed",
		Labels: []string{"order-run:nightly-review:rig:myrig", "wisp"},
	})
	if err != nil {
		t.Fatalf("create rig history bead: %v", err)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/history?scoped_name=nightly-review:rig:myrig"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Entries []struct {
			BeadID     string `json:"bead_id"`
			ScopedName string `json:"scoped_name"`
			Rig        string `json:"rig"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(resp.Entries))
	}
	if resp.Entries[0].BeadID != run.ID {
		t.Fatalf("bead_id = %q, want %q", resp.Entries[0].BeadID, run.ID)
	}
	if resp.Entries[0].ScopedName != "nightly-review:rig:myrig" {
		t.Fatalf("scoped_name = %q, want nightly-review:rig:myrig", resp.Entries[0].ScopedName)
	}
	if resp.Entries[0].Rig != "myrig" {
		t.Fatalf("rig = %q, want myrig", resp.Entries[0].Rig)
	}
}

func TestHandleOrderHistoryMarksAdHocOutputMetadata(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}

	run, err := rigStore.Create(beads.Bead{
		Title:  "ad hoc order output",
		Status: "closed",
		Labels: []string{"order-run:ad-hoc:rig:myrig", "wisp"},
		Metadata: map[string]string{
			"convergence.gate_stdout": "done",
		},
	})
	if err != nil {
		t.Fatalf("create rig history bead: %v", err)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/history?scoped_name=ad-hoc:rig:myrig"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Entries []struct {
			BeadID    string `json:"bead_id"`
			HasOutput bool   `json:"has_output"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(resp.Entries))
	}
	if resp.Entries[0].BeadID != run.ID {
		t.Fatalf("bead_id = %q, want %q", resp.Entries[0].BeadID, run.ID)
	}
	if !resp.Entries[0].HasOutput {
		t.Fatal("has_output = false, want true for captured output metadata")
	}
}

func TestHandleOrderHistoryIncludesStoreRefForCollidingIDs(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2", Rig: "myrig"},
	}

	cityRun, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "legacy city nightly-review",
		Status: "closed",
		Labels: []string{"order-run:nightly-review:rig:myrig", "wisp"},
	})
	if err != nil {
		t.Fatalf("create city history bead: %v", err)
	}
	rigRun, err := rigStore.Create(beads.Bead{
		Title:  "rig nightly-review",
		Status: "closed",
		Labels: []string{"order-run:nightly-review:rig:myrig", "wisp"},
	})
	if err != nil {
		t.Fatalf("create rig history bead: %v", err)
	}
	if cityRun.ID != rigRun.ID {
		t.Fatalf("test requires colliding IDs, got city=%q rig=%q", cityRun.ID, rigRun.ID)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/history?scoped_name=nightly-review:rig:myrig"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Entries []struct {
			BeadID   string `json:"bead_id"`
			StoreRef string `json:"store_ref"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2: %+v", len(resp.Entries), resp.Entries)
	}
	got := map[string]bool{}
	for _, entry := range resp.Entries {
		if entry.BeadID != cityRun.ID {
			t.Fatalf("bead_id = %q, want colliding id %q", entry.BeadID, cityRun.ID)
		}
		got[entry.StoreRef] = true
	}
	for _, want := range []string{"city:test-city", "rig:myrig"} {
		if !got[want] {
			t.Fatalf("entries missing store_ref %q: %+v", want, resp.Entries)
		}
	}
}

func TestHandleOrderHistoryBeforeUsesBufferedBoundedFetch(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2"},
	}

	oldRun, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "old nightly-review",
		Status: "closed",
		Labels: []string{"order-run:nightly-review", "wisp"},
	})
	if err != nil {
		t.Fatalf("create old run: %v", err)
	}
	time.Sleep(time.Millisecond)
	midRun, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "middle nightly-review",
		Status: "closed",
		Labels: []string{"order-run:nightly-review", "wisp"},
	})
	if err != nil {
		t.Fatalf("create middle run: %v", err)
	}
	time.Sleep(time.Millisecond)
	if _, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "new nightly-review",
		Status: "closed",
		Labels: []string{"order-run:nightly-review", "wisp"},
	}); err != nil {
		t.Fatalf("create new run: %v", err)
	}

	h := newTestCityHandler(t, fs)
	before := midRun.CreatedAt.Format(time.RFC3339Nano)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/history?scoped_name=nightly-review&limit=1&before="+url.QueryEscape(before)), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Entries []struct {
			BeadID string `json:"bead_id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1: %+v", len(resp.Entries), resp.Entries)
	}
	if resp.Entries[0].BeadID != oldRun.ID {
		t.Fatalf("bead_id = %q, want older run %q", resp.Entries[0].BeadID, oldRun.ID)
	}
}

func TestHandleOrderHistoryBeforeRequiresEscapedPositiveOffsetCursor(t *testing.T) {
	fs := newFakeState(t)
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2"},
	}

	plusTwo := time.FixedZone("plus-two", 2*60*60)
	oldCreatedAt := time.Date(2026, 5, 9, 10, 0, 0, 123456789, plusTwo)
	cursorCreatedAt := oldCreatedAt.Add(time.Minute)
	newCreatedAt := cursorCreatedAt.Add(time.Minute)
	fs.cityBeadStore = &fixedOrderHistoryStore{
		Store: beads.NewMemStore(),
		rows: []beads.Bead{
			{
				ID:        "old-run",
				Title:     "old nightly-review",
				Status:    "closed",
				CreatedAt: oldCreatedAt,
				Labels:    []string{"order-run:nightly-review", "wisp"},
			},
			{
				ID:        "cursor-run",
				Title:     "cursor nightly-review",
				Status:    "closed",
				CreatedAt: cursorCreatedAt,
				Labels:    []string{"order-run:nightly-review", "wisp"},
			},
			{
				ID:        "new-run",
				Title:     "new nightly-review",
				Status:    "closed",
				CreatedAt: newCreatedAt,
				Labels:    []string{"order-run:nightly-review", "wisp"},
			},
		},
	}

	h := newTestCityHandler(t, fs)
	before := cursorCreatedAt.Format(time.RFC3339Nano)

	rawReq := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/history?scoped_name=nightly-review&limit=1&before="+before), nil)
	rawW := httptest.NewRecorder()
	h.ServeHTTP(rawW, rawReq)
	if rawW.Code != http.StatusBadRequest {
		t.Fatalf("raw status = %d, want %d; body = %s", rawW.Code, http.StatusBadRequest, rawW.Body.String())
	}

	escapedReq := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/history?scoped_name=nightly-review&limit=1&before="+url.QueryEscape(before)), nil)
	escapedW := httptest.NewRecorder()
	h.ServeHTTP(escapedW, escapedReq)
	if escapedW.Code != http.StatusOK {
		t.Fatalf("escaped status = %d, want %d; body = %s", escapedW.Code, http.StatusOK, escapedW.Body.String())
	}

	var resp struct {
		Entries []struct {
			BeadID string `json:"bead_id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(escapedW.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1: %+v", len(resp.Entries), resp.Entries)
	}
	if resp.Entries[0].BeadID != "old-run" {
		t.Fatalf("bead_id = %q, want old-run", resp.Entries[0].BeadID)
	}
}

func TestHandleOrderHistoryBeforeFiltersBeforeStoreLimit(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2"},
	}

	oldRun, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "old nightly-review",
		Status: "closed",
		Labels: []string{"order-run:nightly-review", "wisp"},
	})
	if err != nil {
		t.Fatalf("create old run: %v", err)
	}
	time.Sleep(time.Millisecond)
	cursorRun, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "cursor nightly-review",
		Status: "closed",
		Labels: []string{"order-run:nightly-review", "wisp"},
	})
	if err != nil {
		t.Fatalf("create cursor run: %v", err)
	}
	time.Sleep(time.Millisecond)
	for i := 0; i < 4; i++ {
		if _, err := fs.cityBeadStore.Create(beads.Bead{
			Title:  "newer nightly-review",
			Status: "closed",
			Labels: []string{"order-run:nightly-review", "wisp"},
		}); err != nil {
			t.Fatalf("create newer run %d: %v", i, err)
		}
		time.Sleep(time.Millisecond)
	}

	h := newTestCityHandler(t, fs)
	before := cursorRun.CreatedAt.Format(time.RFC3339Nano)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/history?scoped_name=nightly-review&limit=1&before="+url.QueryEscape(before)), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Entries []struct {
			BeadID string `json:"bead_id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1: %+v", len(resp.Entries), resp.Entries)
	}
	if resp.Entries[0].BeadID != oldRun.ID {
		t.Fatalf("bead_id = %q, want older run %q", resp.Entries[0].BeadID, oldRun.ID)
	}
}

func TestHandleOrderHistoryBeforeFiltersBeforeMergedLimit(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}
	fs.autos = []orders.Order{
		{Name: "nightly-review", Formula: "mol-adopt-pr-v2", Rig: "myrig"},
	}

	oldRun, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "legacy older nightly-review",
		Status: "closed",
		Labels: []string{"order-run:nightly-review:rig:myrig", "wisp"},
	})
	if err != nil {
		t.Fatalf("create old city run: %v", err)
	}
	time.Sleep(time.Millisecond)
	cursorRun, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "cursor nightly-review",
		Status: "closed",
		Labels: []string{"order-run:nightly-review:rig:myrig", "wisp"},
	})
	if err != nil {
		t.Fatalf("create cursor run: %v", err)
	}
	time.Sleep(time.Millisecond)
	for i := 0; i < 3; i++ {
		if _, err := rigStore.Create(beads.Bead{
			Title:  "newer rig nightly-review",
			Status: "closed",
			Labels: []string{"order-run:nightly-review:rig:myrig", "wisp"},
		}); err != nil {
			t.Fatalf("create newer rig run %d: %v", i, err)
		}
		time.Sleep(time.Millisecond)
	}

	h := newTestCityHandler(t, fs)
	before := cursorRun.CreatedAt.Format(time.RFC3339Nano)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/history?scoped_name=nightly-review:rig:myrig&limit=1&before="+url.QueryEscape(before)), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Entries []struct {
			BeadID   string `json:"bead_id"`
			StoreRef string `json:"store_ref"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1: %+v", len(resp.Entries), resp.Entries)
	}
	if resp.Entries[0].BeadID != oldRun.ID || resp.Entries[0].StoreRef != "city:test-city" {
		t.Fatalf("entry = %+v, want old city run %q", resp.Entries[0], oldRun.ID)
	}
}

func TestHandleOrderHistoryDetailUsesRigStore(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}

	run, err := rigStore.Create(beads.Bead{
		Title:  "nightly-review wisp",
		Status: "closed",
		Labels: []string{"order-run:nightly-review:rig:myrig", "wisp"},
		Metadata: map[string]string{
			"convergence.gate_stdout": "done",
		},
	})
	if err != nil {
		t.Fatalf("create rig history bead: %v", err)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/order/history/"+run.ID), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		BeadID string `json:"bead_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.BeadID != run.ID {
		t.Fatalf("bead_id = %q, want %q", resp.BeadID, run.ID)
	}
	if resp.Output != "done" {
		t.Fatalf("output = %q, want done", resp.Output)
	}
}

func TestHandleOrderHistoryDetailUsesStoreRefForCollidingIDs(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	rigStore := fs.stores["myrig"]
	if rigStore == nil {
		t.Fatal("expected rig store")
	}

	cityRun, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "legacy city nightly-review",
		Status: "closed",
		Labels: []string{"order-run:nightly-review:rig:myrig", "wisp"},
		Metadata: map[string]string{
			"convergence.gate_stdout": "city output",
		},
	})
	if err != nil {
		t.Fatalf("create city history bead: %v", err)
	}
	rigRun, err := rigStore.Create(beads.Bead{
		Title:  "rig nightly-review",
		Status: "closed",
		Labels: []string{"order-run:nightly-review:rig:myrig", "wisp"},
		Metadata: map[string]string{
			"convergence.gate_stdout": "rig output",
		},
	})
	if err != nil {
		t.Fatalf("create rig history bead: %v", err)
	}
	if cityRun.ID != rigRun.ID {
		t.Fatalf("test requires colliding IDs, got city=%q rig=%q", cityRun.ID, rigRun.ID)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/order/history/"+rigRun.ID+"?store_ref=rig:myrig"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		BeadID   string `json:"bead_id"`
		StoreRef string `json:"store_ref"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.StoreRef != "rig:myrig" {
		t.Fatalf("store_ref = %q, want rig:myrig", resp.StoreRef)
	}
	if resp.Output != "rig output" {
		t.Fatalf("output = %q, want rig output", resp.Output)
	}
}

func TestHandleOrderHistoryNoStoresReturnsServiceUnavailable(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = nil
	fs.stores = map[string]beads.Store{}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/history?scoped_name=nightly-review"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
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
