package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The testdata/claude-question-menu panes are live captures (Claude Code
// v2.1.314, header path sanitized; named-* from an 80-column named session,
// the rest 140x45 and unnamed) of every AskUserQuestion screen a
// nudge can land on, plus the panes around them that must stay nudgeable.
// idle-quotes-menu.txt is hand-made: an idle composer under tool output that
// quotes a dialog's footer and options.
func readQuestionMenuPane(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "claude-question-menu", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return string(data)
}

var questionMenuLayouts = []struct {
	fixture string
	open    bool
	focused string
}{
	{"single.txt", true, "Red (warm)"},
	{"multi-q1.txt", true, "Red"},
	// The answered tab is marked ☒, so the header row no longer starts with ☐.
	{"multi-q2.txt", true, "Small"},
	// The review screen has no select footer; Enter submits the answers.
	{"multi-review.txt", true, "Submit answers"},
	// A named session draws a rule with its name below the dialog.
	{"named-multi-q1.txt", true, "Red"},
	{"named-multi-review.txt", true, "Submit answers"},
	{"multiselect.txt", true, "[ ] Cheese"},
	// A ticked option marks the tab ☒, and the cursor rests on the unnumbered
	// Submit row.
	{"multiselect-submit.txt", true, "Submit"},
	{"answered-scrollback.txt", false, ""},
	{"idle-composer.txt", false, ""},
	{"idle-quotes-menu.txt", false, ""},
}

func TestParseAskUserQuestionLayouts(t *testing.T) {
	for _, tc := range questionMenuLayouts {
		t.Run(tc.fixture, func(t *testing.T) {
			lines := strings.Split(readQuestionMenuPane(t, tc.fixture), "\n")
			q := parseAskUserQuestion(lines)
			if (q != nil) != tc.open {
				t.Fatalf("parseAskUserQuestion = %+v, want open=%v", q, tc.open)
			}
			if q != nil && q.Focused != tc.focused {
				t.Errorf("Focused = %q, want %q", q.Focused, tc.focused)
			}
			if got := PaneShowsIdlePrompt(lines, DefaultReadyPromptPrefix); got == tc.open {
				t.Errorf("PaneShowsIdlePrompt = %v with open=%v", got, tc.open)
			}
		})
	}
}

// Every open layout refuses a nudge before any key reaches the pane.
func TestNudgeSessionSendsNoKeysIntoAnyQuestionLayout(t *testing.T) {
	for _, tc := range questionMenuLayouts {
		if !tc.open {
			continue
		}
		t.Run(tc.fixture, func(t *testing.T) {
			ex := &paneExecutor{pane: readQuestionMenuPane(t, tc.fixture), inMode: "0"}
			tm := &Tmux{cfg: DefaultConfig(), exec: ex}
			err := tm.NudgeSession("auq", "PR #12: a reviewer replied after your fix")
			if !errors.Is(err, runtime.ErrPendingInteraction) {
				t.Fatalf("NudgeSession = %v, want runtime.ErrPendingInteraction", err)
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
}
