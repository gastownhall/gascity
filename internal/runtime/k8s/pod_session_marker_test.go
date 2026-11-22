package k8s

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestBuildPodCarriesOneProviderOwnedSessionMarker pins the pod marker
// contract: buildPod puts exactly one GC_K8S_POD_SESSION entry in the main
// container's environment, and its value is the logical session name the pod
// hosts — whatever the session template's env layers supplied. The marker is
// the pod's identity for its own worker-side gc commands (every process in
// the pod inherits the container environment, the tmux server included, so
// respawns keep it), so a config-authored copy must never survive and the
// provider's own value never depends on the template having set one.
func TestBuildPodCarriesOneProviderOwnedSessionMarker(t *testing.T) {
	for _, prebaked := range []bool{false, true} {
		shape := "ordinary"
		if prebaked {
			shape = "prebaked"
		}
		t.Run(shape, func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				configLifted string // value the config env layer supplies, "" = absent
			}{
				{name: "config copy is dropped", configLifted: "config-forged-session"},
				{name: "config copy may be empty", configLifted: ""},
				{name: "no config copy", configLifted: "\x00-absent"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					p := newProviderWithOps(newFakeK8sOps())
					p.prebaked = prebaked
					env := map[string]string{}
					if tc.configLifted != "\x00-absent" {
						env[PodSessionEnv] = tc.configLifted
					}
					pod, err := buildPod("reviewer-7", runtime.Config{Command: "/bin/bash", Env: env}, p)
					if err != nil {
						t.Fatalf("buildPod: %v", err)
					}
					var markers []string
					for _, e := range pod.Spec.Containers[0].Env {
						if e.Name == PodSessionEnv {
							markers = append(markers, e.Value)
						}
					}
					if len(markers) != 1 {
						t.Fatalf("pod env carries %d %s entries (%v), want exactly the provider's own", len(markers), PodSessionEnv, markers)
					}
					if markers[0] != "reviewer-7" {
						t.Fatalf("pod %s = %q, want the logical session name the pod hosts", PodSessionEnv, markers[0])
					}
				})
			}
		})
	}
}

// TestBuildPodEnvFiltersTheSessionMarker batteries the env builder itself:
// a config-supplied marker — empty or not — never reaches the pod env, so
// buildPod's own entry is the only writer.
func TestBuildPodEnvFiltersTheSessionMarker(t *testing.T) {
	for _, supplied := range []string{"reviewer-7", ""} {
		env := mustBuildPodEnv(t, map[string]string{PodSessionEnv: supplied}, "/workspace/rig", "dolt", "3307")
		for _, e := range env {
			if e.Name == PodSessionEnv {
				t.Fatalf("buildPodEnv emitted %s=%q; the pod builder owns that name", PodSessionEnv, e.Value)
			}
		}
	}
}

// TestBuildPodExportsSessionMarkerThroughTheLoginShell pins the LINUX_USERNAME
// path: su - clears the pod environment, so the launch command re-exports the
// marker inside the login shell before starting tmux — with or without a
// controller city — or the tmux server it spawns (and every respawn out of
// it) would run without the pod's session identity.
func TestBuildPodExportsSessionMarkerThroughTheLoginShell(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ctrlCity string
	}{
		{name: "remote stage of a controller city", ctrlCity: "/host/city"},
		{name: "no controller city", ctrlCity: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProviderWithOps(newFakeK8sOps())
			env := map[string]string{"LINUX_USERNAME": "gcagent"}
			if tc.ctrlCity != "" {
				env["GC_CITY"] = tc.ctrlCity
			}
			pod, err := buildPod("reviewer-7", runtime.Config{Command: "/bin/bash", Env: env}, p)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			args := strings.Join(pod.Spec.Containers[0].Args, " ")
			// The export lives inside the su -c payload, which is itself
			// quoted as shell data — so the value's quotes arrive escaped.
			want := "export " + PodSessionEnv + "=" + strings.ReplaceAll(shellquote.Quote("reviewer-7"), "'", "'\\''") + "; "
			if !strings.Contains(args, want) {
				t.Fatalf("launch command %q: want the login-shell marker export %q present", args, want)
			}
		})
	}
}
