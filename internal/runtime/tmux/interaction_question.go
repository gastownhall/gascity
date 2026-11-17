package tmux

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
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
// forms of it, which Respond's select leaves to the terminal (TerminalOnly). A
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
	askUserQuestionCursorRe   = regexp.MustCompile(`^❯\s+(.+)$`)
	askUserQuestionReviewTail = []*regexp.Regexp{
		regexp.MustCompile(`^(?:❯\s+)?2\.\s*Cancel$`),
		regexp.MustCompile(`^(?:❯\s+)?1\.\s*Submit answers$`),
		regexp.MustCompile(`^Ready to submit your answers\?$`),
	}
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
	askUserQuestionTailSpan = 2
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
	// ReviewText retains the full rendered review used for native submission
	// proof separately from the question shown to a client.
	ReviewText string
	// Review marks a recognized review page even when only the broad
	// read-only matcher recognizes its layout.
	Review bool
	// Choices are the numbered options' labels in pane order. A page read
	// for a client with no native request (parseQuestionDialog) carries them
	// without a multi-select page's checkboxes; parseAskUserQuestion keeps
	// the rows as drawn for native binding.
	Choices []string
	// Focused is the label of the highlighted option, empty when the dialog
	// draws no cursor.
	Focused string
	// Numbers are the options' numbers, parallel to Choices: the key that
	// selects each.
	Numbers []int
	// Summary is a review page's rows above its prompt, top first: each
	// question and the answer it got. Every review page asks the same
	// thing, so these rows are what tell one questionnaire's apart.
	Summary []string
	// TerminalOnly, when set, says why an option's number key does not
	// answer the page: Respond's select leaves it to the terminal.
	TerminalOnly string
	Provider     string
	NativeID     string
	// CodexPosition and CodexTotal retain the original async request's batch
	// position, even after an earlier question in that batch is answered.
	CodexPosition  int
	CodexTotal     int
	BindingError   string
	FreeNumber     int
	ChatNumber     int
	Draft          string
	FocusedNumber  int
	Collapsed      bool
	MultiSelect    bool
	CheckedChoices map[int]bool
	ClaudeForm     *claudeQuestionForm
}

// parseAskUserQuestion returns the open AskUserQuestion dialog the pane lines
// show, or nil when they show none.
func parseAskUserQuestion(lines []string) *parsedQuestion {
	footer := askUserQuestionFooter(lines)
	review := false
	if footer < 0 {
		footer = askUserQuestionReviewEnd(lines)
		review = footer >= 0
	}
	if footer < 0 {
		return nil
	}

	header := -1
	for i := footer - 1; i >= 0 && footer-i <= questionDialogMaxRows; i-- {
		if askUserQuestionHeaderRe.MatchString(lines[i]) {
			header = i
			break
		}
	}
	if header < 0 {
		return nil
	}

	q := &parsedQuestion{Header: strings.TrimSpace(askUserQuestionHeaderRe.FindStringSubmatch(lines[header])[1])}
	if !review && strings.Contains(joinTrimmedRows(lines[footer:]), askUserQuestionPreviewHint) {
		q.TerminalOnly = questionWithPreviews
	}
	var question []string
	var continuations [][]string
	focused := false
	for _, line := range lines[header+1 : footer] {
		trimmed := strings.TrimSpace(line)
		if m := askUserQuestionOptionRe.FindStringSubmatch(trimmed); m != nil {
			label := strings.TrimSpace(m[3])
			if q.FreeNumber > 0 && label == "Chat about this" {
				q.ChatNumber, _ = strconv.Atoi(m[2])
			}
			// The rows stay as drawn: native binding reads the checkbox
			// state from them (claudeMultipleChoiceState).
			if askUserQuestionCheckboxRe.MatchString(label) {
				q.TerminalOnly = questionMultiSelect
			}
			q.Choices = append(q.Choices, label)
			continuations = append(continuations, nil)
			number, _ := strconv.Atoi(m[2])
			q.Numbers = append(q.Numbers, number)
			if m[1] != "" {
				focused = true
				q.Focused = label
				q.FocusedNumber = number
			}
			continue
		}
		if len(q.Choices) > 0 {
			if m := askUserQuestionCursorRe.FindStringSubmatch(trimmed); m != nil {
				focused = true
				q.Focused = strings.TrimSpace(m[1])
				q.FocusedNumber = 0
				continue
			}
		}
		if strings.HasPrefix(trimmed, "──") && len(q.Choices) > 0 {
			last := len(q.Choices) - 1
			q.FreeNumber = q.Numbers[last]
			if q.Choices[last] != "Type something." {
				q.Draft = strings.Join(append([]string{q.Choices[last]}, continuations[last]...), "\n")
			}
			q.Choices[last] = "Type something."
		}
		if len(q.Choices) > 0 && q.FreeNumber == 0 && trimmed != "" && !strings.HasPrefix(trimmed, "──") {
			last := len(continuations) - 1
			continuations[last] = append(continuations[last], trimmed)
		}
		if len(q.Choices) == 0 && trimmed != "" {
			question = append(question, strings.TrimSpace(strings.TrimPrefix(trimmed, "│")))
		}
	}
	if !focused {
		return nil
	}
	q.Question = strings.Join(question, " ")
	if review {
		q.Review = true
		q.ReviewText = q.Question
		q.Question = "Ready to submit your answers?"
		if page := parseReviewPage(lines); page != nil {
			q.Summary = page.Summary
		} else {
			for _, row := range question {
				if row != "Review your answers" && row != q.Question {
					q.Summary = append(q.Summary, row)
				}
			}
		}
	}
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

// parseQuestionDialog returns the open AskUserQuestion dialog the pane lines
// show — its question page or its review page — or nil when they show none,
// as a client with no native request reads it (presentUnbound).
func parseQuestionDialog(lines []string) *parsedQuestion {
	q := parseNativeQuestionDialog(lines)
	if q != nil {
		q.presentUnbound()
	}
	return q
}

// parseNativeQuestionDialog is parseQuestionDialog with a page
// parseAskUserQuestion recognizes left as drawn, for native binding
// (bindClaudeQuestion), which reads a multi-select page's checkbox rows.
func parseNativeQuestionDialog(lines []string) *parsedQuestion {
	if q := parseAskUserQuestion(lines); q != nil {
		return q
	}
	if q := parseQuestionPage(lines); q != nil {
		return q
	}
	return parseReviewPage(lines)
}

// presentUnbound puts a page parseAskUserQuestion read in the form
// parseQuestionPage reads one, which is what a client with no native request
// is shown and what the page's request ID covers: labels without a
// multi-select page's checkboxes (the checkbox is the option's state, not its
// label, so ticking it leaves the page, and its request ID, the same), and no
// focus on an unnumbered row.
func (q *parsedQuestion) presentUnbound() {
	for i, label := range q.Choices {
		if box := askUserQuestionCheckboxRe.FindString(label); box != "" {
			q.Choices[i] = label[len(box):]
		}
	}
	if q.FreeNumber > 0 {
		// A multi-select page's free-text row carries a checkbox too; its
		// label is the row's own, as drawn.
		first, _, _ := strings.Cut(q.Draft, "\n")
		if box := askUserQuestionCheckboxRe.FindString(first); box != "" {
			if i := slices.Index(q.Numbers, q.FreeNumber); i >= 0 {
				q.Choices[i] = first[len(box):]
			}
		}
	}
	if q.FocusedNumber == 0 {
		q.Focused = ""
	} else if box := askUserQuestionCheckboxRe.FindString(q.Focused); box != "" {
		q.Focused = q.Focused[len(box):]
	}
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
				q.FocusedNumber = number
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
	q := &parsedQuestion{Question: "Ready to submit your answers?", Review: true}
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
			q.FocusedNumber = k + 1
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
// while the page stays open, and different on the next one. A page bound to
// its native request is named by that request (NativeID). Any other covers the
// question, the choices and a review page's Summary, never the header's ☐/☒
// chips, a multi-select page's checkboxes or the focused option, which change
// while the dialog is the same page: a multiSelect toggle ticks a checkbox
// and flips a chip, arrows move the cursor.
func (q *parsedQuestion) requestID() string {
	if q.NativeID != "" {
		return q.NativeID
	}
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
// option rows (a ticked checkbox, a re-wrapped label). The questions are
// compared with their white space folded: a page bound to its native request
// carries the authored question, a page read off the pane its wrapped rows.
func (q *parsedQuestion) leftFor(next *parsedQuestion) bool {
	return next == nil || strings.Join(strings.Fields(next.Question), " ") != strings.Join(strings.Fields(q.Question), " ")
}

// QuestionSelectAction is the InteractionResponse action that answers a
// question page by one of its options: its Text names the option to pick, by
// its label as Pending's Options list it or, when no option carries that
// label, by its number. A page bound to its native request also takes the
// request-bound "answer" action (respondQuestionAnswer).
const QuestionSelectAction = "select"

// questionMetaChoices are the rows a question page offers that a select
// answers nothing with: they open a free-text field or a chat in the terminal,
// so Respond's select does not pick them and Pending does not offer them for
// it.
var questionMetaChoices = map[string]bool{
	"Type something.": true,
	"Chat about this": true,
}

// selectChoices are the labels of the options Respond's select may pick, in
// pane order: every choice but questionMetaChoices, and none on a
// TerminalOnly page or a Codex question, which only its request-bound answer
// reaches.
func (q *parsedQuestion) selectChoices() []string {
	if q.TerminalOnly != "" || q.Provider == "codex" {
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
	switch {
	case q.NativeID != "":
		return fmt.Sprintf("answer its request with respond (action answer) or in the terminal (gc session attach %s), not by a message", session)
	case q.TerminalOnly != "":
		return fmt.Sprintf("answer it in the terminal (gc session attach %s), not by a message: it is %s", session, q.TerminalOnly)
	case q.Provider == "codex":
		return fmt.Sprintf("it is answered in the terminal (gc session attach %s), not by a message", session)
	}
	return fmt.Sprintf("answer it with respond (action %q naming an option) or in the terminal (gc session attach %s), not by a message", QuestionSelectAction, session)
}

// questionOptionKey returns the key that picks the option response names on
// q: action QuestionSelectAction, its Text one of selectChoices by label
// (case and surrounding space ignored) or, when it is no option's label, by
// number. A Codex question, a TerminalOnly page, another action, a text
// naming no offered option or more than one, and an option no single key
// selects are refused with runtime.ErrInteractionActionUnavailable.
func questionOptionKey(q *parsedQuestion, response runtime.InteractionResponse) (string, error) {
	if q.Provider == "codex" {
		return "", fmt.Errorf("%w: a native Codex question (%s) is answered with action \"answer\" naming its request, or in the terminal",
			runtime.ErrInteractionActionUnavailable, q.promptText())
	}
	if q.TerminalOnly != "" {
		return "", fmt.Errorf("%w: the question dialog (%s) is %s; answer it in the terminal",
			runtime.ErrInteractionActionUnavailable, q.promptText(), q.TerminalOnly)
	}
	offered := strings.Join(q.selectChoices(), " | ")
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

// answerChoices excludes the provider's editor and chat controls from the
// model-authored answers shown to a client.
func (q *parsedQuestion) isChoice(index int) bool {
	return q.Numbers[index] > 0 && q.Numbers[index] != q.textOption() && q.Numbers[index] != q.ChatNumber
}

func (q *parsedQuestion) answerChoices() []string {
	var choices []string
	for i, label := range q.Choices {
		if q.isChoice(i) {
			choices = append(choices, label)
		}
	}
	return choices
}

func (q *parsedQuestion) textOption() int {
	if q.FreeNumber != 0 {
		return q.FreeNumber
	}
	for i, label := range q.Choices {
		if label == "Type something." || (q.Provider == "codex" && label == "Other") {
			return q.Numbers[i]
		}
	}
	return 0
}

// respondQuestion answers the question page q that session name's agent pane
// target shows, by the action response names: QuestionSelectAction picks one
// of the page's options with its number key (respondQuestionSelect), and
// "answer" is the request-bound explicit answer to a page bound to its native
// request (respondQuestionAnswer). Ordinary messages and approval verbs remain
// unable to select question menu entries.
func (t *Tmux) respondQuestion(ctx context.Context, name, target string, q *parsedQuestion, response runtime.InteractionResponse) error {
	if response.Action == QuestionSelectAction {
		return t.respondQuestionSelect(ctx, name, target, q, response)
	}
	return t.respondQuestionAnswer(ctx, target, q, response)
}

// respondQuestionAnswer accepts only a request-bound explicit answer.
func (t *Tmux) respondQuestionAnswer(ctx context.Context, target string, q *parsedQuestion, response runtime.InteractionResponse) error {
	if response.Action != "answer" {
		return fmt.Errorf("%w: a question dialog (%s) is answered with action \"answer\" naming its request, or with action %q naming one of its options, not %q",
			runtime.ErrInteractionActionUnavailable, q.promptText(), QuestionSelectAction, response.Action)
	}
	if response.RequestID == "" {
		return fmt.Errorf("%w: answering a question requires request_id", runtime.ErrInteractionResponseInvalid)
	}
	if response.RequestID != q.requestID() {
		return fmt.Errorf("%w: question changed since it was read", runtime.ErrInteractionGone)
	}
	if q.NativeID == "" {
		return fmt.Errorf("%w: native question identity is unavailable; answer in the terminal", runtime.ErrInteractionActionUnavailable)
	}
	if q.MultiSelect || response.Answers != nil {
		return t.respondClaudeMultipleChoices(ctx, target, q, response)
	}
	if strings.TrimSpace(response.Text) == "" || len(response.Text) > 16384 || strings.ContainsFunc(response.Text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return fmt.Errorf("%w: answer must be nonempty text of at most 16384 bytes without control characters", runtime.ErrInteractionResponseInvalid)
	}
	if q.Provider == "codex" && q.Collapsed {
		// Only an explicit bound response opens Codex's native question panel.
		// Polling Pending and ordinary nudges remain read-only.
		if _, err := t.runCtx(inputCtx(ctx), "send-keys", "-t", target, "S-Left"); err != nil {
			return fmt.Errorf("opening native Codex question: %w", err)
		}
		opened, err := t.waitQuestionPanel(ctx, target, q)
		if err != nil {
			return err
		}
		q = opened
	}
	number := 0
	for i, label := range q.Choices {
		if label == response.Text && q.isChoice(i) {
			if number != 0 {
				return fmt.Errorf("%w: answer label is ambiguous", runtime.ErrInteractionActionUnavailable)
			}
			number = q.Numbers[i]
		}
	}
	freeText := number == 0
	if freeText {
		number = q.textOption()
		if number == 0 {
			return fmt.Errorf("%w: question has no free-text editor", runtime.ErrInteractionActionUnavailable)
		}
		if q.Draft != "" {
			return fmt.Errorf("%w: native question editor already contains a draft; finish or clear it in the terminal", runtime.ErrInteractionActionUnavailable)
		}
	}
	if number < 1 || number > 9 {
		return fmt.Errorf("%w: answer cannot be selected with one key", runtime.ErrInteractionActionUnavailable)
	}
	t.cancelCopyModeIfParked(inputCtx(ctx), target)
	if q.Provider == "codex" {
		if _, err := t.runCtx(inputCtx(ctx), "send-keys", "-t", target, "-l", strconv.Itoa(number)); err != nil {
			return fmt.Errorf("sending question answer: %w", err)
		}
	} else if err := t.focusClaudeQuestionRow(ctx, target, q, number); err != nil {
		return err
	}
	if q.Provider != "codex" || freeText {
		// Both CLIs can submit authored choices on a digit. Claude uses
		// navigation so the final native identity/focus check precedes its
		// only submit; Codex's text-editor shortcut does not submit.
		if err := t.waitQuestionFocus(ctx, target, q, number); err != nil {
			return err
		}
		if freeText {
			if err := t.pasteLiteralText(inputCtx(ctx), target, response.Text); err != nil {
				return err
			}
			if err := t.waitQuestionDraft(ctx, target, q, response.Text); err != nil {
				return err
			}
		}
		if _, err := t.runCtx(inputCtx(ctx), "send-keys", "-t", target, "Enter"); err != nil {
			return fmt.Errorf("submitting question answer: %w", err)
		}
	}
	deadline := time.Now().Add(respondVerifyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return interactionAnswerUnconfirmed(ctx.Err())
		case <-time.After(respondVerifyInterval):
		}
		next, err := t.dialogInPane(target)
		if err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				return nil
			}
			return interactionAnswerUnconfirmed(fmt.Errorf("verifying question answer: %w", err))
		}
		if next.question != nil && q.ClaudeForm != nil && isClaudeQuestionFormReview(next.question) {
			return interactionAnswerUnconfirmed(t.submitClaudeQuestionFormReview(ctx, target, q, response.Text, nil, next.question))
		}
		if next.question == nil || next.question.requestID() != q.requestID() {
			return nil
		}
	}
	return interactionAnswerUnconfirmed(fmt.Errorf("question did not clear within %s of the answer", respondVerifyTimeout))
}

// focusClaudeQuestionRow navigates without answering. Numeric shortcuts in
// Claude2.1.308 submit authored choices immediately, before a focus check can
// prove which invocation received them.
func (t *Tmux) focusClaudeQuestionRow(ctx context.Context, target string, q *parsedQuestion, number int) error {
	current, wanted := slices.Index(q.Numbers, q.FocusedNumber), slices.Index(q.Numbers, number)
	if current < 0 || wanted < 0 {
		return fmt.Errorf("%w: native question focus is unknown", runtime.ErrInteractionActionUnavailable)
	}
	for current != wanted {
		key, step := "Down", 1
		if current > wanted {
			key, step = "Up", -1
		}
		if _, err := t.runCtx(inputCtx(ctx), "send-keys", "-t", target, key); err != nil {
			return fmt.Errorf("focusing native question answer: %w", err)
		}
		current += step
		if err := t.waitQuestionFocus(ctx, target, q, q.Numbers[current]); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tmux) waitQuestionFocus(ctx context.Context, target string, q *parsedQuestion, number int) error {
	deadline := time.Now().Add(respondVerifyTimeout)
	for time.Now().Before(deadline) {
		next, err := t.dialogInPane(target)
		if err != nil {
			return fmt.Errorf("verifying question focus: %w", err)
		}
		if next.question == nil || next.question.requestID() != q.requestID() {
			return fmt.Errorf("%w: question changed before submission", runtime.ErrInteractionGone)
		}
		if next.question.FocusedNumber == number {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(respondVerifyInterval):
		}
	}
	return fmt.Errorf("question selection was not confirmed within %s; no answer was submitted", respondVerifyTimeout)
}

func (t *Tmux) waitQuestionPanel(ctx context.Context, target string, q *parsedQuestion) (*parsedQuestion, error) {
	deadline := time.Now().Add(respondVerifyTimeout)
	for time.Now().Before(deadline) {
		next, err := t.dialogInPane(target)
		if err != nil {
			return nil, fmt.Errorf("reading native question panel: %w", err)
		}
		if next.question == nil || next.question.requestID() != q.requestID() {
			return nil, fmt.Errorf("%w: another question owns the native editor", runtime.ErrInteractionGone)
		}
		if !next.question.Collapsed {
			return next.question, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(respondVerifyInterval):
		}
	}
	return nil, fmt.Errorf("native question panel did not open within %s", respondVerifyTimeout)
}

func (t *Tmux) waitQuestionDraft(ctx context.Context, target string, q *parsedQuestion, text string) error {
	deadline := time.Now().Add(respondVerifyTimeout)
	for time.Now().Before(deadline) {
		next, err := t.dialogInPane(target)
		if err != nil {
			return fmt.Errorf("verifying question draft: %w", err)
		}
		if next.question == nil || next.question.requestID() != q.requestID() {
			return fmt.Errorf("%w: question changed before the draft was submitted", runtime.ErrInteractionGone)
		}
		if next.question.FocusedNumber == q.textOption() && strings.Join(strings.Fields(next.question.Draft), " ") == strings.Join(strings.Fields(text), " ") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(respondVerifyInterval):
		}
	}
	return fmt.Errorf("native question draft was not confirmed within %s; no answer was submitted", respondVerifyTimeout)
}
