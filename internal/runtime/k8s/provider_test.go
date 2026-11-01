package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	"github.com/gastownhall/gascity/internal/shellquote"
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

func TestResolvePodSecurityConfigDefaultsAndEnvironmentOverrides(t *testing.T) {
	clearSchedulingEnv(t)
	base := PodSecurityConfig{}
	resolved, err := resolvePodSecurityConfig(base)
	if err != nil {
		t.Fatalf("resolvePodSecurityConfig: %v", err)
	}
	if resolved.RunAsUser != nil || resolved.RunAsGroup != nil || resolved.FSGroup != nil {
		t.Fatalf("unset identity config = %+v, want all fields unset", resolved)
	}

	configuredUser, configuredGroup, configuredFSGroup := int64(1111), int64(0), int64(2222)
	base = PodSecurityConfig{RunAsUser: &configuredUser, RunAsGroup: &configuredGroup, FSGroup: &configuredFSGroup}
	resolved, err = resolvePodSecurityConfig(base)
	if err != nil {
		t.Fatalf("resolvePodSecurityConfig with TOML values: %v", err)
	}
	if resolved.RunAsUser == nil || *resolved.RunAsUser != configuredUser ||
		resolved.RunAsGroup == nil || *resolved.RunAsGroup != configuredGroup ||
		resolved.FSGroup == nil || *resolved.FSGroup != configuredFSGroup {
		t.Fatalf("configured identity = %+v, want user=%d group=%d fsGroup=%d", resolved, configuredUser, configuredGroup, configuredFSGroup)
	}

	t.Setenv("GC_K8S_RUN_AS_USER", "23456")
	t.Setenv("GC_K8S_RUN_AS_GROUP", "0")
	t.Setenv("GC_K8S_FS_GROUP", "34567")
	resolved, err = resolvePodSecurityConfig(base)
	if err != nil {
		t.Fatalf("resolvePodSecurityConfig with environment overrides: %v", err)
	}
	if resolved.RunAsUser == nil || *resolved.RunAsUser != 23456 ||
		resolved.RunAsGroup == nil || *resolved.RunAsGroup != 0 ||
		resolved.FSGroup == nil || *resolved.FSGroup != 34567 {
		t.Fatalf("environment-overridden identity = %+v, want user=23456 group=0 fsGroup=34567", resolved)
	}
}

func TestResolvePodSecurityConfigRejectsInvalidIDs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "non-numeric UID", key: "GC_K8S_RUN_AS_USER", value: "user"},
		{name: "root UID", key: "GC_K8S_RUN_AS_USER", value: "0"},
		{name: "negative UID", key: "GC_K8S_RUN_AS_USER", value: "-1"},
		{name: "negative GID", key: "GC_K8S_RUN_AS_GROUP", value: "-1"},
		{name: "invalid fsGroup", key: "GC_K8S_FS_GROUP", value: "x"},
		{name: "negative fsGroup", key: "GC_K8S_FS_GROUP", value: "-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearSchedulingEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := resolvePodSecurityConfig(PodSecurityConfig{}); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("resolvePodSecurityConfig error = %v, want an error mentioning %s", err, tc.key)
			}
		})
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
		"GC_K8S_RUN_AS_USER",
		"GC_K8S_RUN_AS_GROUP",
		"GC_K8S_FS_GROUP",
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
		"GC_BEADS_SCOPE_ROOT":                 "/host/city/rig",
		"GC_BEADS_PROJECT_ID":                 "rig-project",
		"GC_BEADS_PREFIX":                     "fe",
		"GC_DOLT_DATABASE":                    "bd_rig_project",
		"GC_K8S_CITY_BEADS_PROJECT_ID":        "hq-project",
		"GC_K8S_CITY_BEADS_PREFIX":            "hq",
		"GC_K8S_CITY_DOLT_DATABASE":           "bd_hq_project",
		"GC_K8S_CITY_DOLT_HOST":               "hq.dolt.example",
		"GC_K8S_CITY_DOLT_PORT":               "4406",
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
	if envMap["GC_BEADS_SCOPE_ROOT"] != "/workspace/rig" {
		t.Errorf("GC_BEADS_SCOPE_ROOT = %q, want /workspace/rig", envMap["GC_BEADS_SCOPE_ROOT"])
	}

	// BEADS_DIR should be remapped from controller city path to /workspace.
	if envMap["BEADS_DIR"] != "/workspace/rig/.beads" {
		t.Errorf("BEADS_DIR = %q, want /workspace/rig/.beads", envMap["BEADS_DIR"])
	}
	if envMap["GC_BEADS_PROJECT_ID"] != "rig-project" || envMap["GC_BEADS_PREFIX"] != "fe" {
		t.Errorf("worker store identity = %q/%q, want rig-project/fe", envMap["GC_BEADS_PROJECT_ID"], envMap["GC_BEADS_PREFIX"])
	}
	if envMap["GC_DOLT_DATABASE"] != "bd_rig_project" {
		t.Errorf("worker Dolt database = %q, want selected rig database bd_rig_project", envMap["GC_DOLT_DATABASE"])
	}
	for _, key := range []string{"GC_K8S_CITY_BEADS_PROJECT_ID", "GC_K8S_CITY_BEADS_PREFIX", "GC_K8S_CITY_DOLT_DATABASE", "GC_K8S_CITY_DOLT_HOST", "GC_K8S_CITY_DOLT_PORT"} {
		if _, ok := envMap[key]; ok {
			t.Errorf("controller-only identity projection %s leaked into worker env", key)
		}
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

func TestResolvePodHostedDoltIdentityFailsClosedWhenMissing(t *testing.T) {
	_, err := resolvePodHostedDoltIdentity(map[string]string{
		"GC_DOLT_HOST":     "hosted.dolt.example",
		"GC_DOLT_PORT":     "4406",
		"GC_DOLT_DATABASE": "bd_project",
		"GC_BEADS_PREFIX":  "pr",
		"GC_STORE_ROOT":    "/city",
	}, "", podManagedDoltHost, podManagedDoltPort)
	if err == nil || !strings.Contains(err.Error(), "missing scoped store identity GC_BEADS_PROJECT_ID") {
		t.Fatalf("resolvePodHostedDoltIdentity error = %v, want missing project identity", err)
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
			"GC_CITY":             "/host/city",
			"GC_STORE_ROOT":       "/host/city/custom-scope",
			"GC_BEADS_PROJECT_ID": "project-cs",
			"GC_BEADS_PREFIX":     "cs",
			"GC_DOLT_DATABASE":    "bd_project_cs",
			"GC_DOLT_HOST":        "canonical-dolt.example.com",
			"GC_DOLT_PORT":        "3308",
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
			t.Fatalf("scoped Beads check used pod workdir instead of projected store root: %s", script)
		}
		if !strings.Contains(script, `grep -Fqx "id = \"$PROJECT_ID\"" `+shellquote.Quote(contract.ProjectIdentityPath(""))) {
			t.Fatalf("scoped Beads check did not verify project identity: %s", script)
		}
		found = true
	}
	if !found {
		t.Fatal("initBeadsInPod did not check the projected store root and prefix")
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

func TestStartFailsClosedWhenHostedBeadsFilesAreMissing(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.prebaked = true
	p.postStartSettle = 0
	var scopedBeadsScript string
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "sh" && cmd[1] == "-c" && strings.Contains(cmd[2], "hosted Dolt scoped beads files are incomplete") {
			scopedBeadsScript = cmd[2]
			return "", errors.New("hosted Dolt scoped beads files are incomplete")
		}
		return "", nil
	}

	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/rig",
		Env: map[string]string{
			"GC_AGENT":            "rig/polecat",
			"GC_CITY":             "/city",
			"GC_STORE_ROOT":       "/city/custom-scope",
			"GC_BEADS_PROJECT_ID": "project-cs",
			"GC_BEADS_PREFIX":     "cs",
			"GC_DOLT_DATABASE":    "bd_project_cs",
			"GC_DOLT_PORT":        "31364",
		},
	}
	if err := p.Start(context.Background(), "gc-test-agent", cfg); err == nil ||
		!strings.Contains(err.Error(), "initializing scoped beads") ||
		!strings.Contains(err.Error(), "hosted Dolt scoped beads files are incomplete") {
		t.Fatalf("Start error = %v, want missing hosted scoped beads files error", err)
	}

	if scopedBeadsScript == "" {
		t.Fatal("Start did not run the hosted scoped Beads file check")
	}
	if strings.Contains(scopedBeadsScript, "bd init --server") || strings.Contains(scopedBeadsScript, "mkdir -p .beads") {
		t.Fatalf("missing hosted files must fail closed without mutating Beads state:\n%s", scopedBeadsScript)
	}
	if !strings.Contains(scopedBeadsScript, "hosted Dolt scoped beads files are incomplete") {
		t.Fatalf("hosted missing-files error is absent from script:\n%s", scopedBeadsScript)
	}
}

func TestStartFailsWhenInitBeadsInPodFails(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.prebaked = true
	p.postStartSettle = 0

	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "sh" && cmd[1] == "-c" && containsStr(cmd[2], "hosted Dolt scoped beads files are incomplete") {
			return "", errors.New("hosted Dolt scoped beads files are incomplete")
		}
		return "", nil
	}
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/rig",
		Env: map[string]string{
			"GC_AGENT":            "rig/polecat",
			"GC_CITY":             "/city",
			"GC_STORE_ROOT":       "/city/rig",
			"GC_BEADS_PROJECT_ID": "project-cs",
			"GC_BEADS_PREFIX":     "cs",
			"GC_DOLT_DATABASE":    "bd_project_cs",
			"GC_DOLT_PORT":        "31364",
		},
	}
	if err := p.Start(context.Background(), "gc-test-agent", cfg); err == nil || !strings.Contains(err.Error(), "initializing scoped beads") {
		t.Fatalf("Start error = %v, want scoped beads initialization failure", err)
	}
}

func TestStartDoesNotReleaseWorkspaceWhenCityInitFails(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.prebaked = false
	p.postStartSettle = 0
	cityPath := t.TempDir()
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 2 && containsStr(strings.Join(cmd, " "), "gc init") {
			return "", errors.New("hosted identity required")
		}
		return "", nil
	}
	cfg := runtime.Config{
		Command: "claude",
		WorkDir: cityPath,
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  cityPath,
		},
	}
	if err := p.Start(context.Background(), "gc-test-agent", cfg); err == nil || !strings.Contains(err.Error(), "initializing City in pod") {
		t.Fatalf("Start error = %v, want City initialization failure", err)
	}
	for _, call := range fake.calls {
		if call.method == "execInPod" && strings.Join(call.cmd, " ") == "touch /workspace/.gc-workspace-ready" {
			t.Fatal("workspace-ready marker was written after City initialization failed")
		}
		if call.method == "execInPod" && strings.Contains(strings.Join(call.cmd, " "), "tmux has-session") {
			t.Fatal("tmux was probed after City initialization failed")
		}
	}
}

func TestInitBeadsInPodMissingFilesFailClosed(t *testing.T) {
	fake := newFakeK8sOps()
	missingFilesErr := errors.New("hosted Dolt scoped beads files are incomplete")
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "sh" && cmd[1] == "-c" && strings.Contains(cmd[2], "hosted Dolt scoped beads files are incomplete") {
			return "", missingFilesErr
		}
		return "", nil
	}
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_STORE_ROOT":       "/city/demo-repo",
			"GC_BEADS_PROJECT_ID": "project-demo",
			"GC_BEADS_PREFIX":     "demo",
			"GC_DOLT_DATABASE":    "bd_project_demo",
			"GC_DOLT_HOST":        podManagedDoltHost,
			"GC_DOLT_PORT":        podManagedDoltPort,
		},
	}
	if err := initBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, "/workspace/demo-repo", podManagedDoltHost, podManagedDoltPort); !errors.Is(err, missingFilesErr) {
		t.Fatalf("initBeadsInPod error = %v, want missing hosted scoped beads files", err)
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
	if strings.Contains(script, "bd init --server") || strings.Contains(script, "mkdir -p .beads") || strings.Contains(script, "yes |") {
		t.Errorf("missing hosted files must not initialize the store or create Beads files:\n%s", script)
	}
	if !strings.Contains(script, "hosted Dolt scoped beads files are incomplete") {
		t.Errorf("missing hosted-files error absent from check script:\n%s", script)
	}
	missingFilesCheck := strings.Index(script, "test -f .beads/metadata.json")
	if bdAccess := strings.Index(script, "bd config get issue_prefix"); missingFilesCheck < 0 || bdAccess < 0 || missingFilesCheck > bdAccess {
		t.Errorf("missing-files guard must fail before accessing the hosted Beads store:\n%s", script)
	}
	metadataWrite := strings.Index(script, `json.dump(m,open('.beads/metadata.json','w')`)
	if metadataWrite < 0 || missingFilesCheck > metadataWrite {
		t.Errorf("missing-files guard must precede the pod-local metadata update:\n%s", script)
	}
}

// TestInitBeadsInPodPreservesAndVerifiesProjectID prevents stripping canonical
// identity and silently allowing bd to attach to a different hosted store.
func TestInitBeadsInPodPreservesAndVerifiesProjectID(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_STORE_ROOT":       "/city/demo-repo",
			"GC_BEADS_PROJECT_ID": "project-demo",
			"GC_BEADS_PREFIX":     "demo",
			"GC_DOLT_DATABASE":    "bd_project_demo",
			"GC_DOLT_HOST":        podManagedDoltHost,
			"GC_DOLT_PORT":        podManagedDoltPort,
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

	for _, want := range []string{
		`grep -Fqx "id = \"$PROJECT_ID\"" ` + shellquote.Quote(contract.ProjectIdentityPath("")),
		`m.get("dolt_database", "")).strip() == sys.argv[2]`,
		`m.get("project_id", "")).strip() == sys.argv[3]`,
		`[ "$GOT_PREFIX" = "$PREFIX" ]`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scoped identity verification missing %q in script:\n%s", want, script)
		}
	}
	if strings.Contains(script, "m.pop('project_id'") {
		t.Errorf("identity patch must preserve project_id:\n%s", script)
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
	acceptDialogs := false
	cfg.AcceptStartupDialogs = &acceptDialogs

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

// k8sCodex0156TrustDialog is the pane fixture from
// internal/runtime/dialog_trust_selection_test.go.
const k8sCodex0156TrustDialog = `› Ask Codex to do anything

  Folder access
  /Users/u/src/project

  Trust this folder? Codex can read, edit, and run files here, subject to your
  permission settings. Folder settings can run code automatically, even
  without a model request. Continue only if you trust these files. Your trust
  decision will be saved.

› 1. Trust and continue
  2. Quit

  enter continue · esc quit`

const k8sCodexComposer = `OpenAI Codex (v0.156.0)
› Ask Codex to do anything`

func k8sCodex0156TrustDialogQuitSelected() string {
	return strings.Replace(k8sCodex0156TrustDialog,
		"› 1. Trust and continue\n  2. Quit",
		"  1. Trust and continue\n› 2. Quit", 1)
}

func newStartupDialogTestProvider(fake *fakeK8sOps) *Provider {
	p := newProviderWithOps(fake)
	p.prebaked = true
	p.postStartSettle = 0
	p.startupDialogTimeout = 2 * time.Second
	return p
}

func startupDialogTestConfig() runtime.Config {
	return runtime.Config{Command: "codex", ProcessNames: []string{"codex"}}
}

func isStartupDialogCapture(c fakeCall) bool {
	return c.method == "execInPod" && len(c.cmd) > 1 && c.cmd[0] == "tmux" && c.cmd[1] == "capture-pane"
}

func startupDialogTmuxCalls(fake *fakeK8sOps) []fakeCall {
	var calls []fakeCall
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) > 1 && c.cmd[0] == "tmux" {
			calls = append(calls, c)
		}
	}
	return calls
}

func isStartupDialogSendKeys(c fakeCall, keys ...string) bool {
	if c.method != "execInPod" || len(c.cmd) < 4 || c.cmd[0] != "tmux" || c.cmd[1] != "send-keys" || c.cmd[2] != "-t" || c.cmd[3] != tmuxSession {
		return false
	}
	if len(c.cmd) != 4+len(keys) {
		return false
	}
	for i, key := range keys {
		if c.cmd[4+i] != key {
			return false
		}
	}
	return true
}

func TestStartAcceptsCodex0156TrustDialogBeforeNudge(t *testing.T) {
	fake := newFakeK8sOps()
	p := newStartupDialogTestProvider(fake)
	cfg := startupDialogTestConfig()
	cfg.Nudge = "Check your assigned work."
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) > 1 && cmd[0] == "tmux" && cmd[1] == "capture-pane" {
			return k8sCodex0156TrustDialog, nil
		}
		return "", nil
	}

	if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	calls := startupDialogTmuxCalls(fake)
	enterIndex, nudgeIndex, bareEnterCount := -1, -1, 0
	for i, c := range calls {
		if isStartupDialogSendKeys(c, "Enter") {
			bareEnterCount++
			if enterIndex < 0 {
				enterIndex = i
			}
		}
		if len(c.cmd) > 4 && c.cmd[1] == "send-keys" && c.cmd[4] == "-l" && strings.Join(c.cmd[5:], " ") == cfg.Nudge {
			nudgeIndex = i
		}
	}
	if bareEnterCount != 2 {
		t.Fatalf("bare Enter count = %d, want dialog confirmation and nudge submit; calls=%v", bareEnterCount, calls)
	}
	if enterIndex < 0 || nudgeIndex < 0 || enterIndex >= nudgeIndex {
		t.Fatalf("dialog Enter index = %d, nudge index = %d; want dialog confirmation before nudge; calls=%v", enterIndex, nudgeIndex, calls)
	}
}

func TestStartMovesCodex0156QuitSelectionBeforeEnter(t *testing.T) {
	fake := newFakeK8sOps()
	p := newStartupDialogTestProvider(fake)
	pane := k8sCodex0156TrustDialogQuitSelected()
	var captureFrames, enterFrames []string
	var paneAfterUp string
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) > 1 && cmd[0] == "tmux" && cmd[1] == "capture-pane" {
			captureFrames = append(captureFrames, pane)
			if paneAfterUp == "after-up-pending" {
				paneAfterUp = pane
			}
			return pane, nil
		}
		if len(cmd) > 4 && cmd[0] == "tmux" && cmd[1] == "send-keys" {
			if cmd[4] == "Up" {
				pane = k8sCodex0156TrustDialog
				paneAfterUp = "after-up-pending"
			}
			if cmd[4] == "Enter" {
				enterFrames = append(enterFrames, pane)
			}
		}
		return "", nil
	}

	if err := p.Start(context.Background(), "gc-test-agent", startupDialogTestConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(enterFrames) != 1 || !strings.Contains(enterFrames[0], "› 1. Trust and continue") || strings.Contains(enterFrames[0], "› 2. Quit") {
		t.Fatalf("Enter pane frames = %q, want exactly one frame with Trust selected", enterFrames)
	}
	if !strings.Contains(paneAfterUp, "› 1. Trust and continue") {
		t.Fatalf("first pane read after Up = %q, want Trust selected", paneAfterUp)
	}
	if len(captureFrames) == 0 || !strings.Contains(captureFrames[0], "› 2. Quit") {
		t.Fatalf("initial capture frames = %q, want Quit selected", captureFrames)
	}

	calls := startupDialogTmuxCalls(fake)
	upIndex, freshReadIndex, enterIndex := -1, -1, -1
	for i, c := range calls {
		if isStartupDialogSendKeys(c, "Up") && upIndex < 0 {
			upIndex = i
			continue
		}
		if upIndex >= 0 && freshReadIndex < 0 && isStartupDialogCapture(c) {
			freshReadIndex = i
			continue
		}
		if isStartupDialogSendKeys(c, "Enter") && enterIndex < 0 {
			enterIndex = i
		}
	}
	if upIndex < 0 || freshReadIndex <= upIndex || enterIndex <= freshReadIndex {
		t.Fatalf("action indices Up=%d fresh capture=%d Enter=%d; want Up, fresh pane read, then Enter; calls=%v", upIndex, freshReadIndex, enterIndex, calls)
	}
}

func TestStartAcceptsCodexTrustAfterBlankPane(t *testing.T) {
	fake := newFakeK8sOps()
	p := newStartupDialogTestProvider(fake)
	captureCount := 0
	var captureFrames []string
	var currentPane, enterPane string
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) > 1 && cmd[0] == "tmux" && cmd[1] == "capture-pane" {
			captureCount++
			if captureCount == 1 {
				currentPane = ""
			} else {
				currentPane = k8sCodex0156TrustDialog
			}
			captureFrames = append(captureFrames, currentPane)
			return currentPane, nil
		}
		if isStartupDialogSendKeys(fakeCall{method: "execInPod", cmd: cmd}, "Enter") {
			enterPane = currentPane
		}
		return "", nil
	}

	if err := p.Start(context.Background(), "gc-test-agent", startupDialogTestConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(captureFrames) < 2 || captureFrames[0] != "" || !strings.Contains(captureFrames[1], "Trust this folder?") {
		t.Fatalf("capture frames = %q, want blank pane then delayed trust dialog", captureFrames)
	}
	if !strings.Contains(enterPane, "› 1. Trust and continue") {
		t.Fatalf("Enter pane = %q, want Trust selected", enterPane)
	}
}

func TestStartAcceptsCodexTrustDrawnAfterComposer(t *testing.T) {
	fake := newFakeK8sOps()
	p := newStartupDialogTestProvider(fake)
	captureCount := 0
	composerCaptureCount, firstComposerCaptureCount := 0, 0
	firstTrustCaptureCount, trustCaptureCount, answeredTrustCount := 0, 0, 0
	var currentPane string
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) > 1 && cmd[0] == "tmux" && cmd[1] == "capture-pane" {
			captureCount++
			// The ordered startup-dialog pass currently has ten phases, followed
			// by the watch's first read. Keep the composer on those eleven reads
			// so this test exercises a trust dialog drawn by the late-startup watch.
			if captureCount <= 11 {
				currentPane = k8sCodexComposer
				if firstComposerCaptureCount == 0 {
					firstComposerCaptureCount = captureCount
				}
				composerCaptureCount++
			} else {
				currentPane = k8sCodex0156TrustDialog
				if firstTrustCaptureCount == 0 {
					firstTrustCaptureCount = captureCount
				}
				trustCaptureCount++
			}
			return currentPane, nil
		}
		if isStartupDialogSendKeys(fakeCall{method: "execInPod", cmd: cmd}, "Enter") &&
			strings.Contains(currentPane, "Trust this folder?") &&
			strings.Contains(currentPane, "› 1. Trust and continue") &&
			!strings.Contains(currentPane, "› 2. Quit") {
			answeredTrustCount++
		}
		return "", nil
	}

	if err := p.Start(context.Background(), "gc-test-agent", startupDialogTestConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if composerCaptureCount == 0 || firstComposerCaptureCount == 0 || firstTrustCaptureCount == 0 || trustCaptureCount == 0 || composerCaptureCount+trustCaptureCount != captureCount {
		t.Fatalf("composer captures=%d first composer capture=%d first trust capture=%d trust captures=%d after %d captures; want both panes captured", composerCaptureCount, firstComposerCaptureCount, firstTrustCaptureCount, trustCaptureCount, captureCount)
	}
	if answeredTrustCount != 1 {
		t.Fatalf("trust Enter count = %d, want exactly one bare Enter while Trust and continue is selected", answeredTrustCount)
	}
	if firstComposerCaptureCount >= firstTrustCaptureCount {
		t.Fatalf("composer was not captured before trust appeared: first composer capture=%d first trust capture=%d", firstComposerCaptureCount, firstTrustCaptureCount)
	}
}

func TestStartDoesNotInspectDialogsWhenExplicitlyDisabled(t *testing.T) {
	fake := newFakeK8sOps()
	p := newStartupDialogTestProvider(fake)
	cfg := startupDialogTestConfig()
	accept := false
	cfg.AcceptStartupDialogs = &accept

	if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, c := range startupDialogTmuxCalls(fake) {
		if isStartupDialogCapture(c) || len(c.cmd) > 1 && c.cmd[1] == "send-keys" {
			t.Fatalf("startup dialog interaction with explicit false: %v", c.cmd)
		}
	}
}

func TestStartDoesNotInspectDialogsWithNilAndNoHints(t *testing.T) {
	fake := newFakeK8sOps()
	p := newStartupDialogTestProvider(fake)
	cfg := runtime.Config{Command: "codex"}

	if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, c := range startupDialogTmuxCalls(fake) {
		if isStartupDialogCapture(c) || len(c.cmd) > 1 && c.cmd[1] == "send-keys" {
			t.Fatalf("startup dialog interaction with nil and no hints: %v", c.cmd)
		}
	}
}

func TestStartCleansUpWhenStartupDialogHandlingIsCanceled(t *testing.T) {
	fake := newFakeK8sOps()
	p := newStartupDialogTestProvider(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr strings.Builder
	p.stderr = &stderr
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) > 1 && cmd[0] == "tmux" && cmd[1] == "capture-pane" {
			cancel()
		}
		return "", nil
	}

	err := p.Start(ctx, "gc-test-agent", startupDialogTestConfig())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v, want context.Canceled", err)
	}
	if _, exists := fake.pods["gc-test-agent"]; exists {
		t.Fatal("pod remains after startup dialog cancellation; want cleanup")
	}
	var deleted bool
	for _, c := range fake.calls {
		if c.method == "deletePod" && c.pod == "gc-test-agent" {
			deleted = true
			break
		}
	}
	if !deleted {
		t.Fatal("Start did not delete the pod after startup dialog cancellation")
	}
	if !strings.Contains(stderr.String(), "gc: warning: accepting startup dialogs") {
		t.Fatalf("warning output = %q, want startup dialog warning", stderr.String())
	}
}

func TestStartWarnsAndContinuesWhenStartupDialogReadFails(t *testing.T) {
	fake := newFakeK8sOps()
	p := newStartupDialogTestProvider(fake)
	var stderr strings.Builder
	p.stderr = &stderr
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) > 1 && cmd[0] == "tmux" && cmd[1] == "capture-pane" {
			return "", errors.New("pane capture failed")
		}
		return "", nil
	}

	if err := p.Start(context.Background(), "gc-test-agent", startupDialogTestConfig()); err != nil {
		t.Fatalf("Start should continue after a non-cancellation dialog error: %v", err)
	}
	if !strings.Contains(stderr.String(), "gc: warning: accepting startup dialogs") || !strings.Contains(stderr.String(), "pane capture failed") {
		t.Fatalf("warning output = %q, want dialog warning with underlying error", stderr.String())
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
	body := respawn[2] // sh -c <body>
	if !strings.Contains(body, "tmux respawn-pane -k -t main") {
		t.Errorf("respawn body = %q, want it to respawn the 'main' session in place", body)
	}
	if !strings.Contains(body, `CMD=$(printf '%s' "$@" | base64 -d)`) {
		t.Errorf("respawn body = %q, want it to reconstruct its command from positional chunks", body)
	}
	// The command is base64-shipped in bounded positional chunks, not inlined.
	wantB64 := base64.StdEncoding.EncodeToString([]byte("agent --resume"))
	if got := strings.Join(respawn[4:], ""); got != wantB64 {
		t.Errorf("respawn chunks = %q, want base64 %q of the agent command", got, wantB64)
	}
	if strings.Contains(body, "agent --resume") {
		t.Errorf("respawn body = %q leaked the raw command; it must be passed separately", body)
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
	script := body[2]
	if !strings.Contains(script, "su - "+shellquote.Quote("dev")+" -c") {
		t.Errorf("respawn body = %q, want it su-wrapped for the LINUX_USERNAME tmux socket", script)
	}
	if !strings.Contains(script, "CMD=$(base64 -d)") {
		t.Errorf("respawn body = %q, want the static su script to decode stdin", script)
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

	err := initCityInPod(context.Background(), fake, "gc-mayor", "/city", "", nil)
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

	if !strings.Contains(strings.Join(gcInitCmd, " "), `env -i PATH="$PATH" HOME="$HOME" GC_DOLT=skip`) {
		t.Errorf("gc init should run with a minimal environment and GC_DOLT=skip; got cmd=%v", gcInitCmd)
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
}

func TestInitCityInPodUsesCanonicalHQHostedIdentity(t *testing.T) {
	fake := newFakeK8sOps()
	identity := &podHostedDoltIdentity{
		ProjectID: "hq-project",
		Prefix:    "hq",
		Database:  "bd_hq_project",
		Host:      "hq.dolt.example",
		Port:      "4406",
	}
	if err := initCityInPod(context.Background(), fake, "gc-mayor", "/city", "", identity); err != nil {
		t.Fatalf("initCityInPod: %v", err)
	}
	for _, call := range fake.calls {
		if call.method != "execInPod" {
			continue
		}
		for _, arg := range call.cmd {
			if arg != "gc" {
				continue
			}
			joined := strings.Join(call.cmd, " ")
			for _, want := range []string{"--dolt-host hq.dolt.example", "--dolt-port 4406", "--dolt-database bd_hq_project", "--dolt-project-id hq-project"} {
				if !strings.Contains(joined, want) {
					t.Errorf("gc init command %v missing %q", call.cmd, want)
				}
			}
			return
		}
	}
	t.Fatal("gc init command was not issued")
}

func TestInitCityInPodSanitizesHostedRigEnvironmentForNonHostedCity(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0
	cityPath := t.TempDir()
	var gcInitCmd []string
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) == 6 && cmd[0] == "sh" && cmd[5] == archiveExtractAck {
			return archiveExtractAck, nil
		}
		if len(cmd) >= 4 && cmd[0] == "sh" && cmd[1] == "-c" && cmd[3] == "gc" {
			gcInitCmd = append([]string(nil), cmd...)
		}
		return "", nil
	}

	cfg := runtime.Config{
		Command: "claude",
		WorkDir: cityPath,
		Env: map[string]string{
			"GC_AGENT":                   "rig/polecat",
			"GC_CITY":                    cityPath,
			"GC_STORE_ROOT":              cityPath,
			"GC_BEADS_PROJECT_ID":        "rig-project",
			"GC_BEADS_PREFIX":            "rg",
			"GC_DOLT_DATABASE":           "bd_rig_project",
			"GC_DOLT_HOST":               "rig.dolt.example",
			"GC_DOLT_PORT":               "3307",
			"GC_DOLT_USER":               "rig-user",
			"GC_DOLT_PASSWORD":           "rig-password",
			"GC_BEADS_TRANSPORT":         "direct",
			"GC_BEADS_TARGET":            "external",
			"BEADS_DOLT_SERVER_HOST":     "rig.dolt.example",
			"BEADS_DOLT_SERVER_PORT":     "3307",
			"BEADS_DOLT_SERVER_DATABASE": "bd_rig_project",
			"BEADS_DOLT_SERVER_MODE":     "direct",
			"BEADS_DOLT_SERVER_TLS":      "true",
			"BEADS_DOLT_SERVER_SOCKET":   "/tmp/rig-dolt.sock",
			"BEADS_DOLT_SERVER_USER":     "rig-user",
			"BEADS_DOLT_SERVER_PASSWORD": "rig-password",
			"BEADS_DOLT_PASSWORD":        "rig-password",
		},
	}
	if err := p.Start(context.Background(), "gc-rig-worker", cfg); err != nil {
		t.Fatalf("Start hosted rig worker with non-hosted City: %v", err)
	}
	if len(gcInitCmd) == 0 {
		t.Fatal("gc init command was not issued")
	}
	if got, want := gcInitCmd[2], `exec env -i PATH="$PATH" HOME="$HOME" GC_DOLT=skip "$0" "$@"`; got != want {
		t.Fatalf("gc init launcher = %q, want minimal environment launcher %q", got, want)
	}
	joined := strings.Join(gcInitCmd, " ")
	for _, forbidden := range []string{
		"GC_DOLT_HOST=", "GC_DOLT_PORT=", "GC_DOLT_USER=", "GC_DOLT_PASSWORD=", "GC_DOLT_DATABASE=",
		"GC_BEADS_PROJECT_ID=", "GC_BEADS_PREFIX=", "GC_BEADS_TRANSPORT=", "GC_BEADS_TARGET=",
		"BEADS_DOLT_SERVER_HOST=", "BEADS_DOLT_SERVER_PORT=", "BEADS_DOLT_SERVER_DATABASE=",
		"BEADS_DOLT_SERVER_MODE=", "BEADS_DOLT_SERVER_TLS=", "BEADS_DOLT_SERVER_SOCKET=",
		"BEADS_DOLT_SERVER_USER=", "BEADS_DOLT_SERVER_PASSWORD=", "BEADS_DOLT_PASSWORD=",
		"rig-project", "bd_rig_project", "rig.dolt.example", "rig-user", "rig-password", "/tmp/rig-dolt.sock",
		"--dolt-host", "--dolt-port", "--dolt-database", "--dolt-project-id",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("non-hosted City gc init unexpectedly received rig identity %q: %v", forbidden, gcInitCmd)
		}
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

	err := initCityInPod(ctx, ops, "gc-mayor", t.TempDir(), "", nil)
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
