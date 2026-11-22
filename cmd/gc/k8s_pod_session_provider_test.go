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
