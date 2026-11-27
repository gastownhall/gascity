package runtime

import (
	"context"
	"errors"
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

const codex0156TrustDialog = `› Ask Codex to do anything

  Folder access
  /Users/u/src/project

  Trust this folder? Codex can read, edit, and run files here, subject to your
  permission settings. Folder settings can run code automatically, even
  without a model request. Continue only if you trust these files. Your trust
  decision will be saved.

› 1. Trust and continue
  2. Quit

  enter continue · esc quit`

// codex0156TrustDialogQuitSelected is codex0156TrustDialog with the cursor on
// "2. Quit" instead of the trust row.
var codex0156TrustDialogQuitSelected = strings.Replace(codex0156TrustDialog,
	"› 1. Trust and continue\n  2. Quit",
	"  1. Trust and continue\n› 2. Quit", 1)

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
	// The pane never re-renders with the cursor on the trust row, so the
	// handler must give up visibly instead of confirming "No, exit".
	if !errors.Is(err, ErrWorkspaceTrustUnconfirmed) {
		t.Fatalf("acceptWorkspaceTrustDialog() error = %v, want ErrWorkspaceTrustUnconfirmed", err)
	}

	for _, k := range sent {
		if strings.EqualFold(k, "Enter") {
			t.Fatalf("handler confirmed while %q was the selected row: sent=%v", "No, exit", sent)
		}
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

func TestWorkspaceTrustConfirmKeysCodex0156TrustPreSelected(t *testing.T) {
	if !containsWorkspaceTrustDialog(codex0156TrustDialog) {
		t.Fatal("Codex 0.156 trust dialog was not recognized")
	}
	keys, ok := workspaceTrustConfirmKeys(codex0156TrustDialog)
	if !ok {
		t.Fatal("workspaceTrustConfirmKeys() ok = false, want true")
	}
	if want := []string{"Enter"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("workspaceTrustConfirmKeys() = %v, want %v", keys, want)
	}
}

func TestWorkspaceTrustConfirmKeysCodex0156QuitSelected(t *testing.T) {
	keys, ok := workspaceTrustConfirmKeys(codex0156TrustDialogQuitSelected)
	if !ok {
		t.Fatal("workspaceTrustConfirmKeys() ok = false, want true")
	}
	if want := []string{"Up", "Enter"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("workspaceTrustConfirmKeys() = %v, want %v", keys, want)
	}
}

// TestWorkspaceTrustConfirmKeysCodex0156TrustRowIgnoresCase starts on
// "2. Quit" so the Up can only come from finding the trust row by its label,
// which must match regardless of case, as the other layouts' labels do.
func TestWorkspaceTrustConfirmKeysCodex0156TrustRowIgnoresCase(t *testing.T) {
	content := strings.Replace(codex0156TrustDialogQuitSelected,
		"1. Trust and continue", "1. trust and continue", 1)
	keys, ok := workspaceTrustConfirmKeys(content)
	if !ok {
		t.Fatal("workspaceTrustConfirmKeys() ok = false, want true")
	}
	if want := []string{"Up", "Enter"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("workspaceTrustConfirmKeys() = %v, want %v", keys, want)
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

// TestWorkspaceTrustConfirmKeysAnswersBottomMostTrustQuestion pins which
// layout answers when the capture holds more than one agent's trust question.
// The live dialog is the bottom-most, so a "Trust this folder?" in scrollback
// must not pre-empt another agent's live dialog: answering with the Codex
// layout would press Enter on Claude's "No, exit" or on Gemini's and pi's
// "Don't trust", or find no keys and hand the phase off with the dialog still
// up. The reverse holds too: another agent's stale dialog must not pre-empt a
// live Codex 0.156 one.
func TestWorkspaceTrustConfirmKeysAnswersBottomMostTrustQuestion(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "Codex 0.156 dialog in scrollback above live Claude dialog",
			content: codex0156TrustDialog + "\n$ claude\n" + realTrustDialogNoExitSelected,
			want:    []string{"Down", "Enter"},
		},
		{
			name:    "Codex question quoted in scrollback above live Claude dialog",
			content: "> Codex 0.156 now asks \"Trust this folder?\" at startup\n" + realTrustDialogNoExitSelected,
			want:    []string{"Down", "Enter"},
		},
		{
			name: "Codex 0.156 dialog in scrollback above live Gemini dialog",
			content: codex0156TrustDialog + "\n$ gemini\n" +
				"Do you trust the files in this folder?\n  1. Trust folder (city)\n  2. Trust parent folder\n● 3. Don't trust",
			want: []string{"Up", "Up", "Enter"},
		},
		{
			name:    "Codex 0.156 dialog in scrollback above live pi dialog",
			content: codex0156TrustDialog + "\n$ pi\n" + " Trust project folder?\n\n   Trust\n → Don't trust",
			want:    []string{"Up", "Enter"},
		},
		{
			name:    "Codex 0.156 dialog in scrollback above live older Codex prompt",
			content: codex0156TrustDialogQuitSelected + "\n$ codex\n" + "Do you trust the contents of this directory?",
			want:    []string{"Enter"},
		},
		{
			name:    "Claude dialog in scrollback above live Codex 0.156 dialog",
			content: realTrustDialogNoExitSelected + "\n$ codex\n" + codex0156TrustDialogQuitSelected,
			want:    []string{"Up", "Enter"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys, ok := workspaceTrustConfirmKeys(tt.content)
			if !ok {
				t.Fatal("workspaceTrustConfirmKeys() ok = false, want true")
			}
			if !reflect.DeepEqual(keys, tt.want) {
				t.Fatalf("workspaceTrustConfirmKeys() = %v, want %v", keys, tt.want)
			}
		})
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
	// trust row, so reaching it needs Up rather than Down.
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

// copilotTrustDialog is Copilot CLI 1.0.105's folder-trust dialog as captured
// in a tmux pane by the reporter of gastownhall/gascity#8738. It asks
// Gemini's question but draws a "❯" cursor and numbered rows inside a box.
const copilotTrustDialog = `│ Do you trust the files in this folder?                     │
│                                                            │
│ ❯ 1. Yes                                                   │
│   2. Yes, and remember this folder for future sessions     │
│   3. No (Esc)                                              │
│                                                            │
│ ↑/↓ to navigate · enter to select · esc to cancel          │`

const (
	copilotTrustRowOnce     = "1. Yes                                                   │"
	copilotTrustRowRemember = "2. Yes, and remember this folder for future sessions     │"
	copilotTrustRowNo       = "3. No (Esc)                                              │"
)

// copilotTrustDialogSelecting returns copilotTrustDialog with the cursor on
// row (1, 2 or 3) instead of "1. Yes".
func copilotTrustDialogSelecting(row int) string {
	rows := []string{copilotTrustRowOnce, copilotTrustRowRemember, copilotTrustRowNo}
	var b strings.Builder
	b.WriteString("│ Do you trust the files in this folder?                     │\n")
	b.WriteString("│                                                            │\n")
	for i, r := range rows {
		marker := "  "
		if i+1 == row {
			marker = "❯ "
		}
		b.WriteString("│ " + marker + r + "\n")
	}
	b.WriteString("│                                                            │\n")
	b.WriteString("│ ↑/↓ to navigate · enter to select · esc to cancel          │")
	return b.String()
}

// geminiTrustDialog is Gemini's option list for the question Copilot CLI
// also asks, as the existing Gemini tests render it.
const geminiTrustDialog = "Do you trust the files in this folder?\n" +
	"● 1. Trust folder (city)\n  2. Trust parent folder\n  3. Don't trust"

func TestWorkspaceTrustConfirmKeysCopilotTrustDialog(t *testing.T) {
	if got := copilotTrustDialogSelecting(1); got != copilotTrustDialog {
		t.Fatalf("precondition: copilotTrustDialogSelecting(1) differs from the captured dialog:\n%s", got)
	}
	if !containsWorkspaceTrustDialog(copilotTrustDialog) {
		t.Fatal("Copilot trust dialog was not recognized as a workspace-trust dialog")
	}
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{name: "captured dialog, Yes selected", content: copilotTrustDialog, want: []string{"Enter"}},
		{name: "remember row selected", content: copilotTrustDialogSelecting(2), want: []string{"Up", "Enter"}},
		{name: "No selected", content: copilotTrustDialogSelecting(3), want: []string{"Up", "Up", "Enter"}},
		{
			name:    "scrollback above the dialog",
			content: trustDialogScrollback + "$ copilot --yolo\n" + copilotTrustDialogSelecting(3),
			want:    []string{"Up", "Up", "Enter"},
		},
		{
			name:    "Yes label in another case",
			content: strings.Replace(copilotTrustDialogSelecting(2), "1. Yes ", "1. YES ", 1),
			want:    []string{"Up", "Enter"},
		},
		{
			name:    "borderless rows",
			content: "Do you trust the files in this folder?\n\n  1. Yes\n  2. Yes, and remember this folder for future sessions\n❯ 3. No (Esc)",
			want:    []string{"Up", "Up", "Enter"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys, ok := workspaceTrustConfirmKeys(tt.content)
			if !ok {
				t.Fatal("workspaceTrustConfirmKeys() ok = false, want true")
			}
			if !reflect.DeepEqual(keys, tt.want) {
				t.Fatalf("workspaceTrustConfirmKeys() = %v, want %v", keys, tt.want)
			}
		})
	}
}

// TestWorkspaceTrustConfirmKeysCopilotFailsClosed covers Copilot frames that
// must get no keys: the session-only "1. Yes" row is missing, so the only
// trusting row left would persist trust; the cursor is not drawn yet; or the
// labels are not ones the layout knows.
func TestWorkspaceTrustConfirmKeysCopilotFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name: "only the remember row trusts",
			content: "│ Do you trust the files in this folder?            │\n" +
				"│ ❯ 1. Yes, and remember this folder for future sessions │\n" +
				"│   2. No (Esc)                                      │",
		},
		{
			name:    "no cursor drawn yet",
			content: strings.Replace(copilotTrustDialog, "│ ❯ 1. Yes", "│   1. Yes", 1),
		},
		{
			name:    "unknown labels",
			content: strings.Replace(copilotTrustDialog, "1. Yes ", "1. Allow", 1),
		},
		{
			name:    "Yes with extra words",
			content: strings.Replace(copilotTrustDialog, "1. Yes      ", "1. Yes, once", 1),
		},
		{
			name:    "question only",
			content: "│ Do you trust the files in this folder?                     │",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if keys, ok := workspaceTrustConfirmKeys(tt.content); ok || len(keys) != 0 {
				t.Fatalf("workspaceTrustConfirmKeys() = %v, %v; want no keys", keys, ok)
			}
		})
	}
}

// TestWorkspaceTrustConfirmKeysSharedFilesTrustQuestion pins that Gemini and
// Copilot CLI, which ask the same question, are each answered with their own
// layout, and that a frame both layouts match gets no keys.
func TestWorkspaceTrustConfirmKeysSharedFilesTrustQuestion(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string // nil: no keys
	}{
		{name: "Gemini, trust folder selected", content: geminiTrustDialog, want: []string{"Enter"}},
		{
			name:    "Gemini, Don't trust selected",
			content: "Do you trust the files in this folder?\n  1. Trust folder (city)\n  2. Trust parent folder\n● 3. Don't trust",
			want:    []string{"Up", "Up", "Enter"},
		},
		{
			name:    "Copilot dialog in scrollback above live Gemini dialog",
			content: copilotTrustDialogSelecting(3) + "\n$ gemini\n" + geminiTrustDialog,
			want:    []string{"Enter"},
		},
		{
			name:    "Gemini dialog in scrollback above live Copilot dialog",
			content: geminiTrustDialog + "\n$ copilot\n" + copilotTrustDialogSelecting(2),
			want:    []string{"Up", "Enter"},
		},
		{
			name: "both layouts under one question",
			content: "Do you trust the files in this folder?\n" +
				"● 1. Trust folder (city)\n  2. Don't trust\n" +
				"❯ 1. Yes\n  2. No (Esc)",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys, ok := workspaceTrustConfirmKeys(tt.content)
			if tt.want == nil {
				if ok || len(keys) != 0 {
					t.Fatalf("workspaceTrustConfirmKeys() = %v, %v; want no keys", keys, ok)
				}
				return
			}
			if !ok {
				t.Fatal("workspaceTrustConfirmKeys() ok = false, want true")
			}
			if !reflect.DeepEqual(keys, tt.want) {
				t.Fatalf("workspaceTrustConfirmKeys() = %v, want %v", keys, tt.want)
			}
		})
	}
}

func TestAcceptWorkspaceTrustDialogCopilotUnsafeRowsSendNothing(t *testing.T) {
	withZeroDialogTimings(t)
	content := "│ Do you trust the files in this folder?            │\n" +
		"│ ❯ 1. Yes, and remember this folder for future sessions │\n" +
		"│   2. No (Esc)                                      │"
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
		t.Fatalf("acceptWorkspaceTrustDialog() sent = %v, want no keys without a session-only trust row", sent)
	}
}
