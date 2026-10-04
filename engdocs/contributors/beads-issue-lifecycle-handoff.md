# Beads issue-lifecycle work — handoff

Last updated 2026-08-01. Supersedes `beads-guarded-ops-handoff.md` for everything under epic `ga-ktn9pe.4`.

## What this is

A campaign to give beads one guarded issue-lifecycle surface, used by the `bd` CLI, linked-library
consumers (Gas City's `NativeDoltStore`), and eventually the HTTP/proxied path — so that close/reopen/update
policy is implemented once instead of three times. Eight PRs have merged; a queue of follow-ups remains.

## Where it stands

### Merged upstream (gastownhall/beads)

| SHA | PR | What |
| --- | --- | --- |
| `b92442d1a` | #5191 | The facade: `issueops.Lifecycle` (Create/Update/Close/Reopen), reached via `store.IssueLifecycle()`. Three backends, a cross-backend conformance suite, CLI adoption, and ~10 bug fixes. |
| `ff6eeedbf` | #5206 | A generic status update crossing into a done category now enforces close policy; `bd update --force` gains a dual meaning. |
| `532dadf98` | #5211 | `ga-2kkue`: the proxied single-create path consults `cctx.InfraTypes`, so `bd create -t <infra-type>` lands in `wisps` like every other create surface. |
| `29af03b8c` | #5212 | `ga-z3vht`: `NotPinned` refuses on `issue.Pinned` **or** `status == pinned` (ruling 10). Strictly additive. |
| `252e42c70` | #5217 | `ga-z0qmv`: the assignee-transfer fence enforced in `uow.issueOperations.Update`, predicate shared via `AuthorizeAssigneeTransferWithPools`, plus the cross-backend conformance case the contract never had. |
| `ed8526721` | #5218 | `ga-e6h6i`: `BEADS_DIR` honors `BEADS_TEST_IGNORE_REPO_CONFIG`, closing the config leak that let a dispatched command re-import the repo's `issue-prefix`. |
| `8f421b64f` | #5236 | `ga-ktn9pe.4.8`: idempotent re-close restored by ordering the already-closed no-op ahead of validation — **not** by clearing the pin. |
| `dd3ad8f98` | #5255 | `ga-kjkv1`: a done-crossing generic update applies close's field effects in both funnels; an explicit `closed_at` is judged against the row's status. |

### Open

- **gastownhall/gascity#4885** — draft, deliberately. Routes `NativeDoltStore` writes through
  `store.IssueLifecycle()`. It **cannot compile** until beads publishes a release containing the accessor;
  that is expected and documented in the PR body. Do not add a `replace` directive to make it green.
  Correctness was proven out-of-tree by building against the upstream branch in a scratch dir.

### The public API, as merged

```go
// package issueops — a LEAF package: only non-stdlib deps are internal/types and itself.
// Declares the contract only. No constructor.
type Lifecycle interface { Create; Update; Close; Reopen }

// internal/storage
type Storage interface { /* ~71 legacy methods */; IssueLifecycle() (issueops.Lifecycle, error) }

// every consumer
store, err := beads.OpenBestAvailable(ctx, beadsDir)   // config -> substrate
ops,   err := store.IssueLifecycle()                   // substrate -> lifecycle verbs
```

**The naming rule, which matters more than the name:** a new capability gets a **new role and a new
accessor**, never a method appended to `Lifecycle`. `bd note`, `tag`, `label`, `assign`, `priority`,
`defer`, `done`, `set`, `claim` are all `Update` with a parameter — `IssuePatch` already carries
`Notes`, `Labels`, `Metadata`. Rejecting `AddLabel()` / `SetPriority()` / `AppendNote()`-shaped additions is
the whole discipline. `storage.Storage` reached 71 methods because it lacked this rule.

## Owner rulings — settled, do not relitigate

1. **Constructor** — the accessor *is* the API. No `issueops.New`, no `Source` handle, no `From*`
   constructors. `store.IssueLifecycle()`.
2. **`Claim` stays** in `UpdateRequest` (`bd update --claim` exists today).
3. **`persistence.go`'s issue↔wisp move stays** inside generic Update (main's `DoltStore.UpdateIssue`
   already does it; removing it regresses frozen semantics).
4. **`ForceIDPrefix` stays.** Note: an earlier claim that it had no consumer was **wrong** —
   `bd create --force` uses it at `cmd/bd/create.go:584`.
5. **`bd create --id <occupied>` refuses** instead of silently upserting. Shipped in #5191.
6. **A compound update is one atomic operation** — one hook (including bare `--claim`, which previously
   fired none), delta-only label events, one version commit, with the ID-bearing Dolt commit message kept.
7. **`bd update --parent` replaces all parent edges atomically** (previously removed only the first).
8. **`bd reopen` on a non-closed issue reports nothing-to-do** rather than printing "Reopened".
9. **A generic done-crossing update enforces close policy**, and **`bd update --force` overrides both**
   the assignee fence and close policy. Shipped in #5206.
10. **`bd close` pinned check**: refuse if `issue.Pinned` **OR** `status == pinned`. Strictly additive.
    Decided by audit: Gas Town pins by *status* — an independent 2026-08-02 recount found ~22
    functional non-test sites (`b.List(ListOptions{Status: StatusPinned})` in `hook_check.go`,
    `prime_output.go`, `molecule_step.go`, `up.go` and others), so a boolean-only check would strip
    its protection. That leg is verified and the ruling stands on it.
    **Correction:** the other stated premise — "Gas City reads the boolean
    (`compute_awake_set.go:443,467`)" — is **false**, and it appears in #5212's commit message and PR
    body as well as here. That field is declared `Pinned bool // pin_awake durable wake reason`
    (`cmd/gc/compute_awake_set.go:69`) and populated from `session.WakeCausePinned`
    (`compute_awake_bridge.go:130`). Gas City has **zero** consumers of the beads boolean; its beads
    client has no `Pinned` field and `git log --all -S 'issue.Pinned' -- cmd/gc/` is empty. The
    boolean half of the check is harmless and strictly additive, but do not reason further from the
    Gas City claim.
11. **Commit identity**: author `Julian Knutsen <julianknutsen@users.noreply.github.com>`; trailers
    `Agent-Signature: <model>` and `Co-authored-by: CI Bot <ci@beads.test>`. A council suggested the repo's
    `<agent> on behalf of <human>` form instead — the owner's choice stands.
12. **PR batching**: one PR per bead, opened in waves, P1s first.

## Process for each remaining item

Fable for design/architecture → Opus for implementation → a fable review council before commit → PR
following `CONTRIBUTING.md` + `.github/PULL_REQUEST_TEMPLATE.md` → wait for green CI → merge.

Never Sonnet. Prefer fable for red-teams/councils; fall back to Opus on 429, never Sonnet. **Check
`agents_error` / `<failures>` before trusting an empty findings array** — an all-429 council returns
`{"findings":[]}` which is indistinguishable from a clean pass.

Give the council **three distinct lenses** rather than three identical reviewers: blast-radius (build the
caller table yourself, do not trust the author's), test-adequacy (assume the tests are lying), and
correctness (produce a concrete wrong outcome or downgrade the severity). On Wave 1 those three found
overlapping-but-different things; redundant reviewers would have found one of them.

**Mutation-check every new test before opening the PR.** Revert the production hunk, confirm the new test
fails, restore it verbatim, and confirm `git diff` is empty. A test that passes both ways is worse than no
test, because it reads as coverage. Never do this with `git stash` — the stack is shared across worktrees;
copy the file aside and copy it back.

## Remaining work

### Wave 1 (P1) — done

Both shipped; see the merged table above. Two scoping lessons worth carrying forward:

- `ga-2kkue` was filed as "~2 lines" and the production change really was three, but only after
  establishing that the markdown-batch and graph-plan proxied paths are *also* infra-type-blind. Those
  were left alone deliberately: their embedded counterparts route on flags too, so the two sides are at
  parity with each other, and a mixed-type batch cannot be expressed by one boolean at all.
- `ga-z3vht` stayed at the validation layer on purpose. `bd batch` close bypasses all close validation
  and has no force lever, so a shared-layer guard would have reproduced failure pattern 1 below.

### Wave 2 (P2)

- **`ga-dpfii`** — federated cross-prefix dependency targets classify differently across write plumbings.
  `ExecuteCreate` treats cross-prefix as external and skips the existence check; the uow path only treats a
  literal `external:` prefix as external. Share one classifier, mirroring how `ClassifyPublicCreateError`
  became the single funnel.
- **`ga-tsjxb`** — domain `issue_type` validation skips typed `types.IssueType` values (type-asserts only
  `string`). `TestIssueUpdateAcceptsLegacyIssueTypeRepresentations` pins that typed values round-trip, so
  widening changes accept/reject behavior and needs cross-backend verification.

`ga-z0qmv`, `ga-e6h6i` and `ga-kjkv1` shipped — see the merged table. Three things they taught:

- The `ga-z0qmv` fence was **not** put back where it was removed from. It went into
  `uow.issueOperations.Update` at the facade layer, and the predicate was extracted into
  `issueops.AuthorizeAssigneeTransferWithPools` — pure, pools-injected — so the DBTX path and the uow path
  evaluate one function instead of two copies that can drift. Putting it back in `domain.ApplyUpdate`
  would have repeated the original incident verbatim.
- `ga-e6h6i`'s bead described the mechanism backwards, and it is worth knowing which way round it is:
  nothing was cached and nothing survived a reset. `ResetForTesting` nils the package viper and
  `Initialize` builds a fresh one. The repo config was **re-read from disk on every `Initialize`** because
  in-process dispatch leaks `BEADS_DIR` via a raw `os.Setenv` with no restore, and `BEADS_DIR` was the one
  source that never consulted the ignore set. A fix aimed at cache invalidation would not have worked.
- `ga-kjkv1` was filed as a close-policy bypass and was not one. A metadata-only write moves no status, so
  the row never crosses into done and #5206's gate has nothing to catch. It was an **integrity** defect
  against the `closed_at`-iff-`status == closed` biconditional `types.Validate` enforces on create and
  import but not on the update funnels. Two beads in a row had their premise wrong; check the premise.

### Filed by the fable review councils

Filed rather than folded in. **`.8`, `.9` and `.10` need an owner ruling before anyone implements them.**

- **`ga-ktn9pe.4.8`** (P2, owner ruling) — `bd close --force` leaves `pinned=true` residue, so the new
  boolean trigger now refuses the *idempotent re-close* that used to exit 0. `closeIssueInTx` sets only
  status/closed_at/updated_at/close_reason/closed_by_session/row_lock; there is no pinned-clear anywhere
  in the dolt close lane, and `bd batch` produces the same residue. This matters beyond tidiness: per
  `close.go:189-200` the idempotent re-close is the *only* mechanism that re-drives a stranded molecule
  auto-close. Fix is either clearing pinned on close (mirroring `update.go:329-343`) or exempting
  `status == closed` from the boolean trigger — the latter narrows ruling 10, so it is not an
  implementer's call.
- **`ga-ktn9pe.4.9`** (P2, owner ruling) — `issueops/close.go` has **no pinned check of either kind**, so
  linked-library consumers reaching `store.IssueLifecycle().Close()` are unprotected, and `bd batch`
  bypasses all close validation with no force lever. Needs a ruling on whether `CloseRequest.Force` grows
  yet another meaning (it already gained a dual one in #5206) and whether batch gets a force lever.
- **`ga-ktn9pe.4.10`** (P3, owner ruling) — `bd update --status closed` bypasses the pinned refusal
  entirely and then auto-clears the pin at `update.go:338`. Possibly sanctioned, since update *is* the
  pin/unpin verb. #5206 added blocker/open-children policy to that path but omitted pinned.
- **`ga-ktn9pe.4.11`** (P3) — `bd create --dry-run` previews `ephemeral:false` for an infra type on
  *both* the proxied and embedded paths, because both build the preview from flags alone. Before #5211
  the proxied preview and proxied behavior agreed by both being wrong. Fix both paths together.
- **`ga-ktn9pe.4.12`** (P3) — `forClose`/`forUpdate`/`forDelete`/`forReopen`
  (`internal/validation/issue.go:204-240`) are production-dead; `validateIssueClosable` hand-rolls the
  same chains. #5212 had to update `forClose`'s test purely to keep dead code consistent. Wire it or
  delete it.
- **`ga-ktn9pe.4.13`** (P3) — the new assignee-transfer conformance case seeds only the `issues` table, so
  wisp foreign-assignee transfers are covered by no cross-backend test. A wisp-specific fast path added on
  one backend would reopen the exact divergence class #5217 just closed, on the other table.
- **`ga-ktn9pe.4.14`** (P2, owner ruling) — **two** divergences, not one. `bd close` keeps the pin while the
  `issueops` funnel auto-clears it on any status change away from `pinned` (`update.go:329-343`); and the
  `domain/db` funnel has **no pin auto-clear at all**, so the same `bd update --status closed` behaves
  oppositely on the two write plumbings. This is not cosmetic: `issue.Pinned` is the deletion-protection
  flag at six destructive call sites — `bd gc` (`gc.go:97`, `gc_proxied_server.go:72`), `bd purge`
  (`purge.go:279`, `purge_proxied_server.go:90`) and `bd cleanup` (`cleanup.go:107`). Note that a shared
  helper did **not** prevent this: both funnels already call `ManageClosedAt`, yet the pin block exists in
  one. Whichever direction is ruled, both funnels move in the same commit with a conformance case.
- **`ga-ktn9pe.4.15`** (P3) — the generic-update reopen branch is literal-keyed and, unlike the reopen verb,
  skips `defer_until` clearing and the reopen comment event. Deferred out of `ga-kjkv1` by ruling.

### Wave 3 — CLI consistency (all four approved by the owner)

- **`ga-c69el`** covers two: `bd reopen` never records last-touched (unlike create/update/close — and
  `close.go:29-31`'s own doc claims it does), and prints a bare ID where the others use `formatFeedbackID`.
- **Unify partial-failure exit codes** — `bd close` exits 0, `bd update` exits 1 for the same shape.
  **Needs a bead.** Breaking either way it is unified.
- **Unify the `--json` contract** — create emits a sorted-key object with `schema_version`;
  update/close/reopen emit struct-order arrays without it. **Needs a bead.** Breaking for anything parsing it.

> **Owner input required before implementing the last two.** Bring exact before/after wire shapes and let
> the owner choose; do not pick a direction unilaterally.

### Wave 3 — review findings still owed beads

- Version-commit messages for create/close/reopen lost the issue ID (breaks `bd dolt log | grep <id>`);
  update retains its ID via `updateCommitMessage`.
- A claim-verify replay can double-apply `--append-notes` in a compound facade update
  (`internal/storage/dolt/issue_operations.go:51,65-78`, `claim_verify.go:161-190`).
- No batch-mode HEAD-advance test for `bd close` / `bd reopen` (create and update are covered).
- `issueops` contract docs reference internal symbols rather than the four verbs;
  `CreateDependency.Metadata` is a bare string that must secretly be valid JSON; `CloseRequest.Session`
  is undocumented.
- **`ga-tbr0w`** (filed) — `cmd/bd/ado.go:914-918` writes the non-allowlisted key `source_system` and
  swallows the error.

### Blocked

- **`ga-ktn9pe.4.2`** (Gas City `NativeDoltStore`) and **`ga-jvr4ef`** (HTTP handler thinning) need a
  proxy-resolvable beads release. Local `replace` directives are barred.
- The **proxied-server rewire onto the facade** — it changes Dolt commit granularity (today
  `bd close a b c` is one commit; the facade commits per issue) and the parity suite explicitly could not
  pin the proxied output/exit contract. Needs proxied contract-pin tests committed and green **first**.
- **`ga-ktn9pe.4.7`** gates the OSS publish on `ga-ktn9pe.3` (hosted multibackend rebase). Consider
  splitting: publishing the OSS pseudo-version is what unblocks the two consumers, and neither needs the
  hosted rebase.

## Failure patterns this campaign actually hit

Read this section before writing code. Every item cost a revert or a red CI run.

1. **A policy check added at a shared layer reaches callers that cannot satisfy it.** Happened twice.
   A closed-boundary guard in `UpdateIssueInTx` broke the proxied server and `bd batch`, which had no way to
   translate its refusal. An assignee fence in `domain.ApplyUpdate` read a spec field the proxied caller
   never populated, so `--force` was silently ignored. **Both rode in under commit messages that said
   "validate…".** Before adding any check to a shared helper, write the caller table: every caller, does it
   enforce, can it override. #5206 does this and its transport is deliberately fail-loud — a caller that
   forgets to plumb the override is refused by name rather than silently losing it.
2. **"Unchanged from baseline" is only as good as the baseline.** `TestReopenCommand` was labelled
   pre-existing and that label propagated to four agents; it had actually been broken by commit 1 of the
   branch. It surfaced only when someone diffed the failing set against a real `origin/main` worktree.
   **Always establish a baseline from `origin/main`, not from inside the branch.**
3. **This repo has several ways to print `ok` while running zero tests.** `-run TestSuite` matches nothing
   in `domain/db` (the entry point is `TestDomainDB`). Embedded tests all SKIP without
   `BEADS_TEST_EMBEDDED_DOLT=1`. Always use `-v` and count `RUN/PASS/SKIP/FAIL`.
4. **Ambient environment leaks into test verdicts.** `getOwner()` reads `GIT_AUTHOR_EMAIL`, so
   `bd create --json` gains an `owner` key when it is set — which every agent had exported for commit
   authorship, and CI sets too. A parity test asserting an exact key set passed locally and failed in
   agents. Pin ambient inputs in the harness.
5. **After a squash-merge, every SHA from the merged branch is dangling upstream.** Do not cite branch-local
   commit hashes in shipped source or docs.

## Environment and tooling gotchas

```bash
export GOTOOLCHAIN=go1.26.5
export GOPROXY="file://$(go env GOMODCACHE)/cache/download"
export GOSUMDB=off
export GOFLAGS=-mod=readonly
```

- **`GOTOOLCHAIN` must be exported in the committing shell** or the pre-commit hook's golangci-lint
  self-builds under go1.25 and dies.
- **`GOPROXY` must be the file:// module cache.** The hook runs `go run golangci-lint@v2.10.1`, which does a
  network deprecation lookup on *every* invocation and dies on a TLS blip. `GOPROXY=off` does **not** work —
  it fails the lookup instead.
- **Never `git stash`.** The stash stack is shared across all worktrees of a repo; another session can pop
  your entry. Use a WIP commit or a tarball.
- **`core.hooksPath` lives in the shared git config** (`/data/projects/beads/.git/config`) and has been seen
  pointing at a sibling checkout. Do not "fix" it — it affects other worktrees. Run the gate by hand:
  `gofmt -l internal cmd . | grep -v vendor` and
  `CGO_ENABLED=0 go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1 run --new-from-rev=HEAD`.
- **`gh pr edit` fails** with a Projects-classic deprecation error. Use
  `gh api repos/OWNER/REPO/pulls/N -X PATCH --input -` with a JSON body.
- **Push targets**: `gascity/beads` is a genuine fork of `gastownhall/beads` — PRs go fork → upstream.
  For Gas City, the `julianknutsen` remote **301-redirects to `gastownhall/gascity`**; it is not a fork, so
  pushing there writes to upstream directly.
- **Gas City has a pre-push gate** that runs `make test-fast-parallel` behind a slot mechanism. On a branch
  that cannot compile (e.g. #4885) it can only be bypassed with `--no-verify`, which requires owner consent.
- **Never `go clean -cache`**; never set `GOCACHE`/`TMPDIR`. Never run `./internal/storage/dolt` or
  `./internal/storage/embeddeddolt` unfiltered — 816 and 114 test functions.
- **`go test ./cmd/bd/` has ~25 pre-existing top-level failures** (init/config/doctor/completion), identical
  on `origin/main`. Compare the failing set **by name**, not by count.
- **`make ci-pr-lint` fails on `origin/main` itself** — two gosec findings in `cmd/bd/main.go`. Pre-existing.

## Verification baselines

As of `dd3ad8f98`. Count with `grep -cE '^[[:space:]]*--- PASS'` — `domain/db` nests four levels deep and a
shallower pattern undercounts by an order of magnitude.

```bash
go test -v -count=1 -run TestParity ./cmd/bd/                                    # 43
go test -v -count=1 -run TestDomainDB ./internal/storage/domain/db/              # 800
go test -v -count=1 -run TestIssueOperations ./internal/storage/dolt/            # 73
BEADS_TEST_EMBEDDED_DOLT=1 CGO_ENABLED=1 \
  go test -v -count=1 -run TestEmbeddedIssueOperations ./internal/storage/embeddeddolt/   # 56
go test -v -count=1 ./internal/storage/uow/                                      # 145
go test -v -count=1 ./internal/storage/issueops/                                 # 372
go test -v -count=1 ./internal/validation/                                       # 220
go test -v -count=1 ./internal/config/                                           # 297
BASE_SHA=origin/main bash scripts/check-migration-hygiene.sh                     # clean
```

`cmd/bd/write_verbs_parity_test.go` (40 tests) is the CLI-equivalence evidence. **Its assertions may not be
edited** except for an owner-approved behavior change, in the commit that causes it, with a comment naming
the ruling. It is proven non-vacuous and is order- and environment-independent.

## Key paths

- Beads working tree: `/data/projects/beads-public-issueops-simple` (a worktree of `/data/projects/beads`)
- Gas City consumer: `/data/projects/gascity-native-dolt-issueops-simple`
- Contract: `issueops/issueops.go` · engine: `internal/storage/issueops/` · backends:
  `internal/storage/{dolt,embeddeddolt,uow}/issue_operations*.go` · conformance:
  `internal/storage/conformance/issue_operations_contract.go`
- Design docs from this campaign: `/var/tmp/w46-final-design.md`, `/var/tmp/w46-yagni.md`,
  `/var/tmp/w46-cli-swap-plan.md`, `/var/tmp/w46-closepolicy-workorder.md`
- Recovery backups: `/var/tmp/w46-recovery-backup-20260730T195534`
