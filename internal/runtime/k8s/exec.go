package k8s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// k8sOps abstracts Kubernetes API calls for testability.
// Same pattern as tmux provider's startOps: separates API calls from
// provider logic so unit tests use a fake implementation.
type k8sOps interface {
	createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error)
	getPod(ctx context.Context, name string) (*corev1.Pod, error)
	deletePod(ctx context.Context, name string, grace int64) error
	listPods(ctx context.Context, selector string, fieldSelector string) ([]corev1.Pod, error)
	execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error)
}

// realK8sOps wraps a Kubernetes clientset and REST config for real API calls.
type realK8sOps struct {
	clientset  kubernetes.Interface
	restConfig *rest.Config
	namespace  string
}

func (r *realK8sOps) createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	return r.clientset.CoreV1().Pods(r.namespace).Create(ctx, pod, metav1.CreateOptions{})
}

func (r *realK8sOps) getPod(ctx context.Context, name string) (*corev1.Pod, error) {
	return r.clientset.CoreV1().Pods(r.namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *realK8sOps) deletePod(ctx context.Context, name string, grace int64) error {
	return r.clientset.CoreV1().Pods(r.namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	})
}

func (r *realK8sOps) listPods(ctx context.Context, selector string, fieldSelector string) ([]corev1.Pod, error) {
	opts := metav1.ListOptions{LabelSelector: selector}
	if fieldSelector != "" {
		opts.FieldSelector = fieldSelector
	}
	list, err := r.clientset.CoreV1().Pods(r.namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *realK8sOps) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	req := r.clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(pod).
		Namespace(r.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(r.restConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("creating SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	streamOpts := remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}
	if stdin != nil {
		streamOpts.Stdin = stdin
	}

	if err := exec.StreamWithContext(ctx, streamOpts); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return stdout.String(), fmt.Errorf("exec in pod %s: %s: %w", pod, errMsg, err)
		}
		return stdout.String(), fmt.Errorf("exec in pod %s: %w", pod, err)
	}
	return stdout.String(), nil
}

// fakeK8sOps is an in-memory test double with spy capabilities.
// Records all calls for assertions and returns configurable results.
type fakeK8sOps struct {
	pods  map[string]*corev1.Pod
	calls []fakeCall

	// Configurable behaviors.
	execOutput map[string]string                              // pod+cmd key → stdout
	execErr    map[string]error                               // pod+cmd key → error
	execFunc   func(pod string, cmd []string) (string, error) // dynamic override, checked first
	createErr  error
	deleteErr  error
	getErr     error
	listErr    error
}

type fakeCall struct {
	method    string
	pod       string
	container string
	cmd       []string
	selector  string
}

func newFakeK8sOps() *fakeK8sOps {
	return &fakeK8sOps{
		pods:       make(map[string]*corev1.Pod),
		execOutput: make(map[string]string),
		execErr:    make(map[string]error),
	}
}

func (f *fakeK8sOps) record(method, pod string, cmd []string) {
	f.calls = append(f.calls, fakeCall{method: method, pod: pod, cmd: cmd})
}

func (f *fakeK8sOps) createPod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	f.record("createPod", pod.Name, nil)
	if f.createErr != nil {
		return nil, f.createErr
	}
	p := pod.DeepCopy()
	p.Status.Phase = corev1.PodRunning
	f.pods[pod.Name] = p
	return p, nil
}

func (f *fakeK8sOps) getPod(_ context.Context, name string) (*corev1.Pod, error) {
	f.record("getPod", name, nil)
	if f.getErr != nil {
		return nil, f.getErr
	}
	p, ok := f.pods[name]
	if !ok {
		return nil, fmt.Errorf("pod %q not found", name)
	}
	return p.DeepCopy(), nil
}

func (f *fakeK8sOps) deletePod(_ context.Context, name string, _ int64) error {
	f.record("deletePod", name, nil)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.pods, name)
	return nil
}

func (f *fakeK8sOps) listPods(_ context.Context, selector string, fieldSelector string) ([]corev1.Pod, error) {
	f.calls = append(f.calls, fakeCall{method: "listPods", selector: selector})
	if f.listErr != nil {
		return nil, f.listErr
	}

	// Parse label selector to filter pods.
	var result []corev1.Pod
	for _, p := range f.pods {
		if matchesSelector(p, selector) && matchesFieldSelector(p, fieldSelector) {
			result = append(result, *p.DeepCopy())
		}
	}
	return result, nil
}

func (f *fakeK8sOps) execInPod(_ context.Context, pod, container string, cmd []string, _ io.Reader) (string, error) {
	f.calls = append(f.calls, fakeCall{method: "execInPod", pod: pod, container: container, cmd: cmd})
	if f.execFunc != nil {
		return f.execFunc(pod, cmd)
	}
	key := execKey(pod, cmd)
	if err, ok := f.execErr[key]; ok {
		return "", err
	}
	if out, ok := f.execOutput[key]; ok {
		return out, nil
	}
	return "", nil
}

// setExecResult configures the fake to return specific output for a pod+cmd combo.
// Clears any conflicting entry in the other map.
func (f *fakeK8sOps) setExecResult(pod string, cmd []string, output string, err error) { //nolint:unparam // pod varies by caller context
	key := execKey(pod, cmd)
	if err != nil {
		f.execErr[key] = err
		delete(f.execOutput, key)
	} else {
		f.execOutput[key] = output
		delete(f.execErr, key)
	}
}

func execKey(pod string, cmd []string) string {
	return pod + ":" + strings.Join(cmd, " ")
}

// matchesSelector does simple label matching for the fake.
func matchesSelector(p *corev1.Pod, selector string) bool {
	if selector == "" {
		return true
	}
	for _, part := range strings.Split(selector, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if p.Labels[kv[0]] != kv[1] {
			return false
		}
	}
	return true
}

// matchesFieldSelector does simple field matching for the fake.
func matchesFieldSelector(p *corev1.Pod, fieldSelector string) bool {
	if fieldSelector == "" {
		return true
	}
	for _, part := range strings.Split(fieldSelector, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if kv[0] == "status.phase" {
			if string(p.Status.Phase) != kv[1] {
				return false
			}
		}
	}
	return true
}
