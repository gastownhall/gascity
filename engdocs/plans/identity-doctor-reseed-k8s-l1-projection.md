# Plan: doctor identity check + reseed command + k8s pod-side L1 projection

**Source design:** `ga-xxgld` (closed by pm after decomposition) — child D
of architecture `ga-3ski1` (project-identity three-layer drift).
**Designer:** gascity/designer · 2026-05-11.
**PM:** gascity/pm · 2026-05-11.
**Builder bead (this plan implements):** see PM handoff mail.
**Sibling status:**
- Child A (contract package): landed (closed 2026-05-07).
- Child B (`ga-h0pyln`): IN_PROGRESS on `builder/ga-h0pyln-1` — the
  error-hint string in `cmd/gc/cmd_rig_endpoint.go::readCanonicalProjectID`
  already cites `gc bd doctor --reseed-identity` verbatim; this slice
  must make that command real at runtime.
- Child C (`ga-qku0jy`): OPEN, awaiting builder — owns the
  `project.identity.stamped` event type + payload struct this slice
  consumes. **This builder bead is blocked-by `ga-qku0jy`.**

## Why this slice exists

The three-layer project-identity model (L1 = `.beads/identity.toml`,
L2 = `.beads/metadata.json#project_id`, L3 = dolt
`metadata._project_id`) needs:

1. A **drift probe** (doctor check) so operators can see when the
   layers disagree.
2. An **explicit, audited reseed command** so child B's mismatch
   error can recommend a real recovery path.
3. **K8s pod-side L1 projection** so pods inherit host identity
   instead of generating a fresh L2 and silently drifting.

The designer's pin (`ga-xxgld` §6) is that all three deliverables go
in **one PR** — they share the fake fs + fake dolt + event recorder
test infrastructure and split worse than they integrate.

## Single builder bead, single PR

Total estimate: ~280 LOC source + ~340 LOC tests across four files
plus the two new doctor files.

### Deliverable 1 — Doctor check (`ga-xxgld` §2.1, §2.2)

- New file: `internal/doctor/checks/project_identity.go`.
- Five outcome classifier with the verbatim 10-row decision table in
  `ga-xxgld` §2.1. Read-only by default. `--fix` repairs L2 only;
  never touches L3 (lint guard in §3.4 enforces).
- Register in `cmd/gc/cmd_doctor.go:135` after the existing
  `DeprecatedAttachmentFieldsCheck` line.
- `FixHint` for L3-drift outcomes literally cites
  `gc bd doctor --reseed-identity` — pinned by
  `TestProjectIdentityCheck_FixHintMentionsCommandName` (`ga-xxgld`
  §3.1).

### Deliverable 2 — `gc bd doctor --reseed-identity` (`ga-xxgld` §1.1, §1.3, §2.3)

- New file: `cmd/gc/cmd_bd_doctor.go`.
- Intercept `args[0] == "doctor"` inside `cmd/gc/cmd_bd.go::doBd`
  before the args are forwarded to upstream `bd`. Other `gc bd`
  subcommands are untouched.
- Confirmation rules from `ga-xxgld` §1.3:
  - Interactive TTY: prompt; accept only literal `yes`
    (case-insensitive, trimmed).
  - `--yes`: skip prompt; still print the audit header.
  - `--no-input` without `--yes` (or stdin non-TTY without `--yes`):
    refuse with a clear error.
- Helper signatures in `ga-xxgld` §2.3 (truncateForDisplay,
  isInteractive, dialDoltForScope, upsertDatabaseProjectIDForce,
  recordProjectIdentityStamped) are pinned verbatim — builder may
  copy.
- Reseed emits `project.identity.stamped` with
  `Source="cache_repair"`, `Layer="L3"`, `OldID` populated, via the
  payload struct child C (`ga-qku0jy`) defines.

### Deliverable 3 — K8s pod-side L1 projection (`ga-xxgld` §2.5, §2.6)

- Modify `internal/runtime/k8s/provider.go::initBeadsInPod`
  (lines 743–781 on origin/main) to stage `.beads/identity.toml` via
  the same base64-shell pattern already used for `metadata.json`.
  Patch is pinned verbatim in `ga-xxgld` §2.5.
- Modify `verifyBeadsInPod` (lines 805–820 on origin/main) to test
  for `identity.toml` alongside `metadata.json` and `config.yaml`.
- Add helpers `hostScopeForPod` and `readHostL1File` (signatures
  pinned in `ga-xxgld` §2.6), package-private in the same file.
- Host-L1-absent path: pod's `bd init` fallback unchanged; next
  host-side `gc bd …` migrates L2→L1.

### Deliverable 4 — Lint guard (`ga-xxgld` §3.4)

- AST-scan test asserting `upsertDatabaseProjectIDForce` is called
  ONLY from `cmd/gc/cmd_bd_doctor.go::runReseedIdentity`. Extend the
  existing identity-writers guard from child A (builder grep before
  writing).

## Acceptance criteria (verbatim from `ga-xxgld` §4)

A builder PR is acceptance-complete when all of the following hold:

- [ ] `gc doctor` reports identity drift per scope; `--fix` repairs L2
      from L1 silently; never touches L3 without `--reseed-identity`.
- [ ] `gc bd doctor --reseed-identity` exists as a real subcommand
      (intercepted in `cmd/gc/cmd_bd.go::doBd`), prompts on confirm,
      overwrites L3 from L1, and emits a `project.identity.stamped`
      event with `Source="cache_repair"`, `Layer="L3"`, `OldID`
      populated.
- [ ] K8s pod startup projects L1 into the pod when the host scope
      has it; absent host L1 falls back to the existing `bd init`
      path unchanged.
- [ ] `verifyBeadsInPod` requires `.beads/identity.toml`.
- [ ] Lint guard test ensures the reseed path is the only caller of
      `upsertDatabaseProjectIDForce`.
- [ ] `go test ./internal/doctor/... ./internal/runtime/k8s/... ./cmd/gc/... -count=1` passes.
- [ ] `go vet ./...` clean.
- [ ] All six acceptance bullets in `ga-3ski1`'s "Acceptance" section
      pass (five via tests; sixth — k8s pod identity preservation —
      via the new k8s tests plus a manual repro note in the PR body).

## Test names (pinned in `ga-xxgld` §3 — builder may copy)

### `internal/doctor/checks/project_identity_test.go`

- `TestProjectIdentityCheck_ClassifyOK`
- `TestProjectIdentityCheck_ClassifyMigrationFixable_NoL1`
- `TestProjectIdentityCheck_ClassifyL2DriftFixable`
- `TestProjectIdentityCheck_ClassifyL3DriftUnfixable`
- `TestProjectIdentityCheck_ClassifyL3Unverifiable`
- `TestProjectIdentityCheck_FixRepairsL2OnlyNotL3`
- `TestProjectIdentityCheck_FixRefusesL3UnderAnyOutcome`
- `TestProjectIdentityCheck_MultiScopeAggregation`
- `TestProjectIdentityCheck_FixHintMentionsCommandName`

### `cmd/gc/cmd_bd_doctor_test.go`

- `TestBdDoctor_RejectsWithoutOperation`
- `TestBdDoctor_ReseedHappyPath_Interactive`
- `TestBdDoctor_ReseedHappyPath_AssumeYes`
- `TestBdDoctor_ReseedRefusedOnEmptyConfirm`
- `TestBdDoctor_ReseedRefusedOnNoInput`
- `TestBdDoctor_ReseedRefusedOnLowercaseY`
- `TestBdDoctor_ReseedRefusedWhenL1Absent`
- `TestBdDoctor_ReseedRefusedWhenDoltDown`
- `TestBdDoctor_ReseedEmitsEventOnSuccess`
- `TestBdDoctor_ReseedScopeResolution`
- `TestBdDoctor_PassthroughForUnknownSub`

### `internal/runtime/k8s/provider_test.go` (extensions)

- `TestInitBeadsInPod_PatchCmdReferencesIdentityToml`
- `TestInitBeadsInPod_PatchCmdSkipsIdentityWhenHostL1Absent`
- `TestInitBeadsInPod_PatchCmdBase64EncodesL1Content`
- `TestVerifyBeadsInPod_RequiresIdentityToml`
- `TestHostScopeForPod_RigPath`
- `TestHostScopeForPod_CityPath`

### Lint guard

- `TestOnlyBdDoctorReseedCallsUpsertProjectIDForce`

Total: 27 pinned test names.

## Guardrails (from `ga-xxgld` §6)

- **The reseed path is the ONLY place that calls
  `upsertDatabaseProjectIDForce`.** §3.4 lint guard enforces.
- **The doctor `Fix` method NEVER writes L3.** Decision table §2.1
  has zero paths from `Fix` to L3 mutation.
- **The k8s pod fallback is unchanged when host L1 is absent.** Do
  not make pods authoritative for new-identity generation.
- **Confirmation accepts only literal `yes`** (case-insensitive,
  trimmed). `TestBdDoctor_ReseedRefusedOnLowercaseY` pins.
- **No new event types.** `project.identity.stamped` is owned by
  child C (`ga-qku0jy`). If child C lands with a field-name
  mismatch, update the call site to match — child C wins.
- **Single PR.** All three deliverables share test infrastructure.

## Out of scope (from `ga-xxgld` §5)

- Per-rig dolt data-dir isolation.
- Removing `metadata.json#project_id` entirely.
- Cross-rig identity coordination.
- City-wide `gc bd doctor --reseed-identity --all-scopes`.
- `gc doctor --fix` triggering reseed (must remain explicit).
- GUI/dashboard surface for the reseed flow.

## Coordination

- **Blocked-by `ga-qku0jy`** (child C implementation owns the
  `ProjectIdentityStampedPayload` struct). Once C lands, the call
  site in deliverable 2 wires straight through.
- **Child B (`ga-h0pyln`) is co-flying.** Its mismatch error already
  cites the command this slice creates; landing order is C → D ⇄ B,
  with C strictly first.
- **Diagrams** at
  `/home/jaword/projects/gc-management/.gc/worktrees/gascity/designer/ga-xxgld/`
  (doctor-identity.dot + .png; k8s-l1-projection.dot + .png).

## References

- Design: `bd show ga-xxgld` (closed; design body has the verbatim
  contracts, decision table, helper signatures, and all 27 test
  names).
- Source architecture: `bd show ga-3ski1` (closed).
- Sibling A landed: `ga-401s4`, `ga-7o5mb`, `ga-b4gug`, `ga-4tg3j`.
- Sibling B in flight: `ga-h0pyln` (builder).
- Sibling C ready-to-build: `ga-qku0jy`.
