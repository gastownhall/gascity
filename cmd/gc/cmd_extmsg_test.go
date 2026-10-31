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

// TestExtMsgClientRoutesToSupervisor pins the routing of the client that
// gc extmsg bind, handoff and unbind use. Conversation bindings have no local
// fallback, so where apiClient returns nil for a caller's local fallback
// extMsgClient takes the supervisor-managed client, and when no endpoint is
// usable its failure names the routing reason. The fall-through never consults
// the supervisor's grant requirement. (ga-qr4s5g, PR #7825)
func TestExtMsgClientRoutesToSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origIdentity, origSup, origGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
	})
	apiRouteSupervisorGrantsHook = func() bool {
		t.Errorf("supervisor grant requirement consulted on the extmsg fall-through path")
		return true
	}
	supervisorClient := func(*testing.T) func(string) *api.Client {
		return func(string) *api.Client { return sentinel }
	}
	identity := func(reply controllerIdentityReply) func(*testing.T) func(string) controllerIdentityReply {
		return func(*testing.T) func(string) controllerIdentityReply {
			return func(string) controllerIdentityReply { return reply }
		}
	}

	cases := []struct {
		name       string
		noAPI      string
		toml       string
		identity   func(*testing.T) func(string) controllerIdentityReply
		supervisor func(*testing.T) func(string) *api.Client
		wantClient bool
		wantStderr []string
	}{
		{
			// AC-1: a live controller of unknown hosting mode (a supervisor
			// predating identify) with no [api] port.
			name:       "alive-unknown-mode-no-api-port-routes-to-supervisor",
			toml:       "name = \"t\"\n",
			identity:   identity(controllerIdentityReply{PID: 4242}),
			supervisor: supervisorClient,
			wantClient: true,
		},
		{
			// AC-2: the supervisor hosts the city and requires city grants, where
			// apiClient returns nil for its caller's local fallback.
			name:       "supervisor-hosted-requiring-grants-routes-to-supervisor",
			toml:       "name = \"t\"\n",
			identity:   identity(controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}),
			supervisor: supervisorClient,
			wantClient: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_NO_API", tc.noAPI)
			apiRouteControllerIdentityHook = tc.identity(t)
			apiRouteSupervisorClientHook = tc.supervisor(t)
			dir := writeCityTOMLForRoute(t, t.TempDir(), tc.toml)
			withCityFlag(t, dir)

			var stderr bytes.Buffer
			c, cityPath, ok := extMsgClient("bind", &stderr)
			if tc.wantClient {
				if !ok || c != sentinel {
					t.Fatalf("extMsgClient = (%p, ok=%v), want supervisor sentinel %p; stderr: %s", c, ok, sentinel, stderr.String())
				}
				if cityPath == "" {
					t.Fatalf("extMsgClient cityPath empty, want the resolved city")
				}
				if stderr.Len() != 0 {
					t.Fatalf("extMsgClient stderr = %q, want empty", stderr.String())
				}
				return
			}
			if ok || c != nil {
				t.Fatalf("extMsgClient = (%p, ok=%v), want (nil, false)", c, ok)
			}
			for _, want := range tc.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("extMsgClient stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
		})
	}
}
