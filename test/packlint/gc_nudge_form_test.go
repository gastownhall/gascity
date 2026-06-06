// TestGcNudgeFormPositional guards issue #1491: the bare `gc nudge <target>
// "msg"` form was retired when the `gc nudge` namespace was reduced to
// `drain`/`status`/`poll`. The deprecated form falls through to help-text on
// stderr and exits non-zero; every shipped call site wraps with
// `2>/dev/null || true`, so it silently no-ops. The canonical send-form is
// `gc session nudge <target> "msg"`. This test fails if a pack template,
// formula, asset script, or shipped doc reintroduces the deprecated form.

package packlint

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// nudgeScanDirs is the set of repo-root-relative directories whose embedded
// shell text and command examples must use the canonical `gc session nudge`
// form. Same set as `bd_show_jq_test.go` plus the user-facing docs tree,
// which must not teach migrating users the deprecated form. Design-history
// files under engdocs are intentionally out of scope.
var nudgeScanDirs = []string{
	"examples",
	"internal/bootstrap/packs",
	"docs",
}

// nudgeScanExts limits walking to files that ship embedded shell text or
// teach command syntax to agents and operators.
var nudgeScanExts = map[string]bool{
	".toml": true,
	".md":   true,
	".sh":   true,
}

// nudgeAllowlistFiles are repo-relative paths whose `gc nudge <target>`
// occurrences are intentionally retained as historical or struck-through
// documentation of the resolution itself.
var nudgeAllowlistFiles = map[string]bool{
	"examples/gastown/FUTURE.md": true,
}

// validNudgeSubcommands are the still-supported `gc nudge` subcommands.
// `gc nudge drain`, `gc nudge status`, and `gc nudge poll` remain valid;
// the bare positional form does not.
var validNudgeSubcommands = map[string]bool{
	"drain":  true,
	"status": true,
	"poll":   true,
}

func TestGcNudgeFormPositional(t *testing.T) {
	root := repoRoot()
	var violations []string
	for _, dir := range nudgeScanDirs {
		abs := filepath.Join(root, dir)
		err := filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if !nudgeScanExts[filepath.Ext(path)] {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			if nudgeAllowlistFiles[filepath.ToSlash(rel)] {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			for lineNum, line := range strings.Split(string(data), "\n") {
				if v := violatesNudgeForm(line); v != "" {
					violations = append(violations,
						filepath.ToSlash(rel)+":"+strconv.Itoa(lineNum+1)+": "+v)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if len(violations) > 0 {
		t.Errorf("deprecated `gc nudge <target> \"msg\"` (or `{{ cmd }} nudge ...`) form found"+
			" — silently no-ops because the bare `gc nudge` namespace was reduced to"+
			" `drain`/`status`/`poll` (issue #1491).\n"+
			"Fix: replace with `gc session nudge <target> \"msg\"` (or"+
			" `{{ cmd }} session nudge <target> \"msg\"`).\n\n%s",
			strings.Join(violations, "\n"))
	}
}

func TestViolatesNudgeForm(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		violation bool
	}{
		{name: "deprecated bare positional", line: `gc nudge deacon/ "DOG_DONE: ok"`, violation: true},
		{name: "deprecated templated cmd", line: `{{ cmd }} nudge <target> "message"`, violation: true},
		{name: "deprecated templated rig", line: `gc nudge {{ .RigName }}/refinery "msg"`, violation: true},
		{name: "deprecated indented", line: `    gc nudge dog/ "Compactor needed"`, violation: true},
		{name: "deprecated quoted positional target", line: `gc nudge "deacon/" "DOG_DONE: ok"`, violation: true},
		{name: "canonical session form", line: `gc session nudge deacon/ "DOG_DONE: ok"`, violation: false},
		{name: "canonical templated session form", line: `{{ cmd }} session nudge <target> "message"`, violation: false},
		{name: "still-valid drain subcommand", line: `gc nudge drain --inject`, violation: false},
		{name: "still-valid status subcommand", line: `gc nudge status`, violation: false},
		{name: "still-valid poll subcommand", line: `gc nudge poll --json`, violation: false},
		{name: "markdown link to status", line: `[gc nudge status](#gc-nudge-status) | Show queued`, violation: false},
		{name: "instructional backticked bare command", line: "Use `gc nudge` to alert the witness", violation: true},
		{name: "instructional via backticked bare command", line: "Health check via `gc nudge`", violation: true},
		{name: "instructional bare command dash", line: "Use `gc nudge` - ephemeral, zero Dolt overhead", violation: true},
		{name: "backticked status prose", line: "Use `gc nudge status` to inspect queued nudges", violation: false},
		{name: "backticked valid namespace prose", line: "The `gc nudge` subcommand only exposes deferred-delivery controls (`drain`, `status`, `poll`)", violation: false},
		{name: "prose mention without invocation", line: "The gc nudge namespace is for drain/status/poll only", violation: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := violatesNudgeForm(tc.line) != ""
			if got != tc.violation {
				t.Errorf("violatesNudgeForm(%q) = %v, want %v", tc.line, got, tc.violation)
			}
		})
	}
}

// violatesNudgeForm returns the offending substring if the line contains a
// `gc nudge <token>` or `{{ ... }} nudge <token>` invocation where <token>
// is a positional target rather than one of the still-valid subcommands.
// Returns empty when the line is clean.
//
// Heuristic for distinguishing real invocations from prose mentions: the
// command prefix must occur at the start of the trimmed line, possibly
// after a shell prompt. Mid-line occurrences are treated as prose
// references (e.g., `Use the gc nudge namespace ...`).
func violatesNudgeForm(line string) string {
	if v := violatesBacktickedBareNudge(line); v != "" {
		return v
	}
	for _, prefix := range nudgeCommandPrefixes(line) {
		before := strings.TrimSpace(line[:prefix.start])
		switch before {
		case "", "$", ">", "#":
		default:
			continue
		}
		rest := strings.TrimLeft(line[prefix.end:], " \t")
		if rest == "" {
			continue
		}
		if isWordChar(rest[0]) {
			if validNudgeSubcommands[firstToken(rest)] {
				continue
			}
		}
		return strings.TrimSpace(line[prefix.start:])
	}
	return ""
}

// violatesBacktickedBareNudge catches instructional prose that names the
// retired send interface without an explicit target, such as "Use `gc nudge`".
func violatesBacktickedBareNudge(line string) string {
	const bare = "`gc nudge`"
	if !strings.Contains(line, bare) {
		return ""
	}
	lower := strings.ToLower(line)
	if strings.Contains(lower, "drain") &&
		strings.Contains(lower, "status") &&
		strings.Contains(lower, "poll") {
		return ""
	}
	return bare
}

type nudgePrefix struct {
	start, end int
}

// nudgeCommandPrefixes finds every occurrence of `gc nudge ` or
// `{{ <expr> }} nudge ` on the line and returns the [start,end) byte ranges
// of each prefix (start at the first letter of the command, end after the
// trailing space).
func nudgeCommandPrefixes(line string) []nudgePrefix {
	var out []nudgePrefix
	const literal = "gc nudge "
	for i := 0; i+len(literal) <= len(line); i++ {
		if line[i:i+len(literal)] != literal {
			continue
		}
		if i > 0 && isWordChar(line[i-1]) {
			continue
		}
		out = append(out, nudgePrefix{start: i, end: i + len(literal)})
	}
	const tmplOpen = "{{"
	const tmplNudge = " nudge "
	for i := 0; i+len(tmplOpen) <= len(line); i++ {
		if line[i:i+len(tmplOpen)] != tmplOpen {
			continue
		}
		closeIdx := strings.Index(line[i:], "}}")
		if closeIdx < 0 {
			continue
		}
		afterClose := i + closeIdx + len("}}")
		if afterClose+len(tmplNudge) > len(line) {
			continue
		}
		if line[afterClose:afterClose+len(tmplNudge)] != tmplNudge {
			continue
		}
		out = append(out, nudgePrefix{start: i, end: afterClose + len(tmplNudge)})
	}
	return out
}

// firstToken returns the leading run of word characters. Stopping at any
// non-word byte handles markdown link tails (`status](#...)`), trailing
// punctuation (`drain.`), and quoted forms uniformly.
func firstToken(s string) string {
	for i := 0; i < len(s); i++ {
		if !isWordChar(s[i]) {
			return s[:i]
		}
	}
	return s
}

func isWordChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}
