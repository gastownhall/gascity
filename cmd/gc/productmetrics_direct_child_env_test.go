package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/githubmonitor"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/testutil"
)

const productMetricsDirectChildEnvSpyPath = "GC_TEST_PRODUCT_METRICS_DIRECT_CHILD_ENV_SPY_PATH"

var productMetricsDirectChildObservedKeys = []string{
	execenv.UsageMetricsDisableEnv,
	"BD_DISABLE_METRICS",
	"OTEL_SERVICE_NAME",
	"PWD",
}

// maybeRunProductMetricsDirectChildEnvSpy turns a re-executed cmd/gc test
// binary into a minimal child-environment spy before the normal TestMain
// setup can rewrite its environment or dispatch a command.
func maybeRunProductMetricsDirectChildEnvSpy() {
	path := os.Getenv(productMetricsDirectChildEnvSpyPath)
	if path == "" {
		return
	}
	observed := make([]string, 0, len(productMetricsDirectChildObservedKeys))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && slices.Contains(productMetricsDirectChildObservedKeys, key) {
			observed = append(observed, entry)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(observed, "\n")+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "writing direct-child environment spy: %v\n", err) //nolint:errcheck
		os.Exit(97)
	}
	os.Exit(0)
}

func TestProductMetricsDirectChildEnvHookRun(t *testing.T) {
	entries := captureProductMetricsDirectChildEnv(t, func() error {
		previous := hookRunExecutable
		hookRunExecutable = os.Executable
		defer func() { hookRunExecutable = previous }()

		var stdout, stderr bytes.Buffer
		if code := cmdHookRun([]string{"status"}, hookRunOptions{
			Timeout:         testutil.ExecRaceTimeout,
			TimeoutExitCode: 124,
		}, nil, &stdout, &stderr); code != 0 {
			return fmt.Errorf("cmdHookRun code %d: stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		return nil
	})
	assertProductMetricsDirectChildEnv(t, entries)
}

func TestProductMetricsDirectChildEnvHookWorkQuery(t *testing.T) {
	entries := captureProductMetricsDirectChildEnv(t, func() error {
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		_, err = shellWorkQueryWithEnv(shellquote.Quote(binary), "", nil)
		return err
	})
	assertProductMetricsDirectChildEnv(t, entries)
}

func TestProductMetricsDirectChildEnvPerf(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	invocationSpy := &productMetricsInvocationSpy{}
	withProductMetricsInvocationSpy(t, invocationSpy)
	entries := captureProductMetricsDirectChildEnv(t, func() error {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"perf", "run", "--iter", "1", "--warmup", "0", "--", "status"}, &stdout, &stderr); code != 0 {
			return fmt.Errorf("gc perf run code %d: stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		return nil
	})
	assertProductMetricsDirectChildEnv(t, entries)
	_, recordedIDs := invocationSpy.snapshot()
	if len(recordedIDs) != 1 || recordedIDs[0] != productMetricsGeneratedCommandID114 {
		t.Fatalf("outer perf recorded command IDs = %v, want exactly [perf-run]", recordedIDs)
	}
}

func TestProductMetricsDirectChildEnvPromptSling(t *testing.T) {
	entries := captureProductMetricsDirectChildEnv(t, func() error {
		return defaultSlingCaller(context.Background(), []string{"child-env-spy"})
	})
	assertProductMetricsDirectChildEnv(t, entries)
}

func TestProductMetricsDirectChildEnvGitHubNudge(t *testing.T) {
	installProductMetricsDirectChildSpyCommand(t, "gc")
	entries := captureProductMetricsDirectChildEnv(t, func() error {
		defaultNudgeGitHubPRRepairWorker("/test/city", "worker", beads.Bead{ID: "gc-test"}, githubmonitor.Result{
			Owner:       "owner",
			Repo:        "repo",
			Number:      1,
			FailureKind: "checks_failed",
		})
		return nil
	})
	assertProductMetricsDirectChildEnv(t, entries)
}

func TestProductMetricsDirectChildEnvNudgePoller(t *testing.T) {
	entries := captureProductMetricsDirectChildEnv(t, func() error {
		return ensureNudgePoller(t.TempDir(), "worker", "session-worker")
	})
	assertProductMetricsDirectChildEnv(t, entries)
}

func captureProductMetricsDirectChildEnv(t *testing.T, invoke func() error) []string {
	t.Helper()
	snapshot := filepath.Join(t.TempDir(), "child.env")
	t.Setenv(productMetricsDirectChildEnvSpyPath, snapshot)
	t.Setenv(execenv.UsageMetricsDisableEnv, "0")
	t.Setenv("BD_DISABLE_METRICS", "keep-beads-setting")
	t.Setenv("OTEL_SERVICE_NAME", "keep-otel-setting")

	if err := invoke(); err != nil {
		t.Fatalf("invoke direct child: %v", err)
	}
	deadline := time.Now().Add(testutil.ExecRaceTimeout)
	for {
		data, err := os.ReadFile(snapshot)
		if err == nil && bytes.HasSuffix(data, []byte("\n")) {
			return splitProductMetricsDirectChildEnv(data)
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read direct-child environment snapshot: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("direct-child environment snapshot was not written within %s", testutil.ExecRaceTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func splitProductMetricsDirectChildEnv(data []byte) []string {
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func assertProductMetricsDirectChildEnv(t *testing.T, entries []string) {
	t.Helper()
	if got := valuesForProductMetricsDirectChildKey(entries, execenv.UsageMetricsDisableEnv); !slices.Equal(got, []string{execenv.UsageMetricsDisableValue}) {
		t.Fatalf("child %s values = %#v, want canonical [%s]; env=%#v", execenv.UsageMetricsDisableEnv, got, execenv.UsageMetricsDisableValue, entries)
	}
	assertProductMetricsDirectChildUnrelatedEnv(t, entries)
}

func assertProductMetricsDirectChildUnrelatedEnv(t *testing.T, entries []string) {
	t.Helper()
	for key, want := range map[string]string{
		"BD_DISABLE_METRICS": "keep-beads-setting",
		"OTEL_SERVICE_NAME":  "keep-otel-setting",
	} {
		if got := valuesForProductMetricsDirectChildKey(entries, key); !slices.Equal(got, []string{want}) {
			t.Fatalf("child %s values = %#v, want preserved [%s]; env=%#v", key, got, want, entries)
		}
	}
}

func valuesForProductMetricsDirectChildKey(entries []string, key string) []string {
	values := make([]string, 0, 1)
	for _, entry := range entries {
		entryKey, value, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			values = append(values, value)
		}
	}
	return values
}

func installProductMetricsDirectChildSpyCommand(t *testing.T, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test command symlinks require Unix executable lookup semantics")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(binary, filepath.Join(dir, name)); err != nil {
		t.Fatalf("install %s child spy: %v", name, err)
	}
	path := dir
	if inherited := os.Getenv("PATH"); inherited != "" {
		path += string(os.PathListSeparator) + inherited
	}
	t.Setenv("PATH", path)
}
