package k8s

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// Every pod the provider generates carries exactly one PodSessionEnv entry,
// naming the logical session it hosts, whatever the session config's env says.
func TestBuildPodCarriesOneProviderOwnedSessionMarker(t *testing.T) {
	for _, cfgValue := range []string{"", "spoofed-session", "pod-city--worker-1"} {
		p := newProviderWithOps(newFakeK8sOps())
		cfg := runtime.Config{Command: "/bin/bash", Env: map[string]string{PodSessionEnv: cfgValue}}
		pod, err := buildPod("pod-city--worker-1", cfg, p)
		if err != nil {
			t.Fatalf("buildPod: %v", err)
		}
		var values []string
		for _, e := range pod.Spec.Containers[0].Env {
			if e.Name == PodSessionEnv {
				if e.ValueFrom != nil {
					t.Fatalf("%s is projected from %v; want a literal value", PodSessionEnv, e.ValueFrom)
				}
				values = append(values, e.Value)
			}
		}
		if len(values) != 1 || values[0] != "pod-city--worker-1" {
			t.Fatalf("config %s=%q: pod env %s = %q; want exactly [\"pod-city--worker-1\"]", PodSessionEnv, cfgValue, PodSessionEnv, values)
		}
	}
}

// su - clears the pod environment, so a LINUX_USERNAME pod re-exports the
// marker inside the login shell that starts tmux, even with no controller
// city path. The tmux server's environment then carries it, and respawn-pane
// starts the relaunched agent from that environment.
func TestBuildPodExportsSessionMarkerThroughLoginShell(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	session := "pod-city--worker 1;$(x)"
	cfg := runtime.Config{Command: "/bin/bash", Env: map[string]string{"LINUX_USERNAME": "gcagent", PodSessionEnv: "spoofed"}}
	pod, err := buildPod(session, cfg, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	launch := pod.Spec.Containers[0].Args[0]
	userScript := "export " + PodSessionEnv + "=" + shellquote.Quote(session) + "; "
	if !strings.Contains(launch, "export "+PodSessionEnv+"=") {
		t.Fatalf("login-shell launch does not export %s; got: %s", PodSessionEnv, launch)
	}
	if strings.Contains(launch, "spoofed") {
		t.Fatalf("login-shell launch exports the config-supplied marker; got: %s", launch)
	}
	if got := suPodExports("pod-city", "", session); got != userScript {
		t.Fatalf("suPodExports with no controller city = %q; want %q", got, userScript)
	}
	withCity := suPodExports("pod-city", "/city", session)
	for _, want := range []string{"export " + remoteStageControllerEnv + "=", "export " + remoteStageCityEnv + "=", userScript} {
		if !strings.Contains(withCity, want) {
			t.Fatalf("suPodExports with a controller city = %q; want it to contain %q", withCity, want)
		}
	}
	// Respawn names no environment of its own: the relaunched pane inherits
	// the server environment the entrypoint started, marker included.
	respawn := strings.Join(buildRespawnCommand(cfg), " ")
	if strings.Contains(respawn, PodSessionEnv) {
		t.Fatalf("respawn command rewrites %s; want the server environment preserved: %s", PodSessionEnv, respawn)
	}
}
