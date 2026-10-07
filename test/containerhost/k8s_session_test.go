//go:build integration

package containerhost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// k8sTestImage is the agent image the pods run: what gc-session-k8s needs
// inside a pod (tmux, base64, coreutils) and a procps pgrep.
const k8sTestImage = "gc-k8s-test-agent:latest"

var k8sImageTools = []string{
	"sh", "bash", "env", "cat", "sleep", "touch", "mkdir", "cp", "rm", "tail", "tar",
	"base64", "timeout", "tmux", "pgrep",
}

// TestK8sSessionConformance runs the session conformance suite against
// contrib/session-scripts/gc-session-k8s through the exec provider, with
// kubectl talking to the emulated container host's pod API and kubelet
// instead of a cluster. It is the test the k8s-session job ran when a
// cluster was configured (never, in practice: the job skipped without the
// GC_K8S_AVAILABLE secret).
func TestK8sSessionConformance(t *testing.T) {
	e := newK8sSessionEnv(t)
	p := sessionexec.NewProvider(e.provider)
	var counter int64

	// Lifecycle tests: each creates its own pod.
	runtimetest.RunLifecycleTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		name := fmt.Sprintf("gc-k8s-conform-%d", id)
		// The external gc-session-k8s script can leave a partially created pod
		// when Start fails. Keep this fallback until that script rolls back its
		// own failed starts; the shared runner owns successful-start cleanup.
		t.Cleanup(func() {
			if err := p.Stop(name); err != nil {
				t.Errorf("Stop(%q) during K8s fallback cleanup: %v", name, err)
			}
		})
		return p, runtime.Config{Command: "sleep 300", WorkDir: e.work}, name
	})

	// Shared-session tests: one pod for all metadata/observation/signaling.
	t.Run("SharedSession", func(t *testing.T) {
		name := "gc-k8s-shared"
		cfg := runtime.Config{Command: "sleep 300", WorkDir: e.work}
		if err := p.Start(context.Background(), name, cfg); err != nil {
			t.Fatalf("Start shared session: %v", err)
		}
		t.Cleanup(func() { _ = p.Stop(name) })
		runtimetest.RunSessionTests(t, p, cfg, name)
	})
}

// TestK8sSessionScript checks what the conformance suite only checks for
// errors: that each operation reaches the agent's tmux inside the pod.
func TestK8sSessionScript(t *testing.T) {
	e := newK8sSessionEnv(t)
	const session = "gc-k8s-script"
	cfg := `{"command": "sh -c 'echo agent-started; exec cat'", "work_dir": "` + e.work + `", "env": {"GC_AGENT": "worker"}}`
	if _, errOut, code := e.op(t, cfg, "start", session); code != 0 {
		t.Fatalf("start: exit %d: %s", code, errOut)
	}
	t.Cleanup(func() { e.op(t, "", "stop", session) })

	t.Run("pod shape", func(t *testing.T) {
		got := e.kubectl(t, "-n", "gc", "get", "pod", session, "-o",
			`jsonpath={.metadata.labels.app} {.metadata.labels.gc-session} {.metadata.labels.gc-agent} {.metadata.annotations.gc-session-name} {.status.phase} {.status.initContainerStatuses[0].state.terminated.exitCode}`)
		if want := "gc-agent " + session + " worker " + session + " Running 0"; got != want {
			t.Fatalf("pod = %q, want %q", got, want)
		}
	})
	t.Run("is-running", func(t *testing.T) {
		if out, _, _ := e.op(t, "", "is-running", session); out != "true" {
			t.Fatalf("is-running = %q, want true", out)
		}
	})
	t.Run("start of a live session fails", func(t *testing.T) {
		_, errOut, code := e.op(t, cfg, "start", session)
		if code == 0 || !strings.Contains(errOut, "already exists") {
			t.Fatalf("second start: exit %d (%q), want a failure naming the existing session", code, errOut)
		}
	})
	t.Run("peek shows the agent output", func(t *testing.T) {
		if out, _, _ := e.op(t, "", "peek", session, "0"); !strings.Contains(out, "agent-started") {
			t.Fatalf("peek = %q, want agent-started", out)
		}
	})
	t.Run("nudge types into the agent", func(t *testing.T) {
		e.op(t, "hello-from-nudge", "nudge", session)
		e.op(t, "", "send-keys", session, "Enter")
		// cat echoes the line back once Enter arrives.
		if out, _, _ := e.op(t, "", "peek", session, "20"); strings.Count(out, "hello-from-nudge") < 2 {
			t.Fatalf("pane after nudge = %q, want the typed line and cat's echo", out)
		}
	})
	t.Run("meta round-trip", func(t *testing.T) {
		e.op(t, "v-42", "set-meta", session, "k")
		if out, _, _ := e.op(t, "", "get-meta", session, "k"); out != "v-42" {
			t.Errorf("get-meta = %q, want v-42", out)
		}
		e.op(t, "", "remove-meta", session, "k")
		if out, _, _ := e.op(t, "", "get-meta", session, "k"); out != "" {
			t.Errorf("get-meta after remove-meta = %q, want empty", out)
		}
	})
	t.Run("process-alive sees only the pod", func(t *testing.T) {
		if out, _, _ := e.op(t, "cat", "process-alive", session); out != "true" {
			t.Errorf("process-alive cat = %q, want true", out)
		}
		if out, _, _ := e.op(t, "nonexistent-process-xyz", "process-alive", session); out != "false" {
			t.Errorf("process-alive nonexistent = %q, want false", out)
		}
	})
	t.Run("get-last-activity returns RFC3339", func(t *testing.T) {
		out, _, _ := e.op(t, "", "get-last-activity", session)
		if !regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`).MatchString(out) {
			t.Fatalf("get-last-activity = %q, want an RFC3339 UTC timestamp", out)
		}
	})
	t.Run("copy-to and copy-from", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "note.txt")
		if err := os.WriteFile(src, []byte("copied-into-pod"), 0o644); err != nil {
			t.Fatal(err)
		}
		e.op(t, "", "copy-to", session, src, "notes")
		if out, _, code := e.op(t, "", "copy-from", session, "/workspace/notes/note.txt"); code != 0 || out != "copied-into-pod" {
			t.Fatalf("copy-from = %q (exit %d), want copied-into-pod", out, code)
		}
	})
	t.Run("list-running", func(t *testing.T) {
		if out, _, _ := e.op(t, "", "list-running", "gc-k8s"); out != session {
			t.Fatalf("list-running gc-k8s = %q, want %s", out, session)
		}
	})
	t.Run("stop removes the pod and its processes", func(t *testing.T) {
		if _, _, code := e.op(t, "", "stop", session); code != 0 {
			t.Fatalf("stop: exit %d", code)
		}
		if out, _, _ := e.op(t, "", "is-running", session); out != "false" {
			t.Fatalf("is-running after stop = %q, want false", out)
		}
		if got := e.kubectl(t, "-n", "gc", "get", "pods", "-o", "name"); got != "" {
			t.Fatalf("pods after stop = %q, want none", got)
		}
		all, err := e.host.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 0 {
			t.Fatalf("%d sandboxes survived stop", len(all))
		}
	})
}

type k8sSessionEnv struct {
	*dockerSessionEnv
	provider string
	work     string
}

func newK8sSessionEnv(t *testing.T) *k8sSessionEnv {
	t.Helper()
	base := newDockerSessionEnv(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := base.host.BuildImage(k8sTestImage, ImageSpec{Tools: k8sImageTools}, self); err != nil {
		t.Fatal(err)
	}
	base.script = k8sSessionScript(t)
	// The exec provider runs the adapter with the test process's
	// environment; the wrapper adds the emulated host and the image.
	provider := filepath.Join(base.host.Root, "gc-session-k8s")
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	for _, kv := range append(base.environ(), "GC_K8S_IMAGE="+k8sTestImage) {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(v))
	}
	fmt.Fprintf(&b, "exec bash %s \"$@\"\n", shellQuote(base.script))
	if err := os.WriteFile(provider, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return &k8sSessionEnv{dockerSessionEnv: base, provider: provider, work: t.TempDir()}
}

// op runs one gc-session-k8s operation through the wrapper.
func (e *k8sSessionEnv) op(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return e.run(t, stdin, e.provider, args...)
}

func (e *k8sSessionEnv) kubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, code := e.run(t, "", filepath.Join(e.bin, "kubectl"), args...)
	if code != 0 {
		t.Logf("kubectl %v: exit %d: %s", args, code, errOut)
	}
	return out
}

// k8sSessionScript is the adapter under test: the go_test's data
// (GC_SESSION_K8S_SCRIPT) under bazel, the checkout's copy under go test.
func k8sSessionScript(t *testing.T) string {
	t.Helper()
	if path := bazeltest.DataPath(t, "GC_SESSION_K8S_SCRIPT"); path != "" {
		return path
	}
	return filepath.Join(bazeltest.RepoRoot(t), "contrib", "session-scripts", "gc-session-k8s")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
