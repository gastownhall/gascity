package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime/guard"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
)

// withoutKubeCredentials leaves the process with neither an in-cluster
// service account nor a usable kubeconfig, as in a generated worker pod.
func withoutKubeCredentials(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	kubeconfig := filepath.Join(home, "kubeconfig")
	if err := os.WriteFile(kubeconfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("GC_SESSION", "")
	t.Setenv("GC_TMUX_SESSION", "main")
}

// A worker-side gc command inside a pod the k8s provider generated has no
// Kubernetes credentials, and the pod env carries the provider's session
// marker and not GC_SESSION, so the command resolves the city's own k8s or
// hybrid provider. That provider must be served without a Kubernetes client,
// or `gc runtime drain-ack` and `gc hook --claim --drain-ack` can never
// acknowledge a drain.
func TestK8sMarkedWorkerPodSessionProviderNeedsNoKubeClient(t *testing.T) {
	for _, provider := range []string{"k8s", "hybrid"} {
		t.Run(provider, func(t *testing.T) {
			withoutKubeCredentials(t)
			t.Setenv(sessionk8s.PodSessionEnv, "pod-city--worker-1")
			t.Setenv("GC_HYBRID_REMOTE_MATCH", "")

			cfg := &config.City{Session: config.SessionConfig{Provider: provider, RemoteMatch: "worker"}}
			cfg.Workspace.Name = "pod-city"

			sp, err := newSessionProviderForCity(cfg, t.TempDir())
			if err != nil {
				t.Fatalf("newSessionProviderForCity in a credential-less marked worker pod: %v", err)
			}
			if sp == nil {
				t.Fatal("newSessionProviderForCity returned a nil provider")
			}
			// The pod-local provider refuses lifecycle operations for every
			// name without running anything; a local tmux or k8s backend
			// would instead act on (or fail to reach) a runtime.
			for _, name := range []string{"main", "pod-city--worker-1"} {
				if err := sp.Stop(name); !errors.Is(err, guard.ErrControllerOwnedRuntime) {
					t.Fatalf("Stop(%q) = %v; want the pod-local controller-owned refusal", name, err)
				}
			}
			reg, err := runtimeRegistryForCity(cfg)
			if err != nil {
				t.Fatal(err)
			}
			leaf, err := reg.New(provider, cfg.Session, "pod-city", t.TempDir())
			if err != nil {
				t.Fatalf("registry %q: %v", provider, err)
			}
			if _, ok := leaf.(*sessionk8s.PodLocal); !ok {
				t.Fatalf("registry %q constructed %T; want *k8s.PodLocal", provider, leaf)
			}
		})
	}
}

// Outside a generated pod, or when the city or an override explicitly
// selects another runtime, provider selection and construction stay as they
// were: the marker changes only a k8s or hybrid selection.
func TestK8sPodSessionMarkerLeavesOtherSelectionsUnchanged(t *testing.T) {
	t.Run("unmarked k8s and hybrid still need credentials", func(t *testing.T) {
		for _, marker := range []string{"", "  \t"} {
			for provider, wantErr := range map[string]string{
				"k8s":    "building K8s config",
				"hybrid": "hybrid: k8s backend: building K8s config",
			} {
				withoutKubeCredentials(t)
				t.Setenv(sessionk8s.PodSessionEnv, marker)
				cfg := &config.City{Session: config.SessionConfig{Provider: provider}}
				cfg.Workspace.Name = "pod-city"
				_, err := newSessionProviderForCity(cfg, t.TempDir())
				if err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("%s with marker %q: err = %v; want the credential error %q", provider, marker, err, wantErr)
				}
			}
		}
	})
	t.Run("marked process selecting another runtime constructs it", func(t *testing.T) {
		withoutKubeCredentials(t)
		t.Setenv(sessionk8s.PodSessionEnv, "pod-city--worker-1")
		reg, err := runtimeRegistryForCity(&config.City{})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"fake", "tmux", "subprocess"} {
			sp, err := reg.New(name, config.SessionConfig{}, "pod-city", t.TempDir())
			if err != nil {
				t.Fatalf("registry %q: %v", name, err)
			}
			if _, ok := sp.(*sessionk8s.PodLocal); ok {
				t.Fatalf("registry %q constructed the pod-local provider; want its own runtime", name)
			}
		}
	})
	t.Run("marked process with an explicit override constructs the override", func(t *testing.T) {
		withoutKubeCredentials(t)
		t.Setenv(sessionk8s.PodSessionEnv, "pod-city--worker-1")
		t.Setenv("GC_SESSION", "fake")
		cfg := &config.City{Session: config.SessionConfig{Provider: "k8s"}}
		cfg.Workspace.Name = "pod-city"
		sp, err := newSessionProviderForCity(cfg, t.TempDir())
		if err != nil {
			t.Fatalf("newSessionProviderForCity with GC_SESSION=fake: %v", err)
		}
		if err := sp.Stop("main"); errors.Is(err, guard.ErrControllerOwnedRuntime) {
			t.Fatalf("GC_SESSION=fake override served the pod-local provider (Stop = %v)", err)
		}
	})
}

// A control dispatcher inside a generated pod validates its k8s/hybrid
// selection statically at startup: valid settings pass with no Kubernetes
// credentials, and malformed pod settings or an unregistered runtime name
// fail with the dispatcher's startup context.
func TestValidateDispatchSessionProviderConfigInMarkedPod(t *testing.T) {
	for _, provider := range []string{"k8s", "hybrid"} {
		cases := []struct {
			name    string
			env     map[string]string
			wantErr []string
		}{
			{name: "valid static settings"},
			{
				name: "valid scheduling and managed service",
				env: map[string]string{
					"GC_K8S_NODE_SELECTOR": `{"pool":"agents"}`,
					"GC_K8S_TOLERATIONS":   `[{"key":"k","operator":"Exists"}]`,
					"GC_K8S_DOLT_HOST":     "dolt.svc",
					"GC_K8S_DOLT_PORT":     "3307",
				},
			},
			{name: "malformed node selector", env: map[string]string{"GC_K8S_NODE_SELECTOR": "{"}, wantErr: []string{"GC_K8S_NODE_SELECTOR"}},
			{name: "malformed tolerations", env: map[string]string{"GC_K8S_TOLERATIONS": "nope"}, wantErr: []string{"GC_K8S_TOLERATIONS"}},
			{name: "malformed affinity", env: map[string]string{"GC_K8S_AFFINITY": "[1]"}, wantErr: []string{"GC_K8S_AFFINITY"}},
			{name: "half a managed service", env: map[string]string{"GC_K8S_DOLT_HOST": "dolt.svc"}, wantErr: []string{"GC_K8S_DOLT_HOST", "GC_K8S_DOLT_PORT"}},
		}
		for _, tc := range cases {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				withoutKubeCredentials(t)
				t.Setenv(sessionk8s.PodSessionEnv, "pod-city--control-dispatcher")
				for _, k := range []string{"GC_K8S_NODE_SELECTOR", "GC_K8S_TOLERATIONS", "GC_K8S_AFFINITY", "GC_K8S_PRIORITY_CLASS_NAME", "GC_K8S_DOLT_HOST", "GC_K8S_DOLT_PORT"} {
					t.Setenv(k, tc.env[k])
				}
				cfg := &config.City{Session: config.SessionConfig{Provider: provider}}
				cfg.Workspace.Name = "pod-city"
				err := validateDispatchSessionProviderConfig(cfg, t.TempDir())
				if len(tc.wantErr) == 0 {
					if err != nil {
						t.Fatalf("validateDispatchSessionProviderConfig: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("validateDispatchSessionProviderConfig succeeded; want an error naming %v", tc.wantErr)
				}
				for _, want := range append([]string{"control dispatcher"}, tc.wantErr...) {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q does not contain %q", err, want)
					}
				}
			})
		}
	}
	t.Run("unregistered runtime name", func(t *testing.T) {
		withoutKubeCredentials(t)
		t.Setenv(sessionk8s.PodSessionEnv, "pod-city--control-dispatcher")
		cfg := &config.City{Session: config.SessionConfig{Provider: "k8z"}}
		cfg.Workspace.Name = "pod-city"
		err := validateDispatchSessionProviderConfig(cfg, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "control dispatcher") || !strings.Contains(err.Error(), `"k8z"`) {
			t.Fatalf("validateDispatchSessionProviderConfig(k8z) = %v; want a contextual unresolved-runtime error", err)
		}
	})
}
