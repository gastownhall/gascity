package workflows

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	macDispatchWorkflowPath = ".github/workflows/dispatch-labeled-pr-suite.yml"
	macTrustScriptPath      = ".github/workflows/scripts/pr_trust_check.py"
)

var macSensitivePathFamilies = []string{
	"internal/pathutil/**",
	"internal/fsys/**",
	"internal/testutil/path.go",
	"cmd/gc/path_*.go",
	"cmd/gc/*_path*.go",
	"cmd/gc/city_discovery*.go",
	"cmd/gc/*worktree*.go",
	"cmd/gc/cmd_supervisor_city*.go",
	".github/actions/setup-gascity-macos/**",
	"**/*_darwin.go",
	"**/*_darwin_test.go",
}

type macPolicyWorkflow struct {
	On          any                     `yaml:"on"`
	Permissions any                     `yaml:"permissions"`
	Jobs        map[string]macPolicyJob `yaml:"jobs"`
}

type macPolicyJob struct {
	If          string                  `yaml:"if"`
	Permissions any                     `yaml:"permissions"`
	Concurrency any                     `yaml:"concurrency"`
	Steps       []macPolicyWorkflowStep `yaml:"steps"`
}

type macPolicyWorkflowStep struct {
	Name string         `yaml:"name"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	Env  map[string]any `yaml:"env"`
	With map[string]any `yaml:"with"`
}

func TestMacSensitiveAutoLabelWorkflowContract(t *testing.T) {
	workflow := readMacPolicyWorkflow(t)

	on, ok := workflow.On.(map[string]any)
	if !ok {
		t.Fatalf("%s: on = %#v, want a pull_request_target event map", macDispatchWorkflowPath, workflow.On)
	}
	if got, want := sortedMapKeys(on), []string{"pull_request_target"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: workflow events = %v, want %v", macDispatchWorkflowPath, got, want)
	}
	pullRequestTarget, ok := on["pull_request_target"].(map[string]any)
	if !ok {
		t.Fatalf("%s: pull_request_target = %#v, want an event configuration", macDispatchWorkflowPath, on["pull_request_target"])
	}
	wantActions := []string{"labeled", "opened", "ready_for_review", "reopened", "synchronize"}
	if got := sortedStringValues(t, pullRequestTarget["types"]); !reflect.DeepEqual(got, wantActions) {
		t.Fatalf("%s: pull_request_target actions = %v, want %v", macDispatchWorkflowPath, got, wantActions)
	}

	if got, want := sortedMacPolicyMapKeys(workflow.Jobs), []string{"auto-label-path-sensitive", "dispatch-suite"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: jobs = %v, want exactly %v", macDispatchWorkflowPath, got, want)
	}

	dispatchJob := requireMacPolicyJob(t, workflow, "dispatch-suite")
	autoJob := requireMacPolicyJob(t, workflow, "auto-label-path-sensitive")

	dispatchIf := normalizeWorkflowExpression(dispatchJob.If)
	for _, required := range []string{
		"github.event.action=='labeled'",
		"github.event.label.name=='needs-mac'",
		"github.event.label.name=='needs-review-formulas'",
	} {
		if !strings.Contains(dispatchIf, required) {
			t.Errorf("dispatch-suite if = %q, want %q", dispatchJob.If, required)
		}
	}

	autoIf := normalizeWorkflowExpression(autoJob.If)
	if !strings.Contains(autoIf, "github.event.action!='labeled'") {
		t.Errorf("auto-label-path-sensitive if = %q, want non-labeled event guard", autoJob.If)
	}
	for _, earlyFilter := range []string{"pull_request.draft", "pull_request.head.repo"} {
		if strings.Contains(autoIf, earlyFilter) {
			t.Errorf("auto-label-path-sensitive if = %q filters %q before the job can log its decision", autoJob.If, earlyFilter)
		}
	}

	assertMacPolicyPermissions(t, "dispatch-suite", dispatchJob.Permissions, map[string]string{
		"actions":       "write",
		"contents":      "read",
		"pull-requests": "read",
	})
	assertMacPolicyPermissions(t, "auto-label-path-sensitive", autoJob.Permissions, map[string]string{
		"actions":       "write",
		"contents":      "read",
		"pull-requests": "write",
	})

	assertMacPolicyConcurrency(t, autoJob.Concurrency)
	assertTrustedBaseCheckout(t, "dispatch-suite", dispatchJob)
	assertTrustedBaseCheckout(t, "auto-label-path-sensitive", autoJob)

	workflowSource := macPolicyWorkflowSource(workflow)
	if strings.Contains(strings.ToLower(workflowSource), "secrets.") {
		t.Errorf("%s introduces a secret-backed credential; want github.token only", macDispatchWorkflowPath)
	}
	if !strings.Contains(workflowSource, "${{ github.token }}") {
		t.Errorf("%s does not explicitly source its token from github.token", macDispatchWorkflowPath)
	}
}

func TestMacSensitiveAutoLabelPinsNarrowPathPolicy(t *testing.T) {
	workflow := readMacPolicyWorkflow(t)
	source := macPolicyJobRunSource(requireMacPolicyJob(t, workflow, "auto-label-path-sensitive"))

	for _, family := range macSensitivePathFamilies {
		if !strings.Contains(source, family) {
			t.Errorf("auto-label-path-sensitive policy is missing family %q", family)
		}
	}

	for _, forbidden := range []string{
		"cmd/gc/**",
		"cmd/gc/*.go",
		"cmd/gc/*reaper*.go",
		".github/workflows/**",
		"**/*.go",
	} {
		if strings.Contains(source, forbidden) {
			t.Errorf("auto-label-path-sensitive policy contains overbroad family %q", forbidden)
		}
	}

	positiveFixtures := map[string]string{
		"original escaped regression":   "cmd/gc/bead_worktree_reaper.go",
		"pathutil whole package":        "internal/pathutil/testdata/nested/path.go",
		"fsys whole package":            "internal/fsys/fs.go",
		"single path helper":            "internal/testutil/path.go",
		"path prefix":                   "cmd/gc/path_resolve.go",
		"path infix":                    "cmd/gc/city_path_test.go",
		"city discovery":                "cmd/gc/city_discovery_test.go",
		"worktree family":               "cmd/gc/worktree_create.go",
		"supervisor city family":        "cmd/gc/cmd_supervisor_city_start.go",
		"macOS setup action":            ".github/actions/setup-gascity-macos/action.yml",
		"Darwin implementation":         "internal/runtime/tmux/process_darwin.go",
		"Darwin platform-specific test": "internal/runtime/tmux/process_darwin_test.go",
	}
	negativeFixtures := map[string]string{
		"unrelated cmd gc":             "cmd/gc/cmd_mail.go",
		"api state":                    "cmd/gc/api_state_cache.go",
		"generic reaper":               "cmd/gc/session_reaper.go",
		"docs only":                    "docs/getting-started/index.md",
		"ordinary workflow":            ".github/workflows/ci.yml",
		"package prefix near miss":     "internal/pathutility/path.go",
		"testutil near miss":           "internal/testutil/path_test.go",
		"non-Go path prefix near miss": "cmd/gc/path_notes.md",
	}
	for name, path := range positiveFixtures {
		if !macSensitiveFixtureMatches(path) {
			t.Errorf("positive fixture %q (%s) is not covered by the pinned policy", name, path)
		}
	}
	for name, path := range negativeFixtures {
		if macSensitiveFixtureMatches(path) {
			t.Errorf("negative fixture %q (%s) is covered by the pinned policy", name, path)
		}
	}
}

func TestMacSensitiveAutoLabelGuaranteesFirstMatchingRevisionDispatch(t *testing.T) {
	workflow := readMacPolicyWorkflow(t)
	autoJob := requireMacPolicyJob(t, workflow, "auto-label-path-sensitive")
	source := macPolicyJobSource(autoJob)

	requiredFragments := []string{
		"gh api",
		"/pulls/",
		"/files",
		"--paginate",
		".filename",
		"gh pr edit",
		"--add-label",
		"needs-mac",
		"pr_trust_check.py",
		"gh run list",
		"mac-regression.yml",
		"queued",
		"in_progress",
		"pr_number",
		"head_repo",
		"head_ref",
		"head_sha",
		"base_ref",
		"suite",
	}
	for _, fragment := range requiredFragments {
		if !strings.Contains(strings.ToLower(source), strings.ToLower(fragment)) {
			t.Errorf("auto-label-path-sensitive does not contain required dispatch fragment %q", fragment)
		}
	}
	if !containsAny(source, "gh workflow run", "/actions/workflows/", "actions/workflows/") {
		t.Errorf("auto-label-path-sensitive does not dispatch mac-regression.yml through workflow_dispatch")
	}
	for _, forbidden := range []string{"run_smoke", "run_full", "ruleset"} {
		if strings.Contains(strings.ToLower(source), forbidden) {
			t.Errorf("auto-label-path-sensitive bypasses the Mac workflow gate via %q", forbidden)
		}
	}

	for _, decision := range []string{"matched", "already-labeled", "unmatched", "draft", "fork"} {
		if !containsDecision(source, decision) {
			t.Errorf("auto-label-path-sensitive does not emit an observable %q decision", decision)
		}
	}
	if !containsAny(source, "untrusted", "not trusted") {
		t.Errorf("auto-label-path-sensitive does not emit an observable untrusted-author decision")
	}
	for _, family := range macSensitivePathFamilies {
		if strings.Count(source, family) < 2 {
			t.Errorf("policy family %q is not both matched and reported observably", family)
		}
	}

	runSource := macPolicyJobRunSource(autoJob)
	sameRepoGuard := sourceLineOffsetContainingAll(runSource, "if", "head_repo", "repository")
	fileList := strings.Index(runSource, "gh api")
	labelAdd := strings.Index(runSource, "gh pr edit")
	draftGuard := sourceLineOffsetContainingAll(runSource, "if", "draft")
	trustCheck := strings.Index(runSource, macTrustScriptPath)
	runDedup := strings.Index(runSource, "gh run list")
	dispatch := firstSourceOffset(runSource, "gh workflow run", "/actions/workflows/")
	assertSourceOrder(t, runSource,
		sourceMilestone{name: "same-repository guard", offset: sameRepoGuard},
		sourceMilestone{name: "current PR file listing", offset: fileList},
		sourceMilestone{name: "add-only label", offset: labelAdd},
		sourceMilestone{name: "draft stop", offset: draftGuard},
		sourceMilestone{name: "shared trust check", offset: trustCheck},
		sourceMilestone{name: "head-SHA run deduplication", offset: runDedup},
		sourceMilestone{name: "Mac workflow dispatch", offset: dispatch},
	)
}

func TestMacSensitiveAutoLabelIsAddOnlyAndPreservesManualFallback(t *testing.T) {
	workflow := readMacPolicyWorkflow(t)
	autoJob := requireMacPolicyJob(t, workflow, "auto-label-path-sensitive")
	source := strings.ToLower(macPolicyJobSource(autoJob))

	for _, forbidden := range []string{"--remove-label", "removelabel", "remove_label"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("auto-label-path-sensitive contains removal path %q; needs-mac must be add-only", forbidden)
		}
	}
	for _, required := range []string{
		"head_repo",
		"github.repository",
		"fork",
		"draft",
		"already-labeled",
		"gh pr edit",
		"--add-label",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("auto-label-path-sensitive is missing %q lifecycle handling", required)
		}
	}
}

func TestMacSensitiveAutoLabelUsesOneSharedTrustBoundary(t *testing.T) {
	root := repoRoot(t)
	workflow := readMacPolicyWorkflow(t)
	dispatchSource := macPolicyJobSource(requireMacPolicyJob(t, workflow, "dispatch-suite"))
	autoSource := macPolicyJobSource(requireMacPolicyJob(t, workflow, "auto-label-path-sensitive"))

	for jobName, source := range map[string]string{
		"dispatch-suite":            dispatchSource,
		"auto-label-path-sensitive": autoSource,
	} {
		if count := strings.Count(source, macTrustScriptPath); count != 1 {
			t.Errorf("%s invokes %s %d times, want exactly once", jobName, macTrustScriptPath, count)
		}
	}

	workflowSource := macPolicyWorkflowSource(workflow)
	for _, duplicatedTrustLogic := range []string{"OWNER", "MEMBER", "COLLABORATOR", "blacksmith-allowlist.txt"} {
		if strings.Contains(workflowSource, duplicatedTrustLogic) {
			t.Errorf("%s still embeds trust logic %q instead of using %s", macDispatchWorkflowPath, duplicatedTrustLogic, macTrustScriptPath)
		}
	}

	trustPath := filepath.Join(root, filepath.FromSlash(macTrustScriptPath))
	body, err := os.ReadFile(trustPath)
	if err != nil {
		t.Fatalf("reading shared PR trust check %s: %v", macTrustScriptPath, err)
	}
	trustSource := string(body)
	for _, required := range []string{
		"OWNER",
		"MEMBER",
		"COLLABORATOR",
		"blacksmith-allowlist.txt",
		"author",
		"association",
		"lower",
		"trusted",
	} {
		if !strings.Contains(trustSource, required) {
			t.Errorf("%s does not contain required trust behavior %q", macTrustScriptPath, required)
		}
	}
}

func readMacPolicyWorkflow(t *testing.T) macPolicyWorkflow {
	t.Helper()

	path := filepath.Join(repoRoot(t), filepath.FromSlash(macDispatchWorkflowPath))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", macDispatchWorkflowPath, err)
	}
	var workflow macPolicyWorkflow
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatalf("parsing %s: %v", macDispatchWorkflowPath, err)
	}
	return workflow
}

func requireMacPolicyJob(t *testing.T, workflow macPolicyWorkflow, name string) macPolicyJob {
	t.Helper()

	job, ok := workflow.Jobs[name]
	if !ok {
		t.Fatalf("%s has no %q job", macDispatchWorkflowPath, name)
	}
	return job
}

func assertMacPolicyPermissions(t *testing.T, jobName string, raw any, want map[string]string) {
	t.Helper()

	got := permissionMap(t, macDispatchWorkflowPath+" job "+jobName, raw)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s job %q permissions = %v, want only %v", macDispatchWorkflowPath, jobName, got, want)
	}
}

func assertMacPolicyConcurrency(t *testing.T, raw any) {
	t.Helper()

	concurrency, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("auto-label-path-sensitive concurrency = %#v, want a job-level map", raw)
	}
	group := normalizeWorkflowExpression(fmt.Sprint(concurrency["group"]))
	if group != normalizeWorkflowExpression("auto-label-path-sensitive-${{ github.event.pull_request.number }}") {
		t.Errorf("auto-label-path-sensitive concurrency group = %q, want PR-scoped group", concurrency["group"])
	}
	cancel, ok := concurrency["cancel-in-progress"].(bool)
	if !ok || !cancel {
		t.Errorf("auto-label-path-sensitive cancel-in-progress = %#v, want true", concurrency["cancel-in-progress"])
	}
}

func assertTrustedBaseCheckout(t *testing.T, jobName string, job macPolicyJob) {
	t.Helper()

	var checkouts []macPolicyWorkflowStep
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			checkouts = append(checkouts, step)
		}
	}
	if len(checkouts) != 1 {
		t.Fatalf("%s job %q checkout steps = %d, want 1", macDispatchWorkflowPath, jobName, len(checkouts))
	}
	checkout := checkouts[0]
	if got := normalizeWorkflowExpression(fmt.Sprint(checkout.With["ref"])); got != "github.event.pull_request.base.sha" {
		t.Errorf("%s job %q checkout ref = %q, want pull request base SHA", macDispatchWorkflowPath, jobName, checkout.With["ref"])
	}
	if got := fmt.Sprint(checkout.With["persist-credentials"]); got != "false" {
		t.Errorf("%s job %q checkout persist-credentials = %q, want false", macDispatchWorkflowPath, jobName, got)
	}
}

func macPolicyJobRunSource(job macPolicyJob) string {
	var source strings.Builder
	for _, step := range job.Steps {
		source.WriteString(step.Run)
		source.WriteByte('\n')
	}
	return source.String()
}

func macPolicyJobSource(job macPolicyJob) string {
	var source strings.Builder
	source.WriteString(job.If)
	source.WriteByte('\n')
	for _, step := range job.Steps {
		source.WriteString(step.Name)
		source.WriteByte('\n')
		source.WriteString(step.Uses)
		source.WriteByte('\n')
		source.WriteString(step.Run)
		source.WriteByte('\n')
		for name, value := range step.Env {
			fmt.Fprintf(&source, "%s=%v\n", name, value)
		}
		for name, value := range step.With {
			fmt.Fprintf(&source, "%s=%v\n", name, value)
		}
	}
	return source.String()
}

func macPolicyWorkflowSource(workflow macPolicyWorkflow) string {
	var source strings.Builder
	jobNames := sortedMacPolicyMapKeys(workflow.Jobs)
	for _, jobName := range jobNames {
		source.WriteString(macPolicyJobSource(workflow.Jobs[jobName]))
	}
	return source.String()
}

func macSensitiveFixtureMatches(path string) bool {
	switch {
	case strings.HasPrefix(path, "internal/pathutil/"):
		return true
	case strings.HasPrefix(path, "internal/fsys/"):
		return true
	case path == "internal/testutil/path.go":
		return true
	case strings.HasPrefix(path, "cmd/gc/path_") && strings.HasSuffix(path, ".go"):
		return true
	case strings.HasPrefix(path, "cmd/gc/") &&
		strings.Contains(strings.TrimPrefix(path, "cmd/gc/"), "_path") &&
		strings.HasSuffix(path, ".go"):
		return true
	case strings.HasPrefix(path, "cmd/gc/city_discovery") && strings.HasSuffix(path, ".go"):
		return true
	case strings.HasPrefix(path, "cmd/gc/") &&
		strings.Contains(strings.TrimPrefix(path, "cmd/gc/"), "worktree") &&
		strings.HasSuffix(path, ".go"):
		return true
	case strings.HasPrefix(path, "cmd/gc/cmd_supervisor_city") && strings.HasSuffix(path, ".go"):
		return true
	case strings.HasPrefix(path, ".github/actions/setup-gascity-macos/"):
		return true
	case strings.HasSuffix(path, "_darwin.go"):
		return true
	case strings.HasSuffix(path, "_darwin_test.go"):
		return true
	default:
		return false
	}
}

func containsAny(source string, fragments ...string) bool {
	lowerSource := strings.ToLower(source)
	for _, fragment := range fragments {
		if strings.Contains(lowerSource, strings.ToLower(fragment)) {
			return true
		}
	}
	return false
}

func containsDecision(source, decision string) bool {
	pattern := regexp.MustCompile(`(?i)(^|[^a-z-])` + regexp.QuoteMeta(decision) + `([^a-z-]|$)`)
	return pattern.MatchString(source)
}

type sourceMilestone struct {
	name   string
	offset int
}

func assertSourceOrder(t *testing.T, source string, milestones ...sourceMilestone) {
	t.Helper()

	previous := -1
	for _, milestone := range milestones {
		if milestone.offset < 0 {
			t.Errorf("auto-label-path-sensitive source has no %s", milestone.name)
			continue
		}
		if previous >= 0 && milestone.offset <= previous {
			t.Errorf("auto-label-path-sensitive performs %s out of order\nsource:\n%s", milestone.name, source)
		}
		previous = milestone.offset
	}
}

func sourceLineOffsetContainingAll(source string, fragments ...string) int {
	offset := 0
	for _, line := range strings.SplitAfter(source, "\n") {
		lowerLine := strings.ToLower(line)
		matches := true
		for _, fragment := range fragments {
			if !strings.Contains(lowerLine, strings.ToLower(fragment)) {
				matches = false
				break
			}
		}
		if matches {
			return offset
		}
		offset += len(line)
	}
	return -1
}

func firstSourceOffset(source string, fragments ...string) int {
	first := -1
	for _, fragment := range fragments {
		if offset := strings.Index(source, fragment); offset >= 0 && (first < 0 || offset < first) {
			first = offset
		}
	}
	return first
}

func normalizeWorkflowExpression(value string) string {
	replacer := strings.NewReplacer(
		" ", "",
		"\n", "",
		"\t", "",
		"${{", "",
		"}}", "",
	)
	return strings.ToLower(replacer.Replace(value))
}

func sortedMapKeys(values map[string]any) []string {
	return sortedMacPolicyMapKeys(values)
}

func sortedStringValues(t *testing.T, value any) []string {
	t.Helper()

	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("workflow string list = %#v, want list", value)
	}
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("workflow string list item = %#v, want string", item)
		}
		values = append(values, text)
	}
	sort.Strings(values)
	return values
}

func permissionMap(t *testing.T, owner string, value any) map[string]string {
	t.Helper()

	if value == nil {
		return nil
	}
	raw, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s permissions = %#v, want a permission map", owner, value)
	}
	permissions := make(map[string]string, len(raw))
	for name, level := range raw {
		text, ok := level.(string)
		if !ok {
			t.Fatalf("%s permission %q = %#v, want string", owner, name, level)
		}
		permissions[name] = text
	}
	return permissions
}

func sortedMacPolicyMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
