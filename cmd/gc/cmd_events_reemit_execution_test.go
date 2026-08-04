package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	oldControllerAlive := eventsReemitExecutionControllerAliveHook
	oldSupervisorAlive := supervisorAliveHook
	oldSupervisorCityRunning := supervisorCityRunningHook
	t.Cleanup(func() {
		eventsReemitExecutionControllerAliveHook = oldControllerAlive
		supervisorAliveHook = oldSupervisorAlive
		supervisorCityRunningHook = oldSupervisorCityRunning
	})

	assertRejected := func(t *testing.T, want string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), want) {
			t.Fatalf("reemit = %d; stderr=%q, want %q", code, stderr.String(), want)
		}
	}

	t.Run("controller", func(t *testing.T) {
		eventsReemitExecutionControllerAliveHook = func(string) int { return 1 }
		supervisorAliveHook = func() int { return 0 }
		assertRejected(t, "city controller is running")
	})
	t.Run("supervisor known running", func(t *testing.T) {
		eventsReemitExecutionControllerAliveHook = func(string) int { return 0 }
		supervisorAliveHook = func() int { return 1 }
		supervisorCityRunningHook = func(string) (bool, string, bool) { return true, "running", true }
		assertRejected(t, "city is running under the supervisor")
	})
	t.Run("supervisor unknown", func(t *testing.T) {
		supervisorCityRunningHook = func(string) (bool, string, bool) { return false, "", false }
		assertRejected(t, "could not determine supervisor city state")
	})
	t.Run("supervisor known stopped", func(t *testing.T) {
		supervisorCityRunningHook = func(string) (bool, string, bool) { return false, "stopped", true }
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--city", cityPath, "events", "reemit-execution", "--run", root.ID}, &stdout, &stderr); code != 0 {
			t.Fatalf("known stopped reemit = %d; stderr=%q", code, stderr.String())
		}
	})
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
