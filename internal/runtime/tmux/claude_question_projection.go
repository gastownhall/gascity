package tmux

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	claudeQuestionOptionPrefix    = "@gc_claude_question_v1_"
	claudeQuestionProjectionLimit = 64 * 1024
	claudeQuestionProjectionCount = 32
)

// claudeQuestionProjection is a disposable observation of a native invocation,
// not domain state or an alternative conversation history.
type claudeQuestionProjection struct {
	Version     int              `json:"version"`
	FenceDigest string           `json:"fence_digest"`
	SessionID   string           `json:"session_id"`
	PromptID    string           `json:"prompt_id"`
	ToolUseID   string           `json:"tool_use_id"`
	Questions   []claudeQuestion `json:"questions"`
}

type claudeQuestion struct {
	Header   string `json:"header"`
	Question string `json:"question"`
	Options  []struct {
		Label string `json:"label"`
	} `json:"options"`
	MultiSelect bool `json:"multiSelect"`
}

// ObserveClaudeQuestionHook projects the native AskUserQuestion invocation into
// its own tmux pane. Other backends leave terminal-only questions unchanged.
// PreToolUse must fail closed when a managed tmux invocation cannot be recorded.
func ObserveClaudeQuestionHook(ctx context.Context, input io.Reader, env map[string]string) error {
	return observeClaudeQuestionHook(ctx, input, env, realExecutor{}.executeCtx)
}

func observeClaudeQuestionHook(ctx context.Context, input io.Reader, env map[string]string,
	run func(context.Context, []string) (string, error),
) error {
	fence, managed := runtime.InputFenceFromEnv(env)
	if !managed || env["TMUX"] == "" {
		return nil
	}
	if err := fence.Validate(); err != nil {
		return err
	}
	pane := env["TMUX_PANE"]
	if !runtime.TmuxPaneID(pane) {
		return errors.New("native question hook has no tmux pane identity")
	}
	socket, pid, err := claudeHookSocket(env["TMUX"])
	if err != nil {
		return err
	}
	payload, err := io.ReadAll(io.LimitReader(input, claudeQuestionProjectionLimit+1))
	if err != nil {
		return fmt.Errorf("reading native question callback: %w", err)
	}
	if len(payload) > claudeQuestionProjectionLimit {
		return errors.New("native question callback exceeds observation limit")
	}
	var event struct {
		Event     string `json:"hook_event_name"`
		SessionID string `json:"session_id"`
		PromptID  string `json:"prompt_id"`
		ToolUseID string `json:"tool_use_id"`
		ToolName  string `json:"tool_name"`
		Input     struct {
			Questions []claudeQuestion `json:"questions"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return errors.New("invalid native question callback JSON")
	}
	if event.ToolName != "AskUserQuestion" || event.SessionID == "" || event.PromptID == "" || event.ToolUseID == "" {
		return errors.New("native question callback is missing its tool, session, prompt or invocation identity")
	}
	pre := event.Event == "PreToolUse"
	if !pre && event.Event != "PostToolUse" && event.Event != "PostToolUseFailure" {
		return errors.New("unsupported native question callback event")
	}
	projection := claudeQuestionProjection{
		Version: 1, FenceDigest: fence.Digest(), SessionID: event.SessionID,
		PromptID: event.PromptID, ToolUseID: event.ToolUseID, Questions: event.Input.Questions,
	}
	if pre && len(projection.Questions) == 0 {
		return errors.New("native question callback contains no questions")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	invoke := func(args ...string) (string, error) { return run(ctx, append([]string{"-S", socket}, args...)) }
	server, err := invoke("display-message", "-p", "-t", pane, "#{pid}")
	if err != nil {
		return fmt.Errorf("reading native question tmux server: %w", err)
	}
	if strings.TrimSpace(server) != pid {
		return errors.New("native question tmux server changed")
	}
	guarded := func(inner ...string) error {
		args, err := runtime.TmuxGuardedInputArgs(pane, pane, fence.Digest(), inner, "")
		if err != nil {
			return err
		}
		out, err := invoke(args...)
		if err != nil {
			return fmt.Errorf("recording native question observation: %w", err)
		}
		if strings.TrimSpace(out) != "" {
			return errors.New("native question observation refused by incarnation fence")
		}
		return nil
	}
	key := projection.optionKey()
	if !pre {
		return guarded("set-option", "-p", "-u", "-t", pane, key)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return err
	}
	encoded := base64.RawStdEncoding.EncodeToString(data)
	if len(encoded) > claudeQuestionProjectionLimit {
		return errors.New("native question projection exceeds observation limit")
	}
	options, err := invoke("show-options", "-p", "-t", pane)
	if err != nil {
		return fmt.Errorf("reading native question observations: %w", err)
	}
	count := 0
	for _, line := range strings.Split(options, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 1 || !strings.HasPrefix(fields[0], claudeQuestionOptionPrefix) {
			continue
		}
		if !strings.HasPrefix(fields[0], claudeQuestionOptionPrefix+fence.Digest()+"_") {
			if err := guarded("set-option", "-p", "-u", "-t", pane, fields[0]); err != nil {
				return err
			}
			continue
		}
		if fields[0] != key {
			count++
		}
	}
	if count >= claudeQuestionProjectionCount {
		return errors.New("too many outstanding native question observations")
	}
	return guarded("set-option", "-p", "-t", pane, key, encoded)
}

func claudeHookSocket(raw string) (string, string, error) {
	last := strings.LastIndexByte(raw, ',')
	if last < 0 {
		return "", "", errors.New("invalid native question tmux context")
	}
	previous := strings.LastIndexByte(raw[:last], ',')
	if previous < 0 {
		return "", "", errors.New("invalid native question tmux context")
	}
	socket, pid := raw[:previous], raw[previous+1:last]
	number, err := strconv.Atoi(pid)
	if !filepath.IsAbs(socket) || err != nil || number <= 0 {
		return "", "", errors.New("invalid native question tmux server identity")
	}
	return socket, pid, nil
}

func (p claudeQuestionProjection) optionKey() string {
	sum := sha256.Sum256([]byte(p.SessionID + "\x00" + p.PromptID + "\x00" + p.ToolUseID))
	return claudeQuestionOptionPrefix + p.FenceDigest + "_" + hex.EncodeToString(sum[:16])
}

func (t *Tmux) readClaudeQuestionProjections(target string) ([]claudeQuestionProjection, error) {
	env, err := t.GetAllEnvironment(target)
	if err != nil {
		return nil, fmt.Errorf("reading native question incarnation: %w", err)
	}
	fence, ok := runtime.InputFenceFromEnv(env)
	if !ok {
		return nil, nil
	}
	out, err := t.run("display-message", "-p", "-t", target, fenceReadFormat)
	if err != nil {
		return nil, err
	}
	read, valid := parseFenceRead(out)
	if !valid || read.Pane != guardPaneID(target) || read.Dead || read.Own() != fence.Digest() {
		return nil, errors.New("native question incarnation is no longer current")
	}
	options, err := t.run("show-options", "-p", "-t", target)
	if err != nil {
		return nil, err
	}
	var projections []claudeQuestionProjection
	for _, line := range strings.Split(options, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], claudeQuestionOptionPrefix+fence.Digest()+"_") {
			continue
		}
		if len(fields) != 2 {
			return nil, errors.New("native question observation is malformed")
		}
		encoded := strings.Trim(fields[1], "\"")
		if len(encoded) > claudeQuestionProjectionLimit {
			return nil, errors.New("native question observation exceeds limit")
		}
		data, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, errors.New("native question observation encoding is invalid")
		}
		var projection claudeQuestionProjection
		if err := json.Unmarshal(data, &projection); err != nil {
			return nil, errors.New("native question observation is invalid")
		}
		if projection.Version != 1 || projection.FenceDigest != fence.Digest() || projection.SessionID != env["GC_PROVIDER_SESSION_ID"] ||
			projection.PromptID == "" || projection.ToolUseID == "" || projection.optionKey() != fields[0] {
			return nil, errors.New("native question observation identity is invalid")
		}
		projections = append(projections, projection)
		if len(projections) > claudeQuestionProjectionCount {
			return nil, errors.New("native question observation count exceeds limit")
		}
	}
	return projections, nil
}
