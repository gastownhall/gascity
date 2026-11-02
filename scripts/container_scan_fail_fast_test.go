package scripts_test

import (
	"regexp"
	"strings"
	"testing"
)

// TestContainerScanImagePolicyDoesNotFailFast guards the "Enforce image
// vulnerability policy" step in .github/workflows/container-scan.yml against
// fail-fast behavior. Under "set -e", a bare `trivy image ...` call inside the
// per-image loop aborts the whole step at the first image with an unwaived
// finding, so later images in the array are never scanned and their findings
// are never reported. The step must scan every image, log a pass/fail line
// per image, and still fail the job overall if any image has an unwaived
// HIGH/CRITICAL finding.
func TestContainerScanImagePolicyDoesNotFailFast(t *testing.T) {
	root := repoRoot(t)
	workflow := readFile(t, root, ".github/workflows/container-scan.yml")

	step := extractEnforceImagePolicyStep(t, workflow)

	for _, image := range []string{
		`"gc-agent-base:${IMAGE_TAG}"`,
		`"gc-agent:${IMAGE_TAG}"`,
		`"gc-controller:${IMAGE_TAG}"`,
		`"gc-mcp-mail:${IMAGE_TAG}"`,
	} {
		if !strings.Contains(step, image) {
			t.Errorf("Enforce image vulnerability policy step missing image %s", image)
		}
	}

	if !strings.Contains(step, "if trivy image \\") {
		t.Error(`Enforce image vulnerability policy step must guard the trivy image call with "if" so a per-image failure does not abort the loop before later images are scanned`)
	}

	if !strings.Contains(step, "PASS:") {
		t.Error(`Enforce image vulnerability policy step must log a PASS line for each image with no unwaived findings`)
	}
	if !strings.Contains(step, "FAIL:") {
		t.Error(`Enforce image vulnerability policy step must log a FAIL line for each image with unwaived findings`)
	}

	if !strings.Contains(step, "overall_rc=1") {
		t.Error(`Enforce image vulnerability policy step must track a non-zero aggregate result when any image fails`)
	}
	if !regexp.MustCompile(`exit\s+"?\$overall_rc"?\s*$`).MatchString(strings.TrimRight(step, "\n")) {
		t.Error(`Enforce image vulnerability policy step must end by exiting with the aggregate result code, not silently succeed while an image failed`)
	}

	for _, want := range []string{
		"--severity HIGH,CRITICAL",
		"--ignore-unfixed",
		"--ignorefile .trivyignore.yaml",
	} {
		if !strings.Contains(step, want) {
			t.Errorf("Enforce image vulnerability policy step missing unchanged flag %q", want)
		}
	}
}

// extractEnforceImagePolicyStep returns the raw YAML text of the "Enforce
// image vulnerability policy" step, from its "- name:" line up to (but not
// including) the next step's "- name:" line.
func extractEnforceImagePolicyStep(t *testing.T, workflow string) string {
	t.Helper()
	start := strings.Index(workflow, "- name: Enforce image vulnerability policy")
	if start < 0 {
		t.Fatal(`container-scan.yml missing the "Enforce image vulnerability policy" step`)
	}
	rest := workflow[start:]
	end := strings.Index(rest[1:], "\n      - name:")
	if end < 0 {
		return rest
	}
	return rest[:end+1]
}
