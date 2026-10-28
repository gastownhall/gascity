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
// As of Claude Code 2.1.280 a question page looks like this:
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
// A multi-question dialog leads the header with a tab strip whose chips turn
// from ☐ to ☒ as questions are answered ("←  ☒ Q1  ☐ Q2  ✔ Submit  →") and
// navigates with "Tab/Arrow keys".
//
// A dialog is live only while it is the bottom-most block on screen. Once it is
// answered the frame stays in scrollback with the agent's output (or the idle
// composer) beneath it, so a dialog with anything below it is history.
//
// A question page is keyed on its select footer (which a narrow pane may wrap
// onto a few rows), a ☐/☒ header row above it, and at least one numbered
// option between them. The focused option is not required: focus on the
// "Type something." row draws no cursor. The folder-trust, resume and
// spend-limit dialogs end in "Enter to confirm" and the Codex model switch in
// "Press enter to confirm", so none of them reads as a question.
//
// The question page is checked against a live single-question frame (Claude
// Code 2.1.280) and a live multi-question dialog's pages (ga-ngrme8). A form
// the matcher misses reads as no question and is delivered to as it was before
// this guard existed.

var (
	// askUserQuestionFooterRe matches the select footer, its wrapped rows
	// joined, trimmed.
	askUserQuestionFooterRe = regexp.MustCompile(`^Enter to select\b.*Esc to cancel$`)

	// askUserQuestionHeaderRe matches the header row: a ☐ (open) or ☒
	// (answered) chip, led by the tab arrow in a multi-question dialog.
	askUserQuestionHeaderRe = regexp.MustCompile(`^\s*(?:←\s*)?[☐☒]\s+(.+)$`)

	// askUserQuestionOptionRe matches a numbered option row; the focused one
	// carries the cursor glyph.
	askUserQuestionOptionRe = regexp.MustCompile(`^(❯\s+)?(\d+)\.\s*(.*)$`)
)

const (
	// askUserQuestionTailSpan is how many non-blank rows from the bottom, the
	// footer's own last row included, the footer may end within. A row that
	// starts with a prompt glyph below it is the composer, so the dialog is
	// history.
	askUserQuestionTailSpan = 2

	// askUserQuestionFooterRows is how many rows a narrow pane may wrap the
	// select footer onto.
	askUserQuestionFooterRows = 3

	// askUserQuestionMaxRows bounds how far above the footer the header may
	// be.
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
	// Focused is the label of the highlighted option; empty when no option
	// row draws the cursor.
	Focused string
}

// parseAskUserQuestion returns the open AskUserQuestion page the pane lines
// show, or nil when they show none.
func parseAskUserQuestion(lines []string) *parsedQuestion {
	footer := askUserQuestionFooterRow(lines)
	if footer < 0 {
		return nil
	}
	return parseAskUserQuestionPage(lines, footer)
}

// bottomRows returns the indexes of the last n non-blank lines, bottom first.
func bottomRows(lines []string, n int) []int {
	rows := make([]int, 0, n)
	for i := len(lines) - 1; i >= 0 && len(rows) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			rows = append(rows, i)
		}
	}
	return rows
}

// askUserQuestionFooterRow returns the index of the select footer's first row
// when the footer ends the pane's bottom block, or -1.
func askUserQuestionFooterRow(lines []string) int {
	rows := bottomRows(lines, askUserQuestionTailSpan+askUserQuestionFooterRows-1)
	for skip := 0; skip < askUserQuestionTailSpan && skip < len(rows); skip++ {
		if skip > 0 {
			below := strings.TrimSpace(lines[rows[skip-1]])
			if strings.HasPrefix(below, "❯") || strings.HasPrefix(below, ">") {
				return -1
			}
		}
		joined := ""
		for n := 0; n < askUserQuestionFooterRows && skip+n < len(rows); n++ {
			row := strings.TrimSpace(lines[rows[skip+n]])
			if joined == "" {
				joined = row
			} else {
				joined = row + " " + joined
			}
			if strings.HasPrefix(row, "Enter to select") && askUserQuestionFooterRe.MatchString(joined) {
				return rows[skip+n]
			}
		}
	}
	return -1
}

// askUserQuestionHeaderAbove returns the index of the nearest header row
// within askUserQuestionMaxRows above row, or -1.
func askUserQuestionHeaderAbove(lines []string, row int) int {
	for i := row - 1; i >= 0 && row-i <= askUserQuestionMaxRows; i-- {
		if askUserQuestionHeaderRe.MatchString(lines[i]) {
			return i
		}
	}
	return -1
}

// parseAskUserQuestionPage reads the question page whose footer starts at
// row footer. It needs a header above the footer and a numbered option between
// them.
func parseAskUserQuestionPage(lines []string, footer int) *parsedQuestion {
	header := askUserQuestionHeaderAbove(lines, footer)
	if header < 0 {
		return nil
	}

	q := &parsedQuestion{Header: strings.TrimSpace(askUserQuestionHeaderRe.FindStringSubmatch(lines[header])[1])}
	var question []string
	for _, line := range lines[header+1 : footer] {
		trimmed := strings.TrimSpace(line)
		if m := askUserQuestionOptionRe.FindStringSubmatch(trimmed); m != nil {
			label := strings.TrimSpace(m[3])
			q.Choices = append(q.Choices, label)
			if m[1] != "" {
				q.Focused = label
			}
			continue
		}
		if len(q.Choices) == 0 && trimmed != "" {
			question = append(question, strings.TrimSpace(strings.TrimPrefix(trimmed, "│")))
		}
	}
	if len(q.Choices) == 0 {
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

// requestID is the stable ID of the dialog page: the same on every look while
// the page stays open, whichever option is focused, and new on the next page.
// The header is left out: its chips change as questions are answered.
func (q *parsedQuestion) requestID() string {
	h := sha256.Sum256([]byte(q.Question + "\x00" + strings.Join(q.Choices, "\x00")))
	return fmt.Sprintf("tmux-question-%x", h[:8])
}
