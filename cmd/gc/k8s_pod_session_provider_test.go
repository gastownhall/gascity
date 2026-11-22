package main

import (
	"errors"
	"os"
	"path/filepath"
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
