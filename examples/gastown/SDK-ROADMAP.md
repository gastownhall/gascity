# Gas City SDK Roadmap

What needs to be built in the `gc` binary to make Gas Town run as pure
configuration. Derived from the FUTURE.md review — every `gt` command was
flattened to its bd primitives, and what's left here is what requires Go code.

## Guiding principle

If a gastown `gt` command is just bd operations, it gets inlined into
prompts/formulas as raw `bd` commands. What remains here are things that
touch sessions, the controller, atomic operations, compound bead workflows,
or config infrastructure that only Go can provide.

---

## Tier 0: Already exists

These gc commands are implemented and working today:

- `gc start` / `gc stop` / `gc init`
- `gc rig add` / `gc rig list`
- `gc agent add/suspend/resume` + `gc session list/attach/peek/kill/logs` + `gc runtime drain/undrain/drain-check/drain-ack`
- `gc mail send/inbox/read`
- `gc formula list/show`
- `gc events` (with `--type` and `--since` filters)
- `gc prime`
- `gc bd` (passthrough to beads CLI)
- `gc version`

---

## Tier 1: Core agent work loop

Required for any agent to do useful work. These are the first things
to implement after the current tutorial work.

### gc peek — session output capture

**Why Go:** Needs session provider abstraction. Different providers
expose output differently (tmux capture-pane, API logs, etc.).

**Interface:** `gc peek <agent-name> [--lines N]`
**Delegates to:** `session/tmux` → `tmux capture-pane`

**Scope:** New agent API on session provider interface. ~50 lines.

### gc context --usage — context window utilization

**Why Go:** Provider-specific. Claude Code may expose via env var,
raw API tracks token count, etc. Only the session provider knows.

**Interface:** `gc context --usage` → returns percentage or token count
**Delegates to:** Session provider

**Scope:** New agent API on session provider interface. ~50 lines.

---

## Tier 2: Event system extension

### gc events --watch — blocking event subscription

**Why Go:** Extends existing `gc events` with Kubernetes Watch pattern.
Blocks until matching event arrives or timeout expires.

**Interface:** `gc events --watch [--type=<filter>] [--timeout=<duration>]`
**Returns:** Matching event(s) or "no events" on timeout
**Design reference:** Kubernetes Watch API (resourceVersion for resume,
server-side timeout guarantees response)

**Key detail:** Timeout ensures agent never sees a hung process. Backoff
logic stays in prompt (ZFC) — prompt increases `--timeout` each iteration.

**Scope:** Extension of existing cmd_events.go. ~150 lines.

---

## Tier 3: Complete mail namespace

Mail is partially implemented. These complete it. Each is thin sugar
over bd, but semantic naming makes prompts dramatically clearer.

| Command | bd equivalent | Scope |
|---------|--------------|-------|
| `gc mail archive <id>` | `bd close <id>` | ~10 lines |
| `gc mail delete <id>` | `bd delete <id>` | ~10 lines |
| `gc mail mark-read <id>` | `bd update <id> --label=read` | ~10 lines |
| `gc mail hook <id>` | `bd update <id> --status=hooked` | ~10 lines |
| `gc mail send --human` | Delivery to tmux prompt vs inbox | ~30 lines |
| `gc mail send --notify` | Nudge after mail creation | ~20 lines |

**Total scope:** ~90 lines, all in existing cmd_mail.go.

---

## ~~Tier 4: Molecule lifecycle~~ — RESOLVED

### ~~gc mol squash~~ — inlined to bd commands

**Resolution:** Squash is just two bd commands: `bd close "$MOL_ID"` +
`bd create --type=digest --title="<summary>"`. Closing the molecule root
detaches it from the agent's hook (closed beads don't appear in queries).
Step children are already closed during execution via `bd close`.
No Go command needed — inlined directly into prompts and formulas.

**gc mol namespace removed:** All molecule operations use `bd mol` directly
(wisp, current, list, show). Step completion is `bd close <step-id>`.
Await-signal/await-event replaced by `gc events --watch` with prompt-level
exponential backoff tracking on agent bead labels.

---

## Tier 5: Rig lifecycle

### gc rig start/stop/park/dock/unpark/undock/restart/status

**Why Go:** Rig state management in the controller. Park/dock change
how the reconciler treats agents in that rig.

| Command | What it does |
|---------|-------------|
| `gc rig start <rig>` | Start all agents for rig |
| `gc rig stop <rig>` | Stop all agents for rig |
| `gc rig park <rig>` | Temporary pause — controller skips rig |
| `gc rig unpark <rig>` | Resume parked rig |
| `gc rig dock <rig>` | Permanent disable — rig removed from reconciliation |
| `gc rig undock <rig>` | Re-enable docked rig |
| `gc rig restart <rig>` | Stop + start |
| `gc rig status <rig>` | Report rig health |

**Scope:** ~200 lines. Rig state stored in `.gc/rigs/<name>/state.json`.

### gc agent suspend — generic agent suspension

**Why Go:** Stop a specific agent without draining. Different from
drain (which waits for work to complete).

**Scope:** ~30 lines.

---

## Tier 6: Config infrastructure

### Prompt template rendering

**Why Go:** Primitive #5 in the architecture. Go `text/template`
rendering of `.md.tmpl` files with variables from city/rig/agent config.

**Variables:** `{{ cmd }}`, `{{ .CityRoot }}`, `{{ .RigName }}`,
`{{ .AgentName }}`, plus custom vars from city.toml.

**Scope:** ~100 lines. New package or function in config.

### Pre-start hooks

**Why Go:** Generic `pre_start` field on `[[agent]]` config. Run a
shell command before agent session starts (e.g., `git pull`).

**Config:**
```toml
[[agent]]
name = "refinery"
pre_start = "git pull --rebase"
```

**Scope:** ~30 lines in cmd_start.go / reconcile.go.

### Custom session naming templates

**Why Go:** Allow configurable session name patterns in city.toml
instead of hardcoded `gc-{city}-{agent}`.

**Config:**
```toml
[session]
name_template = "{prefix}-{name}"
```

**Scope:** ~30 lines in session package.

---

## Tier 7: System health

### gc doctor — system diagnostics

**Why Go:** Check city state consistency: stale agent registrations,
orphaned sessions (tmux sessions without agent config), orphaned
worktrees, event log corruption, etc.

**Interface:** `gc doctor [-v] [--fix]`
- Default: report problems
- `-v`: verbose output
- `--fix`: auto-repair what's safe to fix

**Scope:** ~200 lines. Grows over time as we discover failure modes.

### Crash loop backoff (controller)

**Why Go:** Controller tracks per-instance restart count. Session
uptime < threshold = crash (increment counter), >= threshold = normal
exit (reset). `max_restarts` within `restart_window` → backoff.

**Config:**
```toml
[agent.pool]
max_restarts = 3
restart_window = "5m"
```

**Scope:** ~100 lines in reconcile.go. See `crash-loop-backoff.md`
in memory for full design.

---

## Open design questions

### Convoys

Convoys sit in the same space as epics — batch coordination over
related beads. Which layer do they belong in? Options:
- Bead metadata (labels + parent-child relationships)
- Molecule grouping
- Separate primitive

Needs design discussion before implementation.

### Nudge delivery modes

`--mode=immediate/queue/wait-idle` — re-review whether mail
(which is persistent) obviates the need for delivery modes on
nudge (which is ephemeral). May not be needed.

---

## What does NOT need gc implementation

These were resolved as raw bd commands, controller responsibilities,
or prompt-level logic. They get inlined into prompts/formulas:

| Former gt command | Replacement |
|-------------------|-------------|
| `gc sling` | `bd update <bead> --assignee=<role>` |
| `gc done` | `git push` + `bd create --type=merge-request` + `bd close` + exit |
| `gc handoff` | `gc mail send -s "HANDOFF"` + exit |
| `gc escalate` | `gc mail send witness/ -s "ESCALATION"` |
| `gc polecat list/nuke/status` | `gc session list` (with filters) |
| `gc session status/start/stop` | `gc session list` / controller |
| `gc dog done/status/list` | `bd close` + exit / `gc session list` |
| `gc deacon heartbeat/cleanup/redispatch/zombie-scan` | Controller |
| `gc boot status/spawn/triage` | Controller |
| `gc mayor stop/start` | Controller |
| `gc warrant file` | `bd create --type=warrant` |
| `gc compact` | bd list + bd close/delete (prompt-level) |
| `gc patrol digest` | bd list + bd create (prompt-level) |
| `gc worktree` | Raw `git worktree` commands |
| `gc costs` | Removed — provider-specific |
| `gc mq list/submit/integration` | bd queries + git workflow (gastown-gc helper) |
| `gc convoy feed/cleanup` | Deprecated — pool auto-scaling |
| `gc hook` | `bd ready --label=pool:$POOL --unassigned` + `bd update --claim` (prompt-level loop) |
| Agent bead protocol | `bd update --label` + `bd show` |
| Gates | `bd gate list/close/check` via `gc bd` |
| Orders | Prompt-level (filesystem + state.json) |

---

## Estimated total scope

| Tier | Lines (est.) | Priority |
|------|-------------|----------|
| 1: Core agent loop | ~100 | Immediate |
| 2: Event watch | ~150 | High |
| 3: Mail namespace | ~90 | High |
| ~~4: Mol squash~~ | ~~150~~ | ~~RESOLVED~~ |
| 5: Rig lifecycle | ~230 | Medium |
| 6: Config infrastructure | ~160 | Medium |
| 7: System health | ~300 | Low |
| **Total** | **~1,180** | |

~1,200 lines of Go to make Gas Town run as pure configuration.
