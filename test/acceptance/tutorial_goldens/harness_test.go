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
	"syscall"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
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

const (
	defaultShellTimeout       = 90 * time.Second
	gcInitTransientRetryLimit = 2
)

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
		_, _ = runEnvCommandWithTimeout(env, env.Home, 30*time.Second, "gc", "supervisor", "stop", "--wait")
	})
	return w
}

func (w *tutorialWorkspace) home() string {
	return w.env.Home
}

func (w *tutorialWorkspace) setCWD(dir string) {
	w.cwd = dir
	if dir != "" {
		_ = helpers.EnsureClaudeProjectState(w.env.Env, dir)
	}
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
	trimmed := strings.TrimSpace(command)
	for attempt := 1; attempt <= gcInitTransientRetryLimit; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		cmd := exec.CommandContext(ctx, "bash", "-c", command)
		cmd.Dir = w.cwd
		cmd.Env = w.env.Env.List()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		out, err := cmd.CombinedOutput()
		cancel()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return string(out), fmt.Errorf("timed out after %s: %w", timeout, ctx.Err())
		}
		if err == nil {
			if strings.HasPrefix(trimmed, "gc init ") {
				if cfgErr := w.configureInitializedCities(); cfgErr != nil {
					return string(out), cfgErr
				}
			}
			return string(out), nil
		}
		if !strings.HasPrefix(trimmed, "gc init ") || !isTransientGCInitManagedDoltFailure(string(out)) || attempt == gcInitTransientRetryLimit {
			return string(out), err
		}
		w.noteWarning("tutorial runtime workaround: retrying `%s` after transient managed Dolt startup failure (attempt %d/%d)", trimmed, attempt+1, gcInitTransientRetryLimit)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return "", fmt.Errorf("unreachable")
}

func isTransientGCInitManagedDoltFailure(out string) bool {
	msg := strings.ToLower(out)
	return strings.Contains(msg, "dolt server exited during startup") ||
		strings.Contains(msg, "did not become query-ready after 30s")
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

func (w *tutorialWorkspace) firstSessionByTemplate(template string) (string, string, error) {
	w.t.Helper()
	command := "gc session list --template " + template
	out, err := w.runShell(command, "")
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if fields[0] == "ID" || fields[1] != template {
			continue
		}
		return fields[0], fields[4], nil
	}
	return "", "", fmt.Errorf("no session found for template %s in `%s`\n%s", template, command, out)
}

func (w *tutorialWorkspace) firstSessionByTarget(target string) (string, error) {
	w.t.Helper()
	command := "gc session list"
	out, err := w.runShell(command, "")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if fields[0] == "ID" || fields[4] != target {
			continue
		}
		return fields[0], nil
	}
	return "", fmt.Errorf("no session found for target %s in `%s`\n%s", target, command, out)
}

func (w *tutorialWorkspace) waitForSessionByTemplateOrTarget(template, target string, timeout, interval time.Duration) (string, error) {
	w.t.Helper()

	var sessionID string
	ok := waitForCondition(w.t, timeout, interval, func() bool {
		if template != "" {
			if id, _, err := w.firstSessionByTemplate(template); err == nil && id != "" {
				sessionID = id
				return true
			}
		}
		if target != "" {
			if id, err := w.firstSessionByTarget(target); err == nil && id != "" {
				sessionID = id
				return true
			}
		}
		return false
	})
	if ok {
		return sessionID, nil
	}

	out, err := w.runShell("gc session list", "")
	if err != nil {
		return "", fmt.Errorf("resolving session template=%q target=%q: %w", template, target, err)
	}
	return "", fmt.Errorf("no session found for template=%q target=%q in `gc session list`\n%s", template, target, out)
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
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = w.cwd
	cmd.Env = w.env.Env.List()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	if r.cmd.Process != nil {
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
	}
	select {
	case err := <-r.done:
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case <-time.After(5 * time.Second):
		if r.cmd.Process != nil {
			_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
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

func (w *tutorialWorkspace) configureInitializedCities() error {
	hostHome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	observePaths := []string{
		filepath.Join(hostHome, ".claude", "projects"),
		filepath.Join(hostHome, ".codex", "sessions"),
		filepath.Join(hostHome, ".gemini", "tmp"),
	}
	return filepath.WalkDir(w.env.Home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || d.Name() != "city.toml" {
			return nil
		}
		cityDir := filepath.Dir(path)
		if err := helpers.EnsureClaudeProjectState(w.env.Env, cityDir); err != nil {
			return err
		}
		return ensureTutorialObservePaths(path, observePaths)
	})
}

func tutorialSocketName(root string) string {
	base := filepath.Base(root)
	if len(base) > 20 {
		base = base[len(base)-20:]
	}
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_':
			return r
		default:
			return '-'
		}
	}, base)
	return "tg-" + base
}

func ensureTutorialSessionSocket(cityTomlPath, socket string) error {
	data, err := os.ReadFile(cityTomlPath)
	if err != nil {
		return err
	}
	body := string(data)
	if strings.Contains(body, "socket = ") {
		return nil
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += "\n[session]\n"
	body += fmt.Sprintf("socket = %q\n", socket)
	return os.WriteFile(cityTomlPath, []byte(body), 0o644)
}

func ensureTutorialObservePaths(cityTomlPath string, observePaths []string) error {
	if len(observePaths) == 0 {
		return nil
	}
	data, err := os.ReadFile(cityTomlPath)
	if err != nil {
		return err
	}
	body := string(data)
	if strings.Contains(body, "observe_paths = ") {
		return nil
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if !strings.Contains(body, "\n[daemon]\n") && !strings.HasPrefix(body, "[daemon]\n") {
		body += "\n[daemon]\n"
	}
	quoted := make([]string, 0, len(observePaths))
	for _, path := range observePaths {
		quoted = append(quoted, fmt.Sprintf("%q", path))
	}
	body += fmt.Sprintf("observe_paths = [%s]\n", strings.Join(quoted, ", "))
	return os.WriteFile(cityTomlPath, []byte(body), 0o644)
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

func TestRunEnvCommandWithTimeoutUsesAcceptanceGCBinary(t *testing.T) {
	home := t.TempDir()
	runtimeDir := filepath.Join(home, "runtime")
	mustMkdirAll(t, runtimeDir)

	fakeBinDir := filepath.Join(home, "bin")
	mustMkdirAll(t, fakeBinDir)
	fakeGC := filepath.Join(fakeBinDir, "gc")
	writeFile(t, fakeGC, "#!/bin/sh\nprintf 'tutorial-env-gc\\n'\n", 0o755)

	env := helpers.NewEnv(fakeGC, home, runtimeDir).With("PATH", "/does/not/exist")
	tutorial := &tutorialEnv{
		Home:       home,
		RuntimeDir: runtimeDir,
		Env:        env,
	}

	out, err := runEnvCommandWithTimeout(tutorial, home, 2*time.Second, "gc", "supervisor", "status")
	if err != nil {
		t.Fatalf("runEnvCommandWithTimeout: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(out); got != "tutorial-env-gc" {
		t.Fatalf("expected acceptance gc binary output, got %q", got)
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

func resolveEnvCommand(env *tutorialEnv, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("command name is empty")
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return name, nil
	}
	if env != nil && env.Env != nil {
		if name == "gc" {
			return helpers.ResolveGCPath(env.Env)
		}
		if path := findExecutableInPath(env.Env.Get("PATH"), name); path != "" {
			return path, nil
		}
	}
	return exec.LookPath(name)
}

func findExecutableInPath(pathEnv, name string) string {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&0o111 != 0 {
			return path
		}
	}
	return ""
}

func runEnvCommandWithTimeout(env *tutorialEnv, dir string, timeout time.Duration, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if len(argv) == 0 {
		return "", nil
	}
	commandPath, err := resolveEnvCommand(env, argv[0])
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, commandPath, argv[1:]...)
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
