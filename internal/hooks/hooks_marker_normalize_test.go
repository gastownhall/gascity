package hooks

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// A formatter re-wrapping a call across lines changes no behavior, so it must
// not change whether a managed hook file is considered current (#5555).
func TestHookMarkerMatchingSurvivesReformatting(t *testing.T) {
	marker := `runWithWarning(directory, "handoff", "--auto", "context cycle")`
	reformatted := `  const handoff = await runWithWarning(
    directory,
    "handoff",
    "--auto",
    "context cycle",
  );`

	if strings.Contains(reformatted, marker) {
		t.Fatal("fixture is not actually reformatted; literal match still succeeds")
	}
	if !hookContains(reformatted, marker) {
		t.Fatalf("reformatted call did not match marker:\n%s", reformatted)
	}
}

// Reflowed comments must still satisfy the managed-file identity guard.
func TestHookMarkerMatchingSurvivesReflowedComments(t *testing.T) {
	if !hookContains("// Gas City hooks\n// for OpenCode.\n", "Gas City hooks for OpenCode.") {
		t.Fatal("reflowed identity comment did not match")
	}
}

// Whitespace inside string literals is meaningful and must be preserved, so
// two distinct literals never collapse into each other.
func TestHookMarkerMatchingKeepsStringLiteralSpacing(t *testing.T) {
	if hookContains(`run("contextcycle")`, `run("context cycle")`) {
		t.Fatal("literal spacing was collapsed; distinct string literals matched")
	}
	if !hookContains(`run( "context cycle" )`, `run("context cycle")`) {
		t.Fatal("spacing outside the literal should be ignored")
	}
}

func TestHookMarkerMatchingIsNotFooledByMissingContent(t *testing.T) {
	if hookContains(`output.context.push(other)`, "output.context.push(handoff)") {
		t.Fatal("unrelated call matched the marker")
	}
}

// End-to-end: the real bundled OpenCode plugin, put through the kind of
// re-wrapping a formatter performs, must still be recognized as current.
func TestBundledOpenCodePluginSurvivesFormatterRewrap(t *testing.T) {
	fs := fsys.NewFake()
	if err := Install(fs, "/city", "/work", []string{"opencode"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	installed := string(fs.Files["/work/.opencode/plugins/gascity.js"])

	original := `runWithWarning(directory, "handoff", "--auto", "context cycle")`
	rewrapped := `runWithWarning(
      directory,
      "handoff",
      "--auto",
      "context cycle",
    )`
	if !strings.Contains(installed, original) {
		t.Fatalf("bundled plugin no longer contains the call this test rewraps:\n%s", installed)
	}
	formatted := strings.Replace(installed, original, rewrapped, 1)

	if opencodeHookNeedsUpgrade([]byte(installed)) {
		t.Fatal("freshly installed OpenCode plugin reported stale")
	}
	if opencodeHookNeedsUpgrade([]byte(formatted)) {
		t.Fatal("reformatted OpenCode plugin reported stale; formatting must not change lifecycle")
	}
}
