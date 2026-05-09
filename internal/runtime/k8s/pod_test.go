package k8s

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/gastownhall/gascity/internal/runtime"
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
