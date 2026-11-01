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
// Three later screens bend those anchors. Once a question is answered (or a
// multiSelect option ticked) its tab is marked ☒, so the header row can lead
// with ☒. A multiSelect dialog's cursor can rest on its unnumbered Submit row.
// And a multi-question dialog closes on a review screen with no select footer,
// whose last rows are fixed:
//
//	←  ☒ Approach  ☒ Scope  ✔ Submit  →
//	Review your answers
//	 ● Which approach?
//	   → Small
//	Ready to submit your answers?
//	❯ 1. Submit answers
//	  2. Cancel
//
// Enter there submits every answer, so it is as live as a question screen.
//
// The matcher is checked against live frames of a single-select dialog
// (Claude Code 2.1.280 and 2.1.314), and of a two-question dialog's tabs and
// review screen and a multiSelect dialog (2.1.314; testdata/claude-question-menu).
// A status row under the footer is not verified against a live capture. A form
// the matcher misses reads as no question and is delivered to as it was before
// this guard existed.

var (
	// askUserQuestionFooterRe matches the select footer on its trimmed row.
	askUserQuestionFooterRe = regexp.MustCompile(`^Enter to select\b.*Esc to cancel$`)

	// askUserQuestionHeaderRe matches the header row, its first tab marked ☐,
	// or ☒ once answered. A multi-question dialog leads it with the tab arrow
	// ("← ☒ Approach  ☐ Scope  ✔ Submit →").
	askUserQuestionHeaderRe = regexp.MustCompile(`^\s*(?:←\s*)?[☐☒]\s+(.+)$`)

	// askUserQuestionOptionRe matches a numbered option row; the focused one
	// carries the cursor glyph.
	askUserQuestionOptionRe = regexp.MustCompile(`^(❯\s+)?(\d+)\.\s*(.*)$`)

	// askUserQuestionCursorRe matches the focused unnumbered row below the
	// options (a multiSelect dialog's Submit).
	askUserQuestionCursorRe = regexp.MustCompile(`^❯\s+(.+)$`)

	// askUserQuestionReviewTail matches the review screen's last non-blank
	// rows, bottom first, on their trimmed rows.
	askUserQuestionReviewTail = []*regexp.Regexp{
		regexp.MustCompile(`^(?:❯\s+)?2\.\s*Cancel$`),
		regexp.MustCompile(`^(?:❯\s+)?1\.\s*Submit answers$`),
		regexp.MustCompile(`^Ready to submit your answers\?$`),
	}
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
	footer := askUserQuestionFooter(lines)
	if footer < 0 {
		footer = askUserQuestionReviewEnd(lines)
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
		if len(q.Choices) > 0 {
			if m := askUserQuestionCursorRe.FindStringSubmatch(trimmed); m != nil {
				focused = true
				q.Focused = strings.TrimSpace(m[1])
			}
			continue
		}
		if trimmed != "" {
			question = append(question, strings.TrimSpace(strings.TrimPrefix(trimmed, "│")))
		}
	}
	if !focused {
		return nil
	}
	q.Question = strings.Join(question, " ")
	return q
}

// askUserQuestionFooter walks up from the bottom to the select footer,
// allowing only the dialog's own trailing rows below it, and returns its row,
// or -1.
func askUserQuestionFooter(lines []string) int {
	seen := 0
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if askUserQuestionFooterRe.MatchString(trimmed) {
			return i
		}
		seen++
		if seen >= askUserQuestionTailSpan || strings.HasPrefix(trimmed, "❯") || strings.HasPrefix(trimmed, ">") {
			return -1
		}
	}
	return -1
}

// askUserQuestionReviewEnd returns the row just below a review screen whose
// fixed rows end the pane, or -1. Like the select footer, they may carry the
// dialog's own trailing row (a named session's rule, "──── name ─"), but no
// more: an answered review stays in scrollback above the agent's output or
// the composer.
func askUserQuestionReviewEnd(lines []string) int {
	end, matched, seen := -1, 0, 0
	for i := len(lines) - 1; i >= 0 && matched < len(askUserQuestionReviewTail); i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if askUserQuestionReviewTail[matched].MatchString(trimmed) {
			if end < 0 {
				end = i + 1
			}
			matched++
			continue
		}
		seen++
		if matched > 0 || seen >= askUserQuestionTailSpan || strings.HasPrefix(trimmed, "❯") || strings.HasPrefix(trimmed, ">") {
			return -1
		}
	}
	if matched < len(askUserQuestionReviewTail) {
		return -1
	}
	return end
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
