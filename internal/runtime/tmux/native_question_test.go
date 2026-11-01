package tmux

import (
	"context"
	"encoding/json"
	"errors"
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
	literal        []string
	keys           []string
	paste          string
	codex          bool
	advanceOnPaste bool
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
	if !slices.Equal(ex.literal, []string{"2"}) || !slices.Equal(ex.keys, []string{"Enter"}) {
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
	if !slices.Equal(ex.literal, []string{"3", "Fox"}) || !slices.Equal(ex.keys, []string{"Enter"}) {
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

func TestNativeCodexCompletedTurnDropsUnansweredQuestions(t *testing.T) {
	path := nativeQuestionRollout(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	terminal := `{"type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
	next := strings.ReplaceAll(string(data), "call-own-question", "call-next-turn")
	questions, err := codexQuestionsIn(string(data) + terminal + next)
	if err != nil || len(questions) != 1 || !strings.Contains(questions[0].requestID(), "call-next-turn") {
		t.Fatalf("expired previous-turn question must not poison the next turn: %+v,%v", questions, err)
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
	if len(ex.keys) != 0 {
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
				focused = focused || event == "text:2"
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
				if !errors.As(err, &miss) || !slices.Equal(events, []string{"text:2"}) {
					t.Fatalf("respawn error=%v events=%q", err, events)
				}
			} else if err != nil || !slices.Equal(events, []string{"text:2", "key:Enter"}) {
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
