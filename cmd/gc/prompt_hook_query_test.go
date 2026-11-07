package main

import (
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Generated query policy must not consume the provider's prompt budget.
// Exercise prime, lint and startup delivery, since they build contexts separately.
func TestGeneratedPromptHookQueryBudget(t *testing.T) {
	const template = "{{.WorkQuery}}\n{{.AssignedInProgressQuery}}\n{{.AssignedReadyQuery}}\n{{.RoutedPoolQuery}}\n"
	const want = "gc hook query work\ngc hook query assigned-in-progress\ngc hook query assigned-ready\ngc hook query routed-pool\n"
	a := &config.Agent{Name: "worker", Dir: "demo", Provider: "opencode", PromptTemplate: "prompts/worker.template.md"}
	rigs := []config.Rig{{Name: "demo", Path: "/city/repo", DefaultBranch: "main"}}
	f := fsys.NewFake()
	f.Files["/city/prompts/worker.template.md"] = []byte(template)
	ctx := buildAgentPromptContext("/city", "city", a, rigs, io.Discard)
	got := renderPrompt(f, "/city", "city", a.PromptTemplate, ctx, "", io.Discard, nil, nil, nil)
	if len(got) > 256 {
		t.Fatalf("prime query delivery = %d bytes, budget 256", len(got))
	}
	if got != want {
		t.Fatalf("prime query delivery = %q, want %q", got, want)
	}
	ctx = lintPromptContext("/city", *a, nil)
	if got := renderPrompt(f, "/city", "city", a.PromptTemplate, ctx, "", io.Discard, nil, nil, nil); got != want {
		t.Fatalf("lint query delivery = %d bytes, want compact commands", len(got))
	}
	p := &agentBuildParams{
		cityPath: "/city", cityName: "city", fs: f, readOnly: true, rigs: rigs,
		workspace: &config.Workspace{Name: "city"}, providers: config.BuiltinProviders(),
		lookPath:   func(name string) (string, error) { return "/bin/" + name, nil },
		beaconTime: testBeaconTime, beadNames: make(map[string]string), stderr: io.Discard,
	}
	tp, err := resolveTemplate(p, a, a.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tp.Prompt) > 512 || !strings.Contains(tp.Prompt, want) {
		t.Fatalf("startup query delivery = %d bytes, want compact commands within 512 bytes", len(tp.Prompt))
	}
}

// The command door must select the existing query builders, including their
// compatibility, federation and custom-query contracts.
func TestHookQueryCommandUsesConfiguredPolicy(t *testing.T) {
	for _, topo := range []config.QueryTopology{{}, {Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}}, federatedHookTopology()} {
		for _, custom := range []string{"", "printf 'custom'"} {
			a := &config.Agent{Name: "worker", Dir: "demo", WorkQuery: custom}
			for _, tt := range []struct{ kind, want string }{
				{"work", a.EffectiveWorkQueryFor(topo)},
				{"assigned-in-progress", a.EffectiveAssignedInProgressQueryFor(topo)},
				{"assigned-ready", a.EffectiveAssignedReadyQueryFor(topo)},
				{"routed-pool", a.EffectiveRoutedPoolQueryFor(topo)},
			} {
				if got := hookQueryCommand(a, topo, tt.kind); got != tt.want {
					t.Errorf("%s: command differs from configured policy", tt.kind)
				}
			}
		}
	}
}
