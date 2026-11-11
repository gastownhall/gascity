package main

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func recordedACPLaunchServers() []runtime.MCPServerConfig {
	return []runtime.MCPServerConfig{{Name: "notes", Transport: runtime.MCPTransportStdio, Command: "notes-mcp"}}
}

// cliTemplateWithACPLaunch is a template that now resolves the CLI transport
// for a provider that still supports ACP.
func cliTemplateWithACPLaunch() TemplateParams {
	return TemplateParams{
		TemplateName: "worker",
		SessionName:  "worker",
		Command:      "opencode -m acme/model-x",
		ResolvedProvider: &config.ResolvedProvider{
			Name:        "opencode-local",
			ResumeFlag:  "--session",
			ResumeStyle: "flag",
		},
		ACPLaunch: &TemplateACPLaunch{
			Command:    "opencode acp",
			MCPServers: recordedACPLaunchServers(),
		},
	}
}

// TestTemplateParamsForSession pins that only a session whose recorded
// transport is ACP swaps a CLI template onto its precomputed ACP launch.
func TestTemplateParamsForSession(t *testing.T) {
	cases := []struct {
		name     string
		tp       func() TemplateParams
		info     sessionpkg.Info
		wantSwap bool
	}{
		{
			name:     "recorded acp transport",
			tp:       cliTemplateWithACPLaunch,
			info:     sessionpkg.Info{Transport: "acp"},
			wantSwap: true,
		},
		{
			name:     "recorded acp provider",
			tp:       cliTemplateWithACPLaunch,
			info:     sessionpkg.Info{Provider: "acp"},
			wantSwap: true,
		},
		{
			name:     "recorded mcp identity",
			tp:       cliTemplateWithACPLaunch,
			info:     sessionpkg.Info{MCPIdentity: "worker"},
			wantSwap: true,
		},
		{
			name: "recorded tmux transport",
			tp:   cliTemplateWithACPLaunch,
			info: sessionpkg.Info{Transport: "tmux", MCPIdentity: "worker"},
		},
		{
			name: "unrecorded transport",
			tp:   cliTemplateWithACPLaunch,
			info: sessionpkg.Info{Command: "opencode acp"},
		},
		{
			name: "no acp launch",
			tp: func() TemplateParams {
				tp := cliTemplateWithACPLaunch()
				tp.ACPLaunch = nil
				return tp
			},
			info: sessionpkg.Info{Transport: "acp"},
		},
		{
			name: "acp template",
			tp: func() TemplateParams {
				return TemplateParams{TemplateName: "worker", Command: "opencode acp --port 4096", IsACP: true}
			},
			info: sessionpkg.Info{Transport: "acp"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tp := tc.tp()
			got, swapped := templateParamsForSession(tp, tc.info)
			if swapped != tc.wantSwap {
				t.Fatalf("swapped = %v, want %v", swapped, tc.wantSwap)
			}
			if !tc.wantSwap {
				if !reflect.DeepEqual(got, tp) {
					t.Fatalf("templateParamsForSession changed the template: got %+v, want %+v", got, tp)
				}
				return
			}
			if !got.IsACP {
				t.Error("IsACP = false, want true")
			}
			if got.Command != "opencode acp" {
				t.Errorf("Command = %q, want %q", got.Command, "opencode acp")
			}
			if !reflect.DeepEqual(got.MCPServers, recordedACPLaunchServers()) {
				t.Errorf("MCPServers = %+v, want %+v", got.MCPServers, recordedACPLaunchServers())
			}
			if tp.IsACP || tp.Command != "opencode -m acme/model-x" || len(tp.MCPServers) != 0 {
				t.Errorf("input template mutated: %+v", tp)
			}
		})
	}
}

// TestSessionCoreConfigForHashInfoUsesRecordedACPLaunch is the drift-hash
// half of a recorded ACP session under a template that now resolves the CLI
// transport: the hash config is the ACP launch the session keeps.
func TestSessionCoreConfigForHashInfoUsesRecordedACPLaunch(t *testing.T) {
	tp := cliTemplateWithACPLaunch()

	recorded := sessionCoreConfigForHashInfo(tp, sessionpkg.Info{Transport: "acp"})
	if recorded.Command != "opencode acp" {
		t.Errorf("recorded acp: Command = %q, want %q", recorded.Command, "opencode acp")
	}
	if !reflect.DeepEqual(recorded.MCPServers, recordedACPLaunchServers()) {
		t.Errorf("recorded acp: MCPServers = %+v, want %+v", recorded.MCPServers, recordedACPLaunchServers())
	}

	cli := sessionCoreConfigForHashInfo(tp, sessionpkg.Info{Transport: "tmux"})
	if cli.Command != "opencode -m acme/model-x" {
		t.Errorf("recorded tmux: Command = %q, want %q", cli.Command, "opencode -m acme/model-x")
	}
	if len(cli.MCPServers) != 0 {
		t.Errorf("recorded tmux: MCPServers = %+v, want none", cli.MCPServers)
	}
	if runtime.CoreFingerprint(recorded) == runtime.CoreFingerprint(cli) {
		t.Error("recorded acp and recorded tmux core fingerprints match, want them to differ")
	}
}
