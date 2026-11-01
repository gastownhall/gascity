package tmux

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
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
	// Choices are the numbered options' labels in pane order.
	Choices []string
	// Focused is the label of the highlighted option.
	Focused       string
	Numbers       []int
	Provider      string
	NativeID      string
	BindingError  string
	FreeNumber    int
	ChatNumber    int
	Draft         string
	FocusedNumber int
	Collapsed     bool
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
	var continuations [][]string
	for _, line := range block[top:footer] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if m := askUserQuestionOptionRe.FindStringSubmatch(trimmed); m != nil {
			label := strings.TrimSpace(m[3])
			if q.FreeNumber > 0 && label == "Chat about this" {
				q.ChatNumber, _ = strconv.Atoi(m[2])
			}
			q.Choices = append(q.Choices, label)
			continuations = append(continuations, nil)
			number, _ := strconv.Atoi(m[2])
			q.Numbers = append(q.Numbers, number)
			if m[1] != "" {
				q.Focused = label
				q.FocusedNumber = number
			}
			continue
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
		joined := make([]string, 0, len(block)-start)
		for _, row := range block[start:] {
			joined = append(joined, strings.TrimSpace(row))
		}
		if askUserQuestionFooterRe.MatchString(strings.Join(joined, " ")) {
			return start
		}
	}
	return -1
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
		if m[1] != "" {
			q.Focused = labels[k]
		}
	}
	// The menu alone could be quoted history at the pane's bottom; the page
	// is live only with its anchor above: the "Review your answers" heading
	// or the ✔ Submit tab strip with its ☐/☒ chips.
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
	}
	if !anchored {
		return nil
	}
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
// question and the choices, never the header's ☐/☒ chips or the focused
// option, which change while the dialog is the same page — a multiSelect
// toggle flips a chip, arrows move the cursor.
func (q *parsedQuestion) requestID() string {
	if q.NativeID != "" {
		return q.NativeID
	}
	h := sha256.Sum256([]byte(q.Header + "\x00" + q.Question + "\x00" + strings.Join(q.Choices, "\x00")))
	return fmt.Sprintf("tmux-question-%x", h[:8])
}

// answerChoices excludes the provider's editor and chat controls from the
// model-authored answers shown to a client.
func (q *parsedQuestion) isChoice(index int) bool {
	return q.Numbers[index] != q.textOption() && q.Numbers[index] != q.ChatNumber
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

// respondQuestion accepts only a request-bound explicit answer. Ordinary
// messages and approval verbs remain unable to select question menu entries.
func (t *Tmux) respondQuestion(ctx context.Context, target string, q *parsedQuestion, response runtime.InteractionResponse) error {
	if response.Action != "answer" {
		return fmt.Errorf("%w: question requires the answer action", runtime.ErrInteractionActionUnavailable)
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
	if _, err := t.runCtx(inputCtx(ctx), "send-keys", "-t", target, "-l", strconv.Itoa(number)); err != nil {
		return fmt.Errorf("sending question answer: %w", err)
	}
	if q.Provider != "codex" || freeText {
		// Claude digits focus, while Codex digits submit predefined choices.
		// Confirm the selected row before the one Enter that can answer anything.
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
			return ctx.Err()
		case <-time.After(respondVerifyInterval):
		}
		next, err := t.dialogInPane(target)
		if err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				return nil
			}
			return fmt.Errorf("verifying question answer: %w", err)
		}
		if next.question == nil || next.question.requestID() != q.requestID() {
			return nil
		}
	}
	return fmt.Errorf("question did not clear within %s of the answer", respondVerifyTimeout)
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
