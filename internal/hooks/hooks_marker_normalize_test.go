package hooks

import (
	"path/filepath"
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
	if !hookContains(newHookSource(reformatted), marker) {
		t.Fatalf("reformatted call did not match marker:\n%s", reformatted)
	}
}

// Reflowed comments must still satisfy the managed-file identity guard.
func TestHookMarkerMatchingSurvivesReflowedComments(t *testing.T) {
	if !hookContains(newHookSource("// Gas City hooks\n// for OpenCode.\n"), "Gas City hooks for OpenCode.") {
		t.Fatal("reflowed identity comment did not match")
	}
}

// Whitespace inside string literals is meaningful and must be preserved, so
// two distinct literals never collapse into each other.
func TestHookMarkerMatchingKeepsStringLiteralSpacing(t *testing.T) {
	if hookContains(newHookSource(`run("contextcycle")`), `run("context cycle")`) {
		t.Fatal("literal spacing was collapsed; distinct string literals matched")
	}
	if !hookContains(newHookSource(`run( "context cycle" )`), `run("context cycle")`) {
		t.Fatal("spacing outside the literal should be ignored")
	}
}

func TestHookMarkerMatchingIsNotFooledByMissingContent(t *testing.T) {
	if hookContains(newHookSource(`output.context.push(other)`), "output.context.push(handoff)") {
		t.Fatal("unrelated call matched the marker")
	}
}

// End-to-end: a real bundled plugin with one upgrade marker re-wrapped (the
// line breaks, indentation and trailing comma a formatter changes when it
// breaks a line) must still be recognized as current. Otherwise the next
// install backs it up and rewrites it, and every reconcile tick's overlay
// staging reverts it. Markers that are a single identifier, such as
// appendVolatileSystem, cannot be split and need no case.
func TestBundledPluginsSurviveFormatterRewrap(t *testing.T) {
	plugins := map[string]string{
		"pi":       filepath.Join(".pi", "extensions", "gc-hooks.js"),
		"opencode": filepath.Join(".opencode", "plugins", "gascity.js"),
		"mimocode": filepath.Join(".mimocode", "plugin", "gascity.js"),
	}
	stdinEnd := "pending.child.stdin?.end();"
	stdinEndRewrapped := "pending.child.stdin\n      ?.end();"
	turnMap := "const turns = new Map()"
	turnMapRewrapped := "const turns =\n    new Map()"
	unseenGate := "state.userMessageIDs.has(messageID)"
	unseenGateRewrapped := "state.userMessageIDs.has(\n        messageID,\n      )"

	for _, tc := range []struct {
		name      string
		provider  string
		original  string
		rewrapped string
	}{
		{"pi GC_BIN fallback", "pi", `process.env.GC_BIN || "gc"`, "process.env.GC_BIN ||\n  \"gc\""},
		{"opencode handoff call", "opencode", `runWithWarning(directory, "handoff", "--auto", "context cycle")`, `runWithWarning(
      directory,
      "handoff",
      "--auto",
      "context cycle",
    )`},
		{"opencode stdin end", "opencode", stdinEnd, stdinEndRewrapped},
		{"opencode turn map", "opencode", turnMap, turnMapRewrapped},
		{"opencode unseen message gate", "opencode", unseenGate, unseenGateRewrapped},
		{"mimocode stdin end", "mimocode", stdinEnd, stdinEndRewrapped},
		{"mimocode turn map", "mimocode", turnMap, turnMapRewrapped},
		{"mimocode unseen message gate", "mimocode", unseenGate, unseenGateRewrapped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := plugins[tc.provider]
			dst := filepath.Join("/work", rel)
			fs := fsys.NewFake()
			if err := Install(fs, "/city", "/work", []string{tc.provider}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			installed := string(fs.Files[dst])
			if n := strings.Count(installed, tc.original); n != 1 {
				t.Fatalf("bundled plugin contains the rewrapped marker %d times, want 1: %q", n, tc.original)
			}
			if !PreserveManagedFile(rel, []byte(installed)) {
				t.Fatal("freshly installed plugin reported stale")
			}
			formatted := strings.Replace(installed, tc.original, tc.rewrapped, 1)
			if PreserveManagedFile(rel, []byte(strings.Replace(formatted, tc.rewrapped, "", 1))) {
				t.Fatal("plugin stays current without the rewrapped marker, so this case would not test it")
			}
			fs.Files[dst] = []byte(formatted)

			if err := Install(fs, "/city", "/work", []string{tc.provider}); err != nil {
				t.Fatalf("reinstall: %v", err)
			}
			if string(fs.Files[dst]) != formatted {
				t.Error("reinstall rewrote the reformatted plugin; formatting must not change lifecycle")
			}
			if _, ok := fs.Files[dst+".bak"]; ok {
				t.Error("reinstall backed up the reformatted plugin as stale")
			}
			if !PreserveManagedFile(rel, []byte(formatted)) {
				t.Error("reconcile staging would revert the reformatted plugin")
			}
		})
	}
}
