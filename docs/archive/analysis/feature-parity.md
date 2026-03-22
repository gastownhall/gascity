---
title: "Feature Parity"
---

Created 2026-02-27 from exhaustive exploration of gastown `upstream/main`
(92 top-level commands, 425+ subcommands, 180 command files) compared
against gascity `main` (23 top-level commands, ~60 implementation files).

**Goal:** 100% feature parity. Every gastown feature gets a Gas City
equivalent — either via a direct port, a configuration-driven
alternative, or a deliberate architectural decision to handle it
differently.

**Key constraint:** Gas City has ZERO hardcoded roles. Every gastown
command that references mayor/deacon/witness/refinery/polecat/crew must
become role-agnostic infrastructure that any pack can use.

---

## Status Legend

| Status | Meaning |
|--------|---------|
| **DONE** | Fully implemented in Gas City |
| **PARTIAL** | Core exists, missing subcommands or capabilities |
| **TODO** | Not yet implemented, needed for parity |
| **REMAP** | Gastown-specific; handled differently in Gas City by design |
| **VERIFY** | Implementation exists but correctness needs verification |
| **N/A** | Deployment/polish concern, not SDK scope |

---

## 1. City/Town Lifecycle

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt install [path]` | `gc init [path]` | **DONE** | Auto-init on `gc start` too |
| `gt start [path]` | `gc start [path]` | **DONE** | One-shot + controller modes |
| `gt down` / `gt stop` | `gc stop [path]` | **DONE** | Graceful shutdown + orphan cleanup |
| `gt up` | `gc start` | **DONE** | gt up is idempotent boot; gc start one-shot reconcile is equivalent |
| `gt shutdown` | `gc stop` + `gc worktree clean` | **N/A** | WONTFIX: `gc stop` + `gc worktree clean --all` covers it. Graceful handoff wait and uncommitted work protection are domain-layer concerns for the pack config. |
| `gt restart` | `gc restart [path]` | **DONE** | Stop then start |
| `gt status` | `gc status [path]` | **DONE** | City-wide overview: controller, suspended state, all agents/pools, rigs, summary count. |
| `gt enable` / `gt disable` | `gc suspend` / `gc resume` | **DONE** | City-level suspend: hook injection becomes no-op. Also supports `GC_SUSPENDED=1` env override. |
| `gt version` | `gc version` | **DONE** | |
| `gt info` | — | **N/A** | Whats-new splash; polish |
| `gt stale` | — | **N/A** | Binary staleness check; polish |
| `gt uninstall` | — | **N/A** | Deployment concern |
| `gt git-init` | — | **REMAP** | `gc init` handles city setup; git init is user's job |
| `gt thanks` | — | **N/A** | Credits page; polish |

---

## 2. Supervisor / Controller

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt daemon run` | `gc supervisor run` | **DONE** | Canonical foreground control loop |
| `gt daemon start` | `gc supervisor start` | **DONE** | Background supervisor |
| `gt daemon stop` | `gc supervisor stop` | **DONE** | Socket shutdown |
| `gt daemon status` | `gc supervisor status` | **DONE** | PID + uptime |
| `gt daemon logs` | `gc supervisor logs` | **DONE** | Tail supervisor log file |
| `gt daemon enable-supervisor` | `gc supervisor install` / `uninstall` | **DONE** | launchd + systemd |
| Controller flock | Controller flock | **DONE** | `acquireControllerLock` |
| Controller socket IPC | Controller socket IPC | **DONE** | Unix socket + "stop" command |
| Reconciliation loop | Reconciliation loop | **DONE** | Tick-based with fsnotify |
| Config hot-reload | Config hot-reload | **DONE** | Debounced, validates before apply |
| Crash tracking + backoff | Crash tracking + backoff | **DONE** | `crashTracker` with window |
| Idle timeout enforcement | Idle timeout enforcement | **DONE** | `idleTracker` per agent |
| Graceful shutdown dance | Graceful shutdown | **DONE** | Interrupt → wait → kill |
| PID file write/cleanup | PID file write/cleanup | **DONE** | In `runController` |
| Dolt health check ticker | Dolt `EnsureRunning` + order `dolt-health` | **DONE** | `EnsureRunning` via gc-beads-bd script + cooldown order (30s) for periodic health check and restart. |
| Dolt remotes patrol | Order recipe: `dolt-remotes-patrol` | **DONE** | Cooldown order (15m) runs `gc dolt sync`. Lives in `examples/gastown/formulas/orders/dolt-remotes-patrol/`. |
| Feed curator | — | **REMAP** | Gastown tails events.jsonl, deduplicates, aggregates, writes curated feed.jsonl. Gas City's tick-based reconciler covers recovery; curated feed is UX polish. |
| Convoy manager (event polling) | bd on_close hook → `gc convoy autoclose` | **DONE** | Reactive: bd on_close hook triggers `gc convoy autoclose <bead-id>` which checks parent convoy completion. Replaced polling order `convoy-check`. |
| Workspace sync pre-restart | `syncWorktree()` | **DONE** | `git fetch` + `git pull --rebase` + auto-stash/restore in worktree.go. Wired into `gc start` and pool respawn. Guarded by `pre_sync` config flag. |
| KRC pruner | — | **N/A** | No KRC in Gas City |

---

## 3. Agent Management

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt agents list` | `gc session list` | **DONE** | Lists agents with pool/suspend annotations |
| `gt agents menu` | `scripts/agent-menu.sh` | **DONE** | Shell script via session_setup; `prefix-g` keybinding |
| `gt agents check` | `gc doctor` | **DONE** | Agent health in doctor checks |
| `gt agents fix` | `gc doctor --fix` | **DONE** | |
| Agent start (spawn) | Reconciler auto-starts | **REMAP** | No `gc agent start`; reconciler spawns agents on tick. `gc session attach` idempotently starts+attaches. |
| Agent stop (graceful) | `gc runtime drain` / `gc agent suspend` | **REMAP** | No `gc agent stop`; drain stops gracefully with timeout, suspend prevents restart. |
| Agent kill | `gc session kill <name>` | **DONE** | Force-kill; reconciler restarts on next tick |
| Agent attach | `gc session attach <name>` | **DONE** | Interactive terminal; starts session if not running |
| Agent status | `gc session list` | **DONE** | Shows session, running, suspended, draining state |
| Agent peek | `gc session peek [name]` | **DONE** | Scrollback capture with --lines |
| Agent drain | `gc runtime drain <name>` | **DONE** | Pool drain with timeout + drain-ack + drain-check + undrain |
| Agent suspend | `gc agent suspend <name>` | **DONE** | Prevent reconciler spawn (sets `suspended=true` in city.toml) |
| Agent resume | `gc agent resume <name>` | **DONE** | Re-enable spawning (clears `suspended`) |
| Agent nudge | `gc session nudge <name> <msg>` | **DONE** | Send input to running session via tmux send-keys |
| Agent add (runtime) | `gc agent add --name <name>` | **DONE** | Add agent to city.toml (supports --prompt-template, --dir, --suspended) |
| Agent request-restart | `gc runtime request-restart` | **DONE** | Signal agent to restart on next hook check |
| Session cycling (`gt cycle`) | `session_setup` + scripts | **DONE** | Inlined as shell scripts in `examples/gastown/scripts/cycle.sh`, wired via `session_setup` bind-key with if-shell fallback preservation |
| Session restart with handoff | `gc handoff` + reconciler | **DONE** | Core handoff implemented: mail-to-self + restart-requested + reconciler restart + scrollback clearing. `--collect` is WONTFIX (fails ZFC: agent writes better handoff notes than a canned state dump). |
| `gt seance` | — | **P3** | Predecessor session forking: decomposes into events + provider `--fork-session --resume`. Real in gastown but not SDK-critical. |
| `gt cleanup` | `gc doctor --fix` | **DONE** | Zombie/orphan cleanup |
| `gt shell install/remove` | — | **N/A** | Shell integration; deployment |

---

## 4. Pool / Polecat Management

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| Pool min/max scaling | Pool min/max/check | **DONE** | Elastic pool with check command |
| Pool drain with timeout | Pool drain with timeout | **DONE** | `drainOps` in reconciler |
| Polecat spawn (worktree) | Worktree isolation | **DONE** | `isolation = "worktree"` |
| Polecat name pool | — | **REMAP** | Gas City uses `{name}-{N}` numeric; names are config |
| `gt polecat list` | `gc session list` | **DONE** | Pool instances shown with annotations |
| `gt polecat add/remove` | Config-driven | **REMAP** | Edit city.toml pool.max |
| `gt polecat status` | `gc session list` | **DONE** | Per-instance |
| `gt polecat nuke` | `gc session kill` + `gc worktree clean` | **DONE** | Kill + worktree cleanup |
| `gt polecat gc` | `gc doctor --fix` | **DONE** | Stale worktree cleanup |
| `gt polecat stale/prune` | Reconciler | **DONE** | Orphan detection in reconciler |
| `gt polecat identity` | — | **REMAP** | No identity system; agents are config |
| `gt namepool add/reset/set/themes` | — | **REMAP** | No name pool; numeric naming |
| `gt prune-branches` | `gc worktree clean` | **DONE** | Worktree cleanup; stale branch pruning built into removeAgentWorktree |
| Polecat git-state check | `gc worktree clean` / `gc worktree list` | **DONE** | Three safety checks: `HasUncommittedWork`, `HasUnpushedCommits`, `HasStashes`. Blocks removal unless `--force`. List shows combined status. |
| Dolt branch isolation | — | **TODO** | Per-agent dolt branch for write isolation |

---

## 5. Crew Management

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt crew add/remove` | Config-driven | **REMAP** | Add `[[agent]]` to city.toml |
| `gt crew list` | `gc session list` | **DONE** | |
| `gt crew start/stop` | Reconciler / `gc agent suspend+resume` | **REMAP** | No individual start/stop; reconciler auto-starts, suspend prevents restart |
| `gt crew restart` | `gc session kill` (reconciler restarts) | **DONE** | |
| `gt crew status` | `gc session list` | **DONE** | |
| `gt crew at <name>` | `gc session attach <name>` | **DONE** | |
| `gt crew refresh` | `gc handoff --target` | **DONE** | Remote handoff: sends mail + kills session; reconciler restarts |
| `gt crew pristine` | — | **REMAP** | Just git: `git -C <workdir> pull` per agent; witness/deacon prompt can do this |
| `gt crew next/prev` | — | **TODO** | Cycle between crew sessions |
| `gt crew rename` | Config-driven | **REMAP** | Edit city.toml |

---

## 6. Work Management (Beads)

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt show <bead-id>` | `bd show <id>` | **REMAP** | Delegates to bd CLI directly |
| `gt cat <bead-id>` | `bd show <id>` | **REMAP** | Same |
| `gt close [bead-id...]` | `bd close <id>` | **REMAP** | Delegates to bd |
| `gt done` | — | **REMAP** | Inlined to prompt: `git push` + `bd create --type=merge-request` + `bd close` + exit. No SDK command needed. |
| `gt release <issue-id>` | — | **REMAP** | Just bd: `bd update <id> --status=open --assignee=""`. No SDK command needed. |
| `gt ready` | `gc hook` (work_query) | **DONE** | Shows available work |
| Bead CRUD | Bead CRUD | **DONE** | FileStore + BdStore + MemStore |
| Bead dependencies | Bead dependencies | **DONE** | Needs field + Ready() query |
| Bead labels | Bead labels | **DONE** | Labels field |
| Bead types (custom) | — | **TODO** | Register custom bd types (message, agent, molecule, etc.) |
| Agent beads (registration) | — | **REMAP** | Just bd: `bd create --type=agent` + `bd update --label`. No SDK command needed. |
| Agent state tracking | — | **REMAP** | Just bd labels: `idle:N`, `backoff-until:TIMESTAMP`. Liveness = bead last-updated. |
| Bead slots (hook column) | — | **N/A** | WONTFIX: Gas City doesn't use hooked beads. Users can implement via bd labels if needed. |
| Merge request beads | — | **REMAP** | Just bd metadata: `bd update --set-metadata branch=X target=Y`. No structured fields needed — gastown formulas already use this pattern. `BdStore.SetMetadata` supports it. |
| Cross-rig bead routing | Routes file | **DONE** | `routes.jsonl` for multi-rig |
| Beads redirect | Beads redirect | **DONE** | `setupBeadsRedirect` for worktrees |
| `gt audit` | `gc events --type` | **PARTIAL** | Events cover audit; no per-actor query |
| `gt migrate-bead-labels` | — | **N/A** | Migration tool; one-time |

---

## 7. Hook & Dispatch

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt hook` (show/attach/detach/clear) | `gc hook` | **DONE** | Has work_query. Attach/detach/clear are N/A — Gas City doesn't use hooked beads; users can implement via bd if needed. |
| `gt sling <bead> [target]` | `gc sling <target> <bead>` | **DONE** | Routes + nudges |
| `gt unsling` / `gt unhook` | — | **N/A** | WONTFIX: Gas City doesn't use hooked beads. Users can `bd update --hook=""` if needed. |
| Sling to self | `gc sling $GC_AGENT <bead>` | **DONE** | Shell expands `$GC_AGENT`; no special code needed |
| Sling batch (multiple beads) | `doSlingBatch` container expansion | **DONE** | Convoy/epic auto-expand open children; per-child warnings, partial failure, single nudge |
| Sling with formula instantiation | `gc sling --formula` | **DONE** | Creates wisp molecule |
| Sling idempotency | `checkBeadState` pre-flight | **PARTIAL** | Warns on already-assigned/labeled beads; `--force` suppresses. Warns rather than skips. |
| Sling --args (natural language) | — | **TODO** | Store instructions on bead, show via gc prime |
| Sling --merge strategy | `gc sling --merge` | **DONE** | `--merge direct\|mr\|local` stores `merge_strategy` metadata on bead |
| Sling --stdin | `gc sling --stdin` | **DONE** | `--stdin` reads text from stdin (first line = title, rest = description); mutually exclusive with `--formula` |
| Sling --max-concurrent | — | **N/A** | WONTFIX: pool min/max config controls concurrency; agents pull work via `bd ready` so overload is self-limiting. |
| Sling auto-convoy | `gc sling` (default) | **DONE** | Auto-creates convoy on sling; `--no-convoy` to suppress, `--owned` to mark owned |
| Sling --account | — | **TODO** | Per-sling account override for quota rotation. Resolves handle → `CLAUDE_CONFIG_DIR` for spawned agent. Requires `gc account` + `gc quota` command groups. |
| Sling --agent override | — | **N/A** | WONTFIX: Use separate pools with different providers. Priority sorting (`bd ready --sort priority`) handles work routing. Adding pools is already supported via config + `gc agent add`. |
| `gt handoff` | `gc handoff` | **DONE** | Mail-to-self + restart-requested + block |
| `gt broadcast` | — | **DEFER** | Nudge all agents; operator convenience, no programmatic callers. Implement when needed. |
| `gt nudge <target> [msg]` | `gc session nudge <name> <msg>` | **DONE** | Direct message injection via tmux send-keys |

---

## 8. Mail / Messaging

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt mail send` | `gc mail send` | **DONE** | Creates message bead |
| `gt mail inbox` | `gc mail inbox` | **DONE** | Lists unread |
| `gt mail read` | `gc mail read` | **DONE** | Read + close |
| `gt mail peek` | `gc mail check` | **DONE** | Obviated by `gc mail check --inject` |
| `gt mail delete` | — | **DEFER** | Add when needed |
| `gt mail archive` | `gc mail archive` | **DONE** | |
| `gt mail mark-read/mark-unread` | — | **DEFER** | Add when needed |
| `gt mail check` | `gc mail check` | **DONE** | Count unread |
| `gt mail search` | — | **DEFER** | Add when needed |
| `gt mail thread` / `gt mail reply` | — | **DEFER** | Add when needed |
| `gt mail claim/release` | — | **DEFER** | Add when needed |
| `gt mail clear` | — | **DEFER** | Add when needed |
| `gt mail hook` | `gc mail check --inject` | **DONE** | Hook injection via `--inject` flag |
| `gt mail announces` | — | **REMAP** | No channels; direct addressing sufficient |
| `gt mail channel` | — | **REMAP** | Pub/sub channels; domain pattern |
| `gt mail queue` | — | **REMAP** | Claim queues; domain pattern |
| `gt mail group` | — | **REMAP** | Mailing lists; domain pattern |
| `gt mail directory` | — | **N/A** | Directory listing; UX polish |
| `gt mail identity` | — | **REMAP** | Identity is `$GC_AGENT` |
| Mail priority (urgent/high/normal/low) | — | **DEFER** | Add when needed |
| Mail type (task/scavenge/notification/reply) | — | **DEFER** | Add when needed |
| Mail delivery modes (queue/interrupt) | — | **DEFER** | Add when needed |
| Mail threading (thread-id, reply-to) | — | **DEFER** | Add when needed |
| Two-phase delivery (pending → acked) | — | **DEFER** | Add when needed |
| Mail CC | — | **DEFER** | Add when needed |
| Address resolution (@town, @rig, groups) | — | **DEFER** | Add when needed |

---

## 9. Formulas & Molecules

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| Formula TOML parsing | Formula TOML parsing | **DONE** | `internal/formula` |
| `gc formula list` | `gc formula list` | **DONE** | |
| `gc formula show` | `gc formula show` | **DONE** | |
| `gc formula validate` | — | **REMAP** | Just bd: `bd formula show` validates on parse; `bd cook --dry-run` for full check |
| `gt formula create` | — | **REMAP** | User writes `.formula.toml` file; no scaffolding command needed |
| `gt formula run` | — | **REMAP** | Just bd: `bd mol pour <formula>` + `gc sling`; convoy execution is `gc sling --formula` |
| Formula types: workflow | workflow | **DONE** | Sequential steps with dependencies |
| Formula types: convoy | — | **REMAP** | bd owns formula types; `bd cook` + `bd mol pour/wisp` handle all types |
| Formula types: expansion | — | **REMAP** | bd owns formula types; `bd cook` handles expansion |
| Formula types: aspect | — | **REMAP** | bd owns formula types; `bd cook` handles aspects |
| Formula variables (--var) | `gc sling --formula --var` | **DONE** | Passes `--var key=value` through to `bd mol cook` |
| Three-tier resolution (project → city → system) | Five-tier (system + city topo + city local + rig topo + rig local) | **DONE** | System formulas via `go:embed` Layer 0; higher layers shadow by filename |
| Periodic formula dispatch | `gc order list/show/run/check` | **REMAP** | Replaced by file-based order system. Orders live in `orders/<name>/order.toml` with gate evaluation (cooldown, cron, condition, manual). `gc order check` evaluates gates. |
| `gt mol status` | — | **REMAP** | Just bd: `bd mol current --for=$GC_AGENT` |
| `gt mol current` | — | **REMAP** | Just bd: `bd mol current` shows steps with "YOU ARE HERE" |
| `gt mol progress` | — | **REMAP** | Just bd: `bd mol current` shows step status indicators |
| `gt mol attach/detach` | — | **REMAP** | Just bd: `bd update $WISP --assignee=$GC_AGENT` / `--assignee=""` |
| `gt mol step done` | — | **REMAP** | Just bd: `bd close <step-id>` auto-advances |
| `gt mol squash` | — | **REMAP** | Just bd: `bd close $MOL_ID` + `bd create --type=digest` |
| `gt mol burn` | — | **REMAP** | Just bd: `bd mol burn <wisp-id> --force` |
| `gt mol attach-from-mail` | — | **REMAP** | Prompt-level: read mail, pour wisp, assign |
| `gt mol await-signal/event` | — | **REMAP** | Just gc: `gc events --watch --type=... --timeout` |
| `gt mol emit-event` | — | **REMAP** | Just gc: `gc event emit ...` |
| Wisp molecules (ephemeral) | Wisp molecules | **DONE** | Ephemeral bead flag |
| `gt compact` | `mol-wisp-compact` order | **DONE** | Deacon order formula; raw bd commands (list/delete/update --persistent) |

---

## 10. Convoy (Batch Work)

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt convoy create` | `gc convoy create` | **DONE** | Create batch tracking bead |
| `gt convoy add` | `gc convoy add` | **DONE** | Add issues to convoy |
| `gt convoy close` | `gc convoy close` | **DONE** | Close convoy |
| `gt convoy status` | `gc convoy status` | **DONE** | Show progress |
| `gt convoy list` | `gc convoy list` | **DONE** | Dashboard view |
| `gt convoy check` | `gc convoy check` | **DONE** | Auto-close completed convoys |
| `gt convoy land` | — | **TODO** | Land completed convoy (cleanup) |
| `gt convoy launch` | — | **TODO** | Dispatch convoy work |
| `gt convoy stage` | — | **TODO** | Stage convoy for validation |
| `gt convoy stranded` | `gc convoy stranded` | **DONE** | Find convoys with stuck work |
| Auto-close on completion | `gc convoy check` + bd on_close hook | **DONE** | `gc convoy check` (batch scan) + `gc convoy autoclose` (reactive via bd on_close hook) |
| Close-triggers-convoy-check | bd on_close hook → `gc convoy autoclose` | **DONE** | bd on_close hook triggers `gc convoy autoclose <bead-id>` which checks parent convoy. Recursive-safe, idempotent. |
| Reactive feeding | — | **N/A** | WONTFIX: `bd ready` + pool auto-scaling handle work discovery; agents poll their own queues. Reactive push is unnecessary with pull-based GUPP. |
| Blocking dependency check | Bead dependencies | **PARTIAL** | Ready() exists; convoy-specific filtering missing |

---

## 11. Merge Queue

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt mq submit` | — | **REMAP** | Just bd: polecat sets `metadata.branch`/`metadata.target` + assigns to refinery |
| `gt mq list` | — | **REMAP** | Just bd: `bd list --assignee=refinery --status=open` |
| `gt mq status` | — | **REMAP** | Just bd: `bd show $WORK --json \| jq '.metadata'` |
| `gt mq retry` | — | **REMAP** | Just bd: refinery rejects back to pool, new polecat picks up |
| `gt mq reject` | — | **REMAP** | Just bd: `bd update --status=open --assignee="" --set-metadata rejection_reason=...` |
| `gt mq next` | — | **REMAP** | Just bd: `bd list --assignee=$GC_AGENT --limit=1` |
| `gt mq integration` | — | **REMAP** | Git workflow + bead metadata; gastown-gc helper territory |
| MR scoring (priority + age + retry) | — | **REMAP** | bd query ordering; prompt-level concern |
| Conflict detection + retry | — | **REMAP** | Pure git in refinery formula; prompt-level |
| MR bead fields (branch, target, etc.) | — | **REMAP** | Just bd metadata: `--set-metadata branch=X target=Y` |

---

## 12. Rig Management

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt rig add` | `gc rig add` | **DONE** | |
| `gt rig list` | `gc rig list` | **DONE** | |
| `gt rig remove` | — | **N/A** | WONTFIX: edit city.toml + `gc start`; `gc doctor` can detect orphaned state |
| `gt rig status` | `gc rig status` (via gc status) | **PARTIAL** | Per-rig agent status not separated |
| `gt rig start/stop` | `gc rig suspend/resume` | **DONE** | Different naming, same effect |
| `gt rig restart` | `gc rig restart` | **DONE** | Kill agents, reconciler restarts |
| `gt rig park/unpark` | `gc rig suspend/resume` | **DONE** | |
| `gt rig dock/undock` | — | **REMAP** | Same as suspend/resume |
| `gt rig boot` | `gc start` (auto-boots rigs) | **DONE** | |
| `gt rig shutdown` | `gc stop` | **DONE** | |
| `gt rig config show/set/unset` | — | **N/A** | WONTFIX: edit city.toml directly |
| `gt rig settings show/set/unset` | — | **N/A** | WONTFIX: edit city.toml directly |
| `gt rig detect` | — | **N/A** | WONTFIX: `gc rig add <path>` is sufficient |
| `gt rig quick-add` | — | **N/A** | WONTFIX: `gc rig add <path>` is sufficient |
| `gt rig reset` | — | **TODO** | Reset rig to clean state |
| Per-rig agents (witness/refinery) | Rig-scoped agents (`dir = "rig"`) | **DONE** | |
| Rig beads prefix | `rig.prefix` / `EffectivePrefix()` | **DONE** | |
| Fork support (push_url) | — | **WONTFIX** | User runs `git remote set-url --push origin <fork>` in rig dir; worktrees inherit it. No SDK involvement needed. |

---

## 13. Health Monitoring

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt doctor` | `gc doctor` | **DONE** | Comprehensive health checks |
| `gt doctor --fix` | `gc doctor --fix` | **DONE** | Auto-repair |
| Witness patrol (rig-level) | Reconciler tick | **DONE** | Different mechanism, same outcome |
| Deacon patrol (town-level) | Controller loop | **DONE** | Same |
| Stall detection (30min threshold) | Idle timeout | **DONE** | Configurable per agent |
| GUPP violation detection | — | **N/A** | WONTFIX: idle timeout + prompt self-assessment cover this; depends on hooked beads |
| Orphaned work detection | Orphan session cleanup | **DONE** | Reconciler phase 2 |
| Zombie detection (tmux alive, process dead) | Doctor zombie check | **DONE** | |
| `gt deacon` (18 subcommands) | — | **REMAP** | Role-specific; controller handles patrol |
| `gt witness` (5 subcommands) | — | **REMAP** | Role-specific; per-agent health in config |
| `gt boot` (deacon watchdog) | — | **REMAP** | Controller IS the watchdog |
| `gt escalate` | — | **N/A** | WONTFIX: idle timeout + health patrol already cover this; escalation is a prompt-level concern |
| `gt warrant` (death warrants) | — | **REMAP** | Controller handles force-kill decisions |
| Health heartbeat protocol | — | **TODO** | Agent liveness pings with configurable interval |
| `gt patrol` | — | **REMAP** | Patrol is the controller reconcile loop |
| `gt orphans` | Doctor orphan check | **DONE** | |

---

## 14. Hooks (Provider Integration)

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| Hook installation (Claude) | Hook installation (Claude) | **DONE** | `.gc/settings.json` — includes `skipDangerousModePermissionPrompt`, `editorMode`, PATH export |
| Hook installation (Codex) | Hook installation (Codex) | **DONE** | `.codex/hooks.json` — SessionStart primes via `gc prime --hook`; Stop checks queued work via `gc hook --inject` |
| Hook installation (Gemini) | Hook installation (Gemini) | **DONE** | `.gemini/settings.json` — event names (`SessionStart`, `PreCompress`, `BeforeAgent`, `SessionEnd`) verified correct against Gemini CLI docs and gastown upstream. |
| Hook installation (OpenCode) | Hook installation (OpenCode) | **DONE** | `.opencode/plugins/gascity.js` |
| Hook installation (Copilot) | Hook installation (Copilot) | **DONE** | `.github/hooks/gascity.json` with `.github/copilot-instructions.md` as a companion fallback |
| Hook installation (Pi) | Hook installation (Pi) | **DONE** | `.pi/extensions/gc-hooks.js` |
| Hook installation (OMP) | Hook installation (OMP) | **DONE** | `.omp/hooks/gc-hook.ts` |
| Provider `SupportsHooks` flag | `ProviderSpec.SupportsHooks` | **DONE** | Per-provider hook metadata; cross-checked against installer support. `AgentHasHooks` still requires Claude, explicit `install_agent_hooks`, or `hooks_installed`. |
| Provider `InstructionsFile` | `ProviderSpec.InstructionsFile` | **DONE** | Per-provider instructions file (e.g., `CLAUDE.md`, `AGENTS.md`) |
| `gt hooks sync` | — | **TODO** | Regenerate all settings files from config |
| `gt hooks diff` | — | **TODO** | Preview what sync would change |
| `gt hooks base` | — | **TODO** | Edit shared base hook config |
| `gt hooks override <target>` | — | **TODO** | Per-role hook overrides |
| `gt hooks list` | — | **TODO** | Show all managed settings |
| `gt hooks scan` | — | **TODO** | Discover hooks in workspace |
| `gt hooks init` | — | **TODO** | Bootstrap from existing settings |
| `gt hooks registry` | — | **TODO** | Hook marketplace/registry |
| `gt hooks install <id>` | — | **TODO** | Install hook from registry |
| Base + override merge strategy | — | **TODO** | Per-matcher merge semantics |
| 6 hook event types | 4 of 6 implemented | **PARTIAL** | Claude: SessionStart, PreCompact, UserPromptSubmit, Stop all installed. Missing: PreToolUse, PostToolUse. Adding these would enable tool-level guards (e.g., block `rm -rf /`). |
| Roundtrip-safe settings editing | — | **TODO** | Preserve unknown fields when editing settings.json |

---

## 15. Orders

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt order list` | `gc order list` | **DONE** | Lists all orders with gate type, timing, pool |
| `gt order show` | `gc order show` | **DONE** | Shows order details incl. source file, description, gate config |
| `gt order run` | `gc order run` | **DONE** | Executes order manually: instantiates wisp, slings to target pool |
| `gt order check` | `gc order check` | **DONE** | Evaluates gates for all orders, shows due/not-due table |
| `gt order history` | `gc order history` | **DONE** | Show order execution history; queries order-run: labels |
| Order gate types | `internal/orders` | **DONE** | 5 of 5 types: cooldown, cron, condition, manual, event. |
| Order TOML format | `orders/<name>/order.toml` | **DONE** | `[order]` header with gate, formula, interval, schedule, check, pool, enabled fields |
| Order tracking (labels, digest) | `order-run:` labels | **DONE** | Execution recording via bead labels, last-run tracking for gate evaluation |
| Order execution timeout | — | **TODO** | Timeout enforcement |
| Multi-layer order resolution | 4-layer formula resolution | **DONE** | Orders inherit formula resolution: rig formulas dir → city formulas dir → embedded |

---

## 16. Events & Activity

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt log` | `gc events` | **DONE** | JSONL event log |
| `gt log crash` | `gc events --type=agent.crashed` | **DONE** | |
| `gt feed` | — | **N/A** | WONTFIX: `gc events --since/--type/--watch` + OTEL covers this; TUI curator is UX polish |
| `gt activity emit` | `gc event emit` | **DONE** | |
| `gt trail` (recent/recap) | `gc events --since` | **DONE** | |
| `gt trail commits` | — | **N/A** | WONTFIX: `git log --since` is a trivial shell wrapper, not SDK infrastructure |
| `gt trail beads` | — | **N/A** | WONTFIX: `bd list --since` is a trivial shell wrapper |
| `gt trail hooks` | — | **N/A** | WONTFIX: `gc events --type=hook --since` covers this |
| Event visibility tiers (audit/feed/both) | — | **N/A** | WONTFIX: `gc events --type` filtering is sufficient |
| Structured event payloads | `--payload` JSON | **PARTIAL** | Free-form; no typed builders |
| `gc events --watch` | `gc events --watch` | **DONE** | Block until events arrive |
| `gc events --payload-match` | `gc events --payload-match` | **DONE** | Filter by payload fields |

---

## 17. Config Management

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| Config load (TOML) | Config load (TOML) | **DONE** | city.toml with progressive activation |
| Config composition (includes) | Config composition | **DONE** | Fragment includes + layering |
| Config patches | Config patches | **DONE** | Per-agent overrides |
| Config validation | Config validation | **DONE** | Agent, rig, provider validation |
| Config hot-reload | Config hot-reload | **DONE** | fsnotify + debounce |
| `gt config set/get` | `gc config show` | **PARTIAL** | Show only; no set/get |
| `gt config cost-tier` | — | **REMAP** | Provider per agent is config |
| `gt config default-agent` | — | **REMAP** | `workspace.provider` |
| `gt config agent-email-domain` | — | **REMAP** | Agent env config |
| Remote pack fetch | Remote pack fetch | **DONE** | `gc pack fetch/list` |
| Pack lock file | Pack lock file | **DONE** | `.gc/pack.lock` |
| Config provenance tracking | Config provenance | **DONE** | Which file, which line |
| Config revision hash | Config revision hash | **DONE** | For change detection |
| Config --strict mode | Config --strict mode | **DONE** | Promote warnings to errors |

---

## 18. Prompt Templates

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| Role templates (7 roles) | Prompt templates | **DONE** | Any agent, any template file |
| Message templates (spawn/nudge/escalation/handoff) | — | **TODO** | Template rendering for messages |
| Template functions ({{ cmd }}) | Template functions | **DONE** | {{ cmd }}, {{ session }}, {{ basename }}, etc. |
| Shared template composition | Shared templates | **DONE** | `prompts/shared/` directory |
| Template variables (role data) | Template variables | **DONE** | CityRoot, AgentName, RigName, WorkDir, Branch, DefaultBranch, IssuePrefix, WorkQuery, SlingQuery, TemplateName + custom Env |
| `gt prime` | `gc prime` | **DONE** | Output agent prompt |
| `gt role show/list/def/env/home/detect` | — | **REMAP** | Roles are config; `gc prime` + `gc config show` |
| Commands provisioning (`.claude/commands/`) | `overlay_dir` config | **DONE** | Generic `overlay_dir` copies any directory tree into agent workdir at startup |
| CLAUDE.md generation | — | **TODO** | Generate agent-specific CLAUDE.md files |

---

## 19. Worktree Isolation

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| Worktree creation (per agent) | Worktree creation | **DONE** | `isolation = "worktree"` |
| Worktree branch naming | Worktree branch naming | **DONE** | `gc-{rig}-{agent}` |
| Worktree cleanup (nuke) | `gc worktree clean --all` | **DONE** | |
| Worktree submodule init | `createAgentWorktree` | **DONE** | Layer 0 side effect: `git submodule update --init --recursive` after worktree add |
| `gt worktree list` | `gc worktree list` | **DONE** | List all worktrees across rigs |
| `gt worktree remove` | `gc worktree clean` | **DONE** | Remove specific or all worktrees |
| Beads redirect in worktree | Beads redirect | **DONE** | Points to shared rig store |
| Formula symlink in worktree | Formula symlink | **DONE** | Materialized in worktree |
| Worktree gitignore management | `ensureWorktreeGitignore` | **DONE** | Appends infrastructure patterns (.beads/redirect, .gemini/, etc.) to worktree .gitignore. Idempotent, gated on config. |
| Cross-rig worktrees | — | **TODO** | Worktree in another rig's repo |
| Stale worktree repair (doctor) | Doctor worktree check | **DONE** | WorktreeCheck validates .git pointers, --fix removes broken entries |

---

## 20. Dogs (Cross-Rig Workers)

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt dog add/remove` | — | **REMAP** | Config-driven pool agents scoped to city |
| `gt dog list/status` | `gc session list` | **REMAP** | City-wide agents shown |
| `gt dog call/dispatch/done/clear` | `gc sling` | **REMAP** | Sling to city-wide agent pool |

---

## 21. Costs & Accounts

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt costs` | — | **N/A** | Deployment analytics |
| `gt costs record/digest/migrate` | — | **N/A** | |
| `gt account list/add/default/status/switch` | — | **TODO** | Multi-account management for quota rotation |
| `gt quota status/scan/clear/rotate` | — | **TODO** | Rate-limit detection and account rotation |

---

## 22. Dashboard & UI

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt dashboard` | — | **TODO** | Web dashboard for convoy tracking |
| `gt status-line` | `session_setup` + scripts | **DONE** | Inlined as `examples/gastown/scripts/status-line.sh`, called via tmux `#()` in status-right |
| `gt theme` | — | **N/A** | tmux theme management |
| `gt dnd` (Do Not Disturb) | — | **N/A** | Notification suppression |
| `gt notify` | — | **N/A** | Notification level |
| `gt issue show/set/clear` | — | **N/A** | Status line issue tracking |

---

## 23. Dolt Integration

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt dolt init` | `dolt.InitCity` | **DONE** | |
| `gt dolt start/stop/status` | `dolt.EnsureRunning/StopCity` | **DONE** | |
| `gt dolt logs` | `gc dolt logs` | **DONE** | Tail dolt server log with `--follow` |
| `gt dolt sql` | `gc dolt sql` | **DONE** | Interactive SQL shell; auto-connects to running server or falls back to file-based |
| `gt dolt init-rig` | `dolt.InitRigBeads` | **DONE** | |
| `gt dolt list` | `gc dolt list` | **DONE** | List dolt databases with table/row counts |
| `gt dolt migrate` | — | **N/A** | Schema migration; one-time |
| `gt dolt fix-metadata` | — | **TODO** | Repair metadata.json |
| `gt dolt recover` | `gc dolt recover` | **DONE** | Recover from corruption: backup, rebuild metadata, verify |
| `gt dolt cleanup` | — | **TODO** | Remove orphaned databases |
| `gt dolt rollback` | `gc dolt rollback` | **DONE** | List backups or restore with --force |
| `gt dolt sync` | `gc dolt sync` | **DONE** | Push to configured remotes; stages, commits, pushes each database |
| Dolt branch per agent | — | **TODO** | Write isolation branches |
| Dolt health ticker | Order recipe: `dolt-health` | **DONE** | Cooldown order (30s) runs `gc dolt status` + `gc dolt start` on failure. Lives in `examples/gastown/formulas/orders/dolt-health/`. |

---

## 24. Miscellaneous Gastown Commands

| Gastown | Gas City | Status | Notes |
|---------|----------|--------|-------|
| `gt callbacks process` | — | **REMAP** | Handled by hook system |
| `gt checkpoint write/read/clear` | — | **REMAP** | Beads-based recovery is sufficient |
| `gt commit` | — | **N/A** | WONTFIX: agents use `git commit` directly; `$GC_AGENT` env var available for author info |
| `gt signal stop` | — | **REMAP** | Hook signal; provider-specific |
| `gt tap guard` | — | **REMAP** | PR workflow guard; provider-specific hook |
| `gt town next/prev/cycle` | — | **N/A** | Multi-town switching; deployment |
| `gt wl` (wasteland federation) | — | **N/A** | Cross-town federation; future |
| `gt swarm` (deprecated) | — | **N/A** | Superseded by convoy |
| `gt synthesis` | — | **REMAP** | Convoy synthesis is a prompt-level concern; agents use `bd mol pour` + formula steps |
| `gt whoami` | — | **N/A** | WONTFIX: `$GC_AGENT` env var is sufficient |

---

## Priority Summary

### P0 — Critical for feature parity (blocks gastown-as-gc-config)

These are features that gastown's configuration depends on to function:

1. ~~**Agent nudge**~~ — DONE (`gc session nudge <name> <msg>`)
2. ~~**`gc done`**~~ — REMAP (inlined to prompt: `git push` + `bd create` + `bd close` + exit)
3. ~~**Agent bead lifecycle**~~ — REMAP (just bd: `bd create --type=agent` + `bd update --label`)
4. ~~**Bead slot (hook) operations**~~ — N/A WONTFIX (no hooked beads; users can use bd)
5. ~~**Unsling/unhook**~~ — N/A WONTFIX (no hooked beads; users can use bd)
6. ~~**Mail enhancements**~~ — DEFERRED (peek/hook covered by `gc mail check --inject`; rest add when needed)
7. ~~**Molecule lifecycle**~~ — REMAP (all subcommands are just bd: `bd mol current`, `bd close <step>`, `bd update --assignee`)
8. ~~**Merge queue**~~ — REMAP (all subcommands are just bd: `bd list --assignee=...`, `bd update --status=open`)
9. ~~**Convoy tracking**~~ — DONE (`gc convoy create/list/status/add/close/check/stranded`; reactive feeding N/A — pull-based GUPP)
10. ~~**`gc broadcast`**~~ — DEFERRED (no use case yet; revisit when needed)
11. ~~**`gc handoff`**~~ — DONE (`gc handoff <subject> [message]`)
12. ~~**Periodic formula dispatch**~~ — REMAP (replaced by file-based order system: `gc order list/show/run/check` with gate evaluation)
13. ~~**GUPP violation detection**~~ — N/A WONTFIX (idle timeout + prompt-level self-assessment cover this; gastown's check depends on hooked beads which Gas City doesn't use)

### P1 — Important for production use

14. ~~**`gc status`**~~ — DONE (`gc status [path]`)
15. ~~**Order system**~~ — DONE (list, show, run, check, gate evaluation with 4 of 5 gate types)
16. ~~**Event visibility tiers**~~ — N/A WONTFIX (`gc events --type` filtering is sufficient)
17. ~~**Escalation system**~~ — N/A WONTFIX (idle timeout + health patrol already cover this)
18. ~~**`gc release`**~~ — REMAP (just bd: `bd update <id> --status=open --assignee=""`)
19. ~~**tmux status line**~~ — DONE (inlined as shell scripts in `examples/gastown/scripts/`, wired via `session_setup`)
20. ~~**Dolt management**~~ — DONE (`gc dolt logs/sql/list/recover/sync`)
21. ~~**Rig management**~~ — N/A WONTFIX (remove: edit city.toml + `gc doctor`; config/settings: edit city.toml; detect/quick-add: `gc rig add` is sufficient)
22. ~~**Session cycling**~~ — DONE (inlined as `examples/gastown/scripts/cycle.sh` + `bind-key.sh`, wired via `session_setup`)
23. ~~**Stale branch cleanup**~~ — DONE (`gc worktree clean` + `removeAgentWorktree` prunes stale branches)
24. ~~**`gc whoami`**~~ — N/A WONTFIX (not used anywhere; `$GC_AGENT` env var is sufficient)
25. ~~**`gc commit`**~~ — N/A WONTFIX (not used anywhere; agents use `git commit` directly)
26. ~~**Commands provisioning**~~ — DONE (generic `overlay_dir` config field copies any directory tree into agent workdir)
27. ~~**Polecat git-state check**~~ — DONE (3 safety checks in `gc worktree clean` + `gc worktree list`)
28. ~~**Worktree gitignore**~~ — DONE (`ensureWorktreeGitignore` manages infrastructure patterns)

### P2 — Nice-to-have / polish

29. ~~**Feed curation**~~ — N/A WONTFIX (`gc events --since/--type/--watch` + OTEL covers this; TUI curator is UX polish that fails Bitter Lesson)
30. ~~**Trail subcommands**~~ — N/A WONTFIX (`git log --since` + `bd list` are trivial shell wrappers, not SDK infrastructure)
31. ~~**Formula types**~~ — REMAP (bd owns all formula types: `bd cook` + `bd mol pour/wisp`)
32. ~~**Formula create**~~ — REMAP (user writes `.formula.toml` file directly)
33. ~~**Formula variables**~~ — DONE (`gc sling --formula --var` passes through to `bd cook --var`)
34. ~~**Formula validate**~~ — REMAP (`bd formula show` validates on parse; `bd cook --dry-run` for full check)
35. ~~**Config set/get**~~ — DEFERRED to P3 (too many footguns; edit city.toml directly)
36. ~~**Agent menu**~~ — DONE (shell script + session_setup keybinding)
37. ~~**Crew refresh/pristine**~~ — DONE/REMAP (refresh = `gc handoff --target`; pristine = just git pull)
38. ~~**Worktree list/remove**~~ — DONE (`gc worktree list` + `gc worktree clean`)
39. ~~**Submodule init**~~ — DONE (Layer 0 side effect in `createAgentWorktree`)
40. ~~**Compact (wisp TTL)**~~ — DONE (deacon order formula `mol-wisp-compact`; raw bd commands)
41. ~~**Workspace sync pre-restart**~~ — DONE (`syncWorktree()` with fetch + pull --rebase + auto-stash; wired into start + pool respawn)
42. ~~**Close-triggers-convoy-check**~~ — DONE (bd on_close hook → `gc convoy autoclose`; reactive, recursive-safe, idempotent)
43. ~~**Sling --merge strategy**~~ — DONE (`--merge direct|mr|local` stores `merge_strategy` metadata)
44. ~~**Sling auto-convoy**~~ — DONE (default behavior; `--no-convoy` to suppress, `--owned` to mark owned)

### P3 — Future / deferred

45. **`gt seance`** — Predecessor session forking; real in gastown but decomposes into events + provider flags
46. ~~**Hooks lifecycle**~~ — WONTFIX (gastown uses 3 overlay settings.json files — default/crew/witness — instead of base+override merge; `overlay_dir` in city.toml handles installation)
47. **Dashboard** — Web UI for convoy tracking
48. **Address resolution** — @town, @rig group patterns for mail
49. **Cross-rig worktrees** — Agent worktree in another rig's repo
50. **Account management** — `gc account add/list/switch/default/status` + per-sling `--account` for quota rotation
51. **Quota rotation** — `gc quota scan/rotate/status/clear` for multi-account rate-limit management

### Remaining TODO items (not yet resolved)

| # | Feature | Section | Priority |
|---|---------|---------|----------|
| 6 | Convoy land/launch/stage | 10 | P2 |
| 7 | Sling --args | 7 | P2 |
| 11 | PreToolUse/PostToolUse hooks | 14 | P2 |
| ~~12~~ | ~~Order event gate type~~ | ~~15~~ | **DONE** |
| ~~13~~ | ~~Order tracking (last-run)~~ | ~~15~~ | ~~P1 DONE~~ |
| 14 | Message templates | 18 | P2 |
| 15 | CLAUDE.md generation | 18 | P2 |
| ~~19~~ | ~~Sling --stdin~~ | ~~7~~ | **DONE** |
| 20 | Sling --account | 7 | P3 |
| 21 | Hooks sync/diff/base/override/list/scan/init | 14 | P3 |
| 22 | Roundtrip-safe settings editing | 14 | P3 |
| ~~23~~ | ~~Order history~~ | ~~15~~ | ~~DONE~~ |
| 24 | Order execution timeout | 15 | P3 |
| ~~25~~ | ~~Embedded system formulas~~ | ~~9~~ | **DONE** |
| 26 | Dolt fix-metadata | 23 | P3 |
| 27 | Dolt cleanup | 23 | P3 |
| 28 | ~~Dolt rollback CLI~~ | 23 | **DONE** |
| ~~29~~ | ~~Dolt branch per agent~~ | ~~23~~ | **WONTFIX** — gastown implemented then removed (Feb 2026); write contention solved differently |
| ~~30~~ | ~~Rig fork push_url~~ | ~~12~~ | **WONTFIX** |
| 31 | Rig reset | 12 | P3 |
| 32 | ~~Stale worktree repair (doctor)~~ | 19 | **DONE** |
| 33 | Cross-rig worktrees | 19 | P3 |
| 34 | Custom bead types | 6 | P3 |
| ~~35~~ | ~~Crew next/prev cycling~~ | ~~5~~ | **DONE** — tmux keybinding overrides in `examples/gastown/scripts/` (cycle.sh, bind-key.sh, agent-menu.sh) |
| 36 | Convoy blocking dependency | 10 | P3 |
| 37 | Health heartbeat protocol | 13 | P3 |
| 38 | Dashboard (web UI) | 22 | P3 |
| 39 | Account management | 21 | P3 |
| 40 | Quota rotation | 21 | P3 |
| ~~41~~ | ~~Handoff --collect (auto-state)~~ | ~~7~~ | **WONTFIX** |
| 42 | ~~Scrollback clear on restart~~ | 3 | **DONE** |

### N/A — Not SDK scope

- ~~Costs/accounts/quota (deployment analytics)~~ Costs are N/A but accounts + quota are P3 (see #46-47)
- Themes/DND/notifications (UX polish)
- Town cycling (multi-town deployment)
- Wasteland federation (cross-town)
- Shell integration (deployment)
- Agent presets (config handles this)
- ~~Name pools~~ **DONE** (namepool feature)

---

## Effort Estimates

| Priority | TODO Items | Estimated Lines | Notes |
|----------|-----------|-----------------|-------|
| P0 | 0 remaining | — | All P0 items resolved (DONE, REMAP, or N/A) |
| P1 | 0 remaining | — | All P1 items resolved |
| P2 | 6 items (#6-15) | ~1,200-2,300 | Sling flags, convoy features, hooks, templates |
| P3 | 23 items (#20-42) | ~3,400-4,900 | Hook lifecycle, order polish, dolt CLI, formula resolution, rig ops, accounts, dashboard |
| **Total** | **29 TODO items** | **~4,600-7,200** | All P0+P1 cleared; 5 P2 resolved, 2 P2 WONTFIX, 1 P2 REMAP (MR bead fields = just bd metadata) |

Current Gas City: ~14,000 lines of Go (excl. tests, docs, generated).
Feature parity target: ~20,000-23,000 lines.

---

## Audit Log

| Date | Change |
|------|--------|
| 2026-02-27 | Initial audit: 92 gastown commands mapped, 42 features tracked |
| 2026-02-27 | Deep comparison (7 agents): +8 new gaps, 12 status corrections, 38 TODO items remaining. Dolt logs/sql/list/recover/sync→DONE. Order list/show/run/check→DONE. Polecat git-state→DONE. Worktree gitignore→DONE. Periodic dispatch→REMAP (orders). Template vars→PARTIAL (missing DefaultBranch). Gemini hooks→VERIFY. |
| 2026-02-27 | P2 verification: 4 items resolved (workspace sync→DONE, close-triggers-convoy→DONE via bd on_close hook, sling --merge→DONE, sling auto-convoy→DONE). 2 items WONTFIX (reactive feeding — pull-based GUPP obviates; --max-concurrent — pool min/max is sufficient). 1 item REMAP (MR bead fields — just `bd update --set-metadata branch=X target=Y`, gastown formulas already use this). Convoy-check polling order removed. Order tracking→PARTIAL. 31 TODO items remaining (7 P2, 24 P3). |
| 2026-03-06 | Provider parity audit: added auggie/pi/omp presets (7→10 providers). Claude settings.json: added `skipDangerousModePermissionPrompt`, `editorMode`, PATH export. Added `SupportsHooks` and `InstructionsFile` to ProviderSpec (metadata; `AgentHasHooks` retains hardcoded Claude check — `SupportsHooks` fallback reverted as behavioral regression). Added pi/omp hook templates with PATH augmentation. Added `TestSupportsHooksSyncWithProviderSpec` cross-check. |
