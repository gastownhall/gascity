package tmux

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Compile-time checks that both Tmux and Provider implement InteractionProvider.
var (
	_ runtime.InteractionProvider = (*Tmux)(nil)
	_ runtime.InteractionProvider = (*Provider)(nil)
)

// Pending delegates to the underlying Tmux instance.
func (p *Provider) Pending(name string) (*runtime.PendingInteraction, error) {
	return p.tm.Pending(name)
}

// Respond delegates to the underlying Tmux instance.
func (p *Provider) Respond(name string, response runtime.InteractionResponse) error {
	return p.tm.Respond(name, response)
}

// ---------------------------------------------------------------------------
// Pane-based approval detection
// ---------------------------------------------------------------------------

// approvalPatterns detect Claude Code's interactive prompts in tmux pane output.
var (
	// "This command requires approval" or "Approve edits?" patterns
	requiresApprovalRe = regexp.MustCompile(`(?m)(This command requires approval|Approve edits\?)`)

	// Tool call header: "● ToolName(args)" or "● ToolName"
	// Uses greedy match to last ")" to handle nested parens in args.
	toolHeaderRe = regexp.MustCompile(`● (\w+)(?:\((.+)\))?`)
)

// parsedApproval holds the parsed approval prompt from a tmux pane capture.
type parsedApproval struct {
	ToolName string
	Input    string
}

// parseApprovalPrompt parses the tmux pane text for a Claude Code approval prompt.
// Returns nil if no approval prompt is found or if the prompt can't be associated
// with a tool header (avoids false positives from conversational text).
func parseApprovalPrompt(paneText string) *parsedApproval {
	if !requiresApprovalRe.MatchString(paneText) {
		return nil
	}

	// Find the tool header closest to (before) the approval text.
	// This prevents binding a historical tool output to the active prompt.
	approvalIdx := requiresApprovalRe.FindStringIndex(paneText)
	if approvalIdx == nil {
		return nil
	}
	textBeforeApproval := paneText[:approvalIdx[0]]

	// Find the LAST tool header before the approval marker.
	matches := toolHeaderRe.FindAllStringSubmatch(textBeforeApproval, -1)
	if len(matches) == 0 {
		// No tool header found — can't associate this approval with a tool.
		// Return nil to avoid false positives from conversational output.
		return nil
	}
	lastMatch := matches[len(matches)-1]

	approval := &parsedApproval{
		ToolName: lastMatch[1],
	}
	if len(lastMatch) >= 3 && lastMatch[2] != "" {
		approval.Input = lastMatch[2]
	}

	// Try to extract the command/content shown between the tool header and approval prompt.
	if approval.Input == "" {
		approval.Input = extractToolInput(textBeforeApproval, approval.ToolName)
	}

	return approval
}

// extractToolInput extracts the indented tool input block from pane text.
// Claude shows tool input as indented lines between the "● ToolName" header
// and the "This command requires approval" / "Approve edits?" line.
// Searches backwards from the end of textBeforeApproval to find the last
// tool header occurrence.
func extractToolInput(textBeforeApproval, toolName string) string {
	lines := strings.Split(textBeforeApproval, "\n")

	// Find the last line containing the tool header
	headerIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "● "+toolName) {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		return ""
	}

	var captured []string
	for _, line := range lines[headerIdx+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		// Skip UI decoration lines (spinners, box-drawing, etc.)
		if strings.HasPrefix(trimmed, "⎿") || strings.HasPrefix(trimmed, "───") ||
			strings.HasPrefix(trimmed, "│") || trimmed == "Running…" {
			continue
		}
		// Claude indents tool input with leading spaces
		if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") {
			captured = append(captured, trimmed)
		}
	}

	if len(captured) == 0 {
		return ""
	}
	result := strings.Join(captured, "\n")
	// Truncate very long inputs
	if len(result) > 500 {
		result = result[:500] + "…"
	}
	return result
}

// ---------------------------------------------------------------------------
// Pane-based AskUserQuestion detection
// ---------------------------------------------------------------------------

// askUserQuestion* patterns detect Claude Code's AskUserQuestion dialog in
// tmux pane output. Defined once here and shared by Pending (below),
// nudgeSession and snapshotPaneIdleWithPrefix (tmux.go), and
// sendHiddenAttachedText (a sibling fix) — the dialog shape must not be
// duplicated per call site (gm-55kp2u).
var (
	// Dialog footer. Deliberately distinct from the folder-trust dialog's
	// "Enter to confirm · Esc to cancel" footer, which must stay nudgeable.
	askUserQuestionFooterRe = regexp.MustCompile(`Enter to select.*Esc to cancel`)

	// Focused (arrow-selected) option row: "❯ 1. <label>". Claude's idle
	// composer also starts a line with "❯ ", but never followed by
	// "<digit>. " — so this cannot mistake an idle composer prompt for a
	// focused dialog option.
	askUserQuestionOptionRe = regexp.MustCompile(`(?m)^❯\s+\d+\.\s*(.*)$`)

	// Header row above the question text, e.g. " ☐ be-tx30v".
	askUserQuestionHeaderRe = regexp.MustCompile(`(?m)^\s*☐\s+(.+)$`)
)

// parsedAskUserQuestion holds a parsed AskUserQuestion dialog from a tmux
// pane capture: the bead/topic header, the question text, and the label of
// the currently focused (arrow-selected) option.
type parsedAskUserQuestion struct {
	Header  string
	Prompt  string
	Focused string
}

// parseAskUserQuestionPrompt parses tmux pane text for an open Claude Code
// AskUserQuestion dialog. Returns nil unless all three anchors are present —
// footer, a focused numbered option row, and a header row — the three-signal
// design the ruling requires to avoid false positives from an idle composer,
// whose own "❯ " glyph is otherwise indistinguishable from the dialog's
// focused-option row.
func parseAskUserQuestionPrompt(paneText string) *parsedAskUserQuestion {
	if !askUserQuestionFooterRe.MatchString(paneText) {
		return nil
	}
	optionMatches := askUserQuestionOptionRe.FindAllStringSubmatch(paneText, -1)
	if len(optionMatches) == 0 {
		return nil
	}
	headerMatches := askUserQuestionHeaderRe.FindAllStringSubmatchIndex(paneText, -1)
	if len(headerMatches) == 0 {
		return nil
	}

	// Bind to the LAST occurrence of each anchor, mirroring parseApprovalPrompt:
	// scrollback may still hold an earlier, already-answered dialog above the
	// live one.
	focused := optionMatches[len(optionMatches)-1]
	headerLoc := headerMatches[len(headerMatches)-1]

	return &parsedAskUserQuestion{
		Header:  strings.TrimSpace(paneText[headerLoc[2]:headerLoc[3]]),
		Prompt:  extractAskUserQuestionPromptText(paneText[headerLoc[1]:]),
		Focused: strings.TrimSpace(focused[1]),
	}
}

// extractAskUserQuestionPromptText returns the question text on the first
// "│ …" line after the dialog's header row (the dialog left-rules its prompt
// text; the header row itself is the bead/topic line, not the question).
func extractAskUserQuestionPromptText(afterHeader string) string {
	for _, line := range strings.Split(afterHeader, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, "│"); ok {
			return strings.TrimSpace(rest)
		}
		return ""
	}
	return ""
}

// askUserQuestionHash returns a short stable hash identifying a parsed
// AskUserQuestion dialog, mirroring approvalHash.
func askUserQuestionHash(q *parsedAskUserQuestion) string {
	h := sha256.Sum256([]byte(q.Header + "\x00" + q.Prompt + "\x00" + q.Focused))
	return fmt.Sprintf("%x", h[:8])
}

// ---------------------------------------------------------------------------
// Deduplication
// ---------------------------------------------------------------------------

// Per-session dedup state to avoid re-emitting the same approval.
type approvalDedup struct {
	mu       sync.Mutex
	lastHash map[string]string // session name → hash of last emitted approval
}

func approvalHash(a *parsedApproval) string {
	h := sha256.Sum256([]byte(a.ToolName + "\x00" + a.Input))
	return fmt.Sprintf("%x", h[:8])
}

func (d *approvalDedup) isNew(session string, a *parsedApproval) bool {
	hash := approvalHash(a)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastHash[session] == hash {
		return false
	}
	d.lastHash[session] = hash
	return true
}

func (d *approvalDedup) clear(session string) {
	d.mu.Lock()
	delete(d.lastHash, session)
	d.mu.Unlock()
}

// ---------------------------------------------------------------------------
// InteractionProvider implementation
// ---------------------------------------------------------------------------

// Pending checks the tmux pane for an active Claude Code approval prompt.
// Returns nil with no error if no approval is pending.
func (t *Tmux) Pending(name string) (*runtime.PendingInteraction, error) {
	paneText, err := t.CapturePane(name, 40)
	if err != nil {
		// Pane might not exist (session not started yet or already stopped).
		// Check for known "can't find" errors vs unexpected failures.
		// A server that answered with no sessions (ErrNoCurrentTarget) proves
		// the pane is gone. Any other ErrNoServer ("no tmux server running",
		// "error connecting to") is a failed observation, not proof of absence,
		// as in ListRunning. Its message omits the tmux text, which matches
		// runtime.IsSessionGone and would read "unknown" as "gone".
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoCurrentTarget) {
			return nil, fmt.Errorf("capturing pane: %w: %w", runtime.ErrSessionNotFound, err)
		}
		if errors.Is(err, ErrNoServer) {
			return nil, &quietCauseError{
				msg:  fmt.Sprintf("capturing pane of %q: tmux server unreachable: %v", name, runtime.ErrRuntimeUnavailable),
				errs: []error{runtime.ErrRuntimeUnavailable, err},
			}
		}
		if strings.Contains(err.Error(), "can't find") {
			return nil, nil
		}
		return nil, fmt.Errorf("capturing pane: %w", err)
	}

	approval := parseApprovalPrompt(paneText)
	if approval == nil {
		t.approvalDedup().clear(name)
		if question := parseAskUserQuestionPrompt(paneText); question != nil {
			return &runtime.PendingInteraction{
				RequestID: "tmux-question-" + askUserQuestionHash(question),
				Kind:      "question",
				Prompt:    question.Prompt,
				Options:   []string{question.Focused},
				Metadata: map[string]string{
					"header": question.Header,
					"source": "tmux",
				},
			}, nil
		}
		return nil, nil
	}

	// Dedup: don't re-emit the same approval on repeated polls.
	if !t.approvalDedup().isNew(name, approval) {
		// Return the interaction (caller may need it for display) but it's
		// not a new detection. The stable RequestID makes this idempotent.
		_ = struct{}{} // satisfy empty-block linter; dedup check is intentionally a no-op
	}

	requestID := "tmux-" + approvalHash(approval)

	prompt := approval.ToolName + ": " + approval.Input
	if approval.Input == "" {
		prompt = "Allow " + approval.ToolName + "?"
	}

	return &runtime.PendingInteraction{
		RequestID: requestID,
		Kind:      "approval",
		Prompt:    prompt,
		Options:   []string{"Yes", "Yes, and don't ask again", "No"},
		Metadata: map[string]string{
			"tool_name": approval.ToolName,
			"source":    "tmux",
		},
	}, nil
}

const (
	respondVerifyAttempts = 3
	respondVerifyMs       = 500
)

// Respond sends the appropriate keystroke to the tmux pane to approve or deny
// a pending tool approval, then verifies the prompt was consumed.
func (t *Tmux) Respond(name string, response runtime.InteractionResponse) error {
	// Verify the expected approval is still present before sending keys.
	paneText, err := t.CapturePane(name, 40)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return fmt.Errorf("pre-verify capture failed: %w: %w", runtime.ErrSessionNotFound, err)
		}
		return fmt.Errorf("pre-verify capture failed: %w", err)
	}
	// An AskUserQuestion dialog's option menu is per-dialog, not the fixed
	// Yes/Yes-always/No layout the switch below encodes: refuse before it can
	// map an approval action onto the wrong numbered option (gm-55kp2u).
	if parseAskUserQuestionPrompt(paneText) != nil {
		return fmt.Errorf("cannot respond to a question prompt (Kind %q) with approval action %q", "question", response.Action)
	}

	current := parseApprovalPrompt(paneText)
	if current == nil {
		t.approvalDedup().clear(name)
		return nil // prompt already gone
	}
	// If caller specified a RequestID, verify it matches the current prompt.
	if response.RequestID != "" {
		currentID := "tmux-" + approvalHash(current)
		if currentID != response.RequestID {
			return fmt.Errorf("approval prompt changed: expected %s, got %s", response.RequestID, currentID)
		}
	}

	// Map action to keystroke. Claude's prompt shows:
	// 1. Yes
	// 2. Yes, and don't ask again for: <tool>
	// 3. No
	var key string
	switch response.Action {
	case "approve":
		key = "1"
	case "approve_accept_edits", "approve_always":
		key = "2"
	case "deny":
		key = "3"
	default:
		return fmt.Errorf("unknown action %q", response.Action)
	}

	// Exit copy-mode first if the pane is parked (the ga-c4w wheel binding),
	// so the approval keystroke reaches the prompt instead of being swallowed
	// by copy-mode.
	t.cancelCopyModeIfParked(name)

	// Send the keystroke once.
	if _, err := t.run("send-keys", "-t", paneTarget(name), "-l", key); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return fmt.Errorf("send-keys failed: %w: %w", runtime.ErrSessionNotFound, err)
		}
		return fmt.Errorf("send-keys failed: %w", err)
	}

	// Poll to verify the prompt cleared. Do NOT re-send the keystroke —
	// if Claude is slow to process, re-sending would type into whatever
	// comes next (message input or a subsequent approval).
	for range respondVerifyAttempts {
		time.Sleep(time.Duration(respondVerifyMs) * time.Millisecond)

		verifyText, verifyErr := t.CapturePane(name, 40)
		if verifyErr != nil {
			// Pane gone — session ended, treat as success.
			t.approvalDedup().clear(name)
			return nil
		}

		if parseApprovalPrompt(verifyText) == nil {
			// Prompt cleared — success.
			t.approvalDedup().clear(name)
			return nil
		}
	}

	return fmt.Errorf("approval prompt did not clear after %d verify attempts", respondVerifyAttempts)
}
