# Implementation Plan: Worker API Hardening

## Overview

This plan turns the post-`#835` Worker API follow-on branch into a deliberate
hardening effort aimed at `80+` across the agreed architecture and code-quality
criteria. The goal is not to widen the product surface again. The goal is to
make the merged Worker boundary materially easier to reason about, test, and
review by narrowing authorities, shrinking file-level blast radius, reducing
caller duplication, and making runtime/error behavior more explicit.

This plan is grounded in:

- the feature branch and PR that establish the Worker API cutover: `#835`
- the design context in
  [`engdocs/design/worker-conformance.md`](../design/worker-conformance.md)
- the session-model cleanup design in
  [`engdocs/design/session-model-unification.md`](../design/session-model-unification.md)
- the current branch hotspots, especially
  [`internal/worker/handle.go`](../../internal/worker/handle.go),
  [`internal/api/handler_session_create.go`](../../internal/api/handler_session_create.go),
  [`internal/api/handler_session_interaction.go`](../../internal/api/handler_session_interaction.go),
  [`internal/api/handler_session_transcript.go`](../../internal/api/handler_session_transcript.go),
  [`internal/api/handler_session_stream.go`](../../internal/api/handler_session_stream.go),
  [`internal/worker/workertest/phase2_conformance_test.go`](../../internal/worker/workertest/phase2_conformance_test.go),
  and [`cmd/gc/worker_handle.go`](../../cmd/gc/worker_handle.go)

## Baseline Scores

These are the branch scores that motivated this plan:

| Criterion | Baseline |
|---|---:|
| TDD | 78 |
| DRY | 59 |
| Separation of Concerns | 67 |
| Single Responsibility | 51 |
| Clear Abstractions | 75 |
| Low Coupling, High Cohesion | 64 |
| KISS | 56 |
| YAGNI | 62 |
| Prefer Non-Nullable | 71 |
| Prefer Async Notifications | 42 |
| Eliminate Race Conditions | 60 |
| Errors Are Not Optional | 68 |
| Idiomatic Project Layout | 74 |
| Write for Maintainability | 58 |

## Target State

The branch reaches `80+` on all criteria by making these structural changes:

- callers bind to small Worker capability interfaces instead of one broad
  authority surface
- API and CLI stop reconstructing session/runtime resolution on their own and
  instead depend on `internal/worker.Factory`
- Worker, API streaming, and conformance code are split into smaller modules
  with one clear responsibility each
- runtime/session state becomes more explicit and less stringly/nullable
- polling-heavy edges gain structured event reporting and narrower timing
  windows
- Worker failures emit consistent structured evidence instead of disappearing
  into caller-local logging or best-effort fallbacks
- architecture guardrails and review loops prevent drift after merge

## Architecture Decisions

- `internal/worker` remains the canonical authority for session-backed Worker
  handles and transcript/history access.
- Higher layers may adapt or decorate Worker construction, but they should not
  rebuild session/runtime resolution ad hoc once `Factory` owns a path.
- Refactors must land in vertical slices. Each slice needs failing proof first
  when behavior changes, passing targeted verification, broader regression
  checks, and a review loop before moving on.
- The follow-on hardening branch is a separate PR against the Worker feature
  branch, not a restack of the feature itself back onto `main`.
- The hardening work should preserve dogfoodability after every slice.

## Task List

### Task 1: Narrow Worker handle into capability interfaces

**Description:**
Split the broad `worker.Handle` contract into smaller capability interfaces and
update callers to depend on the minimum surface they actually need.

**Acceptance criteria:**
- [ ] Core Worker capabilities are grouped into smaller interfaces.
- [ ] Streaming and observation helpers depend on the smallest required
      capability surface.
- [ ] Existing Worker/API behavior remains unchanged.

**Verification:**
- [ ] `go test -count=1 ./internal/worker ./internal/api`
- [ ] `golangci-lint run --new ./internal/worker ./internal/api`

**Status:** Started on this branch.

### Task 2: Split Worker/API/conformance monolith files into cohesive modules

**Description:**
Break oversized Worker, API streaming, and conformance files into smaller files
organized by responsibility instead of by historical accumulation.

**Acceptance criteria:**
- [x] Session API handlers are split by concern across
      `handler_session_create.go`, `handler_session_interaction.go`,
      `handler_session_transcript.go`, and `handler_session_stream.go`.
- [ ] `internal/worker/workertest/phase2_conformance_test.go` is split into
      scenario-focused files with shared helpers.
- [ ] `internal/worker/handle.go` runtime/transcript/history/admin concerns are
      easier to locate and review.

**Verification:**
- [ ] `go test -count=1 ./internal/api ./internal/worker/...`
- [ ] `go test -count=1 ./internal/worker/workertest`

**Status:** Pending.

### Task 3: Move remaining Worker resolution glue behind `internal/worker.Factory`

**Description:**
Finish centralizing session/runtime resolution behind `Factory` so API and CLI
callers stop constructing session specs by hand.

**Acceptance criteria:**
- [ ] `cmd/gc` uses `Factory` helpers instead of reconstructing worker session
      specs locally.
- [ ] `internal/api` routes session-backed handle construction through
      `Factory.SessionByID`.
- [ ] Runtime-target fallback logic is shared instead of duplicated across
      call sites.

**Verification:**
- [ ] `go test -count=1 ./cmd/gc ./internal/api`
- [ ] `golangci-lint run --new ./cmd/gc ./internal/api ./internal/worker`

**Status:** In progress on this branch.

### Task 4: Add structured Worker operation events and reduce polling

**Description:**
Introduce structured Worker operation records for start, message, nudge,
interrupt, stop, and history flows, then use those records to narrow or remove
poll-driven state transitions where possible.

**Acceptance criteria:**
- [ ] Worker operations emit structured events with identity, timing, and
      outcome.
- [ ] Startup/interrupt edges stop depending purely on ad hoc poll windows.
- [ ] Failure evidence is available to callers without bespoke log scraping.

**Verification:**
- [ ] targeted Worker/session/API regressions for start, interrupt, and pending
      interaction flows
- [ ] broader package tests for `./internal/worker ./internal/session ./internal/api ./cmd/gc`

**Status:** Pending.

### Task 5: Tighten Worker data shapes and required-field construction

**Description:**
Push optional/nullable state to the edge. Make session/runtime construction and
Worker request shapes more explicit, with fewer ambient defaults and sentinel
empty strings.

**Acceptance criteria:**
- [ ] Worker session construction has clearer required-field rules.
- [ ] High-traffic request/result shapes stop relying on ad hoc empty-string
      conventions where explicit types are feasible.
- [ ] Callers consume typed resolution state instead of partially-populated
      structs.

**Verification:**
- [ ] targeted constructor and resolution tests
- [ ] package tests for `./internal/worker ./internal/api ./cmd/gc`

**Status:** Pending.

### Task 6: Add architecture guardrails and enforce review loops

**Description:**
Add tests that protect the Worker boundary and keep the branch on the intended
architecture path, then require review/fix loops before each major slice is
considered done.

**Acceptance criteria:**
- [ ] Guardrail tests fail when callers bypass the Worker boundary or widen
      contracts again.
- [ ] Each major slice completes with a `review-pr` pass and no major/blocking
      findings left unresolved.
- [ ] The hardening PR remains reviewable as a sequence of bounded slices.

**Verification:**
- [ ] targeted guardrail tests
- [ ] `review-pr` reports archived with no unresolved major/blocking findings

**Status:** Pending.

## Checkpoints

### Checkpoint A: Boundary and caller cleanup

- [ ] Tasks 1 and 3 are complete.
- [ ] API and CLI both rely on shared Worker construction paths.
- [ ] No targeted Worker regression is red.

### Checkpoint B: File and module decomposition

- [ ] Task 2 is complete.
- [ ] The largest Worker/API/conformance files are split into cohesive modules.
- [ ] Reviewers can reason about streaming and conformance code without tracing
      thousand-line files.

### Checkpoint C: Robustness and maintainability hardening

- [ ] Tasks 4 through 6 are complete.
- [ ] Failure reporting is more explicit.
- [ ] Polling/race windows are reduced.
- [ ] The hardening PR is green and ready to merge into the Worker feature
      branch.
