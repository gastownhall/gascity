package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const nativeClaudeQuestion = ` ☐ Mascot
Which animal should be the test mascot?
❯ 1. Otter
     Make the otter the test mascot.
  2. Owl
     Make the owl the test mascot.
  3. Type something.
────────────────────────────────────────
  4. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel
`

const nativeCodexQuestion = `• Working (32s • esc to interrupt)
• Queued follow-up inputs

  Which animal should be the test mascot?

  › 1. Otter
    2. Owl
    3. Other

  enter submit   ^] skip   shift+→ main prompt
`

const nativeCodexCollapsed = `• Working (32s • esc to interrupt)
• Queued follow-up inputs
  ? 1 question
    shift+← to answer

› Ask Codex to do anything
`

func TestNativeQuestionChoicesAreActionable(t *testing.T) {
	for name, pane := range map[string]string{"claude": nativeClaudeQuestion, "codex": nativeCodexQuestion} {
		t.Run(name, func(t *testing.T) {
			tm, ex := nativeQuestionTmux(t, pane)
			if name == "codex" {
				path := nativeQuestionRollout(t)
				tm.codexRolloutHeld = func(int) string { return path }
			}
			pending, err := tm.Pending("q")
			if err != nil || pending == nil {
				t.Fatalf("Pending=%+v,%v", pending, err)
			}
			if pending.Kind != "question" || !slices.Equal(pending.Options, []string{"Otter", "Owl"}) || pending.Metadata["answer_action"] != "answer" || pending.Metadata["free_text"] != "true" {
				t.Fatalf("native question must expose choices and free-text capability: %+v", pending)
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
}

// A response must name exactly the native question the operator saw. Approval
// verbs, empty request IDs and stale IDs must not send even preparatory keys.
func TestNativeQuestionRefusesUnboundOrStaleAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		response runtime.InteractionResponse
		want     error
	}{
		"unbound":  {runtime.InteractionResponse{Action: "answer", Text: "Owl"}, runtime.ErrInteractionResponseInvalid},
		"stale":    {runtime.InteractionResponse{RequestID: "old-question", Action: "answer", Text: "Owl"}, runtime.ErrInteractionGone},
		"approval": {runtime.InteractionResponse{Action: "approve"}, runtime.ErrInteractionActionUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			tm, ex := nativeQuestionTmux(t, nativeClaudeQuestion)
			err := tm.Respond("q", tc.response)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Respond=%v,want%v", err, tc.want)
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
}

type nativeAnswerExecutor struct {
	paneExecutor
	literal            []string
	keys               []string
	paste              string
	codex              bool
	advanceOnPaste     bool
	claudeDigitsSubmit bool
}

func (e *nativeAnswerExecutor) execute(args []string) (string, error) {
	if slices.Contains(args, "S-Left") {
		e.pane = nativeCodexQuestion
	}

	if slices.Contains(args, "load-buffer") {
		data, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return "", err
		}
		e.paste = string(data)
	}
	if slices.Contains(args, "paste-buffer") {
		e.literal = append(e.literal, e.paste)
		e.pane = strings.ReplaceAll(e.pane, "Type something.", e.paste)
		if e.advanceOnPaste {
			e.pane = "● Question canceled.\n❯ "
		}
		if e.codex {
			e.pane = strings.ReplaceAll(e.pane, "Other", e.paste)
		}
	}
	if slices.Contains(args, "send-keys") && !slices.Contains(args, "-X") {
		text := args[len(args)-1]
		if slices.Contains(args, "-l") {
			e.literal = append(e.literal, text)
			if text == "2" {
				e.pane = strings.ReplaceAll(nativeClaudeQuestion, "❯ 1. Otter", "  1. Otter")
				e.pane = strings.ReplaceAll(e.pane, "  2. Owl", "❯ 2. Owl")
				if e.codex {
					e.pane = "● Answer received.\n› Ask Codex to do anything"
				}
			}
			if text == "3" {
				e.pane = strings.ReplaceAll(nativeClaudeQuestion, "❯ 1. Otter", "  1. Otter")
				e.pane = strings.ReplaceAll(e.pane, "  3. Type something.", "❯ 3. Type something.")
				if e.codex {
					e.pane = strings.ReplaceAll(nativeCodexQuestion, "› 1. Otter", "  1. Otter")
					e.pane = strings.ReplaceAll(e.pane, "  3. Other", "› 3. Other")
				}
			}
			if text == "Fox" {
				e.pane = strings.ReplaceAll(e.pane, "Type something.", "Fox")
			}
		}
		if !e.codex && slices.Contains(args, "-l") && text == "2" && e.claudeDigitsSubmit {
			e.pane = "● Answer received.\n❯ "
		}
		if !e.codex && (text == "Down" || text == "Up") {
			e.keys = append(e.keys, text)
			question := parseAskUserQuestion(strings.Split(e.pane, "\n"))
			if question != nil {
				index := slices.Index(question.Numbers, question.FocusedNumber)
				if text == "Down" {
					index++
				} else {
					index--
				}
				if index >= 0 && index < len(question.Numbers) {
					rows := strings.Split(e.pane, "\n")
					for i, line := range rows {
						m := askUserQuestionOptionRe.FindStringSubmatch(strings.TrimSpace(line))
						if m == nil {
							continue
						}
						marker := "  "
						if m[2] == fmt.Sprint(question.Numbers[index]) {
							marker = "❯ "
						}
						rows[i] = marker + m[2] + ". " + m[3]
					}
					e.pane = strings.Join(rows, "\n")
				}
			}
		}
		if text == "Enter" {
			e.keys = append(e.keys, text)
			e.pane = "● Answer received.\n❯ "
		}
	}
	return e.paneExecutor.execute(args)
}

func (e *nativeAnswerExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

func TestNativeQuestionChoiceUsesItsOwnMenu(t *testing.T) {
	ex := &nativeAnswerExecutor{paneExecutor: paneExecutor{pane: nativeClaudeQuestion, inMode: "0"}}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	installNativeClaudeRecord(t, tm, nativeClaudeQuestion)
	p, err := tm.Pending("q")
	if err != nil || p == nil {
		t.Fatalf("Pending=%+v,%v", p, err)
	}
	err = tm.Respond("q", runtime.InteractionResponse{RequestID: p.RequestID, Action: "answer", Text: "Owl"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.literal) != 0 || !slices.Equal(ex.keys, []string{"Down", "Enter"}) {
		t.Fatalf("literal=%q keys=%q; must select Owl once without typing into the next prompt", ex.literal, ex.keys)
	}
}

func TestNativeCodexCollapsedQuestionUsesOwnRollout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	row := `{"type":"response_item","payload":{"type":"function_call","name":"request_user_input_async","call_id":"call-own-question","arguments":"{\"questions\":[{\"title\":\"Which animal should be the test mascot?\",\"options\":[\"Otter\",\"Owl\"]}]}"}}` + "\n"
	if err := os.WriteFile(path, []byte(row), 0o600); err != nil {
		t.Fatal(err)
	}
	tm, ex := questionTmux(nativeCodexCollapsed)
	tm.codexRolloutHeld = func(pid int) string {
		if pid != 4242 {
			t.Errorf("pane pid=%d", pid)
		}
		return path
	}
	p, err := tm.Pending("q")
	if err != nil || p == nil || p.Kind != "question" || !strings.Contains(p.RequestID, "call-own-question") || !slices.Equal(p.Options, []string{"Otter", "Owl"}) {
		t.Fatalf("Pending=%+v,%v; expected the live process's native question", p, err)
	}
	requireNoKeys(t, ex.calls, false)
}

func TestNativeQuestionFreeTextUsesEditor(t *testing.T) {
	ex := &nativeAnswerExecutor{paneExecutor: paneExecutor{pane: nativeClaudeQuestion, inMode: "0"}}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	installNativeClaudeRecord(t, tm, nativeClaudeQuestion)
	p, err := tm.Pending("q")
	if err != nil || p == nil {
		t.Fatalf("Pending=%+v,%v", p, err)
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: p.RequestID, Action: "answer", Text: "Fox"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ex.literal, []string{"Fox"}) || !slices.Equal(ex.keys, []string{"Down", "Down", "Enter"}) {
		t.Fatalf("literal=%q keys=%q", ex.literal, ex.keys)
	}
}

func TestNativeQuestionDraftDoesNotChangeIdentity(t *testing.T) {
	before := parseQuestionDialog(strings.Split(nativeClaudeQuestion, "\n"))
	draft := strings.ReplaceAll(nativeClaudeQuestion, "❯ 1. Otter", "  1. Otter")
	draft = strings.ReplaceAll(draft, "  3. Type something.", "❯ 3. Fox")
	after := parseQuestionDialog(strings.Split(draft, "\n"))
	if before == nil || after == nil || before.requestID() != after.requestID() {
		t.Fatalf("draft must preserve the question identity: before=%+v after=%+v", before, after)
	}
}

func TestNativeQuestionChoiceWaitsForFocusedRow(t *testing.T) {
	old := respondVerifyTimeout
	respondVerifyTimeout = time.Millisecond
	t.Cleanup(func() { respondVerifyTimeout = old })
	tm, ex := nativeQuestionTmux(t, nativeClaudeQuestion)
	p, err := tm.Pending("q")
	if err != nil || p == nil {
		t.Fatalf("Pending=%+v,%v", p, err)
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: p.RequestID, Action: "answer", Text: "Owl"}); err == nil {
		t.Fatal("unconfirmed focus must not be submitted")
	}
	for _, call := range ex.calls {
		if slices.Contains(call, "Enter") {
			t.Fatal("Enter submitted the old focused option")
		}
	}
}

func nativeQuestionRollout(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	row := `{"type":"response_item","payload":{"type":"function_call","name":"request_user_input_async","call_id":"call-own-question","arguments":"{\"questions\":[{\"title\":\"Which animal should be the test mascot?\",\"options\":[\"Otter\",\"Owl\"]}]}"}}` + "\n"
	if err := os.WriteFile(path, []byte(row), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeCodexQuestionsAreNotIdle(t *testing.T) {
	for _, pane := range []string{nativeCodexQuestion, nativeCodexCollapsed} {
		if PaneShowsIdlePrompt(strings.Split(pane, "\n"), codexPromptPrefix) {
			t.Fatal("native question reads as an idle composer")
		}
	}
}

func TestNativeCodexAnsweredAndRepeatedQuestions(t *testing.T) {
	path := nativeQuestionRollout(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	answered := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<send_user_message_question_reply>\n[{\"answer\":\"Fox\",\"questionItemId\":\"[\\\"request_user_input_async\\\",\\\"call-own-question\\\",0]\"}]\n</send_user_message_question_reply>"}]}}` + "\n"
	old, err := codexQuestionsIn(string(data))
	if err != nil || len(old) != 1 {
		t.Fatalf("first=%+v,%v", old, err)
	}
	nextRow := strings.ReplaceAll(string(data), "call-own-question", "call-repeated-question")
	next, err := codexQuestionsIn(string(data) + answered + nextRow)
	if err != nil || len(next) != 1 || next[0].requestID() == old[0].requestID() {
		t.Fatalf("answered request must not revive; repeated question has a new identity: %+v,%v", next, err)
	}
}

func TestNativeCodexQuestionRecordsFailClosed(t *testing.T) {
	for _, tail := range []string{`{"type":"response_item","payload":`, "{oops}\n"} {
		if _, err := codexQuestionsIn(tail); err == nil {
			t.Fatal("incomplete records must refuse observation")
		}
	}
}

func TestNativeCodexBoundAnswerOpensAndUsesItsNativePanel(t *testing.T) {
	for _, answer := range []string{"Owl", "Fox"} {
		t.Run(answer, func(t *testing.T) {
			ex := &nativeAnswerExecutor{codex: true, paneExecutor: paneExecutor{pane: nativeCodexCollapsed, inMode: "0"}}
			path := nativeQuestionRollout(t)
			tm := &Tmux{cfg: DefaultConfig(), exec: ex, codexRolloutHeld: func(int) string { return path }}
			pending, err := tm.Pending("q")
			if err != nil || pending == nil {
				t.Fatalf("Pending=%+v,%v", pending, err)
			}
			if err := tm.Respond("q", runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Text: answer}); err != nil {
				t.Fatal(err)
			}
			want := []string{"2"}
			if answer == "Fox" {
				want = []string{"3", "Fox"}
			}
			if !slices.Equal(ex.literal, want) {
				t.Fatalf("typed=%q want=%q", ex.literal, want)
			}
			opened := false
			for _, call := range ex.calls {
				opened = opened || slices.Contains(call, "S-Left")
			}
			if !opened {
				t.Fatal("answer never opened the native question panel")
			}
			if answer == "Owl" && len(ex.keys) != 0 {
				t.Fatalf("choice auto-submits; extra Enter typed into next prompt: %q", ex.keys)
			}
		})
	}
}

func TestNativeCodexAbortedTurnDropsUnansweredQuestions(t *testing.T) {
	path := nativeQuestionRollout(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	terminal := `{"type":"event_msg","payload":{"type":"turn_aborted"}}` + "\n"
	next := strings.ReplaceAll(string(data), "call-own-question", "call-next-turn")
	questions, err := codexQuestionsIn(string(data) + terminal + next)
	if err != nil || len(questions) != 1 || !strings.Contains(questions[0].requestID(), "call-next-turn") {
		t.Fatalf("aborted previous-turn question must not poison the next turn: %+v,%v", questions, err)
	}
}

func TestNativeCodexOtherIsAlsoAValidAuthoredChoice(t *testing.T) {
	pane := strings.ReplaceAll(nativeCodexQuestion, "1. Otter", "1. Other")
	pane = strings.ReplaceAll(pane, "3. Other", "3. Other (write an answer)")
	q := codexQuestionPanel(strings.Split(pane, "\n"))
	if q == nil || !slices.Equal(q.answerChoices(), []string{"Other", "Owl"}) || q.textOption() != 3 {
		t.Fatalf("model choice Other must remain distinct from the native text editor: %+v", q)
	}
}

func TestNativeQuestionExposesCompletePrompt(t *testing.T) {
	question := strings.Repeat("A detailed question. ", 30)
	tm, _ := nativeQuestionTmux(t, strings.ReplaceAll(nativeClaudeQuestion, "Which animal should be the test mascot?", question))
	pending, err := tm.Pending("q")
	if err != nil || pending == nil || !strings.Contains(pending.Prompt, strings.TrimSpace(question)) {
		t.Fatalf("native question prompt was truncated: %+v,%v", pending, err)
	}
}

func TestNativeClaudeRepeatedQuestionHasANewRequestIdentity(t *testing.T) {
	record := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_first","name":"AskUserQuestion","input":{"questions":[{"header":"Mascot","question":"Which animal should be the test mascot?","options":[{"label":"Otter"},{"label":"Owl"}]}]}}]}}` + "\n"
	tm, _ := nativeQuestionTmux(t, nativeClaudeQuestion)
	tm.claudeQuestionTranscript = func(string) (string, error) { return record, nil }
	first, err := tm.Pending("q")
	if err != nil || first == nil {
		t.Fatalf("first=%+v,%v", first, err)
	}
	record = strings.ReplaceAll(record, "toolu_first", "toolu_second")
	second, err := tm.Pending("q")
	if err != nil || second == nil || first.RequestID == second.RequestID {
		t.Fatalf("identical question raised again must reject the old response: first=%+v second=%+v err=%v", first, second, err)
	}
	ex := tm.exec.(*paneExecutor)
	ex.calls = nil
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: first.RequestID, Action: "answer", Text: "Owl"}); !errors.Is(err, runtime.ErrInteractionGone) {
		t.Fatalf("old response=%v", err)
	}
	requireNoKeys(t, ex.calls, false)
}

func nativeQuestionTmux(t *testing.T, pane string) (*Tmux, *paneExecutor) {
	t.Helper()
	tm, ex := questionTmux(pane)
	installNativeClaudeRecord(t, tm, pane)
	return tm, ex
}

func installNativeClaudeRecord(t *testing.T, tm *Tmux, pane string) {
	t.Helper()
	q := parseQuestionDialog(strings.Split(pane, "\n"))
	if q == nil {
		return
	}
	var options []map[string]string
	for _, label := range q.answerChoices() {
		options = append(options, map[string]string{"label": label})
	}
	data, err := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "toolu_native_fixture", "name": "AskUserQuestion", "input": map[string]any{"questions": []any{map[string]any{"header": q.Header, "question": q.Question, "options": options}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	tm.claudeQuestionTranscript = func(string) (string, error) { return string(data) + "\n", nil }
}

func TestNativeQuestionChangedWhilePastingIsNotSubmitted(t *testing.T) {
	ex := &nativeAnswerExecutor{advanceOnPaste: true, paneExecutor: paneExecutor{pane: nativeClaudeQuestion, inMode: "0"}}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	installNativeClaudeRecord(t, tm, nativeClaudeQuestion)
	pending, err := tm.Pending("q")
	if err != nil || pending == nil {
		t.Fatalf("Pending=%+v,%v", pending, err)
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Text: "Fox"}); !errors.Is(err, runtime.ErrInteractionGone) {
		t.Fatalf("changed question must refuse submit: %v", err)
	}
	if slices.Contains(ex.keys, "Enter") {
		t.Fatalf("submitted Enter after question disappeared: %q", ex.keys)
	}
}

func TestNativeQuestionAnswerKeysStayUnderIncarnationFence(t *testing.T) {
	for _, respawn := range []bool{false, true} {
		t.Run(map[bool]string{false: "answer", true: "respawn before submit"}[respawn], func(t *testing.T) {
			m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
			focused, answered := false, false
			m.screen = func(*fenceModel) string {
				if answered {
					return "● Owl.\n❯ "
				}
				if focused {
					pane := strings.ReplaceAll(nativeClaudeQuestion, "❯ 1. Otter", "  1. Otter")
					return strings.ReplaceAll(pane, "  2. Owl", "❯ 2. Owl")
				}
				return nativeClaudeQuestion
			}
			m.onEvent = func(_ *fenceModel, event string) {
				focused = focused || event == "key:Down"
				answered = answered || event == "key:Enter"
			}
			if respawn {
				m.beforeGuard = func(m *fenceModel, n int) {
					if n == 2 {
						m.dead = true
					}
				}
			}
			provider := fenceModelProvider(m)
			installNativeClaudeRecord(t, provider.tm, nativeClaudeQuestion)
			pending, err := provider.Pending(fenceModelSession)
			if err != nil || pending == nil {
				t.Fatalf("Pending=%+v,%v", pending, err)
			}
			err = provider.RespondFenced(context.Background(), fenceModelSession, testFence, runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Text: "Owl"})
			bare, events, _ := m.snapshot()
			if len(bare) != 0 {
				t.Fatalf("question keys bypassed pane guard: %q", bare)
			}
			if respawn {
				var miss *runtime.FenceMismatchError
				if !errors.As(err, &miss) || !slices.Equal(events, []string{"key:Down"}) {
					t.Fatalf("respawn error=%v events=%q", err, events)
				}
			} else if err != nil || !slices.Equal(events, []string{"key:Down", "key:Enter"}) {
				t.Fatalf("answer error=%v events=%q", err, events)
			}
		})
	}
}

// Claude 2.1.308 does not flush the assistant tool invocation while its
// native question is waiting. Missing answer identity must not hide that
// visible input owner or let ordinary messages answer it.
func TestNativeClaudeUnflushedQuestionRemainsVisibleAndGuarded(t *testing.T) {
	tm, ex := questionTmux(nativeClaudeQuestion)
	tm.claudeQuestionTranscript = func(string) (string, error) {
		return `{"type":"user","message":{"content":"ask a native question"}}` + "\n" + `{"type":"attachment"}` + "\n", nil
	}
	pending, err := tm.Pending("q")
	if err != nil || pending == nil || pending.Kind != "question" {
		t.Fatalf("visible native question lost: %+v, %v", pending, err)
	}
	if pending.Metadata["answer_action"] != "" || pending.Metadata["answer_unavailable"] == "" {
		t.Fatalf("unbound question advertised an answer: %+v", pending)
	}
	if err := tm.checkNoApprovalPrompt("q"); !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("message guard = %v", err)
	}
	requireNoKeys(t, ex.calls, false)
}

func projectedClaudeQuestion() claudeQuestionProjection {
	var projection claudeQuestionProjection
	_ = json.Unmarshal([]byte(`{"version":1,"session_id":"native-session","prompt_id":"prompt-first","tool_use_id":"toolu_first","questions":[{"header":"Mascot","question":"Which animal should be the test mascot?","options":[{"label":"Otter"},{"label":"Owl"}]}]}`), &projection)
	return projection
}

func TestNativeClaudePreToolQuestionBindsBeforeAssistantRecord(t *testing.T) {
	tm, ex := questionTmux(nativeClaudeQuestion)
	tm.claudeQuestionTranscript = func(string) (string, error) {
		return `{"type":"user","sessionId":"native-session","promptId":"prompt-first","message":{"content":"ask a native question"}}` + "\n", nil
	}
	tm.claudeQuestionProjections = func(string) ([]claudeQuestionProjection, error) {
		return []claudeQuestionProjection{projectedClaudeQuestion()}, nil
	}
	pending, err := tm.Pending("q")
	if err != nil || pending == nil || pending.Metadata["answer_action"] != "answer" || pending.Metadata["free_text"] != "true" {
		t.Fatalf("native pre-tool invocation must bind an unflushed question: %+v,%v", pending, err)
	}
	requireNoKeys(t, ex.calls, false)
}

func TestNativeClaudeProjectionRejectsStaleOrAmbiguousInvocation(t *testing.T) {
	for _, scenario := range []string{"wrong prompt", "wrong session", "completed", "ambiguous", "new unidentified prompt"} {
		t.Run(scenario, func(t *testing.T) {
			tm, ex := questionTmux(nativeClaudeQuestion)
			projection := projectedClaudeQuestion()
			tail := `{"type":"user","sessionId":"native-session","promptId":"prompt-first","message":{"content":"ask a native question"}}` + "\n"
			switch scenario {
			case "wrong prompt":
				projection.PromptID = "previous-prompt"
			case "wrong session":
				projection.SessionID = "previous-session"
			case "completed":
				tail += `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_first","content":"answered"}]}}` + "\n"
			case "new unidentified prompt":
				tail += `{"type":"user","message":{"content":"a new prompt with no native identity"}}` + "\n"
			}
			projections := []claudeQuestionProjection{projection}
			if scenario == "ambiguous" {
				other := projection
				other.ToolUseID = "toolu_other"
				projections = append(projections, other)
			}
			tm.claudeQuestionTranscript = func(string) (string, error) { return tail, nil }
			tm.claudeQuestionProjections = func(string) ([]claudeQuestionProjection, error) { return projections, nil }
			pending, err := tm.Pending("q")
			if err != nil || pending == nil || pending.Metadata["answer_action"] != "" || pending.Metadata["answer_unavailable"] == "" {
				t.Fatalf("unproven invocation must stay visible but unanswerable: %+v,%v", pending, err)
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
}

func TestNativeClaudePromptIdentityPreventsReusedToolIDAnswer(t *testing.T) {
	tm, ex := questionTmux(nativeClaudeQuestion)
	projection := projectedClaudeQuestion()
	tm.claudeQuestionTranscript = func(string) (string, error) {
		return `{"type":"user","sessionId":"native-session","promptId":"` + projection.PromptID + `","message":{"content":"ask"}}` + "\n", nil
	}
	tm.claudeQuestionProjections = func(string) ([]claudeQuestionProjection, error) { return []claudeQuestionProjection{projection}, nil }
	first, err := tm.Pending("q")
	if err != nil || first == nil {
		t.Fatalf("first=%+v,%v", first, err)
	}
	projection.PromptID = "prompt-next"
	second, err := tm.Pending("q")
	if err != nil || second == nil {
		t.Fatalf("second=%+v,%v", second, err)
	}
	if first.RequestID == second.RequestID {
		t.Fatal("same tool ID in a different native prompt revived the old request")
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: first.RequestID, Action: "answer", Text: "Owl"}); !errors.Is(err, runtime.ErrInteractionGone) {
		t.Fatalf("old request=%v", err)
	}
	requireNoKeys(t, ex.calls, false)
}

func TestNativeClaudeNewPromptRetiresUnfinishedTranscriptInvocation(t *testing.T) {
	tm, ex := questionTmux(nativeClaudeQuestion)
	tm.claudeQuestionTranscript = func(string) (string, error) {
		return `{"type":"user","sessionId":"native-session","promptId":"old","message":{"content":"ask"}}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_old","name":"AskUserQuestion","input":{"questions":[{"header":"Mascot","question":"Which animal should be the test mascot?","options":[{"label":"Otter"},{"label":"Owl"}]}]}}]}}` + "\n" +
			`{"type":"user","sessionId":"native-session","promptId":"new","message":{"content":"ask again"}}` + "\n", nil
	}
	tm.claudeQuestionProjections = func(string) ([]claudeQuestionProjection, error) { return nil, nil }
	pending, err := tm.Pending("q")
	if err != nil || pending == nil || pending.Metadata["answer_action"] != "" {
		t.Fatalf("old prompt revived an unfinished invocation: %+v,%v", pending, err)
	}
	requireNoKeys(t, ex.calls, false)
}

// Claude2.1.308 accepts authored choices immediately on a numeric shortcut.
// Focus navigation must not report a completed native answer as stale.
func TestNativeClaudeChoiceDoesNotUseAutoSubmittingDigit(t *testing.T) {
	ex := &nativeAnswerExecutor{paneExecutor: paneExecutor{pane: nativeClaudeQuestion, inMode: "0"}, claudeDigitsSubmit: true}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	installNativeClaudeRecord(t, tm, nativeClaudeQuestion)
	pending, err := tm.Pending("q")
	if err != nil || pending == nil {
		t.Fatalf("pending=%+v,%v", pending, err)
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Text: "Owl"}); err != nil {
		t.Fatalf("authored answer was delivered but reported stale: %v", err)
	}
	if slices.Contains(ex.literal, "2") || !slices.Equal(ex.keys, []string{"Down", "Enter"}) {
		t.Fatalf("must focus and confirm before one submit: literal=%q keys=%q", ex.literal, ex.keys)
	}
}

// These three panes follow the retained native Claude2.1.308 two-question
// trial: the second question, its complete review, and the received answer.
const nativeClaudeFormColor = `←  ☒ Animal  ☐ Color  ✔ Submit  →
Which qualification color?
❯ 1. Blue
     Choose blue
  2. Green
     Choose green
  3. Type something.
────────────────────────────────────────────────────────────────────────────────
  4. Chat about this
Enter to select · Tab/Arrow keys to navigate · Esc to cancel
`

const nativeClaudeFormReview = `←  ☒ Animal  ☒ Color  ✔ Submit  →
Review your answers
 ● Which qualification animal?
   → Owl
 ● Which qualification color?
   → Green
Ready to submit your answers?
❯ 1. Submit answers
  2. Cancel
`

func nativeClaudeTwoFormProjection(t *testing.T) claudeQuestionProjection {
	t.Helper()
	var projection claudeQuestionProjection
	if err := json.Unmarshal([]byte(`{"version":1,"session_id":"native-form-session","prompt_id":"native-form-prompt","tool_use_id":"toolu_native_form","questions":[{"header":"Animal","question":"Which qualification animal?","options":[{"label":"Otter"},{"label":"Owl"}],"multiSelect":false},{"header":"Color","question":"Which qualification color?","options":[{"label":"Blue"},{"label":"Green"}],"multiSelect":false}]}`), &projection); err != nil {
		t.Fatal(err)
	}
	return projection
}

type nativeClaudeFormExecutor struct {
	nativeAnswerExecutor
	entered     int
	review      string
	afterAnswer func()
}

func (e *nativeClaudeFormExecutor) execute(args []string) (string, error) {
	if slices.Contains(args, "send-keys") && !slices.Contains(args, "-X") && args[len(args)-1] == "Enter" {
		e.entered++
		e.keys = append(e.keys, "Enter")
		if e.entered == 1 {
			e.pane = e.review
			if e.afterAnswer != nil {
				e.afterAnswer()
			}
		} else {
			e.pane = "● Answers received.\n❯ "
		}
		return e.paneExecutor.execute(args)
	}
	return e.nativeAnswerExecutor.execute(args)
}

func (e *nativeClaudeFormExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

func nativeClaudeFormTmux(t *testing.T, review string) (*Tmux, *nativeClaudeFormExecutor, *claudeQuestionProjection) {
	t.Helper()
	ex := &nativeClaudeFormExecutor{nativeAnswerExecutor: nativeAnswerExecutor{paneExecutor: paneExecutor{pane: nativeClaudeFormColor, inMode: "0"}}, review: review}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	projection := nativeClaudeTwoFormProjection(t)
	tm.claudeQuestionTranscript = func(string) (string, error) {
		return `{"type":"user","sessionId":"native-form-session","promptId":"native-form-prompt","message":{"content":"ask this native form"}}` + "\n", nil
	}
	tm.claudeQuestionProjections = func(string) ([]claudeQuestionProjection, error) { return []claudeQuestionProjection{projection}, nil }
	return tm, ex, &projection
}

func TestNativeClaudeLastFormAnswerSubmitsItsBoundReview(t *testing.T) {
	tm, ex, _ := nativeClaudeFormTmux(t, nativeClaudeFormReview)
	pending, err := tm.Pending("q")
	if err != nil || pending == nil || pending.Metadata["answer_action"] != "answer" {
		t.Fatalf("Pending=%+v,%v", pending, err)
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Text: "Green"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ex.keys, []string{"Down", "Enter", "Enter"}) || len(ex.literal) != 0 {
		t.Fatalf("last answer must submit only its own complete review once: keys=%q literal=%q", ex.keys, ex.literal)
	}
}

func TestNativeClaudeReviewRemainsReadOnlyToPendingAndNudges(t *testing.T) {
	tm, ex, _ := nativeClaudeFormTmux(t, nativeClaudeFormReview)
	ex.pane = nativeClaudeFormReview
	pending, err := tm.Pending("q")
	if err != nil || pending == nil || pending.Kind != "question" {
		t.Fatalf("Pending=%+v,%v", pending, err)
	}
	if err := tm.checkNoApprovalPrompt("q"); !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("nudge guard=%v", err)
	}
	requireNoKeys(t, ex.calls, false)
}

func TestNativeClaudeLastFormAnswerRefusesChangedReviewOrInvocation(t *testing.T) {
	for _, scenario := range []string{"changed last answer", "incomplete form", "cancel focused", "new native invocation", "changed native schema"} {
		t.Run(scenario, func(t *testing.T) {
			review := nativeClaudeFormReview
			switch scenario {
			case "changed last answer":
				review = strings.ReplaceAll(review, "→ Green", "→ Blue")
			case "incomplete form":
				review = strings.ReplaceAll(review, " ● Which qualification animal?\n   → Owl\n", "")
			case "cancel focused":
				review = strings.ReplaceAll(review, "❯ 1. Submit answers", "  1. Submit answers")
				review = strings.ReplaceAll(review, "  2. Cancel", "❯ 2. Cancel")
			}
			tm, ex, projection := nativeClaudeFormTmux(t, review)
			if scenario == "new native invocation" {
				ex.afterAnswer = func() { projection.ToolUseID = "toolu_next_form" }
			}
			if scenario == "changed native schema" {
				ex.afterAnswer = func() { projection.Questions[0].Question = "A different first question?" }
			}
			pending, err := tm.Pending("q")
			if err != nil || pending == nil {
				t.Fatalf("Pending=%+v,%v", pending, err)
			}
			err = tm.Respond("q", runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Text: "Green"})
			if err == nil {
				t.Fatal("unverified or changed review was reported submitted")
			}
			if !slices.Equal(ex.keys, []string{"Down", "Enter"}) {
				t.Fatalf("review was answered without its proof: %q", ex.keys)
			}
		})
	}
}

func TestNativeClaudeFormReviewSubmitStaysUnderIncarnationFence(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	stage, focused := 0, false
	m.screen = func(*fenceModel) string {
		if stage >= 2 {
			return "● Answers received.\n❯ "
		}
		if stage == 1 {
			return nativeClaudeFormReview
		}
		if focused {
			return strings.ReplaceAll(strings.ReplaceAll(nativeClaudeFormColor, "❯ 1. Blue", "  1. Blue"), "  2. Green", "❯ 2. Green")
		}
		return nativeClaudeFormColor
	}
	m.onEvent = func(_ *fenceModel, event string) {
		focused = focused || event == "key:Down"
		if event == "key:Enter" {
			stage++
		}
	}
	m.beforeGuard = func(m *fenceModel, n int) {
		if n == 3 {
			m.dead = true
		}
	}
	provider := fenceModelProvider(m)
	projection := nativeClaudeTwoFormProjection(t)
	provider.tm.claudeQuestionTranscript = func(string) (string, error) {
		return `{"type":"user","sessionId":"native-form-session","promptId":"native-form-prompt","message":{"content":"ask"}}` + "\n", nil
	}
	provider.tm.claudeQuestionProjections = func(string) ([]claudeQuestionProjection, error) { return []claudeQuestionProjection{projection}, nil }
	pending, err := provider.Pending(fenceModelSession)
	if err != nil || pending == nil {
		t.Fatalf("Pending=%+v,%v", pending, err)
	}
	err = provider.RespondFenced(context.Background(), fenceModelSession, testFence, runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Text: "Green"})
	var miss *runtime.FenceMismatchError
	bare, events, _ := m.snapshot()
	if !errors.As(err, &miss) || len(bare) != 0 || !slices.Equal(events, []string{"key:Down", "key:Enter"}) {
		t.Fatalf("review submit escaped incarnation guard: error=%v bare=%q events=%q", err, bare, events)
	}
}

// Unchecked rows and Submit match the retained native Claude2.1.308 capture.
// The checked glyph also occurs in the maintained2.1.314 native fixture.
const nativeClaudeMultiplePane = `←  ☐ Animals  ✔ Submit  →
Which qualification animals?
❯ 1. [ ] Otter
         Choose otter
  2. [ ] Owl
         Choose owl
  3. [ ] Fox
         Choose fox
  4. [ ] Type something
     Submit
────────────────────────────────────────────────────────────────────────────────
  5. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel
`

type nativeMultipleExecutor struct {
	paneExecutor
	focus       int
	checked     map[int]bool
	stage       int
	keys        []string
	literal     []string
	labels      []string
	afterToggle func()
}

func (e *nativeMultipleExecutor) render() string {
	if e.stage == 2 {
		return "● Multiple answers received.\n❯ "
	}
	if e.stage == 1 {
		var selected []string
		for i, label := range e.labels {
			if e.checked[i+1] {
				selected = append(selected, label)
			}
		}
		return "←  ☒ Animals  ✔ Submit  →\nReview your answers\n ● Which qualification animals?\n   → " + strings.Join(selected, ", ") + "\nReady to submit your answers?\n❯ 1. Submit answers\n  2. Cancel\n"
	}
	pane := nativeClaudeMultiplePane
	for _, checked := range e.checked {
		if checked {
			pane = strings.Replace(pane, "☐ Animals", "☒ Animals", 1)
			break
		}
	}
	for i, label := range e.labels {
		number := i + 1
		before := fmt.Sprintf("%d. [ ] %s", number, []string{"Otter", "Owl", "Fox"}[i])
		mark := "[ ]"
		if e.checked[number] {
			mark = "[✔]"
		}
		pane = strings.ReplaceAll(pane, before, fmt.Sprintf("%d. %s %s", number, mark, label))
	}
	rows := strings.Split(pane, "\n")
	for i, line := range rows {
		if match := askUserQuestionOptionRe.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
			mark := "  "
			if match[2] == fmt.Sprint(e.focus) {
				mark = "❯ "
			}
			rows[i] = mark + match[2] + ". " + match[3]
		}
		if strings.TrimSpace(line) == "Submit" && e.focus == 0 {
			rows[i] = "❯    Submit"
		}
	}
	return strings.Join(rows, "\n")
}

func (e *nativeMultipleExecutor) execute(args []string) (string, error) {
	if slices.Contains(args, "send-keys") && !slices.Contains(args, "-X") {
		key := args[len(args)-1]
		if slices.Contains(args, "-l") {
			e.literal = append(e.literal, key)
		} else {
			e.keys = append(e.keys, key)
			if key == "Down" || key == "Up" {
				numbers := []int{1, 2, 3, 4, 0, 5}
				index := slices.Index(numbers, e.focus)
				if key == "Down" {
					index++
				} else {
					index--
				}
				if index >= 0 && index < len(numbers) {
					e.focus = numbers[index]
				}
			}
			if key == "Enter" {
				switch {
				case e.stage == 1:
					e.stage = 2
				case e.focus == 0:
					e.stage = 1
				case e.focus >= 1 && e.focus <= 3:
					e.checked[e.focus] = !e.checked[e.focus]
					if e.afterToggle != nil {
						e.afterToggle()
					}
				}
			}
		}
	}
	e.pane = e.render()
	return e.paneExecutor.execute(args)
}

func (e *nativeMultipleExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

func nativeMultipleTmux(t *testing.T) (*Tmux, *nativeMultipleExecutor, *claudeQuestionProjection) {
	t.Helper()
	ex := &nativeMultipleExecutor{paneExecutor: paneExecutor{inMode: "0"}, focus: 1, checked: map[int]bool{}, labels: []string{"Otter", "Owl", "Fox"}}
	ex.pane = ex.render()
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	var projection claudeQuestionProjection
	if err := json.Unmarshal([]byte(`{"version":1,"session_id":"native-multi-session","prompt_id":"native-multi-prompt","tool_use_id":"toolu_native_multi","questions":[{"header":"Animals","question":"Which qualification animals?","multiSelect":true,"options":[{"label":"Otter"},{"label":"Owl"},{"label":"Fox"}]}]}`), &projection); err != nil {
		t.Fatal(err)
	}
	tm.claudeQuestionTranscript = func(string) (string, error) {
		return `{"type":"user","sessionId":"native-multi-session","promptId":"native-multi-prompt","message":{"content":"ask multiple"}}` + "\n", nil
	}
	tm.claudeQuestionProjections = func(string) ([]claudeQuestionProjection, error) { return []claudeQuestionProjection{projection}, nil }
	return tm, ex, &projection
}

func TestNativeClaudeTypedMultipleChoicesExposeAuthoredSchemaWithoutInput(t *testing.T) {
	tm, ex, _ := nativeMultipleTmux(t)
	pending, err := tm.Pending("q")
	if err != nil || pending == nil || !slices.Equal(pending.Options, []string{"Otter", "Owl", "Fox"}) || pending.Metadata["multi_select"] != "true" || pending.Metadata["answer_action"] != "answer" || pending.Metadata["free_text"] != "false" {
		t.Fatalf("multiple-choice native schema=%+v,%v", pending, err)
	}
	requireNoKeys(t, ex.calls, false)
	if err := tm.checkNoApprovalPrompt("q"); !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("question nudge guard=%v", err)
	}
	requireNoKeys(t, ex.calls, false)
}

// Submit is unnumbered but appears before Chat about this in the captured
// provider pane. Navigation must preserve that row order, including when the
// cursor is already on Submit; no answer is authorized by this observation.
func TestNativeClaudeTypedMultipleChoicesKeepSubmitInVisibleOrder(t *testing.T) {
	tm, ex, _ := nativeMultipleTmux(t)
	for _, focus := range []int{1, 0} {
		ex.focus = focus
		ex.pane = ex.render()
		dialog, err := tm.dialogInPane("q")
		if err != nil || dialog.question == nil {
			t.Fatalf("dialog=%+v,%v", dialog, err)
		}
		q := dialog.question
		if !slices.Equal(q.Numbers, []int{1, 2, 3, 4, 0, 5}) ||
			!slices.Equal(q.Choices, []string{"Otter", "Owl", "Fox", "Type something.", "Submit", "Chat about this"}) {
			t.Fatalf("focus=%d reordered visible rows: numbers=%v choices=%q", focus, q.Numbers, q.Choices)
		}
		if q.FocusedNumber != focus || focus == 0 && q.Focused != "Submit" {
			t.Fatalf("focus=%d parsed focus=%d,%q", focus, q.FocusedNumber, q.Focused)
		}
	}
	requireNoKeys(t, ex.calls, false)
}

func TestNativeClaudeTypedMultipleChoicesSelectOnlyExactAuthoredSet(t *testing.T) {
	tm, ex, _ := nativeMultipleTmux(t)
	pending, err := tm.Pending("q")
	if err != nil || pending == nil {
		t.Fatalf("Pending=%+v,%v", pending, err)
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Answers: []string{"Fox", "Otter"}}); err != nil {
		t.Fatal(err)
	}
	if !ex.checked[1] || ex.checked[2] || !ex.checked[3] || ex.stage != 2 || len(ex.literal) != 0 {
		t.Fatalf("typed selection lost/encoded: checked=%v stage=%d literal=%q", ex.checked, ex.stage, ex.literal)
	}
	if !slices.Equal(ex.keys, []string{"Enter", "Down", "Down", "Enter", "Down", "Down", "Enter", "Enter"}) {
		t.Fatalf("unexpected navigation/toggle/submit: %q", ex.keys)
	}
}

func TestNativeClaudeTypedMultipleChoicesRejectMalformedBeforeKeys(t *testing.T) {
	for name, response := range map[string]runtime.InteractionResponse{
		"unknown":       {Action: "answer", Answers: []string{"Badger"}},
		"duplicate":     {Action: "answer", Answers: []string{"Otter", "Otter"}},
		"mixed text":    {Action: "answer", Text: "Otter", Answers: []string{"Fox"}},
		"empty":         {Action: "answer", Answers: []string{}},
		"free text":     {Action: "answer", Text: "Other animal"},
		"approval verb": {Action: "approve", Answers: []string{"Otter"}},
	} {
		t.Run(name, func(t *testing.T) {
			tm, ex, _ := nativeMultipleTmux(t)
			pending, err := tm.Pending("q")
			if err != nil || pending == nil {
				t.Fatalf("Pending=%+v,%v", pending, err)
			}
			response.RequestID = pending.RequestID
			if err := tm.Respond("q", response); err == nil {
				t.Fatal("malformed selection accepted")
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
}

func TestNativeClaudeTypedMultipleChoicesSchemaChangeRefusesStaleAnswer(t *testing.T) {
	tm, ex, projection := nativeMultipleTmux(t)
	first, err := tm.Pending("q")
	if err != nil || first == nil {
		t.Fatalf("Pending=%+v,%v", first, err)
	}
	projection.Questions[0].Options[2].Label = "Badger"
	ex.labels[2] = "Badger"
	ex.pane = ex.render()
	second, err := tm.Pending("q")
	if err != nil || second == nil || first.RequestID == second.RequestID {
		t.Fatalf("changed authored schema reused answer identity: first=%+v next=%+v,%v", first, second, err)
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: first.RequestID, Action: "answer", Answers: []string{"Otter"}}); !errors.Is(err, runtime.ErrInteractionGone) {
		t.Fatalf("stale native schema=%v", err)
	}
	requireNoKeys(t, ex.calls, false)
}

func TestNativeClaudeTypedMultipleChoicesStopAfterSchemaChangesDuringToggle(t *testing.T) {
	tm, ex, projection := nativeMultipleTmux(t)
	ex.afterToggle = func() { projection.Questions[0].Options[2].Label = "Badger"; ex.labels[2] = "Badger" }
	pending, err := tm.Pending("q")
	if err != nil || pending == nil {
		t.Fatalf("Pending=%+v,%v", pending, err)
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Answers: []string{"Otter", "Fox"}}); !errors.Is(err, runtime.ErrInteractionGone) {
		t.Fatalf("schema changed during response=%v", err)
	}
	if !slices.Equal(ex.keys, []string{"Enter"}) {
		t.Fatalf("continued after schema changed: %q", ex.keys)
	}
}

func TestNativeClaudeTypedMultipleChoiceKeysKeepIncarnationFence(t *testing.T) {
	_, state, projection := nativeMultipleTmux(t)
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	m.screen = func(*fenceModel) string { return state.render() }
	m.onEvent = func(_ *fenceModel, event string) {
		if key, ok := strings.CutPrefix(event, "key:"); ok {
			_, _ = state.execute([]string{"send-keys", key})
		}
	}
	m.beforeGuard = func(m *fenceModel, n int) {
		if n == 2 {
			m.dead = true
		}
	}
	provider := fenceModelProvider(m)
	provider.tm.claudeQuestionTranscript = func(string) (string, error) {
		return `{"type":"user","sessionId":"native-multi-session","promptId":"native-multi-prompt","message":{"content":"ask multiple"}}` + "\n", nil
	}
	provider.tm.claudeQuestionProjections = func(string) ([]claudeQuestionProjection, error) { return []claudeQuestionProjection{*projection}, nil }
	pending, err := provider.Pending(fenceModelSession)
	if err != nil || pending == nil {
		t.Fatalf("Pending=%+v,%v", pending, err)
	}
	err = provider.RespondFenced(context.Background(), fenceModelSession, testFence, runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Answers: []string{"Otter", "Fox"}})
	var miss *runtime.FenceMismatchError
	bare, events, _ := m.snapshot()
	if !errors.As(err, &miss) || len(bare) != 0 || !slices.Equal(events, []string{"key:Enter"}) {
		t.Fatalf("typed selection escaped its incarnation: err=%v bare=%q events=%q", err, bare, events)
	}
}

// A desired selection set must converge from a partially completed earlier
// attempt. Blindly replaying toggles would clear Otter and leave Owl selected.
func TestNativeClaudeTypedMultipleChoicesReplacePreselectedSet(t *testing.T) {
	tm, ex, _ := nativeMultipleTmux(t)
	ex.checked[1], ex.checked[2] = true, true
	ex.pane = ex.render()
	pending, err := tm.Pending("q")
	if err != nil || pending == nil {
		t.Fatalf("Pending=%+v,%v", pending, err)
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Answers: []string{"Fox", "Otter"}}); err != nil {
		t.Fatal(err)
	}
	if !ex.checked[1] || ex.checked[2] || !ex.checked[3] || ex.stage != 2 || len(ex.literal) != 0 {
		t.Fatalf("preselected set did not converge: checked=%v stage=%d literal=%q", ex.checked, ex.stage, ex.literal)
	}
	if !slices.Equal(ex.keys, []string{"Down", "Enter", "Down", "Enter", "Down", "Down", "Enter", "Enter"}) {
		t.Fatalf("re-toggled retained choice or missed prior selection: %q", ex.keys)
	}
}

// If all desired boxes already match, a new explicit response submits them
// without toggling any authored choice. This is not an operation-journal replay.
func TestNativeClaudeTypedMultipleChoicesKeepAlreadySelectedSet(t *testing.T) {
	tm, ex, _ := nativeMultipleTmux(t)
	ex.checked[1], ex.checked[3] = true, true
	ex.pane = ex.render()
	pending, err := tm.Pending("q")
	if err != nil || pending == nil {
		t.Fatalf("Pending=%+v,%v", pending, err)
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: pending.RequestID, Action: "answer", Answers: []string{"Otter", "Fox"}}); err != nil {
		t.Fatal(err)
	}
	if !ex.checked[1] || ex.checked[2] || !ex.checked[3] || ex.stage != 2 || len(ex.literal) != 0 {
		t.Fatalf("matching set was changed: checked=%v stage=%d literal=%q", ex.checked, ex.stage, ex.literal)
	}
	if !slices.Equal(ex.keys, []string{"Down", "Down", "Down", "Down", "Enter", "Enter"}) {
		t.Fatalf("matching boxes were toggled again: %q", ex.keys)
	}
}

// A runtime-only caller is still an input boundary. Typed question answers
// must never be silently ignored while an approval owns the same pane.
func TestNativeTypedAnswersCannotGrantOrDenyApproval(t *testing.T) {
	for name, response := range map[string]runtime.InteractionResponse{
		"grant":          {Action: "approve", Answers: []string{"Otter"}},
		"deny":           {Action: "deny", Answers: []string{"Otter"}},
		"mixed text":     {Action: "approve", Text: "other input", Answers: []string{"Otter"}},
		"explicit empty": {Action: "approve", Answers: []string{}},
		"answer verb":    {Action: "answer", Answers: []string{"Otter"}},
	} {
		t.Run(name, func(t *testing.T) {
			ex := &fakeExecutor{agentPane: true, out: "assistant ready", outs: []string{approvalPromptPane(), "0", ""}}
			tm := &Tmux{cfg: DefaultConfig(), exec: ex}
			err := tm.Respond("q", response)
			if !errors.Is(err, runtime.ErrInteractionResponseInvalid) {
				t.Fatalf("typed answers reached an approval: %v", err)
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
}

func TestNativeTypedAnswersCannotBecomeNoPendingApprovalNoop(t *testing.T) {
	ex := &fakeExecutor{agentPane: true, out: "assistant ready"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	err := tm.Respond("q", runtime.InteractionResponse{Action: "approve", Answers: []string{"Otter"}})
	if !errors.Is(err, runtime.ErrInteractionResponseInvalid) {
		t.Fatalf("typed answers without a question became a successful approval/noop: %v", err)
	}
	requireNoKeys(t, ex.calls, false)
}

func TestNativeApprovalWithoutTypedAnswersKeepsExistingActions(t *testing.T) {
	for action, key := range map[string]string{"approve": "1", "deny": "3"} {
		t.Run(action, func(t *testing.T) {
			ex := &fakeExecutor{agentPane: true, out: "assistant ready", outs: []string{approvalPromptPane(), "0", ""}}
			tm := &Tmux{cfg: DefaultConfig(), exec: ex}
			if err := tm.Respond("q", runtime.InteractionResponse{Action: action}); err != nil {
				t.Fatal(err)
			}
			if keys := sentKeys(ex.calls); !slices.Equal(keys, []string{key}) {
				t.Fatalf("ordinary approval changed: sent %q, want %q", keys, key)
			}
		})
	}
}
