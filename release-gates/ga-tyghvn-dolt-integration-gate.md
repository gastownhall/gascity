# Release Gate: ga-tyghvn dolt_integration tag

Bead: ga-tyghvn  
Branch: builder/ga-tnaipt-wire-dolt-integration-tag  
Commit under gate: eb2b83d375f5a5760e59dbaf7bdf17f916be7f30  
Base: origin/main e025d64bc723456794b7dc201c32d2d982000a17  
Date: 2026-07-14

## Summary

This gate evaluates a single test-only change to `examples/bd/dolt/compact_real_dolt_test.go`: the real-Dolt compact integration test now builds under the repository's broad `integration` tag as well as the narrower `dolt_integration` tag.

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so the deployer seven-criterion release gate is the operative checklist.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Deploy bead states "Reviewed + PASSED by reviewer gascity/reviewer"; source bead `ga-29lmx5` is referenced in the deploy bead. |
| 2 | Acceptance criteria met | PASS | The only code delta changes the build constraint from `//go:build dolt_integration` to `//go:build integration || dolt_integration`, matching the intended behavior. Targeted tests passed under both tags. |
| 3 | Tests pass | PASS | `go test -tags integration ./examples/bd/dolt/... -run TestCompactScriptRealDoltRemotePush -count=1` passed. `go test -tags dolt_integration ./examples/bd/dolt/... -run TestCompactScriptRealDoltRemotePush -count=1` passed. `TMPDIR=/tmp/gtg make test-fast-parallel` passed all 8 shards. `TMPDIR=/tmp/gtg go vet ./...` passed. `gofmt -l examples/bd/dolt/compact_real_dolt_test.go` produced no output. |
| 4 | No high-severity review findings open | PASS | No open HIGH findings are recorded in the deploy bead. The reviewed change is a one-line test build-tag fix with no production or security surface. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this gate file; this checklist is the only deployer-added file and is committed as the branch tip. |
| 6 | Branch diverges cleanly from main | PASS | `git rev-list --left-right --count origin/main...origin/builder/ga-tnaipt-wire-dolt-integration-tag` returned `0 1`. `git merge-tree --write-tree origin/main origin/builder/ga-tnaipt-wire-dolt-integration-tag` succeeded with tree `9f9aa9f9956f841503cc2080fb0ed5b53657cd9e`. |
| 7 | Single feature theme | PASS | The commit touches only `examples/bd/dolt/compact_real_dolt_test.go`; the branch has one test-infrastructure theme. |

## Notes

An initial `make test-fast-parallel` run used a long `/var/tmp/gotmp-ga-tyghvn-fast2...` path and failed in cmd/gc shards because Unix socket paths exceeded platform limits (`bind: invalid argument`) and controller-poke tests timed out. Rerunning the same fast gate with the short `TMPDIR=/tmp/gtg` path passed all shards. `/tmp` had sufficient free space before the short-path rerun.
