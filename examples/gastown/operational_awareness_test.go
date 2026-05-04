// Package gastown_test asserts the operational-awareness template
// fragment ships a non-fatal Dolt diagnostic protocol. The fragment
// is rendered into agent prompts (gc prime, boot context, deacon
// patrol), so its prose is operationally load-bearing — false claims
// like "safe — does not kill the process" lead operators to destroy
// the very evidence they are trying to capture (issue #1485).
package gastown_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// killQUITRe matches `kill -QUIT` as an executable invocation:
// anchored at start-of-line (with optional leading whitespace) and
// followed by the QUIT signal across the common shape variations —
// `kill -QUIT`, `kill  -QUIT` (multi-space), `kill\t-QUIT` (tab), and
// `kill \\\n-QUIT` (line continuation). The line anchor matters: an
// inline backticked mention like `... use \`kill -QUIT\` ...` in
// markdown prose does NOT begin a line, so it does not match.
// Combined with stripShellComments, this leaves only active shell
// statements as match candidates.
var killQUITRe = regexp.MustCompile(`(?m)^[ \t]*kill[ \t\\]+\n?[ \t]*-QUIT(\s|$)`)

// operationalAwarenessFragment is the on-disk path to the template
// fragment that ships into every gastown agent prompt via the
// city's global_fragments list.
const operationalAwarenessFragment = "packs/gastown/template-fragments/operational-awareness.template.md"

// stripShellComments removes lines whose first non-whitespace
// character is `#`, so commented-out documentation (like the
// SIGQUIT-escalation example) doesn't trip content fences that
// scan for active recommendations.
func stripShellComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestOperationalAwarenessFragmentNonFatalDiagnostic is the regression
// fence for issue #1485. The fragment must (1) not actively recommend
// `kill -QUIT` (fatal to Dolt's Go runtime), (2) document at least one
// non-fatal in-process diagnostic as an active step, and (3) not carry
// the original "safe — does not kill the process" claim.
func TestOperationalAwarenessFragmentNonFatalDiagnostic(t *testing.T) {
	path := filepath.Join(exampleDir(), operationalAwarenessFragment)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", operationalAwarenessFragment, err)
	}
	body := string(data)
	active := stripShellComments(body)

	t.Run("no_active_kill_QUIT", func(t *testing.T) {
		if m := killQUITRe.FindString(active); m != "" {
			t.Errorf("%s contains an active `kill -QUIT` step (match: %q).\n"+
				"SIGQUIT is fatal to Dolt's Go runtime — it dumps goroutines AND exits. "+
				"This destroys the evidence the diagnostic protocol claims to preserve. "+
				"Use a non-fatal in-process diagnostic (e.g. `gc dolt sql -q \"SHOW FULL PROCESSLIST\"`) "+
				"as the default; document SIGQUIT only as a commented-out last-resort escalation. "+
				"See issue #1485.", operationalAwarenessFragment, m)
		}
	})

	t.Run("documents_non_fatal_default", func(t *testing.T) {
		wantOne := []string{
			"SHOW FULL PROCESSLIST",
			"gc dolt sql -q",
		}
		for _, w := range wantOne {
			if strings.Contains(active, w) {
				return
			}
		}
		t.Errorf("%s does not document any non-fatal Dolt diagnostic "+
			"as an active step; expected at least one of %v outside "+
			"shell comments. Without an active non-fatal default, "+
			"operators fall back to fatal restarts. See issue #1485.",
			operationalAwarenessFragment, wantOne)
	})

	t.Run("no_false_safe_claim", func(t *testing.T) {
		if strings.Contains(body, "safe — does not kill the process") {
			t.Errorf("%s still contains the false-safe SIGQUIT claim "+
				"(\"safe — does not kill the process\"). SIGQUIT terminates "+
				"the Dolt server. See issue #1485.", operationalAwarenessFragment)
		}
	})
}
