package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/supervisor"
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

// extMsgRouteRequest is one request an extMsgAPIRecorder answered, with its
// typed body.
type extMsgRouteRequest struct {
	method string
	host   string
	path   string
	bind   genclient.ExtMsgBindInputBody
	unbind genclient.ExtMsgUnbindInputBody
}

// extMsgAPIRecorder is an http.RoundTripper standing in for one extmsg API
// endpoint: it records each request and answers a bind with the binding the
// request asked for and an unbind with one ended agent binding, or with a
// problem response when status is set.
type extMsgAPIRecorder struct {
	t        *testing.T
	status   int
	mu       sync.Mutex
	requests []extMsgRouteRequest
}

func (r *extMsgAPIRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := extMsgRouteRequest{method: req.Method, host: req.URL.Host, path: req.URL.Path}
	var answer any
	switch {
	case strings.HasSuffix(req.URL.Path, "/extmsg/bind"):
		if err := json.NewDecoder(req.Body).Decode(&rec.bind); err != nil {
			return nil, fmt.Errorf("decoding extmsg bind request: %w", err)
		}
		record := genclient.SessionBindingRecord{ID: "bind-1", BindingGeneration: 1, Status: genclient.BindingStatus(extmsg.BindingActive)}
		if rec.bind.Conversation != nil {
			record.Conversation = *rec.bind.Conversation
		}
		if rec.bind.AgentName != nil {
			record.AgentName = *rec.bind.AgentName
		}
		answer = record
	case strings.HasSuffix(req.URL.Path, "/extmsg/unbind"):
		if err := json.NewDecoder(req.Body).Decode(&rec.unbind); err != nil {
			return nil, fmt.Errorf("decoding extmsg unbind request: %w", err)
		}
		unbound := []genclient.SessionBindingRecord{{
			ID:           "bind-1",
			AgentName:    "myrig/front",
			Conversation: genclient.ConversationRef{Provider: "discord", ConversationId: "conv-1", AccountId: "acct", ScopeId: "scope-1", Kind: "room"},
			Status:       genclient.BindingStatus(extmsg.BindingEnded),
		}}
		answer = genclient.ExtMsgUnbindBody{Unbound: &unbound}
	default:
		r.t.Errorf("unexpected extmsg request %s %s", req.Method, req.URL.Path)
		return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
	}
	r.mu.Lock()
	r.requests = append(r.requests, rec)
	r.mu.Unlock()

	status, contentType := http.StatusOK, "application/json"
	if r.status != 0 {
		title, detail := http.StatusText(r.status), "refused by the test endpoint"
		code := int64(r.status)
		status, contentType = r.status, "application/problem+json"
		answer = genclient.ErrorModel{Title: &title, Detail: &detail, Status: &code}
	}
	body, err := json.Marshal(answer)
	if err != nil {
		return nil, fmt.Errorf("encoding extmsg answer: %w", err)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

func (r *extMsgAPIRecorder) recorded() []extMsgRouteRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]extMsgRouteRequest(nil), r.requests...)
}

// extMsgRouteFixture drives the real API routing ladder for one extmsg
// command: the controller identity, the supervisor-managed client and the
// grant predicates sit behind the apiRoute hooks, the supervisor's endpoint
// and a standalone [api] endpoint (through http.DefaultTransport) are
// recorders, and every global is restored when the test ends. Tests using it
// must not run in parallel.
type extMsgRouteFixture struct {
	cityPath            string
	supervisor          *extMsgAPIRecorder
	standalone          *extMsgAPIRecorder
	supervisorResolves  int
	supervisorAvailable bool
}

const extMsgRouteCityName = "t"

func newExtMsgRouteFixture(t *testing.T, cityTOML string, identity controllerIdentityReply) *extMsgRouteFixture {
	t.Helper()
	f := &extMsgRouteFixture{
		cityPath:            writeCityTOMLForRoute(t, t.TempDir(), "[workspace]\nname = \""+extMsgRouteCityName+"\"\n"+cityTOML),
		supervisor:          &extMsgAPIRecorder{t: t},
		standalone:          &extMsgAPIRecorder{t: t},
		supervisorAvailable: true,
	}
	t.Setenv("GC_NO_API", "")
	t.Setenv("GC_HOME", t.TempDir())

	origCity := cityFlag
	origTransport := http.DefaultTransport
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	origGrants, origWriteGrants := apiRouteSupervisorGrantsHook, apiRouteSupervisorWriteGrantsHook
	t.Cleanup(func() {
		cityFlag = origCity
		http.DefaultTransport = origTransport
		apiRouteControllerIdentityHook, apiRouteSupervisorClientHook = origIdentity, origSup
		apiRouteSupervisorGrantsHook, apiRouteSupervisorWriteGrantsHook = origGrants, origWriteGrants
	})

	cityFlag = f.cityPath
	http.DefaultTransport = f.standalone
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return identity }
	apiRouteSupervisorClientHook = func(string) *api.Client {
		f.supervisorResolves++
		if !f.supervisorAvailable {
			return nil
		}
		return api.NewCityScopedClientWithHTTPClient("http://supervisor.test", extMsgRouteCityName, &http.Client{Transport: f.supervisor})
	}
	apiRouteSupervisorGrantsHook = func() bool { return false }
	apiRouteSupervisorWriteGrantsHook = func() bool { return false }
	return f
}

// extMsgVerbCase runs one extmsg verb with valid arguments against the
// fixture's city.
type extMsgVerbCase struct {
	verb      string
	path      string
	agentName string
	replace   bool
	wantOut   string
	run       func(stdout, stderr io.Writer) int
}

func extMsgVerbCases() []extMsgVerbCase {
	conv := extMsgConversationFlags{scopeID: "scope-1", provider: "discord", accountID: "acct", conversationID: "conv-1", kind: "room"}
	return []extMsgVerbCase{
		{
			verb: "bind", path: "/v0/city/" + extMsgRouteCityName + "/extmsg/bind", agentName: "myrig/front",
			wantOut: "bound discord/conv-1 -> agent myrig/front (binding bind-1, generation 1)\n",
			run: func(stdout, stderr io.Writer) int {
				return cmdExtMsgBind(conv, "myrig/front", "", false, false, stdout, stderr)
			},
		},
		{
			verb: "handoff", path: "/v0/city/" + extMsgRouteCityName + "/extmsg/bind", agentName: "myrig/specialist", replace: true,
			wantOut: "handed off discord/conv-1 -> agent myrig/specialist (binding bind-1, generation 1)\n",
			run: func(stdout, stderr io.Writer) int {
				return cmdExtMsgBind(conv, "myrig/specialist", "", true, false, stdout, stderr)
			},
		},
		{
			verb: "unbind", path: "/v0/city/" + extMsgRouteCityName + "/extmsg/unbind", agentName: "myrig/front",
			wantOut: "unbound discord/conv-1 from agent myrig/front (binding bind-1)\n",
			run: func(stdout, stderr io.Writer) int {
				return cmdExtMsgUnbind(conv, "myrig/front", "", false, stdout, stderr)
			},
		},
	}
}

// assertExtMsgMutation checks that requests is exactly the one POST verb
// sends, to its city-scoped route on host, carrying its conversation and
// target.
func assertExtMsgMutation(t *testing.T, vc extMsgVerbCase, host string, requests []extMsgRouteRequest) {
	t.Helper()
	if len(requests) != 1 {
		t.Fatalf("extmsg %s requests = %+v, want exactly one", vc.verb, requests)
	}
	req := requests[0]
	if req.method != http.MethodPost || req.host != host || req.path != vc.path {
		t.Fatalf("extmsg %s request = %s %s%s, want POST %s%s", vc.verb, req.method, req.host, req.path, host, vc.path)
	}
	conv, agent := req.unbind.Conversation, req.unbind.AgentName
	if vc.path != "/v0/city/"+extMsgRouteCityName+"/extmsg/unbind" {
		conv, agent = req.bind.Conversation, req.bind.AgentName
		gotReplace := req.bind.Replace != nil && *req.bind.Replace
		if gotReplace != vc.replace {
			t.Fatalf("extmsg %s replace = %v, want %v", vc.verb, gotReplace, vc.replace)
		}
	}
	if conv == nil || conv.Provider != "discord" || conv.ConversationId != "conv-1" || conv.ScopeId != "scope-1" || conv.AccountId != "acct" || conv.Kind != "room" {
		t.Fatalf("extmsg %s conversation = %+v, want discord/conv-1 in scope-1", vc.verb, conv)
	}
	if agent == nil || *agent != vc.agentName {
		t.Fatalf("extmsg %s agent = %v, want %q", vc.verb, agent, vc.agentName)
	}
}

// TestCmdExtMsgVerbsRouteMutationsToSupervisor pins that bind, handoff and
// unbind, which are mutations with no local fallback, reach a
// supervisor-hosted city's supervisor when it requires only read grants: its
// read gate passes POSTs through, so declining it would strand the command.
// A supervisor requiring no grants is the positive control, and a refusal
// from the supervisor exits nonzero after the one request.
func TestCmdExtMsgVerbsRouteMutationsToSupervisor(t *testing.T) {
	hosted := controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	for _, vc := range extMsgVerbCases() {
		t.Run(vc.verb+"/read-grant-only supervisor", func(t *testing.T) {
			f := newExtMsgRouteFixture(t, "", hosted)
			useReadAuthOnlySupervisor(t)

			var stdout, stderr bytes.Buffer
			if code := vc.run(&stdout, &stderr); code != 0 {
				t.Fatalf("extmsg %s code = %d, want 0; stderr = %q", vc.verb, code, stderr.String())
			}
			assertExtMsgMutation(t, vc, "supervisor.test", f.supervisor.recorded())
			if got := stdout.String(); got != vc.wantOut {
				t.Fatalf("extmsg %s stdout = %q, want %q", vc.verb, got, vc.wantOut)
			}
			if got := f.standalone.recorded(); len(got) != 0 {
				t.Fatalf("extmsg %s sent standalone requests %+v, want none", vc.verb, got)
			}
		})
		t.Run(vc.verb+"/supervisor without grants", func(t *testing.T) {
			f := newExtMsgRouteFixture(t, "", hosted)

			var stdout, stderr bytes.Buffer
			if code := vc.run(&stdout, &stderr); code != 0 {
				t.Fatalf("extmsg %s code = %d, want 0; stderr = %q", vc.verb, code, stderr.String())
			}
			assertExtMsgMutation(t, vc, "supervisor.test", f.supervisor.recorded())
			if got := stdout.String(); got != vc.wantOut {
				t.Fatalf("extmsg %s stdout = %q, want %q", vc.verb, got, vc.wantOut)
			}
		})
		t.Run(vc.verb+"/supervisor refuses", func(t *testing.T) {
			f := newExtMsgRouteFixture(t, "", hosted)
			useReadAuthOnlySupervisor(t)
			f.supervisor.status = http.StatusForbidden

			var stdout, stderr bytes.Buffer
			if code := vc.run(&stdout, &stderr); code != 1 {
				t.Fatalf("extmsg %s code = %d, want 1 on a refused mutation; stderr = %q", vc.verb, code, stderr.String())
			}
			assertExtMsgMutation(t, vc, "supervisor.test", f.supervisor.recorded())
			if stdout.Len() != 0 {
				t.Fatalf("extmsg %s stdout = %q, want empty on refusal", vc.verb, stdout.String())
			}
			if !strings.Contains(stderr.String(), "gc extmsg "+vc.verb+":") {
				t.Fatalf("extmsg %s stderr = %q, want the refusal reported", vc.verb, stderr.String())
			}
		})
	}
}

// TestCmdExtMsgUnknownHostingRoutesToSupervisor pins that an extmsg verb
// reaches the supervisor for a live controller of unknown hosting mode (a
// supervisor predating identify) whose city.toml names no usable [api]
// endpoint: the mutation has no local fallback, so the supervisor is its only
// endpoint. A usable loopback [api] port is the control: that controller
// serves the city itself.
func TestCmdExtMsgUnknownHostingRoutesToSupervisor(t *testing.T) {
	unknown := controllerIdentityReply{PID: 4242, HostingMode: controllerHostingUnknown}
	cases := []struct {
		name     string
		cityTOML string
		wantHost string
	}{
		{name: "no api port", wantHost: "supervisor.test"},
		{name: "non-loopback api bind", cityTOML: "\n[api]\nport = 9443\nbind = \"0.0.0.0\"\n", wantHost: "supervisor.test"},
		{name: "usable api port", cityTOML: "\n[api]\nport = 9443\n", wantHost: "127.0.0.1:9443"},
	}
	for _, tc := range cases {
		for _, vc := range extMsgVerbCases() {
			t.Run(tc.name+"/"+vc.verb, func(t *testing.T) {
				f := newExtMsgRouteFixture(t, tc.cityTOML, unknown)

				var stdout, stderr bytes.Buffer
				if code := vc.run(&stdout, &stderr); code != 0 {
					t.Fatalf("extmsg %s code = %d, want 0; stderr = %q", vc.verb, code, stderr.String())
				}
				served, other := f.supervisor, f.standalone
				if tc.wantHost != "supervisor.test" {
					served, other = f.standalone, f.supervisor
				}
				assertExtMsgMutation(t, vc, tc.wantHost, served.recorded())
				if got := other.recorded(); len(got) != 0 {
					t.Fatalf("extmsg %s sent requests %+v to the other endpoint, want none", vc.verb, got)
				}
				if got := stdout.String(); got != vc.wantOut {
					t.Fatalf("extmsg %s stdout = %q, want %q", vc.verb, got, vc.wantOut)
				}
			})
		}
	}
}

// useWriteAuthSupervisor points GC_HOME at a supervisor.toml that requires a
// signed city write grant, and puts the real grant predicates behind the
// routing hooks until the test ends.
func useWriteAuthSupervisor(t *testing.T) {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())
	const toml = "[supervisor]\nwrite_auth_verify_key = \"k1:AAAA\"\n"
	if err := os.WriteFile(supervisor.ConfigPath(), []byte(toml), 0o644); err != nil {
		t.Fatalf("writing supervisor.toml: %v", err)
	}
	prevGrants, prevWriteGrants := apiRouteSupervisorGrantsHook, apiRouteSupervisorWriteGrantsHook
	apiRouteSupervisorGrantsHook = supervisorRequiresCityGrants
	apiRouteSupervisorWriteGrantsHook = supervisorRequiresCityWriteGrants
	t.Cleanup(func() {
		apiRouteSupervisorGrantsHook, apiRouteSupervisorWriteGrantsHook = prevGrants, prevWriteGrants
	})
}

// TestCmdExtMsgWriteGrantSupervisorReportsGrantRequired pins that a
// supervisor-hosted city whose supervisor.toml requires city write grants
// refuses each extmsg verb up front with the grant diagnostic: gc mints no
// grant, so the supervisor client is never resolved and nothing is sent.
func TestCmdExtMsgWriteGrantSupervisorReportsGrantRequired(t *testing.T) {
	hosted := controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	for _, vc := range extMsgVerbCases() {
		t.Run(vc.verb, func(t *testing.T) {
			f := newExtMsgRouteFixture(t, "", hosted)
			useWriteAuthSupervisor(t)

			var stdout, stderr bytes.Buffer
			if code := vc.run(&stdout, &stderr); code != 1 {
				t.Fatalf("extmsg %s code = %d, want 1; stderr = %q", vc.verb, code, stderr.String())
			}
			want := "gc extmsg " + vc.verb + ": the supervisor requires a signed city write grant (grant-required), which gc cannot attach to this command yet\n"
			if got := stderr.String(); got != want {
				t.Fatalf("extmsg %s stderr = %q, want %q", vc.verb, got, want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("extmsg %s stdout = %q, want empty", vc.verb, stdout.String())
			}
			if f.supervisorResolves != 0 {
				t.Fatalf("extmsg %s resolved the supervisor client %d times, want 0", vc.verb, f.supervisorResolves)
			}
			if got := append(f.supervisor.recorded(), f.standalone.recorded()...); len(got) != 0 {
				t.Fatalf("extmsg %s sent requests %+v, want none", vc.verb, got)
			}
		})
	}
}

// TestCmdExtMsgNoClientReportsReason pins that an extmsg verb whose API
// resolution yields no client exits 1 naming the routing reason, sends
// nothing, and takes no local fallback.
func TestCmdExtMsgNoClientReportsReason(t *testing.T) {
	cases := []struct {
		name                string
		cityTOML            string
		identity            controllerIdentityReply
		noAPI               string
		supervisorAvailable bool
		wantReason          string
	}{
		{
			name:                "escape hatch",
			identity:            controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor},
			noAPI:               "1",
			supervisorAvailable: true,
			wantReason:          "escape-hatch",
		},
		{
			name:       "no controller or supervisor",
			identity:   controllerIdentityReply{},
			wantReason: "controller-down",
		},
		{
			name:                "standalone non-loopback bind",
			cityTOML:            "\n[api]\nport = 9443\nbind = \"0.0.0.0\"\n",
			identity:            controllerIdentityReply{PID: 4242, HostingMode: controllerHostingStandalone},
			supervisorAvailable: true,
			wantReason:          "non-loopback-bind",
		},
	}
	for _, tc := range cases {
		for _, vc := range extMsgVerbCases() {
			t.Run(tc.name+"/"+vc.verb, func(t *testing.T) {
				f := newExtMsgRouteFixture(t, tc.cityTOML, tc.identity)
				f.supervisorAvailable = tc.supervisorAvailable
				t.Setenv("GC_NO_API", tc.noAPI)

				var stdout, stderr bytes.Buffer
				if code := vc.run(&stdout, &stderr); code != 1 {
					t.Fatalf("extmsg %s code = %d, want 1; stderr = %q", vc.verb, code, stderr.String())
				}
				want := "gc extmsg " + vc.verb + ": requires the city API server (" + tc.wantReason + "; no local fallback for conversation bindings)\n"
				if got := stderr.String(); got != want {
					t.Fatalf("extmsg %s stderr = %q, want %q", vc.verb, got, want)
				}
				if stdout.Len() != 0 {
					t.Fatalf("extmsg %s stdout = %q, want empty", vc.verb, stdout.String())
				}
				if got := append(f.supervisor.recorded(), f.standalone.recorded()...); len(got) != 0 {
					t.Fatalf("extmsg %s sent requests %+v, want none", vc.verb, got)
				}
			})
		}
	}
}

// TestCmdExtMsgStandaloneRouting pins that a standalone controller serving
// the city on a usable loopback [api] port gets each extmsg verb on that
// endpoint, and that the supervisor client is never resolved.
func TestCmdExtMsgStandaloneRouting(t *testing.T) {
	standalone := controllerIdentityReply{PID: 4242, HostingMode: controllerHostingStandalone}
	for _, vc := range extMsgVerbCases() {
		t.Run(vc.verb, func(t *testing.T) {
			f := newExtMsgRouteFixture(t, "\n[api]\nport = 9443\n", standalone)

			var stdout, stderr bytes.Buffer
			if code := vc.run(&stdout, &stderr); code != 0 {
				t.Fatalf("extmsg %s code = %d, want 0; stderr = %q", vc.verb, code, stderr.String())
			}
			assertExtMsgMutation(t, vc, "127.0.0.1:9443", f.standalone.recorded())
			if got := stdout.String(); got != vc.wantOut {
				t.Fatalf("extmsg %s stdout = %q, want %q", vc.verb, got, vc.wantOut)
			}
			if f.supervisorResolves != 0 {
				t.Fatalf("extmsg %s resolved the supervisor client %d times, want 0", vc.verb, f.supervisorResolves)
			}
		})
	}
}
