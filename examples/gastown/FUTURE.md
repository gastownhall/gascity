# Gas Town Example — Future Work

Tracks gc commands and features referenced in prompts/formulas that
don't exist yet in the gc binary. This file is the gap analysis between
"Gas Town expressed as configuration" and "gc can actually run it."

## Missing gc commands

Commands referenced in prompts and formulas but not yet implemented.
Grouped by priority tier.

### Tier 1: Core Propulsion

These are required for any agent to do useful work.

| Command | Description | Referenced in |
|---------|-------------|---------------|
| `gc hook` | **NEEDS IMPL:** Thin wrapper over bd protocol: (1) `bd list --assignee=$GC_AGENT --status=in_progress` (current work), (2) `bd ready --assignee=<pool>` (search pool), (3) `bd update <bead> --claim --assignee=$GC_AGENT` (atomic grab). Returns current/claimed bead or nothing. | All 8 prompts, most formulas |
| ~~`gc sling <bead> <rig>`~~ | **RESOLVED:** Use `bd update <bead> --assignee=<role>` + pool auto-scaling | mayor, deacon, convoy-feed, orphan-scan, session-gc |
| ~~`gc done`~~ | **RESOLVED:** Push branch + `bd create --type=merge-request --assignee=refinery` + `bd close <work-bead>` + exit | polecat, dog |
| ~~`gc nudge <target> "msg"`~~ | **RESOLVED:** Use `gc session nudge <name> <msg>` for message delivery. Scoped to health patrol (deacon/dog). Remove from mayor/crew/witness prompts. | mayor, deacon, witness, crew, refinery, boot-triage |
| ~~`gc polecat list/nuke/status/remove`~~ | **RESOLVED:** `gc session list` (with filters) for listing/status. Self-nuke on success; reconciler + idempotent resume on crash; crash loop backoff prevents thrashing. No polecat-specific commands. | mayor, witness, refinery |
| ~~`gc session status/start/stop`~~ | **RESOLVED:** Controller reconciler handles liveness + restart. `gc session list` for status. | witness, deacon, boot-triage |

### Tier 2: Agent Management

Required before multi-agent orchestration works.

| Command | Description | Referenced in |
|---------|-------------|---------------|
| ~~`gc handoff -s "..." -m "..."`~~ | **RESOLVED:** `gc mail send -s "HANDOFF" -m "..."` + exit. On restart, `gc hook` finds in-progress work; handoff mail in ready queue provides context. | mayor, deacon, witness, refinery, polecat, crew |
| ~~`gc prime`~~ | **RESOLVED:** Already implemented. | polecat, all prompts (recovery note) |
| ~~`gc escalate "desc" -s SEVERITY`~~ | **RESOLVED:** Just mail: `gc mail send witness/ -s "ESCALATION: <desc>" -m "<details>"`. Prompt spells out the protocol. | polecat |
| ~~`gc mq list/submit/integration`~~ | **RESOLVED:** MR beads replace merge queue (`gc hook` for refinery). Integration branches are git workflow + bead metadata — gastown-gc helper territory, not SDK primitive. | refinery |
| ~~`gc deacon heartbeat/cleanup-orphans/redispatch/zombie-scan`~~ | **RESOLVED:** Controller handles all: liveness (no heartbeat file), orphan cleanup (reconciler), redispatch (`bd update --assignee=<pool>`), zombie detection (dead session restart + crash loop backoff). | deacon |
| ~~`gc boot status/spawn/triage`~~ | **RESOLVED:** Controller handles agent liveness and restart. Boot role's job (watch deacon, restart if dead) is the controller's reconcile loop. | boot |
| ~~`gc dog status/done/clear/list/add/remove`~~ | **RESOLVED:** Dogs are pooled agents. `gc session list` for status, `bd close` + exit for done, pool auto-scaling for add/remove. | dog, deacon-patrol |
| ~~`gc mayor stop/start`~~ | **RESOLVED:** Mayor is just an agent. Controller handles liveness and restart. No role-specific commands. | deacon |

### Tier 3: Operational

Important for full Gas Town operation.

| Command | Description | Referenced in |
|---------|-------------|---------------|
| `gc peek <target> [lines]` | **NEEDS IMPL:** New agent API — get last N lines of session output. Delegates to `session/tmux` (`tmux capture-pane`). | witness, boot-triage |
| ~~`gc feed --since <duration>`~~ | **RESOLVED:** Already exists as `gc events --since <duration> [--type <type>]` | deacon-patrol, boot-triage, digest-generate |
| ~~`gc worktree <rig>` / `list` / `remove`~~ | **RESOLVED:** Not needed. Worktree setup handled by `pre_start` calling pack scripts. Cross-rig work is raw `git worktree` commands in the prompt. | crew |
| `gc convoy list/check/stranded/create/status` | **OPEN:** Convoys sit in the same space as epics — batch coordination over related beads. Which layer do they belong in? Bead metadata? Molecules? Separate primitive? | deacon-patrol, convoy-feed, convoy-cleanup |
| `gc context --usage` | **NEEDS IMPL:** New agent API — query session provider for context window utilization. Provider-specific (env var, API, etc.). Prompt decides what to do with the number. | deacon-patrol, refinery-patrol |
| `gc rig start/stop/park/dock/unpark/undock/restart/reboot/status` | **NEEDS IMPL:** Rig lifecycle management. start/stop (agents up/down), park/unpark (temporary pause — controller skips), dock/undock (permanent disable), status (rig health), restart/reboot (stop+start). | deacon, witness, mayor, crew |
| ~~`gc crew stop <name>`~~ | **RESOLVED:** Replace with `gc agent suspend <name>` — generic agent suspension, not role-specific. | crew |

### Tier 4: Maintenance

Supporting infrastructure for long-running systems.

| Command | Description | Referenced in |
|---------|-------------|---------------|
| ~~`gc warrant file <target> --reason "..."`~~ | **RESOLVED:** Just a bead: `bd create --type=warrant --assignee=boot --desc "reason"`. Stuck/stalled detection is prompt-level judgment (ZFC), not controller. | deacon-patrol |
| ~~`gc compact --dry-run/--verbose/report`~~ | **RESOLVED:** Just bd queries. List expired wisps, promote or delete based on status/labels, send digest via mail. All prompt-level logic. | deacon-patrol |
| ~~`gc patrol digest --yesterday`~~ | **RESOLVED:** Just bd queries. List yesterday's patrol digest beads, aggregate into permanent bead, delete sources. Prompt-level work. | deacon-patrol |
| `gc doctor -v / --fix` | **NEEDS IMPL:** System health diagnostics — check city state consistency, stale locks, orphaned sessions, etc. `--fix` for auto-repair. | session-gc, deacon-patrol |
| ~~`gc costs`~~ | **REMOVED:** Not needed. Provider-specific, already disabled in gastown. | deacon-patrol |

### Tier 5: Extended mail operations

Mail is partially implemented (`gc mail send/inbox/read` exist). Complete the namespace — each is thin sugar over bd, but semantic naming makes prompts clearer.

| Command | Description | Referenced in |
|---------|-------------|---------------|
| `gc mail archive <id>` | **NEEDS IMPL:** Close message bead (remove from inbox). Thin wrapper over `bd close`. | deacon, witness, refinery |
| `gc mail delete <id>` | **NEEDS IMPL:** Delete message bead. Thin wrapper over `bd delete`. | deacon |
| `gc mail mark-read <id>` | **NEEDS IMPL:** Label message as read. Thin wrapper over `bd update --label=read`. | mayor |
| `gc mail hook <id>` | **NEEDS IMPL:** Hook existing mail as assignment. Thin wrapper over `bd update --status=hooked`. | all prompts |
| `gc mail send --human` | **NEEDS IMPL:** Send to human overseer. Flag for delivery channel (tmux prompt vs inbox). | crew |
| `gc mail send --notify` | **NEEDS IMPL:** Send with tmux bell notification. Nudge after mail creation. | crew |

## Missing gc features

Features referenced in prompts/formulas that go beyond individual commands.

| Feature | Description | Referenced in |
|---------|-------------|---------------|
| Custom session naming templates | **NEEDS IMPL:** Gas Town uses `{prefix}-{name}` patterns; gc derives `gc-{city}-{agent}`. Allow configurable naming in city.toml. | Implicit in all session references |
| Pre-start hooks (`needs_pre_sync`) | **NEEDS IMPL:** Generic `pre_start` hook on `[[agent]]` config — run a shell command before agent session starts. Not gastown-specific. | refinery, polecat, crew role configs |
| Prompt template rendering | **NEEDS IMPL:** Go `text/template` rendering of prompt files. Variables from city/rig/agent config. Already primitive #5 in the architecture. | All 8 prompts |
| Nudge delivery modes | **OPEN:** Re-review whether mail obviates nudge modes. Future discussion. | witness, deacon, refinery, crew |
| ~~Event channel system~~ | **RESOLVED:** Merged into single primitive below. | refinery-patrol |
| ~~Activity feed subscription~~ | **RESOLVED:** Both await-event and await-signal collapse to `gc events --watch [--type=<filter>] [--timeout=<duration>]`. Kubernetes Watch pattern. Blocking mode on existing `gc events` command. Backoff logic stays in prompt (ZFC). | deacon-patrol, witness-patrol, refinery-patrol |
| ~~Gate system~~ | **RESOLVED:** Gates are beads with metadata (await_type, timeout, waiters). `bd gate list/close/check` already works via `gc bd` passthrough. No gc command needed. | deacon-patrol |
| ~~Order system~~ | **RESOLVED:** Orders are formulas with trigger frontmatter. Deacon reads order dir, checks trigger conditions (filesystem + state.json), executes if open. No gt/gc/bd order commands — all prompt-level. Spec §16 is Tutorial 05c territory. | deacon-patrol |
| ~~Wisp lifecycle~~ | **RESOLVED:** Squash inlined to raw bd: `bd close "$MOL_ID"` + `bd create --type=digest --title="<summary>"`. Closing the root detaches from hook. Step children closed via `bd close`. Await-signal/await-event replaced by `gc events --watch` + prompt-level backoff. Full `gc mol` namespace removed — use `bd mol` directly. | deacon, witness, refinery |
| ~~Agent bead protocol~~ | **RESOLVED:** Just bd operations. Agent bead is a bead with `type=agent` + labels (`idle:N`, `backoff-until:TIMESTAMP`). Liveness = "when was bead last updated." All via `bd update --label` and `bd show`. | witness-patrol, deacon-patrol |

## What exists today

gc commands currently implemented (as of this writing):

- `gc start` / `gc stop` / `gc init`
- `gc rig add` / `gc rig list`
- `gc agent add/suspend/resume` + `gc session list/attach/peek/kill/logs` + `gc runtime drain/undrain/drain-check/drain-ack`
- `gc mail send/inbox/read`
- `gc formula list/show`
- `gc events`
- `gc version`

## Deprecated formulas

Formulas superseded by the assignee + pool auto-scaling model.

| Formula | Reason |
|---------|--------|
| `mol-convoy-feed` | Pool auto-scaling replaces manual dispatch. Agents spawn when `bd ready --assignee=<role>` has work. |
| `mol-convoy-cleanup` | bd on_close hook triggers `gc convoy autoclose` reactively; no polling needed. |
| `mol-convoy-check` | Superseded by bd on_close hook → `gc convoy autoclose`. Removed. |

## Statistics

- **Total gt commands referenced in prompts/formulas:** ~75 unique subcommands
- **Resolved (just bd / already exists / controller / not needed):** ~55
- **Needs implementation in gc SDK:** ~15 commands + 5 features
- **Open design questions:** 2 (convoys, nudge delivery modes)
