package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// writeSupervisorTOMLForTest writes body as the isolated supervisor.toml that
// clearGCEnv's GC_HOME resolves.
func writeSupervisorTOMLForTest(t *testing.T, body string) {
	t.Helper()
	path := supervisor.ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubNoSupervisorForStatusAPIURL keeps a supervisor or service manager on
// the test host from answering for the isolated GC_HOME.
func stubNoSupervisorForStatusAPIURL(t *testing.T) {
	t.Helper()
	origAlive := supervisorAliveHook
	supervisorAliveHook = func() int { return 0 }
	origSM := supervisorServiceManagerActive
	supervisorServiceManagerActive = func() bool { return false }
	origReach := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() {
		supervisorAliveHook = origAlive
		supervisorServiceManagerActive = origSM
		supervisorAPIReachable = origReach
	})
}

func runSupervisorStatusJSONForAPIURL(t *testing.T) (map[string]any, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	run([]string{"supervisor", "status", "--json"}, &stdout, &stderr)
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	return payload, stderr.String()
}

func TestSupervisorStatusJSONReportsAPIURLFromConfig(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string
	}{
		{name: "default port and bind", toml: "[supervisor]\n", want: "http://127.0.0.1:8372"},
		{name: "configured port", toml: "[supervisor]\nport = 9555\n", want: "http://127.0.0.1:9555"},
		{name: "wildcard bind dials loopback", toml: "[supervisor]\nbind = \"0.0.0.0\"\nport = 9556\n", want: "http://127.0.0.1:9556"},
		{name: "ipv6 loopback bind", toml: "[supervisor]\nbind = \"::1\"\nport = 9557\n", want: "http://[::1]:9557"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			stubNoSupervisorForStatusAPIURL(t)
			writeSupervisorTOMLForTest(t, tc.toml)

			payload, _ := runSupervisorStatusJSONForAPIURL(t)
			if payload["running"] != false {
				t.Fatalf("running = %v, want false with no supervisor", payload["running"])
			}
			if got := payload["api_url"]; got != tc.want {
				t.Fatalf("api_url = %v, want %q; payload=%v", got, tc.want, payload)
			}
			if want, err := supervisorAPIBaseURL(); err != nil || want != tc.want {
				t.Fatalf("supervisorAPIBaseURL() = %q, %v; api_url must match the CLI's own endpoint %q", want, err, tc.want)
			}
		})
	}
}

func TestSupervisorStatusJSONOmitsAPIURLWhenConfigUnreadable(t *testing.T) {
	clearGCEnv(t)
	stubNoSupervisorForStatusAPIURL(t)
	writeSupervisorTOMLForTest(t, "[supervisor\nport = \n")

	payload, stderr := runSupervisorStatusJSONForAPIURL(t)
	if _, ok := payload["api_url"]; ok {
		t.Fatalf("api_url = %v, want absent when supervisor.toml cannot be parsed", payload["api_url"])
	}
	if !bytes.Contains([]byte(stderr), []byte("API URL")) {
		t.Fatalf("stderr = %q, want a warning naming the unresolved API URL", stderr)
	}
}

func cityStatusJSONForAPIURL(t *testing.T, cfg *config.City, cityPath string) map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := doCityStatusJSON(runtime.NewFake(), cfg, cityPath, &stdout, &stderr); code != 0 {
		t.Fatalf("doCityStatusJSON = %d; stderr=%s", code, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	return payload
}

func TestCityStatusJSONReportsStandaloneAPIURLFromConfig(t *testing.T) {
	cases := []struct {
		name string
		api  config.APIConfig
		want string // "" means absent
	}{
		{name: "default bind", api: config.APIConfig{Port: 9123}, want: "http://127.0.0.1:9123"},
		{name: "configured loopback bind", api: config.APIConfig{Bind: "localhost", Port: 9124}, want: "http://localhost:9124"},
		{name: "non-loopback bind with mutations", api: config.APIConfig{Bind: "10.1.2.3", Port: 9125, AllowMutations: true}, want: "http://10.1.2.3:9125"},
		{name: "non-loopback read-only bind", api: config.APIConfig{Bind: "10.1.2.3", Port: 9126}, want: ""},
		{name: "no api port", api: config.APIConfig{}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			stubNoSupervisorForStatusAPIURL(t)
			writeSupervisorTOMLForTest(t, "[supervisor]\nport = 9555\n")
			cityPath := t.TempDir()
			cfg := &config.City{Workspace: config.Workspace{Name: "bright-lights"}, API: tc.api}

			payload := cityStatusJSONForAPIURL(t, cfg, cityPath)
			got, ok := payload["api_url"]
			if tc.want == "" {
				if ok {
					t.Fatalf("api_url = %v, want absent", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("api_url = %v, want %q", got, tc.want)
			}
		})
	}
}

func TestCityStatusJSONReportsSupervisorAPIURLForRegisteredCity(t *testing.T) {
	clearGCEnv(t)
	stubNoSupervisorForStatusAPIURL(t)
	writeSupervisorTOMLForTest(t, "[supervisor]\nport = 9555\n")
	cityPath := t.TempDir()
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("register city: %v", err)
	}
	// The supervisor ignores the city's [api] port and serves it on its own.
	cfg := &config.City{Workspace: config.Workspace{Name: "bright-lights"}, API: config.APIConfig{Port: 9123}}

	payload := cityStatusJSONForAPIURL(t, cfg, cityPath)
	if got, want := payload["api_url"], "http://127.0.0.1:9555"; got != want {
		t.Fatalf("api_url = %v, want supervisor endpoint %q; payload controller=%v", got, want, payload["controller"])
	}
}

func TestCityStatusJSONAPIRouteReportsAPIURL(t *testing.T) {
	clearGCEnv(t)
	stubNoSupervisorForStatusAPIURL(t)
	writeSupervisorTOMLForTest(t, "[supervisor]\nport = 9555\n")
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "bright-lights"}, API: config.APIConfig{Port: 9123}}
	cr := api.CachedRead[api.StatusView]{Body: api.StatusView{CityName: "bright-lights", CityPath: cityPath}}

	var stdout, stderr bytes.Buffer
	if code := renderCityStatusFromAPI(cityPath, cfg, cr, newFakeDrainOps(), true, &stdout, &stderr); code != 0 {
		t.Fatalf("renderCityStatusFromAPI = %d; stderr=%s", code, stderr.String())
	}
	var status StatusJSON
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if want := "http://127.0.0.1:9123"; status.APIURL != want {
		t.Fatalf("api_url = %q, want %q on the API route", status.APIURL, want)
	}
}
