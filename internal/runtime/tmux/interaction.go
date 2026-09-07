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
	proceedRe          = regexp.MustCompile(`(?m)^\s*Do you want to proceed\?\s*$`)
	approvalOptionRe   = regexp.MustCompile(`^\s*(❯)?\s*([1-9])\. (.+?)\s*$`)

	// Tool call header: "● ToolName(args)" or "● ToolName"
	// Uses greedy match to last ")" to handle nested parens in args.
	toolHeaderRe = regexp.MustCompile(`● (\w+)(?:\((.+)\))?`)
)

// approvalOption binds a visible label to its literal selection key.
type approvalOption struct {
	Key   string
	Label string
}

// parsedApproval holds the active tool request and its currently displayed menu.
type parsedApproval struct {
	ToolName string
	Input    string
	Options  []approvalOption
}

// parseApprovalPrompt parses the tmux pane text for a Claude Code approval prompt.
// Returns nil if no approval prompt is found or if the prompt can't be associated
// with a tool header or bordered tool modal (avoids conversational matches).
func parseApprovalPrompt(paneText string) *parsedApproval {
	questions := proceedRe.FindAllStringIndex(paneText, -1)
	if len(questions) == 0 {
		return nil
	}
	question := questions[len(questions)-1]
	options := parseApprovalOptions(paneText[question[1]:])
	if options == nil {
		return nil
	}
	start := 0
	if len(questions) > 1 {
		// Earlier approval dialogs in scrollback cannot supply the current tool.
		start = questions[len(questions)-2][1]
	}
	before := paneText[start:question[0]]
	// Claude 2.1.263 renders a bordered Bash modal without either the old
	// approval marker or a structured tool-call bullet. Associate its command
	// only with that modal, never with preceding conversational output.
	lines := strings.Split(before, "\n")
	for i := len(lines) - 1; i > 0; i-- {
		if strings.TrimSpace(lines[i]) != "Bash command" {
			continue
		}
		if toolHeaderRe.MatchString(strings.Join(lines[i+1:], "\n")) {
			// A newer legacy tool header owns this prompt even when the prior
			// Bash dialog's question/menu was erased from the captured pane.
			break
		}
		border := strings.TrimSpace(lines[i-1])
		if len([]rune(border)) < 8 || strings.Trim(border, "─") != "" {
			return nil
		}
		var input []string
		for _, line := range lines[i+1:] {
			if strings.HasPrefix(line, "   ") && strings.TrimSpace(line) != "" {
				input = append(input, strings.TrimSpace(line))
			}
		}
		if len(input) == 0 {
			return nil
		}
		return &parsedApproval{ToolName: "Bash", Input: strings.Join(input, "\n"), Options: options}
	}
	markers := requiresApprovalRe.FindAllStringIndex(before, -1)
	if len(markers) == 0 {
		return nil
	}
	textBeforeApproval := before[:markers[len(markers)-1][0]]
	matches := toolHeaderRe.FindAllStringSubmatch(textBeforeApproval, -1)
	if len(matches) == 0 {
		return nil
	}
	lastMatch := matches[len(matches)-1]
	approval := &parsedApproval{ToolName: lastMatch[1], Input: lastMatch[2], Options: options}
	if approval.Input == "" {
		approval.Input = extractToolInput(textBeforeApproval, approval.ToolName)
	}
	return approval
}

// Require a selected, numbered menu with unambiguous one-time approval and
// denial. Wrapped labels are retained, including workspace/mode escalation
// choices, but those choices are never mapped to an approval action.
func parseApprovalOptions(text string) []approvalOption {
	var options []approvalOption
	seen := make(map[string]bool)
	selected, yes, no := 0, 0, 0
	footer := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if footer {
			return nil
		}
		if strings.HasPrefix(trimmed, "Esc to cancel") {
			footer = true
			continue
		}
		match := approvalOptionRe.FindStringSubmatch(line)
		if match != nil {
			if seen[match[2]] {
				return nil
			}
			seen[match[2]] = true
			if match[1] != "" {
				selected++
			}
			options = append(options, approvalOption{Key: match[2], Label: match[3]})
			continue
		}
		if len(options) == 0 || !strings.HasPrefix(line, "      ") {
			return nil
		}
		options[len(options)-1].Label += " " + trimmed
	}
	for _, option := range options {
		if option.Label == "Yes" {
			yes++
		}
		if option.Label == "No" {
			no++
		}
	}
	if selected != 1 || yes != 1 || no != 1 {
		return nil
	}
	return options
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
	return strings.Join(captured, "\n")
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
	identity := a.ToolName + "\x00" + a.Input
	for _, option := range a.Options {
		identity += "\x00" + option.Key + "\x00" + option.Label
	}
	h := sha256.Sum256([]byte(identity))
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
		if errors.Is(err, ErrSessionNotFound) {
			return nil, fmt.Errorf("capturing pane: %w: %w", runtime.ErrSessionNotFound, err)
		}
		if strings.Contains(err.Error(), "can't find") || strings.Contains(err.Error(), "no server") {
			return nil, nil
		}
		return nil, fmt.Errorf("capturing pane: %w", err)
	}

	approval := parseApprovalPrompt(paneText)
	if approval == nil {
		t.approvalDedup().clear(name)
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

	options := make([]string, len(approval.Options))
	for i, option := range approval.Options {
		options[i] = option.Label
	}
	return &runtime.PendingInteraction{
		RequestID: requestID,
		Kind:      "approval",
		Prompt:    prompt,
		Options:   options,
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
// a pending tool approval, then verifies the prompt was consumed. Only approve
// (once) and deny are supported; persistent approval and permission-mode changes
// must be made in the native UI, whose menu positions vary between releases.
func (t *Tmux) Respond(name string, response runtime.InteractionResponse) error {
	// Verify the expected approval is still present before sending keys.
	paneText, err := t.CapturePane(name, 40)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return fmt.Errorf("pre-verify capture failed: %w: %w", runtime.ErrSessionNotFound, err)
		}
		return fmt.Errorf("pre-verify capture failed: %w", err)
	}
	current := parseApprovalPrompt(paneText)
	if current == nil {
		if proceedRe.MatchString(paneText) {
			return fmt.Errorf("cannot safely identify the current approval menu")
		}
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

	// Select by the current label, never a fixed position: newer Claude
	// menus place "switch to auto mode" at the former denial position.
	var label string
	switch response.Action {
	case "approve":
		label = "Yes"
	case "deny":
		label = "No"
	default:
		return fmt.Errorf("unsupported approval action %q; only approve-once or deny is supported", response.Action)
	}
	var key string
	for _, option := range current.Options {
		if option.Label == label {
			key = option.Key
		}
	}
	if key == "" {
		return fmt.Errorf("current approval menu has no unambiguous %q option", label)
	}

	// Exit copy-mode first if the pane is parked (the ga-c4w wheel binding),
	// so the approval keystroke reaches the prompt instead of being swallowed
	// by copy-mode.
	t.cancelCopyModeIfParked(name)

	// Send the keystroke once.
	if _, err := t.run("send-keys", "-t", name, "-l", key); err != nil {
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
