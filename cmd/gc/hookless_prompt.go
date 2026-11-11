package main

// Hookless startup context: providers without an executable hook surface.
//
// A provider with supports_hooks = false (fx, for example) cannot run the
// SessionStart hook itself, so the context that hook adds — the step reminder,
// unread mail and a durable auto-handoff continuation from
// `gc prime --hook` — never reaches its model. Core composes the same commands
// the pi overlay runs at a turn start (internal/bootstrap/packs/core/overlay/
// per-provider/pi/.pi/extensions/gc-hooks.js) into the launch payload instead.
//
// The read is deliberately non-destructive. `gc prime --hook --json` is the
// preview form (cmd_prime.go runs it with consumeHandoff=false), so a launch
// that fails after this point cannot archive the only unread auto-handoff mail.
// The cost is that an agent can see the same mail in more than one incarnation;
// at-least-once delivery is the deliberate trade against losing a handoff.
//
// Nothing here is fingerprinted: runtime.Config.PromptSuffix and Nudge are
// excluded from CoreFingerprint, LaunchFingerprint and LiveFingerprint, so
// volatile mail cannot drive a config-drift restart.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const (
	// hooklessContextTimeout bounds the two child commands. A hook runs inside
	// the agent, where a slow store read only delays that agent's first model
	// request; here it delays the launch, so it is bounded.
	hooklessContextTimeout = 30 * time.Second
	// hooklessContextLimit bounds what Core adds to one launch payload. Prime
	// output is normally a few hundred bytes; an unusually large mailbox would
	// otherwise inflate an argv prompt past the exec limit the promptDelivery
	// guard already checked. Over the limit the context is skipped (and logged)
	// rather than truncated: a half-rendered handoff is worse than none, since
	// the mail stays unread and the next launch can deliver it whole.
	hooklessContextLimit = 32 * 1024
)

// hooklessCommand runs one child command and returns its stdout. It is a
// package variable so tests can record argv and env without a real gc binary.
var hooklessCommand = func(ctx context.Context, bin string, args []string, env []string, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return out, fmt.Errorf("gc %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return out, fmt.Errorf("gc %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// hooklessStartupContext returns the SessionStart context a hook-capable
// provider would inject at its first turn, for a provider that has no hooks:
// `gc prime --hook` (preview form, so nothing is consumed) plus the output of
// `gc hook --inject` (a silent compatibility no-op on this revision, composed
// so a provider keeps receiving it when that command carries content again).
//
// It runs as the session's own identity, never the controller's: the child env
// is the launch env (which already carries GC_CITY_PATH, the store bindings and
// GC_BIN), with the session and hook markers set explicitly so `gc prime`
// proves the same managed session its in-agent hook would. An error is logged
// and yields no context; hook context is additive and must never fail a launch.
func hooklessStartupContext(env map[string]string, info sessionpkg.Info, cityPath, workDir string) string {
	// Only a managed launch names the exact binary it must call: the launch env
	// carries GC_BIN from the gc that resolved it (template_resolve.go). Never
	// fall back to an ambient "gc" — a different installation on PATH would run
	// the hook as another city's binary, and a test fixture with no launch env
	// must not spawn a real one.
	bin := strings.TrimSpace(env["GC_BIN"])
	if bin == "" {
		return ""
	}
	childEnv := hooklessHookEnv(env, info, cityPath)
	ctx, cancel := context.WithTimeout(context.Background(), hooklessContextTimeout)
	defer cancel()

	prime, err := hooklessCommand(ctx, bin, []string{"prime", "--hook", "--json"}, childEnv, workDir)
	if err != nil {
		log.Printf("hookless startup context: session %q: %v", info.ID, err)
		return ""
	}
	injection := hooklessPrimeContent(prime)
	if hook, hookErr := hooklessCommand(ctx, bin, []string{"hook", "--inject"}, childEnv, workDir); hookErr != nil {
		// Same rule as prime: the launch proceeds, the failure is visible.
		log.Printf("hookless startup context: session %q: %v", info.ID, hookErr)
	} else if extra := strings.TrimSpace(string(hook)); extra != "" {
		if injection != "" {
			injection += "\n\n"
		}
		injection += extra
	}
	if len(injection) > hooklessContextLimit {
		log.Printf("hookless startup context: session %q: %d bytes exceeds the %d-byte limit; skipping this launch's context",
			info.ID, len(injection), hooklessContextLimit)
		return ""
	}
	return injection
}

// hooklessPrimeContent decodes the `--json` preview envelope. An unparseable
// payload is a failure, not empty output: silently launching without the
// context would look exactly like a provider that had nothing to deliver.
func hooklessPrimeContent(raw []byte) string {
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		log.Printf("hookless startup context: decoding `gc prime --hook --json`: %v", err)
		return ""
	}
	return strings.TrimSpace(result.Content)
}

// hooklessHookEnv is the launch env plus the markers an in-agent SessionStart
// hook carries. GC_STARTUP_PROMPT_DELIVERED keeps prime to hook-only context:
// the role prompt is already in the launch payload, and prime must not repeat
// it (cmd_prime.go writePrimePromptWithFormat suppresses it in that case).
func hooklessHookEnv(env map[string]string, info sessionpkg.Info, cityPath string) []string {
	overrides := make(map[string]string, len(env)+8)
	for k, v := range env {
		overrides[k] = v
	}
	overrides[managedSessionHookEnv] = "1"
	overrides["GC_HOOK_EVENT_NAME"] = "SessionStart"
	overrides[startupPromptDeliveredEnv] = "1"
	if cityPath != "" {
		overrides["GC_CITY_PATH"] = cityPath
	}
	if info.ID != "" {
		overrides["GC_SESSION_ID"] = info.ID
	}
	if name := strings.TrimSpace(info.SessionName); name != "" {
		overrides["GC_SESSION_NAME"] = name
	}
	if alias := strings.TrimSpace(info.Alias); alias != "" {
		overrides["GC_ALIAS"] = alias
	}
	if agent := strings.TrimSpace(info.AgentName); agent != "" {
		overrides["GC_AGENT"] = agent
	}
	return execenv.MergeMap(execenv.Environ(), overrides)
}

// hooklessStartupContextApplies reports whether a launch needs Core to compose
// the SessionStart context. The provider must both lack an executable hook
// mechanism (SupportsHooks) and have none installed for the agent
// (TemplateParams.HookEnabled): the pi builtin, for example, loads
// gc-hooks.js from its own argv while AgentHasHooks is false unless the agent
// also lists install_agent_hooks, and composing there would inject the same
// mail a second time. A launch with no prompt carrier attaches nothing.
func hooklessStartupContextApplies(tp TemplateParams, cfg *runtime.Config) bool {
	if tp.HookEnabled || tp.ResolvedProvider == nil || tp.ResolvedProvider.SupportsHooks {
		return false
	}
	return cfg.PromptSuffix != "" || cfg.Nudge != ""
}

// applyHooklessStartupContext appends context to the launch payload that
// carries the startup prompt. The argv suffix is re-quoted as one element, the
// same way the one-shot initial_message override is appended
// (session_lifecycle_parallel.go); a provider that delivers its prompt through
// the nudge instead (prompt_mode = "none" or ACP) gets it appended there.
// It reports whether the context found a carrier: a suppressed prompt has
// nothing to attach to, so the context is dropped rather than delivered as its
// own unsolicited turn.
func applyHooklessStartupContext(cfg *runtime.Config, context string) bool {
	context = strings.TrimSpace(context)
	if context == "" {
		return false
	}
	if cfg.PromptSuffix != "" {
		if parts := shellquote.Split(cfg.PromptSuffix); len(parts) == 1 {
			quoted := shellquote.Quote(parts[0] + "\n\n" + context)
			// The promptDelivery guard ran before this point; keep the
			// composed suffix under the same argv bound rather than growing
			// past it after the fact.
			if len(quoted) < maxPromptSuffixQuotedBytes {
				cfg.PromptSuffix = quoted
				return true
			}
			log.Printf("hookless startup context: prompt and context would reach %d argv bytes (limit %d); skipping this launch's context", len(quoted), maxPromptSuffixQuotedBytes)
			return false
		}
	}
	if cfg.Nudge != "" {
		cfg.Nudge += "\n\n" + context
		return true
	}
	return false
}
