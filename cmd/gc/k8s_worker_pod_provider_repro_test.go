package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
)

// A worker-side gc command inside a pod the k8s provider generated has no
// Kubernetes credentials: no in-cluster service account and no kubeconfig.
// The pod env carries GC_TMUX_SESSION and not GC_SESSION, so the command
// resolves the city's own "k8s" provider, and the pod-local marker
// (GC_K8S_POD_SESSION, the logical session name buildPod stamps) selects the
// client-free provider. Building that session provider must not require a
// live Kubernetes client, or `gc runtime drain-ack` and `gc hook --claim
// --drain-ack` can never acknowledge a drain.
func TestK8sGeneratedWorkerPodSessionProviderNeedsNoKubeClient(t *testing.T) {
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
	t.Setenv(sessionk8s.PodSessionEnv, "reviewer-7")

	cityPath := t.TempDir()
	cfg := &config.City{Session: config.SessionConfig{Provider: "k8s"}}
	cfg.Workspace.Name = "pod-city"

	sp, err := newSessionProviderForCity(cfg, cityPath)
	if err != nil {
		t.Fatalf("newSessionProviderForCity in a credential-less generated worker pod: %v", err)
	}
	if sp == nil {
		t.Fatal("newSessionProviderForCity returned a nil provider")
	}
}

// A hybrid city's factory eagerly builds both backends, so a worker pod of a
// hybrid city hits the same client wall through its k8s backend. The marked
// arm must intercept selection before either backend is constructed, so no
// local tmux backend is built and no remote-match routing applies to the
// pod's own session.
func TestHybridGeneratedWorkerPodSessionProviderNeedsNoKubeClient(t *testing.T) {
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
	t.Setenv(sessionk8s.PodSessionEnv, "reviewer-7")

	cityPath := t.TempDir()
	cfg := &config.City{Session: config.SessionConfig{Provider: "hybrid", RemoteMatch: "reviewer"}}
	cfg.Workspace.Name = "pod-city"

	sp, err := newSessionProviderForCity(cfg, cityPath)
	if err != nil {
		t.Fatalf("newSessionProviderForCity in a credential-less generated worker pod of a hybrid city: %v", err)
	}
	if sp == nil {
		t.Fatal("newSessionProviderForCity returned a nil provider")
	}
}
