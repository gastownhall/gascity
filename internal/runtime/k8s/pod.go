package k8s

import (
	"encoding/base64"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gastownhall/gascity/internal/runtime"
)

// buildPod creates a pod manifest compatible with gc-session-k8s.
// Same labels, annotations, container names, volumes, and tmux-inside-pod
// pattern so mixed-mode migration works.
func buildPod(name string, cfg runtime.Config, p *Provider) (*corev1.Pod, error) {
	podName := SanitizeName(name)
	label := SanitizeLabel(name)
	agentName := cfg.Env["GC_ALIAS"]
	if agentName == "" {
		agentName = cfg.Env["GC_AGENT"]
	}
	if agentName == "" {
		agentName = "unknown"
	}
	agentLabel := SanitizeLabel(agentName)

	// Resolve pod-side working directory.
	// Controller resolves dirs relative to its cityPath; pods use /workspace.
	podWorkDir := "/workspace"
	ctrlCity := cfg.Env["GC_CITY"]
	if ctrlCity == "" {
		ctrlCity = cfg.Env["GC_CITY_PATH"]
	}
	if ctrlCity == "" {
		ctrlCity = cfg.Env["GC_CITY_ROOT"]
	}
	if ctrlCity != "" && cfg.WorkDir != "" && cfg.WorkDir != ctrlCity {
		if rel, ok := strings.CutPrefix(cfg.WorkDir, ctrlCity+"/"); ok {
			podWorkDir = "/workspace/" + rel
		}
	}

	// Build the command the agent runs. Base64-encode to avoid quoting issues.
	agentCmd := cfg.Command
	if agentCmd == "" {
		agentCmd = "/bin/bash"
	}
	// Remap controller-side city path references to pod-side /workspace.
	// The controller expands {{.ConfigDir}} templates using its own city path
	// (e.g. /city/packs/...) but pods have files at /workspace/....
	if ctrlCity != "" {
		agentCmd = strings.ReplaceAll(agentCmd, ctrlCity, "/workspace")
	}
	cmdB64 := base64.StdEncoding.EncodeToString([]byte(agentCmd))

	// Pod entrypoint: wait for workspace ready → pre_start → tmux → keepalive.
	// Each pre_start command is base64-encoded and decoded at runtime to prevent
	// shell metacharacter injection from user-supplied commands.
	var preStartCmds string
	for _, cmd := range cfg.PreStart {
		c := cmd
		if ctrlCity != "" {
			c = strings.ReplaceAll(c, ctrlCity, "/workspace")
		}
		b64 := base64.StdEncoding.EncodeToString([]byte(c))
		preStartCmds += fmt.Sprintf("echo '%s' | base64 -d | sh; ", b64)
	}

	// Dynamic user creation: when LINUX_USERNAME is set, the container starts
	// as root (see securityContext below), creates the user, sets up workspace
	// ownership, then drops privileges via su for the tmux session.
	linuxUsername := cfg.Env["LINUX_USERNAME"]
	var userSetup string
	if linuxUsername != "" {
		userSetup = fmt.Sprintf(
			`id "%s" >/dev/null 2>&1 || useradd -m -s /bin/bash "%s"; `+
				`echo "%s ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/"%s" && chmod 0440 /etc/sudoers.d/"%s"; `+
				`mkdir -p "%s" && chown -R "%s" "%s"; `+
				`export HOME="/home/%s"; `,
			linuxUsername, linuxUsername,
			linuxUsername, linuxUsername, linuxUsername,
			podWorkDir, linuxUsername, podWorkDir,
			linuxUsername,
		)
	}
	credCopy := `mkdir -p $HOME/.claude && cp -rL /tmp/claude-secret/. $HOME/.claude/ 2>/dev/null; git config --global --add safe.directory '*' 2>/dev/null; `
	wsWait := ""
	if !p.prebaked {
		wsWait = `while [ ! -f /workspace/.gc-workspace-ready ]; do sleep 0.5; done; `
	}

	var tmuxCmd string
	if linuxUsername != "" {
		// Run tmux session as the dynamic user via su.
		tmuxCmd = fmt.Sprintf(
			"%s%s%s%sCMD=$(echo '%s' | base64 -d) && "+
				`su - %s -c "cd %s && tmux new-session -d -s %s \"$CMD\" && sleep infinity"`,
			userSetup, credCopy, wsWait, preStartCmds, cmdB64,
			linuxUsername, podWorkDir, tmuxSession,
		)
	} else {
		tmuxCmd = fmt.Sprintf(
			"%s%s%sCMD=$(echo '%s' | base64 -d) && tmux new-session -d -s %s \"$CMD\" && sleep infinity",
			credCopy, wsWait, preStartCmds, cmdB64, tmuxSession,
		)
	}

	// Build environment, remapping K8s-specific vars.
	env := buildPodEnv(cfg.Env, podWorkDir)

	// Build volume mounts for the main container.
	// When prebaked, skip the ws EmptyDir — it would shadow baked image content.
	var mainVolMounts []corev1.VolumeMount
	var volumes []corev1.Volume

	if !p.prebaked {
		mainVolMounts = append(mainVolMounts, corev1.VolumeMount{
			Name: "ws", MountPath: "/workspace",
		})
		volumes = append(volumes, corev1.Volume{
			Name: "ws", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	mainVolMounts = append(mainVolMounts, corev1.VolumeMount{
		Name: "claude-config", MountPath: "/tmp/claude-secret", ReadOnly: true,
	})
	volumes = append(volumes, corev1.Volume{
		Name: "claude-config", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: "claude-credentials",
				Optional:   boolPtr(true),
			},
		},
	})

	// If GC_CITY differs from work_dir, add a city volume (not needed when prebaked).
	if !p.prebaked && ctrlCity != "" && ctrlCity != cfg.WorkDir {
		mainVolMounts = append(mainVolMounts, corev1.VolumeMount{
			Name: "city", MountPath: ctrlCity,
		})
		volumes = append(volumes, corev1.Volume{
			Name:         "city",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	// Resources.
	resources, err := buildResources(p)
	if err != nil {
		return nil, err
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: p.namespace,
			Labels: map[string]string{
				"app":        "gc-agent",
				"gc-session": label,
				"gc-agent":   agentLabel,
			},
			Annotations: map[string]string{
				"gc-session-name": name,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: p.serviceAccount,
			RestartPolicy:      corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "agent",
				Image:           p.image,
				ImagePullPolicy: corev1.PullAlways,
				WorkingDir:      podWorkDir,
				Command:         []string{"/bin/sh", "-c"},
				Args:            []string{tmuxCmd},
				Env:             env,
				Stdin:           true,
				TTY:             true,
				Resources:       resources,
				VolumeMounts:    mainVolMounts,
				SecurityContext: agentSecurityContext(linuxUsername),
			}},
			Volumes: volumes,
		},
	}

	// Add init container when staging is needed (skip when prebaked).
	if !p.prebaked && needsStaging(cfg, ctrlCity) {
		initVolMounts := []corev1.VolumeMount{
			{Name: "ws", MountPath: "/workspace"},
		}
		if ctrlCity != "" && ctrlCity != cfg.WorkDir {
			initVolMounts = append(initVolMounts, corev1.VolumeMount{
				Name: "city", MountPath: "/city-stage",
			})
		}
		pod.Spec.InitContainers = []corev1.Container{{
			Name:            "stage",
			Image:           p.image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"sh", "-c", "while [ ! -f /workspace/.gc-ready ]; do sleep 0.5; done"},
			VolumeMounts:    initVolMounts,
		}}
	}

	return pod, nil
}

// agentSecurityContext returns a container security context.
// When a dynamic linux username is configured, the container starts as root
// (UID 0) so it can create the user at runtime before dropping privileges.
// When no dynamic user is set, returns nil (uses Dockerfile default: gcagent).
func agentSecurityContext(linuxUsername string) *corev1.SecurityContext {
	if linuxUsername == "" {
		return nil
	}
	var rootUID int64
	return &corev1.SecurityContext{
		RunAsUser: &rootUID,
	}
}

// buildPodEnv creates the env var list for the agent container.
// Removes controller-only vars and remaps K8s-specific ones.
func buildPodEnv(cfgEnv map[string]string, podWorkDir string) []corev1.EnvVar {
	// Start with cfg.Env, removing controller-only vars.
	skip := map[string]bool{
		"GC_BEADS":               true,
		"GC_SESSION":             true,
		"GC_EVENTS":              true,
		"GC_DOLT_HOST":           true,
		"GC_DOLT_PORT":           true,
		"BEADS_DOLT_SERVER_HOST": true,
		"BEADS_DOLT_SERVER_PORT": true,
		// Note: GC_DOLT_USER, GC_DOLT_PASSWORD, BEADS_DOLT_SERVER_USER,
		// and BEADS_DOLT_PASSWORD are intentionally NOT stripped — agents
		// need auth credentials to authenticate against the in-cluster
		// Dolt service. Only host/port are stripped and replaced with
		// K8s-specific endpoints.
	}

	var env []corev1.EnvVar
	for k, v := range cfgEnv {
		if skip[k] {
			continue
		}
		val := v
		// Remap city/workdir vars to pod-visible paths.
		switch k {
		case "GC_CITY":
			val = "/workspace"
		case "GC_CITY_PATH", "GC_CITY_ROOT":
			val = "/workspace"
		case "GC_DIR":
			val = podWorkDir
		}
		env = append(env, corev1.EnvVar{Name: k, Value: val})
	}

	// Add tmux session env so agent's tmux provider uses the same session.
	env = append(env, corev1.EnvVar{Name: "GC_TMUX_SESSION", Value: tmuxSession})

	// CLAUDE_CONFIG_DIR: use dynamic username home if LINUX_USERNAME is set,
	// otherwise fall back to the baked-in gcagent user.
	linuxUser := cfgEnv["LINUX_USERNAME"]
	if linuxUser != "" {
		env = append(env, corev1.EnvVar{Name: "CLAUDE_CONFIG_DIR", Value: "/home/" + linuxUser + "/.claude"})
	} else {
		env = append(env, corev1.EnvVar{Name: "CLAUDE_CONFIG_DIR", Value: "/home/gcagent/.claude"})
	}

	// Inject K8s Dolt discovery for agent-side bd init.
	// GC_DOLT_HOST/PORT are stripped (controller-only), so inject K8s-specific
	// defaults that point to the in-cluster Dolt service.
	envMap := make(map[string]bool, len(env))
	for _, e := range env {
		envMap[e.Name] = true
	}
	if !envMap["GC_K8S_DOLT_HOST"] {
		env = append(env, corev1.EnvVar{
			Name: "GC_K8S_DOLT_HOST", Value: "dolt.gc.svc.cluster.local",
		})
	}
	if !envMap["GC_K8S_DOLT_PORT"] {
		env = append(env, corev1.EnvVar{
			Name: "GC_K8S_DOLT_PORT", Value: "3307",
		})
	}

	// Inject GITHUB_TOKEN from optional K8s secret for git push in pods.
	env = append(env, corev1.EnvVar{
		Name: "GITHUB_TOKEN",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "git-credentials"},
				Key:                  "token",
				Optional:             boolPtr(true),
			},
		},
	})

	return env
}

// needsStaging returns true if the session config requires file staging
// via init container.
func needsStaging(cfg runtime.Config, ctrlCity string) bool {
	if cfg.OverlayDir != "" {
		return true
	}
	if len(cfg.CopyFiles) > 0 {
		return true
	}
	// Rig agents have a work_dir subdirectory.
	if cfg.WorkDir != "" && cfg.WorkDir != ctrlCity {
		return true
	}
	return false
}

// buildResources creates resource requirements from the provider config.
// Returns an error if any resource quantity string is invalid, instead of
// panicking via MustParse.
func buildResources(p *Provider) (corev1.ResourceRequirements, error) {
	req := corev1.ResourceRequirements{}
	if p.cpuRequest != "" || p.memRequest != "" {
		req.Requests = corev1.ResourceList{}
		if p.cpuRequest != "" {
			q, err := resource.ParseQuantity(p.cpuRequest)
			if err != nil {
				return req, fmt.Errorf("parsing GC_K8S_CPU_REQUEST %q: %w", p.cpuRequest, err)
			}
			req.Requests[corev1.ResourceCPU] = q
		}
		if p.memRequest != "" {
			q, err := resource.ParseQuantity(p.memRequest)
			if err != nil {
				return req, fmt.Errorf("parsing GC_K8S_MEM_REQUEST %q: %w", p.memRequest, err)
			}
			req.Requests[corev1.ResourceMemory] = q
		}
	}
	if p.cpuLimit != "" || p.memLimit != "" {
		req.Limits = corev1.ResourceList{}
		if p.cpuLimit != "" {
			q, err := resource.ParseQuantity(p.cpuLimit)
			if err != nil {
				return req, fmt.Errorf("parsing GC_K8S_CPU_LIMIT %q: %w", p.cpuLimit, err)
			}
			req.Limits[corev1.ResourceCPU] = q
		}
		if p.memLimit != "" {
			q, err := resource.ParseQuantity(p.memLimit)
			if err != nil {
				return req, fmt.Errorf("parsing GC_K8S_MEM_LIMIT %q: %w", p.memLimit, err)
			}
			req.Limits[corev1.ResourceMemory] = q
		}
	}
	return req, nil
}

func boolPtr(b bool) *bool { return &b }
