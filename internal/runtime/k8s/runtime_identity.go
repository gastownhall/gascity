package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	providerRuntimeFingerprintVersion = "k8s-v3"

	providerRuntimeFingerprintAnnotation        = "gascity.dev/provider-runtime-fingerprint"
	providerRuntimeFingerprintVersionAnnotation = "gascity.dev/provider-runtime-fingerprint-version"
	providerRuntimeImageAnnotation              = "gascity.dev/provider-runtime-image"
	providerRuntimeProviderAnnotation           = "gascity.dev/provider"
	providerRuntimeClaudeSecretName             = "claude-credentials"
	providerRuntimeGitSecretName                = "git-credentials"
)

type runtimeIdentitySpec struct {
	Provider              string                 `json:"provider"`
	Image                 string                 `json:"image"`
	ShareProcessNamespace bool                   `json:"share_process_namespace"`
	LaunchMaterialMode    string                 `json:"launch_material_mode"`
	InitImage             string                 `json:"init_image,omitempty"`
	ServiceAccount        string                 `json:"service_account,omitempty"`
	Resources             runtimeResourceSpec    `json:"resources"`
	WorkspaceMode         string                 `json:"workspace_mode"`
	WorkspacePVC          string                 `json:"workspace_pvc,omitempty"`
	WorkspaceRoot         string                 `json:"workspace_root,omitempty"`
	WorkspaceReadyMarkers []string               `json:"workspace_ready_markers,omitempty"`
	CredentialSecrets     []runtimeSecretSpec    `json:"credential_secrets,omitempty"`
	CredentialEnv         []runtimeSecretEnvSpec `json:"credential_env,omitempty"`
	ProviderHome          string                 `json:"provider_home"`
	LinuxUsername         string                 `json:"linux_username,omitempty"`
	NodeSelector          map[string]string      `json:"node_selector,omitempty"`
	Tolerations           []corev1.Toleration    `json:"tolerations,omitempty"`
	Affinity              *corev1.Affinity       `json:"affinity,omitempty"`
	PriorityClassName     string                 `json:"priority_class_name,omitempty"`
	ManagedDoltEnv        map[string]string      `json:"managed_dolt_env,omitempty"`
}

type runtimeResourceSpec struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type runtimeSecretSpec struct {
	Name       string `json:"name"`
	SecretName string `json:"secret_name"`
	Optional   bool   `json:"optional"`
}

type runtimeSecretEnvSpec struct {
	Name       string `json:"name"`
	SecretName string `json:"secret_name"`
	Key        string `json:"key"`
	Optional   bool   `json:"optional"`
}

// DesiredProviderRuntimeIdentity returns the deterministic provider-substrate
// identity for pods this provider would create for cfg.
func (p *Provider) DesiredProviderRuntimeIdentity(_ context.Context, _ string, cfg runtime.Config) (runtime.ProviderRuntimeIdentity, error) {
	return p.desiredProviderRuntimeIdentity(cfg)
}

// ObserveRuntimeCompatibility classifies the currently visible pod, if any,
// against the desired provider-runtime identity.
func (p *Provider) ObserveRuntimeCompatibility(ctx context.Context, name string, cfg runtime.Config) (runtime.CompatibilityObservation, error) {
	compat, _, err := p.observeRuntimeCompatibility(ctx, name, cfg)
	return compat, err
}

func (p *Provider) observeRuntimeCompatibility(ctx context.Context, name string, cfg runtime.Config) (runtime.CompatibilityObservation, *corev1.Pod, error) {
	desired, err := p.desiredProviderRuntimeIdentity(cfg)
	if err != nil {
		return runtime.CompatibilityObservation{}, nil, err
	}
	label := SanitizeLabel(name)
	pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil {
		return runtime.CompatibilityObservation{}, nil, err
	}
	if len(pods) == 0 {
		return runtime.CompatibilityObservation{
			Supported:  true,
			Exists:     false,
			Compatible: true,
			Desired:    desired,
			Reason:     "absent",
		}, nil, nil
	}
	pod := &pods[0]
	compat := p.runtimeCompatibilityForPod(ctx, pod, desired)
	return compat, pod, nil
}

func (p *Provider) runtimeCompatibilityForPod(ctx context.Context, pod *corev1.Pod, desired runtime.ProviderRuntimeIdentity) runtime.CompatibilityObservation {
	running := pod != nil && pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning
	alive := false
	if running {
		if _, err := p.ops.execInPod(ctx, pod.Name, "agent", []string{"tmux", "has-session", "-t", tmuxSession}, nil); err == nil {
			alive = true
		}
	}
	current := providerRuntimeIdentityFromPod(pod)
	compat := runtime.CompatibilityObservation{
		Supported:                 true,
		Exists:                    pod != nil,
		Running:                   running,
		Alive:                     alive,
		Compatible:                true,
		Desired:                   desired,
		Current:                   current,
		Reason:                    "compatible",
		SafeToReplaceWithoutDrain: !alive,
	}
	if pod == nil {
		compat.Compatible = true
		compat.Reason = "absent"
		return compat
	}
	actualImage := podAgentImage(pod)
	annotatedImage := strings.TrimSpace(pod.Annotations[providerRuntimeImageAnnotation])
	switch {
	case strings.TrimSpace(current.Fingerprint) == "":
		compat.Compatible = false
		compat.Reason = "missing-runtime-identity"
	case strings.TrimSpace(current.Version) != desired.Version:
		compat.Compatible = false
		compat.Reason = "runtime-version-mismatch"
	case strings.TrimSpace(current.Fingerprint) != desired.Fingerprint:
		compat.Compatible = false
		compat.Reason = "runtime-fingerprint-mismatch"
	case annotatedImage != "" && annotatedImage != p.image:
		compat.Compatible = false
		compat.Reason = "runtime-image-mismatch"
	case actualImage != "" && actualImage != p.image:
		compat.Compatible = false
		compat.Reason = "runtime-image-mismatch"
	}
	return compat
}

func (p *Provider) desiredProviderRuntimeIdentity(cfg runtime.Config) (runtime.ProviderRuntimeIdentity, error) {
	spec, err := p.runtimeIdentitySpec(cfg)
	if err != nil {
		return runtime.ProviderRuntimeIdentity{}, err
	}
	breakdown, err := json.Marshal(spec)
	if err != nil {
		return runtime.ProviderRuntimeIdentity{}, fmt.Errorf("marshaling provider runtime identity: %w", err)
	}
	sum := sha256.Sum256(breakdown)
	return runtime.ProviderRuntimeIdentity{
		Fingerprint: fmt.Sprintf("%s:%x", providerRuntimeFingerprintVersion, sum[:]),
		Version:     providerRuntimeFingerprintVersion,
		Breakdown:   string(breakdown),
	}, nil
}

func (p *Provider) runtimeIdentitySpec(cfg runtime.Config) (runtimeIdentitySpec, error) {
	ctrlCity := controllerCityPath(cfg.Env)
	staging := !p.prebaked && needsStaging(cfg, ctrlCity)
	workspaceMode := "staged"
	if p.prebaked {
		workspaceMode = "prebaked"
	}
	if p.usesPersistentWorkspace() {
		staging = false
		workspaceMode = "persistent-pvc"
	}
	var initImage string
	if staging {
		initImage = p.image
	}
	var readyMarkers []string
	if !p.prebaked && !p.usesPersistentWorkspace() {
		readyMarkers = []string{"/workspace/.gc-workspace-ready", "/workspace/.gc-ready"}
	}
	managedDoltEnv, err := projectedPodDoltEnv(cfg.Env, p.managedServiceHost, p.managedServicePort)
	if err != nil {
		return runtimeIdentitySpec{}, err
	}
	return runtimeIdentitySpec{
		Provider:              "k8s",
		Image:                 p.image,
		ShareProcessNamespace: true,
		LaunchMaterialMode:    "staged-files",
		InitImage:             initImage,
		ServiceAccount:        p.serviceAccount,
		Resources:             providerRuntimeResources(p),
		WorkspaceMode:         workspaceMode,
		WorkspacePVC:          strings.TrimSpace(p.workspacePVC),
		WorkspaceRoot:         workspaceIdentityRoot(p),
		WorkspaceReadyMarkers: readyMarkers,
		CredentialSecrets: []runtimeSecretSpec{
			{Name: "claude-config", SecretName: providerRuntimeClaudeSecretName, Optional: true},
		},
		CredentialEnv:     providerRuntimeCredentialEnv(cfg.Env),
		ProviderHome:      providerRuntimeHome(cfg.Env),
		LinuxUsername:     strings.TrimSpace(cfg.Env["LINUX_USERNAME"]),
		NodeSelector:      cloneStringMap(p.nodeSelector),
		Tolerations:       cloneTolerations(p.tolerations),
		Affinity:          cloneAffinity(p.affinity),
		PriorityClassName: p.priorityClassName,
		ManagedDoltEnv:    managedDoltEnv,
	}, nil
}

func providerRuntimeCredentialEnv(cfgEnv map[string]string) []runtimeSecretEnvSpec {
	env := []runtimeSecretEnvSpec{
		{Name: "GITHUB_TOKEN", SecretName: providerRuntimeGitSecretName, Key: "token", Optional: true},
	}
	if strings.TrimSpace(cfgEnv["CLAUDE_CODE_OAUTH_TOKEN"]) == "" {
		env = append(env, runtimeSecretEnvSpec{
			Name:       "CLAUDE_CODE_OAUTH_TOKEN",
			SecretName: providerRuntimeClaudeSecretName,
			Key:        "CLAUDE_CODE_OAUTH_TOKEN",
			Optional:   true,
		})
	}
	return env
}

func providerRuntimeHome(cfgEnv map[string]string) string {
	if linuxUser := strings.TrimSpace(cfgEnv["LINUX_USERNAME"]); linuxUser != "" {
		return "/home/" + linuxUser
	}
	return "/home/gcagent"
}

func providerRuntimeResources(p *Provider) runtimeResourceSpec {
	resources := runtimeResourceSpec{}
	if p.cpuRequest != "" || p.memRequest != "" {
		resources.Requests = map[string]string{}
		if p.cpuRequest != "" {
			resources.Requests["cpu"] = p.cpuRequest
		}
		if p.memRequest != "" {
			resources.Requests["memory"] = p.memRequest
		}
	}
	if p.cpuLimit != "" || p.memLimit != "" {
		resources.Limits = map[string]string{}
		if p.cpuLimit != "" {
			resources.Limits["cpu"] = p.cpuLimit
		}
		if p.memLimit != "" {
			resources.Limits["memory"] = p.memLimit
		}
	}
	return resources
}

func workspaceIdentityRoot(p *Provider) string {
	if !p.usesPersistentWorkspace() {
		return ""
	}
	return p.podWorkspaceRoot()
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneAffinity(in *corev1.Affinity) *corev1.Affinity {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

func providerRuntimeIdentityFromPod(pod *corev1.Pod) runtime.ProviderRuntimeIdentity {
	if pod == nil {
		return runtime.ProviderRuntimeIdentity{}
	}
	return runtime.ProviderRuntimeIdentity{
		Fingerprint: strings.TrimSpace(pod.Annotations[providerRuntimeFingerprintAnnotation]),
		Version:     strings.TrimSpace(pod.Annotations[providerRuntimeFingerprintVersionAnnotation]),
	}
}

func podAgentImage(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == "agent" {
			return container.Image
		}
	}
	if len(pod.Spec.Containers) == 0 {
		return ""
	}
	return pod.Spec.Containers[0].Image
}
