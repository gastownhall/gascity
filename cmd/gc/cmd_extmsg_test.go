package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/extmsg"
)

func TestCmdExtMsgBindRejectsInvalidTargetFlags(t *testing.T) {
	tests := []struct {
		name      string
		agentName string
		sessionID string
		replace   bool
		wantMsg   string
	}{
		{name: "bind requires a target", wantMsg: "gc extmsg bind: --agent or --session is required"},
		{name: "handoff requires a target", replace: true, wantMsg: "gc extmsg handoff: --agent or --session is required"},
		{name: "bind target mutually exclusive", agentName: "myrig/a", sessionID: "sess-1", wantMsg: "gc extmsg bind: --agent and --session are mutually exclusive"},
		{name: "handoff target mutually exclusive", agentName: "myrig/a", sessionID: "sess-1", replace: true, wantMsg: "gc extmsg handoff: --agent and --session are mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			// These validations short-circuit before the command reaches the
			// city API, so an unconfigured city does not affect the result.
			code := cmdExtMsgBind(extMsgConversationFlags{}, tt.agentName, tt.sessionID, tt.replace, false, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("cmdExtMsgBind code = %d, want 1", code)
			}
			if got := stderr.String(); !strings.Contains(got, tt.wantMsg) {
				t.Fatalf("stderr = %q, want it to contain %q", got, tt.wantMsg)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty on validation failure", stdout.String())
			}
		})
	}
}

func TestConversationRefValidatesRequiredFlags(t *testing.T) {
	// scope-id is set so conversationRef does not need to load city config.
	if _, err := (&extMsgConversationFlags{scopeID: "alpha", conversationID: "123"}).conversationRef("/nonexistent"); err == nil || !strings.Contains(err.Error(), "--provider is required") {
		t.Fatalf("conversationRef(no provider) err = %v, want --provider is required", err)
	}
	if _, err := (&extMsgConversationFlags{scopeID: "alpha", provider: "telegram"}).conversationRef("/nonexistent"); err == nil || !strings.Contains(err.Error(), "--conversation-id is required") {
		t.Fatalf("conversationRef(no conversation-id) err = %v, want --conversation-id is required", err)
	}

	ref, err := (&extMsgConversationFlags{
		scopeID:        "alpha",
		provider:       "telegram",
		accountID:      "default",
		conversationID: "7113355",
		kind:           "dm",
	}).conversationRef("/nonexistent")
	if err != nil {
		t.Fatalf("conversationRef(valid): %v", err)
	}
	want := extmsg.ConversationRef{
		ScopeID:        "alpha",
		Provider:       "telegram",
		AccountID:      "default",
		ConversationID: "7113355",
		Kind:           extmsg.ConversationDM,
	}
	if ref != want {
		t.Fatalf("conversationRef = %+v, want %+v", ref, want)
	}
}

func TestConversationRefIfSetIsNilWhenUnset(t *testing.T) {
	got, err := (&extMsgConversationFlags{}).conversationRefIfSet("/nonexistent")
	if err != nil {
		t.Fatalf("conversationRefIfSet(unset): %v", err)
	}
	if got != nil {
		t.Fatalf("conversationRefIfSet(unset) = %+v, want nil so unbind filters by session/agent", got)
	}

	ref, err := (&extMsgConversationFlags{scopeID: "alpha", provider: "telegram", conversationID: "123"}).conversationRefIfSet("/nonexistent")
	if err != nil {
		t.Fatalf("conversationRefIfSet(set): %v", err)
	}
	if ref == nil || ref.Provider != "telegram" || ref.ConversationID != "123" {
		t.Fatalf("conversationRefIfSet(set) = %+v, want a populated ref", ref)
	}
}

func TestPrintExtMsgBindingOutputs(t *testing.T) {
	record := extmsg.SessionBindingRecord{
		ID:                "bind-1",
		Conversation:      extmsg.ConversationRef{Provider: "telegram", ConversationID: "7113355"},
		AgentName:         "myrig/specialist",
		Status:            extmsg.BindingActive,
		BindingGeneration: 3,
	}

	var jsonOut bytes.Buffer
	if code := printExtMsgBinding(&jsonOut, true, record, "handed off"); code != 0 {
		t.Fatalf("printExtMsgBinding(json) code = %d, want 0", code)
	}
	var decoded extmsg.SessionBindingRecord
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatalf("decode json output: %v", err)
	}
	if decoded.ID != "bind-1" || decoded.AgentName != "myrig/specialist" || decoded.BindingGeneration != 3 {
		t.Fatalf("decoded json = %+v, want the binding record", decoded)
	}

	var humanOut bytes.Buffer
	if code := printExtMsgBinding(&humanOut, false, record, "handed off"); code != 0 {
		t.Fatalf("printExtMsgBinding(human) code = %d, want 0", code)
	}
	human := humanOut.String()
	for _, want := range []string{"handed off", "telegram/7113355", "agent myrig/specialist", "binding bind-1", "generation 3"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human output = %q, want it to contain %q", human, want)
		}
	}
}

// TestExtMsgClientRoutesToSupervisor pins the routing decision for the
// extmsg verbs, which have no local fallback: a live controller of unknown
// hosting mode (a supervisor predating identify) with no standalone [api]
// port, and a supervisor-hosted city whose supervisor requires city grants,
// resolve the supervisor-managed client, where apiClient would return nil
// for a local fallback these verbs do not have. A controller that identifies
// as standalone is never sent to the supervisor, and each refusal names the
// routing reason. The supervisor's grant requirement is never consulted on
// this route. (ga-qr4s5g, PR #7825)
func TestExtMsgClientRoutesToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origIdentity, origSup, origGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
	})
	// The extmsg route never consults the grants hook (PreferSupervisor does
	// not read it), so consulting it is a routing regression — as is a
	// grants-required supervisor city failing to resolve the supervisor.
	apiRouteSupervisorGrantsHook = func() bool {
		t.Errorf("supervisor grant requirement consulted on the extmsg route")
		return true
	}

	legacyHosted := func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
	supervisorHosted := func(string) controllerIdentityReply {
		return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	}

	cases := []struct {
		name         string
		gcNoAPI      string
		identity     func(string) controllerIdentityReply
		supervisor   func(string) *api.Client
		toml         string
		wantSentinel bool
		wantReasons  []string
	}{
		{
			// A controller predating identify answers only ping: alive, mode
			// unknown. With no standalone [api] port the supervisor-managed
			// client is the only endpoint these verbs can use.
			name:         "alive-unknown-mode-no-api-port-routes-to-supervisor",
			gcNoAPI:      "",
			identity:     legacyHosted,
			supervisor:   func(string) *api.Client { return sentinel },
			toml:         "name = \"t\"\n",
			wantSentinel: true,
		},
		{
			// The supervisor hosting this city requires signed city grants.
			// apiClient returns nil on its general route (it mints no grant),
			// but these verbs have no local fallback, so the fall-through
			// asks the supervisor and reports a refusal like any API error.
			name:         "supervisor-hosted-requiring-grants-routes-to-supervisor",
			gcNoAPI:      "",
			identity:     supervisorHosted,
			supervisor:   func(string) *api.Client { return sentinel },
			toml:         "name = \"t\"\n",
			wantSentinel: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_NO_API", tc.gcNoAPI)
			apiRouteControllerIdentityHook = tc.identity
			apiRouteSupervisorClientHook = tc.supervisor
			withCityFlag(t, writeCityTOMLForRoute(t, t.TempDir(), tc.toml))

			var stderr bytes.Buffer
			c, cityPath, ok := extMsgClient("bind", &stderr)
			if tc.wantSentinel {
				if !ok || c != sentinel {
					t.Fatalf("extMsgClient = (%p, ok=%v), want the supervisor sentinel %p; stderr: %s",
						c, ok, sentinel, stderr.String())
				}
				if cityPath == "" {
					t.Fatalf("extMsgClient cityPath = empty, want the resolved city path")
				}
				if stderr.Len() != 0 {
					t.Fatalf("extMsgClient stderr = %q, want nothing on success", stderr.String())
				}
				return
			}
			if ok || c != nil {
				t.Fatalf("extMsgClient = (%p, ok=%v), want (nil, false); stderr: %s", c, ok, stderr.String())
			}
			if cityPath != "" {
				t.Fatalf("extMsgClient cityPath = %q, want empty on failure", cityPath)
			}
			got := stderr.String()
			if !strings.Contains(got, "gc extmsg bind: requires the city API server") {
				t.Fatalf("stderr = %q, want it to name the refused verbs", got)
			}
			for _, want := range tc.wantReasons {
				if !strings.Contains(got, want) {
					t.Fatalf("stderr = %q, want it to contain reason %q", got, want)
				}
			}
		})
	}
}
