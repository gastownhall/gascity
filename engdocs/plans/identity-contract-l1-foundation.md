# Plan: identity contract package + L1 file format (ga-ich5z)

> **Status:** decomposing — 2026-05-06
> **Parent architecture:** `ga-3ski1` (closed; decomposed into A/B/C/D)
> **Designer spec:** `ga-ich5z` (this bead) — full UX design with state
> diagrams, sequence diagrams, edge-case decisions, and per-subtest
> test plan
> **Decomposed into:** 4 builder beads (see Children below)
> **Blocks:** child B (`ga-a75ro`), child C (`ga-ue241`), child D
> (`ga-xxgld`) — all wait on the `ReadProjectIdentity` /
> `WriteProjectIdentity` signatures landing in code

## Context

Project identity (`project_id`) currently lives in 2 uncoordinated
places — `metadata.json` (gitignored cache) and the dolt
`metadata._project_id` row — with 7+ writers and no canonical source.
The architect proposed a 3-layer model:

- **L1** — git-tracked `.beads/identity.toml` (canonical, this bead)
- **L2** — `metadata.json#project_id` (cache, regenerated from L1)
- **L3** — dolt `metadata._project_id` row (DB stamp, verified
  against L1 on connect)

This bead establishes **L1 only**: the file format, the reader/writer
contract, the gitignore negation, and a lint guard against unauthorized
writers. The reconcile logic (which decides what id to write when L1
is absent) is child B.

## Why split this way

Designer's handoff suggested 6 sequential steps, but they fall into
4 coherent units that can ship as separate PRs / builder turns:

1. **Read path + path helper** — bottom of the API; required by
   B (reconcile) and any caller that wants to *check* L1.
2. **Write path** — required by B (the only legitimate writer outside
   tests).
3. **Lint guard** — repo-wide invariant enforcement; can land any
   time after the package exists.
4. **Gitignore negation** — fully independent; no Go code dependency.

Splitting Read vs Write keeps each PR small, lets the builder run
the read-path tests against an empty filesystem (no Write needed),
and lets the lint guard land as a pure invariant test.

## Children

| ID         | Title                                                                | Routing label   | Routes to          | Depends on |
|------------|----------------------------------------------------------------------|-----------------|--------------------|------------|
| `ga-401s4` | Add identity contract package: `ProjectIdentityPath` + `ReadProjectIdentity` + tests | `ready-to-build` | `gascity/builder` | —          |
| `ga-7o5mb` | Add `WriteProjectIdentity` + tests                                   | `ready-to-build` | `gascity/builder` | `ga-401s4` |
| `ga-b4gug` | Add lint guard test: no external writers to `identity.toml`          | `ready-to-build` | `gascity/builder` | `ga-401s4` |
| `ga-4tg3j` | Add `!.beads/identity.toml` negation to city + rig gitignore entries | `ready-to-build` | `gascity/builder` | —          |

Total acceptance for `ga-ich5z`: all 4 children closed, plus the
parent's own checklist (which is implicitly covered by the union of
the children).

## Acceptance for the parent (ga-ich5z)

Met when all of the following hold (these mirror the designer's
"Builder acceptance checklist"):

- [ ] `internal/beads/contract/identity.go` exists with three
      exported functions, full doc comments, strict-on-parse,
      lenient-on-read semantics
- [ ] `internal/beads/contract/identity_test.go` covers subtests
      A1-A12, B1-B10, C1-C2 from the designer's test plan
- [ ] Lint guard test (D1) passes: no Go file outside the contract
      package writes to a path ending `identity.toml`
- [ ] `cmd/gc/gitignore.go` adds `!.beads/identity.toml` after
      `.beads/*` in BOTH `cityGitignoreEntries` and
      `rigGitignoreEntries`
- [ ] `cmd/gc/gitignore_test.go` asserts presence + ordering
- [ ] `go test ./internal/beads/contract/... ./cmd/gc/... -count=1`
      passes
- [ ] `go vet ./...` clean

## Notes for builders

- **Read the designer's bead in full before starting any child.** It
  has explicit pseudocode, state diagrams, and edge-case decisions
  that take precedence over anything you might infer from the parent
  architecture (`ga-3ski1`).
- **Section 7 of the design** lists 6 edge-case decisions
  (symlinks, `null` literal, case sensitivity, duplicate keys,
  concurrent writers, `.beads/` mkdir-on-write). Honor all of them;
  do not re-litigate.
- **`fsys.WriteFileIfChangedAtomic`** is the helper to use — do not
  hand-roll temp+rename. It already gives you idempotence, atomicity,
  and symlink replacement.
- **`BurntSushi/toml`** with `MetaData.Undecoded()` is the strict-parse
  idiom. Existing usage in `internal/config/config.go` (search for
  `Undecoded`) is the reference pattern.

## Out of scope

These belong to siblings B/C/D of `ga-3ski1` and must not creep into
this branch:

- `EnsureProjectIdentity` reconcile entrypoint (child B = `ga-a75ro`)
- `metadata.json#project_id` regeneration from L1 (child C = `ga-ue241`)
- `gc-beads-bd.sh` wrapper updates (child C)
- `project.identity.stamped` event registration (child C)
- `gc doctor` identity drift check (child D = `ga-xxgld`)
- k8s pod-side L1 projection (child D)
