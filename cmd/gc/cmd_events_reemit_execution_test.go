package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func TestEventsReemitExecutionDryRunProjectsWithoutOpeningEventLog(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	cityPath, root := setupExecutionReemitCity(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc events reemit-execution dry run = %d; stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("dry run opened event log: stat err=%v", err)
	}
	var got struct {
		RunID      string `json:"run_id"`
		WorkCount  int    `json:"work_count"`
		StepCount  int    `json:"step_count"`
		EventCount int    `json:"event_count"`
		Applied    bool   `json:"applied"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal dry-run summary: %v; stdout=%q", err, stdout.String())
	}
	if got.RunID != root.ID || got.WorkCount != 0 || got.StepCount != 1 || got.EventCount != 1 || got.Applied {
		t.Fatalf("dry-run summary = %+v, want one unapplied step for %q", got, root.ID)
	}
}

func TestEventsReemitExecutionDryRunDoesNotRefreshRuntimeAssets(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BOOTSTRAP", "")

	cityPath, root := setupExecutionReemitCity(t)
	retiredAsset := filepath.Join(cityPath, ".gc", "system", "packs", "retired.txt")
	if err := os.MkdirAll(filepath.Dir(retiredAsset), 0o755); err != nil {
		t.Fatalf("create retired runtime asset: %v", err)
	}
	if err := os.WriteFile(retiredAsset, []byte("preserve me"), 0o644); err != nil {
		t.Fatalf("write retired runtime asset: %v", err)
	}
	before := snapshotExecutionReemitRuntime(t, cityPath)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc events reemit-execution dry run = %d; stderr=%s", code, stderr.String())
	}
	if after := snapshotExecutionReemitRuntime(t, cityPath); !reflect.DeepEqual(after, before) {
		t.Fatalf("dry run changed runtime assets:\n got %#v\nwant %#v", after, before)
	}
	if _, err := os.Stat(retiredAsset); err != nil {
		t.Fatalf("dry run changed runtime assets: %v", err)
	}
}

func TestEventsReemitExecutionDryRunFailureDoesNotRefreshBdRuntimeAssets(t *testing.T) {
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BOOTSTRAP", "")
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("GC_SESSION", "fake")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"reemit\"\n\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatalf("write city config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("create city runtime: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "controller.lock"), nil, 0o600); err != nil {
		t.Fatalf("write controller lock: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", "gcg-missing"}, &stdout, &stderr); code == 0 {
		t.Fatalf("gc events reemit-execution unexpectedly succeeded; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "validating existing bd store") {
		t.Fatalf("dry-run failure = %q, want existing bd store validation", stderr.String())
	}
	if _, err := os.Stat(gcBeadsBdScriptPath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("dry-run failure refreshed bd runtime assets: stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads")); !os.IsNotExist(err) {
		t.Fatalf("dry-run failure created a bd store: stat err=%v", err)
	}
}

func TestEventsReemitExecutionDryRunRejectsMissingFileStoreWithoutCreatingIt(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	cityPath, root := setupExecutionReemitCity(t)
	storePath := filepath.Join(cityPath, ".gc", "beads.json")
	if err := os.Remove(storePath); err != nil {
		t.Fatalf("remove persisted file store: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID}, &stdout, &stderr); code == 0 {
		t.Fatalf("gc events reemit-execution unexpectedly succeeded; stdout=%q", stdout.String())
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("dry run created missing file store: stat err=%v", err)
	}
}

func TestEventsReemitExecutionApplyAppendsProjectedBatch(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	cityPath, root := setupExecutionReemitCity(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID, "--apply"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc events reemit-execution --apply = %d; stderr=%s", code, stderr.String())
	}
	got, err := events.ReadAll(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("read emitted events: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("emitted event count = %d, want 1; events=%#v", len(got), got)
	}
	if got[0].Type != events.ExecutionStepDefined || got[0].Actor != "execution-reemit" || got[0].RunID != root.ID || got[0].StepID != "build" {
		t.Fatalf("emitted event = %#v, want projected execution step", got[0])
	}
	var summary struct {
		Applied bool `json:"applied"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("unmarshal apply summary: %v; stdout=%q", err, stdout.String())
	}
	if !summary.Applied {
		t.Fatalf("apply summary = %#v, want applied", summary)
	}
}

func TestEventsReemitExecutionRejectsUnsafeSelectorsAndProviders(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	cityPath, root := setupExecutionReemitCity(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing city", args: []string{"events", "reemit-execution", "--run", root.ID}, want: "--city is required"},
		{name: "missing run", args: []string{"--city", cityPath, "events", "reemit-execution"}, want: "--run is required"},
		{name: "invalid city", args: []string{"--city", filepath.Join(cityPath, "missing"), "events", "reemit-execution", "--run", root.ID}, want: "resolving --city"},
		{name: "rig", args: []string{"--city", cityPath, "--rig", "repo", "events", "reemit-execution", "--run", root.ID}, want: "--rig is not supported"},
		{name: "context", args: []string{"--city", cityPath, "--context", "remote", "events", "reemit-execution", "--run", root.ID}, want: "remote city selection is not supported"},
		{name: "city url", args: []string{"--city", cityPath, "--city-url", "http://127.0.0.1:9999", "events", "reemit-execution", "--run", root.ID}, want: "remote city selection is not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("gc %v = %d; stderr=%q, want %q", tc.args, code, stderr.String(), tc.want)
			}
		})
	}

	t.Run("configured provider", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"reemit\"\n\n[beads]\nprovider = \"file\"\n\n[events]\nprovider = \"file\"\n"), 0o644); err != nil {
			t.Fatalf("write configured provider: %v", err)
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID, "--apply"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "requires the default file event provider") {
			t.Fatalf("configured provider apply = %d; stderr=%q", code, stderr.String())
		}
	})

	t.Run("environment override", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"reemit\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
			t.Fatalf("restore default provider: %v", err)
		}
		t.Setenv("GC_EVENTS", "fake")
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID, "--apply"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "requires the default file event provider") {
			t.Fatalf("GC_EVENTS apply = %d; stderr=%q", code, stderr.String())
		}
	})

	t.Run("remote environment", func(t *testing.T) {
		t.Setenv("GC_CITY_URL", "http://127.0.0.1:9999")
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "remote city selection is not supported") {
			t.Fatalf("GC_CITY_URL reemit = %d; stderr=%q", code, stderr.String())
		}
	})
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("unsafe invocation opened event log: stat err=%v", err)
	}
}

func TestEventsReemitExecutionRejectsRunningStateAndAllowsStoppedSupervisorCity(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	cityPath, root := setupExecutionReemitCity(t)

	assertRejected := func(t *testing.T, want string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), want) {
			t.Fatalf("reemit = %d; stderr=%q, want %q", code, stderr.String(), want)
		}
	}

	t.Run("held lock", func(t *testing.T) {
		release := holdFlock(t, filepath.Join(cityPath, ".gc", "controller.lock"))
		defer release()
		assertRejected(t, "city controller is running")
	})
	t.Run("stopped local city does not call supervisor hooks", func(t *testing.T) {
		oldSupervisorAlive := supervisorAliveHook
		oldSupervisorCityRunning := supervisorCityRunningHook
		supervisorAliveHook = func() int { panic("supervisor probe called") }
		supervisorCityRunningHook = func(string) (bool, string, bool) { panic("city enumeration called") }
		t.Cleanup(func() { supervisorAliveHook, supervisorCityRunningHook = oldSupervisorAlive, oldSupervisorCityRunning })
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID}, &stdout, &stderr); code != 0 {
			t.Fatalf("stopped reemit = %d; stderr=%q", code, stderr.String())
		}
	})
}

func TestEventsReemitExecutionHoldsControllerLockUntilCompletion(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	cityPath, root := setupExecutionReemitCity(t)
	acquired := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	previousHook := executionReemitAfterLockAcquiredHook
	executionReemitAfterLockAcquiredHook = func() {
		close(acquired)
		<-release
	}
	t.Cleanup(func() { executionReemitAfterLockAcquiredHook = previousHook })

	result := make(chan struct {
		code   int
		stderr string
	}, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		result <- struct {
			code   int
			stderr string
		}{
			code:   run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID}, &stdout, &stderr),
			stderr: stderr.String(),
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-acquired:
	case <-ctx.Done():
		t.Fatalf("reemit command did not reach controller-lock barrier: %v", ctx.Err())
	}

	lockPath := filepath.Join(cityPath, ".gc", "controller.lock")
	competitor, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open competing controller lock: %v", err)
	}
	defer competitor.Close() //nolint:errcheck // test cleanup
	if err := syscall.Flock(int(competitor.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("competing controller lock = %v, want EWOULDBLOCK or EAGAIN", err)
	}

	close(release)
	select {
	case got := <-result:
		if got.code != 0 {
			t.Fatalf("reemit command = %d; stderr=%q", got.code, got.stderr)
		}
	case <-ctx.Done():
		t.Fatalf("reemit command did not complete after releasing barrier: %v", ctx.Err())
	}

	if err := syscall.Flock(int(competitor.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("controller lock remained held after reemit completion: %v", err)
	}
	if err := syscall.Flock(int(competitor.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock competing controller lock: %v", err)
	}
}

func TestEventsReemitExecutionProjectionFailureDoesNotOpenEventLog(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	cityPath, _ := setupExecutionReemitCity(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", "gcg-missing", "--apply"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "projecting run") {
		t.Fatalf("projection failure = %d; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("projection failure opened event log: stat err=%v", err)
	}
}

func setupExecutionReemitCity(t *testing.T) (string, beads.Bead) {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"reemit\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatalf("write city config: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatalf("ensure file store layout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "controller.lock"), nil, 0o600); err != nil {
		t.Fatalf("write controller lock: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityPath); err != nil {
		t.Fatalf("ensure file store: %v", err)
	}
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	root, err := store.Create(beads.Bead{ID: "gcg-reemit-root", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
	}})
	if err != nil {
		t.Fatalf("create graph root: %v", err)
	}
	if _, err := store.Create(beads.Bead{ID: "gcg-reemit-step", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:             root.ID,
		beadmeta.StepIDMetadataKey:                 "build",
		beadmeta.NativeStepDependenciesMetadataKey: "[]",
	}}); err != nil {
		t.Fatalf("create graph step: %v", err)
	}
	return cityPath, root
}

func snapshotExecutionReemitRuntime(t *testing.T, cityPath string) map[string]string {
	t.Helper()
	runtimeDir := filepath.Join(cityPath, ".gc")
	snapshot := make(map[string]string)
	err := filepath.WalkDir(runtimeDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(runtimeDir, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			snapshot[relative] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[relative] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot runtime assets: %v", err)
	}
	return snapshot
}
