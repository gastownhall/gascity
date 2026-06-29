package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoltVersionPins(t *testing.T) {
	const doltPin = "2.1.7"
	const doltFloor = "2.1.0"
	repoRoot := repoRoot(t)

	assertContains := func(rel, want string) {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(content), want) {
			t.Fatalf("%s missing %q", rel, want)
		}
	}
	assertCount := func(rel, want string, count int) {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if got := strings.Count(string(content), want); got != count {
			t.Fatalf("%s has %d copies of %q, want %d", rel, got, want, count)
		}
	}

	assertContains("deps.env", "DOLT_VERSION="+doltPin)
	assertContains("contrib/k8s/Dockerfile.base", "ARG DOLT_VERSION="+doltPin)
	assertCount("contrib/k8s/dolt-statefulset.yaml", "image: dolthub/dolt:"+doltPin, 2)
	assertContains("README.md", "| dolt | Beads provider `bd` | "+doltFloor+" or newer")
	assertContains("README.md", "Managed Dolt checks require a final Dolt "+doltFloor+" or newer.")
	assertContains("examples/bd/dolt/pack.toml", "# Minimum dolt version: "+doltFloor+".")
	assertContains("examples/bd/dolt/doctor/check-dolt/run.sh", `required="`+doltFloor+`"`)
	assertContains("examples/bd/dolt/assets/scripts/mol-dog-backup.sh", `MIN_DOLT_BACKUP_VERSION="`+doltFloor+`"`)

	for _, platform := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		assertContains(".github/scripts/install-dolt-archive.sh", doltPin+":"+platform)
	}

	// Validate every DOLT_VERSION assignment in both .yml and .yaml workflows,
	// using the same shared scanner as the bd pin guard so neither analog can
	// false-pass on partial drift or a .yaml workflow.
	assertWorkflowPins(t, repoRoot, "DOLT_VERSION", doltPin)
}
