package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// SIGINT handling modes for --sigint.
const (
	sigintIgnore = "ignore"
	sigintExit   = "exit"
	sigintCancel = "cancel"
)

// session/cancel handling modes for --on-cancel.
const (
	onCancelReply  = "reply"
	onCancelIgnore = "ignore"
)

// usage is the unstable ACP token-usage object attached to a prompt result.
type usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

// options is the parsed scenario for one fakeacp process.
type options struct {
	sessionID         string
	logDir            string
	chunks            []string
	messageIDs        bool
	thought           string
	toolCall          bool
	turnDelay         time.Duration
	stopReason        string
	usage             *usage
	promptError       string
	exitDuringPrompt  bool
	requestFSRead     string
	requestPermission bool
	requestIDString   bool
	permissionTimeout time.Duration
	sigint            string
	onCancel          string
	cancelLatency     time.Duration
}

// parseOptions parses the scenario flags. Defaults reproduce the plain echo
// agent the Provider conformance suites rely on.
func parseOptions(args []string) (options, error) {
	var o options
	var chunks, usageSpec string
	fs := flag.NewFlagSet("fakeacp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.sessionID, "session-id", "fakeacp-session-1", "ACP session ID returned by session/new")
	fs.StringVar(&o.logDir, "log-dir", "", "directory for methods.jsonl, initialize.json, prompts.jsonl, responses.jsonl, signals.jsonl")
	fs.StringVar(&chunks, "chunks", "", "reply as '|'-separated agent_message_chunk updates sharing one messageId")
	fs.BoolVar(&o.messageIDs, "message-ids", false, "attach a messageId to the default reply chunk and the thought")
	fs.StringVar(&o.thought, "thought", "", "send an agent_thought_chunk with this text before the reply")
	fs.BoolVar(&o.toolCall, "tool-call", false, "send a pending tool_call then a completed tool_call_update with text content")
	fs.DurationVar(&o.turnDelay, "turn-delay", 0, "wait this long before answering a prompt (a cancel ends the wait)")
	fs.StringVar(&o.stopReason, "stop-reason", "end_turn", "stopReason for a completed prompt")
	fs.StringVar(&usageSpec, "usage", "", "IN,OUT token counts for the prompt result's usage object")
	fs.StringVar(&o.promptError, "prompt-error", "", "answer the prompt with JSON-RPC error -32603 and this message")
	fs.BoolVar(&o.exitDuringPrompt, "exit-during-prompt", false, "exit 3 after receiving a prompt without answering it")
	fs.StringVar(&o.requestFSRead, "request-fs-read", "", "send fs/read_text_file for this path before answering")
	fs.BoolVar(&o.requestPermission, "request-permission", false, "send session/request_permission before answering and act on the reply")
	fs.BoolVar(&o.requestIDString, "request-id-string", false, "use string JSON-RPC ids (perm-1, fs-1) for fake-originated requests")
	fs.DurationVar(&o.permissionTimeout, "permission-timeout", 0, "on expiry send $/cancel_request and treat the permission as rejected (0 = wait forever)")
	fs.StringVar(&o.sigint, "sigint", sigintIgnore, "SIGINT handling: ignore, exit (status 130), or cancel (answer the in-flight prompt canceled)")
	fs.StringVar(&o.onCancel, "on-cancel", onCancelReply, "session/cancel handling: reply (answer the in-flight prompt canceled) or ignore")
	fs.DurationVar(&o.cancelLatency, "cancel-latency", 0, "delay before answering a canceled prompt")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if chunks != "" {
		o.chunks = strings.Split(chunks, "|")
	}
	if usageSpec != "" {
		u, err := parseUsage(usageSpec)
		if err != nil {
			return options{}, err
		}
		o.usage = &u
	}
	switch o.sigint {
	case sigintIgnore, sigintExit, sigintCancel:
	default:
		return options{}, fmt.Errorf("--sigint %q: want ignore, exit, or cancel", o.sigint)
	}
	switch o.onCancel {
	case onCancelReply, onCancelIgnore:
	default:
		return options{}, fmt.Errorf("--on-cancel %q: want reply or ignore", o.onCancel)
	}
	return o, nil
}

func parseUsage(spec string) (usage, error) {
	in, out, ok := strings.Cut(spec, ",")
	if !ok {
		return usage{}, errors.New("--usage: want IN,OUT")
	}
	inTokens, err := strconv.Atoi(strings.TrimSpace(in))
	if err != nil {
		return usage{}, fmt.Errorf("--usage input tokens: %w", err)
	}
	outTokens, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return usage{}, fmt.Errorf("--usage output tokens: %w", err)
	}
	return usage{InputTokens: inTokens, OutputTokens: outTokens, TotalTokens: inTokens + outTokens}, nil
}
