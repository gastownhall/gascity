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
