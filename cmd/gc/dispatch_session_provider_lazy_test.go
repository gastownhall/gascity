package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestControlDispatchBuildsNoSessionProviderUnlessARetryRecycles pins
// ga-vnycm2.18 (g1): building the session provider reads the session snapshot
// (two bd calls), and only a pooled transient retry that recycles its
// subject's session uses it. A workflow whose retries all pass dispatches
// every control without building one. The recycle path itself, which must
// still build it, is pinned by the retry-eval recycle test in
// cmd_convoy_dispatch_test.go.
func TestControlDispatchBuildsNoSessionProviderUnlessARetryRecycles(t *testing.T) {
	store, convoyID, workflowID := startMemScopedWorkflow(t)
	cfg := buildMemGraphWorkflowConfig(t)
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	built := 0
	prevProvider := dispatchControlSessionProvider
	dispatchControlSessionProvider = func() (runtime.Provider, error) {
		built++
		return runtime.NewFake(), nil
	}
	t.Cleanup(func() { dispatchControlSessionProvider = prevProvider })

	dispatched := map[string]int{}
	for step := 0; step < 200; step++ {
		if mustGetMemBead(t, store, workflowID).Status == "closed" {
			break
		}
		progressed := false
		for _, bead := range memGraphReady(t, store) {
			kind := bead.Metadata["gc.kind"]
			if !graphroute.IsControlDispatcherKind(kind) {
				continue
			}
			if err := runControlDispatcherWithStoreAndConfig(cityPath, cityPath, store, bead.ID, cfg, io.Discard, io.Discard); err != nil {
				t.Fatalf("dispatch %s (%s): %v", bead.ID, kind, err)
			}
			dispatched[kind]++
			progressed = true
		}
		for {
			ready := memGraphReady(t, store)
			if i, ok := firstClaimableGraphWorkerBead(ready, "worker"); ok {
				worker := "worker"
				if err := store.Update(ready[i].ID, beads.UpdateOpts{Assignee: &worker}); err != nil {
					t.Fatalf("claim %s: %v", ready[i].ID, err)
				}
				ready[i] = mustGetMemBead(t, store, ready[i].ID)
			}
			bead, ok, err := selectExecutableGraphWorkerBead(ready, "worker")
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			executeMemGraphWorkerBead(t, store, bead, convoyID, cityPath, "success")
			progressed = true
		}
		if !progressed {
			t.Fatalf("workflow %s made no progress", workflowID)
		}
	}
	if got := mustGetMemBead(t, store, workflowID).Status; got != "closed" {
		t.Fatalf("workflow status = %q, want closed", got)
	}
	if dispatched["retry"] == 0 {
		t.Fatalf("premise: no retry control was dispatched (dispatched=%v)", dispatched)
	}
	if built != 0 {
		t.Fatalf("session provider built %d times for %v, want 0: no retry recycled a session", built, dispatched)
	}
}

// TestControlDispatcherReportsABrokenSessionProviderConfigAtStartup pins the
// other half of the lazy provider: a session provider that cannot be built
// still fails the control dispatcher fast, at startup and with a clear error,
// before it serves any control -- not later, on the first retry that recycles.
// A provider that builds lets the dispatcher start and serve.
func TestControlDispatcherReportsABrokenSessionProviderConfigAtStartup(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevBuild := buildSessionProviderByName
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		buildSessionProviderByName = prevBuild
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})
	scans := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		scans++
		return nil, nil
	}

	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return nil, errors.New("injected provider failure")
	}
	var stderr bytes.Buffer
	err := runWorkflowServe("", false, io.Discard, &stderr)
	if err == nil || !strings.Contains(err.Error(), "session provider config is invalid") || !strings.Contains(err.Error(), "injected provider failure") {
		t.Fatalf("runWorkflowServe with a broken provider = %v, want a startup error naming the session provider config", err)
	}
	if !strings.Contains(stderr.String(), "session provider config is invalid") {
		t.Fatalf("stderr = %q, want the startup error logged", stderr.String())
	}
	if scans != 0 {
		t.Fatalf("dispatcher scanned %d times with a broken provider, want 0: it must fail before serving", scans)
	}

	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return runtime.NewFake(), nil
	}
	if err := runWorkflowServe("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe with a working provider: %v", err)
	}
	if scans == 0 {
		t.Fatal("dispatcher with a working provider never scanned")
	}
}

// A marked Kubernetes worker has no Kubernetes credentials of its own. It
// validates the static city settings but leaves client construction to the controller.
func TestK8sWorkerDispatcherSkipsLiveProviderConstruction(t *testing.T) {
	clearGCEnv(t)
	clearKubernetesClientConfigForDispatchTest(t)
	t.Setenv("GC_SESSION_ID", "de-wisp-test")
	t.Setenv(config.K8sWorkerPodMarkerEnv, "true")

	cfg := &config.City{}
	cfg.Session.Provider = "k8s"
	built := 0
	prevBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		built++
		return nil, errors.New("live provider construction should be skipped")
	}
	t.Cleanup(func() { buildSessionProviderByName = prevBuild })

	if err := validateDispatchSessionProviderConfig(cfg, t.TempDir()); err != nil {
		t.Fatalf("marked K8s worker validation without API credentials: %v", err)
	}
	if built != 0 {
		t.Fatalf("live provider construction called %d times, want 0", built)
	}
}

func TestK8sWorkerControlDispatcherServesWithoutKubernetesClient(t *testing.T) {
	clearGCEnv(t)
	clearKubernetesClientConfigForDispatchTest(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	cityConfig := "[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n\n[session]\nprovider = \"k8s\"\n" + testControlDispatcherAgentTOML("")
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfig), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_SESSION_ID", "de-wisp-test")
	t.Setenv("GC_SESSION_NAME", "test-city/control-dispatcher")
	t.Setenv(config.K8sWorkerPodMarkerEnv, "true")

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevBuild := buildSessionProviderByName
	cityFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		buildSessionProviderByName = prevBuild
	})
	scans := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		scans++
		return nil, nil
	}
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		t.Fatal("marked Kubernetes worker attempted live provider construction")
		return nil, nil
	}

	if err := runWorkflowServe("control-dispatcher", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe without Kubernetes API credentials: %v", err)
	}
	if scans == 0 {
		t.Fatal("marked Kubernetes worker did not serve any control scan")
	}
}

func TestK8sWorkerMarkerDoesNotSkipSessionProviderOverride(t *testing.T) {
	clearGCEnv(t)
	clearKubernetesClientConfigForDispatchTest(t)
	t.Setenv("GC_SESSION_ID", "de-wisp-test")
	t.Setenv(config.K8sWorkerPodMarkerEnv, "true")
	t.Setenv("GC_SESSION", "tmux")

	cfg := &config.City{}
	cfg.Session.Provider = "k8s"
	built := 0
	prevBuild := buildSessionProviderByName
	buildSessionProviderByName = func(_ *config.City, name string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		built++
		if name != "tmux" {
			t.Fatalf("provider selected = %q, want GC_SESSION override tmux", name)
		}
		return runtime.NewFake(), nil
	}
	t.Cleanup(func() { buildSessionProviderByName = prevBuild })

	if err := validateDispatchSessionProviderConfig(cfg, t.TempDir()); err != nil {
		t.Fatalf("validate overridden provider: %v", err)
	}
	if built != 1 {
		t.Fatalf("provider construction called %d times, want 1", built)
	}
}

func TestLocalManagedDispatcherStillConstructsConfiguredProvider(t *testing.T) {
	clearGCEnv(t)
	clearKubernetesClientConfigForDispatchTest(t)
	t.Setenv("GC_SESSION_ID", "de-wisp-test")
	t.Setenv(config.K8sWorkerPodMarkerEnv, "")

	cfg := &config.City{}
	cfg.Session.Provider = "k8s"
	err := validateDispatchSessionProviderConfig(cfg, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "building K8s config") {
		t.Fatalf("local managed dispatcher validation error = %v, want ordinary K8s client-config validation", err)
	}
}

func TestK8sWorkerDispatcherRejectsInvalidStaticProviderConfig(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*testing.T, *config.City)
		want      string
	}{
		{
			name: "invalid pod identity",
			configure: func(_ *testing.T, cfg *config.City) {
				invalid := int64(-1)
				cfg.Session.K8s.RunAsUser = &invalid
			},
			want: "must be greater than zero",
		},
		{
			name: "invalid scheduling JSON",
			configure: func(t *testing.T, _ *config.City) {
				t.Setenv("GC_K8S_NODE_SELECTOR", "{")
			},
			want: "parsing GC_K8S_NODE_SELECTOR",
		},
		{
			name: "invalid Secret projection",
			configure: func(_ *testing.T, cfg *config.City) {
				cfg.Session.K8s.SecretEnv = []config.K8sSecretEnv{{Name: "bad-name", Secret: "token", Key: "key"}}
			},
			want: "not a valid environment variable name",
		},
		{
			name: "reserved worker marker projection",
			configure: func(_ *testing.T, cfg *config.City) {
				cfg.Session.K8s.SecretEnv = []config.K8sSecretEnv{{Name: config.K8sWorkerPodMarkerEnv, Secret: "token", Key: "key"}}
			},
			want: "reserved for the Kubernetes worker marker",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGCEnv(t)
			clearKubernetesClientConfigForDispatchTest(t)
			t.Setenv("GC_SESSION_ID", "de-wisp-test")
			t.Setenv(config.K8sWorkerPodMarkerEnv, "true")
			cfg := &config.City{}
			cfg.Session.Provider = "k8s"
			tt.configure(t, cfg)
			err := validateDispatchSessionProviderConfig(cfg, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("marked K8s worker validation error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestK8sSessionProviderConstructionStillRequiresClientConfig(t *testing.T) {
	clearGCEnv(t)
	clearKubernetesClientConfigForDispatchTest(t)
	_, err := k8sProviderFromSession(config.K8sConfig{}, "test-city")
	if err == nil || !strings.Contains(err.Error(), "building K8s config") {
		t.Fatalf("K8s provider construction error = %v, want missing client config", err)
	}
}

func clearKubernetesClientConfigForDispatchTest(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing-kubeconfig"))
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
}
