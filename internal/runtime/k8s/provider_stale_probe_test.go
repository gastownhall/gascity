package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
)

// addAgedRunningPod adds a Running pod created far enough in the past that the
// startup grace period has expired — the window in which Start is willing to
// delete a same-name pod it judges stale.
func addAgedRunningPod(fake *fakeK8sOps, name, sessionLabel string) { //nolint:unparam // name varies with the session under test
	fake.pods[name] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Labels:            map[string]string{"app": "gc-agent", "gc-session": sessionLabel},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * startupGracePeriod)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func deletedPods(fake *fakeK8sOps) []string {
	var out []string
	for _, c := range fake.calls {
		if c.method == "deletePod" {
			out = append(out, c.pod)
		}
	}
	return out
}

func staleProbeConfig() runtime.Config {
	return runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env:          map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/workspace"},
	}
}

// TestStartDoesNotDeleteLivePodWhenTmuxProbeCannotAnswer is the ga-vcjr9
// companion guard. Start decides a same-name Running pod is stale by exec'ing
// `tmux has-session` inside it, and it used to read ANY error from that exec as
// "tmux is dead" — including an apiserver/kubelet transport failure that never
// reached the container. Past the startup grace period that verdict deletes the
// pod, so one spurious exec failure destroys a live agent's box and the work in
// it.
//
// This only became load-bearing with identity-stable pool names. While every
// start attempt minted a fresh name the pre-check never found a pod at all;
// now every pool retry re-targets the slot's own name and takes this path.
func TestStartDoesNotDeleteLivePodWhenTmuxProbeCannotAnswer(t *testing.T) {
	transportFailures := map[string]error{
		"dial refused":     errors.New("error dialing backend: dial tcp 10.0.0.7:10250: connect: connection refused"),
		"apiserver 500":    errors.New("error sending request: Internal error occurred: error executing command in container"),
		"stream timeout":   errors.New("exec in pod gc-test-agent: i/o timeout"),
		"context canceled": context.Canceled,
	}

	for label, transportErr := range transportFailures {
		t.Run(label, func(t *testing.T) {
			fake := newFakeK8sOps()
			p := newProviderWithOps(fake)
			addAgedRunningPod(fake, "gc-test-agent", "gc-test-agent")
			fake.setExecResult("gc-test-agent",
				[]string{"tmux", "has-session", "-t", "main"}, "", transportErr)

			err := p.Start(context.Background(), "gc-test-agent", staleProbeConfig())
			if err == nil {
				t.Fatal("Start succeeded on an unanswerable liveness probe; it must defer, not recreate")
			}
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("Start error = %v, want ErrRuntimeUnavailable (the probe could not answer)", err)
			}
			if got := deletedPods(fake); len(got) != 0 {
				t.Fatalf("Start deleted %v on a probe that never reached the pod; a transport failure is not a tmux negative", got)
			}
			for _, c := range fake.calls {
				if c.method == "createPod" {
					t.Fatal("Start created a replacement pod while the incumbent's liveness was unknown")
				}
			}
		})
	}
}

// TestStartDeletesStalePodOnDefinitiveTmuxNegative is the discriminating
// control for the test above. Same pod, same age, same code path — the only
// difference is that the probe genuinely ran inside the container and reported
// no session (a non-zero exit). That IS a tmux negative, and the stale pod must
// still be recreated. Without this control the guard above could be satisfied
// by never deleting anything.
func TestStartDeletesStalePodOnDefinitiveTmuxNegative(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addAgedRunningPod(fake, "gc-test-agent", "gc-test-agent")

	// `tmux has-session` ran and exited 1: no such session. The real
	// execInPod surfaces this as a client-go ExitError, the same shape
	// Provider.Exec already dispatches on.
	probed := 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) > 1 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			probed++
			if probed == 1 {
				return "", execerr.CodeExitError{
					Err:  errors.New("no server running on /tmp/tmux-1000/default"),
					Code: 1,
				}
			}
		}
		return "", nil
	}

	if err := p.Start(context.Background(), "gc-test-agent", staleProbeConfig()); err != nil {
		t.Fatalf("Start on a definitively stale pod: %v", err)
	}
	if got := deletedPods(fake); len(got) == 0 {
		t.Fatal("a definitively dead tmux past the grace period must still recreate the pod")
	}
}

// TestStartRecreatesAPodWhoseAgentContainerIsNotRunning closes the gap the
// probe guard would otherwise open. A pod can be phase Running while its agent
// container is crash-looping or terminated, and every exec into it then fails
// at the apiserver with an error shaped exactly like a transport flake. If that
// read as "I could not tell", a genuinely broken pod would never be replaced —
// the guard would trade one stall for another.
//
// The pod's own status settles it. That is a second observation channel,
// independent of the connection in doubt, so a not-running container is a
// definitive tmux negative and the pod is recreated.
func TestStartRecreatesAPodWhoseAgentContainerIsNotRunning(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addAgedRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.pods["gc-test-agent"].Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "agent",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}},
	}}
	// Every exec fails the way the apiserver fails for a dead container —
	// deliberately not an ExitError, so only the pod status can decide.
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) > 1 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			return "", errors.New("error executing command in container: container is not running")
		}
		return "", nil
	}
	// Block the recreate so the test ends at the decision it is about, the
	// same way TestStartDeletesOldPodWithDeadTmux does.
	fake.createErr = errors.New("intentional: verify deletion only")

	err := p.Start(context.Background(), "gc-test-agent", staleProbeConfig())
	if errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("Start deferred on a pod whose agent container is dead: %v — a broken pod would never be replaced", err)
	}
	if got := deletedPods(fake); len(got) == 0 {
		t.Fatal("a pod whose agent container is not running must be recreated, not deferred forever")
	}
}

// TestStartRetriesAnUnansweredLivenessProbeBeforeGivingUp keeps the guard from
// converting every transient blip into a stalled slot: one flake followed by a
// definitive answer must be resolved on the answer, not on the flake.
func TestStartRetriesAnUnansweredLivenessProbeBeforeGivingUp(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addAgedRunningPod(fake, "gc-test-agent", "gc-test-agent")

	probes := 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) > 1 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			probes++
			if probes == 1 {
				return "", errors.New("error dialing backend: connection reset by peer")
			}
			return "", nil // second probe answers: tmux is alive
		}
		return "", nil
	}

	err := p.Start(context.Background(), "gc-test-agent", staleProbeConfig())
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start error = %v, want ErrSessionExists — the retry answered that the session is live", err)
	}
	if probes < 2 {
		t.Fatalf("liveness probe ran %d time(s); an unanswered probe must be retried before a verdict", probes)
	}
	if got := deletedPods(fake); len(got) != 0 {
		t.Fatalf("Start deleted %v after the retry reported a live session", got)
	}
}
