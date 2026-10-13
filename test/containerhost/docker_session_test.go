//go:build integration

package containerhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// TestDockerSessionScript runs scripts/gc-session-docker through the exec
// session protocol against the emulated container host: every scenario of
// the former real-Docker harness (scripts/test-docker-session), with the
// adapter's tmux running for real inside each emulated container.
func TestDockerSessionScript(t *testing.T) {
	e := newDockerSessionEnv(t)
	const session = "gc-docker-test"
	work := t.TempDir()

	start := func(t *testing.T, name, image string, extra map[string]any) (string, int) {
		t.Helper()
		cfg := map[string]any{
			"command":  "sleep 300",
			"work_dir": work,
			"env":      map[string]string{"GC_DOCKER_IMAGE": image, "GC_DOCKER_HOME_MOUNT": "false"},
		}
		for k, v := range extra {
			cfg[k] = v
		}
		out, errOut, code := e.op(t, mustJSON(t, cfg), "start", name)
		return out + errOut, code
	}
	mustStart := func(t *testing.T, name string, extra map[string]any) {
		t.Helper()
		out, code := start(t, name, testImage, extra)
		if code != 0 {
			t.Fatalf("start %s: exit %d\n%s", name, code, out)
		}
	}
	stop := func(name string) { e.op(t, "", "stop", name) }
	isRunning := func(t *testing.T, name string) string {
		t.Helper()
		out, _, _ := e.op(t, "", "is-running", name)
		return out
	}

	t.Run("pre-pull fails on missing image", func(t *testing.T) {
		_, code := start(t, session+"-prepull", "gc-nonexistent-image-42:latest", nil)
		stop(session + "-prepull")
		if code != 1 {
			t.Fatalf("start with a missing image: exit %d, want 1", code)
		}
	})

	t.Run("tmux requirement fails on image without tmux", func(t *testing.T) {
		out, code := start(t, session+"-notmux", noTmuxImage, nil)
		stop(session + "-notmux")
		if code != 1 {
			t.Fatalf("start without tmux: exit %d, want 1\n%s", code, out)
		}
		if !strings.Contains(out, "tmux not found") {
			t.Fatalf("start without tmux: output %q, want it to mention %q", out, "tmux not found")
		}
	})

	t.Run("start with ready_prompt_prefix", func(t *testing.T) {
		mustStart(t, session+"-ready", map[string]any{"command": "entrypoint.sh", "ready_prompt_prefix": "> "})
		defer stop(session + "-ready")
		if got := isRunning(t, session+"-ready"); got != "true" {
			t.Fatalf("prompt-detected container is-running = %q, want true", got)
		}
	})

	t.Run("start with process_names", func(t *testing.T) {
		mustStart(t, session+"-proc", map[string]any{"command": "entrypoint.sh", "process_names": []string{"sleep"}})
		defer stop(session + "-proc")
		if got := isRunning(t, session+"-proc"); got != "true" {
			t.Fatalf("process-detected container is-running = %q, want true", got)
		}
	})

	t.Run("start with ready_delay_ms", func(t *testing.T) {
		begin := time.Now()
		mustStart(t, session+"-delay", map[string]any{"command": "delay-entrypoint.sh", "ready_delay_ms": 1000})
		elapsed := time.Since(begin)
		stop(session + "-delay")
		if elapsed < time.Second {
			t.Fatalf("ready_delay_ms 1000 returned after %s, want >= 1s", elapsed)
		}
	})

	t.Run("start with no hints", func(t *testing.T) {
		mustStart(t, session+"-nohints", nil)
		defer stop(session + "-nohints")
		if got := isRunning(t, session+"-nohints"); got != "true" {
			t.Fatalf("no-hints container is-running = %q, want true", got)
		}
	})

	t.Run("start idempotency keeps the same container", func(t *testing.T) {
		name := session + "-idem"
		mustStart(t, name, nil)
		defer stop(name)
		first := e.docker(t, "inspect", "-f", "{{.Id}}", name)
		mustStart(t, name, nil)
		second := e.docker(t, "inspect", "-f", "{{.Id}}", name)
		if first == "" || first != second {
			t.Fatalf("container ID changed across idempotent start: %q -> %q", first, second)
		}
	})

	// The remaining scenarios share one session, as the harness did.
	mustStart(t, session, map[string]any{"command": "entrypoint.sh", "ready_prompt_prefix": "> "})
	t.Cleanup(func() { stop(session) })

	t.Run("container labels", func(t *testing.T) {
		if got := e.docker(t, "inspect", "-f", `{{index .Config.Labels "gc.managed"}}`, session); got != "true" {
			t.Errorf("gc.managed label = %q, want true", got)
		}
		if got := e.docker(t, "inspect", "-f", `{{index .Config.Labels "gc.agent"}}`, session); got != session {
			t.Errorf("gc.agent label = %q, want %q", got, session)
		}
	})

	t.Run("is-running", func(t *testing.T) {
		if got := isRunning(t, session); got != "true" {
			t.Fatalf("is-running = %q, want true", got)
		}
	})

	t.Run("peek shows output", func(t *testing.T) {
		out, _, _ := e.op(t, "", "peek", session, "5")
		if !strings.Contains(out, "Initializing") {
			t.Fatalf("peek 5 = %q, want it to contain Initializing", out)
		}
	})

	t.Run("peek lines=0 captures output", func(t *testing.T) {
		name := session + "-peek0"
		mustStart(t, name, map[string]any{"command": "entrypoint.sh", "ready_prompt_prefix": "> "})
		defer stop(name)
		out, _, _ := e.op(t, "", "peek", name, "0")
		if !strings.Contains(out, "Initializing") {
			t.Fatalf("peek 0 = %q, want it to contain Initializing", out)
		}
	})

	t.Run("process-alive", func(t *testing.T) {
		if out, _, _ := e.op(t, "sleep", "process-alive", session); out != "true" {
			t.Errorf("process-alive sleep = %q, want true", out)
		}
		if out, _, _ := e.op(t, "nonexistent-process-xyz", "process-alive", session); out != "false" {
			t.Errorf("process-alive nonexistent-process-xyz = %q, want false", out)
		}
	})

	t.Run("process-alive via tmux pane command", func(t *testing.T) {
		paneCmd := e.docker(t, "exec", "-e", "TMUX_TMPDIR=/run/gc-tmux", session, "tmux", "-u",
			"display-message", "-t", "main:0.0", "-p", "#{pane_current_command}")
		if paneCmd != "sleep" {
			t.Errorf("pane_current_command = %q, want sleep (the entrypoint execs it)", paneCmd)
		}
		if out, _, _ := e.op(t, paneCmd, "process-alive", session); out != "true" {
			t.Errorf("process-alive %q = %q, want true", paneCmd, out)
		}
	})

	t.Run("set-meta get-meta remove-meta", func(t *testing.T) {
		e.op(t, "test-value-42", "set-meta", session, "my-key")
		if out, _, _ := e.op(t, "", "get-meta", session, "my-key"); out != "test-value-42" {
			t.Errorf("meta round-trip = %q, want test-value-42", out)
		}
		if out, _, _ := e.op(t, "", "get-meta", session, "no-such-key"); out != "" {
			t.Errorf("missing meta = %q, want empty", out)
		}
		e.op(t, "", "remove-meta", session, "my-key")
		if out, _, _ := e.op(t, "", "get-meta", session, "my-key"); out != "" {
			t.Errorf("removed meta = %q, want empty", out)
		}
	})

	t.Run("nudge delivers text", func(t *testing.T) {
		name := session + "-nudge"
		mustStart(t, name, map[string]any{"command": "cat"})
		defer stop(name)
		// nudge returns after its own paste debounce and Enter, by which
		// time cat's tty echo is on the pane.
		e.op(t, "hello-from-nudge", "nudge", name)
		if out, _, _ := e.op(t, "", "peek", name, "20"); !strings.Contains(out, "hello-from-nudge") {
			t.Fatalf("pane after nudge = %q, want hello-from-nudge", out)
		}
	})

	t.Run("get-last-activity returns RFC3339", func(t *testing.T) {
		name := session + "-activity"
		mustStart(t, name, nil)
		defer stop(name)
		out, _, _ := e.op(t, "", "get-last-activity", name)
		if !regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`).MatchString(out) {
			t.Fatalf("get-last-activity = %q, want an RFC3339 UTC timestamp", out)
		}
	})

	t.Run("clear-scrollback", func(t *testing.T) {
		name := session + "-clear"
		mustStart(t, name, map[string]any{"command": "scroll-entrypoint.sh", "ready_prompt_prefix": "SCROLL_DONE"})
		defer stop(name)
		before, _, _ := e.op(t, "", "peek", name, "0")
		if !strings.Contains(before, "scrollline-1\n") {
			t.Fatalf("scrollback before clear = %q, want the early lines", before)
		}
		e.op(t, "", "clear-scrollback", name)
		after, _, _ := e.op(t, "", "peek", name, "0")
		if strings.Contains(after, "scrollline-1\n") {
			t.Fatalf("scrollback after clear-scrollback still has the early lines: %q", after)
		}
	})

	t.Run("list-running finds session", func(t *testing.T) {
		out, _, _ := e.op(t, "", "list-running", "gc-docker-test")
		if !strings.Contains(out, session) {
			t.Fatalf("list-running gc-docker-test = %q, want %s", out, session)
		}
	})

	t.Run("session_setup runs inside the container", func(t *testing.T) {
		name := session + "-setup"
		// The marker is under the bind-mounted work dir, so a touch inside
		// the container is visible on the host.
		marker := filepath.Join(work, "setup-marker")
		mustStart(t, name, map[string]any{"session_setup": []string{"touch " + marker}})
		defer stop(name)
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("session_setup marker: %v", err)
		}
	})

	t.Run("session_setup rewrites tmux targets to the in-container session", func(t *testing.T) {
		name := session + "-tmuxsetup"
		mustStart(t, name, map[string]any{
			"session_setup": []string{"tmux set-option -t " + name + " status-right 'DOCKER_SETUP_OK'"},
		})
		defer stop(name)
		got := e.docker(t, "exec", "-e", "TMUX_TMPDIR=/run/gc-tmux", name, "tmux", "-u",
			"show-options", "-t", "main", "-v", "status-right")
		if !strings.Contains(got, "DOCKER_SETUP_OK") {
			t.Fatalf("in-container status-right = %q, want DOCKER_SETUP_OK", got)
		}
	})

	t.Run("session_setup_script is piped from the host into the container", func(t *testing.T) {
		name := session + "-script"
		marker := filepath.Join(work, "script-marker")
		script := filepath.Join(t.TempDir(), "setup.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustStart(t, name, map[string]any{"session_setup_script": script})
		defer stop(name)
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("session_setup_script marker: %v", err)
		}
	})

	t.Run("TERM propagated to the container", func(t *testing.T) {
		env := e.docker(t, "inspect", "-f", "{{range .Config.Env}}{{println .}}{{end}}", session)
		if !regexp.MustCompile(`(?m)^TERM=.+$`).MatchString(env) {
			t.Fatalf("container env = %q, want a TERM entry", env)
		}
	})

	t.Run("interrupt", func(t *testing.T) {
		if _, errOut, code := e.op(t, "", "interrupt", session); code != 0 {
			t.Fatalf("interrupt: exit %d: %s", code, errOut)
		}
	})

	t.Run("unknown operation exits 2", func(t *testing.T) {
		if _, _, code := e.op(t, "", "future-op", session); code != 2 {
			t.Fatalf("future-op: exit %d, want 2", code)
		}
	})

	t.Run("stop", func(t *testing.T) {
		if _, errOut, code := e.op(t, "", "stop", session); code != 0 {
			t.Fatalf("stop: exit %d: %s", code, errOut)
		}
		if got := isRunning(t, session); got != "false" {
			t.Fatalf("is-running after stop = %q, want false", got)
		}
		if pids := containerPIDsByName(t, e.host, session); len(pids) != 0 {
			t.Fatalf("processes %v of %s survived stop", pids, session)
		}
	})

	t.Run("stop is idempotent", func(t *testing.T) {
		if _, errOut, code := e.op(t, "", "stop", session); code != 0 {
			t.Fatalf("second stop: exit %d: %s", code, errOut)
		}
	})
}

const (
	testImage   = "gc-test-agent"
	noTmuxImage = "gc-test-agent-notmux"
)

// The harness's test images: Alpine + procps + tmux + bash with three
// entrypoint scripts, and the same without tmux.
var (
	baseTools = []string{"sh", "bash", "env", "cat", "sleep", "seq", "touch", "mkdir", "chmod", "timeout", "which", "pgrep"}

	entrypoints = map[string]string{
		"bin/entrypoint.sh":        "#!/bin/sh\necho \"Initializing...\"\nsleep 1\necho \"> \"\nexec sleep 300\n",
		"bin/delay-entrypoint.sh":  "#!/bin/sh\necho \"delayed start\"\nexec sleep 300\n",
		"bin/scroll-entrypoint.sh": "#!/bin/sh\nfor i in $(seq 1 50); do echo \"scrollline-$i\"; done\necho \"SCROLL_DONE\"\nexec sleep 300\n",
	}
)

type dockerSessionEnv struct {
	host   *Host
	bin    string
	home   string
	script string
}

func newDockerSessionEnv(t *testing.T) *dockerSessionEnv {
	t.Helper()
	for _, tool := range []string{"bash", "jq", "tmux"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required (pinned in the rbe-west worker image, tools/rbe/worker-env.txt): %v", tool, err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(stateParent, "h")
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.RemoveAll(); err != nil {
			t.Errorf("removing emulated containers: %v", err)
		}
	})
	if err := h.BuildImage(testImage, ImageSpec{Tools: append([]string{"tmux"}, baseTools...), Files: entrypoints}, self); err != nil {
		t.Fatal(err)
	}
	if err := h.BuildImage(noTmuxImage, ImageSpec{Tools: baseTools}, self); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "cli")
	if err := InstallCLI(bin, self); err != nil {
		t.Fatal(err)
	}
	return &dockerSessionEnv{
		host:   h,
		bin:    bin,
		home:   t.TempDir(),
		script: dockerSessionScript(t),
	}
}

// dockerSessionScript is the adapter under test: the go_test's data
// (GC_SESSION_DOCKER_SCRIPT) under bazel, the checkout's copy under go test.
func dockerSessionScript(t *testing.T) string {
	t.Helper()
	if path := bazeltest.DataPath(t, "GC_SESSION_DOCKER_SCRIPT"); path != "" {
		return path
	}
	return filepath.Join(bazeltest.RepoRoot(t), "scripts", "gc-session-docker")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *dockerSessionEnv) environ() []string {
	return []string{
		"PATH=" + e.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + e.home,
		RootEnv + "=" + e.host.Root,
		"LANG=C.UTF-8",
	}
}

// op runs one exec-protocol operation; stdout is trimmed of its trailing
// newline as the exec provider trims it.
func (e *dockerSessionEnv) op(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return e.run(t, stdin, "bash", append([]string{e.script}, args...)...)
}

// docker runs the emulated docker CLI directly and returns trimmed stdout.
func (e *dockerSessionEnv) docker(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, code := e.run(t, "", filepath.Join(e.bin, "docker"), args...)
	if code != 0 {
		t.Logf("docker %v: exit %d: %s", args, code, errOut)
	}
	return out
}

func (e *dockerSessionEnv) run(t *testing.T, stdin, name string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %s %v: %v", name, args, err)
		}
		code = exitErr.ExitCode()
	}
	return strings.TrimRight(out.String(), "\n"), errOut.String(), code
}

func containerPIDsByName(t *testing.T, h *Host, name string) []int {
	t.Helper()
	c, err := h.Lookup(name)
	if err != nil {
		return nil
	}
	return containerPIDs(c.ID)
}
