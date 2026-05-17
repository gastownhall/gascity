package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func writeManagementJSONTestCity(t *testing.T, cityPath string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityPath, body)
	writePackToml(t, cityPath, "[pack]\nname = \"test-city\"\nschema = 2\n")
}

func decodeOneJSONLine(t *testing.T, stdout *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want 1: %q", len(lines), stdout.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	return payload
}

func validateManagementJSONPayload(t *testing.T, command []string, stdout *bytes.Buffer) map[string]any {
	t.Helper()
	payload := decodeOneJSONLine(t, stdout)
	schemaRaw, err := readBuiltinSchema(command, jsonSchemaResultRole)
	if err != nil {
		t.Fatalf("read schema for %v: %v", command, err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaRaw))
	if err != nil {
		t.Fatalf("parse schema for %v: %v", command, err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("parse payload for %v: %v\n%s", command, err, stdout.String())
	}
	compiler := jsonschema.NewCompiler()
	schemaURL := strings.Join(command, "/") + "/result.schema.json"
	if err := compiler.AddResource(schemaURL, schemaDoc); err != nil {
		t.Fatalf("add schema resource for %v: %v", command, err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compile schema for %v: %v", command, err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Fatalf("payload for %v does not validate: %v\n%s", command, err, stdout.String())
	}
	return payload
}

func runManagementJSONPayload(t *testing.T, cityPath string, args ...string) map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"--city", cityPath}, args...), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run %v = %d; stderr=%q stdout=%q", args, code, stderr.String(), stdout.String())
	}
	if len(args) < 2 {
		t.Fatalf("management JSON command needs at least two command words: %v", args)
	}
	command := args[:2]
	return validateManagementJSONPayload(t, command, &stdout)
}

func TestAgentAddJSONEmitsOnlySummary(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	writeManagementJSONTestCity(t, cityPath, "[workspace]\nname = \"test-city\"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "agent", "add", "--name", "worker", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run agent add --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	payload := decodeOneJSONLine(t, &stdout)
	if payload["schema_version"] != "1" || payload["ok"] != true || payload["command"] != "agent add" || payload["name"] != "worker" {
		t.Fatalf("payload = %+v", payload)
	}
	if strings.Contains(stdout.String(), "Scaffolded agent") {
		t.Fatalf("stdout contains human text: %q", stdout.String())
	}
}

func TestManagementJSONSuccessPayloadsValidateDeclaredSchemas(t *testing.T) {
	type testCase struct {
		name  string
		setup func(t *testing.T) (cityPath string, args []string)
		check func(t *testing.T, payload map[string]any)
	}
	tests := []testCase{
		{
			name: "agent add",
			setup: func(t *testing.T) (string, []string) {
				cityPath := t.TempDir()
				writeManagementJSONTestCity(t, cityPath, "[workspace]\nname = \"test-city\"\n")
				return cityPath, []string{"agent", "add", "--name", "worker", "--json"}
			},
		},
		{
			name: "agent suspend",
			setup: func(t *testing.T) (string, []string) {
				cityPath := writeManagementJSONAgentCity(t, false)
				return cityPath, []string{"agent", "suspend", "frontend/worker", "--json"}
			},
		},
		{
			name: "agent resume",
			setup: func(t *testing.T) (string, []string) {
				cityPath := writeManagementJSONAgentCity(t, true)
				return cityPath, []string{"agent", "resume", "frontend/worker", "--json"}
			},
		},
		{
			name: "rig add",
			setup: func(t *testing.T) (string, []string) {
				t.Setenv("GC_DOLT", "skip")
				t.Setenv("GC_BEADS", "bd")
				cityPath := t.TempDir()
				writeManagementJSONTestCity(t, cityPath, "[workspace]\nname = \"test-city\"\n")
				return cityPath, []string{"rig", "add", filepath.Join(t.TempDir(), "frontend"), "--prefix", "fe", "--json"}
			},
		},
		{
			name: "rig suspend",
			setup: func(t *testing.T) (string, []string) {
				cityPath := writeManagementJSONRigCity(t)
				return cityPath, []string{"rig", "suspend", "frontend", "--json"}
			},
		},
		{
			name: "rig resume",
			setup: func(t *testing.T) (string, []string) {
				cityPath := writeManagementJSONRigCity(t)
				return cityPath, []string{"rig", "resume", "frontend", "--json"}
			},
		},
		{
			name: "rig remove",
			setup: func(t *testing.T) (string, []string) {
				cityPath := writeManagementJSONRigCity(t)
				return cityPath, []string{"rig", "remove", "frontend", "--json"}
			},
		},
		{
			name: "rig set-endpoint",
			setup: func(t *testing.T) (string, []string) {
				cityPath := writeManagementJSONRigEndpointCity(t)
				return cityPath, []string{"rig", "set-endpoint", "frontend", "--self", "--port", "28232", "--force", "--json"}
			},
			check: func(t *testing.T, payload map[string]any) {
				if got, ok := payload["dry_run"].(bool); !ok || got {
					t.Fatalf("dry_run = %#v, want false", payload["dry_run"])
				}
			},
		},
		{
			name: "wait cancel",
			setup: func(t *testing.T) (string, []string) {
				cityPath, waitID := writeManagementJSONWaitCity(t, waitStatePending)
				return cityPath, []string{"wait", "cancel", waitID, "--json"}
			},
		},
		{
			name: "wait ready",
			setup: func(t *testing.T) (string, []string) {
				cityPath, waitID := writeManagementJSONWaitCity(t, waitStatePending)
				return cityPath, []string{"wait", "ready", waitID, "--json"}
			},
		},
		{
			name: "service restart",
			setup: func(t *testing.T) (string, []string) {
				cityPath := writeManagementJSONServiceCity(t)
				return cityPath, []string{"service", "restart", "review-intake", "--json"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGCEnv(t)
			cityPath, args := tt.setup(t)
			payload := runManagementJSONPayload(t, cityPath, args...)
			if tt.check != nil {
				tt.check(t, payload)
			}
		})
	}
}

func TestAgentSuspendResumeJSONReportsResolvedIdentity(t *testing.T) {
	tests := []struct {
		name             string
		action           string
		input            string
		initialSuspended bool
		chdirRig         bool
		wantName         string
		wantQualified    string
	}{
		{
			name:          "qualified input keeps leaf name",
			action:        "suspend",
			input:         "frontend/worker",
			wantName:      "worker",
			wantQualified: "frontend/worker",
		},
		{
			name:             "bare input with rig context qualifies",
			action:           "resume",
			input:            "worker",
			initialSuspended: true,
			chdirRig:         true,
			wantName:         "worker",
			wantQualified:    "frontend/worker",
		},
		{
			name:             "unqualified city agent remains city scoped",
			action:           "resume",
			input:            "citywide",
			initialSuspended: true,
			wantName:         "citywide",
			wantQualified:    "citywide",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGCEnv(t)
			cityPath := writeManagementJSONAgentCity(t, tt.initialSuspended)
			if tt.chdirRig {
				t.Chdir(filepath.Join(cityPath, "frontend"))
			}
			payload := runManagementJSONPayload(t, cityPath, "agent", tt.action, tt.input, "--json")
			if payload["name"] != tt.wantName || payload["qualified_name"] != tt.wantQualified {
				t.Fatalf("identity = (%#v, %#v), want (%q, %q): %+v",
					payload["name"], payload["qualified_name"], tt.wantName, tt.wantQualified, payload)
			}
		})
	}
}

func TestWaitReadyJSONReportsClosedWaitRetryIdentity(t *testing.T) {
	clearGCEnv(t)
	cityPath, waitID := writeManagementJSONWaitCity(t, waitStateCanceled)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if err := store.Close(waitID); err != nil {
		t.Fatalf("close wait: %v", err)
	}

	payload := runManagementJSONPayload(t, cityPath, "wait", "ready", waitID, "--json")
	if payload["retried"] != true {
		t.Fatalf("retried = %#v, want true: %+v", payload["retried"], payload)
	}
	replacement, ok := payload["ready_wait_id"].(string)
	if !ok || replacement == "" || replacement == waitID {
		t.Fatalf("ready_wait_id = %#v, want replacement different from %q: %+v", payload["ready_wait_id"], waitID, payload)
	}
	if payload["name"] != replacement {
		t.Fatalf("name = %#v, want replacement %q: %+v", payload["name"], replacement, payload)
	}
	original, err := store.Get(waitID)
	if err != nil {
		t.Fatalf("get original wait: %v", err)
	}
	if original.Status != "closed" {
		t.Fatalf("original status = %q, want closed", original.Status)
	}
	created, err := store.Get(replacement)
	if err != nil {
		t.Fatalf("get replacement wait %q: %v", replacement, err)
	}
	if created.Metadata["state"] != waitStateReady || created.Metadata["retried_from_wait"] != waitID {
		t.Fatalf("replacement metadata = %+v", created.Metadata)
	}
}

func TestRigSuspendResumeRemoveJSONEmitOnlySummaries(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeManagementJSONTestCity(t, cityPath, "[workspace]\nname = \"test-city\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"frontend\"\nprefix = \"fe\"\n")

	for _, tc := range []struct {
		args      []string
		command   string
		action    string
		suspended any
	}{
		{[]string{"rig", "suspend", "frontend", "--json"}, "rig suspend", "suspend", true},
		{[]string{"rig", "resume", "frontend", "--json"}, "rig resume", "resume", false},
		{[]string{"rig", "remove", "frontend", "--json"}, "rig remove", "remove", nil},
	} {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"--city", cityPath}, tc.args...), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run %v = %d; stderr=%q stdout=%q", tc.args, code, stderr.String(), stdout.String())
		}
		payload := decodeOneJSONLine(t, &stdout)
		if payload["schema_version"] != "1" || payload["ok"] != true || payload["command"] != tc.command || payload["action"] != tc.action || payload["rig"] != "frontend" {
			t.Fatalf("%v payload = %+v", tc.args, payload)
		}
		if tc.suspended != nil && payload["suspended"] != tc.suspended {
			t.Fatalf("%v suspended = %v, want %v", tc.args, payload["suspended"], tc.suspended)
		}
	}
}

func writeManagementJSONAgentCity(t *testing.T, workerSuspended bool) string {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	workerSuspendedLine := ""
	if workerSuspended {
		workerSuspendedLine = "suspended = true\n"
	}
	writeManagementJSONTestCity(t, cityPath, fmt.Sprintf(`[workspace]
name = "test-city"

[[rigs]]
name = "frontend"
path = %q
prefix = "fe"

[[agent]]
name = "worker"
dir = "frontend"
%s
[[agent]]
name = "citywide"
%s`, rigPath, workerSuspendedLine, workerSuspendedLine))
	return cityPath
}

func writeManagementJSONRigCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeManagementJSONTestCity(t, cityPath, fmt.Sprintf(`[workspace]
name = "test-city"

[[rigs]]
name = "frontend"
path = %q
prefix = "fe"
`, rigPath))
	return cityPath
}

func writeManagementJSONRigEndpointCity(t *testing.T) string {
	t.Helper()
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityPath, rigPath)
	writePackToml(t, cityPath, "[pack]\nname = \"test-city\"\nschema = 2\n")
	writeRigEndpointMetadata(t, cityPath, "hq")
	writeRigEndpointMetadata(t, rigPath, "fe")
	writeRigEndpointRuntimeState(t, cityPath, 3311)
	writeRigEndpointCanonicalConfig(t, rigPath, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "dolt-server.port"), []byte("3311\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origVerify := verifyRigExternalEndpoint
	t.Cleanup(func() { verifyRigExternalEndpoint = origVerify })
	verifyRigExternalEndpoint = func(contract.ConfigState, string, string) error { return nil }
	return cityPath
}

func writeManagementJSONWaitCity(t *testing.T, state string) (string, string) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	cityPath := t.TempDir()
	writeManagementJSONTestCity(t, cityPath, "[workspace]\nname = \"test-city\"\n")
	if err := ensurePersistedScopeLocalFileStore(cityPath); err != nil {
		t.Fatalf("initialize file store: %v", err)
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	wait, err := store.Create(beads.Bead{
		Title:       "wait:test-session",
		Type:        waitBeadType,
		Description: "test wait",
		Labels:      []string{waitBeadLabel},
		Metadata: map[string]string{
			"session_id":       "",
			"kind":             "deps",
			"state":            state,
			"dep_ids":          "dep-test",
			"dep_mode":         "all",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	return cityPath, wait.ID
}

func writeManagementJSONServiceCity(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/city/test-city/service/review-intake/restart" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","action":"restart","service":"review-intake"}`))
	}))
	t.Cleanup(server.Close)
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}
	cityPath := t.TempDir()
	writeManagementJSONTestCity(t, cityPath, fmt.Sprintf(`[workspace]
name = "test-city"

[api]
bind = %q
port = %s

[[service]]
name = "review-intake"

[service.workflow]
contract = "gc.healthz.v1"
`, host, port))
	startFakeControllerSocket(t, cityPath, "1234\n")
	return cityPath
}

func TestRigAddJSONEmitsOnlySummary(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	writeManagementJSONTestCity(t, cityPath, "[workspace]\nname = \"test-city\"\n")
	rigPath := filepath.Join(t.TempDir(), "frontend")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "rig", "add", rigPath, "--prefix", "fe", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run rig add --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	payload := decodeOneJSONLine(t, &stdout)
	if payload["schema_version"] != "1" || payload["ok"] != true || payload["command"] != "rig add" || payload["rig"] != "frontend" || payload["prefix"] != "fe" {
		t.Fatalf("payload = %+v", payload)
	}
	if strings.Contains(stdout.String(), "Adding rig") || strings.Contains(stdout.String(), "Rig added") {
		t.Fatalf("stdout contains human text: %q", stdout.String())
	}
}

func TestManagementJSONSchemasDeclared(t *testing.T) {
	commands := [][]string{
		{"agent", "add"},
		{"agent", "suspend"},
		{"agent", "resume"},
		{"rig", "add"},
		{"rig", "suspend"},
		{"rig", "resume"},
		{"rig", "remove"},
		{"rig", "set-endpoint"},
		{"wait", "cancel"},
		{"wait", "ready"},
		{"service", "restart"},
	}
	for _, command := range commands {
		args := append(append([]string{}, command...), "--json-schema=result")
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run %v = %d; stderr=%q stdout=%q", args, code, stderr.String(), stdout.String())
		}
		var schema map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &schema); err != nil {
			t.Fatalf("%v schema is not JSON: %v\n%s", command, err, stdout.String())
		}
		if schema["$schema"] == "" {
			t.Fatalf("%v schema missing $schema: %+v", command, schema)
		}
	}
}
