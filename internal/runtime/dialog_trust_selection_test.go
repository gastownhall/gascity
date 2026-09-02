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
	const content = ` Do you trust the contents of this directory?

 (layout not yet fully rendered)`

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
