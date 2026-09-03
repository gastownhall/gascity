package runtime

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// realTrustDialogNoExitSelected is the verbatim pane content captured from a
// dying pool seat (hold-court--builder-pool,
// .gc/sessions/hold-court--builder-pool/start-stderr.log, 2026-09-01). The
// cursor marker sits on "No, exit", not on the trust option.
const realTrustDialogNoExitSelected = ` Accessing workspace:

 /home/u/.gc/worktrees/hold-court/builder

 Quick safety check: Is this a project you created or one you trust? (Like your
 own code, a well-known open source project, or work from your team). If not,
 take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 Security guide

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel`

func TestWorkspaceTrustDialogDoesNotConfirmNoExit(t *testing.T) {
	withZeroDialogTimings(t)

	if !containsWorkspaceTrustDialog(realTrustDialogNoExitSelected) {
		t.Fatalf("precondition: matcher should recognize the real trust dialog")
	}

	var sent []string
	err := acceptWorkspaceTrustDialog(
		context.Background(),
		newStartupDialogBudget(time.Second),
		func(int) (string, error) { return realTrustDialogNoExitSelected, nil },
		func(keys ...string) error { sent = append(sent, keys...); return nil },
	)
	if err != nil {
		t.Fatalf("acceptWorkspaceTrustDialog() error = %v", err)
	}

	if len(sent) > 0 && strings.EqualFold(sent[0], "Enter") {
		t.Errorf("handler confirmed while %q was the selected row: sent=%v\n"+
			"want the selection moved onto the trust option first (e.g. [Down Enter]), "+
			"as acceptMCPTrustDialog already does", "No, exit", sent)
	}
}

func TestWorkspaceTrustConfirmKeysTrustPreSelected(t *testing.T) {
	const content = ` Quick safety check: Is this a project you created or one you trust?

 ❯ Yes, I trust this folder
   No, exit

 Enter to confirm · Esc to cancel`

	keys, ok := workspaceTrustConfirmKeys(content)
	if !ok {
		t.Fatalf("workspaceTrustConfirmKeys() ok = false, want true")
	}
	if want := []string{"Enter"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("workspaceTrustConfirmKeys() = %v, want %v", keys, want)
	}
}

func TestWorkspaceTrustConfirmKeysNoExitSelected(t *testing.T) {
	keys, ok := workspaceTrustConfirmKeys(realTrustDialogNoExitSelected)
	if !ok {
		t.Fatalf("workspaceTrustConfirmKeys() ok = false, want true")
	}
	if want := []string{"Down", "Enter"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("workspaceTrustConfirmKeys() = %v, want %v", keys, want)
	}
}

func TestWorkspaceTrustConfirmKeysUnrecognizedLayoutSendsNothing(t *testing.T) {
	const content = ` Quick safety check: Is this a project you created or one you trust?

 ❯ Some future option we don't recognize
   Another unrecognized option

 Enter to confirm · Esc to cancel`

	keys, ok := workspaceTrustConfirmKeys(content)
	if ok {
		t.Fatalf("workspaceTrustConfirmKeys() ok = true, want false; keys = %v", keys)
	}
	if len(keys) != 0 {
		t.Errorf("workspaceTrustConfirmKeys() keys = %v, want empty when not ok", keys)
	}

	withZeroDialogTimings(t)
	var sent []string
	err := acceptWorkspaceTrustDialog(
		context.Background(),
		newStartupDialogBudget(100*time.Millisecond),
		func(int) (string, error) { return content, nil },
		func(keys ...string) error { sent = append(sent, keys...); return nil },
	)
	if err != nil {
		t.Fatalf("acceptWorkspaceTrustDialog() error = %v", err)
	}
	if len(sent) != 0 {
		t.Errorf("acceptWorkspaceTrustDialog() sent = %v, want no keys sent for an unrecognized trust layout", sent)
	}
}

// trustDialogScrollback is plausible pane scrollback sitting above the live
// dialog. peek uses capture-pane -S -120, so this is what index 0 of the
// scanned content actually is. It deliberately carries both a "> " row and a
// "❯ " row: neither may be mistaken for the dialog's cursor.
const trustDialogScrollback = `$ gc rig add hold-court /home/u/src/hold-court
> rig "hold-court" registered
$ git log --oneline -3
1639750d7 feat: green — Fix trust dialog selection
17333ea11 Retry store-read timeouts instead of quarantining finalizers
c96e54a3a feat(rppcheck): verify declared nudge fallback capability
$ git show HEAD --stat
> internal/runtime/dialog.go | 118 ++++++++++++-
> 1 file changed, 114 insertions(+), 4 deletions(-)
$ cat notes.md
> Reviewer asked:
> ❯ why does the seat die during startup?
$ gc pool start builder
  starting seat 1 of 4
  starting seat 2 of 4
$ claude
 Welcome to Claude Code

`

func TestWorkspaceTrustConfirmKeysIgnoresScrollbackCursor(t *testing.T) {
	content := trustDialogScrollback + realTrustDialogNoExitSelected

	keys, ok := workspaceTrustConfirmKeys(content)
	if !ok {
		t.Fatalf("workspaceTrustConfirmKeys() ok = false, want true")
	}
	if want := []string{"Down", "Enter"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("workspaceTrustConfirmKeys() = %v, want %v\n"+
			"scrollback above the dialog must not supply the cursor row", keys, want)
	}
}

func TestWorkspaceTrustConfirmKeysBorderedLayout(t *testing.T) {
	var bordered strings.Builder
	for i, line := range strings.Split(realTrustDialogNoExitSelected, "\n") {
		if i > 0 {
			bordered.WriteString("\n")
		}
		bordered.WriteString("│ " + line)
	}

	keys, ok := workspaceTrustConfirmKeys(bordered.String())
	if !ok {
		t.Fatalf("workspaceTrustConfirmKeys() ok = false, want true for a bordered dialog")
	}
	if want := []string{"Down", "Enter"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("workspaceTrustConfirmKeys() = %v, want %v", keys, want)
	}
}

func TestWorkspaceTrustConfirmKeysUpwardMovement(t *testing.T) {
	// pi renders its trust prompt with the cursor able to start below the
	// trust row, which is the only layout that needs Up rather than Down.
	const content = ` Trust project folder?

 /home/u/src/hold-court

   Trust
 → Don't trust

 Enter to confirm · Esc to cancel`

	keys, ok := workspaceTrustConfirmKeys(content)
	if !ok {
		t.Fatalf("workspaceTrustConfirmKeys() ok = false, want true")
	}
	if want := []string{"Up", "Enter"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("workspaceTrustConfirmKeys() = %v, want %v", keys, want)
	}
}
