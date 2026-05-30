package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestBuildPod_NodeSelector(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.nodeSelector = map[string]string{"workload": "gc-agents"}
	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if pod.Spec.NodeSelector["workload"] != "gc-agents" {
		t.Errorf("NodeSelector[workload] = %q, want \"gc-agents\"", pod.Spec.NodeSelector["workload"])
	}
}

func TestBuildPod_Tolerations(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.tolerations = []corev1.Toleration{{
		Key: "gc-agents", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
	}}
	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if len(pod.Spec.Tolerations) != 1 {
		t.Fatalf("len(Tolerations) = %d, want 1", len(pod.Spec.Tolerations))
	}
	if pod.Spec.Tolerations[0].Key != "gc-agents" {
		t.Errorf("Toleration.Key = %q, want \"gc-agents\"", pod.Spec.Tolerations[0].Key)
	}
}

func TestBuildPod_Affinity(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "node-type", Operator: corev1.NodeSelectorOpIn, Values: []string{"gpu"},
					}},
				}},
			},
		},
	}
	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if pod.Spec.Affinity == nil {
		t.Fatal("Affinity is nil")
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		t.Fatal("NodeAffinity is nil")
	}
	expressions := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions
	if expressions[0].Values[0] != "gpu" {
		t.Fatalf("affinity value = %q, want gpu", expressions[0].Values[0])
	}
}

func TestBuildPod_PriorityClassName(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.priorityClassName = "gc-agent-high"
	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if pod.Spec.PriorityClassName != "gc-agent-high" {
		t.Errorf("PriorityClassName = %q, want \"gc-agent-high\"", pod.Spec.PriorityClassName)
	}
}

func TestBuildPod_NoSchedulingFields_NoBehaviorChange(t *testing.T) {
	// Zero-value scheduling fields must not alter default pod behavior.
	p := newProviderWithOps(newFakeK8sOps())
	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if pod.Spec.NodeSelector != nil {
		t.Errorf("NodeSelector should be nil when not set")
	}
	if len(pod.Spec.Tolerations) != 0 {
		t.Errorf("Tolerations should be empty when not set")
	}
	if pod.Spec.Affinity != nil {
		t.Errorf("Affinity should be nil when not set")
	}
	if pod.Spec.PriorityClassName != "" {
		t.Errorf("PriorityClassName should be empty when not set")
	}
}

func TestBuildPod_ClonesSchedulingFields(t *testing.T) {
	seconds := int64(30)
	p := newProviderWithOps(newFakeK8sOps())
	p.nodeSelector = map[string]string{"workload": "gc-agents"}
	p.tolerations = []corev1.Toleration{{
		Key:               "gc-agents",
		Operator:          corev1.TolerationOpExists,
		Effect:            corev1.TaintEffectNoSchedule,
		TolerationSeconds: &seconds,
	}}
	p.affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "node-type", Operator: corev1.NodeSelectorOpIn, Values: []string{"gpu"},
					}},
				}},
			},
		},
	}

	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	pod.Spec.NodeSelector["workload"] = "changed"
	pod.Spec.Tolerations[0].Key = "changed"
	pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0] = "changed"

	if p.nodeSelector["workload"] != "gc-agents" {
		t.Fatalf("provider nodeSelector mutated to %q", p.nodeSelector["workload"])
	}
	if p.tolerations[0].Key != "gc-agents" {
		t.Fatalf("provider toleration key mutated to %q", p.tolerations[0].Key)
	}
	values := p.affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values
	if values[0] != "gpu" {
		t.Fatalf("provider affinity value mutated to %q", values[0])
	}
}

func TestBuildPod_WaitsForEitherWorkspaceReadyMarker(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	cfg := runtime.Config{Command: "/bin/bash"}
	pod, err := buildPod("test-session", cfg, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	entrypoint := buildPodLaunchMaterial(cfg, p).Entrypoint
	if !strings.Contains(entrypoint, `/workspace/.gc-workspace-ready ] && [ ! -f /workspace/.gc-ready`) {
		t.Fatalf("entrypoint does not accept both workspace ready markers: %s", entrypoint)
	}
	if got := pod.Spec.Containers[0].Command; len(got) != 2 || got[0] != "/bin/sh" || got[1] != podLaunchEntrypoint {
		t.Fatalf("container command = %#v, want bounded launch script command", got)
	}
	if len(pod.Spec.Containers[0].Args) != 0 {
		t.Fatalf("container args = %#v, want none", pod.Spec.Containers[0].Args)
	}
}

func TestBuildPodEntrypointLaunchesTmuxFromWorkDirAfterPreStart(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	cfg := runtime.Config{
		Command:  "codex",
		WorkDir:  "/city/rigs/frontend",
		PreStart: []string{"rm -rf /workspace/rigs/frontend && mkdir -p /workspace/rigs/frontend"},
		Env: map[string]string{
			"GC_CITY": "/city",
		},
	}
	pod, err := buildPod("test-session", cfg, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	if len(pod.Spec.Containers[0].Args) != 0 {
		t.Fatalf("container args = %v, want none", pod.Spec.Containers[0].Args)
	}
	if got := pod.Spec.Containers[0].WorkingDir; got != podEntrypointWorkDir {
		t.Fatalf("container workingDir = %q, want stable entrypoint dir %q", got, podEntrypointWorkDir)
	}
	material := buildPodLaunchMaterial(cfg, p)
	entrypoint := material.Entrypoint
	preStartIdx := strings.Index(entrypoint, "for __gc_pre_start")
	launchIdx := strings.Index(entrypoint, "cd '/workspace/rigs/frontend' && tmux new-session")
	if preStartIdx == -1 {
		t.Fatalf("entrypoint does not run staged pre_start scripts: %s", entrypoint)
	}
	if launchIdx == -1 {
		t.Fatalf("entrypoint does not cd to projected workdir before tmux: %s", entrypoint)
	}
	if preStartIdx > launchIdx {
		t.Fatalf("entrypoint cd happens before pre_start; want pre_start then final cd: %s", entrypoint)
	}
	if len(material.PreStart) != 1 || material.PreStart[0] != "rm -rf /workspace/rigs/frontend && mkdir -p /workspace/rigs/frontend" {
		t.Fatalf("pre_start material = %#v", material.PreStart)
	}
	if !strings.Contains(material.Agent, "exec sh -c 'codex'") {
		t.Fatalf("agent launch script missing command:\n%s", material.Agent)
	}
}

func TestBuildPodEnablesSharedProcessNamespace(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	pod, err := buildPod("test-session", runtime.Config{
		Command: "codex",
		WorkDir: "/city/rigs/frontend",
		Env: map[string]string{
			"GC_CITY": "/city",
		},
	}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if pod.Spec.ShareProcessNamespace == nil || !*pod.Spec.ShareProcessNamespace {
		t.Fatalf("ShareProcessNamespace = %#v, want true", pod.Spec.ShareProcessNamespace)
	}
}

func TestBuildPodEntrypointDeliversPromptSuffixInLaunchCommand(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	prompt := "Full startup prompt\nwith quoted ' text"
	cfg := runtime.Config{
		Command:      "codex --model gpt-5.5",
		WorkDir:      "/city/rigs/frontend",
		PromptSuffix: shellquote.Quote(prompt),
		PromptFlag:   "--prompt",
		Env: map[string]string{
			"GC_CITY": "/city",
		},
	}
	pod, err := buildPod("test-session", cfg, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	if strings.Contains(strings.Join(pod.Spec.Containers[0].Command, " "), prompt) || strings.Contains(strings.Join(pod.Spec.Containers[0].Args, " "), prompt) {
		t.Fatalf("pod argv leaked prompt text: command=%#v args=%#v", pod.Spec.Containers[0].Command, pod.Spec.Containers[0].Args)
	}
	material := buildPodLaunchMaterial(cfg, p)
	if material.Prompt != prompt || !material.HasPrompt {
		t.Fatalf("launch prompt material = (%q, %v), want raw prompt", material.Prompt, material.HasPrompt)
	}
	for _, want := range []string{
		podLaunchPromptPath,
		"rm -f '/gc-launch/prompt.txt'",
		`exec sh -c 'codex --model gpt-5.5 --prompt "$1"' sh "$__gc_prompt"`,
	} {
		if !strings.Contains(material.Agent, want) {
			t.Fatalf("agent launch script missing %q:\n%s", want, material.Agent)
		}
	}
}

func TestBuildPodDoesNotEmbedLargeLaunchMaterialInPodArgs(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	prompt := strings.Repeat("large prompt ", 20000)
	command := "codex --model gpt-5.5 " + strings.Repeat("--flag ", 2000)
	preStart := "printf %s " + strings.Repeat("x", 20000)
	cfg := runtime.Config{
		Command:      command,
		WorkDir:      "/city/rigs/frontend",
		PromptSuffix: shellquote.Quote(prompt),
		PromptFlag:   "--prompt",
		PreStart:     []string{preStart},
		Env: map[string]string{
			"GC_CITY": "/city",
		},
	}
	pod, err := buildPod("test-session", cfg, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	argv := strings.Join(append(append([]string{}, pod.Spec.Containers[0].Command...), pod.Spec.Containers[0].Args...), "\x00")
	if len(argv) > 4096 {
		t.Fatalf("pod argv length = %d, want bounded under 4096: %#v %#v", len(argv), pod.Spec.Containers[0].Command, pod.Spec.Containers[0].Args)
	}
	for _, forbidden := range []string{prompt, command, preStart} {
		if strings.Contains(argv, forbidden) {
			t.Fatalf("pod argv leaked large launch material")
		}
	}
	material := buildPodLaunchMaterial(cfg, p)
	if material.Prompt != prompt || material.Agent == "" || len(material.PreStart) != 1 {
		t.Fatalf("launch material did not preserve large inputs: %#v", material)
	}
}

func TestBuildPodEntrypointTransfersPromptFileOwnershipForDynamicUser(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	pod, err := buildPod("test-session", runtime.Config{
		Command:      "codex",
		WorkDir:      "/city/rigs/frontend",
		PromptSuffix: shellquote.Quote("full startup prompt"),
		Env: map[string]string{
			"GC_CITY":        "/city",
			"LINUX_USERNAME": "agentuser",
		},
	}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if len(pod.Spec.InitContainers) == 0 || pod.Spec.InitContainers[0].SecurityContext == nil || pod.Spec.InitContainers[0].SecurityContext.RunAsUser == nil || *pod.Spec.InitContainers[0].SecurityContext.RunAsUser != 0 {
		t.Fatalf("dynamic-user launch init container must run as root to stage/chown launch material: %#v", pod.Spec.InitContainers)
	}

	material := buildPodLaunchMaterial(runtime.Config{
		Command:      "codex",
		WorkDir:      "/city/rigs/frontend",
		PromptSuffix: shellquote.Quote("full startup prompt"),
		Env: map[string]string{
			"GC_CITY":        "/city",
			"LINUX_USERNAME": "agentuser",
		},
	}, p)
	entrypoint := material.Entrypoint
	if want := "chown -R 'agentuser' '/gc-launch'"; !strings.Contains(entrypoint, want) {
		t.Fatalf("entrypoint does not transfer launch material ownership with %q:\n%s", want, entrypoint)
	}
	if want := `su -m 'agentuser' -c`; !strings.Contains(entrypoint, want) {
		t.Fatalf("entrypoint does not drop to dynamic user with %q:\n%s", want, entrypoint)
	}
	if want := "rm -f '/gc-launch/prompt.txt'"; !strings.Contains(material.Agent, want) {
		t.Fatalf("agent launch script does not remove prompt file with %q:\n%s", want, material.Agent)
	}
}

func TestBuildPodEntrypointPreservesRuntimeIdentityForDynamicUser(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	pod, err := buildPod("test-session", runtime.Config{
		Command: "/bin/bash",
		WorkDir: "/city/.gc/agents/worker",
		Env: map[string]string{
			"GC_AGENT":          "worker",
			"GC_ALIAS":          "worker",
			"GC_CITY":           "/city",
			"GC_INSTANCE_TOKEN": "tok-worker",
			"GC_RUNTIME_EPOCH":  "12",
			"GC_SESSION_ID":     "gc-session-pending",
			"LINUX_USERNAME":    "agentuser",
		},
	}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	material := buildPodLaunchMaterial(runtime.Config{
		Command: "/bin/bash",
		WorkDir: "/city/.gc/agents/worker",
		Env: map[string]string{
			"GC_AGENT":          "worker",
			"GC_ALIAS":          "worker",
			"GC_CITY":           "/city",
			"GC_INSTANCE_TOKEN": "tok-worker",
			"GC_RUNTIME_EPOCH":  "12",
			"GC_SESSION_ID":     "gc-session-pending",
			"LINUX_USERNAME":    "agentuser",
		},
	}, p)
	entrypoint := material.Entrypoint
	if want := `export HOME='/home/agentuser' USER='agentuser' LOGNAME='agentuser' SHELL="/bin/bash"`; !strings.Contains(entrypoint, want) {
		t.Fatalf("entrypoint does not prepare preserved user env with %q:\n%s", want, entrypoint)
	}
	if want := `su -m 'agentuser' -c`; !strings.Contains(entrypoint, want) {
		t.Fatalf("entrypoint must preserve container env when switching users with %q:\n%s", want, entrypoint)
	}
	if strings.Contains(entrypoint, `su - agentuser -c`) {
		t.Fatalf("entrypoint uses login su, which drops runtime identity env:\n%s", entrypoint)
	}

	envMap := map[string]string{}
	for _, item := range pod.Spec.Containers[0].Env {
		envMap[item.Name] = item.Value
	}
	for key, want := range map[string]string{
		"GC_SESSION_ID":     "gc-session-pending",
		"GC_INSTANCE_TOKEN": "tok-worker",
		"GC_RUNTIME_EPOCH":  "12",
	} {
		if got := envMap[key]; got != want {
			t.Fatalf("%s env = %q, want %q", key, got, want)
		}
	}
}

func TestBuildPodPersistentWorkspacePVCUsesMountedCityRoot(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.workspacePVC = "demo-city-workspace"
	p.workspaceRoot = "/workspace/cities/demo-city"

	cfg := runtime.Config{
		Command:  "gc agent-script --script /host/city/packs/demo/agent.yaml",
		WorkDir:  "/host/city/.gc/worktrees/demo/cartographer",
		PreStart: []string{"test -d /host/city/rigs/demo"},
		Env: map[string]string{
			"GC_AGENT":             "demo/cartographer",
			"GC_CITY":              "/host/city",
			"GC_CITY_PATH":         "/host/city",
			"GC_DIR":               "/host/city/.gc/worktrees/demo/cartographer",
			"GC_RIG_ROOT":          "/host/city/rigs/demo",
			"GC_STORE_ROOT":        "/host/city/rigs/demo",
			"BEADS_DIR":            "/host/city/rigs/demo/.beads",
			"GC_CITY_RUNTIME_DIR":  "/host/city/.gc/runtime",
			"GC_K8S_WORKSPACE_PVC": "demo-city-workspace",
		},
	}

	pod, err := buildPod("test-session", cfg, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != "launch" {
		t.Fatalf("persistent workspace pod should use only launch staging init container: %#v", pod.Spec.InitContainers)
	}

	workspaceMount, ok := volumeMountByName(pod.Spec.Containers[0].VolumeMounts, "workspace")
	if !ok {
		t.Fatal("missing persistent workspace volume mount")
	}
	if workspaceMount.MountPath != "/workspace/cities/demo-city" {
		t.Fatalf("workspace mount path = %q, want /workspace/cities/demo-city", workspaceMount.MountPath)
	}
	workspaceVolume, ok := volumeByName(pod.Spec.Volumes, "workspace")
	if !ok || workspaceVolume.PersistentVolumeClaim == nil {
		t.Fatalf("missing persistent workspace PVC volume: %#v", workspaceVolume)
	}
	if workspaceVolume.PersistentVolumeClaim.ClaimName != "demo-city-workspace" {
		t.Fatalf("workspace claim = %q, want demo-city-workspace", workspaceVolume.PersistentVolumeClaim.ClaimName)
	}
	if _, ok := volumeMountByName(pod.Spec.Containers[0].VolumeMounts, "ws"); ok {
		t.Fatal("persistent workspace pod should not mount staged ws EmptyDir")
	}
	if _, ok := volumeByName(pod.Spec.Volumes, "city"); ok {
		t.Fatal("persistent workspace pod should not create compatibility city EmptyDir")
	}

	material := buildPodLaunchMaterial(cfg, p)
	entrypoint := material.Entrypoint
	if strings.Contains(entrypoint, ".gc-workspace-ready") || strings.Contains(entrypoint, ".gc-ready") {
		t.Fatalf("persistent workspace entrypoint should not wait for staged workspace markers: %s", entrypoint)
	}
	if !strings.Contains(entrypoint, "cd '/workspace/cities/demo-city/.gc/worktrees/demo/cartographer' && tmux new-session") {
		t.Fatalf("entrypoint does not launch from persistent workdir: %s", entrypoint)
	}
	if len(material.PreStart) != 1 || material.PreStart[0] != "test -d /workspace/cities/demo-city/rigs/demo" {
		t.Fatalf("pre_start command was not remapped to persistent workspace root: %#v", material.PreStart)
	}
	if !strings.Contains(material.Agent, "gc agent-script --script /workspace/cities/demo-city/packs/demo/agent.yaml") {
		t.Fatalf("agent command was not remapped to persistent workspace root: %s", material.Agent)
	}

	envMap := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		envMap[e.Name] = e.Value
	}
	want := map[string]string{
		"GC_CITY":             "/workspace/cities/demo-city",
		"GC_CITY_PATH":        "/workspace/cities/demo-city",
		"GC_DIR":              "/workspace/cities/demo-city/.gc/worktrees/demo/cartographer",
		"GC_RIG_ROOT":         "/workspace/cities/demo-city/rigs/demo",
		"GC_STORE_ROOT":       "/workspace/cities/demo-city/rigs/demo",
		"BEADS_DIR":           "/workspace/cities/demo-city/rigs/demo/.beads",
		"GC_CITY_RUNTIME_DIR": "/workspace/cities/demo-city/.gc/runtime",
	}
	for key, wantValue := range want {
		if got := envMap[key]; got != wantValue {
			t.Fatalf("%s = %q, want %q", key, got, wantValue)
		}
	}
}

func volumeMountByName(mounts []corev1.VolumeMount, name string) (corev1.VolumeMount, bool) {
	for _, mount := range mounts {
		if mount.Name == name {
			return mount, true
		}
	}
	return corev1.VolumeMount{}, false
}

func volumeByName(volumes []corev1.Volume, name string) (corev1.Volume, bool) {
	for _, volume := range volumes {
		if volume.Name == name {
			return volume, true
		}
	}
	return corev1.Volume{}, false
}
