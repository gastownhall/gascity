package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
)

func TestProductMetricsDirectChildEnvSessionSubmitPoller(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "child.env")
	spy := writeChildEnvSpy(t, dir)
	t.Setenv("GC_TEST_PRODUCT_METRICS_CHILD_ENV_SPY", snapshot)
	t.Setenv(execenv.UsageMetricsDisableEnv, "0")
	t.Setenv("BD_DISABLE_METRICS", "keep-beads-setting")
	t.Setenv("OTEL_SERVICE_NAME", "keep-otel-setting")

	previous := sessionSubmitPollerExecutable
	sessionSubmitPollerExecutable = func() (string, error) { return spy, nil }
	t.Cleanup(func() { sessionSubmitPollerExecutable = previous })

	if err := ensureSessionSubmitPoller(dir, "worker", "session-worker"); err != nil {
		t.Fatalf("ensureSessionSubmitPoller: %v", err)
	}
	deadline := time.Now().Add(execHangBudget)
	var data []byte
	for {
		var err error
		data, err = os.ReadFile(snapshot)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read child environment snapshot: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("child environment snapshot was not written within %s", execHangBudget)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Windows line endings come from the .cmd spy's echo; normalizing keeps
	// the recorded values comparable without relaxing what is compared.
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	got := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	want := []string{execenv.UsageMetricsDisableValue, "keep-beads-setting", "keep-otel-setting"}
	if !slices.Equal(got, want) {
		t.Fatalf("session submit poller environment = %#v, want %#v", got, want)
	}
}
