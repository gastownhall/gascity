package containerhost

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestResourceArgs(t *testing.T) {
	for _, tt := range []struct {
		in   []string
		want []string
		ok   bool
	}{
		{[]string{"pods"}, nil, true},
		{[]string{"pod", "a", "b"}, []string{"a", "b"}, true},
		{[]string{"pod/a", "pods/b"}, []string{"a", "b"}, true},
		{[]string{"po", "a"}, []string{"a"}, true},
		{[]string{"deployments"}, nil, false},
		{[]string{"pod/a", "svc/b"}, nil, false},
		{nil, nil, false},
	} {
		got, err := resourceArgs(tt.in)
		if (err == nil) != tt.ok || len(got) != len(tt.want) || (len(got) > 0 && !reflect.DeepEqual(got, tt.want)) {
			t.Errorf("resourceArgs(%q) = %q, %v; want %q, ok=%v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}

type kubectlRun struct {
	t *testing.T
	h *Host
}

func (r kubectlRun) run(stdin string, args ...string) (string, string, int) {
	r.t.Helper()
	var out, errOut bytes.Buffer
	code := r.h.Kubectl(args, strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), code
}

func newKubectlRun(t *testing.T) kubectlRun {
	t.Helper()
	h, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return kubectlRun{t: t, h: h}
}

// A pod whose image is absent stays Pending, which exercises the API side
// (store, selectors, output, delete) without starting any process.
const pendingPod = `{"apiVersion": "v1", "kind": "Pod",
 "metadata": {"name": "p1", "labels": {"app": "gc-agent", "gc-session": "s1"}},
 "spec": {"restartPolicy": "Never",
  "containers": [{"name": "agent", "image": "absent:latest", "command": ["sleep", "1"],
   "volumeMounts": [{"name": "ws", "mountPath": "/workspace"}]}],
  "volumes": [{"name": "ws", "emptyDir": {}}]}}`

func TestKubectlEmptyListPrintsAnEmptyItemsArray(t *testing.T) {
	k := newKubectlRun(t)
	out, _, code := k.run("", "-n", "gc", "get", "pods", "-o", "json")
	if code != 0 || !strings.Contains(out, `"items": []`) {
		t.Fatalf("empty get -o json = %q (exit %d), want an empty items array", out, code)
	}
	if out, _, _ := k.run("", "-n", "gc", "get", "pods", "-o", "jsonpath={.items[*].metadata.name}"); out != "" {
		t.Fatalf("empty jsonpath = %q, want empty", out)
	}
}

func TestKubectlPodAPIWithoutAKubelet(t *testing.T) {
	k := newKubectlRun(t)
	if out, errOut, code := k.run(pendingPod, "-n", "gc", "apply", "-f", "-"); code != 0 || out != "pod/p1 created\n" {
		t.Fatalf("apply = %q %q (exit %d)", out, errOut, code)
	}
	if out, _, code := k.run(pendingPod, "-n", "gc", "apply", "-f", "-"); code != 0 || out != "pod/p1 unchanged\n" {
		t.Fatalf("re-apply = %q (exit %d), want unchanged", out, code)
	}
	out, _, _ := k.run("", "-n", "gc", "get", "pod", "p1", "-o", "jsonpath={.status.phase} {.status.containerStatuses[0].state.waiting.reason}")
	if out != "Pending ErrImagePull" {
		t.Fatalf("status = %q, want Pending ErrImagePull", out)
	}
	if out, _, _ := k.run("", "-n", "gc", "get", "pods", "-l", "gc-session=s1", "-o", "jsonpath={.items[0].metadata.name}"); out != "p1" {
		t.Fatalf("label selector = %q, want p1", out)
	}
	if out, _, _ := k.run("", "-n", "gc", "get", "pods", "-l", "gc-session=s1", "--field-selector=status.phase=Running", "-o", "name"); out != "" {
		t.Fatalf("Running field selector = %q, want no Pending pod", out)
	}
	if _, errOut, code := k.run("", "-n", "gc", "get", "pods", "--field-selector=spec.schedulerName=x"); code == 0 || !strings.Contains(errOut, "field label not supported") {
		t.Fatalf("unsupported field selector: exit %d %q", code, errOut)
	}
	if _, errOut, code := k.run("", "-n", "other", "get", "pod", "p1"); code != 1 || !strings.Contains(errOut, `pods "p1" not found`) {
		t.Fatalf("other namespace: exit %d %q, want NotFound", code, errOut)
	}
	if _, errOut, code := k.run("", "-n", "gc", "exec", "p1", "--", "true"); code != 1 || !strings.Contains(errOut, "container not found") {
		t.Fatalf("exec into a Pending pod: exit %d %q", code, errOut)
	}
	if out, _, code := k.run("", "-n", "gc", "delete", "pod", "-l", "gc-session=s1", "--ignore-not-found", "--grace-period=5"); code != 0 || out != "pod \"p1\" deleted\n" {
		t.Fatalf("delete = %q (exit %d)", out, code)
	}
	if _, _, code := k.run("", "-n", "gc", "wait", "--for=delete", "pod/p1", "--timeout=1s"); code != 0 {
		t.Fatalf("wait --for=delete after delete: exit %d", code)
	}
	if _, _, code := k.run("", "-n", "gc", "delete", "pod", "p1", "--ignore-not-found"); code != 0 {
		t.Fatalf("idempotent delete: exit %d", code)
	}
}

func TestKubectlRejectsUnmodelledInput(t *testing.T) {
	k := newKubectlRun(t)
	hostPath := strings.Replace(pendingPod, `"emptyDir": {}`, `"hostPath": {"path": "/"}`, 1)
	for name, tt := range map[string]struct {
		stdin string
		args  []string
	}{
		"hostPath volume":   {hostPath, []string{"apply", "-f", "-"}},
		"non-pod kind":      {`{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "d"}}`, []string{"apply", "-f", "-"}},
		"namespace clash":   {strings.Replace(pendingPod, `"name": "p1",`, `"name": "p1", "namespace": "x",`, 1), []string{"-n", "gc", "apply", "-f", "-"}},
		"unsupported verb":  {"", []string{"logs", "p1"}},
		"unknown get flag":  {"", []string{"get", "pods", "--watch"}},
		"pod to local copy": {"", []string{"cp", "p1:/x", "/tmp/x"}},
	} {
		if _, _, code := k.run(tt.stdin, tt.args...); code == 0 {
			t.Errorf("%s: kubectl %q succeeded, want a failure", name, tt.args)
		}
	}
}
