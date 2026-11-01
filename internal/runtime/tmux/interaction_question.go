package tmux

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Pane-based AskUserQuestion detection
// ---------------------------------------------------------------------------
//
// Claude Code's AskUserQuestion tool opens a dialog that owns the pane's input:
// a submitted Enter selects the focused option, so any typed nudge answers it.
// As of Claude Code 2.1.280 it looks like this:
//
//	 ☐ be-tx30v
//	│ Push the deploy branch and open the PR. How do you want to proceed?
//	❯ 1. Run gates, then ship if clean (Recommended)
//	     Run gates, then ship if clean
//	  2. Keep holding
//	     Keep holding
//	  3. Type something.
//	────────────────────────────────────────────────────────────────────────
//	  4. Chat about this
//	Enter to select · ↑/↓ to navigate · Esc to cancel
//
// The dialog is live only while its select footer is the last thing on screen.
// Once it is answered the frame stays in scrollback with the agent's output (or
// the idle composer) beneath it, so a footer with anything below it is history.
// Detection also needs the dialog's other two anchors above the footer, a ☐
// header row and the focused numbered option, so the folder-trust dialog
// ("Enter to confirm") and an idle composer's bare "❯" never read as a question.
//
// The matcher is checked against one live frame shape, a single-select,
// single-question dialog (Claude Code 2.1.280). A multiSelect question, a
// multi-question dialog's later tabs, or a status row under the footer are not
// verified against a live capture. A form the matcher misses reads as no
// question and is delivered to as it was before this guard existed.

var (
	// askUserQuestionFooterRe matches the select footer on its trimmed row.
	askUserQuestionFooterRe = regexp.MustCompile(`^Enter to select\b.*Esc to cancel$`)

	// askUserQuestionHeaderRe matches the ☐ header row. A multi-question dialog
	// leads it with the tab arrow ("← ☐ Approach  ☐ Scope  ✔ Submit →").
	askUserQuestionHeaderRe = regexp.MustCompile(`^\s*(?:←\s*)?☐\s+(.+)$`)

	// askUserQuestionOptionRe matches a numbered option row; the focused one
	// carries the cursor glyph.
	askUserQuestionOptionRe = regexp.MustCompile(`^(❯\s+)?(\d+)\.\s*(.*)$`)
)

const (
	// askUserQuestionTailSpan is how many non-blank rows from the bottom, the
	// footer's own row included, the footer may sit within. A row that starts
	// with a prompt glyph below it is the composer, so the dialog is history.
	askUserQuestionTailSpan = 2

	// askUserQuestionMaxRows bounds how far above the footer the header may be.
	askUserQuestionMaxRows = 60

	// askUserQuestionPromptMax bounds the prompt text a client is shown, in runes.
	askUserQuestionPromptMax = 400
)

// parsedQuestion is the AskUserQuestion dialog a pane shows.
type parsedQuestion struct {
	Header   string
	Question string
	// Choices are the numbered options' labels in pane order.
	Choices []string
	// Focused is the label of the highlighted option.
	Focused string
}

// parseAskUserQuestion returns the open AskUserQuestion dialog the pane lines
// show, or nil when they show none.
func parseAskUserQuestion(lines []string) *parsedQuestion {
	// Walk up from the bottom to the footer, allowing only the dialog's own
	// trailing rows below it.
	footer, seen := -1, 0
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if askUserQuestionFooterRe.MatchString(trimmed) {
			footer = i
			break
		}
		seen++
		if seen >= askUserQuestionTailSpan || strings.HasPrefix(trimmed, "❯") || strings.HasPrefix(trimmed, ">") {
			return nil
		}
	}
	if footer < 0 {
		return nil
	}

	header := -1
	for i := footer - 1; i >= 0 && footer-i <= askUserQuestionMaxRows; i-- {
		if askUserQuestionHeaderRe.MatchString(lines[i]) {
			header = i
			break
		}
	}
	if header < 0 {
		return nil
	}

	q := &parsedQuestion{Header: strings.TrimSpace(askUserQuestionHeaderRe.FindStringSubmatch(lines[header])[1])}
	var question []string
	focused := false
	for _, line := range lines[header+1 : footer] {
		trimmed := strings.TrimSpace(line)
		if m := askUserQuestionOptionRe.FindStringSubmatch(trimmed); m != nil {
			label := strings.TrimSpace(m[3])
			q.Choices = append(q.Choices, label)
			if m[1] != "" {
				focused = true
				q.Focused = label
			}
			continue
		}
		if len(q.Choices) == 0 && trimmed != "" {
			question = append(question, strings.TrimSpace(strings.TrimPrefix(trimmed, "│")))
		}
	}
	if !focused {
		return nil
	}
	q.Question = strings.Join(question, " ")
	return q
}

// promptText renders the description a client shows for the pending question:
// its header, the question and the visible choices, bounded.
func (q *parsedQuestion) promptText() string {
	text := q.Header
	if q.Question != "" {
		text += ": " + q.Question
	}
	if len(q.Choices) > 0 {
		text += " [" + strings.Join(q.Choices, " | ") + "]"
	}
	if utf8.RuneCountInString(text) > askUserQuestionPromptMax {
		text = string([]rune(text)[:askUserQuestionPromptMax-1]) + "…"
	}
	return text
}

// requestID is the stable ID of the dialog: the same on every look while it
// stays open, whichever option is focused.
func (q *parsedQuestion) requestID() string {
	h := sha256.Sum256([]byte(q.Header + "\x00" + q.Question + "\x00" + strings.Join(q.Choices, "\x00")))
	return fmt.Sprintf("tmux-question-%x", h[:8])
}
