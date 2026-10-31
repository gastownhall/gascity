//go:build windows

package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNativeBdProviderDirectLifecycleRunsTheScriptsBd pins the direct arm of
// op_provider_owned_lifecycle for a scope whose Dolt bd itself starts: the bd
// each op runs, where, and what a completed binding keeps out of its way.
func TestNativeBdProviderDirectLifecycleRunsTheScriptsBd(t *testing.T) {
	tests := []struct {
		op   string
		want [][]string
	}{
		{"start", [][]string{{"ping"}}},
		{"ensure-ready", [][]string{{"ping"}}},
		{"health", [][]string{{"ping"}}},
		{"probe", [][]string{{"ping"}}},
		{"recover", [][]string{{"dolt", "stop"}, {"ping"}}},
		{"stop", [][]string{{"dolt", "stop"}}},
		{"shutdown", [][]string{{"dolt", "stop"}}},
	}
	for _, tt := range tests {
		t.Run(tt.op, func(t *testing.T) {
			n := newNativeBdTest(t)
			scope := n.scope("", nativeDirectLocalScope)
			if stderr, err := n.run(n.environ(scope, staleProviderEnv...), tt.op); err != nil {
				t.Fatalf("%s: %v (stderr %q)", tt.op, err, stderr)
			}
			for _, call := range n.requireArgs(tt.want...) {
				requireCallDir(t, call, scope)
				requireCallEnv(t, call, "BEADS_DIR", filepath.Join(scope, ".beads"))
				requireCallEnv(t, call, "GC_BEADS_TRANSPORT", "direct")
				requireCallEnvAbsent(t, call, providerOwnedBdScrubbedEnv...)
				requireCallEnvAbsent(t, call, "BEADS_DOLT_PROXIED_SERVER", "GC_BEADS_PROVIDER_INIT")
			}
		})
	}
}

// TestNativeBdProviderStopTreatsNothingToStopAsDone pins
// provider_owned_retire_local_dolt: bd's direct stop exits 1 when no server
// runs, and gc stop has to stay re-runnable. Anything else is bd's failure,
// surfaced with bd's own words and status.
func TestNativeBdProviderStopTreatsNothingToStopAsDone(t *testing.T) {
	tests := []struct {
		name   string
		reply  nativeFakeBdReply
		status int
	}{
		{name: "not running, on stderr", reply: nativeFakeBdReply{Exit: 1, Stderr: "Error: dolt server is not running\n"}},
		{name: "no server, on stdout", reply: nativeFakeBdReply{Exit: 1, Stdout: "no server found for this workspace\n"}},
		{name: "any other failure", reply: nativeFakeBdReply{Exit: 3, Stderr: "Error: access denied\n"}, status: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNativeBdTest(t)
			n.replies["dolt stop"] = tt.reply
			scope := n.scope("", nativeDirectLocalScope)
			stderr, err := n.run(n.environ(scope), "stop")
			n.requireArgs([]string{"dolt", "stop"})
			if tt.status == 0 {
				if err != nil {
					t.Fatalf("stop: %v (stderr %q), want nothing-to-stop to succeed", err, stderr)
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != tt.status {
				t.Fatalf("stop error = %v (%T), want bd's own exit status %d", err, err, tt.status)
			}
			if !strings.Contains(stderr, "access denied") {
				t.Fatalf("stderr = %q, want bd's diagnostics", stderr)
			}
		})
	}
}

// TestNativeBdProviderRecoverStopsAtAFailedRetire pins the script's set -e: a
// retire bd refused ends recover before its ping.
func TestNativeBdProviderRecoverStopsAtAFailedRetire(t *testing.T) {
	n := newNativeBdTest(t)
	n.replies["dolt stop"] = nativeFakeBdReply{Exit: 3, Stderr: "Error: access denied\n"}
	scope := n.scope("", nativeDirectLocalScope)
	_, err := n.run(n.environ(scope), "recover")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("recover error = %v (%T), want bd's exit status 3", err, err)
	}
	n.requireArgs([]string{"dolt", "stop"})
}

// TestNativeBdProviderStopsOnlyADoltItOwns pins provider_owned_scope_is_local
// through the direct stop, which retires a Dolt only for a local scope. The
// config checks are grep's: one line at a time, whole-line where anchored.
func TestNativeBdProviderStopsOnlyADoltItOwns(t *testing.T) {
	direct := `{"backend":"dolt","dolt_mode":"server"}`
	tests := []struct {
		name  string
		files map[string]string
		env   []string
		local bool
	}{
		{name: "canonical endpoint bd starts", files: map[string]string{"config.yaml": "gc.endpoint_origin: city_canonical\ndolt.auto-start: true\n"}, local: true},
		{name: "canonical endpoint bd does not start", files: map[string]string{"config.yaml": "gc.endpoint_origin: city_canonical\ndolt.auto-start: false\n"}},
		{name: "canonical endpoint silent on auto-start", files: map[string]string{"config.yaml": "gc.endpoint_origin: city_canonical\n"}},
		{name: "managed city endpoint", files: map[string]string{"config.yaml": "gc.endpoint_origin: managed_city\n"}, local: true},
		{name: "explicit endpoint", files: map[string]string{"config.yaml": "gc.endpoint_origin: explicit\n"}},
		{name: "a configured dolt port", files: map[string]string{"config.yaml": "issue_prefix: gt\ndolt.port: 3317\n"}},
		{name: "no endpoint settings", files: map[string]string{"config.yaml": "issue_prefix: gt\n"}, local: true},
		{name: "no config.yaml", files: map[string]string{"metadata.json": direct}},
		{name: "bd bound to another server", files: map[string]string{"config.yaml": "gc.endpoint_origin: managed_city\n", "metadata.json": `{"backend":"dolt","dolt_mode":"server","dolt_server_host":"db.example"}`}},
		{name: "an empty server binding", files: map[string]string{"config.yaml": "gc.endpoint_origin: managed_city\n", "metadata.json": `{"backend":"dolt","dolt_mode":"server","dolt_server_host":""}`}, local: true},
		{name: "CRLF config", files: map[string]string{"config.yaml": "gc.endpoint_origin: managed_city\r\n"}, local: true},
		{name: "a commented origin is no origin", files: map[string]string{"config.yaml": "gc.endpoint_origin: city_canonical # set by gc\ndolt.auto-start: false\n"}, local: true},
		{name: "an origin split across lines is no origin", files: map[string]string{"config.yaml": "gc.endpoint_origin:\n  explicit\n"}, local: true},
		{name: "target local", files: map[string]string{"metadata.json": direct}, env: []string{"GC_BEADS_TARGET=local"}, local: true},
		{name: "target external", files: map[string]string{"config.yaml": "gc.endpoint_origin: managed_city\n"}, env: []string{"GC_BEADS_TARGET=external"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNativeBdTest(t)
			scope := n.scope("", tt.files)
			if stderr, err := n.run(n.environ(scope, tt.env...), "stop"); err != nil {
				t.Fatalf("stop: %v (stderr %q)", err, stderr)
			}
			if tt.local {
				n.requireArgs([]string{"dolt", "stop"})
			} else {
				n.requireArgs()
			}
		})
	}
}

// TestNativeBdProviderClassifiesTransportAsTheScriptDoes pins
// provider_owned_transport and scope_is_proxied through start, whose ping
// carries the proxy selector exactly when the scope is proxied.
func TestNativeBdProviderClassifiesTransportAsTheScriptDoes(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		env     []string
		proxied bool
	}{
		{name: "bd recorded proxied", files: map[string]string{"metadata.json": `{"backend":"dolt","dolt_mode":"proxied-server"}`}, proxied: true},
		{name: "a recorded mode is normalized", files: map[string]string{"metadata.json": `{"backend":" Dolt ","dolt_mode":"  Proxied-Server # note"}`}, proxied: true},
		{name: "recorded direct beats the selector", files: map[string]string{"metadata.json": `{"backend":"dolt","dolt_mode":"server"}`}, env: []string{"BEADS_DOLT_PROXIED_SERVER=1"}},
		{name: "a legacy config mode beats the selector", files: map[string]string{"config.yaml": "dolt.mode: server\n"}, env: []string{"BEADS_DOLT_PROXIED_SERVER=1"}},
		{name: "an empty legacy config mode does not", files: map[string]string{"config.yaml": "dolt.mode:   \n"}, env: []string{"BEADS_DOLT_PROXIED_SERVER=1"}, proxied: true},
		{name: "an unmarked scope follows the selector", env: []string{"BEADS_DOLT_PROXIED_SERVER=1"}, proxied: true},
		{name: "an unmarked scope is direct", files: nil},
		{name: "a doltlite backend is never proxied", files: map[string]string{"metadata.json": `{"backend":"doltlite","dolt_mode":"proxied-server"}`}},
		{name: "only dolt names the dolt backend", files: map[string]string{"metadata.json": `{"backend":"bd","dolt_mode":"proxied-server"}`}},
		{name: "a doltlite database is never proxied", files: map[string]string{"metadata.json": `{"database":"doltlite","dolt_mode":"proxied-server"}`}},
		{name: "the configured backend decides an unrecorded one", files: map[string]string{"metadata.json": `{"dolt_mode":"proxied-server"}`}, env: []string{"GC_BEADS_BACKEND=doltlite"}},
		{name: "unparseable metadata says nothing", files: map[string]string{"metadata.json": `{not json`}, env: []string{"BEADS_DOLT_PROXIED_SERVER=1"}, proxied: true},
		{name: "an explicit transport wins", files: map[string]string{"metadata.json": `{"backend":"dolt","dolt_mode":"server"}`}, env: []string{"GC_BEADS_TRANSPORT=proxied"}, proxied: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNativeBdTest(t)
			scope := n.scope("", tt.files)
			if stderr, err := n.run(n.environ(scope, tt.env...), "start"); err != nil {
				t.Fatalf("start: %v (stderr %q)", err, stderr)
			}
			call := n.requireArgs([]string{"ping"})[0]
			if tt.proxied {
				requireCallEnv(t, call, "GC_BEADS_TRANSPORT", "proxied")
				requireCallEnv(t, call, "BEADS_DOLT_PROXIED_SERVER", "1")
			} else {
				requireCallEnv(t, call, "GC_BEADS_TRANSPORT", "direct")
				requireCallEnvAbsent(t, call, "BEADS_DOLT_PROXIED_SERVER")
			}
		})
	}
}
