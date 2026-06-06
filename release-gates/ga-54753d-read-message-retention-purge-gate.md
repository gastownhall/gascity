# Release Gate: read-message retention purge

- Gate run: 2026-06-01T07:17:40-07:00
- Deploy bead: ga-54753d
- Source deploy child: ga-93j6pj.1
- Review bead: ga-in3cpf
- Branch: builder/ga-93j6pj-1-retention-purge
- Input commit: aa621eceb3100c81bd14525e11a34aff81d0ce74
- Base checked: origin/main at 757d16d25a7100d57c228a44af1733adc2cfeb0d
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses the deployer release-gate criteria and the repository guidance in `TESTING.md`.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-in3cpf` is closed and notes contain `REVIEWER VERDICT: PASS (2026-06-01T13:51:58Z)`. Deploy bead `ga-54753d` was routed as reviewed PASS. |
| 2 | Acceptance criteria met | PASS | Prerequisite retention config slice is merged per `ga-93j6pj.1` notes. Diff from current main contains only `cmd/gc/city_runtime.go`, `cmd/gc/wisp_gc.go`, `cmd/gc/wisp_gc_test.go`, plus this gate artifact. Wisp GC now reads `mail.retention_ttl`, queries only wisp-tier messages with `mail.read=true`, preserves unread/unset/recent/main-tier/non-message beads, disables retention at zero TTL, and logs purge count plus TTL. |
| 3 | Tests pass | PASS | `make test` passed with observable log `/tmp/gascity-test.jsonl.dm4LbG`. `go vet ./...` passed with no output. |
| 4 | No high-severity review findings open | PASS | Review notes list two LOW informational findings and no blockers; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Before writing this gate, `git status --short --branch` showed a clean feature branch aligned with `origin/builder/ga-93j6pj-1-retention-purge`. This gate file is committed as the branch tip and status is rechecked after commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` exited 0 and produced tree `ef30cb67fa070caca1fb37e48f33561d1c673d9b`. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem and one user-facing behavior: `cmd/gc` wisp GC handling for retained read-message mail cleanup. |

## Acceptance Evidence

- `newWispGCForConfig` wires `cfg.Mail.RetentionTTLDuration()` into the existing daemon wisp GC on startup and reload.
- `readMessageWispGCEntries` restricts the purge candidate query to `Type: "message"`, `Status: "closed"`, `IncludeClosed: true`, `TierMode: beads.TierWisps`, and metadata `mail.read=true`.
- `runGC` continues molecule/tracking cleanup and adds the read-message retention purge only when `mail.retention_ttl` is positive.
- Tests cover config wiring, old read-message purge, zero-retention disable/log suppression, retention count/TTL logging, and the existing molecule/tracking GC paths.

## Diff Scope

`git diff --stat origin/main...HEAD` before this gate artifact:

```text
cmd/gc/city_runtime.go |  13 +---
cmd/gc/wisp_gc.go      | 102 +++++++++++++++++++++++-------
cmd/gc/wisp_gc_test.go | 166 ++++++++++++++++++++++++++++++++++++++++++++-----
3 files changed, 231 insertions(+), 50 deletions(-)
```

Final gate result: PASS.
