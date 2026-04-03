package tmux

import (
	"crypto/sha256"
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
// Deduplication
// ---------------------------------------------------------------------------

// Per-session dedup state to avoid re-emitting the same approval.
type approvalDedup struct {
	mu       sync.Mutex
	lastHash map[string]string // session name → hash of last emitted approval
}

var dedup = &approvalDedup{lastHash: make(map[string]string)}

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
		if strings.Contains(err.Error(), "can't find") || strings.Contains(err.Error(), "no server") {
			return nil, nil
		}
		return nil, fmt.Errorf("capturing pane: %w", err)
	}

	approval := parseApprovalPrompt(paneText)
	if approval == nil {
		dedup.clear(name)
		return nil, nil
	}

	// Dedup: don't re-emit the same approval on repeated polls.
	if !dedup.isNew(name, approval) {
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
		return fmt.Errorf("pre-verify capture failed: %w", err)
	}
	current := parseApprovalPrompt(paneText)
	if current == nil {
		dedup.clear(name)
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

	// Send the keystroke once.
	if _, err := t.run("send-keys", "-t", name, "-l", key); err != nil {
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
			dedup.clear(name)
			return nil
		}

		if parseApprovalPrompt(verifyText) == nil {
			// Prompt cleared — success.
			dedup.clear(name)
			return nil
		}
	}

	return fmt.Errorf("approval prompt did not clear after %d verify attempts", respondVerifyAttempts)
}
