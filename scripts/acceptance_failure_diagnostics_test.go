package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A managed Dolt server that drops mid-test leaves its only evidence in logs
// under the test's temp dir, which t.Cleanup removes. Failing acceptance tests
// copy those logs to GC_TEST_FAILURE_ARTIFACT_DIR first
// (internal/testutil.SaveFailureDiagnostics), so every job that runs the
// acceptance suite against real bd and dolt must set that directory before
// its tests and upload it on failure.

const (
	acceptanceRealToolingEnv   = "GC_REQUIRE_ACCEPTANCE_TOOLING"
	acceptanceFailureDirEnv    = "GC_TEST_FAILURE_ARTIFACT_DIR"
	acceptanceFailureDirValue  = "${RUNNER_TEMP}/failure-artifacts"
	acceptanceFailureDirUpload = "${{ runner.temp }}/failure-artifacts"
	uploadArtifactAction       = "actions/upload-artifact@"
)

type failureDiagnosticsWorkflow struct {
	Jobs map[string]struct {
		Env   map[string]string `yaml:"env"`
		Steps []struct {
			Name string            `yaml:"name"`
			If   string            `yaml:"if"`
			Uses string            `yaml:"uses"`
			Run  string            `yaml:"run"`
			With map[string]string `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func TestRealToolingAcceptanceJobsUploadFailureDiagnostics(t *testing.T) {
	root := repoRoot(t)
	checked := 0
	for _, workflow := range []string{"ci.yml", "nightly.yml"} {
		body, err := os.ReadFile(filepath.Join(root, ".github", "workflows", workflow)) //nolint:gosec // a fixed path inside the repo
		if err != nil {
			t.Fatalf("read %s: %v", workflow, err)
		}
		var doc failureDiagnosticsWorkflow
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("parse %s: %v", workflow, err)
		}
		for name, job := range doc.Jobs {
			if job.Env[acceptanceRealToolingEnv] == "" {
				continue
			}
			route, firstTest, lastTest, upload := -1, -1, -1, -1
			for i, step := range job.Steps {
				switch {
				case strings.Contains(step.Run, acceptanceFailureDirEnv+"="+acceptanceFailureDirValue) &&
					strings.Contains(step.Run, "$GITHUB_ENV"):
					route = i
				case strings.Contains(step.Run, "go test") && strings.Contains(step.Run, "./test/acceptance"):
					if firstTest < 0 {
						firstTest = i
					}
					lastTest = i
				case strings.HasPrefix(step.Uses, uploadArtifactAction) &&
					step.With["path"] == acceptanceFailureDirUpload &&
					strings.Contains(step.If, "failure()"):
					upload = i
				}
			}
			if firstTest < 0 {
				continue
			}
			checked++
			where := workflow + ":" + name
			if route < 0 || route > firstTest {
				t.Errorf("%s runs real-tooling acceptance tests without first exporting %s=%s to $GITHUB_ENV",
					where, acceptanceFailureDirEnv, acceptanceFailureDirValue)
			}
			if upload < lastTest {
				t.Errorf("%s needs an `if: failure()` %s step uploading %s after its last acceptance test",
					where, uploadArtifactAction, acceptanceFailureDirUpload)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("no job sets %s and runs ./test/acceptance; the scan is broken", acceptanceRealToolingEnv)
	}
}
