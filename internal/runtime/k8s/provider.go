package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Compile-time interface checks.
var (
	_ runtime.Provider     = (*Provider)(nil)
	_ runtime.ExecProvider = (*Provider)(nil)
)

// Provider is a native Kubernetes session provider using client-go.
// Eliminates subprocess overhead by making direct API calls over reused
// HTTP/2 connections. Pod manifests are compatible with gc-session-k8s.
type Provider struct {
	ops                k8sOps
	namespace          string
	image              string
	k8sContext         string
	managedServiceHost string
	managedServicePort string
	cpuRequest         string
	memRequest         string
	cpuLimit           string
	memLimit           string
	serviceAccount     string              // pod service account name (GC_K8S_SERVICE_ACCOUNT)
	prebaked           bool                // skip staging + init container for prebaked images
	nodeSelector       map[string]string   // GC_K8S_NODE_SELECTOR (JSON)
	tolerations        []corev1.Toleration // GC_K8S_TOLERATIONS (JSON)
	affinity           *corev1.Affinity    // GC_K8S_AFFINITY (JSON)
	priorityClassName  string              // GC_K8S_PRIORITY_CLASS_NAME
	postStartSettle    time.Duration       // settle time before post-start liveness check
	stderr             io.Writer           // warning output (default os.Stderr)
}

type schedulingFields struct {
	nodeSelector      map[string]string
	tolerations       []corev1.Toleration
	affinity          *corev1.Affinity
	priorityClassName string
}

// NewProvider creates a K8s session provider.
// Configuration is read from environment variables (matching gc-session-k8s):
//   - GC_K8S_NAMESPACE — namespace (default: "gc")
//   - GC_K8S_IMAGE — container image (required for Start)
//   - GC_K8S_CONTEXT — kubectl context (default: current)
//   - GC_K8S_SERVICE_ACCOUNT — pod service account name (default: namespace default)
//   - GC_K8S_CPU_REQUEST, GC_K8S_MEM_REQUEST — resource requests
//   - GC_K8S_CPU_LIMIT, GC_K8S_MEM_LIMIT — resource limits
//
// The in-cluster Dolt service alias defaults to the provider defaults
// (dolt.gc.svc.cluster.local:3307). Pods receive projected GC_DOLT_* env;
// GC_K8S_DOLT_* remains a deprecated compatibility input for the provider-
// managed in-cluster alias only.
//
// Uses rest.InClusterConfig() when running in a pod, falls back to
// clientcmd.BuildConfigFromFlags() for local development.
func NewProvider() (*Provider, error) {
	namespace := envOrDefault("GC_K8S_NAMESPACE", "gc")
	image := os.Getenv("GC_K8S_IMAGE")
	k8sContext := os.Getenv("GC_K8S_CONTEXT")

	restConfig, err := buildRESTConfig(k8sContext)
	if err != nil {
		return nil, fmt.Errorf("building K8s config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating K8s clientset: %w", err)
	}

	managedServiceHost, managedServicePort, err := managedServiceAlias()
	if err != nil {
		return nil, err
	}

	scheduling, err := parseSchedulingEnv()
	if err != nil {
		return nil, err
	}

	return &Provider{
		ops: &realK8sOps{
			clientset:  clientset,
			restConfig: restConfig,
			namespace:  namespace,
		},
		namespace:          namespace,
		image:              image,
		k8sContext:         k8sContext,
		managedServiceHost: managedServiceHost,
		managedServicePort: managedServicePort,
		cpuRequest:         envOrDefault("GC_K8S_CPU_REQUEST", "500m"),
		memRequest:         envOrDefault("GC_K8S_MEM_REQUEST", "1Gi"),
		cpuLimit:           envOrDefault("GC_K8S_CPU_LIMIT", "2"),
		memLimit:           envOrDefault("GC_K8S_MEM_LIMIT", "4Gi"),
		serviceAccount:     os.Getenv("GC_K8S_SERVICE_ACCOUNT"),
		prebaked:           os.Getenv("GC_K8S_PREBAKED") == "true",
		postStartSettle:    3 * time.Second,
		stderr:             os.Stderr,
		nodeSelector:       scheduling.nodeSelector,
		tolerations:        scheduling.tolerations,
		affinity:           scheduling.affinity,
		priorityClassName:  scheduling.priorityClassName,
	}, nil
}

func parseSchedulingEnv() (schedulingFields, error) {
	var scheduling schedulingFields
	if v := os.Getenv("GC_K8S_NODE_SELECTOR"); v != "" {
		if err := json.Unmarshal([]byte(v), &scheduling.nodeSelector); err != nil {
			return schedulingFields{}, fmt.Errorf("parsing GC_K8S_NODE_SELECTOR: %w", err)
		}
	}
	if v := os.Getenv("GC_K8S_TOLERATIONS"); v != "" {
		if err := json.Unmarshal([]byte(v), &scheduling.tolerations); err != nil {
			return schedulingFields{}, fmt.Errorf("parsing GC_K8S_TOLERATIONS: %w", err)
		}
	}
	if v := os.Getenv("GC_K8S_AFFINITY"); v != "" {
		if err := json.Unmarshal([]byte(v), &scheduling.affinity); err != nil {
			return schedulingFields{}, fmt.Errorf("parsing GC_K8S_AFFINITY: %w", err)
		}
	}
	scheduling.priorityClassName = os.Getenv("GC_K8S_PRIORITY_CLASS_NAME")
	return scheduling, nil
}

// newProviderWithOps creates a provider with a custom k8sOps (for testing).
func newProviderWithOps(ops k8sOps) *Provider {
	return &Provider{
		ops:                ops,
		namespace:          "test-ns",
		image:              "test-image:latest",
		managedServiceHost: podManagedDoltHost,
		managedServicePort: podManagedDoltPort,
		cpuRequest:         "500m",
		memRequest:         "1Gi",
		cpuLimit:           "2",
		memLimit:           "4Gi",
		stderr:             io.Discard,
	}
}

// Start creates a new K8s pod running a tmux session with the agent command.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.image == "" {
		return fmt.Errorf("starting session %q: GC_K8S_IMAGE is required", name)
	}
	podName := SanitizeName(name)
	label := SanitizeLabel(name)

	// Check for existing pod (any phase).
	existing, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err == nil && len(existing) > 0 {
		pod := &existing[0]
		if pod.Status.Phase == corev1.PodRunning {
			// Check if tmux is alive — stale pod detection.
			_, tmuxErr := p.ops.execInPod(ctx, pod.Name, "agent",
				[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
			if tmuxErr == nil {
				return fmt.Errorf("%w: session %q (pod: %s)", runtime.ErrSessionExists, name, pod.Name)
			}
			// tmux dead — but if the pod is young, workspace init may still
			// be blocking the tmux server from starting. Don't delete pods
			// that are still within the startup window.
			if time.Since(pod.CreationTimestamp.Time) < startupGracePeriod {
				return fmt.Errorf("%w: session %q (pod: %s)", runtime.ErrSessionInitializing, name, pod.Name)
			}
			// Stale pod — tmux dead and past grace period, recreate.
		}
		// Clean up existing pod.
		_ = p.ops.deletePod(ctx, pod.Name, 5)
		_ = waitForDeletion(ctx, p.ops, pod.Name, 30*time.Second)
	}

	// Build and create pod.
	pod, err := buildPod(name, cfg, p)
	if err != nil {
		return fmt.Errorf("building pod for session %q: %w", name, err)
	}
	_, err = p.ops.createPod(ctx, pod)
	if err != nil {
		return fmt.Errorf("creating pod for session %q: %w", name, err)
	}

	// cleanup deletes the pod on any startup failure after creation.
	// Uses a fresh background context so cleanup succeeds even if the
	// original ctx was canceled (which is the common failure path).
	cleanup := func(_ string) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = p.ops.deletePod(cleanupCtx, podName, 5)
	}

	ctrlCity := cfg.Env["GC_CITY"]

	if !p.prebaked {
		// Stage files via init container if needed.
		if needsStaging(cfg, ctrlCity) {
			if err := stageFiles(ctx, p.ops, podName, cfg, ctrlCity, p.stderr); err != nil {
				cleanup("staging failed")
				return fmt.Errorf("staging files for session %q: %w", name, err)
			}
		}
	}

	// Wait for main container to be running.
	if err := waitForPodRunning(ctx, p.ops, podName, 120*time.Second); err != nil {
		cleanup("pod not running")
		return fmt.Errorf("waiting for pod %q: %w", podName, err)
	}

	if !p.prebaked {
		// Initialize the city inside the pod.
		if ctrlCity != "" {
			if err := initCityInPod(ctx, p.ops, podName, ctrlCity); err != nil {
				fmt.Fprintf(p.stderr, "gc: warning: initCityInPod for %s: %v\n", podName, err) //nolint:errcheck
			}
		}

		// Signal entrypoint to proceed.
		if _, err := p.ops.execInPod(ctx, podName, "agent",
			[]string{"touch", "/workspace/.gc-workspace-ready"}, nil); err != nil {
			fmt.Fprintf(p.stderr, "gc: warning: touch .gc-workspace-ready in %s: %v\n", podName, err) //nolint:errcheck
		}
	}

	// Ensure .beads/ inside the pod. This remains warning-only so older staged
	// or prebaked workspaces can self-heal instead of failing session startup.
	podWorkDir := projectedPodWorkDir(cfg)
	if err := initBeadsInPod(ctx, p.ops, podName, cfg, podWorkDir, p.managedServiceHost, p.managedServicePort); err != nil {
		fmt.Fprintf(p.stderr, "gc: warning: initBeadsInPod for %s: %v\n", podName, err) //nolint:errcheck
	}

	// Wait for tmux session.
	if err := waitForTmux(ctx, p.ops, podName, 60*time.Second); err != nil {
		cleanup("tmux not ready")
		return fmt.Errorf("waiting for tmux in pod %q: %w", podName, err)
	}

	// Enable pane logging + run session setup (shared with the relaunch tail).
	p.runPodPostLaunchSetup(ctx, podName, cfg)

	requiresPostStartLiveness := k8sRequiresPostStartLiveness(cfg)

	// Post-start liveness check: verify interactive sessions survived startup.
	// Agents that fail immediately (e.g. --resume with a stale session key)
	// exit within a second. A brief settle lets us detect this before
	// returning success to the reconciler, which triggers recordWakeFailure
	// and the crash-loop recovery (clear session_key, bump continuation_epoch).
	//
	// Some configured commands are intentionally one-turn processes. Those
	// should return from Start after the first tmux appearance and let normal
	// session reconciliation observe completion, rather than converting clean
	// command exit into startup failure.
	if requiresPostStartLiveness && p.postStartSettle > 0 {
		timer := time.NewTimer(p.postStartSettle)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			cleanup("post-start settle canceled")
			return fmt.Errorf("waiting for post-start settle for session %q: %w", name, ctx.Err())
		case <-timer.C:
		}
	}
	if requiresPostStartLiveness {
		_, tmuxErr := p.ops.execInPod(ctx, podName, "agent",
			[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
		if tmuxErr != nil {
			cleanup("session died immediately after startup")
			return fmt.Errorf("%w: session %q died immediately after startup: %w",
				runtime.ErrSessionDiedDuringStartup, name, tmuxErr)
		}
	}

	// Send initial nudge if configured (matches tmux adapter step 6).
	if cfg.Nudge != "" {
		_ = p.Nudge(name, runtime.TextContent(cfg.Nudge))
	}

	return nil
}

// runPodPostLaunchSetup enables pane logging and runs session_setup and
// session_setup_script inside the pod, best-effort. Shared by Start (after the
// entrypoint launches the agent) and Relaunch (after the respawn). k8s does not
// run SessionLive (RunLive is a no-op), matching the pre-un-weld behavior.
func (p *Provider) runPodPostLaunchSetup(ctx context.Context, podName string, cfg runtime.Config) {
	// Enable pane logging for diagnostics.
	_, _ = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "pipe-pane", "-t", tmuxSession, "-o", "cat >> /tmp/agent-output.log"}, nil)

	// Run session_setup commands inside the pod.
	for _, cmd := range cfg.SessionSetup {
		if cmd == "" {
			continue
		}
		_, _ = p.ops.execInPod(ctx, podName, "agent",
			[]string{"sh", "-c", cmd}, nil)
	}

	// Run session_setup_script.
	if cfg.SessionSetupScript != "" {
		script, err := os.ReadFile(cfg.SessionSetupScript)
		if err != nil {
			fmt.Fprintf(p.stderr, "gc: warning: reading session_setup_script %q for %s: %v\n", cfg.SessionSetupScript, podName, err) //nolint:errcheck
		} else {
			_, _ = p.ops.execInPod(ctx, podName, "agent",
				[]string{"sh"}, strings.NewReader(string(script)))
		}
	}
}

// Relaunch re-launches the agent inside the already-running (warm) pod without
// recreating it: it respawns the in-pod tmux "main" pane (respawn-pane -k) with
// the (possibly changed) command over execInPod, then re-runs the post-launch
// setup tail. The pod stays warm via the entrypoint's `sleep infinity`, so a
// launch-only config change reaches the live pod without a full reprovision —
// the k8s half of the runtime/transport un-weld (B3a, mirroring tmux B1 / ssh).
//
// The pod must be Running with a live tmux "main" session, else
// [runtime.ErrSessionNotFound] (the reconciler decides whether to Start fresh —
// it does NOT recreate the pod here). Staging, city/beads init, and PreStart are
// NOT re-run (provision-half); env is provision-half too (set in the pod spec at
// create time, not re-injected — respawn-pane carries no env), matching tmux/ssh.
//
// CAVEAT (unverified on a real cluster — see the B3 design doc): for
// LINUX_USERNAME pods the entrypoint runs tmux under `su - <user>`, so the
// respawn is su-wrapped to reach that user's tmux socket; and if the in-pod tmux
// server itself died (not just the agent), respawn-pane fails and Relaunch
// returns ErrSessionNotFound so the reconciler reprovisions.
func (p *Provider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return fmt.Errorf("%w: session %q (no running pod to relaunch into)", runtime.ErrSessionNotFound, name)
	}
	// The tmux server + "main" session must be alive to respawn into; a dead
	// session means the box is not warm enough — reprovision, don't respawn.
	if _, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "has-session", "-t", tmuxSession}, nil); err != nil {
		return fmt.Errorf("%w: session %q (pod %s has no live tmux session)", runtime.ErrSessionNotFound, name, podName)
	}

	// Respawn the agent in the warm "main" session.
	if _, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"sh", "-c", buildRespawnCommand(cfg)}, nil); err != nil {
		return fmt.Errorf("k8s relaunch %q: respawn-pane: %w", name, err)
	}

	// Re-run the post-launch setup tail (pipe-pane logging + session_setup[/script]).
	p.runPodPostLaunchSetup(ctx, podName, cfg)

	// Post-relaunch liveness: detect an agent that dies immediately.
	if k8sRequiresPostStartLiveness(cfg) {
		if p.postStartSettle > 0 {
			timer := time.NewTimer(p.postStartSettle)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return fmt.Errorf("k8s relaunch %q: %w", name, ctx.Err())
			case <-timer.C:
			}
		}
		if _, err := p.ops.execInPod(ctx, podName, "agent",
			[]string{"tmux", "has-session", "-t", tmuxSession}, nil); err != nil {
			return fmt.Errorf("%w: session %q died immediately after relaunch: %w",
				runtime.ErrSessionDiedDuringStartup, name, err)
		}
	}

	if cfg.Nudge != "" {
		_ = p.Nudge(name, runtime.TextContent(cfg.Nudge))
	}
	return nil
}

func k8sRequiresPostStartLiveness(cfg runtime.Config) bool {
	if cfg.Lifecycle == runtime.LifecycleOneShot {
		return false
	}
	return runtime.HasManagedStartupHints(cfg)
}

// Stop deletes the pod(s) for the named session. Missing/not-found is
// idempotent (no error: the session is genuinely gone), but a transport
// failure surfaces. A list failure is "unknown state", NOT "session gone" —
// swallowing it would let the seam adapter (Runtime.Teardown → Stop) drop
// tracking while pods and their PVCs keep running untracked, leaking the most
// expensive runtime. Delete errors are joined for the same reason; only a
// genuine Kubernetes NotFound (the pod raced to gone) is treated as idempotent.
// This mirrors the ssh provider's Stop discrimination.
func (p *Provider) Stop(name string) error {
	ctx := context.Background()
	label := SanitizeLabel(name)

	pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // session genuinely gone
		}
		return fmt.Errorf("k8s stop %q: listing pods: %w", name, err)
	}
	var errs []error
	for i := range pods {
		if delErr := p.ops.deletePod(ctx, pods[i].Name, 5); delErr != nil && !apierrors.IsNotFound(delErr) {
			errs = append(errs, fmt.Errorf("deleting pod %q: %w", pods[i].Name, delErr))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("k8s stop %q: %w", name, errors.Join(errs...))
	}
	return nil
}

// Interrupt sends Ctrl-C to the tmux session inside the pod.
func (p *Provider) Interrupt(name string) error {
	_ = p.carrier().Interrupt(context.Background(), name) // best-effort
	return nil
}

// IsRunning reports whether the session has a running pod with a live tmux session.
func (p *Provider) IsRunning(name string) bool {
	ctx := context.Background()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return false
	}
	// Pod Running + tmux session alive.
	_, err = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
	return err == nil
}

// IsAttached reports whether a user terminal is connected to the tmux
// session inside the pod.
func (p *Provider) IsAttached(name string) bool {
	ctx := context.Background()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return false
	}
	output, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_attached}"}, nil)
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) == "1"
}

// Attach shells out to kubectl exec -it for full TTY passthrough.
func (p *Provider) Attach(name string) error {
	ctx := context.Background()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return fmt.Errorf("attach: no running pod for session %q", name)
	}

	args := []string{}
	if p.k8sContext != "" {
		args = append(args, "--context", p.k8sContext)
	}
	args = append(args, "-n", p.namespace, "exec", "-it", podName, "--",
		"tmux", "attach", "-t", tmuxSession)

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ProcessAlive checks if the named processes are running inside the pod.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	ctx := context.Background()
	label := SanitizeLabel(name)

	pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil || len(pods) == 0 {
		return false
	}
	pod := &pods[0]

	// Check deletionTimestamp — pod in graceful shutdown is not alive.
	if pod.DeletionTimestamp != nil {
		return false
	}
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	for _, pname := range processNames {
		_, err := p.ops.execInPod(ctx, pod.Name, "agent",
			[]string{"pgrep", "-f", pname}, nil)
		if err == nil {
			return true
		}
	}
	return false
}

// Nudge types a message into the tmux session followed by Enter.
// Uses -l (literal mode) so tmux key names in the message text are not
// interpreted as keystrokes. Content blocks are flattened to text.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	_ = p.carrier().Nudge(context.Background(), name, content) // best-effort
	return nil
}

// SendKeys sends bare keystrokes to the tmux session.
func (p *Provider) SendKeys(name string, keys ...string) error {
	_ = p.carrier().SendKeys(context.Background(), name, keys...) // best-effort
	return nil
}

// RunLive re-applies session_live commands. Not yet supported for K8s.
func (p *Provider) RunLive(_ string, _ runtime.Config) error {
	return nil
}

// SetMeta stores a key-value pair in the tmux environment.
func (p *Provider) SetMeta(name, key, value string) error {
	ctx := context.Background()
	podName, err := p.findPod(ctx, name)
	if err != nil {
		return nil // best-effort
	}
	_, _ = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "set-environment", "-t", tmuxSession, key, value}, nil)
	return nil
}

// GetMeta retrieves a metadata value from the tmux environment.
func (p *Provider) GetMeta(name, key string) (string, error) {
	ctx := context.Background()
	podName, err := p.findPod(ctx, name)
	if err != nil {
		return "", nil
	}
	output, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "show-environment", "-t", tmuxSession, key}, nil)
	if err != nil {
		return "", nil
	}
	output = strings.TrimSpace(output)
	// tmux output: "KEY=VALUE" (set), "-KEY" (unset).
	if strings.HasPrefix(output, "-") {
		return "", nil // explicitly unset
	}
	if _, val, ok := strings.Cut(output, "="); ok {
		return val, nil
	}
	return "", nil
}

// RemoveMeta removes a metadata key from the tmux environment.
func (p *Provider) RemoveMeta(name, key string) error {
	ctx := context.Background()
	podName, err := p.findPod(ctx, name)
	if err != nil {
		return nil // best-effort
	}
	_, _ = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "set-environment", "-t", tmuxSession, "-u", key}, nil)
	return nil
}

// Peek captures the last N lines of tmux pane output (best-effort: empty on failure).
func (p *Provider) Peek(name string, lines int) (string, error) {
	out, _ := p.carrier().Peek(context.Background(), name, lines) // best-effort
	return out, nil
}

// ListRunning returns names of all running sessions with the given prefix.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	ctx := context.Background()
	pods, err := p.ops.listPods(ctx, "app=gc-agent", "status.phase=Running")
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range pods {
		pod := &pods[i]
		// Prefer annotation (raw name) over label (sanitized).
		name := pod.Annotations["gc-session-name"]
		if name == "" {
			name = pod.Labels["gc-session"]
		}
		if name == "" {
			continue
		}
		if prefix == "" || strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// GetLastActivity returns the time of the last I/O in the tmux session.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	ctx := context.Background()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return time.Time{}, nil
	}
	output, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_activity}"}, nil)
	if err != nil {
		return time.Time{}, nil
	}
	epoch := strings.TrimSpace(output)
	if epoch == "" {
		return time.Time{}, nil
	}
	secs, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		return time.Time{}, nil
	}
	return time.Unix(secs, 0), nil
}

// ClearScrollback clears the tmux scrollback buffer (best-effort).
func (p *Provider) ClearScrollback(name string) error {
	_ = p.carrier().ClearScrollback(context.Background(), name) // best-effort
	return nil
}

// Capabilities reports K8s provider capabilities. The K8s provider
// supports activity tracking via tmux session_activity but does not
// support attachment detection from the controller host.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{
		CanReportActivity: true,
	}
}

// SleepCapability reports that k8s sessions can participate in timed-only
// idle sleep. The controller cannot observe attachment state from the host.
func (p *Provider) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityTimedOnly
}

// CopyTo copies a local file/directory into the pod via tar.
func (p *Provider) CopyTo(name, src, relDst string) error {
	ctx := context.Background()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return nil // best-effort
	}
	dst := "/workspace"
	if relDst != "" {
		dst = "/workspace/" + relDst
	}
	return copyToPod(ctx, p.ops, podName, "agent", src, dst)
}

// --- Internal helpers ---

// findRunningPod finds a running pod by session label.
// carrier returns the tmux carrier that drives this provider's sessions over
// the pod exec connection ([Provider.Exec]). The in-box tmux session is always
// tmuxSession ("main").
func (p *Provider) carrier() runtime.Carrier {
	return runtime.NewTmuxCarrier(p, tmuxSession)
}

// Exec implements [runtime.ExecProvider]: it runs argv inside the session
// pod's "agent" container and returns the command's standard output and exit
// code (execInPod returns stdout; stderr is folded into err on a transport
// failure). A command that runs but exits non-zero yields that code with a nil
// error; only a transport failure (no running pod, stream error) yields err.
// This is the connection the tmux carrier drives the session through.
func (p *Provider) Exec(ctx context.Context, name string, argv []string) ([]byte, int, error) {
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return nil, -1, fmt.Errorf("k8s exec %q: %w", name, err)
	}
	out, err := p.ops.execInPod(ctx, podName, "agent", argv, nil)
	if err != nil {
		var exitErr execerr.ExitError
		if errors.As(err, &exitErr) && exitErr.Exited() {
			// Ran and exited non-zero: the command's own result, not a
			// transport failure (per the ExecProvider contract).
			return []byte(out), exitErr.ExitStatus(), nil
		}
		return []byte(out), -1, err
	}
	return []byte(out), 0, nil
}

func (p *Provider) findRunningPod(ctx context.Context, name string) (string, error) {
	label := SanitizeLabel(name)
	pods, err := p.ops.listPods(ctx, "gc-session="+label, "status.phase=Running")
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("no running pod for session %q", name)
	}
	return pods[0].Name, nil
}

// findPod finds a pod by session label (any phase).
func (p *Provider) findPod(ctx context.Context, name string) (string, error) {
	label := SanitizeLabel(name)
	pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("no pod for session %q", name)
	}
	return pods[0].Name, nil
}

// waitForDeletion waits for a pod to be deleted.
func waitForDeletion(ctx context.Context, ops k8sOps, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := ops.getPod(ctx, name)
		if err != nil {
			return nil // gone
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("pod %s not deleted after %s", name, timeout)
}

// waitForPodRunning waits for the pod to reach Running phase.
func waitForPodRunning(ctx context.Context, ops k8sOps, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		pod, err := ops.getPod(ctx, name)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return nil
		case corev1.PodFailed:
			return fmt.Errorf("pod %s failed", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("pod %s not running after %s", name, timeout)
}

// waitForTmux waits for the tmux session to be available inside the pod.
func waitForTmux(ctx context.Context, ops k8sOps, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := ops.execInPod(ctx, name, "agent",
			[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("tmux session not ready in pod %s after %s", name, timeout)
}

// initCityInPod copies the city directory and runs gc init inside the pod.
func initCityInPod(ctx context.Context, ops k8sOps, podName, ctrlCity string) error {
	// Copy city dir (excluding .gc/) into the pod.
	if err := copyDirToPod(ctx, ops, podName, "agent", ctrlCity, "/tmp/city-src"); err != nil {
		return err
	}
	// Run gc init --from with GC_DOLT=skip so gc init does not attempt to
	// start a local Dolt server. Pod sessions consume the projected GC_DOLT_*
	// connection target through env; they do not rewrite canonical .beads files.
	_, err := ops.execInPod(ctx, podName, "agent",
		[]string{"env", "GC_DOLT=skip", "gc", "init", "--from", "/tmp/city-src", "/workspace"}, nil)
	if err != nil {
		return err
	}
	// Clean up.
	_, _ = ops.execInPod(ctx, podName, "agent",
		[]string{"rm", "-rf", "/tmp/city-src"}, nil)
	return nil
}

// initBeadsInPod ensures the pod workspace has usable .beads state. It keeps
// the older warning-only self-heal behavior for prebaked or older staged
// workspaces by patching existing metadata and bootstrapping missing state.
func initBeadsInPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, workDir, managedServiceHost, managedServicePort string) error {
	projected, err := projectedPodDoltEnv(cfg.Env, managedServiceHost, managedServicePort)
	if err != nil {
		return err
	}
	if len(projected) == 0 {
		return nil
	}
	doltHost := projected["GC_DOLT_HOST"]
	doltPort := projected["GC_DOLT_PORT"]
	storeRoot := projectedPodStoreRoot(cfg, workDir)
	prefix := strings.TrimSpace(cfg.Env["GC_BEADS_PREFIX"])
	if prefix == "" {
		return fmt.Errorf("missing projected GC_BEADS_PREFIX")
	}

	portNum, err := strconv.Atoi(doltPort)
	if err != nil {
		return fmt.Errorf("invalid projected GC_DOLT_PORT %q: %w", doltPort, err)
	}
	patchJSON, err := json.Marshal(map[string]any{
		"dolt_server_host": doltHost,
		"dolt_server_port": portNum,
	})
	if err != nil {
		return fmt.Errorf("marshaling beads patch: %w", err)
	}
	patchB64 := base64.StdEncoding.EncodeToString(patchJSON)
	prefixB64 := base64.StdEncoding.EncodeToString([]byte(prefix))
	storeRootB64 := base64.StdEncoding.EncodeToString([]byte(storeRoot))

	patchCmd := fmt.Sprintf(
		`WD=$(echo '%s' | base64 -d) && cd "$WD" && PATCH=$(echo '%s' | base64 -d) && `+
			`if [ -f .beads/metadata.json ]; then `+
			`python3 -c "import json,sys; `+
			`m=json.load(open('.beads/metadata.json')); `+
			`p=json.loads(sys.argv[1]); m.update(p); m.pop('project_id', None); `+
			`json.dump(m,open('.beads/metadata.json','w'),indent=2)" "$PATCH" 2>/dev/null || `+
			`printf '%%s' "$PATCH" | python3 -c "import json,sys; `+
			`m=json.load(open('.beads/metadata.json')); `+
			`p=json.loads(sys.stdin.read()); m.update(p); m.pop('project_id', None); `+
			`json.dump(m,open('.beads/metadata.json','w'),indent=2)"; `+
			`else PREFIX=$(echo '%s' | base64 -d) && `+
			`DOLT_HOST=$(echo '%s' | base64 -d) && `+
			`DOLT_PORT=$(echo '%s' | base64 -d) && `+
			`yes | BEADS_DIR="$WD/.beads" bd init --server --server-host "$DOLT_HOST" --server-port "$DOLT_PORT" -p "$PREFIX" --skip-hooks --skip-agents; fi`,
		storeRootB64, patchB64, prefixB64,
		base64.StdEncoding.EncodeToString([]byte(doltHost)),
		base64.StdEncoding.EncodeToString([]byte(doltPort)),
	)
	_, err = ops.execInPod(ctx, podName, "agent", []string{"sh", "-c", patchCmd}, nil)
	return err
}

// verifyBeadsInPod confirms that canonical tracked .beads files are already
// present in the mounted workspace for bd-backed sessions. It intentionally
// does not create or rewrite .beads state inside the pod.
//
//nolint:unparam // tests exercise this helper through the canonical managed service constants.
func verifyBeadsInPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, storeRoot, managedServiceHost, managedServicePort string) error {
	projected, err := projectedPodDoltEnv(cfg.Env, managedServiceHost, managedServicePort)
	if err != nil {
		return err
	}
	if len(projected) == 0 {
		return nil
	}
	_, err = ops.execInPod(ctx, podName, "agent", []string{
		"sh", "-c",
		`cd "$1" && test -f .beads/metadata.json && test -f .beads/config.yaml`,
		"sh", storeRoot,
	}, nil)
	if err != nil {
		return fmt.Errorf("canonical .beads files missing or unreadable at %s: %w", storeRoot, err)
	}
	return nil
}

func buildRESTConfig(k8sContext string) (*rest.Config, error) {
	// Try in-cluster first.
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	// Fall back to kubeconfig.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if k8sContext != "" {
		overrides.CurrentContext = k8sContext
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

func managedServiceAlias() (string, string, error) {
	host := strings.TrimSpace(os.Getenv("GC_K8S_DOLT_HOST"))
	port := strings.TrimSpace(os.Getenv("GC_K8S_DOLT_PORT"))
	switch {
	case host == "" && port == "":
		return podManagedDoltHost, podManagedDoltPort, nil
	case host == "" || port == "":
		return "", "", fmt.Errorf("requires both GC_K8S_DOLT_HOST and GC_K8S_DOLT_PORT when either is set")
	default:
		return host, port, nil
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
