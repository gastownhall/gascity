package convergence

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestResolveEvaluateStep_DefaultPath(t *testing.T) {
	f := Formula{Name: "test"}
	step, err := ResolveEvaluateStep("/home/user/city", f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if step.Name != EvaluateStepName {
		t.Errorf("Name = %q, want %q", step.Name, EvaluateStepName)
	}
	// production paths are NormalizePathForCompare form; compare firmlink-aware (#4934)
	want := filepath.Join("/home/user/city", DefaultEvaluatePromptPath)
	testutil.AssertSamePath(t, step.PromptPath, want)
}

func TestResolveEvaluateStep_CustomPath(t *testing.T) {
	f := Formula{
		Name:           "test",
		EvaluatePrompt: "custom/my-evaluate.md",
	}
	step, err := ResolveEvaluateStep("/home/user/city", f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if step.Name != EvaluateStepName {
		t.Errorf("Name = %q, want %q", step.Name, EvaluateStepName)
	}
	want := filepath.Join("/home/user/city", "custom/my-evaluate.md")
	testutil.AssertSamePath(t, step.PromptPath, want)
}

func TestResolveEvaluateStep_PathTraversal(t *testing.T) {
	f := Formula{
		Name:           "test",
		EvaluatePrompt: "../../etc/passwd",
	}
	_, err := ResolveEvaluateStep("/home/user/city", f)
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("error should mention path escaping, got: %v", err)
	}
}

func TestValidateEvaluatePrompt_Valid(t *testing.T) {
	content := []byte("Run bd meta set to record convergence.agent_verdict with the result.")
	if err := ValidateEvaluatePrompt(content); err != nil {
		t.Errorf("expected no error for valid content, got: %v", err)
	}
}

func TestValidateEvaluatePrompt_MissingBdMetaSet(t *testing.T) {
	content := []byte("Record convergence.agent_verdict for this evaluation.")
	err := ValidateEvaluatePrompt(content)
	if err == nil {
		t.Fatal("expected error for missing 'bd meta set'")
	}
	if !strings.Contains(err.Error(), "bd meta set") {
		t.Errorf("error should mention missing 'bd meta set', got: %v", err)
	}
}

func TestValidateEvaluatePrompt_MissingAgentVerdict(t *testing.T) {
	content := []byte("Use bd meta set to store the evaluation outcome.")
	err := ValidateEvaluatePrompt(content)
	if err == nil {
		t.Fatal("expected error for missing 'convergence.agent_verdict'")
	}
	if !strings.Contains(err.Error(), "convergence.agent_verdict") {
		t.Errorf("error should mention missing 'convergence.agent_verdict', got: %v", err)
	}
}

func TestValidateEvaluatePrompt_MissingBoth(t *testing.T) {
	content := []byte("This prompt has neither required substring.")
	err := ValidateEvaluatePrompt(content)
	if err == nil {
		t.Fatal("expected error for missing both substrings")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "bd meta set") {
		t.Errorf("error should mention missing 'bd meta set', got: %v", err)
	}
	if !strings.Contains(errStr, "convergence.agent_verdict") {
		t.Errorf("error should mention missing 'convergence.agent_verdict', got: %v", err)
	}
}

func TestValidateEvaluatePrompt_EmptyContent(t *testing.T) {
	err := ValidateEvaluatePrompt([]byte{})
	if err == nil {
		t.Fatal("expected error for empty content")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "bd meta set") {
		t.Errorf("error should mention missing 'bd meta set', got: %v", err)
	}
	if !strings.Contains(errStr, "convergence.agent_verdict") {
		t.Errorf("error should mention missing 'convergence.agent_verdict', got: %v", err)
	}
}

// Pins the canonical-path-at-ingest bug this migration fixes (ga-iawy13.4):
// a relative cityPath (e.g. "." from an as-yet-unresolved city path) makes
// the current bare EvalSymlinks-without-Abs canonicalization leave
// canonCity relative, so the function silently succeeds but returns a
// relative PromptPath instead of an absolute one. Once canonCity is
// normalized via pathutil.NormalizePathForCompare (which absolutizes
// first), PromptPath must be absolute.
func TestResolveEvaluateStep_RelativeCityPathReturnsAbsolutePromptPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	f := Formula{Name: "test"}
	step, err := ResolveEvaluateStep(".", f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !filepath.IsAbs(step.PromptPath) {
		t.Fatalf("PromptPath = %q, want an absolute path — cityPath must be canonicalized to absolute before joining, not left relative", step.PromptPath)
	}
	want := filepath.Join(dir, DefaultEvaluatePromptPath)
	testutil.AssertSamePath(t, step.PromptPath, want)
}

// Pins the symlink-presence rejection itself, which the comparison above sits
// on top of. Normalizing realResolved must not weaken it: normalizing BOTH
// operands (e.g. via pathutil.SamePath) would re-resolve the prompt path
// through its own symlink, both sides would compare equal, and this rejection
// would silently stop firing. Portable — this runs on every platform, unlike
// the darwin-guarded alias tests.
func TestResolveEvaluateStep_SymlinkedPromptStillRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	city := t.TempDir()
	outside := t.TempDir()

	target := filepath.Join(outside, "real-evaluate.md")
	if err := os.WriteFile(target, []byte("bd meta set convergence.agent_verdict\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(city, DefaultEvaluatePromptPath)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	_, err := ResolveEvaluateStep(city, Formula{Name: "test"})
	if err == nil {
		t.Fatal("expected a symlinked evaluate prompt to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "contains symlinks") {
		t.Errorf("expected a symlink rejection, got: %v", err)
	}
}

// Pins the darwin half of the same comparison contract. canonCity comes from
// pathutil.NormalizePathForCompare, which on macOS collapses the platform
// temp root's /private/var (or /private/tmp) spelling back to /var (or /tmp);
// the symlink-presence check's realResolved comes from bare EvalSymlinks and
// carries the /private spelling. Comparing the two raw conventions rejects a
// prompt file that is not a symlink at all, so realResolved must be
// normalized before the comparison.
//
// This needs the real os.TempDir() root (the /private alias only exists on the
// platform temp trees) AND the prompt file actually present on disk — the
// check is guarded by `err == nil`, so a missing file makes EvalSymlinks fail
// and the comparison is skipped entirely.
func TestResolveEvaluateStep_DarwinPrivateTempAliasWithExistingPrompt(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms")
	}
	city, err := os.MkdirTemp(os.TempDir(), "gc-eval-alias-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(city) })

	prompt := filepath.Join(city, DefaultEvaluatePromptPath)
	if err := os.MkdirAll(filepath.Dir(prompt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prompt, []byte("bd meta set convergence.agent_verdict\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	step, err := ResolveEvaluateStep(city, Formula{Name: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v — the symlink-presence check must normalize realResolved before comparing it against a path built on the alias-collapsed canonCity", err)
	}
	testutil.AssertSamePath(t, step.PromptPath, prompt)
}
