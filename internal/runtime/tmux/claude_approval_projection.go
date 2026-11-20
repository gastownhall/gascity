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
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const claudeApprovalOptionPrefix = "@gc_claude_approval_v1_"

// claudeApprovalProjection is a disposable native callback observation. A
// consumed observation survives a controller restart and an uncertain key send;
// it is never approval authority again. A native completion clears only its ID.
type claudeApprovalProjection struct {
	Version     int    `json:"version"`
	FenceDigest string `json:"fence_digest"`
	SessionID   string `json:"session_id"`
	PromptID    string `json:"prompt_id"`
	ToolUseID   string `json:"tool_use_id"`
	Command     string `json:"command"`
	Description string `json:"description"`
	Consumed    bool   `json:"consumed"`
	encoded     string
}

func (p claudeApprovalProjection) optionKey() string {
	sum := sha256.Sum256([]byte(p.SessionID + "\x00" + p.PromptID + "\x00" + p.ToolUseID))
	return claudeApprovalOptionPrefix + p.FenceDigest + "_" + hex.EncodeToString(sum[:16])
}

func (p claudeApprovalProjection) encode() (string, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	encoded := base64.RawStdEncoding.EncodeToString(data)
	if len(encoded) > claudeQuestionProjectionLimit {
		return "", errors.New("native approval observation exceeds limit")
	}
	return encoded, nil
}

func (p claudeApprovalProjection) observationDigest() string {
	data, _ := base64.RawStdEncoding.DecodeString(p.encoded)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ObserveClaudeApprovalHook records Bash's native PreToolUse identity before
// Claude publishes its assistant record. It never approves or changes a call.
func ObserveClaudeApprovalHook(ctx context.Context, input io.Reader, env map[string]string) error {
	return observeClaudeApprovalHook(ctx, input, env, realExecutor{}.executeCtx)
}

func observeClaudeApprovalHook(ctx context.Context, input io.Reader, env map[string]string,
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
		return errors.New("native approval hook has no tmux pane identity")
	}
	socket, pid, err := claudeHookSocket(env["TMUX"])
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(input, claudeQuestionProjectionLimit+1))
	if err != nil || len(data) > claudeQuestionProjectionLimit {
		return errors.New("native approval callback cannot be read within observation limit")
	}
	var event struct {
		Event     string                                `json:"hook_event_name"`
		SessionID string                                `json:"session_id"`
		PromptID  string                                `json:"prompt_id"`
		ToolUseID string                                `json:"tool_use_id"`
		ToolName  string                                `json:"tool_name"`
		Input     struct{ Command, Description string } `json:"tool_input"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return errors.New("invalid native approval callback JSON")
	}
	if event.ToolName != "Bash" || event.SessionID == "" || event.SessionID != env["GC_PROVIDER_SESSION_ID"] ||
		event.PromptID == "" || event.ToolUseID == "" || event.Input.Command == "" {
		return errors.New("native approval callback has no matching session, prompt, invocation or command")
	}
	pre := event.Event == "PreToolUse"
	if !pre && event.Event != "PostToolUse" && event.Event != "PostToolUseFailure" {
		return errors.New("unsupported native approval callback event")
	}
	p := claudeApprovalProjection{
		Version: 1, FenceDigest: fence.Digest(), SessionID: event.SessionID,
		PromptID: event.PromptID, ToolUseID: event.ToolUseID, Command: event.Input.Command, Description: event.Input.Description,
	}
	encoded, err := p.encode()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	invoke := func(ctx context.Context, args []string) (string, error) {
		return run(ctx, append([]string{"-S", socket}, args...))
	}
	server, err := invoke(ctx, []string{"display-message", "-p", "-t", pane, "#{pid}"})
	if err != nil || strings.TrimSpace(server) != pid {
		return errors.New("native approval tmux server changed or is unavailable")
	}
	options, err := invoke(ctx, []string{"show-options", "-p", "-t", pane})
	if err != nil {
		return fmt.Errorf("reading native approval observations: %w", err)
	}
	observations, err := decodeClaudeApprovalOptions(options, fence.Digest(), event.SessionID)
	if err != nil {
		return err
	}
	before := ""
	for _, old := range observations {
		if old.optionKey() != p.optionKey() {
			continue
		}
		if old.Command != p.Command || old.Description != p.Description || old.PromptID != p.PromptID || old.ToolUseID != p.ToolUseID {
			return errors.New("native approval callback conflicts with its recorded invocation")
		}
		before = old.encoded
		if pre && old.Consumed {
			return errors.New("native approval invocation was already consumed")
		}
	}
	if !pre && before == "" {
		return nil // A late completion can retire only its own existing invocation.
	}
	if pre && before == "" && len(observations) >= claudeQuestionProjectionCount {
		return errors.New("too many outstanding native approval observations")
	}
	if !pre {
		encoded = ""
	}
	return updateClaudeApprovalOption(ctx, invoke, pane, pane, fence.Digest(), p.optionKey(), before, encoded)
}

func decodeClaudeApprovalOptions(options, digest, session string) ([]claudeApprovalProjection, error) {
	var result []claudeApprovalProjection
	for _, line := range strings.Split(options, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], claudeApprovalOptionPrefix+digest+"_") {
			continue
		}
		if len(fields) != 2 {
			return nil, errors.New("native approval observation is malformed")
		}
		encoded := strings.Trim(fields[1], "\"")
		if len(encoded) > claudeQuestionProjectionLimit {
			return nil, errors.New("native approval observation exceeds limit")
		}
		data, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, errors.New("native approval observation encoding is invalid")
		}
		var fieldsByName map[string]json.RawMessage
		if err := json.Unmarshal(data, &fieldsByName); err != nil || len(fieldsByName) != 8 ||
			(strings.TrimSpace(string(fieldsByName["consumed"])) != "true" && strings.TrimSpace(string(fieldsByName["consumed"])) != "false") {
			return nil, errors.New("native approval observation has an unsupported shape")
		}
		for _, field := range []string{"version", "fence_digest", "session_id", "prompt_id", "tool_use_id", "command", "description", "consumed"} {
			if fieldsByName[field] == nil || strings.TrimSpace(string(fieldsByName[field])) == "null" {
				return nil, errors.New("native approval observation is missing a typed field")
			}
		}
		var p claudeApprovalProjection
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, errors.New("native approval observation is invalid")
		}
		if p.Version != 1 || p.FenceDigest != digest || p.SessionID == "" || p.SessionID != session || p.PromptID == "" || p.ToolUseID == "" || p.Command == "" || p.optionKey() != fields[0] {
			return nil, errors.New("native approval observation identity is invalid")
		}
		p.encoded = encoded
		result = append(result, p)
		if len(result) > claudeQuestionProjectionCount {
			return nil, errors.New("native approval observation count exceeds limit")
		}
	}
	return result, nil
}

// updateClaudeApprovalOption is a compare-and-set inside the existing atomic
// incarnation guard. Concurrent responses cannot both consume one observation.
func updateClaudeApprovalOption(ctx context.Context, run func(context.Context, []string) (string, error),
	target, pane, digest, key, before, after string,
) error {
	inner := []string{"set-option", "-p", "-t", target, key, after}
	if after == "" {
		inner = []string{"set-option", "-p", "-u", "-t", target, key}
	}
	args, err := runtime.TmuxGuardedInputArgs(target, pane, digest, inner, "")
	if err != nil {
		return err
	}
	// key and before come only from our bounded, validated projection encoding.
	args[4] = "#{&&:" + args[4] + ",#{==:#{" + key + "}," + before + "}}"
	out, err := run(ctx, args)
	if err != nil {
		return fmt.Errorf("updating native approval observation: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("%w: native approval observation or incarnation changed", runtime.ErrInteractionGone)
	}
	return nil
}

func (t *Tmux) readClaudeApprovalProjections(target string) ([]claudeApprovalProjection, error) {
	return t.readClaudeApprovalProjectionsCtx(context.Background(), target)
}

func (t *Tmux) readClaudeApprovalProjectionsCtx(ctx context.Context, target string) ([]claudeApprovalProjection, error) {
	guard := paneGuardFrom(ctx)
	readError := func(err error) error {
		if guard != nil {
			return guard.gone(err)
		}
		return err
	}
	env, err := t.getAllEnvironmentCtx(ctx, target)
	if err != nil {
		return nil, readError(err)
	}
	fence, managed := runtime.InputFenceFromEnv(env)
	if !managed {
		return nil, nil
	}
	if err := fence.Validate(); err != nil {
		return nil, err
	}
	readFence := func() (string, error) {
		out, err := t.runCtxRaw(ctx, "display-message", "-p", "-t", target, fenceReadFormat)
		if err != nil {
			return "", readError(err)
		}
		read, valid := parseFenceRead(out)
		if guard != nil && valid {
			switch {
			case read.Absent() || read.Pane != guardPaneID(target):
				return "", guard.gone(errAgentPaneGone)
			case read.Dead || read.Own() != guard.digest:
				return "", guard.recordMiss(read.Found())
			case read.Pane != guard.verifiedPane():
				return "", guard.recordMiss(runtime.FenceFoundUnproven)
			}
		}
		if !valid || read.Pane != guardPaneID(target) || read.Dead || read.Own() != fence.Digest() {
			return "", errors.New("native approval incarnation is no longer current")
		}
		return out, nil
	}
	out, err := readFence()
	if err != nil {
		return nil, err
	}
	options, err := t.runCtxRaw(ctx, "show-options", "-p", "-t", target)
	if err != nil {
		return nil, readError(err)
	}
	after, err := readFence()
	if err != nil {
		return nil, err
	}
	if after != out {
		return nil, errors.New("native approval incarnation changed while reading observations")
	}
	return decodeClaudeApprovalOptions(options, fence.Digest(), env["GC_PROVIDER_SESSION_ID"])
}

func (t *Tmux) consumeClaudeApproval(ctx context.Context, target string, p *claudeApprovalProjection) error {
	if p == nil || p.Consumed {
		return nil
	}
	if guard := paneGuardFrom(ctx); guard != nil && (guard.digest != p.FenceDigest || guard.verifiedPane() != guardPaneID(target)) {
		return guard.recordMiss(runtime.FenceFoundUnproven)
	}
	consumed := *p
	consumed.Consumed = true
	encoded, err := consumed.encode()
	if err != nil {
		return err
	}
	return updateClaudeApprovalOption(ctx, func(ctx context.Context, args []string) (string, error) {
		return t.runCtxRaw(ctx, args...)
	}, target, guardPaneID(target), p.FenceDigest, p.optionKey(), p.encoded, encoded)
}

func (t *Tmux) consumeClaudeApprovalsForStop(ctx context.Context, target string) error {
	projections, err := t.readClaudeApprovalProjectionsCtx(ctx, target)
	if err != nil {
		return err
	}
	for i := range projections {
		if err := t.consumeClaudeApproval(ctx, target, &projections[i]); err != nil {
			return err
		}
	}
	return nil
}

// claudeApprovalInputKey carries one observed invocation to its own key command.
// The compare-and-set and that command run in the same tmux command list.
type claudeApprovalInputKey struct{}

func withClaudeApprovalInput(ctx context.Context, p *claudeApprovalProjection) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, claudeApprovalInputKey{}, p)
}

func claudeApprovalInput(ctx context.Context) *claudeApprovalProjection {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(claudeApprovalInputKey{}).(*claudeApprovalProjection)
	return p
}

func claudeApprovalGuardedInputArgs(target string, p *claudeApprovalProjection, guarded []string) ([]string, error) {
	if p == nil {
		return guarded, nil
	}
	if p.Consumed || p.encoded == "" || len(guarded) != 7 {
		return nil, fmt.Errorf("%w: native approval observation is already consumed or unavailable", runtime.ErrInteractionGone)
	}
	consumed := *p
	consumed.Consumed = true
	encoded, err := consumed.encode()
	if err != nil {
		return nil, err
	}
	set, err := tmuxCommandString("set-option", "-p", "-t", target, p.optionKey(), encoded)
	if err != nil {
		return nil, err
	}
	guarded[4] = "#{&&:" + guarded[4] + ",#{==:#{" + p.optionKey() + "}," + p.encoded + "}}"
	guarded[5] = set + " ; " + guarded[5]
	return guarded, nil
}

func (t *Tmux) ordinaryClaudeApprovalInput(ctx context.Context, args []string, p *claudeApprovalProjection) (string, error) {
	in, err := parseInputCommand(args)
	if err != nil {
		return "", err
	}
	guarded, err := guardedInputArgs(in.target, p.FenceDigest, args, "")
	if err == nil {
		guarded, err = claudeApprovalGuardedInputArgs(in.target, p, guarded)
	}
	if err != nil {
		return "", err
	}
	out, err := t.runCtxRaw(ctx, guarded...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) != "" {
		return "", fmt.Errorf("%w: native approval observation or incarnation changed", runtime.ErrInteractionGone)
	}
	return "", nil
}
