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

### 2) WorkerCore requirement groups

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

Provider-native mechanisms may vary:

- caller-supplied session ID
- provider-native resume token
- SDK session handle
- persisted local state

The contract does not care how continuation is implemented. It cares
that the same conversation is preserved.

### 4) Canonical transcript and session-history contract

Transcript handling becomes a first-class worker contract.

#### 4.1 Canonical normalized history model

This work explicitly formalizes a canonical normalized history model
that today is implicit in `internal/sessionlog`.

The model must support:

- stable session identity
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

#### 4.3 Transcript lifecycle coverage

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

#### 6.2 Upstream grounding

Profile claims and fake-worker behavior are grounded in upstream
provider reality:

- Codex and Gemini should be informed by their open-source CLIs
- Claude should be informed by observed live behavior and existing GC
  integration knowledge

When current GC assumptions differ from provider reality, the profile
should reflect the provider and the suite should go red until GC adapts.

### 7) Certification semantics

Tier-1 is an explicit certification state attached to a worker profile.

Suggested status progression:

- `experimental`
- `core-conformant`
- `tier-1-candidate`
- `tier-1-certified`

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

This allows the report to distinguish:

- `required_by_gc but missing_upstream`
- `supported_upstream but failing_in_gc`
- `optional_in_gc and available_upstream`

### 9) Scenario DSL and harness

The conformance program uses a reusable scenario/state-machine model.

#### 9.1 DSL shape

The scenario catalog is:

- data-first
- JSON as the canonical format
- explicit actions plus expected observations
- provider-specific expectation overrides allowed
- thin Go escape hatch for rare custom assertions

The DSL supports:

- multi-phase scenarios across process lifetimes
- scenario state persistence across phases
- hard assertions vs soft observations
- requirement-code reuse across providers and test tiers

Example scenario shape:

- `start_fresh`
- observe `startup_outcome=ready`
- `send_input`
- observe transcript/history growth
- `crash_worker`
- `continue_session`
- observe `same_logical_conversation=true`

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

### 10) Deterministic fake workers

Required `WorkerCore` CI uses launched hermetic worker doubles, not
generic in-process mocks.

#### 10.1 Fake worker engine

V1 uses one generic programmable fake worker engine:

- standalone helper executable
- Go source in-repo
- built on demand during tests

Provider flavor is encoded declaratively on top of the shared engine.

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

#### 10.3 Real transport path

Fake workers are launched through the real worker path, including the
real transport/runtime layer for the profile, so the required suite
still exercises:

- command construction
- env propagation
- transcript discovery
- startup behavior
- continuation wiring

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

### Phase 3: live nightly WorkerInference

Goal: certify real provider behavior against the same shared contract.

Deliverables:

- provider setup adapters
- isolated nightly canary accounts/config homes
- provider-neutral shared live corpus
- provider-specific supplemental cases
- per-profile nightly jobs and aggregation
- incident, flake, and soft-observation reporting

### Phase 4: promote canonical worker boundary

Goal: make `internal/worker` the real production source of truth.

Deliverables:

- move canonical built-in worker definitions under `internal/worker`
- treat `config.ProviderSpec` as user override/materialization layer
- make profile identity and certification fingerprint explicit in
  production code
- gradually adapt production callers toward the worker boundary

### Phase 5: tier-1 certification and policy

Goal: turn conformance results into an explicit provider-support policy.

Deliverables:

- explicit certification states in reports
- policy for promotion to `tier-1-certified`
- optional expansion to additional profiles and providers

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
