# Release Gate: ga-a3ry.1 phase 1 drift detection foundation

Bead: ga-fd01
Branch: builder/ga-a3ry-1
Feature commit: ffb6e99b984d4b7fabbf22dd4090671719eee84b
Base: origin/main at 7f77aed3e
Evaluated: 2026-05-15

Note: this branch does not contain docs/PROJECT_MANIFEST.md. Gate criteria
were evaluated against the deployer role's Release Gate Criteria table.

## Summary

PASS. The branch is a clean one-commit feature branch on origin/main. It
contains the phase-1 pure-data drift detection helpers, daemon config kill
switch, unit coverage, and generated schema/reference docs. Phase-2 runtime
wiring remains out of scope for this bead.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Bead notes contain reviewer verdict PASS from reviewer-gascity/reviewer dated 2026-04-30. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/drift.go` defines SupervisorStatus, PackRootStatus, SupervisorClient, DetectBinaryDrift, DetectPackDrift, PollReady, and restartLoopGuard. `internal/config/config.go` adds `[daemon].auto_restart_on_drift` with default-true AutoRestartOnDriftEnabled. Tests cover the drift helpers and config default/true/false behavior. Generated schema/reference docs include `auto_restart_on_drift`. |
| 3 | Tests pass | PASS | `make test-fast-parallel` passed all fast jobs. Focused checks passed: `go test ./cmd/gc -run 'TestDetectBinaryDrift\|TestDetectPackDrift\|TestPollReady\|TestRestartLoopGuard' -count=1`, `go test ./internal/config -run TestDaemonAutoRestartOnDrift -count=1`, `go test ./test/docsync -run TestSchemaFreshness -count=1`, and `go vet ./...`. |
| 4 | No high-severity review findings open | PASS | Review notes report "NONE blocking"; no unresolved HIGH or CRITICAL findings were identified in the bead review notes. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file. This gate file is the only deployer change and will be committed before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` passed before the gate commit; `git diff --check origin/main...HEAD` passed with no whitespace errors. The feature branch is based directly on origin/main 7f77aed3e. |

## Acceptance Evidence

| Area | Evidence |
|------|----------|
| Pure-data detection helpers | `cmd/gc/drift.go` lines 16, 31, 39, 53, 68, 112, 142, 149, and 156 define the expected helper types/functions. |
| Drift helper tests | `cmd/gc/drift_test.go` contains TestDetectBinaryDrift, TestDetectPackDrift, TestPollReady_succeedsImmediately, TestPollReady_timesOut, TestPollReady_eventuallySucceeds, and TestRestartLoopGuard. |
| Config field and default accessor | `internal/config/config.go` contains `AutoRestartOnDrift *bool` and `AutoRestartOnDriftEnabled()` defaulting nil to true. |
| Config tests | `internal/config/config_test.go` contains TestDaemonAutoRestartOnDriftDefault, TestDaemonAutoRestartOnDriftExplicitTrue, and TestDaemonAutoRestartOnDriftExplicitFalse. |
| Schema/reference docs | `docs/schema/city-schema.json`, `docs/schema/city-schema.txt`, and `docs/reference/config.md` contain `auto_restart_on_drift`. |
| Phase-2 out of scope | No production runtime wiring is included; this matches the bead description's phase-1 scope. |

## Commands Run

```text
make test-fast-parallel
go vet ./...
go test ./cmd/gc -run 'TestDetectBinaryDrift|TestDetectPackDrift|TestPollReady|TestRestartLoopGuard' -count=1
go test ./internal/config -run TestDaemonAutoRestartOnDrift -count=1
go test ./test/docsync -run TestSchemaFreshness -count=1
git merge-base --is-ancestor origin/main HEAD
git diff --check origin/main...HEAD
git config core.hooksPath
```

## Results

```text
make test-fast-parallel: All fast jobs passed
go vet ./...: clean
go test ./cmd/gc: ok github.com/gastownhall/gascity/cmd/gc 0.311s
go test ./internal/config: ok github.com/gastownhall/gascity/internal/config 0.004s
go test ./test/docsync: ok github.com/gastownhall/gascity/test/docsync 1.157s
git merge-base --is-ancestor origin/main HEAD: pass
git diff --check origin/main...HEAD: clean
git config core.hooksPath: .githooks
```
