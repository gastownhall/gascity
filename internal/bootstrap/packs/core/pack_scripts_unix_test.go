//go:build unix

package core

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestCoreWorkerPromptsEscalateToConfiguredRecipient runs each core worker
// prompt's shipped BLOCKED command against a fake gc and pins that it mails
// $GC_ESCALATION_RECIPIENT with --notify, as escalate.sh does, so a
// configured triage agent is woken instead of the mail landing in the
// sessionless human inbox. Unset and empty both fall back to human, for which
// --notify is a no-op. The section must not leave the recipient to the
// model's judgment or hide the command behind a template action.
func TestCoreWorkerPromptsEscalateToConfiguredRecipient(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{name: "set", env: []string{"GC_ESCALATION_RECIPIENT=triage-agent"}, want: "triage-agent"},
		{name: "qualified", env: []string{"GC_ESCALATION_RECIPIENT=myrig/triage"}, want: "myrig/triage"},
		{name: "spaces", env: []string{"GC_ESCALATION_RECIPIENT=ops desk"}, want: "ops desk"},
		{name: "unset", want: "human"},
		{name: "empty", env: []string{"GC_ESCALATION_RECIPIENT="}, want: "human"},
	}
	sections := make(map[string]string, len(coreWorkerPrompts))
	for _, path := range coreWorkerPrompts {
		t.Run(path, func(t *testing.T) {
			data, err := fs.ReadFile(PackFS, path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			section := promptSection(t, string(data), "Escalation")
			sections[path] = section
			for _, banned := range []string{"gc mail send human", "mail that instead", "coordinator", "{{"} {
				if strings.Contains(section, banned) {
					t.Errorf("Escalation section contains %q; routing must come from GC_ESCALATION_RECIPIENT and render verbatim:\n%s", banned, section)
				}
			}
			for _, want := range []string{"GC_ESCALATION_RECIPIENT", "`human`"} {
				if !strings.Contains(section, want) {
					t.Errorf("Escalation section does not name %q:\n%s", want, section)
				}
			}
			block := singleBashBlock(t, section)
			scriptPath := filepath.Join(t.TempDir(), "escalation-block.sh")
			if err := os.WriteFile(scriptPath, []byte(block), 0o644); err != nil {
				t.Fatalf("write escalation block: %v", err)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					binDir, _ := fakeGCBin(t, `printf '%s\n' "$@" > "$ARGV_LOG"`)
					argvPath := filepath.Join(t.TempDir(), "argv")
					out, err := runPackScript(t, scriptPath, binDir, escalationEnvKeys,
						append(tc.env, "ARGV_LOG="+argvPath))
					if err != nil {
						t.Fatalf("escalation block failed: %v\n%s", err, out)
					}
					logged, err := os.ReadFile(argvPath)
					if err != nil {
						t.Fatalf("escalation block never ran gc: %v\n%s", err, out)
					}
					got := strings.Split(strings.TrimSuffix(string(logged), "\n"), "\n")
					want := []string{
						"mail", "send", tc.want, "--notify",
						"-s", "BLOCKED: Brief description", "-m", "Details of the issue",
					}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("gc argv = %q, want %q", got, want)
					}
				})
			}
		})
	}
	// graph-worker.md is not a template and cannot share a fragment, so the
	// two copies are pinned identical to keep them from drifting.
	if first, second := sections[coreWorkerPrompts[0]], sections[coreWorkerPrompts[1]]; first != second {
		t.Errorf("Escalation sections differ:\n%s:\n%s\n%s:\n%s",
			coreWorkerPrompts[0], first, coreWorkerPrompts[1], second)
	}
}

// TestMolDoWorkClosesOnlyItsVerifiedClaimedStep runs the do-work step's close
// block in a session shell, where GC_BEAD_ID is never exported (issue #7991).
// The block must close the step `gc hook current --id-only` names once it has
// verified that step is this session's in-progress do-work step and not the
// work bead, and must exit non-zero, closing nothing, whenever it cannot.
func TestMolDoWorkClosesOnlyItsVerifiedClaimedStep(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	block := molDoWorkCloseStepBlock(t)
	scriptPath := filepath.Join(t.TempDir(), "close-step.sh")
	if err := os.WriteFile(scriptPath, []byte(block), 0o644); err != nil {
		t.Fatalf("write close-step block: %v", err)
	}
	good := closeStepCase{hookOut: "ga-step", convoy: closeStepConvoy, show: closeStepClaimed, wantClose: true}
	with := func(name string, edit func(*closeStepCase)) closeStepCase {
		c := good
		c.name = name
		edit(&c)
		return c
	}
	claimed := func(fields string) string {
		return `[{"id":"ga-step",` + fields + `}]`
	}
	cases := []closeStepCase{
		with("AC-1 assignee is the session id", func(*closeStepCase) {}),
		with("AC-1 assignee is the session name", func(c *closeStepCase) {
			c.show = claimed(`"status":"in_progress","assignee":"rig--polecat-1","metadata":{"gc.step_ref":"mol-do-work.do-work"}`)
		}),
		with("AC-1 assignee is the alias", func(c *closeStepCase) {
			c.env = []string{"GC_ALIAS=rig/polecat"}
			c.show = claimed(`"status":"in_progress","assignee":"rig/polecat","metadata":{"gc.step_ref":"mol-do-work.do-work"}`)
		}),
		with("AC-1 GC_BEAD_ID names another bead", func(c *closeStepCase) {
			c.env = []string{"GC_BEAD_ID=ga-other"}
		}),
		with("AC-1 bd show prints an object", func(c *closeStepCase) {
			c.show = strings.TrimSuffix(strings.TrimPrefix(closeStepClaimed, "["), "]")
		}),
		with("AC-2 hook current fails", func(c *closeStepCase) {
			c.hookOut, c.hookRC, c.wantClose = "", 1, false
		}),
		with("AC-2 hook current prints nothing", func(c *closeStepCase) {
			c.hookOut, c.wantClose = "", false
		}),
		with("AC-2 hook current prints nothing while GC_BEAD_ID is set", func(c *closeStepCase) {
			c.hookOut, c.wantClose = "", false
			c.env = []string{"GC_BEAD_ID=ga-step"}
		}),
		with("AC-3 step is closed", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"closed","assignee":"gcg-session-me","metadata":{"gc.step_ref":"mol-do-work.do-work"}`), false, "ga-step"
		}),
		with("AC-3 step is open", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"open","assignee":"gcg-session-me","metadata":{"gc.step_ref":"mol-do-work.do-work"}`), false, "ga-step"
		}),
		with("AC-3 step belongs to another session", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"in_progress","assignee":"gcg-session-other","metadata":{"gc.step_ref":"mol-do-work.do-work"}`), false, "ga-step"
		}),
		with("AC-3 step is unassigned and no alias is set", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"in_progress","assignee":"","metadata":{"gc.step_ref":"mol-do-work.do-work"}`), false, "ga-step"
		}),
		with("AC-3 step is the drain step", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"in_progress","assignee":"gcg-session-me","metadata":{"gc.step_ref":"mol-do-work.drain"}`), false, "ga-step"
		}),
		with("AC-3 step has no step ref", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"in_progress","assignee":"gcg-session-me","metadata":{}`), false, "ga-step"
		}),
		with("AC-3 step ref only ends in the do-work ref", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"in_progress","assignee":"gcg-session-me","metadata":{"gc.step_ref":"x.mol-do-work.do-work"}`), false, "ga-step"
		}),
		with("AC-3 hook names the work bead", func(c *closeStepCase) {
			c.hookOut, c.wantClose, c.wantNamed = "ga-work", false, "ga-work"
			c.show = `[{"id":"ga-work","status":"in_progress","assignee":"gcg-session-me","metadata":{"gc.step_ref":"mol-do-work.do-work"}}]`
		}),
		with("AC-4 convoy read fails", func(c *closeStepCase) {
			c.convoy, c.wantClose = "", false
		}),
		with("AC-4 convoy has two children", func(c *closeStepCase) {
			c.convoy, c.wantClose, c.wantNamed = `{"children":[{"id":"ga-work"},{"id":"ga-more"}]}`, false, "cv-1"
		}),
		with("AC-4 convoy has no children", func(c *closeStepCase) {
			c.convoy, c.wantClose, c.wantNamed = `{"children":[]}`, false, "cv-1"
		}),
		with("AC-4 bd show fails", func(c *closeStepCase) {
			c.show, c.wantClose = "", false
		}),
		with("AC-4 closing update fails", func(c *closeStepCase) {
			c.updateRC, c.wantClose = 1, false
		}),
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir, logPath := fakeGCBin(t, tc.fakeGC())
			if err := os.WriteFile(logPath, nil, 0o644); err != nil {
				t.Fatalf("create gc log: %v", err)
			}
			env := append([]string{"GC_SESSION_ID=gcg-session-me", "GC_SESSION_NAME=rig--polecat-1"}, tc.env...)
			out, runErr := runPackScript(t, scriptPath, binDir, closeStepEnvKeys, env)
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read gc log: %v", err)
			}
			logged := string(data)
			var updates []string
			for _, line := range strings.Split(logged, "\n") {
				if strings.HasPrefix(line, "gc bd update ") {
					updates = append(updates, line)
				}
			}
			if strings.Contains(logged, "ga-other") {
				t.Errorf("block named the GC_BEAD_ID bead ga-other:\n%s", logged)
			}
			if tc.wantClose {
				if runErr != nil {
					t.Fatalf("block failed: %v\noutput:\n%s\ngc calls:\n%s", runErr, out, logged)
				}
				want := []string{"gc bd update ga-step --set-metadata gc.outcome=pass --status=closed"}
				if strings.Join(updates, "\n") != strings.Join(want, "\n") {
					t.Fatalf("updates = %q, want %q\noutput:\n%s", updates, want, out)
				}
				return
			}
			if runErr == nil {
				t.Fatalf("block exited 0 without a verified claimed step\noutput:\n%s\ngc calls:\n%s", out, logged)
			}
			if tc.updateRC == 0 && len(updates) != 0 {
				t.Fatalf("block issued updates %q without a verified claimed step\noutput:\n%s", updates, out)
			}
			if strings.TrimSpace(out) == "" {
				t.Fatal("block failed without a diagnostic")
			}
			if !strings.Contains(out, tc.wantNamed) {
				t.Fatalf("diagnostic does not name %s:\n%s", tc.wantNamed, out)
			}
		})
	}
}
