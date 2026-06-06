# Release Gate — ga-ihtj (ArchiveMany ErrAlreadyArchived fix + ga-ipc4 feature)

**Bead:** ga-ihtj (re-review of ga-dkf7; carries ga-ipc4 feature + ga-dkf7 fix)
**Originating work:** ga-ipc4 (ArchiveMany batch path) + ga-dkf7 (preserve ErrAlreadyArchived)
**Branch:** `release/ga-ihtj` — cherry-picks of 285aa325 and 6812b429 onto `origin/main` (`issues.jsonl` stripped per EXCLUDES discipline)
**Evaluator:** gascity/deployer on 2026-04-24
**Verdict:** **PASS**

## Deploy strategy note

`ga-ipc4` (commit 285aa325) introduced `ArchiveMany` and its CLI multi-id
surface; the first-pass review (ga-dkf7) returned REQUEST-CHANGES
because the success path dropped `mail.ErrAlreadyArchived`. The builder
addressed that with commit 6812b429 and the re-review bead ga-ihtj
passed. Both commits ship together because the fix builds directly on
the ArchiveMany plumbing introduced in 285aa325 — neither is shippable
alone. A fresh branch off `origin/main` keeps this change independent
of the in-flight `release/ga-lipl` (PR #1170, beadmail Reply-title
work).

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | ga-ihtj notes: `Re-review verdict: PASS` from `gascity/reviewer` on builder commit 6812b429 (mail `gm-wisp-cp5y`). First-pass ga-dkf7 returned REQUEST-CHANGES on 285aa325; the re-review covers both commits. Single-pass sufficient while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | `ArchiveMany([]string)` batch method present on `mail.Provider`; beadmail single-round-trip `store.CloseAll` preserved for the open subset; `mail.ErrAlreadyArchived` returned per-id for already-closed beads (matches single-id `Archive` semantics); CLI `gc mail delete` / `gc mail archive` accept multiple ids; fake/exec/MCP providers conform; new conformance tests `ArchiveMany_AllSucceed`, `ArchiveMany_EmptyReturnsNil`, `ArchiveMany_PreservesInputOrder`, `ArchiveMany_MixedOpenClosed` apply across all providers; bench `BenchmarkArchiveManyVsSingle` at N=20 and N=200; acceptance subtest `Delete_MultiID_BatchClose` added; `docs/reference/cli.md` updated. |
| 3 | Tests pass | PASS | `go test ./internal/mail/...` green (mail 0.003s, beadmail 0.005s, exec 13.885s); `go test ./cmd/gc -run 'TestMailDelete\|TestMailArchive'` green (0.027s); `go vet ./internal/mail/... ./cmd/gc/...` clean; `go build ./...` clean. |
| 4 | No high-severity review findings open | PASS | Zero HIGH findings. Three non-blocking observations in the re-review: (a) 2N subprocess cost on BdStore fallback path mirrors existing per-id fallback shape — not a regression; (b) TOCTOU window between per-id Get and batched CloseAll is pre-existing and fundamental to Get-then-Close; (c) memstore 20-id batch perf deviation (~1.2x vs 10x target) is tracked separately as ga-dyv7. |
| 5 | Final branch is clean | PASS | `git status` on tracked tree clean after the two cherry-picks. Only `.gitkeep` and `release-gates/ga-bxq5-gate.md` untracked — both are stale scaffold from the prior FAIL gate on ga-bxq5 (blocked on PRs #1147/#1149), unrelated to this deploy. |
| 6 | Branch diverges cleanly from main | PASS | 2 commits ahead of `origin/main` after cherry-picks (plus the gate commit once added). No content conflicts — the only cherry-pick conflict was the expected `issues.jsonl` modify/delete on 285aa325 (issues.jsonl is a bd-sync artifact absent from `origin/main`), stripped via the `EXCLUDES` recipe. 6812b429 applied without conflict. |

## Cherry-pick log

| Source SHA | Branch SHA | Summary |
|------------|------------|---------|
| 285aa325 | 78e6ee4d | perf(mail): add ArchiveMany batch path for multi-id gc mail delete/archive (ga-ipc4) |
| 6812b429 | 962e4f31 | fix(beadmail): preserve ErrAlreadyArchived in ArchiveMany (ga-dkf7) |

`EXCLUDES`: `issues.jsonl` (bd-sync artifact not present on `origin/main`).
Intermediate bd-sync commits (`6ca2008a`, `f5f163ff`) not cherry-picked — they touch only `issues.jsonl`.

## Acceptance criteria — ga-ipc4 / ga-dkf7 done-when

- [x] `mail.Provider.ArchiveMany([]string) ([]ArchiveResult, error)` defined and implemented across beadmail / fake / exec / MCP.
- [x] CLI `gc mail delete <id>...` and `gc mail archive <id>...` accept multiple ids; single-id behavior byte-for-byte unchanged.
- [x] Single-round-trip `store.CloseAll` preserved on the open subset (architectural BdStore win intact).
- [x] `mail.ErrAlreadyArchived` returned per-id for already-closed message beads — verified by `ArchiveMany_MixedOpenClosed` across every provider.
- [x] CLI prints "Already deleted `<id>`" / "Already archived `<id>`" for pre-closed ids (CLI switch at `cmd/gc/cmd_mail.go` fires on `errors.Is(r.Err, mail.ErrAlreadyArchived)`).
- [x] Bench `BenchmarkArchiveManyVsSingle` present at N=20 and N=200; memstore perf deviation called out in bench comment and tracked by ga-dyv7.
- [x] `docs/reference/cli.md` updated with multi-id usage.
- [x] Acceptance subtest `Delete_MultiID_BatchClose` in `test/acceptance/mail_lifecycle_test.go`.

## Test evidence

```
$ go vet ./internal/mail/... ./cmd/gc/...
(clean)

$ go build ./...
(clean)

$ go test ./internal/mail/...
ok   github.com/gastownhall/gascity/internal/mail           0.003s
ok   github.com/gastownhall/gascity/internal/mail/beadmail  0.005s
ok   github.com/gastownhall/gascity/internal/mail/exec      13.885s
?    github.com/gastownhall/gascity/internal/mail/mailtest  [no test files]

$ go test ./cmd/gc -run 'TestMailDelete|TestMailArchive'
ok   github.com/gastownhall/gascity/cmd/gc                  0.027s
```

## Non-blocking follow-ups

- **ga-dyv7** — recalibrate memstore done-when for `ArchiveMany`
  (current ≈1.2x faster at N=20, spec target 10x; architectural BdStore
  win preserved at N=200 ≈8.7x faster). Tracked separately; not a
  deploy blocker.
