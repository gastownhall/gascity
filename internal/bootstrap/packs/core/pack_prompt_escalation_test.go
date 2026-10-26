package core

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// coreWorkerPromptPaths are the shipped prompts that tell a live worker how
// to escalate when it is blocked.
var coreWorkerPromptPaths = []string{
	"assets/prompts/graph-worker.md",
	"assets/prompts/pool-worker.template.md",
}

// workerEscalationSection returns the body of a prompt's ## Escalation
// section, from after the heading up to the next ## heading.
func workerEscalationSection(t *testing.T, path, body string) string {
	t.Helper()
	const heading = "## Escalation\n"
	start := strings.Index(body, heading)
	if start < 0 {
		t.Fatalf("%s has no ## Escalation section", path)
	}
	rest := body[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// workerEscalationBashBlock returns the Escalation section's single fenced
// bash block, the command a blocked worker copies. Any count but one is a
// defect: zero leaves no pinned command, two invite the model to choose.
func workerEscalationBashBlock(t *testing.T, path, section string) string {
	t.Helper()
	const (
		openFence  = "```bash\n"
		closeFence = "\n```\n"
	)
	var blocks []string
	for rest := section; ; {
		i := strings.Index(rest, openFence)
		if i < 0 {
			break
		}
		rest = rest[i+len(openFence):]
		j := strings.Index(rest, closeFence)
		if j < 0 {
			t.Fatalf("%s: Escalation bash block is not closed", path)
		}
		blocks = append(blocks, rest[:j+1])
		rest = rest[j+len(closeFence):]
	}
	if len(blocks) != 1 {
		t.Fatalf("%s: Escalation section carries %d bash blocks, want exactly 1", path, len(blocks))
	}
	return blocks[0]
}

// runWorkerEscalationBlock executes a prompt's pinned escalation command
// under the same environment contract as the escalate.sh tests and returns
// the argv gc received, one element per line, so quoting survives.
func runWorkerEscalationBlock(t *testing.T, block string, extraEnv []string) []string {
	t.Helper()
	binDir, _ := fakeGCBin(t, `printf '%s\n' "$@" > "$ARGV_LOG"`+"\n")
	dir := t.TempDir()
	blockPath := filepath.Join(dir, "escalation-block.sh")
	if err := os.WriteFile(blockPath, []byte(block), 0o755); err != nil {
		t.Fatalf("write escalation block: %v", err)
	}
	argvPath := filepath.Join(dir, "argv.log")
	env := append(append([]string(nil), extraEnv...), "ARGV_LOG="+argvPath)
	out, err := runPackScript(t, blockPath, binDir, escalationEnvKeys, env)
	if err != nil {
		t.Fatalf("escalation command failed: %v\n%s", err, out)
	}
	logged, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("escalation command never called gc: %v\n%s", err, out)
	}
	return strings.Split(strings.TrimSuffix(string(logged), "\n"), "\n")
}

// TestCoreWorkerPromptsEscalateToConfiguredRecipient pins the escalation
// contract of the core worker prompts. A blocked worker mails
// GC_ESCALATION_RECIPIENT — the routing every other core escalation path
// (escalate.sh, the human-gate scripts) already honors — defaulting to the
// reserved human inbox, and the pinned command wakes an agent recipient with
// --notify, which gc already treats as a no-op for the sessionless human
// alias. Hard-coding `gc mail send human`, as these prompts did, lands
// worker escalations in an inbox nothing watches while the configured
// recipient never hears them (issue #7154). The section carries no template
// action, so rendering leaves the command verbatim, and the two prompts'
// sections stay byte-identical so the copies cannot drift.
func TestCoreWorkerPromptsEscalateToConfiguredRecipient(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{name: "set", env: []string{"GC_ESCALATION_RECIPIENT=triage-agent"}, want: "triage-agent"},
		{name: "qualified", env: []string{"GC_ESCALATION_RECIPIENT=myrig/triage"}, want: "myrig/triage"},
		{name: "spaces", env: []string{"GC_ESCALATION_RECIPIENT=ops desk"}, want: "ops desk"},
		// The default route stays human: unset and empty both resolve to the
		// reserved mailbox (`:-`, not `-`), with --notify along for the ride
		// as the pinned no-op gc already skips for human.
		{name: "unset", want: "human"},
		{name: "empty", env: []string{"GC_ESCALATION_RECIPIENT="}, want: "human"},
	}
	var sections []string
	for _, path := range coreWorkerPromptPaths {
		t.Run(path, func(t *testing.T) {
			data, err := fs.ReadFile(PackFS, path)
			if err != nil {
				t.Fatalf("core pack missing %s: %v", path, err)
			}
			section := workerEscalationSection(t, path, string(data))
			sections = append(sections, section)

			if strings.Contains(section, "gc mail send human") {
				t.Errorf("%s: Escalation hard-codes the human recipient; BLOCKED mail must route through ${GC_ESCALATION_RECIPIENT:-human} like every other core escalation path", path)
			}
			if strings.Contains(section, "mail that instead") || strings.Contains(section, "coordinator") {
				t.Errorf("%s: Escalation offers a coordinator alternative; recipient routing is configuration (GC_ESCALATION_RECIPIENT), not model judgment", path)
			}
			if !strings.Contains(section, "GC_ESCALATION_RECIPIENT") {
				t.Errorf("%s: Escalation section does not name GC_ESCALATION_RECIPIENT", path)
			}
			if !strings.Contains(section, "`human`") {
				t.Errorf("%s: Escalation section does not document the human fallback", path)
			}
			if strings.Contains(section, "{{") {
				t.Errorf("%s: Escalation section contains a template action; the pinned command must survive rendering verbatim", path)
			}

			block := workerEscalationBashBlock(t, path, section)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					argv := runWorkerEscalationBlock(t, block, tc.env)
					want := strings.Join([]string{
						"mail", "send", tc.want, "--notify",
						"-s", "BLOCKED: Brief description",
						"-m", "Details of the issue",
					}, "\n")
					if got := strings.Join(argv, "\n"); got != want {
						t.Errorf("escalation argv =\n%s\nwant\n%s", got, want)
					}
				})
			}
		})
	}
	if sections[0] != sections[1] {
		t.Errorf("the Escalation sections of %s and %s must stay byte-identical; they have drifted", coreWorkerPromptPaths[0], coreWorkerPromptPaths[1])
	}
}
