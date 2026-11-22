package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
)

// isolateWorkerPodCredentialShell homes the process into a credential-less
// worker pod shell: an empty kubeconfig, no in-cluster service endpoints, and
// no GC_SESSION override. Each test decides the marker's value.
func isolateWorkerPodCredentialShell(t *testing.T) {
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

// TestUnmarkedK8sHybridSelectionKeepsItsCredentialWall pins the other side of
// the pod-marker contract: with no marker, k8s and hybrid selection build the
// full provider exactly as before, so a controller-side (or credentialed)
// process with no usable configuration still gets the construction failure it
// always got. The marker must not weaken the unmarked path's failure mode,
// and must not make construction succeed through some silent fallback.
func TestUnmarkedK8sHybridSelectionKeepsItsCredentialWall(t *testing.T) {
	for _, tc := range []struct {
		provider   string
		remoteName string
	}{
		{provider: "k8s"},
		{provider: "hybrid", remoteName: "reviewer"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			isolateWorkerPodCredentialShell(t)
			t.Setenv(sessionk8s.PodSessionEnv, "")

			cityPath := t.TempDir()
			cfg := &config.City{Session: config.SessionConfig{Provider: tc.provider, RemoteMatch: tc.remoteName}}
			cfg.Workspace.Name = "pod-city"

			sp, err := newSessionProviderForCity(cfg, cityPath)
			if err == nil {
				t.Fatalf("newSessionProviderForCity without the marker = %T, want the credential construction failure preserved", sp)
			}
			if !strings.Contains(err.Error(), "no configuration has been provided") {
				t.Fatalf("err = %v, want the Kubernetes credential failure text", err)
			}
		})
	}
}

// TestMarkerDoesNotRerouteOtherSelectedRuntimes pins the marker's scope: it
// selects the pod-local provider only for the k8s and hybrid selections a
// generated pod actually uses. An explicitly selected other runtime — by the
// city's own session-provider name, or by an explicit GC_SESSION override of
// a k8s city — keeps its ordinary factory and executes as before, even in a
// process carrying the marker.
func TestMarkerDoesNotRerouteOtherSelectedRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cityProvider string
		envOverride  string
	}{
		{name: "city selects another runtime", cityProvider: "fake"},
		{name: "explicit selection overrides a k8s city", cityProvider: "k8s", envOverride: "fake"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateWorkerPodCredentialShell(t)
			t.Setenv(sessionk8s.PodSessionEnv, "reviewer-7")
			t.Setenv("GC_SESSION", tc.envOverride)

			cityPath := t.TempDir()
			cfg := &config.City{Session: config.SessionConfig{Provider: tc.cityProvider}}
			cfg.Workspace.Name = "pod-city"

			sp, err := newSessionProviderForCity(cfg, cityPath)
			if err != nil {
				t.Fatalf("newSessionProviderForCity with marker + explicit %q selection: %v; the marker must not reroute other runtimes", tc.envOverride, err)
			}
			// The fake answers every operation on any session; the pod-local
			// provider refuses any other session. Serving "somebody-else" is
			// the proof the provider is the ordinary one.
			if err := sp.SetMeta("somebody-else", "gc-keep-runs", "1"); err != nil {
				t.Fatalf("SetMeta on a foreign session = %v; want the ordinary (non-pod-local) provider behavior", err)
			}
		})
	}
}
