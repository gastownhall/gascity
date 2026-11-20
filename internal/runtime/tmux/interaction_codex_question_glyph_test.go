package tmux

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Captured from the owned Codex0.162.1 / gpt-6.1-sol medium native
// qualification on frozen927737f19. The accepted async request was still
// waiting while GC's pending endpoint incorrectly reported no interaction.
// Source receipt SHA256:0bd15cffc3e2a10d005bd92629f07db111dea73affa0818d0e82e16b38ad3cb5.
const codexQuestionGlyphNativeScreen = `› For a native question transport qualification, invoke
  request_user_input_async with questions [{"title":"Which animal should be the
  qualification mascot?","options":["Otter","Owl"]}]. Wait for my native
  answer; do not answer it yourself and do not substitute plain chat for this
  native tool. After the answer arrives, reply exactly RFC8191_FIRST_<the
  answer>.


• Which animal should be the qualification mascot?

  • Otter
  • Owl

• Working (3m 58s • esc to interrupt)

• Queued follow-up inputs
  ? 1 question
    ⇧← to answer


› Ask Codex to do anything

  GPT-6.1-Sol medium · ~/rfc8191/cities/rfc-questions-44246c31d05d4943b45f2df50…
  ? for shortcuts                                      ⚠ 1 warning · f2 to view
`

func codexGlyphQuestionRollout(t *testing.T) string {
	t.Helper()
	path := nativeQuestionRollout(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Match the authored question in the actual collapsed screen while keeping
	// the existing deterministic native invocation fixture identity.
	data = []byte(strings.ReplaceAll(string(data), "test mascot", "qualification mascot"))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexQuestionGlyphPendingWhileWorkingAndMessageGuard(t *testing.T) {
	tm, ex := nativeQuestionTmux(t, codexQuestionGlyphNativeScreen)
	path := codexGlyphQuestionRollout(t)
	tm.codexRolloutHeld = func(int) string { return path }
	pending, err := tm.Pending("q")
	if err != nil || pending == nil || pending.Kind != "question" {
		t.Fatalf("accepted native async question hidden by glyph hint: pending=%+v err=%v", pending, err)
	}
	if pending.RequestID != "tmux-codex-question-call-own-question-0" ||
		!strings.Contains(pending.Prompt, "Which animal should be the qualification mascot?") ||
		!slices.Equal(pending.Options, []string{"Otter", "Owl"}) || pending.Metadata["answer_action"] != "answer" {
		t.Fatalf("native request binding/choices=%+v", pending)
	}
	requireNoKeys(t, ex.calls, false)
	if err := tm.checkNoApprovalPrompt("q"); !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("ordinary message must not answer a glyph question: %v", err)
	}
	requireNoKeys(t, ex.calls, false)
	for label, tc := range map[string]struct {
		response runtime.InteractionResponse
		want     error
	}{
		"missing request": {runtime.InteractionResponse{Action: "answer", Text: "Owl"}, runtime.ErrInteractionResponseInvalid},
		"stale request":   {runtime.InteractionResponse{RequestID: "old-question", Action: "answer", Text: "Owl"}, runtime.ErrInteractionGone},
		"approval action": {runtime.InteractionResponse{RequestID: pending.RequestID, Action: "approve"}, runtime.ErrInteractionActionUnavailable},
	} {
		t.Run(label, func(t *testing.T) {
			if err := tm.Respond("q", tc.response); !errors.Is(err, tc.want) {
				t.Fatalf("Respond=%v want=%v", err, tc.want)
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
}

func TestCodexQuestionGlyphAndTextHintsBothRequireLiveNativeQueue(t *testing.T) {
	for _, hint := range []string{"⇧← to answer", "shift+← to answer"} {
		t.Run(hint, func(t *testing.T) {
			pane := strings.ReplaceAll(codexQuestionGlyphNativeScreen, "⇧← to answer", hint)
			if got := codexQuestionCount(strings.Split(pane, "\n")); got != 1 {
				t.Fatalf("live native queue count=%d want1", got)
			}
			for name, nonQuestion := range map[string]string{
				"no queue heading":  strings.ReplaceAll(pane, "• Queued follow-up inputs", "• Prior assistant output"),
				"no question count": strings.ReplaceAll(pane, "? 1 question", "a question was discussed"),
				"no answer hint":    strings.ReplaceAll(pane, hint, "answer in ordinary chat"),
			} {
				if got := codexQuestionCount(strings.Split(nonQuestion, "\n")); got != 0 {
					t.Errorf("%s inferred question from prose: %d", name, got)
				}
			}
		})
	}
}

func TestCodexQuestionGlyphDoesNotPermitInputWithoutNativeIdentity(t *testing.T) {
	for _, kind := range []string{"no owned rollout", "queue request mismatch"} {
		t.Run(kind, func(t *testing.T) {
			tm, ex := nativeQuestionTmux(t, codexQuestionGlyphNativeScreen)
			tm.codexRolloutHeld = func(int) string { return "" }
			if kind == "queue request mismatch" {
				path := codexGlyphQuestionRollout(t)
				if err := os.WriteFile(path, []byte("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\"}}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				tm.codexRolloutHeld = func(int) string { return path }
			}
			if pending, err := tm.Pending("q"); pending != nil || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("unknown native identity must fail closed: pending=%+v err=%v", pending, err)
			}
			if err := tm.checkNoApprovalPrompt("q"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("unknown request permitted ordinary message: %v", err)
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
}

// Async question inputs stay queued after the model emits its current turn's
// final answer. Observed on Codex0.162.1: task_complete followed the accepted
// request while the native queue still showed two unanswered questions.
func TestCodexAsyncQuestionRemainsBoundAfterModelTurnCompletes(t *testing.T) {
	tm, ex := nativeQuestionTmux(t, codexQuestionGlyphNativeScreen)
	path := codexGlyphQuestionRollout(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\"}}\n")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	tm.codexRolloutHeld = func(int) string { return path }
	pending, err := tm.Pending("q")
	if err != nil || pending == nil || pending.RequestID != "tmux-codex-question-call-own-question-0" {
		t.Fatalf("native async input was discarded at model turn completion: pending=%+v err=%v", pending, err)
	}
	if err := tm.checkNoApprovalPrompt("q"); !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("ordinary input must remain guarded while async question is queued: %v", err)
	}
	requireNoKeys(t, ex.calls, false)
}

func TestCodexCompletedAsyncQuestionStillRequiresMatchingLiveQueue(t *testing.T) {
	for _, mode := range []string{"no queue", "count mismatch"} {
		t.Run(mode, func(t *testing.T) {
			pane := strings.ReplaceAll(codexQuestionGlyphNativeScreen, "• Queued follow-up inputs", "• Prior assistant output")
			if mode == "count mismatch" {
				pane = strings.ReplaceAll(codexQuestionGlyphNativeScreen, "? 1 question", "? 2 questions")
			}
			tm, ex := nativeQuestionTmux(t, pane)
			path := codexGlyphQuestionRollout(t)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, []byte("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\"}}\n")...)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			tm.codexRolloutHeld = func(int) string { return path }
			pending, err := tm.Pending("q")
			if pending != nil {
				t.Fatalf("unmatched async input exposed as answerable: %+v", pending)
			}
			if mode == "count mismatch" && !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("queue mismatch was not refused: %v", err)
			}
			if err := tm.Respond("q", runtime.InteractionResponse{RequestID: "tmux-codex-question-call-own-question-0", Action: "answer", Text: "Owl"}); err == nil {
				t.Fatal("unmatched input was answered")
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
}

// Captured unchanged from Codex0.162.1 on aa8ec2d44, case10 observation0082.
// The live queue showed a20s timer while the same native request remained unanswered.
// GC returned supported:true without a pending interaction for this screen.
// Screen SHA256:f81bb50c89091271e0f403260d63b6d03d657113c8062eea42b71170fa26a019.
const codexQuestionAgedNativeScreen = `qualification animal?", "options": ["Otter", "Owl"]}]. Immediately after
  acceptance, while it remains open, invoke clock.sleep exactly ONCE with
  duration_ms 60000. Use no other tools. Do not answer yourself or substitute
  chat. After sleep finishes and the real native answer arrives, reply exactly
  RFC8191_RECOVERY_16751b31bcb643d39842bf82191c0f31_ followed by its animal
  label. Never repeat either tool.


• Which qualification animal?

  • Otter
  • Owl

• Working (14s • esc to interrupt)

• Queued follow-up inputs
  ? 1 question · 20s
    ⇧← to answer


› Ask Codex to do anything

  GPT-6.1-Sol medium · ~/rfc8191/cities/rfc-questions-7ffcbbc787f74ad286603bacb…
  ? for shortcuts`

func TestCodexQuestionAgedNativeQueuePreservesPendingAndMessageGuard(t *testing.T) {
	tm, ex := nativeQuestionTmux(t, codexQuestionAgedNativeScreen)
	path := nativeQuestionRollout(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.ReplaceAll(string(data), "Which animal should be the test mascot?", "Which qualification animal?"))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	tm.codexRolloutHeld = func(int) string { return path }
	pending, err := tm.Pending("q")
	if err != nil || pending == nil || pending.RequestID != "tmux-codex-question-call-own-question-0" || !slices.Equal(pending.Options, []string{"Otter", "Owl"}) {
		t.Fatalf("aged live native request disappeared: pending=%+v err=%v", pending, err)
	}
	if err := tm.checkNoApprovalPrompt("q"); !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("aged request permitted ordinary input: %v", err)
	}
	requireNoKeys(t, ex.calls, false)
}

func TestCodexQuestionTimerRequiresLiveNativeQueueAndMatchingIdentity(t *testing.T) {
	for label, change := range map[string]func(string) string{
		"no heading": func(s string) string {
			return strings.ReplaceAll(s, "• Queued follow-up inputs", "• Previous output")
		},
		"no answer hint":    func(s string) string { return strings.ReplaceAll(s, "⇧← to answer", "answer in chat") },
		"invalid duration":  func(s string) string { return strings.ReplaceAll(s, " · 20s", " · later") },
		"negative duration": func(s string) string { return strings.ReplaceAll(s, " · 20s", " · -20s") },
		"trailing prose":    func(s string) string { return strings.ReplaceAll(s, " · 20s", " · 20s discussed earlier") },
	} {
		t.Run(label, func(t *testing.T) {
			tm, ex := nativeQuestionTmux(t, change(codexQuestionAgedNativeScreen))
			tm.codexRolloutHeld = func(int) string { return nativeQuestionRollout(t) }
			if pending, err := tm.Pending("q"); pending != nil || err != nil {
				t.Fatalf("invalid live-queue shape: pending=%+v err=%v", pending, err)
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
	for _, mode := range []string{"no rollout", "count mismatch", "resolved request"} {
		t.Run(mode, func(t *testing.T) {
			pane := codexQuestionAgedNativeScreen
			if mode == "count mismatch" {
				pane = strings.ReplaceAll(pane, "? 1 question", "? 2 questions")
			}
			tm, ex := nativeQuestionTmux(t, pane)
			path := nativeQuestionRollout(t)
			if mode == "resolved request" {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tm.codexRolloutHeld = func(int) string {
				if mode == "no rollout" {
					return ""
				}
				return path
			}
			if pending, err := tm.Pending("q"); pending != nil || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("unowned or inconsistent timed queue: pending=%+v err=%v", pending, err)
			}
			if err := tm.Respond("q", runtime.InteractionResponse{RequestID: "tmux-codex-question-call-own-question-0", Action: "answer", Text: "Otter"}); err == nil {
				t.Fatal("unowned or inconsistent queue accepted an answer")
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
}
