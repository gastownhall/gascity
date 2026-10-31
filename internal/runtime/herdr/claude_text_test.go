package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// A Claude session must never receive multi-line text as a paste: Claude Code
// marks it <pasted_content> and the model declines instructions inside it.
// Multi-line startup text goes to a file that is appended to the system prompt
// and named by a one-line kickoff keeping the beacon; the kickoff must name the
// file because a resumed Claude keeps the system prompt of the life it
// resumes. A multi-line nudge goes to a file named by one line. A one-line
// nudge, and any text for another agent kind, is delivered as it is.
func TestClaudeSessionsNeverReceiveMultilinePastes(t *testing.T) {
	const (
		name   = "gastown__worker"
		beacon = "[gastown] gastown/worker-1 • 2026-09-28T10:00:00"
		role   = "# GC Role Worker\n\nRun the claim block first."
		claim  = "Run gc hook --claim --json now."
		nudge  = "Blocker closed.\n\nContinue your bead."
	)
	primed := beacon + "\n\n" + role
	for _, tc := range []struct {
		name string
		cfg  runtime.Config
		// priorKind is the agent-kind marker a crashed prior life left behind.
		priorKind string
		// startup is the text the session must be able to read at startup.
		startup string
		// claude sessions get the file delivery: args is the argv after
		// herdr's `--` ahead of the system-prompt file, and lead is the
		// beacon the kickoff keeps.
		claude bool
		args   string
		lead   string
	}{
		{
			name:    "fresh claude launch",
			cfg:     runtime.Config{Command: "claude --effort medium", PromptSuffix: shellquote.Quote(primed), Nudge: claim},
			startup: primed + "\n\n" + claim,
			claude:  true,
			args:    "--effort medium ",
			lead:    beacon + " ",
		},
		{
			// gc's restart of a session with a resume key: no prime, and the
			// whole restart text in the nudge.
			name:    "resumed claude launch",
			cfg:     runtime.Config{Command: "claude --resume 7f3c2a1e", Nudge: primed + "\n\n---\n\n" + claim},
			startup: primed + "\n\n---\n\n" + claim,
			claude:  true,
			args:    "--resume 7f3c2a1e ",
			lead:    beacon + " ",
		},
		{
			name:    "claude prime without a beacon",
			cfg:     runtime.Config{Command: "claude", PromptSuffix: shellquote.Quote(role)},
			startup: role,
			claude:  true,
		},
		{
			name:    "codex launch",
			cfg:     runtime.Config{Command: "codex", PromptSuffix: shellquote.Quote(primed)},
			startup: primed,
		},
		{
			name:      "codex after a crashed claude life",
			cfg:       runtime.Config{Command: "codex", PromptSuffix: shellquote.Quote(primed)},
			priorKind: claudeKind,
			startup:   primed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			if tc.priorKind != "" {
				if err := p.SetMeta(name, metaAgentKind, tc.priorKind); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Start(context.Background(), name, tc.cfg); err != nil {
				t.Fatalf("Start: %v", err)
			}
			for _, text := range []string{nudge, "You have mail."} {
				if err := p.Nudge(name, runtime.TextContent(text)); err != nil {
					t.Fatalf("Nudge(%q): %v", text, err)
				}
			}
			calls := fakeCalls(t, state)
			if !strings.Contains(calls, "agent prompt %5 You have mail.\n") {
				t.Fatalf("one-line nudge was not delivered as it is:\n%s", calls)
			}
			if !tc.claude {
				if strings.Contains(calls, "--append-system-prompt-file") || !strings.Contains(calls, "agent prompt %5 "+tc.startup+" --wait") {
					t.Fatalf("%s startup text was not delivered as it is:\n%s", tc.cfg.Command, calls)
				}
				if !strings.Contains(calls, "agent prompt %5 "+nudge+"\n") {
					t.Fatalf("%s multi-line nudge was not delivered as it is:\n%s", tc.cfg.Command, calls)
				}
				return
			}
			if strings.Contains(calls, "\n\n") {
				t.Fatalf("multi-line text reached the Claude pane:\n%s", calls)
			}
			path := filepath.Join(p.metaDir, sanitize(name), claudeStartupPromptKey)
			if !regexp.MustCompile(`(?m)^agent start \S+ --kind claude .* -- ` + regexp.QuoteMeta(tc.args+"--append-system-prompt-file "+path) + `$`).MatchString(calls) {
				t.Fatalf("claude was not launched with the startup text as a system-prompt file %s:\n%s", path, calls)
			}
			if b, err := os.ReadFile(path); err != nil || string(b) != tc.startup {
				t.Fatalf("startup file = %q, %v; want %q", b, err, tc.startup)
			}
			kickoff := tc.lead + "Your Gas City instructions for this session are in " + path + ". Read that file and follow it now, starting with the first action it names."
			if !regexp.MustCompile(`(?m)^agent prompt %5 ` + regexp.QuoteMeta(kickoff) + ` --wait `).MatchString(calls) {
				t.Fatalf("first turn is not the one-line kickoff naming the startup file:\n%s", calls)
			}
			pointer := regexp.MustCompile(`(?m)^agent prompt %5 Gas City sent you a message for this session in (\S+)\. Read that file and act on it now\.$`).FindStringSubmatch(calls)
			if pointer == nil {
				t.Fatalf("multi-line nudge was not replaced by a one-line pointer:\n%s", calls)
			}
			if b, err := os.ReadFile(pointer[1]); err != nil || string(b) != nudge {
				t.Fatalf("nudge file = %q, %v; want %q", b, err, nudge)
			}
		})
	}
}

// A sidecar whose agent kind cannot be read must not cost the nudge: it is
// delivered as it is, exactly as before the Claude file delivery existed.
func TestClaudeSafeNudgeDeliversVerbatimWhenTheKindIsUnreadable(t *testing.T) {
	p, _ := newFakeHerdrProvider(t)
	const name = "gastown__worker"
	// A directory where the marker file belongs makes its read fail (EISDIR).
	if err := os.MkdirAll(filepath.Join(p.metaDir, sanitize(name), metaAgentKind), 0o700); err != nil {
		t.Fatal(err)
	}
	const text = "Blocker closed.\n\nContinue your bead."
	if got, err := p.claudeSafeNudge(name, text); err != nil || got != text {
		t.Fatalf("claudeSafeNudge = %q, %v; want the text as it is", got, err)
	}
}

// Each multi-line Claude nudge leaves a file that must outlive its delivery,
// so writing one prunes the session's nudge files older than
// claudeNudgeRetention and nothing else: a younger nudge may still be unread,
// and the rest of the sidecar is the session's metadata.
func TestClaudeSafeNudgePrunesOnlyExpiredNudgeFiles(t *testing.T) {
	p, _ := newFakeHerdrProvider(t)
	const name = "gastown__worker"
	if err := p.SetMeta(name, metaAgentKind, claudeKind); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(p.metaDir, sanitize(name))
	expired := time.Now().Add(-claudeNudgeRetention - time.Minute)
	kept := map[string]bool{
		"nudge-1.md":           false,
		"nudge-2.md":           true,
		metaBoundPane:          true,
		claudeStartupPromptKey: true,
	}
	for key := range kept {
		if err := p.SetMeta(name, key, "x"); err != nil {
			t.Fatal(err)
		}
		mtime := expired
		if key == "nudge-2.md" {
			mtime = time.Now().Add(-claudeNudgeRetention + time.Minute)
		}
		if err := os.Chtimes(filepath.Join(dir, key), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	got, err := p.claudeSafeNudge(name, "Blocker closed.\n\nContinue your bead.")
	if err != nil {
		t.Fatalf("claudeSafeNudge: %v", err)
	}
	for key, want := range kept {
		_, err := os.Stat(filepath.Join(dir, key))
		if present := err == nil; present != want || (err != nil && !errors.Is(err, os.ErrNotExist)) {
			t.Errorf("%s present = %v (%v); want %v", key, present, err, want)
		}
	}
	pointer := regexp.MustCompile(`in (\S+)\. Read that file`).FindStringSubmatch(got)
	if pointer == nil {
		t.Fatalf("claudeSafeNudge = %q; want a pointer to the nudge file", got)
	}
	if _, err := os.Stat(pointer[1]); err != nil {
		t.Fatalf("the nudge just written was pruned: %v", err)
	}
}
