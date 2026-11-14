package cipolicy

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// watchdogJobID is the single job in the CI verdict watchdog workflow. The
// workflow is privileged (checks: write on a trigger reachable from arbitrary
// pushes), so its trigger, permissions, gate, and emitted conclusion are
// pinned here rather than left to review vigilance.
//
// The job id mirrors the spelling GitHub itself uses for the run conclusion, so
// the US-locale misspell linter must not rewrite this literal out of sync with
// the YAML.
const watchdogJobID = "flag-cancelled-while-queued" //nolint:misspell // must match the workflow job id verbatim

// watchdogExpression matches a GitHub Actions template expression.
var watchdogExpression = regexp.MustCompile(`\$\{\{[^}]*\}\}`)

func watchdogPath() string {
	return filepath.Join("..", "..", ".github", "workflows", "ci-verdict-watchdog.yml")
}

func loadWatchdogWorkflow(t *testing.T) map[string]any {
	t.Helper()
	return readYAMLMap(t, watchdogPath())
}

func watchdogSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(watchdogPath())
	if err != nil {
		t.Fatalf("read %s: %v", watchdogPath(), err)
	}
	return string(data)
}

func watchdogStepRun(t *testing.T, workflow map[string]any) string {
	t.Helper()
	body, ok := step(t, job(t, workflow, watchdogJobID), 0)["run"].(string)
	if !ok {
		t.Fatal("watchdog step run body is not a string")
	}
	return body
}

func TestWatchdogTriggersOnlyOnCompletedCIRuns(t *testing.T) {
	triggers := triggerMap(t, loadWatchdogWorkflow(t))
	want := map[string]any{
		"workflow_run": map[string]any{
			"workflows": []any{"CI"},
			"types":     []any{"completed"},
		},
	}
	if !reflect.DeepEqual(triggers, want) {
		t.Fatalf(
			"watchdog triggers = %#v, want only completed workflow_run on CI: %#v",
			triggers,
			want,
		)
	}
}

// TestWatchdogPermissionsAreMinimal pins the token scope at both levels.
// checks: write is held by the one job only, never by the workflow (#5413
// scoped it down from the workflow level), so every other job added later
// starts read-only.
func TestWatchdogPermissionsAreMinimal(t *testing.T) {
	workflow := loadWatchdogWorkflow(t)
	cases := []struct {
		level       string
		permissions any
		want        map[string]any
	}{
		{"workflow", workflow["permissions"], map[string]any{"actions": "read"}},
		{
			"job " + watchdogJobID,
			job(t, workflow, watchdogJobID)["permissions"],
			map[string]any{"checks": "write", "actions": "read"},
		},
	}
	for _, tc := range cases {
		got, ok := tc.permissions.(map[string]any)
		if !ok {
			t.Fatalf("watchdog %s permissions are not a mapping", tc.level)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("watchdog %s permissions = %#v, want exactly %#v", tc.level, got, tc.want)
		}
	}
}

func TestWatchdogGateRequiresCancelledPushRun(t *testing.T) {
	gate, ok := job(t, loadWatchdogWorkflow(t), watchdogJobID)["if"].(string)
	if !ok {
		t.Fatal("watchdog job gate is not a string")
	}
	for _, condition := range []string{
		"github.event.workflow_run.conclusion == 'cancelled'",
		"github.event.workflow_run.event == 'push'",
	} {
		if !strings.Contains(gate, condition) {
			t.Fatalf("watchdog gate %q must require %q", gate, condition)
		}
	}
	if !strings.Contains(gate, "&&") {
		t.Fatalf("watchdog gate %q must require both conditions together", gate)
	}
}

func TestWatchdogPostsNeutralNonVerdictCheck(t *testing.T) {
	body := watchdogStepRun(t, loadWatchdogWorkflow(t))
	for _, required := range []string{
		`/check-runs`,
		`head_sha="${HEAD_SHA}"`,
		`conclusion="neutral"`,
		`${RUN_URL}`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("watchdog check-run POST must contain %q:\n%s", required, body)
		}
	}
	if strings.Contains(body, `conclusion="success"`) {
		t.Fatalf("watchdog must never claim a passing verdict for a canceled run:\n%s", body)
	}
}

func TestWatchdogBindsEventValuesThroughEnv(t *testing.T) {
	workflow := loadWatchdogWorkflow(t)
	env, ok := step(t, job(t, workflow, watchdogJobID), 0)["env"].(map[string]any)
	if !ok {
		t.Fatal("watchdog step env is not a mapping")
	}
	bound := make(map[string]bool, len(env))
	for _, value := range env {
		if text, ok := value.(string); ok {
			bound[strings.TrimSpace(text)] = true
		}
	}

	for _, expression := range watchdogExpression.FindAllString(watchdogSource(t), -1) {
		if !strings.Contains(expression, "github.event") {
			continue
		}
		if !bound[expression] {
			t.Fatalf("event expression %s must be bound through step env", expression)
		}
	}

	if found := watchdogExpression.FindString(watchdogStepRun(t, workflow)); found != "" {
		t.Fatalf(
			"watchdog run body must not interpolate %s; template expressions belong in env to keep attacker-influenced text out of the shell",
			found,
		)
	}
}
