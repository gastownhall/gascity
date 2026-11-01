package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

func TestProviderImplementsInterface(_ *testing.T) {
	// Compile-time check is in provider.go, but verify at test time too.
	var _ runtime.Provider = (*Provider)(nil)
}

func TestManagedServiceAliasDefaults(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "canonical-dolt.example.com")
	t.Setenv("GC_DOLT_PORT", "4407")

	host, port, err := managedServiceAlias()
	if err != nil {
		t.Fatalf("managedServiceAlias() error = %v", err)
	}
	if host != podManagedDoltHost {
		t.Fatalf("host = %q, want %q", host, podManagedDoltHost)
	}
	if port != podManagedDoltPort {
		t.Fatalf("port = %q, want %q", port, podManagedDoltPort)
	}
}

func TestManagedServiceAliasCompatOverride(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "canonical-dolt.example.com")
	t.Setenv("GC_DOLT_PORT", "4407")
	t.Setenv("GC_K8S_DOLT_HOST", "legacy-dolt.example.com")
	t.Setenv("GC_K8S_DOLT_PORT", "3308")

	host, port, err := managedServiceAlias()
	if err != nil {
		t.Fatalf("managedServiceAlias() error = %v", err)
	}
	if host != "legacy-dolt.example.com" {
		t.Fatalf("host = %q, want legacy-dolt.example.com", host)
	}
	if port != "3308" {
		t.Fatalf("port = %q, want 3308", port)
	}
}

func TestManagedServiceAliasRejectsPartialCompatOverride(t *testing.T) {
	t.Setenv("GC_K8S_DOLT_HOST", "legacy-dolt.example.com")

	_, _, err := managedServiceAlias()
	if err == nil {
		t.Fatal("expected partial compatibility override to fail")
	}
	if got := err.Error(); got != "requires both GC_K8S_DOLT_HOST and GC_K8S_DOLT_PORT when either is set" {
		t.Fatalf("managedServiceAlias() error = %q", got)
	}
}

func TestParseSchedulingEnvHappyPath(t *testing.T) {
	clearSchedulingEnv(t)
	t.Setenv("GC_K8S_NODE_SELECTOR", `{"workload":"gc-agents"}`)
	t.Setenv("GC_K8S_TOLERATIONS", `[{"key":"gc-agents","operator":"Exists","effect":"NoSchedule","tolerationSeconds":60}]`)
	t.Setenv("GC_K8S_AFFINITY", `{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"node-type","operator":"In","values":["gpu"]}]}]}}}`)
	t.Setenv("GC_K8S_PRIORITY_CLASS_NAME", "gc-agent-high")

	scheduling, err := parseSchedulingEnv()
	if err != nil {
		t.Fatalf("parseSchedulingEnv: %v", err)
	}
	if scheduling.nodeSelector["workload"] != "gc-agents" {
		t.Fatalf("nodeSelector[workload] = %q, want gc-agents", scheduling.nodeSelector["workload"])
	}
	if len(scheduling.tolerations) != 1 {
		t.Fatalf("len(tolerations) = %d, want 1", len(scheduling.tolerations))
	}
	if scheduling.tolerations[0].TolerationSeconds == nil || *scheduling.tolerations[0].TolerationSeconds != 60 {
		t.Fatalf("tolerationSeconds = %v, want 60", scheduling.tolerations[0].TolerationSeconds)
	}
	if scheduling.affinity == nil ||
		scheduling.affinity.NodeAffinity == nil ||
		scheduling.affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("affinity did not parse required node affinity: %#v", scheduling.affinity)
	}
	if got := scheduling.priorityClassName; got != "gc-agent-high" {
		t.Fatalf("priorityClassName = %q, want gc-agent-high", got)
	}
}

func TestParseSchedulingEnvRejectsMalformedJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "node selector", key: "GC_K8S_NODE_SELECTOR"},
		{name: "tolerations", key: "GC_K8S_TOLERATIONS"},
		{name: "affinity", key: "GC_K8S_AFFINITY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearSchedulingEnv(t)
			t.Setenv(tc.key, "{")

			_, err := parseSchedulingEnv()
			if err == nil {
				t.Fatal("expected malformed JSON to fail")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error = %q, want to mention %s", err, tc.key)
			}
		})
	}
}

func TestParseSchedulingEnvEmptyAndNullAffinitySemantics(t *testing.T) {
	t.Run("empty strings are unset", func(t *testing.T) {
		clearSchedulingEnv(t)

		scheduling, err := parseSchedulingEnv()
		if err != nil {
			t.Fatalf("parseSchedulingEnv: %v", err)
		}
		if scheduling.nodeSelector != nil {
			t.Fatalf("nodeSelector = %#v, want nil", scheduling.nodeSelector)
		}
		if len(scheduling.tolerations) != 0 {
			t.Fatalf("len(tolerations) = %d, want 0", len(scheduling.tolerations))
		}
		if scheduling.affinity != nil {
			t.Fatalf("affinity = %#v, want nil", scheduling.affinity)
		}
		if scheduling.priorityClassName != "" {
			t.Fatalf("priorityClassName = %q, want empty", scheduling.priorityClassName)
		}
	})

	t.Run("affinity null is unset", func(t *testing.T) {
		clearSchedulingEnv(t)
		t.Setenv("GC_K8S_AFFINITY", "null")

		scheduling, err := parseSchedulingEnv()
		if err != nil {
			t.Fatalf("parseSchedulingEnv: %v", err)
		}
		if scheduling.affinity != nil {
			t.Fatalf("affinity = %#v, want nil", scheduling.affinity)
		}
	})

	t.Run("affinity empty object is explicit empty", func(t *testing.T) {
		clearSchedulingEnv(t)
		t.Setenv("GC_K8S_AFFINITY", "{}")

		scheduling, err := parseSchedulingEnv()
		if err != nil {
			t.Fatalf("parseSchedulingEnv: %v", err)
		}
		if scheduling.affinity == nil {
			t.Fatal("affinity = nil, want explicit empty affinity")
		}
		if scheduling.affinity.NodeAffinity != nil {
			t.Fatalf("NodeAffinity = %#v, want nil", scheduling.affinity.NodeAffinity)
		}
	})
}

func clearSchedulingEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GC_K8S_NODE_SELECTOR",
		"GC_K8S_TOLERATIONS",
		"GC_K8S_AFFINITY",
		"GC_K8S_PRIORITY_CLASS_NAME",
	} {
		t.Setenv(key, "")
	}
}

func TestProjectedPodStoreRootPrefersGCStoreRoot(t *testing.T) {
	cfg := runtime.Config{
		WorkDir: "/host/city/workspaces/agent",
		Env: map[string]string{
			"GC_CITY":       "/host/city",
			"GC_STORE_ROOT": "/host/city/rigs/frontend",
		},
	}

	podWorkDir := projectedPodWorkDir(cfg)
	if podWorkDir != "/workspace/workspaces/agent" {
		t.Fatalf("projectedPodWorkDir = %q, want %q", podWorkDir, "/workspace/workspaces/agent")
	}
	if got := projectedPodStoreRoot(cfg, podWorkDir); got != "/workspace/rigs/frontend" {
		t.Fatalf("projectedPodStoreRoot = %q, want %q", got, "/workspace/rigs/frontend")
	}
}

func TestIsRunning(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// No pod → not running.
	if p.IsRunning("gc-test-agent") {
		t.Error("IsRunning returned true for non-existent session")
	}

	// Pod exists + tmux alive → running.
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent", []string{"tmux", "has-session", "-t", "main"}, "", nil)

	if !p.IsRunning("gc-test-agent") {
		t.Error("IsRunning returned false for running session")
	}

	// Pod exists but tmux dead → not running.
	fake.setExecResult("gc-test-agent", []string{"tmux", "has-session", "-t", "main"}, "",
		fmt.Errorf("no session: main"))

	if p.IsRunning("gc-test-agent") {
		t.Error("IsRunning returned true for session with dead tmux")
	}
}

func TestStop(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Stop non-existent session is idempotent.
	if err := p.Stop("nonexistent"); err != nil {
		t.Fatalf("Stop non-existent: %v", err)
	}

	// Stop existing pod.
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	if err := p.Stop("gc-test-agent"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Verify pod was deleted.
	if _, exists := fake.pods["gc-test-agent"]; exists {
		t.Error("pod still exists after Stop")
	}
}

func TestListRunning(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Empty list.
	names, err := p.ListRunning("gc-test-")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected 0 running, got %d", len(names))
	}

	// Add two running pods with annotations.
	addRunningPodWithAnnotation(fake, "gc-test-mayor", "gc-test-mayor", "gc-test-mayor")
	addRunningPodWithAnnotation(fake, "gc-test-polecat", "gc-test-polecat", "gc-test-polecat")
	addRunningPodWithAnnotation(fake, "gc-other-agent", "gc-other-agent", "gc-other-agent")

	names, err = p.ListRunning("gc-test-")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("expected 2 running with prefix, got %d: %v", len(names), names)
	}

	// Empty prefix returns all.
	names, err = p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning all: %v", err)
	}
	if len(names) != 3 {
		t.Errorf("expected 3 running, got %d", len(names))
	}
}

func TestNudge(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	err := p.Nudge("gc-test-agent", runtime.TextContent("hello world"))
	if err != nil {
		t.Fatalf("Nudge: %v", err)
	}

	// Verify exec was called with literal mode:
	// Call 1: ["tmux", "send-keys", "-t", "main", "-l", "hello world"]
	// Call 2: ["tmux", "send-keys", "-t", "main", "Enter"]
	foundLiteral := false
	foundEnter := false
	for _, c := range fake.calls {
		if c.method != "execInPod" {
			continue
		}
		if len(c.cmd) >= 6 && c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" &&
			c.cmd[4] == "-l" && c.cmd[5] == "hello world" {
			foundLiteral = true
		}
		if len(c.cmd) >= 5 && c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" &&
			c.cmd[4] == "Enter" {
			foundEnter = true
		}
	}
	if !foundLiteral {
		t.Error("no tmux send-keys -l call recorded for Nudge")
	}
	if !foundEnter {
		t.Error("no tmux send-keys Enter call recorded for Nudge")
	}
}

func TestSendKeys(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	err := p.SendKeys("gc-test-agent", "Down", "Enter")
	if err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	// Verify the keys were passed to tmux.
	// Args: ["tmux", "send-keys", "-t", "main", "Down", "Enter"]
	found := false
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 6 {
			if c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" &&
				c.cmd[4] == "Down" && c.cmd[5] == "Enter" {
				found = true
			}
		}
	}
	if !found {
		t.Error("no tmux send-keys call with Down Enter")
	}
}

// TestNudgePropagatesTransportError verifies that a transport failure (no
// running pod for the session) surfaces as a non-nil error instead of being
// swallowed — Nudge is not best-effort at the delivery layer, callers up
// through worker.RuntimeHandle.Nudge and `gc session nudge` rely on this
// error to report failed delivery (#4389). It also verifies the missing-pod
// case is specifically [runtime.ErrSessionNotFound] — distinct from a live
// pod's exec-stream failure — so callers like internal/session/chat.go and
// internal/api/session_resolution.go can no-op on a gone session instead of
// hard-failing (sjarmak's #4405 review).
func TestNudgePropagatesTransportError(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// No pod registered for this session name, so findRunningPod fails.
	err := p.Nudge("gc-missing-agent", runtime.TextContent("hello world"))
	if err == nil {
		t.Fatal("Nudge: expected error for missing pod, got nil")
	}
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Errorf("Nudge missing-pod error = %v, want errors.Is(..., runtime.ErrSessionNotFound)", err)
	}
}

// TestSendKeysMissingSessionIsNoOp verifies SendKeys honors the documented
// best-effort contract (runtime.go SendKeys_MissingSession): a missing pod
// (ErrSessionNotFound at the carrier) is a no-op returning nil, not an error.
// This is the deliberate asymmetry with Nudge (#4389/#4405): SendKeys is
// best-effort on a gone session, while a genuine transport failure to a live
// pod still propagates (see TestSendKeysExecStreamFailureIsNotErrSessionNotFound).
func TestSendKeysMissingSessionIsNoOp(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	err := p.SendKeys("gc-missing-agent", "Down", "Enter")
	if err != nil {
		t.Fatalf("SendKeys: expected nil for missing pod (best-effort contract), got %v", err)
	}
}

// TestNudgeExecStreamFailureIsNotErrSessionNotFound verifies the other half
// of sjarmak's #4405 review: a running pod whose exec stream fails (a real
// transport failure — #4389's actual bug) must NOT be mistaken for a gone
// session. Only the pod-not-found case is ErrSessionNotFound; this failure
// mode must propagate as a plain error so callers correctly treat it as a
// hard failure rather than silently no-opping.
func TestNudgeExecStreamFailureIsNotErrSessionNotFound(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "send-keys", "-t", "main", "-l", "hello world"},
		"", errors.New("stream error: broken pipe"))

	err := p.Nudge("gc-test-agent", runtime.TextContent("hello world"))
	if err == nil {
		t.Fatal("Nudge: expected error for exec-stream failure, got nil")
	}
	if errors.Is(err, runtime.ErrSessionNotFound) {
		t.Errorf("Nudge exec-stream-failure error = %v, must NOT be ErrSessionNotFound (pod exists, this is a real transport failure)", err)
	}
}

// TestSendKeysExecStreamFailureIsNotErrSessionNotFound mirrors
// TestNudgeExecStreamFailureIsNotErrSessionNotFound for SendKeys.
func TestSendKeysExecStreamFailureIsNotErrSessionNotFound(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "send-keys", "-t", "main", "Down", "Enter"},
		"", errors.New("stream error: broken pipe"))

	err := p.SendKeys("gc-test-agent", "Down", "Enter")
	if err == nil {
		t.Fatal("SendKeys: expected error for exec-stream failure, got nil")
	}
	if errors.Is(err, runtime.ErrSessionNotFound) {
		t.Errorf("SendKeys exec-stream-failure error = %v, must NOT be ErrSessionNotFound (pod exists, this is a real transport failure)", err)
	}
}

func TestInterrupt(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Interrupt non-existent session is best-effort.
	if err := p.Interrupt("nonexistent"); err != nil {
		t.Fatalf("Interrupt non-existent: %v", err)
	}

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	if err := p.Interrupt("gc-test-agent"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	// Verify C-c was sent.
	// Args: ["tmux", "send-keys", "-t", "main", "C-c"]
	found := false
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 5 {
			if c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" && c.cmd[4] == "C-c" {
				found = true
			}
		}
	}
	if !found {
		t.Error("no tmux send-keys C-c call recorded")
	}
}

func TestMetaOps(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	// SetMeta.
	if err := p.SetMeta("gc-test-agent", "GC_DRAIN", "true"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	// GetMeta — configure fake to return the value.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "show-environment", "-t", "main", "GC_DRAIN"},
		"GC_DRAIN=true\n", nil)

	val, err := p.GetMeta("gc-test-agent", "GC_DRAIN")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "true" {
		t.Errorf("GetMeta = %q, want %q", val, "true")
	}

	// GetMeta with unset key.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "show-environment", "-t", "main", "MISSING"},
		"-MISSING\n", nil)

	val, err = p.GetMeta("gc-test-agent", "MISSING")
	if err != nil {
		t.Fatalf("GetMeta unset: %v", err)
	}
	if val != "" {
		t.Errorf("GetMeta unset = %q, want empty", val)
	}

	// RemoveMeta.
	if err := p.RemoveMeta("gc-test-agent", "GC_DRAIN"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
}

func TestPeek(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	// Configure fake to return captured output.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "capture-pane", "-t", "main", "-p", "-S", "-50"},
		"line1\nline2\nline3\n", nil)

	output, err := p.Peek("gc-test-agent", 50)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if output != "line1\nline2\nline3\n" {
		t.Errorf("Peek output = %q, want lines", output)
	}
}

func TestGetLastActivity(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	// Detached tmux advances #{window_activity} on pane output but pins
	// #{session_activity} at creation/attach time, so the output clock is the
	// one GetLastActivity must read: the newest window's, as the local tmux
	// provider reads it, not just the current window's.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "list-windows", "-t", "main", "-F", "#{window_activity}"},
		"1709200000\n1709300000\n", nil)
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "display-message", "-t", "main", "-p", "#{window_activity}"},
		"1709200000\n", nil)
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "display-message", "-t", "main", "-p", "#{session_activity}"},
		"1600000000\n", nil)

	activity, err := p.GetLastActivity("gc-test-agent")
	if err != nil {
		t.Fatalf("GetLastActivity: %v", err)
	}
	want := time.Unix(1709300000, 0)
	if !activity.Equal(want) {
		t.Errorf("GetLastActivity = %v, want %v", activity, want)
	}

	// Non-existent session returns zero time.
	activity, err = p.GetLastActivity("nonexistent")
	if err != nil {
		t.Fatalf("GetLastActivity nonexistent: %v", err)
	}
	if !activity.IsZero() {
		t.Errorf("expected zero time, got %v", activity)
	}
}

// pokedPod is a running pod whose in-pod tmux reports an activity clock that
// gc's own keystrokes advance, as tmux counts them as window activity: every
// send-keys moves it to the pod's time, which runs skew off the provider's
// clock (now). sendErr fails the send-keys whose last argument it names, after
// the keystrokes reached the pane; refuse makes tmux reject every send-keys: a
// non-zero exit, with nothing typed.
type pokedPod struct {
	activity, now time.Time
	skew          time.Duration
	sendErr       map[string]error
	refuse        bool
}

func newPokedPod(t *testing.T, genuine time.Time, skew time.Duration) (*Provider, *pokedPod) {
	t.Helper()
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	pod := &pokedPod{activity: genuine, now: genuine.Add(5 * time.Hour), skew: skew}
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) < 2 || cmd[0] != "tmux" {
			return "", nil
		}
		switch cmd[1] {
		case "list-windows":
			return strconv.FormatInt(pod.activity.Unix(), 10) + "\n", nil
		case "send-keys":
			if pod.refuse {
				return "", execerr.CodeExitError{Err: errors.New("can't find pane"), Code: 1}
			}
			pod.activity = pod.now.Add(pod.skew)
			return "", pod.sendErr[cmd[len(cmd)-1]]
		}
		return "", nil
	}
	p.now = func() time.Time { return pod.now }
	return p, pod
}

// TestPokeEchoIsDiscountedFromLastActivity: a nudge or keys advance
// #{window_activity} with their own keystroke echo even when the agent never
// answers. GetLastActivity counts the echo while the agent may still reply,
// reverts to the activity before the poke once the grace passes unanswered,
// so a nudged but silent agent does not look perpetually active (#3049,
// #4187), and keeps the agent's real output that follows. The pod's clock is
// not the provider's: with it ahead or behind, the echo is still matched and
// a reply 10s after the poke is still the agent's own.
func TestPokeEchoIsDiscountedFromLastActivity(t *testing.T) {
	for _, tc := range []struct {
		name string
		poke func(*Provider) error
	}{
		{"Nudge", func(p *Provider) error { return p.Nudge("gc-test-agent", runtime.TextContent("wake up")) }},
		{"SendKeys", func(p *Provider) error { return p.SendKeys("gc-test-agent", "Enter") }},
	} {
		for _, skew := range []time.Duration{0, -10 * time.Second, 10 * time.Second} {
			t.Run(tc.name+" pod clock "+skew.String(), func(t *testing.T) {
				genuine := time.Unix(1709300000, 0) // the agent's last real output
				p, pod := newPokedPod(t, genuine, skew)
				if err := tc.poke(p); err != nil {
					t.Fatalf("poke: %v", err)
				}
				poked, echo := pod.now, pod.activity

				if got, _ := p.GetLastActivity("gc-test-agent"); !got.Equal(echo) {
					t.Errorf("inside the grace window LastActivity = %v, want the echo %v", got, echo)
				}
				pod.now = poked.Add(runtime.PokeGrace + time.Second)
				if got, _ := p.GetLastActivity("gc-test-agent"); !got.Equal(genuine) {
					t.Errorf("unanswered poke after the grace: LastActivity = %v, want the genuine %v", got, genuine)
				}
				pod.activity = echo.Add(10 * time.Second) // the agent answers
				pod.now = poked.Add(time.Minute)
				if got, _ := p.GetLastActivity("gc-test-agent"); !got.Equal(pod.activity) {
					t.Errorf("after the agent answered LastActivity = %v, want its output %v", got, pod.activity)
				}
			})
		}
	}
}

// TestUndeliveredNudgeIsNotDiscounted: a nudge whose submit never lands
// reports the failure and records no poke, so its echo is not discounted:
// only input gc delivered is gc's to take back.
func TestUndeliveredNudgeIsNotDiscounted(t *testing.T) {
	p, pod := newPokedPod(t, time.Unix(1709300000, 0), 0)
	pod.sendErr = map[string]error{"Enter": errors.New("stream error: broken pipe")}
	if err := p.Nudge("gc-test-agent", runtime.TextContent("wake up")); err == nil {
		t.Fatal("Nudge: want the failed submit reported, got nil")
	}
	echo := pod.now

	pod.now = echo.Add(runtime.PokeGrace + time.Second)
	if got, _ := p.GetLastActivity("gc-test-agent"); !got.Equal(echo) {
		t.Errorf("LastActivity = %v, want the raw clock %v: an undelivered nudge records no poke", got, echo)
	}
}

// TestPokeThatTypedNothingKeepsBaseline: a nudge with no text, keys with
// none, or a send-keys tmux refuses types nothing into the pane, so it must
// not cost an earlier unanswered poke its genuine baseline: once the grace
// passes, LastActivity is still the activity from before the first poke.
func TestPokeThatTypedNothingKeepsBaseline(t *testing.T) {
	for _, tc := range []struct {
		name string
		poke func(*Provider, *pokedPod) error
	}{
		{"empty Nudge", func(p *Provider, _ *pokedPod) error {
			return p.Nudge("gc-test-agent", runtime.TextContent(""))
		}},
		{"no keys", func(p *Provider, _ *pokedPod) error { return p.SendKeys("gc-test-agent") }},
		{"refused send-keys", func(p *Provider, pod *pokedPod) error {
			pod.refuse = true
			return p.SendKeys("gc-test-agent", "Enter")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			genuine := time.Unix(1709300000, 0) // the agent's last real output
			p, pod := newPokedPod(t, genuine, 0)
			if err := p.Nudge("gc-test-agent", runtime.TextContent("wake up")); err != nil {
				t.Fatalf("Nudge: %v", err)
			}
			pod.now = pod.now.Add(time.Minute) // far past that nudge's echo window
			if err := tc.poke(p, pod); err != nil {
				t.Fatalf("poke: %v", err)
			}

			pod.now = pod.now.Add(runtime.PokeGrace + time.Second)
			if got, _ := p.GetLastActivity("gc-test-agent"); !got.Equal(genuine) {
				t.Errorf("LastActivity = %v, want the genuine %v from before the first poke", got, genuine)
			}
		})
	}
}

func TestClearScrollback(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	if err := p.ClearScrollback("gc-test-agent"); err != nil {
		t.Fatalf("ClearScrollback: %v", err)
	}

	found := false
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 3 {
			if c.cmd[0] == "tmux" && c.cmd[1] == "clear-history" {
				found = true
			}
		}
	}
	if !found {
		t.Error("no tmux clear-history call recorded")
	}
}

func TestProcessAlive(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Empty process names → always true.
	if !p.ProcessAlive("any", nil) {
		t.Error("ProcessAlive with nil names should return true")
	}

	// No pod → false.
	if p.ProcessAlive("nonexistent", []string{"claude"}) {
		t.Error("ProcessAlive returned true for non-existent pod")
	}

	// Pod with process running.
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent", []string{"pgrep", "-f", "claude"}, "1234\n", nil)

	if !p.ProcessAlive("gc-test-agent", []string{"claude"}) {
		t.Error("ProcessAlive returned false when process is running")
	}

	// Pod being deleted (has deletionTimestamp).
	now := metav1.Now()
	fake.pods["gc-test-agent"].DeletionTimestamp = &now

	if p.ProcessAlive("gc-test-agent", []string{"claude"}) {
		t.Error("ProcessAlive returned true for terminating pod")
	}
}

func TestStartRequiresImage(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.image = "" // no image

	err := p.Start(context.Background(), "test", runtime.Config{})
	if err == nil {
		t.Fatal("Start should fail without image")
	}
	if want := "GC_K8S_IMAGE is required"; !contains(err.Error(), want) {
		t.Errorf("error = %q, want containing %q", err, want)
	}
}

func TestStartCreatesPodsAndWaits(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Configure fake to make tmux has-session succeed immediately.
	// The fake createPod sets phase=Running automatically.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command:      "claude --settings .gc/settings.json",
		ProcessNames: []string{"claude"},
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  "/workspace",
		},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Verify pod was created.
	if _, exists := fake.pods["gc-test-agent"]; !exists {
		t.Error("pod not created")
	}

	// Verify labels on the created pod.
	pod := fake.pods["gc-test-agent"]
	if pod.Labels["app"] != "gc-agent" {
		t.Errorf("label app = %q, want gc-agent", pod.Labels["app"])
	}
	if pod.Labels["gc-session"] != "gc-test-agent" {
		t.Errorf("label gc-session = %q, want gc-test-agent", pod.Labels["gc-session"])
	}
	if pod.Annotations["gc-session-name"] != "gc-test-agent" {
		t.Errorf("annotation gc-session-name = %q, want gc-test-agent", pod.Annotations["gc-session-name"])
	}
}

func TestStartDetectsStalePod(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Add a stale pod in Failed phase. This avoids the tmux liveness check
	// (only done for Running pods) and goes straight to delete+recreate.
	fake.pods["gc-test-agent"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gc-test-agent",
			Labels: map[string]string{"app": "gc-agent", "gc-session": "gc-test-agent"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}

	// After deletion and recreation, tmux works.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  "/workspace",
		},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start with stale pod: %v", err)
	}

	// Verify deletePod was called (to remove stale pod).
	found := false
	for _, c := range fake.calls {
		if c.method == "deletePod" && c.pod == "gc-test-agent" {
			found = true
		}
	}
	if !found {
		t.Error("stale pod was not deleted before recreation")
	}
}

func TestStartRejectsExistingLiveSession(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Pre-existing pod with live tmux.
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env:          map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/workspace"},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err == nil {
		t.Fatal("Start should fail for existing live session")
	}
	if want := "already exists"; !contains(err.Error(), want) {
		t.Errorf("error = %q, want containing %q", err, want)
	}
}

func TestStartTreatsYoungPodWithDeadTmuxAsInitializing(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Pod created recently — still within startup grace period.
	fake.pods["gc-test-agent"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "gc-test-agent",
			Labels:            map[string]string{"app": "gc-agent", "gc-session": "gc-test-agent"},
			CreationTimestamp: metav1.Now(),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	// tmux not up yet (workspace init still blocking).
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "",
		fmt.Errorf("no server running on /tmp/tmux-1000/default"))

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env:          map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/workspace"},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err == nil {
		t.Fatal("Start should return error for initializing pod")
	}
	if !errors.Is(err, runtime.ErrSessionInitializing) {
		t.Errorf("error = %v, want ErrSessionInitializing", err)
	}

	// Must NOT have deleted the pod — it's still initializing.
	for _, c := range fake.calls {
		if c.method == "deletePod" && c.pod == "gc-test-agent" {
			t.Error("young pod was deleted despite still initializing")
		}
	}
}

func TestStartDeletesOldPodWithDeadTmux(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Pod created long ago — well past the startup grace period.
	fake.pods["gc-test-agent"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "gc-test-agent",
			Labels:            map[string]string{"app": "gc-agent", "gc-session": "gc-test-agent"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	// tmux dead — genuinely stale. `tmux has-session` RAN in the container and
	// exited 1, which is what makes this a stale pod rather than an
	// unreachable one; the real execInPod surfaces that as a client-go
	// ExitError wrapping tmux's stderr, and Start now requires that
	// distinction before it will delete a Running pod (ga-vcjr9).
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "",
		execerr.CodeExitError{Err: fmt.Errorf("no server running on /tmp/tmux-1000/default"), Code: 1})

	// Block createPod so Start() stops after deletion — we only need to
	// verify the stale pod was cleaned up, not the full startup.
	fake.createErr = fmt.Errorf("intentional: verify deletion only")

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  "/workspace",
		},
	}
	_ = p.Start(context.Background(), "gc-test-agent", cfg)

	// Must have deleted the stale pod.
	found := false
	for _, c := range fake.calls {
		if c.method == "deletePod" && c.pod == "gc-test-agent" {
			found = true
		}
	}
	if !found {
		t.Error("old stale pod was not deleted before recreation")
	}
}

func TestPodManifestCompatibility(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/demo-rig",
		Env: map[string]string{
			"GC_AGENT": "demo-rig/polecat",
			"GC_CITY":  "/city",
		},
	}

	pod, err := buildPod("gc-bright-demo-rig-polecat", cfg, p)
	if err != nil {
		t.Fatal(err)
	}

	// Container name must be "agent".
	if pod.Spec.Containers[0].Name != "agent" {
		t.Errorf("container name = %q, want %q", pod.Spec.Containers[0].Name, "agent")
	}

	// Init container name must be "stage" (when staging needed).
	if len(pod.Spec.InitContainers) == 0 {
		t.Fatal("expected init container for rig agent")
	}
	if pod.Spec.InitContainers[0].Name != "stage" {
		t.Errorf("init container name = %q, want %q", pod.Spec.InitContainers[0].Name, "stage")
	}

	// Labels must match gc-session-k8s format.
	if pod.Labels["app"] != "gc-agent" {
		t.Errorf("label app = %q, want gc-agent", pod.Labels["app"])
	}

	// Verify volume names.
	volNames := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		volNames[v.Name] = true
	}
	for _, name := range []string{"ws", "claude-config", "city"} {
		if !volNames[name] {
			t.Errorf("missing volume %q", name)
		}
	}

	// The manifest's workingDir is the workspace root, which always exists —
	// the kubelet chdirs there before the entrypoint runs. The pod-mapped agent
	// directory is entered by the entrypoint instead. gc-session-k8s builds its
	// manifest the same way, so the two providers stay interchangeable.
	if pod.Spec.Containers[0].WorkingDir != podWorkspaceRoot {
		t.Errorf("workingDir = %q, want %q",
			pod.Spec.Containers[0].WorkingDir, podWorkspaceRoot)
	}
	if args := strings.Join(pod.Spec.Containers[0].Args, " "); !strings.Contains(args, "cd "+shellquote.Quote("/workspace/demo-rig")) {
		t.Errorf("entrypoint should enter the pod-mapped agent dir; got: %s", args)
	}
}

func TestWorkspaceVolumeMountsAtRoot(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())

	tests := []struct {
		name    string
		workDir string
	}{
		{"default workspace", "/city"},
		{"rig subdirectory", "/city/demo-rig"},
		{"deep gc subdirectory", "/city/.gc/agents/deacon"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := runtime.Config{
				Command: "claude",
				WorkDir: tt.workDir,
				Env: map[string]string{
					"GC_AGENT": "test/agent",
					"GC_CITY":  "/city",
				},
			}

			pod, err := buildPod("gc-test-agent", cfg, p)
			if err != nil {
				t.Fatal(err)
			}

			for _, vm := range pod.Spec.Containers[0].VolumeMounts {
				if vm.Name == "ws" {
					if vm.MountPath != "/workspace" {
						t.Errorf("ws volume MountPath = %q, want /workspace", vm.MountPath)
					}
					return
				}
			}
			// ws volume not found — only expected for prebaked
			if !p.prebaked {
				t.Error("ws volume mount not found on agent container")
			}
		})
	}
}

func mustBuildPodEnv(t *testing.T, cfgEnv map[string]string, podWorkDir, managedServiceHost, managedServicePort string) []corev1.EnvVar {
	t.Helper()
	env, err := buildPodEnv(cfgEnv, podWorkDir, managedServiceHost, managedServicePort)
	if err != nil {
		t.Fatalf("buildPodEnv: %v", err)
	}
	return env
}

func TestBuildPodEnvRemapsVars(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":                            "mayor",
		"GC_CITY":                             "/host/city",
		"GC_CITY_PATH":                        "/host/city",
		"GC_DIR":                              "/host/city/rig",
		"GC_RIG_ROOT":                         "/host/city/rig",
		"GC_STORE_ROOT":                       "/host/city/rig",
		"BEADS_DIR":                           "/host/city/rig/.beads",
		"GT_ROOT":                             "/host/city",
		"GC_CITY_RUNTIME_DIR":                 "/host/city/.gc/runtime",
		"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": "/host/city/.gc/runtime/control-dispatcher-trace.log",
		"GC_PACK_STATE_DIR":                   "/host/city/.gc/runtime/packs/rlm",
		"GC_PACK_DIR":                         "/host/city/packs/maintenance",
		"GC_SESSION":                          "exec:gc-session-k8s",
		"GC_BEADS":                            "exec:something",
		"GC_EVENTS":                           "exec:other",
		"GC_DOLT_HOST":                        "",
		"GC_DOLT_PORT":                        "3307",
		"BEADS_DOLT_SERVER_HOST":              "",
		"BEADS_DOLT_SERVER_PORT":              "3307",
		"GC_K8S_DOLT_HOST":                    "legacy-dolt.example.com",
		"GC_K8S_DOLT_PORT":                    "3308",
		"GC_DOLT_USER":                        "admin",
		"GC_DOLT_PASSWORD":                    "secret",
		"BEADS_DOLT_SERVER_USER":              "admin",
		"BEADS_DOLT_PASSWORD":                 "secret",
		"GC_MAIL":                             "exec:mail",
		"GC_MCP_MAIL_URL":                     "http://localhost:8765",
		"CUSTOM_VAR":                          "preserved",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace/rig", podManagedDoltHost, podManagedDoltPort)

	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	// GC_CITY should be remapped to /workspace.
	if envMap["GC_CITY"] != "/workspace" {
		t.Errorf("GC_CITY = %q, want /workspace", envMap["GC_CITY"])
	}
	if envMap["GC_CITY_PATH"] != "/workspace" {
		t.Errorf("GC_CITY_PATH = %q, want /workspace", envMap["GC_CITY_PATH"])
	}

	// GC_DIR should be remapped to pod work dir.
	if envMap["GC_DIR"] != "/workspace/rig" {
		t.Errorf("GC_DIR = %q, want /workspace/rig", envMap["GC_DIR"])
	}

	// GC_RIG_ROOT should be remapped from controller city path to /workspace.
	if envMap["GC_RIG_ROOT"] != "/workspace/rig" {
		t.Errorf("GC_RIG_ROOT = %q, want /workspace/rig", envMap["GC_RIG_ROOT"])
	}

	// GC_STORE_ROOT should be remapped from controller city path to /workspace.
	if envMap["GC_STORE_ROOT"] != "/workspace/rig" {
		t.Errorf("GC_STORE_ROOT = %q, want /workspace/rig", envMap["GC_STORE_ROOT"])
	}

	// BEADS_DIR should be remapped from controller city path to /workspace.
	if envMap["BEADS_DIR"] != "/workspace/rig/.beads" {
		t.Errorf("BEADS_DIR = %q, want /workspace/rig/.beads", envMap["BEADS_DIR"])
	}

	// GT_ROOT should be remapped from controller city path to /workspace.
	if envMap["GT_ROOT"] != "/workspace" {
		t.Errorf("GT_ROOT = %q, want /workspace", envMap["GT_ROOT"])
	}

	// GC_CITY_RUNTIME_DIR should be remapped.
	if envMap["GC_CITY_RUNTIME_DIR"] != "/workspace/.gc/runtime" {
		t.Errorf("GC_CITY_RUNTIME_DIR = %q, want /workspace/.gc/runtime", envMap["GC_CITY_RUNTIME_DIR"])
	}

	// GC_CONTROL_DISPATCHER_TRACE_DEFAULT should be remapped.
	if envMap["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"] != "/workspace/.gc/runtime/control-dispatcher-trace.log" {
		t.Errorf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want /workspace/.gc/runtime/control-dispatcher-trace.log", envMap["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"])
	}

	// GC_PACK_STATE_DIR should be remapped.
	if envMap["GC_PACK_STATE_DIR"] != "/workspace/.gc/runtime/packs/rlm" {
		t.Errorf("GC_PACK_STATE_DIR = %q, want /workspace/.gc/runtime/packs/rlm", envMap["GC_PACK_STATE_DIR"])
	}

	// GC_PACK_DIR should be remapped.
	if envMap["GC_PACK_DIR"] != "/workspace/packs/maintenance" {
		t.Errorf("GC_PACK_DIR = %q, want /workspace/packs/maintenance", envMap["GC_PACK_DIR"])
	}

	// Controller-only vars should be removed. The pod adapter reprojects the
	// canonical GC target and derives the BEADS host/port mirror from it.
	for _, key := range []string{"GC_SESSION", "GC_BEADS", "GC_EVENTS", "GC_K8S_DOLT_HOST", "GC_K8S_DOLT_PORT"} {
		if _, exists := envMap[key]; exists {
			t.Errorf("controller-only var %s should be removed", key)
		}
	}
	// Canonical Dolt connection vars should remain present, and local/controller
	// endpoints should be reprojected to the in-cluster managed service target.
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD", "BEADS_DOLT_SERVER_USER", "BEADS_DOLT_PASSWORD"} {
		if _, exists := envMap[key]; !exists {
			t.Errorf("connection var %s should be preserved in agent pods", key)
		}
	}
	if envMap["GC_DOLT_HOST"] != podManagedDoltHost {
		t.Errorf("GC_DOLT_HOST = %q, want %q", envMap["GC_DOLT_HOST"], podManagedDoltHost)
	}
	if envMap["GC_DOLT_PORT"] != podManagedDoltPort {
		t.Errorf("GC_DOLT_PORT = %q, want %q", envMap["GC_DOLT_PORT"], podManagedDoltPort)
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != podManagedDoltHost {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", envMap["BEADS_DOLT_SERVER_HOST"], podManagedDoltHost)
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != podManagedDoltPort {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", envMap["BEADS_DOLT_SERVER_PORT"], podManagedDoltPort)
	}

	// Mail vars should be passed through to agent pods.
	if envMap["GC_MAIL"] != "exec:mail" {
		t.Errorf("GC_MAIL = %q, want exec:mail", envMap["GC_MAIL"])
	}
	if envMap["GC_MCP_MAIL_URL"] != "http://localhost:8765" {
		t.Errorf("GC_MCP_MAIL_URL = %q, want http://localhost:8765", envMap["GC_MCP_MAIL_URL"])
	}

	// Custom vars should be preserved.
	if envMap["CUSTOM_VAR"] != "preserved" {
		t.Errorf("CUSTOM_VAR = %q, want preserved", envMap["CUSTOM_VAR"])
	}

	// GC_TMUX_SESSION should be added.
	if envMap["GC_TMUX_SESSION"] != "main" {
		t.Errorf("GC_TMUX_SESSION = %q, want main", envMap["GC_TMUX_SESSION"])
	}
}

func TestBuildPodEnvReprojectsExternalRuntimeRoots(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_CITY":                             "/host/city",
		"GC_CITY_PATH":                        "/host/city",
		"GC_CITY_RUNTIME_DIR":                 "/var/tmp/gascity-runtime",
		"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": "/var/tmp/gascity-runtime/control-dispatcher-trace.log",
		"GC_PACK_STATE_DIR":                   "/var/tmp/gascity-runtime/packs/rlm",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", podManagedDoltHost, podManagedDoltPort)

	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_CITY_RUNTIME_DIR"] != "/workspace/.gc/runtime" {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want /workspace/.gc/runtime", envMap["GC_CITY_RUNTIME_DIR"])
	}
	if envMap["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"] != "/workspace/.gc/runtime/control-dispatcher-trace.log" {
		t.Fatalf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want /workspace/.gc/runtime/control-dispatcher-trace.log", envMap["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"])
	}
	if envMap["GC_PACK_STATE_DIR"] != "/workspace/.gc/runtime/packs/rlm" {
		t.Fatalf("GC_PACK_STATE_DIR = %q, want /workspace/.gc/runtime/packs/rlm", envMap["GC_PACK_STATE_DIR"])
	}
}

func TestBuildPodEnvProjectsManagedDoltEndpoint(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":               "worker",
		"GC_DOLT_HOST":           "",
		"GC_DOLT_PORT":           "4123",
		"BEADS_DOLT_SERVER_HOST": "",
		"BEADS_DOLT_SERVER_PORT": "4123",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", podManagedDoltHost, podManagedDoltPort)
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_DOLT_HOST"] != podManagedDoltHost {
		t.Errorf("GC_DOLT_HOST = %q, want %q", envMap["GC_DOLT_HOST"], podManagedDoltHost)
	}
	if envMap["GC_DOLT_PORT"] != podManagedDoltPort {
		t.Errorf("GC_DOLT_PORT = %q, want %q", envMap["GC_DOLT_PORT"], podManagedDoltPort)
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != podManagedDoltHost {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", envMap["BEADS_DOLT_SERVER_HOST"], podManagedDoltHost)
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != podManagedDoltPort {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", envMap["BEADS_DOLT_SERVER_PORT"], podManagedDoltPort)
	}
}

func TestBuildPodEnvProjectsManagedLocalDoltTarget(t *testing.T) {
	env := mustBuildPodEnv(t, map[string]string{
		"GC_AGENT":         "worker",
		"GC_DOLT_PORT":     "31364",
		"GC_K8S_DOLT_HOST": "legacy-dolt.example.com",
		"GC_K8S_DOLT_PORT": "3309",
	}, "/workspace", podManagedDoltHost, podManagedDoltPort)

	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_DOLT_HOST"] != podManagedDoltHost {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", envMap["GC_DOLT_HOST"], podManagedDoltHost)
	}
	if envMap["GC_DOLT_PORT"] != podManagedDoltPort {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", envMap["GC_DOLT_PORT"], podManagedDoltPort)
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != podManagedDoltHost {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want %q", envMap["BEADS_DOLT_SERVER_HOST"], podManagedDoltHost)
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != podManagedDoltPort {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", envMap["BEADS_DOLT_SERVER_PORT"], podManagedDoltPort)
	}
}

// TestBuildPodEnvProjectsWithheldManagedLocalDoltTarget pins the pod half of
// the gastownhall/gascity#1656 withhold. A managed-direct session whose
// controller-local port gc withheld, so host-local bd follows the scope's
// .beads/dolt-server.port mirror, carries neither host nor port, only the
// managed-local marker. A pod cannot read the controller's mirror, so it must
// still reach the in-cluster managed service exactly as the port-only shape
// does, and the controller-side marker must stay out of the pod.
func TestBuildPodEnvProjectsWithheldManagedLocalDoltTarget(t *testing.T) {
	cfgEnv := runtimetest.WithheldManagedDoltEnv()
	cfgEnv["GC_AGENT"] = "worker"

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", podManagedDoltHost, podManagedDoltPort)
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	for key, want := range map[string]string{
		"GC_DOLT_HOST":           podManagedDoltHost,
		"GC_DOLT_PORT":           podManagedDoltPort,
		"BEADS_DOLT_SERVER_HOST": podManagedDoltHost,
		"BEADS_DOLT_SERVER_PORT": podManagedDoltPort,
	} {
		if got := envMap[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if got, ok := envMap["GC_DOLT_MANAGED_LOCAL"]; ok {
		t.Fatalf("GC_DOLT_MANAGED_LOCAL = %q reached the pod; the pod's target is the in-cluster service, not a controller-local server", got)
	}
}

// TestBuildPodEnvOmitsDoltForEmptyTargetWithoutManagedLocalMarker guards the
// other side of that marker: empty host and port without GC_DOLT_MANAGED_LOCAL
// is a scope without Dolt or a failed resolution, which leaves the same empty
// keys, and must project nothing rather than the managed alias.
func TestBuildPodEnvOmitsDoltForEmptyTargetWithoutManagedLocalMarker(t *testing.T) {
	cfgEnv := runtimetest.WithheldManagedDoltEnv()
	cfgEnv["GC_AGENT"] = "worker"
	cfgEnv["GC_DOLT_MANAGED_LOCAL"] = ""

	for _, e := range mustBuildPodEnv(t, cfgEnv, "/workspace", podManagedDoltHost, podManagedDoltPort) {
		switch e.Name {
		case "GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT":
			t.Fatalf("%s = %q projected for a session env without a Dolt target", e.Name, e.Value)
		}
	}
}

func TestBuildPodEnvRejectsHostOnlyProjectedTarget(t *testing.T) {
	_, err := buildPodEnv(map[string]string{
		"GC_AGENT":     "worker",
		"GC_DOLT_HOST": "canonical-dolt.example.com",
	}, "/workspace", podManagedDoltHost, podManagedDoltPort)
	if err == nil {
		t.Fatal("expected host-only GC_DOLT_* projection to fail")
	}
	if got := err.Error(); got != "requires both GC_DOLT_HOST and GC_DOLT_PORT when GC_DOLT_HOST is set" {
		t.Fatalf("buildPodEnv error = %q", got)
	}
}

func TestBuildPodEnvPreservesExplicitDoltVars(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":               "worker",
		"GC_DOLT_HOST":           "custom-dolt.example.com",
		"GC_DOLT_PORT":           "3308",
		"BEADS_DOLT_SERVER_HOST": "custom-dolt.example.com",
		"BEADS_DOLT_SERVER_PORT": "3308",
		"GC_K8S_DOLT_HOST":       "legacy-dolt.example.com",
		"GC_K8S_DOLT_PORT":       "3309",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", podManagedDoltHost, podManagedDoltPort)

	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	// Explicit canonical values should pass through unchanged and the legacy
	// K8s-only aliases should be stripped.
	if envMap["GC_DOLT_HOST"] != "custom-dolt.example.com" {
		t.Errorf("GC_DOLT_HOST = %q, want custom-dolt.example.com", envMap["GC_DOLT_HOST"])
	}
	if envMap["GC_DOLT_PORT"] != "3308" {
		t.Errorf("GC_DOLT_PORT = %q, want 3308", envMap["GC_DOLT_PORT"])
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != "custom-dolt.example.com" {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want custom-dolt.example.com", envMap["BEADS_DOLT_SERVER_HOST"])
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != "3308" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want 3308", envMap["BEADS_DOLT_SERVER_PORT"])
	}
	if _, exists := envMap["GC_K8S_DOLT_HOST"]; exists {
		t.Error("GC_K8S_DOLT_HOST should be stripped")
	}
	if _, exists := envMap["GC_K8S_DOLT_PORT"]; exists {
		t.Error("GC_K8S_DOLT_PORT should be stripped")
	}
}

func TestBuildPodEnvMirrorsBeadsEndpointFromProjectedGCDoltVars(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":               "worker",
		"GC_DOLT_HOST":           "canonical-dolt.example.com",
		"GC_DOLT_PORT":           "3308",
		"BEADS_DOLT_SERVER_HOST": "stale-beads.example.com",
		"BEADS_DOLT_SERVER_PORT": "9911",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", podManagedDoltHost, podManagedDoltPort)
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_DOLT_HOST"] != "canonical-dolt.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical-dolt.example.com", envMap["GC_DOLT_HOST"])
	}
	if envMap["GC_DOLT_PORT"] != "3308" {
		t.Fatalf("GC_DOLT_PORT = %q, want 3308", envMap["GC_DOLT_PORT"])
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != "canonical-dolt.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want mirrored canonical host", envMap["BEADS_DOLT_SERVER_HOST"])
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != "3308" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want mirrored canonical port", envMap["BEADS_DOLT_SERVER_PORT"])
	}
}

func TestBuildPodEnvUsesProviderManagedAlias(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":     "worker",
		"GC_DOLT_PORT": "31364",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", "pod-dolt.internal", "4407")
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_DOLT_HOST"] != "pod-dolt.internal" {
		t.Fatalf("GC_DOLT_HOST = %q, want pod-dolt.internal", envMap["GC_DOLT_HOST"])
	}
	if envMap["GC_DOLT_PORT"] != "4407" {
		t.Fatalf("GC_DOLT_PORT = %q, want 4407", envMap["GC_DOLT_PORT"])
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != "pod-dolt.internal" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want pod-dolt.internal", envMap["BEADS_DOLT_SERVER_HOST"])
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != "4407" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want 4407", envMap["BEADS_DOLT_SERVER_PORT"])
	}
}

func TestBuildPodEnvRemapsLoopbackDoltTargetToManagedService(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":     "worker",
		"GC_DOLT_HOST": "127.0.0.1",
		"GC_DOLT_PORT": "3308",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", "pod-dolt.internal", "4407")
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_DOLT_HOST"] != "pod-dolt.internal" {
		t.Fatalf("GC_DOLT_HOST = %q, want pod-dolt.internal", envMap["GC_DOLT_HOST"])
	}
	if envMap["GC_DOLT_PORT"] != "4407" {
		t.Fatalf("GC_DOLT_PORT = %q, want 4407", envMap["GC_DOLT_PORT"])
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != "pod-dolt.internal" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want pod-dolt.internal", envMap["BEADS_DOLT_SERVER_HOST"])
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != "4407" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want 4407", envMap["BEADS_DOLT_SERVER_PORT"])
	}
}

func TestBuildPodEnvFallbackCityPath(t *testing.T) {
	// When GC_CITY is absent, the remap should fall back to GC_CITY_PATH.
	cfgEnv := map[string]string{
		"GC_CITY_PATH": "/host/city",
		"GC_RIG_ROOT":  "/host/city/rig",
		"BEADS_DIR":    "/host/city/rig/.beads",
		"GT_ROOT":      "/host/city",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace/rig", podManagedDoltHost, podManagedDoltPort)
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_RIG_ROOT"] != "/workspace/rig" {
		t.Errorf("GC_RIG_ROOT = %q, want /workspace/rig", envMap["GC_RIG_ROOT"])
	}
	if envMap["BEADS_DIR"] != "/workspace/rig/.beads" {
		t.Errorf("BEADS_DIR = %q, want /workspace/rig/.beads", envMap["BEADS_DIR"])
	}
	if envMap["GT_ROOT"] != "/workspace" {
		t.Errorf("GT_ROOT = %q, want /workspace", envMap["GT_ROOT"])
	}
}

func TestBuildPodEnvFallbackCityRoot(t *testing.T) {
	// When both GC_CITY and GC_CITY_PATH are absent, fall back to GC_CITY_ROOT.
	cfgEnv := map[string]string{
		"GC_CITY_ROOT": "/host/city",
		"GC_RIG_ROOT":  "/host/city/rig",
		"BEADS_DIR":    "/host/city/rig/.beads",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace/rig", podManagedDoltHost, podManagedDoltPort)
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_RIG_ROOT"] != "/workspace/rig" {
		t.Errorf("GC_RIG_ROOT = %q, want /workspace/rig", envMap["GC_RIG_ROOT"])
	}
	if envMap["BEADS_DIR"] != "/workspace/rig/.beads" {
		t.Errorf("BEADS_DIR = %q, want /workspace/rig/.beads", envMap["BEADS_DIR"])
	}
}

func TestNeedsStaging(t *testing.T) {
	tests := []struct {
		name     string
		cfg      runtime.Config
		ctrlCity string
		want     bool
	}{
		{
			name:     "no staging",
			cfg:      runtime.Config{WorkDir: "/workspace"},
			ctrlCity: "/workspace",
			want:     false,
		},
		{
			name: "overlay dir",
			cfg:  runtime.Config{OverlayDir: "/some/overlay"},
			want: true,
		},
		{
			name:     "pack overlay dir",
			cfg:      runtime.Config{WorkDir: "/city", PackOverlayDirs: []string{"/some/pack"}},
			ctrlCity: "/city",
			want:     true,
		},
		{
			name: "copy files",
			cfg:  runtime.Config{CopyFiles: []runtime.CopyEntry{{Src: "/a"}}},
			want: true,
		},
		{
			name:     "rig agent (different work_dir)",
			cfg:      runtime.Config{WorkDir: "/city/rig"},
			ctrlCity: "/city",
			want:     true,
		},
		{
			name:     "city agent (same work_dir)",
			cfg:      runtime.Config{WorkDir: "/city"},
			ctrlCity: "/city",
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsStaging(tt.cfg, tt.ctrlCity)
			if got != tt.want {
				t.Errorf("needsStaging = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodManifestAddsInitContainerForPackOverlayCityAgent(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())

	cfg := runtime.Config{
		Command:         "kiro-cli chat --no-interactive --agent gascity",
		WorkDir:         "/city",
		ProviderName:    "kiro",
		PackOverlayDirs: []string{"/packs/core/overlay"},
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  "/city",
		},
	}

	pod, err := buildPod("gc-city-mayor", cfg, p)
	if err != nil {
		t.Fatal(err)
	}

	if len(pod.Spec.InitContainers) == 0 {
		t.Fatal("expected init container for city agent with pack overlay")
	}
	if pod.Spec.InitContainers[0].Name != "stage" {
		t.Errorf("init container name = %q, want %q", pod.Spec.InitContainers[0].Name, "stage")
	}
}

func TestBuildPodPrebaked(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.prebaked = true

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/demo-rig",
		Env: map[string]string{
			"GC_AGENT": "demo-rig/polecat",
			"GC_CITY":  "/city",
		},
		OverlayDir: "/some/overlay", // would normally trigger staging
	}

	pod, err := buildPod("gc-bright-demo-rig-polecat", cfg, p)
	if err != nil {
		t.Fatal(err)
	}

	// No init containers when prebaked.
	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("expected 0 init containers when prebaked, got %d", len(pod.Spec.InitContainers))
	}

	// No "ws" EmptyDir volume.
	for _, v := range pod.Spec.Volumes {
		if v.Name == "ws" {
			t.Error("prebaked pod should not have 'ws' EmptyDir volume")
		}
		if v.Name == "city" {
			t.Error("prebaked pod should not have 'city' EmptyDir volume")
		}
	}

	// No "ws" volume mount on main container.
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "ws" {
			t.Error("prebaked pod should not have 'ws' volume mount")
		}
	}

	// claude-config Secret volume must still be present.
	hasClaudeConfig := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "claude-config" {
			hasClaudeConfig = true
		}
	}
	if !hasClaudeConfig {
		t.Error("prebaked pod missing claude-config Secret volume")
	}

	// Entrypoint should NOT contain workspace-ready wait.
	entrypoint := pod.Spec.Containers[0].Args[0]
	if containsStr(entrypoint, ".gc-workspace-ready") {
		t.Error("prebaked entrypoint should not wait for .gc-workspace-ready")
	}
}

func TestInitBeadsInPodUsesProjectedStoreRootAndPrefix(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		WorkDir: "/host/city/rigs/frontend",
		Env: map[string]string{
			"GC_CITY":         "/host/city",
			"GC_STORE_ROOT":   "/host/city/custom-scope",
			"GC_BEADS_PREFIX": "cs",
			"GC_DOLT_HOST":    "canonical-dolt.example.com",
			"GC_DOLT_PORT":    "3308",
		},
	}
	podWorkDir := projectedPodWorkDir(cfg)
	if err := initBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, podWorkDir, podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("initBeadsInPod: %v", err)
	}
	wantStoreRootB64 := base64.StdEncoding.EncodeToString([]byte("/workspace/custom-scope"))
	wantPrefixB64 := base64.StdEncoding.EncodeToString([]byte("cs"))
	wrongWorkDirB64 := base64.StdEncoding.EncodeToString([]byte("/workspace/rigs/frontend"))
	found := false
	for _, c := range fake.calls {
		if c.method != "execInPod" || len(c.cmd) < 3 {
			continue
		}
		if c.cmd[0] != "sh" || c.cmd[1] != "-c" {
			continue
		}
		script := c.cmd[2]
		if !strings.Contains(script, wantStoreRootB64) || !strings.Contains(script, wantPrefixB64) {
			continue
		}
		if strings.Contains(script, wrongWorkDirB64) {
			t.Fatalf("repair script used pod workdir instead of projected store root: %s", script)
		}
		if !strings.Contains(script, "m.pop('project_id'") {
			t.Fatalf("repair script did not strip project_id: %s", script)
		}
		found = true
	}
	if !found {
		t.Fatal("initBeadsInPod did not use projected store root and prefix")
	}
}

// TestInitBeadsInPodPatchesWithheldManagedLocalTargetToAlias pins the pod's
// .beads/metadata.json repair for a withheld managed-direct session
// (gastownhall/gascity#1656): the session carries no port, but its target is
// still the managed server, so the metadata must be patched onto the
// in-cluster service instead of the repair returning before it runs.
func TestInitBeadsInPodPatchesWithheldManagedLocalTargetToAlias(t *testing.T) {
	fake := newFakeK8sOps()
	env := runtimetest.WithheldManagedDoltEnv()
	env["GC_BEADS_PREFIX"] = "demo"
	cfg := runtime.Config{Env: env}
	if err := initBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, "/workspace/demo-repo", podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("initBeadsInPod: %v", err)
	}

	portNum, err := strconv.Atoi(podManagedDoltPort)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := json.Marshal(map[string]any{
		"dolt_server_host": podManagedDoltHost,
		"dolt_server_port": portNum,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPatchB64 := base64.StdEncoding.EncodeToString(patch)
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 3 && c.cmd[0] == "sh" && c.cmd[1] == "-c" && strings.Contains(c.cmd[2], wantPatchB64) {
			return
		}
	}
	t.Fatalf("initBeadsInPod did not patch .beads/metadata.json onto %s:%s; calls=%v", podManagedDoltHost, podManagedDoltPort, fake.calls)
}

func TestVerifyBeadsInPodChecksCanonicalFiles(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_STORE_ROOT": "/host/city/frontend",
			"GC_DOLT_HOST":  "dolt.gc.svc.cluster.local",
			"GC_DOLT_PORT":  "3307",
		},
	}

	if err := verifyBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, "/workspace/frontend", podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("verifyBeadsInPod: %v", err)
	}

	found := false
	for _, c := range fake.calls {
		if c.method != "execInPod" || len(c.cmd) < 5 {
			continue
		}
		if c.cmd[0] != "sh" || c.cmd[1] != "-c" {
			continue
		}
		script := c.cmd[2]
		if containsStr(script, "test -f .beads/metadata.json") &&
			containsStr(script, "test -f .beads/config.yaml") &&
			!containsStr(script, "bd init") &&
			c.cmd[4] == "/workspace/frontend" {
			found = true
		}
	}
	if !found {
		t.Fatal("verifyBeadsInPod did not check canonical .beads files with the expected workdir")
	}
}

func TestVerifyBeadsInPodRunsForManagedProjection(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_DOLT_PORT": "31364",
		},
	}

	if err := verifyBeadsInPod(context.Background(), fake, "test-pod", cfg, "/workspace/demo-repo", podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("verifyBeadsInPod() error = %v", err)
	}
	if len(fake.calls) == 0 {
		t.Fatal("expected managed projection to trigger canonical .beads verification")
	}
}

func TestVerifyBeadsInPodSkipsWithoutProjectedTarget(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{Env: map[string]string{}}

	if err := verifyBeadsInPod(context.Background(), fake, "test-pod", cfg, "/workspace/demo-repo", podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("verifyBeadsInPod() error = %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("expected no pod exec calls without a projected Dolt target, got %d", len(fake.calls))
	}
}

func TestVerifyBeadsInPodRejectsHostOnlyProjectedTarget(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_DOLT_HOST": "canonical-dolt.example.com",
		},
	}

	err := verifyBeadsInPod(context.Background(), fake, "test-pod", cfg, "/workspace/frontend", podManagedDoltHost, podManagedDoltPort)
	if err == nil {
		t.Fatal("expected host-only GC_DOLT_* projection to fail")
	}
	if got := err.Error(); got != "requires both GC_DOLT_HOST and GC_DOLT_PORT when GC_DOLT_HOST is set" {
		t.Fatalf("verifyBeadsInPod error = %q", got)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("expected no pod exec calls after invalid projected target, got %d", len(fake.calls))
	}
}

func TestStartUsesPodBeadsRepairScript(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.prebaked = true
	p.postStartSettle = 0

	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/rig",
		Env: map[string]string{
			"GC_AGENT":        "rig/polecat",
			"GC_CITY":         "/city",
			"GC_STORE_ROOT":   "/city/custom-scope",
			"GC_BEADS_PREFIX": "cs",
			"GC_DOLT_PORT":    "31364",
		},
	}
	if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	foundRepair := false
	for _, c := range fake.calls {
		if c.method != "execInPod" || len(c.cmd) < 3 {
			continue
		}
		if c.cmd[0] != "sh" || c.cmd[1] != "-c" {
			continue
		}
		script := c.cmd[2]
		if containsStr(script, "bd init --server") && containsStr(script, "m.pop('project_id'") {
			foundRepair = true
			break
		}
	}
	if !foundRepair {
		t.Fatal("Start did not invoke the pod .beads repair/bootstrap script")
	}
}

func TestStartWarnsWhenInitBeadsInPodFails(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.prebaked = true
	p.postStartSettle = 0

	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "sh" && cmd[1] == "-c" && containsStr(cmd[2], "bd init --server") {
			return "", errors.New("missing canonical beads")
		}
		return "", nil
	}
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/rig",
		Env: map[string]string{
			"GC_AGENT":     "rig/polecat",
			"GC_CITY":      "/city",
			"GC_DOLT_PORT": "31364",
		},
	}
	if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
		t.Fatalf("Start should warn and continue when pod beads repair fails: %v", err)
	}
}

// TestInitBeadsInPodBdInitSetsBEADSDIR verifies that the pod bootstrap bd init
// sets BEADS_DIR so bd does not create a .git/ as a side effect in the pod
// workspace. Regression for #399.
func TestInitBeadsInPodBdInitSetsBEADSDIR(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_DOLT_HOST":    podManagedDoltHost,
			"GC_DOLT_PORT":    podManagedDoltPort,
			"GC_BEADS_PREFIX": "demo",
		},
	}
	if err := initBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, "/workspace/demo-repo", podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("initBeadsInPod: %v", err)
	}
	var script string
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 3 && c.cmd[0] == "sh" && c.cmd[1] == "-c" {
			script = c.cmd[2]
			break
		}
	}
	if script == "" {
		t.Fatal("no sh -c exec call found")
	}
	want := `BEADS_DIR="$WD/.beads" bd init --server`
	if !strings.Contains(script, want) {
		t.Errorf("bd init invocation missing BEADS_DIR env prefix: %q not found in script:\n%s", want, script)
	}
}

// TestInitBeadsInPodStripsProjectIDFromMetadata verifies that the metadata
// patch removes the controller's project_id so the agent pod's bd does not
// fail with PROJECT IDENTITY MISMATCH against the in-cluster Dolt server.
// The staged .beads/metadata.json carries the controller's project_id, which
// is wrong for the pod and must be dropped so bd rediscovers it.
func TestInitBeadsInPodStripsProjectIDFromMetadata(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_DOLT_HOST":    podManagedDoltHost,
			"GC_DOLT_PORT":    podManagedDoltPort,
			"GC_BEADS_PREFIX": "demo",
		},
	}

	if err := initBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, "/workspace/demo-repo", podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("initBeadsInPod: %v", err)
	}

	var script string
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 3 && c.cmd[0] == "sh" && c.cmd[1] == "-c" {
			script = c.cmd[2]
			break
		}
	}
	if script == "" {
		t.Fatal("no sh -c exec call found")
	}

	// Both the argv and stdin python3 fallback paths must drop project_id
	// after merging the patch into the staged metadata.
	want := "m.pop('project_id', None)"
	count := strings.Count(script, want)
	if count < 2 {
		t.Errorf("expected %q to appear in both python3 patch invocations (>=2 times), got %d\nscript:\n%s", want, count, script)
	}
	if strings.Contains(script, "<<<") {
		t.Errorf("metadata patch script must be POSIX sh compatible; found bash here-string in:\n%s", script)
	}
	if !strings.Contains(script, `printf '%s' "$PATCH" | python3 -c`) {
		t.Errorf("metadata patch fallback should pipe PATCH into python3 stdin for POSIX sh compatibility:\n%s", script)
	}
}

func TestStartSkipsStagingWhenPrebaked(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.prebaked = true

	// Configure fake so tmux check succeeds.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/rig",
		Env: map[string]string{
			"GC_AGENT": "rig/polecat",
			"GC_CITY":  "/city",
		},
		OverlayDir: "/some/overlay",
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start prebaked: %v", err)
	}

	// Verify no staging-related exec calls occurred.
	for _, c := range fake.calls {
		if c.method == "execInPod" {
			// Should not see touch .gc-workspace-ready
			if len(c.cmd) >= 2 && c.cmd[0] == "touch" && containsStr(c.cmd[1], ".gc-workspace-ready") {
				t.Error("prebaked Start should not touch .gc-workspace-ready")
			}
			// Should not see gc init
			if len(c.cmd) >= 2 && c.cmd[0] == "gc" && c.cmd[1] == "init" {
				t.Error("prebaked Start should not run gc init")
			}
		}
	}
}

func TestStartProjectsRigStateAfterCityInitAndBeforeReadyMarker(t *testing.T) {
	cfg := rigProjectionTestFixture(t)
	statePath := citylayout.SuspensionStateFile(cfg.Env["GC_CITY"])
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"city":{"suspended":false},"rigs":{"gascity":{"suspended":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeK8sOps()
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 4 && cmd[0] == "sh" && strings.Contains(cmd[2], "__GC_SITE_BINDING_MISSING__") {
			return siteBindingReadOutput("__GC_SITE_BINDING_MISSING__"), nil
		}
		if len(cmd) >= 3 && cmd[0] == "sh" && strings.Contains(cmd[2], "tar xf -") {
			return "gc-stage-ok", nil
		}
		if len(cmd) >= 4 && cmd[0] == "env" && cmd[2] == "gc" && cmd[3] == "init" {
			if err := os.WriteFile(statePath, []byte(`{"city":{"suspended":true},"rigs":{"gascity":{"suspended":true}}}`), 0o600); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	p := newProviderWithOps(&drainingArchiveOps{fakeK8sOps: fake})
	if err := p.Start(context.Background(), "rig-agent", cfg); err != nil {
		t.Fatalf("Start rig session: %v", err)
	}
	initAt := findExecCall(fake.calls, func(cmd []string) bool {
		return len(cmd) >= 4 && cmd[0] == "env" && cmd[2] == "gc" && cmd[3] == "init"
	})
	projectionAt := findExecCall(fake.calls, func(cmd []string) bool {
		return len(cmd) >= 4 && cmd[0] == "sh" && cmd[3] == "gc-rig-projection"
	})
	readyAt := findExecCall(fake.calls, func(cmd []string) bool {
		return len(cmd) >= 2 && cmd[0] == "touch" && strings.Contains(cmd[1], ".gc-workspace-ready")
	})
	rigReadyAt := findExecCall(fake.calls, func(cmd []string) bool {
		return len(cmd) >= 2 && cmd[0] == "touch" && strings.Contains(cmd[1], "gc-rig-projection-ready-")
	})
	if initAt < 0 || projectionAt <= initAt || readyAt <= projectionAt || rigReadyAt <= projectionAt {
		t.Fatalf("call order init=%d projection=%d workspace-ready=%d rig-ready=%d; want init < projection < both markers; calls=%#v", initAt, projectionAt, readyAt, rigReadyAt, fake.calls)
	}
	projectedState, err := base64.StdEncoding.DecodeString(fake.calls[projectionAt].cmd[5])
	if err != nil {
		t.Fatalf("decode projected suspension state: %v", err)
	}
	var state suspensionstate.State
	if err := json.Unmarshal(projectedState, &state); err != nil {
		t.Fatalf("decode projected suspension state JSON: %v", err)
	}
	if got := state.City.Suspended; got == nil || !*got {
		t.Errorf("projected city suspension = %v, want latest true state", got)
	}
	if got := state.Rigs["gascity"].Suspended; got == nil || !*got {
		t.Errorf("projected rig suspension = %v, want latest true state", got)
	}
}

func TestStartProjectsRigStateBeforePrebakedEntrypoint(t *testing.T) {
	cfg := rigProjectionTestFixture(t)
	fake := newFakeK8sOps()
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 4 && cmd[0] == "sh" && strings.Contains(cmd[2], "__GC_SITE_BINDING_MISSING__") {
			return siteBindingReadOutput("__GC_SITE_BINDING_MISSING__"), nil
		}
		return "", nil
	}
	p := newProviderWithOps(fake)
	p.prebaked = true
	if err := p.Start(context.Background(), "rig-agent", cfg); err != nil {
		t.Fatalf("Start prebaked rig session: %v", err)
	}

	pod := fake.pods[SanitizeName("rig-agent")]
	if pod == nil || len(pod.Spec.Containers) == 0 || len(pod.Spec.Containers[0].Args) == 0 {
		t.Fatal("fake did not retain the rig-scoped prebaked pod entrypoint")
	}
	entrypoint := pod.Spec.Containers[0].Args[0]
	if !strings.Contains(entrypoint, "gc-rig-projection-ready-") || strings.Contains(entrypoint, ".gc-workspace-ready") {
		t.Fatalf("rig-scoped prebaked entrypoint should wait on its unique projection marker only: %s", entrypoint)
	}
	projectionAt := findExecCall(fake.calls, func(cmd []string) bool {
		return len(cmd) >= 4 && cmd[0] == "sh" && cmd[3] == "gc-rig-projection"
	})
	readyAt := findExecCall(fake.calls, func(cmd []string) bool {
		return len(cmd) >= 2 && cmd[0] == "touch" && strings.Contains(cmd[1], "gc-rig-projection-ready-")
	})
	if projectionAt < 0 || readyAt <= projectionAt {
		t.Fatalf("call order projection=%d ready=%d; want projection before ready marker", projectionAt, readyAt)
	}
	for _, call := range fake.calls {
		if len(call.cmd) >= 4 && call.cmd[0] == "env" && call.cmd[2] == "gc" && call.cmd[3] == "init" {
			t.Fatal("prebaked Start must not run gc init")
		}
	}
}

func TestStartPrebakedRigProjectionWaitsForDynamicUser(t *testing.T) {
	cfg := rigProjectionTestFixture(t)
	cfg.Env["LINUX_USERNAME"] = "gcuser"
	fake := newFakeK8sOps()
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 4 && cmd[0] == "sh" && strings.Contains(cmd[2], "__GC_SITE_BINDING_MISSING__") {
			return siteBindingReadOutput("__GC_SITE_BINDING_MISSING__"), nil
		}
		return "", nil
	}
	p := newProviderWithOps(fake)
	p.prebaked = true
	if err := p.Start(context.Background(), "rig-agent", cfg); err != nil {
		t.Fatalf("Start prebaked rig session with dynamic user: %v", err)
	}

	pod := fake.pods[SanitizeName("rig-agent")]
	if pod == nil || len(pod.Spec.Containers) == 0 || len(pod.Spec.Containers[0].Args) == 0 {
		t.Fatal("fake did not retain the rig-scoped prebaked pod entrypoint")
	}
	entrypoint := pod.Spec.Containers[0].Args[0]
	userSetupAt := strings.Index(entrypoint, `useradd -m -s /bin/bash "gcuser"`)
	markerWaitAt := strings.Index(entrypoint, "gc-rig-projection-ready-")
	if userSetupAt < 0 || markerWaitAt <= userSetupAt {
		t.Fatalf("prebaked entrypoint must create the dynamic user before waiting for projection: %s", entrypoint)
	}

	projectionAt := findExecCall(fake.calls, func(cmd []string) bool {
		return len(cmd) >= 4 && cmd[0] == "sh" && cmd[3] == "gc-rig-projection"
	})
	if projectionAt < 0 {
		t.Fatalf("Start did not apply rig projection; calls=%#v", fake.calls)
	}
	projectionScript := fake.calls[projectionAt].cmd[2]
	userWaitAt := strings.Index(projectionScript, `until id "$5"`)
	chownAt := strings.Index(projectionScript, `chown "$5"`)
	if userWaitAt < 0 || chownAt <= userWaitAt || !strings.Contains(projectionScript, `[ "$attempts" -ge 30 ]`) {
		t.Fatalf("projection must wait for the dynamic user before chown with a bounded retry: %s", projectionScript)
	}
	if got := fake.calls[projectionAt].cmd[8]; got != "gcuser" {
		t.Fatalf("projection dynamic user = %q, want gcuser", got)
	}
}

func TestRelaunchProjectsRigStateBeforeRespawn(t *testing.T) {
	cfg := rigProjectionTestFixture(t)
	fake := newFakeK8sOps()
	name := "rig-agent"
	podName := SanitizeName(name)
	fake.pods[podName] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Labels: map[string]string{"gc-session": SanitizeLabel(name)}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Env: []corev1.EnvVar{
			{Name: "GC_RIG", Value: "gascity"},
			{Name: "GC_RIG_ROOT", Value: "/workspace/rigs/gascity"},
		}}}},
	}
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 4 && cmd[0] == "sh" && strings.Contains(cmd[2], "__GC_SITE_BINDING_MISSING__") {
			return siteBindingReadOutput("__GC_SITE_BINDING_MISSING__"), nil
		}
		return "", nil
	}
	p := newProviderWithOps(fake)
	if err := p.Relaunch(context.Background(), name, cfg); err != nil {
		t.Fatalf("Relaunch rig session: %v", err)
	}
	projectionAt := findExecCall(fake.calls, func(cmd []string) bool {
		return len(cmd) >= 4 && cmd[0] == "sh" && cmd[3] == "gc-rig-projection"
	})
	respawnAt := findExecCall(fake.calls, func(cmd []string) bool {
		return len(cmd) >= 3 && cmd[0] == "sh" && strings.Contains(cmd[2], "respawn-pane -k")
	})
	if projectionAt < 0 || respawnAt <= projectionAt {
		t.Fatalf("call order projection=%d respawn=%d; want projection before respawn", projectionAt, respawnAt)
	}
}

func TestRelaunchRejectsStaleRigPodEnvironment(t *testing.T) {
	cfg := rigProjectionTestFixture(t)
	fake := newFakeK8sOps()
	name := "rig-agent"
	podName := SanitizeName(name)
	fake.pods[podName] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Labels: map[string]string{"gc-session": SanitizeLabel(name)}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Env: []corev1.EnvVar{
			{Name: "GC_RIG", Value: "gascity"},
			{Name: "GC_RIG_ROOT", Value: "/workspace/rigs/old"},
		}}}},
	}
	fake.setExecResult(podName, []string{"tmux", "has-session", "-t", "main"}, "", nil)
	p := newProviderWithOps(fake)
	err := p.Relaunch(context.Background(), name, cfg)
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Relaunch error = %v, want ErrSessionNotFound for stale rig env", err)
	}
	for _, call := range fake.calls {
		if len(call.cmd) >= 3 && call.cmd[0] == "sh" && strings.Contains(call.cmd[2], "respawn-pane -k") {
			t.Fatalf("Relaunch respawned a pod with stale rig env: %#v", fake.calls)
		}
	}
}

func TestStartChecksLivePodBeforeRigProjection(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "rig-agent", "rig-agent")
	fake.setExecResult("rig-agent", []string{"tmux", "has-session", "-t", "main"}, "", nil)
	cfg := rigProjectionTestFixture(t)
	cfg.Env["GC_RIG_ROOT"] = "/different/path"
	err := p.Start(context.Background(), "rig-agent", cfg)
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start error = %v, want ErrSessionExists despite invalid unused projection", err)
	}
	for _, call := range fake.calls {
		if call.method == "createPod" || call.method == "deletePod" {
			t.Fatalf("Start changed an existing live Pod: %#v", fake.calls)
		}
	}
}

func TestStartRigProjectionFailuresDoNotCreateOrLeakPod(t *testing.T) {
	t.Run("build failure creates no pod", func(t *testing.T) {
		cfg := rigProjectionTestFixture(t)
		cfg.Env["GC_RIG_ROOT"] = "/outside/city"
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		if err := p.Start(context.Background(), "rig-agent", cfg); err == nil {
			t.Fatal("Start succeeded with an unmapped rig root")
		}
		for _, call := range fake.calls {
			if call.method == "createPod" {
				t.Fatalf("build failure created a pod: %#v", fake.calls)
			}
		}
	})

	t.Run("apply failure deletes created pod", func(t *testing.T) {
		cfg := rigProjectionTestFixture(t)
		fake := newFakeK8sOps()
		fake.execFunc = func(_ string, cmd []string) (string, error) {
			if len(cmd) >= 4 && cmd[0] == "sh" && strings.Contains(cmd[2], "__GC_SITE_BINDING_MISSING__") {
				return siteBindingReadOutput("[[rig]\nname = \"gascity\"\npath = ["), nil
			}
			return "", nil
		}
		p := newProviderWithOps(fake)
		p.prebaked = true
		if err := p.Start(context.Background(), "rig-agent", cfg); err == nil {
			t.Fatal("Start succeeded with malformed pod site TOML")
		}
		created, deleted := false, false
		for _, call := range fake.calls {
			created = created || call.method == "createPod"
			deleted = deleted || call.method == "deletePod"
		}
		if !created || !deleted {
			t.Fatalf("projection failure cleanup: created=%v deleted=%v calls=%#v", created, deleted, fake.calls)
		}
	})

	t.Run("projection marker failure deletes created pod", func(t *testing.T) {
		cfg := rigProjectionTestFixture(t)
		fake := newFakeK8sOps()
		fake.execFunc = func(_ string, cmd []string) (string, error) {
			if len(cmd) >= 4 && cmd[0] == "sh" && strings.Contains(cmd[2], "__GC_SITE_BINDING_MISSING__") {
				return siteBindingReadOutput("__GC_SITE_BINDING_MISSING__"), nil
			}
			if len(cmd) >= 2 && cmd[0] == "touch" && strings.Contains(cmd[1], "gc-rig-projection-ready-") {
				return "", errors.New("marker write failed")
			}
			return "", nil
		}
		p := newProviderWithOps(fake)
		p.prebaked = true
		if err := p.Start(context.Background(), "rig-agent", cfg); err == nil || !strings.Contains(err.Error(), "marking rig projection ready") {
			t.Fatalf("Start error = %v, want strict projection-marker failure", err)
		}
		deleted := false
		for _, call := range fake.calls {
			deleted = deleted || call.method == "deletePod"
		}
		if !deleted {
			t.Fatalf("failed projection marker did not clean up Pod: %#v", fake.calls)
		}
	})
}

func TestRelaunchRigProjectionFailuresDoNotRespawn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		badRoot   bool
		readError error
		readData  string
	}{
		{name: "build", badRoot: true},
		{name: "read", readError: errors.New("permission denied")},
		{name: "malformed TOML", readData: "[[rig]\nname = \"gascity\"\npath = ["},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := rigProjectionTestFixture(t)
			if tc.badRoot {
				cfg.Env["GC_RIG_ROOT"] = "/outside/city"
			}
			fake := newFakeK8sOps()
			name := "rig-agent"
			podName := SanitizeName(name)
			fake.pods[podName] = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Labels: map[string]string{"gc-session": SanitizeLabel(name)}},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Env: []corev1.EnvVar{
					{Name: "GC_RIG", Value: "gascity"},
					{Name: "GC_RIG_ROOT", Value: "/workspace/rigs/gascity"},
				}}}},
			}
			fake.execFunc = func(_ string, cmd []string) (string, error) {
				if len(cmd) >= 4 && cmd[0] == "sh" && strings.Contains(cmd[2], "__GC_SITE_BINDING_MISSING__") {
					if tc.readError != nil {
						return "", tc.readError
					}
					return siteBindingReadOutput(tc.readData), nil
				}
				return "", nil
			}
			p := newProviderWithOps(fake)
			err := p.Relaunch(context.Background(), name, cfg)
			if !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("Relaunch error = %v, want ErrSessionNotFound", err)
			}
			for _, call := range fake.calls {
				if len(call.cmd) >= 3 && call.cmd[0] == "sh" && strings.Contains(call.cmd[2], "respawn-pane -k") {
					t.Fatalf("Relaunch issued respawn after projection failure: %#v", fake.calls)
				}
			}
		})
	}
}

func rigProjectionTestFixture(t *testing.T) runtime.Config {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "rigs", "gascity"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[[rigs]]
name = "gascity"
path = "rigs/gascity"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return runtime.Config{
		Command: "codex",
		WorkDir: cityPath,
		Env: map[string]string{
			"GC_AGENT":    "gascity/codex",
			"GC_CITY":     cityPath,
			"GC_RIG":      "gascity",
			"GC_RIG_ROOT": filepath.Join(cityPath, "rigs", "gascity"),
		},
	}
}

func siteBindingReadOutput(data string) string {
	return config.SiteBindingPath(podWorkspaceRoot) + "\n" + data
}

func siteBindingReadOutputAt(path, data string) string {
	return path + "\n" + data
}

func findExecCall(calls []fakeCall, match func([]string) bool) int {
	for i, call := range calls {
		if call.method == "execInPod" && match(call.cmd) {
			return i
		}
	}
	return -1
}

type drainingArchiveOps struct {
	*fakeK8sOps
}

func (o *drainingArchiveOps) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	if stdin != nil && len(cmd) >= 3 && cmd[0] == "sh" && strings.Contains(cmd[2], "tar xf -") {
		if _, err := io.Copy(io.Discard, stdin); err != nil {
			return "", err
		}
	}
	return o.fakeK8sOps.execInPod(ctx, pod, container, cmd, nil)
}

func TestStartDetectsImmediateSessionDeath(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0 // no delay in tests

	// tmux has-session succeeds during waitForTmux, then fails on post-start check.
	hasSessionCalls := 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			hasSessionCalls++
			if hasSessionCalls <= 1 {
				return "", nil // first call: tmux alive (waitForTmux)
			}
			return "", fmt.Errorf("no server running on /tmp/tmux-1000/default")
		}
		return "", nil
	}

	cfg := runtime.Config{
		Command:      "claude --resume stale-key",
		Env:          map[string]string{"GC_AGENT": "deacon", "GC_CITY": "/workspace"},
		ProcessNames: []string{"claude"},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err == nil {
		t.Fatal("Start should fail when session dies immediately after startup")
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("Start error = %v, want ErrSessionDiedDuringStartup", err)
	}

	// Pod should have been cleaned up.
	if _, exists := fake.pods["gc-test-agent"]; exists {
		t.Error("pod should have been deleted after immediate session death")
	}
}

func TestStartAllowsOneShotLifecycleCommands(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{
			name:    "direct agent script",
			command: "gc agent-script --script /workspace/rig/assets/scripts/hyperscale-worker.yaml",
		},
		{
			name:    "wrapped one shot",
			command: "env GC_LOG_LEVEL=debug custom-once --work",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeK8sOps()
			p := newProviderWithOps(fake)
			p.postStartSettle = 100 * time.Millisecond

			hasSessionCalls := 0
			fake.execFunc = func(_ string, cmd []string) (string, error) {
				if len(cmd) >= 3 && cmd[0] == "tmux" && cmd[1] == "has-session" {
					hasSessionCalls++
					if hasSessionCalls == 1 {
						return "", nil
					}
					return "", fmt.Errorf("no server running on /tmp/tmux-1000/default")
				}
				return "", nil
			}

			cfg := runtime.Config{
				Command:   tt.command,
				Env:       map[string]string{"GC_AGENT": "hyperscale/worker", "GC_CITY": "/workspace"},
				Lifecycle: runtime.LifecycleOneShot,
				Nudge:     "Check your hook for work.",
			}

			started := time.Now()
			err := p.Start(context.Background(), "gc-test-agent", cfg)
			if err != nil {
				t.Fatalf("Start should allow one-shot lifecycle command: %v", err)
			}
			if elapsed := time.Since(started); elapsed >= p.postStartSettle {
				t.Fatalf("Start returned after %v, want before settle duration %v", elapsed, p.postStartSettle)
			}
			if hasSessionCalls != 1 {
				t.Fatalf("tmux has-session calls = %d, want only waitForTmux check", hasSessionCalls)
			}
			if _, exists := fake.pods["gc-test-agent"]; !exists {
				t.Fatal("pod should remain for normal session reconciliation after one-shot command")
			}
		})
	}
}

func TestStartChecksLivenessForScriptCommandWithoutOneShotLifecycle(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0

	hasSessionCalls := 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			hasSessionCalls++
			if hasSessionCalls == 1 {
				return "", nil
			}
			return "", fmt.Errorf("no server running on /tmp/tmux-1000/default")
		}
		return "", nil
	}

	cfg := runtime.Config{
		Command: "gc agent-script --script /workspace/rig/assets/scripts/hyperscale-worker.yaml",
		Env:     map[string]string{"GC_AGENT": "hyperscale/worker", "GC_CITY": "/workspace"},
		Nudge:   "Check your hook for work.",
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("Start error = %v, want ErrSessionDiedDuringStartup", err)
	}
	if hasSessionCalls != 2 {
		t.Fatalf("tmux has-session calls = %d, want waitForTmux and post-start liveness checks", hasSessionCalls)
	}
}

func TestStartChecksLivenessForCustomCommandWithSetupAndNudgeHints(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0

	// tmux has-session succeeds during waitForTmux, then fails on post-start check.
	hasSessionCalls := 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			hasSessionCalls++
			if hasSessionCalls == 1 {
				return "", nil
			}
			return "", fmt.Errorf("no server running on /tmp/tmux-1000/default")
		}
		return "", nil
	}

	cfg := runtime.Config{
		Command:      "custom-agent --interactive",
		Env:          map[string]string{"GC_AGENT": "custom/worker", "GC_CITY": "/workspace"},
		SessionSetup: []string{"printf setup-ready >/tmp/agent-ready"},
		Nudge:        "Check your hook for work.",
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("Start error = %v, want ErrSessionDiedDuringStartup", err)
	}
	if hasSessionCalls != 2 {
		t.Fatalf("tmux has-session calls = %d, want waitForTmux and post-start liveness checks", hasSessionCalls)
	}
	if _, exists := fake.pods["gc-test-agent"]; exists {
		t.Error("pod should have been deleted after immediate session death")
	}
}

func TestStartSucceedsWhenSessionStaysAlive(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0

	// tmux has-session always succeeds.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command:      "claude --session-id fresh-key",
		Env:          map[string]string{"GC_AGENT": "deacon", "GC_CITY": "/workspace"},
		ProcessNames: []string{"claude"},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start should succeed when session stays alive: %v", err)
	}
}

func TestStartHonorsCancellationDuringPostStartSettle(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 100 * time.Millisecond

	hasSessionCalls := 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			hasSessionCalls++
		}
		return "", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	cfg := runtime.Config{
		Command:      "claude --session-id fresh-key",
		Env:          map[string]string{"GC_AGENT": "deacon", "GC_CITY": "/workspace"},
		ProcessNames: []string{"claude"},
	}

	started := time.Now()
	err := p.Start(ctx, "gc-test-agent", cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v, want context canceled", err)
	}
	if elapsed := time.Since(started); elapsed >= p.postStartSettle {
		t.Fatalf("Start returned after %v, want before settle duration %v", elapsed, p.postStartSettle)
	}
	if hasSessionCalls != 1 {
		t.Fatalf("tmux has-session calls = %d, want 1 before settle cancellation", hasSessionCalls)
	}
	if _, exists := fake.pods["gc-test-agent"]; exists {
		t.Error("pod should have been deleted after settle cancellation")
	}
}

func TestStartSendsNudge(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0

	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		Env: map[string]string{
			"GC_AGENT": "deacon",
			"GC_CITY":  "/workspace",
		},
		Nudge: "Run 'gc prime' to check patrol status.",
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Verify nudge was sent via tmux send-keys.
	var foundText, foundEnter bool
	for _, c := range fake.calls {
		if c.method != "execInPod" {
			continue
		}
		if len(c.cmd) >= 6 && c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" && c.cmd[4] == "-l" {
			foundText = true
			if c.cmd[5] != cfg.Nudge {
				t.Errorf("nudge text = %q, want %q", c.cmd[5], cfg.Nudge)
			}
		}
		if len(c.cmd) == 5 && c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" && c.cmd[4] == "Enter" {
			foundEnter = true
		}
	}
	if !foundText {
		t.Error("Start did not send nudge text via tmux send-keys")
	}
	if !foundEnter {
		t.Error("Start did not send Enter after nudge text")
	}
}

func TestStartSkipsNudgeWhenEmpty(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0

	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  "/workspace",
		},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Verify no send-keys calls with -l flag (nudge text).
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 5 &&
			c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" && c.cmd[4] == "-l" {
			t.Error("Start sent nudge text when Nudge was empty")
		}
	}
}

// --- Relaunch (un-weld B3a) ---

// findExecCmd returns the cmd of the first execInPod call whose joined cmd
// contains substr (nil if none).
func findExecCmd(fake *fakeK8sOps, substr string) []string { //nolint:unparam // substr varies in future tests
	for _, c := range fake.calls {
		if c.method == "execInPod" && strings.Contains(strings.Join(c.cmd, " "), substr) {
			return c.cmd
		}
	}
	return nil
}

func TestProvider_RelaunchRespawnsAgentInWarmPod(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	hasSessionAlive(fake, "s") // guard + liveness recheck both succeed

	if err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent --resume"}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}

	respawn := findExecCmd(fake, "respawn-pane")
	if respawn == nil {
		t.Fatal("Relaunch did not issue tmux respawn-pane over execInPod")
	}
	body := respawn[len(respawn)-1] // sh -c <body>
	if !strings.Contains(body, "tmux respawn-pane -k -t main") {
		t.Errorf("respawn body = %q, want it to respawn the 'main' session in place", body)
	}
	// The command is base64-shipped, not inlined verbatim.
	wantB64 := base64.StdEncoding.EncodeToString([]byte("agent --resume"))
	if !strings.Contains(body, wantB64) {
		t.Errorf("respawn body = %q, want base64 %q of the agent command", body, wantB64)
	}
	if strings.Contains(body, "agent --resume") {
		t.Errorf("respawn body = %q leaked the raw command; it must be base64-shipped", body)
	}
	// Warm reuse: no pod was created or deleted.
	for _, c := range fake.calls {
		if c.method == "createPod" || c.method == "deletePod" {
			t.Errorf("Relaunch must reuse the warm pod, but called %s", c.method)
		}
	}
}

func TestProvider_RelaunchMissingPodIsErrSessionNotFound(t *testing.T) {
	fake := newFakeK8sOps() // no pods
	p := newProviderWithOps(fake)
	err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent"})
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Relaunch err = %v, want ErrSessionNotFound", err)
	}
	if findExecCmd(fake, "respawn-pane") != nil {
		t.Error("respawn-pane must not be issued when there is no running pod")
	}
}

func TestProvider_RelaunchDeadTmuxIsErrSessionNotFound(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	// Pod runs but tmux "main" is gone: relaunch must NOT recreate the pod.
	fake.setExecResult("s", []string{"tmux", "has-session", "-t", tmuxSession}, "", errors.New("no session"))
	err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent"})
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Relaunch err = %v, want ErrSessionNotFound", err)
	}
	if findExecCmd(fake, "respawn-pane") != nil {
		t.Error("respawn-pane must not be issued when the tmux session is dead")
	}
}

func TestProvider_RelaunchSuWrapsForLinuxUsername(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	hasSessionAlive(fake, "s")

	cfg := runtime.Config{Command: "agent", Env: map[string]string{"LINUX_USERNAME": "dev"}}
	if err := p.Relaunch(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	body := findExecCmd(fake, "respawn-pane")
	if body == nil {
		t.Fatal("no respawn-pane call")
	}
	last := body[len(body)-1]
	if !strings.Contains(last, `su - dev -c`) {
		t.Errorf("respawn body = %q, want it su-wrapped for the LINUX_USERNAME tmux socket", last)
	}
}

// A pane's pipe-pane survives respawn-pane -k, and `pipe-pane -o` toggles (it
// closes an existing pipe). Relaunch re-runs the post-launch setup, so every
// pipe-pane it issues must (re)open the pane log rather than toggle it, or
// alternate relaunches would silently turn diagnostic logging off.
func TestProvider_RelaunchKeepsPaneLogPipeOn(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	hasSessionAlive(fake, "s")

	for i := 0; i < 2; i++ {
		if err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent"}); err != nil {
			t.Fatalf("Relaunch #%d: %v", i+1, err)
		}
	}

	var pipes [][]string
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) > 1 && c.cmd[0] == "tmux" && c.cmd[1] == "pipe-pane" {
			pipes = append(pipes, c.cmd)
		}
	}
	if len(pipes) != 2 {
		t.Fatalf("pipe-pane calls = %d (%v), want one per Relaunch", len(pipes), pipes)
	}
	for _, cmd := range pipes {
		for _, arg := range cmd {
			if arg == "-o" {
				t.Errorf("pipe-pane cmd = %q uses -o, which toggles the pipe off on an already-piped pane", cmd)
			}
		}
		if got := cmd[len(cmd)-1]; got != "cat >> /tmp/agent-output.log" {
			t.Errorf("pipe-pane shell command = %q, want append to /tmp/agent-output.log", got)
		}
	}
}

// --- Test helpers ---

func addRunningPod(fake *fakeK8sOps, name, sessionLabel string) { //nolint:unparam // name varies in future tests
	fake.pods[name] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"app": "gc-agent", "gc-session": sessionLabel},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// addFailedPod adds a pod that exists by session label but is NOT Running, so
// IsRunning(name) is false while Stop (list-by-label, any phase) still finds it.
func addFailedPod(fake *fakeK8sOps, name, sessionLabel string) { //nolint:unparam // name varies in future tests
	fake.pods[name] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"app": "gc-agent", "gc-session": sessionLabel},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
}

func addRunningPodWithAnnotation(fake *fakeK8sOps, name, sessionLabel, sessionName string) {
	fake.pods[name] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{"app": "gc-agent", "gc-session": sessionLabel},
			Annotations: map[string]string{"gc-session-name": sessionName},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestBuildPodServiceAccount(t *testing.T) {
	cfg := runtime.Config{
		Command: "/bin/bash",
		Env:     map[string]string{"GC_AGENT": "test"},
	}

	t.Run("sets ServiceAccountName when configured", func(t *testing.T) {
		p := newProviderWithOps(newFakeK8sOps())
		p.serviceAccount = "gc-agent"

		pod, err := buildPod("test-pod", cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		if pod.Spec.ServiceAccountName != "gc-agent" {
			t.Errorf("ServiceAccountName = %q, want %q", pod.Spec.ServiceAccountName, "gc-agent")
		}
	})

	t.Run("leaves ServiceAccountName empty when not configured", func(t *testing.T) {
		p := newProviderWithOps(newFakeK8sOps())

		pod, err := buildPod("test-pod", cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		if pod.Spec.ServiceAccountName != "" {
			t.Errorf("ServiceAccountName = %q, want empty", pod.Spec.ServiceAccountName)
		}
	})
}

func TestInitCityInPodSkipsDolt(t *testing.T) {
	fake := newFakeK8sOps()

	err := initCityInPod(context.Background(), fake, "gc-mayor", "/city")
	if err != nil {
		t.Fatalf("initCityInPod: %v", err)
	}

	// gc init must run with GC_DOLT=skip so it does not attempt to start a
	// local Dolt server. In K8s pods, the in-cluster Dolt service is set up
	// separately by verifyBeadsInPod.
	var gcInitCmd []string
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) > 0 {
			for _, arg := range c.cmd {
				if arg == "gc" {
					gcInitCmd = c.cmd
					break
				}
			}
		}
		if gcInitCmd != nil {
			break
		}
	}
	if gcInitCmd == nil {
		t.Fatal("gc init command not found in exec calls")
	}

	hasSkip := false
	for _, arg := range gcInitCmd {
		if arg == "GC_DOLT=skip" {
			hasSkip = true
			break
		}
	}
	if !hasSkip {
		t.Errorf("gc init should run with GC_DOLT=skip; got cmd=%v", gcInitCmd)
	}

	// Pod-local init only scaffolds a session filesystem; it must not register
	// or start a city, and must not run provider login/readiness probes (a
	// gateway-backed provider cannot satisfy a first-party-login probe, and the
	// controller owns readiness). Assert both flags are present.
	for _, flag := range []string{"--no-start", "--skip-provider-readiness"} {
		found := false
		for _, arg := range gcInitCmd {
			if arg == flag {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("gc init should run with %s; got cmd=%v", flag, gcInitCmd)
		}
	}

	// The pod's own gc never gets --remote-stage: an older pod image does
	// not know the flag, and the record is written directly instead.
	for _, arg := range gcInitCmd {
		if arg == "--remote-stage" {
			t.Errorf("gc init in the pod must not carry --remote-stage (old images refuse unknown flags); got cmd=%v", gcInitCmd)
		}
	}
}

func TestBuildPodRigProjectionUsesPodPathAndEffectiveSuspension(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`
[workspace]
suspended_on_start = true

[[rigs]]
name = "gascity"
path = "rigs/gascity"
suspended_on_start = true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	// An explicit resume overrides the authored suspended_on_start default.
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "runtime", "suspension-state.json"), []byte(`{"city":{"suspended":false},"rigs":{"gascity":{"suspended":false},"other":{"suspended":true}},"agents":{"gascity/codex":{"suspended":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	projection, err := buildPodRigProjection(cityPath, runtime.Config{Env: map[string]string{
		"GC_CITY":     cityPath,
		"GC_RIG":      "gascity",
		"GC_RIG_ROOT": filepath.Join(cityPath, "rigs", "gascity"),
	}})
	if err != nil {
		t.Fatalf("buildPodRigProjection: %v", err)
	}
	if projection == nil {
		t.Fatal("projection = nil, want rig projection")
	}
	if got, want := projection.binding.Path, "/workspace/rigs/gascity"; got != want {
		t.Errorf("projected rig path = %q, want %q", got, want)
	}
	if suspensionstate.EffectiveCitySuspended(projection.suspension, true) {
		t.Error("effective city suspension = true, want explicit resume state to override suspended_on_start")
	}
	if suspensionstate.EffectiveRigSuspended(projection.suspension, "gascity", true) {
		t.Error("effective rig suspension = true, want explicit resume state to override suspended_on_start")
	}
	if got := projection.suspension.City.Suspended; got == nil || *got {
		t.Errorf("projected city suspension override = %v, want explicit false", got)
	}
	if got := projection.suspension.Rigs["gascity"].Suspended; got == nil || *got {
		t.Errorf("projected suspension override = %v, want explicit false", got)
	}
	if got := projection.suspension.Rigs["other"].Suspended; got == nil || !*got {
		t.Errorf("other rig suspension = %v, want preserved true", got)
	}
	if got := projection.suspension.Agents["gascity/codex"].Suspended; got == nil || !*got {
		t.Errorf("reserved agent suspension = %v, want preserved true", got)
	}
}

func TestBuildPodRigProjectionKeepsSuspendedOnStartDefaults(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
suspended_on_start = true

[[rigs]]
name = "gascity"
path = "rigs/gascity"
suspended_on_start = true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	projection, err := buildPodRigProjection(cityPath, runtime.Config{Env: map[string]string{
		"GC_CITY":     cityPath,
		"GC_RIG":      "gascity",
		"GC_RIG_ROOT": filepath.Join(cityPath, "rigs", "gascity"),
	}})
	if err != nil {
		t.Fatalf("buildPodRigProjection: %v", err)
	}
	if got := projection.suspension.City.Suspended; got == nil || !*got {
		t.Errorf("city suspension = %v, want explicit true from suspended_on_start", got)
	}
	if got := projection.suspension.Rigs["gascity"].Suspended; got == nil || !*got {
		t.Errorf("rig suspension = %v, want explicit true from suspended_on_start", got)
	}
}

func TestRefreshPodRigSuspensionSnapshotUsesLatestState(t *testing.T) {
	cityPath := t.TempDir()
	statePath := citylayout.SuspensionStateFile(cityPath)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"city":{"suspended":true},"rigs":{"gascity":{"suspended":true},"other":{"suspended":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	projection := &podRigProjection{
		binding:              config.RigSiteBinding{Name: "gascity", Path: "/workspace/rigs/gascity"},
		suspension:           suspensionstate.State{},
		citySuspendedOnStart: false,
		rigSuspendedOnStart:  false,
	}
	if err := refreshPodRigSuspensionSnapshot(cityPath, projection); err != nil {
		t.Fatalf("refreshPodRigSuspensionSnapshot: %v", err)
	}
	if got := projection.suspension.City.Suspended; got == nil || !*got {
		t.Errorf("refreshed city suspension = %v, want true", got)
	}
	if got := projection.suspension.Rigs["gascity"].Suspended; got == nil || !*got {
		t.Errorf("refreshed rig suspension = %v, want true", got)
	}
	if got := projection.suspension.Rigs["other"].Suspended; got == nil || !*got {
		t.Errorf("other rig suspension = %v, want preserved true", got)
	}
}

func TestBuildPodRigProjectionUsesSchema2AbsoluteSiteBinding(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "rigs", "gascity"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[[rigs]]
name = "gascity"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte(`[pack]
name = "city"
schema = 2
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "site.toml"), []byte(fmt.Sprintf(`[[rig]]
name = "gascity"
path = %q
`, filepath.Join(cityPath, "rigs", "gascity"))), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := runtime.Config{WorkDir: filepath.Join(cityPath, "rigs", "gascity"), Env: map[string]string{
		"GC_CITY":     cityPath + "/",
		"GC_RIG":      "gascity",
		"GC_RIG_ROOT": filepath.Join(cityPath, "rigs", ".", "gascity"),
	}}
	projection, err := buildPodRigProjection(controllerCityPath(cfg.Env), cfg)
	if err != nil {
		t.Fatalf("buildPodRigProjection from schema-2 site binding: %v", err)
	}
	if got := projection.binding.Path; got != "/workspace/rigs/gascity" {
		t.Fatalf("projected path = %q, want /workspace/rigs/gascity", got)
	}
	pod, err := buildPod("rig-agent", cfg, newProviderWithOps(newFakeK8sOps()))
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if got := projectedPodWorkDir(cfg); got != "/workspace/rigs/gascity" {
		t.Fatalf("projected workdir = %q, want normalized /workspace path", got)
	}
	if got := podEnvValue(pod, "GC_RIG_ROOT"); got != "/workspace/rigs/gascity" {
		t.Fatalf("pod GC_RIG_ROOT = %q, want normalized /workspace path", got)
	}
}

func TestBuildPodRigProjectionRejectsUnpairedRigEnvironment(t *testing.T) {
	for _, env := range []map[string]string{
		{"GC_RIG": "gascity"},
		{"GC_RIG_ROOT": "/city/rigs/gascity"},
	} {
		if _, err := buildPodRigProjection("/city", runtime.Config{Env: env}); err == nil || !strings.Contains(err.Error(), "requires both GC_RIG and GC_RIG_ROOT") {
			t.Errorf("buildPodRigProjection(%v) error = %v, want paired environment error", env, err)
		}
	}
}

func TestBuildPodRigProjectionRejectsUnmappedRigPath(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[[rigs]]
name = "gascity"
path = "../outside"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := buildPodRigProjection(cityPath, runtime.Config{Env: map[string]string{
		"GC_CITY":     cityPath,
		"GC_RIG":      "gascity",
		"GC_RIG_ROOT": filepath.Join(cityPath, "..", "outside"),
	}})
	if err == nil || !strings.Contains(err.Error(), "outside controller City") {
		t.Fatalf("buildPodRigProjection error = %v, want unmapped rig path error", err)
	}
}

func TestBuildPodRigProjectionRejectsGCPathThatDoesNotMatchConfiguredRig(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[[rigs]]
name = "gascity"
path = "rigs/gascity"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := buildPodRigProjection(cityPath, runtime.Config{Env: map[string]string{
		"GC_CITY":     cityPath,
		"GC_RIG":      "gascity",
		"GC_RIG_ROOT": filepath.Join(cityPath, "rigs", "..", "other"),
	}})
	if err == nil || !strings.Contains(err.Error(), "does not match configured path") {
		t.Fatalf("buildPodRigProjection error = %v, want GC_RIG_ROOT/configured path mismatch", err)
	}
}

func TestApplyPodRigProjectionReplacesOnlyRelevantBindingAndState(t *testing.T) {
	projection := &podRigProjection{
		binding:       config.RigSiteBinding{Name: "gascity", Path: "/workspace/rigs/gascity"},
		linuxUsername: "gcuser",
		suspension: suspensionstate.State{
			City: suspensionstate.Override{Suspended: boolPtr(false)},
			Rigs: map[string]suspensionstate.Override{
				"gascity": {Suspended: boolPtr(false)},
			},
		},
	}
	fake := newFakeK8sOps()
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 4 && cmd[0] == "sh" && strings.Contains(cmd[2], "__GC_SITE_BINDING_MISSING__") {
			return siteBindingReadOutputAt("/workspace/.gc/site-target.toml", "workspace_name = \"workspace\"\nworkspace_prefix = \"ws\"\n[[rig]]\nname = \"old\"\npath = \"/workspace/rigs/old\"\ncustom = \"keep\"\n"), nil
		}
		return "", nil
	}

	if err := applyPodRigProjection(context.Background(), fake, "pod", projection); err != nil {
		t.Fatalf("applyPodRigProjection: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("exec calls = %d, want site read and atomic projection write", len(fake.calls))
	}
	write := fake.calls[1]
	if write.method != "execInPod" || len(write.cmd) < 9 || write.cmd[0] != "sh" {
		t.Fatalf("projection write command = %#v", write)
	}
	if got := write.cmd[6]; got != "/workspace/.gc/site-target.toml" {
		t.Errorf("projection write target = %q, want resolved symlink target", got)
	}
	if got := write.cmd[8]; got != "gcuser" || !strings.Contains(write.cmd[2], `chown "$5"`) {
		t.Errorf("projection writer should restore dynamic-user ownership; argv=%#v", write.cmd)
	}
	siteData, err := base64.StdEncoding.DecodeString(write.cmd[4])
	if err != nil {
		t.Fatalf("decode projected site binding: %v", err)
	}
	if !strings.Contains(string(siteData), `workspace_name = "workspace"`) || !strings.Contains(string(siteData), `workspace_prefix = "ws"`) || !strings.Contains(string(siteData), `name = "gascity"`) || !strings.Contains(string(siteData), `path = "/workspace/rigs/gascity"`) || !strings.Contains(string(siteData), `name = "old"`) || !strings.Contains(string(siteData), `custom = "keep"`) {
		t.Fatalf("projected site binding did not preserve workspace identity and other rig data:\n%s", siteData)
	}
	stateData, err := base64.StdEncoding.DecodeString(write.cmd[5])
	if err != nil {
		t.Fatalf("decode projected suspension state: %v", err)
	}
	var state suspensionstate.State
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("decode projected suspension state JSON: %v", err)
	}
	if got := state.Rigs["gascity"].Suspended; got == nil || *got {
		t.Fatalf("projected rig suspension = %v, want explicit false", got)
	}
	if got := state.City.Suspended; got == nil || *got {
		t.Fatalf("projected city suspension = %v, want explicit false", got)
	}
	if len(state.Rigs) != 1 {
		t.Fatalf("projected rig states = %#v, want only selected rig", state.Rigs)
	}
}

func TestApplyPodRigProjectionCreatesMissingSiteBinding(t *testing.T) {
	projection := &podRigProjection{
		binding: config.RigSiteBinding{Name: "gascity", Path: "/workspace/rigs/gascity"},
		suspension: suspensionstate.State{Rigs: map[string]suspensionstate.Override{
			"gascity": {Suspended: boolPtr(false)},
		}},
	}
	fake := newFakeK8sOps()
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 4 && cmd[0] == "sh" && strings.Contains(cmd[2], "__GC_SITE_BINDING_MISSING__") {
			return siteBindingReadOutput("__GC_SITE_BINDING_MISSING__"), nil
		}
		return "", nil
	}
	if err := applyPodRigProjection(context.Background(), fake, "pod", projection); err != nil {
		t.Fatalf("applyPodRigProjection with missing site.toml: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("exec calls = %d, want site read and projection write", len(fake.calls))
	}
	write := fake.calls[1].cmd
	siteData, err := base64.StdEncoding.DecodeString(write[4])
	if err != nil {
		t.Fatalf("decode projected site binding: %v", err)
	}
	if !strings.Contains(string(siteData), `name = "gascity"`) || !strings.Contains(string(siteData), `path = "/workspace/rigs/gascity"`) {
		t.Fatalf("missing site binding did not produce selected rig binding:\n%s", siteData)
	}
}

type cleanupCaptureOps struct {
	*fakeK8sOps
	cancel              context.CancelFunc
	uploadErr           error
	cleanupCalled       bool
	cleanupContextError error
	cleanupRemaining    time.Duration
}

func (o *cleanupCaptureOps) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	_, _ = o.fakeK8sOps.execInPod(ctx, pod, container, cmd, stdin)
	if len(cmd) == 6 && cmd[0] == "sh" && cmd[5] == archiveExtractAck {
		o.cancel()
		return "", o.uploadErr
	}
	if strings.Join(cmd, " ") == "rm -rf /tmp/city-src" {
		o.cleanupCalled = true
		o.cleanupContextError = ctx.Err()
		if deadline, ok := ctx.Deadline(); ok {
			o.cleanupRemaining = time.Until(deadline)
		}
	}
	return "", nil
}

func TestInitCityInPodRemovesPartialCopyAfterUploadFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	uploadErr := errors.New("pod upload failed")
	ops := &cleanupCaptureOps{fakeK8sOps: newFakeK8sOps(), cancel: cancel, uploadErr: uploadErr}

	err := initCityInPod(ctx, ops, "gc-mayor", t.TempDir())
	if !errors.Is(err, uploadErr) {
		t.Fatalf("initCityInPod error = %v, want %v", err, uploadErr)
	}
	if !ops.cleanupCalled || ops.cleanupContextError != nil || ops.cleanupRemaining <= 0 || ops.cleanupRemaining > 5*time.Second {
		t.Fatalf("cleanup called=%v context error=%v remaining=%s; want live context with a five-second deadline", ops.cleanupCalled, ops.cleanupContextError, ops.cleanupRemaining)
	}
	last := ops.calls[len(ops.calls)-1]
	if last.method != "execInPod" || strings.Join(last.cmd, " ") != "rm -rf /tmp/city-src" {
		t.Fatalf("last pod command = %v, want partial-copy cleanup", last.cmd)
	}
}

// remoteStageWrite finds the direct remote-stage write among the fake's calls.
func remoteStageWrite(fake *fakeK8sOps) (fakeCall, bool) {
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) == 3 && c.cmd[0] == "sh" && strings.Contains(c.cmd[2], remoteStageRecordPath) {
			return c, true
		}
	}
	return fakeCall{}, false
}

// TestStartRecordsTheRemoteStageInBothModes (S5-A): staged and prebaked pods
// alike get .gc/remote-stage.toml, written by a direct exec that works with
// any pod gc version, naming the controller's city and path.
func TestStartRecordsTheRemoteStageInBothModes(t *testing.T) {
	for _, prebaked := range []bool{false, true} {
		t.Run(fmt.Sprintf("prebaked=%v", prebaked), func(t *testing.T) {
			fake := newFakeK8sOps()
			p := newProviderWithOps(fake)
			p.prebaked = prebaked
			p.cityName = "bright-lights"
			p.postStartSettle = 0
			fake.setExecResult("gc-test-agent", []string{"tmux", "has-session", "-t", "main"}, "", nil)
			cfg := runtime.Config{
				Command: "claude",
				WorkDir: "/city",
				Env:     map[string]string{"GC_AGENT": "rig/polecat", "GC_CITY": "/city"},
			}
			if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
				t.Fatalf("Start: %v", err)
			}
			call, ok := remoteStageWrite(fake)
			if !ok {
				t.Fatalf("no remote-stage write among %d calls", len(fake.calls))
			}
			for _, want := range []string{`controller_city = "bright-lights"`, `controller_path = "/city"`, "staged_at = "} {
				if !strings.Contains(call.stdin, want) {
					t.Errorf("record %q lacks %q", call.stdin, want)
				}
			}
			if strings.Contains(call.stdin, "/tmp/city-src") {
				t.Errorf("record names the staging copy: %q", call.stdin)
			}
			// The pod spec marks every process a remote stage from its
			// start, which is what closes the prebaked window: the agent can
			// start before the record lands, but never without the env.
			env := map[string]string{}
			for _, pod := range fake.pods {
				for _, e := range pod.Spec.Containers[0].Env {
					env[e.Name] = e.Value
				}
			}
			if env[remoteStageControllerEnv] != "/city" || env[remoteStageCityEnv] != "bright-lights" {
				t.Errorf("pod env %s=%q %s=%q, want /city and bright-lights", remoteStageControllerEnv, env[remoteStageControllerEnv], remoteStageCityEnv, env[remoteStageCityEnv])
			}
		})
	}
}

// TestStartFailsClosedWhenTheRemoteStageRecordFails: a pod that cannot be
// marked is deleted and the start fails, in both modes.
func TestStartFailsClosedWhenTheRemoteStageRecordFails(t *testing.T) {
	for _, prebaked := range []bool{false, true} {
		t.Run(fmt.Sprintf("prebaked=%v", prebaked), func(t *testing.T) {
			fake := newFakeK8sOps()
			p := newProviderWithOps(fake)
			p.prebaked = prebaked
			p.postStartSettle = 0
			fake.execFunc = func(_ string, cmd []string) (string, error) {
				if len(cmd) == 3 && cmd[0] == "sh" && strings.Contains(cmd[2], remoteStageRecordPath) {
					return "", errors.New("read-only file system")
				}
				return "", nil
			}
			cfg := runtime.Config{Command: "claude", WorkDir: "/city", Env: map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/city"}}
			err := p.Start(context.Background(), "gc-test-agent", cfg)
			if err == nil || !strings.Contains(err.Error(), "remote stage") || !strings.Contains(err.Error(), "read-only file system") {
				t.Fatalf("Start with an unwritable record = %v, want a remote-stage failure", err)
			}
			deleted := false
			for _, c := range fake.calls {
				if c.method == "deletePod" {
					deleted = true
				}
				if c.method == "execInPod" && len(c.cmd) >= 2 && c.cmd[0] == "touch" && containsStr(c.cmd[1], ".gc-workspace-ready") {
					t.Error("the ready sentinel was touched on a pod that could not be marked")
				}
			}
			if !deleted {
				t.Error("the unmarked pod was not deleted")
			}
		})
	}
}

// TestRelaunchRecordsTheRemoteStageBeforeRespawning (S5-A): a warm pod an
// older provider created carries neither the record nor the pod-spec
// environment, and a respawned pane takes the tmux server's environment, not
// the respawn command's. So Relaunch writes the record, naming the
// controller's city and path, before it respawns the agent.
func TestRelaunchRecordsTheRemoteStageBeforeRespawning(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.cityName = "bright-lights"
	addRunningPod(fake, "s", "s")
	hasSessionAlive(fake, "s")

	cfg := runtime.Config{Command: "agent", Env: map[string]string{"GC_CITY": "/city", "LINUX_USERNAME": "dev"}}
	if err := p.Relaunch(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	record, respawn := -1, -1
	for i, c := range fake.calls {
		if c.method != "execInPod" {
			continue
		}
		joined := strings.Join(c.cmd, " ")
		if record < 0 && strings.Contains(joined, remoteStageRecordPath) {
			record = i
		}
		if respawn < 0 && strings.Contains(joined, "respawn-pane") {
			respawn = i
		}
	}
	if record < 0 {
		t.Fatalf("Relaunch wrote no remote-stage record among %d calls", len(fake.calls))
	}
	if respawn < 0 || record > respawn {
		t.Fatalf("remote-stage record at call %d, respawn at call %d; the record must land before the agent respawns", record, respawn)
	}
	call, _ := remoteStageWrite(fake)
	for _, want := range []string{`controller_city = "bright-lights"`, `controller_path = "/city"`} {
		if !strings.Contains(call.stdin, want) {
			t.Errorf("record %q lacks %q", call.stdin, want)
		}
	}
}

// TestRelaunchFailsClosedWhenTheRemoteStageRecordFails: a warm pod that
// cannot be marked is not respawned into, and the error says why.
func TestRelaunchFailsClosedWhenTheRemoteStageRecordFails(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) == 3 && cmd[0] == "sh" && strings.Contains(cmd[2], remoteStageRecordPath) {
			return "", errors.New("read-only file system")
		}
		return "", nil
	}

	err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent", Env: map[string]string{"GC_CITY": "/city"}})
	if err == nil || !strings.Contains(err.Error(), "remote stage") || !strings.Contains(err.Error(), "read-only file system") {
		t.Fatalf("Relaunch with an unwritable record = %v, want a remote-stage failure", err)
	}
	if findExecCmd(fake, "respawn-pane") != nil {
		t.Error("the agent was respawned into a pod that could not be marked")
	}
}
