package herdr

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Claude Code wraps multi-line input that arrives as a terminal paste in
// <pasted_content> tags and follows instructions inside it only when the
// user's own message asks it to. Every herdr delivery is such a paste, so a
// role prompt or a multi-line nudge arrived as text the model is told not to
// obey, with an empty user message around it, and sessions sat refusing their
// prime. A single line is typed input and is the user's own message. So a
// multi-line startup text rides the system prompt (the file is read once, at
// launch) and a multi-line nudge rides a file the one typed line points at.

const (
	claudeKind             = "claude"
	metaAgentKind          = "GC_HERDR_AGENT_KIND"
	claudeStartupPromptKey = "startup-prompt.md"
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

// claudeStartupKickoff is the typed first turn of a session whose startup text
// went into the system prompt. It keeps the beacon line, which is what makes
// the session findable in Claude Code's /resume picker.
func claudeStartupKickoff(startupText string) string {
	const kickoff = "Your Gas City instructions for this session are appended to your system prompt. Follow them now, starting with the first action they name."
	first, _, _ := strings.Cut(startupText, "\n")
	if strings.HasPrefix(first, "[") && strings.Contains(first, " • ") {
		return first + " " + kickoff
	}
	return kickoff
}

// claudeSafeNudge returns text unchanged unless it is multi-line and the
// session is a Claude agent; then the text goes to a file and the nudge becomes
// one typed line naming it.
func (p *Provider) claudeSafeNudge(name, text string) (string, error) {
	if !isMultiline(text) {
		return text, nil
	}
	if kind, err := p.GetMeta(name, metaAgentKind); err != nil || kind != claudeKind {
		return text, nil
	}
	key := fmt.Sprintf("nudge-%d.md", time.Now().UnixNano())
	path, err := p.writeMessageFile(name, key, text)
	if err != nil {
		return "", fmt.Errorf("herdr: write nudge for %q: %w", name, err)
	}
	return "Gas City sent you a message for this session in " + path + ". Read that file and act on it now.", nil
}
