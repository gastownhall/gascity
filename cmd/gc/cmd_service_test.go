package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

type fakeServiceReader struct {
	items []workspacesvc.Status
	get   map[string]workspacesvc.Status
	err   error
}

func (f fakeServiceReader) ListServices() ([]workspacesvc.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

func (f fakeServiceReader) GetService(name string) (workspacesvc.Status, error) {
	if f.err != nil {
		return workspacesvc.Status{}, f.err
	}
	status, ok := f.get[name]
	if !ok {
		return workspacesvc.Status{}, fmt.Errorf("missing service %s", name)
	}
	return status, nil
}

func TestDoServiceListUsesLiveStatuses(t *testing.T) {
	cfg := &config.City{
		Services: []config.Service{{
			Name:     "healthz",
			Workflow: config.ServiceWorkflowConfig{Contract: "gc.healthz.v1"},
		}},
	}
	reader := fakeServiceReader{
		items: []workspacesvc.Status{{
			ServiceName:      "healthz",
			Kind:             "workflow",
			MountPath:        "/svc/healthz",
			PublishMode:      "direct",
			State:            "ready",
			LocalState:       "ready",
			PublicationState: "direct",
			URL:              "http://127.0.0.1:9443/svc/healthz",
		}},
	}

	var stdout, stderr bytes.Buffer
	if code := doServiceList("", cfg, reader, false, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "healthz") || !strings.Contains(out, "ready") || !strings.Contains(out, "http://127.0.0.1:9443/svc/healthz") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestDoServiceDoctorFallsBackToConfigView(t *testing.T) {
	cfg := &config.City{
		Services: []config.Service{{
			Name:        "review-intake",
			PublishMode: "private",
			Workflow:    config.ServiceWorkflowConfig{Contract: "pack.gc/review.v1"},
		}},
	}

	var stdout, stderr bytes.Buffer
	if code := doServiceDoctor("", cfg, nil, "review-intake", false, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Contract:          pack.gc/review.v1") {
		t.Fatalf("missing contract in output:\n%s", out)
	}
	if !strings.Contains(out, "Observed State:    controller API unavailable") {
		t.Fatalf("missing fallback note in output:\n%s", out)
	}
}

func TestDoServiceDoctorMissingService(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doServiceDoctor("", &config.City{}, nil, "missing", false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `service "missing" not found`) {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestDoServiceListJSON(t *testing.T) {
	cfg := &config.City{
		Services: []config.Service{{
			Name:     "healthz",
			Workflow: config.ServiceWorkflowConfig{Contract: "gc.healthz.v1"},
		}},
	}
	reader := fakeServiceReader{
		items: []workspacesvc.Status{{
			ServiceName: "healthz",
			Kind:        "workflow",
			MountPath:   "/svc/healthz",
			State:       "ready",
			LocalState:  "ready",
		}},
	}

	var stdout, stderr bytes.Buffer
	if code := doServiceList("/tmp/city", cfg, reader, true, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var payload struct {
		SchemaVersion string `json:"schema_version"`
		CityPath      string `json:"city_path"`
		Live          bool   `json:"live"`
		Services      []struct {
			ServiceName string `json:"service_name"`
			State       string `json:"state"`
		} `json:"services"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.SchemaVersion != "1" || payload.CityPath != "/tmp/city" || !payload.Live || len(payload.Services) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Services[0].ServiceName != "healthz" || payload.Services[0].State != "ready" {
		t.Fatalf("service = %+v", payload.Services[0])
	}
}

func TestDoServiceDoctorJSON(t *testing.T) {
	cfg := &config.City{
		Services: []config.Service{{
			Name:     "review-intake",
			Workflow: config.ServiceWorkflowConfig{Contract: "pack.gc/review.v1"},
		}},
	}

	var stdout, stderr bytes.Buffer
	if code := doServiceDoctor("/tmp/city", cfg, nil, "review-intake", true, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var payload struct {
		SchemaVersion string `json:"schema_version"`
		Live          bool   `json:"live"`
		ObservedState string `json:"observed_state"`
		Service       struct {
			ServiceName      string `json:"service_name"`
			WorkflowContract string `json:"workflow_contract"`
		} `json:"service"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.SchemaVersion != "1" || payload.Live || payload.ObservedState != "controller_unavailable" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Service.ServiceName != "review-intake" || payload.Service.WorkflowContract != "pack.gc/review.v1" {
		t.Fatalf("service = %+v", payload.Service)
	}
}

// serviceRestartGrantRefusal is the refusal `gc service restart` prints for a
// supervisor that requires a city write grant. It names no remote --context:
// the command does not accept one yet.
const serviceRestartGrantRefusal = "gc service restart: the supervisor requires a signed city write grant (grant-required), which gc cannot attach to this command yet\n"

// TestServiceRestartClientGrantGatedSupervisor proves `gc service restart`
// never posts to a supervisor that requires city write grants: that client
// mints no grant, so the restart would be refused with a 401. Such a
// supervisor yields (nil, "grant-required") and the command fails without an
// HTTP call; one that requires no grants, or only read grants (its read gate
// passes the restart's POST through), gets the supervisor-managed client. A
// supervisor-hosted city never takes its [api] port, which the supervisor
// ignores; a standalone controller, or one predating identify, does.
func TestServiceRestartClientGrantGatedSupervisor(t *testing.T) {
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	origGrants, origWriteGrants := apiRouteSupervisorGrantsHook, apiRouteSupervisorWriteGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
		apiRouteSupervisorWriteGrantsHook = origWriteGrants
	})
	supervisor := api.NewCityScopedClient("http://supervisor.sentinel:1", "t")
	// The restart is a mutation, so only the write-grant requirement decides
	// it; the read-or-write requirement is apiClient's.
	apiRouteSupervisorGrantsHook = func() bool {
		t.Errorf("read-or-write grant requirement consulted for the restart mutation")
		return true
	}
	var (
		supervisorHosted = controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
		standalone       = controllerIdentityReply{PID: 4242, HostingMode: controllerHostingStandalone}
		legacy           = controllerIdentityReply{PID: 4242}
	)
	cases := []struct {
		name     string
		identity controllerIdentityReply
		apiPort  int
		// noSupervisor makes the supervisor-managed client unavailable.
		noSupervisor bool
		writeGrants  bool
		readAuthOnly bool
		want         string // "supervisor", "api-port" or "" (nil)
		wantReason   string
	}{
		{name: "write-grants-required", identity: supervisorHosted, writeGrants: true, wantReason: "grant-required"},
		{name: "write-grants-required-with-api-port", identity: supervisorHosted, apiPort: 9443, writeGrants: true, wantReason: "grant-required"},
		{name: "no-controller-write-grants-required", writeGrants: true, wantReason: "grant-required"},
		{name: "grants-not-required", identity: supervisorHosted, want: "supervisor"},
		{name: "supervisor-hosted-with-api-port-uses-supervisor", identity: supervisorHosted, apiPort: 9443, want: "supervisor"},
		{name: "read-grants-only", identity: supervisorHosted, readAuthOnly: true, want: "supervisor"},
		{name: "standalone-with-api-port", identity: standalone, apiPort: 8080, writeGrants: true, want: "api-port"},
		{name: "unknown-hosting-with-api-port", identity: legacy, apiPort: 8080, writeGrants: true, want: "api-port"},
		{name: "no-supervisor-client", identity: supervisorHosted, noSupervisor: true, writeGrants: true, wantReason: "controller-down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return tc.identity }
			apiRouteSupervisorClientHook = func(string) *api.Client {
				if tc.want == "api-port" {
					t.Errorf("supervisor client resolved for a controller that serves its own [api] port")
				}
				if tc.noSupervisor {
					return nil
				}
				return supervisor
			}
			apiRouteSupervisorWriteGrantsHook = func() bool { return tc.writeGrants }
			if tc.readAuthOnly {
				useReadAuthOnlySupervisor(t)
			}
			cfg := &config.City{}
			cfg.API.Port = tc.apiPort
			client, reason := serviceRestartClient(t.TempDir(), cfg)
			switch tc.want {
			case "supervisor":
				if client != supervisor {
					t.Fatalf("serviceRestartClient client = %p, want supervisor %p", client, supervisor)
				}
			case "api-port":
				if client == nil || client == supervisor {
					t.Fatalf("serviceRestartClient client = %p, want the [api] port %d client", client, tc.apiPort)
				}
			default:
				if client != nil {
					t.Fatalf("serviceRestartClient client = %p, want nil", client)
				}
			}
			if reason != tc.wantReason {
				t.Fatalf("serviceRestartClient reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// TestDoServiceRestartRefusals verifies a restart with no client names why: a
// supervisor that requires a city write grant, or no controller at all.
func TestDoServiceRestartRefusals(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{reason: "grant-required", want: serviceRestartGrantRefusal},
		{reason: "controller-down", want: "gc service restart: controller is not running\n"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := doServiceRestart(nil, tc.reason, "web", &stdout, &stderr); code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if stderr.String() != tc.want {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.want)
			}
		})
	}
}

// TestCmdServiceRestartGrantGatedSupervisor pins `gc service restart` to
// serviceRestartClient's write-grant check: for a supervisor-hosted city whose
// supervisor requires city write grants, the command exits 1 with the grant
// refusal and makes no HTTP call, though city.toml sets an [api] port.
func TestCmdServiceRestartGrantGatedSupervisor(t *testing.T) {
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	origGrants, origWriteGrants := apiRouteSupervisorGrantsHook, apiRouteSupervisorWriteGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
		apiRouteSupervisorWriteGrantsHook = origWriteGrants
	})
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
		return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	}
	apiRouteSupervisorClientHook = func(string) *api.Client {
		return api.NewCityScopedClient("http://supervisor.sentinel:1", "t")
	}
	apiRouteSupervisorGrantsHook = func() bool { return true }
	apiRouteSupervisorWriteGrantsHook = func() bool { return true }
	t.Setenv("GC_CITY", writeCityTOMLForRoute(t, t.TempDir(), "[workspace]\nname = \"t\"\n\n[api]\nport = 9443\n"))

	var stdout, stderr bytes.Buffer
	if code := cmdServiceRestart("web", &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), serviceRestartGrantRefusal) {
		t.Errorf("stderr missing the grant-required refusal %q:\n%s", serviceRestartGrantRefusal, stderr.String())
	}
}
