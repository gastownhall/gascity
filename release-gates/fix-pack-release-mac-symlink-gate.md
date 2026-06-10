# Release Gate: fix(pack-release) Mac symlink resolution

- Deploy bead: `ga-2whn6s`
- Source review bead: `ga-evr2co`
- PR: https://github.com/gastownhall/gascity/pull/3300
- Source branch: `builder/ga-o9uf6g`
- Source commit: `acc20cde788f50073d5fd3d272b2c738077c306f`
- Evaluated on: 2026-06-10

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate
uses the deployer release criteria and the repository test guidance in
`TESTING.md` and `Makefile`.

## Summary

This change resolves a macOS pack release failure where `/tmp` and
`/private/tmp` path spelling diverged between `filepath.Abs` and
`git rev-parse --show-toplevel`. The implementation canonicalizes the source
path with `filepath.EvalSymlinks` before computing the relative pack path.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-evr2co` is closed with `REVIEWER VERDICT: PASS`; deploy bead `ga-2whn6s` records reviewed + passed status. |
| 2 | Acceptance criteria met | PASS | The patch is limited to `cmd/gc/cmd_pack_release.go` and adds symlink resolution before `localGitRoot`/`filepath.Rel`; focused pack release tests pass. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestPackRelease' -count=1` passed; `make test` passed; `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Review notes list style/security/spec/coverage PASS and `BLOCKERS: NONE`; no HIGH findings recorded. |
| 5 | Final branch is clean | PASS | Clean detached gate worktree before writing this file; final clean status verified after committing the gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree` between `origin/main` and `acc20cde788f50073d5fd3d272b2c738077c306f` produced a clean merged result with no conflicts. |
| 7 | Single feature theme | PASS | Commit set touches one file in `cmd/gc` and one behavior: local pack-release path canonicalization for symlinked temp directories. |

## Test Evidence

```text
$ go test ./cmd/gc -run 'TestPackRelease' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	0.686s

$ make test
observable go test: PASS log=/tmp/gascity-test.jsonl.RhmpXa

$ go vet ./...
# no output
```

## CI Evidence

PR #3300 was open and mergeable at gate time. GitHub status checks observed
before adding this gate file were green for CI required/preflight, integration
shards, CodeQL, dashboard SPA, and cmd/gc process shards.
