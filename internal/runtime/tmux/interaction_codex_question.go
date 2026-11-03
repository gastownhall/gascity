package tmux

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

var (
	codexQueuedQuestionCount = regexp.MustCompile(`^[?？] (\d+) questions?$`)
	codexQuestionPosition    = regexp.MustCompile(`^([1-9][0-9]*) of ([1-9][0-9]*)$`)
)

// codexQuestionPanel reads the native async question editor, never an approval
// menu or conversational bullet list. Pending itself does not open the editor.
func codexQuestionPanel(lines []string) *parsedQuestion {
	end := trimBlankTail(lines, len(lines))
	if end == 0 {
		return nil
	}
	footer := lines[end-1]
	// Codex renders Control-] as a key glyph on macOS. Keep all three
	// native editor controls required in either rendering.
	if !strings.Contains(footer, "enter submit") || (!strings.Contains(footer, "^] skip") && !strings.Contains(footer, "⌃] skip")) || !strings.Contains(footer, "main prompt") {
		return nil
	}
	start := -1
	for i := end - 2; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "• Queued follow-up inputs" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	q := &parsedQuestion{Header: "Codex question", Provider: "codex"}
	var prompt []string
	var continuations [][]string
	for _, line := range lines[start+1 : end-1] {
		row := strings.TrimSpace(line)
		if m := approvalOptionRe.FindStringSubmatch(row); m != nil {
			number, _ := strconv.Atoi(m[2])
			q.Numbers = append(q.Numbers, number)
			q.Choices = append(q.Choices, m[3])
			continuations = append(continuations, nil)
			if m[1] != "" {
				q.Focused = m[3]
				q.FocusedNumber = number
			}
		} else if len(q.Choices) > 0 && row != "" {
			last := len(continuations) - 1
			continuations[last] = append(continuations[last], row)
		} else if len(q.Choices) == 0 && row != "" {
			prompt = append(prompt, row)
		}
	}
	// The editor counter is a complete row before the authored question, not
	// a prefix to strip from arbitrary titles. Keep a lone "1 of 2" title as
	// data; a wrapped title that resembles this control still has to match the
	// native batch position, total and exact remaining title below.
	if len(prompt) > 1 {
		if position := codexQuestionPosition.FindStringSubmatch(prompt[0]); position != nil {
			index, indexErr := strconv.Atoi(position[1])
			total, totalErr := strconv.Atoi(position[2])
			if indexErr == nil && totalErr == nil && index <= total {
				q.CodexPosition, q.CodexTotal = index, total
				prompt = prompt[1:]
			}
		}
	}
	q.Question = strings.Join(prompt, " ")
	if len(q.Numbers) > 0 {
		last := len(q.Numbers) - 1
		q.FreeNumber = q.Numbers[last]
		if q.Choices[last] != "Other" && q.Choices[last] != "Other (write an answer)" {
			q.Draft = strings.Join(append([]string{q.Choices[last]}, continuations[last]...), "\n")
		}
		q.Choices[last] = "Other"
	}
	if q.Question == "" {
		return nil
	}
	return q
}

func codexQuestionCount(lines []string) int {
	// A collapsed question indicator must be in the live bottom block and have
	// its native answer hint. A past panel above a later composer is not live.
	start := -1
	for i := len(lines) - 1; i >= 0 && len(lines)-i < 20; i-- {
		if strings.TrimSpace(lines[i]) == "• Queued follow-up inputs" {
			start = i
			break
		}
	}
	if start < 0 {
		return 0
	}
	count := 0
	hint := false
	for _, line := range lines[start+1:] {
		row := strings.TrimSpace(line)
		if m := codexQueuedQuestionCount.FindStringSubmatch(row); m != nil {
			count, _ = strconv.Atoi(m[1])
		}
		// Codex renders the same Shift-Left shortcut as a keyboard glyph on
		// macOS. Both forms still require the live queue heading and count;
		// the owned native request below remains the answer authority.
		hint = hint || strings.Contains(row, "shift+← to answer") || strings.Contains(row, "⇧← to answer")
	}
	if !hint {
		return 0
	}
	return count
}

// codexNativeQuestions reads only the rollout held by this pane's own process
// tree. Native call IDs distinguish identical questions raised twice. The TUI
// queue must agree with the recorded requests; disagreement refuses input.
func (t *Tmux) codexNativeQuestions(target string, visible *parsedQuestion, count int) (*parsedQuestion, error) {
	path := t.codexRolloutOf(target)
	if path == "" {
		return nil, fmt.Errorf("%w: cannot identify the question's native Codex rollout", runtime.ErrRuntimeUnavailable)
	}
	tail, err := readQuestionTranscriptTail(path)
	if err != nil {
		return nil, err
	}
	questions, err := codexQuestionsIn(tail)
	if err != nil {
		return nil, err
	}
	if visible != nil {
		// The observed counter indexes one original async request, not a
		// guessed ordering across distinct unresolved invocations. Until that
		// provider layout is qualified, refuse a counter spanning several calls.
		if visible.CodexTotal > 0 && !codexQuestionsShareBatch(questions) {
			return nil, codexQuestionMismatch(visible, questions)
		}
		var found *parsedQuestion
		for _, q := range questions {
			if q.Question == visible.Question && equalQuestionChoices(q.answerChoices(), visible.answerChoices()) && codexQuestionPositionMatches(q, visible) {
				if found != nil {
					return nil, fmt.Errorf("%w: native Codex question is ambiguous", runtime.ErrRuntimeUnavailable)
				}
				found = q
			}
		}
		if found == nil {
			return nil, codexQuestionMismatch(visible, questions)
		}
		visible.NativeID = found.NativeID
		return visible, nil
	}
	if count != len(questions) || count == 0 {
		return nil, fmt.Errorf("%w: Codex queue and native requests disagree", runtime.ErrRuntimeUnavailable)
	}
	return questions[0], nil
}

func codexQuestionPositionMatches(native, visible *parsedQuestion) bool {
	if visible.CodexTotal == 0 {
		// Codex drops the counter when advancing within an async batch, before
		// the earlier reply is necessarily recorded. The caller still requires
		// one unique exact native title/choice match in the proven editor.
		return true
	}
	return native.CodexPosition == visible.CodexPosition && native.CodexTotal == visible.CodexTotal
}

func codexQuestionsShareBatch(questions []*parsedQuestion) bool {
	if len(questions) == 0 {
		return false
	}
	batch := ""
	for _, question := range questions {
		// NativeID is authored above from call_id plus the original index.
		// Remove only that final index, preserving every byte of the call ID.
		key := strings.TrimSuffix(question.NativeID, fmt.Sprintf("-%d", question.CodexPosition-1))
		if batch != "" && key != batch {
			return false
		}
		batch = key
	}
	return true
}

// codexQuestionMismatch describes only the parsed question shapes needed to
// diagnose a refused native binding. It never includes the pane, rollout path,
// native call IDs, draft answers or unrelated transcript fields. Bound both
// the fields and the escaped summaries so control characters cannot expand an
// arbitrarily large error while exact matching above remains unchanged.
func codexQuestionMismatch(visible *parsedQuestion, native []*parsedQuestion) error {
	shapes := make([]string, 0, min(len(native), 4))
	for _, question := range native[:min(len(native), 4)] {
		shapes = append(shapes, codexQuestionDiagnosticShape(question))
	}
	return fmt.Errorf("%w: question editor has no matching native request (visible=%s; native_count=%d; native_shapes=[%s])",
		runtime.ErrRuntimeUnavailable, codexQuestionDiagnosticShape(visible), len(native), strings.Join(shapes, "; "))
}

func codexQuestionDiagnosticShape(question *parsedQuestion) string {
	choices := question.answerChoices()
	labels := make([]string, 0, min(len(choices), 4))
	for _, choice := range choices[:min(len(choices), 4)] {
		labels = append(labels, codexQuestionDiagnosticText(choice, 48))
	}
	shape := fmt.Sprintf("{title=%q choices=%q choice_count=%d position=%d total=%d}", codexQuestionDiagnosticText(question.Question, 96), labels, len(choices), question.CodexPosition, question.CodexTotal)
	return codexQuestionDiagnosticText(shape, 512)
}

func codexQuestionDiagnosticText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}

func equalQuestionChoices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func codexQuestionsIn(tail string) ([]*parsedQuestion, error) {
	type nativeQuestion struct {
		Title   string   `json:"title"`
		Options []string `json:"options"`
	}
	var pending []*parsedQuestion
	for _, line := range strings.Split(tail, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				Type      string `json:"type"`
				Name      string `json:"name"`
				CallID    string `json:"call_id"`
				Arguments string `json:"arguments"`
				Role      string `json:"role"`
				Content   []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("%w: native Codex rollout contains an incomplete record", runtime.ErrRuntimeUnavailable)
		}
		p := row.Payload
		// Async user inputs outlive the model turn that queued them. A
		// task_complete record is not an answer or dismissal: Codex keeps the
		// question editor live until the user answers. The pane queue and exact
		// native occurrence below remain required before any answer is sent.
		if row.Type == "event_msg" && p.Type == "turn_aborted" {
			pending = nil
			continue
		}
		if row.Type != "response_item" {
			continue
		}
		if p.Type == "function_call" && (p.Name == "request_user_input_async" || strings.HasSuffix(p.Name, ".request_user_input_async")) && p.CallID != "" {
			var args struct {
				Questions []nativeQuestion `json:"questions"`
			}
			if err := json.Unmarshal([]byte(p.Arguments), &args); err != nil {
				return nil, fmt.Errorf("decoding native Codex questions: %w", err)
			}
			for index, native := range args.Questions {
				if native.Title == "" {
					return nil, fmt.Errorf("%w: native Codex question has no title", runtime.ErrRuntimeUnavailable)
				}
				q := &parsedQuestion{Header: "Codex question", Provider: "codex", Collapsed: true, Question: native.Title, NativeID: fmt.Sprintf("tmux-codex-question-%s-%d", p.CallID, index), CodexPosition: index + 1, CodexTotal: len(args.Questions)}
				for _, option := range native.Options {
					if len(q.Choices) == 32 {
						break
					}
					if len(option) <= 512 {
						q.Choices = append(q.Choices, option)
						q.Numbers = append(q.Numbers, len(q.Choices))
					}
				}
				q.FreeNumber = len(q.Choices) + 1
				q.Choices = append(q.Choices, "Other")
				q.Numbers = append(q.Numbers, len(q.Choices))
				pending = append(pending, q)
			}
		}
		if p.Type == "message" && p.Role == "user" {
			for _, c := range p.Content {
				text := strings.TrimSpace(c.Text)
				const open = "<send_user_message_question_reply>"
				const closingTag = "</send_user_message_question_reply>"
				if !strings.HasPrefix(text, open) || !strings.HasSuffix(text, closingTag) {
					continue
				}
				text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, open), closingTag))
				var replies []struct {
					QuestionID string `json:"questionItemId"`
				}
				if json.Unmarshal([]byte(text), &replies) != nil {
					continue
				}
				for _, reply := range replies {
					var identity []json.RawMessage
					if json.Unmarshal([]byte(reply.QuestionID), &identity) != nil || len(identity) != 3 {
						continue
					}
					var tool, call string
					var index int
					if json.Unmarshal(identity[0], &tool) != nil || json.Unmarshal(identity[1], &call) != nil || json.Unmarshal(identity[2], &index) != nil || tool != "request_user_input_async" {
						continue
					}
					id := fmt.Sprintf("tmux-codex-question-%s-%d", call, index)
					keep := pending[:0]
					for _, q := range pending {
						if q.NativeID != id {
							keep = append(keep, q)
						}
					}
					pending = keep
				}
			}
		}
	}
	return pending, nil
}
