package convergence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConditionEnvEnviron(t *testing.T) {
	env := ConditionEnv{
		BeadID:               "bead-123",
		Iteration:            3,
		CityPath:             "/home/test/city",
		WispID:               "wisp-456",
		DocPath:              "/docs/review.md",
		ArtifactDir:          "/tmp/artifacts",
		IterationDurationMs:  1500,
		CumulativeDurationMs: 4500,
		MaxIterations:        10,
		AgentVerdict:         "approve",
		AgentProvider:        "anthropic",
		AgentModel:           "claude-3",
	}

	vars := env.Environ()
	lookup := make(map[string]string)
	for _, v := range vars {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) == 2 {
			lookup[parts[0]] = parts[1]
		}
	}

	// Required vars.
	checks := map[string]string{
		"PATH":                      conditionPATH(),
		"BEADS_DIR":                 "/home/test/city/.beads",
		"GC_BEAD_ID":                "bead-123",
		"GC_ITERATION":              "3",
		"GC_CITY_ROOT":              "/home/test/city",
		"GC_CITY_PATH":              "/home/test/city",
		"GC_CITY_RUNTIME_DIR":       "/home/test/city/.gc/runtime",
		"GC_WISP_ID":                "wisp-456",
		"GC_DOC_PATH":               "/docs/review.md",
		"GC_ARTIFACT_DIR":           "/tmp/artifacts",
		"GC_ITERATION_DURATION_MS":  "1500",
		"GC_CUMULATIVE_DURATION_MS": "4500",
		"GC_MAX_ITERATIONS":         "10",
		"GC_AGENT_VERDICT":          "approve",
		"GC_AGENT_PROVIDER":         "anthropic",
		"GC_AGENT_MODEL":            "claude-3",
	}

	for key, want := range checks {
		got, ok := lookup[key]
		if !ok {
			t.Errorf("missing env var %s", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// HOME and TMPDIR should be present.
	if _, ok := lookup["HOME"]; !ok {
		t.Error("missing HOME env var")
	}
	if _, ok := lookup["TMPDIR"]; !ok {
		t.Error("missing TMPDIR env var")
	}
}

func TestConditionEnvEnvironOptionalEmpty(t *testing.T) {
	env := ConditionEnv{
		BeadID:      "bead-789",
		Iteration:   1,
		CityPath:    "/city",
		WispID:      "wisp-abc",
		ArtifactDir: "/tmp/art",
		// DocPath, AgentVerdict, AgentProvider, AgentModel all empty.
	}

	vars := env.Environ()
	lookup := make(map[string]string)
	for _, v := range vars {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) == 2 {
			lookup[parts[0]] = parts[1]
		}
	}

	// Optional vars should be absent when empty.
	for _, key := range []string{"GC_DOC_PATH", "GC_AGENT_VERDICT", "GC_AGENT_PROVIDER", "GC_AGENT_MODEL"} {
		if _, ok := lookup[key]; ok {
			t.Errorf("expected %s to be absent for empty value, but it was present", key)
		}
	}

	// Required vars should still be present.
	if _, ok := lookup["GC_BEAD_ID"]; !ok {
		t.Error("missing GC_BEAD_ID")
	}
	if _, ok := lookup["PATH"]; !ok {
		t.Error("missing PATH")
	}
}

func TestResolveConditionPath(t *testing.T) {
	t.Run("absolute path", func(t *testing.T) {
		dir := t.TempDir()
		script := filepath.Join(dir, "check.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := ResolveConditionPath("/some/city", script)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != script {
			t.Errorf("got %q, want %q", got, script)
		}
	})

	t.Run("relative path", func(t *testing.T) {
		dir := t.TempDir()
		script := filepath.Join(dir, "gates", "check.sh")
		if err := os.MkdirAll(filepath.Join(dir, "gates"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := ResolveConditionPath(dir, "gates/check.sh")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != script {
			t.Errorf("got %q, want %q", got, script)
		}
	})

	t.Run("symlink allowed", func(t *testing.T) {
		dir := t.TempDir()
		realScript := filepath.Join(dir, "real.sh")
		link := filepath.Join(dir, "link.sh")

		if err := os.WriteFile(realScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realScript, link); err != nil {
			t.Fatal(err)
		}

		got, err := ResolveConditionPath(dir, "link.sh")
		if err != nil {
			t.Fatalf("unexpected error for symlink: %v", err)
		}
		if got != link {
			t.Errorf("got %q, want %q", got, link)
		}
	})

	t.Run("path traversal rejection", func(t *testing.T) {
		dir := t.TempDir()
		// Create a script outside the city directory.
		parent := filepath.Dir(dir)
		script := filepath.Join(parent, "outside.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(script) }()

		_, err := ResolveConditionPath(dir, "../outside.sh")
		if err == nil {
			t.Fatal("expected error for path traversal, got nil")
		}
		if !strings.Contains(err.Error(), "traversal") {
			t.Errorf("expected path traversal error, got: %v", err)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		_, err := ResolveConditionPath("/some/city", "")
		if err == nil {
			t.Fatal("expected error for empty path, got nil")
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		_, err := ResolveConditionPath("/some/city", "/nonexistent/file.sh")
		if err == nil {
			t.Fatal("expected error for nonexistent file, got nil")
		}
	})
}

func TestRunConditionPass(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "pass.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := ConditionEnv{
		BeadID:      "b1",
		CityPath:    dir,
		WispID:      "w1",
		ArtifactDir: dir,
	}

	result := RunCondition(context.Background(), script, env, 5*time.Second, 0)
	if result.Outcome != GatePass {
		t.Errorf("Outcome = %q, want %q", result.Outcome, GatePass)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "ok") {
		t.Errorf("Stdout = %q, want to contain 'ok'", result.Stdout)
	}
	if result.Duration <= 0 {
		t.Error("Duration should be positive")
	}
}

func TestRunConditionFail(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho failing >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := ConditionEnv{
		BeadID:      "b2",
		CityPath:    dir,
		WispID:      "w2",
		ArtifactDir: dir,
	}

	result := RunCondition(context.Background(), script, env, 5*time.Second, 0)
	if result.Outcome != GateFail {
		t.Errorf("Outcome = %q, want %q", result.Outcome, GateFail)
	}
	if result.ExitCode == nil || *result.ExitCode != 1 {
		t.Errorf("ExitCode = %v, want 1", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "failing") {
		t.Errorf("Stderr = %q, want to contain 'failing'", result.Stderr)
	}
}

func TestRunConditionUsesWorkDir(t *testing.T) {
	cityDir := t.TempDir()
	workDir := filepath.Join(cityDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "target.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(cityDir, "check-workdir.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd\nprintf '%s\\n' \"$BEADS_DIR\"\ncat target.txt\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := ConditionEnv{
		BeadID:      "b-workdir",
		CityPath:    cityDir,
		WorkDir:     workDir,
		WispID:      "w-workdir",
		ArtifactDir: cityDir,
	}

	result := RunCondition(context.Background(), script, env, 5*time.Second, 0)
	if result.Outcome != GatePass {
		t.Fatalf("Outcome = %q, want %q (stderr=%q)", result.Outcome, GatePass, result.Stderr)
	}
	if !strings.Contains(result.Stdout, workDir) {
		t.Errorf("Stdout = %q, want to contain workdir %q", result.Stdout, workDir)
	}
	wantBeadsDir := filepath.Join(cityDir, ".beads")
	if !strings.Contains(result.Stdout, wantBeadsDir) {
		t.Errorf("Stdout = %q, want to contain BEADS_DIR %q", result.Stdout, wantBeadsDir)
	}
	if !strings.Contains(result.Stdout, "ok") {
		t.Errorf("Stdout = %q, want to contain file contents", result.Stdout)
	}
}

func TestConditionPATHUsesResolvedToolDirs(t *testing.T) {
	origPath := os.Getenv("PATH")
	t.Cleanup(func() {
		_ = os.Setenv("PATH", origPath)
	})

	toolDir := t.TempDir()
	for _, name := range []string{"bd", "gc"} {
		path := filepath.Join(toolDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Setenv("PATH", toolDir+":"+SafePATH); err != nil {
		t.Fatal(err)
	}

	got := conditionPATH()
	if !strings.HasPrefix(got, toolDir+":") && got != toolDir {
		t.Fatalf("conditionPATH() = %q, want prefix %q", got, toolDir)
	}
}

func TestRunConditionTimeout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "slow.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := ConditionEnv{
		BeadID:      "b3",
		CityPath:    dir,
		WispID:      "w3",
		ArtifactDir: dir,
	}

	result := RunCondition(context.Background(), script, env, 100*time.Millisecond, 0)
	if result.Outcome != GateTimeout {
		t.Errorf("Outcome = %q, want %q", result.Outcome, GateTimeout)
	}
	if result.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil for timeout", result.ExitCode)
	}
}

func TestRunConditionTimeoutRetry(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "slow.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := ConditionEnv{
		BeadID:      "b3r",
		CityPath:    dir,
		WispID:      "w3r",
		ArtifactDir: dir,
	}

	result := RunCondition(context.Background(), script, env, 100*time.Millisecond, 2)
	if result.Outcome != GateTimeout {
		t.Errorf("Outcome = %q, want %q", result.Outcome, GateTimeout)
	}
	if result.RetryCount != 2 {
		t.Errorf("RetryCount = %d, want 2", result.RetryCount)
	}
}

func TestRunConditionNotFound(t *testing.T) {
	env := ConditionEnv{
		BeadID:      "b4",
		CityPath:    t.TempDir(),
		WispID:      "w4",
		ArtifactDir: t.TempDir(),
	}

	result := RunCondition(context.Background(), "/nonexistent/script.sh", env, 5*time.Second, 0)
	if result.Outcome != GateError {
		t.Errorf("Outcome = %q, want %q", result.Outcome, GateError)
	}
}

func TestRunConditionOutputCapture(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "output.sh")
	content := "#!/bin/sh\necho stdout-data\necho stderr-data >&2\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	env := ConditionEnv{
		BeadID:      "b5",
		CityPath:    dir,
		WispID:      "w5",
		ArtifactDir: dir,
	}

	result := RunCondition(context.Background(), script, env, 5*time.Second, 0)
	if !strings.Contains(result.Stdout, "stdout-data") {
		t.Errorf("Stdout = %q, want to contain 'stdout-data'", result.Stdout)
	}
	if !strings.Contains(result.Stderr, "stderr-data") {
		t.Errorf("Stderr = %q, want to contain 'stderr-data'", result.Stderr)
	}
	if result.Truncated {
		t.Error("Truncated should be false for small output")
	}
}

func TestRunConditionOutputTruncation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "big.sh")

	// Generate output larger than MaxOutputBytes using printf.
	content := "#!/bin/sh\nprintf '%0*d' 5096 0\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	env := ConditionEnv{
		BeadID:      "b6",
		CityPath:    dir,
		WispID:      "w6",
		ArtifactDir: dir,
	}

	result := RunCondition(context.Background(), script, env, 5*time.Second, 0)
	if result.Outcome != GatePass {
		t.Errorf("Outcome = %q, want %q", result.Outcome, GatePass)
	}
	if len(result.Stdout) > MaxOutputBytes {
		t.Errorf("Stdout length = %d, should be <= %d", len(result.Stdout), MaxOutputBytes)
	}
	if !result.Truncated {
		t.Error("Truncated should be true for large output")
	}
}

func TestRunConditionParentContextCancelled(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "slow.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := ConditionEnv{
		BeadID:      "b-parent",
		CityPath:    dir,
		WispID:      "w-parent",
		ArtifactDir: dir,
	}

	// Cancel the parent context immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := RunCondition(ctx, script, env, 5*time.Second, 2)
	// Should get GateError (parent canceled), NOT GateTimeout.
	if result.Outcome != GateError {
		t.Errorf("Outcome = %q, want %q (parent context canceled)", result.Outcome, GateError)
	}
	// Should NOT have retried (parent was already canceled).
	if result.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0 (should not retry on parent cancel)", result.RetryCount)
	}
}

func TestRunConditionEnvVarsAvailable(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "envcheck.sh")
	// Script prints specific env vars to stdout.
	content := "#!/bin/sh\necho \"BEAD=$GC_BEAD_ID\"\necho \"ITER=$GC_ITERATION\"\necho \"PATH=$PATH\"\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	env := ConditionEnv{
		BeadID:      "bead-env-test",
		Iteration:   7,
		CityPath:    dir,
		WispID:      "w-env",
		ArtifactDir: dir,
	}

	result := RunCondition(context.Background(), script, env, 5*time.Second, 0)
	if result.Outcome != GatePass {
		t.Fatalf("Outcome = %q, want pass; stderr: %s", result.Outcome, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "BEAD=bead-env-test") {
		t.Errorf("expected GC_BEAD_ID in output, got: %s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "ITER=7") {
		t.Errorf("expected GC_ITERATION in output, got: %s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "PATH="+conditionPATH()) {
		t.Errorf("expected resolved PATH in output, got: %s", result.Stdout)
	}
}
