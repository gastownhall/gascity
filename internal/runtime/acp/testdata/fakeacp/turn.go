package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Fixed identities for scripted tool calls.
const (
	permissionToolCallID = "call_1"
	permissionTitle      = "Run: touch marker"
	scriptedToolCallID   = "tool_1"
	scriptedToolTitle    = "Read fixture"
	scriptedToolOutput   = "fixture contents"
)

// promptParams accepts the ACP session/prompt shape ({sessionId, prompt})
// and the legacy {messages:[{content}]} shape.
type promptParams struct {
	SessionID string          `json:"sessionId"`
	Prompt    []contentBlock  `json:"prompt"`
	Messages  []promptMessage `json:"messages"`
}

type promptMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// flattenPrompt concatenates the text blocks of a prompt.
func flattenPrompt(p promptParams) string {
	blocks := p.Prompt
	if len(blocks) == 0 {
		for _, m := range p.Messages {
			blocks = append(blocks, m.Content...)
		}
	}
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "" || block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// waitFor waits d or until ctx ends; it reports whether d elapsed.
func waitFor(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// runTurn plays the scripted scenario for one prompt. It returns without
// answering when the turn was canceled; the canceller answers instead.
func (a *agent) runTurn(t *turn, raw json.RawMessage) {
	var params promptParams
	if err := json.Unmarshal(raw, &params); err != nil {
		a.answerError(t, fmt.Sprintf("invalid session/prompt params: %v", err))
		return
	}
	text := flattenPrompt(params)
	a.logs.prompt(t.id, params.SessionID, text)

	if a.opts.exitDuringPrompt {
		os.Exit(exitDuringPrompt)
	}
	if a.opts.promptError != "" {
		a.answerError(t, a.opts.promptError)
		return
	}
	if !waitFor(t.ctx, a.opts.turnDelay) {
		return
	}
	if a.opts.thought != "" {
		a.sendChunk("agent_thought_chunk", a.opts.thought, a.optionalMessageID())
	}
	if a.opts.requestFSRead != "" {
		a.requestFSRead(t)
		if t.ctx.Err() != nil {
			return
		}
	}
	if a.opts.requestPermission {
		switch a.requestPermission(t) {
		case permissionAborted:
			return
		case permissionCancelled:
			a.answer(t, "canceled", nil)
			return
		}
	}
	if a.opts.toolCall {
		a.sendToolCall()
	}
	if len(a.opts.chunks) > 0 {
		id := newUUID()
		for _, chunk := range a.opts.chunks {
			a.sendChunk("agent_message_chunk", chunk, id)
		}
	} else {
		a.sendChunk("agent_message_chunk", "echo: "+text, a.optionalMessageID())
	}
	if t.ctx.Err() != nil {
		return
	}
	a.answer(t, a.opts.stopReason, a.opts.usage)
}

func (a *agent) optionalMessageID() string {
	if a.opts.messageIDs {
		return newUUID()
	}
	return ""
}

func (a *agent) sendUpdate(update map[string]any) {
	a.w.notify("session/update", map[string]any{
		"sessionId": a.opts.sessionID,
		"update":    update,
	})
}

func (a *agent) sendChunk(kind, text, messageID string) {
	update := map[string]any{
		"sessionUpdate": kind,
		"content":       textBlock(text),
	}
	if messageID != "" {
		update["messageId"] = messageID
	}
	a.sendUpdate(update)
}

func textBlock(text string) map[string]string {
	return map[string]string{"type": "text", "text": text}
}

func toolContent(text string) []map[string]any {
	return []map[string]any{{"type": "content", "content": textBlock(text)}}
}

func (a *agent) sendToolCall() {
	a.sendUpdate(map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    scriptedToolCallID,
		"title":         scriptedToolTitle,
		"kind":          "read",
		"status":        "pending",
	})
	a.sendUpdate(map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    scriptedToolCallID,
		"status":        "completed",
		"content":       toolContent(scriptedToolOutput),
	})
}

// requestFSRead asks the client for a file and waits for any reply (logged
// by the reader) or the end of the turn. The turn continues either way.
func (a *agent) requestFSRead(t *turn) {
	id, reply := a.out.request("fs", "fs/read_text_file", map[string]any{
		"sessionId": a.opts.sessionID,
		"path":      a.opts.requestFSRead,
	})
	select {
	case <-reply:
	case <-t.ctx.Done():
		a.out.forget(id)
	}
}

type permissionDecision int

const (
	permissionAllowed permissionDecision = iota
	permissionRejected
	permissionCancelled // client answered outcome "canceled"
	permissionAborted   // the turn itself was canceled while waiting
)

// requestPermission sends session/request_permission for a pending execute
// tool call, waits for the reply (or --permission-timeout), and reports the
// tool call's final status.
func (a *agent) requestPermission(t *turn) permissionDecision {
	toolCall := map[string]any{
		"toolCallId": permissionToolCallID,
		"title":      permissionTitle,
		"kind":       "execute",
		"status":     "pending",
	}
	announce := map[string]any{"sessionUpdate": "tool_call"}
	for k, v := range toolCall {
		announce[k] = v
	}
	a.sendUpdate(announce)
	id, reply := a.out.request("perm", "session/request_permission", map[string]any{
		"sessionId": a.opts.sessionID,
		"toolCall":  toolCall,
		"options": []map[string]string{
			{"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"},
			{"optionId": "allow_always", "name": "Always allow", "kind": "allow_always"},
			{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"},
			{"optionId": "reject_always", "name": "Always reject", "kind": "reject_always"},
		},
	})

	var expired <-chan time.Time
	if a.opts.permissionTimeout > 0 {
		timer := time.NewTimer(a.opts.permissionTimeout)
		defer timer.Stop()
		expired = timer.C
	}

	decision, optionID := permissionRejected, ""
	select {
	case msg := <-reply:
		decision, optionID = parsePermissionReply(msg)
	case <-expired:
		a.out.forget(id)
		a.w.notify("$/cancel_request", map[string]any{"requestId": id})
		optionID = "timeout"
	case <-t.ctx.Done():
		a.out.forget(id)
		return permissionAborted
	}
	if t.ctx.Err() != nil {
		return permissionAborted
	}

	switch decision {
	case permissionAllowed:
		a.sendUpdate(map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    permissionToolCallID,
			"status":        "completed",
			"content":       toolContent("permission granted: " + optionID),
		})
	case permissionRejected:
		a.sendUpdate(map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    permissionToolCallID,
			"status":        "failed",
			"content":       toolContent("permission rejected: " + optionID),
		})
	}
	return decision
}

// parsePermissionReply maps a client reply to a decision. Errors and
// unrecognized outcomes count as rejections.
func parsePermissionReply(msg message) (permissionDecision, string) {
	if msg.Error != nil {
		return permissionRejected, fmt.Sprintf("error %d", msg.Error.Code)
	}
	var result struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		return permissionRejected, "malformed reply"
	}
	switch {
	case result.Outcome.Outcome == "canceled":
		return permissionCancelled, "canceled"
	case result.Outcome.Outcome == "selected" && strings.HasPrefix(result.Outcome.OptionID, "allow_"):
		return permissionAllowed, result.Outcome.OptionID
	default:
		return permissionRejected, result.Outcome.OptionID
	}
}

// newUUID returns a random RFC 4122 version 4 UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		fmt.Fprintf(os.Stderr, "fakeacp: random: %v\n", err)
		os.Exit(1)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
