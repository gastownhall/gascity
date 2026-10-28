//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// staleProviderEnv is what an ambient environment can carry into an op: a
// legacy GC-managed endpoint and a proxy selector from somewhere else.
var staleProviderEnv = []string{
	"GC_DOLT=stale", "GC_DOLT_HOST=stale-host", "GC_DOLT_PORT=1111", "GC_DOLT_USER=stale",
	"GC_DOLT_PASSWORD=stale", "GC_DOLT_DATA_DIR=C:\\stale\\data", "GC_DOLT_LOG_FILE=C:\\stale\\dolt.log",
	"GC_DOLT_STATE_FILE=C:\\stale\\state.json", "GC_DOLT_PID_FILE=C:\\stale\\dolt.pid",
	"GC_DOLT_LOCK_FILE=C:\\stale\\dolt.lock", "GC_DOLT_CONFIG_FILE=C:\\stale\\dolt.yaml",
	"BEADS_DOLT_SERVER_HOST=stale-host", "BEADS_DOLT_SERVER_PORT=1111", "BEADS_DOLT_AUTO_START=false",
	"BEADS_DOLT_PROXIED_SERVER=1",
}

// TestNativeBdProviderInitRunsTheScriptsBdInit pins op_provider_owned_init:
// the bd init argv for each transport and target, run from the scope, and the
// environment run_provider_owned_bd gives it.
func TestNativeBdProviderInitRunsTheScriptsBdInit(t *testing.T) {
	head := []string{"init", "--init-if-missing", "--quiet"}
	proxied := []string{"--proxied-server", "--proxied-server-idle-timeout", "0"}
	tests := []struct {
		name     string
		env      []string
		database string
		flags    []string
		// scrubbed: bd must not see the stale legacy endpoint. Only a
		// direct-external init keeps it, to write the binding it names.
		scrubbed bool
		proxied  bool
	}{
		{
			name:     "direct local",
			env:      []string{"GC_BEADS_TRANSPORT=direct", "GC_BEADS_TARGET=local"},
			database: "hq",
			flags:    []string{"--server"},
			scrubbed: true,
		},
		{
			name:     "direct local without a database",
			env:      []string{"GC_BEADS_TRANSPORT=direct", "GC_BEADS_TARGET=local"},
			flags:    []string{"--server"},
			scrubbed: true,
		},
		{
			name:     "direct external over tcp",
			env:      []string{"GC_BEADS_TRANSPORT=direct", "GC_BEADS_TARGET=external", "GC_DOLT_HOST=db.example", "GC_DOLT_PORT=3317"},
			database: "hq",
			flags:    []string{"--server", "--external", "--server-host", "db.example", "--server-port", "3317"},
		},
		{
			name:     "direct external over a socket",
			env:      []string{"GC_BEADS_TRANSPORT=direct", "GC_BEADS_TARGET=external", "GC_DOLT_HOST=db.example", "GC_DOLT_PORT=3317", "BEADS_DOLT_SERVER_SOCKET=C:\\run\\dolt.sock"},
			database: "hq",
			flags:    []string{"--server", "--external", "--server-socket", "C:\\run\\dolt.sock"},
		},
		{
			name:     "proxied local",
			env:      []string{"GC_BEADS_TRANSPORT=proxied", "GC_BEADS_TARGET=local"},
			database: "hq",
			flags:    proxied,
			scrubbed: true,
			proxied:  true,
		},
		{
			name:     "proxied external over tcp",
			env:      []string{"GC_BEADS_TRANSPORT=proxied", "GC_BEADS_TARGET=external", "GC_BEADS_PROXY_EXTERNAL_HOST=up.example", "GC_BEADS_PROXY_EXTERNAL_PORT=3318"},
			database: "hq",
			flags:    append(append([]string{}, proxied...), "--proxied-server-external-host", "up.example", "--proxied-server-external-port", "3318"),
			proxied:  true,
		},
		{
			name:     "proxied external over a socket",
			env:      []string{"GC_BEADS_TRANSPORT=proxied", "GC_BEADS_TARGET=external", "GC_BEADS_PROXY_EXTERNAL_HOST=up.example", "GC_BEADS_PROXY_EXTERNAL_PORT=3318", "GC_BEADS_PROXY_EXTERNAL_SOCKET=C:\\run\\up.sock"},
			database: "hq",
			flags:    append(append([]string{}, proxied...), "--proxied-server-external-socket-path", "C:\\run\\up.sock"),
			proxied:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNativeBdTest(t)
			scope := n.scope(filepath.Join("rigs", "r1"), nil)
			environ := n.environ(scope, append(append([]string{}, staleProviderEnv...), tt.env...)...)
			args := []string{"init", scope, "gt"}
			if tt.database != "" {
				args = append(args, tt.database)
			}
			if stderr, err := n.run(environ, args...); err != nil {
				t.Fatalf("init: %v (stderr %q)", err, stderr)
			}
			want := append(append(append([]string{}, head...), tt.flags...), "-p", "gt", "--skip-hooks", "--skip-agents")
			if tt.database != "" {
				want = append(want, "--database", tt.database)
			}
			want = append(want, scope)
			call := n.requireArgs(want)[0]
			requireCallDir(t, call, scope)
			requireCallEnv(t, call, "BEADS_DIR", filepath.Join(scope, ".beads"))
			requireCallEnv(t, call, "GC_BEADS_PROVIDER_INIT", "1")
			if tt.proxied {
				requireCallEnv(t, call, "BEADS_DOLT_PROXIED_SERVER", "1")
			} else {
				requireCallEnvAbsent(t, call, "BEADS_DOLT_PROXIED_SERVER")
			}
			if tt.scrubbed {
				requireCallEnvAbsent(t, call, providerOwnedBdScrubbedEnv...)
			} else {
				requireCallEnv(t, call, "GC_DOLT", "stale")
				requireCallEnv(t, call, "BEADS_DOLT_AUTO_START", "false")
			}
		})
	}
}

// TestNativeBdProviderInitRefusesWhatTheScriptRefuses pins the init dies: each
// is status 1 with the script's message, and no bd runs.
func TestNativeBdProviderInitRefusesWhatTheScriptRefuses(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		args []string
		msg  string
	}{
		{
			name: "no prefix",
			env:  []string{"GC_BEADS_TRANSPORT=direct", "GC_BEADS_TARGET=local"},
			args: []string{"init", "SCOPE"},
			msg:  "usage: gc-beads-bd init <dir> <prefix> [dolt_database]",
		},
		{
			name: "no transport",
			env:  []string{"GC_BEADS_TARGET=local"},
			args: []string{"init", "SCOPE", "gt"},
			msg:  "invalid provider-owned beads transport/target: /local",
		},
		{
			name: "an unknown target",
			env:  []string{"GC_BEADS_TRANSPORT=direct", "GC_BEADS_TARGET=remote"},
			args: []string{"init", "SCOPE", "gt"},
			msg:  "invalid provider-owned beads transport/target: direct/remote",
		},
		{
			name: "direct external without a port",
			env:  []string{"GC_BEADS_TRANSPORT=direct", "GC_BEADS_TARGET=external", "GC_DOLT_HOST=db.example"},
			args: []string{"init", "SCOPE", "gt"},
			msg:  "direct-external init requires GC_DOLT_HOST and GC_DOLT_PORT",
		},
		{
			name: "proxied external without a host",
			env:  []string{"GC_BEADS_TRANSPORT=proxied", "GC_BEADS_TARGET=external", "GC_BEADS_PROXY_EXTERNAL_PORT=3318"},
			args: []string{"init", "SCOPE", "gt"},
			msg:  "proxied-external init requires GC_BEADS_PROXY_EXTERNAL_HOST and GC_BEADS_PROXY_EXTERNAL_PORT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNativeBdTest(t)
			scope := n.scope("", nil)
			args := append([]string{}, tt.args...)
			for i, arg := range args {
				if arg == "SCOPE" {
					args[i] = scope
				}
			}
			stderr, err := n.run(n.environ(scope, tt.env...), args...)
			requireScriptExit(t, err, 1)
			if strings.TrimSpace(stderr) != tt.msg {
				t.Fatalf("stderr = %q, want %q", stderr, tt.msg)
			}
			n.requireArgs()
		})
	}
}
