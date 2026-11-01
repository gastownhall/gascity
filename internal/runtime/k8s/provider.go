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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
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
	cityName           string              // the controller's city name, for the pod's remote-stage record
	nodeSelector       map[string]string   // GC_K8S_NODE_SELECTOR (JSON)
	tolerations        []corev1.Toleration // GC_K8S_TOLERATIONS (JSON)
	affinity           *corev1.Affinity    // GC_K8S_AFFINITY (JSON)
	priorityClassName  string              // GC_K8S_PRIORITY_CLASS_NAME
	postStartSettle    time.Duration       // settle time before post-start liveness check
	stderr             io.Writer           // warning output (default os.Stderr)
	pokes              runtime.PokeLedger  // gc's own input, discounted from GetLastActivity
	now                func() time.Time    // the poke ledger's clock; nil is time.Now
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
	if _, err := resolvePodHostedDoltIdentity(cfg.Env, "", p.managedServiceHost, p.managedServicePort); err != nil {
		return fmt.Errorf("resolving scoped store identity for session %q: %w", name, err)
	}
	ctrlCity := controllerCityPath(cfg.Env)
	var cityIdentity *podHostedDoltIdentity
	if !p.prebaked && ctrlCity != "" {
		identity, err := resolvePodHostedDoltIdentity(cfg.Env, "GC_K8S_CITY_", p.managedServiceHost, p.managedServicePort)
		if err != nil {
			return fmt.Errorf("resolving City store identity for session %q: %w", name, err)
		}
		cityIdentity = identity
	}
	podName := SanitizeName(name)
	label := SanitizeLabel(name)

	// Check for existing pod (any phase).
	existing, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err == nil && len(existing) > 0 {
		pod := &existing[0]
		if pod.Status.Phase == corev1.PodRunning {
			// Stale-pod detection: is the session's tmux server alive?
			alive, definitive, probeErr := p.probeTmuxLiveness(ctx, pod)
			if alive {
				return fmt.Errorf("%w: session %q (pod: %s)", runtime.ErrSessionExists, name, pod.Name)
			}
			// tmux not answering — but if the pod is young, workspace init may
			// still be blocking the tmux server from starting. Don't delete
			// pods that are still within the startup window.
			if time.Since(pod.CreationTimestamp.Time) < startupGracePeriod {
				return fmt.Errorf("%w: session %q (pod: %s)", runtime.ErrSessionInitializing, name, pod.Name)
			}
			if !definitive {
				// Past the grace period the next statement deletes this pod.
				// Only a probe that actually ran inside the container may
				// authorize that: an apiserver or kubelet transport failure
				// says nothing about tmux, and treating it as a negative
				// destroys a live agent's box and the work in it. Report the
				// established "I could not tell" signal so the caller defers
				// instead of recreating.
				return fmt.Errorf("%w: tmux liveness probe for session %q (pod: %s) could not answer: %w",
					runtime.ErrRuntimeUnavailable, name, pod.Name, probeErr)
			}
			// Stale pod — tmux definitively dead and past grace period, recreate.
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

	workDirStaged := false

	if !p.prebaked {
		// Stage files via init container if needed.
		if needsStaging(cfg, ctrlCity) {
			staged, err := stageFiles(ctx, p.ops, podName, cfg, ctrlCity, p.stderr)
			if err != nil {
				cleanup("staging failed")
				return fmt.Errorf("staging files for session %q: %w", name, err)
			}
			workDirStaged = staged
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
			skipRel := cityTemplateSkipRel(workDirStaged, ctrlCity, cfg.WorkDir)
			if err := initCityInPod(ctx, p.ops, podName, ctrlCity, skipRel, cityIdentity); err != nil {
				cleanup("City initialization failed")
				return fmt.Errorf("initializing City in pod for session %q: %w", name, err)
			}
		}
	}

	// Record the pod as a remote stage of the controller's city, staged and
	// prebaked alike (S5-A): on a split city its infrastructure-class
	// commands then refuse instead of creating a pod-local empty binding. A
	// direct write, so it works whatever gc the pod image carries. The pod
	// spec's remote-stage environment already covers the processes started
	// from it (remoteStageEnv); the record is the durable copy, and the only
	// cover for a process started with a cleared environment. Failing to write it fails the start: a pod that cannot be
	// marked is not one this controller hands work to.
	if ctrlCity != "" {
		if err := writeRemoteStageInPod(ctx, p.ops, podName, p.cityName, ctrlCity); err != nil {
			cleanup("remote stage record")
			return fmt.Errorf("recording pod %q as a remote stage of city %s (%s): %w; the pod was removed rather than run unmarked", podName, remoteStageCityName(p.cityName, ctrlCity), ctrlCity, err)
		}
	}

	// The selected scope's hosted identity must be initialized and verified
	// before the entrypoint can run pre_start or start tmux.
	podWorkDir := projectedPodWorkDir(cfg)
	if err := initBeadsInPod(ctx, p.ops, podName, cfg, podWorkDir, p.managedServiceHost, p.managedServicePort); err != nil {
		cleanup("scoped beads initialization failed")
		return fmt.Errorf("initializing scoped beads in pod for session %q: %w", name, err)
	}

	if !p.prebaked {
		if _, err := p.ops.execInPod(ctx, podName, "agent",
			[]string{"touch", "/workspace/.gc-workspace-ready"}, nil); err != nil {
			cleanup("workspace readiness signal failed")
			return fmt.Errorf("signaling workspace ready for session %q: %w", name, err)
		}
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
	// Enable pane logging for diagnostics. No -o: the pipe survives
	// respawn-pane -k, and -o would toggle it off on every other Relaunch;
	// plain pipe-pane replaces any existing pipe, so this is idempotent.
	_, _ = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "pipe-pane", "-t", tmuxSession, "cat >> /tmp/agent-output.log"}, nil)

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
// NOT re-run here (k8s treats PreStart as provision-half); env is provision-half
// too (set in the pod spec at create time, not re-injected — respawn-pane carries
// no env). The remote-stage record is the exception: it is written again before
// the respawn, so a warm pod an older provider created is marked too. NOTE: tmux
// diverges — as of the relaunch pre_start fix it re-runs PreStart on Relaunch
// (launch-half), while k8s and ssh intentionally do not.
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

	// Record the warm pod as a remote stage before the agent respawns, as
	// Start does before its ready sentinel (S5-A): a pod an older provider
	// created carries neither the record nor the pod-spec environment, and
	// the respawned agent must not run unmarked. A failed write fails the
	// relaunch rather than respawning into an unmarked pod.
	if ctrlCity := cfg.Env["GC_CITY"]; ctrlCity != "" {
		if err := writeRemoteStageInPod(ctx, p.ops, podName, p.cityName, ctrlCity); err != nil {
			return fmt.Errorf("k8s relaunch %q: recording pod %q as a remote stage of city %s (%s): %w; the agent was not respawned", name, podName, remoteStageCityName(p.cityName, ctrlCity), ctrlCity, err)
		}
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
// interpreted as keystrokes. Content blocks are flattened to text. The
// delivery is a poke whose keystroke echo GetLastActivity discounts (see
// [Provider.poke]).
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	ctx := context.Background()
	return p.poke(ctx, name, func() error { return p.carrier().Nudge(ctx, name, content) })
}

// SendKeys sends bare keystrokes to the tmux session, as a poke whose
// keystroke echo GetLastActivity discounts (see [Provider.poke]).
// Best-effort on a missing session (contract: no-op), but a genuine
// transport failure to a live pod is propagated (#4389).
func (p *Provider) SendKeys(name string, keys ...string) error {
	ctx := context.Background()
	if err := p.poke(ctx, name, func() error { return p.carrier().SendKeys(ctx, name, keys...) }); err != nil && !errors.Is(err, runtime.ErrSessionNotFound) {
		return err
	}
	return nil
}

// poke runs send, gc's own input to the session, and records it in the poke
// ledger: tmux counts gc's keystrokes as window activity even when the agent
// never answers, so GetLastActivity discounts their echo. The session's raw
// activity is read before send types anything, the genuine baseline, and
// again once send has delivered, the echo: the pod stamps both on its own
// clock, so the echo is matched there and only the grace runs on the
// provider's ([runtime.PokeLedger]).
func (p *Provider) poke(ctx context.Context, name string, send func() error) error {
	commit := p.pokes.Begin(name, p.rawActivity(ctx, name))
	if err := send(); err != nil {
		return err
	}
	commit(p.clock(), p.rawActivity(ctx, name))
	return nil
}

// clock is the poke ledger's time source.
func (p *Provider) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
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

// ListRunningComplete implements [runtime.ListingAttestation]: the apiserver's
// Running-pod list either errors or lists every Running gc-agent pod. The
// listing is complete but not city-scoped: the selector is app=gc-agent in the
// shared namespace, so a same-named session of another city in that namespace
// is listed too and can mask this city's session dying.
func (p *Provider) ListRunningComplete() bool { return true }

// GetLastActivity returns the time of the last I/O in the tmux session (zero
// if unknown), read as the local tmux provider reads it: the newest
// #{window_activity} of the session's windows, less the keystroke echo of
// gc's own nudges and keys (see [Provider.poke]). #{session_activity} is
// pinned at creation/attach time for a detached session and does not advance
// when the agent prints output.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	return p.pokes.Discount(name, p.rawActivity(context.Background(), name), p.clock()), nil
}

// rawActivity reads the session's raw activity clock, the newest
// #{window_activity} of the in-pod tmux session's windows (zero if unknown).
func (p *Provider) rawActivity(ctx context.Context, name string) time.Time {
	activity, err := runtime.ReadTmuxWindowActivity(ctx, p, name, tmuxSession)
	if err != nil {
		return time.Time{}
	}
	return activity
}

// ClearScrollback clears the tmux scrollback buffer (best-effort).
func (p *Provider) ClearScrollback(name string) error {
	_ = p.carrier().ClearScrollback(context.Background(), name) // best-effort
	return nil
}

// Capabilities reports K8s provider capabilities. The K8s provider
// supports activity tracking via tmux window_activity but does not
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

// livenessProbeAttempts is how many times probeTmuxLiveness will ask before
// reporting that it could not tell. The exec path through the apiserver and
// kubelet flakes independently of the pod, and a single blip is not evidence
// about tmux; two unanswered attempts in a row are at least a pattern.
const livenessProbeAttempts = 2

// probeTmuxLiveness asks whether the session's tmux server is alive inside pod.
//
// It returns three-valued, not two: alive, and whether the answer is
// definitive. A definitive answer means something actually established the
// fact — either the pod's own status says the agent container is not running,
// or `tmux has-session` ran inside it and exited. An indefinite answer means
// the exec never got there (SPDY dial failure, apiserver error, stream timeout,
// canceled context), which says nothing at all about tmux.
//
// Collapsing those two into "not alive" is how a transport flake becomes a
// deleted pod. Callers that act destructively on a negative must require
// definitive; callers that only defer may ignore it.
func (p *Provider) probeTmuxLiveness(ctx context.Context, pod *corev1.Pod) (alive, definitive bool, err error) {
	// The pod's status is a second, independent channel, and it settles the
	// case the exec cannot: a pod whose phase is Running but whose agent
	// container is not (crash loop, OOM, terminated) answers every exec with an
	// apiserver-level error that is indistinguishable from a flake. Reading
	// that as "I could not tell" would leave a genuinely broken pod in place
	// forever. The container not running IS a definitive tmux negative, and it
	// comes from the list we already did rather than from the connection that
	// is in doubt.
	if running, known := agentContainerRunning(pod); known && !running {
		return false, true, fmt.Errorf("agent container in pod %s is not running", pod.Name)
	}
	podName := pod.Name
	for attempt := 0; attempt < livenessProbeAttempts; attempt++ {
		_, err = p.ops.execInPod(ctx, podName, "agent",
			[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
		if err == nil {
			return true, true, nil
		}
		var exitErr execerr.ExitError
		if errors.As(err, &exitErr) && exitErr.Exited() {
			// The command ran in the container and reported no session. That
			// is a real tmux negative — the same discrimination Provider.Exec
			// already draws between an exit status and a transport failure.
			return false, true, err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return false, false, err
}

// agentContainerRunning reports whether the pod's agent container is running,
// and whether the pod status said anything about it at all. A status that has
// not been populated yet is not evidence either way.
func agentContainerRunning(pod *corev1.Pod) (running, known bool) {
	if pod == nil {
		return false, false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "agent" {
			return cs.State.Running != nil, true
		}
	}
	return false, false
}

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

// findRunningPod resolves the running pod for name. A missing pod (scaled
// down, evicted, never provisioned) is reported as [runtime.ErrSessionNotFound]
// so callers can distinguish "session is gone" from a genuine transport
// failure reaching a pod that does exist — the same distinction Relaunch
// already draws at its own call site.
func (p *Provider) findRunningPod(ctx context.Context, name string) (string, error) {
	label := SanitizeLabel(name)
	pods, err := p.ops.listPods(ctx, "gc-session="+label, "status.phase=Running")
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("%w: no running pod for session %q", runtime.ErrSessionNotFound, name)
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

type podHostedDoltIdentity struct {
	ProjectID string
	Prefix    string
	Database  string
	Host      string
	Port      string
}

func resolvePodHostedDoltIdentity(env map[string]string, prefix, managedHost, managedPort string) (*podHostedDoltIdentity, error) {
	projectKey, prefixKey, databaseKey := "GC_BEADS_PROJECT_ID", "GC_BEADS_PREFIX", "GC_DOLT_DATABASE"
	if prefix != "" {
		projectKey, prefixKey, databaseKey = prefix+"BEADS_PROJECT_ID", prefix+"BEADS_PREFIX", prefix+"DOLT_DATABASE"
	}
	projected, err := validateProjectedHostedStoreIdentity(env, prefix, managedHost, managedPort)
	if err != nil {
		return nil, err
	}
	if len(projected) == 0 {
		return nil, nil
	}
	return &podHostedDoltIdentity{
		ProjectID: strings.TrimSpace(env[projectKey]),
		Prefix:    strings.TrimSpace(env[prefixKey]),
		Database:  strings.TrimSpace(env[databaseKey]),
		Host:      projected["GC_DOLT_HOST"],
		Port:      projected["GC_DOLT_PORT"],
	}, nil
}

// initCityInPod copies the city directory and runs gc init inside the pod.
func initCityInPod(ctx context.Context, ops k8sOps, podName, ctrlCity, skipRel string, identity *podHostedDoltIdentity) error {
	// Streaming may leave a partial directory if the source or pod exec fails.
	// Use a bounded cleanup context even when the caller has been canceled.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = ops.execInPod(cleanupCtx, podName, "agent", []string{"rm", "-rf", "/tmp/city-src"}, nil)
	}()

	// Copy the city directory into the pod, including its current .gc state.
	if err := copyDirToPodSkipping(ctx, ops, podName, "agent", ctrlCity, "/tmp/city-src", skipRel); err != nil {
		return err
	}
	// Run gc init --from with GC_DOLT=skip so it does not start a local Dolt
	// server. Hosted City identity is passed explicitly because init's scope
	// validation requires the controller-owned project identity.
	// exec inherits the agent container's selected-scope environment, which may
	// describe a hosted rig store rather than the HQ City being initialized.
	// Keep only the runtime environment needed to find gc and its home config,
	// plus the remote-stage identity that fences storage genesis before the
	// on-disk record exists; HQ hosted identity is supplied below through
	// explicit init flags.
	args := []string{
		"sh", "-c",
		`exec env -i PATH="$PATH" HOME="$HOME" GC_DOLT=skip GC_REMOTE_STAGE_CONTROLLER="$GC_REMOTE_STAGE_CONTROLLER" GC_REMOTE_STAGE_CITY="$GC_REMOTE_STAGE_CITY" "$0" "$@"`,
		"gc", "init", "--from", "/tmp/city-src", "/workspace",
	}
	if identity != nil {
		args = append(args,
			"--dolt-host", identity.Host,
			"--dolt-port", identity.Port,
			"--dolt-database", identity.Database,
			"--dolt-project-id", identity.ProjectID,
		)
	}
	args = append(args, "--no-start", "--skip-provider-readiness")
	_, err := ops.execInPod(ctx, podName, "agent", args, nil)
	if err != nil {
		return err
	}
	return nil
}

// remoteStageRecordPath is where a pod's remote-stage record lives: gc reads
// <city>/.gc/remote-stage.toml, and the pod's city root is /workspace in
// both staged and prebaked pods.
const remoteStageRecordPath = podWorkspaceRoot + "/.gc/remote-stage.toml"

// The remote-stage environment cmd/gc's remote_stage.go honors from process
// start. Keep the names in step with cmd/gc (remoteStageControllerEnv,
// remoteStageCityEnv).
const (
	remoteStageControllerEnv = "GC_REMOTE_STAGE_CONTROLLER"
	remoteStageCityEnv       = "GC_REMOTE_STAGE_CITY"
)

// remoteStageCityName is the controller's city name, else its path's base.
func remoteStageCityName(cityName, ctrlCity string) string {
	if strings.TrimSpace(cityName) == "" {
		return filepath.Base(ctrlCity)
	}
	return cityName
}

// remoteStageEnv marks the processes started from a pod's environment as a
// remote stage of ctrlCity, or nothing without a controller city: the
// entrypoint, the agent and its tmux server (re-exported through su for
// LINUX_USERNAME pods: suRemoteStageExports), and kubectl exec'd commands. A
// process started with a deliberately cleared environment relies on the
// on-disk record.
func remoteStageEnv(cityName, ctrlCity string) []corev1.EnvVar {
	if strings.TrimSpace(ctrlCity) == "" {
		return nil
	}
	return []corev1.EnvVar{
		{Name: remoteStageControllerEnv, Value: ctrlCity},
		{Name: remoteStageCityEnv, Value: remoteStageCityName(cityName, ctrlCity)},
	}
}

// remoteStageRecord renders the record cmd/gc's remote_stage.go reads.
func remoteStageRecord(cityName, ctrlCity string, now time.Time) string {
	cityName = remoteStageCityName(cityName, ctrlCity)
	return fmt.Sprintf("controller_city = %q\ncontroller_path = %q\nstaged_at = %q\n", cityName, ctrlCity, now.UTC().Format(time.RFC3339))
}

// writeRemoteStageInPod writes the pod's remote-stage record with a plain
// shell write (temp file, then rename), independent of the pod's gc version.
func writeRemoteStageInPod(ctx context.Context, ops k8sOps, podName, cityName, ctrlCity string) error {
	script := "mkdir -p " + podWorkspaceRoot + "/.gc && cat > " + remoteStageRecordPath + ".tmp && mv -f " + remoteStageRecordPath + ".tmp " + remoteStageRecordPath
	_, err := ops.execInPod(ctx, podName, "agent", []string{"sh", "-c", script}, strings.NewReader(remoteStageRecord(cityName, ctrlCity, time.Now())))
	return err
}

// initBeadsInPod verifies the existing canonical hosted-Dolt scope before
// updating only the pod-local server endpoint mirror. Missing or conflicting
// state fails closed without initializing or mutating the hosted store.
func initBeadsInPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, workDir, managedServiceHost, managedServicePort string) error {
	identity, err := resolvePodHostedDoltIdentity(cfg.Env, "", managedServiceHost, managedServicePort)
	if err != nil {
		return err
	}
	if identity == nil {
		return nil
	}
	storeRoot := projectedPodStoreRoot(cfg, workDir)
	identityPath := shellquote.Quote(contract.ProjectIdentityPath(""))
	portNum, err := strconv.Atoi(identity.Port)
	if err != nil {
		return fmt.Errorf("invalid projected GC_DOLT_PORT %q: %w", identity.Port, err)
	}
	patchJSON, err := json.Marshal(map[string]any{
		"dolt_server_host": identity.Host,
		"dolt_server_port": portNum,
	})
	if err != nil {
		return fmt.Errorf("marshaling beads patch: %w", err)
	}
	patchB64 := base64.StdEncoding.EncodeToString(patchJSON)
	projectIDB64 := base64.StdEncoding.EncodeToString([]byte(identity.ProjectID))
	prefixB64 := base64.StdEncoding.EncodeToString([]byte(identity.Prefix))
	databaseB64 := base64.StdEncoding.EncodeToString([]byte(identity.Database))
	doltHostB64 := base64.StdEncoding.EncodeToString([]byte(identity.Host))
	doltPortB64 := base64.StdEncoding.EncodeToString([]byte(identity.Port))
	storeRootB64 := base64.StdEncoding.EncodeToString([]byte(storeRoot))

	initCmd := fmt.Sprintf(
		`WD=$(echo '%s' | base64 -d) && PROJECT_ID=$(echo '%s' | base64 -d) && PREFIX=$(echo '%s' | base64 -d) && `+
			`DATABASE=$(echo '%s' | base64 -d) && DOLT_HOST=$(echo '%s' | base64 -d) && DOLT_PORT=$(echo '%s' | base64 -d) && `+
			`PATCH=$(echo '%s' | base64 -d) && cd "$WD" || exit 1; `+
			`test -f .beads/metadata.json && test -f `+identityPath+` && test -f .beads/config.yaml || `+
			`{ echo 'hosted Dolt scoped beads files are incomplete' >&2; exit 1; }; `+
			`grep -Fqx "id = \"$PROJECT_ID\"" `+identityPath+` || `+
			`{ echo 'hosted Dolt project identity does not match session scope' >&2; exit 1; }; `+
			`python3 -c 'import json,sys; m=json.load(open(sys.argv[1])); `+
			`assert str(m.get("dolt_database", "")).strip() == sys.argv[2]; `+
			`assert str(m.get("project_id", "")).strip() == sys.argv[3]' `+
			`.beads/metadata.json "$DATABASE" "$PROJECT_ID" || `+
			`{ echo 'hosted Dolt database or project identity does not match session scope' >&2; exit 1; }; `+
			`GOT_PREFIX=$(BEADS_DIR="$WD/.beads" bd config get issue_prefix 2>/dev/null) || exit 1; `+
			`[ "$GOT_PREFIX" = "$PREFIX" ] || { echo 'hosted Dolt beads prefix does not match session scope' >&2; exit 1; }; `+
			`python3 -c "import json,sys; m=json.load(open('.beads/metadata.json')); `+
			`p=json.loads(sys.argv[1]); m.update(p); `+
			`json.dump(m,open('.beads/metadata.json','w'),indent=2)" "$PATCH" 2>/dev/null || `+
			`printf '%%s' "$PATCH" | python3 -c "import json,sys; `+
			`m=json.load(open('.beads/metadata.json')); `+
			`p=json.loads(sys.stdin.read()); m.update(p); `+
			`json.dump(m,open('.beads/metadata.json','w'),indent=2)"`,
		storeRootB64, projectIDB64, prefixB64, databaseB64, doltHostB64, doltPortB64, patchB64,
	)
	_, err = ops.execInPod(ctx, podName, "agent", []string{"sh", "-c", initCmd}, nil)
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
