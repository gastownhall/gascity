package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
)

// markedDispatcherCity is the city config a pod-hosted control dispatcher
// would start with: session provider name, no GC_SESSION override.
func markedDispatcherCity(provider string) *config.City {
	cfg := &config.City{Session: config.SessionConfig{Provider: provider}}
	cfg.Workspace.Name = "pod-city"
	return cfg
}

// TestValidateDispatchSessionProviderConfigMarkedValidStaticSettings pins
// the marked startup-validation contract: a pod-hosted dispatcher with a
// composed registry and a resolvable k8s or hybrid selection passes
// validation statically — no Kubernetes client construction, no API probe —
// provided the pod-creation settings it carries parse.
func TestValidateDispatchSessionProviderConfigMarkedValidStaticSettings(t *testing.T) {
	t.Setenv("GC_SESSION", "")
	t.Setenv(sessionk8s.PodSessionEnv, "reviewer-7")
	for _, provider := range []string{"k8s", "hybrid"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("GC_K8S_NODE_SELECTOR", "")
			t.Setenv("GC_K8S_TOLERATIONS", "")
			t.Setenv("GC_K8S_AFFINITY", "")
			t.Setenv("GC_K8S_DOLT_HOST", "")
			t.Setenv("GC_K8S_DOLT_PORT", "")
			if err := validateDispatchSessionProviderConfig(markedDispatcherCity(provider), t.TempDir()); err != nil {
				t.Fatalf("marked validation of %q: %v", provider, err)
			}
		})
	}
}

// TestValidateDispatchSessionProviderConfigMarkedStaticFailures keeps the
// marked validation loud about malformed pod-creation settings: malformed
// scheduling JSON and a half-set managed Dolt alias both fail at startup
// with their settings named, instead of surfacing only when a retry recycles
// a session.
func TestValidateDispatchSessionProviderConfigMarkedStaticFailures(t *testing.T) {
	t.Setenv("GC_SESSION", "")
	t.Setenv(sessionk8s.PodSessionEnv, "reviewer-7")
	t.Setenv("GC_K8S_TOLERATIONS", "")
	t.Setenv("GC_K8S_AFFINITY", "")
	t.Setenv("GC_K8S_DOLT_HOST", "")
	t.Setenv("GC_K8S_DOLT_PORT", "")
	cfg := markedDispatcherCity("k8s")

	t.Run("malformed node selector", func(t *testing.T) {
		t.Setenv("GC_K8S_NODE_SELECTOR", "{not json")
		err := validateDispatchSessionProviderConfig(cfg, t.TempDir())
		if err == nil {
			t.Fatal("malformed GC_K8S_NODE_SELECTOR: err = nil, want a startup error")
		}
		if !strings.Contains(err.Error(), "GC_K8S_NODE_SELECTOR") || !strings.Contains(err.Error(), "control dispatcher") {
			t.Fatalf("err = %v, want the setting and the dispatcher startup named", err)
		}
	})
	t.Run("half-set managed service alias", func(t *testing.T) {
		t.Setenv("GC_K8S_NODE_SELECTOR", "")
		t.Setenv("GC_K8S_DOLT_HOST", "dolt.gc.svc.cluster.local")
		err := validateDispatchSessionProviderConfig(cfg, t.TempDir())
		if err == nil {
			t.Fatal("half-set GC_K8S_DOLT_HOST/PORT: err = nil, want a startup error")
		}
		if !strings.Contains(err.Error(), "GC_K8S_DOLT_HOST") || !strings.Contains(err.Error(), "control dispatcher") {
			t.Fatalf("err = %v, want the setting and the dispatcher startup named", err)
		}
	})
}

// TestValidateDispatchSessionProviderConfigMarkedRejectsUnresolvedName
// pins the marked-path rule the unmarked path deliberately does not have: a
// selection name that resolves to nothing (and would otherwise fall to the
// tmux fallback) fails the marked startup validation naming the name. A
// resolvable non-k8s/hybrid name (tmux) keeps its own factory and its
// validation, without the pod-creation setting checks.
func TestValidateDispatchSessionProviderConfigMarkedRejectsUnresolvedName(t *testing.T) {
	t.Setenv("GC_SESSION", "")
	t.Setenv(sessionk8s.PodSessionEnv, "reviewer-7")

	for _, tc := range []struct {
		name     string
		provider string
		wantErr  string // "" = nil error
	}{
		{name: "unresolved name", provider: "no-such-runtime", wantErr: "no-such-runtime"},
		{name: "resolvable other name", provider: "tmux", wantErr: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Malformed k8s settings must not fail a tmux selection's marked
			// validation: they are the pod-creation settings of the k8s and
			// hybrid factories, not tmux's.
			t.Setenv("GC_K8S_NODE_SELECTOR", "{not json")
			err := validateDispatchSessionProviderConfig(markedDispatcherCity(tc.provider), t.TempDir())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("marked validation of %q: %v, want nil", tc.provider, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("marked validation of %q: err = nil, want a refusal naming %q", tc.provider, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}

	// Unmarked (the ordinary controller shape): an unresolved name falls to
	// the tmux fallback and constructs, so it stays nil — this is the
	// behavior the marked path deliberately diverges from.
	t.Setenv(sessionk8s.PodSessionEnv, "")
	t.Setenv("GC_K8S_NODE_SELECTOR", "")
	if err := validateDispatchSessionProviderConfig(markedDispatcherCity("no-such-runtime"), t.TempDir()); err != nil {
		t.Fatalf("unmarked validation of an unresolved name: %v, want nil (tmux fallback)", err)
	}
}
