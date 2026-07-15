package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdGCIntegrationShardRunsOnlyIntegrationManifest(t *testing.T) {
	fixture := newIntegrationShardFixture(t, nil)

	out, err := fixture.run(t)
	if err != nil {
		t.Fatalf("test-integration-shard failed: %v\n%s", err, out)
	}

	captured, err := os.ReadFile(fixture.capturePath)
	if err != nil {
		t.Fatalf("read captured go invocation: %v", err)
	}
	invocation := string(captured)
	for _, testName := range []string{
		"TestCapstoneIntegrationRealMinter",
		"TestControllerDiscoversAddedCronOrderWithoutRestart",
		"TestManagedBdRigProviderStoreRecoversAfterHardKillPortRebind",
		"TestPhase2HookEnabledClaudeLaunchPromptDeliveryProof",
		"TestPhase2WorkerCoreRealTransportProof",
	} {
		if !strings.Contains(invocation, testName) {
			t.Errorf("final go test invocation missing %s:\n%s", testName, invocation)
		}
	}
	if strings.Contains(invocation, "TestOrdinaryUnit") {
		t.Fatalf("final go test invocation includes ordinary unit test:\n%s", invocation)
	}
}

func TestCmdGCIntegrationShardRejectsUnassignedTaggedTest(t *testing.T) {
	fixture := newIntegrationShardFixture(t, []string{"TestNewIntegrationProof"})

	out, err := fixture.run(t)
	if err == nil {
		t.Fatalf("test-integration-shard succeeded with an unassigned tagged test:\n%s", out)
	}
	if !strings.Contains(string(out), "unassigned cmd/gc integration test: TestNewIntegrationProof") {
		t.Fatalf("failure does not identify manifest drift:\n%s", out)
	}
}

type integrationShardFixture struct {
	binDir      string
	homeDir     string
	capturePath string
}

func newIntegrationShardFixture(t *testing.T, extraTaggedTests []string) integrationShardFixture {
	t.Helper()

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	capturePath := filepath.Join(tmp, "go-test.capture")
	taggedTests := append([]string{
		"TestOrdinaryUnit",
		"TestCapstoneIntegrationRealMinter",
		"TestControllerDiscoversAddedCronOrderWithoutRestart",
		"TestManagedBdRigProviderStoreRecoversAfterHardKillPortRebind",
		"TestPhase2HookEnabledClaudeLaunchPromptDeliveryProof",
		"TestPhase2WorkerCoreRealTransportProof",
	}, extraTaggedTests...)
	var taggedOutput strings.Builder
	for _, testName := range taggedTests {
		taggedOutput.WriteString("          echo ")
		taggedOutput.WriteString(shellQuote(testName))
		taggedOutput.WriteString("\n")
	}

	writeExecutable(t, filepath.Join(binDir, "go"), `#!/usr/bin/env bash
set -euo pipefail

capture_path=`+shellQuote(capturePath)+`

case "$1" in
  env)
    case "$2" in
      GOPATH) echo /tmp/fake-gopath ;;
      GOCACHE) echo /tmp/fake-gocache ;;
      GOMODCACHE) echo /tmp/fake-gomodcache ;;
      GOTMPDIR) echo "" ;;
      GOROOT) echo /tmp/fake-goroot ;;
      *) echo "unexpected go env key: $2" >&2; exit 1 ;;
    esac
    ;;
  test)
    is_list=0
    is_integration=0
    previous=""
    for arg in "$@"; do
      [[ "$arg" == "-list" ]] && is_list=1
      [[ "$previous" == "-tags" && "$arg" == "integration" ]] && is_integration=1
      previous="$arg"
    done
    if [[ "$is_list" == 1 ]]; then
      if [[ "$is_integration" == 1 ]]; then
`+taggedOutput.String()+`      else
        echo TestOrdinaryUnit
      fi
      exit 0
    fi
    printf '%s\n' "$*" > "$capture_path"
    ;;
  *)
    echo "unexpected go command: $*" >&2
    exit 1
    ;;
esac
`)

	return integrationShardFixture{
		binDir:      binDir,
		homeDir:     filepath.Join(tmp, "home"),
		capturePath: capturePath,
	}
}

func (f integrationShardFixture) run(t *testing.T) ([]byte, error) {
	t.Helper()
	repo := repoRoot(t)
	cmd := exec.Command(
		filepath.Join(repo, "scripts", "test-integration-shard"),
		"packages-cmd-gc-integration",
	)
	cmd.Dir = repo
	cmd.Env = []string{
		"PATH=" + f.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + f.homeDir,
		"GC_TEST_NO_SLICE=1",
		"SYS_USR_CGO_FALLBACK=0",
	}
	return cmd.CombinedOutput()
}
