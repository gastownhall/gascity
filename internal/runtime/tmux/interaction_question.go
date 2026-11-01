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
	Focused       string
	Numbers       []int
	Provider      string
	NativeID      string
	FreeNumber    int
	ChatNumber    int
	Draft         string
	FocusedNumber int
	Collapsed     bool
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
	var continuations [][]string
	focused := false
	for _, line := range lines[header+1 : footer] {
		trimmed := strings.TrimSpace(line)
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
				focused = true
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
