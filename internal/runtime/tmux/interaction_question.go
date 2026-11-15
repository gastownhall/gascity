package tmux

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ---------------------------------------------------------------------------
// Pane-based AskUserQuestion detection
// ---------------------------------------------------------------------------
//
// Claude Code's AskUserQuestion tool opens a dialog that owns the pane's input:
// a submitted Enter selects the focused option, so any typed nudge answers it.
// As of Claude Code 2.1.304 its question page looks like this:
//
//	 ←  ☐ Scope  ☐ Approach  ☐ Timing  ✔ Submit  →
//	Which scope should the change cover?
//	❯ 1. Small
//	     Only the bug fix
//	  4. Type something.
//	 5. Chat about this
//	Enter to select · Tab/Arrow keys to navigate · Esc to cancel
//
// The live dialog is keyed by its select footer alone, as the pane's
// bottom-most block: focus in the "Type something." row draws no ❯ cursor,
// an answered question's chip reads ☒ instead of ☐, and neither the header
// row nor the question text is stable across releases. The rows above the
// footer are display fields only. A wrapped-around questionnaire shows a
// second shape first, the review page, which has no footer:
//
//	 ←  ☒ Q1  ☒ Q2  ☒ Q3  ☒ Q4  ✔ Submit  →
//	Review your answers
//	 …
//	Ready to submit your answers?
//	❯ 1. Submit answers
//	2. Cancel
//
// Its key is its own bottom-most block: the two-option "Submit answers /
// Cancel" menu under "Ready to submit your answers?", blank rows between them
// allowed, anchored higher up by "Review your answers" or the ✔ Submit tab
// strip. One Enter on this page submits every answer.
//
// An option's number key answers a single-select page, but not two other
// forms of it, which are answered in the terminal (TerminalOnly). A
// multi-select page draws a checkbox before each label ("❯ 1. [ ] Cheese",
// "[✔]" once ticked; testdata/claude-question/question-multiselect-*.txt) and
// the key ticks that option in place. A page whose options carry previews
// adds "n to add notes" to its footer, and there the key only moves the focus.
//
// The dialog is live only while the footer is the last thing on screen. Once
// it is answered the frame stays in scrollback with the agent's output (or
// the idle composer) beneath it, so a footer with anything below it is
// history. No dialog gc handles today ends in this footer — resume and the
// spend-limit modal end in "Enter to confirm …", the Codex model-switch
// modal in "Press enter to confirm or esc to go back", and the session-limit
// chooser and the feedback dialog in their options.
//
// The matcher is verified against the captures under testdata/claude-question
// (see TestParseQuestionDialog_Fixtures for each one's provenance). A form it
// misses reads as no question and is delivered to as it was before this guard
// existed.

var (
	// askUserQuestionFooterRe matches the select footer on its trimmed row, or
	// on the single-spaced join of the rows a narrow pane wrapped it across.
	askUserQuestionFooterRe = regexp.MustCompile(`^Enter to select\b.*\bEsc to cancel$`)

	// askUserQuestionHeaderRe matches the tab-chip header row. A
	// multi-question dialog leads it with the tab arrow ("← ☐ Approach …
	// ✔ Submit →") and an answered question's chip reads ☒. Display only:
	// the header never decides whether a dialog is live.
	askUserQuestionHeaderRe = regexp.MustCompile(`^\s*(?:←\s*)?[☒☐]\s+(.+)$`)

	// askUserQuestionOptionRe matches a numbered option row; the focused one
	// carries the cursor glyph. Focus in the "Type something." row draws no
	// cursor, so the glyph is a display field too.
	askUserQuestionOptionRe = regexp.MustCompile(`^(❯\s+)?(\d+)\.\s*(.*)$`)

	// askUserQuestionCheckboxRe matches the checkbox a multi-select page
	// draws before an option's label: "[ ]", or "[✔]" once ticked.
	askUserQuestionCheckboxRe = regexp.MustCompile(`^\[.\](?:\s+|$)`)
)

// askUserQuestionPreviewHint is the hint the select footer carries when the
// page's options show previews, which a number key only focuses.
const askUserQuestionPreviewHint = "to add notes"

// Why a number key does not answer a question page (TerminalOnly).
const (
	questionMultiSelect  = "a multi-select page, where an option's key ticks it in place"
	questionWithPreviews = "a page whose options show previews, where an option's key only focuses it"
)

const (
	// askUserQuestionFooterWrapRows is how many rows from the pane's bottom
	// the select footer may span: its own row plus the adjacent rows a
	// narrow pane wraps it onto, joined to match askUserQuestionFooterRe.
	askUserQuestionFooterWrapRows = 3

	// questionDialogMaxRows bounds how far above the footer the question
	// page's header row may sit.
	questionDialogMaxRows = 60

	// askUserQuestionPromptMax bounds, in runes, the text a client is shown
	// for a question: the prompt Pending reports and the description a
	// refusal carries.
	askUserQuestionPromptMax = 400
)

// parsedQuestion is the AskUserQuestion dialog a pane shows.
type parsedQuestion struct {
	Header   string
	Question string
	// Choices are the numbered options' labels in pane order, without a
	// multi-select page's checkboxes.
	Choices []string
	// Numbers are the options' numbers, parallel to Choices: the key that
	// selects each.
	Numbers []int
	// Focused is the label of the highlighted option, empty when the dialog
	// draws no cursor.
	Focused string
	// Summary is a review page's rows above its prompt, top first: each
	// question and the answer it got. Every review page asks the same
	// thing, so these rows are what tell one questionnaire's apart.
	Summary []string
	// TerminalOnly, when set, says why an option's number key does not
	// answer the page: it is answered in the terminal instead.
	TerminalOnly string
}

// parseQuestionDialog returns the open AskUserQuestion dialog the pane lines
// show — its question page or its review page — or nil when they show none.
func parseQuestionDialog(lines []string) *parsedQuestion {
	if q := parseQuestionPage(lines); q != nil {
		return q
	}
	return parseReviewPage(lines)
}

// parseQuestionPage returns the question dialog whose select footer is the
// pane's bottom-most block, or nil. The footer is the key; everything above it
// is display.
func parseQuestionPage(lines []string) *parsedQuestion {
	block := trimTrailingBlankRows(lines)
	footer := questionFooterRow(block)
	if footer < 0 {
		return nil
	}

	q := &parsedQuestion{}
	if strings.Contains(joinTrimmedRows(block[footer:]), askUserQuestionPreviewHint) {
		q.TerminalOnly = questionWithPreviews
	}
	header := -1
	for i := footer - 1; i >= 0 && footer-i <= questionDialogMaxRows; i-- {
		if askUserQuestionHeaderRe.MatchString(block[i]) {
			header = i
			q.Header = strings.TrimSpace(askUserQuestionHeaderRe.FindStringSubmatch(block[i])[1])
			break
		}
	}
	// Without a header row the dialog's own rows are the non-blank block the
	// footer caps; with one they sit between it and the footer.
	top := footer
	if header >= 0 {
		top = header + 1
	} else {
		for top > 0 && strings.TrimSpace(block[top-1]) != "" && footer-(top-1) <= questionDialogMaxRows {
			top--
		}
	}
	var question []string
	for _, line := range block[top:footer] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if m := askUserQuestionOptionRe.FindStringSubmatch(trimmed); m != nil {
			label := strings.TrimSpace(m[3])
			// The checkbox is the option's state, not its label: ticking it
			// leaves the page, and so its request ID, the same.
			if box := askUserQuestionCheckboxRe.FindString(label); box != "" {
				label = label[len(box):]
				q.TerminalOnly = questionMultiSelect
			}
			number, _ := strconv.Atoi(m[2])
			q.Choices = append(q.Choices, label)
			q.Numbers = append(q.Numbers, number)
			if m[1] != "" {
				q.Focused = label
			}
			continue
		}
		if len(q.Choices) == 0 {
			question = append(question, strings.TrimSpace(strings.TrimPrefix(trimmed, "│")))
		}
	}
	q.Question = strings.Join(question, " ")
	return q
}

// questionFooterRow returns the index in block, which ends in a non-blank
// row, of the row the select footer starts on, or -1. The footer must own the
// pane's bottom: its rows are block's last ones, so a footer with a composer
// or output standing below it does not match, however it is wrapped. A wrap
// puts the footer on adjacent rows, so a blank row ends it.
func questionFooterRow(block []string) int {
	top := len(block)
	for top > 0 && len(block)-top < askUserQuestionFooterWrapRows && strings.TrimSpace(block[top-1]) != "" {
		top--
	}
	// Bottom-most candidate first: the footer's own row, then any row it
	// wrapped onto above it.
	for start := len(block) - 1; start >= top; start-- {
		if !strings.HasPrefix(strings.TrimSpace(block[start]), "Enter to select") {
			continue
		}
		if askUserQuestionFooterRe.MatchString(joinTrimmedRows(block[start:])) {
			return start
		}
	}
	return -1
}

// joinTrimmedRows joins rows, each trimmed, with single spaces: the text of a
// line the pane wrapped across them.
func joinTrimmedRows(rows []string) string {
	trimmed := make([]string, 0, len(rows))
	for _, row := range rows {
		trimmed = append(trimmed, strings.TrimSpace(row))
	}
	return strings.Join(trimmed, " ")
}

// trimTrailingBlankRows drops the blank rows a capture ends with, so the
// dialog is matched as the pane's bottom-most block.
func trimTrailingBlankRows(lines []string) []string {
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[:end]
}

// lastNonBlankRows returns the indexes of the last n non-blank rows of lines,
// top first, or of as many as lines holds.
func lastNonBlankRows(lines []string, n int) []int {
	rows := make([]int, n)
	k := n
	for i := len(lines) - 1; i >= 0 && k > 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			k--
			rows[k] = i
		}
	}
	return rows[k:]
}

// parseReviewPage returns the dialog's review page — the pane's bottom-most
// non-blank rows are the "Submit answers / Cancel" menu under "Ready to submit
// your answers?" — or nil. Blank rows may stand between the three, as they do
// between a question page's question and its options. One Enter on this page
// submits every answer.
func parseReviewPage(lines []string) *parsedQuestion {
	rows := lastNonBlankRows(lines, 3)
	if len(rows) < 3 || strings.TrimSpace(lines[rows[0]]) != "Ready to submit your answers?" {
		return nil
	}
	prompt := rows[0]
	q := &parsedQuestion{Question: "Ready to submit your answers?"}
	labels := []string{"Submit answers", "Cancel"}
	for k, row := range rows[1:] {
		m := askUserQuestionOptionRe.FindStringSubmatch(strings.TrimSpace(lines[row]))
		if m == nil || m[2] != fmt.Sprint(k+1) || strings.TrimSpace(m[3]) != labels[k] {
			return nil
		}
		q.Choices = append(q.Choices, labels[k])
		q.Numbers = append(q.Numbers, k+1)
		if m[1] != "" {
			q.Focused = labels[k]
		}
	}
	// The menu alone could be quoted history at the pane's bottom; the page
	// is live only with its anchor above: the "Review your answers" heading
	// or the ✔ Submit tab strip with its ☐/☒ chips. The rows between the
	// anchor and the prompt are the answers under review.
	anchored := false
	for i := prompt - 1; i >= 0 && prompt-i <= questionDialogMaxRows; i-- {
		row := lines[i]
		if askUserQuestionHeaderRe.MatchString(row) && q.Header == "" {
			q.Header = strings.TrimSpace(askUserQuestionHeaderRe.FindStringSubmatch(row)[1])
		}
		if strings.Contains(row, "Review your answers") ||
			(strings.Contains(row, "✔ Submit") && (strings.Contains(row, "☐") || strings.Contains(row, "☒"))) {
			anchored = true
			break
		}
		if trimmed := strings.TrimSpace(row); trimmed != "" {
			q.Summary = append(q.Summary, trimmed)
		}
	}
	if !anchored {
		return nil
	}
	slices.Reverse(q.Summary)
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
	return boundPromptText(text)
}

// boundPromptText cuts text a client is shown for a question to
// askUserQuestionPromptMax runes, ending a cut one in an ellipsis.
func boundPromptText(text string) string {
	if utf8.RuneCountInString(text) <= askUserQuestionPromptMax {
		return text
	}
	return string([]rune(text)[:askUserQuestionPromptMax-1]) + "…"
}

// requestID is the stable ID of the page on screen: the same on every look
// while the page stays open, and different on the next one. It covers the
// question, the choices and a review page's Summary, never the header's ☐/☒
// chips, a multi-select page's checkboxes or the focused option, which change
// while the dialog is the same page: a multiSelect toggle ticks a checkbox
// and flips a chip, arrows move the cursor.
func (q *parsedQuestion) requestID() string {
	text := q.Question + "\x00" + strings.Join(q.Choices, "\x00")
	if len(q.Summary) > 0 {
		text += "\x01" + strings.Join(q.Summary, "\x00")
	}
	h := sha256.Sum256([]byte(text))
	return fmt.Sprintf("tmux-question-%x", h[:8])
}

// leftFor reports whether next, the question dialog the pane shows after an
// option of page q was typed (nil for none), is past q: no dialog, or a page
// with another question, such as the review page. A dialog's questions are
// unique, and no redraw of a page changes its question, as one can change its
// option rows (a ticked checkbox, a re-wrapped label).
func (q *parsedQuestion) leftFor(next *parsedQuestion) bool {
	return next == nil || next.Question != q.Question
}

// QuestionSelectAction is the InteractionResponse action that answers a
// question page: its Text names the option to pick, by its label as
// Pending's Options list it or, when no option carries that label, by its
// number.
const QuestionSelectAction = "select"

// questionMetaChoices are the rows a question page offers that answer
// nothing: they open a free-text field or a chat in the terminal, so
// Respond does not pick them and Pending does not offer them.
var questionMetaChoices = map[string]bool{
	"Type something.": true,
	"Chat about this": true,
}

// answerChoices are the labels of the options Respond may pick, in pane
// order: every choice but questionMetaChoices, and none on a TerminalOnly
// page.
func (q *parsedQuestion) answerChoices() []string {
	if q.TerminalOnly != "" {
		return nil
	}
	out := make([]string, 0, len(q.Choices))
	for _, c := range q.Choices {
		if !questionMetaChoices[c] {
			out = append(out, c)
		}
	}
	return out
}

// answerHint says how the page is answered, for a refusal to type at it.
func (q *parsedQuestion) answerHint(session string) string {
	if q.TerminalOnly != "" {
		return fmt.Sprintf("answer it in the terminal (gc session attach %s), not by a message: it is %s", session, q.TerminalOnly)
	}
	return fmt.Sprintf("answer it with respond (action %q naming an option) or in the terminal (gc session attach %s), not by a message", QuestionSelectAction, session)
}

// questionOptionKey returns the key that picks the option response names on
// q: action QuestionSelectAction, its Text one of answerChoices by label
// (case and surrounding space ignored) or, when it is no option's label, by
// number. A TerminalOnly page, another action, a text naming no offered
// option or more than one, and an option no single key selects are refused
// with runtime.ErrInteractionActionUnavailable.
func questionOptionKey(q *parsedQuestion, response runtime.InteractionResponse) (string, error) {
	if q.TerminalOnly != "" {
		return "", fmt.Errorf("%w: the question dialog (%s) is %s; answer it in the terminal",
			runtime.ErrInteractionActionUnavailable, q.promptText(), q.TerminalOnly)
	}
	offered := strings.Join(q.answerChoices(), " | ")
	if response.Action != QuestionSelectAction {
		return "", fmt.Errorf("%w: a question dialog (%s) is answered with action %q naming one of its options [%s], not %q",
			runtime.ErrInteractionActionUnavailable, q.promptText(), QuestionSelectAction, offered, response.Action)
	}
	want := strings.TrimSpace(response.Text)
	// A label names its option even where it reads as another option's
	// number (a label "1" on option 2).
	byLabel, byNumber := -1, -1
	for i, label := range q.Choices {
		if questionMetaChoices[label] || i >= len(q.Numbers) {
			continue
		}
		if strings.EqualFold(label, want) {
			if byLabel >= 0 && q.Numbers[byLabel] != q.Numbers[i] {
				return "", fmt.Errorf("%w: %q names more than one option of the question dialog (%s); select it by its number",
					runtime.ErrInteractionActionUnavailable, want, q.promptText())
			}
			byLabel = i
		}
		if byNumber < 0 && strconv.Itoa(q.Numbers[i]) == want {
			byNumber = i
		}
	}
	pick := byLabel
	if pick < 0 {
		pick = byNumber
	}
	if pick < 0 {
		return "", fmt.Errorf("%w: the question dialog (%s) offers no option %q to select; its options are [%s] (free text and chat are answered in the terminal)",
			runtime.ErrInteractionActionUnavailable, q.promptText(), want, offered)
	}
	if n := q.Numbers[pick]; n < 1 || n > 9 {
		return "", fmt.Errorf("%w: option %d (%s) cannot be selected with one key; answer it in the terminal",
			runtime.ErrInteractionActionUnavailable, n, q.Choices[pick])
	}
	return strconv.Itoa(q.Numbers[pick]), nil
}
