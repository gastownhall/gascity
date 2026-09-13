# Release Gate: ga-s760.2

Bead: ga-it5y — Review: per-entry CopyFiles drift diff (ga-s760.2 / MF-A)
Source bead: ga-s760.2 — Diagnostic enrichment: per-entry CopyFiles diff in drift dump (MF-A)
Feature branch: builder/ga-s760-2
Evaluated by: gascity/deployer
Date: 2026-05-16

## Gate Result

PASS

## Scope Note

`builder/ga-s760-2` contains the required `ga-s760.1` prerequisite commits
followed by the `ga-s760.2` diagnostic commits. `ga-s760.2` depends on the
shared `runtime.FingerprintVersion` constant and the versioned breakdown shape,
so the gate evaluated the combined branch as the PR surface.

`docs/PROJECT_MANIFEST.md` is not present in this worktree; this gate uses the
release criteria supplied by the deployer role prompt.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-it5y contains two `Reviewer verdict: PASS` notes. The prerequisite review bead ga-34pf also contains two PASS verdicts for the hash-versioning prerequisite. |
| 2 | Acceptance criteria met | PASS | Code inspection found `runtime.FingerprintVersion`, versioned Config/Core/Live fingerprint output, legacy/version-mismatch classifiers, silent rebaseline at all three reconciler drift paths, typed `BreakdownV1` / `BreakdownCopyEntry`, raw JSON drift rendering, and per-entry CopyFiles diff rendering. Tests cover all named FR/NFR surfaces from ga-s760.1 and ga-s760.2. |
| 3 | Tests pass | PASS | `make test-fast-parallel` passed all 8 fast shards. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Review notes list no unresolved HIGH findings. Both security reviews found no security-sensitive concerns. |
| 5 | Final branch is clean | PASS | Worktree was clean before gate file creation; after the gate commit there are no uncommitted changes. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` completed without conflicts (`8f65485f363a095942eba2e44e020553903eab9c`). |

## Commits Evaluated

| Bead | Review | Commits |
|------|--------|---------|
| ga-s760.1 | PASS via ga-34pf | 18b75456d, d28426f54 |
| ga-s760.2 | PASS via ga-it5y | 3c673da8, ba5cc2ad, c8643cdd3 |

## Acceptance Evidence

### ga-s760.1

- `internal/runtime/fingerprint.go` defines `FingerprintVersion` and prefixes
  Config/Core/Live fingerprints with the version.
- `runtime.IsLegacyOrMismatchedVersion` and `runtime.IsVersionMismatchedHash`
  distinguish legacy hashes from version mismatches.
- `cmd/gc/session_reconciler.go` calls `silentRebaselineSessionHashes` in core
  drift, live drift, and asleep named-session drift paths before the normal
  drain path.
- `silentRebaselineSessionHashes` writes `started_config_hash`,
  `started_live_hash`, `live_hash`, and `core_hash_breakdown` together.
- Tests present for versioned output, classifier behavior, silent rebaseline on
  legacy/mismatched hashes, and same-version real drift draining.

### ga-s760.2

- `runtime.BreakdownV1` and `runtime.BreakdownCopyEntry` define the typed
  versioned breakdown shape.
- `runtime.CoreFingerprintBreakdown` returns `BreakdownV1` with
  `Version = FingerprintVersion`.
- `runtime.LogCoreFingerprintDrift` accepts stored breakdown JSON, supports
  legacy map fallback, and renders per-entry CopyFiles diffs.
- Reconciler call sites pass raw `core_hash_breakdown` metadata JSON into the
  runtime drift renderer.
- Tests cover JSON round-trip, legacy fallback, changed entries, added/removed
  entries, probed flag flips, and the two-space stored/current column shape.

## Test Evidence

Commands run:

```text
make test-fast-parallel
go vet ./...
git merge-tree --write-tree HEAD origin/main
```

Results:

```text
make test-fast-parallel: PASS
go vet ./...: PASS
merge-tree with origin/main: PASS
```
