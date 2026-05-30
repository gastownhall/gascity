package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gastownhall/gascity/internal/runtime"
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

func TestParseWorkspaceEnvIsOptInByPVC(t *testing.T) {
	t.Run("root without pvc is ignored", func(t *testing.T) {
		clearWorkspaceEnv(t)
		t.Setenv("GC_K8S_WORKSPACE_ROOT", "/workspace/cities/demo-city")

		workspace, err := parseWorkspaceEnv()
		if err != nil {
			t.Fatalf("parseWorkspaceEnv: %v", err)
		}
		if workspace.pvc != "" {
			t.Fatalf("workspace pvc = %q, want empty", workspace.pvc)
		}
		if workspace.root != defaultPodWorkspaceRoot {
			t.Fatalf("workspace root = %q, want default %q", workspace.root, defaultPodWorkspaceRoot)
		}
	})

	t.Run("pvc defaults root", func(t *testing.T) {
		clearWorkspaceEnv(t)
		t.Setenv("GC_K8S_WORKSPACE_PVC", "demo-city-workspace")

		workspace, err := parseWorkspaceEnv()
		if err != nil {
			t.Fatalf("parseWorkspaceEnv: %v", err)
		}
		if workspace.pvc != "demo-city-workspace" {
			t.Fatalf("workspace pvc = %q, want demo-city-workspace", workspace.pvc)
		}
		if workspace.root != defaultPodWorkspaceRoot {
			t.Fatalf("workspace root = %q, want default %q", workspace.root, defaultPodWorkspaceRoot)
		}
	})

	t.Run("pvc trims absolute root", func(t *testing.T) {
		clearWorkspaceEnv(t)
		t.Setenv("GC_K8S_WORKSPACE_PVC", "demo-city-workspace")
		t.Setenv("GC_K8S_WORKSPACE_ROOT", "/workspace/cities/demo-city/")

		workspace, err := parseWorkspaceEnv()
		if err != nil {
			t.Fatalf("parseWorkspaceEnv: %v", err)
		}
		if workspace.root != "/workspace/cities/demo-city" {
			t.Fatalf("workspace root = %q, want /workspace/cities/demo-city", workspace.root)
		}
	})

	t.Run("pvc rejects relative root", func(t *testing.T) {
		clearWorkspaceEnv(t)
		t.Setenv("GC_K8S_WORKSPACE_PVC", "demo-city-workspace")
		t.Setenv("GC_K8S_WORKSPACE_ROOT", "workspace/cities/demo-city")

		_, err := parseWorkspaceEnv()
		if err == nil {
			t.Fatal("expected relative workspace root to fail")
		}
		if !strings.Contains(err.Error(), "GC_K8S_WORKSPACE_ROOT") {
			t.Fatalf("error = %q, want GC_K8S_WORKSPACE_ROOT context", err)
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

func clearWorkspaceEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GC_K8S_WORKSPACE_PVC",
		"GC_K8S_WORKSPACE_ROOT",
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

func TestInventoryListsPodsOnceWithoutTmuxProbes(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("gc-test-agent-%02d", i)
		addRunningPodWithAnnotation(fake, name, name, name)
	}
	addRunningPodWithAnnotation(fake, "gc-other-agent", "gc-other-agent", "gc-other-agent")
	addRunningPodWithAnnotation(fake, "gc-test-deleting", "gc-test-deleting", "gc-test-deleting")
	deleted := fake.pods["gc-test-deleting"]
	now := metav1.Now()
	deleted.DeletionTimestamp = &now
	fake.pods["gc-test-pending"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "gc-test-pending",
			Labels:      map[string]string{"app": "gc-agent", "gc-session": "gc-test-pending"},
			Annotations: map[string]string{"gc-session-name": "gc-test-pending"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	inventory, err := p.Inventory(context.Background(), "gc-test-")
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if !inventory.Complete {
		t.Fatal("Inventory.Complete = false, want true")
	}
	if len(inventory.Observations) != 30 {
		t.Fatalf("inventory observations = %d, want 30", len(inventory.Observations))
	}
	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("gc-test-agent-%02d", i)
		running, known := inventory.RunningKnown(name)
		if !known || !running {
			t.Fatalf("RunningKnown(%q) = (%v, %v), want (true, true)", name, running, known)
		}
	}
	if running, known := inventory.RunningKnown("gc-test-deleting"); !known || running {
		t.Fatalf("RunningKnown(deleting) = (%v, %v), want known stopped", running, known)
	}

	listCalls := 0
	for _, c := range fake.calls {
		switch c.method {
		case "listPods":
			listCalls++
			if c.selector != "app=gc-agent" || c.fieldSelector != "status.phase=Running" {
				t.Fatalf("listPods selector = (%q, %q), want app=gc-agent/status.phase=Running", c.selector, c.fieldSelector)
			}
		case "execInPod":
			t.Fatalf("Inventory performed tmux exec: %+v", c)
		}
	}
	if listCalls != 1 {
		t.Fatalf("listPods calls = %d, want 1", listCalls)
	}
}

func TestListRuntimeArtifactsIncludesPendingPodsWithSessionID(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPodWithAnnotation(fake, "gc-test-live", "gc-test-live", "gc-test-live")
	fake.pods["gc-test-pending"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "gc-test-pending",
			Labels:      map[string]string{"app": "gc-agent", "gc-session": "gc-test-pending"},
			Annotations: map[string]string{"gc-session-name": "gc-test-pending-full"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "agent",
				Env:  []corev1.EnvVar{{Name: "GC_SESSION_ID", Value: "gc-session-pending"}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	artifacts, err := p.ListRuntimeArtifacts("gc-test-")
	if err != nil {
		t.Fatalf("ListRuntimeArtifacts: %v", err)
	}
	got := map[string]string{}
	for _, artifact := range artifacts {
		got[artifact.Name] = artifact.SessionID
	}
	if _, ok := got["gc-test-live"]; !ok {
		t.Fatalf("ListRuntimeArtifacts missing running pod: %#v", got)
	}
	if got["gc-test-pending-full"] != "gc-session-pending" {
		t.Fatalf("pending artifact session ID = %q, want gc-session-pending; all=%#v", got["gc-test-pending-full"], got)
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

	// Configure fake to return epoch timestamp.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "display-message", "-t", "main", "-p", "#{session_activity}"},
		"1709300000\n", nil)

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
	if pod.Annotations[providerRuntimeProviderAnnotation] != "k8s" {
		t.Errorf("annotation %s = %q, want k8s", providerRuntimeProviderAnnotation, pod.Annotations[providerRuntimeProviderAnnotation])
	}
	if pod.Annotations[providerRuntimeFingerprintAnnotation] == "" {
		t.Fatalf("missing %s annotation", providerRuntimeFingerprintAnnotation)
	}
	if pod.Annotations[providerRuntimeFingerprintVersionAnnotation] != providerRuntimeFingerprintVersion {
		t.Errorf("annotation %s = %q, want %s", providerRuntimeFingerprintVersionAnnotation, pod.Annotations[providerRuntimeFingerprintVersionAnnotation], providerRuntimeFingerprintVersion)
	}
	if pod.Annotations[providerRuntimeImageAnnotation] != p.image {
		t.Errorf("annotation %s = %q, want %s", providerRuntimeImageAnnotation, pod.Annotations[providerRuntimeImageAnnotation], p.image)
	}
}

func TestObserveRuntimeCompatibilityClassifiesCompatibleAndLegacyPods(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	cfg := runtime.Config{
		Command: "claude",
		Env:     map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/workspace"},
	}

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	legacy, err := p.ObserveRuntimeCompatibility(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("ObserveRuntimeCompatibility legacy: %v", err)
	}
	if legacy.Compatible || legacy.Reason != "missing-runtime-identity" || !legacy.Alive {
		t.Fatalf("legacy compatibility = %#v, want alive incompatible missing-runtime-identity", legacy)
	}

	annotatePodWithDesiredRuntimeIdentity(t, p, "gc-test-agent", cfg)
	compatible, err := p.ObserveRuntimeCompatibility(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("ObserveRuntimeCompatibility compatible: %v", err)
	}
	if !compatible.Compatible || compatible.Desired.Fingerprint == "" || compatible.Current.Fingerprint != compatible.Desired.Fingerprint {
		t.Fatalf("compatible observation = %#v", compatible)
	}
}

func TestProviderRuntimeFingerprintChangesForSubstrateFields(t *testing.T) {
	baseProvider := newProviderWithOps(newFakeK8sOps())
	baseCfg := runtime.Config{
		Env: map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/workspace"},
	}
	base := mustDesiredProviderRuntimeIdentity(t, baseProvider, baseCfg)

	tests := []struct {
		name   string
		mutate func(*Provider, *runtime.Config)
	}{
		{
			name: "main image",
			mutate: func(p *Provider, _ *runtime.Config) {
				p.image = "test-image:v2"
			},
		},
		{
			name: "service account",
			mutate: func(p *Provider, _ *runtime.Config) {
				p.serviceAccount = "agent-runtime"
			},
		},
		{
			name: "resources",
			mutate: func(p *Provider, _ *runtime.Config) {
				p.cpuRequest = "750m"
				p.memLimit = "6Gi"
			},
		},
		{
			name: "prebaked workspace mode",
			mutate: func(p *Provider, _ *runtime.Config) {
				p.prebaked = true
			},
		},
		{
			name: "persistent workspace pvc",
			mutate: func(p *Provider, _ *runtime.Config) {
				p.workspacePVC = "demo-city-workspace"
				p.workspaceRoot = "/workspace/cities/demo-city"
			},
		},
		{
			name: "linux username home projection",
			mutate: func(_ *Provider, cfg *runtime.Config) {
				cfg.Env["LINUX_USERNAME"] = "gascity"
			},
		},
		{
			name: "scheduling",
			mutate: func(p *Provider, _ *runtime.Config) {
				p.nodeSelector = map[string]string{"workload": "gc-agents"}
				p.priorityClassName = "gc-agent-high"
			},
		},
		{
			name: "managed dolt service env",
			mutate: func(_ *Provider, cfg *runtime.Config) {
				cfg.Env["GC_DOLT_HOST"] = "127.0.0.1"
				cfg.Env["GC_DOLT_PORT"] = "3307"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProviderWithOps(newFakeK8sOps())
			cfg := runtime.Config{Env: map[string]string{}}
			for k, v := range baseCfg.Env {
				cfg.Env[k] = v
			}
			tt.mutate(p, &cfg)
			got := mustDesiredProviderRuntimeIdentity(t, p, cfg)
			if got.Fingerprint == base.Fingerprint {
				t.Fatalf("fingerprint did not change for %s: %s", tt.name, got.Fingerprint)
			}
		})
	}
}

func TestProviderRuntimeIdentityIncludesSharedProcessNamespace(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	cfg := runtime.Config{
		Env: map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/workspace"},
	}

	identity := mustDesiredProviderRuntimeIdentity(t, p, cfg)
	if identity.Version != "k8s-v3" {
		t.Fatalf("identity version = %q, want k8s-v3", identity.Version)
	}
	var spec runtimeIdentitySpec
	if err := json.Unmarshal([]byte(identity.Breakdown), &spec); err != nil {
		t.Fatalf("unmarshal identity breakdown: %v\n%s", err, identity.Breakdown)
	}
	if !spec.ShareProcessNamespace {
		t.Fatalf("ShareProcessNamespace = false in identity breakdown: %s", identity.Breakdown)
	}
	if spec.LaunchMaterialMode != "staged-files" {
		t.Fatalf("LaunchMaterialMode = %q, want staged-files in identity breakdown: %s", spec.LaunchMaterialMode, identity.Breakdown)
	}
}

func TestProviderRuntimeIdentityIncludesClaudeOAuthSecretEnv(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	cfg := runtime.Config{Env: map[string]string{"GC_AGENT": "mayor"}}

	identity := mustDesiredProviderRuntimeIdentity(t, p, cfg)
	var spec runtimeIdentitySpec
	if err := json.Unmarshal([]byte(identity.Breakdown), &spec); err != nil {
		t.Fatalf("unmarshal identity breakdown: %v\n%s", err, identity.Breakdown)
	}

	for _, env := range spec.CredentialEnv {
		if env.Name == "CLAUDE_CODE_OAUTH_TOKEN" {
			if env.SecretName != providerRuntimeClaudeSecretName || env.Key != "CLAUDE_CODE_OAUTH_TOKEN" || !env.Optional {
				t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN credential env = %#v", env)
			}
			return
		}
	}
	t.Fatalf("missing CLAUDE_CODE_OAUTH_TOKEN credential env in %#v", spec.CredentialEnv)
}

func TestProviderRuntimeIdentityPreservesExplicitClaudeOAuthEnv(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	cfg := runtime.Config{Env: map[string]string{
		"GC_AGENT":                "mayor",
		"CLAUDE_CODE_OAUTH_TOKEN": "explicit-token",
	}}

	identity := mustDesiredProviderRuntimeIdentity(t, p, cfg)
	var spec runtimeIdentitySpec
	if err := json.Unmarshal([]byte(identity.Breakdown), &spec); err != nil {
		t.Fatalf("unmarshal identity breakdown: %v\n%s", err, identity.Breakdown)
	}

	for _, env := range spec.CredentialEnv {
		if env.Name == "CLAUDE_CODE_OAUTH_TOKEN" {
			t.Fatalf("explicit CLAUDE_CODE_OAUTH_TOKEN should not be represented as secret env: %#v", env)
		}
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

func TestStartStagesLaunchMaterialWithoutEmbeddingPromptInExecArgs(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0
	prompt := strings.Repeat("large prompt ", 20000)
	command := "codex --model gpt-5.5 " + strings.Repeat("--flag ", 2000)
	preStart := "printf %s " + strings.Repeat("x", 20000)
	cfg := runtime.Config{
		Command:      command,
		WorkDir:      "/city/rigs/frontend",
		PromptSuffix: "'" + prompt + "'",
		PromptFlag:   "--prompt",
		PreStart:     []string{preStart},
		Env: map[string]string{
			"GC_AGENT": "demo-rig/polecat",
			"GC_CITY":  "/city",
		},
	}
	fake.setExecResult("gc-test-agent", []string{"tmux", "has-session", "-t", "main"}, "", nil)

	if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pod := fake.pods["gc-test-agent"]
	if pod == nil {
		t.Fatal("created pod not recorded")
	}
	argv := strings.Join(append(append([]string{}, pod.Spec.Containers[0].Command...), pod.Spec.Containers[0].Args...), "\x00")
	if len(argv) > 4096 {
		t.Fatalf("pod argv length = %d, want bounded under 4096", len(argv))
	}
	for _, forbidden := range []string{prompt, command, preStart} {
		if strings.Contains(argv, forbidden) {
			t.Fatalf("pod argv leaked large launch material")
		}
	}

	launchReadyIdx, workspaceReadyIdx := -1, -1
	for i, c := range fake.calls {
		if c.method != "execInPod" {
			continue
		}
		joined := strings.Join(c.cmd, " ")
		for _, forbidden := range []string{prompt, command, preStart} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("exec argv leaked large launch material in call %#v", c)
			}
		}
		if c.container == "launch" && len(c.cmd) == 2 && c.cmd[0] == "touch" && c.cmd[1] == podLaunchReadyMarker {
			launchReadyIdx = i
		}
		if c.container == "stage" && len(c.cmd) == 2 && c.cmd[0] == "touch" && c.cmd[1] == "/workspace/.gc-ready" {
			workspaceReadyIdx = i
		}
	}
	if launchReadyIdx == -1 {
		t.Fatal("launch material was not marked ready")
	}
	if workspaceReadyIdx == -1 {
		t.Fatal("workspace staging was not marked ready")
	}
	if launchReadyIdx > workspaceReadyIdx {
		t.Fatalf("workspace was marked ready before launch material: launch=%d workspace=%d", launchReadyIdx, workspaceReadyIdx)
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
	annotatePodWithDesiredRuntimeIdentity(t, p, "gc-test-agent", cfg)
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err == nil {
		t.Fatal("Start should fail for existing live session")
	}
	if want := "already exists"; !contains(err.Error(), want) {
		t.Errorf("error = %q, want containing %q", err, want)
	}
}

func TestStartRejectsIncompatibleLiveSessionWithoutDeletingPod(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	err := p.Start(context.Background(), "gc-test-agent", runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env:          map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/workspace"},
	})
	if !errors.Is(err, runtime.ErrRuntimeIncompatible) {
		t.Fatalf("Start error = %v, want ErrRuntimeIncompatible", err)
	}
	for _, c := range fake.calls {
		if c.method == "deletePod" && c.pod == "gc-test-agent" {
			t.Fatal("incompatible live pod was deleted by Start")
		}
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
	annotatePodWithDesiredRuntimeIdentity(t, p, "gc-test-agent", cfg)
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

func TestStartReplacesYoungIncompatiblePodWithDeadTmux(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	fake.pods["gc-test-agent"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "gc-test-agent",
			Labels:            map[string]string{"app": "gc-agent", "gc-session": "gc-test-agent"},
			CreationTimestamp: metav1.Now(),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "",
		fmt.Errorf("no server running on /tmp/tmux-1000/default"))
	fake.createErr = fmt.Errorf("intentional: verify incompatible replacement")

	err := p.Start(context.Background(), "gc-test-agent", runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env:          map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/workspace"},
	})
	if errors.Is(err, runtime.ErrSessionInitializing) {
		t.Fatalf("Start error = %v, want replacement attempt, not ErrSessionInitializing", err)
	}
	foundDelete := false
	for _, c := range fake.calls {
		if c.method == "deletePod" && c.pod == "gc-test-agent" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Fatal("young incompatible pod with dead tmux was not deleted")
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
	// tmux dead — genuinely stale.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "",
		fmt.Errorf("no server running on /tmp/tmux-1000/default"))

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

	// Init containers must stage launch material first, then workspace files.
	if len(pod.Spec.InitContainers) < 2 {
		t.Fatalf("expected launch and stage init containers for rig agent: %#v", pod.Spec.InitContainers)
	}
	if pod.Spec.InitContainers[0].Name != "launch" || pod.Spec.InitContainers[1].Name != "stage" {
		t.Errorf("init containers = %#v, want launch then stage", pod.Spec.InitContainers)
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
	for _, name := range []string{"launch", "ws", "claude-config", "city"} {
		if !volNames[name] {
			t.Errorf("missing volume %q", name)
		}
	}

	// The shell starts from a stable workspace root; the entrypoint cd's into
	// the projected agent worktree after pre-start scripts finish.
	if pod.Spec.Containers[0].WorkingDir != "/workspace" {
		t.Errorf("workingDir = %q, want /workspace",
			pod.Spec.Containers[0].WorkingDir)
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

func TestBuildPodEnvClaudeOAuthTokenUsesOptionalSecret(t *testing.T) {
	env := mustBuildPodEnv(t, map[string]string{}, "/workspace/rig", podManagedDoltHost, podManagedDoltPort)

	for _, v := range env {
		if v.Name != "CLAUDE_CODE_OAUTH_TOKEN" {
			continue
		}
		if v.Value != "" {
			t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN literal value = %q, want secret ref", v.Value)
		}
		if v.ValueFrom == nil || v.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN does not come from a secret: %#v", v)
		}
		ref := v.ValueFrom.SecretKeyRef
		if ref.Name != providerRuntimeClaudeSecretName || ref.Key != "CLAUDE_CODE_OAUTH_TOKEN" {
			t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN secret ref = %s/%s, want %s/CLAUDE_CODE_OAUTH_TOKEN", ref.Name, ref.Key, providerRuntimeClaudeSecretName)
		}
		if ref.Optional == nil || !*ref.Optional {
			t.Fatal("CLAUDE_CODE_OAUTH_TOKEN secret ref should be optional")
		}
		return
	}
	t.Fatal("missing CLAUDE_CODE_OAUTH_TOKEN env")
}

func TestBuildPodEnvClaudeOAuthTokenExplicitEnvTakesPrecedence(t *testing.T) {
	env := mustBuildPodEnv(
		t,
		map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "explicit-token"},
		"/workspace/rig",
		podManagedDoltHost,
		podManagedDoltPort,
	)

	for _, v := range env {
		if v.Name != "CLAUDE_CODE_OAUTH_TOKEN" {
			continue
		}
		if v.Value != "explicit-token" {
			t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %q, want explicit-token", v.Value)
		}
		if v.ValueFrom != nil {
			t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN should preserve explicit env instead of secret ref: %#v", v)
		}
		return
	}
	t.Fatal("missing CLAUDE_CODE_OAUTH_TOKEN env")
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
			name: "no controller city",
			cfg:  runtime.Config{},
			want: false,
		},
		{
			name:     "city root workdir",
			cfg:      runtime.Config{WorkDir: "/workspace"},
			ctrlCity: "/workspace",
			want:     true,
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
			want:     true,
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

	if len(pod.Spec.InitContainers) < 2 {
		t.Fatalf("expected launch and stage init containers for city agent with pack overlay: %#v", pod.Spec.InitContainers)
	}
	if pod.Spec.InitContainers[0].Name != "launch" || pod.Spec.InitContainers[1].Name != "stage" {
		t.Errorf("init containers = %#v, want launch then stage", pod.Spec.InitContainers)
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

	// Prebaked pods still use the bounded launch-material init container, but
	// skip workspace staging.
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != "launch" {
		t.Errorf("expected only launch init container when prebaked, got %#v", pod.Spec.InitContainers)
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
	entrypoint := buildPodLaunchMaterial(cfg, p).Entrypoint
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
	if err := initBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, podWorkDir, defaultPodWorkspaceRoot, podManagedDoltHost, podManagedDoltPort); err != nil {
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

func TestInitBeadsInPodUsesPersistentWorkspaceStoreRoot(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		WorkDir: "/host/city/rigs/frontend",
		Env: map[string]string{
			"GC_CITY":         "/host/city",
			"GC_STORE_ROOT":   "/host/city/rigs/frontend",
			"GC_BEADS_PREFIX": "fe",
			"GC_DOLT_HOST":    "canonical-dolt.example.com",
			"GC_DOLT_PORT":    "3308",
		},
	}

	err := initBeadsInPod(
		context.Background(),
		fake,
		"gc-test-pod",
		cfg,
		"/workspace/cities/demo-city/rigs/frontend",
		"/workspace/cities/demo-city",
		podManagedDoltHost,
		podManagedDoltPort,
	)
	if err != nil {
		t.Fatalf("initBeadsInPod: %v", err)
	}
	wantStoreRootB64 := base64.StdEncoding.EncodeToString([]byte("/workspace/cities/demo-city/rigs/frontend"))
	wrongStoreRootB64 := base64.StdEncoding.EncodeToString([]byte("/workspace/rigs/frontend"))
	for _, c := range fake.calls {
		if c.method != "execInPod" || len(c.cmd) < 3 {
			continue
		}
		script := c.cmd[2]
		if strings.Contains(script, wantStoreRootB64) {
			if strings.Contains(script, wrongStoreRootB64) {
				t.Fatalf("repair script included default staged root in persistent mode: %s", script)
			}
			return
		}
	}
	t.Fatal("initBeadsInPod did not use persistent workspace store root")
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
	if err := initBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, "/workspace/demo-repo", defaultPodWorkspaceRoot, podManagedDoltHost, podManagedDoltPort); err != nil {
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

	if err := initBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, "/workspace/demo-repo", defaultPodWorkspaceRoot, podManagedDoltHost, podManagedDoltPort); err != nil {
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
		t.Fatalf("repair script uses Bash-only here-string despite running under /bin/sh:\n%s", script)
	}
	if !strings.Contains(script, `printf '%s' "$PATCH" | python3 -c`) {
		t.Fatalf("repair script missing POSIX sh stdin fallback:\n%s", script)
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

func annotatePodWithDesiredRuntimeIdentity(t *testing.T, p *Provider, podName string, cfg runtime.Config) {
	t.Helper()
	identity := mustDesiredProviderRuntimeIdentity(t, p, cfg)
	pod := p.ops.(*fakeK8sOps).pods[podName]
	if pod == nil {
		t.Fatalf("pod %q not found", podName)
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[providerRuntimeFingerprintAnnotation] = identity.Fingerprint
	pod.Annotations[providerRuntimeFingerprintVersionAnnotation] = identity.Version
	pod.Annotations[providerRuntimeImageAnnotation] = p.image
	pod.Annotations[providerRuntimeProviderAnnotation] = "k8s"
}

func mustDesiredProviderRuntimeIdentity(t *testing.T, p *Provider, cfg runtime.Config) runtime.ProviderRuntimeIdentity {
	t.Helper()
	identity, err := p.desiredProviderRuntimeIdentity(cfg)
	if err != nil {
		t.Fatalf("desiredProviderRuntimeIdentity: %v", err)
	}
	return identity
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
}
