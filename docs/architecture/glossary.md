# Glossary

Authoritative definitions of Gas City terms. If a term's usage
elsewhere conflicts with this glossary, this glossary wins and the
other source should be updated.

> Last verified against code: 2026-03-01

## Primitives

- **Agent Protocol**: Start/stop/prompt/observe agents regardless of
  session provider. Covers identity, pools, sandboxes, resume, and
  crash adoption. Layer 0-1 primitive. See
  [`internal/agent/`](../../internal/agent/) and
  [`internal/session/`](../../internal/session/).

- **Bead**: A single unit of work. Everything is a bead: tasks, mail,
  molecules, convoys, epics. Defined in the `Bead` struct with ID,
  Title, Status (`open` / `in_progress` / `closed`), Type, Assignee,
  ParentID, Ref, Needs, Description, and Labels. The universal
  persistence substrate. See [`internal/beads/`](../../internal/beads/).

- **Config**: TOML parsing with progressive activation (Levels 0-8
  based on section presence) and multi-layer override resolution.
  `city.toml` is the single config file. See
  [`internal/config/`](../../internal/config/).

- **Event Bus**: Append-only pub/sub log of all system activity. Two
  tiers: critical (bounded queue for infrastructure) and optional
  (fire-and-forget for audit). Events are immutable with monotonically
  increasing sequence numbers. See
  [`internal/events/`](../../internal/events/).

- **Prompt Template**: Go `text/template` in Markdown defining what
  each role does. The behavioral specification. All role behavior is
  user-supplied configuration rendered through templates.

## Derived Mechanisms

- **Automation**: A formula or shell script dispatch triggered by a
  gate condition. Lives in formula directories as
  `automations/<name>/automation.toml`. Exec automations run shell
  scripts directly (no LLM, no agent, no wisp). Formula automations
  create wisps dispatched to agents. See
  [`internal/automations/`](../../internal/automations/).

- **Convoy**: A container bead that groups related issues as a batch
  work tracking unit. Child beads link to a convoy via ParentID.
  Convoys track completion progress.

- **Dispatch (Sling)**: The routing mechanism that composes: find/spawn
  agent -> select formula -> create molecule -> hook to agent -> nudge
  -> create convoy -> log event. See
  [`cmd/gc/cmd_sling.go`](../../cmd/gc/cmd_sling.go).

- **Epic**: A container bead type that groups child beads for batch
  expansion during dispatch. Like convoy, children link via ParentID.

- **Formula**: A parsed definition from a `*.formula.toml` file with a
  Name, Description, Version, Vars, and Steps array. Each step has an
  ID, Title, Description, and Needs list (dependencies on other steps).
  Formulas define sequences of named work items with dependency
  ordering. See [`internal/formula/`](../../internal/formula/).

- **Gate**: The trigger condition for an automation. Types: `cooldown`
  (interval since last run), `cron` (schedule), `condition` (shell
  exits 0), `event` (specific event type occurs), `manual` (explicit
  invocation only). See
  [`internal/automations/gates.go`](../../internal/automations/gates.go).

- **Health Patrol**: Ping agents (Agent Protocol), compare thresholds
  (Config), publish stalls (Event Bus), restart with backoff. The
  supervision model follows Erlang/OTP patterns.

- **Hook**: Provider-specific agent configuration files installed into
  working directories. Each provider (Claude, Gemini, OpenCode,
  Copilot) has its own format. Hook-enabled agents integrate with Gas
  City automatically: `gc hook` checks for work, `gc prime` outputs
  the behavioral prompt. See
  [`internal/hooks/`](../../internal/hooks/).

- **Label**: A string tag on a bead (`Labels []string`). Labels enable
  pool dispatch (e.g., `pool:dog`), rig scoping (e.g.,
  `rig:tower-of-hanoi`), and arbitrary categorization. Beads are
  queryable by label via `ListByLabel`.

- **Messaging**: Inter-agent communication composed from primitives.
  Mail = `TaskStore.Create(bead{type:"message"})`. Nudge =
  `AgentProtocol.SendPrompt()`. No new primitive needed.

- **Molecule**: A formula instantiated at runtime: one root bead plus
  one step bead per formula step. Progress is tracked by closing step
  beads. `CurrentStep()` computes the next runnable step from
  dependency state.

- **Nudge**: Text sent to an agent's session to wake or redirect it.
  Used for CLI agents that don't accept command-line prompts. Defined
  in `Agent.Nudge` config and delivered via `session.Provider.Nudge()`.

- **Wisp**: An ephemeral molecule. Created by `gc sling --formula` or
  automation dispatch. Wisps auto-close and are garbage-collected after
  a configurable TTL (`wisp_ttl`). The bead store's `MolCook` method
  instantiates wisps from formulas.

## Infrastructure

- **City**: A Gas City instance as a directory on disk containing
  `city.toml` (config), `.gc/` (runtime state), and registered rigs.
  The top-level unit of deployment.

- **Controller**: The long-running daemon that drives all SDK
  infrastructure: config watch (fsnotify), reconciliation tick
  (start/stop agents to match config), automation dispatch (evaluate
  gates, fire due automations). See
  [`cmd/gc/controller.go`](../../cmd/gc/controller.go).

- **Overlay**: A directory tree copied into an agent's working
  directory before startup. Used for pre-staging sandbox configuration.
  See [`internal/overlay/`](../../internal/overlay/).

- **Pool**: Elastic scaling for an agent. The `PoolConfig` struct
  defines Min, Max, Check (shell command returning desired count), and
  DrainTimeout. Pool instances use label-based work dispatch
  (`pool:<name>`). See [`cmd/gc/pool.go`](../../cmd/gc/pool.go).

- **Provider** (Session): Manages agent sessions. The `Provider`
  interface defines lifecycle (Start, Stop, Interrupt), querying
  (IsRunning, ProcessAlive), communication (Attach, Nudge, SendKeys),
  and metadata (SetMeta, GetMeta). Implementations: tmux (production),
  subprocess (remote), k8s (Kubernetes), Fake (test). See
  [`internal/session/session.go`](../../internal/session/session.go).

- **Rig**: An external project directory registered in the city. Each
  rig gets its own beads database, agent hooks, and pack expansion.
  Agents are scoped to rigs via their `dir` field. See
  [`internal/config/config.go`](../../internal/config/config.go).

- **Pack**: A reusable agent configuration directory loaded from
  `pack.toml`. Contains agent definitions, formulas, prompts, and
  automations. City-level packs stamp city-scoped agents;
  rig-level packs stamp rig-scoped agents. `city_agents` in the
  pack metadata partitions which agents are city-scoped vs
  rig-scoped. See
  [`internal/config/pack.go`](../../internal/config/pack.go).

## Design Principles

- **Bitter Lesson**: Every primitive must become MORE useful as models
  improve, not less. Don't build heuristics or decision trees.

- **GUPP**: "If you find work on your hook, YOU RUN IT." No
  confirmation, no waiting. The hook having work IS the assignment.

- **NDI (Nondeterministic Idempotence)**: The system converges to
  correct outcomes because work (beads), hooks, and molecules are all
  persistent. Sessions come and go; the work survives.

- **ZFC (Zero Framework Cognition)**: Go handles transport, not
  reasoning. If a line of Go contains a judgment call, it's a
  violation.
