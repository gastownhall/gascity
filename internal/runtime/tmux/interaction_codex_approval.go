package tmux

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

var (
	codexApprovalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// These are the native literal wrappers observed on Codex0.162.1. This is not
	// a JavaScript evaluator: comments, other statements, computed values and
	// multiple calls are unsupported and make the approval unavailable.
	codexLiteralExecWrapper       = regexp.MustCompile(`(?s)^\s*const\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*await\s+tools\.exec_command\(\s*(\{.*\})\s*\)\s*;\s*text\(\s*([A-Za-z_$][A-Za-z0-9_$]*)\s*\)\s*;\s*$`)
	codexLiteralInlineExecWrapper = regexp.MustCompile(`(?s)^\s*text\(\s*await\s+tools\.exec_command\(\s*(\{.*\})\s*\)\s*\)\s*;\s*$`)
)

// codexLiteralExecCommand recognizes only one literal native tool call. JSON
// string decoding preserves command bytes; punctuation inside strings is data.
func codexLiteralExecCommand(code string) (string, string, bool) {
	if len(code) > 64*1024 {
		return "", "", false
	}
	var object string
	if match := codexLiteralExecWrapper.FindStringSubmatch(code); match != nil && match[1] == match[3] {
		object = match[2]
	} else if match := codexLiteralInlineExecWrapper.FindStringSubmatch(code); match != nil {
		object = match[1]
	} else {
		return "", "", false
	}
	pos := 1
	skip := func() {
		for pos < len(object) && strings.ContainsRune(" \t\r\n", rune(object[pos])) {
			pos++
		}
	}
	values := map[string]json.RawMessage{}
	for {
		skip()
		if pos >= len(object) {
			return "", "", false
		}
		if object[pos] == '}' {
			pos++
			break
		}
		var key string
		if object[pos] == '"' {
			decoder := json.NewDecoder(strings.NewReader(object[pos:]))
			if decoder.Decode(&key) != nil {
				return "", "", false
			}
			pos += int(decoder.InputOffset())
		} else {
			start := pos
			for pos < len(object) && (object[pos] >= 'a' && object[pos] <= 'z' || object[pos] == '_') {
				pos++
			}
			key = object[start:pos]
		}
		switch key {
		case "cmd", "sandbox_permissions", "justification", "prefix_rule", "workdir", "yield_time_ms", "max_output_tokens", "shell", "login", "tty":
		default:
			return "", "", false
		}
		if _, duplicate := values[key]; duplicate {
			return "", "", false
		}
		skip()
		if pos >= len(object) || object[pos] != ':' {
			return "", "", false
		}
		pos++
		decoder := json.NewDecoder(strings.NewReader(object[pos:]))
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return "", "", false
		}
		pos += int(decoder.InputOffset())
		values[key] = raw
		skip()
		if pos >= len(object) {
			return "", "", false
		}
		if object[pos] == '}' {
			pos++
			break
		}
		if object[pos] != ',' {
			return "", "", false
		}
		pos++
	}
	if strings.TrimSpace(object[pos:]) != "" {
		return "", "", false
	}
	var command, description string
	if json.Unmarshal(values["cmd"], &command) != nil || command == "" {
		return "", "", false
	}
	if raw, present := values["justification"]; present && json.Unmarshal(raw, &description) != nil {
		return "", "", false
	}
	return command, description, true
}

type codexApprovalInvocation struct {
	id, command, description string
	supported                bool
}

// bindCodexApproval uses only the rollout held by this pane's process tree.
// The exact native UUID and unresolved outer invocation identify one occurrence;
// an outer input marked completed is not completion until its output is logged.
func (t *Tmux) bindCodexApproval(target string, approval *parsedApproval) error {
	if approval == nil || approval.ToolName != "exec" {
		return nil
	}
	path := t.codexRolloutOf(target)
	if path == "" {
		env, err := t.GetAllEnvironment(target)
		if err != nil {
			return fmt.Errorf("reading native Codex approval identity: %w", err)
		}
		if env["GC_SESSION_ID"] != "" || env["GC_PROVIDER_SESSION_ID"] != "" {
			return fmt.Errorf("%w: cannot identify the managed approval's native Codex rollout", runtime.ErrRuntimeUnavailable)
		}
		return nil // Genuinely legacy/unmanaged runtime: visible-content identity.
	}
	if len(approval.CommandRows) == 0 || approval.Unreadable != "" {
		return fmt.Errorf("%w: native Codex approval command boundaries are unavailable", runtime.ErrRuntimeUnavailable)
	}
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(name) < 36 {
		return fmt.Errorf("%w: native Codex approval has no conversation UUID", runtime.ErrRuntimeUnavailable)
	}
	id := name[len(name)-36:]
	if !codexApprovalUUID.MatchString(id) {
		return fmt.Errorf("%w: native Codex approval conversation UUID is invalid", runtime.ErrRuntimeUnavailable)
	}
	owned, err := sessionlog.TurnTranscriptOwnedBy("codex", path, id)
	if err != nil {
		return fmt.Errorf("%w: reading native Codex approval UUID: %w", runtime.ErrRuntimeUnavailable, err)
	}
	if !owned {
		return fmt.Errorf("%w: native Codex approval rollout does not record its owned UUID", runtime.ErrRuntimeUnavailable)
	}
	tail, err := readQuestionTranscriptTail(path)
	if err != nil {
		return err
	}
	var pending []codexApprovalInvocation
	for _, line := range strings.Split(tail, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct {
			Type    string
			Payload struct {
				Type, Name, Input, Arguments string
				CallID                       string `json:"call_id"`
			}
		}
		if json.Unmarshal([]byte(line), &row) != nil {
			return fmt.Errorf("%w: native Codex approval record is incomplete", runtime.ErrRuntimeUnavailable)
		}
		p := row.Payload
		if row.Type == "event_msg" && (p.Type == "task_started" || p.Type == "task_complete" || p.Type == "turn_aborted") {
			pending = nil
			continue
		}
		if row.Type != "response_item" {
			continue
		}
		if p.Type == "custom_tool_call" && p.Name == "exec" {
			if p.CallID == "" {
				return fmt.Errorf("%w: native Codex approval invocation has no identity", runtime.ErrRuntimeUnavailable)
			}
			command, description, supported := codexLiteralExecCommand(p.Input)
			pending = append(pending, codexApprovalInvocation{p.CallID, command, description, supported})
		}
		if p.Type == "function_call" && (p.Name == "exec_command" || strings.HasSuffix(p.Name, ".exec_command")) {
			if p.CallID == "" {
				return fmt.Errorf("%w: native Codex approval invocation has no identity", runtime.ErrRuntimeUnavailable)
			}
			var args struct {
				Command       string `json:"cmd"`
				Justification string `json:"justification"`
			}
			err := json.Unmarshal([]byte(p.Arguments), &args)
			pending = append(pending, codexApprovalInvocation{p.CallID, args.Command, args.Justification, err == nil && args.Command != ""})
		}
		if p.Type == "function_call_output" || p.Type == "custom_tool_call_output" {
			if p.CallID == "" {
				return fmt.Errorf("%w: native Codex approval completion has no invocation identity", runtime.ErrRuntimeUnavailable)
			}
			keep := pending[:0]
			for _, call := range pending {
				if call.id != p.CallID {
					keep = append(keep, call)
				}
			}
			pending = keep
		}
	}
	var found *codexApprovalInvocation
	for i := range pending {
		call := &pending[i]
		if !call.supported {
			return fmt.Errorf("%w: native Codex approval wrapper is unsupported; only one literal exec_command call is supported", runtime.ErrInteractionActionUnavailable)
		}
		if !nativeApprovalCommandMatches(call.command, approval.CommandRows, approval.CommandWidth, approval.CommandWidth-2) {
			continue
		}
		if found != nil {
			return fmt.Errorf("%w: native Codex approval is ambiguous", runtime.ErrRuntimeUnavailable)
		}
		found = call
	}
	if found == nil {
		return fmt.Errorf("%w: visible approval has no matching unresolved native Codex invocation", runtime.ErrRuntimeUnavailable)
	}
	approval.Command, approval.Description = found.command, found.description
	approval.Input = truncateApprovalInput(found.command)
	approval.NativeID = "codex-" + id + "-" + found.id
	return nil
}
