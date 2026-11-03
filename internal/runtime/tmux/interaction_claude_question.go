package tmux

import (
	"context"
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
	for _, c := range pending {
		for index, native := range c.Questions {
			var options []string
			for _, option := range native.Options {
				options = append(options, option.Label)
			}
			if native.MultiSelect || strings.Join(strings.Fields(native.Question), " ") != strings.Join(strings.Fields(q.Question), " ") || !equalQuestionChoices(options, q.answerChoices()) {
				continue
			}
			if match != "" {
				return fmt.Errorf("%w: native Claude question is ambiguous", runtime.ErrRuntimeUnavailable)
			}
			match = fmt.Sprintf("tmux-question-%s-%s-%s-%d", latestSession, latestPrompt, c.ID, index)
			if latestSession != "" && latestPrompt != "" {
				form = cloneClaudeQuestionForm(c.Questions, index)
			}
			q.Question = native.Question
		}
	}
	if match == "" {
		return fmt.Errorf("%w: visible question has no matching native Claude invocation", runtime.ErrRuntimeUnavailable)
	}
	q.NativeID = match
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
func matchesClaudeQuestionReview(review *parsedQuestion, form *claudeQuestionForm, answer string) bool {
	if form == nil || len(form.Questions) < 2 || form.Index != len(form.Questions)-1 ||
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
		if question.MultiSelect {
			return false
		}
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
		if strings.TrimSpace(value) == "" || i == form.Index && normalize(value) != normalize(answer) {
			return false
		}
	}
	return true
}

// submitClaudeQuestionFormReview runs only after a request-bound last answer.
// Pending, binding and background delivery remain read-only. The original
// invocation/schema and the entire visible review are rechecked before its one
// fenced Enter; a changed invocation, value, menu or focus authorizes no key.
func (t *Tmux) submitClaudeQuestionFormReview(ctx context.Context, target string, answered *parsedQuestion, answer string, review *parsedQuestion) error {
	verify := func(current *parsedQuestion) error {
		if current == nil || !matchesClaudeQuestionReview(current, answered.ClaudeForm, answer) ||
			current.Question != review.Question || !slices.Equal(current.Choices, review.Choices) {
			return fmt.Errorf("%w: native question review changed or is incomplete", runtime.ErrInteractionGone)
		}
		probe := *answered
		probe.NativeID, probe.ClaudeForm = "", nil
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
			return ctx.Err()
		case <-time.After(respondVerifyInterval):
		}
		next, err := t.dialogInPane(target)
		if err != nil {
			return fmt.Errorf("verifying native question review submission: %w", err)
		}
		if next.question == nil || !isClaudeQuestionFormReview(next.question) || next.question.Question != review.Question {
			return nil
		}
	}
	return fmt.Errorf("native question review did not clear within %s; no submit was repeated", respondVerifyTimeout)
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
