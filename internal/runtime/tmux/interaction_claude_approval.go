package tmux

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/gastownhall/gascity/internal/runtime"
)

// boxedApprovalRows keeps the visible command's row boundaries. They can prove
// whether a native command explains the pane, but cannot reconstruct hard
// newlines or a long word without the provider's exact command.
func boxedApprovalRows(lines []string) ([]string, int) {
	open := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "╌") {
			open = i
			break
		}
	}
	if open < 0 {
		return nil, 0
	}
	width := len([]rune(strings.TrimRight(lines[open], " "))) - 4
	var rows []string
	for _, line := range lines[open+1:] {
		if strings.HasPrefix(strings.TrimSpace(line), "╌") {
			return rows, width
		}
		line = strings.TrimRight(line, " ")
		left := strings.TrimLeft(line, " ")
		if strings.HasPrefix(left, "│") {
			line = strings.TrimPrefix(strings.TrimPrefix(left, "│"), " ")
		} else {
			// The older unmarked box adds one layout column only.
			line = strings.TrimPrefix(line, " ")
		}
		rows = append(rows, line)
	}
	return nil, 0
}

// nativeApprovalCommandMatches checks exact native bytes against every drawn
// row. A boundary may be a native newline, a full-width word split, or one
// space lost at a proven word wrap. It never strips arbitrary shell whitespace.
// Unknown terminal widths, non-ASCII display widths and control expansions are
// deliberately unsupported until their native rendering can be proven.
func nativeApprovalCommandMatches(command string, rows []string, width int, firstWidth ...int) bool {
	if len(rows) == 0 || width <= 0 || command == "" {
		return false
	}
	for _, r := range command {
		if r != '\n' && (r > unicode.MaxASCII || unicode.IsControl(r)) {
			return false
		}
	}
	at := 0
	for i, row := range rows {
		limit := width
		if i == 0 && len(firstWidth) == 1 {
			limit = firstWidth[0]
		}
		if limit <= 0 || len(row) > limit || !strings.HasPrefix(command[at:], row) {
			return false
		}
		at += len(row)
		if i == len(rows)-1 {
			return at == len(command)
		}
		if at == len(command) {
			return false
		}
		if command[at] == '\n' {
			at++
			continue
		}
		if len(row) == limit && strings.HasPrefix(command[at:], rows[i+1]) {
			continue
		}
		first := rows[i+1]
		if space := strings.IndexByte(first, ' '); space >= 0 {
			first = first[:space]
		}
		if command[at] == ' ' && len(row)+1+len(first) > limit && strings.HasPrefix(command[at+1:], rows[i+1]) {
			at++
			continue
		}
		return false
	}
	return false
}

type claudeApprovalInvocation struct {
	id, prompt, command, description string
	projection                       *claudeApprovalProjection
}

// bindClaudeApproval reads only the exact native conversation already resolved
// by the pane's runtime identity. It requires one unresolved Bash invocation
// whose intact command explains the visible rows; same wording on a later call
// has another request identity. Pending is read-only and never answers a menu.
func (t *Tmux) bindClaudeApproval(target string, approval *parsedApproval) error {
	if approval.ToolName != "Bash" {
		return nil
	}
	env, err := t.GetAllEnvironment(target)
	if err != nil {
		return fmt.Errorf("reading native Claude approval identity: %w", err)
	}
	expected := env["GC_PROVIDER_SESSION_ID"]
	if expected == "" {
		if env["GC_SESSION_ID"] != "" {
			return fmt.Errorf("%w: managed Claude approval has no native conversation UUID", runtime.ErrRuntimeUnavailable)
		}
		return nil // Genuinely legacy/unmanaged runtime: visible-content identity.
	}
	if len(approval.CommandRows) == 0 {
		return fmt.Errorf("%w: native Claude approval command boundaries are unavailable", runtime.ErrRuntimeUnavailable)
	}
	read := t.claudeQuestionTranscript
	if read == nil {
		read = t.readClaudeQuestionTranscript
	}
	tail, err := read(target)
	if err != nil {
		return fmt.Errorf("reading native Claude approval invocation: %w", err)
	}
	if tail == "" {
		return fmt.Errorf("%w: native Claude approval record is unavailable", runtime.ErrRuntimeUnavailable)
	}
	var pending []claudeApprovalInvocation
	completed := map[string]bool{}
	prompt, latestSession := "", ""
	for _, line := range strings.Split(tail, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct {
			Type, SessionID, PromptID string
			Message                   struct{ Content json.RawMessage }
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return fmt.Errorf("%w: native Claude approval record is incomplete", runtime.ErrRuntimeUnavailable)
		}
		content := strings.TrimSpace(string(row.Message.Content))
		if row.Type == "user" && !strings.HasPrefix(content, "[") {
			pending, prompt, latestSession = nil, row.PromptID, row.SessionID
		}
		if !strings.HasPrefix(content, "[") {
			continue
		}
		var blocks []struct {
			Type, ID, Name string
			ToolUseID      string `json:"tool_use_id"`
			Input          struct{ Command, Description string }
		}
		if err := json.Unmarshal(row.Message.Content, &blocks); err != nil {
			return fmt.Errorf("%w: native Claude approval content is incomplete", runtime.ErrRuntimeUnavailable)
		}
		result := false
		for _, block := range blocks {
			if row.Type == "assistant" && block.Type == "tool_use" && block.Name == "Bash" {
				if row.SessionID != expected || block.ID == "" || block.Input.Command == "" {
					return fmt.Errorf("%w: native Claude approval identity disagrees with its runtime", runtime.ErrRuntimeUnavailable)
				}
				pending = append(pending, claudeApprovalInvocation{id: block.ID, prompt: prompt, command: block.Input.Command, description: block.Input.Description})
			}
			if row.Type == "user" && block.Type == "tool_result" {
				if block.ToolUseID == "" {
					return fmt.Errorf("%w: native Claude approval completion has no invocation identity", runtime.ErrRuntimeUnavailable)
				}
				result = true
				completed[block.ToolUseID] = true
				keep := pending[:0]
				for _, call := range pending {
					if call.id != block.ToolUseID {
						keep = append(keep, call)
					}
				}
				pending = keep
			}
		}
		if row.Type == "user" && !result {
			pending, prompt, latestSession = nil, row.PromptID, row.SessionID
		}
	}
	// A callback may precede the assistant record, but it cannot establish the
	// current native prompt. Anchor it to the canonical user record first.
	if latestSession == expected && prompt != "" {
		readProjections := t.claudeApprovalProjections
		if readProjections == nil {
			readProjections = t.readClaudeApprovalProjections
		}
		projections, err := readProjections(target)
		if err != nil {
			return fmt.Errorf("reading native Claude approval observations: %w", err)
		}
		for _, projection := range projections {
			if projection.SessionID != expected || projection.PromptID != prompt || completed[projection.ToolUseID] {
				continue
			}
			matched := false
			for i := range pending {
				call := &pending[i]
				if call.id != projection.ToolUseID {
					continue
				}
				if call.prompt != projection.PromptID || call.command != projection.Command || call.description != projection.Description {
					return fmt.Errorf("%w: native Claude approval callback conflicts with its transcript", runtime.ErrRuntimeUnavailable)
				}
				observed := projection
				call.projection = &observed
				matched = true
			}
			if !matched {
				observed := projection
				pending = append(pending, claudeApprovalInvocation{
					id: projection.ToolUseID, prompt: projection.PromptID,
					command: projection.Command, description: projection.Description, projection: &observed,
				})
			}
		}
	}
	var found *claudeApprovalInvocation
	for i := range pending {
		call := &pending[i]
		if !nativeApprovalCommandMatches(call.command, approval.CommandRows, approval.CommandWidth) {
			continue
		}
		if found != nil {
			return fmt.Errorf("%w: native Claude approval is ambiguous", runtime.ErrRuntimeUnavailable)
		}
		found = call
	}
	if found == nil {
		return fmt.Errorf("%w: visible approval has no matching unresolved native Claude invocation", runtime.ErrRuntimeUnavailable)
	}
	approval.Command, approval.Description = found.command, found.description
	approval.Input = truncateApprovalInput(strings.TrimSuffix(found.command+"\n"+found.description, "\n"))
	approval.NativeID = "claude-" + expected + "-" + found.prompt + "-" + found.id
	approval.NativeCommand = found.command
	approval.NativeSessionID, approval.NativePromptID, approval.NativeToolUseID = expected, found.prompt, found.id
	approval.ClaudeProjection = found.projection
	if found.projection != nil && found.projection.Consumed {
		return fmt.Errorf("%w: native Claude approval invocation was already consumed", runtime.ErrRuntimeUnavailable)
	}
	return nil
}
