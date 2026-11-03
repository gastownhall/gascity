package tmux

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// claudeQuestionForm is the authored native invocation observed while binding
// one question. Only an explicit answer uses it to complete a matching review.
type claudeQuestionForm struct {
	Questions []claudeQuestion
	Index     int
}

// claudeMultipleChoiceState reads the native checkbox rows. Unknown markers,
// an existing free-text selection or an edited Other row remain unanswerable.
func claudeMultipleChoiceState(q *parsedQuestion) ([]string, map[int]bool, error) {
	choices := slices.Clone(q.Choices)
	checked := map[int]bool{}
	for i, label := range choices {
		if !q.isChoice(i) {
			continue
		}
		switch {
		case strings.HasPrefix(label, "[ ] "):
			choices[i], checked[q.Numbers[i]] = strings.TrimPrefix(label, "[ ] "), false
		case strings.HasPrefix(label, "[✔] "):
			choices[i], checked[q.Numbers[i]] = strings.TrimPrefix(label, "[✔] "), true
		default:
			return nil, nil, fmt.Errorf("%w: native multiple-choice checkbox state is unknown", runtime.ErrInteractionActionUnavailable)
		}
	}
	draft := strings.Join(strings.Fields(q.Draft), " ")
	if draft != "[ ] Type something" && draft != "[ ] Type something Submit" {
		return nil, nil, fmt.Errorf("%w: native multiple-choice free-text row is not empty and unchecked", runtime.ErrInteractionActionUnavailable)
	}
	return choices, checked, nil
}

func sameClaudeChecks(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for number, selected := range a {
		if actual, ok := b[number]; !ok || actual != selected {
			return false
		}
	}
	return true
}

func (t *Tmux) readClaudeMultipleState(target string, expected *parsedQuestion) (*parsedQuestion, error) {
	dialog, err := t.dialogInPane(target)
	if err != nil {
		return nil, err
	}
	current := dialog.question
	if current == nil || !current.MultiSelect || current.NativeID != expected.NativeID || !sameClaudeQuestionForm(current.ClaudeForm, expected.ClaudeForm) {
		return nil, fmt.Errorf("%w: native multiple-choice request or authored schema changed", runtime.ErrInteractionGone)
	}
	return current, nil
}

func (t *Tmux) waitClaudeMultipleState(ctx context.Context, target string, expected *parsedQuestion, checked map[int]bool) (*parsedQuestion, error) {
	deadline := time.Now().Add(respondVerifyTimeout)
	for time.Now().Before(deadline) {
		current, err := t.readClaudeMultipleState(target, expected)
		if err != nil {
			return nil, err
		}
		if sameClaudeChecks(current.CheckedChoices, checked) {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(respondVerifyInterval):
		}
	}
	return nil, fmt.Errorf("native multiple-choice selection was not confirmed within %s", respondVerifyTimeout)
}

// respondClaudeMultipleChoices applies one typed desired selection set. Every
// navigation/toggle/submit remains request and incarnation bound; a changed
// native schema or unconfirmed checkbox stops before the next key.
func (t *Tmux) respondClaudeMultipleChoices(ctx context.Context, target string, q *parsedQuestion, response runtime.InteractionResponse) error {
	if !q.MultiSelect || q.Provider == "codex" || q.ClaudeForm == nil {
		return fmt.Errorf("%w: this native question does not support typed multiple selections", runtime.ErrInteractionActionUnavailable)
	}
	if response.Text != "" || len(response.Answers) == 0 || len(response.Answers) > 32 {
		return fmt.Errorf("%w: multiple selections require one to 32 authored answer labels and no text", runtime.ErrInteractionResponseInvalid)
	}
	wanted := map[int]bool{}
	seen := map[string]bool{}
	for _, answer := range response.Answers {
		if seen[answer] {
			return fmt.Errorf("%w: duplicate multiple-choice answer", runtime.ErrInteractionResponseInvalid)
		}
		seen[answer] = true
		number, matches := 0, 0
		for i, label := range q.Choices {
			if q.isChoice(i) && label == answer {
				number, matches = q.Numbers[i], matches+1
			}
		}
		if matches != 1 {
			return fmt.Errorf("%w: answer is not one unambiguous authored choice", runtime.ErrInteractionActionUnavailable)
		}
		wanted[number] = true
	}
	for number := range q.CheckedChoices {
		if _, selected := wanted[number]; !selected {
			wanted[number] = false
		}
	}
	ctx, cancel := context.WithTimeout(ctx, respondVerifyTimeout)
	defer cancel()
	current := q
	for _, number := range q.Numbers {
		selected, authored := wanted[number]
		if !authored || current.CheckedChoices[number] == selected {
			continue
		}
		if err := t.focusClaudeQuestionRow(ctx, target, current, number); err != nil {
			return err
		}
		focused, err := t.readClaudeMultipleState(target, current)
		if err != nil {
			return err
		}
		if focused.FocusedNumber != number || !sameClaudeChecks(focused.CheckedChoices, current.CheckedChoices) {
			return fmt.Errorf("%w: native checkbox state changed before toggle", runtime.ErrInteractionGone)
		}
		expected := map[int]bool{}
		for key, value := range current.CheckedChoices {
			expected[key] = value
		}
		expected[number] = selected
		if _, err := t.runCtx(inputCtx(ctx), "send-keys", "-t", target, "Enter"); err != nil {
			return fmt.Errorf("toggling native question choice: %w", err)
		}
		current, err = t.waitClaudeMultipleState(ctx, target, focused, expected)
		if err != nil {
			return err
		}
	}
	if !sameClaudeChecks(current.CheckedChoices, wanted) {
		return fmt.Errorf("%w: native multiple-choice set is not the requested set", runtime.ErrInteractionGone)
	}
	if err := t.focusClaudeQuestionRow(ctx, target, current, 0); err != nil {
		return err
	}
	current, err := t.readClaudeMultipleState(target, current)
	if err != nil {
		return err
	}
	if current.FocusedNumber != 0 || current.Focused != "Submit" || !sameClaudeChecks(current.CheckedChoices, wanted) {
		return fmt.Errorf("%w: complete native selections and Submit focus were not confirmed", runtime.ErrInteractionGone)
	}
	if _, err := t.runCtx(inputCtx(ctx), "send-keys", "-t", target, "Enter"); err != nil {
		return fmt.Errorf("submitting native multiple selections: %w", err)
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
			return interactionAnswerUnconfirmed(err)
		}
		if next.question != nil && isClaudeQuestionFormReview(next.question) {
			return interactionAnswerUnconfirmed(t.submitClaudeQuestionFormReview(ctx, target, current, "", response.Answers, next.question))
		}
		if next.question == nil || next.question.requestID() != q.requestID() {
			return nil
		}
	}
	return interactionAnswerUnconfirmed(fmt.Errorf("native multiple selections did not clear within %s; no submit was repeated", respondVerifyTimeout))
}

func cloneClaudeQuestionForm(questions []claudeQuestion, index int) *claudeQuestionForm {
	form := &claudeQuestionForm{Questions: slices.Clone(questions), Index: index}
	for i := range form.Questions {
		form.Questions[i].Options = slices.Clone(form.Questions[i].Options)
	}
	return form
}

// bindClaudeQuestion names the native tool invocation, so an identical question
// raised again cannot accept a response to the first one. Older runtimes with
// no native identity stay visible and guarded, but offer terminal answers only.
func (t *Tmux) bindClaudeQuestion(target string, q *parsedQuestion) error {
	read := t.claudeQuestionTranscript
	if read == nil {
		read = t.readClaudeQuestionTranscript
	}
	tail, err := read(target)
	if err != nil {
		return err
	}
	if tail == "" {
		return nil
	}
	type question = claudeQuestion
	type call struct {
		ID        string
		Questions []question
	}
	var pending []call
	completed := map[string]bool{}
	latestSession, latestPrompt := "", ""
	for _, line := range strings.Split(tail, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			PromptID  string `json:"promptId"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return fmt.Errorf("%w: native Claude question record is incomplete", runtime.ErrRuntimeUnavailable)
		}
		content := strings.TrimSpace(string(row.Message.Content))
		if row.Type == "user" && !strings.HasPrefix(content, "[") {
			pending = nil // A new native prompt retires unfinished prior-turn invocations.
			latestSession, latestPrompt = row.SessionID, row.PromptID
		}
		if !strings.HasPrefix(content, "[") {
			continue
		} // Plain user text cannot name a tool interaction.
		var items []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
			Input     struct {
				Questions []question `json:"questions"`
			} `json:"input"`
		}
		if err := json.Unmarshal(row.Message.Content, &items); err != nil {
			return fmt.Errorf("%w: native Claude question content is incomplete", runtime.ErrRuntimeUnavailable)
		}

		toolResult := false
		for _, item := range items {
			if row.Type == "assistant" && item.Type == "tool_use" && item.Name == "AskUserQuestion" && item.ID != "" {
				pending = append(pending, call{item.ID, item.Input.Questions})
			}
			if row.Type == "user" && item.Type == "tool_result" && item.ToolUseID != "" {
				toolResult = true
				completed[item.ToolUseID] = true
				keep := pending[:0]
				for _, c := range pending {
					if c.ID != item.ToolUseID {
						keep = append(keep, c)
					}
				}
				pending = keep
			}
		}
		if row.Type == "user" && !toolResult {
			pending = nil
		}
		if row.Type == "user" && (!toolResult || row.PromptID != "") {
			latestSession, latestPrompt = row.SessionID, row.PromptID
		}
	}
	if latestSession != "" && latestPrompt != "" {
		read := t.claudeQuestionProjections
		if read == nil {
			read = t.readClaudeQuestionProjections
		}
		projections, err := read(target)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, c := range pending {
			seen[c.ID] = true
		}
		for _, projection := range projections {
			if projection.Version != 1 || projection.SessionID != latestSession || projection.PromptID != latestPrompt ||
				projection.ToolUseID == "" || completed[projection.ToolUseID] || seen[projection.ToolUseID] {
				continue
			}
			pending = append(pending, call{projection.ToolUseID, projection.Questions})
			seen[projection.ToolUseID] = true
		}
	}
	match := ""
	var form *claudeQuestionForm
	var multiChoices []string
	var multiChecks map[int]bool
	for _, c := range pending {
		for index, native := range c.Questions {
			var options []string
			for _, option := range native.Options {
				options = append(options, option.Label)
			}
			choices := q.answerChoices()
			var normalized []string
			var checks map[int]bool
			if native.MultiSelect {
				var err error
				normalized, checks, err = claudeMultipleChoiceState(q)
				if err != nil {
					continue
				}
				choices = nil
				for i, label := range normalized {
					if q.isChoice(i) {
						choices = append(choices, label)
					}
				}
			}
			if strings.Join(strings.Fields(native.Question), " ") != strings.Join(strings.Fields(q.Question), " ") || !equalQuestionChoices(options, choices) {
				continue
			}
			if match != "" {
				return fmt.Errorf("%w: native Claude question is ambiguous", runtime.ErrRuntimeUnavailable)
			}
			match = fmt.Sprintf("tmux-question-%s-%s-%s-%d", latestSession, latestPrompt, c.ID, index)
			if latestSession != "" && latestPrompt != "" {
				form = cloneClaudeQuestionForm(c.Questions, index)
			}
			if native.MultiSelect {
				data, err := json.Marshal(c.Questions)
				if err != nil {
					return fmt.Errorf("encoding native question schema: %w", err)
				}
				digest := sha256.Sum256(data)
				match += fmt.Sprintf("-multi-%x", digest[:8])
				multiChoices, multiChecks = normalized, checks
			}
			q.Question = native.Question
		}
	}
	if match == "" {
		return fmt.Errorf("%w: visible question has no matching native Claude invocation", runtime.ErrRuntimeUnavailable)
	}
	q.NativeID = match
	if multiChoices != nil {
		q.MultiSelect, q.CheckedChoices = true, multiChecks
		q.Choices = multiChoices
		// The unnumbered Submit row is above Chat about this in the native
		// pane. Keep navigation in that visible order rather than appending
		// Submit after every numbered row and waiting for the wrong focus.
		submitIndex := len(q.Numbers)
		if index := slices.Index(q.Numbers, q.ChatNumber); q.ChatNumber > 0 && index >= 0 {
			submitIndex = index
		}
		q.Numbers = slices.Insert(q.Numbers, submitIndex, 0)
		q.Choices = slices.Insert(q.Choices, submitIndex, "Submit")
		if q.FocusedNumber > 0 {
			if index := slices.Index(q.Numbers, q.FocusedNumber); index >= 0 {
				q.Focused = q.Choices[index]
			}
		}
	}
	q.ClaudeForm = form
	return nil
}

func isClaudeQuestionFormReview(q *parsedQuestion) bool {
	return q.Provider != "codex" && strings.HasPrefix(strings.Join(strings.Fields(q.Question), " "), "Review your answers ")
}

func sameClaudeQuestionForm(a, b *claudeQuestionForm) bool {
	if a == nil || b == nil || a.Index != b.Index || len(a.Questions) != len(b.Questions) {
		return false
	}
	for i, left := range a.Questions {
		right := b.Questions[i]
		if left.Header != right.Header || left.Question != right.Question || left.MultiSelect != right.MultiSelect || len(left.Options) != len(right.Options) {
			return false
		}
		for j := range left.Options {
			if left.Options[j].Label != right.Options[j].Label {
				return false
			}
		}
	}
	return true
}

// matchesClaudeQuestionReview requires the whole native review, in authored
// order, and the last value this explicit response supplied. Previous answers
// remain the provider's visible values; empty or incomplete reviews are refused.
func matchesClaudeQuestionReview(review *parsedQuestion, form *claudeQuestionForm, answer string, selections []string) bool {
	if form == nil || len(form.Questions) == 0 || len(form.Questions) < 2 && !form.Questions[0].MultiSelect || form.Index != len(form.Questions)-1 ||
		!slices.Equal(review.answerChoices(), []string{"Submit answers", "Cancel"}) || review.FocusedNumber != 1 || review.Focused != "Submit answers" {
		return false
	}
	normalize := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	remaining, ok := strings.CutPrefix(normalize(review.Question), "Review your answers ")
	if !ok {
		return false
	}
	remaining, ok = strings.CutSuffix(remaining, " Ready to submit your answers?")
	if !ok {
		return false
	}
	for i, question := range form.Questions {

		remaining, ok = strings.CutPrefix(remaining, "● "+normalize(question.Question)+" → ")
		if !ok {
			return false
		}
		value := remaining
		if i+1 < len(form.Questions) {
			marker := " ● " + normalize(form.Questions[i+1].Question) + " → "
			var next string
			value, next, ok = strings.Cut(remaining, marker)
			if !ok {
				return false
			}
			remaining = strings.TrimPrefix(marker, " ") + next
		}
		if strings.TrimSpace(value) == "" {
			return false
		}
		if i == form.Index {
			wanted := answer
			if question.MultiSelect {
				if len(selections) == 0 {
					return false
				}
				var authored []string
				for _, option := range question.Options {
					if slices.Contains(selections, option.Label) {
						authored = append(authored, option.Label)
					}
				}
				if len(authored) != len(selections) {
					return false
				}
				wanted = strings.Join(authored, ", ")
			} else if selections != nil {
				return false
			}
			if normalize(value) != normalize(wanted) {
				return false
			}
		}
	}
	return true
}

// submitClaudeQuestionFormReview runs only after a request-bound last answer.
// Pending, binding and background delivery remain read-only. The original
// invocation/schema and the entire visible review are rechecked before its one
// fenced Enter; a changed invocation, value, menu or focus authorizes no key.
func (t *Tmux) submitClaudeQuestionFormReview(ctx context.Context, target string, answered *parsedQuestion, answer string, selections []string, review *parsedQuestion) error {
	verify := func(current *parsedQuestion) error {
		if current == nil || !matchesClaudeQuestionReview(current, answered.ClaudeForm, answer, selections) ||
			current.Question != review.Question || !slices.Equal(current.Choices, review.Choices) {
			return fmt.Errorf("%w: native question review changed or is incomplete", runtime.ErrInteractionGone)
		}
		probe := *answered
		probe.NativeID, probe.ClaudeForm = "", nil
		if probe.MultiSelect {
			// Reconstruct the original checkbox question for an authority-only
			// rebind. The visible pane is the independently checked review now;
			// this probe never navigates or types into it.
			probe.Choices, probe.Numbers = slices.Clone(answered.Choices), slices.Clone(answered.Numbers)
			if index := slices.Index(probe.Numbers, 0); index >= 0 {
				probe.Choices = slices.Delete(probe.Choices, index, index+1)
				probe.Numbers = slices.Delete(probe.Numbers, index, index+1)
			}
			for i, label := range probe.Choices {
				if probe.isChoice(i) {
					prefix := "[ ] "
					if answered.CheckedChoices[probe.Numbers[i]] {
						prefix = "[✔] "
					}
					probe.Choices[i] = prefix + label
				}
			}
			probe.MultiSelect, probe.CheckedChoices = false, nil
		}
		if err := t.bindClaudeQuestion(target, &probe); err != nil {
			return fmt.Errorf("rechecking native question review: %w", err)
		}
		if probe.NativeID != answered.NativeID || !sameClaudeQuestionForm(probe.ClaudeForm, answered.ClaudeForm) {
			return fmt.Errorf("%w: native question invocation changed before review submission", runtime.ErrInteractionGone)
		}
		return nil
	}
	if err := verify(review); err != nil {
		return err
	}
	current, err := t.dialogInPane(target)
	if err != nil {
		return fmt.Errorf("reading native question review before submission: %w", err)
	}
	if err := verify(current.question); err != nil {
		return err
	}
	if _, err := t.runCtx(inputCtx(ctx), "send-keys", "-t", target, "Enter"); err != nil {
		return fmt.Errorf("submitting native question review: %w", err)
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
			return interactionAnswerUnconfirmed(fmt.Errorf("verifying native question review submission: %w", err))
		}
		if next.question == nil || !isClaudeQuestionFormReview(next.question) || next.question.Question != review.Question {
			return nil
		}
	}
	return interactionAnswerUnconfirmed(fmt.Errorf("native question review did not clear within %s; no submit was repeated", respondVerifyTimeout))
}

func (t *Tmux) readClaudeQuestionTranscript(target string) (string, error) {
	env, err := t.GetAllEnvironment(target)
	if err != nil {
		return "", fmt.Errorf("reading question runtime identity: %w", err)
	}
	id := env["GC_PROVIDER_SESSION_ID"]
	if id == "" {
		return "", nil
	}
	workDir := env["GC_DIR"]
	if workDir == "" {
		return "", fmt.Errorf("%w: native question runtime has no work directory", runtime.ErrRuntimeUnavailable)
	}
	root := env["CLAUDE_CONFIG_DIR"]
	if root == "" {
		home := env["HOME"]
		if home == "" {
			home, err = os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolving native question home: %w", err)
			}
		}
		root = filepath.Join(home, ".claude")
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(workDir, root)
	}
	path := sessionlog.FindSessionFileByID([]string{filepath.Join(root, "projects")}, workDir, id)
	if path == "" {
		return "", fmt.Errorf("%w: native question transcript is not yet available", runtime.ErrRuntimeUnavailable)
	}
	return readQuestionTranscriptTail(path)
}

func readQuestionTranscriptTail(path string) (string, error) {
	const limit = 8 * 1024 * 1024
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("reading native question transcript: %w", err)
	}
	tail, err := readFileTail(path, limit)
	if err != nil {
		return "", fmt.Errorf("reading native question transcript: %w", err)
	}
	if info.Size() > limit {
		_, rest, found := strings.Cut(tail, "\n")
		if !found {
			return "", fmt.Errorf("%w: native question record exceeds observation limit", runtime.ErrRuntimeUnavailable)
		}
		tail = rest
	}
	return tail, nil
}
