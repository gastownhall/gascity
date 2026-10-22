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
	legacy        bool
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
			name:     "curated model args",
			provider: "opencode-local",
			providers: map[string]ProviderSpec{
				"opencode-local": {Base: basePtr("builtin:opencode"), Args: []string{"-m", "opencode/big-pickle"}},
			},
			wantOmitted:   []string{"-m", "opencode/big-pickle"},
			wantTransport: "",
			wantLaunch:    "opencode --model opencode/big-pickle",
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

// acpOmittedArgs is the pure layer walk: raw args from layers strictly more
// specific than the leaf-most layer that sets acp_args, leaf-most args first,
// then args_append from root to leaf; nil when no layer sets acp_args.
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
			name: "args_append collected root to leaf after args",
			layers: []ProviderSpec{
				{Args: []string{"-m", "x"}, ArgsAppend: []string{"leaf-append"}},
				{ArgsAppend: []string{"root-append"}},
				{ACPArgs: []string{"acp"}},
			},
			want: []string{"-m", "x", "root-append", "leaf-append"},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := acpOmittedArgs(tt.layers); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("acpOmittedArgs = %q, want %q", got, tt.want)
			}
		})
	}
}
