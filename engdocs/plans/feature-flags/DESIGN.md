# Feature-Flag Subsystem — Design (`internal/rollout`)

_Status: DESIGN for review — no code yet. Produced by a multi-agent workflow (Opus explore · Fable design/synthesize/red-team/harden), 31 agents. First consumer: gating gascity's adoption of the beads compare-and-swap APIs._

**Winning approach:** DESIGN 2: Capability-Resolved Rollout Gates (internal/rollout)

## Why this approach

Design 2 wins on the two axes that matter most for THIS flag: robustness and CAS fit. Its Off/Auto/Require Mode is the only model that matches how gascity actually rolls out correctness changes (the GC_WORK_RECORD_ENFORCE warn-then-enforce and GC_WISP_GC_* dry-run-then-act precedents) and the only one that lets a mixed fleet adopt CAS without choosing between 'off' and 'refused writes' — while still making Require a hard fail-closed contract and making silent unconditional fallback structurally impossible. Its domain-local config placement ([beads] conditional_writes beside bd_compatibility) inherits the existing fragment-merge machinery with zero new layering code and honors progressive-activation-by-section, where D1's and D3's central [features] tables fight the native idiom and need new merge wiring. Its ResolveConditionalWriter seam puts the enable-AND-capable product in exactly one tested function, and its per-store capability model (interface assert + memoized probe + authoritative exit-13 latch) is the only correct answer for the multi-store reality (graphBeadStore vs drainMemberOwningStore vs the deployed sqlite graph store). All three designs tie at 5 on principle_fit and testability — they share the same defensible line (the exclusion bans agent-behavior toggles that smarter models obviate; infra rollout gates select mechanical transport paths invisible to prompts) and the same DI-value test seam. D2's genuine weakness is lifecycle softness, which is exactly where the runners-up are strongest, so the synthesis grafts D3's teeth (mandatory Expires with past-due CI failure, soft cap on active non-Stable flags, tombstones with their own expiry, the prompt-package import-boundary test, the bd-help-text interim probe, test-failure-not-panic registration) and D1's precision (typed Origin tracking end-to-end, per-FIELD merge discipline with a registry-driven coverage test, EXECUTED machine-checkable removal predicates wired into the TestBDVersionPins lockstep, per-flag Latch metadata, RetiredKeys tombstones in undecoded.go). The result is concrete and buildable in staged PRs: (1) internal/rollout + registry + BeadsConfig field + Resolve at both composition roots; (2) beads.ConditionalWriter + typed errors + BdStore exit-9/13 classifier + dedicated retry policy + Mem/File/Caching/sqlite implementations; (3) C4+C6 CAS call sites; (4) library bump + C2 wire change; (5) formula_v2 migration deleting the global-setter anti-pattern.

## Design scores (1–5)

| Design | principle | testability | robustness | maintainability | cas_fit |
|---|---|---|---|---|---|
| DESIGN 1: Config-Native Feature Gates ([features] section + registry in internal/config) | 5 | 5 | 4 | 4 | 4 |
| DESIGN 2: Capability-Resolved Rollout Gates (internal/rollout, Off/Auto/Require Mode on owning config section) | 5 | 5 | 5 | 4 | 5 |
| DESIGN 3: Rollout Registry (internal/rollout, descriptor-first with Expires/soft-cap/tombstone-expiry) | 5 | 5 | 4 | 5 | 4 |

<details><summary>Per-design scoring notes</summary>

- **DESIGN 1: Config-Native Feature Gates ([features] section + registry in internal/config)** — Best origin-tracking story (GateValue{Enabled,Origin}), best tombstone/RetiredFeatureKeys handling, and the per-FIELD fragment-merge insight (whole-section replacement resets sibling flags — the exact daemon.formula_v2 footgun) is load-bearing and must survive into any winner. Executable RemovalConditions (version predicates run by a lifecycle test) are the sharpest anti-rot teeth of the three. Weaknesses: bool-only enable gives operators no observe/degrade middle state — on a mixed fleet (one stale bd) the choice is 'off' or 'brick that store's writes', which will stall real rollouts; the registry living inside internal/config bloats an already 4000+-line package; a central [features] table drifts from the progressive-activation-by-owning-section idiom that BDCompatibility already established for exactly this kind of bd-semantics opt-in.
- **DESIGN 2: Capability-Resolved Rollout Gates (internal/rollout, Off/Auto/Require Mode on owning config section)** — The tri-state Mode (Off|Auto|Require) is the single best idea in the set: it matches the in-tree warn-then-enforce precedents (GC_WORK_RECORD_ENFORCE, GC_WISP_GC_* dry-run), gives mixed fleets a loud-degrade path that never silently converts a refused CAS into an unconditional write, and makes graduation a default walk (Off→Auto→Require) instead of a cliff. Domain-local placement ([beads] conditional_writes beside bd_compatibility) inherits the existing IsDefined("beads") fragment merge with ZERO new merge code and honors section-presence activation. ResolveConditionalWriter(store, mode) puts the enable∧capable product in exactly one tested function. Per-store capability (interface assert + memoized probe + exit-13 latch) is correct for the multi-store reality (graphBeadStore vs drainMemberOwningStore vs sqlite). Weaknesses: lifecycle enforcement is softer than Design 3 (GraduationCriterion/RemovalTrigger are fields, not executed predicates; no Expires date, no cap); the Enable-as-closure registry field is awkward; discoverability of domain-scattered fields depends entirely on the registry+doctor.
- **DESIGN 3: Rollout Registry (internal/rollout, descriptor-first with Expires/soft-cap/tombstone-expiry)** — The strongest lifecycle machinery of the three: mandatory Expires on every non-Stable flag with past-due failing CI, a soft cap (~8 active non-Stable flags) that forces cleanup before addition, tombstones that carry their OWN expiry, and the TestBDVersionPins lockstep wiring that makes CI itself demand the default flip. The prompt-package import-boundary test and the 'no scope field can express per-agent' type-system argument are the crispest structural enforcement of the capability-flag line. The `bd update --help` grep for --if-revision is the only workable capability detector for today's untagged beads#4682. Weaknesses: Bool-only for CAS surrenders the Auto degrade mode (its 'CAS has no dry-run' argument conflates observe-the-write with tolerate-the-incapable-store — the latter is what mixed fleets need); mustRegister panics collide with the no-panics-in-library convention; descriptor-keyed Get is marginally weaker than a typed method per flag for compiler-enforced removal; help-text probe fragility is real (mitigated by the exit-13 latch but converts misdetection into runtime refusals).

</details>

## 1. Overview, goals, and non-goals

### 1.1 What we are building

Two deliverables, one design — the second is how the first ships safely:

1. **Beads CAS adoption.** beads PR gastownhall/beads#4682 gives every bead an opaque `revision int64` nonce and conditional writes: `--if-revision N` on `bd update/close/assign/delete` (exit 9 = precondition failed with a machine JSON body `{code, expected_revision, current_revision}`; exit 13 = refusal, never a silent unconditional fallback) and a library `ConditionalWriter` surface. gascity adopts it for its three known lost-update consumers — the C4 dispatch epoch fence, the C6 drain reservation, and C2 API optimistic concurrency — behind one operator knob:

   ```toml
   # city.toml
   [beads]
   conditional_writes = "auto"   # "off" (default) | "auto" | "require"
   ```

2. **Rollout Gates (`internal/rollout`).** The knob is the first registered consumer of a standardized subsystem for SDK infrastructure rollout/migration gates — a typed field on the owning config section, a mandatory descriptor in one registry file with owner/expiry/removal teeth, one resolution point, and DI-threaded immutable values. It replaces the pattern we would otherwise repeat: a ninth ad-hoc `os.Getenv` gate.

The core shape, in signatures (full semantics in later sections):

```go
// internal/rollout — side-effect-free; imports stdlib + internal/config only.
type Mode string
const (
    Off     Mode = "off"     // legacy path, byte-identical to today
    Auto    Mode = "auto"    // CAS where capable; loud degrade where not
    Require Mode = "require" // CAS or typed refusal — fail closed
)

func Resolve(cfg *config.City, opts ResolveOptions) (Flags, []Notice) // once, in the shared config loaders
func (f Flags) BeadsConditionalWrites() Mode                          // typed accessor; no string keys anywhere
```

```go
// internal/beads — capability is a separate, per-store axis.
type ConditionalWriter interface {
    UpdateIssueIfMatch(id string, rev int64, patch IssuePatch) error
    CloseIssueIfMatch(id string, rev int64) error
    DeleteIssueIfMatch(id string, rev int64) error
    CompareAndSetMetadataKey(id, key, expected, next string) (bool, error)
}
```

The governing invariant: **effective behavior = operator intent AND runtime capability.** Capability can veto intent (`auto` degrades loudly, `require` refuses with a typed error); it can never raise it, and no code path anywhere converts `ErrConditionalWriteUnsupported` into an unconditional write.

### 1.2 Why now: the untagged-#4682 reality

The CAS APIs exist on beads `main` but not in a tagged release. Waiting for the tag — and then for the bundled-`bd` version pin in `deps.env` to cross the floor — would leave three known TOCTOU races open for an unbounded interval:

- **C4:** `molecule.Attach`'s read-compare-`SetMetadata` on `gc.control_epoch` (molecule.go:262–310) lets two processors both win the epoch fence.
- **C6:** `reserveDrainMember`'s read-then-write on `gc.drain.reserved_by` (drain.go:1222–1246) lets two drains claim one member.
- **C2:** API bead mutations have no optimistic-concurrency story at all.

Operators who run "beads latest" (the maintainer fleet does) can close these races **today** if the code path exists and is gated. The flag decouples when the code lands from what any given fleet's `bd` supports. Two consequences shape the whole design:

- **A new knob, orthogonal to `bd_compatibility`.** Opting into CAS on an untagged `bd` must not buy any other future bd-1.1.x semantics. The knobs re-converge at graduation, when the version pin absorbs the floor.
- **Capability is per resolved store, not per process.** One city writes through the bundled `bd` CLI (versioned by `deps.env`), the native Dolt library (versioned by `go.mod`), *and* — on the deployed controller topology that actually holds `gc.control_epoch` and `gc.drain.reserved_by` — a **sqlite graph store**. These upgrade independently; a sqlite `CompareAndSetMetadataKey` is therefore an in-scope blocking deliverable, not a footnote, and capability is probed live per store (never persisted — restart re-probes, per "no status files").

### 1.3 Why a subsystem and not a ninth env var

The tree already contains the counterfactual: `GC_DOLT_AUTO_GC_ENABLED` (env fills only when config is nil), `GC_EVENTS_ROTATION_ENABLED`, `GC_ALLOW_PROD_DOLT_PORT_IN_TESTS`, and the formula_v2 apparatus — a config field wired through `applyFeatureFlags` at 8 scattered `cmd/gc` call sites, a duplicate `syncFeatureFlags` root in the API server, two package-level `atomic.Bool`s, a process-wide test mutex, and ~20 save/restore blocks in `molecule_test.go`. Roughly eight divergent truthy parsers and two contradictory precedence rules. Every new gate copies one of these at random.

The repo's own rule — no abstraction until two implementations exist — is honored by contract, not deferral: the registry ships in stage 1 with **two** Specs registered on day one (beads CAS as `infra-rollout`, formula_v2 as `infra-migration`, each with owner, version anchor, and expiry), plus freeze tests that make the legacy mechanism un-copyable (golden-list boundary test on `SetFormulaV2Enabled`/`applyFeatureFlags`/`syncFeatureFlags` call sites; frozen baseline on new `GC_*` env reads). The formula_v2 code migration is a committed blocking bead in the same milestone; its slippage trips its own registered Spec's lifecycle teeth.

### 1.4 The principle line, stated once

AGENTS.md's permanent exclusion — "No capability flags — a sentence in the prompt is sufficient" — bans Go-side toggles over **agent behavior**, the kind a smarter model makes redundant. A rollout gate selects between two **mechanical transports** (conditional vs unconditional write), invisible to every prompt and template; no model improvement changes whether the operator's installed `bd` parses `--if-revision` or whether the sqlite store implements an interface. The litmus, applied per flag: *would a 10x-smarter model obviate this?* Yes → forbidden, put the sentence in the prompt. No → infra gate, belongs in config. CI blocks the naive smuggling paths (closed `Category` enum with no agent-capability member, no scope field on Spec, the prompt-package import boundary and AST lint); the semantic classification is enforced by a named-human CODEOWNERS gate on the registry file. The principle-fit section carries the full argument and its honest limits; nothing else in this document relitigates it.

### 1.5 Goals — acceptance criteria

"Testable / robust / maintainable" are pass/fail gates on the PRs, not aspirations.

**Testable** — merged only if:

- Zero package-level mutable flag state: no `atomic.Bool`, no `SetX()`, no singleton. `Flags` is an immutable value threaded by DI; the mode has exactly one home (the beads factory stamps it onto every store it opens).
- Tests build flag state per instance via typed options — `rollout.ForTest(t, rollout.WithBeadsConditionalWrites(rollout.Require))` — so deleting a flag breaks tests at **compile time**. No string-keyed override path exists.
- `Resolve` takes an injected `LookupEnv`; no `t.Setenv` anywhere; `GC_BEADS_CONDITIONAL_WRITES` is registered in testenv `LeakVectorVars` (registry-test enforced). Everything is `t.Parallel`-safe by construction, not discipline.
- Capability-absent is an instance toggle on fake stores (interface set intact), and a store-agnostic `ConditionalWriter` conformance suite passes over MemStore, FileStore, CachingStore-over-MemStore, and sqlite in unit CI, plus BdStore against real `bd` under `//go:build integration`.

**Robust** — merged only if:

- `off` is byte-identical to today, asserted by test. Nobody who does nothing is affected.
- The four-cell matrix holds and each cell is tested per consumer:

  | mode | store capable | behavior |
  |---|---|---|
  | `off` | — | legacy write, byte-identical |
  | `auto` | yes | CAS |
  | `auto` | no | legacy write + once-latched diagnostic + typed degrade event |
  | `require` | no | typed refusal + store-open preflight + doctor ERROR (fail closed) |

- No silent fallback is expressible: config typos are fatal at load (registry-driven enum validation); an unparseable env value on a correctness flag fails startup fast; a fragment defining an unrelated `[beads]` sibling key cannot reset the flag (per-field merge preservation + hand-written regression test).
- Mode is process-latched: it can never flip mid-run; the reload path carries the boot snapshot and surfaces divergence as a "pending restart" notice. Every degrade/refusal diagnostic carries `mode` + `origin` in its first line.

**Maintainable** — merged only if:

- Adding a flag is one PR touching four test-enforced places (config field + accessor, Spec, `Flags` accessor, DI threading); removing one is compile-enforced (the accessor's deletion finds every consumer) plus a version-anchored tombstone for the retired TOML key.
- Every lifecycle check in the merge-blocking path is deterministic per commit — no wall-clock-vs-`time.Now()` anywhere in Check. Graduation is a plain Go test against `deps.env` version anchors (Off→Auto when `BD_VERSION` crosses the floor; deletion when `BD_PREV_VERSION` does); calendar staleness lives in a non-blocking nightly radar that files beads.
- Per-category rules are enforced: `infra-rollout`/`infra-migration` flags can never be immortal — their terminal state is deletion; only `infra-killswitch` may be long-lived. `registry.go` is CODEOWNERS-gated with dual Owner (bead + GitHub handle).

For orientation, resolution precedence (exact semantics are pinned in the resolution section):

| # | layer | note |
|---|---|---|
| 1 | built-in default (`Spec.Default`) | CAS: `off` |
| 2 | merged config (pack → city → fragment → patch) | existing loader chain, untouched |
| 3 | env override (`GC_BEADS_CONDITIONAL_WRITES`) | break-glass; per-process; strict grammar |
| 4 | per-store capability | veto only — can never raise a mode |
| 5 | test override (`rollout.ForTest`) | structural; tests never call `Resolve` |

### 1.6 Non-goals — what v1 deliberately excludes

Each exclusion is a decision with a reopening condition, not an omission.

- **Per-layer origin provenance.** Origin is collapsed to the three values recoverable with zero loader changes: `builtin | config | env`. That answers the one audit question that matters ("is a forgotten env var pinning this?"). "Which fragment set this" requires new per-field provenance plumbing through `mergeFragment`; deferred until someone asks, costed honestly as compose.go surgery then.
- **`/v0/config/explain` extension.** Rides on per-layer origin; deferred with it. Slice 1 observability is `gc doctor` only; the typed status-wire surface arrives in stage 4, riding the `go.mod`-bump PR that already forces the OpenAPI/dashboard regen for `Bead.Revision`.
- **Hot-reload / reload-tolerant flags.** v1 is **process-latched for all flags**; the `Latch` Spec field does not exist. The reload path carries the boot-resolved snapshot into all later-constructed components — never a re-resolved mode — because a legacy writer racing a CAS writer on `gc.control_epoch` inside one process is the exact corruption the flag prevents. Reload tolerance returns only when a concrete reload-tolerant flag exists.
- **Per-rig scope.** City-global only. No planned gate needs per-rig granularity; scope machinery waits for a concrete consumer.
- **Per-agent scope.** Refused by the registry (no scope field), guarded by a reflection test on `config.Agent`/`AgentPatch`/`AgentOverride`, and documented as the forbidden shape regardless of declaration site. This one is permanent, not v1.
- **A dynamic flag service.** No percentage rollouts, no runtime toggling, no remote config, no persisted flag or capability state. Capability is probed from live state and cached only in-process; restart re-probes.
- **Wholesale absorption of legacy env vars.** `GC_DOLT_AUTO_GC_ENABLED` and `GC_EVENTS_ROTATION_ENABLED` migrate in stage 5 with their existing precedence preserved per-Spec (`EnvSemantics: fills-nil`); any precedence unification is a separate, release-noted breaking change — never a migration side effect.
- **Fleet-wide writer coordination.** CAS mutual exclusion holds only when every writer to a ledger is CAS-active or exactly one writer exists. v1 documents that invariant in the runbook and warns in doctor under declared multi-writer topologies; it does not enforce single-writer fleets.
- **Speculative machinery.** No generic merge-coverage reflection harness (hand-written per-flag merge tests, per the `daemon.formula_v2` template), no removal-predicate DSL (one ~20-line Go test), no flag-count cap (deleted; the anti-rot teeth are expiry anchors and owners, not a ceiling).

## 2. Principle reconciliation: rollout gates are not capability flags

AGENTS.md lists "No capability flags — a sentence in the prompt is sufficient" under **What Gas City does NOT contain**. Every member of that list — no skills system, no MCP registration, no decision logic in Go, no hardcoded roles — governs the same thing: what a *reasoning agent* may do. And every member is justified by one criterion, stated in the same section: each excluded thing becomes **less** useful as models improve. A Go-side toggle over agent behavior is banned because a smarter model makes it redundant — the prompt already carries the intent, and the toggle is a heuristic crutch that rots.

A rollout gate is a different object. It selects between two **mechanical transports** — for beads CAS, between `bd update --if-revision N` and today's unconditional `bd update` — based on a deployment fact: which bd binary is installed, whether a store implements `ConditionalWriter`, whether the operator has scheduled the migration. No prompt, template, or agent can observe the difference; both branches move the same bytes to the same ledger with different concurrency guarantees.

### 2.1 The litmus

Two questions, asked of every proposed flag. They are printed in the header of `internal/rollout/registry.go` and repeated in the PR template:

1. **Would a 10x-smarter model make this flag unnecessary?** If yes, it is an agent-capability flag. Delete it and move the sentence to the prompt.
2. **Do both branches move bytes rather than make decisions?** If either branch encodes a judgment call (`if idle > N then nudge`), it is decision logic in Go wearing infra clothes — also banned, by a different clause of the same list.

Applied to beads CAS, the answer to (1) is *no* on every leg: no model improvement changes whether the operator's installed bd parses `--if-revision`, whether the sqlite graph store holding `gc.control_epoch` implements `CompareAndSetMetadataKey`, or whether the fleet has finished its migration window. The answer to (2) is *yes*: the gate is `mode != off && store satisfies ConditionalWriter` — interface satisfaction ANDed with a static operator input, the same mechanics as the existing `bdReadyProjectionEnabled` version gate. No heuristic, no threshold, no reasoning.

This is not a novel carve-out. `daemon.formula_v2` and `beads.bd_compatibility` are in-tree, accepted flags of exactly this kind. The subsystem standardizes settled practice; it does not open a new category.

### 2.2 The shape of the two things

| | Capability flag (banned) | Rollout gate (this subsystem) |
|---|---|---|
| Governs | What an agent may do / how it behaves | Which mechanical code path the SDK executes |
| Visible to prompts | Yes — that is its purpose | Never — enforced below |
| Obviated by smarter models | Yes (the prompt carries the intent) | No (bd's argv parser does not get smarter) |
| Correct home | A sentence in the pack's prompt template | A typed field on the owning config section |
| Terminal state | Should never exist | Deletion, forced by lifecycle teeth (§8) |

```toml
# Rollout gate: selects a transport. Invisible to every prompt.
[beads]
conditional_writes = "auto"   # off | auto | require

# The forbidden shape — never expressible through this subsystem,
# and flagged in review regardless of where it is declared:
[[agent]]
name = "worker"
# allow_force_push = true     <- per-agent behavior toggle. If an agent
#                                needs to know it, it belongs in the prompt.
```

### 2.3 What CI enforces structurally

The naive smuggling paths are blocked by build-failing tests. Each is concrete and shipped in stage 1 (PR-1a/1b):

**Closed Category enum.** `Spec.Category` is a three-member closed enum — `infra-rollout | infra-migration | infra-killswitch`. There is no agent-capability member and `registry_test.go` rejects any value outside the set. You cannot register a behavioral flag without misclassifying it, and misclassification is what the human gate (§2.4) exists to catch.

```go
type Category string

const (
    InfraRollout    Category = "infra-rollout"    // adopt a new mechanical path, terminal state: deletion
    InfraMigration  Category = "infra-migration"  // retire an old mechanical path, terminal state: deletion
    InfraKillswitch Category = "infra-killswitch" // emergency off for a subsystem, may be long-lived
)

type Spec struct {
    Key            string
    Category       Category   // closed enum, no agent-capability member
    ConfigPath     string     // reflection-verified against config.City toml tags
    EnvOverride    string     // "" or one GC_* name in testenv LeakVectorVars
    EnvSemantics   EnvSemantics
    Default        string
    Owner          Owner      // bead ID + GitHub handle/team
    Expires        string     // mandatory for rollout/migration, forbidden for killswitch
    VersionAnchor  string
    SelectsBetween [2]string  // the two mechanical code paths, named (§2.4)
    Justification  string     // the written litmus answer — documentation, not a CI tooth
}
// Note what is absent: there is no Scope field. The registry cannot
// express a per-agent or per-rig flag at all.
```

**No scope field, plus the Agent-struct reflection guard.** The registry's refusal of per-agent scope is real but insufficient on its own — `config.Agent` is a routine extension point with a documented field-sync checklist. So a reflection test fails the build if `config.Agent`, `AgentPatch`, or `AgentOverride` ever gains a field typed `rollout.Mode` (or an accessor returning it). The honest statement: the *registry* makes per-agent flags inexpressible; the *config system* could still express one, and that shape is forbidden by review rule regardless of declaration site (§2.4).

**The import boundary actually exists.** Today prompt rendering (`renderPrompt`, `buildTemplateData`, `PromptContext`) lives in `package main` of `cmd/gc` — the same package as the composition root that calls `rollout.Resolve`, so a naive "prompt packages must not import rollout" test would be vacuous or permanently red. PR-1a extracts rendering into `internal/prompt` (a mechanical move that also fixes the rendering-in-CLI layering smell). Only then is the forbidden edge testable, and a build-failing test asserts it: **`internal/prompt` imports `internal/rollout` → red**. The instant a flag value would flow into a prompt through the type system's front door, the build blocks it.

**Registry-driven AST lint.** Import analysis cannot see a value smuggled through `cmd/gc`, which legitimately imports both packages — and `PromptContext.Env` is an open `map[string]string` that flows wholesale into template data. So an AST-level lint (same mechanism as `TestNoLeakVectorReadsAtPackageInit`) asserts, repo-wide:

- no `PromptContext` construction site references any `rollout.Flags` accessor;
- no write to `PromptContext.Env` references any `rollout.Flags` accessor;
- no template `FuncMap` closure references any `rollout.Flags` accessor.

The lint is registry-driven — it derives the accessor list from the registry, so it grows automatically with every flag and never needs a hand-maintained denylist.

**Reverse parity, where it is mechanically definable.** Any config field typed `rollout.Mode` anywhere in `config.City` must have a `Spec` (reflection-checked). `*bool` kill-switches are *not* mechanically distinguishable from ordinary optional config; their classification is review-governed, and this document says so rather than claiming a bidirectional test that cannot exist.

### 2.4 What review governs — stated honestly

CI blocks the naive paths. It cannot evaluate semantics: a flag value laundered through a bare `bool` into a template data struct three hops away defeats every check above, and a judgment-in-Go gate (`if idleFor > threshold { nudge() }`) can wear a compliant `infra-killswitch` label. The semantic half of the line is enforced by **review with teeth**, and the teeth are specific:

- **`SelectsBetween` is mandatory.** Every Spec must name its two mechanical code paths — for CAS: `{"conditional bd write (--if-revision)", "unconditional bd write"}`. An author who cannot fill this field with two transports has written a decision, not a gate, and the review conversation starts from that artifact rather than from vibes.
- **The litmus questions live in the registry file header** and in the PR-template checklist, including the value-flow item: *"does any template data struct field trace to a rollout flag?"*
- **`registry.go` is CODEOWNERS-gated** by a named human team. Every new Spec, every `Expires` extension, every category assignment gets a named-human review — the only real gate for semantic classification in a repo where most PRs are agent-authored.
- **The contributor doc states the rule that closes the config-system gap:** a per-agent toggle that changes what an agent may do is the forbidden shape *regardless of where it is declared* — registry, `config.Agent`, `Agent.Env`, or a bare env var. The worked example is the tempting one: staged per-cohort CAS adoption via `Agent.ConditionalWritesOptIn` is rejected even though it feels like rollout, because per-agent scope is precisely the shape that mutates into behavioral toggles and leaks into prompts via `Agent.Env`.

We deliberately do not overclaim. `Justification` is checked only for presence; no test can grade its truth. Overclaiming structural enforcement is how checks get cargo-culted and then neutered — the design's posture is a small set of hard mechanical walls plus a named-human gate on the one file every flag must touch.

### 2.5 The remaining principles, in one pass

- **"Keep judgment out of Go."** The gate is `enabled && interface-satisfied`. Capability never *raises* a mode (off stays off); reality only vetoes intent. No line of the gate reasons about work.
- **"A primitive must become more useful as models improve."** The gate is orthogonal to model quality by construction — its inputs are a TOML field and an interface assertion. It neither gains nor loses value with smarter models, which is exactly the profile of infrastructure rather than a banned heuristic.
- **"Config is the universal activation mechanism."** Not merely reconciled — it *is* the design: the enable axis is a typed field on the owning config section, resolved through the existing pack→city→fragment→patch chain. Env is a thin audited overlay, not a parallel truth (§4).
- **"No status files — query live state."** Capability is probed from live state (bd subprocess, interface satisfaction, exit-13 outcome) and cached only in-process; nothing is persisted, restart re-probes (§6).
- **SDK self-sufficiency and ZERO roles.** No role name appears in any key, default, or resolution input; removing any `[[agent]]` entry cannot change a flag verdict because agents are nowhere in the resolution path.

## 3. Flag model and the registry

### 3.1 Two value kinds, both typed — never an open map

The subsystem admits exactly two flag value kinds. There is no generic `map[string]bool` "features" bag: an open map would defeat the unknown-key typo detection in `internal/config/undecoded.go` (which is reflection-driven over typed structs), the jsonschema doc generation, and the existing field-sync tests. Every flag is a typed field on its owning config section, and every read is a typed accessor.

**Kind 1: `rollout.Mode`** — a three-state enum for correctness and migration gates that need an observe/degrade middle state between "off" and "hard contract" (in-tree precedents for the shape: `GC_WORK_RECORD_ENFORCE` warn→enforce, `GC_WISP_GC_*` dry-run→act):

```go
package rollout

// Mode is the value kind for correctness/migration gates.
type Mode string

const (
	Off     Mode = "off"     // legacy path, byte-identical to pre-flag behavior
	Auto    Mode = "auto"    // new path where the resolved store is capable;
	                         // loud once-latched degrade to legacy otherwise
	Require Mode = "require" // new path or typed refusal — fail-closed,
	                         // a silent unconditional fallback does not exist
)
```

How capability AND-gates a resolved `Mode` per store is the capability section's topic; the point here is that the *value model* itself carries the degrade state, so a mixed fleet is expressible as configuration rather than as an error condition.

**Kind 2: `*bool`, nil = built-in default** — for simple kill-switches, generalizing the existing `DaemonConfig.FormulaV2` / `EffectiveAutoGCEnabled` idiom: absent means "the default", explicit `false` (or `true`) is an operator decision, and the pointer distinguishes the two.

A Spec's kind is implied by which arm of its `Default` is set (§3.3) — there is no separate `Kind` field to drift.

In TOML, the two kinds look like ordinary fields on their owning sections (placement and fragment-merge rules are the config-placement section's topic):

```toml
[beads]
conditional_writes = "auto"   # rollout.Mode: off | auto | require; absent ⇒ built-in default (off)

[daemon]
formula_v2 = false            # *bool kill-switch: absent ⇒ default (true); explicit false = operator off
```

Note the import direction: `internal/rollout` imports `internal/config` (its `Resolve` takes `*config.City`), so config structs cannot reference `rollout.Mode`. A Mode flag's config field is a validated string (`toml:"conditional_writes,omitempty" jsonschema:"enum=off,enum=auto,enum=require"`); the string→`Mode` mapping and the typed read surface (`Flags.BeadsConditionalWrites() Mode`) live in `internal/rollout`. This asymmetry is load-bearing for the reverse-parity tests below.

### 3.2 Scope: city-global only — and what that claim honestly means

Flags are city-global. The `Spec` type has **no scope field**: the registry cannot describe a per-rig or per-agent flag, so nobody arrives at per-agent capability toggles by following the paved road. Per-rig scope waits until a concrete gate needs it.

Stated honestly: the *registry* refuses per-agent scope; the *config system* could still express one — `config.Agent` grows fields by a documented checklist, and `Agent.Env` flows into prompt template data. So the claim is not "inexpressible by construction"; it is the registry's refusal plus two mechanical tripwires plus one review rule:

1. A reflection test in `internal/rollout`'s test package (which may import both packages — no production cycle) fails if `config.Agent`, `config.AgentPatch`, or `config.AgentOverride` ever gains a `rollout.Mode`-typed field.
2. The reverse-parity walk (§3.6) flags any Mode-shaped config field anywhere that lacks a Spec.
3. The contributor doc states the rule the tests cannot check: **a per-agent toggle that changes what an agent may do is the forbidden capability-flag shape regardless of where it is declared.** (The full principle-line enforcement — prompt-boundary import test, AST lint — is the principle section's topic.)

### 3.3 The Spec: nine load-bearing fields, each with a tooth

`internal/rollout/registry.go` holds one descriptor per flag. Every surviving field either does mechanical work in a test or gates review; the fields that were pure form-filling were deleted (see the end of this subsection).

```go
// Category classifies why a gate exists and selects its lifecycle rules.
// The enum is CLOSED: there is no agent-capability member and none may be added.
type Category string

const (
	InfraRollout    Category = "infra-rollout"    // staged adoption of a new mechanical transport
	InfraMigration  Category = "infra-migration"  // retiring a legacy in-tree mechanism
	InfraKillswitch Category = "infra-killswitch" // operator emergency-off for a shipped subsystem
)

// EnvSemantics pins how a Spec's env var interacts with explicit config.
type EnvSemantics string

const (
	EnvOverrides EnvSemantics = "overrides"  // env wins over explicit config (break-glass; default for new flags)
	EnvFillsNil  EnvSemantics = "fills-nil"  // env applies only when config leaves the field unset
	                                         // (preserves absorbed legacy flags' shipped precedence)
)

// Default carries the built-in value. Exactly one arm is set; the set arm
// determines the flag's value kind. Enforced by Validate.
type Default struct {
	Mode *Mode
	Bool *bool
}

// Owner is dual: the bead tracks the work; the GitHub handle/team is the
// named human the lifecycle radar and CODEOWNERS review actually reach.
type Owner struct {
	Bead   string // e.g. "ga-9wsri"
	GitHub string // "@handle" or "@org/team"
}

type Spec struct {
	Key            string       // canonical dotted name, e.g. "beads.conditional_writes"
	Category       Category
	ConfigPath     string       // toml path on config.City; reflection-verified (§3.6)
	EnvOverride    string       // "" or exactly one GC_*-prefixed var
	EnvSemantics   EnvSemantics // meaningful only when EnvOverride != ""
	Default        Default
	Owner          Owner
	Expires        string       // YYYY-MM-DD; feeds the non-blocking nightly radar
	VersionAnchor  string       // repo-pinned removal floor (deps.env key or in-repo version constant)
	SelectsBetween [2]string    // the two MECHANICAL code paths this flag selects between
	Justification  string       // the written answer to "why doesn't a 10x-smarter model obviate this?"

	// Lifecycle bookkeeping — zero until the corresponding event; validated
	// by the lifecycle tests, not by authors at registration time.
	GraduatedIn string // version anchor at which the default flipped
	FlipDueBy   string // bounded machine-checked deferral set by a version-bump PR
}
```

Per-field rationale — what each field costs and what enforces it:

| Field | Job | Tooth |
|---|---|---|
| `Key` | canonical identity; names the flag in doctor, events, notices | non-empty, unique (`Validate`) |
| `Category` | selects enforced lifecycle rules; the closed enum is the structural half of the principle line | member of closed enum; per-category rules in §3.6.2 |
| `ConfigPath` | binds the Spec to its owning config field | reflection-resolved against `config.City` toml tags; type must match kind |
| `EnvOverride` | the one sanctioned break-glass surface | `""` or `GC_*`-prefixed, unique, and registered in `testenv.LeakVectorVars` |
| `EnvSemantics` | prevents absorption of a legacy flag from silently inverting its shipped precedence | member of closed enum; absorbed flags must declare `fills-nil` unless a release-noted breaking change says otherwise |
| `Default` | the built-in value, in ONE home | exactly one arm set; zero-value-config equality test (§3.6.3) closes the two-homes drift |
| `Owner` | who the radar files beads against, who review pings | both parts non-empty; GitHub part matches `@handle`/`@org/team`; the real gate is CODEOWNERS (§3.5) |
| `Expires` | wall-clock staleness signal for the nightly radar and doctor WARN — **never** a merge-blocking date bomb | mandatory for rollout/migration, **forbidden** for killswitch |
| `VersionAnchor` | the deterministic removal floor the two-stage graduation test executes | mandatory (non-empty, syntactically a deps.env key or in-repo anchor) for rollout/migration, forbidden for killswitch; presence *in* deps.env is not required at registration — the lifecycle test arms itself the day the anchor lands (the untagged-#4682 reality) |
| `SelectsBetween` | forces the author to articulate two mechanical transports — the reviewable artifact that separates rollout gates from judgment-in-Go wearing infra clothes | both entries non-empty and distinct; semantic honesty is CODEOWNERS review's job |
| `Justification` | documentation of the principle-line answer, kept where reviewers will read it | **explicitly not a CI tooth** — a non-emptiness check only invites `"n/a"`; the litmus questions live in the registry file header and the human gate is review |

**Deleted fields, deliberately:** `Stability` (a one-line `Stable` edit was an immortality escape hatch — per-category rules replace it: only killswitches may be long-lived, and that is a property of `Category`, not a mutable tier); `IntroducedIn` (duplicates `git blame`); `GraduationCriterion` (free text duplicating `VersionAnchor`); `Latch` (v1 is process-latched for every flag — a field nothing reads is metadata theater); and the ~8-flag soft cap (governance for a population problem that doesn't exist, whose only failure mode was training people to bump the constant). Each deletion removes a place for form-filling to rot; none removes an enforcement.

### 3.4 The canonical slice is unexported

The registry is package-private. No other package — and critically, no *test* — can mutate shared state:

```go
// specs is the canonical registry. Unexported by design: an exported mutable
// slice would let one test's synthetic append leak into every parallel
// sibling that builds Flags from the registry.
var specs = []Spec{ /* §3.7 */ }

// Specs returns a defensive copy of the canonical registry.
func Specs() []Spec {
	out := make([]Spec, len(specs))
	copy(out, specs)
	return out
}

// Validate reports every structural violation in reg. It takes the registry
// as a PARAMETER: registry_test.go runs it against the canonical set, while
// rollout's own subsystem tests (e.g. "Validate rejects a missing Owner")
// construct throwaway []Spec literals and never touch shared state.
func Validate(reg []Spec) []error
```

`Resolve` and `ForTest` likewise consume a `[]Spec` (defaulting to the canonical set), so a validator test provoking a bad Spec and a parallel consumer test building `Flags` are structurally isolated — no cleanup discipline, no ordering dependence. (The typed `With*` override options on `ForTest` are the test-seams section's topic.)

### 3.5 Dual Owner and the CODEOWNERS gate

`Owner` is dual on purpose. The bead ID is the work-tracking half — but beads in this project get closed and bulk-purged, and a plain `go test` cannot verify a bead exists, so a bead alone is decorative. The GitHub handle/team is the half that stays reachable, and it is backed by the one mechanism that actually inserts a named human into an agent-authored repo's review loop:

```
# .github/CODEOWNERS
/internal/rollout/registry.go @gastownhall/gascity-admin
```

Every new Spec, every `Expires` extension, every `FlipDueBy` deferral, and every category claim is a diff to `registry.go` and therefore requires a named-human review. This is stated plainly: the semantic classification of a flag — "is this really an infra gate, or a judgment call wearing infra clothes?" — is **enforced by review-with-teeth, not by CI**. CI blocks the naive paths (closed enum, no scope field, parity walks); the CODEOWNERS gate plus the `SelectsBetween` articulation and the file-header litmus questions ("would a 10x-smarter model obviate this?" / "do both branches move bytes rather than make decisions?") are the enforcement for everything a test cannot judge.

### 3.6 Registry tests

`registry_test.go` lives in-package (it can see `specs` and the unexported resolved values without any stringly public API) and fails the build — not panics at init — on violation. Every check is deterministic per commit; no wall-clock comparison appears anywhere in the merge-blocking path.

1. **Shape and completeness.** Unique non-empty `Key`s; `Category` in the closed enum; exactly one `Default` arm set; `SelectsBetween` entries non-empty and distinct; `Owner.Bead` and `Owner.GitHub` non-empty with the GitHub part matching `@handle`/`@org/team`; `EnvOverride` either `""` or `GC_*`-prefixed and unique across Specs.
2. **Per-category lifecycle rules.** `infra-rollout` and `infra-migration` MUST carry `Expires` and `VersionAnchor` — these categories may never be immortal; their legal terminal state is deletion. `infra-killswitch` MUST carry neither — it is the only legitimately long-lived category, and immortality is a property of the category, not an editable tier.
3. **Default equality (the two-homes drift closer).** `Resolve` over a zero-value `config.City` with an empty injected `LookupEnv` must yield exactly `Spec.Default` for every flag. This is the test that makes a graduation PR atomic: flipping the accessor's `""`→default mapping in `internal/config` without updating `Spec.Default` (or vice versa) is a red build, so `gc doctor` can never render a default the binary doesn't have.
4. **ConfigPath forward parity.** A reflection walk over `config.City`'s toml tags resolves every `Spec.ConfigPath` to a real field, and the field's type must match the Spec's kind: Mode-kind flags land on a `string` field carrying exactly the `enum=off,enum=auto,enum=require` jsonschema tag; bool-kind flags land on a `*bool`.
5. **Reverse parity — the mechanical half only.** The same walk fails on: (a) any field anywhere in `config.City` typed `rollout.Mode` without a Spec (a tripwire — today's import direction makes such a field impossible, and this test keeps it that way); (b) any `string` config field whose jsonschema enum is exactly the Mode spellings but which has no Spec — this signature is how a Mode flag actually manifests in config, so a shadow tri-state gate can't hide in a typed field; (c) any `rollout.Mode`-typed field in `config.Agent`/`AgentPatch`/`AgentOverride`, unconditionally (the per-agent guard from §3.2). What this test cannot do is stated honestly: a `*bool` kill-switch is mechanically indistinguishable from ordinary optional config, so `*bool` classification is review-governed — the frozen `GC_*` env-read baseline and the legacy-mechanism golden list (freeze section) are what make the *bypass* loud, not this walk.
6. **Env hygiene.** Every non-`""` `EnvOverride` must appear in `internal/testenv`'s `LeakVectorVars`, so a live agent-session `GC_BEADS_CONDITIONAL_WRITES=require` can never leak into test processes and flip a test's resolution.

### 3.7 Day-one contents: born at N=2

The registry never exists with one consumer. Stage 1 registers two Specs — the CAS gate whose code lands in stages 2–4, and the existing formula_v2 mechanism whose code migrates in stage 5 but whose *descriptor* (owner, expiry, removal anchor) enters the anti-rot regime immediately, so stage-5 slippage trips the Spec's own lifecycle teeth:

```go
var specs = []Spec{
	{
		Key:          "beads.conditional_writes",
		Category:     InfraRollout,
		ConfigPath:   "beads.conditional_writes",
		EnvOverride:  "GC_BEADS_CONDITIONAL_WRITES", // named consumer: deployments with
		EnvSemantics: EnvOverrides,                  // baked/immutable config
		Default:      Default{Mode: ptr(Off)},
		Owner:        Owner{Bead: "<stage-1 bead>", GitHub: "@gastownhall/gascity-admin"},
		Expires:      "2027-01-15", // radar/doctor WARN signal, never a merge-blocking date
		VersionAnchor: "bdConditionalWritesMinVersion", // lands in deps.env when beads tags #4682
		SelectsBetween: [2]string{
			"conditional write: bd --if-revision / store CompareAndSet",
			"unconditional read-then-write (legacy, status-quo TOCTOU)",
		},
		Justification: "Whether the installed bd parses --if-revision and whether a " +
			"resolved store implements ConditionalWriter are deployment facts about " +
			"infrastructure versions, invisible to every prompt and template; no model " +
			"improvement changes them.",
	},
	{
		Key:          "daemon.formula_v2",
		Category:     InfraMigration,
		ConfigPath:   "daemon.formula_v2",
		EnvOverride:  "", // the legacy mechanism has no env var; none is being added
		Default:      Default{Bool: ptr(true)}, // matches today's FormulaV2Enabled() nil⇒true
		Owner:        Owner{Bead: "<migration bead>", GitHub: "@gastownhall/gascity-admin"},
		Expires:      "2026-12-31",
		VersionAnchor: "gcFormulaV2RemovalFloor", // in-repo gc version anchor for legacy-path deletion
		SelectsBetween: [2]string{
			"formula compiler v2 graph workflow infrastructure",
			"formula v1 sequential in-session execution",
		},
		Justification: "Selects between two shipped execution substrates during a " +
			"code migration; which one runs is an operator deployment choice, not " +
			"anything a model reasons about.",
	},
}
```

Two details worth pinning: the CAS `VersionAnchor` names an anchor that does **not** yet exist in `deps.env` — that is the correct representation of the untagged-#4682 reality, and the two-stage graduation test (lifecycle section) arms itself the day the anchor lands; and formula_v2's registration precedes its code migration by design — the descriptor is the commitment device, the freeze tests (two-consumers section) make the old mechanism un-copyable, and the migration's completion is what deletes `cmd/gc/feature_flags.go` rather than this registry growing a third home for the same flag.

## 4. Config placement and fragment-merge safety

### 4.1 The flag lives on the owning section, not in a central table

The CAS gate is a typed field on `BeadsConfig`, directly beside its closest precedent (`BDCompatibility`, `internal/config/config.go:1377`):

```go
// internal/config/config.go — BeadsConfig

// ConditionalWrites selects the write discipline for stores this city opens:
// "off" (legacy read-then-write, byte-identical to today), "auto" (CAS where
// the resolved store is capable, loud degrade otherwise), or "require"
// (CAS or typed refusal — never an unconditional fallback).
// Empty defaults to "off". Rollout gate: see internal/rollout/registry.go
// (Key "beads.conditional_writes") for owner, expiry, and removal trigger.
ConditionalWrites string `toml:"conditional_writes,omitempty" jsonschema:"enum=off,enum=auto,enum=require"`
```

```toml
# city.toml — operator opt-in
[beads]
bd_compatibility   = "bd-1.0.5"
conditional_writes = "require"
```

Read access goes through exactly one pure accessor, which is the *only* place the built-in default is encoded on the config side:

```go
// ConditionalWritesMode returns the configured conditional-writes mode.
// Load-time validation (§4.3) guarantees any non-empty value is a member of
// the enum; this accessor only ever maps the empty string to the default.
func (b BeadsConfig) ConditionalWritesMode() rollout.Mode {
    if b.ConditionalWrites == "" {
        return rollout.Off // must equal the registry Spec.Default; registry_test enforces equality
    }
    return rollout.Mode(b.ConditionalWrites)
}
```

Why the owning section and not a central `[features]` table:

- **Progressive activation is section-presence.** `[beads]` is where an operator already declares beads behavior; a CAS opt-in appearing anywhere else breaks the "config section = capability" model the loader is built around.
- **Layering is inherited, not rebuilt.** pack → city → fragment → patch resolution for `[beads]` already exists; a new table would need its own merge wiring.
- **Discoverability is recovered elsewhere.** Central listing is the registry's job (`internal/rollout/registry.go`, rendered by `gc doctor`), not the TOML file's.

The registry entry binds the two homes: the CAS Spec's `ConfigPath` is `"beads.conditional_writes"`, and registry_test reflection-resolves it against `City`'s toml tags, so renaming or deleting the field without touching the Spec (or vice versa) fails the build. A second registry assertion constructs a zero-value `config.City` and requires `ConditionalWritesMode() == Spec.Default`, closing the two-homes default drift.

Unknown *keys* are already handled: `undecoded.go` fatals on a typo'd key name (`conditional_write = "auto"` → unknown-key error with an edit-distance suggestion). This section adds the missing half — bad *values* (§4.3).

### 4.2 Fragment merge: the mandatory per-field preservation branch

`mergeFragment` treats `[beads]` as a whole-table last-writer-wins section (`internal/config/compose.go:1030`):

```go
if fragMeta.IsDefined("beads") {
    base.Beads = fragment.Beads
}
```

Without intervention this is a silent `require → off` downgrade vector: any included fragment that defines *any* `[beads]` key replaces the whole struct, and the fragment's zero-value `ConditionalWrites` erases the city's explicit opt-in.

```toml
# city.toml
include = ["shared-pack.toml"]
[beads]
conditional_writes = "require"

# shared-pack.toml — one unrelated sibling key
[beads]
prefix = "mc"
# → without §4.2, conditional_writes resolves to "" → Off. Doctor shows
#   Origin=builtin. The operator believes the epoch fence is enforced.
```

The fix is the exact pattern the codebase already carries for its one real rollout flag — the `daemon.formula_v2` preservation branch immediately below (`compose.go:1039-1045`). Every registry flag whose field lives in a whole-table-LWW section MUST get the same hand-written branch:

```go
// internal/config/compose.go — mergeFragment
if fragMeta.IsDefined("beads") {
    conditionalWrites := base.Beads.ConditionalWrites
    base.Beads = fragment.Beads
    if !fragMeta.IsDefined("beads", "conditional_writes") {
        base.Beads.ConditionalWrites = conditionalWrites
    }
}
```

Semantics, stated precisely:

- A fragment that **explicitly defines** `beads.conditional_writes` wins (last-writer-wins is preserved for deliberate overrides — a fragment may legitimately set `"auto"` over a pack's `"off"`).
- A fragment that defines **only sibling keys** leaves the base value untouched. `toml.MetaData.IsDefined` distinguishes "key present" from "zero value", which a struct comparison cannot.
- The `daemon` template also preserves across its deprecated `graph_workflows` alias; `conditional_writes` has no alias, so the single-key check is complete. Any future flag that ships with an alias must check both keys, exactly as the daemon branch does.

**Each such branch gets a hand-written regression test, modeled on `TestLoadWithIncludesPreservesExplicitFormulaV2FalseAcrossDaemonFragment` (`compose_test.go`).** The generic registry-driven reflection merge harness that earlier drafts proposed is deleted: it had zero consumers (no planned flag opens a new section), and `toml.MetaData.IsDefined` has known shape-dependent subtleties that a synthetic-fragment generator would paper over. The proven idiom is a concrete test per flag:

```go
func TestLoadWithIncludesPreservesConditionalWritesAcrossBeadsFragment(t *testing.T) {
    fs := fsys.NewFake()
    fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[beads]
conditional_writes = "require"
`)
    fs.Files["/city/fragment.toml"] = []byte(`
[beads]
prefix = "mc"
`)
    cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
    if err != nil {
        t.Fatalf("LoadWithIncludes: %v", err)
    }
    if got := cfg.Beads.ConditionalWritesMode(); got != rollout.Require {
        t.Fatalf("ConditionalWritesMode = %q, want require to survive a sibling-key beads fragment", got)
    }
    if cfg.Beads.Prefix != "mc" {
        t.Fatalf("Beads.Prefix = %q, want fragment field applied", cfg.Beads.Prefix)
    }
}
```

A companion test asserts the deliberate-override direction (fragment sets `conditional_writes = "auto"` → resolved mode is Auto), so the branch can't drift into "base always wins" either.

Lifecycle rule (recorded in the flag-addition checklist, one sentence, no machinery): *a flag whose config field lands in an existing whole-table-LWW section adds the per-field `IsDefined` preservation branch and its regression test in the same PR; a flag opening a new section adds per-field `IsDefined` merge branches — never whole-struct assignment — plus a hand-written merge test (the `mergeSessionSleep` + `daemon.formula_v2` pattern), written when that flag actually appears.*

### 4.3 Load-time enum validation: a typo can never mean "off"

The accessor's `"" → default` mapping is safe only if the accessor never sees an unvalidated non-empty value. Today it would: nothing in `internal/config` validates enum *values*. The in-tree precedent is itself the bug — `NormalizedBDCompatibility` (`config.go:1401`) silently maps any unknown value to `bd-1.0.4` via its `default:` case, and the validation its doc comment promises ("validation reports unknown values separately when loading user config") does not exist. Copying that idiom means `conditional_writes = "requre"` silently resolves to Off — a silent fallback on the exact knob whose design contract is "no silent fallback".

The subsystem therefore ships a registry-driven, **hard-error** validation walk, run at config load beside the existing hard validator (`ValidateDoltConfig`, invoked at `compose.go:701` — deliberately *not* appended to the warnings-only `ValidateSemantics` stream at `compose.go:711`, because a mangled correctness mode must stop the load, not decorate it):

```go
// internal/config/validate_rollout_flags.go

// ValidateRolloutFlagValues rejects out-of-enum values for every registered
// rollout flag. Driven by the registry: each Spec.ConfigPath is resolved
// against cfg by the same reflection used in registry_test, so a new flag
// gets load-time validation with zero per-flag code here.
func ValidateRolloutFlagValues(cfg *City, source string, specs []rollout.Spec) error {
    for _, spec := range specs {
        raw := resolveConfigPath(cfg, spec.ConfigPath) // reflection over toml tags
        if raw == "" || spec.Allows(raw) {
            continue
        }
        return fmt.Errorf(
            "%s: [%s] %s: invalid value %q (allowed: %s)",
            source, spec.Section(), spec.Field(), raw, strings.Join(spec.AllowedValues(), ", "))
    }
    return nil
}
```

Failure is fatal and names all three things the operator needs:

```
city.toml: [beads] conditional_writes: invalid value "requre" (allowed: off, auto, require)
```

Contract points:

- **Every registry flag gets this for free.** The walk iterates Specs; there is no per-flag validation code to forget. For `Mode` flags the allowed set is `off|auto|require`; `*bool` kill-switches are type-checked by TOML decoding itself and skip the walk.
- **Accessors stay total but never exercised on garbage.** `ConditionalWritesMode` keeps a trivially total mapping, but validation guarantees the non-empty input is a member of the enum before any accessor runs. No `default:` case that quietly picks a winner.
- **Validation runs on the merged result**, after `mergeFragment` and patches — so a bad value introduced by *any* layer (pack, fragment, patch) is caught, and a good value destroyed by a merge bug is exercised by §4.2's tests rather than masked here.
- **jsonschema enum tags remain doc-gen only.** They feed the generated schema and editor tooling; they are not, and have never been, runtime enforcement. The walk is the runtime tooth.

**Same-PR bugfix:** `bd_compatibility` joins the walk as a validated enum field (it is not a rollout Spec; the validator additionally accepts a small static list of pre-existing enum-valued fields, of which `bd_compatibility` is the first). `NormalizedBDCompatibility`'s `default:` case becomes defensively unreachable, and its doc-comment claim becomes true instead of aspirational. Fixing the cited precedent in the same PR matters: the next flag author will copy whatever `bd_compatibility` does.

### 4.4 What each layer catches

| Failure mode | Caught by |
| --- | --- |
| Typo'd key (`conditional_write = ...`) | `undecoded.go` unknown-key fatal + suggestion (existing) |
| Typo'd value (`"requre"`, `"Require"`, `"required"`) | §4.3 fatal enum walk at load |
| Fragment sibling key wipes the flag | §4.2 preservation branch + hand-written regression test |
| Field renamed without registry update (or vice versa) | registry_test ConfigPath reflection check |
| Accessor default drifts from Spec.Default | registry_test zero-value-City equality assertion |
| Deliberate fragment override of the flag | last-writer-wins preserved; §4.2 companion test pins it |

Nothing in this section adds new merge machinery, new provenance plumbing, or a parallel config surface: one field, one accessor, one merge branch mirroring an in-tree template, two hand-written tests, and one registry-driven validator that every future flag inherits.

## 5. Resolution: precedence, origin, env break-glass, latching

### 5.1 Precedence

Resolution is a strict five-layer stack. The first three layers are what `rollout.Resolve` computes; the last two are deliberately *not* precedence layers inside the resolver — one is a downstream veto, one is structural.

| # | Layer | Wins when | Reported `Origin` |
|---|-------|-----------|-------------------|
| 1 | Builtin default | Flag absent from every config layer and env (`Spec.Default`, mirrored by the accessor's `""` mapping) | `builtin` |
| 2 | Merged config | Key present in any layer of the **existing** pack → city → fragment → patch chain. Resolve receives the already-merged `*config.City`; the compose pipeline is untouched by this design (per-field merge preservation is section 4's concern) | `config` |
| 3 | Env override | `Spec.EnvOverride != ""`, the var is set, the value parses, and `Spec.EnvSemantics` permits it to apply (5.5) | `env` |
| 4 | Runtime capability veto | Per resolved store, at the beads factory / consumption seam (section 7). **A veto can only lower effective behavior, never raise it**: `off` stays `off` on a fully capable store (no auto-enable), `auto ∧ ¬capable` degrades loudly, `require ∧ ¬capable` refuses. Capability never rewrites the resolved mode — it ANDs with it downstream, which is why it is not an `Origin` value | — (surfaces as DEGRADED / FAIL-CLOSED, not as an origin) |
| 5 | Test override | Tests build the `Flags` value directly via `rollout.ForTest(t, rollout.WithBeadsConditionalWrites(rollout.Require), ...)` and never call `Resolve`. There is no global for an override to fight, so no precedence conflict is expressible (section 9) | — |

Reality vetoes intent; it never quietly wins. Intent (layers 1–3) is what doctor reports as the resolved mode; the veto is reported separately, per store.

### 5.2 `Resolve`: signature and invocation contract

```go
package rollout

type ResolveOptions struct {
    // LookupEnv is injected for testability; nil means os.LookupEnv.
    // No env read ever happens at package init (TestNoLeakVectorReadsAtPackageInit).
    LookupEnv func(key string) (string, bool)
}

// Resolve computes the immutable Flags value for this process from the
// already-merged config plus env overrides. It returns an error — and the
// caller MUST treat it as fatal at startup — when an env override on a
// correctness-category flag is unparseable (5.4).
func Resolve(cfg *config.City, opts ResolveOptions) (Flags, error)

// Flags is an immutable value. Notices produced during resolution are
// retained on it for the life of the process (5.6) and rendered by
// doctor/status; they are never only a startup stderr line.
func (f Flags) Notices() []Notice
func (f Flags) Origin(key string) Origin
```

`Resolve` is folded into the shared config loaders (`loadCityConfig` and its `loadCityConfig*` variants in `cmd/gc/cmd_agent.go`), so `cfg` and `Flags` travel together as one value into every command path — resolution correctness does not depend on per-command discipline across the ~30 load sites. (Threading from there into stores is section 6; this section only pins that there is exactly one `Resolve` call per process, at config-load time.)

### 5.3 Origin: three values, honestly scoped

```go
type Origin string

const (
    OriginBuiltin Origin = "builtin" // field zero-valued everywhere, env unset
    OriginConfig  Origin = "config"  // field set in the merged config, env unset/inapplicable
    OriginEnv     Origin = "env"     // env override applied
)
```

These are the only three values recoverable from `Resolve`'s inputs with **zero loader changes**: the merged `config.City` plus one env lookup. Per-layer provenance (`pack` vs `city` vs `fragment`) does not exist in the compose pipeline today — `mergeFragment` is destructive last-writer-wins and `Provenance` tracks only imports/agents/rigs — so claiming finer origins would require new `compose.go` plumbing for a diagnostic nicety. We defer it (and the `/v0/config/explain` extension that would render it) until an operator actually asks "which fragment set this," and cost it as new provenance plumbing then. `builtin|config|env` answers the one break-glass audit question that matters: *is a forgotten env var pinning this flag?*

Origin travels with the value: every refusal, degrade diagnostic, doctor row, and (later) status-wire entry carries `mode` + `origin` together, e.g. `conditional_writes=off (env: GC_BEADS_CONDITIONAL_WRITES)`.

### 5.4 Env grammar: mode names only, fail-fast on garbage

Each Spec declares at most one override var (`Spec.EnvOverride`, `GC_*`-prefixed, registered in `testenv.LeakVectorVars` — enforced by a registry test so a live agent-session value can never leak into test processes).

**Grammar is per value-kind, and deliberately narrow:**

- `rollout.Mode` flags accept **only the literal mode names**: `off`, `auto`, `require`. No truthy spellings — `1`, `true`, `on`, `yes` are all parse errors for a tri-state. A boolean spelling cannot express which of three states the operator meant, and a typo'd truthy value must never be able to downgrade `require` silently.
- `*bool` kill-switch flags accept `strconv.ParseBool` spellings.

**Failure behavior splits by category:**

- **Correctness categories (`infra-rollout`, `infra-migration`): unparseable env value ⇒ the process refuses to start.** `Resolve` returns an error naming the variable, the raw value, and the accepted grammar:

  ```
  rollout: GC_BEADS_CONDITIONAL_WRITES="disable" is not a valid value; accepted: off|auto|require
  ```

  Rationale: the env var on these flags exists as break-glass (5.5). A break-glass that silently no-ops at 2am — one ignored warning line in a journal nobody is tailing while the operator believes the flag flipped — is a failed break-glass. Starting in the wrong mode is strictly worse than not starting.
- **`infra-killswitch`**: an unparseable value records an `invalid-env-ignored` Notice and keeps the config-resolved value (the existing `GC_EVENTS_ROTATION_ENABLED` behavior at `cmd/gc/providers.go:998-1002`). Kill-switches gate non-correctness machinery; refusing startup over them is disproportionate.

This is the *env* grammar only. Invalid values in **config** never reach `Resolve` at all: load-time enum validation (section 4) rejects them fatally, so accessors and the resolver only ever see `""` or a validated member.

### 5.5 `EnvSemantics`: per-Spec precedence, no retroactive changes

```go
type EnvSemantics string

const (
    EnvOverrides EnvSemantics = "overrides" // env beats explicit config (break-glass); default for new flags
    EnvFillsNil  EnvSemantics = "fills-nil" // env applies only when config left the field unset
)
```

The codebase today ships both precedences and they contradict each other: `GC_DOLT_AUTO_GC_ENABLED` fills only when config is nil (`cmd/gc/dolt_start_managed.go:973` — explicit config wins), while `GC_EVENTS_ROTATION_ENABLED` overrides. We do **not** "unify" these as a migration side effect. Absorbing a legacy flag into the registry preserves its existing precedence via its Spec's `EnvSemantics` — `GC_DOLT_AUTO_GC_ENABLED` registers as `fills-nil` — because flipping a live operator's precedence silently is exactly the class of behavior change this subsystem exists to prevent (an operator with `auto_gc_enabled = false` in `city.toml` and a stale `=1` in a supervisor wrapper would get auto-GC re-enabled on upgrade with zero config diff). Unifying a legacy flag onto `overrides` is a separate, release-noted breaking change with a doctor callout, never a migration footnote.

New flags default to `overrides`, because for them env exists only as break-glass, and a break-glass that cannot override explicit config isn't one.

**The CAS flag keeps its env var, with a named consumer.** `GC_BEADS_CONDITIONAL_WRITES` (`overrides`) is justified not by local operators — for a process-latched flag, exporting a var and restarting costs the same as editing `city.toml` and restarting — but by deployments with baked, immutable config, where the unit environment is the only injectable surface. That consumer is real today; the var ships in slice 1.

```toml
# city.toml — the operator's declared intent
[beads]
conditional_writes = "require"
```

```bash
# incident break-glass in the controller's unit env — wins, per-process, until restart-with-cleanup
GC_BEADS_CONDITIONAL_WRITES=off
```

### 5.6 Env-contradicts-config is push-loud, and Notices outlive startup

When a **valid** env override changes the value of a flag that is **explicitly set in any config layer** (origin would have been `config`), `Resolve` does three things:

1. Records an `env-override-contradicts-config` Notice on the `Flags` value, carrying key, config value, env value, and var name.
2. The composition root emits a **startup structured log line** echoing the effective resolution: `conditional_writes=off (env GC_BEADS_CONDITIONAL_WRITES) overriding explicit city.toml value "require"`.
3. The daemon fires a **typed registered event** (`events.RegisterPayload`) so the divergence lands in event history and is alertable — not merely discoverable by an operator who thinks to run doctor.

Env override set but config silent (origin would have been `builtin`) records a plain `env-override-active` Notice with no event — nothing was contradicted.

**Notice lifetime is part of the contract**: `Resolve`'s Notices live on the `Flags` value held by `controllerState` for the whole process lifetime, and doctor/status render them verbatim. A journal rotation three weeks after boot must not erase the only record of *why* the effective mode is what it is. Boundary test: start with a contradicting env var, query doctor against the running daemon, assert the notice renders.

**Break-glass scope is per-process, and we say so.** `gc` is a multi-process system (controller daemon, agent-invoked `gc hook --claim`, supervisor children with curated env); an env override affects only the process that reads it. The supported whole-city change is config edit + restart. We make cross-process divergence *visible* rather than impossible: every refusal and degrade diagnostic carries `mode` + `origin`, so a controller writing at `off (env)` while a CLI path refuses at `require (config)` is attributable from the first log line of either side. (Restricting `EnvOverride` to the daemon entry point was considered and rejected for v1 — it complicates the resolver with entry-point awareness for a divergence the above already surfaces; revisit if a real bifurcated incident occurs.)

### 5.7 Latching: v1 is process-latched, everywhere

**Every flag in v1 latches at process start. There is no `Latch` field on `Spec`** — it was cut as YAGNI: no reload-tolerant flag exists yet, and a per-flag latch axis would ship untested machinery whose failure mode is the exact corruption the CAS flag prevents.

The operational definition, pinned so the reload path cannot subvert it:

- `controllerState` retains the boot-resolved `Flags` value.
- The config hot-reload path (`controllerState.loadCurrentConfigSnapshot`, `cmd/gc/api_state.go:1803`) **carries the boot snapshot forward into every later-constructed component**. It never hands a re-`Resolve`d mode to a store or consumer constructed after reload. Stores are born lazily and continuously in the controller (per-rig stores, drain member stores); without whole-process latching, one routine `city.toml` edit would put a legacy (unconditional) writer and a CAS writer inside the same process racing on `gc.control_epoch` — the precise mid-run mode flip the latch exists to make impossible. "Epoch-fence semantics never change under in-flight work" is only true if *process* is the latching unit.
- When the on-disk config now diverges from the latched value, the reload records a persistent `pending-restart` Notice:

  ```
  pending restart: conditional_writes require (city.toml) != off (latched at start)
  ```

  surfaced as a **doctor WARNING** and, once the status wire lands (section 10), on the wire. The operator learns the edit did not take effect *and* what will change on the next restart — no silent divergence between file and behavior in either direction.
- `ResolveOptions` (the injected `LookupEnv`) threads into the reload seam, so reload behavior is unit-testable with a map-backed fake and no `t.Setenv`.

**Regression test (ships with the subsystem, not with the first consumer):** boot with `conditional_writes = "off"`, rewrite `city.toml` to `"require"`, trigger `loadCurrentConfigSnapshot`, construct a new store through the factory, assert the store receives `Off` and the `pending-restart` Notice fired.

When a concrete reload-tolerant flag eventually exists, reload semantics come back as a designed feature — with per-component snapshot-generation visibility so doctor can never report a value a still-running component provably isn't using. Until then, restart is the only mode transition, and that is a feature.

## 6. Caller API and threading

The failure mode this section exists to kill is *wiring drift*: a resolution API that is correct in every unit test but skipped on one production path, silently yielding the zero value. cmd/gc has no `run()` choke point — config is loaded independently at ~30 sites (`cmd_hook.go`, `cmd_sling.go`, `cmd_formula.go` ×6, `beads_provider_lifecycle.go` ×4, `apiroute.go`, ...), which is exactly how `applyFeatureFlags` grew 8 scattered call sites. So the design does not ask commands to remember anything. Resolution happens inside the shared loaders, the mode is stamped where stores are born, and consumers hold no mode at all.

### 6.1 Resolve lives in the shared config loaders

`rollout.Resolve` is folded into the loader family in `cmd/gc` (`loadCityConfig`, `loadCityConfigFS`, `loadCityConfigWithBuiltinPacks`, `loadCityConfigWithoutBuiltinPackRefresh*`, `loadCityConfigForEditFS`, `loadCityConfigAllowMissingProviderReferences` — all funnel through one internal helper). The loader return signature changes so cfg and Flags travel as one value:

```go
// cmd/gc — the ONLY production call path into rollout.Resolve.
func loadCityConfig(cityPath string, warningWriter ...io.Writer) (*config.City, rollout.Flags, error)

func loadCityConfigWithBuiltinPacks(cityPath string, includes ...string) (*config.City, rollout.Flags, *config.Provenance, error)
```

Changing the arity is the enforcement mechanism: the compiler visits every one of the ~30 load sites in the migration PR, and no future load site can come into existence without deciding what to do with `Flags`. Paths that provably construct no stores (config-edit tooling) discard it with `_`; everything else threads it. Production passes `ResolveOptions{}` (nil `LookupEnv` → `os.LookupEnv`); the reload seam threads an injected `LookupEnv` (section 8). Resolve's `[]Notice` is retained **on** the returned `Flags` value — not printed-and-dropped — so doctor and the status wire can render origin/invalid-env/pending-restart facts for the life of the process.

`api.Server` receives the boot-resolved `Flags` through its `State` at construction and never re-resolves. It reads `state.Flags()` for *rendering only* (status wire, section 11); the mode itself acts below the API layer, at store construction. The `syncFeatureFlags(state.Config())` calls at `server.go:197/203` are the named dual-root anti-pattern this retires (deleted in stage 5); the CAS flag never acquires a second resolution root at all.

### 6.2 `rollout.Flags`: immutable value, one typed accessor per flag

```go
package rollout

// Flags is an immutable snapshot of every registered flag, resolved once
// per process at config load. It is a value type: copy it, thread it,
// never point at it from a package-level variable.
type Flags struct {
    beadsConditionalWrites resolved[Mode] // {value Mode; origin Origin}
    formulaV2              resolved[bool]
    notices                []Notice
}

func (f Flags) BeadsConditionalWrites() Mode   // typed; no string keys anywhere
func (f Flags) OriginOf(key string) Origin     // builtin | config | env (doctor/status only)
func (f Flags) Notices() []Notice
```

One exported accessor per flag, generated alongside a paired `rollout.WithBeadsConditionalWrites(Mode)` ForTest option (section 9). No `Get(key string)`, no map. Deleting a flag deletes its accessor and its With\* option, and the compiler finds every consumer — production and test corpus alike. Flag removal is a compile-enforced operation, which is the anti-rot property the boilerplate buys.

There is no package-level state behind any of this: no `atomic.Bool`, no `SetX()`, no `sync.Once` holding values. The `formula.SetFormulaV2Enabled` / `molecule.SetGraphApplyEnabled` global-setter bridge (`compile.go:632`, `graph_apply.go:30`) and the `formulatest.LockV2ForTest` mutex are the anti-pattern this deletes; the stage-1 freeze test (section 10) prevents new recruitment while stage 5 migrates them.

### 6.3 One home for the mode: the beads factory stamps every store

The conditional-writes mode has exactly one production home — `OpenStoreAtForCity` (`internal/beads/factory.go:77`). `StoreOpenOptions` gains the field; the factory stamps it onto every store it opens:

```go
type StoreOpenOptions struct {
    ScopeRoot string
    CityPath  string
    Provider  string
    // ... existing fields ...

    // ConditionalWrites is the resolved city-global mode, stamped onto
    // every store this open produces. Latched for the store's lifetime.
    ConditionalWrites rollout.Mode
}
```

Every store type in `internal/beads` (BdStore, FileStore, MemStore, ExecStore, NativeDoltStore) carries the stamped mode as unexported instance state set at construction; `CachingStore` delegates to its backing store. There is **no** caller-facing `WithConditionalWrites` option — that shape is deliberately inexpressible. With a per-store option plus a mode parameter on the seam, tests could wire `store=Require / seam=Off`, a state production can never reach; with factory stamping and a parameterless seam, the divergence cannot be written down.

`rollout.Mode`'s zero value is `ModeUnset`, distinct from `Off`. The factory maps unset → `Off` **and** records it in the store-open `BeadsDiagnostic` (`PreflightGate: "conditional_writes", PreflightReason: "mode not threaded; defaulted to off"`). An unthreaded open path therefore behaves exactly like today's default — it can never *raise* enforcement — but it is visible in doctor and greppable in tests rather than silently indistinguishable from a deliberate `off`.

### 6.4 `ResolveConditionalWriter(store)`: nothing to pass, nothing to get wrong

```go
// internal/beads. The single tested composition point of policy × capability.
// The mode is read from the store's factory stamp; there is no mode
// parameter, so callers cannot contradict the store.
func ResolveConditionalWriter(store Store) (ConditionalWriter, *BeadsDiagnostic, error)
```

Return contract (semantics detailed in section 7): `Off` → `(nil, nil, nil)`, caller takes the byte-identical legacy path; `Auto`∧capable → writer; `Auto`∧incapable → `(nil, diagnostic, nil)` with the once-latched degrade event; `Require`∧incapable → typed error, fail closed. The stamp is read through an unexported interface implemented by every store type (compile-asserted with `var _`); wrappers forward it. Because the interface is unexported, only `internal/beads` can implement it — no consumer can synthesize a differently-moded store.

#### 6.4.1 As-built amendments (2026-07-11, PR-S2b — S2-T10/T11/T12)

The build surfaced one hard compiler fact and four deliberate deviations, all
settled in the PR-S2b bounded design pass and red-teams. This section is the
write-back; where it contradicts §6.3/§6.4/§7.3/§12.2 above, this section wins.

1. **`internal/beads` cannot import `internal/rollout` — the mode type lives
   in `internal/rollout/gate`.** rollout imports config, and config
   transitively reaches beads (config → orders → beads), so §6.3's
   `ConditionalWrites rollout.Mode` is an import cycle as written. The
   consumer-facing half (Mode, ParseMode, Capability, Decision,
   ResolveCapability) moved to the stdlib-only leaf package
   `internal/rollout/gate`; rollout re-exports it all via type/const aliases,
   so `rollout.Mode` and `gate.Mode` are one identical type and everything in
   §5 is unchanged. `TestRolloutImportBoundary` allowlists the subpackage and
   holds `gate/` itself to stdlib-only.
2. **The stamp is one embedded struct with its own mutex, and the stamp write
   reports whether it landed.** Every package-beads store type embeds
   `condWritesStamp` (mode + defaulted marker + degrade-once latch, one
   mutex); FileStore inherits through `*MemStore`, DoltliteReadStore through
   `*BdStore` (with a prober shadow — F2), CachingStore carries nothing and
   delegates carrier + prober to its backing, reporting `landed=false` when
   the backing cannot carry a mode so the factory logs the drop instead of
   believing it took. §12.2's "same mutex as the capability latch" wording
   assumed only BdStore; the generalized stamp owns its own mutex (Mem/File/
   doltlite have no capability mutex), disjoint from `condWriteMu`, no
   nesting. The at-most-once emission guarantee is unchanged.
3. **The seam returns the degrade diagnostic on EVERY call**; the once-latch
   (`noteConditionalDegradeOnce`) ships tested but unwired, for the stage-3
   emitter only. First-call-only diagnostics would make resolution
   order-dependent hidden state.
4. **The factory's unset→Off default is logged at debug, not recorded on
   `BeadsDiagnostic`** — that struct is on the HTTP wire (`StatusResponse`),
   and in the inert stage every open is unthreaded, so §6.3's diagnostic
   record would stamp a transitional condition onto every `gc status`
   fleet-wide as permanent wire vocabulary. Wire visibility for per-store
   verdicts lands with §12.5. The SEAM's returned diagnostic reuses the
   existing PreflightGate/PreflightReason fields on a fresh value and never
   rides the status wire.
5. **exec.Store stays unstamped** (separate package cannot implement the
   unexported carrier; it implements no conditional writes either way), and
   **`beadstest.WithStampedMode`/`OpenMem` (§7.3's idiom) is deferred** to the
   first external consumer (sqlite S2-T9 or stage-3 entry-point tests) — S2b
   seam tests live in package beads and stamp directly. Both are
   enforcement-lowering-only gaps by construction; the stage-3 sweep must
   revisit exec if a Require deployment ever runs an exec provider.

Stage 3 as built (same change series):

- **Wrappers declare resolution targets** instead of forwarding the carrier:
  the exported `ConditionalWritesResolveTargeter` lets an interface-embedding
  wrapper (the typed class wrappers, the cmd/gc policy store) point the seam
  at its inner store; the seam follows targets bounded and cycle-safe, and
  CachingStore's forwarding follows its backing's target too (the production
  sandwich is cache → policy wrapper → stamped store). The mode stays
  unforgeable — a wrapper can redirect resolution, never supply a mode.
- **Threading is the shared open helper, not the §6.1 loader arity change:**
  `openStoreResultAtForCity` (behind every CLI/runtime open and the
  controller's city store) resolves per-process from the config it already
  loads; `openRigStore` threads the controller's boot latch; the control
  dispatcher's bd stores route through the factory (no preflight checker →
  bd fallback by construction, never native). The remaining out-of-factory
  constructions are read-only/diagnostic paths, safe as unset→legacy.
- **C6 and C4 are live** per §9.1/§9.2 with the ambiguity contract (§9.3):
  drain claim/release value-CAS with self-win re-reads; Attach's CAS-last
  epoch fence with the loser feeding the existing partial-attach recovery
  (`findExistingAttach` now prefers a live root over a fence loser's
  neutralized one under the same key); `syncControlEpochToAttempt` and
  `advanceAttachEpochIfNeeded` fenced with benign-loss semantics, all
  bounded to one re-issue (never unbounded retry).
- **The degraded event is emitted**, latched once per store: the factory
  callback reaches the controller's event provider on rig stores and a
  lazily-constructed city event-log recorder on the shared CLI/control
  paths (built inside the once-latched callback, so routine opens pay
  nothing).
- **The sqlite graph store (S2-T9) does not exist on this lineage** — the
  provider was removed on mainline (#3151) and hard-errors at config load;
  the graph class resolves to the primary store, which is covered. The §10
  sqlite deliverable applies only to branches that still carry that store.

#### 6.4.2 Review-response amendments (2026-07-14, local review at 8329a6257)

The pre-merge local review (REQUEST CHANGES, 12 findings) drove a second
as-built pass. Where these contradict §6.4.1 or earlier sections, these win.

1. **Cache rule: forward and EVICT — never patch, never adopt.** The
   write-through/refresh-adopt behavior §6.4.1 shipped is gone on BOTH sides
   of a fenced write. The backend does not return the committed row, so a
   post-write refresh cannot be attributed to our write — installing anything
   derived from local knowledge fabricates a snapshot that never existed at
   that revision. Success evicts; precondition failure and CAS exhaustion
   evict; gate refusal/unsupported touch nothing; ambiguous errors mark
   dirty. The refresh, when it succeeds, feeds the change notification
   verbatim and nothing else (`caching_store_conditional.go`).
2. **Require refuses unstampable opens; auto degrades loudly.** The factory's
   carrier-less/not-landed paths (`unstampableResult`) now fail closed under
   require with `ConditionalWritesRequiredError` instead of logging and
   proceeding; under auto they warn and fire the degrade callback directly.
   "Require fails open through a store that cannot carry the mode" is now
   inexpressible.
3. **Attach candidates are created SPECULATIVELY under an active fence.**
   Steps instantiate with `DeferAssignees` and the
   `gc.attach_fence_pending` marker; only the fence winner activates
   (`activateAttachCandidate`), the loser is neutralized with propagated
   errors, and `findExistingAttach` recovers pending-only states
   deterministically (smallest bead ID wins). No candidate is runnable
   before the fence verdict.
4. **Fence loss is a convergent transient, not a terminal failure.**
   `molecule.ErrEpochConflict` is wrapped transient at the dispatch
   boundary; `IsTransientControllerError` also accepts CAS exhaustion and
   unsupported. `ConditionalWritesRequiredError` stays terminal by design —
   a require refusal never converges by retrying. Drain reservation failures
   are classified by `retryableDrainReservationError` before the control is
   closed.
5. **The enum is validated at config load** (`validateConditionalWrites` in
   `config.Parse`): a typo can never mean "off". The §6.3 claim that the
   string is mapped "in internal/rollout — never here" is amended: rollout
   still owns the resolve, but load rejects unknown spellings.
6. **Require is preflighted and observable at boot**: the controller probes
   every owned store's resolution eagerly (`preflightConditionalWrites`,
   ERROR line per incapable store) and logs the resolved-flag notices. The
   §12.5 status-wire surface remains future work.
7. **FileStore revisions are downgrade-safe**: `revisions_sealed` marks
   files whose Revisions map is authoritative; unsealed files (written by an
   older binary that dropped the map) re-seed deterministically at
   `revisionContinuityFloor` (2^40, above any plausible prior revision), so
   a downgrade/upgrade cycle can never resurrect a stale fence.
8. **Backend contract pins**: empty `UpdateOpts` is a typed error
   (`ErrEmptyConditionalUpdate`) on every store — never a silent no-op or a
   fence skip (conformance row); BdStore's emulation does a final-lap
   re-read before declaring exhaustion; the degrade event maps the
   build-tagged `*beads.DoltliteReadStore` to wire kind `bd`; the
   graduation forcing function now also validates the REAL `deps.env`
   (anchor floor vs `BD_PREV_VERSION`) the moment it arms.

#### 6.4.3 §12.5 status wire as built (2026-07-15, post-merge follow-up)

The status-wire half of §12.5 shipped standalone (the C2 ride-along never
happened; C2 is still blocked on the beads lib bump):

- `StatusBody.conditional_writes` carries `StatusConditionalWrites`
  (mode/origin/effective + per-store `StatusConditionalWriteStoreVerdict`
  rows + `StatusRolloutNotice` mirror of rollout.Notice). Effective severity
  order: fail_closed > degraded > pending_restart > active; off
  short-circuits with no store rows (notices still travel).
- The verdicts come from `beads.InspectConditionalWrites` — a
  side-effect-free reader of the stamp and the probe/latch memos. It NEVER
  runs the four-verb probe: a status poll costs zero subprocesses, and an
  unexercised store honestly reports `probe=unprobed`. Probe and latch stay
  independent so §12.6's skew states render as written
  (probe=capable latch=incapable → "restart to re-probe").
- `gc status` renders the block (silent when off with no notices) and
  includes it verbatim in `--json`. The local no-controller fallback path
  carries no block — a stopped daemon has no latched state to show; doctor's
  §12.1 local re-resolve remains that path's surface. The §12.5
  doctor-queries-live-API switch is still open.

### 6.5 What each layer holds

| Layer | Holds | Never holds |
|---|---|---|
| cmd/gc loaders | `cfg` + `Flags` (resolved once, together) | — |
| beads factory | `Mode` (from `Flags`, stamped per store) | the full `Flags` |
| stores | latched mode, instance state, dies with the store | config, env |
| dispatch / molecule / API consumers | store handles (`graphBeadStore()`, `drainMemberOwningStore(member)`) | any mode value |
| `api.Server` | boot `Flags` snapshot, render-only | a re-resolve path |

The payoff of the bottom two rows: C4 and C6 call `ResolveConditionalWriter` on whatever store they already hold. They cannot be handed the wrong mode because they are never handed a mode. Per-store capability heterogeneity (sqlite graph store vs. a rig's bd store) is handled where it exists — on the store — not threaded through consumer options.

### 6.6 Entry-point tests: the wiring is the contract

Seam tests prove the seam; they say nothing about whether a command reached it (the `routeReadCmd` lesson). Stage 1 lands one entry-point test per CAS-relevant command — **controller, hook, sling, api server** — each asserting that `require` in a real temp `city.toml` is observed at the bd wire by a probe write:

```toml
# t.TempDir() city
[beads]
conditional_writes = "require"
```

```go
func TestHookClaimObservesConditionalWritesRequire(t *testing.T) {
    cityDir := writeTempCity(t, requireCityToml)
    runner := newRecordingRunner(t,
        withHelpAdvertising("--if-revision"), // capability probe passes
    )
    // Drive the real command entry point (not the seam) against cityDir,
    // store construction routed through the factory with the fake runner.
    runHookClaim(t, cityDir, runner)

    argv := runner.lastWriteArgv()
    if !slices.Contains(argv, "--if-revision") {
        t.Fatalf("hook claim wrote unconditionally under require: %q", argv)
    }
}
```

These four tests are the regression net for the exact bug class the loader-folding and factory-stamping exist to prevent: a command path that loads config but drops `Flags`, or opens a store outside the factory. Any such path fails here — with `require` visibly not observed — instead of shipping as a silent `Off` writer against a fleet whose config promises fencing.

## 7. Testability

The subsystem is testable by construction, not by discipline. Every seam is per-instance and typed; there is no package-level `atomic.Bool`, no `SetX()`, no save/restore idiom, no `t.Setenv`, and no state a parallel test can observe from a sibling. This section specifies the five seams, the conformance suite that keeps fakes honest, and the named regression tests that are merge gates.

### 7.1 Value seam: `rollout.ForTest` with typed `With*` options

Tests never call `Resolve`. They construct the immutable `Flags` value directly:

```go
// internal/rollout/fortest.go

// ForTestOption sets one flag on a Flags value under construction.
// Exactly one With* constructor exists per registered flag, generated
// alongside the flag's Flags accessor in the same file.
type ForTestOption func(*flagsBuilder)

// ForTest builds Flags from the canonical registry's defaults plus
// explicit typed overrides.
func ForTest(tb testing.TB, opts ...ForTestOption) Flags

// WithBeadsConditionalWrites overrides beads.conditional_writes.
func WithBeadsConditionalWrites(m Mode) ForTestOption
```

```go
flags := rollout.ForTest(t, rollout.WithBeadsConditionalWrites(rollout.Require))
store := beadstest.OpenMem(t, beadstest.WithStampedMode(flags.BeadsConditionalWrites()))
```

Properties this buys, each deliberate:

- **Compile-time flag removal.** Deleting a flag deletes its `Flags` accessor *and* its `With*` option in the same file. The compiler then finds every production call site **and every test**. There is no string-keyed override path, so the "forty tests fail one by one at runtime with unknown-key errors" cleanup mode does not exist.
- **Structural isolation.** `Flags` is a value handed to the constructor under test. Two `t.Parallel` tests with opposite modes cannot observe each other because nothing is process-scoped. This retires the pattern it replaces: the ~20 save/restore blocks in `molecule_test.go` and the `formulatest.LockV2ForTest` serializing mutex exist only because `SetFormulaV2Enabled` is a package global (deleted in stage 5).
- **No registry mutation from subsystem tests.** The canonical `[]Spec` is unexported behind a read-only accessor. The registry validator and the `Flags` builder both take a `[]Spec` parameter, so `internal/rollout`'s own tests (e.g. "validator rejects a Spec with no Owner") construct **local synthetic registries** as local values:

```go
func TestValidatorRejectsMissingOwner(t *testing.T) {
    t.Parallel()
    specs := []rollout.Spec{{Key: "x.y", Category: rollout.InfraRollout /* no Owner */}}
    err := rollout.ValidateSpecs(specs)
    // ...
}
```

A panicking or forgetful test can never leak a phantom Spec into a parallel sibling's `ForTest` defaults, because there is no shared slice to append to.

### 7.2 Resolver seam: injected `LookupEnv`, `LeakVectorVars` enforced

`Resolve` never touches `os.LookupEnv` directly:

```go
type ResolveOptions struct {
    // LookupEnv defaults to os.LookupEnv when nil. Tests inject a
    // map-backed fake; no test in the repo calls t.Setenv for a flag var.
    LookupEnv func(key string) (string, bool)
}
```

```go
env := map[string]string{"GC_BEADS_CONDITIONAL_WRITES": "require"}
flags, notices, err := rollout.Resolve(cfg, rollout.ResolveOptions{
    LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
})
```

Unit tests against the map fake cover the full precedence and grammar matrix without process-env mutation (which would also panic under `t.Parallel`):

| Case | Assertion |
|---|---|
| env unset, config unset | default; Origin `builtin` |
| env unset, config `require` | `Require`; Origin `config` |
| env `off`, config `require` | `Off`; Origin `env`; env-contradicts-config startup log + typed event emitted |
| env `1` / `true` / `disable` on a Mode flag | `Resolve` returns an error (startup fails fast) naming var, raw value, and the `off\|auto\|require` grammar |
| env set, flag has `EnvSemantics: fills-nil`, config explicitly set | config wins (legacy-precedence preservation, tested per absorbed flag) |

Two enforcement tests close the leak vectors:

- **`LeakVectorVars` registration.** `GC_BEADS_CONDITIONAL_WRITES` is registered in `internal/testenv`'s `LeakVectorVars`, so the testenv gate scrubs it from every test process — a live agent-session export can never silently flip a test's resolution. A registry test asserts the invariant generically: *every non-empty `Spec.EnvOverride` appears in `LeakVectorVars`*, so a future flag cannot forget it.
- **Frozen `GC_*` baseline.** The stage-1 inventory test fails on any new `os.Getenv`/`os.LookupEnv` site matching `"GC_"` outside testenv gates, registry `EnvOverride`s, and the checked-in baseline — so a shadow env flag cannot appear without a loud, reviewed baseline diff.

### 7.3 Capability seam: instance toggles, never interface-stripping wrappers

The `withoutConditionalWrites(store)` wrapper pattern is **banned**. A wrapper struct hides *every* optional interface, not just the one under test — `internal/beads` has at least five type-asserted capabilities (`ConditionalAssignmentReleaser`, `AtomicTxStore`, `StorageCreateStore`, `StorageGraphApplyStore`, `ParentProjectionWaiter`), and `class_store.go:15` already documents the in-tree bite: optional interfaces are not promoted through embedding. A test meaning to flip one axis would silently flip five, and e.g. `CachingStore`'s graph-apply fallback would take a branch production never pairs with CAS-incapable stores.

Instead, capability absence is a per-instance field on the fakes:

```go
// internal/beads/mem_store.go
type MemStore struct {
    // DisableConditionalWrites makes every ConditionalWriter method
    // return ErrConditionalWriteUnsupported while the interface set —
    // including all other optional capabilities — stays intact.
    DisableConditionalWrites bool
    // ...
}
```

`FileStore` gets the identical toggle. This drives the `auto`-degrade and `require`-fail-closed matrix cells deterministically:

```go
mem := beadstest.OpenMem(t, beadstest.WithStampedMode(rollout.Auto))
mem.DisableConditionalWrites = true
w, diag, err := beads.ResolveConditionalWriter(mem)
// assert: w == nil, diag.PreflightGate == "conditional_writes", err == nil (loud degrade)
```

Capability-absent-*by-interface* (a store type that genuinely lacks the methods) is tested only where it is real, with a purpose-built minimal store type in the test file — never by wrapping a full-featured store.

Note the shape `ResolveConditionalWriter(store)` — **no mode parameter**. The mode is stamped onto the store by the factory (section 5), and tests stamp it through the same entry point (`beadstest.WithStampedMode`, which calls the factory's internal stamping path). The formerly-possible contradiction — store constructed at `Require`, seam called with `Off` — is now a state tests *cannot express*, so the suite can no longer accumulate green coverage of unreachable production states.

### 7.4 Classifier and ambiguity tests: one seam, the fake `CommandRunner`

There is exactly one injection point for everything bd-shaped: the store's existing injected `CommandRunner` (the `bdReadyProjectionEnabled` shape — `s.runner(s.dir, "bd", ...)`). The lazy capability probe, the exit-code classifier, and the CAS retry policy all run through it. There is **no** `WithBDCapabilityProbe`; with a single seam, a fake probe and a fake runner can never contradict each other, and the previously-possible "capable probe, exit-13 runtime" hybrid is unconstructable.

The fake is a scripted runner keyed on argv:

```go
type scriptedRunner struct {
    t     *testing.T
    calls []scriptedCall // matched in order or by argv predicate
}

type scriptedCall struct {
    match  func(args []string) bool
    stdout string
    exit   int   // 0 = success
    err    error // non-ExitError transport failures (i/o timeout, broken pipe)
    apply  func() // mutates fake backing state BEFORE returning err — "committed but ambiguous"
}
```

The classifier unit-test table, every row driven through this one fake:

| Scripted bd behavior | Required classification |
|---|---|
| exit 9, stdout `{"code":"precondition_failed","expected_revision":4,"current_revision":7}` | `PreconditionFailedError{Expected:4, Current:7}` |
| exit 9, JSON body surrounded by log noise | same — defensive parse tolerates surrounding text |
| exit 13, body `code == "conditional-write-unsupported"` | `ErrConditionalWriteUnsupported`; per-store latch trips (assert a second write skips `--if-revision` classification and reports latched) |
| exit 13, no body / other body code (the beads#3734 close-authority shape) | typed **non-latching** refusal attributed to that write; latch NOT tripped (assert next write still attempts CAS) |
| usage/unknown-flag error mentioning `--if-revision` (what pre-#4682 bd actually emits) | `ErrConditionalWriteUnsupported`; latch trips |
| transport error (`i/o timeout`) with `apply` executed — the write committed | ambiguity contract engages: retry path MUST self-win-check on re-read before concluding loss; asserting a raw re-CAS with the stale expected revision fails the test |
| repeated unrelated-key revision churn during the emulation loop | bounded attempts + backoff, then the typed exhaustion error — distinct from `PreconditionFailed` — surfaces; the loop never spins unbounded |

Probe-specific assertions on the same fake:

- **Laziness:** constructing the store issues zero runner calls; the first conditional write triggers the four-verb help probe (`update`/`close`/`assign`/`delete` — a mid-merge dev bd can support one but not another); the second write issues no probe (memoized under the store mutex, the `readyProjectionChecked` idiom).
- **Nothing persisted:** no test may assert on any on-disk probe artifact, because none exists; a fresh store re-probes (no-status-files).

### 7.5 The `ConditionalWriter` conformance suite: fakes that predict production

Green in-process tests are worthless if `MemStore`'s revision discipline diverges from bd's (#4682's opaque per-bead nonce). A store that bumps revision only on `Update` while real bd bumps on *every* mutation including `assign` would train consumer retry loops to reuse stale revisions — exit-9 livelock in production, 100% green CI. The countermeasure is a store-agnostic conformance suite whose table **is** the interface contract, duplicated verbatim in the `ConditionalWriter` doc comment:

```go
// internal/beads/beadstest/conformance.go

// RunConditionalWriterConformance asserts the revision-discipline and
// CAS-semantics contract documented on beads.ConditionalWriter.
func RunConditionalWriterConformance(t *testing.T, open func(t *testing.T) beads.Store)
```

Rows (each a subtest):

- **Revision bump discipline:** every mutation — update, close, assign, delete-adjacent metadata writes, label edits, `CompareAndSetMetadataKey` itself — bumps `Revision`; reads never do.
- **Exit-9 equivalence:** a stale expected revision yields `PreconditionFailedError` carrying both revisions, on every store, with identical semantics to bd's exit-9 body.
- **Empty-expected semantics:** `CompareAndSetMetadataKey(id, key, "", next)` claims only when the key is absent/empty; a set key yields `PreconditionFailed` with the current value recoverable by re-read.
- **Monotonicity:** revisions strictly increase per bead; no mutation ever reuses or decreases one.
- **Contention:** two goroutines racing one key — exactly one wins, the loser gets `PreconditionFailed`, never a silent double-apply.

Execution matrix:

| Store | Tier |
|---|---|
| `MemStore` | unit CI |
| `FileStore` | unit CI |
| `CachingStore` over `MemStore` | unit CI |
| sqlite graph store | unit CI (blocking deliverable of the C4/C6 PR) |
| `BdStore` against real bd | `//go:build integration`, slotted into the contract-test system (PR #3714) |

The integration leg is the anchor: it is what makes the in-process rows *evidence* rather than self-consistent fiction. If bd's discipline changes, the integration run reds and the doc-comment contract plus all four fakes get updated in one reviewed diff.

**Merge gate: the CachingStore livelock regression.** A MemStore-backed `CachingStore` test in the `ConditionalWriter` PR (stage 2, not deferred to C4): CAS succeeds, the post-write refresh `Get` is scripted to fail once, then a `PreconditionFailed` occurs — assert the cache entry was **evicted** (next `Get` hits the backing store and sees the fresh revision) in both paths, and that an exit-9 retry loop converges rather than re-failing forever on a locally-patched stale revision. This pins EVICT-never-patch against the existing `refreshBeadAfterWrite` optimistic-patch template, which a CAS port must not follow.

### 7.6 Config, merge, and validation tests

- **Accessor tests are pure struct construction** — no loader, no files: `BeadsConfig{ConditionalWrites: "require"}` asserts `ConditionalWritesMode() == rollout.Require`; the zero value asserts the default. A registry test generalizes the latter: for every Spec, a zero-value `config.City`'s typed accessor equals `Spec.Default` — closing the two-homes drift between `registry.go` and the accessor's `""` mapping (this is the test a half-landed graduation PR trips).
- **Hand-written fragment-merge regression, one per flag** (template: the `daemon.formula_v2` special case at `compose.go:1030-1047`): base layer sets `conditional_writes = "require"`; an included fragment defines only an unrelated `[beads]` sibling key (`prefix = "mc"`); assert the merged config still resolves `Require`. This is the test that keeps the whole-table-LWW footgun from silently downgrading a correctness opt-in through routine layering. Deliberately hand-written — the generic reflection merge harness is deleted (zero consumers, known `toml.MetaData` subtleties); a flag opening a *new* section owes its own hand-written test per the lifecycle doc.
- **Load-time enum validation:** `conditional_writes = "requre"` (and `"Require"`, `"required"`) fails config load with an error naming the field, the bad value, and the allowed set — asserted at the `ValidateSemantics` walk, so accessors are proven never to see an unvalidated non-empty value.

### 7.7 The reload regression test (process-latch pinned by test)

The mixed-mode-writers-after-reload corruption class gets a dedicated regression test at the controller-state level:

```go
func TestConditionalWritesLatchSurvivesReload(t *testing.T) {
    // 1. Boot controllerState with city.toml conditional_writes = "off".
    // 2. Rewrite city.toml on disk to "require".
    // 3. Trigger the reload path (loadCurrentConfigSnapshot, api_state.go:1808).
    // 4. Construct a NEW beads store through the post-reload snapshot.
    // 5. Assert the new store's stamped mode is Off (the boot-latched value),
    //    NOT the on-disk Require.
    // 6. Assert a persistent Notice was recorded:
    //    "pending restart: conditional_writes require (city.toml) != off (latched at start)"
    //    and that doctor's rendering path classifies it as a WARNING.
}
```

`ResolveOptions` (with its injected `LookupEnv`) threads into the reload seam, so the reload path's env behavior is unit-testable with the same map-backed fake — without which reload env-precedence would be testable only via `t.Setenv`, which `LeakVectorVars` scrubbing deliberately defeats.

### 7.8 Entry-point tests: threading completeness is tested where it breaks

Seam tests cannot catch an un-threaded production path — the `routeReadCmd` lesson. Since cmd/gc has no single `run()` choke point (config loads at ~30 sites), each CAS-relevant entry point gets a test that goes in through the front door:

```go
// Pattern, one per entry point: controller, gc hook --claim, gc sling, api server.
// 1. Temp city with conditional_writes = "require" in city.toml.
// 2. Invoke the command's real entry path (fake CommandRunner / capability-
//    disabled store behind it).
// 3. Drive one probe write; assert it observes Require — either CAS argv
//    (--if-revision) reaches the runner, or the typed fail-closed refusal
//    surfaces. A silent legacy write fails the test.
```

These four tests are what make "resolution is folded into `loadCityConfig*` and stamped by the factory" a verified property instead of a design intention: a future command path that constructs a store without the shared loader gets the zero-value `Off` and reds the entry-point test for whichever surface it serves.

### 7.9 Suite hygiene

- **`testenv` import:** the new `internal/rollout` test package ships its generated `testenv_import_test.go` (`go run scripts/add-testenv-import.go`) in the same PR — the pre-push hook (`TestRequiresDedicatedTestenvImportFile`) rejects the push otherwise, and targeted `go test` runs will not surface it.
- **Doctor exit contract pinned:** a doctor-level test asserts FAIL-CLOSED (`require` ∧ incapable) and radar-surfaced past-due lifecycle items exit nonzero, and DEGRADED exits 0 — so monitoring integrations cannot drift.
- **Lifecycle tests are deterministic per commit:** the two-stage graduation test compares repo-pinned anchors in `deps.env`; nothing in the merge-blocking path compares against `time.Now()`, so a commit's pass/fail never changes without a diff and `git bisect` stays sound (wall-clock staleness lives in the non-blocking nightly radar).
- **What is deliberately absent:** no `t.Setenv`, no snapshot/restore helpers, no test mutex, no `SetXForTest` package function, no interface-stripping wrapper. If a test needs one of these, the production seam is wrong — fix the seam.

## 8. ConditionalWriter: interface, classifier, and conformance

This section defines the capability axis in `internal/beads`: the optional store interface, its typed errors, the BdStore exit-code classifier and probe, the bounded metadata-CAS emulation, the CachingStore eviction rule, and the conformance suite that pins one revision contract across every store. Mode resolution and consumer-side conflict semantics live in their own sections; everything here is store-level and mode-blind — a store either can do conditional writes or it cannot, and it reports which, loudly, in types.

### 8.1 Interface and typed errors

`ConditionalWriter` is a new optional interface in `internal/beads/beads.go`, modeled exactly on `ConditionalAssignmentReleaser` (beads.go:109) and discovered the same way: type-assert on the **resolved** store at the call site, never on a wrapper.

```go
// ConditionalWriter is implemented by stores that can apply a write only when
// the caller's snapshot of the bead is still current.
//
// REVISION CONTRACT (normative — the conformance suite in
// conditional_writer_conformance_test.go executes this table against every
// implementing store, including real bd under the integration build tag):
//
//   - Every bead carries an opaque int64 revision. Callers may test it only
//     for equality; arithmetic, ordering across beads, and gap inference are
//     all undefined.
//   - EVERY mutation of the issue row bumps the revision: field updates,
//     label add/remove, metadata writes (any key), assign, close, reopen,
//     delete. Reads never bump. Cross-bead writes never bump this bead.
//   - A bead's revision is monotonically increasing for the lifetime of the
//     bead and is never reused.
//
// GRANULARITY CONTRACT: consumers may assume NEITHER value-level nor
// revision-level conflict semantics. Backends differ: sqlite and the native
// library implement CompareAndSetMetadataKey as server-side value-CAS
// (an unrelated-key write does not conflict); BdStore emulates it over
// --if-revision (an unrelated-key write CAN produce a spurious retry
// internally). Callers get the value-CAS RESULT either way, but must not
// build timing or interference assumptions on top of it.
type ConditionalWriter interface {
	// UpdateIssueIfMatch applies opts only if the bead's revision equals
	// expectedRevision; otherwise it returns *PreconditionFailedError.
	UpdateIssueIfMatch(id string, expectedRevision int64, opts UpdateOpts) error
	CloseIssueIfMatch(id string, expectedRevision int64) error
	DeleteIssueIfMatch(id string, expectedRevision int64) error

	// CompareAndSetMetadataKey atomically sets metadata[key] = next iff the
	// current value equals expected. expected == "" matches a key that is
	// absent OR present with the empty value (the two states are
	// indistinguishable to callers; release paths write "" to clear).
	// Returns (true, nil) on swap, (false, nil) on a genuine value mismatch
	// (the caller lost), and (false, err) for everything else.
	CompareAndSetMetadataKey(id, key, expected, next string) (bool, error)
}
```

Typed errors, beside the existing sentinels in beads.go:

```go
// ErrConditionalWriteUnsupported: this store (or the bd behind it) cannot do
// conditional writes. Latching this per store instance is the capability veto;
// no code path in internal/beads converts it into an unconditional write.
var ErrConditionalWriteUnsupported = errors.New("conditional writes unsupported")

// PreconditionFailedError: the write was rejected because the revision moved
// (bd exit 9). Expected/Current come from bd's machine JSON body when
// parseable; zero otherwise (Raw preserves the body for forensics).
type PreconditionFailedError struct {
	ID       string
	Expected int64
	Current  int64
	Raw      string
}

// GateRefusalError: bd refused THIS write for a policy reason (exit 13 whose
// body code is anything other than conditional-write-unsupported — e.g. the
// beads#3734 close-authority guard). Per-write, never latches capability.
type GateRefusalError struct {
	ID   string
	Verb string
	Code string // machine body code, "" if absent
	Raw  string
}

// CASRetriesExhaustedError: BdStore's bounded metadata-CAS emulation ran out
// of attempts under cross-key revision interference. Distinct from
// PreconditionFailedError: the caller did NOT lose the value race; the store
// could not get a clean shot. Consumers back off and re-enter level-triggered.
type CASRetriesExhaustedError struct {
	ID, Key      string
	Attempts     int
	LastRevision int64
}
```

Every implementing store carries a compile assertion (`var _ ConditionalWriter = (*BdStore)(nil)` etc.). Implementations in stage 2: **BdStore** (below), **MemStore** and **FileStore** natively (each with a `DisableConditionalWrites bool` instance toggle whose methods return `ErrConditionalWriteUnsupported` while the interface set stays intact — no hiding wrapper, per the class_store.go:15 optional-interface-promotion lesson), **CachingStore** by forwarding to `c.backing` (§8.5), **NativeDoltStore** by delegating to the beads library's ConditionalWriter (compile-time capability via go.mod), and the **sqlite graph store** as a single conditional `UPDATE ... WHERE revision = ?` (its own blocking deliverable; see the sqlite section).

### 8.2 BdStore: argv building and the exit-code classifier

All `--if-revision` argv construction stays inside `internal/beads` (`TestNoBdExecOutsideBeads` already forbids bd exec elsewhere). The runner already hands back stdout alongside the `*exec.ExitError` (bdstore.go's `classifyBDExecResult` path returns `out` even on failure), so the classifier is a pure function over `(out, err)`:

| Signal from bd | Classification | Latches store incapable? |
|---|---|---|
| exit 9, stdout body parses to `{code, expected_revision, current_revision}` | `*PreconditionFailedError{Expected, Current}` | no |
| exit 9, body unparseable | `*PreconditionFailedError` with zero Expected/Current, `Raw` set | no |
| exit 13, body `code == "conditional-write-unsupported"` | `ErrConditionalWriteUnsupported` | **yes** |
| exit 13, any other or absent body code | `*GateRefusalError` (this write only) | no |
| usage/unknown-flag error mentioning `--if-revision` (what pre-#4682 bd actually emits — it exits with a generic usage error, never 13) | `ErrConditionalWriteUnsupported` | **yes** |
| `isBdAmbiguousWriteError` class (i/o timeout, broken pipe, conn reset) | returned as-is; the write MAY have committed — consumers apply their self-win contract (consumer-semantics section) | no |
| everything else | existing write-error classification (`isBdNotFound` → `ErrNotFound`, etc.) | no |

Two rules are load-bearing:

1. **The exit-13 latch is body-code-gated, not exit-code-gated.** bd has other write-authority gates on exit 13 (the close-authority guard is in production today). Latching on the bare number would convert one policy refusal into a process-lifetime silent degrade of every subsequent fenced write under `auto` — the exact clobber class CAS exists to prevent. A 13 without the machine body code is a per-write `GateRefusalError` and the store stays capable.
2. **Exit-9 body parsing is defensive.** Tolerate surrounding noise (the `extractJSON` idiom already used for `bd sql` output), and degrade to a zero-valued `PreconditionFailedError` rather than misclassifying — a precondition failure with unknown revisions is still a precondition failure.

The classifier is exhaustively unit-tested through the injected `CommandRunner` fake: exit 9 with body, exit 9 with noise-wrapped body, exit 13 with the unsupported body code, bare exit 13, the old-bd `unknown flag: --if-revision` usage string, and an ambiguous error injected **after** the fake has committed the write.

**Retry policy is dedicated and separate from the blind transient loop.** Conditional writes never route through `runBDTransientWrite`/`isBdTransientWriteError` (bdstore.go:1873): replaying a stale `--if-revision N` after a connection error is wrong (the first attempt may have committed and bumped the revision), and blind retry of exit 9 is worse (it converts a signal into a spin). The dedicated wrapper: connection/serialization-class errors re-read the bead's revision before any re-attempt; exit 9 is surfaced to the caller immediately (the caller re-reads and re-decides — that is the whole point of CAS); nothing is ever downgraded to an unconditional write.

### 8.3 Capability probe: lazy, four-verb, one seam

Capability has two axes that doctor renders separately:

- **Probe verdict** — "does this bd parse `--if-revision`?", memoized once per store instance.
- **Runtime latch** — "did a real conditional write come back unsupported?", set by the classifier rows above. The latch is **authoritative over the probe** in both directions of skew (PATH drift, in-place downgrade).

The probe runs through the store's **existing** `CommandRunner` — the same seam `bdReadyProjectionEnabled` uses (`s.runner(s.dir, "bd", "version")`, bdstore_ready_projection.go:69-88). There is deliberately **no** `WithBDCapabilityProbe` option: a second injection seam would let tests wire a capable-probe/incapable-runner hybrid that no deployment can produce. One fake runner controls probe output and per-call exit codes from one place, so probe/runtime consistency is structural in tests.

```go
type BdStore struct {
	// ...
	condWriteMu       sync.Mutex
	condWriteProbed   bool
	condWriteCapable  bool // probe verdict
	condWriteLatched  bool // runtime unsupported latch (authoritative)
}

func (s *BdStore) conditionalWritesCapable() (bool, error) {
	s.condWriteMu.Lock()
	defer s.condWriteMu.Unlock()
	if s.condWriteLatched {
		return false, nil
	}
	if s.condWriteProbed {
		return s.condWriteCapable, nil
	}
	// Lazy: reached on the FIRST conditional write, never at construction.
	for _, verb := range []string{"update", "close", "assign", "delete"} {
		out, err := s.runner(s.dir, "bd", verb, "--help")
		if err != nil || !bytes.Contains(out, []byte("--if-revision")) {
			s.condWriteProbed, s.condWriteCapable = true, false
			return false, nil
		}
	}
	s.condWriteProbed, s.condWriteCapable = true, true
	return true, nil
}
```

Design points, each answering a specific red-team finding:

- **Lazy, not construction-time.** Short-lived CLI paths (`gc hook`) open stores constantly; four `--help` subprocesses at every store open is an unacceptable tax for mode=off or read-only invocations. The probe fires on the first conditional write and is memoized under the mutex, mirroring `readyProjectionChecked`.
- **All four verbs.** The consumers use update, close, assign, and delete; a dev bd mid-merge of #4682 can support one but not another. A single-verb probe would report capable and then eat runtime refusals with doctor showing a clean probe.
- **Help-grep is the interim detector only.** The day beads tags the release containing #4682, the probe switches to `ProbeBDVersion` + `deps.CompareVersions` against a new `bdConditionalWritesMinVersion` anchor in deps.env (added under the `TestBDVersionPins` lockstep) — exactly the `bdReadyProjectionMinVersion` shape. The runtime latch stays authoritative either way; correctness never rests on a version string or help text alone.
- **Nothing is persisted.** Per "no status files — query live state", the probe result and the latch are instance state that dies with the store; a restart re-probes the live bd. Operators upgrading bd in place restart to re-evaluate — doctor's DEGRADED explanation says so explicitly.

### 8.4 CompareAndSetMetadataKey on BdStore: bounded emulation, typed exhaustion

bd's primitive is revision-CAS (`--if-revision N`); the interface promises value-CAS on one metadata key. BdStore emulates: read the bead, check the value, write the key under the observed revision.

The hazard is **cross-key interference**: control and member beads are metadata-hot (controller_error stamps, attempt logs, heartbeats), so an unrelated-key write between the read and the CAS produces a spurious exit 9 even though nobody touched *our* key — and each retry costs a fresh ~100ms+ bd subprocess. The loop is therefore bounded, with a typed exhaustion error that consumers can distinguish from a genuine loss:

```go
const (
	casEmulationMaxAttempts = 4
	casEmulationBaseBackoff = 25 * time.Millisecond // doubles per attempt, jittered
)

func (s *BdStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	var pre *PreconditionFailedError
	for attempt := 1; ; attempt++ {
		b, err := s.Get(id)
		if err != nil {
			return false, err
		}
		if b.Metadata[key] != expected { // ""≡absent per the interface contract
			return false, nil // genuine value loss: the caller lost the race
		}
		err = s.runConditionalWrite(id, b.Revision,
			"update", id, "--set-metadata", key+"="+next, "--if-revision", strconv.FormatInt(b.Revision, 10))
		switch {
		case err == nil:
			return true, nil
		case errors.As(err, &pre): // revision moved; value re-checked next lap
			if attempt == casEmulationMaxAttempts {
				return false, &CASRetriesExhaustedError{ID: id, Key: key,
					Attempts: attempt, LastRevision: b.Revision}
			}
			sleepWithJitter(attempt)
		default:
			return false, err // unsupported / gate refusal / ambiguous: surface as-is
		}
	}
}
```

Exhaustion is **not** `PreconditionFailedError` and not `(false, nil)`: the value never mismatched, so telling the caller "you lost" would strand reservations (the C6 self-win contract depends on the distinction). Consumers treat exhaustion as a transient — back off and re-enter through the level-triggered pass.

**Sidestep under evaluation (stage-2 spike, decided before C4/C6 land):** implement BdStore value-CAS as a single conditional SQL `UPDATE` — the `ReleaseIfCurrent` template at bdstore.go:1097, including its `releaseIfCurrentViaEmbeddedDoltSQL` fallback — with a JSON-path predicate on the metadata column. This eliminates cross-key interference entirely (the predicate tests the *value*, not the revision) at zero subprocess-retry cost. Disqualifier the spike must clear: the raw SQL path bypasses bd's write layer, so the same `UPDATE` must also bump the revision column itself (`revision = revision + 1`) or it breaks the revision contract for every other conditional writer; if bd's schema or the embedded fallback can't guarantee that atomically, the emulation loop remains the shipping implementation and the SQL path is dropped, not half-adopted.

### 8.5 CachingStore: forward, and evict — never patch

CachingStore implements `ConditionalWriter` by type-asserting `c.backing` and forwarding, following the `ReleaseIfCurrent` template at caching_store_writes.go:138 (`ErrConditionalWriteUnsupported` when the backing store doesn't implement it). The cache-maintenance rule diverges from the existing template on purpose:

The existing write path refreshes the bead after a successful write and, when that refresh fails transiently, **optimistically patches** the cached clone. A CAS port of that fallback is poison: the local patch cannot synthesize the new revision, `CachingStore.Get` serves the cached clone, and every consumer's exit-9 recovery then re-reads the **stale** revision through the cache and re-fails — a livelock indistinguishable from real contention.

Rule: **evict, never patch.**

- CAS success + successful refresh → refresh the cache entry (normal path).
- CAS success + **failed** refresh → `delete(c.beads, id)` (and the deps/dirty bookkeeping), forcing the next `Get` to the backing store.
- **Every** `PreconditionFailedError` from the backing store → evict the entry too. The cached revision is proven stale by construction; keeping it guarantees the caller's re-read feeds the next attempt the same dead revision.

The MemStore-backed CachingStore regression test — CAS succeeds, refresh is forced to fail, assert the next Get hits the backing store and a retry loop converges instead of livelocking; plus the PreconditionFailed-evicts case — is a **merge gate of the stage-2 ConditionalWriter PR**, not a pre-C4 follow-up.

### 8.6 Conformance suite: one contract, every store

The revision contract in §8.1's doc comment is only worth what enforces it. The single named failure mode: real bd bumps revision on *every* mutation (assign included); a fake that bumps only on Update trains consumer retry loops to reuse stale revisions after an interleaved assign — green CI, exit-9 livelock in production. So the contract is executable:

```go
// internal/beads/conditional_writer_conformance_test.go
func RunConditionalWriterConformance(t *testing.T, name string, open func(t *testing.T) Store) {
	t.Run(name+"/every_mutation_bumps_revision", ...)      // update, labels, metadata,
	                                                       // assign, close, reopen — full verb matrix
	t.Run(name+"/reads_never_bump", ...)
	t.Run(name+"/revision_monotonic_never_reused", ...)
	t.Run(name+"/stale_revision_is_precondition_failed", ...) // typed, Expected/Current populated
	                                                          // where the backend can supply them
	t.Run(name+"/cas_empty_expected_claims_absent_or_empty_only", ...)
	t.Run(name+"/cas_value_mismatch_is_false_nil_not_error", ...)
	t.Run(name+"/cas_winner_value_visible_to_loser_reread", ...)
	t.Run(name+"/disable_toggle_returns_typed_unsupported_with_interfaces_intact", ...)
}
```

Rows in the matrix:

| Store | Tier | Notes |
|---|---|---|
| MemStore | unit CI | native implementation; also drives the `DisableConditionalWrites` row |
| FileStore | unit CI | native implementation |
| CachingStore over MemStore | unit CI | forwarding + both eviction cases (§8.5) |
| sqlite graph store | unit CI | the conditional-UPDATE implementation; same suite, no special-casing |
| BdStore against real bd | `//go:build integration` | the authority row; slots into the existing Beads↔GasCity contract-test system (PR #3714) so a bd version bump that changes bump discipline fails *here*, not in a production drain |

The doc comment is normative and the integration row verifies bd complies with it; if a future bd diverges, the suite goes red and the contract is amended **consciously** — in the interface comment, in the fakes, and in every consumer's retry assumptions, in one reviewed diff. Divergent granularity (BdStore's emulation vs sqlite's value-CAS) is exercised by the suite only through the caller-visible result surface, matching the granularity contract: no conformance case may assert interference behavior the contract says is undefined.

What deliberately does **not** exist in this section's deliverables: a `withoutConditionalWrites` wrapper (it would silently strip the other five optional store interfaces — the in-tree embedding lesson), a second probe seam, any persisted capability state, and any path — retry wrapper, classifier arm, cache fallback, or conformance shim — that turns `ErrConditionalWriteUnsupported` into an unconditional write.

I have full grounding on the in-tree code. Writing the section now.

## 9. CAS consumers: C4 epoch fence, C6 drain reservation, C2 API

One knob (`[beads] conditional_writes`), three consumers, landed in code order: C6 and C4 ship together in stage 3 (they share the sqlite `CompareAndSetMetadataKey` blocking deliverable), C2 ships in stage 4 with the beads library bump. Each consumer gets a **written contract** in this section — not "re-read and converge" hand-waving — because the red-team demonstrated that every unspecified exit-9 path in these three call sites is a distinct correctness bug (stranded reservations, orphan sub-DAGs, false losses on committed writes).

Two rules the seam guarantees to every consumer, stated once here:

1. **`PreconditionFailed` is a value observation, not a value fact.** Per the granularity contract on `ConditionalWriter` (§7), a consumer may assume neither value-level nor revision-level conflict semantics — BdStore's revision emulation can conflict on an unrelated metadata key. Every consumer contract below therefore begins its exit-9 handling with a **re-read**, never with a conclusion.
2. **Mode is invisible at the call site.** Consumers call `beads.ResolveConditionalWriter(store)` (mode is factory-stamped, §6) and get one of: a writer (CAS active), `nil` + once-latched diagnostic (auto degraded → take the byte-identical legacy branch), or a typed refusal error (require ∧ incapable → fail closed). No consumer ever inspects the flag.

### 9.1 C6 — drain reservation: the three-outcome self-win contract

Today's `reserveDrainMember` (internal/dispatch/drain.go:1223–1246) is a read-then-write with **three outcomes**, and the CAS port must preserve all three — drains are level-triggered and re-entered, so "already mine" is a normal, frequent state:

| current owner of `gc.exclusive_drain_reservation` | today (legacy) | must remain |
|---|---|---|
| `""` | `SetMetadata(member, key, control.ID)` — claim | claim |
| `== control.ID` | `return nil` — idempotent re-entry | success |
| `== other` | `drainReservationError` → skip member | skip |

The naive port — "`CompareAndSetMetadataKey(memberID, key, "", control.ID)`; exit 9 = another drain won → skip" — collapses the middle row into a loss. A re-entered drain would then skip a member **it owns**, no other drain can ever claim it (owner ≠ their ID), and release only covers manifest rows we processed: a permanently stranded, undrainable member that reads as contention. The contract:

```go
// reserveDrainMemberCAS claims exclusive drain access via value-CAS on the
// member's owning store. Contract: PreconditionFailed is never a loss verdict
// by itself — the caller re-reads and applies the three-outcome table.
func reserveDrainMemberCAS(memberStore beads.Store, cw beads.ConditionalWriter, control, member beads.Bead) error {
	ok, err := cw.CompareAndSetMetadataKey(member.ID, beadmeta.ExclusiveDrainReservationMetadataKey, "", control.ID)
	if ok {
		return nil // claimed
	}
	var pf *beads.PreconditionFailedError
	if err != nil && !errors.As(err, &pf) {
		return fmt.Errorf("%s: reserving drain member %s: %w", control.ID, member.ID, err) // transport/exhaustion: surface, retry next tick
	}
	// Exit-9 (or ok=false): observation, not verdict. Re-read and decide.
	current, err := memberStore.Get(member.ID)
	if err != nil { ... }
	switch owner := strings.TrimSpace(current.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]); {
	case owner == control.ID:
		return nil // SELF-WIN: idempotent re-entry, or our own committed-but-unacknowledged write (§9.3)
	case owner == "":
		// Spurious conflict (BdStore cross-key revision interference, or a raced
		// release). Re-issue the CAS once; a second spurious failure surfaces as
		// a transient error and the level-triggered pass retries next tick.
		return retryReserveOnce(memberStore, cw, control, member)
	default:
		return drainReservationError{ControlID: control.ID, MemberID: member.ID, Owner: owner}
	}
}
```

Notes that make this buildable:

- **Store routing is unchanged.** The CAS runs on `drainMemberOwningStore(store, member.ID, opts)` — members may live in the work-class store, not the graph store — and capability is asserted on *that* resolved store, per member. A mixed topology (graph store capable, one rig's bd store not) degrades only the members it owns.
- **Release is symmetric and rides the same PR.** `releaseDrainReservations` becomes `CompareAndSetMetadataKey(memberID, key, control.ID, "")`: losing that CAS means the member was already re-claimed by a successor drain, which is precisely the case where clearing it would be a clobber — the loss is the correct outcome and is logged at debug, never retried.
- **Tests (MemStore, in-process, merge gate of stage 3):** (a) plain contention — two controls race, exactly one owns, loser skips; (b) **re-entry** — reserve, re-enter the same drain, assert `nil` not skip; (c) **ambiguous-retry** — fake runner commits the write then returns `i/o timeout`; assert the retry self-wins (§9.3); (d) spurious-conflict — inject `PreconditionFailed` with the key still empty, assert one bounded re-issue.

### 9.2 C4 — Attach epoch fence: CAS-last, losers feed the existing partial-attach recovery

`molecule.Attach` (internal/molecule/molecule.go:251–311) brackets two unfenced multi-write operations — `Instantiate` (the sub-DAG) and `DepAdd` (the blocking edge) — between an early epoch *check* (line 260–268, `ErrEpochConflict`) and a late epoch *increment* (line 308–311, plain `SetMetadata`). CAS on one key cannot make that whole span atomic; the design decision is **which side of the span the authoritative fence sits on**, and the answer is pinned: **CAS-last**.

- **CAS-first is rejected** because a crash after the CAS but before `Instantiate` burns the epoch with no idempotency record: the retry re-reads the advanced epoch, `findExistingAttach` finds nothing (nothing was created), and the attempt-numbering that `syncControlEpochToAttempt` (internal/dispatch/control.go:304) exists to repair goes permanently skewed.
- **CAS-last** means both racers may fully materialize sub-DAGs before one loses — so the loser's cleanup must be specified, and it is: the loser is wired into the **existing** partial-attach recovery machinery rather than a new mechanism.

The port:

1. Keep the early cheap epoch check exactly as-is (fast-fail for the common already-advanced case; byte-identical when `ExpectedEpoch == 0`).
2. Keep `findExistingAttach` running **before** the fence (molecule.go:251) — this ordering is load-bearing for the ambiguity contract (§9.3) and is now documented on `AttachOptions.ExpectedEpoch` as a contract, not an implementation accident.
3. Replace the final `SetMetadata` increment with the fence:

```go
ok, err := cw.CompareAndSetMetadataKey(attachBeadID, beadmeta.ControlEpochMetadataKey,
	strconv.Itoa(opts.ExpectedEpoch), strconv.Itoa(opts.ExpectedEpoch+1))
```

4. **Loser path** (`ok == false` / `PreconditionFailed`, after side effects exist): Attach itself neutralizes what it just created, because only Attach knows the IDs — (a) stamp the just-created sub-DAG via the existing `markFailed` walk (molecule.go:1291, sets `molecule_failed=true` on all created beads), which makes the orphan root discoverable by `failedAttemptAttachRootID`'s query (control.go:569: idempotency key + root bead + `molecule_failed:true`) and skippable by `findExistingAttach`'s existing `molecule_failed` guard (molecule.go:343); (b) `DepRemove(attachBeadID, result.RootID)` to detach the blocking edge so the attach bead cannot wedge on an orphan root no processor will ever run; (c) return `ErrEpochConflict` wrapped in the dispatch layer's `partialAttemptAttachError` shape so `markControllerSpawnError` (control.go:321) classifies it hard-for-this-attempt rather than transient-retry. The next level-triggered pass re-enters, `findExistingAttach` returns the **winner's** sub-DAG, and the system converges with zero new recovery machinery.
5. `syncControlEpochToAttempt` collapses onto the same helper: `CompareAndSetMetadataKey(control.ID, key, itoa(current), itoa(attemptNum))`. Its exit-9 is benign by construction — another processor advanced the epoch first — so the contract is: re-read; if `current >= attemptNum`, return nil; else re-issue once.

Capability is asserted on the **graph-class store** that actually holds `gc.control_epoch` — on the deployed topology that is the sqlite graph store, which is why §10's sqlite `CompareAndSetMetadataKey` is a blocking deliverable of this same PR, not a follow-up.

**Test (integration, stage-3 merge gate):** two concurrent `Attach` calls sharing an idempotency key and `ExpectedEpoch`; assert exactly one sub-DAG survives live, the loser's root carries `molecule_failed=true` with no inbound blocking edge from the attach bead, and a third re-entrant call returns the winner via `findExistingAttach`.

### 9.3 The ambiguity contract: committed-but-unacknowledged writes

`isBdAmbiguousWriteError` (internal/beads/bdstore.go:1884) already names the class — `i/o timeout`, `broken pipe`, `connection reset`, `deadline exceeded` — where **the write may have committed** even though the caller saw an error. For CAS this is lethal in a specific way: the retry's `PreconditionFailed` may be caused by *our own first attempt*. The contract, documented on `ConditionalWriter` and enforced per consumer:

| written value | can the writer recognize its own committed write on re-read? | on ambiguous error, concluding "lost" is… |
|---|---|---|
| **writer-identifying** (C6 reservation = `control.ID`; C6 release; C2 mutations attributed by revision) | yes — re-read and compare | **forbidden** without a self-win check first |
| **non-identifying** (C4 epoch: `expected+1` is indistinguishable from a competitor's increment) | no | **tolerated only because** `findExistingAttach` idempotency runs before the fence and converges the retry onto whichever sub-DAG won |

Mechanically: CAS calls are **never** routed through the `isBdTransientWriteError` blind retry loop (it contains the ambiguous class and would replay a stale `--if-revision N`); the dedicated CAS policy (§7) surfaces exit 9 immediately and the *consumer* re-reads and re-decides per its table above. The C4 tolerance is written on the seam as a conditional: if anyone ever reorders `findExistingAttach` after the epoch check, the tolerance is void and the ambiguity contract is violated — the comment says so at both sites.

**Test (fake `CommandRunner`, unit):** inject an ambiguous transport error *after* committing the write; assert C6 re-entry self-wins (member stays reserved by us, no skip) and C4 converges via the idempotency path with exactly one live sub-DAG.

### 9.4 The fleet-scoped mixed-writer invariant

CAS provides mutual exclusion **only among CAS writers**. A single legacy writer to the same ledger — a second gc node at `off`, an older binary, an Auto-degraded node with a stale bd — still blind-`SetMetadata`s over CAS-won values, and the CAS node's doctor reads ACTIVE while the race it paid for is open. The invariant, stated verbatim in the design doc and the runbook:

> CAS mutual exclusion on a ledger holds only when **every writer to that ledger is CAS-active**, or **exactly one writer exists**.

Within one process this is guaranteed by construction: the mode is process-latched (§5) and factory-stamped (§6), so one process cannot mix write disciplines on one store. Across processes it cannot be guaranteed, only surfaced: `gc doctor` warns when the resolved mode is `auto` but any store's verdict is DEGRADED under a declared multi-writer topology, and the `beads.conditional_writes.degraded` event (§11) makes the degraded node visible fleet-wide rather than only to whoever runs doctor on it. Until the sqlite `ConditionalWriter` integration test soaks against the deployed store shape, the runbook forbids `require` on the deployed topology (§10).

### 9.5 C2 — API optimistic concurrency: ETag / If-Match / 412

C2 is sequenced last because it is the only consumer that needs the beads **library** bump (`go.mod`), and that bump has an unavoidable wire consequence: `beads.Bead` is embedded directly in response types (`BeadGraphResponse` at internal/api/handler_beads.go:374, and every other bead-bearing response), so the moment the library version carrying `Revision int64` lands, **`revision` appears in the OpenAPI schema whether or not the flag is on**. `TestOpenAPISpecInSync` will red on any PR that bumps go.mod without regenerating. Therefore the wire change is *not* flag-gated and *cannot* be: the go.mod bump PR carries, atomically, the genspec regen, all three tracked OpenAPI copies, the dashboard TS regen, and `make dashboard-check` — and the C2 handler work plus the status-wire `beads_conditional_writes` struct (§11) ride that same PR, because the spec-regen tax is already paid.

The HTTP surface, kept deliberately boring (standard RFC 9110 conditional requests):

- **ETag out:** every bead-returning GET sets `ETag: "<revision>"` (strong, quoted decimal of `Bead.Revision`). The body's `revision` field and the header always agree; clients may use either.
- **If-Match in:** mutating bead endpoints (update, close, delete, assign) accept a typed Huma header param — `IfMatch string \`header:"If-Match"\`` — parsed as exactly one strong ETag. Weak validators (`W/"..."`), lists, and `*` are rejected with the standard Huma 422 validation error; there is no partial support to misread.

| client sends | flag/store verdict | behavior |
|---|---|---|
| no `If-Match` | any | legacy unconditional semantics, byte-identical to today — clients migrate incrementally |
| `If-Match: "42"` | active (mode ∈ {auto, require} ∧ store capable) | store-level `*IfMatch` write; success → 2xx with fresh `ETag`; revision mismatch → **HTTP 412** with the registered `apierr` `precondition_failed` body carrying `expected_revision` and `current_revision` (mapped from `PreconditionFailedError` — the same forensics the log line gets) |
| `If-Match: "42"` | inactive (mode off, or store incapable) | **HTTP 501** with registered `apierr` `conditional_writes_unsupported`, naming the mode/verdict and origin. Never 2xx: silently executing an unconditional write under a presented precondition is the API-shaped silent fallback this design forbids. 501 (not 412) so client retry loops terminate — a 412 would send well-behaved clients into re-GET-and-retry against a server that can never honor the condition |

Handler sketch (one shared helper, not per-endpoint logic):

```go
// conditionalBeadWrite resolves the CAS verdict for the request and either
// runs the conditional write, the legacy write, or refuses — exactly one path.
func conditionalBeadWrite(store beads.Store, ifMatch string,
	legacy func() error,
	conditional func(cw beads.ConditionalWriter, expected int64) error) error {

	if ifMatch == "" {
		return legacy()
	}
	expected, err := parseStrongETag(ifMatch) // 422 on weak/list/*
	if err != nil { return err }
	cw, diag, err := beads.ResolveConditionalWriter(store)
	if err != nil || cw == nil { // require∧incapable, or off, or auto∧incapable
		return apierr.ConditionalWritesUnsupported(diag) // 501, typed, never silent
	}
	return conditional(cw, expected) // PreconditionFailedError → apierr.PreconditionFailed → 412
}
```

Note the asymmetry with C4/C6: the API consumer performs **no re-read and no self-win logic**. The HTTP client owns the retry loop (re-GET, rebase, resend with the new ETag) — that is the entire point of surfacing 412 with `current_revision` — so the ambiguity contract's writer-identifying row is satisfied by the client, not the server. The server's only obligations are: never convert a presented precondition into an unconditional write, and never return a stale ETag (the CachingStore evict-on-`PreconditionFailed` discipline from §7 is what makes the second obligation hold; its regression test is a stage-2 merge gate, before any C2 handler exists).

**Tests (stage 4):** handler-level table tests for all four rows above; a 412 round-trip asserting `expected_revision`/`current_revision` in the body and that a follow-up GET's ETag equals `current_revision`; `TestOpenAPISpecInSync` and `make dashboard-check` green in the same PR as the go.mod bump.

## 10. The sqlite graph store (deployed reality)

Everything in this design that talks about the epoch fence and the drain reservation is, on the fleet that motivated the work, talking about **one SQLite file**. The deployed controller runs the `deploy/sqlite-b36-probe-attribution` lineage, where `[beads] graph_store = "sqlite"` routes the graph coordination class to an embedded pure-Go SQLite store (`modernc.org/sqlite`, CGO_ENABLED=0) at `<city>/.gc/beads.sqlite`, minting `gcg-` bead IDs. That store — not Dolt, not bd — holds `beadmeta.ControlEpochMetadataKey` (`gc.control_epoch`) and `beadmeta.ExclusiveDrainReservationMetadataKey` (`gc.exclusive_drain_reservation`, the drain "reserved_by" key). Two facts follow, and they set this section's scope:

1. **`origin/main` has no `SQLiteStore` at all** (verified: `internal/beads/` on this lineage contains `bdstore*`, `caching_store*`, `memstore`, `doltlite_read_store` — no sqlite files; `resolveClassStore` in `cmd/gc/class_store.go` is an identity seam that returns the work store for every class). The C4/C6 code lands on main, but the fence it guards executes on the deploy lineage.
2. Without a sqlite `ConditionalWriter`, the flag is dead on arrival exactly where it matters: under `auto` the graph store fails the interface assert → permanent DEGRADED → zero correctness gain on the deployed fleet while a dev laptop's doctor shows ACTIVE; under `require`, every `molecule.Attach` epoch advance and every exclusive-drain reservation returns a typed refusal → the hottest control path stalls fleet-wide.

The sqlite `CompareAndSetMetadataKey` plus an integration test against the deployed store shape is therefore a **blocking deliverable of the C4/C6 PR** — a merge-gate checklist item, not a risks footnote.

### 10.1 Write-path facts that constrain the implementation

The deployed store's shape (read from `deploy/sqlite-b36-probe-attribution:internal/beads/sqlite_store.go`) dictates the CAS design:

- **Dual representation.** `bead_json` on the `beads` table is canonical for reads (`getTx` does `SELECT bead_json FROM beads WHERE id=?`); the `metadata(bead_id, meta_key, meta_value)` table with `PRIMARY KEY(bead_id, meta_key)` is a query index (`idx_metadata_key_value`). Every write (`Update` → `upsertBeadTx`) rewrites both. A CAS that updates only the index row leaves reads serving the stale value from `bead_json`; a CAS that updates only `bead_json` breaks `ListByMetadata`. **Both representations move in one transaction or the store is corrupt.**
- **Concurrency model.** One write connection (`MaxOpenConns=1`) serializes in-process writers; WAL mode + `busy_timeout=5000` + the application-level `retryOnBusy` (3 × 150 ms) handle cross-process contention — and cross-process contention is real: the controller and every short-lived `gc` CLI invocation on the host (`gc ready`, order dispatch, sweeps) open the same file, sharing an in-process handle via `graphStoreHandleCache`.
- **Snapshot-upgrade safety.** Mutations use the `ReleaseIfCurrent` template: deferred `BeginTx` → `getTx` → mutate in Go → `upsertBeadTx` → `Commit`, inside `retryOnBusy`. Under WAL, a deferred transaction that read a snapshot and then writes fails with `SQLITE_BUSY_SNAPSHOT` if **any** other commit intervened; `isSQLiteBusy` matches that error string (`"database is locked"`), so `retryOnBusy` re-runs the whole closure against a fresh read. This is what makes read-compare-write inside one deferred tx sound — but we do not lean on it alone: the CAS guard below lives in the WHERE clause of the committing statement, so the verdict is evaluated by SQLite at write time, never by Go against a possibly-stale snapshot.
- **Local commits are unambiguous.** Unlike the bd subprocess transport, a local WAL `COMMIT` either returns nil or the transaction rolled back. The ambiguous-outcome self-win contract (consumer-contracts section) is a bd-transport artifact; consumers keep it because the interface is store-agnostic, but the sqlite implementation never manufactures that state.

### 10.2 `CompareAndSetMetadataKey`: the conditional UPDATE

New file `internal/beads/sqlite_store_conditional.go` — **new-file-only**, per the upstream-alignment rules, so the identical commit applies to both the deploy lineage (where `SQLiteStore` exists today) and main (when the store is promoted).

```go
var _ ConditionalWriter = (*SQLiteStore)(nil)

// CompareAndSetMetadataKey sets key to next iff its current value equals
// expected, treating an absent metadata row as "". This is VALUE-CAS:
// concurrent writes to OTHER metadata keys on the same bead do not fail the
// guard (contrast the BdStore revision-emulation loop, which they do).
// The compare is the WHERE clause of the committing UPDATE, so the verdict
// and the mutation are one atomic statement — a stale in-Go compare is
// structurally impossible.
func (s *SQLiteStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	var won bool
	err := retryOnBusy(func() error {
		won = false
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil) // single write conn, MaxOpenConns=1
		if err != nil {
			return fmt.Errorf("sqlite cas %q: begin tx: %w", id, err)
		}
		defer tx.Rollback() //nolint:errcheck
		b, err := s.getTx(ctx, tx, id) // ErrNotFound mapping preserved
		if err != nil {
			return err
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string, 1)
		}
		b.Metadata[key] = next
		b.UpdatedAt = time.Now()
		payload, err := json.Marshal(b)
		if err != nil {
			return fmt.Errorf("sqlite cas %q: marshal: %w", id, err)
		}
		// THE guard. COALESCE folds "no row" to "", so expected=="" means
		// "claim if unset" — the exact drain-reservation shape.
		res, err := tx.ExecContext(ctx, `
			UPDATE beads
			   SET bead_json = ?, updated_at = ?, revision = revision + 1
			 WHERE id = ?
			   AND COALESCE((SELECT meta_value FROM metadata
			                  WHERE bead_id = ? AND meta_key = ?), '') = ?`,
			string(payload), b.UpdatedAt.UnixNano(), id, id, key, expected)
		if err != nil {
			return fmt.Errorf("sqlite cas %q: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // lost: deferred Rollback, nothing written, won stays false
		}
		// Guard passed: keep the metadata index in lockstep with bead_json.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO metadata(bead_id, meta_key, meta_value) VALUES(?, ?, ?)
			ON CONFLICT(bead_id, meta_key) DO UPDATE SET meta_value = excluded.meta_value`,
			id, key, next); err != nil {
			return fmt.Errorf("sqlite cas %q: index row: %w", id, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		won = true
		return nil
	})
	return won, err
}
```

Notes that survive review questions:

- **Why the marshalled `bead_json` can't clobber a concurrent sibling write:** if any other commit lands between `getTx` and the UPDATE, the deferred tx's write upgrade fails `SQLITE_BUSY_SNAPSHOT` and `retryOnBusy` re-runs the closure with a fresh read. The guard's WHERE clause is defense-in-depth on top of that, not the only line.
- **Loss returns `(false, nil)`** — the interface's value-CAS verdict. The caller re-reads and re-decides per the consumer contracts (self-win check for writer-identifying values; skip vs converge). `ErrNotFound` propagates from `getTx`; `reserveDrainMember` already treats it as a no-op, which also covers the retention sweeper deleting a terminal bead between read and CAS.
- **`Delete` + retention interplay:** the 4-hour terminal-record sweeper can remove a bead under a contender; the guard then hits zero rows via the `id` predicate and the earlier read's `ErrNotFound` — never a false win.

### 10.3 The revision column and the rest of the interface

Capability is interface satisfaction on the resolved store — all-or-nothing — and the conformance suite (test-seams section) runs sqlite in **unit CI**. So the blocking PR ships the full `ConditionalWriter`, not just the metadata method; the revision-keyed trio is the same conditional-statement shape three more times:

```go
func (s *SQLiteStore) UpdateIssueIfMatch(id string, expected int64, opts UpdateOpts) error
func (s *SQLiteStore) CloseIssueIfMatch(id string, expected int64) error
func (s *SQLiteStore) DeleteIssueIfMatch(id string, expected int64) error
// guard: WHERE id = ? AND revision = ?; RowsAffected == 0 → re-read revision
// in-tx → PreconditionFailedError{Expected: expected, Current: cur} (or ErrNotFound).
```

- **Schema migration, idempotent on the live deployed file:** `applySchema` (already run on every open) gains a `pragma table_info(beads)` column check and, when absent, `ALTER TABLE beads ADD COLUMN revision INTEGER NOT NULL DEFAULT 0`. `ADD COLUMN` is schema-only (no row rewrite) — safe against a WAL file other processes hold open. Existing rows start at 0; `upsertBeadTx` bumps via the `ON CONFLICT` arm (`revision = beads.revision + 1`), the insert arm starts at 1; reads stamp `Bead.Revision` from the column (column authoritative, `bead_json` never carries it) once the Stage-4 library bump adds the field — until then the conformance suite reads `Current` off `PreconditionFailedError`, which is the revision oracle the interface already guarantees.
- **Mixed-binary ABA — the sharp edge that justifies the settled scoping.** During a deploy window, an *old* gc binary writing the same file mutates beads through the pre-revision `upsertBeadTx`, which never names the column — mutations that **don't bump revision**. A revision-keyed CAS by the new controller can then pass its guard despite an intervening write: classic ABA. `CompareAndSetMetadataKey` is immune (it compares the value itself), **and C4/C6 consume only `CompareAndSetMetadataKey`** — which is exactly why the value-CAS method is the blocking correctness core and the revision trio is trustworthy on this file only once every gc binary on the host runs a revision-bumping build. The runbook carries this rule verbatim.

### 10.4 Wrapper transparency on the resolved path

The controller never holds a bare `*SQLiteStore`. The resolved graph store is wrapped, and each wrapper interacts differently with the type assert (the `class_store.go` lesson: optional interfaces are NOT promoted through hand-rolled delegation):

| Wrapper (deploy lineage) | Shape | ConditionalWriter status |
|---|---|---|
| `noCloseGraphStore{*beads.SQLiteStore}` | embeds the concrete pointer | promoted automatically; add `var _` assert anyway |
| `lazyGraphStore` (self-healing open) | hand-rolled per-method delegation | **must add explicit forwarding methods** or the resolved store reads incapable forever |
| `beadPolicyStore` / `beadPolicyGraphStore` (main) | hand-rolled delegation | same — explicit forwarding (this wrapper already dropped `ListGraphOnlyHandle` once; the regression class is proven) |
| `CachingStore` | covered in the machinery section (forward + evict) | — |

Forwarding rule for `lazyGraphStore`: while unhealed, `CompareAndSetMetadataKey` returns the **open error** — never `ErrConditionalWriteUnsupported`. A transient open failure must fail loud (matching the store's documented fail-loud reads/writes), not latch the store incapable and silently degrade `auto` to the legacy path. A wrapper-transparency test resolves the graph store exactly as the controller registers it (`graph_store="sqlite"` → lazy wrapper → shared-handle cache → policy wrap) and asserts the **resolved** value satisfies `ConditionalWriter`.

### 10.5 The blocking integration test

`test/integration/graph_store_sqlite_cas_test.go`, `//go:build integration`, staged where `SQLiteStore` exists — the deploy lineage today (`test/integration/graph_store_sqlite_convergence_test.go` and `test/agents/graph-store-sqlite-worker.sh` on that branch are the templates for topology and the second-process harness). Legs, each a named subtest:

1. **Resolved-path capability** — temp city with `[beads] graph_store = "sqlite"`; resolve through the controller's registration path; assert `ConditionalWriter` satisfaction on the resolved store; assert an unhealed lazy store returns the open error, not `ErrConditionalWriteUnsupported`.
2. **Epoch-fence exclusion (in-process)** — seed a `gcg-` control bead with `gc.control_epoch = "3"` plus sibling metadata; 8 goroutines CAS `"3" → "4"`; exactly one `true`; final value `"4"`; sibling keys byte-identical; revision advanced exactly once for the CAS.
3. **Drain-reservation exclusion (cross-process)** — the deployed contention is controller-vs-CLI on one `.gc/beads.sqlite`: a second OS process hammers `CompareAndSetMetadataKey(member, gc.exclusive_drain_reservation, "", <its control ID>)` across M members while the test process competes with its own ID; assert exactly one owner per member, losers observed `false`, no `SQLITE_BUSY` leaks through `retryOnBusy`, and index-vs-`bead_json` agreement on re-read (`Get` and `ListByMetadata` return the same owner).
4. **Deployed-file migration** — open a fixture `beads.sqlite` created with the pre-revision schema verbatim and populated rows carrying `gc.control_epoch`; assert the open migrates idempotently (open twice), CAS works against pre-existing rows, revisions start at 0.
5. **Busy/snapshot retry** — pin the WAL write lock past `busy_timeout` from a helper connection; assert the CAS converges to a correct verdict after retry, never a false win.

The store-agnostic conformance suite additionally runs `SQLiteStore` in **unit** CI (`t.TempDir()`, pure-Go driver — no build-tag excuse; only the multi-process leg needs the integration tag).

**Gate:** the C4/C6 PR's merge checklist names this file green *on the lineage the fleet deploys from*. main's identity `resolveClassStore` means main-only green proves nothing about the deployed fence.

### 10.6 Interim rules: doctor rendering and the runbook prohibition

Until the deliverable lands and soaks, the four-cell matrix instantiates on the deployed topology as:

| `conditional_writes` | Deployed gc (no sqlite ConditionalWriter) | After the deliverable |
|---|---|---|
| `off` | legacy, byte-identical to today | legacy |
| `auto` | graph class **DEGRADED**: interface assert fails, once-per-store `beads.conditional_writes.degraded` event, legacy writes — zero gain on the fence keys | CAS on `gc.control_epoch` / `gc.exclusive_drain_reservation` |
| `require` | typed refusal on **every** epoch advance and drain reservation → controller-wide stall; doctor ERROR, nonzero exit | CAS |

`gc doctor` renders the graph-class store's verdict specifically (`store=graph kind=sqlite capable=false reason="SQLiteStore predates ConditionalWriter"`), per the observability section's per-store array — never folded into an aggregate boolean.

Runbook text (verbatim, shipped with the C4/C6 PR):

```markdown
### conditional_writes where [beads] graph_store = "sqlite" — interim rules

- `require` is FORBIDDEN while the running gc predates the sqlite
  ConditionalWriter. Every molecule.Attach epoch advance and every
  exclusive-drain reservation would refuse → controller-wide stall.
  gc doctor renders this ERROR (nonzero exit) before you deploy it. Believe it.
- `auto` is safe but a no-op for graph-class writes: DEGRADED, typed event,
  today's TOCTOU behavior retained. You gain nothing on the fence keys.
- Lift the prohibition only when ALL hold:
  (1) sqlite ConditionalWriter + the deployed-topology integration test are
      merged on the lineage the fleet deploys (deploy/sqlite-b36-probe-attribution
      today — NOT origin/main);
  (2) conformance suite green including sqlite;
  (3) >= 1 week soak on the reference deployment at `auto` with zero degraded events
      from the graph store and doctor showing graph=capable.
- Revision-keyed CAS (UpdateIssueIfMatch et al.) on this file is trustworthy
  only when every gc binary on the host runs a revision-bumping build —
  an old binary's writes do not bump the column (ABA). Value-CAS
  (CompareAndSetMetadataKey) is immune; C4/C6 use only value-CAS.
- Mixed writers on one .gc/beads.sqlite are the controller PLUS every
  short-lived gc CLI process on the host. CAS mutual exclusion holds only
  when every writer is CAS-active or exactly one writer exists. Flip modes
  via city.toml + controller restart only; a per-process env override
  (GC_BEADS_CONDITIONAL_WRITES) splits the writer set on a single file —
  exactly the mixed-writer topology doctor warns about.
```

## 11. Lifecycle and flag-debt enforcement

The registry's anti-rot teeth are worthless if they fire as wall-clock time bombs. This repo's merge pipeline is driven by an autonomous fleet whose quality gates treat any red as a stall (prior art: the zero-merges RCA, the tracked trivyignore cliff of 2026-08-07). A check that reds `main` with zero diff — same commit passing Tuesday, failing Wednesday — trains everyone to neuter it the first time it fires, breaks bisect, and wedges every open PR at once. So lifecycle enforcement is split along one bright line:

| Tooth | Trigger | Where it runs | Blocking? |
|---|---|---|---|
| Registry structural validation (Category, ConfigPath reflection, Default parity, dual Owner, per-category field rules, EnvOverride ∈ LeakVectorVars) | any commit | `registry_test.go`, PR CI | **yes** — deterministic per commit |
| Graduation stage 1 (default must leave Off) | deps.env `BD_VERSION` crosses the floor | `scripts/bd_version_pin_test.go` family, PR CI | **yes** — fires only in the diff that moves the anchor |
| Graduation stage 2 (flag must be deleted) | deps.env `BD_PREV_VERSION` crosses the floor | same test, PR CI | **yes** — fires only in the diff that moves the anchor |
| Wall-clock `Expires` past due | calendar | nightly radar → bead against Owner + doctor WARN | **no** — except when `registry.go` is in the PR diff |
| Tombstone past `RemovedIn`+1 | version anchor comparison | nightly radar → bead | **no** |
| Owner-bead liveness | bead closed/purged | nightly radar → bead against Owner.GitHub | **no** |

**Normative rule: no merge-blocking check in this subsystem may compare against `time.Now()`.** Every hard CI failure must be a pure function of repo state at the commit — version anchors in `deps.env`, fields in `registry.go`, code in the tree. Wall-clock staleness is real debt, but it is the radar's job, not Check's.

### 11.1 Lifecycle fields on the Spec

The lifecycle-bearing subset of `Spec` (full shape in §4):

```go
type Owner struct {
    Bead   string // "ga-xxxxx" — work tracking; the radar files/updates against it
    GitHub string // "@handle" or "@org/team" — the named human gate; both required non-empty
}

type Spec struct {
    // ... identity/config/env fields (§4) ...
    Owner         Owner
    Expires       string // "2027-01-15"; radar-only. Mandatory for rollout/migration, forbidden for killswitch.
    VersionAnchor string // deps.env key naming the capability floor, e.g. "BD_CONDITIONAL_WRITES_MIN_VERSION".
                         // Mandatory for rollout/migration, forbidden for killswitch.
    GraduatedIn   string // BD_VERSION value at the Off→Auto flip; "" until stage 1 fires. Set in the flip PR.
    FlipDueBy     string // bounded deferral: a BD_VERSION literal. Set only by a bump PR that trips stage 1.
}
```

Owner beads get closed and bulk-purged in this project (the cache-reconcile incident), so `Owner.Bead` alone is decorative — a fired trigger naming a tombstoned bead reaches nobody. `Owner.GitHub` plus the CODEOWNERS line is the mechanical human gate:

```
# .github/CODEOWNERS
/internal/rollout/registry.go @gastownhall/gascity-admins
```

Every Spec addition, `Expires` extension, `FlipDueBy` deferral, and category claim now requires a named-human review. In an agent-authored repo this is the only real tooth for semantic judgments ("is this genuinely a killswitch?"); the design says so plainly rather than pretending a test can read a justification string.

### 11.2 Two-stage version-anchored graduation: one plain Go test

No predicate DSL, no `RemovalTrigger` mini-language. Graduation is one ~20-line test in the `TestBDVersionPins` family (`scripts/bd_version_pin_test.go` — it already owns `readDotenv`/`repoRoot` and keeps every bd anchor in lockstep). The CAS floor is a Go constant in `internal/beads` mirroring the `bdReadyProjectionMinVersion = "1.0.5"` precedent, tied to a new `deps.env` key `BD_CONDITIONAL_WRITES_MIN_VERSION` under the existing lockstep assertions:

```go
func TestConditionalWritesGraduation(t *testing.T) {
    env := readDotenv(t, filepath.Join(repoRoot(t), "deps.env"))
    floor := env["BD_CONDITIONAL_WRITES_MIN_VERSION"] // lockstep with beads.bdConditionalWritesMinVersion
    spec := rollout.SpecByKey(t, "beads.conditional_writes")

    // Stage 1: the installable default bd can CAS — the builtin default must leave Off.
    if deps.CompareVersions(env["BD_VERSION"], floor) >= 0 && spec.Default == rollout.Off {
        if spec.FlipDueBy == "" || deps.CompareVersions(env["BD_VERSION"], spec.FlipDueBy) > 0 {
            t.Fatalf("bd %s supports --if-revision: flip beads.conditional_writes default Off→Auto and set GraduatedIn, "+
                "or set FlipDueBy=%s in this PR (owner %s / %s)",
                env["BD_VERSION"], env["BD_VERSION"], spec.Owner.Bead, spec.Owner.GitHub)
        }
    }

    // Stage 2: the minimum-supported bd can CAS — the flag itself is now debt; delete it.
    if deps.CompareVersions(env["BD_PREV_VERSION"], floor) >= 0 {
        t.Fatalf("min-supported bd %s supports --if-revision: DELETE the flag — Spec, Flags accessor, ForTest option, "+
            "BeadsConfig.ConditionalWrites + mergeFragment branch, legacy read-then-write branches, this test — "+
            "and mint the RetiredKeys tombstone (owner %s / %s)",
            env["BD_PREV_VERSION"], spec.Owner.Bead, spec.Owner.GitHub)
    }
}
```

Why two stages, and why these anchors: `BD_VERSION` (the installable default) moves fast; `BD_PREV_VERSION` (the min-supported contract-matrix floor) historically barely moves — it sits at v1.0.4 today, still below the 1.0.5 ready-projection floor introduced a full bd generation ago. A single trigger keyed to the floor plausibly never fires; a single trigger keyed to `BD_VERSION` demands deletion while old bd is still supported. Stage 1 forces the *default flip* the moment the anchor that moves crosses; stage 2 forces *deletion* — the terminal state — the moment the slow anchor crosses. "Default flipped, flag and dual code paths in tree forever" is no longer a green state.

Both stages are deterministic per commit: they only change verdict when someone edits `deps.env`, i.e., inside the PR that makes graduation possible, where a red is actionable by the person holding the pen.

**The `FlipDueBy` grace marker.** A version bump is often driven by something else entirely — a bd CVE fix — and forcing a same-PR semantic flip on the epoch-fence path (a change our own risk register says needs soak) would make the bump author choose between reverting a security fix and rush-shipping `Require`-adjacent behavior. So stage 1 offers exactly one bounded escape: the bump PR may set `FlipDueBy` to the `BD_VERSION` it is landing. The deferral holds while `BD_VERSION <= FlipDueBy`; the *next* anchor bump exceeds it and the test reds again. Properties:

- **Diff-visible.** Setting or raising `FlipDueBy` is an edit to `registry.go` — CODEOWNERS-gated, reviewed by a named human as debt.
- **Bounded.** Grace is one anchor bump, not a date. Re-deferral requires another loud registry edit; the radar independently files against the Owner while any `FlipDueBy` is pending.
- **Silent-forever impossible.** There is no state in which the anchor is past the floor, the default is Off, and CI is green without a visible, reviewed deferral in the file.

`GraduatedIn` is recorded in the Spec by the flip PR (stage 1's demanded edit), so the registry itself carries the fact stage-2 tooling and the radar reason about — no git archaeology.

### 11.3 The nightly radar: wall-clock staleness, non-blocking

Wall-clock `Expires` moves **entirely** out of the merge path. A scheduled nightly workflow runs `go run ./scripts/rolloutradar`, which imports `internal/rollout`, walks the registry, and for each finding **files or updates a bead against `Owner.Bead`** (idempotent: one bead per flag per finding class, updated not duplicated) and reds a **non-blocking** status. Findings:

- `Expires` past due (flag neither graduated, deleted, nor visibly extended).
- `FlipDueBy` set and pending (a deferred stage-1 flip awaiting its dedicated, soaked PR).
- Tombstone past its version window (§11.5).
- `Owner.Bead` closed or purged — the radar re-files a fresh bead and names `Owner.GitHub`, so a fired trigger can never point at a tombstoned bead with nobody attached.

The same findings feed `gc doctor`'s Rollout Flags section: approaching/announced items render as WARN; radar-surfaced *past-due* items render per the doctor exit contract pinned in §10 (ERROR, nonzero exit). Operators see the debt; the merge pipeline never stalls on it.

**The one exception:** past-due `Expires` *does* hard-fail PR CI when `internal/rollout/registry.go` itself is in the diff — you must confront the registry's debt to touch the registry. Mechanically: the Check workflow sets `GC_ROLLOUT_EXPIRY_GATE=1` only when the PR's changed-files list includes `registry.go`, and the expiry assertion in `registry_test.go` is gated on that env var. A commit's verdict still never changes without a diff *to this file*, so bisect and unrelated PRs stay green.

### 11.4 Per-category immortality rules

Enforced in `registry_test.go`, deterministically:

| Category | `Expires` | `VersionAnchor` | Legal terminal state |
|---|---|---|---|
| `infra-rollout` | **mandatory** | **mandatory** | deletion (stage 2) |
| `infra-migration` | **mandatory** | **mandatory** | deletion (stage 2) |
| `infra-killswitch` | **forbidden** | **forbidden** | may be long-lived |

Rollout and migration flags may **never** be immortal: their whole purpose is to stop existing. There is no `Stability` enum and no "Stable" promotion — the escape hatch where a one-line edit simultaneously exempted a flag from expiry and freed cap headroom does not exist, because neither the hatch nor the cap exists (the soft cap is deleted; a count ceiling delivered zero value at N=2 and maximum friction during incidents). A rollout flag that wants to live forever has exactly one path: reclassify as `infra-killswitch` in a CODEOWNERS-reviewed diff that also *deletes* its `Expires` and `VersionAnchor` — a reclassification no reviewer will wave through by accident, because the diff shape is unmistakable.

Killswitches skip the graduation machinery but not the radar: owner-bead liveness and doctor rendering still apply.

### 11.5 Version-anchored tombstones

When a flag is deleted, its TOML key is minted into `RetiredKeys` in `internal/config/undecoded.go`, downgrading the key from fatal-unknown-field to a friendly warning:

```go
// internal/config/undecoded.go
type RetiredKey struct {
    Path      string // "beads.conditional_writes"
    RemovedIn string // version anchor at removal, e.g. "v1.2.0"
    Message   string // rendered verbatim in the load warning
}

var retiredKeys = []RetiredKey{
    {
        Path:      "beads.conditional_writes",
        RemovedIn: "v1.2.0",
        Message:   "conditional writes graduated to always-on in v1.2.0; delete this line from city.toml",
    },
}
```

```toml
# operator's stale city.toml
[beads]
conditional_writes = "auto"
# → warning at load: city.toml: "beads.conditional_writes" was retired in v1.2.0 —
#   conditional writes graduated to always-on in v1.2.0; delete this line from city.toml
```

Tombstone lifetime is **version-anchored, never wall-clock, never "one release"** — this fleet deploys branches, not tags (the reference deployment runs a `deploy/*` branch), so "one release" has no machine meaning for the binaries actually running. The nightly radar flags a tombstone for deletion once the current version anchor exceeds `RemovedIn` by more than one bump (`RemovedIn+1`), filing a bead to remove the entry. No tombstone comparison ever appears in a merge-blocking test; the time-bomb class is not relocated into `undecoded.go`. The concrete version in the warning text is also better operator UX than "recently": the slow-upgrading operator the tombstone exists for learns exactly which version removed the key.

### 11.6 ADDING a flag: the four-place checklist

One PR, four places, each with a test that fails if skipped:

1. **Config field + pure accessor** on the *owning* section struct (e.g. `BeadsConfig.ConditionalWrites` with jsonschema enum tags, accessor mapping `""`→default). If the section is whole-table LWW in `mergeFragment`, the per-field `IsDefined` preservation branch **and its hand-written merge regression test** (the `daemon.formula_v2` template) land here (§5). Load-time enum validation is registry-driven and comes for free.
2. **The Spec** in `internal/rollout/registry.go`. `registry_test.go` gates, all deterministic: Category in the closed enum; `ConfigPath` reflection-resolves against `config.City`'s toml tags; `EnvOverride` is `""` or a `GC_*` name registered in testenv `LeakVectorVars`; **`Default` equals the typed accessor's value over a zero-value `config.City`** (closing the two-homes drift where doctor renders the registry while the binary behaves per the accessor — a half-landed graduation PR fails here); dual Owner non-empty; per-category `Expires`/`VersionAnchor` rules (§11.4); `SelectsBetween` names both mechanical code paths.
3. **Typed accessor on `Flags`** plus the generated `rollout.With<FlagName>(...)` ForTest option, always as a pair.
4. **DI threading**: factory stamping for store-mediated flags, options-struct fields for consumers, plus the entry-point test asserting a temp-`city.toml` value is observed by a probe (§7).

Graduation PRs use the same checklist in reverse gear: the stage-1 flip edits the accessor's builtin mapping **and** `Spec.Default` **and** `Spec.GraduatedIn` in one diff — the Default-parity test makes a partial flip unmergeable.

### 11.7 REMOVING a flag: compile-enforced

Removal is one PR that deletes, in order: the Spec; the `Flags` accessor and its `With*` ForTest option; the config field and its accessor; the `mergeFragment` preservation branch and merge test; the dead legacy code branches; the graduation test; the `LeakVectorVars` entry. Then it mints the tombstone (§11.5).

The compiler is the enforcement. Because every production read is a typed method (`flags.BeadsConditionalWrites()`) and every test override is a typed option (`rollout.WithBeadsConditionalWrites(rollout.Require)`), deleting the flag breaks **every** production call site *and every test* at compile time — no string-keyed lookup survives to fail at runtime, no grep-and-pray. The four-cell-matrix tests per consumer (§8) go with it; the legacy path's disappearance means the `off` cell's byte-identical assertion has nothing left to compare against, which is the point.

What keeps removal *reached* rather than merely *possible*: stage 2 of the graduation test reds the anchor-bump PR until this deletion PR exists, the radar nags the Owner's bead in the interim, and no category besides killswitch has a legal state in which the flag simply stays. A flag with no reachable removal state is rejected at review — CODEOWNERS on `registry.go` — as a disguised permanent toggle.

## 12. Observability and operability

The operating principle for this section: **the running daemon is the source of truth; every other surface either renders the daemon's own snapshot or says loudly that it could not.** Observability ships in three deliberate stages so the correctness-critical PRs are never hostage to wire-schema churn.

| Surface | Stage | Mechanism | What it answers |
|---|---|---|---|
| `gc doctor` — Rollout Flags section | 1 | Registry-rendered, local resolution with mandatory banner | "What would this shell resolve, and is any store incapable?" |
| `beads.conditional_writes.degraded` typed event | registered 2, emitted 3 | Event bus, latched once per store | "Did any store silently fall back to legacy writes, ever?" (push, alertable) |
| Status wire + live-API doctor | 4 | Huma-typed aggregate + per-store array, rides the go.mod-bump regen | "What is the daemon *actually* running, including its latches and notices?" |
| `/v0/config/explain` per-layer origin | deferred | New compose.go provenance plumbing | "Which fragment set this?" — built when someone asks, costed honestly then |

### 12.1 `gc doctor` (slice 1)

Doctor gains a **Rollout Flags** section rendered by iterating the registry — a new flag gets its row for free from its `Spec` (Key, Owner, Expires, ConfigPath) plus the resolved `Flags` value and, for capability-gated flags, the per-store verdicts.

Slice-1 doctor is a separate process resolving from **its own** shell env and PATH. That is not the daemon's view, so the banner is unconditional whenever doctor resolves locally:

```
Rollout Flags
  ! city not running — values resolved from this shell's env and PATH
    and may differ from the daemon

  beads.conditional_writes   mode=require   origin=config   owner=ga-c4cas / @gastownhall/gascity-flags
    expires: 2027-01-15 (radar-tracked)
    stores:
      graph (sqlite)        probe=capable    latch=unlatched   ACTIVE
      rig gastown (bd)      probe=incapable  latch=unlatched   FAIL-CLOSED
        reason: bd 1.1.0 help lacks --if-revision (all four verbs probed)
    effective: FAIL-CLOSED — require set but store "rig gastown" is incapable;
    CAS writes to that store will refuse. Fix bd or set conditional_writes=auto|off and restart.
```

Rendering rules, all registry-driven:

- **Probe vs latch are separate columns, always.** `probe` is what capability detection reports now (help-text grep today, version-compare post-tag); `latch` is the runtime exit-13/unknown-flag verdict a live store has accumulated. In local mode `latch` is always `unlatched` (doctor's freshly probed stores have no write history); in live-API mode (12.4) it is the daemon's real latch. Collapsing these two was explicitly rejected — "doctor says capable but writes refuse" incidents are diagnosed by exactly this split.
- **Effective status** per flag: `ACTIVE` / `DEGRADED` (auto ∧ any incapable store) / `FAIL-CLOSED` (require ∧ any incapable store) / `off`, plus `pending restart` (12.1.1). Aggregation is worst-of: `fail_closed > degraded > pending-restart > active > off`.
- The **graph-class store is always rendered as its own row** — until the sqlite `ConditionalWriter` integration test soaks on the deployed topology, this row is the operator's only honest view of whether the epoch fence is real where it matters.
- When mode is `auto`, any store is `DEGRADED`, and the config declares a multi-writer topology, doctor prints the **mixed-writer warning**: CAS mutual exclusion holds only when every writer to a ledger is CAS-active or exactly one writer exists.

#### 12.1.1 Pending restart in slice 1

The daemon's authoritative pending-restart notice (boot-latched value ≠ on-disk config after a reload) lives in `controllerState` and reaches doctor exactly via the live API in stage 4. Slice-1 doctor approximates it from **live state only** (no status files): if a running daemon is found in the process table and its start time predates the mtime of any config layer that defines a registry flag, doctor renders:

```
  ⚠ pending restart? city.toml modified after daemon start (daemon: Jul 08 14:02,
    city.toml: Jul 09 09:15). Process-latched flags keep their boot values until restart.
```

This is labeled as an approximation in the output; the exact latched-vs-on-disk comparison arrives with 12.4.

#### 12.1.2 Exit-code contract (pinned by test)

| Condition | Rendering | Exit |
|---|---|---|
| `FAIL-CLOSED` on any flag (require ∧ incapable) | ERROR | nonzero |
| Radar-surfaced past-due lifecycle item (expired Spec, stale tombstone) | ERROR | nonzero |
| `DEGRADED` (auto ∧ incapable) | WARNING | 0 |
| Pending restart, env-overrides-config notice | WARNING | 0 |
| Everything else | INFO | 0 |

Rationale: monitoring pipelines wire doctor into cron health checks. FAIL-CLOSED means writes are being refused *right now* — page-worthy. DEGRADED is today's exact legacy behavior plus a flag that wants attention — a page on every auto/stale-bd host would get the check deleted within a week. `TestDoctorRolloutExitContract` constructs a fixture per cell of this table and asserts both the exit code and the ERROR/WARNING classification, so the contract cannot drift silently.

### 12.2 The push surface: `beads.conditional_writes.degraded`

DEGRADED is the state most likely to persist unnoticed for months (a fleet on `auto` with one stale-bd host), so it cannot depend on someone thinking to run doctor. Stage 2 registers, stage 3 emits:

```go
// internal/events — added to KnownEventTypes; payload registered per the
// typed-events invariant (TestEveryKnownEventTypeHasRegisteredPayload).
const EventBeadsConditionalWritesDegraded = "beads.conditional_writes.degraded"

type ConditionalWritesDegradedPayload struct {
    StoreID   string `json:"store_id"`   // e.g. "rig/gastown", "graph"
    StoreKind string `json:"store_kind"` // bd | native | sqlite-graph | caching
    Mode      string `json:"mode"`       // "auto" (require refuses instead of degrading)
    Origin    string `json:"origin"`     // builtin | config | env — where the mode came from
    Reason    string `json:"reason"`     // "bd 1.1.0 lacks --if-revision (unknown flag)" | "exit 13: conditional-write-unsupported"
    BDVersion string `json:"bd_version,omitempty"`
}
```

Emission is **latched once per store instance**, guarded by the same mutex as the capability latch: the first capability veto on a store fires the event and the log line; subsequent vetoes are silent (log storms structurally impossible, mirroring `native_store_unavailable`). Because `internal/beads` is Layer 0 and must not import the event bus, the factory injects a callback at store-open:

```go
// internal/beads — factory wiring, nil-safe.
type OpenOptions struct {
    // ...
    OnConditionalWritesDegraded func(ConditionalWritesDegradedPayload) // nil ⇒ log-only (bare CLI contexts)
}
```

`OpenStoreAtForCity` wires it to the bus wherever a bus exists (controller, API server); short-lived CLI paths without a bus fall back to the structured log alone. The event lands in event history and the dashboard's existing event views with zero new UI work, and is the thing an operator alerts on instead of polling doctor.

Require-mode refusals do **not** get their own event type: each refusal is a typed error that propagates to the failing operation (a stalled drain is already loud), plus the store-open preflight `BeadsDiagnostic` and the doctor ERROR. What they do get is log discipline (12.3).

### 12.3 Structured diagnostics and log discipline

- **Every refusal and degrade line carries `mode` and `origin`.** Break-glass scope is per-process, so two processes of one city can legitimately resolve opposite modes; the first log line must make that visible without correlation work: `conditional_writes refused: store=rig/gastown gate=drain_reservation mode=require origin=env reason="bd 1.1.0 lacks --if-revision"`.
- **Store-open veto** (auto ∧ incapable) emits the factory-style `BeadsDiagnostic{PreflightGate:"conditional_writes", PreflightReason:...}` and one `conditional_writes_unavailable` structured log per store — the `native_store_unavailable` vocabulary operators already grep for.
- **Contention forensics:** every `PreconditionFailedError` carries `Expected` and `Current` revisions, so a genuinely contended key versus the CachingStore stale-revision livelock versus BdStore cross-key revision interference are distinguishable from the error text alone.
- **Startup is where env problems surface, fatally or loudly.** An unparseable `GC_BEADS_CONDITIONAL_WRITES` value fails startup naming the var, the raw value, and the grammar (`off|auto|require` — nothing else). A *valid* env value that contradicts an explicitly-set config value starts up but emits a startup structured log plus a typed event and a retained notice: `conditional_writes=off (env GC_BEADS_CONDITIONAL_WRITES) overriding require (config)`.

### 12.4 Notice retention

Resolve's notices are not stderr ephemera. They are retained **on the `Flags` value for the process lifetime** and rendered by every later surface:

```go
// internal/rollout
type Origin string // "builtin" | "config" | "env"

type NoticeKind string

const (
    NoticeEnvOverridesConfig NoticeKind = "env_overrides_config" // valid env contradicts explicit config
    NoticePendingRestart     NoticeKind = "pending_restart"      // on-disk config ≠ boot-latched value (recorded at reload)
    NoticeInvalidEnvIgnored  NoticeKind = "invalid_env_ignored"  // kill-switch flags only; Mode flags fail startup instead
)

type Notice struct {
    Flag   string     // registry Key
    Kind   NoticeKind
    Origin Origin     // origin of the EFFECTIVE value
    Detail string     // e.g. `require (city.toml) != off (latched at start) — restart to apply`
}

func (f Flags) Notices() []Notice // immutable copy
```

`controllerState` holds the boot-resolved `Flags`, so the notices survive log rotation by construction — the answer to "*why* is the effective mode what it is?" is recoverable from a three-week-old daemon without a restart. The reload path appends `NoticePendingRestart` when on-disk config diverges from a process-latched value; it never mutates existing notices. Boundary test (stage 4, once the wire exists): start a daemon with an env override contradicting a temp `city.toml`, query the status endpoint, assert the `env_overrides_config` notice is present verbatim.

### 12.5 Status wire and live-API doctor (stage 4)

The wire type rides the C2/go.mod-bump PR, which already forces genspec, the three tracked OpenAPI copies, and dashboard TS for `Bead.Revision` — the flag field is free cargo on an unavoidable regen. One boolean cannot express a mixed fleet, so the type is an aggregate **plus** a typed per-store array:

```go
// internal/api — Huma-registered; spec generated, never hand-written.
type BeadsConditionalWritesStatus struct {
    Mode      string                          `json:"mode" enum:"off,auto,require"`
    Origin    string                          `json:"origin" enum:"builtin,config,env"`
    Effective string                          `json:"effective" enum:"off,active,degraded,fail_closed,pending_restart"`
    Stores    []ConditionalWriteStoreVerdict  `json:"stores"`
    Notices   []RolloutNotice                 `json:"notices"`
}

type ConditionalWriteStoreVerdict struct {
    StoreID string `json:"store_id"`
    Kind    string `json:"kind" enum:"bd,native,sqlite-graph,caching,mem,file"`
    Probe   string `json:"probe" enum:"capable,incapable,unprobed"`
    Latch   string `json:"latch" enum:"capable,incapable,unlatched"`
    Capable bool   `json:"capable"` // probe ∧ latch, the value the write path actually uses
    Reason  string `json:"reason,omitempty"`
}
```

This is the daemon's **own latched snapshot** — boot-resolved mode, real per-store latches, retained notices — not a re-derivation. From this stage, `gc doctor` queries the live API whenever the city is up and renders that snapshot verbatim (probe *and* latch columns now both real); local re-resolution with the 12.1 banner becomes the fallback for a stopped city only. Doctor and the dashboard agree by construction because they render the same array.

### 12.6 Runbook entries

**Break-glass: disable CAS during an incident.**
The supported whole-city rollback is a config edit plus restart — the flag is process-latched, there is no hot flip:

```toml
# city.toml
[beads]
conditional_writes = "off"   # was "require"; restart the city to apply
```

`GC_BEADS_CONDITIONAL_WRITES` exists for deployments where config is baked and immutable — its named consumer. Its scope is **per-process**: it affects only processes that read it at start. Setting it in the controller's unit does *not* change what an operator shell or an agent-invoked `gc hook` resolves — expect the two vantage points to disagree, and read the `origin=` field in the first log line before concluding anything. Grammar is exactly `off|auto|require`; any other spelling (`disable`, `false`, `Require `) **fails the process at startup** naming the var, the raw value, and the grammar — a break-glass that silently no-ops at 2am is a failed break-glass. After the incident, remove the var: the `env_overrides_config` notice in doctor/status is the standing indicator that you forgot.

**Restart after a bd upgrade (or downgrade).**
Capability is probed lazily and latched per store instance; nothing is persisted (restart re-probes live state — no status files). Upgrading bd in place does **not** clear an existing incapable latch: the store stays DEGRADED (auto) or refusing (require) until the process restarts. Doctor makes this legible as `probe=capable latch=incapable` — the fix line it prints is "restart to re-probe", not "reinstall bd". A downgrade in place trips the unknown-flag classifier on the next CAS write, flips the latch incapable mid-run, and fires the degraded event once; fix PATH/version, then restart.

**Pending restart after a config edit.**
Editing `conditional_writes` on a running city records the pending-restart notice at reload (`pending restart: conditional_writes require (city.toml) != off (latched at start)`); doctor renders it as a WARNING. New components constructed after the reload still receive the **boot** value — that is the latch working, not a bug. Restart to apply.

**`require` on the deployed sqlite topology.**
Forbidden until the sqlite `CompareAndSetMetadataKey` integration test (a blocking deliverable of the C4/C6 PR) has soaked against the deployed store shape. Until then, run `auto` and watch the graph-store row in doctor. And the fleet-scoped invariant, stated plainly: CAS mutual exclusion holds only when **every** writer to a ledger is CAS-active or exactly one writer exists — one node at `off`, one older binary, or one Auto-degraded host re-opens the races for everyone. Doctor warns on exactly this combination (auto + DEGRADED + declared multi-writer topology); treat that warning as "the flag is currently decorative on this ledger."

## 13. Migration of existing ad-hoc flags

Stages 1–4 unavoidably leave two flag mechanisms in the tree: `internal/rollout` and the legacy pile (`cmd/gc/feature_flags.go`, `internal/api` `syncFeatureFlags`, two package-global `atomic.Bool`s, and ~8 divergent `GC_*` env parsers). The failure mode is not that the old code exists — it is that the old code keeps *recruiting*: an agent adding flag #3 greps for "feature flag", finds `applyFeatureFlags` with seven call sites versus `internal/rollout` with one consumer, and copies the global-setter pattern. This section makes the old mechanisms frozen at stage 1, absorbed on a committed schedule, and un-copyable in between.

### 13.1 Stage-1 freeze: the legacy mechanisms stop growing before they shrink

Both freeze tests land in the stage-1 PR, before any migration code. They are ratchets: shrinkage requires a baseline edit (loud, reviewed, trivially approved); growth fails CI naming the offending file.

**13.1.1 Legacy flag-mechanism golden list.** A boundary test in `cmd/gc` (same shape as `TestGCNonTestFilesStayOnWorkerBoundary`, `cmd/gc/worker_boundary_import_test.go:11`) walks non-test source and fails on any reference to the four legacy symbols beyond a checked-in inventory:

```go
// cmd/gc/legacy_flag_freeze_test.go — TEMPORARY: deleted in stage 5 when the inventory hits zero.
var legacyFlagInventory = map[string]int{ // file → reference count; shrink-only
	"cmd/gc/feature_flags.go":           1, // applyFeatureFlags definition
	"cmd/gc/cmd_start.go":               1, // :673
	"cmd/gc/controller.go":              1, // :923
	"cmd/gc/cmd_agent.go":               2, // :52, :70
	"cmd/gc/cmd_sling.go":               1, // :247
	"cmd/gc/api_state.go":               1, // :1808 (reload path)
	"cmd/gc/doctor_provider_catalog.go": 1, // :146
	"internal/api/server.go":            3, // syncFeatureFlags def :229 + calls :197, :203
}
// Frozen symbols: applyFeatureFlags, syncFeatureFlags,
// formula.SetFormulaV2Enabled, molecule.SetGraphApplyEnabled.
```

Test files are frozen by per-package count ceiling (not file inventory — `internal/molecule` alone has 44 setter save/restores today and enumerating them buys nothing). A new test using `SetFormulaV2Enabled` in a package whose ceiling is met fails with "use rollout.ForTest(t, rollout.WithFormulaV2(...)) instead".

**13.1.2 `GC_*` env-read frozen baseline.** A registry-driven AST lint in `internal/rollout` (precedent: `TestNoLeakVectorReadsAtPackageInit`) walks every package for `os.Getenv`/`os.LookupEnv` calls whose string-literal argument matches `^GC_`, and fails unless the (file, var) pair is in one of three buckets:

1. `internal/testenv`'s documented test-gate vars;
2. a registry `Spec.EnvOverride` read inside `rollout.Resolve` (the only production home for flag env reads);
3. the checked-in baseline `internal/rollout/gc_env_baseline.go` — an enumerated `[]envReadSite{{File, Var}}` covering today's identity/path/creds/tuning reads (`GC_DOLT_ARCHIVE_LEVEL`, `GC_EVENTS_ROTATION_MAX_SIZE_BYTES`, the supervisor re-export sites, etc.).

Non-literal env names in those calls are forbidden outside `internal/testenv`. A shadow flag now costs a reviewed baseline edit in a CODEOWNERS-adjacent file — mechanically more expensive than writing a Spec. Unlike the golden list, **this test is permanent infrastructure**: it outlives the migration and guards against flag #9 arriving as a bare `os.Getenv("GC_SKIP_EPOCH_VERIFY")`.

### 13.2 formula_v2: Spec on day one, code migration as a committed bead

The registry is born at N=2: the formula_v2 Spec registers in the stage-1 PR, months before its code migrates, so the legacy mechanism sits inside the anti-rot regime from day one and stage-5 slippage trips the Spec's own teeth (nightly radar files against the Owner bead; any PR touching `registry.go` hard-fails on the past-due entry).

```go
// internal/rollout/registry.go — registered in stage 1
{
	Key:            "daemon.formula_v2",
	Category:       InfraMigration,
	ConfigPath:     "daemon.formula_v2", // *bool, nil → enabled (default-ON kill of the v1 path)
	EnvOverride:    "",                  // no env var exists today; none is added
	Default:        BoolDefault(true),
	Owner:          Owner{Bead: "ga-XXXXX", GitHub: "@gastownhall/gascity-flags"},
	Expires:        "…",                 // mandatory: infra-migration is never immortal
	VersionAnchor:  "formula-v1 removal floor",
	SelectsBetween: [2]string{"graph-compiled v2 molecules (graph-apply instantiation)", "sequential v1 step execution"},
	Justification:  "selects between two mechanical formula-materialization transports during the v1→v2 migration; invisible to prompts",
}
```

The **stage-5 code migration is a blocking bead in the same milestone as CAS**, not a "follows later" note. Its deletion inventory (all verified against the current tree):

| Deleted | Replaced by |
|---|---|
| `cmd/gc/feature_flags.go` + all 7 call sites (13.1.1 table) | `Flags.FormulaV2()` resolved once in `loadCityConfig*`, threaded by DI |
| `internal/api/server.go` `syncFeatureFlags` (:197, :203, :229) | server options struct carries `rollout.Flags`; the server never re-resolves |
| `formula` package `formulaV2Enabled` atomic.Bool + `SetFormulaV2Enabled` | explicit parameter, the existing `ValidateHostRequirements(f, formulaV2Enabled bool)` shape |
| `internal/molecule/graph_apply.go:25` `graphApplyEnabled` atomic.Bool + `SetGraphApplyEnabled`/`GraphApplyEnabled` | field on the molecule/Instantiate options struct |
| `internal/formulatest/v2.go` `LockV2ForTest` mutex + helpers | nothing — per-instance values need no process mutex |
| ~44 setter save/restores in `internal/molecule` tests, plus `internal/formula` (`compile_test.go`, `requirements_test.go`, `testhelper_test.go`), `internal/graphroute`, `internal/dispatch`, `internal/api/handler_sling_test.go` | `rollout.ForTest(t, rollout.WithFormulaV2(false))` — compile-time-typed, `t.Parallel`-safe |

The `daemon.formula_v2` config field, its accessor, and its existing per-field `mergeFragment` preservation branch (`compose.go:1042`) are **not** deleted in stage 5 — they are the flag's config home until the flag itself graduates to deletion under its own version-anchored trigger, at which point `daemon.formula_v2` gets its own tombstone.

### 13.3 Absorbing the env one-offs: EnvSemantics preserves shipped precedence

The two legacy env gates have **opposite precedence today**, and absorption must not silently unify them — a precedence flip on a shipped operator interface is a breaking change, never a migration side effect:

| Env var | Config home | Precedence today (verified) | `Spec.EnvSemantics` | Category |
|---|---|---|---|---|
| `GC_DOLT_AUTO_GC_ENABLED` | `[dolt] auto_gc_enabled` (`*bool`) | fills **only when config is nil** — explicit config wins (`dolt_start_managed.go:972`) | `EnvFillsNil` | infra-killswitch (no Expires; long-lived allowed) |
| `GC_EVENTS_ROTATION_ENABLED` | `[events.rotation] enabled` | set env **wins over config**; invalid warns and keeps config (`providers.go:998`) | `EnvOverrides` | infra-killswitch |
| `GC_BEADS_CONDITIONAL_WRITES` | `[beads] conditional_writes` | (new) | `EnvOverrides` | infra-rollout |

`Resolve` honors the per-Spec semantics:

```go
switch spec.EnvSemantics {
case EnvFillsNil: // legacy contract: env is a default, config is authoritative
	if envOK && !explicitInConfig {
		val, origin = envVal, OriginEnv
	}
case EnvOverrides: // break-glass contract: env wins, contradiction is push-loud
	if envOK {
		if explicitInConfig && envVal != cfgVal {
			notices = append(notices, contradictionNotice(spec, cfgVal, envVal)) // + startup log + typed event (§ resolution)
		}
		val, origin = envVal, OriginEnv
	}
}
```

Absorption mechanics, both flags, stage 5: register the Spec (ConfigPath reflection-verified against the existing toml tags — no config field moves), reroute the `os.Getenv` read from `dolt_start_managed.go` / `providers.go` into `Resolve`, delete `parseEnvAutoGCEnabled` and `parseEventsRotationEnabled` in favor of the one shared bool grammar, and register both vars in `LeakVectorVars`. **Grammar superset test:** the shared bool grammar is extended with `enabled`/`disabled` (case-insensitive, trimmed) specifically so it is a strict superset of both legacy parsers; a unit test feeds every spelling either legacy parser accepted (`ParseBool` spellings, `ON`/`OFF`, `y`/`yes`/`enabled`, …) and asserts identical results — no operator's working unit file breaks on upgrade.

Out of scope, deliberately: the sibling numeric tuning vars (`GC_DOLT_MAX_CONNECTIONS`, `GC_EVENTS_ROTATION_MAX_SIZE_BYTES`, `GC_EVENTS_ROTATION_RETAIN_AGE`, …) are configuration overlays, not rollout gates — they do not enter the registry, but they ARE pinned in the 13.1.2 baseline so they cannot multiply silently. The supervisor child-env re-export of `GC_DOLT_AUTO_GC_ENABLED` (`beads_provider_lifecycle.go:2003/2039`) is part of the flag's shipped interface and is untouched; its read sites live in the baseline. If the fills-nil/overrides pair is ever unified on env-wins, that is a standalone, release-noted breaking change with a doctor callout — with its own migration section, not this one.

### 13.4 The graph_workflows tombstone

`daemon.graph_workflows` is a live deprecated alias today: field at `config.go:2297`, honored only when `formula_v2` is absent (`config.go:4290`), with its own clause in the merge special case (`compose.go:1042`). Stage 1 registers the retirement **obligation** (a bead linked from the formula_v2 Owner bead — it cannot be forgotten); stage 5 executes it:

```go
// internal/config/undecoded.go — mechanism ships in stage 1; this entry is minted in stage 5
var retiredKeys = []RetiredKey{
	{
		Key:       "daemon.graph_workflows",
		RemovedIn: "v1.5", // gc version anchor at the stage-5 merge — never a wall-clock date
		Message: "daemon.graph_workflows was a deprecated alias for daemon.formula_v2 " +
			"(alias removed in v1.5); set daemon.formula_v2 instead",
	},
}
```

A retired key downgrades from fatal-unknown-key to this warning; the alias-honoring branch at `config.go:4290` and the `graph_workflows` clause at `compose.go:1042` are deleted in the same PR. The nightly radar (not PR CI) flags the tombstone for deletion once the current version anchor exceeds `RemovedIn+1` — version-anchored because this fleet deploys branches, and "one release" has no machine meaning here.

### 13.5 Order and exit criteria

Freeze (stage 1) → CAS ships on the new subsystem (stages 2–4) → absorption (stage 5, committed bead). The milestone is closed only when:

- `grep -rn "applyFeatureFlags\|syncFeatureFlags\|SetFormulaV2Enabled\|SetGraphApplyEnabled\|LockV2ForTest" --include="*.go"` returns zero hits, tests included;
- `cmd/gc/feature_flags.go` and `internal/formulatest/v2.go` are deleted, and `cmd/gc/legacy_flag_freeze_test.go` is deleted with them (its inventory reached zero — a freeze test guarding nothing is debt too);
- `os.Getenv`/`LookupEnv` reads of `GC_DOLT_AUTO_GC_ENABLED` and `GC_EVENTS_ROTATION_ENABLED` exist only inside `rollout.Resolve`;
- `graph_workflows` appears only in `retiredKeys` and its test;
- the 13.1.2 env-read baseline test remains, permanently, as the tax collector for any future shadow flag.

## 14. Rejected alternatives and red-team dispositions

Rejection here followed three tests, applied uniformly: (1) **N≥1** — a mechanism with zero present consumers does not ship (`no premature abstraction`); (2) **deterministic teeth** — any merge-blocking check must produce the same verdict for the same commit on any day; (3) **honest claims** — an enforcement claim CI cannot actually make is reworded to what review enforces, never left inflated. Every alternative below failed at least one.

### 14.1 Rejected alternatives

#### 14.1.1 Central `[features]` table

```toml
# REJECTED
[features]
beads_conditional_writes = "require"
daemon_formula_v2        = true

# ADOPTED — the flag lives on the subsystem it gates
[beads]
conditional_writes = "require"   # beside bd_compatibility
```

Rejected because it fights the config system's native idiom on three fronts. Progressive activation is *by section presence* (`md.IsDefined`), so a flag divorced from its owning section stops participating in the activation model its subsystem uses. A new `[features]` table is a **new section**, which under `mergeFragment`'s per-section semantics needs its own merge wiring from day one — the "central table is simpler" intuition is exactly backwards, since `[beads]` placement reuses the existing table plumbing plus one per-field preservation branch (§4). And co-location is the real reviewer affordance: `conditional_writes` sits one line from `bd_compatibility`, whose version-gate semantics it will eventually merge into at graduation. The one thing a central table buys — discoverability — is recovered losslessly by the registry, `gc doctor`'s Rollout Flags section, and (stage 4) the status wire, which enumerate every flag regardless of which section holds it.

#### 14.1.2 Per-consumer knobs

```toml
# REJECTED
[beads]
conditional_writes_dispatch = "require"   # C4 epoch fence
conditional_writes_drain    = "auto"      # C6 reservation
conditional_writes_api      = "off"       # C2 If-Match
```

Rejected because it configures a race back into existence. `gc.control_epoch` and `gc.drain.reserved_by` can live on the *same store*; `dispatch=require, drain=off` makes one process a CAS writer and a legacy writer against one ledger — the mixed-writer clobber that §9's fleet invariant exists to forbid, now expressible in TOML and green in every test. It also triples the lifecycle surface (three Specs, three graduation predicates, three tombstones, a 12-cell operational matrix in doctor) and creates permanent partial states with no forcing function to collapse them. Staged adoption is real but it is a *code-landing* sequence (C4/C6 in stage 3, C2 in stage 4), not a config surface: an operator opting in opts the whole write discipline in.

#### 14.1.3 Plain-bool gate

```go
// REJECTED
ConditionalWrites *bool `toml:"conditional_writes,omitempty"` // on|off cliff

// ADOPTED
func (b BeadsConfig) ConditionalWritesMode() rollout.Mode // Off | Auto | Require
```

Rejected because a bool makes heterogeneous-fleet rollout a cliff between "off" (no protection) and "refused writes" (one stale bd bricks the drain path). The middle state is not decoration: `Auto` = *use CAS where the resolved store is capable, degrade loudly elsewhere* is what makes incremental adoption survivable, and `Require` = *fail closed* is a distinct contract, not "very on". The repo's own correctness rollouts already walk this shape (`GC_WORK_RECORD_ENFORCE` warn→enforce, `GC_WISP_GC_*` dry-run→act). Kill-switches that genuinely are binary keep the `*bool` idiom — the two value kinds coexist in the registry by design (§2).

#### 14.1.4 RemovalTrigger predicate DSL

```go
// REJECTED: a predicate mini-language, evaluator, and "version-bound flag"
// classifier — generic machinery with exactly one instantiation.
type RemovalTrigger struct{ Expr string } // "BD_PREV_VERSION >= 1.2.0" …

// ADOPTED: one plain test in the TestBDVersionPins family (~20 lines).
func TestConditionalWritesGraduationStages(t *testing.T) {
	pins := loadDepsEnv(t)
	spec := rollout.SpecFor(t, "beads.conditional_writes")
	// Stage 1: installable bd crossed the floor, default still Off.
	if deps.CompareVersions(pins.BDVersion, bdConditionalWritesMinVersion) >= 0 &&
		spec.Default == rollout.Off && !flipDeferredWithin(spec, pins) {
		t.Fatalf("bd %s has --if-revision: flip default Off→Auto (owner %s/%s) or set FlipDueBy",
			pins.BDVersion, spec.OwnerBead, spec.OwnerHandle)
	}
	// Stage 2: min-supported floor crossed, flag still registered.
	if deps.CompareVersions(pins.BDPrevVersion, bdConditionalWritesMinVersion) >= 0 {
		t.Fatalf("min-supported bd %s has --if-revision: DELETE flag+accessor+config field+legacy branches; mint tombstone RemovedIn=%s",
			pins.BDPrevVersion, pins.Anchor)
	}
}
```

Rejected at N=1. The hypothetical second version-anchored flag will likely key on a *different* anchor source (a `go.mod` library version, not `deps.env`), so the DSL would take its first breaking rewrite at N=2 — the classic framework-before-second-consumer failure. The plain test has identical teeth (deterministic, fires only when someone edits `deps.env`, names the owner) and zero grammar to maintain. Extract a shared helper if and when a second such test exists.

#### 14.1.5 Soft cap (~8 active non-Stable flags fails CI)

Rejected as N=0 governance. The complete historical inventory of flag-shaped things in this tree is ~8–10; the registry ships with 2. The cap's only guaranteed firing scenario is an engineer adding a legitimate kill-switch *during an incident*, where the fix-under-fire is bumping the constant — training everyone that the cap is editable friction. Worse, the cap actively sharpened the `Stability=Stable` abuse (LD-2): promoting a flag to Stable freed cap headroom *and* dodged expiry in one line. Deleting the cap and the Stability enum together closes that loop. Anti-rot is carried by per-flag mechanisms that scale with N instead of gating it: mandatory Expires + version anchors for rollout/migration categories, the nightly radar filing beads against owners, and CODEOWNERS on `registry.go`. If flag-count anxiety ever materializes, the answer is a doctor INFO line, not a build failure.

#### 14.1.6 Daemon-only EnvOverride restriction (red-team OO-8 sub-fix)

Proposed: restrict `EnvOverride` on process-latched correctness flags to the daemon entry point, so `gc hook`/`gc sling` CLI paths resolve config-only and cross-process mode divergence becomes inexpressible. **Rejected for v1.** It gives the resolver entry-point awareness — a new resolution axis ("which binary am I?") threaded through `ResolveOptions` and every loader — to prevent a divergence the adopted fixes already make visible in the first log line: break-glass scope is documented as per-process (§5), every refusal/degrade diagnostic carries `mode=… origin=…`, and an env override contradicting explicitly-set config emits a startup structured log plus typed event. The threat model (operator break-glasses the controller unit while agent-invoked CLI paths still resolve `require`) is real but diagnosable in seconds under the adopted design. Revisit trigger: one actual bifurcated incident where origin-tagged diagnostics proved insufficient — then the restriction returns as a per-Spec field, not a resolver rewrite.

#### 14.1.7 Direction-explicit `force-off` env grammar (red-team PV-4 sub-fix)

```bash
# REJECTED: a second, direction-aware grammar for downgrades
GC_BEADS_CONDITIONAL_WRITES=force-off

# ADOPTED: mode names only; anything else fails startup on correctness flags
GC_BEADS_CONDITIONAL_WRITES=off|auto|require
```

Proposed so that downgrading a declared `require` could never be a typo'd truthy value. **Rejected as superseded**, not as wrong: the adopted grammar is *stricter* than the proposal's premise. Mode-flag env vars accept only the three literal mode names — no truthy spellings exist for tri-state flags at all — and an unparseable value on a correctness-category flag fails startup fast, naming the var, the raw value, and the grammar (§5). A typo therefore cannot downgrade anything silently; it stops the process with instructions. Adding `force-off` on top would be a second grammar to document, parse, and test, defending against a scenario with no remaining teeth.

#### 14.1.8 Other mechanisms deleted or deferred (cross-reference)

Each of these appeared in an earlier draft and was removed by a specific finding; the surviving decision lives in the cited section.

| Mechanism | Fate | Killed by | Survivor |
|---|---|---|---|
| `Latch (process\|reload)` Spec field | Deleted | PV-7, OO-1, T-2, Y-5 | v1 is process-latched for all flags (§6) |
| `Stability` enum, `IntroducedIn`, `GraduationCriterion` | Deleted | LD-2, Y-5 | Per-category lifecycle rules + `GraduatedIn` stamped at flip (§11) |
| `WithConditionalWrites(mode)` store option | Deleted | T-3 | Factory stamps mode; `ResolveConditionalWriter(store)` takes no mode param (§6) |
| `WithBDCapabilityProbe` injection seam | Deleted | T-7 | Probe rides the store's existing `CommandRunner` (§7) |
| `withoutConditionalWrites(store)` test wrapper | Deleted | T-5 | `mem.DisableConditionalWrites` instance toggle (§10) |
| String-keyed `ForTest(t, "key", val)` | Deleted | T-9 | Generated typed `With*` option funcs (§10) |
| Generic registry-driven merge-coverage reflection harness | Deleted | Y-9 | Hand-written per-flag merge test on the `daemon.formula_v2` template (§4) |
| Wall-clock `Expires` in merge-blocking CI | Deleted | LD-1, T-6, Y-2 | Nightly radar + diff-gated hard failure (§11) |
| Five-value Origin (`builtin\|pack\|city\|fragment\|env`) + `/v0/config/explain` extension | Deferred | Y-3 | Three-value Origin `builtin\|config\|env`; per-layer costed as new compose.go plumbing when asked for (§5) |
| Status-wire flag struct in slice 1 | Deferred to stage 4 | Y-8, OO-5 | Rides the C2/go.mod-bump PR's unavoidable spec regen (§12) |
| `ModelImprovementJustification` as a CI-checked tooth | Demoted to documentation | LD-8, Y-11 | CODEOWNERS review on `registry.go` is the semantic gate (§13) |

### 14.2 Red-team disposition table

Five lenses, 57 findings, zero findings rejected outright; two *sub-fixes* rejected (§14.1.6, §14.1.7). IDs number each lens's findings in review order. Legend: **A** = adopted as specified; **A/am** = adopted with an amended mechanism; **A/st** = adopted, lands in a named later stage; **Rej** = sub-fix rejected.

#### principle-violation

| ID | Sev | Finding | Disp. | Resolution |
|---|---|---|---|---|
| PV-1 | BLK | Whole-table `[beads]` LWW fragment merge silently wipes `conditional_writes` (require→off) | A | Mandatory per-field `IsDefined("beads","conditional_writes")` branch in `mergeFragment` + hand-written merge regression test per flag (§4); generic harness dropped per Y-9 |
| PV-2 | HIGH | Import-edge prompt-boundary test unimplementable (rendering lives in cmd/gc `package main`; `PromptContext.Env` leaks values without imports) | A/am | Extract `internal/prompt` (PR-1a) so the forbidden edge exists; registry-driven AST lint over PromptContext construction/Env writes/FuncMaps; value-flow half honestly stated as review-governed (§13) |
| PV-3 | HIGH | Registry costs incentivize bypass via new bare `GC_*` getenv or `*bool` idiom | A | Frozen-baseline `GC_*` env-read inventory test + golden-list freeze on legacy flag mechanisms, both in stage 1; reverse parity mechanical for `rollout.Mode` only; `*bool` classification stated review-governed (§8) |
| PV-4 | MED | Stale env var silently downgrades an explicit `require` | A + Rej | (a) startup log + typed event when env contradicts explicit config, (b) origin on status wire — adopted (§5); (c) `force-off` spelling rejected → §14.1.7 |
| PV-5 | MED | Registry polices form, not semantics — judgment-in-Go gate wearing infra clothes passes every test | A | Required `SelectsBetween [2]string`; CODEOWNERS on `registry.go`; litmus questions in file header + PR template; design text says review-with-teeth explicitly (§13) |
| PV-6 | MED | "Per-agent scope inexpressible by construction" overstated — `config.Agent` can express it | A | Claim reworded; reflection test fails if Agent/AgentPatch/AgentOverride gains a `rollout.Mode` field; contributor doc names the forbidden shape regardless of declaration site (§2) |
| PV-7 | LOW | Reload two-truths: components hold different snapshots invisibly | A | Fix option (a) taken: v1 process-latched for all flags, reload machinery deleted, `Latch` field deleted (§6) |

#### testability

| ID | Sev | Finding | Disp. | Resolution |
|---|---|---|---|---|
| T-1 | HIGH | The claimed `run()` composition root does not exist (~30 independent config-load sites) | A | Resolve folded into `loadCityConfig`/`loadCityConfigWithBuiltinPacks`; factory stamps mode onto every store it opens; entry-point tests for controller/hook/sling/api (§6) |
| T-2 | HIGH | `latch=process` self-contradictory with hot-reload re-Resolve | A | Whole-process latch; reload carries the boot snapshot into all later components; regression test: boot Off → rewrite Require → reload → new store observes Off + Notice (§6) |
| T-3 | MED | Mode has two homes (store option and seam parameter) that tests can wire contradictorily | A | Single home: factory-stamped; `ResolveConditionalWriter(store)` reads it; `WithConditionalWrites` deleted (§6) |
| T-4 | MED | Fake-store revision discipline unspecified — green CI predicts nothing about bd | A | Store-agnostic conformance suite (Mem/File/Caching/sqlite in unit CI; BdStore under `//go:build integration`); bump discipline is the interface doc comment; slots into the PR #3714 contract-test system (§10) |
| T-5 | MED | `withoutConditionalWrites` wrapper silently strips all five optional store interfaces | A | Wrapper deleted; per-instance `DisableConditionalWrites` toggle keeps the interface set intact (§10) |
| T-6 | MED | Date-based Expires is a zero-diff CI time bomb | A | Merged into LD-1 disposition (§11) |
| T-7 | LOW | Duplicate probe seams let tests wire probe/runner contradictions | A | One seam: probe runs through the existing `CommandRunner`; `WithBDCapabilityProbe` deleted (§7) |
| T-8 | LOW | Exported mutable `Registry` slice leaks mutations across parallel tests | A | Canonical slice unexported behind a read-only accessor; validator and `ForTest` take a `[]Spec` parameter (§2) |
| T-9 | LOW | String-keyed `ForTest` reintroduces stringly reads; flag removal degrades to runtime failure | A | Typed `With*` option funcs generated per accessor; deletion breaks tests at compile time (§10) |

#### cas-correctness

| ID | Sev | Finding | Disp. | Resolution |
|---|---|---|---|---|
| CC-1 | HIGH | Reservation CAS collapses idempotent re-entry into a false loss → stranded undrainable members | A | Exit-9 contract: re-read; `current==control.ID` → success (self-win); other → skip; preserves the drain.go three-outcome contract; MemStore re-entry + ambiguous-retry tests (§9) |
| CC-2 | HIGH | Attach epoch CAS ordering unspecified; both orderings have distinct wedge modes | A | CAS-LAST pinned; exit-9 loser wired into existing `isPartialAttemptAttachError`/`molecule_failed` recovery; concurrent-Attach integration test sharing an idempotency key (§9) |
| CC-3 | HIGH | Ambiguous transport errors may be committed CAS writes; blind re-read converts self-wins into false losses | A | Per-consumer ambiguity contract: writer-identifying values must self-win-check on re-read; epoch tolerates false loss only via `findExistingAttach` idempotency, documented on the seam; injected-ambiguity test via fake runner (§9) |
| CC-4 | HIGH | No sqlite ConditionalWriter exists; the deployed controller is exactly where the fence matters | A | Promoted to blocking deliverable of the C4/C6 PR + integration test against the deployed store shape; doctor renders the graph-store verdict; runbook forbids `require` on the deployed topology until it soaks (§9, stage 3) |
| CC-5 | MED | Mixed-writer fleets (or one reloaded process) silently re-open the races | A | Fleet-scoped invariant in design + runbook; whole-process latch pins reload; doctor warns on DEGRADED under declared multi-writer topology (§9) |
| CC-6 | MED | Latching incapable on bare exit 13 conflates capability absence with per-write policy refusals | A | Latch only when the machine-parseable body code equals `conditional-write-unsupported`; bare 13 → typed non-latching refusal; both encoded in classifier tests (§7) |
| CC-7 | MED | Pre-#4682 bd never emits 13 — it rejects `--if-revision` as an unknown flag; the loud-degrade cell was unreachable | A | Classifier maps usage/unknown-flag errors mentioning `--if-revision` → `ErrConditionalWriteUnsupported` + latch; test for the old-bd rejection string (§7) |
| CC-8 | MED | Divergent conflict granularity per backend + emulation starvation on metadata-hot control beads | A | Granularity contract on the interface (assume neither value- nor revision-level semantics); bounded emulation loop + typed exhaustion error; bd-sql value-CAS (`ReleaseIfCurrent` template) evaluated (§9) |
| CC-9 | MED | CachingStore refresh-or-patch template leaves stale revisions → exit-9 livelock | A | Evict, never patch: delete cache entry on CAS-success-with-failed-refresh and on every PreconditionFailed; livelock regression test is a merge gate of the stage-2 PR (§9) |
| CC-10 | LOW | Single-verb help probe + construction-time subprocess tax on short-lived CLI paths | A | Lazy memoized probe on first conditional write; greps all four verb helps; doctor renders probe verdict and runtime latch separately (§7) |

#### lifecycle-debt

| ID | Sev | Finding | Disp. | Resolution |
|---|---|---|---|---|
| LD-1 | BLK | Wall-clock Expires reds every PR with zero diff; fleet treats red as a stall; trains date-bumping | A | No bare date-vs-`time.Now()` in the Check path; version-anchored deterministic tests; wall-clock staleness → nightly non-blocking radar filing beads; expiry hard-fails PR CI only when `registry.go` is in the diff; tombstones version-anchored (§11) |
| LD-2 | HIGH | `Stability=Stable` is an immortality hatch; the cap sharpens the incentive | A | Stability enum deleted; per-category rules in `registry_test`: rollout/migration may never be immortal, terminal state is deletion; only killswitch is long-lived; cap deleted (§11, §14.1.5) |
| LD-3 | HIGH | Removal predicate keyed to `BD_PREV_VERSION`, which historically never moves; no terminal-state check | A | Two-stage test: `BD_VERSION` crosses floor ⇒ demand Off→Auto flip; `BD_PREV_VERSION` crosses ⇒ demand deletion of flag/accessor/config field/legacy branches; `GraduatedIn` recorded at flip (§11) |
| LD-4 | HIGH | Nothing forces the stage-5 formula_v2 migration; the old mechanism keeps recruiting | A | Stage-1 freeze (golden-list boundary test + `GC_*` baseline); formula_v2 Spec registered day one with its own expiry so slippage trips its own teeth; migration is a committed same-milestone blocking bead; `graph_workflows` tombstone obligation registered (§8, stage 1/5) |
| LD-5 | MED | Owner is a decorative bead ID no test can resolve to a human | A | Dual Owner (bead ID + GitHub handle/team); `registry.go` under CODEOWNERS with a named human team (§2) |
| LD-6 | MED | Trigger firing forces a rush Require-path flip inside an unrelated (possibly CVE) bump PR | A | `FlipDueBy = current anchor + 1 bump` — machine-checked, diff-visible, bounded deferral; silent-forever stays impossible (§11) |
| LD-7 | MED | Built-in default lives in two files (Spec.Default vs accessor mapping) with no equality check | A | `registry_test` constructs a zero-value `config.City` and asserts each flag's typed accessor equals `Spec.Default` (§2) |
| LD-8 | LOW | Non-empty `ModelImprovementJustification` is compliance theater | A/am | Field kept as documentation; enforcement claim relocated to the CODEOWNERS human gate; the suggested min-length/content lint not taken — a content lint is the same theater with more grammar (§13) |
| LD-9 | LOW | Tombstone lifetime "one release" is meaningless in a branch-deployed fleet | A | Tombstones minted with `RemovedIn=<version anchor>`; radar flags for deletion once the current anchor exceeds `RemovedIn+1`; no wall clock (§11) |

#### operability-observability

| ID | Sev | Finding | Disp. | Resolution |
|---|---|---|---|---|
| OO-1 | BLK | Hot-reload hands a re-Resolved mode to new stores while old stores hold the boot mode — legacy and CAS writers race in one process | A | Whole-process latch; reload path carries the boot-resolved Flags into all later-constructed components; persistent pending-restart Notice → doctor WARNING; regression test pinned (§6) |
| OO-2 | HIGH | Invalid config value (`"requre"`) silently resolves to Off; the cited precedent is itself broken | A | Registry-driven `ValidateSemantics` walk rejects out-of-enum values fatally, naming field/value/allowed set; accessors only ever map `""`; pre-existing `NormalizedBDCompatibility` silent-normalize fixed in the same PR (§4) |
| OO-3 | HIGH | Doctor is a re-derivation (its shell env, its PATH, no view of daemon latches) and can lie in both directions | A/st | Slice 1: explicit local-resolution banner; stage 4: doctor queries the live API and renders the daemon's own latched snapshot; latched-vs-on-disk divergence rendered as pending-restart (§12) |
| OO-4 | HIGH | Break-glass env fails open on typo — one unread stderr line, then the wrong mode | A | Unparseable env on a correctness flag fails startup fast, naming var/raw value/grammar; Notices retained on the Flags value for the process lifetime (§5) |
| OO-5 | MED | The `{mode, capable, active}` triple cannot express a mixed fleet | A/st | Wire type is aggregate verdict + typed per-store array `{store_id, kind, capable, reason}` + origin + retained Notices; rides the stage-4 regen (§12) |
| OO-6 | MED | DEGRADED — the most-likely-to-persist state — emits nothing pushable | A | Typed `beads.conditional_writes.degraded {store, mode, reason, bd_version}` event, registered via `events.RegisterPayload`, latched once per store (§12, stage 2/3) |
| OO-7 | MED | Uniform env-wins silently inverts `GC_DOLT_AUTO_GC_ENABLED`'s fills-nil precedence on absorption | A | Per-Spec `EnvSemantics (overrides\|fills-nil)` preserves each legacy flag's contract; unification only as an explicit release-noted breaking change; env-contradicts-config Notice/event (§5) |
| OO-8 | MED | Env is per-process, config per-city; two processes of one city can resolve opposite modes undetected | A + Rej | Core adopted: break-glass documented per-process; mode+origin in every refusal/degrade diagnostic; env-contradicts-config startup event. Daemon-only EnvOverride sub-fix rejected → §14.1.6 |
| OO-9 | LOW | No exit-code contract for the doctor section | A | Pinned by test: FAIL-CLOSED and radar-surfaced past-due items = ERROR + nonzero exit; DEGRADED = warning + exit 0 (§12) |
| OO-10 | LOW | Resolve's Notices have no defined lifetime; origin facts evaporate after startup | A | Notices retained on the Flags value for the process lifetime; rendered by doctor and (stage 4) the status wire (§5) |

#### yagni-scope

| ID | Sev | Finding | Disp. | Resolution |
|---|---|---|---|---|
| Y-1 | HIGH | The registry ships with one flag — an N=1 abstraction against the repo's two-implementations rule | A/am | Fix option (b): two Specs registered day one (beads CAS + formula_v2, each with owner/anchor/expiry); the formula_v2 code migration is a committed same-milestone blocking bead whose slippage trips its own Spec's teeth (§8) |
| Y-2 | HIGH | Calendar-triggered CI failures wedge the autonomous merge fleet | A | Merged into LD-1 disposition (§11) |
| Y-3 | HIGH | Five-value Origin requires per-field provenance plumbing that does not exist; "extend explain" is bespoke | A | Origin collapsed to the three zero-loader-change values `builtin\|config\|env`; per-layer origin and the explain extension deferred and honestly costed as new compose.go plumbing (§5) |
| Y-4 | HIGH | "Settle env precedence once" retroactively changes a live production knob | A | Same mechanism as OO-7: per-Spec `EnvSemantics`; new flags default to overrides, absorbed flags keep their shipped precedence (§5) |
| Y-5 | MED | 13-field Spec is form-filling tax burying the two fields that matter | A/am | Five fields deleted (Stability, IntroducedIn, GraduationCriterion, Latch, plus the cap); Category *kept* — amended from taxonomy label to carrier of enforced per-category lifecycle rules; `SelectsBetween` added per PV-5 (§2) |
| Y-6 | MED | Predicate DSL built for one predicate | A | One plain Go test in the `TestBDVersionPins` family; no DSL → §14.1.4 |
| Y-7 | MED | Soft cap is governance for a population problem that does not exist | A | Cap deleted → §14.1.5 |
| Y-8 | MED | Four observability surfaces for a default-off experimental flag bloat the correctness PR | A | Slice 1 ships doctor only; status wire rides the stage-4 regen that Bead.Revision forces anyway; explain deferred (§12) |
| Y-9 | MED | Generic merge-coverage reflection harness has zero consumers and known `toml.MetaData` subtleties | A | Harness deleted; hand-written per-flag merge test on the `daemon.formula_v2` template; one-sentence lifecycle-doc rule covers future new-section flags (§4) |
| Y-10 | LOW | Import-boundary test oversold as making smuggling "structurally impossible" | A | Coverage stated honestly: CI blocks the naive import/AST paths; value-flow half is review-governed with the PR-template checklist item (§13, with PV-2's mechanisms) |
| Y-11 | LOW | Justification-as-test-enforced-string inverts its purpose | A | Same disposition as LD-8: documentation field; litmus lives in the file header and PR template; CODEOWNERS is the gate (§13) |
| Y-12 | LOW | CAS env override has no demonstrated consumer (process-latched ⇒ restart either way) | A | Kept with its consumer named in the Spec rationale: deployments with baked/immutable config, where env is the only injectable surface (§5) |

### 14.3 Audit summary

57 findings across six lenses: 2 BLOCKERs and 9 HIGHs adopted with normative amendments (the fragment-merge preservation branch, whole-process latching, the sqlite ConditionalWriter promotion, and the deterministic lifecycle teeth being the four that materially reshaped the design); 0 findings rejected outright; 2 sub-fixes rejected with recorded revisit triggers (§14.1.6, §14.1.7); 11 mechanisms deleted and 3 deferred with named owners for their return conditions (§14.1.8). Every disposition above cites the section where the surviving mechanism is specified; if a future PR touches one of these seams, this table is the record of *why* the seam looks the way it does.

## CAS rollout plan (staged)

STAGE 1 — Subsystem + flag plumbing (no behavior change; flag inert). PR-1a: extract prompt rendering from cmd/gc package main into internal/prompt (mechanical move; enables the real import-boundary test). PR-1b: internal/rollout (unexported registry, Spec, Resolve with injected LookupEnv, typed Flags + ForTest With* options, Notices retained on Flags); TWO Specs registered day one (beads CAS infra-rollout + formula_v2 infra-migration, each with dual Owner, version anchor, expiry); BeadsConfig.ConditionalWrites field + pure accessor + jsonschema enum; the per-field IsDefined("beads","conditional_writes") preservation branch in mergeFragment + hand-written merge regression test; registry-driven load-time enum validation (and the bd_compatibility silent-normalize bugfix); Resolve folded into loadCityConfig/loadCityConfigWithBuiltinPacks; reload path carries the boot-latched snapshot + pending-restart Notice + regression test; registry tests (completeness, ConfigPath reflection, Default==zero-value-accessor equality, EnvOverride∈LeakVectorVars, per-category immortality rules, Agent-struct rollout.Mode guard); freeze tests (legacy flag-mechanism golden list; GC_* env-read frozen baseline); prompt-boundary import test + registry-driven AST lint; CODEOWNERS line for registry.go; gc doctor Rollout Flags section (local-resolution banner, pinned exit codes). GATE: make test + entry-point tests green; flag resolves but nothing consumes it.

STAGE 2 — ConditionalWriter machinery in internal/beads (still no consumer). Interface + typed errors (PreconditionFailedError{Expected,Current}, ErrConditionalWriteUnsupported) with the revision-bump contract as the interface doc comment; BdStore --if-revision argv building + exit-code classifier (exit-9 defensive JSON parse; exit-13 latch ONLY on body code conditional-write-unsupported; unknown-flag-mentioning---if-revision → unsupported+latch; bare-13 → typed non-latching refusal); lazy memoized four-verb capability probe through the existing CommandRunner (no WithBDCapabilityProbe); dedicated CAS retry policy (re-read before re-attempt; bounded emulation loop + typed exhaustion; ambiguity self-win contract); factory stamps the resolved Mode onto every store it opens; ResolveConditionalWriter(store) seam; MemStore/FileStore native implementations + DisableConditionalWrites instance toggles; CachingStore forward + EVICT on success-with-failed-refresh AND on PreconditionFailed (livelock regression test is a MERGE GATE of this PR); NativeDoltStore delegation behind the library-version build reality; conformance suite over Mem/File/Caching in unit CI + BdStore under //go:build integration (slots into contract-test system); typed beads.conditional_writes.degraded event registered. GATE: conformance suite green across all in-process stores; classifier fake-runner tests cover 9/13-with-body/13-bare/unknown-flag/ambiguous-committed.

STAGE 3 — C4 + C6 consumers (flag becomes real). BLOCKING deliverable: sqlite graph store CompareAndSetMetadataKey (single conditional UPDATE, ReleaseIfCurrent template) + integration test against the deployed store shape (staged against the deployed store shape). C4: molecule.Attach read-compare-SetMetadata collapses to CompareAndSetMetadataKey on graphBeadStore(), CAS-LAST, exit-9 loser wired into isPartialAttemptAttachError/molecule_failed recovery; concurrent-Attach integration test sharing an idempotency key. C6: reserveDrainMember → CompareAndSetMetadataKey on drainMemberOwningStore(member) with the three-outcome self-win re-read (re-entry + ambiguous-retry MemStore tests). Doctor renders per-store verdicts incl. the graph-class store; runbook: fleet-scoped mixed-writer invariant + require forbidden on deployed topology until the sqlite test soaks; degraded event live. GATE: four-cell matrix tests per consumer; off-mode byte-identical assertion; soak on the reference deployment in auto before recommending require anywhere.

STAGE 4 — beads library bump + C2 API. go.mod bump absorbs Bead.Revision on the wire in the SAME PR: genspec regen, three tracked OpenAPI copies, dashboard TS, make dashboard-check; typed If-Match Huma header (ETag=revision); apierr precondition_failed → HTTP 412 with expected/current; explicit conditional_writes_unsupported apierr when If-Match is presented while inactive (never silently ignore a precondition); no-If-Match requests keep legacy semantics. The status-wire beads_conditional_writes struct (aggregate verdict + typed per-store array + origin + retained notices) rides this PR's unavoidable regen; doctor switches to querying the live API when the city is up. GATE: TestOpenAPISpecInSync, dashboard-check, 412/If-Match handler tests.

STAGE 5 — formula_v2 migration (committed same-milestone blocking bead, slippage trips its own registered Spec's lifecycle teeth). Delete cmd/gc/feature_flags.go, api server syncFeatureFlags, SetFormulaV2Enabled/SetGraphApplyEnabled atomic.Bools, formulatest.LockV2ForTest mutex, ~20 molecule_test save/restores; thread via Flags accessors/DI; absorb GC_DOLT_AUTO_GC_ENABLED and GC_EVENTS_ROTATION_ENABLED with EnvSemantics=fills-nil preserved (any precedence unification is a separate release-noted breaking change); register the graph_workflows tombstone with RemovedIn version anchor.

GRADUATION (post-tag): add bdConditionalWritesMinVersion anchor to deps.env under TestBDVersionPins lockstep; probe switches from help-grep to version-compare; two-stage plain-Go lifecycle test enforces Off→Auto when BD_VERSION crosses the floor (FlipDueBy grace = +1 anchor bump for the version-bump PR) and DELETION (flag, accessor, config field, legacy read-then-write branches, tombstone mint) once BD_PREV_VERSION crosses; nightly radar files beads against the Owner for wall-clock staleness throughout.

## Settled decisions

### Registry shape (typed Spec)

**Decision:** internal/rollout holds an UNEXPORTED canonical []Spec (read-only accessor; validator and ForTest take a []Spec parameter so subsystem tests use local synthetic registries). Spec is cut to fields with enforcement or operational teeth: Key; Category (closed enum infra-rollout|infra-migration|infra-killswitch — kept because it now carries enforced per-category lifecycle rules, see lifecycle decision); ConfigPath (reflection-verified against City toml tags); EnvOverride ("" or one GC_* name registered in testenv LeakVectorVars) + EnvSemantics (overrides|fills-nil); Default (registry_test asserts a zero-value config.City's typed accessor equals it — closes the two-homes drift); Owner (dual: bead ID + GitHub handle/team); Expires + VersionAnchor/removal floor (mandatory for rollout/migration, forbidden for killswitch); SelectsBetween [2]string naming the two mechanical code paths; Justification (documentation, not a CI tooth). DELETED: Stability enum, IntroducedIn, GraduationCriterion, Latch, the ~8-flag soft cap. registry.go goes under CODEOWNERS with a named human team.


_Rationale: Every surviving field does mechanical work or gates review; the deleted five were form-filling tax (YAGNI-5), the exported-slice mutation leak (T-8) and the Stable-immortality hatch (LD-2) are closed by construction, and CODEOWNERS is the only real tooth for semantic classification in an agent-authored repo (LD-5, PV-5)._

### Flag value model and scope

**Decision:** Two typed kinds only: rollout.Mode (Off|Auto|Require) for correctness/migration gates, *bool nil=default for kill-switches. City-global scope only. Honest claims replace overstated ones: the REGISTRY refuses per-agent scope (no scope field); the config system could still express one, so a reflection test fails if config.Agent/AgentPatch/AgentOverride ever gains a rollout.Mode-typed field, and the contributor doc states that a per-agent toggle changing what an agent may do is the forbidden shape regardless of declaration site.


_Rationale: PV-6: 'inexpressible by construction' was misleading — the honest version pairs the registry's refusal with the two mechanical checks that CAN exist plus a documented review rule._

### Config placement + fragment-merge preservation (BLOCKER 1)

**Decision:** The flag field lives on the OWNING config section (BeadsConfig.ConditionalWrites beside BDCompatibility), read via a pure accessor mapping ""→default. MANDATORY per-field preservation branch in mergeFragment for every registry flag in a whole-table-LWW section, exactly mirroring the existing daemon.formula_v2 special case (verified at compose.go:1030-1047): a fragment defining an unrelated [beads] sibling key must NOT reset conditional_writes. Enforced by a HAND-WRITTEN merge regression test per flag (template: the daemon.formula_v2 pattern) — the generic registry-driven reflection merge harness is DELETED (no planned flag opens a new section; a one-sentence lifecycle-doc rule covers that future case).


_Rationale: As written the design shipped a silent require→off downgrade through routine fragment layering (PV-1 BLOCKER, verified in-tree). Y-9: the generic harness had zero consumers and known toml.MetaData subtleties; hand-written tests are the proven idiom._

### Load-time validation (no silent fallback via typo)

**Decision:** config load (ValidateSemantics walk driven by registry ConfigPaths) rejects out-of-enum values for every registry flag with a fatal error naming field, bad value, and allowed set. Accessors only ever map ""; they never see an unvalidated non-empty value. The pre-existing NormalizedBDCompatibility silent-normalize (config.go:1401 default: case) is filed and fixed in the same PR.


_Rationale: OO-2: conditional_writes="requre" silently resolving to Off is a silent fallback that falsifies the design's central claim; the cited precedent is itself broken and must not be copied._

### Resolution precedence, Origin, and env semantics

**Decision:** Precedence: builtin default → merged config (existing pack→city→fragment→patch chain, untouched) → env override → per-store runtime capability veto (can never raise, only veto) → structural test override. Origin is COLLAPSED to the three values recoverable with zero loader changes: builtin | config | env (per-layer provenance and the /v0/config/explain extension are deferred until someone asks, costed honestly as new compose.go plumbing then). Env grammar for Mode flags accepts ONLY the mode names (off|auto|require — no truthy spellings for tri-state); an unparseable env value on a correctness-category flag FAILS STARTUP FAST naming var, raw value, and grammar (a break-glass that silently no-ops at 2am is a failed break-glass); when a valid env override CONTRADICTS an explicitly-set config value, Resolve emits a startup structured log + typed event, not just a pull-surface Notice. Per-Spec EnvSemantics preserves each absorbed legacy flag's existing precedence (GC_DOLT_AUTO_GC_ENABLED stays fills-nil; unifying it later is an explicit release-noted breaking change, never a migration side effect). GC_BEADS_CONDITIONAL_WRITES is kept with its named consumer: deployments with baked/immutable config. Break-glass scope is documented as per-process; refusal/degrade diagnostics always carry mode+origin so cross-process divergence is visible in the first log line. Resolve's Notices are retained on the Flags value for the process lifetime and rendered by doctor/status.


_Rationale: Resolves Y-3 (Origin provenance doesn't exist to 'extend'), OO-4 (fail-open typo), PV-4/OO-7/Y-4 (env-wins retro-change and stale-var downgrade made push-loud and per-Spec), OO-8 (per-process divergence), OO-10 (notice lifetime), Y-12 (env override justified by hosted consumer)._

### Composition root and mode threading (single home)

**Decision:** Resolve is folded into the shared loaders loadCityConfig/loadCityConfigWithBuiltinPacks so cfg and Flags travel as one value — resolution stops depending on per-command discipline across the ~30 config-load sites. The conditional-writes mode has EXACTLY ONE home: the beads factory (OpenStoreAtForCity/factory.go) stamps the resolved Mode onto every store it opens; ResolveConditionalWriter(store) takes NO mode parameter and reads the stamped mode — WithConditionalWrites as a caller-facing option is deleted, so the tested-but-unreachable store-says-Require/seam-says-Off state is inexpressible. Entry-point tests (controller, hook, sling, api server) assert that require in a temp city.toml is observed by a probe write.


_Rationale: T-1: cmd/gc has no run() choke point (verified: applyFeatureFlags call sites scattered incl. cmd_sling.go:247, cmd_agent.go:52); T-3: two homes let tests and prod diverge. Factory-stamping satisfies both threading-completeness and single-home; entry-point tests are the routeReadCmd lesson._

### Latching and hot-reload (BLOCKER 2)

**Decision:** v1 of the subsystem is PROCESS-LATCHED for ALL flags; the Latch Spec field is deleted (YAGNI — no reload-tolerant flag exists yet). Operationally: controllerState retains the boot-resolved Flags; the reload path (cmd/gc/api_state.go:1808) carries that boot snapshot into ALL later-constructed components — it never hands a re-Resolved mode to new stores while old stores hold the boot mode. When on-disk config diverges from the latched value, a persistent 'pending restart: conditional_writes require (city.toml) != off (latched at start)' Notice is recorded, surfaced in doctor as a WARNING and later on the status wire. Regression test: boot Off, rewrite config to Require, trigger reload, construct a new store, assert it receives Off and the Notice fired. ResolveOptions (injected LookupEnv) threads into the reload seam.


_Rationale: OO-1 BLOCKER / T-2 / PV-7 / CC-5: the design text permitted a legacy writer racing a CAS writer on gc.control_epoch inside one process after a routine reload — the exact corruption the flag prevents. Whole-process latching is the only definition that makes 'epoch-fence semantics never flip mid-run' true, and YAGNI independently wanted the reload machinery gone._

### Capability model: per-store, one seam, precise classifier

**Decision:** Capability is per RESOLVED store via the optional ConditionalWriter interface (ConditionalAssignmentReleaser template) with typed ErrConditionalWriteUnsupported and PreconditionFailedError{Expected,Current}. ONE injection seam: the capability probe runs through the store's existing CommandRunner (the bdReadyProjectionEnabled shape); WithBDCapabilityProbe is deleted so fake probe and fake runner can never contradict. The probe is LAZY (memoized on first conditional write, not store construction — no subprocess tax on every gc hook), greps the help of ALL FOUR verbs the consumers use (update/close/assign/delete — a mid-merge dev bd can support one but not another), and switches to ProbeBDVersion vs a deps.env bdConditionalWritesMinVersion anchor the day beads tags the release. Classifier: exit 9 → defensively parse the stdout JSON body into PreconditionFailedError; exit 13 latches capable=false ONLY when the machine-parseable body code equals conditional-write-unsupported — a bare 13 (e.g. the beads#3734 close-authority gate) surfaces as a typed NON-latching per-write refusal; usage/unknown-flag errors mentioning --if-revision (what pre-#4682 bd actually emits) map to ErrConditionalWriteUnsupported and trip the latch. Doctor renders probe verdict and runtime latch separately. Nothing persisted; restart re-probes (no-status-files).


_Rationale: CC-6 (policy-refusal conflation silently degrades every subsequent fenced write), CC-7 (old bd never emits 13 — the loud-degrade cell was unreachable), CC-10 (single-verb probe + construction-time cost), T-7 (duplicate seams test unreachable states)._

### CAS write semantics per consumer (fail-closed, no silent fallback)

**Decision:** Four-cell matrix stands: off→byte-identical legacy; auto∧capable→CAS; auto∧incapable→legacy with once-per-store latched diagnostic + typed event; require∧incapable→typed refusal + store-open preflight + doctor ERROR. No code path converts ErrConditionalWriteUnsupported into a plain write. Consumer contracts are now EXPLICIT: (C6 drain reservation) exit 9 → re-read the key; current==control.ID → treat as success and proceed (self-win — preserves the existing three-outcome idempotent-re-entry contract at drain.go:1222-1246); current==other → skip. (C4 Attach epoch) CAS-LAST ordering is pinned; the exit-9 loser wires into the EXISTING partial-attach recovery (isPartialAttemptAttachError, molecule_failed stamping) — loser marks its just-created sub-DAG molecule_failed and neutralizes its dep edge; the level-triggered pass converges on the winner via findExistingAttach. (Ambiguity contract) ambiguous transport errors (isBdAmbiguousWriteError class) on writer-identifying values MUST self-win-check on re-read before concluding loss; the epoch increment tolerates a false loss ONLY because findExistingAttach idempotency runs before the fence — documented on the seam, tested by injecting an ambiguous error after a committed write via the fake CommandRunner. (Granularity) the interface documents that consumers may assume neither value-level nor revision-level conflict semantics; the BdStore read-revision→--if-revision emulation loop is BOUNDED (attempts+backoff) with a typed exhaustion error distinct from PreconditionFailed, and a bd-sql conditional-UPDATE value-CAS (the ReleaseIfCurrent template, bdstore.go:1097) is evaluated to sidestep cross-key interference on metadata-hot control beads. (CachingStore) EVICT, never patch: delete the cache entry on CAS-success-with-failed-refresh AND on every PreconditionFailed; the MemStore-backed CachingStore livelock regression test is a MERGE GATE of the ConditionalWriter PR.


_Rationale: CC-1/CC-2/CC-3 were correctness-eating: self-owned reservations read as losses (stranded undrainable members), unspecified Attach ordering wedges workflows via orphan sub-DAGs, and committed-but-ambiguous CAS writes convert self-wins into false losses. CC-8/CC-9 close the starvation and stale-revision-livelock modes._

### sqlite ConditionalWriter is a blocking deliverable

**Decision:** The sqlite graph store's CompareAndSetMetadataKey (single conditional UPDATE) plus an integration test against the REAL deployed store shape (the deploy/sqlite-b36-probe-attribution topology holding gc.control_epoch / gc.drain.reserved_by) is promoted from a risks footnote to a BLOCKING deliverable of the C4/C6 PR. Until it lands: doctor renders the graph-class store's capability verdict specifically, and the runbook forbids require on the deployed topology. The design doc and runbook also state the fleet-scoped invariant: CAS mutual exclusion holds only when every writer to a ledger is CAS-active or exactly one writer exists; doctor warns when auto is DEGRADED under a declared multi-writer topology.


_Rationale: CC-4 (verified: no sqlite ConditionalWriter exists in-tree; the deployed controller is exactly where the fence matters — without this the flag is permanent DEGRADED where it was motivated, or fleet-stalling refusals) and CC-5 (mixed-writer honesty)._

### Test seams (all typed, all per-instance)

**Decision:** (1) rollout.ForTest takes TYPED With* option funcs (rollout.WithBeadsConditionalWrites(rollout.Require)) generated alongside each Flags accessor — deleting a flag breaks tests at COMPILE time; the string-keyed unknown-key path does not exist. (2) Resolve takes injected LookupEnv (map-backed fake; no t.Setenv; GC_BEADS_CONDITIONAL_WRITES registered in LeakVectorVars, enforced by a registry test). (3) Capability-absent is an INSTANCE TOGGLE (mem.DisableConditionalWrites=true → methods return ErrConditionalWriteUnsupported, interface set intact) — the withoutConditionalWrites wrapper is deleted because it silently strips all five optional store interfaces (the class_store.go:15 lesson). (4) A store-agnostic ConditionalWriter CONFORMANCE SUITE (which operations bump revision, exit-9 equivalence, empty-expected semantics, monotonicity — documented as the interface's doc-comment contract) runs over MemStore, FileStore, CachingStore-over-MemStore, and sqlite in unit CI, and over BdStore against real bd under //go:build integration; it slots into the existing contract-test system (PR #3714). New internal/rollout test package ships its generated testenv_import_test.go.


_Rationale: T-9 (stringly ForTest), T-5 (wrapper erases sibling capabilities — already bitten in-tree), T-4 (fake revision-discipline divergence makes green CI predict nothing about production bd)._

### Lifecycle enforcement: deterministic teeth, no time bombs

**Decision:** Merge-blocking CI checks must be DETERMINISTIC PER COMMIT — no bare date-vs-time.Now() anywhere in the Check path (this repo's agent fleet treats red as a stall; the trivyignore cliff is the prior art). Two-stage version-anchored graduation as ONE plain Go test in the TestBDVersionPins family (~20 lines, NO predicate DSL): stage 1 — deps.env BD_VERSION >= bdConditionalWritesMinVersion && default still Off ⇒ fail demanding the Off→Auto flip; stage 2 — BD_PREV_VERSION >= floor && flag still registered ⇒ fail demanding DELETION (flag, accessor, config field, dead legacy branches); GraduatedIn is recorded in the Spec at flip time. The firing test offers a bounded diff-visible deferral: the version-bump PR may set a machine-checked FlipDueBy = current anchor + 1 bump, so a CVE-driven bd bump never forces a rush Require flip in the same PR — silent-forever stays impossible. Wall-clock Expires moves ENTIRELY to a scheduled non-blocking nightly radar that files/updates a bead against the Owner and feeds a doctor WARN; expiry only hard-fails PR CI when registry.go itself is in the diff. Per-category rules in registry_test: infra-rollout|infra-migration may NEVER be immortal — mandatory Expires + version anchor, terminal state is deletion; only infra-killswitch may be long-lived. The soft cap is DELETED. Tombstones (RetiredKeys in undecoded.go) are minted with RemovedIn=<version anchor> and flagged for deletion by the radar once the anchor exceeds RemovedIn+1 — no wall clock, no 'one release' ambiguity in a branch-deployed fleet. Owner is dual (bead + GitHub handle) and registry.go is CODEOWNERS-gated.


_Rationale: LD-1 BLOCKER + T-6 + Y-2 (zero-diff red = fleet-wide stall + trained neutering), LD-3 (BD_PREV_VERSION historically doesn't move — verified still v1.0.4 vs the 1.0.5 ready-projection floor — and nothing checked the terminal state), LD-6 (bump-PR blast radius), LD-2 (Stable hatch), Y-6/Y-7 (DSL and cap were N=0 machinery), LD-5/LD-9 (orphan owners, undefined release boundaries)._

### Two consumers at stage 1 + legacy-mechanism freeze

**Decision:** The registry ships in stage 1 with TWO Specs registered on day one: beads CAS (infra-rollout) AND formula_v2 (infra-migration, Owner + version-anchored expiry) — the abstraction is born describing two real consumers even though formula_v2's code migrates in stage 5. Stage 1 also lands the FREEZE: a golden-list boundary test (the TestGCNonTestFilesStayOnWorkerBoundary shape) failing on any NEW call site of SetFormulaV2Enabled/SetGraphApplyEnabled/applyFeatureFlags/syncFeatureFlags beyond the current inventory, plus a frozen-baseline inventory test failing on any NEW os.Getenv/LookupEnv site matching "GC_" outside testenv gates, registry EnvOverrides, and an enumerated checked-in baseline — a shadow flag now requires a loud, reviewed baseline edit. The formula_v2 code migration (deleting cmd/gc/feature_flags.go, syncFeatureFlags, both atomic.Bools, the formulatest mutex, ~20 save/restores) is a COMMITTED blocking bead in the same milestone, and its slippage trips the registered Spec's own lifecycle teeth. A tombstone obligation for the deprecated graph_workflows alias is registered in the same pass. Reverse parity is claimed only where mechanically definable: any field typed rollout.Mode (in City OR Agent/AgentPatch/AgentOverride) must have a Spec / must not exist respectively; *bool classification is honestly review-governed.


_Rationale: Y-1 (N=1 abstraction vs the repo's two-implementations rule) and LD-4 (nothing forced stage 5; the old mechanism keeps recruiting) resolve each other: registering both consumers first makes the registry N=2 in contract, the freeze makes the old pattern un-copyable, and the Spec's own expiry makes stage-5 slippage self-punishing. PV-3: the bypass had to become mechanically more expensive than the sanctioned path._

### Observability: minimal in slice 1, honest, push-based for degrade

**Decision:** Slice 1 ships gc doctor ONLY: registry-rendered Rollout Flags section (resolved mode, Origin builtin|config|env, Owner, per-store capability verdicts with probe-vs-latch shown separately, ACTIVE/DEGRADED/FAIL-CLOSED/pending-restart) — ALWAYS with an explicit banner when resolving locally: 'city not running — values resolved from this shell's env and PATH and may differ from the daemon'. Doctor exit contract is pinned by test: FAIL-CLOSED (require∧incapable) and radar-surfaced past-due items render as ERRORS with nonzero exit; DEGRADED is a warning with exit 0. Stage 2/3 add the PUSH surface: a typed registered event beads.conditional_writes.degraded {store, mode, reason, bd_version}, latched once per store — DEGRADED shows in event history and is alertable instead of depending on someone running doctor. The status-wire type — an aggregate verdict PLUS a typed per-store array {store_id, kind, capable, reason} (one boolean cannot express a mixed fleet) including origin and retained Notices — rides the C2/go.mod-bump PR, which already forces genspec + three spec copies + dashboard TS for Bead.Revision; once it exists, doctor queries the live API when the city is up and renders the daemon's OWN latched snapshot. The /v0/config/explain extension is deferred with per-layer origin.


_Rationale: Y-8 (four surfaces for a default-off experimental flag bloats the correctness PR), OO-3 (doctor as re-derivation can lie in both directions — live-API is the fix, staged where the wire regen is free), OO-5 (triple can't carry per-store), OO-6 (the most-likely-to-persist state emitted nothing pushable), OO-9 (exit-code contract)._

### Principle line: how the capability-flag exclusion is actually enforced

**Decision:** The line stands — the exclusion bans agent-behavior toggles that smarter models obviate; infra rollout gates select between two mechanical transports invisible to prompts — but enforcement claims are made honest. Structural (CI): closed Category enum with no agent-capability member; no scope field on Spec; the Agent-struct reflection guard; prompt rendering EXTRACTED from package main into internal/prompt (small mechanical move that also fixes the rendering-in-CLI layering smell) so the forbidden import edge internal/prompt→internal/rollout actually exists and is testable; a registry-driven AST lint (the TestNoLeakVectorReadsAtPackageInit precedent) asserting no PromptContext construction, no PromptContext.Env write, and no template FuncMap references any rollout.Flags accessor. Review-governed (stated as such, not oversold): the value-flow half — a flag value laundered through a bare bool into template data — is caught by the SelectsBetween articulation, the litmus questions in the registry file header ('would a 10x-smarter model obviate this?' / 'do both branches move bytes rather than make decisions?'), the PR-template checklist item ('does any template data struct field trace to a rollout flag?'), and the CODEOWNERS human gate on registry.go. The design text says explicitly: the semantic line is enforced by review-with-teeth; CI blocks the naive paths.


_Rationale: PV-2 (the import test as originally claimed was unimplementable — rendering lives in the same package as the composition root — and PromptContext.Env leaks values without any import edge), PV-5 (form vs semantics), Y-10/Y-11 (overclaiming is how checks get cargo-culted then neutered)._

### Rejected findings

**Decision:** TWO rejections. (1) OO-8's 'consider restricting EnvOverride on process-latched correctness flags to the daemon entry point only' — REJECTED as a v1 mechanism: it complicates the resolver with entry-point awareness for a divergence that the adopted fixes (documented per-process scope + origin-tagged refusal/degrade diagnostics + env-contradicts-config startup event) already make visible in the first log line; revisit if a real bifurcated incident occurs. (2) PV-4's 'require force-off spelling for downgrades' — REJECTED as separate grammar: superseded by the stricter adopted rule that Mode-flag env vars accept ONLY the literal mode names and anything else fails startup on correctness flags; a typo'd truthy value can therefore never downgrade require silently, which was the scenario's teeth.


_Rationale: Both were 'consider' suggestions whose threat is fully covered by adopted amendments with less mechanism._

## Red-team verdicts (all folded into the decisions above)

- **principle-violation**: PROCEED_WITH_AMENDMENTS (1 blocker(s))
- **testability**: PROCEED_WITH_AMENDMENTS (0 blocker(s))
- **cas-correctness**: PROCEED_WITH_AMENDMENTS (0 blocker(s))
- **lifecycle-debt**: PROCEED_WITH_AMENDMENTS (1 blocker(s))
- **operability-observability**: PROCEED_WITH_AMENDMENTS (1 blocker(s))
- **yagni-scope**: PROCEED_WITH_AMENDMENTS (0 blocker(s))

## Decisions locked (2026-07-09 review)

Three build-shaping questions were decided; the design above is authoritative and these override any contrary phrasing in it:

- **Scope = FULL REGISTRY NOW.** Stage 1 ships `internal/rollout` + the typed registry with BOTH the CAS gate and `formula_v2` registered, and the `formula_v2` code migration lands as a **blocking same-milestone** bead (satisfies the "two implementations" rule for real, not on paper).
- **Break-glass on a malformed `GC_BEADS_CONDITIONAL_WRITES` = WARN AND USE CONFIG.** Do not refuse to start. Log a loud warning, ignore the malformed override, fall back to the config-declared mode, and keep the notice on the status wire. (Availability over strict mode-correctness on a mistyped break-glass.)
- **Require mode + mixed-writer topology = `gc doctor` ERROR (block).** When config declares `Require` on a multi-writer topology containing any non-CAS-capable writer, doctor hard-errors — the fleet-scoped invariant cannot hold, so refuse to let the operator believe it does. (Not merely a warning.)
- **`Auto`/capability-resolution is a GENERAL mechanism, NOT beads-locked.** The tri-state `Mode` and the capability-resolution machinery are subsystem-level: a flag opts into `Auto` by supplying a general `rollout.Capability` predicate (`func(ctx) (capable bool, reason string)` — or a small interface), and the resolver computes `enable ∧ capable` generically. beads CAS is consumer #1 and supplies a bd/store capability predicate; a future non-beads flag can supply its own. `ResolveConditionalWriter(store, mode)` is CAS's thin, consumer-owned adapter over the general resolver — NOT the general API. The general core (registry, `Mode`, resolve(enable, capabilityPredicate) → effective) lives in `internal/rollout` with zero beads imports; an import-boundary test forbids `internal/rollout` from importing `internal/beads`. Flags with no runtime capability question (e.g. `formula_v2`) simply supply no predicate and use `Off`/`Require` (≡ off/on).

## Open questions still to resolve (during stage-1 planning; not build-blocking)

3. **Deployed-topology test ownership:** the sqlite `ConditionalWriter` integration test must run against the store shape the reference deployment actually runs — port that harness into main's fixtures, or stage a deploy-branch test as the stage-3 gate? (Owner TBD.)
4. **Named humans / CODEOWNERS:** dual `Owner` (bead ID + GitHub handle/team) per flag, and a `CODEOWNERS` entry for `internal/rollout/registry.go` (the only human gate on `Expires` extensions + `Category`). Owning handle for the CAS flag and for `formula_v2`? (Default: Julian, unless delegated.)
5. **`internal/prompt` extraction (PR-1a):** moves prompt rendering out of `cmd/gc` package main to make the prompt-boundary import test real. Do it in this milestone, or defer and rely on the AST lint + review checklist in v1? (Leaning: do it, since "full registry now" wants the structural enforcement real.)
6. **Graduation-pace forcing function:** stage-2 default-flip/deletion fires when `BD_PREV_VERSION` crosses the CAS floor, but that anchor has historically not moved. Add a radar that flags `BD_PREV_VERSION` lagging `BD_VERSION` by >2 releases, or accept indefinite `Auto`-with-legacy-branches once the default has flipped?
