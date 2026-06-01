# Release Gate: Mail Retention TTL Config Wiring

Date: 2026-06-01
Deploy bead: ga-b61zh6
Source review bead: ga-as3tjd
PR: https://github.com/gastownhall/gascity/pull/2869
Branch: builder/ga-a5muun-1-mail-retention-v2
Reviewed PR head before this gate: 1843e79ffa7170a54f83a048d257020a5d0cc63c
Base checked: origin/main b2b659d421ace115fcc47575b202c0a4541ad75a

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses the deployer prompt criteria and the source bead acceptance checklist.

## Scope

The release diff from current `origin/main` contains one mail retention configuration wiring slice plus this release gate:

| Path | Status | Purpose |
|---|---:|---|
| `internal/config/config.go` | M | Adds `[mail].retention_ttl` config storage and duration parsing. |
| `internal/config/validate_durations.go` | M | Validates non-empty mail retention duration values. |
| `internal/config/config_test.go` | M | Covers accepted and rejected retention TTL duration forms. |
| `internal/config/validate_durations_test.go` | M | Covers invalid mail retention duration validation. |
| `internal/beads/hqstore.go` | M | Threads parsed retention TTL into HQStore options and accessor. |
| `internal/beads/hqstore_production_test.go` | M | Covers the HQStore option/accessor path. |
| `cmd/gc/cmd_init.go` | M | Adds the scaffolded retention TTL comment to generated city config. |
| `cmd/gc/main_test.go` | M | Updates init output expectations. |
| `docs/reference/config.md` | M | Documents the new mail config field. |
| `docs/schema/city-schema.json` | M | Updates generated config schema. |
| `docs/schema/city-schema.txt` | M | Updates generated schema reference text. |
| `release-gates/ga-b61zh6-mail-retention-ttl-config-gate.md` | A | Release gate evidence. |

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-as3tjd` shows a closed review bead with `REVIEW VERDICT: PASS` for PR #2869 and commit `1843e79f`. |
| 2 | Acceptance criteria met | PASS | `[mail].retention_ttl` is parsed as a Go duration, invalid values are rejected by duration validation, the parsed duration is available through HQStore options/accessor, init output and schema/docs are updated, and no purge loop is introduced in this config-only slice. |
| 3 | Tests pass | PASS | Targeted config, HQStore, and init tests passed; `go vet ./...` passed; `make test-fast-parallel` passed with `All fast jobs passed`. |
| 4 | No high-severity review findings open | PASS | Review notes list informational findings only; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Detached PR-head worktree was clean before gate creation; final clean status is verified after committing this gate. |
| 6 | Branch diverges cleanly from main | PASS | `git fetch origin main` refreshed current base; `git merge-tree --write-tree origin/main HEAD` exited 0; `git diff --check origin/main...HEAD` exited 0. |
| 7 | Single feature theme | PASS | Commit set is one mail retention TTL configuration wiring slice across config, HQStore option plumbing, generated schema/docs, and init scaffold tests. |

## Acceptance Evidence

| Source criterion | Result | Evidence |
|---|---|---|
| Add `[mail].retention_ttl` config parsing. | PASS | `internal/config/config.go` stores the field and parses it via `time.ParseDuration`. |
| Reject invalid duration values. | PASS | `ValidateDurations` checks non-empty mail retention TTL values; tests cover invalid `7d` and valid empty, `0`, and `168h` values. |
| Thread the duration into HQStore without adding purge behavior. | PASS | `WithHQStoreMailRetentionTTL` and `MailRetentionTTL()` expose the parsed value; review confirms the purge loop is intentionally out of scope. |
| Update generated config scaffold and expectations. | PASS | `cmd/gc/cmd_init.go` adds an idempotent scaffold comment and `cmd/gc/main_test.go` matches the generated TOML. |
| Update docs and schema. | PASS | `docs/reference/config.md`, `docs/schema/city-schema.json`, and `docs/schema/city-schema.txt` include the new field. |

## Test Evidence

- PASS: `go test ./internal/config -run 'TestMailConfigRetentionTTLDuration|TestValidateDurationsBadMailRetentionTTL' -count=1`
- PASS: `go test ./internal/beads -run 'TestHQStoreOptionAndBranchCoverage' -count=1`
- PASS: `go test ./cmd/gc -run 'TestCityInitExactOutput_DefaultScaffold|TestDoInit' -count=1`
- PASS: `go vet ./...`
- PASS: `make test-fast-parallel`
- PASS: `.githooks/pre-commit` is active via `core.hooksPath=.githooks`; commit hook runs `go test ./test/docsync`.

Gate result: PASS.
