//go:build acceptance_c

package tutorialgoldens

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type tutorialWorkspace struct {
	t         *testing.T
	env       *tutorialEnv
	cwd       string
	warnings  []string
	warnMu    sync.Mutex
	diagNotes []string
	diagMu    sync.Mutex
}

const defaultShellTimeout = 90 * time.Second

func newTutorialWorkspace(t *testing.T) *tutorialWorkspace {
	t.Helper()
	env := newTutorialEnv(t)
	w := &tutorialWorkspace{
		t:   t,
		env: env,
		cwd: env.Home,
	}
	t.Cleanup(func() {
		var cityDirs []string
		_ = filepath.WalkDir(env.Home, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			if d.Name() == "city.toml" {
				cityDirs = append(cityDirs, filepath.Dir(path))
			}
			return nil
		})
		for _, cityDir := range cityDirs {
			_, _ = runEnvCommandWithTimeout(env, cityDir, 20*time.Second, "gc", "stop")
		}
		_, _ = runEnvCommandWithTimeout(env, env.Home, 20*time.Second, "gc", "supervisor", "stop")
	})
	return w
}

func (w *tutorialWorkspace) home() string {
	return w.env.Home
}

func (w *tutorialWorkspace) setCWD(dir string) {
	w.cwd = dir
}

func (w *tutorialWorkspace) noteWarning(format string, args ...any) {
	w.warnMu.Lock()
	defer w.warnMu.Unlock()
	w.warnings = append(w.warnings, fmt.Sprintf(format, args...))
}

func (w *tutorialWorkspace) noteDiagnostic(format string, args ...any) {
	w.diagMu.Lock()
	defer w.diagMu.Unlock()
	w.diagNotes = append(w.diagNotes, fmt.Sprintf(format, args...))
}

func (w *tutorialWorkspace) attachDiagnostics(t *testing.T, pageName string) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("diagnostics for %s", pageName)
		if len(w.warnings) > 0 {
			t.Logf("soft workarounds:\n%s", strings.Join(w.warnings, "\n"))
		}
		if len(w.diagNotes) > 0 {
			t.Logf("diagnostic notes:\n%s", strings.Join(w.diagNotes, "\n"))
		}
		for _, cmd := range []string{
			"gc status",
			"gc session list",
			"bd list --json --limit=20",
			"ls -la",
			"find . -maxdepth 3 -type f | sort",
		} {
			out, err := w.runShell(cmd, "")
			label := cmd
			if err != nil {
				t.Logf("%s failed: %v\n%s", label, err, out)
				continue
			}
			if strings.TrimSpace(out) != "" {
				t.Logf("%s:\n%s", label, out)
			}
		}
		controllerLog := filepath.Join(w.cwd, ".gc", "acceptance-controller.log")
		if data, err := os.ReadFile(controllerLog); err == nil {
			t.Logf("%s:\n%s", controllerLog, string(data))
		}
	})
}

func (w *tutorialWorkspace) runShell(command, stdin string) (string, error) {
	return w.runShellWithTimeout(defaultShellTimeout, command, stdin)
}

func (w *tutorialWorkspace) runShellWithTimeout(timeout time.Duration, command, stdin string) (string, error) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = w.cwd
	cmd.Env = w.env.Env.List()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return string(out), fmt.Errorf("timed out after %s: %w", timeout, ctx.Err())
	}
	return string(out), err
}

func (w *tutorialWorkspace) sessionTargetByID(sessionID, template string) (string, error) {
	w.t.Helper()
	command := "gc session list"
	if template != "" {
		command += " --template " + template
	}
	out, err := w.runShell(command, "")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != sessionID {
			continue
		}
		if template != "" && fields[1] != template {
			continue
		}
		return fields[4], nil
	}
	return "", fmt.Errorf("session %s not found in `%s`\n%s", sessionID, command, out)
}

type runningShell struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc

	mu     sync.Mutex
	buffer bytes.Buffer
	done   chan error
}

func (w *tutorialWorkspace) startShell(command, stdin string) (*runningShell, error) {
	w.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = w.cwd
	cmd.Env = w.env.Env.List()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	rs := &runningShell{
		cmd:    cmd,
		cancel: cancel,
		done:   make(chan error, 1),
	}
	cmd.Stdout = rs
	cmd.Stderr = rs
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	go func() {
		rs.done <- cmd.Wait()
	}()
	return rs, nil
}

func (r *runningShell) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buffer.Write(p)
}

func (r *runningShell) output() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buffer.String()
}

func (r *runningShell) waitFor(substr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(r.output(), substr) {
			return nil
		}
		select {
		case err := <-r.done:
			if err != nil && !strings.Contains(r.output(), substr) {
				return fmt.Errorf("process exited before %q: %w\n%s", substr, err, r.output())
			}
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for %q\n%s", substr, r.output())
}

func (r *runningShell) stop() error {
	r.cancel()
	select {
	case err := <-r.done:
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case <-time.After(5 * time.Second):
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
		<-r.done
		return nil
	}
}

func expandHome(home, path string) string {
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

var beadIDPattern = regexp.MustCompile(`\b[a-z]{2}-[a-z0-9.]+\b`)

func firstBeadID(s string) string {
	return beadIDPattern.FindString(s)
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
}

func writeFile(t *testing.T, path, body string, perm os.FileMode) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func appendFile(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, body); err != nil {
		t.Fatalf("appending %s: %v", path, err)
	}
}

func replaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	body := string(data)
	if !strings.Contains(body, old) {
		t.Fatalf("%s missing expected snippet %q", path, old)
	}
	body = strings.Replace(body, old, new, 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func waitForCondition(t *testing.T, timeout, interval time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(interval)
	}
	return false
}

func runEnvCommandWithTimeout(env *tutorialEnv, dir string, timeout time.Duration, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if len(argv) == 0 {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env.Env.List()
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return string(out), fmt.Errorf("timed out after %s: %w", timeout, ctx.Err())
	}
	return string(out), err
}
