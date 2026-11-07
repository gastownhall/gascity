package workflows

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const nightlyWorkflowPath = ".github/workflows/nightly.yml"

type nightlyAggregateWorkflow struct {
	Jobs map[string]nightlyAggregateJob `yaml:"jobs"`
}

type nightlyAggregateJob struct {
	Name  string                 `yaml:"name"`
	If    string                 `yaml:"if"`
	Needs nightlyAggregateNeeds  `yaml:"needs"`
	Steps []nightlyAggregateStep `yaml:"steps"`
}

type nightlyAggregateStep struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

type nightlyAggregateNeeds []string

func (n *nightlyAggregateNeeds) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case 0:
		*n = nil
		return nil
	case yaml.ScalarNode:
		*n = []string{node.Value}
		return nil
	case yaml.SequenceNode:
		var values []string
		if err := node.Decode(&values); err != nil {
			return err
		}
		*n = values
		return nil
	default:
		return fmt.Errorf("needs must be a string or sequence, got YAML kind %d", node.Kind)
	}
}

func TestNightlyHasOneAlwaysRunAggregateDependingOnEveryJob(t *testing.T) {
	workflow := readNightlyAggregateWorkflow(t)
	summaryKey, summary := requireNightlySummaryJob(t, workflow)

	normalizedIf := strings.ToLower(strings.Join(strings.Fields(summary.If), ""))
	if !strings.Contains(normalizedIf, "always()") {
		t.Fatalf("%s job %q if = %q, want always()", nightlyWorkflowPath, summaryKey, summary.If)
	}

	wantNeeds := make([]string, 0, len(workflow.Jobs)-1)
	for key := range workflow.Jobs {
		if key != summaryKey {
			wantNeeds = append(wantNeeds, key)
		}
	}
	sort.Strings(wantNeeds)
	gotNeeds := append([]string(nil), summary.Needs...)
	sort.Strings(gotNeeds)
	if !reflect.DeepEqual(gotNeeds, wantNeeds) {
		t.Fatalf("%s job %q needs = %v, want every other Nightly job %v",
			nightlyWorkflowPath, summaryKey, gotNeeds, wantNeeds)
	}
}

func TestNightlyAggregateFailsForEveryNonSuccessDependency(t *testing.T) {
	workflow := readNightlyAggregateWorkflow(t)
	summaryKey, summary := requireNightlySummaryJob(t, workflow)
	step := requireNightlySummaryRunStep(t, summaryKey, summary)
	dependencyEnv := requireNightlyDependencyEnvironment(t, summaryKey, summary.Needs, step.Env)

	allSuccess := make(map[string]string, len(dependencyEnv))
	for _, envName := range dependencyEnv {
		allSuccess[envName] = "success"
	}
	if output, err := runNightlySummaryScript(t, step.Run, allSuccess); err != nil {
		t.Fatalf("all-success Nightly summary failed: %v\n%s", err, output)
	}

	for _, dependency := range append([]string(nil), summary.Needs...) {
		for _, conclusion := range []string{"failure", "canceled", "skipped"} {
			t.Run(dependency+"/"+conclusion, func(t *testing.T) {
				env := make(map[string]string, len(allSuccess))
				for key, value := range allSuccess {
					env[key] = value
				}
				env[dependencyEnv[dependency]] = conclusion
				output, err := runNightlySummaryScript(t, step.Run, env)
				if err == nil {
					t.Fatalf("Nightly summary passed with required job %q = %q\n%s",
						dependency, conclusion, output)
				}
			})
		}
	}
}

func readNightlyAggregateWorkflow(t *testing.T) nightlyAggregateWorkflow {
	t.Helper()
	path := filepath.Join(repoRoot(t), nightlyWorkflowPath)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var workflow nightlyAggregateWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(workflow.Jobs) == 0 {
		t.Fatalf("%s has no jobs", path)
	}
	return workflow
}

func requireNightlySummaryJob(t *testing.T, workflow nightlyAggregateWorkflow) (string, nightlyAggregateJob) {
	t.Helper()
	var keys []string
	for key, job := range workflow.Jobs {
		if job.Name == "Nightly summary" {
			keys = append(keys, key)
		}
	}
	if len(keys) != 1 {
		t.Fatalf("%s has %d jobs named %q (%v), want exactly one",
			nightlyWorkflowPath, len(keys), "Nightly summary", keys)
	}
	return keys[0], workflow.Jobs[keys[0]]
}

func requireNightlySummaryRunStep(t *testing.T, summaryKey string, summary nightlyAggregateJob) nightlyAggregateStep {
	t.Helper()
	var runSteps []nightlyAggregateStep
	for _, step := range summary.Steps {
		if strings.TrimSpace(step.Run) == "" {
			continue
		}
		for _, expression := range step.Env {
			if strings.Contains(expression, "needs.") && strings.Contains(expression, ".result") {
				runSteps = append(runSteps, step)
				break
			}
		}
	}
	if len(runSteps) != 1 {
		t.Fatalf("%s job %q aggregate run steps = %d, want one script fed by needs.*.result",
			nightlyWorkflowPath, summaryKey, len(runSteps))
	}
	return runSteps[0]
}

func requireNightlyDependencyEnvironment(
	t *testing.T,
	summaryKey string,
	dependencies []string,
	env map[string]string,
) map[string]string {
	t.Helper()
	result := make(map[string]string, len(dependencies))
	for _, dependency := range dependencies {
		needle := "needs." + dependency + ".result"
		for envName, expression := range env {
			if strings.Contains(expression, needle) {
				if prior, exists := result[dependency]; exists {
					t.Fatalf("%s job %q maps %s through both %s and %s",
						nightlyWorkflowPath, summaryKey, dependency, prior, envName)
				}
				result[dependency] = envName
			}
		}
		if result[dependency] == "" {
			t.Fatalf("%s job %q does not expose needs.%s.result to its aggregate script",
				nightlyWorkflowPath, summaryKey, dependency)
		}
	}
	return result
}

func runNightlySummaryScript(t *testing.T, script string, values map[string]string) (string, error) {
	t.Helper()
	summaryPath := filepath.Join(t.TempDir(), "summary.md")
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Env = append(os.Environ(), "GITHUB_STEP_SUMMARY="+summaryPath)
	for key, value := range values {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	output, err := cmd.CombinedOutput()
	return string(output), err
}
