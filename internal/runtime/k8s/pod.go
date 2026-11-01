package k8s

import (
	"encoding/base64"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const (
	podManagedDoltHost = "dolt.gc.svc.cluster.local"
	podManagedDoltPort = "3307"

	// podWorkspaceRoot is the pod-side projection of the city root. It is the
	// only directory guaranteed to exist when the container starts — it is the
	// "ws" EmptyDir mount point for staged pods and the image WORKDIR for
	// prebaked ones — so it is what the pod spec's WorkingDir may safely name.
	podWorkspaceRoot = "/workspace"

	// Linux limits the size of one execve argument to 128 KiB. Keep encoded
	// command chunks well below that limit while allowing the full prompt payload
	// to retain the existing shared prompt-resolution threshold.
	agentCommandChunkSize = 32 * 1024
	agentCommandArg0      = "gc-agent-command"
)

func controllerCityPath(cfgEnv map[string]string) string {
	ctrlCity := strings.TrimSpace(cfgEnv["GC_CITY"])
	if ctrlCity == "" {
		ctrlCity = strings.TrimSpace(cfgEnv["GC_CITY_PATH"])
	}
	if ctrlCity == "" {
		ctrlCity = strings.TrimSpace(cfgEnv["GC_CITY_ROOT"])
	}
	return ctrlCity
}

func remapControllerPathToPod(val, ctrlCity string) string {
	val = strings.TrimSpace(val)
	ctrlCity = strings.TrimSpace(ctrlCity)
	if val == "" || ctrlCity == "" {
		return val
	}
	if val == ctrlCity || strings.HasPrefix(val, ctrlCity+"/") {
		return "/workspace" + val[len(ctrlCity):]
	}
	return val
}

// projectedPodWorkDir maps the controller-side WorkDir onto its pod-side path.
// For a pool or workflow worker this is a per-bead directory
// (<rig>/<beadID>-<slug>) that does not exist until the entrypoint creates it.
func projectedPodWorkDir(cfg runtime.Config) string {
	podWorkDir := podWorkspaceRoot
	ctrlCity := controllerCityPath(cfg.Env)
	if ctrlCity != "" && cfg.WorkDir != "" && cfg.WorkDir != ctrlCity {
		if rel, ok := strings.CutPrefix(cfg.WorkDir, ctrlCity+"/"); ok {
			podWorkDir = podWorkspaceRoot + "/" + rel
		}
	}
	return podWorkDir
}

// agentCommandB64 resolves the agent command and its configured startup prompt,
// remaps controller-side city path references to the pod-side /workspace, and
// returns its base64 form. PromptSuffix is already shell-quoted by the shared
// prompt-delivery resolver; it is empty for nudge-mode and oversized fallback.
// Shared by buildPod (the pod entrypoint) and Relaunch (respawn over execInPod)
// so the entrypoint launch and a relaunch produce a byte-identical command.
func agentCommandB64(cfg runtime.Config) string {
	cmd := cfg.Command
	if cmd == "" {
		cmd = "/bin/bash"
	}
	if cfg.PromptSuffix != "" {
		if cfg.PromptFlag != "" {
			cmd += " " + cfg.PromptFlag + " " + cfg.PromptSuffix
		} else {
			cmd += " " + cfg.PromptSuffix
		}
	}
	// The controller expands {{.ConfigDir}} templates using its own city path
	// (e.g. /city/packs/...) but pods have files at /workspace/.... Apply this
	// after adding the prompt so path references in either part are remapped.
	if ctrlCity := controllerCityPath(cfg.Env); ctrlCity != "" {
		cmd = strings.ReplaceAll(cmd, ctrlCity, "/workspace")
	}
	return base64.StdEncoding.EncodeToString([]byte(cmd))
}

func agentCommandChunks(cfg runtime.Config) []string {
	encoded := agentCommandB64(cfg)
	chunks := make([]string, 0, (len(encoded)+agentCommandChunkSize-1)/agentCommandChunkSize)
	for len(encoded) > 0 {
		chunkLen := agentCommandChunkSize
		if len(encoded) < chunkLen {
			chunkLen = len(encoded)
		}
		chunks = append(chunks, encoded[:chunkLen])
		encoded = encoded[chunkLen:]
	}
	return chunks
}

// agentCommandShellArgs puts the command chunks after the shell's $0 argument.
// The static script can therefore reconstruct the payload from "$@" without
// embedding a long encoded string or the decoded command in shell source.
func agentCommandShellArgs(script string, cfg runtime.Config) []string {
	args := []string{script, agentCommandArg0}
	return append(args, agentCommandChunks(cfg)...)
}

// buildAgentLaunchCommand builds the static shell transport shared by Start and
// Relaunch. Dynamic-user launches stream the encoded command through stdin to a
// static su -c script; the inner script decodes it before passing the command
// to tmux as one quoted argument. tmuxCommand is a fixed command selected by
// the caller.
func buildAgentLaunchCommand(cfg runtime.Config, tmuxCommand string, keepAlive bool) string {
	keepAliveSuffix := ""
	if keepAlive {
		keepAliveSuffix = " && sleep infinity"
	}

	if user := cfg.Env["LINUX_USERNAME"]; user != "" {
		userScript := fmt.Sprintf(
			`CMD=$(base64 -d) && cd %s && %s "$CMD"%s`,
			shellquote.Quote(projectedPodWorkDir(cfg)), tmuxCommand, keepAliveSuffix,
		)
		return fmt.Sprintf(
			`printf '%%s' "$@" | su - %s -c %s`,
			shellquote.Quote(user), shellquote.Quote(userScript),
		)
	}

	return fmt.Sprintf(
		`CMD=$(printf '%%s' "$@" | base64 -d) && %s "$CMD"%s`,
		tmuxCommand, keepAliveSuffix,
	)
}

// buildRespawnCommand builds the in-pod shell argv that respawns the agent in
// the existing tmux "main" session (respawn-pane -k), reusing the warm pod. When
// LINUX_USERNAME is set the entrypoint runs tmux under `su - <user>`, so the
// respawn is wrapped in the same su to reach that user's tmux socket.
func buildRespawnCommand(cfg runtime.Config) []string {
	script := buildAgentLaunchCommand(cfg, fmt.Sprintf("tmux respawn-pane -k -t %s", tmuxSession), false)
	args := []string{"sh", "-c"}
	return append(args, agentCommandShellArgs(script, cfg)...)
}

func projectedPodStoreRoot(cfg runtime.Config, podWorkDir string) string {
	storeRoot := strings.TrimSpace(cfg.Env["GC_STORE_ROOT"])
	if storeRoot == "" {
		storeRoot = strings.TrimSpace(cfg.WorkDir)
	}
	if storeRoot == "" {
		storeRoot = controllerCityPath(cfg.Env)
	}
	storeRoot = remapControllerPathToPod(storeRoot, controllerCityPath(cfg.Env))
	if storeRoot == "" {
		return podWorkDir
	}
	return storeRoot
}

func projectedPodRuntimeDir(cfgEnv map[string]string, ctrlCity string) string {
	podCity := "/workspace"
	runtimeDir := strings.TrimSpace(cfgEnv["GC_CITY_RUNTIME_DIR"])
	if runtimeDir == "" {
		return citylayout.RuntimeDataDir(podCity)
	}
	remapped := remapControllerPathToPod(runtimeDir, ctrlCity)
	if remapped != runtimeDir {
		return remapped
	}
	return citylayout.RuntimeDataDir(podCity)
}

func projectControllerRuntimePathToPod(path, ctrlCity, ctrlRuntimeDir, podRuntimeDir string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}
	if remapped := remapControllerPathToPod(path, ctrlCity); remapped != path {
		return remapped
	}
	if ctrlRuntimeDir != "" && pathutil.PathWithin(ctrlRuntimeDir, path) {
		normalizedRoot := pathutil.NormalizePathForCompare(ctrlRuntimeDir)
		normalizedPath := pathutil.NormalizePathForCompare(path)
		rel, err := filepath.Rel(normalizedRoot, normalizedPath)
		if err == nil {
			if rel == "." {
				return podRuntimeDir
			}
			return filepath.Join(podRuntimeDir, rel)
		}
	}
	return path
}

// projectedPodDoltEnv adapts the controller projection to a pod-visible Dolt
// target. Managed-local controller projections intentionally omit GC_DOLT_HOST
// and use a host-local runtime port; pods translate that blank-host managed
// shape to the provider-configured in-cluster alias at this adapter edge so
// agents still consume one GC_DOLT_* connection contract. A managed-local
// projection whose port the controller withheld, so host-local bd follows the
// scope's .beads/dolt-server.port mirror (gastownhall/gascity#1656), carries
// no port either and says so with GC_DOLT_MANAGED_LOCAL=1; a pod cannot read
// that mirror, so it gets the same alias. Without the marker, an empty host
// and port means no Dolt target. Explicit GC_DOLT_HOST values are preserved
// as written.
// BEADS_DOLT_SERVER_HOST/PORT are compatibility mirrors derived from the GC
// projection, not independent input authorities.
func controllerLocalDoltHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	switch host {
	case "", "127.0.0.1", "localhost", "0.0.0.0", "::1", "::":
		return true
	default:
		return false
	}
}

func projectedPodDoltEnv(cfgEnv map[string]string, managedHost, managedPort string) (map[string]string, error) {
	host := strings.TrimSpace(cfgEnv["GC_DOLT_HOST"])
	port := strings.TrimSpace(cfgEnv["GC_DOLT_PORT"])
	managedHost = strings.TrimSpace(managedHost)
	managedPort = strings.TrimSpace(managedPort)
	if managedHost == "" {
		managedHost = podManagedDoltHost
	}
	if managedPort == "" {
		managedPort = podManagedDoltPort
	}

	switch {
	case host == "" && port == "" && strings.TrimSpace(cfgEnv["GC_DOLT_MANAGED_LOCAL"]) != "1":
		return map[string]string{}, nil
	case host != "" && port == "":
		return nil, fmt.Errorf("requires both GC_DOLT_HOST and GC_DOLT_PORT when GC_DOLT_HOST is set")
	case controllerLocalDoltHost(host):
		host = managedHost
		port = managedPort
	}

	projected := map[string]string{
		"GC_DOLT_HOST":           host,
		"GC_DOLT_PORT":           port,
		"BEADS_DOLT_SERVER_HOST": host,
		"BEADS_DOLT_SERVER_PORT": port,
	}
	return projected, nil
}

// buildPod creates a pod manifest compatible with gc-session-k8s.
// Same labels, annotations, container names, volumes, and tmux-inside-pod
// pattern so mixed-mode migration works.
func buildPod(name string, cfg runtime.Config, p *Provider) (*corev1.Pod, error) {
	if err := validateSecretProjection(p.secretEnv, p.secretMounts); err != nil {
		return nil, err
	}
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
	// Controller resolves dirs relative to its city path; pods use /workspace.
	podWorkDir := projectedPodWorkDir(cfg)
	ctrlCity := controllerCityPath(cfg.Env)

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
	if linuxUsername != "" && hasConfiguredPodIdentity(p.podSecurity) {
		return nil, fmt.Errorf("LINUX_USERNAME cannot be combined with explicit Kubernetes pod identity (session.k8s.run_as_user/run_as_group/fs_group or GC_K8S_RUN_AS_USER/GC_K8S_RUN_AS_GROUP/GC_K8S_FS_GROUP)")
	}
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

	// The pod spec's WorkingDir names the workspace root, because the kubelet
	// chdirs into it before this command runs and a per-bead workDir does not
	// exist yet. Create and enter the real working directory here instead.
	//
	// Placement matters twice over. It must come *after* wsWait, because until
	// staging signals ready the workspace content is still being written and a
	// shell sitting in a subdirectory of it is standing on shifting ground. And
	// it must come *before* preStartCmds, because pre_start previously ran in
	// podWorkDir (the container's WorkingDir) and must keep doing so.
	enterWorkDir := fmt.Sprintf("mkdir -p %s && cd %s && ",
		shellquote.Quote(podWorkDir), shellquote.Quote(podWorkDir))

	// The launch script stays static; the command arrives in bounded positional
	// chunks after its $0 argument and is reconstructed at runtime.
	entrypointPrefix := credCopy + wsWait + enterWorkDir + preStartCmds
	if linuxUsername != "" {
		// Run tmux as the dynamic user via su. userSetup already created and
		// chowned podWorkDir as root; enterWorkDir is idempotent and keeps
		// pre_start in the same directory as the historical path.
		entrypointPrefix = userSetup + entrypointPrefix
	}
	tmuxCmd := entrypointPrefix + buildAgentLaunchCommand(
		cfg,
		fmt.Sprintf("tmux new-session -d -s %s", tmuxSession),
		true,
	)

	// Build environment, remapping K8s-specific vars.
	env, err := buildPodEnvWithSecretEnv(cfg.Env, podWorkDir, p.managedServiceHost, p.managedServicePort, p.secretEnv)
	if err != nil {
		return nil, err
	}

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

	secretMounts := p.secretMounts
	defaultSecretMounts := len(secretMounts) == 0
	if len(secretMounts) == 0 {
		secretMounts = []runtime.K8sSecretMount{{Secret: "claude-credentials", MountPath: "/tmp/claude-secret"}}
	}
	for i, projection := range secretMounts {
		volumeName := fmt.Sprintf("gc-secret-%d", i)
		if defaultSecretMounts {
			volumeName = "claude-config"
		}
		mainVolMounts = append(mainVolMounts, corev1.VolumeMount{
			Name: volumeName, MountPath: projection.MountPath, ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: volumeName, VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: projection.Secret,
					Optional:   boolPtr(true),
				},
			},
		})
	}

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
			SecurityContext:    agentPodSecurityContext(p.podSecurity, linuxUsername == ""),
			ServiceAccountName: p.serviceAccount,
			RestartPolicy:      corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "agent",
				Image:           p.image,
				ImagePullPolicy: corev1.PullAlways,
				// Not podWorkDir: the runtime resolves this before the entrypoint
				// runs, so naming a per-bead directory that nothing has created
				// yet is unsafe. containerd creates the whole chain itself as
				// root:root 0755, leaving the non-root agent unable to write into
				// its own working directory; other runtimes may refuse to start
				// the container. The entrypoint creates and enters podWorkDir
				// itself, as the agent user, so it comes out owned correctly.
				WorkingDir:      podWorkspaceRoot,
				Command:         []string{"/bin/sh", "-c"},
				Args:            agentCommandShellArgs(tmuxCmd, cfg),
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

	// Apply optional scheduling fields.
	pod.Spec.NodeSelector = maps.Clone(p.nodeSelector)
	pod.Spec.Tolerations = cloneTolerations(p.tolerations)
	if p.affinity != nil {
		pod.Spec.Affinity = p.affinity.DeepCopy()
	}
	pod.Spec.PriorityClassName = p.priorityClassName

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
			SecurityContext: restrictedAgentSecurityContext(),
		}}
	}

	return pod, nil
}

func cloneTolerations(in []corev1.Toleration) []corev1.Toleration {
	if len(in) == 0 {
		return nil
	}
	out := append([]corev1.Toleration(nil), in...)
	for i := range out {
		if in[i].TolerationSeconds != nil {
			seconds := *in[i].TolerationSeconds
			out[i].TolerationSeconds = &seconds
		}
	}
	return out
}

// agentSecurityContext returns a container security context. The normal path
// runs as the image's numeric non-root fallback or an optional pod identity
// and satisfies Restricted Pod Security. The legacy LINUX_USERNAME path still
// starts as root so it can create the account and hand off to su.
func agentSecurityContext(linuxUsername string) *corev1.SecurityContext {
	if linuxUsername == "" {
		return restrictedAgentSecurityContext()
	}
	var rootUID int64
	return &corev1.SecurityContext{
		RunAsUser: &rootUID,
	}
}

func restrictedAgentSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

func agentPodSecurityContext(config PodSecurityConfig, restricted bool) *corev1.PodSecurityContext {
	security := &corev1.PodSecurityContext{
		RunAsUser:  cloneInt64Ptr(config.RunAsUser),
		RunAsGroup: cloneInt64Ptr(config.RunAsGroup),
		FSGroup:    cloneInt64Ptr(config.FSGroup),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
	if restricted {
		security.RunAsNonRoot = boolPtr(true)
		// The image's HOME and prebaked workspace are group-writable by GID 0.
		// Keep an explicitly requested nonzero primary GID while adding GID 0
		// as a supplementary group so those image paths remain writable.
		if config.RunAsGroup != nil && *config.RunAsGroup != 0 {
			security.SupplementalGroups = []int64{0}
		}
	}
	if !restricted && security.RunAsUser == nil && security.RunAsGroup == nil && security.FSGroup == nil {
		return nil
	}
	return security
}

func hasConfiguredPodIdentity(config PodSecurityConfig) bool {
	return config.RunAsUser != nil || config.RunAsGroup != nil || config.FSGroup != nil
}

func cloneInt64Ptr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// buildPodEnv creates the env var list for the agent container.
// Removes controller-only vars, strips deprecated K8s compatibility inputs,
// and remaps pod-visible ones.
func buildPodEnv(cfgEnv map[string]string, podWorkDir, managedServiceHost, managedServicePort string) ([]corev1.EnvVar, error) {
	return buildPodEnvWithSecretEnv(cfgEnv, podWorkDir, managedServiceHost, managedServicePort, nil)
}

func buildPodEnvWithSecretEnv(cfgEnv map[string]string, podWorkDir, managedServiceHost, managedServicePort string, secretEnv []runtime.K8sSecretEnv) ([]corev1.EnvVar, error) {
	// Start with cfg.Env, removing controller-only vars.
	// Auth creds (GC_DOLT_USER, GC_DOLT_PASSWORD, BEADS_DOLT_*_USER/PASSWORD) intentionally pass through.
	// GC_DOLT_MANAGED_LOCAL marks a controller-local target, which
	// projectedPodDoltEnv rewrites to the in-cluster service.
	skip := map[string]bool{
		"GC_BEADS":               true,
		"GC_SESSION":             true,
		"GC_EVENTS":              true,
		"GC_K8S_DOLT_HOST":       true,
		"GC_K8S_DOLT_PORT":       true,
		"GC_DOLT_HOST":           true,
		"GC_DOLT_PORT":           true,
		"GC_DOLT_MANAGED_LOCAL":  true,
		"BEADS_DOLT_SERVER_HOST": true,
		"BEADS_DOLT_SERVER_PORT": true,
	}

	ctrlCity := controllerCityPath(cfgEnv)
	ctrlRuntimeDir := strings.TrimSpace(cfgEnv["GC_CITY_RUNTIME_DIR"])
	podRuntimeDir := projectedPodRuntimeDir(cfgEnv, ctrlCity)

	var env []corev1.EnvVar
	for k, v := range cfgEnv {
		if skip[k] {
			continue
		}
		val := v
		// Remap city/workdir vars to pod-visible paths.
		switch k {
		case "GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT":
			val = "/workspace"
		case "GC_DIR":
			val = podWorkDir
		case "GC_CITY_RUNTIME_DIR":
			val = podRuntimeDir
		case "GC_CONTROL_DISPATCHER_TRACE_DEFAULT", "GC_PACK_STATE_DIR":
			val = projectControllerRuntimePathToPod(val, ctrlCity, ctrlRuntimeDir, podRuntimeDir)
		case "GC_STORE_ROOT", "GC_BEADS_SCOPE_ROOT", "GC_RIG_ROOT", "BEADS_DIR", "GT_ROOT", "GC_PACK_DIR":
			val = remapControllerPathToPod(val, ctrlCity)
		}
		env = append(env, corev1.EnvVar{Name: k, Value: val})
	}

	projectedDolt, err := projectedPodDoltEnv(cfgEnv, managedServiceHost, managedServicePort)
	if err != nil {
		return nil, err
	}
	projectedKeys := make([]string, 0, len(projectedDolt))
	for key := range projectedDolt {
		projectedKeys = append(projectedKeys, key)
	}
	sort.Strings(projectedKeys)
	for _, key := range projectedKeys {
		env = append(env, corev1.EnvVar{Name: key, Value: projectedDolt[key]})
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

	if len(secretEnv) == 0 {
		secretEnv = []runtime.K8sSecretEnv{{Name: "GITHUB_TOKEN", Secret: "git-credentials", Key: "token"}}
	}
	for _, projection := range secretEnv {
		env = append(env, corev1.EnvVar{
			Name: projection.Name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: projection.Secret},
					Key:                  projection.Key,
					Optional:             boolPtr(true),
				},
			},
		})
	}

	return env, nil
}

var secretProjectionEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateSecretProjection(secretEnv []runtime.K8sSecretEnv, secretMounts []runtime.K8sSecretMount) error {
	seenEnv := make(map[string]struct{}, len(secretEnv))
	for i, projection := range secretEnv {
		if strings.TrimSpace(projection.Name) == "" || strings.TrimSpace(projection.Secret) == "" || strings.TrimSpace(projection.Key) == "" {
			return fmt.Errorf("k8s secret_env[%d] requires non-blank name, secret, and key", i)
		}
		if !secretProjectionEnvName.MatchString(projection.Name) {
			return fmt.Errorf("k8s secret_env[%d].name %q is not a valid environment variable name", i, projection.Name)
		}
		if _, exists := seenEnv[projection.Name]; exists {
			return fmt.Errorf("k8s secret_env[%d].name %q is duplicated", i, projection.Name)
		}
		seenEnv[projection.Name] = struct{}{}
	}
	seenMount := make(map[string]struct{}, len(secretMounts))
	for i, projection := range secretMounts {
		if strings.TrimSpace(projection.Secret) == "" || strings.TrimSpace(projection.MountPath) == "" {
			return fmt.Errorf("k8s secret_mounts[%d] requires non-blank secret and mount_path", i)
		}
		if !path.IsAbs(projection.MountPath) {
			return fmt.Errorf("k8s secret_mounts[%d].mount_path %q must be absolute", i, projection.MountPath)
		}
		if _, exists := seenMount[projection.MountPath]; exists {
			return fmt.Errorf("k8s secret_mounts[%d].mount_path %q is duplicated", i, projection.MountPath)
		}
		seenMount[projection.MountPath] = struct{}{}
	}
	return nil
}

// needsStaging returns true if the session config requires file staging
// via init container.
func needsStaging(cfg runtime.Config, ctrlCity string) bool {
	if cfg.OverlayDir != "" {
		return true
	}
	if len(cfg.PackOverlayDirs) > 0 {
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
