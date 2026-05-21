package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignDarwinLocalUsesExplicitStableIdentity(t *testing.T) {
	env := newSignTestEnv(t, "Darwin")

	result := env.run(t, "GC_SIGN_IDENTITY=Apple Development: Example (TEAMID)")
	if result.err != nil {
		t.Fatalf("sign-darwin-local.sh failed: %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}

	log := env.readLog(t)
	if !strings.Contains(log, "codesign\t--force\t--sign\tApple Development: Example (TEAMID)\t--identifier\tcom.gascity.gc\t"+env.binary) {
		t.Fatalf("expected stable codesign invocation, got log:\n%s", log)
	}
	if !strings.Contains(log, "xattr\t-d\tcom.apple.provenance\t"+env.binary) {
		t.Fatalf("expected provenance xattr cleanup, got log:\n%s", log)
	}
}

func TestSignDarwinLocalAutoDetectsStableIdentity(t *testing.T) {
	env := newSignTestEnv(t, "Darwin")
	env.securityOutput = "  1) 1234567890ABCDEF \"Developer ID Application: Gas City (TEAMID)\"\n"

	result := env.run(t)
	if result.err != nil {
		t.Fatalf("sign-darwin-local.sh failed: %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}

	log := env.readLog(t)
	if !strings.Contains(log, "codesign\t--force\t--sign\tDeveloper ID Application: Gas City (TEAMID)\t--identifier\tcom.gascity.gc\t"+env.binary) {
		t.Fatalf("expected auto-detected stable codesign invocation, got log:\n%s", log)
	}
}

func TestSignDarwinLocalAutoDetectsStableIdentityByDocumentedPriority(t *testing.T) {
	env := newSignTestEnv(t, "Darwin")
	env.securityOutput = strings.Join([]string{
		"  1) 1234567890ABCDEF \"GasCity Dev\"",
		"  2) 1234567890ABCDEF \"Developer ID Application: Gas City (TEAMID)\"",
		"  3) 1234567890ABCDEF \"Apple Development: Example (TEAMID)\"",
		"",
	}, "\n")

	result := env.run(t)
	if result.err != nil {
		t.Fatalf("sign-darwin-local.sh failed: %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}

	log := env.readLog(t)
	if !strings.Contains(log, "codesign\t--force\t--sign\tApple Development: Example (TEAMID)\t--identifier\tcom.gascity.gc\t"+env.binary) {
		t.Fatalf("expected Apple Development to win documented priority, got log:\n%s", log)
	}
}

func TestSignDarwinLocalLeavesGoSignatureWhenNoIdentity(t *testing.T) {
	env := newSignTestEnv(t, "Darwin")

	result := env.run(t)
	if result.err != nil {
		t.Fatalf("sign-darwin-local.sh failed: %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}

	if log := env.readLog(t); strings.Contains(log, "codesign") {
		t.Fatalf("expected no codesign invocation, got log:\n%s", log)
	}
	if !strings.Contains(result.stdout, "leaving Go linker signature unchanged") {
		t.Fatalf("expected no-identity guidance, got stdout:\n%s", result.stdout)
	}
}

func TestSignDarwinLocalDoesNotFallbackToAdhocWhenAutoSignFails(t *testing.T) {
	env := newSignTestEnv(t, "Darwin")
	env.securityOutput = "  1) 1234567890ABCDEF \"Apple Development: Example (TEAMID)\"\n"

	result := env.run(t, "CODESIGN_EXIT=1")
	if result.err != nil {
		t.Fatalf("sign-darwin-local.sh failed: %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}

	log := env.readLog(t)
	if strings.Count(log, "codesign") != 1 {
		t.Fatalf("expected exactly one stable codesign attempt, got log:\n%s", log)
	}
	if strings.Contains(log, "codesign\t--force\t--sign\t-\t") {
		t.Fatalf("expected no ad-hoc fallback, got log:\n%s", log)
	}
	if !strings.Contains(result.stderr, "leaving Go linker signature unchanged") {
		t.Fatalf("expected fallback guidance, got stderr:\n%s", result.stderr)
	}
}

func TestSignDarwinLocalExplicitIdentityFailureExitsNonZero(t *testing.T) {
	env := newSignTestEnv(t, "Darwin")

	result := env.run(t, "GC_SIGN_IDENTITY=Apple Development: Missing (TEAMID)", "CODESIGN_EXIT=1")
	if result.err == nil {
		t.Fatalf("expected explicit signing failure to exit non-zero\nstdout:\n%s\nstderr:\n%s", result.stdout, result.stderr)
	}

	log := env.readLog(t)
	if strings.Count(log, "codesign") != 1 {
		t.Fatalf("expected exactly one stable codesign attempt, got log:\n%s", log)
	}
	if !strings.Contains(result.stderr, "failed to sign") {
		t.Fatalf("expected explicit signing failure message, got stderr:\n%s", result.stderr)
	}
}

func TestSignDarwinLocalAdhocSignsOnlyWhenOptedIn(t *testing.T) {
	env := newSignTestEnv(t, "Darwin")

	result := env.run(t, "GC_ADHOC_SIGN=1")
	if result.err != nil {
		t.Fatalf("sign-darwin-local.sh failed: %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}

	log := env.readLog(t)
	if !strings.Contains(log, "codesign\t--force\t--sign\t-\t"+env.binary) {
		t.Fatalf("expected opt-in ad-hoc codesign invocation, got log:\n%s", log)
	}
}

func TestSignDarwinLocalSkipsNonDarwin(t *testing.T) {
	env := newSignTestEnv(t, "Linux")

	result := env.run(t)
	if result.err != nil {
		t.Fatalf("sign-darwin-local.sh failed: %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}
	if log := env.readLog(t); log != "" {
		t.Fatalf("expected no signing commands on non-Darwin, got log:\n%s", log)
	}
}

type signTestEnv struct {
	t              *testing.T
	binDir         string
	binary         string
	logFile        string
	repoRoot       string
	securityOutput string
}

type signResult struct {
	stdout string
	stderr string
	err    error
}

func newSignTestEnv(t *testing.T, unameOutput string) *signTestEnv {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Dir(wd)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	env := &signTestEnv{
		t:        t,
		binDir:   binDir,
		binary:   filepath.Join(tmp, "gc"),
		logFile:  filepath.Join(tmp, "commands.log"),
		repoRoot: repoRoot,
	}
	if err := os.WriteFile(env.binary, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	env.writeStub(t, "uname", "printf '%s\\n' "+shellQuote(unameOutput)+"\n")
	env.writeStub(t, "security", "printf '%s' \"${SECURITY_OUTPUT:-}\"\n")
	env.writeStub(t, "codesign", "printf 'codesign' >> \"$SIGN_LOG\"\nfor arg in \"$@\"; do printf '\\t%s' \"$arg\" >> \"$SIGN_LOG\"; done\nprintf '\\n' >> \"$SIGN_LOG\"\nexit \"${CODESIGN_EXIT:-0}\"\n")
	env.writeStub(t, "xattr", "printf 'xattr' >> \"$SIGN_LOG\"\nfor arg in \"$@\"; do printf '\\t%s' \"$arg\" >> \"$SIGN_LOG\"; done\nprintf '\\n' >> \"$SIGN_LOG\"\nexit 0\n")

	return env
}

func (e *signTestEnv) run(t *testing.T, extraEnv ...string) signResult {
	t.Helper()

	script := filepath.Join(e.repoRoot, "scripts", "sign-darwin-local.sh")
	cmd := exec.Command(script, e.binary)
	cmd.Dir = e.repoRoot
	cmd.Env = append([]string{
		"PATH=" + e.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SIGN_LOG=" + e.logFile,
		"SECURITY_OUTPUT=" + e.securityOutput,
	}, extraEnv...)

	stdout, stderr := strings.Builder{}, strings.Builder{}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return signResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func (e *signTestEnv) readLog(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(e.logFile)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (e *signTestEnv) writeStub(t *testing.T, name, body string) {
	t.Helper()

	path := filepath.Join(e.binDir, name)
	content := "#!/usr/bin/env sh\n" + body
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
