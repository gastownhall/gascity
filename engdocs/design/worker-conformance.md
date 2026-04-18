---
title: "Worker Conformance & Canonical Worker API"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-04-04 |
| Author(s) | Codex |
| Issue | `test-i83` |
| Supersedes | N/A |

## Summary

Gas City needs a canonical, testable worker contract for tier-1 agent
providers. Today, provider behavior is spread across provider presets,
config resolution, runtime startup logic, transcript readers, and
session-management code. The existing `runtime.Provider` conformance
suite proves low-level runtime mechanics, but it does not prove the
behavioral contract that Gas City actually depends on for Claude,
Codex, and Gemini as first-class interactive workers.

This design introduces a transport-agnostic worker conformance program
and uses it as the executable specification for a future canonical
Worker API.

The design has four pillars:

1. `WorkerCore`: required, deterministic, non-inference conformance for
   the worker behaviors Gas City actually relies on.
2. `WorkerInference`: nightly live certification against real providers,
   using aggressive multi-turn and tool-using workflows.
3. A canonical normalized transcript and interaction contract, treated
   as core session functionality rather than incidental observability.
4. A phased migration toward a dedicated `internal/worker` package that
   becomes the long-term home of the canonical worker boundary.

The first required slice focuses on transcript/session-history and
continuation semantics across the three canonical profiles:

- `claude/tmux-cli`
- `codex/tmux-cli`
- `gemini/tmux-cli`

The goal is not to produce an easy green board. The goal is to make the
real support gaps explicit, stable, and actionable.

## Problem

### Pain today

1. Gas City has no single canonical worker contract for tier-1
   interactive providers.
2. The existing runtime conformance suite validates substrate behavior
   such as start/stop/metadata/peek, but not worker behaviors such as
   startup input delivery, continuation, transcript correctness, or
   structured required interactions.
3. Support for Claude, Codex, and Gemini is fragmented across:
   - built-in provider definitions in `internal/config/provider.go`
   - config and startup materialization in `internal/config/resolve.go`
     and `cmd/gc/template_resolve.go`
   - transport/runtime behavior in `internal/runtime`
   - transcript discovery and normalization in `internal/sessionlog`
4. Transcript handling is tier-1 product functionality, not just a
   debugging aid. Session creation, human-agent chat, ongoing parsing,
   compaction handling, and restart continuity all depend on it.
5. We cannot answer “is Codex tier-1?” or “what are Gemini’s remaining
   gaps?” with a stable, machine-readable support matrix.
6. Live failures today often mix together product bugs, provider drift,
   environment drift, and missing evidence.

### Why `runtime.Provider` is not enough

`runtime.Provider` remains important, but it is the transport substrate,
not the product-level worker contract. Gas City needs a stricter
boundary above it for fully managed interactive coding-agent workers.

Examples of behaviors that belong to the worker contract rather than the
runtime substrate:

- config-to-worker translation
- bounded bring-up and safe initial input delivery
- continuation into the same logical conversation
- transcript discovery and canonical normalization
- required user interactions
- worker-level tool-use substrate and observability

## Goals

- Define a canonical, transport-agnostic worker contract for tier-1
  interactive providers.
- Make the contract executable first, then extract the production API
  from what the tests prove.
- Certify concrete worker profiles, not vague provider families.
- Make transcript and session-history correctness a first-class part of
  the worker contract.
- Provide a deterministic required CI gate for non-inference worker
  behaviors.
- Provide an aggressive nightly live certification suite for real
  provider behavior.
- Produce a machine-readable support matrix plus human-readable CI
  summaries and evidence bundles.
- Ground fake-worker profiles in upstream provider reality, not only in
  Gas City’s current assumptions.

## Non-goals

- Full controller or reconciler certification in v1 beyond a thin E2E
  smoke layer.
- A bespoke dashboard or web UI for conformance reporting.
- Immediate migration of every production call site to the new worker
  boundary.
- Certification of every provider and backend in the repository.
- Making live inference certification a blocking PR gate in v1.
- Automatically certifying arbitrary city-specific derived profiles.

## Design Principles

1. **Behavioral contract first.**
   The canonical worker contract is defined by observable behavior, not
   by mirroring today’s internal method calls.
2. **Transport-agnostic worker core.**
   `claude/tmux-cli` and a future `claude/sdk-acp` should implement the
   same core worker contract.
3. **Transcript is product surface.**
   Transcript and session-history behavior are not “debug extras.”
4. **Executable spec before production extraction.**
   The conformance system defines the contract first; the future
   production `internal/worker` API is extracted from that proven shape.
5. **Strict required core, truthful red.**
   Initial failures are expected and useful.
6. **One shared contract vocabulary.**
   Deterministic doubles, live nightly certification, and E2E smoke
   reuse the same requirement codes and scenario model.
7. **JSON-first artifacts.**
   Machine-readable results are canonical; human summaries are derived.

## Proposed Design

### 1) Worker contract layers

The worker program is defined in layers:

1. **Runtime substrate**
   - Existing `runtime.Provider`
   - Broad transport/session lifecycle substrate
   - May support many backends that are not tier-1 workers
2. **WorkerCore**
   - Required conformance contract for tier-1 interactive workers
   - Deterministic, hermetic, non-inference
   - Required in PR CI
3. **WorkerInference**
   - Live nightly certification against real providers
   - Aggressive multi-turn and tool-using workflows
4. **Thin E2E worker smoke**
   - Reuses the same contract vocabulary
   - Verifies that `gc` and controller/session wiring can drive the same
     worker behavior end to end

`runtime.Provider` remains the substrate. The future canonical Worker
API is a stricter layer above it.

#### 1.1 Single-writer migration rule

At every migration phase, each persisted field and each semantic
decision has exactly one authoritative writer. `internal/worker` may
compute, normalize, and classify behavior before it becomes the
production source of truth for built-in profiles, but it may not become
an untracked second writer alongside `runtime`, `config`, or
`internal/session`.

#### 1.2 Responsibility matrix

| Concern | Phase 1-3 authoritative owner | Phase 4+ authoritative owner | Notes |
|---|---|---|---|
| Transport process/session lifecycle | `internal/runtime` | `internal/runtime` | Worker requirements depend on substrate behavior but do not duplicate pane/process/container control. |
| Launch materialization from city config | `internal/config` plus template/session materialization | `internal/worker` built-in profile plus `internal/config` override materialization | Until Phase 4, `WorkerClaims` is derived from materialized config and runtime metadata; it is not a second hand-authored source. |
| Worker semantic contract, claims, requirement catalog, transcript contract | `internal/worker` | `internal/worker` | New canonical behavioral boundary. |
| Session/bead persistence and controller reconciliation state | `internal/session` plus `cmd/gc/session_*` | `internal/session` plus `cmd/gc/session_*` | `internal/worker` returns observations and state transitions; it does not write bead/session metadata directly. |
| Transcript discovery and normalization | `internal/sessionlog` implementation wrapped by `internal/worker` contract | `internal/worker` | Code may migrate, but the contract becomes explicit immediately. |

#### 1.3 Identity layers

The design distinguishes the following identities explicitly:

| Identity layer | Meaning | Current primary owner | Long-term owner |
|---|---|---|---|
| GC session identity | Stable Gas City session row / `session_key` used by UI, controller, and persistence | `internal/session` | `internal/session` |
| GC continuation orchestration state | GC-local decision state such as `continuation_epoch` and `continuation_reset_pending` | `internal/session` plus `cmd/gc/session_*` | `internal/session` |
| Logical conversation identity | Canonical notion of “same conversation” across restarts | implicit today | `internal/worker` |
| Provider continuation handle | Provider-native resume token, session ID, SDK handle, or equivalent | provider launch/adapter path | `internal/worker` profile adapter |
| Runtime instance identity | Process/pane/container instance currently executing the worker | `internal/runtime` | `internal/runtime` |
| Transcript stream identity | Raw transcript file/stream generation or rotation instance | `internal/sessionlog` readers | `internal/worker` transcript adapter |

When these disagree, the contract resolves them intentionally:

- GC session identity anchors user-visible session ownership.
- Logical conversation identity decides continuation conformance.
- Provider continuation handles and transcript stream identity are
  evidence for the logical conversation, not substitutes for it.

#### 1.4 Relationship to `session.Manager`

`internal/worker` does not replace `session.Manager` in Phase 1.

Instead:

- `session.Manager` remains the persistence and orchestration facade
  over beads/session state
- `internal/worker` becomes the canonical place where worker behavior,
  transcript normalization, claims, and conformance semantics are
  defined
- Phase 1-3 production call sites should consume worker semantics
  through `session.Manager` or adjacent adapters rather than creating a
  parallel orchestration stack

The intended end state is composition, not dual authority:

- `internal/worker` defines the behavioral contract
- `session.Manager` persists and routes state transitions returned by
  that contract
- no new worker behavior may be introduced directly in
  `session.Manager`, `cmd/gc/session_*`, or `internal/sessionlog`
  without a corresponding `internal/worker` contract entry

#### 1.5 Concrete extraction target: Worker handle API

The intended end-state boundary is a production worker handle in
`internal/worker` that hides tmux panes, provider CLIs, supervisor
lifecycles, and transcript-file conventions from behavioral callers.

The canonical repository term remains **worker** for consistency with
this design, even if some human-facing surfaces continue to say
"agent."

Illustrative shape:

```go
type Handle interface {
	Start(ctx context.Context) error
	StartResolved(ctx context.Context, startCommand string, hints runtime.Config) error
	Attach(ctx context.Context) error
	Create(ctx context.Context, mode CreateMode) (session.Info, error)
	Stop(ctx context.Context) error
	Kill(ctx context.Context) error
	Close(ctx context.Context) error
	Rename(ctx context.Context, title string) error
	Peek(ctx context.Context, lines int) (string, error)

	State(ctx context.Context) (State, error)

	Message(ctx context.Context, req MessageRequest) (MessageResult, error)
	Interrupt(ctx context.Context, req InterruptRequest) error
	Nudge(ctx context.Context, req NudgeRequest) (NudgeResult, error)
	Transcript(ctx context.Context, req TranscriptRequest) (*TranscriptResult, error)
	TranscriptPath(ctx context.Context) (string, error)
	AgentMappings(ctx context.Context) ([]AgentMapping, error)
	AgentTranscript(ctx context.Context, agentID string) (*AgentTranscriptResult, error)

	History(ctx context.Context, req HistoryRequest) (*HistorySnapshot, error)

	Pending(ctx context.Context) (*PendingInteraction, error)
	PendingStatus(ctx context.Context) (*PendingInteraction, bool, error)
	Respond(ctx context.Context, req InteractionResponse) error
}
```

The exact method names may evolve during extraction, but the boundary
must preserve these semantics:

- `Start`
  - materializes and starts one worker instance for one worker profile
  - completes with a bounded startup outcome such as `ready`,
    `blocked`, or `failed`
- `State`
  - returns worker-level state such as `starting`, `ready`, `busy`,
    `blocked`, `stopping`, `stopped`, or `failed`
  - includes enough structured detail to explain blocked and failure
    states without scraping raw tmux panes in worker-level tests
- `Message`
  - is the canonical turn-submission operation
  - may internally use wait-idle, tmux nudge, provider SDK calls, or
    other transport-specific delivery mechanisms
  - does not expose those transport mechanics as worker-contract
    requirements
- `Interrupt`
  - is the canonical soft stop for in-flight work
  - proves the worker can stop the current turn without requiring the
    caller to manage raw key sequences
- `Nudge`
  - remains available as a best-effort wake or redirect primitive for
    callers that need it
  - is not the main behavioral turn API and must not leak tmux-only
    semantics into the worker contract
- `History`
  - exposes the canonical normalized transcript/history contract
  - hides transcript discovery, raw provider file formats, and
    generation bookkeeping behind `internal/worker`

Current branch API-to-test traceability:

- lifecycle materialization and identity (`Create`, `Start`, `StartResolved`,
  `Attach`, `State`, `Stop`, `Kill`, `Close`, `Rename`, `Peek`)
  - deterministic coverage lives in `internal/worker/handle_test.go`
  - top-level caller boundary coverage lives in
    `cmd/gc/worker_boundary_import_test.go` and
    `internal/api/worker_boundary_test.go`
- turn delivery (`Message`, `Interrupt`, `Nudge`)
  - deterministic worker coverage lives in `internal/worker/handle_test.go`
  - live shared-corpus behavioral coverage lives in
    `test/acceptance/worker_inference/worker_inference_test.go`
- transcript and subagent surfaces (`Transcript`, `TranscriptPath`,
  `AgentMappings`, `AgentTranscript`, `History`)
  - deterministic coverage lives in `internal/worker/handle_test.go`,
    `internal/worker/sessionlog_adapter_test.go`, and
    `internal/worker/factory_test.go`
  - live transcript/continuation coverage lives in
    `test/acceptance/worker_inference/worker_inference_test.go`
- interaction surfaces (`Pending`, `PendingStatus`, `Respond`)
  - deterministic coverage lives in `internal/worker/handle_test.go`
    and `internal/worker/workertest/phase2_conformance_test.go`
  - live blocked-state classification coverage lives in
    `test/acceptance/worker_inference/classification_test.go`

The worker handle is therefore above `runtime.Provider`.

`runtime.Provider` continues to own:

- process or pane creation
- raw interrupt and stop mechanics
- low-level input injection
- raw output peeking
- transport-specific capabilities and limits

The worker handle owns:

- worker-level startup outcomes and state
- behavioral turn submission
- continuation semantics
- transcript/history semantics
- required interaction semantics
- worker-level incident and blocked-state classification

#### 1.6 Test-layer rule for the canonical worker API

Once the worker handle exists, worker conformance must target that
boundary directly rather than asserting worker behavior through
transport and CLI internals.

The layering rule is:

- `internal/runtime/*` conformance proves substrate mechanics such as
  tmux/session startup, key delivery, idle waiting, interrupt signals,
  and other transport capabilities
- `internal/worker/*` conformance proves worker semantics such as
  startup state, message delivery, continuation, interrupt/recover,
  transcript/history behavior, and structured interactions
- thin `gc` and supervisor E2E smoke proves that config materialization
  and orchestration can drive the same worker semantics end to end

Worker-level conformance may use repo-owned setup adapters and worker
factories, but it must not directly depend on:

- shelling `gc start` / `gc stop` for core behavioral assertions
- supervisor process management details
- tmux pane scraping or tmux-session existence checks
- provider transcript file-path discovery
- raw CLI key sequences such as `C-c`, `C-u`, or dialog-dismiss keys

Those remain valid evidence and test surfaces lower in the stack, but
they are not the canonical worker boundary.

#### 1.7 Phase 4 extraction rule

Phase 4 promotion means more than moving profile definitions under
`internal/worker`. It also requires that the shared conformance corpus
drive the real worker handle rather than a mix of:

- `gc` CLI orchestration
- supervisor lifecycle helpers
- direct `runtime.Provider` calls
- transport-specific evidence probes

The desired post-cutover shape is:

- worker tests call the worker handle
- runtime tests call the runtime substrate
- E2E tests call `gc`

No single suite should be the authority for all three layers at once.

### 2) WorkerCore requirement groups

`WorkerCore` contains two required sublayers:

1. **Worker behavioral requirements**
   - the user-visible behavior Gas City depends on
2. **Worker adapter/materialization requirements**
   - the config-to-worker translation, env/auth/workspace/hook staging,
     and other launch semantics required to produce that behavior

This keeps materialization inside the contract without collapsing it
into runtime transport mechanics.

`WorkerCore` is organized into the following requirement groups:

1. **Startup Materialization**
   - provider preset resolution
   - startup config materialization
   - env/auth/config propagation
   - hook staging/install expectations
   - workspace preparation
2. **Worker Bring-Up**
   - bounded startup outcome: `ready`, `blocked`, or `failed`
   - safe point for initial input delivery
   - clean surfacing of startup failures
   - each profile declares a conformance-testable startup bound;
     default expectation is 60s unless a stricter bound is declared
3. **Input Delivery**
   - initial task input is delivered exactly once and processed
   - follow-up input reaches the intended worker
   - delivery mechanism is not part of the contract
4. **Session Identity And Continuation**
   - fresh start creates a new conversation
   - continuation reopens the same logical conversation after restart
   - implementation mechanism is provider-specific and not prescribed
5. **Transcript And Session History**
   - transcript discovery
   - canonical normalization
   - incremental updates
   - compaction handling
   - torn/partial tail resilience
   - restart/resume continuity
6. **Required User Interactions**
   - if a worker can enter user-required interaction states, it must
     surface them through the structured interactions API
   - otherwise it is not tier-1 conformant
7. **Control And Recovery**
   - interrupt
   - stop
   - blocked-state handling
   - crash and restart recovery
8. **Operational Observability**
   - peek/raw operational output
   - enough surfaced state to explain startup, blocked, and failure
     conditions

#### 2.1 Runtime substrate versus worker contract

The existing runtime substrate remains authoritative for these
mechanisms:

| Existing substrate surface | Stays in `runtime` | Worker contract depends on it for |
|---|---|---|
| `Start` / launch mechanics | yes | bring-up, continuation, startup delivery |
| `Stop` / `Interrupt` | yes | control and recovery |
| `Peek` / raw operational output | yes | operational observability |
| optional idle/nudge/wait capabilities | yes | bounded startup and safe input delivery |
| optional structured interaction transport | yes | required user interactions |

`WorkerCore` never requires a transport to become tmux-like. Instead,
it defines the behavioral outcomes that any transport-backed profile
must satisfy using whatever substrate mechanisms are appropriate for
that transport.

#### 2.2 Transport-agnostic proof rule

V1 certification profiles are `*/tmux-cli`, but the contract itself may
not encode tmux-only assumptions such as ANSI scraping boundaries,
terminal geometry, or pane-specific naming rules.

To keep that honest:

- no requirement code may depend on tmux-only artifacts
- provider-specific overrides may adjust evidence extraction, not the
  meaning of the requirement
- Phase 2 adds at least one non-certifying alternate-transport proof
  profile, such as an ACP-backed or adapter-only fake profile, to catch
  contract overfitting before Phase 4 promotion

### 3) Continuation is a behavioral guarantee

`WorkerCore` requires continuation semantics, not a specific resume
mechanism.

The required guarantee is:

- fresh start creates a new logical conversation
- continuation start reopens the prior logical conversation
- the proof is behavioral and historical, not just flag-based

The canonical continuation oracle requires both:

1. **History continuity**
   - the normalized transcript/history remains continuous across the
     restart boundary
2. **Behavioral continuity**
   - a post-restart turn that depends on prior context proves the worker
     is still in the same conversation

The oracle also requires two anti-false-positive checks:

- **fresh-session negative control**
  - the same scenario must prove that a fresh session with the same
    workspace does not satisfy the remembered context
- **provider-native evidence when available**
  - if the provider exposes a session or continuation identifier, the
    harness must record and compare it as supporting evidence

Replayed prompts, copied workspace state, or transcript reseeding alone
do not satisfy continuation.

Provider-native mechanisms may vary:

- caller-supplied session ID
- provider-native resume token
- SDK session handle
- persisted local state

The contract does not care how continuation is implemented. It cares
that the same conversation is preserved.

The requirement catalog must tag continuation requirements explicitly:

- history continuity checks are `deterministic` and usually
  `live_certifiable`
- behavioral continuation proofs are `both`: deterministic against fake
  workers and live against real providers
- tier-1 promotion requires the live behavioral continuation proof to
  be green, not only the deterministic half

The first slice also includes a thin GC continuation smoke scenario
that drives the current wake/reset path using the same `WC-CONT-*`
codes. This does not turn controller certification into the main suite;
it prevents the first slice from certifying a fake-worker restart path
while the production `continuation_epoch` / reset flow is broken.

### 4) Canonical transcript and session-history contract

Transcript handling becomes a first-class worker contract.

#### 4.1 Canonical normalized history model

This work explicitly formalizes a canonical normalized history model
that today is implicit in `internal/sessionlog`.

The model must support:

- stable session identity
- explicit logical conversation identity separate from runtime instance
  and transcript stream identity
- ordered normalized entries
- actor roles
- message content and blocks
- tool calls and tool results
- interaction events where applicable
- continuity markers across compaction and restart
- partial/incomplete entry handling
- provider-specific structured extensions

The model is **core plus extensions**, not a lossy lowest-common
denominator message list.

Each normalized history snapshot must carry:

- `gc_session_id`
- `logical_conversation_id`
- `transcript_stream_id`
- `generation`
- `cursor`
- `continuity`
- `tail_state`
- `entries[]`

Each normalized entry must distinguish:

- **core semantic fields**
  - normalized entry ID
  - kind
  - actor/role
  - ordering key
  - timestamps when available
  - content or tool/interaction payload
  - raw provenance pointer
  - lifecycle state such as `final`, `partial`, `superseded`, or
    `unknown`
- **synthetic/derived fields**
  - deterministic adapter-generated fields used to normalize providers
    that lack native structure
  - these must be marked as derived, not treated as provider-native fact
- **provider-specific extensions**
  - provider-native IDs, event types, metadata, and richer structures
    that Gas City may need for UX, support, or future features

The contract must explicitly name which core fields are semantically
meaningful for every provider and which are derived conveniences added
by the adapter.

#### 4.2 Provider-specific raw adapters

The canonical contract is the normalized history model. Raw transcript
formats remain provider-specific.

Each worker profile supplies:

- transcript discovery conventions
- provider-native raw transcript fixtures
- provider-specific normalization adapters
- provider-specific extension preservation rules

This preserves real differences in raw transcript layout while making
the normalized history contract shared and testable.

Provider-specific extension preservation is not optional hand-waving.
At minimum, adapters must preserve when available:

- provider-native conversation or session identifiers
- raw record identifiers or offsets
- provider-native tool and interaction subtype labels
- compaction or rewrite markers
- timestamps and ordering hints

Transcript discovery must bind to the intended workspace and logical
session. Ambiguous matches or cross-workspace attachment are explicit
conformance failures, not best-effort behavior.

#### 4.3 Snapshot, incremental, and generation semantics

The contract defines one canonical read model:

- a **snapshot** is the full normalized history view for one logical
  conversation at one `generation`
- an **incremental update** is an ordered delta from a prior `cursor`
  within the same `generation`
- a **generation reset** occurs when append-only assumptions are broken,
  for example by rewrite, rotation, truncation, or compaction that
  invalidates prior cursors

Required invariants:

- repeated reads against unchanged raw state are idempotent
- cursors are opaque but stable within a generation
- if a prior cursor is invalidated, the adapter must surface a
  generation reset explicitly rather than silently replaying or dropping
  history
- evidence bundles must record cursor and generation state so failures
  can be diagnosed

#### 4.4 Compaction, rewrite, and torn-tail semantics

Compaction and torn tails are separate contract cases.

For compaction and rewrite:

- append-style compaction may preserve the same generation if prior
  cursor semantics remain valid
- rewrite, rotation, or replacement that invalidates prior cursors must
  advance the generation and emit explicit continuity metadata
- silent heuristic parent substitution is not sufficient for
  conformance; if continuity cannot be proven, the adapter must emit an
  explicit degraded-history state

For torn or partial tails:

- malformed or partial raw records must never corrupt prior finalized
  normalized history
- a partial record may be withheld or surfaced as `partial`, but not as
  a finalized entry
- replacement or finalization of a partial record must be deterministic
  and idempotent across repeated reads
- unknown or malformed provider records must either become `unknown`
  extension-bearing entries or explicit degradation events; they may not
  disappear silently if doing so would change continuity semantics

#### 4.5 Transcript lifecycle coverage

Transcript conformance includes:

- initial transcript creation
- incremental parsing of new entries
- compaction/rewrite handling
- torn or partial tail handling
- restart/resume continuity
- malformed or unknown entry degradation

Coverage is split into:

1. **Deterministic parser/fixture conformance**
   - exhaustive provider-native fixtures
   - compaction and malformed-edge coverage
2. **Live transcript certification**
   - transcript is actually produced by a real worker
   - discovery and incremental updates work in practice

Live certification does not need to force every pathological rewrite in
nightly. Deterministic fixtures are the primary proof for compaction,
rotation, malformed tails, and duplicate replay edges; live nightly
proves discovery, creation, and incremental behavior against real
providers.

### 5) Structured required interactions

Tier-1 workers may not have opaque user-required interactions.

Rule:

- if the worker can enter a state that requires user input or approval,
  it must support the structured interactions API
- otherwise the profile is not tier-1 conformant

This applies to:

- startup dialogs
- permission prompts
- trust/onboarding prompts
- tool-use approvals
- other user-required interaction states

Workers that truly never require user interaction do not need that
capability.

#### 5.1 User-visible interaction contract

Structured interactions are not only a side API. For tier-1 workers:

- required interactions must become durable, resumable session-history
  records with lifecycle states such as `opened`, `pending`,
  `resolved`, `dismissed`, and `resumed_after_restart`
- a blocked turn must become visible to the caller within a declared
  bounded window
- the system must distinguish `blocked`, `failed`, `ready`, resumed,
  and interrupted outcomes in a way the caller can render without
  guessing

Live certification must never silently certify opaque user-required
states. If GC cannot turn an interaction into an actionable structured
event, the profile fails tier-1 criteria.

### 6) Worker profiles and claims

The conformance unit is a **worker profile**, not a provider family.

Examples:

- `claude/tmux-cli`
- `codex/tmux-cli`
- `gemini/tmux-cli`

The profile identity includes:

- provider family
- transport/runtime class
- worker behavior/claims version
- transcript adapter version

Version surfaces are classified deliberately:

- **profile compatibility version**
  - provider family + transport class + worker behavior/claims version +
    transcript adapter version
- **catalog version**
  - shared conformance-contract version used by the suite
- **internal implementation revisions**
  - fake-worker engine revision, harness revision, and fixture capture
    revisions that are not independently user-facing compatibility
    signals

#### 6.1 Claims model

Each profile exposes explicit `WorkerClaims`.

Claims are a hybrid of:

- **derived claims**
  - resume capability
  - prompt delivery mode
  - hook support
  - transport class
  - other directly derivable behavior
- **explicit semantic claims**
  - structured interaction support
  - transcript discovery semantics
  - startup dialog handling semantics
  - other composite behaviors

The claims surface becomes the authoritative classification layer for:

- required vs optional
- claimed vs unsupported
- live-certifiable vs deterministic-only

Authority is phase-specific and single-sourced:

- **Phase 1-3**
  - `config.ProviderSpec` plus runtime metadata remain the launch-time
    source of truth
  - `WorkerClaims` is a derived, machine-generated view used by the
    conformance system
  - hand-authoring `ProviderSpec` semantics and `WorkerClaims`
    independently is forbidden
- **Phase 4+**
  - built-in worker profiles under `internal/worker` become the semantic
    source of truth
  - `config.ProviderSpec` becomes override/materialization input
  - `ResolvedProvider` becomes a materialized launch view, not the
    canonical behavioral definition

#### 6.2 Upstream grounding

Profile claims and fake-worker behavior are grounded in upstream
provider reality:

- Codex and Gemini should be informed by their open-source CLIs
- Claude should be informed by observed live behavior and existing GC
  integration knowledge

When current GC assumptions differ from provider reality, the profile
should reflect the provider and the suite should go red until GC adapts.

#### 6.3 Upstream versioning and drift loop

Each profile version must record the upstream reality it was certified
against, including when applicable:

- upstream CLI or SDK version or version range
- transcript fixture capture set version
- fake profile version
- catalog version

The claims model must also distinguish:

- `supported_upstream`
- `unsupported_upstream`
- `unknown_upstream`

`unknown_upstream` is not treated as `unsupported`. Phase 1 profile
authoring must record an upstream investigation note before a capability
is marked unsupported.

Nightly live failures must be triaged into one of:

- GC bug against unchanged upstream behavior
- fake profile or fixture drift
- upstream provider behavior change requiring a new profile version or
  fixture refresh
- provider/environment incident

Confirmed upstream drift must feed back into the deterministic suite by
updating fake profiles and transcript fixtures rather than leaving
`WorkerCore` frozen on stale assumptions.

Every new or revised canonical profile must start with an upstream
grounding record containing:

- upstream CLI or SDK version inspected
- command/help/changelog evidence used
- transcript evidence or live capture used
- any known GC divergence from upstream reality

This is where known partial integrations must be recorded. For example,
if upstream provider behavior exists but GC has not wired it cleanly
yet, the profile should classify that as `supported_upstream but
failing_in_gc`, not `unsupported`.

### 7) Certification semantics

Tier-1 is an explicit certification state attached to a worker profile.

Suggested status progression:

- `experimental`
- `core-conformant`
- `tier-1-candidate`
- `tier-1-certified`

These states are not only CI labels. They carry support posture:

| State | Intended posture |
|---|---|
| `experimental` | opt-in only, not recommendable by default, support best-effort |
| `core-conformant` | deterministic core is green, but live behavior may still be incomplete or unstable |
| `tier-1-candidate` | close to supportable, not yet default-selectable until live stability is proven |
| `tier-1-certified` | default-eligible and recommendable subject to product policy |

#### 7.1 Tier-1 bar

A profile becomes `tier-1-certified` only when all of the following are
true:

- `WorkerCore` required suite: 100% pass
- no `unsupported` on required core requirements
- all live-certifiable required core requirements are green for a
  stability window, initially 7 consecutive nightlies
- critical `WorkerInference` workflows are green
- no blocker-class known gaps remain in:
  - continuation
  - transcript correctness
  - required interactions
  - startup/input delivery
  - tool-use flow

#### 7.2 Certified built-ins vs derived profiles

Certification attaches to canonical built-in profiles, not arbitrary
city overrides.

If a city overrides a certified built-in profile:

- the result is a derived profile
- certification is not inherited automatically

V1 uses a conservative certification-preserving override set. Only a
small, explicitly defined set of behavior-preserving overrides may keep
the certified identity.

### 8) Conformance catalog and requirement codes

Every requirement gets a stable code, for example:

- `WC-BRINGUP-001`
- `WC-CONT-002`
- `WC-TX-004`
- `WC-INT-003`

The catalog is:

- JSON-first
- top-level versioned
- shared across deterministic, live, and E2E layers

Each requirement carries:

- requirement code
- group/category
- responsibility-domain tags
- hard vs soft observation classification
- live-certifiability metadata
- product requirement classification
- upstream/provider reality classification
- preconditions
- actions
- oracle definition
- accepted evidence sources
- pass/fail/unsupported algorithm
- retry policy
- claim-gating rules

This allows the report to distinguish:

- `required_by_gc but missing_upstream`
- `supported_upstream but failing_in_gc`
- `optional_in_gc and available_upstream`

Requirement codes are only meaningful if they are normative. A code is
not just a label; it is the executable oracle definition reused across
deterministic, live, and E2E runs.

### 9) Scenario DSL and harness

The conformance program uses a reusable scenario/state-machine model.

#### 9.1 DSL shape

The scenario catalog is:

- data-first
- JSON as the canonical format
- explicit actions plus expected observations
- provider-specific expectation overrides allowed
- thin Go escape hatch for rare custom assertions

The JSON catalog is declarative, not a general-purpose programming
language. If a case needs arbitrary branching or provider-specific code
to express the common path, that is a harness-design bug and should be
fixed by adding a reusable primitive rather than by embedding logic in
the scenario file.

The DSL supports:

- multi-phase scenarios across process lifetimes
- scenario state persistence across phases
- hard assertions vs soft observations
- requirement-code reuse across providers and test tiers
- bounded waits and quiescence semantics
- negative assertions
- correlation keys across phases and artifacts

Example scenario shape:

- `start_fresh`
- observe `startup_outcome=ready`
- `send_input`
- observe transcript/history growth
- `crash_worker`
- `continue_session`
- observe `same_logical_conversation=true`

Provider-specific overrides are intentionally narrow. They may change:

- provider-native evidence extractors
- raw transcript path/schema bindings
- provider-native event labels or subtype mappings
- bounded timing tolerances where the oracle already permits a range

They may not change:

- requirement meaning
- success or failure conditions
- continuation oracle semantics
- transcript continuity semantics

The thin Go escape hatch is only for adding new reusable primitives or
oracles. It is not a license for per-profile bespoke test logic.

#### 9.2 Harness API

The harness API is worker-behavior shaped rather than internal-API
shaped.

The main harness supports behaviors such as:

- create fresh session
- continue existing session
- deliver initial and follow-up input
- wait for bounded startup outcome
- inspect structured interactions
- fetch normalized transcript snapshot/incremental updates
- interrupt or stop
- collect evidence artifacts

The harness owns the canonical `same logical conversation` oracle. The
top-level pass condition is a conjunction of:

- normalized history continuity
- provider-native continuation evidence when available
- deterministic or live behavioral proof, depending on the tier
- fresh-session negative control

### 9.3 First-slice DSL requirements

Before Phase 1 is considered complete, the first transcript and
continuation slice must be expressible in the data-first catalog
without per-provider custom code for the main `WC-TX-*` and
`WC-CONT-*` cases. If a scenario cannot be expressed without bespoke Go
for the common path, that is a design failure in the harness, not an
excuse to bypass the DSL.

### 10) Deterministic fake workers

Required `WorkerCore` CI uses launched hermetic worker doubles, not
generic in-process mocks.

#### 10.1 Fake worker engine

V1 uses one generic programmable fake worker engine:

- standalone helper executable
- Go source in-repo
- built on demand during tests
- deterministic out-of-band control channel for barriers, faults, and
  precise state transitions

Provider flavor is encoded declaratively on top of the shared engine.

The fake engine is intentionally narrow. It exists to simulate contract
relevant behavior, not to emulate full provider internals or model
quality. It must not grow into a second product implementation.

The control channel may be a Unix socket, named pipe, or equivalent
test-only mechanism. Its purpose is to let the harness deterministically
trigger events such as:

- crash after partial transcript write
- delay transcript creation until after startup
- rewrite or rotate transcript files
- require a structured interaction before tool completion
- duplicate or suppress delivery at specific points for negative tests

#### 10.2 Provider-shaped profiles

Each fake profile supplies provider-specific behavior via:

- claims/spec data
- argv/env rules
- transcript layout/schema
- continuation token/session behavior
- structured interaction behavior
- tool-event behavior

The engine must support scripted:

- startup outcomes
- input delivery and consumption
- same-conversation continuation
- transcript creation/update/compaction
- torn tails and malformed transcript cases
- structured interactions
- tool events and file mutations
- crash/restart boundaries
- delayed transcript creation
- rewrite/rename compaction
- partial writes and torn tails
- duplicate-delivery and lost-delivery negative cases
- ambiguous transcript discovery

#### 10.3 Real transport path

Fake workers are launched through the real worker path, including the
real transport/runtime layer for the profile, so the required suite
still exercises:

- command construction
- env propagation
- transcript discovery
- startup behavior
- continuation wiring

Phase 1 also keeps a lightweight non-tmux substrate proof path using an
in-process or fake runtime-backed harness run for the shared `WC-TX-*`
and `WC-CONT-*` scenarios. The real tmux path remains the main required
path for canonical profiles, while the lightweight path exists to catch
tmux-specific leakage in the supposedly transport-agnostic contract.

### 10.4 WorkerCore determinism contract

Required deterministic conformance must remain hermetic and repeatable.

Rules:

- no real provider or network dependency
- fixed fixture workspace and fake-worker seeds
- stable transcript ordering rules
- bounded polling and explicit quiescence windows
- explicit timestamps and execution traces in evidence bundles
- failures in harness setup are ordinary failures, not incidents

### 11) WorkerInference nightly certification

`WorkerInference` runs against real providers and is intentionally
aggressive.

#### 11.1 Shared corpus

Nightly live certification uses:

- one provider-neutral shared core corpus
- provider-specific supplemental cases only where necessary

The shared corpus includes:

- one-shot response
- tool-using task
- multi-turn workflow
- explicit continuation proof task after restart
- interrupt/recover/continue behavior

Tasks should prefer machine-checkable success criteria:

- file or workspace mutations
- transcript/history evidence
- tool-result evidence
- structured interaction evidence

LLM/judge scoring is allowed only as a secondary fallback.

#### 11.2 Live environment setup

Each live profile uses a provider setup adapter that owns:

- install/setup
- auth staging
- isolated config home/XDG state
- transcript path isolation
- normalized harness configuration

Setup adapters live in repo-owned code/scripts and are invoked through
make targets, not embedded ad hoc in workflow YAML.

Credential isolation requirements are strict:

- per-profile and per-job isolated config/XDG/auth state
- no shared writable credential home across profiles or runs
- no artifact upload of auth material, refresh tokens, or provider
  config homes
- explicit cleanup of staged credentials and transcript roots at job end
- live certification must not auto-approve privileged or destructive
  interactions; only explicitly allow-listed low-risk setup actions may
  be automated

#### 11.3 Nightly result classification

Nightly live runs distinguish:

- `pass`
- `fail`
- `unsupported`
- `not_certifiable_live`
- `provider_incident`
- `environment_error`
- `flaky_live`

Nightly includes:

- a small bounded retry policy for clearly transient failures
- explicit tracking of both initial failure and post-retry outcome

V1 retry and flake policy:

- at most one immediate retry for setup or transport-class transient
  failures
- at most one delayed retry for clear rate-limit or capacity incidents
- retry-pass results are recorded as `flaky_live`
- `flaky_live` results do not count toward certification stability
  windows
- `provider_incident` or `environment_error` requires explicit evidence
  of where the failure happened: setup, auth, provider transport, or
  contract execution

Classification should fail closed:

- use `fail` when the worker reached enough of the contract surface to
  evaluate the requirement and then violated it
- use `environment_error` when repo, setup, auth staging, or harness
  prerequisites never became valid
- use `provider_incident` only when setup succeeded and the failure is
  attributable to provider transport/capacity/service behavior rather
  than GC logic

### 12) CI, reporting, and artifacts

The conformance system lands as first-class CI jobs and make targets.

#### 12.1 Make targets

V1 should add dedicated targets such as:

- `make test-worker-core PROFILE=claude`
- `make test-worker-core PROFILE=codex`
- `make test-worker-core PROFILE=gemini`
- `make test-worker-inference PROFILE=claude`
- `make test-worker-inference PROFILE=codex`
- `make test-worker-inference PROFILE=gemini`
- `make test-worker-e2e-smoke PROFILE=...`

#### 12.2 Required PR CI

Required deterministic CI is:

- path-filtered initially
- per-profile
- required as soon as the first trustworthy slice lands

Initial required jobs:

- `worker-core-claude`
- `worker-core-codex`
- `worker-core-gemini`

The path filter should be broad across worker-affecting areas:

- `internal/worker/**`
- `internal/runtime/**`
- `internal/sessionlog/**`
- `internal/config/**`
- `cmd/gc/template_resolve*.go`
- `cmd/gc/session_*`
- relevant transcript/session APIs
- `.github/workflows/**`
- `Makefile`
- test harness and worker setup scripts

This is intentionally a broad filter. If CI or shared tooling changes
could plausibly affect worker behavior, the worker-core jobs should run.

Deterministic `worker-core` fails hard on any harness or environment
problem.

#### 12.3 Nightly CI

Nightly live certification is:

- per-profile
- separate from required PR CI
- scheduled and manually runnable

Suggested jobs:

- `worker-inference-claude`
- `worker-inference-codex`
- `worker-inference-gemini`

Each has its own artifacts, and one aggregation job rolls them up.

#### 12.4 Evidence and summaries

Every failing run emits a standardized evidence bundle by default.

Expected artifact contents include:

- JSON result report
- claims snapshot
- startup/materialization summary
- scenario execution trace
- oracle evaluation trace
- harness/profile/catalog version snapshot
- timestamps and durations
- normalized transcript snapshot
- raw provider transcript fragments
- structured interaction state
- runtime/peek output
- workspace artifacts

Green runs still retain compact result artifacts for history and trend
analysis.

Human-readable support summaries are emitted in CI job summaries, while
JSON reports and evidence bundles remain the source of truth in CI
artifacts.

Evidence retention must be operationally bounded and safe:

- failure bundles retain the full requirement-scoped evidence set
- green runs retain compact summary artifacts
- artifact upload must redact secrets, auth material, and unrelated
  workspace data by default
- transcript fragments in artifacts should be scoped to the failing
  requirement where possible rather than uploading unbounded raw logs
- green artifacts should stay compact, on the order of summary JSON plus
  small metadata snapshots rather than raw workspaces
- failure artifacts should be size-capped per profile and retained for a
  shorter bounded window than long-lived summary reports

V1 target budgets:

- green artifact target: <= 1 MB per profile run
- failure artifact target: <= 25 MB per failing profile run
- required PR retention target: 14 days
- nightly retention target for compact summaries: 30 days

Aggregation jobs should also compute baseline deltas:

- newly passing requirements
- newly failing requirements
- changed support classifications

#### 12.5 Soft observations

Nightly should also capture soft, non-gating observations such as:

- startup latency
- first-response latency
- continuation latency
- usage/cost telemetry capture when available

### 13) Human support matrix

The primary human-facing output in v1 is the CI summary and artifact
rollup, not a dedicated dashboard.

Rollups should answer:

- which profiles are green or red
- which critical requirements are failing
- whether the change is regression, improvement, incident, or flake
- what the latest certification state is for each profile

### 14) Repo layout direction

This design introduces a new long-term home for the worker contract.

Suggested direction:

- `internal/worker/`
  - worker profiles and claims
  - normalized transcript/history contract
  - structured interaction contract
  - requirement catalog
  - scenario DSL and harness interfaces
- `internal/worker/fake/` or similar
  - generic fake worker engine
  - declarative provider-specific fake profiles and fixtures
- `test/integration/workerconformance/`
  - thin E2E smoke coverage using the same requirement catalog

`runtime` remains the transport substrate. `config` becomes an
override/materialization layer. `sessionlog` is gradually absorbed into
the explicit worker transcript contract.

During Phases 1-3, `internal/worker` should wrap the existing
`internal/sessionlog` implementation for production reads rather than
forking transcript normalization immediately. The conformance suite may
define the canonical model earlier, but production transcript parsing
must have one implementation path at a time. Parallel transcript
normalizers are explicitly out of bounds before the Phase 4 cutover.

Similarly, WorkerCore startup-materialization coverage in Phases 1-3
must exercise the existing `template_resolve` and provider-resolution
path rather than bypassing it with prebuilt runtime configs. That code
remains the current authority until the Phase 4 cutover.

## Phased Migration Plan

### Phase 1: transcript/history and continuation backbone

Goal: land the first required deterministic slice across all three
canonical profiles.

Deliverables:

- new `internal/worker` package skeleton
- explicit normalized transcript/history contract
- top-level conformance catalog and requirement codes
- deterministic transcript fixture conformance
- fake worker engine scaffold
- fake profile behavior for `claude/tmux-cli`, `codex/tmux-cli`,
  `gemini/tmux-cli`
- required deterministic transcript + continuation suite
- dedicated `worker-core-*` CI jobs and artifacts
- thin GC continuation smoke covering current wake/reset state
- one alternate-transport proof profile to validate contract neutrality
- upstream grounding notes for the three canonical profiles

Done when:

- all `WC-TX-*` and `WC-CONT-*` required deterministic requirements are
  green for the three canonical fake-backed profiles
- the thin GC continuation smoke is green on required CI
- transcript normalization changes made in this phase land through
  `internal/worker` contract surfaces first
- no new transcript or continuation behavior is added outside
  `internal/worker` without matching catalog coverage

### Phase 2: startup, input delivery, interactions, tool substrate

Goal: expand `WorkerCore` to the rest of the required behavioral
surface.

Deliverables:

- startup materialization requirements
- bring-up requirements
- input delivery requirements
- structured required interactions requirements
- tool-event and tool-use substrate requirements
- richer negative-contract coverage

Done when:

- required `WC-START-*`, `WC-BRINGUP-*`, `WC-INPUT-*`, `WC-INT-*`, and
  core tool-substrate requirements are green for the canonical profiles
- the alternate-transport proof profile remains green
- blocked-turn and structured-interaction requirements are represented
  durably in the canonical transcript model

### Phase 3: live nightly WorkerInference

Goal: certify real provider behavior against the same shared contract.

Deliverables:

- provider setup adapters
- isolated nightly canary accounts/config homes
- provider-neutral shared live corpus
- provider-specific supplemental cases
- per-profile nightly jobs and aggregation
- incident, flake, and soft-observation reporting

Done when:

- nightly per-profile jobs run from repo-owned setup adapters
- `provider_incident`, `environment_error`, `fail`, and `flaky_live`
  classifications are emitted with evidence
- live continuation and transcript discovery proofs are wired to the
  same requirement codes as deterministic core

### Phase 4: promote canonical worker boundary

Goal: make `internal/worker` the real production source of truth.

Deliverables:

- introduce a concrete worker handle API in `internal/worker` for
  `Start` / `Stop` / `State` / `Message` / `Interrupt` / `History`
- move canonical built-in worker definitions under `internal/worker`
- treat `config.ProviderSpec` as user override/materialization layer
- make profile identity and certification fingerprint explicit in
  production code
- gradually adapt production callers toward the worker boundary
- move `template_resolve` provider semantics behind the worker profile
  materialization boundary
- make `internal/sessionlog` an implementation detail of
  `internal/worker` or remove it as an independent contract owner
- move the shared worker conformance corpus onto the worker handle
  rather than raw `gc` / supervisor / tmux orchestration
- reduce worker-level `gc` and supervisor coverage to thin E2E wiring
  proofs

Current branch progress:

- the shared worker corpus already runs through the concrete
  `internal/worker` handle
- canonical built-in profile definitions now live under
  `internal/worker/builtin`, with `config.ProviderSpec` materialized from
  that source rather than hand-authoring built-ins separately
- worker handles stamp explicit profile identity and certification
  fingerprint metadata into production session beads
- transcript discovery policy now routes through
  `internal/worker/transcript` for worker, API, and `gc session logs`
- API submit / background-nudge / stop-turn / respond paths now call the
  worker handle rather than invoking `session.Manager` semantics
  directly
- API session creation and configured named-session materialization now
  materialize sessions through a worker-owned creation seam rather than
  calling `session.Manager` lifecycle methods directly
- session transcript streaming now projects from worker-history reads
  through the concrete handle instead of parsing provider files directly
  in the production stream path
- API and `gc` callers no longer depend on concrete
  `*worker.SessionHandle` or `worker.SessionLogAdapter`; they consume the
  canonical `worker.Handle` / `worker.Factory` surfaces instead

Remaining branch-local Phase 4 gaps:

- reconciler and controller orchestration still own start/stop policy
  beneath the worker boundary

Done when:

- the shared worker corpus no longer shells `gc start` / `gc stop` or
  inspects tmux state directly for worker-semantic assertions
- built-in profile behavior is no longer defined outside
  `internal/worker`
- `config.ProviderSpec` no longer acts as the canonical definition of
  built-in worker semantics
- `sessionlog` no longer defines the normalized transcript contract as
  an independent authority
- worker-semantic tests no longer require transport-specific evidence
  gathering outside `internal/worker`
- new worker behavior outside `internal/worker` is forbidden by policy

### Phase 5: tier-1 certification and policy

Goal: turn conformance results into an explicit provider-support policy.

Deliverables:

- explicit certification states in reports
- policy for promotion to `tier-1-certified`
- optional expansion to additional profiles and providers

Done when:

- certification states appear in rollups with the support posture
  semantics defined in this document
- promotion and demotion policy is enforced from conformance data rather
  than manual judgment alone

## First Required Slice

The first required slice should be:

- transcript and session-history conformance
- continuation conformance

Why this slice first:

- it is one of the most fragile and provider-specific areas
- it directly affects user-facing human-agent chat behavior
- it forces the normalized history contract to become explicit early
- it produces immediate insight for Claude, Codex, and Gemini at once

The first slice should combine:

- provider-native transcript fixture conformance
- fake-worker-driven lifecycle and continuation scenarios

And it should run across all three canonical profiles from day one.

## Risks

1. **Scope sprawl**
   - mitigated by explicit non-goals and a phased migration plan
2. **Overfitting the fake workers to current GC assumptions**
   - mitigated by grounding profiles in upstream provider behavior
3. **Nightly noise**
   - mitigated by incident classification, bounded retries, and flake
     tracking
4. **Certification confusion**
   - mitigated by profile-level certification and conservative override
     preservation rules
5. **Never finishing the migration**
   - mitigated by making the migration path an explicit part of the
     design instead of an implied future cleanup

## Open Questions

No blocker-level open design questions remain for v1. Future work may
refine:

- the exact certification-preserving override fingerprint
- the exact catalog schema and scenario step vocabulary
- the stability-window duration for tier-1 promotion

Those are implementation-detail refinements, not unresolved direction.
