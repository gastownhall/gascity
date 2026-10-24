package config

import (
	"reflect"
	"strings"
	"testing"
)

// acpOmittedArgsCase is one provider configuration whose ACPOmittedArgs,
// default transport, and default launch command must agree on every
// resolution path that applies to it.
type acpOmittedArgsCase struct {
	name      string
	provider  string
	providers map[string]ProviderSpec
	// legacy marks providers without a `base =` declaration. Those resolve
	// through lookupProvider's Phase A auto-inheritance, which only
	// ResolveProvider applies; the cache treats them as chain roots.
	legacy bool
	// workspaceCmd sets workspace.start_command, which only ResolveProvider
	// takes: the chain and the cache resolve the provider alone, so the
	// cmd/gc transport helpers bypass the cache when it is set
	// (TestResolveTemplateOpenCodeWrapperACPArgsKeepACPTransport).
	workspaceCmd  string
	wantOmitted   []string
	wantTransport string
	wantLaunch    string
}

func runACPOmittedArgsCases(t *testing.T, cases []acpOmittedArgsCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := &Agent{Name: "worker", Provider: tc.provider}
			var ws *Workspace
			if tc.workspaceCmd != "" {
				ws = &Workspace{StartCommand: tc.workspaceCmd}
			}
			resolved, err := ResolveProvider(agent, ws, tc.providers, func(name string) (string, error) { return name, nil })
			if err != nil {
				t.Fatalf("ResolveProvider: %v", err)
			}
			checkACPOmittedArgsResolution(t, "ResolveProvider", resolved, tc)
			if tc.legacy || tc.workspaceCmd != "" {
				return
			}

			chained, err := ResolveProviderChain(tc.provider, tc.providers[tc.provider], tc.providers)
			if err != nil {
				t.Fatalf("ResolveProviderChain: %v", err)
			}
			checkACPOmittedArgsResolution(t, "ResolveProviderChain", &chained, tc)

			cfg := &City{Providers: tc.providers}
			if err := BuildResolvedProviderCache(cfg); err != nil {
				t.Fatalf("BuildResolvedProviderCache: %v", err)
			}
			cached, ok := ResolvedProviderCached(cfg, tc.provider)
			if !ok {
				t.Fatalf("ResolvedProviderCached(%q) missing", tc.provider)
			}
			checkACPOmittedArgsResolution(t, "ResolvedProviderCached", &cached, tc)
		})
	}
}

func checkACPOmittedArgsResolution(t *testing.T, path string, resolved *ResolvedProvider, tc acpOmittedArgsCase) {
	t.Helper()
	if len(tc.wantOmitted) == 0 {
		if len(resolved.ACPOmittedArgs) != 0 {
			t.Errorf("%s: ACPOmittedArgs = %q, want empty", path, resolved.ACPOmittedArgs)
		}
	} else if !reflect.DeepEqual(resolved.ACPOmittedArgs, tc.wantOmitted) {
		t.Errorf("%s: ACPOmittedArgs = %q, want %q", path, resolved.ACPOmittedArgs, tc.wantOmitted)
	}
	transport := resolved.ProviderSessionCreateTransport()
	if transport != tc.wantTransport {
		t.Errorf("%s: ProviderSessionCreateTransport() = %q, want %q", path, transport, tc.wantTransport)
	}
	launch, err := BuildProviderLaunchCommand("", resolved, nil, transport)
	if err != nil {
		t.Fatalf("%s: BuildProviderLaunchCommand: %v", path, err)
	}
	if launch.Command != tc.wantLaunch {
		t.Errorf("%s: launch command = %q, want %q", path, launch.Command, tc.wantLaunch)
	}
}

// TestResolveProviderACPOmittedArgs covers providers whose ACP launch would
// omit args a more specific layer declared (issue #1504): they record the
// omitted tokens and default to the CLI transport, whose launch keeps them.
func TestResolveProviderACPOmittedArgs(t *testing.T) {
	runACPOmittedArgsCases(t, []acpOmittedArgsCase{
		{
			name:     "wrapper args",
			provider: "opencode-local",
			providers: map[string]ProviderSpec{
				"opencode-local": {Base: basePtr("builtin:opencode"), Args: []string{"-m", "acme/model-x"}},
			},
			wantOmitted:   []string{"-m", "acme/model-x"},
			wantTransport: "",
			wantLaunch:    "opencode -m acme/model-x",
		},
		{
			name:     "wrapper args_append",
			provider: "opencode-local",
			providers: map[string]ProviderSpec{
				"opencode-local": {Base: basePtr("builtin:opencode"), ArgsAppend: []string{"--foo"}},
			},
			wantOmitted:   []string{"--foo"},
			wantTransport: "",
			wantLaunch:    "opencode --foo",
		},
		{
			name:     "legacy same-name override",
			provider: "opencode",
			providers: map[string]ProviderSpec{
				"opencode": {Args: []string{"-m", "acme/model-x"}},
			},
			legacy:        true,
			wantOmitted:   []string{"-m", "acme/model-x"},
			wantTransport: "",
			wantLaunch:    "opencode -m acme/model-x",
		},
		{
			name:     "legacy command match",
			provider: "fast",
			providers: map[string]ProviderSpec{
				"fast": {Command: "opencode", Args: []string{"-m", "acme/model-x"}},
			},
			legacy:        true,
			wantOmitted:   []string{"-m", "acme/model-x"},
			wantTransport: "",
			wantLaunch:    "opencode -m acme/model-x",
		},
		{
			name:     "leaf args over middle acp_args",
			provider: "leaf",
			providers: map[string]ProviderSpec{
				"middle": {Base: basePtr("builtin:opencode"), ACPArgs: []string{"acp", "--port", "0"}},
				"leaf":   {Base: basePtr("provider:middle"), Args: []string{"-m", "acme/model-x"}},
			},
			wantOmitted:   []string{"-m", "acme/model-x"},
			wantTransport: "",
			wantLaunch:    "opencode -m acme/model-x",
		},
		{
			name:     "curated model beside other args",
			provider: "opencode-local",
			providers: map[string]ProviderSpec{
				"opencode-local": {Base: basePtr("builtin:opencode"), Args: []string{"-m", "opencode/big-pickle", "--foo"}},
			},
			wantOmitted:   []string{"--foo"},
			wantTransport: "",
			wantLaunch:    "opencode --foo --model opencode/big-pickle",
		},
		{
			name:     "leaf args over middle args_append",
			provider: "leaf",
			providers: map[string]ProviderSpec{
				"middle": {Base: basePtr("builtin:opencode"), ArgsAppend: []string{"--foo"}},
				"leaf":   {Base: basePtr("provider:middle"), Args: []string{"-m", "acme/model-x"}},
			},
			wantOmitted:   []string{"-m", "acme/model-x"},
			wantTransport: "",
			wantLaunch:    "opencode -m acme/model-x",
		},
		{
			name:     "acp_command without acp_args",
			provider: "opencode-local",
			providers: map[string]ProviderSpec{
				"opencode-local": {Base: basePtr("builtin:opencode"), ACPCommand: "opencode-acp", Args: []string{"-m", "acme/model-x"}},
			},
			wantOmitted:   []string{"-m", "acme/model-x"},
			wantTransport: "",
			wantLaunch:    "opencode -m acme/model-x",
		},
		{
			name:     "workspace start_command",
			provider: "opencode-local",
			providers: map[string]ProviderSpec{
				"opencode-local": {Base: basePtr("builtin:opencode"), Args: []string{"-m", "acme/model-x"}},
			},
			workspaceCmd:  "my-agent --serve",
			wantOmitted:   nil,
			wantTransport: SessionTransportACP,
			wantLaunch:    "my-agent --serve acp",
		},
	})
}

// TestResolveProviderChain_OpenCodeCustomArgsUseCLITransport is the issue
// #1504 reproduction: a custom provider that extends builtin:opencode and
// sets args launches on the CLI transport with those args, instead of an
// ACP launch ("opencode acp") that silently drops them.
func TestResolveProviderChain_OpenCodeCustomArgsUseCLITransport(t *testing.T) {
	leaf := ProviderSpec{
		Base: basePtr("builtin:opencode"),
		Args: []string{"-m", "local/model"},
	}
	r, err := ResolveProviderChain("opencode-local", leaf, customs(map[string]ProviderSpec{}))
	if err != nil {
		t.Fatalf("ResolveProviderChain: %v", err)
	}
	transport := r.ProviderSessionCreateTransport()
	if transport == SessionTransportACP {
		t.Fatalf("ProviderSessionCreateTransport() = %q, want the CLI transport", transport)
	}
	launch, err := BuildProviderLaunchCommand("", &r, nil, transport)
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommand: %v", err)
	}
	if !strings.HasPrefix(launch.Command, "opencode -m local/model") {
		t.Fatalf("launch command = %q, want it to start with %q", launch.Command, "opencode -m local/model")
	}
}

// acpOmittedArgs is the pure layer walk: the args the layers strictly more
// specific than the leaf-most layer that sets acp_args contribute to the
// merged Args, replayed root to leaf (args replaces, args_append extends);
// nil when no layer sets acp_args.
func TestACPOmittedArgsLayerWalk(t *testing.T) {
	tests := []struct {
		name   string
		layers []ProviderSpec // leaf -> root
		want   []string
	}{
		{
			name:   "no acp_args anywhere",
			layers: []ProviderSpec{{Args: []string{"a"}}},
			want:   nil,
		},
		{
			name: "leaf args over builtin acp_args",
			layers: []ProviderSpec{
				{Args: []string{"-m", "x"}},
				{ACPArgs: []string{"acp"}},
			},
			want: []string{"-m", "x"},
		},
		{
			name: "same-layer args and acp_args are never omitted",
			layers: []ProviderSpec{
				{Args: []string{"-m", "x"}, ACPArgs: []string{"acp"}},
			},
			want: nil,
		},
		{
			name: "args below the acp_args layer are not omitted",
			layers: []ProviderSpec{
				{ACPArgs: []string{"acp"}},
				{Args: []string{"-m", "x"}},
			},
			want: nil,
		},
		{
			name: "args_append collected root to leaf",
			layers: []ProviderSpec{
				{ArgsAppend: []string{"leaf-append"}},
				{ArgsAppend: []string{"middle-append"}},
				{ACPArgs: []string{"acp"}},
			},
			want: []string{"middle-append", "leaf-append"},
		},
		{
			name: "args then args_append on one layer",
			layers: []ProviderSpec{
				{Args: []string{"-m", "x"}, ArgsAppend: []string{"leaf-append"}},
				{ACPArgs: []string{"acp"}},
			},
			want: []string{"-m", "x", "leaf-append"},
		},
		{
			name: "args_append masked by a more specific args",
			layers: []ProviderSpec{
				{Args: []string{"-m", "x"}},
				{ArgsAppend: []string{"middle-append"}},
				{ACPArgs: []string{"acp"}},
			},
			want: []string{"-m", "x"},
		},
		{
			name: "args_append masked by a more specific empty args",
			layers: []ProviderSpec{
				{Args: []string{}},
				{ArgsAppend: []string{"middle-append"}},
				{ACPArgs: []string{"acp"}},
			},
			want: nil,
		},
		{
			name: "only the leaf-most args are taken",
			layers: []ProviderSpec{
				{Args: []string{"-m", "leaf"}},
				{Args: []string{"-m", "middle"}},
				{ACPArgs: []string{"acp"}},
			},
			want: []string{"-m", "leaf"},
		},
		{
			name: "args_append on the acp_args layer is not omitted",
			layers: []ProviderSpec{
				{ArgsAppend: []string{"leaf-append"}},
				{ArgsAppend: []string{"acp-layer-append"}, ACPArgs: []string{"acp"}},
			},
			want: []string{"leaf-append"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := acpOmittedArgs(tt.layers); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("acpOmittedArgs = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveProviderACPOmittedArgsKeepsDeclaredACPLaunch covers wrappers
// whose ACP launch drops nothing the CLI launch carries: no args, only
// schema-managed flags (both launches carry them as option defaults, as they
// do the option_defaults spelling), an args_append a more specific args
// replaces, or acp_args on the args layer or a more specific one. They keep
// the ACP default and launch with the effective acp_args.
func TestResolveProviderACPOmittedArgsKeepsDeclaredACPLaunch(t *testing.T) {
	runACPOmittedArgsCases(t, []acpOmittedArgsCase{
		{
			name:     "wrapper without args",
			provider: "opencode-local",
			providers: map[string]ProviderSpec{
				"opencode-local": {Base: basePtr("builtin:opencode")},
			},
			wantTransport: SessionTransportACP,
			wantLaunch:    "opencode acp",
		},
		{
			name:     "curated model args",
			provider: "opencode-local",
			providers: map[string]ProviderSpec{
				"opencode-local": {Base: basePtr("builtin:opencode"), Args: []string{"-m", "opencode/big-pickle"}},
			},
			wantTransport: SessionTransportACP,
			wantLaunch:    "opencode acp --model opencode/big-pickle",
		},
		{
			name:     "curated model option_defaults",
			provider: "opencode-local",
			providers: map[string]ProviderSpec{
				"opencode-local": {Base: basePtr("builtin:opencode"), OptionDefaults: map[string]string{"model": "opencode/big-pickle"}},
			},
			wantTransport: SessionTransportACP,
			wantLaunch:    "opencode acp --model opencode/big-pickle",
		},
		{
			name:     "legacy curated model args",
			provider: "opencode",
			providers: map[string]ProviderSpec{
				"opencode": {Args: []string{"-m", "opencode/big-pickle"}},
			},
			legacy:        true,
			wantTransport: SessionTransportACP,
			wantLaunch:    "opencode acp --model opencode/big-pickle",
		},
		{
			name:     "middle args_append under leaf empty args",
			provider: "leaf",
			providers: map[string]ProviderSpec{
				"middle": {Base: basePtr("builtin:opencode"), ArgsAppend: []string{"--foo"}},
				"leaf":   {Base: basePtr("provider:middle"), Args: []string{}},
			},
			wantTransport: SessionTransportACP,
			wantLaunch:    "opencode acp",
		},
		{
			name:     "explicit empty args",
			provider: "opencode-local",
			providers: map[string]ProviderSpec{
				"opencode-local": {Base: basePtr("builtin:opencode"), Args: []string{}},
			},
			wantTransport: SessionTransportACP,
			wantLaunch:    "opencode acp",
		},
		{
			name:     "args and acp_args on one layer",
			provider: "opencode-local",
			providers: map[string]ProviderSpec{
				"opencode-local": {
					Base:    basePtr("builtin:opencode"),
					Args:    []string{"-m", "acme/model-x"},
					ACPArgs: []string{"acp", "--port", "4096"},
				},
			},
			wantTransport: SessionTransportACP,
			wantLaunch:    "opencode acp --port 4096",
		},
		{
			name:     "leaf acp_args over middle args",
			provider: "leaf",
			providers: map[string]ProviderSpec{
				"middle": {Base: basePtr("builtin:opencode"), Args: []string{"-m", "acme/model-x"}},
				"leaf":   {Base: basePtr("provider:middle"), ACPArgs: []string{"acp", "--port", "4096"}},
			},
			wantTransport: SessionTransportACP,
			wantLaunch:    "opencode acp --port 4096",
		},
	})
}

// TestACPDroppedArgs covers the args an ACP session's warning names: the
// declared args the CLI launch carries and the ACP launch does not. An
// agent's args replace the provider's on the CLI launch, so they are what an
// agent that declares args drops; schema-managed flags ride both launches as
// option defaults and are never named.
func TestACPDroppedArgs(t *testing.T) {
	wrapper := map[string]ProviderSpec{
		"opencode-local": {Base: basePtr("builtin:opencode"), Args: []string{"-m", "acme/model-x"}},
	}
	builtins := BuiltinProviders()
	cases := []struct {
		name      string
		providers map[string]ProviderSpec
		agent     Agent
		ws        *Workspace
		want      []string
	}{
		{
			name:      "wrapper args",
			providers: wrapper,
			agent:     Agent{Name: "worker", Provider: "opencode-local"},
			want:      []string{"-m", "acme/model-x"},
		},
		{
			name:      "agent args",
			providers: builtins,
			agent:     Agent{Name: "worker", Provider: "opencode", Args: []string{"-m", "acme/model-x"}},
			want:      []string{"-m", "acme/model-x"},
		},
		{
			name:      "agent curated model args",
			providers: builtins,
			agent:     Agent{Name: "worker", Provider: "opencode", Args: []string{"-m", "opencode/big-pickle"}},
		},
		{
			name:      "agent curated model beside other args",
			providers: builtins,
			agent:     Agent{Name: "worker", Provider: "opencode", Args: []string{"--foo", "-m", "opencode/big-pickle"}},
			want:      []string{"--foo"},
		},
		{
			name:      "agent args replace wrapper args",
			providers: wrapper,
			agent:     Agent{Name: "worker", Provider: "opencode-local", Args: []string{"--foo"}},
			want:      []string{"--foo"},
		},
		{
			name: "no acp_args",
			providers: map[string]ProviderSpec{
				"custom": {Command: "custom-agent", SupportsACP: boolPtr(true), ACPCommand: "custom-acp", Args: []string{"--foo"}},
			},
			agent: Agent{Name: "worker", Provider: "custom", Args: []string{"--bar"}},
		},
		{
			name:      "workspace start_command",
			providers: wrapper,
			agent:     Agent{Name: "worker", Provider: "opencode-local", Args: []string{"--foo"}},
			ws:        &Workspace{StartCommand: "my-agent --serve"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := tc.agent
			resolved, err := ResolveProvider(&agent, tc.ws, tc.providers, func(name string) (string, error) { return name, nil })
			if err != nil {
				t.Fatalf("ResolveProvider: %v", err)
			}
			if got := resolved.ACPDroppedArgs(&agent); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ACPDroppedArgs = %q, want %q", got, tc.want)
			}
		})
	}
}
