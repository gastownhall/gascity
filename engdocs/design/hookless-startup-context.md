# Hookless startup context (fx)

| Field | Value |
|---|---|
| Status | Implemented |
| Date | 2026-11-11 |
| Author(s) | fx-harness lane |
| Issue | — |
| Supersedes | — |

A provider with `supports_hooks = false` has no executable lifecycle hook, so
the context a SessionStart hook would inject — the step reminder, unread mail
and a durable auto-handoff continuation from `gc prime --hook` — never reaches
its model. This note records the measured gap for fx, the smallest Core seam
that closes it, and what it deliberately does not cover.

## Problem

Core renders an agent's startup prompt in `cmd/gc/template_resolve.go` and
routes it to argv, a flag or a nudge via `cmd/gc/startup_prompt_delivery.go`.
Hook-capable providers add turn-start context themselves: the pi overlay
(`internal/bootstrap/packs/core/overlay/per-provider/pi/.pi/extensions/gc-hooks.js`)
runs `gc prime --hook` on `session_start` and `gc hook --inject`,
`gc nudge drain --inject` and `gc mail check --inject` on `before_agent_start`.

fx has no user-registerable hook surface, and its libfx hooks observe only, so
none of that runs. Its ACP observer stream (`_meta.fx.hooks`) reports turn
events but cannot inject, and ACP carries no compaction event.

Measured on the installed binary (stable `v0.0.16`, revision `481104fdbc24`,
not the dev channel) with a loopback OpenAI-compatible stub, no credentials:

- `fx ask --json --no-save '<prompt>'` runs one turn and prints
  `final_output` and `usage`; the prompt is one argv element.
- `fx '<prompt>'` does **not** start a TUI turn on this build: a positional
  prompt prints fx's help and exits 0. The TUI accepts its first message only
  after startup, so a TUI launch must deliver the prompt as typed input (Core's
  post-ready nudge), not as argv.
- With either shape, the model request contained no prime context: no step
  reminder, no unread mail, no auto-handoff continuation.

`gc hook --inject` is an unconditional silent no-op on this revision
(`cmd/gc/cmd_hook.go`: "silent legacy Stop-hook compatibility"). It is still
composed here so a hookless provider keeps receiving it if that command carries
content again.

## Decision

A launch is hookless only when both facts hold: the resolved provider declares
no executable hook mechanism (`supports_hooks = false`) and no hooks are
installed for the agent (`TemplateParams.HookEnabled`). Both are checked because
the pi builtin loads `gc-hooks.js` from its own argv while `AgentHasHooks` is
false unless the agent also lists `install_agent_hooks`; composing there would
deliver the same mail twice. For a hookless launch, Core composes the
SessionStart hook context into the launch payload it already delivers. The
implementation is `cmd/gc/hookless_prompt.go`, called from
`buildPreparedStartWithTranscript` (`cmd/gc/session_lifecycle_parallel.go`):

1. `gc prime --hook --json` — the preview form. It renders hook-only context
   (`writePrimePromptWithFormat` suppresses the role prompt when
   `GC_STARTUP_PROMPT_DELIVERED=1`) including the step reminder and unread
   mail.
2. `gc hook --inject` — composed after it; empty today, retained for the
   forward-compatible shape.
3. The combined text is appended to whichever field carries the startup prompt:
   the argv suffix (re-quoted as one element, the idiom the one-shot
   `initial_message` override already uses) or the nudge
   (`prompt_mode = "none"` and ACP).

Both child commands run as the session's own identity with the launch env plus
`GC_MANAGED_SESSION_HOOK=1`, `GC_HOOK_EVENT_NAME=SessionStart`,
`GC_STARTUP_PROMPT_DELIVERED=1`, the session id/name/alias/agent and
`GC_CITY_PATH`, never the controller's ambient identity. The launch env must
name `GC_BIN`: hook context is never composed through an ambient `gc` of
unknown provenance.

## Safety rules

- **Non-destructive.** `gc prime --hook --json` is the preview form
  (`doPrimeWithHookFormatOpts(..., consumeHandoff=false)`), so a launch that
  fails after composition cannot archive the only unread auto-handoff mail. The
  accepted cost is at-least-once delivery: the same mail may appear in a later
  incarnation. Measured: after a launch whose request carried the mail, the
  message was still unread (`gc mail inbox fx-ask`).
- **No fingerprint effect.** `runtime.Config.PromptSuffix` and `Nudge` are
  excluded from `CoreFingerprint`, `LaunchFingerprint` and `LiveFingerprint`, and
  the injection is applied after `promptHash` is taken, so volatile mail cannot
  drive a drift restart or re-prime a session (S19).
- **Bounded and observable.** The child commands share a 30 s bound. A failure
  is logged with the command and session and yields no context; hook context is
  additive and never fails a launch. Context over 32 KiB is dropped with a log
  line rather than truncated: the mail stays unread and a later launch can
  deliver it whole.
- **No carrier, no context.** A suppressed startup prompt (deterministic control
  dispatchers, client provider chats) attaches nothing, so no unsolicited turn
  is created.

## Tests and evidence

- `cmd/gc/hookless_prompt_test.go` — command composition, session identity and
  hook markers, failure isolation, the size limit, both carriers, the no-carrier
  case, and the fingerprint-invariance property.
- End-to-end, real tmux runtime and reconciler, fx against the stub model:
  a hookless launch's argv prompt and the captured model request both contain
  the extra prime block; with one unread mail the request carried its
  `<system-reminder>` body while the message stayed unread.

## What this does not cover

- **Turn starts after the first.** Later nudges and mail are Core's own delivery
  paths (`gc session nudge`, the conversation lane), and the agent's prompt
  tells it to claim work. fx's TUI continues a running turn with typed input,
  but Core does not classify that as a steer (`steerFamilies` is empty and no
  runtime reports `SteerProtocol`); this change adds no steering claim.
- **Compaction.** fx has no CLI compaction signal and ACP cannot see it, so
  `gc handoff --auto` on compaction remains uncovered.
- **CLI/API resume launches** (`startupPromptCity.applyTo`,
  `cmd/gc/worker_handle.go`). They carry no hook context in this change; the
  reconciler start path is the gated one.
- **ACP.** Nothing in `internal/runtime/acp` changes, and no ACP protocol
  feature is assumed. A provider that is hookless but ACP-transported gets the
  context in its nudge payload through the same helper.
- **fx upstream.** No fx change is requested or assumed.

## fx provider profiles that this was proven with

```toml
[providers.fx]
base = ""
command = "fx"
# 0.0.16 TUI: a positional prompt prints help; Core types the prompt after
# startup (prompt_mode "arg" only works on builds that accept a first prompt).
prompt_mode = "none"
process_names = ["fx"]
resume_flag = "-r"
supports_hooks = false
ready_delay_ms = 2000

[providers.fx-ask]
base = ""
command = "fx"
args = ["ask", "--json", "--no-save", "--no-color"]
prompt_mode = "arg"
process_names = ["fx"]
supports_hooks = false
```

Select `fx` for a TUI agent; select `fx-ask` and set that agent's
`lifecycle = "one_shot"` for a headless step. Model and permission defaults come
from the settings file of the environment fx runs in (`~/.fx/settings.json`);
do not put `--model` or permission flags before the TUI's positional prompt on
this build. `--no-save` gives up resume for the headless variant, and `-r` needs
a session id discovered after a turn (`fx sessions --json`).
