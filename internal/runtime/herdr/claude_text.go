package herdr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude Code wraps multi-line input that arrives as a terminal paste in
// <pasted_content> tags and follows instructions inside it only when the
// user's own message asks it to. Every herdr delivery is such a paste, so a
// role prompt or a multi-line nudge arrived as text the model is told not to
// obey, with an empty user message around it, and sessions sat refusing their
// prime. A single line is typed input and is the user's own message. So
// multi-line text for a Claude session goes to a file and the one typed line
// names it. A startup file is also appended to the system prompt at launch,
// but an interactive --resume keeps the system prompt of the life it resumes,
// so the typed line names the file either way.

const (
	claudeKind             = "claude"
	metaAgentKind          = "GC_HERDR_AGENT_KIND"
	claudeStartupPromptKey = "startup-prompt.md"
	claudeNudgePrefix      = "nudge-"
	// claudeNudgeRetention is how long a nudge file outlives its delivery.
	// Claude reads it only when it takes the typed line, which can wait behind
	// a long turn, so the bound is generous; Stop's clearMeta removes the rest.
	claudeNudgeRetention = 24 * time.Hour
)

func isMultiline(text string) bool { return strings.Contains(text, "\n") }

// writeMessageFile stores text in the session's owner-only sidecar directory
// and returns its path. Stop's clearMeta removes it with the session.
func (p *Provider) writeMessageFile(name, key, text string) (string, error) {
	if err := p.SetMeta(name, key, text); err != nil {
		return "", err
	}
	return filepath.Join(p.metaDir, sanitize(name), sanitize(key)), nil
}

// prepareStartupTurn returns the text start types as the session's first
// turn. It first drops a prior life's agent-kind marker: Stop wipes the
// sidecar but a crash does not, and a stale claude marker would turn the next
// non-Claude life's multi-line nudges into file pointers. For a Claude launch
// it records the kind and moves multi-line startup text into a file, which the
// launch appends to the system prompt and the returned kickoff names.
func (p *Provider) prepareStartupTurn(name string, spec *launchSpec, startupText string) (string, error) {
	if err := p.RemoveMeta(name, metaAgentKind); err != nil {
		fmt.Fprintf(os.Stderr, "herdr: clearing prior-life agent kind for %q failed: %v\n", name, err) //nolint:errcheck // best-effort diagnostic
	}
	if spec.Kind != claudeKind {
		return startupText, nil
	}
	if err := p.SetMeta(name, metaAgentKind, claudeKind); err != nil {
		return "", fmt.Errorf("herdr: record agent kind for %q: %w", name, err)
	}
	if !isMultiline(startupText) {
		return startupText, nil
	}
	path, err := p.writeMessageFile(name, claudeStartupPromptKey, startupText)
	if err != nil {
		return "", fmt.Errorf("herdr: write startup prompt for %q: %w", name, err)
	}
	spec.Args = append(spec.Args, "--append-system-prompt-file", path)
	return claudeStartupKickoff(startupText, path), nil
}

// claudeStartupKickoff is the typed first turn of a session whose startup text
// went to the file at path. It keeps the beacon line, which is what makes the
// session findable in Claude Code's /resume picker.
func claudeStartupKickoff(startupText, path string) string {
	kickoff := "Your Gas City instructions for this session are in " + path + ". Read that file and follow it now, starting with the first action it names."
	first, _, _ := strings.Cut(startupText, "\n")
	if strings.HasPrefix(first, "[") && strings.Contains(first, " • ") {
		return first + " " + kickoff
	}
	return kickoff
}

// claudeSafeNudge returns text unchanged unless it is multi-line and the
// session is a Claude agent; then the text goes to a file and the nudge becomes
// one typed line naming it. A kind that cannot be read leaves the text as it is.
func (p *Provider) claudeSafeNudge(name, text string) (string, error) {
	if !isMultiline(text) {
		return text, nil
	}
	kind, err := p.GetMeta(name, metaAgentKind)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr: reading the agent kind of %q failed, so its multi-line nudge goes as it is: %v\n", name, err) //nolint:errcheck // best-effort diagnostic
	}
	if kind != claudeKind {
		return text, nil
	}
	now := time.Now()
	p.pruneNudgeFiles(name, now)
	path, err := p.writeMessageFile(name, fmt.Sprintf("%s%d.md", claudeNudgePrefix, now.UnixNano()), text)
	if err != nil {
		return "", fmt.Errorf("herdr: write nudge for %q: %w", name, err)
	}
	return "Gas City sent you a message for this session in " + path + ". Read that file and act on it now.", nil
}

// pruneNudgeFiles removes the session's nudge files older than
// claudeNudgeRetention, so a long-lived session's sidecar stays bounded.
func (p *Provider) pruneNudgeFiles(name string, now time.Time) {
	entries, err := os.ReadDir(filepath.Join(p.metaDir, sanitize(name)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr: listing nudge files of %q failed: %v\n", name, err) //nolint:errcheck // best-effort diagnostic
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), claudeNudgePrefix) {
			continue
		}
		info, err := e.Info()
		if err == nil && now.Sub(info.ModTime()) >= claudeNudgeRetention {
			err = p.RemoveMeta(name, e.Name())
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "herdr: pruning nudge file %s of %q failed: %v\n", e.Name(), name, err) //nolint:errcheck // best-effort diagnostic
		}
	}
}
