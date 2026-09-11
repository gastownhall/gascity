# Release Gate: native-store preflight diagnostic docs

Bead: `ga-i9brye`
Source review bead: `ga-k2iovc`
Source branch: `builder/ga-7v7qej-native-embedded-diag`
Release branch: `release/ga-i9brye-native-store-preflight`
Reviewed commit: `f7a16b332b92209ec1fb481f9e5b74243d79a3f3`
Base: `origin/main` at `4c3b612b7fc0cbfff01ae527a9dcd4a1e9ee5741`

The prompted `docs/PROJECT_MANIFEST.md` path is not present in this Gas City
checkout. No Gas City `PROJECT_MANIFEST.md` or `SOFTWARE_FACTORY_MANIFEST.md`
was found under the repository or management tree, so this gate uses the
deployer release criteria from the role prompt.

## Diff Scope

`git diff --name-status origin/main..HEAD`:

```text
M	docs/getting-started/troubleshooting.md
M	internal/beads/contract/preflight_checker.go
```

This is one release unit: the preflight error message and the matching
troubleshooting documentation for the same `dolt_mode_safe` fallback.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-k2iovc` reports `REVIEW VERDICT: PASS`, reviewer `gascity/reviewer`, commit `f7a16b332b92209ec1fb481f9e5b74243d79a3f3`. |
| 2 | Acceptance criteria met | PASS | `rg -n "native_embedded" internal/beads/contract/preflight_checker.go docs/getting-started/troubleshooting.md` returned no matches. `rg -n "dolt_mode=server\|Dolt server mode\|dolt_mode_safe\|per-call bd" ...` shows the new failure message in `preflight_checker.go:186` and the troubleshooting remedy in `docs/getting-started/troubleshooting.md:254-282`. The diff changes only the diagnostic string plus the matching docs page. |
| 3 | Tests pass | PASS | `go test ./internal/beads/contract` passed. `make check-docs` passed. `go test ./test/docsync` passed. `go build -o /tmp/gc-ga-i9brye ./cmd/gc` passed. `go vet ./...` passed. `make test-fast-parallel` passed with all fast shards green. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-k2iovc` contain only `[PASS]` and `[INFO]` findings, with `SECURITY: Pure string + docs change. No injection surface, no auth/access change. No OWASP concerns.` |
| 5 | Final branch is clean | PASS | Before this gate file, `git status --short --branch` returned only `## release/ga-i9brye-native-store-preflight`. After committing this gate file, the same command again returned only `## release/ga-i9brye-native-store-preflight`. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` passed and printed `origin/main is ancestor of HEAD`; the reviewed commit is directly based on `origin/main`. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem and one docs page for the same user-visible behavior: explaining and documenting native-store fallback when `bd context` reports embedded Dolt mode. |

Gate result: PASS.
