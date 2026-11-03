package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// questionInteraction is what a runtime reports for an open AskUserQuestion
// dialog: a question the operator answers in the terminal, with no options.
func questionInteraction() *runtime.PendingInteraction {
	return &runtime.PendingInteraction{
		RequestID: "tmux-question-0123456789abcdef",
		Kind:      "question",
		Prompt:    "be-tx30v: How do you want to proceed?",
	}
}

// startedQuestionSession is a running session whose runtime reports an open
// question dialog. The runtime's call log is cleared so a test sees only what
// the send under test does.
func startedQuestionSession(t *testing.T) (*Manager, *runtime.Fake, Info) {
	t.Helper()
	sp := runtime.NewFake()
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, "", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.SetPendingInteraction(info.SessionName, questionInteraction())
	sp.Calls = nil
	return mgr, sp, info
}

// requireNothingTyped fails when the runtime was asked to type or press
// anything into the session.
func requireNothingTyped(t *testing.T, sp *runtime.Fake, sessionName string) {
	t.Helper()
	for _, call := range sp.Calls {
		switch call.Method {
		case "Nudge", "NudgeNow", "SendKeys", "Interrupt":
			if call.Name == sessionName {
				t.Fatalf("typed into a session with an open question dialog: %#v", call)
			}
		}
	}
}

// The live-only sends have no queueing fallback, so a nudge that typed into an
// open question would answer it: they must refuse with ErrPendingInteraction
// before the runtime sees the message.
func TestSendLiveOnlyRefusesQuestionInteraction(t *testing.T) {
	for name, send := range map[string]func(*Manager, string) (bool, error){
		"SendLiveOnly": func(m *Manager, id string) (bool, error) {
			return m.SendLiveOnly(context.Background(), id, "hello", nil)
		},
		"SendImmediateLiveOnly": func(m *Manager, id string) (bool, error) {
			return m.SendImmediateLiveOnly(context.Background(), id, "hello", nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			mgr, sp, info := startedQuestionSession(t)
			delivered, err := send(mgr, info.ID)
			if delivered || !errors.Is(err, ErrPendingInteraction) {
				t.Fatalf("delivered=%v err=%v, want delivered=false and ErrPendingInteraction", delivered, err)
			}
			requireNothingTyped(t, sp, info.SessionName)
		})
	}
}

// FollowUp queues behind a pending interaction and InterruptNow would press
// Escape into it; both refuse for a question as they do for an approval.
func TestSubmitRefusesQuestionInteraction(t *testing.T) {
	for name, intent := range map[string]SubmitIntent{
		"follow_up":     SubmitIntentFollowUp,
		"interrupt_now": SubmitIntentInterruptNow,
	} {
		t.Run(name, func(t *testing.T) {
			mgr, sp, info := startedQuestionSession(t)
			outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, intent)
			if !errors.Is(err, ErrPendingInteraction) {
				t.Fatalf("Submit(%s) = %+v, %v; want ErrPendingInteraction", name, outcome, err)
			}
			requireNothingTyped(t, sp, info.SessionName)
		})
	}
}

// A question has no verbs to respond with. The operator text names what the
// dialog is and where it is answered, not an approve/deny the runtime will not
// take; an approval keeps its respond wording.
func TestPendingInteractionErrorWordingIsKindAware(t *testing.T) {
	err := PendingInteractionError(questionInteraction())
	if !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("PendingInteractionError(question) = %v, want ErrPendingInteraction", err)
	}
	text := err.Error()
	for _, banned := range []string{"approve or deny", "respond", "permission prompt"} {
		if strings.Contains(text, banned) {
			t.Errorf("question refusal %q mentions %q, which does not apply to it", text, banned)
		}
	}
	for _, want := range []string{"question", "How do you want to proceed?", "gc session attach"} {
		if !strings.Contains(text, want) {
			t.Errorf("question refusal %q does not mention %q", text, want)
		}
	}

	approval := PendingInteractionError(&runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "approve?", Options: []string{"approve", "deny"}})
	if want := "respond to it (approve or deny) before sending text"; !strings.Contains(approval.Error(), want) {
		t.Errorf("approval refusal %q lost its %q wording", approval.Error(), want)
	}
}

func TestQuestionAnswerRequiresExplicitRequest(t *testing.T) {
	mgr, sp, info := startedQuestionSession(t)
	err := mgr.Respond(info.ID, runtime.InteractionResponse{Action: "answer", Text: "Owl"})
	if !errors.Is(err, ErrInteractionMismatch) {
		t.Fatalf("unbound question answer=%v", err)
	}
	if n := sp.CountCalls("Respond", info.SessionName); n != 0 {
		t.Fatalf("unbound answer reached runtime %d times", n)
	}
}

func TestTypedQuestionAnswersValidateShapeBeforeRuntime(t *testing.T) {
	for _, response := range []runtime.InteractionResponse{
		{RequestID: "q", Action: "approve", Answers: []string{"Otter"}},
		{RequestID: "q", Action: "answer", Text: "Owl", Answers: []string{"Otter"}},
		{RequestID: "q", Action: "answer", Answers: []string{"Otter", "Otter"}},
		{RequestID: "q", Action: "answer", Answers: []string{""}},
		{RequestID: "q", Action: "answer", Answers: []string{"Owl\n"}},
	} {
		if err := ValidateInteractionResponse(response); !errors.Is(err, ErrInteractionMismatch) {
			t.Errorf("malformed typed answer %+v = %v, want interaction mismatch", response, err)
		}
	}
}

func TestTypedQuestionAnswersRequireCurrentAuthoredLabels(t *testing.T) {
	for name, tc := range map[string]struct {
		options  []string
		multiple bool
		answers  []string
	}{
		"unknown":                {[]string{"Otter", "Owl"}, true, []string{"Fox"}},
		"ambiguous native label": {[]string{"Otter", "Otter"}, true, []string{"Otter"}},
		"single select":          {[]string{"Otter", "Owl"}, false, []string{"Owl"}},
	} {
		t.Run(name, func(t *testing.T) {
			mgr, sp, info := startedQuestionSession(t)
			metadata := map[string]string{"multi_select": "false"}
			if tc.multiple {
				metadata["multi_select"] = "true"
			}
			sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "q", Kind: "question", Options: tc.options, Metadata: metadata})
			err := mgr.Respond(info.ID, runtime.InteractionResponse{RequestID: "q", Action: "answer", Answers: tc.answers})
			if !errors.Is(err, ErrInteractionMismatch) {
				t.Fatalf("unknown/unavailable selection reached runtime: %v", err)
			}
			if calls := sp.CountCalls("Respond", info.SessionName); calls != 0 {
				t.Fatalf("Respond calls=%d, want zero", calls)
			}
		})
	}
}

func TestTypedQuestionAnswersReachRuntimeAsAuthoredValues(t *testing.T) {
	mgr, sp, info := startedQuestionSession(t)
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "q", Kind: "question", Options: []string{"Otter", "Owl"}, Metadata: map[string]string{"multi_select": "true"}})
	if err := mgr.Respond(info.ID, runtime.InteractionResponse{RequestID: "q", Action: "answer", Answers: []string{"Owl", "Otter"}}); err != nil {
		t.Fatal(err)
	}
	responses := sp.Responses[info.SessionName]
	if len(responses) != 1 || strings.Join(responses[0].Answers, "|") != "Owl|Otter" {
		t.Fatalf("typed selections lost or encoded in text: %+v", responses)
	}
}
