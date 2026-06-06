---
title: "Orchestration Framework Survey"
---

> Wasteland item: w-gc-004. Compares Gas City's primitives against the
> dominant multi-agent orchestration frameworks of 2025-2026. Identifies
> borrowable patterns and surfaces honest gaps. Audience: SDK contributors
> and consumers.

## Reference: Gas City's nine concepts

Five primitives — **Session, Task Store (Beads), Event Bus, Config,
Prompt Templates** — and four derived mechanisms — **Messaging,
Formulas/Molecules, Dispatch (Sling), Health Patrol**. See
[`AGENTS.md`](../../AGENTS.md) for the canonical definitions. The
non-negotiables that distinguish Gas City from peer frameworks are
**ZERO hardcoded roles**, **Zero Framework Cognition (ZFC)**, and the
**Bitter Lesson** filter on every primitive (see
[`engdocs/contributors/primitive-test.md`](../contributors/primitive-test.md)).

The mapping convention used below: a checkmark means the framework has
a roughly equivalent first-class concept; "partial" means the concept
exists but is weaker or implicit; "—" means no equivalent. The mapping
is intentionally lossy — see "Where mapping is forced" at the end.

## Frameworks

### AutoGen (Microsoft)

Source: [microsoft.github.io/autogen](https://microsoft.github.io/autogen/stable/user-guide/core-user-guide/core-concepts/architecture.html),
[github.com/microsoft/autogen](https://github.com/microsoft/autogen).

- **Core abstractions:** `AgentRuntime`, `RoutedAgent`, `MessageContext`,
  `Topic`, `Subscription`. AutoGen Core models agents as actors that
  exchange typed messages.
- **Roles defined as:** Python subclasses of `RoutedAgent` plus
  `@message_handler` decorators. Behavior is code, not config.
- **Persistence:** In-memory by default; `AgentRuntime` can be standalone
  (single process) or distributed (gRPC host servicer + workers).
- **Events / messaging:** Pub/sub via topics is the *primary* concept —
  closer to Gas City's Event Bus than to its Mail. Direct send is also
  supported.
- **Work tracking:** Not first-class. Tasks are messages; persistence is
  the consumer's problem.
- **Health/failure:** Distributed runtime handles worker reconnection;
  agent-level recovery is application code.
- **Mapped to Gas City:** Session = partial (agent instance), Task Store
  = —, Event Bus = ✓ (pub/sub topics), Config = — (code), Prompt
  Templates = — (system prompts inline in code), Messaging = ✓,
  Dispatch = partial (topic routing), Health Patrol = partial.
- **Gets right:** Strong actor-style typed-message contract; clean
  separation between runtime and agent logic. The runtime-vs-agent
  split is the same instinct Gas City has with `controller` vs
  user-configured roles.
- **Where Gas City diverges:** Roles in AutoGen are Python classes. Gas
  City's ZERO-hardcoded-roles invariant rules that out: a `[[agent]]`
  TOML stanza plus a prompt template is the entire role definition.

### CrewAI

Source: [docs.crewai.com](https://docs.crewai.com/concepts/agents).

- **Core abstractions:** `Agent`, `Task`, `Crew`, `Process` (sequential
  or hierarchical), `Tool`.
- **Roles defined as:** `role`, `goal`, `backstory` strings on the
  `Agent`. Configurable via YAML or Python. This is the closest
  industry analog to Gas City's "roles are config."
- **Persistence:** Optional in-process memory; no built-in durable
  store.
- **Events / messaging:** No first-class bus. Inter-agent communication
  is mediated by `allow_delegation` and the crew's `Process`.
- **Work tracking:** `Task` objects executed by the crew; no
  dependency graph beyond linear/hierarchical processes.
- **Health/failure:** `max_retry_limit` per agent (default 2);
  `step_callback` for observability. No stall detection.
- **Mapped to Gas City:** Session = partial, Task Store = partial
  (in-memory tasks), Event Bus = — (callbacks only), Config = ✓ (YAML),
  Prompt Templates = ✓ (`role`/`goal`/`backstory`), Messaging = partial
  (delegation), Dispatch = partial (`Process`), Health Patrol = —.
- **Gets right:** YAML-defined agents demonstrably work; the `Process`
  enum is a clean activation knob analogous to Gas City's progressive
  capability levels.
- **Where Gas City diverges:** CrewAI's `Process` is hardcoded (sequential
  and hierarchical; a `consensual` variant is sketched as a `TODO` in
  [`src/crewai/process.py`](https://github.com/crewAIInc/crewAI/blob/main/lib/crewai/src/crewai/process.py)
  but not implemented as of this survey). Gas City pushes orchestration
  shape into formulas, where the model can compose new shapes. Also,
  CrewAI's in-process memory fails the NDI ("work survives session death")
  test.

**Contradicts the wasteland item's premise:** CrewAI already treats roles
as YAML config. The "ZERO hardcoded roles" stance is not unique; what
*is* unique is Gas City's strict reading of the rule — per AGENTS.md,
"If a line of Go references a specific role name, it's a bug." Not just
no role-identity branches: no role-name mentions in Go source at all.

### LangGraph

Source: [github.com/langchain-ai/langgraph](https://github.com/langchain-ai/langgraph)
README (concept docs at docs.langchain.com would not render via WebFetch —
*not verified beyond README*).

- **Core abstractions:** Per the README, LangGraph is a "low-level
  orchestration framework for building, managing, and deploying
  long-running, stateful agents," inspired by Pregel and Apache Beam,
  with public-interface inspiration from NetworkX. The library's
  standard concepts (StateGraph, nodes, edges, channels, checkpointers,
  threads) are referenced by feature pages — *not verified directly via
  fetch in this survey*.
- **Roles defined as:** Code. Each node is a Python function that reads
  and writes a typed `State`.
- **Persistence:** Checkpointers (described in the README's
  "durable execution" bullet) persist graph state per-thread so runs
  resume from failure.
- **Events / messaging:** Streaming over node outputs; the graph itself
  is the event substrate (no separate bus).
- **Work tracking:** State channels with reducers. The graph step is
  the unit of work; there is no separate task store.
- **Health/failure:** Durable execution is the headline feature — a
  crashed run resumes at the last checkpoint.
- **Mapped to Gas City:** Session = ✓ (thread + checkpoint), Task Store
  = partial (state channels, not a queryable work table), Event Bus =
  partial (streamed deltas), Config = — (Python), Prompt Templates =
  —, Messaging = partial, Dispatch = ✓ (edges as router functions),
  Health Patrol = partial (resumption replaces stall detection).
- **Gets right:** Durable resume from checkpoint is the strongest
  failure model in the survey. Gas City's NDI promise is achieved
  through a different route (work-as-bead, redundant observers); the
  checkpoint approach is the obvious alternative.
- **Where Gas City diverges:** LangGraph encodes control flow in Python
  (`add_edge`, `add_conditional_edges`). Conditional edges are
  judgment-call branches in framework code — a ZFC violation by Gas
  City's standard. Gas City instead expresses branching as gate
  conditions on the Event Bus, evaluated against bead state.

### OpenAI Swarm and OpenAI Agents SDK

Sources: [github.com/openai/swarm](https://github.com/openai/swarm),
[openai.github.io/openai-agents-python](https://openai.github.io/openai-agents-python/).

Swarm is explicitly **deprecated** ("experimental, educational" — the
repo points users to the OpenAI Agents SDK as its production successor).
Both are covered together because the conceptual core (handoff as a
primitive) is preserved.

- **Core abstractions:** `Agent` (instructions + tools), `handoff`,
  `Guardrail`, `Session`, `Trace`.
- **Roles defined as:** Python `Agent` instances with an instruction
  string and a tool list. Handoffs are returned from tool functions.
- **Persistence:** Swarm is stateless between calls (Chat Completions
  semantics). The Agents SDK adds a `Sessions` layer with SQLite,
  Redis, MongoDB, SQLAlchemy, and Dapr backends.
- **Events / messaging:** Built-in tracing for visualization and eval;
  no general-purpose event bus.
- **Work tracking:** None — work is implicit in conversation history.
- **Health/failure:** Guardrails validate inputs/outputs; no
  stall/restart loop.
- **Mapped to Gas City:** Session = ✓, Task Store = —, Event Bus =
  partial (tracing), Config = — (code), Prompt Templates = partial
  (instruction string), Messaging = ✓ (handoff), Dispatch = ✓
  (handoff), Health Patrol = —.
- **Gets right:** Handoff as a return value from a tool function is a
  beautifully small primitive. Gas City's Sling does more (formula
  selection, convoy creation, event log) but the handoff-as-tool
  pattern is worth borrowing for in-session delegation.
- **Where Gas City diverges:** Handoff transfers a conversation; Sling
  creates persistent work units. Gas City's NDI principle requires the
  latter.

### smolagents (Hugging Face)

Source: [huggingface.co/docs/smolagents](https://huggingface.co/docs/smolagents/index).

- **Core abstractions:** `CodeAgent`, `ToolCallingAgent`, `Tool`. The
  CodeAgent variant has the model emit Python that is executed in a
  sandbox.
- **Roles defined as:** Code (Python class instances). The library
  intentionally keeps abstractions thin ("~thousand lines of code").
- **Persistence:** None built in.
- **Events / messaging:** None — single-agent loop is the default.
- **Multi-agent support:** A "building multi-agent systems" tutorial
  exists but the framework offers no native bus or task store.
- **Mapped to Gas City:** This is essentially a per-agent runtime
  *primitive* (Session + tool loop) and nothing else.
- **Gets right:** The Bitter Lesson is honored explicitly — the docs
  brag about *not* abstracting. CodeAgent's "model writes code that
  composes tool calls" is strictly more expressive than JSON tool
  calling and a pattern Gas City consumers can reach for via the
  prompt layer.
- **Where Gas City diverges:** smolagents punts everything above the
  single-agent loop to the user. Gas City punts roles but provides
  persistence, dispatch, and health.

### Inspect AI (UK AISI)

Source: [inspect.aisi.org.uk/agents.html](https://inspect.aisi.org.uk/agents.html).

- **Core abstractions:** `Solver`, `Agent` (via `@agent` decorator),
  `AgentState` (messages + output), `Tool`, `Scorer`. ReAct is the
  default agent; Deep Agent and an Agent Bridge to external frameworks
  exist.
- **Roles defined as:** Python decorator + parameters (`name`,
  `description`, `prompt`).
- **Persistence:** `AgentState` persists across calls within a task;
  task transcripts are the durable artifact.
- **Multi-agent support:** `handoff()` shares conversation history;
  `as_tool()` delegates with isolated interfaces.
- **Mapped to Gas City:** Session = ✓ (`AgentState`), Task Store =
  partial (transcripts), Event Bus = — (transcript is the log),
  Config = —, Prompt Templates = ✓, Messaging = ✓ (`handoff`/`as_tool`),
  Dispatch = partial, Health Patrol = — (evals are externally scored,
  not health-monitored).
- **Gets right:** The handoff-vs-as-tool distinction is sharper than
  most peers — it cleanly separates "share my context" from "delegate
  with an opaque interface." Gas City conflates these under Sling
  today.
- **Where Gas City diverges:** Inspect is an *evals* framework
  repurposed for agent workflows; production coordination (pools,
  health patrol, durable mail) is out of scope.

### Claude Agent SDK (Anthropic)

Source: [code.claude.com/docs/en/agent-sdk/overview](https://code.claude.com/docs/en/agent-sdk/overview).

- **Core abstractions:** `query()` loop, `ClaudeAgentOptions`, built-in
  tools (Read, Edit, Bash, Glob, Grep, WebSearch, WebFetch, Monitor,
  AskUserQuestion), hooks (`PreToolUse`, `PostToolUse`, `Stop`,
  `SessionStart`, `SessionEnd`, `UserPromptSubmit`), subagents,
  sessions (JSONL on filesystem), MCP servers, skills, slash commands,
  plugins.
- **Roles defined as:** `AgentDefinition` objects with a
  `description`, `prompt`, and tool allowlist — config-shaped though
  passed as code. Filesystem-defined skills (`.claude/skills/*/SKILL.md`)
  and subagents are pure config.
- **Persistence:** Sessions are JSONL files on the local filesystem; can
  be resumed by ID or forked. Managed Agents (separate offering) host
  an event log server-side.
- **Events / messaging:** Hooks are the event mechanism — code runs at
  named lifecycle points. No general pub/sub bus.
- **Multi-agent support:** Subagents spawned via the `Agent` tool with
  per-subagent prompts and tool lists. Messages carry
  `parent_tool_use_id` for tracking.
- **Mapped to Gas City:** Session = ✓ (resume + fork), Task Store = —
  (work lives in the conversation), Event Bus = partial (hooks),
  Config = ✓ (`AgentDefinition`, `.claude/`), Prompt Templates = ✓
  (Markdown skills and subagent prompts), Messaging = partial
  (subagent results bubble up), Dispatch = partial (Agent tool
  invocation), Health Patrol = —.
- **Gets right:** This is the closest sibling to Gas City. The
  filesystem-as-config pattern (`.claude/skills/`, `.claude/commands/`)
  validates the city-as-directory model. Hooks at named lifecycle
  points are exactly the seam Gas City uses for its bead-lifecycle
  projection.
- **Where Gas City diverges:** Claude Agent SDK has no Task Store and
  no inter-session messaging — every coordination problem collapses
  back to nesting (subagent within parent). Gas City separates the
  work (beads) from the workers (sessions) so work survives session
  death. The Agent SDK has an explicit "no skills system / no MCP /
  no decision logic" *opposite* stance: it embraces all three.

### Magentic-One (Microsoft Research)

Source: [microsoft.com/en-us/research/articles/magentic-one](https://www.microsoft.com/en-us/research/articles/magentic-one-a-generalist-multi-agent-system-for-solving-complex-tasks/).

- **Core abstractions:** Orchestrator + four specialist agents
  (WebSurfer, FileSurfer, Coder, ComputerTerminal). Two ledgers:
  *Task Ledger* (outer loop: facts, guesses, plan) and *Progress
  Ledger* (inner loop: assignments, progress).
- **Roles defined as:** Python classes built atop AutoGen; the role set
  is hardcoded.
- **Persistence:** Ledgers are in-memory state on the Orchestrator.
- **Health/failure:** "If the Orchestrator finds that progress is not
  being made for enough steps, it can update the Task Ledger and
  create a new plan." Stall detection is implicit replanning.
- **Mapped to Gas City:** Session = ✓, Task Store = partial (the two
  ledgers are orchestrator-internal in-memory state, not a persisted
  queryable work store like beads), Event Bus = — (AutoGen pub/sub
  inherited), Config = — (code), Prompt Templates = partial, Messaging =
  ✓, Dispatch = ✓ (Orchestrator assigns), Health Patrol = partial
  (replanning runs inside the orchestrator loop; it is not a separable
  patrol primitive that probes external state and publishes stalls).
- **Gets right:** Explicit dual-ledger separation between the *plan*
  and the *progress against the plan*. Gas City has a single bead
  graph that conflates these. Worth borrowing as a query convention,
  even if not as a primitive.
- **Where Gas City diverges:** Magentic-One is the canonical
  hardcoded-role system. Its name-the-roles-in-code design is the
  exact anti-pattern Gas City was built to escape.

### Platform offerings (noted, not surveyed)

AWS Bedrock Agents, Google Vertex Agent Builder, and the OpenAI
Assistants API are vendor-hosted services rather than orchestration
*frameworks*. They expose agents-as-managed-resources with vendor-defined
lifecycles. Comparing them to Gas City's primitives is a category error;
they are competitors to *running* a Gas City, not to *being* one. Skipped
to avoid padding.

## Cross-cutting findings

### Where Gas City's stance is genuinely different

1. **ZERO hardcoded roles is real but narrower than it sounds.** CrewAI
   already defines roles in YAML. Claude Agent SDK already defines
   subagents in config. The actual unique constraint, per AGENTS.md, is
   strict: *"If a line of Go references a specific role name, it's a
   bug."* That is stronger than "no branching on role identity" — even
   a string mention of a role name in Go source is a violation. Neither
   CrewAI's `Process` enum nor Magentic-One's Orchestrator could exist
   inside Gas City's `internal/`.
2. **ZFC is genuinely uncommon.** LangGraph's conditional edges,
   Magentic-One's stall detection, CrewAI's `Process` selection, and
   AutoGen's topic routing all contain judgment calls in framework
   code. Only smolagents and Inspect AI take a similarly minimal
   stance, and they do so by punting orchestration entirely.
3. **Work as a first-class persistent primitive is rare.** Only
   Magentic-One (in-memory ledgers) and LangGraph (state channels)
   approach a task store, and neither is queryable like Beads. Most
   frameworks model work as conversation history.
4. **Health Patrol as a separable concern is almost unique.** No
   surveyed framework has a stall-detect-and-publish loop separable
   from the agents being monitored. LangGraph resumes from checkpoint;
   Magentic-One replans from inside the Orchestrator. Gas City's
   controller-driven Health Patrol is closer to a Kubernetes liveness
   probe than to an agent feature.

### Borrowable patterns Gas City should consider

1. **LangGraph's checkpointer model for session resume.** Gas City's
   session lifecycle projection already approaches this; a deliberate
   "resume from last bead transition" contract would tighten it and
   match an idiom users coming from LangGraph already know.
2. **Inspect AI's handoff-vs-as-tool distinction.** Gas City Sling
   today collapses "delegate with shared context" and "delegate as an
   opaque RPC." Splitting the surface (perhaps by formula type) would
   reduce the temptation to overload molecule shape.
3. **Claude Agent SDK's filesystem-as-config (`.claude/skills/`,
   `.claude/commands/`).** This validates the city-as-directory model
   and suggests a natural place for Gas City formulas to live as
   discoverable Markdown plus TOML rather than purely TOML stanzas.
4. **Magentic-One's dual ledger (Task / Progress).** Even without
   adding a primitive, a convention for distinguishing "what the plan
   is" beads from "what has happened" beads (already partially encoded
   in bead types and convoys) would make orchestrator-style consumers
   easier to write.
5. **OpenAI Agents SDK's pluggable session backend.** The
   SQLite/Redis/MongoDB/Dapr matrix is overkill for Gas City, but the
   shape — a Session interface with concrete adapters — is consistent
   with Gas City's runtime provider split (tmux/subprocess/exec/k8s/fake).

### Honest gaps the survey reveals in Gas City

1. **No checkpoint primitive.** LangGraph's "durable execution"
   resumes a graph mid-edge. Gas City's NDI principle covers session
   *death* well (work survives because beads survive), but a partially
   completed molecule has no equivalent of LangGraph's
   "resume from this exact step." The closest analog is re-hooking the
   highest-priority unfinished child bead, which loses any
   in-tool-call state.
2. **Inter-session messaging is thinner than peers'.** Mail-as-bead is
   durable but slow (mail = `TaskStore.Create`). AutoGen's typed
   pub/sub topics and the Claude Agent SDK's structured hook payloads
   both have lower-latency and better-typed message shapes. Gas City's
   Event Bus is event-typed but is not designed for agent-to-agent
   reply patterns; mail is, but it pays a bead-CRUD cost per message.

### Where mapping is forced

The nine-concepts grid above flattens a real difference: most surveyed
frameworks have *one* axis (the conversation) on which messaging, work
tracking, and persistence all collapse. Gas City has *three* axes
(Session / Task Store / Event Bus). Calling a LangGraph state channel
a "Task Store" or a Swarm handoff "Dispatch" is approximate. The
correct reading of the table is: where a row says "—", the framework
genuinely has no equivalent abstraction, not just a different name.

## Open questions

- Is the checkpoint gap (Finding 1 above) worth a primitive, or is
  rehooking the next bead good enough in practice? Apply the
  Primitive Test before adding anything.
- Should Sling acquire an explicit "handoff" variant that shares
  session context rather than spawning a new molecule, à la Inspect
  AI's `handoff()` vs `as_tool()`?
- Where should formulas physically live? Today they are TOML stanzas;
  Claude Agent SDK's `.claude/skills/*/SKILL.md` filesystem layout is
  a strong precedent for a Markdown-plus-frontmatter discovery model.

## References

- AutoGen architecture: <https://microsoft.github.io/autogen/stable/user-guide/core-user-guide/core-concepts/architecture.html>
- AutoGen repo: <https://github.com/microsoft/autogen>
- CrewAI agents: <https://docs.crewai.com/concepts/agents>
- LangGraph repo README: <https://github.com/langchain-ai/langgraph>
- OpenAI Swarm: <https://github.com/openai/swarm>
- OpenAI Agents SDK: <https://openai.github.io/openai-agents-python/>
- smolagents docs: <https://huggingface.co/docs/smolagents/index>
- Inspect AI agents: <https://inspect.aisi.org.uk/agents.html>
- Claude Agent SDK overview: <https://code.claude.com/docs/en/agent-sdk/overview>
- Magentic-One research article: <https://www.microsoft.com/en-us/research/articles/magentic-one-a-generalist-multi-agent-system-for-solving-complex-tasks/>

LangGraph's concept pages (`concepts/low_level`, `concepts/persistence`)
returned only redirect shells via WebFetch at the time of writing; the
claims about checkpointers, threads, and durable execution were
verified against the project README only. Treat LangGraph specifics as
provisional pending a docs-site fetch.
