package scripts_test

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Advisory probes (ga-ylpbsy.55). The contract radar (gc HEAD against beads
// main HEAD) and MCP mail conformance (against the pinned mcp_agent_mail)
// are live probes of upstream code. They must never block a merge, so no
// required check waits on them, and they run in their own workflow rather
// than in ci.yml beside Check and CI / required. GitHub reports a red Check
// or CI / required without naming the need that failed, so a tool that
// reads such a red from the failed jobs of its run (the adopt-pr gate's
// infrastructure rerun does) would count a red probe in the same run as part
// of it. Their triggers, runners and shared path filter are ci.yml's.

const advisoryProbesWorkflowFile = "advisory-probes.yml"

// advisoryProbeChecks maps each probe job to its check name.
var advisoryProbeChecks = map[string]string{
	"contract-radar-bd-head": "Contract radar (gc HEAD x bd main HEAD)",
	"mcp-mail":               "MCP mail conformance",
}

// advisoryProbeFilterPaths: the paths each probe's filter must keep (a
// floor, not an exact list). The mail entries include the mcp_agent_mail
// lock (#8310): #8307 bumped it and merged without the conformance job
// running.
var advisoryProbeFilterPaths = map[string][]string{
	"mail": {
		"internal/mail/**",
		"contrib/mail-scripts/**",
		".github/requirements/mcp-agent-mail.in",
		".github/requirements/mcp-agent-mail.overrides.txt",
		".github/requirements/mcp-agent-mail.txt",
	},
	"beads": {
		"go.mod",
		"internal/beads/**",
		"test/acceptance/beads_cli_contract_test.go",
		"deps.env",
		".github/scripts/install-bd-archive.sh",
		"cmd/gc/init_provider_readiness.go",
	},
}

// advisoryProbesDoc is the part of a workflow file the probe tests compare
// beyond its jobs.
type advisoryProbesDoc struct {
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency map[string]string `yaml:"concurrency"`
}

func readAdvisoryProbesDoc(t *testing.T, name string) advisoryProbesDoc {
	t.Helper()
	var doc advisoryProbesDoc
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), filepath.Join(".github", "workflows", name))), &doc); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return doc
}

// withoutInputDescriptions drops the display-only input descriptions from a
// workflow's triggers.
func withoutInputDescriptions(on map[string]any) map[string]any {
	call, ok := on["workflow_call"].(map[string]any)
	if !ok {
		return on
	}
	inputs, ok := call["inputs"].(map[string]any)
	if !ok {
		return on
	}
	for _, raw := range inputs {
		if input, ok := raw.(map[string]any); ok {
			delete(input, "description")
		}
	}
	return on
}

func jobStep(t *testing.T, file, jobID, stepID string, job ciCriticalPathJob) ciCriticalPathStep {
	t.Helper()
	for _, step := range job.Steps {
		if step.ID == stepID {
			return step
		}
	}
	t.Fatalf("%s job %s has no step with id %q", file, jobID, stepID)
	return ciCriticalPathStep{}
}

func pathFilters(t *testing.T, where, source string) map[string][]string {
	t.Helper()
	var filters map[string][]string
	if err := yaml.Unmarshal([]byte(source), &filters); err != nil {
		t.Fatalf("%s filters: %v", where, err)
	}
	return filters
}

// TestAdvisoryProbesRunOutsideTheRequiredCIWorkflow: each probe is a job of
// advisory-probes.yml under its check name, and neither a job of ci.yml, the
// workflow of the required Check and CI / required, nor named like one. No
// probe workflow job carries a required check name, so the probes stay
// advisory.
func TestAdvisoryProbesRunOutsideTheRequiredCIWorkflow(t *testing.T) {
	ci := readCriticalPathWorkflow(t, "ci.yml")
	probes := readCriticalPathWorkflow(t, advisoryProbesWorkflowFile)
	for id, name := range advisoryProbeChecks {
		job, ok := probes.Jobs[id]
		if !ok {
			t.Errorf("%s has no %s job", advisoryProbesWorkflowFile, id)
		} else if job.Name != name {
			t.Errorf("%s job %s is named %q, want its check name %q", advisoryProbesWorkflowFile, id, job.Name, name)
		}
		if _, ok := ci.Jobs[id]; ok {
			t.Errorf("ci.yml still has the advisory %s job; it belongs in %s", id, advisoryProbesWorkflowFile)
		}
		for ciID, ciJob := range ci.Jobs {
			if ciJob.Name == name {
				t.Errorf("ci.yml job %s is named %q, an advisory probe's check name", ciID, name)
			}
		}
	}

	found := requiredCheckJobs(t)
	for required, producers := range found {
		for _, producer := range producers {
			if strings.HasPrefix(producer, advisoryProbesWorkflowFile+":") {
				t.Errorf("%s produces the required check %q; the probes must stay advisory", producer, required)
			}
		}
	}
	for name, want := range map[string]string{"Check": "ci.yml:check", "CI / required": "ci.yml:ci-required"} {
		if !reflect.DeepEqual(found[name], []string{want}) {
			t.Errorf("required check %q is produced by %v, want exactly %s", name, found[name], want)
		}
	}
}

// TestAdvisoryProbesKeepCITriggersInTheirOwnConcurrencyGroup: the probes run
// on exactly the events ci.yml runs on (pushes to main, pull requests, merge
// groups and RC Gate's workflow_call), with ci.yml's read-only token, and
// supersede like ci.yml does (a pull request's newer push cancels its older
// run; push, merge-group and RC runs are never canceled in progress). The
// group's prefix is the workflow's own: concurrency groups are shared by
// every workflow of the repository, so ci.yml's prefix would let a probe run
// cancel or queue behind a CI run.
func TestAdvisoryProbesKeepCITriggersInTheirOwnConcurrencyGroup(t *testing.T) {
	ci := readAdvisoryProbesDoc(t, "ci.yml")
	probes := readAdvisoryProbesDoc(t, advisoryProbesWorkflowFile)

	for _, event := range []string{"workflow_call", "push", "pull_request", "merge_group"} {
		if _, ok := probes.On[event]; !ok {
			t.Errorf("%s has no %s trigger", advisoryProbesWorkflowFile, event)
		}
	}
	if got, want := withoutInputDescriptions(probes.On), withoutInputDescriptions(ci.On); !reflect.DeepEqual(got, want) {
		t.Errorf("%s triggers = %v, want ci.yml's %v", advisoryProbesWorkflowFile, got, want)
	}
	if want := map[string]string{"contents": "read"}; !reflect.DeepEqual(probes.Permissions, want) || !reflect.DeepEqual(ci.Permissions, want) {
		t.Errorf("permissions: %s %v, ci.yml %v; want both %v", advisoryProbesWorkflowFile, probes.Permissions, ci.Permissions, want)
	}

	const ciPrefix, probesPrefix = "ci-", "advisory-probes-"
	ciKey, ok := strings.CutPrefix(ci.Concurrency["group"], ciPrefix)
	if !ok {
		t.Fatalf("ci.yml concurrency group %q does not start with %q", ci.Concurrency["group"], ciPrefix)
	}
	if got, want := probes.Concurrency["group"], probesPrefix+ciKey; got != want {
		t.Errorf("%s concurrency group = %q, want %q (ci.yml's key under the workflow's own prefix)", advisoryProbesWorkflowFile, got, want)
	}
	if got, want := probes.Concurrency["cancel-in-progress"], ci.Concurrency["cancel-in-progress"]; got != want {
		t.Errorf("%s cancel-in-progress = %q, want ci.yml's %q", advisoryProbesWorkflowFile, got, want)
	}

	files, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.y*ml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob workflows: %v (%d files)", err, len(files))
	}
	for _, path := range files {
		if filepath.Base(path) == advisoryProbesWorkflowFile {
			continue
		}
		for _, group := range concurrencyGroups(t, path) {
			if strings.HasPrefix(group, probesPrefix) {
				t.Errorf("%s concurrency group %q uses the %q prefix of %s", filepath.Base(path), group, probesPrefix, advisoryProbesWorkflowFile)
			}
		}
	}
}

// concurrencyGroups returns a workflow's concurrency groups, its own and
// each job's, in either the string or the mapping form.
func concurrencyGroups(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Concurrency yaml.Node `yaml:"concurrency"`
		Jobs        map[string]struct {
			Concurrency yaml.Node `yaml:"concurrency"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatalf("parse %s: %v", filepath.Base(path), err)
	}
	group := func(node yaml.Node) string {
		switch node.Kind {
		case yaml.ScalarNode:
			return node.Value
		case yaml.MappingNode:
			var block struct {
				Group string `yaml:"group"`
			}
			if err := node.Decode(&block); err != nil {
				t.Fatalf("%s concurrency: %v", filepath.Base(path), err)
			}
			return block.Group
		default:
			return ""
		}
	}
	var groups []string
	if g := group(wf.Concurrency); g != "" {
		groups = append(groups, g)
	}
	for _, job := range wf.Jobs {
		if g := group(job.Concurrency); g != "" {
			groups = append(groups, g)
		}
	}
	return groups
}

// TestAdvisoryProbesRouteOnCIPathFilters: one routing job decides, as
// ci.yml's runner-policy and changes jobs did, which runners the probes use
// and whether each runs: the same runner policy script, the same
// paths-filter inputs, the probe's filter folded with ci.yml's exact shared
// filter. The radar reports red; MCP mail stays continue-on-error, because
// upstream mcp_agent_mail's API may drift.
func TestAdvisoryProbesRouteOnCIPathFilters(t *testing.T) {
	ci := readCriticalPathWorkflow(t, "ci.yml")
	probes := readCriticalPathWorkflow(t, advisoryProbesWorkflowFile)
	route, ok := probes.Jobs["route"]
	if !ok {
		t.Fatalf("%s has no route job", advisoryProbesWorkflowFile)
	}
	if len(route.Needs) != 0 || route.If != "" {
		t.Errorf("route needs %v, if %q; want an unconditional first job", route.Needs, route.If)
	}
	if got, want := route.RunsOn, ci.Jobs["runner-policy"].RunsOn; got != want {
		t.Errorf("route runs-on %q, want ci.yml runner-policy's %q", got, want)
	}

	policy := jobStep(t, advisoryProbesWorkflowFile, "route", "policy", route)
	ciPolicy := jobStep(t, "ci.yml", "runner-policy", "policy", ci.Jobs["runner-policy"])
	if strings.TrimSpace(policy.Run) != strings.TrimSpace(ciPolicy.Run) || !reflect.DeepEqual(policy.Env, ciPolicy.Env) {
		t.Errorf("route policy step run %q env %v, want ci.yml's run %q env %v", policy.Run, policy.Env, ciPolicy.Run, ciPolicy.Env)
	}

	filter := jobStep(t, advisoryProbesWorkflowFile, "route", "filter", route)
	ciFilter := jobStep(t, "ci.yml", "changes", "filter", ci.Jobs["changes"])
	if filter.Uses != ciFilter.Uses {
		t.Errorf("route filter uses %q, want ci.yml's %q", filter.Uses, ciFilter.Uses)
	}
	for _, input := range []string{"token", "base", "ref"} {
		if filter.With[input] != ciFilter.With[input] {
			t.Errorf("route filter with.%s = %q, want ci.yml's %q", input, filter.With[input], ciFilter.With[input])
		}
	}
	filters := pathFilters(t, advisoryProbesWorkflowFile, filter.With["filters"])
	ciFilters := pathFilters(t, "ci.yml", ciFilter.With["filters"])
	if got := slices.Sorted(maps.Keys(filters)); !slices.Equal(got, []string{"beads", "mail", "shared"}) {
		t.Errorf("route filters = %v, want [beads mail shared]", got)
	}
	if !slices.Equal(filters["shared"], ciFilters["shared"]) {
		t.Errorf("route shared filter = %v, want ci.yml's %v", filters["shared"], ciFilters["shared"])
	}
	for name, required := range advisoryProbeFilterPaths {
		for _, path := range required {
			if !slices.Contains(filters[name], path) {
				t.Errorf("route %s filter is missing %q", name, path)
			}
		}
	}

	wantOutputs := map[string]string{
		"mail":          "${{ steps.filter.outputs.mail == 'true' || steps.filter.outputs.shared == 'true' }}",
		"beads":         "${{ steps.filter.outputs.beads == 'true' || steps.filter.outputs.shared == 'true' }}",
		"runner_8vcpu":  "${{ steps.policy.outputs.runner_8vcpu }}",
		"runner_32vcpu": "${{ steps.policy.outputs.runner_32vcpu }}",
	}
	if !reflect.DeepEqual(route.Outputs, wantOutputs) {
		t.Errorf("route outputs = %v, want %v", route.Outputs, wantOutputs)
	}

	for id, want := range map[string]struct {
		cond, runsOn    string
		continueOnError bool
	}{
		"contract-radar-bd-head": {"${{ needs.route.outputs.beads == 'true' }}", "${{ needs.route.outputs.runner_32vcpu }}", false},
		"mcp-mail":               {"needs.route.outputs.mail == 'true'", "${{ needs.route.outputs.runner_8vcpu }}", true},
	} {
		job := probes.Jobs[id]
		if !slices.Equal(job.Needs, []string{"route"}) {
			t.Errorf("%s needs %v, want [route]", id, job.Needs)
		}
		if job.If != want.cond {
			t.Errorf("%s if %q, want %q", id, job.If, want.cond)
		}
		if job.RunsOn != want.runsOn {
			t.Errorf("%s runs-on %q, want %q", id, job.RunsOn, want.runsOn)
		}
		if job.ContinueOnError != want.continueOnError {
			t.Errorf("%s continue-on-error %v, want %v", id, job.ContinueOnError, want.continueOnError)
		}
	}
}

// TestRCGateRunsTheAdvisoryProbes: RC Gate calls advisory-probes.yml beside
// its reusable ci.yml call, with the same force_blacksmith input, and its
// summary needs that call, so a red radar fails the gate.
func TestRCGateRunsTheAdvisoryProbes(t *testing.T) {
	var rc struct {
		Jobs map[string]struct {
			Uses        string              `yaml:"uses"`
			With        map[string]any      `yaml:"with"`
			Permissions map[string]string   `yaml:"permissions"`
			Needs       ciCriticalPathNeeds `yaml:"needs"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), filepath.Join(".github", "workflows", "rc-gate.yml"))), &rc); err != nil {
		t.Fatalf("parse rc-gate.yml: %v", err)
	}
	if got := rc.Jobs["ci_parity"].Uses; got != "./.github/workflows/ci.yml" {
		t.Errorf("rc-gate ci_parity uses %q, want ./.github/workflows/ci.yml", got)
	}
	var callers []string
	for id, job := range rc.Jobs {
		if job.Uses == "./.github/workflows/"+advisoryProbesWorkflowFile {
			callers = append(callers, id)
		}
	}
	if !slices.Equal(callers, []string{"probes_parity"}) {
		t.Fatalf("rc-gate jobs calling %s = %v, want [probes_parity]", advisoryProbesWorkflowFile, callers)
	}
	probes := rc.Jobs["probes_parity"]
	if !reflect.DeepEqual(probes.With, map[string]any{"force_blacksmith": true}) {
		t.Errorf("rc-gate probes_parity with %v, want force_blacksmith: true like ci_parity", probes.With)
	}
	if !reflect.DeepEqual(probes.Permissions, map[string]string{"contents": "read"}) {
		t.Errorf("rc-gate probes_parity permissions %v, want contents: read", probes.Permissions)
	}
	if !slices.Contains(rc.Jobs["rc_summary"].Needs, "probes_parity") {
		t.Errorf("rc-gate rc_summary needs %v, want probes_parity", rc.Jobs["rc_summary"].Needs)
	}
}
