package k8s

import (
	"encoding/base64"
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

func TestBuildPodSecretProjectionDefaultsAndOverrides(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		p := newProviderWithOps(newFakeK8sOps())
		pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
		if err != nil {
			t.Fatal(err)
		}
		if !hasSecretEnvRef(pod.Spec.Containers[0].Env, "GITHUB_TOKEN", "git-credentials", "token") {
			t.Fatal("default GITHUB_TOKEN SecretKeyRef missing")
		}
		if !hasSecretVolume(pod, "claude-credentials", "/tmp/claude-secret") {
			t.Fatal("default claude-credentials read-only mount missing")
		}
	})
	t.Run("custom categories replace independently", func(t *testing.T) {
		p := newProviderWithOps(newFakeK8sOps())
		p.secretEnv = []runtime.K8sSecretEnv{
			{Name: "GITHUB_TOKEN", Secret: "git-auth", Key: "token"},
			{Name: "SERVICE_TOKEN", Secret: "service-auth", Key: "credential"},
		}
		p.secretMounts = []runtime.K8sSecretMount{{Secret: "app-auth", MountPath: "/var/run/app-auth"}}
		pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []struct{ name, secret, key string }{
			{"GITHUB_TOKEN", "git-auth", "token"}, {"SERVICE_TOKEN", "service-auth", "credential"},
		} {
			if !hasSecretEnvRef(pod.Spec.Containers[0].Env, want.name, want.secret, want.key) {
				t.Errorf("SecretKeyRef %s -> %s/%s missing", want.name, want.secret, want.key)
			}
		}
		if hasSecretEnvRef(pod.Spec.Containers[0].Env, "GITHUB_TOKEN", "git-credentials", "token") {
			t.Fatal("legacy env projection was retained with custom list")
		}
		if !hasSecretVolume(pod, "app-auth", "/var/run/app-auth") || hasSecretVolume(pod, "claude-credentials", "/tmp/claude-secret") {
			t.Fatal("custom mount replacement incorrect")
		}
	})
	t.Run("custom env retains default mount", func(t *testing.T) {
		p := newProviderWithOps(newFakeK8sOps())
		p.secretEnv = []runtime.K8sSecretEnv{{Name: "GITEA_TOKEN", Secret: "gitea-auth", Key: "token"}}
		pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
		if err != nil {
			t.Fatal(err)
		}
		if !hasSecretVolume(pod, "claude-credentials", "/tmp/claude-secret") {
			t.Fatal("custom env projection removed the default mount")
		}
	})
	t.Run("custom mount retains default env", func(t *testing.T) {
		p := newProviderWithOps(newFakeK8sOps())
		p.secretMounts = []runtime.K8sSecretMount{{Secret: "service-files", MountPath: "/run/service"}}
		pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
		if err != nil {
			t.Fatal(err)
		}
		if !hasSecretEnvRef(pod.Spec.Containers[0].Env, "GITHUB_TOKEN", "git-credentials", "token") {
			t.Fatal("custom mount projection removed the default env")
		}
	})
}

func TestBuildPodRejectsMalformedSecretProjection(t *testing.T) {
	tests := []struct {
		name string
		p    *Provider
		want string
	}{
		{"blank env name", &Provider{secretEnv: []runtime.K8sSecretEnv{{Secret: "s", Key: "k"}}}, "name"},
		{"invalid env name", &Provider{secretEnv: []runtime.K8sSecretEnv{{Name: "bad-name", Secret: "s", Key: "k"}}}, "environment variable"},
		{"reserved worker marker", &Provider{secretEnv: []runtime.K8sSecretEnv{{Name: "GC_K8S_WORKER_POD", Secret: "s", Key: "k"}}}, "reserved for the Kubernetes worker marker"},
		{"duplicate env name", &Provider{secretEnv: []runtime.K8sSecretEnv{{Name: "TOKEN", Secret: "s", Key: "k"}, {Name: "TOKEN", Secret: "s2", Key: "k"}}}, "duplicated"},
		{"blank mount secret", &Provider{secretMounts: []runtime.K8sSecretMount{{MountPath: "/secret"}}}, "secret"},
		{"relative mount path", &Provider{secretMounts: []runtime.K8sSecretMount{{Secret: "s", MountPath: "secret"}}}, "absolute"},
		{"duplicate mount path", &Provider{secretMounts: []runtime.K8sSecretMount{{Secret: "s", MountPath: "/secret"}, {Secret: "s2", MountPath: "/secret"}}}, "duplicated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProviderWithOps(newFakeK8sOps())
			p.secretEnv, p.secretMounts = tt.p.secretEnv, tt.p.secretMounts
			_, err := buildPod("test-session", runtime.Config{}, p)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("buildPod error = %v, want text %q", err, tt.want)
			}
		})
	}
}

func hasSecretEnvRef(env []corev1.EnvVar, name, secret, key string) bool {
	for _, item := range env {
		if item.Name == name && item.Value == "" && item.ValueFrom != nil && item.ValueFrom.SecretKeyRef != nil {
			ref := item.ValueFrom.SecretKeyRef
			return ref.Name == secret && ref.Key == key && ref.Optional != nil && *ref.Optional
		}
	}
	return false
}

func hasSecretVolume(pod *corev1.Pod, secret, mountPath string) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.Secret == nil || volume.Secret.SecretName != secret || volume.Secret.Optional == nil || !*volume.Secret.Optional {
			continue
		}
		for _, mount := range pod.Spec.Containers[0].VolumeMounts {
			if mount.Name == volume.Name && mount.MountPath == mountPath && mount.ReadOnly {
				return true
			}
		}
	}
	return false
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

// perBeadWorkDirConfig is a pool/workflow worker's runtime config: WorkDir is a
// per-bead directory under the rig (<rig>/<beadID>-<slug>) that nothing has
// created yet.
func perBeadWorkDirConfig() runtime.Config {
	return runtime.Config{
		Command: "/bin/bash",
		WorkDir: "/city/rigs/testrig/tr-abc-slug",
		Env:     map[string]string{"GC_CITY": "/city"},
	}
}

const perBeadPodWorkDir = "/workspace/rigs/testrig/tr-abc-slug"

func TestBuildPod_UsesRestrictedSecurityWithoutPinningIdentity(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	pod, err := buildPod("test-session", perBeadWorkDirConfig(), p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	podSecurity := pod.Spec.SecurityContext
	if podSecurity == nil {
		t.Fatal("pod securityContext is nil")
	}
	if podSecurity.RunAsNonRoot == nil || !*podSecurity.RunAsNonRoot {
		t.Errorf("pod RunAsNonRoot = %v, want true", podSecurity.RunAsNonRoot)
	}
	if podSecurity.RunAsUser != nil || podSecurity.RunAsGroup != nil || podSecurity.FSGroup != nil {
		t.Errorf("pod identity is pinned by default: runAsUser=%v runAsGroup=%v fsGroup=%v", podSecurity.RunAsUser, podSecurity.RunAsGroup, podSecurity.FSGroup)
	}
	if podSecurity.SeccompProfile == nil || podSecurity.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod seccompProfile = %v, want RuntimeDefault", podSecurity.SeccompProfile)
	}

	assertRestrictedContainer := func(name string, security *corev1.SecurityContext) {
		t.Helper()
		if security == nil {
			t.Fatalf("%s securityContext is nil", name)
		}
		if security.RunAsNonRoot == nil || !*security.RunAsNonRoot {
			t.Errorf("%s RunAsNonRoot = %v, want true", name, security.RunAsNonRoot)
		}
		if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation {
			t.Errorf("%s AllowPrivilegeEscalation = %v, want false", name, security.AllowPrivilegeEscalation)
		}
		if security.Capabilities == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != corev1.Capability("ALL") {
			t.Errorf("%s capabilities = %v, want drop ALL", name, security.Capabilities)
		}
		if security.RunAsUser != nil || security.RunAsGroup != nil {
			t.Errorf("%s pins container identity: runAsUser=%v runAsGroup=%v", name, security.RunAsUser, security.RunAsGroup)
		}
	}
	assertRestrictedContainer("agent", pod.Spec.Containers[0].SecurityContext)
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("len(InitContainers) = %d, want 1", len(pod.Spec.InitContainers))
	}
	assertRestrictedContainer("stage", pod.Spec.InitContainers[0].SecurityContext)
}

func TestBuildPod_AppliesOptionalPodIdentityConfiguration(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	runAsUser, runAsGroup, fsGroup := int64(12345), int64(2000), int64(1001)
	p.podSecurity = PodSecurityConfig{
		RunAsUser:  &runAsUser,
		RunAsGroup: &runAsGroup,
		FSGroup:    &fsGroup,
	}

	pod, err := buildPod("test-session", perBeadWorkDirConfig(), p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	security := pod.Spec.SecurityContext
	if security == nil {
		t.Fatal("pod securityContext is nil")
	}
	if security.RunAsUser == nil || *security.RunAsUser != runAsUser {
		t.Errorf("pod RunAsUser = %v, want %d", security.RunAsUser, runAsUser)
	}
	if security.RunAsGroup == nil || *security.RunAsGroup != runAsGroup {
		t.Errorf("pod RunAsGroup = %v, want %d", security.RunAsGroup, runAsGroup)
	}
	if security.FSGroup == nil || *security.FSGroup != fsGroup {
		t.Errorf("pod FSGroup = %v, want %d", security.FSGroup, fsGroup)
	}
	if len(security.SupplementalGroups) != 1 || security.SupplementalGroups[0] != 0 {
		t.Errorf("pod SupplementalGroups = %v, want [0] while preserving primary GID %d", security.SupplementalGroups, runAsGroup)
	}
	if strings.Contains(strings.Join(pod.Spec.Containers[0].Args, " "), "chown") {
		t.Error("normal non-legacy pod entrypoint contains runtime chown")
	}
}

func TestBuildPod_LinuxUsernameRejectsConfiguredPodIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured PodSecurityConfig
	}{
		{name: "run_as_user", configured: PodSecurityConfig{RunAsUser: int64Ptr(12345)}},
		{name: "run_as_group", configured: PodSecurityConfig{RunAsGroup: int64Ptr(2000)}},
		{name: "fs_group", configured: PodSecurityConfig{FSGroup: int64Ptr(1001)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProviderWithOps(newFakeK8sOps())
			p.podSecurity = tc.configured
			cfg := perBeadWorkDirConfig()
			cfg.Env["LINUX_USERNAME"] = "developer"

			if _, err := buildPod("test-session", cfg, p); err == nil ||
				!strings.Contains(err.Error(), "LINUX_USERNAME") ||
				!strings.Contains(err.Error(), "pod identity") {
				t.Fatalf("buildPod error = %v, want identity conflict", err)
			}
		})
	}
}

func TestBuildPod_LinuxUsernameRejectsEnvironmentPodIdentityOverrides(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		value string
	}{
		{name: "run_as_user", env: "GC_K8S_RUN_AS_USER", value: "12345"},
		{name: "run_as_group", env: "GC_K8S_RUN_AS_GROUP", value: "2000"},
		{name: "fs_group", env: "GC_K8S_FS_GROUP", value: "1001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearSchedulingEnv(t)
			t.Setenv(tc.env, tc.value)
			podIdentity, err := resolvePodSecurityConfig(PodSecurityConfig{})
			if err != nil {
				t.Fatalf("resolvePodSecurityConfig: %v", err)
			}
			p := newProviderWithOps(newFakeK8sOps())
			p.podSecurity = podIdentity
			cfg := perBeadWorkDirConfig()
			cfg.Env["LINUX_USERNAME"] = "developer"

			if _, err := buildPod("test-session", cfg, p); err == nil ||
				!strings.Contains(err.Error(), "LINUX_USERNAME") ||
				!strings.Contains(err.Error(), "pod identity") {
				t.Fatalf("buildPod error = %v, want identity conflict", err)
			}
		})
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}

func TestBuildPod_LinuxUsernameKeepsLegacyRootSetup(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	cfg := perBeadWorkDirConfig()
	cfg.Env["LINUX_USERNAME"] = "developer"

	pod, err := buildPod("test-session", cfg, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	security := pod.Spec.Containers[0].SecurityContext
	if security == nil || security.RunAsUser == nil || *security.RunAsUser != 0 {
		t.Fatalf("legacy Linux username securityContext = %v, want runAsUser 0", security)
	}
	args := strings.Join(pod.Spec.Containers[0].Args, " ")
	for _, want := range []string{"useradd", "/etc/sudoers.d/", "chown -R"} {
		if !strings.Contains(args, want) {
			t.Errorf("legacy Linux username startup is missing %q: %s", want, args)
		}
	}
	startupTokens := shellquote.Split(pod.Spec.Containers[0].Args[0])
	hasSuInvocation := false
	for i := 0; i+3 < len(startupTokens); i++ {
		if startupTokens[i] == "su" && startupTokens[i+1] == "-" &&
			startupTokens[i+2] == "developer" && startupTokens[i+3] == "-c" {
			hasSuInvocation = true
			break
		}
	}
	if !hasSuInvocation {
		t.Errorf("legacy Linux username startup is missing su - developer -c invocation: %v", startupTokens)
	}
}

// TestBuildPod_WorkingDirIsAlwaysAnExistingPath pins that the pod spec never
// names a directory that may not exist yet. The kubelet chdirs into the
// container's WorkingDir before the entrypoint runs, so a per-bead WorkingDir
// is created by the runtime as root:root (containerd) or rejected outright —
// either way no command, including pre_start, gets to create it correctly.
// The workspace root always exists (EmptyDir mount when staged, WORKDIR in the
// prebaked image), so the spec points there and the entrypoint enters the
// per-bead directory itself.
func TestBuildPod_WorkingDirIsAlwaysAnExistingPath(t *testing.T) {
	for _, prebaked := range []bool{false, true} {
		name := "staged"
		if prebaked {
			name = "prebaked"
		}
		t.Run(name, func(t *testing.T) {
			p := newProviderWithOps(newFakeK8sOps())
			p.prebaked = prebaked
			pod, err := buildPod("test-session", perBeadWorkDirConfig(), p)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			if got := pod.Spec.Containers[0].WorkingDir; got != podWorkspaceRoot {
				t.Errorf("WorkingDir = %q, want %q (a path guaranteed to exist)", got, podWorkspaceRoot)
			}
		})
	}
}

// TestBuildPod_EntrypointCreatesAndEntersWorkDir pins that the entrypoint
// creates the per-bead WorkingDir and cds into it, so the agent still starts in
// its own directory. This must hold for prebaked images too: prebaked pods
// mount no shared volume, so an init container physically cannot create a
// directory the main container would see.
func TestBuildPod_EntrypointCreatesAndEntersWorkDir(t *testing.T) {
	for _, prebaked := range []bool{false, true} {
		name := "staged"
		if prebaked {
			name = "prebaked"
		}
		t.Run(name, func(t *testing.T) {
			p := newProviderWithOps(newFakeK8sOps())
			p.prebaked = prebaked
			pod, err := buildPod("test-session", perBeadWorkDirConfig(), p)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			args := strings.Join(pod.Spec.Containers[0].Args, " ")
			quoted := shellquote.Quote(perBeadPodWorkDir)
			if !strings.Contains(args, "mkdir -p "+quoted) {
				t.Errorf("entrypoint should mkdir the per-bead WorkingDir; got: %s", args)
			}
			if !strings.Contains(args, "cd "+quoted) {
				t.Errorf("entrypoint should cd into the per-bead WorkingDir; got: %s", args)
			}
		})
	}
}

// TestBuildPod_EntrypointCreatesWorkDirAsDynamicUser pins the same contract on
// the LINUX_USERNAME path, where root creates and chowns the directory before
// dropping privileges and the tmux session cds into it.
func TestBuildPod_EntrypointCreatesWorkDirAsDynamicUser(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	cfg := perBeadWorkDirConfig()
	cfg.Env["LINUX_USERNAME"] = "gcagent"

	pod, err := buildPod("test-session", cfg, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	args := strings.Join(pod.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "mkdir -p \""+perBeadPodWorkDir+"\"") {
		t.Errorf("entrypoint should mkdir the per-bead WorkingDir as root; got: %s", args)
	}
	if !strings.Contains(args, "cd "+shellquote.Quote(perBeadPodWorkDir)) {
		t.Errorf("tmux session should start in the per-bead WorkingDir; got: %s", args)
	}
}

// TestBuildPod_EntersWorkDirAfterStagingAndBeforePreStart pins the ordering of
// the entrypoint, which two silent regressions depend on. Entering the work dir
// must happen after the staging wait, or the shell sits in a subdirectory of a
// workspace that is still being written. And it must happen before pre_start,
// because pre_start used to run in the container's WorkingDir — which was the
// per-bead dir — and commands there may use relative paths.
func TestBuildPod_EntersWorkDirAfterStagingAndBeforePreStart(t *testing.T) {
	for _, username := range []string{"", "gcagent"} {
		name := "no-dynamic-user"
		if username != "" {
			name = "dynamic-user"
		}
		t.Run(name, func(t *testing.T) {
			p := newProviderWithOps(newFakeK8sOps())
			cfg := perBeadWorkDirConfig()
			cfg.PreStart = []string{"echo pre-start-marker"}
			if username != "" {
				cfg.Env["LINUX_USERNAME"] = username
			}
			pod, err := buildPod("test-session", cfg, p)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			args := strings.Join(pod.Spec.Containers[0].Args, " ")

			stagingWait := strings.Index(args, ".gc-workspace-ready")
			enter := strings.Index(args, "cd "+shellquote.Quote(perBeadPodWorkDir))
			// pre_start commands are base64-encoded into the entrypoint.
			preStart := strings.Index(args, base64.StdEncoding.EncodeToString([]byte("echo pre-start-marker")))

			if stagingWait < 0 || enter < 0 || preStart < 0 {
				t.Fatalf("entrypoint missing a stage (wait=%d enter=%d preStart=%d): %s",
					stagingWait, enter, preStart, args)
			}
			if enter < stagingWait {
				t.Errorf("entering the work dir must come after the staging wait; got: %s", args)
			}
			if preStart < enter {
				t.Errorf("pre_start must run after entering the work dir; got: %s", args)
			}
		})
	}
}

// TestBuildPod_InitContainerOnlyWaitsForStaging pins that the staging init
// container is back to a single responsibility — waiting for the controller to
// finish staging. Creating the WorkingDir there only ever worked for staged,
// non-prebaked pods; the entrypoint now owns it for every topology.
func TestBuildPod_InitContainerOnlyWaitsForStaging(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	pod, err := buildPod("test-session", perBeadWorkDirConfig(), p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("len(InitContainers) = %d, want 1", len(pod.Spec.InitContainers))
	}
	cmd := strings.Join(pod.Spec.InitContainers[0].Command, " ")
	if strings.Contains(cmd, "mkdir") {
		t.Errorf("init container should not create the WorkingDir; got: %s", cmd)
	}
	if !strings.Contains(cmd, ".gc-ready") {
		t.Errorf("init container should wait for staging; got: %s", cmd)
	}
}
