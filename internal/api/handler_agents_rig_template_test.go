package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/config"
)

// Regression for ga-l24lrx: GET /agent/{dir}/{base} resolves a rig-qualified
// identity ("myrig/rigbot") through the generic scope="rig" template that
// serves every rig ("rigbot"). The instance has no config of its own: the only
// config behind it is the template, so an edit addressed to one rig's instance
// would change every rig. The qualified edit routes refuse it with a 409 that
// names the template, and leave every rig's instance as it was.
func TestQualifiedAgentEditsRefuseGenericRigScopedTemplateInstance(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Rigs = append(fs.cfg.Rigs, config.Rig{Name: "otherrig", Path: "/tmp/otherrig"})
	fs.cfg.Agents = append(fs.cfg.Agents, config.Agent{Name: "rigbot", Scope: "rig", Provider: "test-agent"})
	h := newTestCityHandler(t, fs)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, cityURL(fs, path), strings.NewReader(body))
		} else {
			req = httptest.NewRequest(method, cityURL(fs, path), nil)
		}
		req.Header.Set("X-GC-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	expectInstancesNotSuspended := func(when string) {
		t.Helper()
		for _, rig := range []string{"myrig", "otherrig"} {
			w := do("GET", "/agent/"+rig+"/rigbot", "")
			if w.Code != http.StatusOK {
				t.Fatalf("%s: GET %s/rigbot = %d, want 200; body = %s", when, rig, w.Code, w.Body.String())
			}
			var got agentResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("%s: decoding GET %s/rigbot: %v", when, rig, err)
			}
			if got.Suspended {
				t.Fatalf("%s: %s/rigbot reads suspended", when, rig)
			}
		}
	}

	expectInstancesNotSuspended("before the edits")
	refused := []struct {
		method, path, body string
		hint               string
	}{
		{"POST", "/agent/myrig/rigbot/suspend", "", "suspend agent rigbot to suspend it in every rig, or suspend rig myrig"},
		{"POST", "/agent/myrig/rigbot/resume", "", "resume agent rigbot to resume it in every rig, or resume rig myrig"},
		{"PATCH", "/agent/myrig/rigbot", `{"suspended":true}`, "update agent rigbot to update it in every rig"},
		{"PATCH", "/agent/myrig/rigbot", `{"scope":"city"}`, "update agent rigbot to update it in every rig"},
		{"DELETE", "/agent/myrig/rigbot", "", "delete agent rigbot to delete it in every rig"},
	}
	for _, r := range refused {
		w := do(r.method, r.path, r.body)
		if w.Code != http.StatusConflict {
			t.Errorf("%s %s %s = %d, want 409; body = %s", r.method, r.path, r.body, w.Code, w.Body.String())
			continue
		}
		body := w.Body.String()
		for _, want := range []string{"conflict-wrong-state", "agent myrig/rigbot is a per-rig instance of the rig-scoped template rigbot", r.hint} {
			if !strings.Contains(body, want) {
				t.Errorf("%s %s %s: body lacks %q; body = %s", r.method, r.path, r.body, want, body)
			}
		}
	}
	if len(fs.suspended) != 0 {
		t.Errorf("a refused edit reached the state: suspended = %v", fs.suspended)
	}
	if a, ok := findAgent(fs.cfg, "rigbot"); !ok || a.Suspended || a.Scope != "rig" {
		t.Fatalf("a refused edit changed the shared template: found = %v, agent = %+v", ok, a)
	}
	expectInstancesNotSuspended("after the refused edits")

	// The template itself, and a rig agent with config of its own, stay editable.
	if w := do("POST", "/agent/rigbot/suspend", ""); w.Code != http.StatusOK || !fs.suspended["rigbot"] {
		t.Fatalf("suspend template = %d (suspended = %v), want 200; body = %s", w.Code, fs.suspended, w.Body.String())
	}
	if w := do("POST", "/agent/myrig/worker/suspend", ""); w.Code != http.StatusOK || !fs.suspended["myrig/worker"] {
		t.Fatalf("suspend myrig/worker = %d (suspended = %v), want 200; body = %s", w.Code, fs.suspended, w.Body.String())
	}
}

func TestRigTemplateInstance(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "myrig"}, {Name: "otherrig"}},
		Agents: []config.Agent{
			{Name: "rigbot", Scope: "rig"},
			{Name: "packbot", BindingName: "pack", Scope: "rig"},
			{Name: "ownbot", Scope: "rig"},
			{Name: "ownbot", Dir: "otherrig"},
			{Name: "citybot"},
		},
	}
	for _, tc := range []struct {
		name              string
		cfg               *config.City
		wantTemplate, rig string
		wantInstance      bool
	}{
		{name: "myrig/rigbot", cfg: cfg, wantTemplate: "rigbot", rig: "myrig", wantInstance: true},
		{name: "otherrig/rigbot", cfg: cfg, wantTemplate: "rigbot", rig: "otherrig", wantInstance: true},
		{name: "myrig/packbot", cfg: cfg, wantTemplate: "pack.packbot", rig: "myrig", wantInstance: true},
		{name: "myrig/ownbot", cfg: cfg, wantTemplate: "ownbot", rig: "myrig", wantInstance: true},
		// An agent with config of its own is not an instance, even when a
		// generic template of the same name serves the rig.
		{name: "otherrig/ownbot", cfg: cfg},
		{name: "rigbot", cfg: cfg},
		{name: "norig/rigbot", cfg: cfg},
		{name: "myrig/citybot", cfg: cfg},
		{name: "myrig/rigbot", cfg: nil},
	} {
		template, rig, ok := rigTemplateInstance(tc.cfg, tc.name)
		if ok != tc.wantInstance || template != tc.wantTemplate || rig != tc.rig {
			t.Errorf("rigTemplateInstance(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.name, template, rig, ok, tc.wantTemplate, tc.rig, tc.wantInstance)
		}
	}
}

// A [[patches.agent]] (or rig/provider) patch whose target is absent from the
// merged config is rolled back at reload; the client named a target that does
// not exist, so it is a 400, not a 500.
func TestMutationErrorMapsPatchTargetNotFoundToBadRequest(t *testing.T) {
	cfg := &config.City{}
	applyErr := config.ApplyPatches(cfg, config.Patches{Agents: []config.AgentPatch{{Dir: "myrig", Name: "rigbot"}}})
	if applyErr == nil {
		t.Fatal("ApplyPatches accepted a patch for a missing agent")
	}
	wrapped := fmt.Errorf("refreshing updated city config: %w", fmt.Errorf("applying patches: %w", applyErr))
	var se huma.StatusError
	if !errors.As(mutationError(wrapped), &se) || se.GetStatus() != http.StatusBadRequest {
		t.Fatalf("mutationError(%v) = %v, want 400", wrapped, mutationError(wrapped))
	}
}
