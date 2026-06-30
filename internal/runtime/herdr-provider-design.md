# herdr as a gascity runtime provider — feasibility & interface mapping

**Status:** IMPLEMENTED & conformance-passing (branch `feat/herdr-runtime-provider`).

## Implemented (2026-06-29)
`internal/runtime/herdr/`: `client.go` (herdr CLI client), `provider.go` (the full
`runtime.Provider` + `ServerLifecycleProvider`), `capabilities.go` (`IdleWaitProvider` →
native `agent wait`, `ImmediateNudgeProvider`), `provider_live_test.go` +
`conformance_test.go`. Registered as the `"herdr"` runtime in `cmd/gc/runtime_registry.go`;
the full `cmd/gc` binary builds clean (cgo/ICU). **Passes the full `runtimetest` Provider
conformance suite + a live integration test against herdr 0.7.1.** One bug surfaced & fixed
by conformance: some herdr verbs return an empty body on success, so `run()` treats
whitespace-only output as a no-payload success.

**Layout — one space per agent (2026-06-29):** `Start` places each agent in its **own tab** under a
**per-rig (or per-town) workspace** — workspace = the rig (`<rig>--…`) or town (`<town>__…`), tab =
the agent (segment after the last `__`); see `workspaceTabFor`. herdr auto-spawns a stray shell pane
on `workspace`/`tab create`, so `Start` closes it, leaving the tab holding only the agent. Find-or-
create of the shared rig workspace is mutex-serialized so concurrent same-rig Starts don't race to
duplicate it. Teardown needs no special handling: closing the agent's pane drops its tab, and the
last tab's workspace (herdr collapses empties). This replaces the earlier default that tiled every
agent as a **pane** in one shared tab.

**To select it:** set the runtime to `"herdr"` (the same selector that picks tmux/k8s/ssh) —
per-agent, per-rig, or city default. tmux stays the default + fallback. **Pilot safely:** flip
`city.toml` to herdr but pin the **mayor to tmux** first (don't run the orchestrator on an
unproven runtime — a wedge can't self-recover); bake, then drop the override. Omitted optional
capabilities (Relaunch/ProcessTableScanner/InterruptBoundaryWait/Dialog) degrade gracefully.

**Goal:** add **herdr** (<https://herdr.dev>) as an **opt-in** runtime provider alongside
tmux / ssh / k8s, selectable through the existing runtime registry. NOT a tmux replacement —
an additive backend, piloted on low-stakes agents first.

## Validated against herdr 0.7.1 (live smoke test, 2026-06-29)
Probed the installed `herdr 0.7.1` and ran a sandboxed smoke test (isolated named session →
`agent start` a throwaway process → exercise the verbs → cleaned up). Confirmed against reality —
this supersedes the doc-derived guesses in the tables below.

**Works now, over the JSON socket API (clean request/response):**
- `Provider.Start` ← `herdr agent start <name> --cwd PATH --env K=V --no-focus -- <argv>`
  (returns `pane_id`/`workspace_id`/`terminal_id`; **agents are addressable BY NAME** → 1:1 with
  gascity sessions — cleaner than the pane-id path).
- `IsRunning`/`ListRunning` ← `agent list` / `agent get <name>`.
- `ProcessAlive` + the hard-kill PID ← `pane process-info` → `shell_pid` + the full foreground
  process tree (argv/cmdline/cwd/pid). Excellent — covers both.
- `Nudge` ← `agent send <name> <text>` (literal) or `pane run` (text+Enter); `SendKeys` ← `pane send-keys`.
- `Peek` ← `agent read --source visible`. **CORRECTION:** `visible` = current rendered screen
  (what `fingerprint.go` needs); `recent`/`recent-unwrapped` are **scrollback only** (returned empty
  until lines scroll off). Full history = `recent-unwrapped` + `visible`.
- `WaitForIdle` ← `agent wait <name> --status idle --timeout MS` (native).
- `Attach` ← `agent attach <name> [--takeover]`.

**New finding — startup ordering:** a named session's **server must be running before any
subcommand can reach its socket** (`herdr --session <name> <cmd>` → `NotFound` otherwise). So
`Provider.Start` must ensure the session-server is up first. Maps to `ServerLifecycleProvider.
ConfigureServer` = own one shared herdr session-server (≈ the tmux `-L gc` server), one agent/pane
per gascity session. (`herdr server` works headless but prints a "did you mean the TUI?" hint —
find the blessed headless-start invocation.)

**Confirmed gaps (real in 0.7.1):** no signal API (soft interrupt = `send-keys ctrl+c`; hard kill
via process-info PID); no per-session KV (`report-metadata` is display-only + ttl) → sidecar file;
no `ClearScrollback`; `IsAttached`/`GetLastActivity` not directly exposed (infer / best-effort).

**Bonus confirmed:** native `agent_status` + `agent wait` + `report-agent` (a bare bash loop shows
`unknown`; real agents populate via detection manifests) — the liveness-improvement upside is real,
not just docs.

---

> Doc-derived mapping below (pre-validation); the tables are still accurate except where the
> validation section above corrects them (Peek source; the first-class `agent` command).

## Why it's a clean fit
`internal/runtime` already abstracts the multiplexer: a core `Provider` interface
(`runtime.go:119`) + ~11 optional capability interfaces, with backends for **tmux, ssh, k8s,
exec, subprocess, hybrid, fake**. Backend selection is name-based via
`internal/runtime/registry` ("builtin providers register from `cmd/gc`; pack-declared runtimes
register during city load"). So adding herdr is: implement the contract → `registry.Register
("herdr", factory)` → pass the `runtimetest` conformance suite. `internal/runtime/tmux/`
(~290 LOC) is the reference impl. **The "setting" the request wants largely already exists** —
herdr just becomes another selectable runtime name.

## herdr surface (CLI + local socket API)
- Socket: unix `~/.config/herdr/sessions/<name>/herdr.sock`, newline-delimited JSON,
  request/response **plus push event subscriptions**, no auth (local). Injects `HERDR_*` env
  into managed processes (caller env otherwise passes through).
- Primitives: `workspace.create/close`, `pane.split/run/send_text/send_keys/read/close/get/
  list/process_info`, `wait agent-status`, `wait output`, `events.subscribe`
  (`agent_status_changed`, `output_matched`, `pane.exited`, …), `server.stop/reload_config`,
  `pane.report_agent` (native agent state).

## `Provider` method → herdr mapping (24 methods)
| gascity method | herdr | notes |
|---|---|---|
| `Start` | `workspace.create`/`pane.split` (`cwd`, `env`, `--no-focus`) + `pane.run` | headless via `--no-focus`/`--no-session` |
| `Stop` | `workspace.close` / `pane.close` | idempotent |
| `Interrupt` | `pane.send_keys ctrl+c` | ⚠ keystroke, not a real signal (see Gaps) |
| `IsRunning` | `workspace.get`/`pane.get`/`workspace.list` | ✅ |
| `IsAttached` | — | ⚠ no dedicated query (Gaps) |
| `Attach` | `herdr --session <name>` / `terminal attach` | ✅ |
| `ProcessAlive` | `pane.process_info` (PID, fg pgroup, argv) → match `processNames` | ✅ |
| `Nudge` | `pane.run` (text+Enter) / `pane.send_text` | ✅ |
| `SetMeta`/`GetMeta`/`RemoveMeta` | — | ⚠ no general KV (Gaps) |
| `Peek(name, lines)` | `pane.read --source recent-unwrapped --lines N` | ✅ (also `--source detection`) |
| `ListRunning(prefix)` | `workspace.list` + label/name-prefix filter | ✅ orphan detection |
| `GetLastActivity` | — | ⚠ no timestamp (Gaps) |
| `ClearScrollback` | — | ⚠ not in docs; best-effort (Gaps) |
| `CopyTo` | host-side fs copy into pane `foreground_cwd` | ✅ provider-agnostic |
| `SendKeys` | `pane.send_keys` | ✅ exact match (`enter`/`esc`/`ctrl+h`/…) |
| `RunLive` | re-issue `pane.run` for session_live cmds | ✅ |
| `Capabilities` | provider self-reports | ✅ |

## Capability interfaces → herdr
| capability | herdr | |
|---|---|---|
| `IdleWaitProvider.WaitForIdle` | `wait agent-status --status idle` / `events.wait` | ✅✅ **native** |
| `InterruptBoundaryWaitProvider` | `wait output --match "<turn_aborted>"` / `events.wait output_matched` | ✅✅ **native** |
| `ImmediateNudgeProvider.NudgeNow` | `pane.send_input`/`pane.run` (no wait-idle) | ✅ |
| `DialogProvider.DismissKnownDialogs` | `pane.read --source detection` + `pane.send_keys` | ✅ |
| `RelaunchProvider.Relaunch` | `pane.run` new agent cmd in the warm pane | ✅ (respawn-in-place) |
| `ProcessTableScanner` | host `/proc`/`ps` scan for `GC_SESSION_ID` (+ `pane.process_info` for PID map); `TerminateRuntime` = OS kill | ✅ host-side, provider-agnostic |
| `ServerLifecycleProvider` | `server.reload_config` / `server.stop` + per-session socket | ✅ (= the tmux server / `-L gc`) |
| `InteractionProvider` | `pane.read` + `pane.send_keys`, or `agent_status=blocked` + `report_agent` | ◑ partial |
| `InterruptedTurnResetProvider` | `pane.send_keys` (Gemini-specific) | ✅ |
| `TransportCapabilityProvider` | self-report | ✅ |
| `ExecProvider.Exec` | — | ⚠ skip (optional; falls back to dedicated verbs) |

## Gaps (and bridges)
1. **Per-session metadata** (`SetMeta/GetMeta/RemoveMeta`) — herdr's `report_metadata` is
   display-only, not a KV. gascity uses meta for **drain signaling** + **config-fingerprint
   storage**. **Bridge:** a tiny host-side KV keyed by session name (a file under the city
   runtime dir). Low risk, fully owned by the provider.
2. **`IsAttached`** — no dedicated query. **Bridge:** parse `herdr status` / infer from
   `pane.get`; or return false (the contract permits "attach detection unsupported").
3. **`GetLastActivity`** — no timestamp. **Bridge:** subscribe to `output` events and stamp,
   or diff `pane.read`. Best-effort per the contract.
4. **`ClearScrollback`** — not in the API. **Bridge:** best-effort no-op (contract allows), or
   close+respawn the pane on restart.
5. **Signals** — only keystroke `ctrl+c` (no signal API). **Bridge:** soft `Interrupt` =
   `send_keys ctrl+c`; hard kill (`Stop` / `TerminateRuntime`) = OS kill via
   `pane.process_info` PID (gascity already OS-kills by PID — e.g. wedge recovery).
6. **`ExecProvider`** — no capture-stdout+exit one-shot. **Skip it** (optional; the carrier
   falls back to the dedicated wire verbs Peek/SendKeys/Nudge).

## Upsides over tmux (worth the pilot)
1. **Structured JSON socket API + error codes** vs scraping tmux text output (brittle).
2. **Native push events** (`events.subscribe`) — gascity **polls** today for liveness/wedge
   detection. `pane.exited` → instant dead-agent detection; `agent_status_changed` →
   real-time. **Directly relevant to this town's repeated wedged-agent incidents.**
3. **Native agent-state** (`agent_status` working/idle/done/blocked, `--source detection`,
   `wait agent-status`/`wait output`) maps *directly* onto `IdleWaitProvider` /
   `InterruptBoundaryWaitProvider` (tmux implements these by polling/scraping). The
   incident-prone `fingerprint.go` stuck-detection could be augmented by herdr's native model.

## Recommended approach
1. `internal/runtime/herdr/` — implement `Provider` (mirror `tmux/`), driving a thin
   newline-JSON client over the unix socket (+ CLI fallback).
2. `registry.Register("herdr", factory)` in the `cmd/gc` wiring.
3. Expose via the **existing** runtime-selection setting (per-agent / per-rig / city default —
   the same field that picks tmux/ssh/k8s). herdr is opt-in; tmux stays default.
4. Implement the gap bridges (sidecar meta KV; PID-based hard-kill).
5. Pass `runtimetest`/`fake_conformance_test`-style conformance + a herdr integration test.
6. **Pilot** on one low-stakes agent (a dog, or a single rig's witness) behind the setting;
   watch liveness/wedge detection specifically; promote only when it's boring.

## Effort
Comparable to the tmux provider (~300–500 LOC) + the socket client + 4 small gap bridges.
A contained PR; the conformance suite de-risks correctness. The biggest *value* lever is #2/#3
(events + native agent-state) — potentially a real improvement to wedge detection, not just
multiplexer parity.
